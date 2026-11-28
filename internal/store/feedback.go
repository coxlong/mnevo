package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"gorm.io/gorm"
)

type feedbackRow struct {
	ID, SessionID, RequestKey, Reference, Kind, Reason, Evidence string
}

// SubmitFeedback durably records an idempotent Agent feedback submission bound to f.Reference.
func (s *Store) SubmitFeedback(ctx context.Context, f domain.FeedbackInput) (domain.ReceiptResult, error) {
	if !f.Valid() {
		return domain.ReceiptResult{Status: "invalid"}, nil
	}
	var result domain.ReceiptResult
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		access, err := s.lookupReference(tx, f.Reference)
		if err != nil {
			return err
		}
		t := domain.Task{SessionID: access.sessionID, ID: access.taskID}
		var auditRef, auditStrat string
		var auditVer int

		if access.detail.Status == "ok" {
			result, err = saveFeedback(ctx, tx, access.sessionID, f)
			if err != nil {
				return err
			}
			auditRef, auditVer, auditStrat = f.Reference, access.detail.Version, access.detail.Strategy
		} else {
			// If a verified correction superseded the version before the caller retried,
			// return the original receipt only.
			var id, sessID, taskID, strat string
			var ver int
			nowMS := s.now().UnixMilli()
			err := tx.Raw(`SELECT f.id, d.session_id, d.task_id, d.version, d.strategy FROM feedback f
				JOIN deliveries d ON d.reference = f.reference
				JOIN memories m ON m.id = d.memory_id
				WHERE f.request_key = ? AND d.reference = ? AND d.expires > ?
				AND d.revoked = 0 AND m.enabled = 1 AND m.state = 'confirmed'
				AND f.kind = ? AND f.reason = ? AND f.evidence = ?`,
				f.Key, f.Reference, nowMS, f.Kind, f.Reason, f.Evidence).
				Row().Scan(&id, &sessID, &taskID, &ver, &strat)
			if err == nil {
				result = domain.ReceiptResult{Status: "accepted", Receipt: id}
				t = domain.Task{SessionID: sessID, ID: taskID}
				auditRef, auditVer, auditStrat = f.Reference, ver, strat
			} else if errors.Is(err, sql.ErrNoRows) {
				result = domain.ReceiptResult{Status: "unavailable"}
			} else {
				return err
			}
		}
		return s.Audit(ctx, tx, "submit_memory_feedback", t, AuditRecord{
			Status: result.Status, Reference: auditRef, Version: auditVer, Strategy: auditStrat,
		})
	})
	return result, err
}

func saveFeedback(ctx context.Context, tx *gorm.DB, sessionID string, f domain.FeedbackInput) (domain.ReceiptResult, error) {
	var id string
	old := domain.FeedbackInput{Key: f.Key}
	err := tx.Raw(`SELECT id, reference, kind, reason, evidence FROM feedback
		WHERE session_id = ? AND request_key = ?`, sessionID, f.Key).
		Row().Scan(&id, &old.Reference, &old.Kind, &old.Reason, &old.Evidence)
	if err == nil {
		if old != f {
			return domain.ReceiptResult{Status: "conflict"}, nil
		}
		return domain.ReceiptResult{Status: "accepted", Receipt: id}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ReceiptResult{}, err
	}
	row := feedbackRow{
		ID:         domain.Token(),
		SessionID:  sessionID,
		RequestKey: f.Key,
		Reference:  f.Reference,
		Kind:       f.Kind,
		Reason:     f.Reason,
		Evidence:   f.Evidence,
	}
	err = tx.WithContext(ctx).Table("feedback").Create(&row).Error
	return domain.ReceiptResult{Status: "accepted", Receipt: row.ID}, err
}

type pendingFeedback struct {
	PolicyVersion                                      int
	ID, Reference, Kind, Reason, Evidence, MemoryID    string
	MemoryKind, Summary, Details, TagsJSON, Conditions string
	DeliveryStrategy                                   string
	Task                                               domain.Task
	Version, Current, Attempts                         int
	Allowed                                            bool
}

func (s *Store) nextPendingFeedback(db *gorm.DB, id string) (pendingFeedback, error) {
	var p pendingFeedback
	nowMS := s.now().UnixMilli()
	err := db.Raw(`SELECT f.id, f.kind, f.reason, f.evidence, d.reference, d.session_id, d.task_id,
		d.version, m.version, m.enabled = 1 AND m.state = 'confirmed' AND (v.expires_at = 0 OR v.expires_at > ?),
		f.attempts, m.id, m.kind, v.summary, v.details, v.tags, v.conditions, d.strategy, f.policy_version
		FROM feedback f
		JOIN deliveries d ON d.reference = f.reference
		JOIN memories m ON m.id = d.memory_id
		JOIN memory_versions v ON v.memory_id = d.memory_id AND v.version = d.version
		WHERE f.state = 'pending' AND f.next_attempt <= ? AND (? = '' OR f.id = ?)
		ORDER BY f.rowid LIMIT 1`, nowMS, nowMS, id, id).
		Row().Scan(
		&p.ID, &p.Kind, &p.Reason, &p.Evidence, &p.Reference, &p.Task.SessionID, &p.Task.ID,
		&p.Version, &p.Current, &p.Allowed, &p.Attempts, &p.MemoryID, &p.MemoryKind,
		&p.Summary, &p.Details, &p.TagsJSON, &p.Conditions, &p.DeliveryStrategy, &p.PolicyVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return pendingFeedback{}, nil
	}
	return p, err
}

// VerifyNext processes one pending feedback item (Jev gate -> LLM verification for revisions).
func (s *Store) VerifyNext(ctx context.Context) error {
	var snapshot pendingFeedback
	var running domain.Policy

	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		snapshot, err = s.nextPendingFeedback(tx, "")
		if err != nil || snapshot.ID == "" {
			return err
		}
		running, err = s.policy.LoadVersion(snapshot.PolicyVersion)
		if err != nil {
			running = s.policy.Active()
		}
		if snapshot.PolicyVersion == 0 {
			if err := tx.Exec("UPDATE feedback SET policy_version = ? WHERE id = ?", running.Version, snapshot.ID).Error; err != nil {
				return err
			}
			snapshot.PolicyVersion = running.Version
		}
		return nil
	})
	if err != nil || snapshot.ID == "" {
		return err
	}

	var outcome string
	var latency int64
	var revised *domain.MemoryDraft
	isRevisionKind := snapshot.Kind == domain.FeedbackCorrection || snapshot.Kind == domain.FeedbackOutdated

	if snapshot.Allowed && snapshot.Version == snapshot.Current && isRevisionKind {
		providers := s.models.Providers()
		if providers.Gate == nil || providers.Verifier == nil {
			outcome = "models_not_configured"
		} else {
			started := time.Now()
			gateCtx, cancelGate := context.WithTimeout(ctx, 5*time.Second)
			prob, gateErr := providers.Gate.Decide(gateCtx, running.Prompt("jev_verify_gate"), map[string]any{
				"memory_type":       snapshot.MemoryKind,
				"current_summary":   snapshot.Summary,
				"feedback_kind":     snapshot.Kind,
				"feedback_reason":   snapshot.Reason,
				"feedback_evidence": domain.TruncateRunes(snapshot.Evidence, 800),
			})
			latency += max(1, time.Since(started).Milliseconds())
			cancelGate()

			switch {
			case gateErr != nil:
				outcome = "jev_error"
			case prob <= running.Value("feedback.jev_threshold"):
				outcome = "jev_skip"
			default:
				var tags []string
				_ = json.Unmarshal([]byte(snapshot.TagsJSON), &tags)
				started = time.Now()
				llmCtx, cancelLLM := context.WithTimeout(ctx, domain.Milliseconds(running.Value("feedback.llm_ms")))
				verdict, llmErr := providers.Verifier.Verify(llmCtx, running.Prompt("feedback_verify"), model.FeedbackVerifyInput{
					MemoryType:       snapshot.MemoryKind,
					CurrentSummary:   snapshot.Summary,
					CurrentDetails:   snapshot.Details,
					CurrentCondition: snapshot.Conditions,
					CurrentTags:      tags,
					FeedbackKind:     snapshot.Kind,
					FeedbackReason:   snapshot.Reason,
					FeedbackEvidence: snapshot.Evidence,
				})
				latency += max(1, time.Since(started).Milliseconds())
				cancelLLM()

				switch {
				case llmErr != nil || verdict.ShouldRevise == nil:
					outcome = "llm_error"
				case !*verdict.ShouldRevise:
					outcome = "llm_unconfirmed"
				default:
					d := domain.MemoryDraft{
						Kind:       snapshot.MemoryKind,
						Summary:    verdict.Summary,
						Details:    verdict.Details,
						Conditions: verdict.Conditions,
						Tags:       verdict.Tags,
					}
					if len(d.Tags) == 0 {
						d.Tags = tags
					}
					if d.Conditions == "" {
						d.Conditions = snapshot.Conditions
					}
					if d.Normalize() {
						revised = &d
						outcome = "jev_keep+llm_revised"
					} else {
						outcome = "llm_invalid_revision"
					}
				}
			}
		}
	}

	var reembedID string
	var reembedVer int

	err = s.Transaction(ctx, func(tx *gorm.DB) error {
		current, err := s.nextPendingFeedback(tx, snapshot.ID)
		if err != nil || current.ID != snapshot.ID || current.Attempts != snapshot.Attempts {
			return err
		}
		auditRec := AuditRecord{
			Status: "recorded", Reference: current.Reference, Version: current.Version,
			ModelOutcome: outcome, ModelMS: latency, Strategy: running.Label(),
		}

		switch {
		case !current.Allowed || current.Version != current.Current:
			auditRec.Status = "stale"
		case (outcome == "jev_error" || outcome == "llm_error") && current.Attempts < int(running.Value("feedback.retries")):
			auditRec.Status = "retry_pending"
			next := s.now().Add(time.Second * time.Duration(1<<current.Attempts))
			if err := tx.Exec(`UPDATE feedback SET attempts = attempts + 1, next_attempt = ? WHERE id = ?`,
				next.UnixMilli(), current.ID).Error; err != nil {
				return err
			}
			return s.Audit(ctx, tx, "verify_feedback", current.Task, auditRec)
		case outcome == "jev_skip":
			auditRec.Status = "jev_skipped"
		case outcome == "llm_unconfirmed" || outcome == "llm_invalid_revision":
			auditRec.Status = "unconfirmed"
		case revised != nil:
			nextVer := current.Version + 1
			tagsJSON, _ := json.Marshal(revised.Tags)
			srcID := domain.Token()
			excerpt := current.Reason
			if current.Evidence != "" {
				excerpt += "\n" + current.Evidence
			}
			if err := tx.Exec(`INSERT INTO sources(id, origin, location, excerpt, created_at) VALUES (?, 'feedback', ?, ?, ?)`,
				srcID, "feedback:"+current.ID, domain.TruncateRunes(excerpt, 4096), s.now().UnixMilli()).Error; err != nil {
				return err
			}
			if err := tx.Exec(`INSERT INTO memory_versions
				(memory_id, version, source_id, summary, details, tags, conditions, expires_at, feedback_id, created_at)
				SELECT memory_id, ?, ?, ?, ?, ?, ?, expires_at, ?, ?
				FROM memory_versions WHERE memory_id = ? AND version = ?`,
				nextVer, srcID, revised.Summary, revised.Details, string(tagsJSON), revised.Conditions,
				current.ID, s.now().UnixMilli(), current.MemoryID, current.Version).Error; err != nil {
				return err
			}
			if err := tx.Exec(`UPDATE memories SET version = ?, fingerprint = NULL, reinforced_at = ? WHERE id = ?`,
				nextVer, s.now().UnixMilli(), current.MemoryID).Error; err != nil {
				return err
			}
			auditRec.Status = "applied"
			auditRec.Summary = revised.Summary
			auditRec.Version = nextVer
			reembedID, reembedVer = current.MemoryID, nextVer
		}

		if auditRec.Status != "stale" {
			reinforce := auditRec.Status == "applied" || current.Kind == domain.FeedbackHelpful
			if err := s.aggregateFeedback(tx, running, current, reinforce); err != nil {
				return err
			}
			if problem := feedbackProblem(current.Kind, auditRec.Status); problem != "" {
				baseVer := domain.StrategyNumber(current.DeliveryStrategy)
				if err := s.recordObservationTx(tx, baseVer, domain.Observation{
					Kind:     problem,
					Task:     current.Task.ID,
					MemoryID: current.MemoryID,
					Evidence: "feedback " + current.ID + " (" + auditRec.Status + "): " + domain.TruncateRunes(current.Reason, 400),
				}, false); err != nil {
					return err
				}
			}
		}

		if err := tx.Exec(`UPDATE feedback SET state = ? WHERE id = ?`, auditRec.Status, current.ID).Error; err != nil {
			return err
		}
		return s.Audit(ctx, tx, "verify_feedback", current.Task, auditRec)
	})
	if err != nil {
		return err
	}
	if reembedID != "" {
		_ = s.EmbedMemory(ctx, reembedID, reembedVer)
	}
	return nil
}

func feedbackProblem(kind, status string) string {
	switch kind {
	case domain.FeedbackNotApplicable:
		return "false_injection"
	case domain.FeedbackCorrection, domain.FeedbackOutdated:
		if status == "unconfirmed" || status == "jev_skipped" {
			return "bad_revision"
		}
	}
	return ""
}

func (s *Store) aggregateFeedback(tx *gorm.DB, pol domain.Policy, f pendingFeedback, reinforce bool) error {
	category := "agent"
	if (f.Kind == domain.FeedbackCorrection || f.Kind == domain.FeedbackOutdated) && reinforce {
		category = "verified"
	}
	nowMS := s.now().UnixMilli()
	if err := tx.Exec(`INSERT INTO feedback_summary(memory_id, kind, category, count, last_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(memory_id, kind, category) DO UPDATE SET count = min(count+1, 1000000), last_at = excluded.last_at`,
		f.MemoryID, f.Kind, category, nowMS).Error; err != nil {
		return err
	}
	if !reinforce {
		return nil
	}
	effect := tx.Exec("INSERT INTO feedback_effects VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING",
		f.MemoryID, f.Task.ID, f.Kind, nowMS)
	if effect.Error != nil || effect.RowsAffected == 0 {
		return effect.Error
	}
	halfLifeMS := pol.Value("decay."+f.MemoryKind+"_days") * float64((24 * time.Hour).Milliseconds())
	boost := pol.Value("feedback.reinforcement")
	return tx.Exec(`UPDATE memories
		SET strength = min(1, strength * pow(2, -max(0, ? - reinforced_at) / ?) + ?),
		    reinforced_at = ?
		WHERE id = ?`, nowMS, halfLifeMS, boost, nowMS, f.MemoryID).Error
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"gorm.io/gorm"
)

func (s *Store) recordObservationTx(tx *gorm.DB, baseVersion int, o domain.Observation, trusted bool) error {
	if !domain.ValidObservationKind(o.Kind) || o.Task == "" || len(o.Task) > 128 ||
		strings.TrimSpace(o.Evidence) == "" || len(o.Evidence) > 1024 {
		return errors.New("invalid observation")
	}
	return tx.Exec(`INSERT INTO observations(id, kind, base_version, task_id, memory_id, evidence, trusted, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, base_version, task_id) DO UPDATE SET
			evidence = excluded.evidence,
			memory_id = CASE WHEN excluded.memory_id <> '' THEN excluded.memory_id ELSE observations.memory_id END,
			trusted = 1
		WHERE excluded.trusted = 1 AND observations.trusted = 0`,
		domain.Token(), o.Kind, baseVersion, o.Task, o.MemoryID, o.Evidence, trusted, s.now().UnixMilli()).Error
}

// ReportMiss records an unverified Agent-reported recall omission against the latest missed recall task.
func (s *Store) ReportMiss(ctx context.Context, note string) (bool, error) {
	if strings.TrimSpace(note) == "" || len(note) > 1024 {
		return false, nil
	}
	accepted := false
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var sessionID, taskID, label string
		err := tx.Raw(`SELECT session_id, task_id, strategy FROM audit
			WHERE operation = 'recall' AND result IN ('no_match','filtered') AND task_id <> ''
			ORDER BY id DESC LIMIT 1`).Row().Scan(&sessionID, &taskID, &label)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		baseVer := domain.StrategyNumber(label)
		if err := s.recordObservationTx(tx, baseVer, domain.Observation{
			Kind: "miss", Task: taskID, Evidence: note,
		}, false); err != nil {
			return err
		}
		accepted = true
		return s.Audit(ctx, tx, "strategy_observe", domain.Task{SessionID: sessionID, ID: taskID}, AuditRecord{
			Status: "unverified", Strategy: fmt.Sprintf("settings-v%d", baseVer),
		})
	})
	return accepted, err
}

// RecordObservation records an administrator-verified diagnostic observation bound to task audit history.
func (s *Store) RecordObservation(ctx context.Context, o domain.Observation) (bool, error) {
	if !domain.ValidObservationKind(o.Kind) || o.Task == "" || strings.TrimSpace(o.Evidence) == "" {
		return false, errors.New("invalid observation")
	}
	found := false
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var foundTask, label string
		operation, state := "recall", "delivered"
		switch o.Kind {
		case "miss":
			state = "no_match"
		case "wrong_attribution":
			operation, state = "verify_feedback", "recorded"
		case "bad_revision":
			operation, state = "verify_feedback", "applied"
		}
		err := tx.Raw(`SELECT task_id, strategy FROM audit WHERE task_id = ? AND operation = ? AND result = ?
			ORDER BY id DESC LIMIT 1`, o.Task, operation, state).Row().Scan(&foundTask, &label)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		baseVer := domain.StrategyNumber(label)
		if err := s.recordObservationTx(tx, baseVer, o, true); err != nil {
			return err
		}
		found = true
		return s.Audit(ctx, tx, "strategy_observe", domain.Task{ID: foundTask}, AuditRecord{
			Status: "confirmed", Strategy: fmt.Sprintf("settings-v%d", baseVer),
		})
	})
	return found, err
}

// ConfirmObservation promotes an unverified Agent observation to trusted=1.
func (s *Store) ConfirmObservation(ctx context.Context, id string) (bool, error) {
	found := false
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE observations SET trusted = 1 WHERE id = ?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		found = true
		return s.Audit(ctx, tx, "strategy_observe_confirm", domain.Task{}, AuditRecord{Status: "confirmed", Reference: id})
	})
	return found, err
}

// ListObservations lists recent diagnostic observations (or one specific ID).
func (s *Store) ListObservations(ctx context.Context, id string) ([]domain.ObservationRecord, error) {
	var rows []domain.ObservationRecord
	err := s.db.WithContext(ctx).Raw(`SELECT id, kind, base_version AS base, task_id AS task, memory_id, evidence, trusted, created_at
		FROM observations WHERE ? = '' OR id = ? ORDER BY created_at DESC LIMIT 60`, id, id).Scan(&rows).Error
	return rows, err
}

// ListProposals lists recent meta-learning proposals.
func (s *Store) ListProposals(ctx context.Context) ([]domain.Proposal, error) {
	var rows []domain.Proposal
	err := s.db.WithContext(ctx).Raw(`SELECT id, base_version AS base, kind, target_type, target_key,
		before_val, after_val, before_text, after_text, diagnosis, evidence, impact, risk, status, actor, resolved_at, published
		FROM proposals ORDER BY rowid DESC LIMIT 40`).Scan(&rows).Error
	return rows, err
}

// Diagnose runs the Dual-System meta-learning pipeline:
// Statistical threshold -> System 1 (Jev) systemic gate -> System 2 (LLM) root-cause diagnosis -> Go hard-bounds validation.
func (s *Store) Diagnose(ctx context.Context) error {
	providers := s.models.Providers()
	if providers.Gate == nil || providers.Diagnoser == nil {
		return nil
	}
	pol := s.policy.Active()
	since := s.now().AddDate(0, 0, -int(pol.Value("meta.window_days"))).UnixMilli()
	threshold := int(pol.Value("meta.threshold"))

	var kinds []string
	if err := s.db.WithContext(ctx).Raw(`SELECT kind FROM observations
		WHERE base_version = ? AND trusted = 1 AND created_at >= ?
		GROUP BY kind HAVING count(*) >= ? ORDER BY kind`,
		pol.Version, since, threshold).Scan(&kinds).Error; err != nil || len(kinds) == 0 {
		return err
	}

	learnableSettings := make(map[string]domain.Setting)
	for k, v := range pol.Settings {
		if v.Learn {
			learnableSettings[k] = v
		}
	}
	learnablePrompts := make(map[string]domain.PromptSetting)
	for k, v := range pol.Prompts {
		if v.Learn {
			learnablePrompts[k] = v
		}
	}

	for _, kind := range kinds {
		var existing int
		if err := s.db.WithContext(ctx).Raw(`SELECT count(*) FROM proposals WHERE base_version = ? AND kind = ?`,
			pol.Version, kind).Row().Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			continue
		}

		var rows []struct {
			ID, TaskID, MemoryID, Evidence string
		}
		if err := s.db.WithContext(ctx).Raw(`SELECT id, task_id, memory_id, evidence FROM observations
			WHERE base_version = ? AND kind = ? AND trusted = 1 AND created_at >= ?
			ORDER BY created_at DESC, id LIMIT 20`, pol.Version, kind, since).Scan(&rows).Error; err != nil {
			return err
		}
		samples := make([]model.MetaObservationSample, 0, len(rows))
		obsIDs := make([]string, 0, len(rows))
		for _, r := range rows {
			obsIDs = append(obsIDs, r.ID)
			sample := model.MetaObservationSample{
				ObservationID: r.ID, TaskID: r.TaskID, MemoryID: r.MemoryID, Evidence: r.Evidence,
			}
			_ = s.db.WithContext(ctx).Raw(`SELECT match_context, model_outcome, summary FROM audit
				WHERE task_id = ? AND operation IN ('recall','verify_feedback') ORDER BY id DESC LIMIT 1`, r.TaskID).
				Row().Scan(&sample.MatchContext, &sample.ModelOutcome, &sample.Summary)
			samples = append(samples, sample)
		}

		// System 1 (Jev) systemic gate.
		started := time.Now()
		gateCtx, cancelGate := context.WithTimeout(ctx, 5*time.Second)
		prob, gateErr := providers.Gate.Decide(gateCtx, pol.Prompt("jev_diagnose_gate"), map[string]any{
			"problem_kind": kind, "policy_version": pol.Version, "sample_count": len(samples), "samples": samples,
		})
		jevMS := max(1, time.Since(started).Milliseconds())
		cancelGate()
		if gateErr != nil {
			return gateErr
		}
		if prob <= pol.Value("meta.jev_threshold") {
			_ = s.Audit(ctx, nil, "meta_diagnose", domain.Task{}, AuditRecord{
				Strategy: pol.Label(), Status: "jev_skipped", Summary: kind, ModelOutcome: "jev_skip", ModelMS: jevMS,
			})
			continue
		}

		// System 2 (LLM) root-cause diagnosis.
		started = time.Now()
		llmCtx, cancelLLM := context.WithTimeout(ctx, domain.Milliseconds(pol.Value("meta.llm_ms")))
		diag, llmErr := providers.Diagnoser.Diagnose(llmCtx, pol.Prompt("meta_diagnose"), model.MetaDiagnosisInput{
			ProblemKind: kind, PolicyVersion: pol.Version, Samples: samples,
			LearnableSettings: learnableSettings, LearnablePrompts: learnablePrompts,
		})
		llmMS := max(1, time.Since(started).Milliseconds())
		cancelLLM()
		if llmErr != nil {
			return llmErr
		}

		var beforeVal, afterVal float64
		var beforeText, afterText string
		autoApply := false

		switch diag.TargetType {
		case "setting":
			item, ok := pol.Settings[diag.TargetKey]
			if !ok || !item.Learn {
				continue
			}
			clamped := math.Max(item.Bounds[0], math.Min(item.Bounds[1], diag.AfterValue))
			if domain.IntegerSetting(diag.TargetKey) {
				clamped = math.Round(clamped)
			}
			if clamped == item.Value {
				continue
			}
			beforeVal, afterVal = item.Value, clamped
			autoApply = item.Apply == "auto"

		case "prompt":
			item, ok := pol.Prompts[diag.TargetKey]
			if !ok || !item.Learn || strings.TrimSpace(diag.AfterPrompt) == "" || diag.AfterPrompt == item.Value {
				continue
			}
			if err := domain.ValidatePrompt(diag.TargetKey, domain.PromptSetting{
				Value: diag.AfterPrompt, Learn: item.Learn, Apply: item.Apply,
			}); err != nil {
				continue
			}
			beforeText, afterText = item.Value, diag.AfterPrompt

		case "advice":
		default:
			continue
		}

		idsJSON, _ := json.Marshal(obsIDs)
		propID := domain.Token()
		var inserted bool
		err := s.Transaction(ctx, func(tx *gorm.DB) error {
			res := tx.Exec(`INSERT INTO proposals
				(id, base_version, kind, target_type, target_key, before_val, after_val, before_text, after_text, diagnosis, evidence, impact, risk)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(base_version, kind, target_type, target_key) DO NOTHING`,
				propID, pol.Version, kind, diag.TargetType, diag.TargetKey,
				beforeVal, afterVal, beforeText, afterText, diag.Diagnosis, string(idsJSON), diag.Impact, diag.Risk)
			if res.Error != nil {
				return res.Error
			}
			inserted = res.RowsAffected == 1
			return s.Audit(ctx, tx, "meta_diagnose", domain.Task{}, AuditRecord{
				Strategy: pol.Label(), Status: "proposed", Reference: propID,
				Summary:      fmt.Sprintf("%s -> %s:%s", kind, diag.TargetType, diag.TargetKey),
				ModelOutcome: "jev_keep+llm_diagnosed", ModelMS: jevMS + llmMS,
			})
		})
		if err != nil {
			return err
		}
		if inserted && autoApply {
			_, _, err = s.ResolveProposal(ctx, propID, true, "preauthorized")
			return err
		}
	}
	return nil
}

var (
	errProposalStale  = errors.New("proposal strategy version changed")
	errProposalDenied = errors.New("proposal no longer permitted")
)

// ResolveProposal serializes proposal decisions and checks the active policy at publication.
func (s *Store) ResolveProposal(ctx context.Context, id string, approve bool, actor string) (string, domain.Policy, error) {
	if err := ctx.Err(); err != nil {
		return "", domain.Policy{}, err
	}
	// Once published, finish recording the decision even if the caller disconnects.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), domain.IPCBudget)
	defer cancel()
	var status string
	var active domain.Policy
	err := s.Transaction(commitCtx, func(tx *gorm.DB) error {
		var change domain.Proposal
		q := tx.Raw(`SELECT id, base_version AS base, target_type, target_key,
			before_val, after_val, before_text, after_text, status
			FROM proposals WHERE id = ?`, id).Scan(&change)
		if q.Error != nil {
			return q.Error
		}
		active = s.policy.Active()
		if q.RowsAffected == 0 {
			status = "unavailable"
			return nil
		}
		if change.Status != "pending" {
			status = change.Status
			if (approve && status == "rejected") || (!approve && status == "approved") {
				status = "conflict"
			}
			return nil
		}

		reason := "proposal " + id
		published := 0
		decisionActor := actor
		resolvedAt := s.now().UnixMilli()
		status = "rejected"
		// A request can end after publishing the file but before committing SQLite.
		// Recover that decision from the version archive before accepting another one.
		if active.Version > change.Base {
			previous, err := s.policy.LoadVersion(change.Base + 1)
			if err == nil && previous.Version == change.Base+1 && previous.Reason == reason {
				published = previous.Version
				decisionActor = previous.Actor
				resolvedAt = previous.UpdatedAt
				status = "approved"
			}
		}
		if published == 0 && approve {
			if err := ctx.Err(); err != nil {
				return err
			}
			if change.TargetType == "advice" {
				status = "approved"
				if change.Base != active.Version {
					status = "stale"
				}
			} else {
				updated, err := s.policy.Mutate(actor, reason, func(p *domain.Policy) error {
					return applyProposal(p, change, actor)
				})
				switch {
				case errors.Is(err, errProposalStale):
					status, active = "stale", s.policy.Active()
				case errors.Is(err, errProposalDenied):
					status, active = "denied", s.policy.Active()
				case err != nil:
					return err
				default:
					status, active, published = "approved", updated, updated.Version
				}
			}
		}

		if err := tx.Exec(`UPDATE proposals SET status = ?, actor = ?, resolved_at = ?, published = ? WHERE id = ?`,
			status, decisionActor, resolvedAt, published, id).Error; err != nil {
			return err
		}
		if err := s.Audit(commitCtx, tx, "strategy_"+status, domain.Task{}, AuditRecord{
			Status: status, Reference: id, Strategy: active.Label(),
		}); err != nil {
			return err
		}
		if !approve && status == "approved" {
			status = "conflict"
		}
		return nil
	})
	return status, active, err
}

func applyProposal(p *domain.Policy, change domain.Proposal, actor string) error {
	if change.Base != p.Version {
		return errProposalStale
	}

	switch change.TargetType {
	case "setting":
		item, ok := p.Settings[change.TargetKey]
		if !ok || !item.Learn || item.Value != change.BeforeVal ||
			(actor == "preauthorized" && item.Apply != "auto") {
			return errProposalDenied
		}
		item.Value = change.AfterVal
		p.Settings[change.TargetKey] = item
	case "prompt":
		item, ok := p.Prompts[change.TargetKey]
		if !ok || !item.Learn || item.Value != change.BeforeText || actor == "preauthorized" {
			return errProposalDenied
		}
		item.Value = change.AfterText
		p.Prompts[change.TargetKey] = item
	default:
		return errProposalDenied
	}

	if err := domain.ValidatePolicy(*p); err != nil {
		return errProposalDenied
	}
	return nil
}

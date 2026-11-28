package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"unicode/utf8"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/recall"
	"gorm.io/gorm"
)

// Recall runs recall under the current strategy.
func (s *Store) Recall(ctx context.Context, t domain.Task, prompt string) (domain.RecallResult, error) {
	return s.RecallWithPolicy(ctx, t, prompt, s.policy.Active())
}

// RecallWithPolicy runs recall under the same pinned policy as the caller's timeout.
func (s *Store) RecallWithPolicy(ctx context.Context, t domain.Task, prompt string, pol domain.Policy) (domain.RecallResult, error) {
	return recall.Run(ctx, s, s.models.Providers(), t, model.RecallInput{Prompt: prompt}, pol, s.now)
}

// RecallFromTranscript uses the previous assistant answer to resolve follow-up prompts.
func (s *Store) RecallFromTranscript(ctx context.Context, t domain.Task, prompt, path string, pol domain.Policy) (domain.RecallResult, error) {
	input := model.RecallInput{Prompt: prompt}
	providers := s.models.Providers()
	if providers.RecallReady() && ctx.Err() == nil && strings.TrimSpace(prompt) != "" {
		input.PreviousAnswer = previousAssistantReply(path, t.SessionID, prompt)
	}
	return recall.Run(ctx, s, providers, t, input, pol, s.now)
}

// RecallCandidates streams eligible rows and releases the query before any model calls.
func (s *Store) RecallCandidates(ctx context.Context, modelID string, nowMS int64) iter.Seq2[recall.Candidate, error] {
	return func(yield func(recall.Candidate, error) bool) {
		rows, err := s.db.WithContext(ctx).Raw(`SELECT m.id, m.kind, m.version, m.strength, m.reinforced_at,
		v.summary, v.details, v.tags AS tags_json, v.conditions, e.vector
		FROM memories m
		JOIN memory_versions v ON v.memory_id = m.id AND v.version = m.version
		JOIN memory_embeddings e ON e.memory_id = m.id AND e.version = m.version AND e.model = ?
		WHERE m.enabled = 1 AND m.state = 'confirmed'
		AND (v.expires_at = 0 OR v.expires_at > ?)`, modelID, nowMS).Rows()
		if err != nil {
			yield(recall.Candidate{}, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var c recall.Candidate
			var vector sql.RawBytes
			err := rows.Scan(&c.ID, &c.Kind, &c.Version, &c.Strength, &c.ReinforcedAt,
				&c.Summary, &c.Details, &c.TagsJSON, &c.Conditions, &vector)
			c.Vector = vector
			if !yield(c, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(recall.Candidate{}, err)
		}
	}
}

func (s *Store) RecordRecallOutcome(ctx context.Context, t domain.Task, result domain.RecallResult, promptDigest string) error {
	return s.recordRecallOutcome(ctx, nil, t, result, promptDigest)
}

func (s *Store) recordRecallOutcome(ctx context.Context, tx *gorm.DB, t domain.Task, result domain.RecallResult, promptDigest string) error {
	return s.Audit(ctx, tx, "recall", t, AuditRecord{
		Strategy: result.Strategy, Status: result.Status, ModelOutcome: result.ModelOutcome,
		ModelMS: result.ModelMS, MatchContext: "prompt_sha256=" + promptDigest,
	})
}

// CommitRecall rechecks current memory versions and atomically saves deliveries and their audit records.
func (s *Store) CommitRecall(ctx context.Context, t domain.Task, result domain.RecallResult, plan recall.DeliveryPlan) (domain.RecallResult, error) {
	remainingChars := plan.Characters
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		if t.Event == "UserPromptSubmit" {
			var activeTaskID string
			err := tx.Raw("SELECT active_task_id FROM sessions WHERE session_id = ?", t.SessionID).Row().Scan(&activeTaskID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if activeTaskID != t.ID {
				return s.recordRecallOutcome(ctx, tx, t, result, plan.PromptDigest)
			}
		}

		var deliveries []domain.DeliveredMemory
		var filteredCount int
		var lastFailureOutcome string
		var totalJevMS int64

		for _, c := range plan.Evaluations {
			totalJevMS += c.Latency
			matchCtx := fmt.Sprintf("prompt_sha256=%s vec=%.3f score=%.3f jev=%.3f", plan.PromptDigest, c.Similarity, c.Score, c.Probability)

			if c.Outcome != "jev_keep" {
				if c.Outcome == "jev_skip" {
					filteredCount++
					if err := s.Audit(ctx, tx, "recall", t, AuditRecord{
						Strategy: result.Strategy, Status: "filtered", Version: c.Version,
						ModelOutcome: c.Outcome, ModelMS: plan.EmbeddingMS + c.Latency, MatchContext: matchCtx,
					}); err != nil {
						return err
					}
				} else {
					lastFailureOutcome = c.Outcome
				}
				continue
			}

			var currentVersion int
			var expiresAt int64
			err := tx.Raw(`SELECT m.version, v.expires_at FROM memories m
				JOIN memory_versions v ON v.memory_id = m.id AND v.version = m.version
				WHERE m.id = ? AND m.enabled = 1 AND m.state = 'confirmed'`, c.ID).
				Row().Scan(&currentVersion, &expiresAt)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if currentVersion != c.Version || (expiresAt > 0 && expiresAt <= s.now().UnixMilli()) {
				continue
			}

			item := domain.DeliveredMemory{
				Kind:       c.Kind,
				Layer:      "summary",
				Summary:    c.Summary,
				Conditions: c.Conditions,
				Tags:       c.Tags,
				Version:    c.Version,
			}
			if plan.WantsDetail && c.Details != "" && utf8.RuneCountInString(c.Details) <= remainingChars/2 {
				item.Summary += "\n" + c.Details
				item.Layer = "detail"
			}
			cost := utf8.RuneCountInString(item.Summary) + utf8.RuneCountInString(item.Conditions) + 100
			if cost > remainingChars {
				continue
			}

			var existingRef string
			var existingVer int
			err = tx.Raw(`SELECT reference, version FROM deliveries
				WHERE session_id = ? AND task_id = ? AND memory_id = ? AND revoked = 0 AND expires > ?
				ORDER BY rowid DESC LIMIT 1`, t.SessionID, t.ID, c.ID, s.now().UnixMilli()).
				Row().Scan(&existingRef, &existingVer)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if existingRef != "" && existingVer == c.Version {
				item.Reference = existingRef
			} else {
				item.Reference = domain.Token()
				if err := tx.Exec(`INSERT INTO deliveries(reference, session_id, task_id, memory_id, version, strategy, expires)
					VALUES (?, ?, ?, ?, ?, ?, ?)`,
					item.Reference, t.SessionID, t.ID, c.ID, c.Version, result.Strategy,
					s.now().Add(plan.ReferenceTTL).UnixMilli()).Error; err != nil {
					return err
				}
			}

			remainingChars -= cost
			deliveries = append(deliveries, item)

			auditSummary, err := jsonAuditDelivery(item.Summary, item.Conditions, item.Layer)
			if err != nil {
				return err
			}
			if err := s.Audit(ctx, tx, "recall", t, AuditRecord{
				Strategy: result.Strategy, Status: "delivered", Reference: item.Reference,
				Version: item.Version, Summary: auditSummary, ModelOutcome: "jev_keep",
				ModelMS: plan.EmbeddingMS + c.Latency, MatchContext: matchCtx,
			}); err != nil {
				return err
			}
		}

		if len(deliveries) > 0 {
			result.Status = "delivered"
			result.Recalls = deliveries
			result.ModelOutcome = "jev_keep"
			result.ModelMS = plan.EmbeddingMS + totalJevMS
			return nil
		}
		if filteredCount > 0 {
			result.Status = "filtered"
			result.ModelOutcome = "jev_skip"
			result.ModelMS = plan.EmbeddingMS + totalJevMS
			return nil
		}
		if lastFailureOutcome != "" {
			if lastFailureOutcome == "jev_timeout" {
				result.Status = "timeout"
			}
			result.ModelOutcome = lastFailureOutcome
			result.ModelMS = plan.EmbeddingMS + totalJevMS
		}
		return s.recordRecallOutcome(ctx, tx, t, result, plan.PromptDigest)
	})
	return result, err
}

func jsonAuditDelivery(summary, conditions, layer string) (string, error) {
	data, err := json.Marshal(map[string]string{
		"content":    summary,
		"conditions": conditions,
		"layer":      layer,
	})
	return string(data), err
}

type authorizedDelivery struct {
	detail    domain.MemoryDetail
	sessionID string
	taskID    string
	memoryID  string
}

func (s *Store) lookupReference(tx *gorm.DB, ref string) (authorizedDelivery, error) {
	if ref == "" || len(ref) > 64 {
		return authorizedDelivery{detail: domain.MemoryDetail{Status: "unavailable"}}, nil
	}
	var res authorizedDelivery
	var tagsJSON, srcID, srcOrigin, srcLoc, srcExcerpt string
	nowMS := s.now().UnixMilli()
	err := tx.Raw(`SELECT d.session_id, d.task_id, d.memory_id, d.version, v.summary, v.details, v.tags, v.conditions,
		m.kind, d.strategy, src.id, src.origin, src.location, src.excerpt
		FROM deliveries d
		JOIN memories m ON m.id = d.memory_id AND m.version = d.version
		JOIN memory_versions v ON v.memory_id = m.id AND v.version = d.version
		JOIN sources src ON src.id = v.source_id
		WHERE d.reference = ? AND d.revoked = 0 AND d.expires > ?
		AND m.enabled = 1 AND m.state = 'confirmed'
		AND (v.expires_at = 0 OR v.expires_at > ?)`,
		ref, nowMS, nowMS).
		Row().Scan(&res.sessionID, &res.taskID, &res.memoryID, &res.detail.Version,
		&res.detail.Summary, &res.detail.Details, &tagsJSON, &res.detail.Conditions,
		&res.detail.Kind, &res.detail.Strategy, &srcID, &srcOrigin, &srcLoc, &srcExcerpt)
	if errors.Is(err, sql.ErrNoRows) {
		return authorizedDelivery{detail: domain.MemoryDetail{Status: "unavailable"}}, nil
	}
	if err != nil {
		return authorizedDelivery{}, err
	}
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
	res.detail.Status = "ok"
	res.detail.Reference = ref
	res.detail.Tags = tags
	res.detail.Evidence = &domain.Evidence{ID: srcID, Origin: srcOrigin, Location: srcLoc, Excerpt: srcExcerpt}
	return res, nil
}

// GetReference retrieves full memory details for an active, unrevoked delivery reference.
func (s *Store) GetReference(ctx context.Context, ref string) (domain.MemoryDetail, error) {
	var out authorizedDelivery
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = s.lookupReference(tx, ref)
		if err != nil {
			return err
		}
		return s.Audit(ctx, tx, "get_memory", domain.Task{SessionID: out.sessionID, ID: out.taskID}, AuditRecord{
			Status:    out.detail.Status,
			Reference: out.detail.Reference,
			Version:   out.detail.Version,
			Strategy:  out.detail.Strategy,
		})
	})
	return out.detail, err
}

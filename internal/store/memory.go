package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"gorm.io/gorm"
)

// EnqueueFormation durably queues a structured draft or raw natural-language formation job.
func (s *Store) EnqueueFormation(ctx context.Context, t domain.Task, origin, location, rawText string, draft *domain.MemoryDraft) (domain.ReceiptResult, error) {
	rawText = strings.TrimSpace(rawText)
	var draftJSON string
	if draft != nil {
		if !draft.Normalize() {
			return domain.ReceiptResult{Status: "invalid"}, nil
		}
		data, err := json.Marshal(draft)
		if err != nil {
			return domain.ReceiptResult{}, err
		}
		draftJSON = string(data)
	} else if rawText == "" {
		return domain.ReceiptResult{Status: "invalid"}, nil
	}

	id := domain.Digest(t.SessionID + "\x00" + t.ID + "\x00" + origin + "\x00" + location + "\x00" + rawText + "\x00" + draftJSON)
	err := s.insertFormation(s.db.WithContext(ctx), id, t, origin, location, rawText, draftJSON)
	return domain.ReceiptResult{Status: "accepted", Receipt: id}, err
}

func (s *Store) insertFormation(db *gorm.DB, id string, t domain.Task, origin, location, rawText, draftJSON string) error {
	return db.Exec(`INSERT INTO formation
		(id, session_id, task_id, origin, location, raw_text, draft_json, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		id, t.SessionID, t.ID, origin, location, rawText, draftJSON, s.now().UnixMilli()).Error
}

// ImportDocument synchronously imports text/Markdown using Jev (System 1) -> LLM (System 2) -> Embedding.
func (s *Store) ImportDocument(ctx context.Context, in domain.ImportInput) (domain.ImportResult, error) {
	if !in.Valid() {
		return domain.ImportResult{Status: "invalid"}, nil
	}
	providers := s.models.Providers()
	if providers.Gate == nil || providers.Extractor == nil {
		return domain.ImportResult{Status: "models_not_configured", Message: "请先在模型配置中启用 Jev 与 LLM"}, nil
	}

	pol := s.policy.Active()
	chunks := splitChunks(in.Content, 6000)
	var allDrafts []domain.MemoryDraft
	var jevSkipped int
	var totalMS int64

	for _, chunk := range chunks {
		started := time.Now()
		gateCtx, cancelGate := context.WithTimeout(ctx, 5*time.Second)
		prob, gateErr := providers.Gate.Decide(gateCtx, pol.Prompt("jev_extract_gate"), map[string]any{
			"origin":   "import",
			"location": in.Location,
			"excerpt":  domain.TruncateRunes(chunk, 1200),
		})
		totalMS += max(1, time.Since(started).Milliseconds())
		cancelGate()
		if gateErr != nil {
			return domain.ImportResult{Status: "jev_error", Message: gateErr.Error()}, nil
		}
		if prob <= pol.Value("formation.jev_threshold") {
			jevSkipped++
			continue
		}

		started = time.Now()
		llmCtx, cancelLLM := context.WithTimeout(ctx, domain.Milliseconds(pol.Value("formation.llm_ms")))
		drafts, llmErr := providers.Extractor.Extract(llmCtx, pol.Prompt("memory_extract"), chunk)
		totalMS += max(1, time.Since(started).Milliseconds())
		cancelLLM()
		if llmErr != nil {
			return domain.ImportResult{Status: "llm_error", Message: llmErr.Error()}, nil
		}
		allDrafts = append(allDrafts, drafts...)
	}

	if len(allDrafts) == 0 {
		status, outcome := "no_memories", "llm_empty"
		if jevSkipped == len(chunks) {
			status, outcome = "jev_skipped", "jev_skip"
		}
		_ = s.Audit(ctx, nil, "memory_import", domain.Task{}, AuditRecord{
			Strategy: pol.Label(), Status: status, Summary: in.Location, ModelOutcome: outcome, ModelMS: totalMS,
		})
		return domain.ImportResult{Status: status, ModelMS: totalMS}, nil
	}

	type writtenItem struct {
		id      string
		version int
	}
	var written []writtenItem
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		srcID := domain.Token()
		if err := tx.Exec(`INSERT INTO sources(id, origin, location, excerpt, created_at) VALUES (?, 'import', ?, ?, ?)`,
			srcID, in.Location, domain.TruncateRunes(in.Content, 4096), s.now().UnixMilli()).Error; err != nil {
			return err
		}
		for _, d := range allDrafts {
			id, ver, err := s.writeMemory(tx, "", d, srcID, "import", in.Location)
			if err != nil {
				return err
			}
			written = append(written, writtenItem{id: id, version: ver})
		}
		return s.Audit(ctx, tx, "memory_import", domain.Task{}, AuditRecord{
			Strategy:     pol.Label(),
			Status:       "ok",
			Summary:      fmt.Sprintf("%s (%d memories)", in.Location, len(written)),
			ModelOutcome: "jev_keep+llm_extracted",
			ModelMS:      totalMS,
		})
	})
	if err != nil {
		return domain.ImportResult{}, err
	}

	for _, w := range written {
		_ = s.EmbedMemory(ctx, w.id, w.version)
	}
	records, _ := s.ListMemories(ctx, "", "")
	return domain.ImportResult{
		Status:   "ok",
		Message:  fmt.Sprintf("已导入 %d 条记忆", len(written)),
		Count:    len(written),
		ModelMS:  totalMS,
		Memories: records,
	}, nil
}

// FormNext processes the oldest pending formation job with external Jev + LLM calls
// executed strictly outside SQLite write transactions.
func (s *Store) FormNext(ctx context.Context) error {
	var job struct {
		ID, SessionID, TaskID, Origin, Location, RawText, DraftJSON string
	}
	err := s.db.WithContext(ctx).Raw(`SELECT id, session_id, task_id, origin, location, raw_text, draft_json
		FROM formation WHERE state = 'pending' ORDER BY received_at, id LIMIT 1`).
		Row().Scan(&job.ID, &job.SessionID, &job.TaskID, &job.Origin, &job.Location, &job.RawText, &job.DraftJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	t := domain.Task{SessionID: job.SessionID, ID: job.TaskID}
	pol := s.policy.Active()
	providers := s.models.Providers()

	var drafts []domain.MemoryDraft
	var modelOutcome string
	var modelMS int64
	finalState := "confirmed"
	errMsg := ""

	if job.DraftJSON != "" {
		var d domain.MemoryDraft
		if err := json.Unmarshal([]byte(job.DraftJSON), &d); err != nil || !d.Normalize() {
			finalState, errMsg = "invalid", "invalid structured draft"
		} else {
			drafts = []domain.MemoryDraft{d}
		}
	} else {
		needsJevGate := job.Origin == "transcript" || job.Origin == "import"
		switch {
		case needsJevGate && (providers.Gate == nil || providers.Extractor == nil):
			finalState, modelOutcome = "skipped_unconfigured", "models_not_configured"
		case !needsJevGate && providers.Extractor == nil:
			finalState, modelOutcome = "skipped_unconfigured", "llm_not_configured"
		default:
			proceed := true
			if needsJevGate {
				started := time.Now()
				gateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				prob, gErr := providers.Gate.Decide(gateCtx, pol.Prompt("jev_extract_gate"), map[string]any{
					"origin":   job.Origin,
					"location": job.Location,
					"excerpt":  domain.TruncateRunes(job.RawText, 1200),
				})
				modelMS += max(1, time.Since(started).Milliseconds())
				cancel()
				if gErr != nil {
					finalState, modelOutcome, errMsg = "error", "jev_error", gErr.Error()
					proceed = false
				} else if prob <= pol.Value("formation.jev_threshold") {
					finalState, modelOutcome = "jev_skipped", "jev_skip"
					proceed = false
				} else {
					modelOutcome = "jev_keep"
				}
			}
			if proceed {
				promptKey := "memory_extract"
				if job.Origin == "transcript" {
					promptKey = "transcript_extract"
				}
				started := time.Now()
				llmCtx, cancel := context.WithTimeout(ctx, domain.Milliseconds(pol.Value("formation.llm_ms")))
				extracted, lErr := providers.Extractor.Extract(llmCtx, pol.Prompt(promptKey), job.RawText)
				modelMS += max(1, time.Since(started).Milliseconds())
				cancel()
				if lErr != nil {
					finalState, modelOutcome, errMsg = "error", "llm_error", lErr.Error()
				} else if len(extracted) == 0 {
					finalState, modelOutcome = "no_memories", "llm_empty"
				} else {
					drafts = extracted
					if modelOutcome != "" {
						modelOutcome += "+llm_extracted"
					} else {
						modelOutcome = "llm_extracted"
					}
				}
			}
		}
	}

	type createdMem struct {
		id      string
		version int
	}
	var created []createdMem

	err = s.Transaction(ctx, func(tx *gorm.DB) error {
		var memIDs []string
		if len(drafts) > 0 {
			loc := job.Location
			if loc == "" {
				loc = "formation:" + job.ID
			}
			excerpt := job.RawText
			if excerpt == "" {
				excerpt = drafts[0].Summary
				if drafts[0].Details != "" {
					excerpt += "\n" + drafts[0].Details
				}
			}
			srcID := domain.Token()
			if err := tx.Exec(`INSERT INTO sources(id, origin, location, excerpt, created_at) VALUES (?, ?, ?, ?, ?)`,
				srcID, job.Origin, loc, domain.TruncateRunes(excerpt, 4096), s.now().UnixMilli()).Error; err != nil {
				return err
			}
			for _, d := range drafts {
				id, ver, err := s.writeMemory(tx, "", d, srcID, job.Origin, loc)
				if err != nil {
					return err
				}
				memIDs = append(memIDs, id)
				created = append(created, createdMem{id: id, version: ver})
			}
		}
		idsJSON, _ := json.Marshal(memIDs)
		if err := tx.Exec(`UPDATE formation SET state = ?, memory_ids = ?, error = ? WHERE id = ?`,
			finalState, string(idsJSON), errMsg, job.ID).Error; err != nil {
			return err
		}
		firstRef, firstVer := "", 0
		if len(created) > 0 {
			firstRef, firstVer = created[0].id, created[0].version
		}
		return s.Audit(ctx, tx, "form_memory", t, AuditRecord{
			Strategy: pol.Label(), Status: finalState, Reference: firstRef, Version: firstVer,
			Summary: fmt.Sprintf("origin=%s count=%d", job.Origin, len(created)), ModelOutcome: modelOutcome, ModelMS: modelMS,
		})
	})
	if err != nil {
		return err
	}

	for _, m := range created {
		_ = s.EmbedMemory(ctx, m.id, m.version)
	}
	return nil
}

func (s *Store) writeMemory(tx *gorm.DB, id string, d domain.MemoryDraft, sourceID, origin, location string) (string, int, error) {
	fp := domain.MemoryFingerprint(d)
	nowMS := s.now().UnixMilli()
	var version int

	// Older builds left fingerprints on deleted rows.
	if err := tx.Exec("UPDATE memories SET fingerprint = NULL WHERE fingerprint = ? AND state = 'deleted'", fp).Error; err != nil {
		return "", 0, err
	}
	if id == "" {
		err := tx.Raw(`SELECT id, version FROM memories WHERE fingerprint = ?`, fp).Row().Scan(&id, &version)
		if err == nil {
			return id, version, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", 0, err
		}
		id = domain.Token()
		if err := tx.Exec(`INSERT INTO memories(id, kind, state, enabled, version, fingerprint, strength, reinforced_at, created_at)
			VALUES (?, ?, 'confirmed', 1, 0, ?, 1, ?, ?)`, id, d.Kind, fp, nowMS, nowMS).Error; err != nil {
			return "", 0, err
		}
	} else {
		if err := tx.Raw(`SELECT version FROM memories WHERE id = ? AND state <> 'deleted'`, id).Row().Scan(&version); err != nil {
			return "", 0, err
		}
	}

	if sourceID == "" {
		sourceID = domain.Token()
		excerpt := d.Summary
		if d.Details != "" {
			excerpt += "\n" + d.Details
		}
		if err := tx.Exec(`INSERT INTO sources(id, origin, location, excerpt, created_at) VALUES (?, ?, ?, ?, ?)`,
			sourceID, origin, location, excerpt, nowMS).Error; err != nil {
			return "", 0, err
		}
	}

	tagsJSON, err := json.Marshal(d.Tags)
	if err != nil {
		return "", 0, err
	}
	version++
	if err := tx.Exec(`INSERT INTO memory_versions(memory_id, version, source_id, summary, details, tags, conditions, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, version, sourceID, d.Summary, d.Details, string(tagsJSON), d.Conditions, d.ExpiresAt, nowMS).Error; err != nil {
		return "", 0, err
	}
	if err := tx.Exec(`UPDATE memories SET version = ?, kind = ?, state = 'confirmed', enabled = 1,
		fingerprint = ?, strength = 1, reinforced_at = ? WHERE id = ?`,
		version, d.Kind, fp, nowMS, id).Error; err != nil {
		return "", 0, err
	}
	return id, version, nil
}

// CreateMemory creates a confirmed memory directly and eagerly embeds it when an Embedder is active.
func (s *Store) CreateMemory(ctx context.Context, d domain.MemoryDraft) (string, int, error) {
	if !d.Normalize() {
		return "", 0, errors.New("invalid memory draft")
	}
	var id string
	var version int
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		id, version, err = s.writeMemory(tx, "", d, "", "admin", "local-admin:"+domain.Token())
		if err != nil {
			return err
		}
		return s.Audit(ctx, tx, "memory_add", domain.Task{}, AuditRecord{Status: "ok", Reference: id, Version: version})
	})
	if err != nil {
		return "", 0, err
	}
	_ = s.EmbedMemory(ctx, id, version)
	return id, version, nil
}

// CorrectMemory publishes a new version for an existing non-deleted memory and re-embeds it.
func (s *Store) CorrectMemory(ctx context.Context, id string, d domain.MemoryDraft) (int, bool, error) {
	if id == "" || !d.Normalize() {
		return 0, false, errors.New("invalid memory correction")
	}
	var version int
	found := true
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var err error
		_, version, err = s.writeMemory(tx, id, d, "", "admin", "local-admin:"+domain.Token())
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		return s.Audit(ctx, tx, "memory_correct", domain.Task{}, AuditRecord{Status: "ok", Reference: id, Version: version})
	})
	if err != nil || !found {
		return 0, found, err
	}
	_ = s.EmbedMemory(ctx, id, version)
	return version, true, nil
}

// SetMemoryEnabled enables or disables a confirmed memory (revoking active deliveries on disable).
func (s *Store) SetMemoryEnabled(ctx context.Context, id string, enabled bool) (bool, error) {
	found := false
	action := "disable"
	if enabled {
		action = "enable"
	}
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		res := tx.Exec("UPDATE memories SET enabled = ? WHERE id = ? AND state = 'confirmed'", enabled, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		found = true
		if !enabled {
			if err := tx.Exec("UPDATE deliveries SET revoked = 1 WHERE memory_id = ?", id).Error; err != nil {
				return err
			}
		}
		return s.Audit(ctx, tx, "memory_"+action, domain.Task{}, AuditRecord{Status: "ok", Reference: id})
	})
	return found, err
}

// SetMemoryState transitions a memory to disputed, invalid, or deleted (scrubbing content & embeddings on delete).
func (s *Store) SetMemoryState(ctx context.Context, id, state string) (bool, error) {
	switch state {
	case domain.StateDisputed, domain.StateInvalid, domain.StateDeleted:
	default:
		return false, errors.New("invalid target state")
	}
	found := false
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE memories SET state = ?, enabled = 0,
			fingerprint = CASE WHEN ? = 'deleted' THEN NULL ELSE fingerprint END
			WHERE id = ? AND state <> 'deleted'`, state, state, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		found = true
		if state == domain.StateDeleted {
			if err := scrubDeletedMemory(tx, id); err != nil {
				return err
			}
		}
		if err := tx.Exec("UPDATE deliveries SET revoked = 1 WHERE memory_id = ?", id).Error; err != nil {
			return err
		}
		return s.Audit(ctx, tx, "memory_"+state, domain.Task{}, AuditRecord{Status: "ok", Reference: id})
	})
	return found, err
}

// ListMemories searches non-deleted memories (or all versions of id when id != "").
func (s *Store) ListMemories(ctx context.Context, filter, id string) ([]domain.MemoryRecord, error) {
	embedModel := ""
	if emb := s.models.Providers().Embedder; emb != nil {
		embedModel = emb.ModelID()
	}
	type rawRow struct {
		ID           string
		Kind         string
		State        string
		Enabled      bool
		Version      int
		SourceID     string
		Origin       string
		Location     string
		Summary      string
		Details      string
		TagsJSON     string
		Conditions   string
		ExpiresAt    int64
		Strength     float64
		ReinforcedAt int64
		Embedded     bool
	}
	var rows []rawRow
	err := s.db.WithContext(ctx).Raw(`SELECT m.id, m.kind, m.state, m.enabled, v.version, v.source_id,
		src.origin, src.location, v.summary, v.details, v.tags AS tags_json, v.conditions, v.expires_at,
		m.strength, m.reinforced_at,
		(? <> '' AND EXISTS(SELECT 1 FROM memory_embeddings e WHERE e.memory_id = m.id AND e.version = v.version AND e.model = ?)) AS embedded
		FROM memories m
		JOIN memory_versions v ON v.memory_id = m.id
		JOIN sources src ON src.id = v.source_id
		WHERE m.state <> 'deleted' AND ((? <> '' AND m.id = ?) OR (? = '' AND m.version = v.version))
		AND instr(lower(v.summary || ' ' || v.tags || ' ' || v.conditions || ' ' || m.kind || ' ' || m.id), lower(?)) > 0
		ORDER BY m.reinforced_at DESC, m.id, v.version DESC LIMIT 100`,
		embedModel, embedModel, id, id, id, filter).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]domain.MemoryRecord, len(rows))
	for i, r := range rows {
		var tags []string
		_ = json.Unmarshal([]byte(r.TagsJSON), &tags)
		out[i] = domain.MemoryRecord{
			ID: r.ID, Kind: r.Kind, State: r.State, Enabled: r.Enabled, Version: r.Version,
			SourceID: r.SourceID, Origin: r.Origin, Location: r.Location, Summary: r.Summary,
			Details: r.Details, Tags: tags, Conditions: r.Conditions, ExpiresAt: r.ExpiresAt,
			Strength: r.Strength, ReinforcedAt: r.ReinforcedAt, Embedded: r.Embedded,
		}
	}
	return out, nil
}

func scrubDeletedMemory(tx *gorm.DB, id string) error {
	if err := tx.Exec("DELETE FROM memory_embeddings WHERE memory_id = ?", id).Error; err != nil {
		return err
	}
	if err := tx.Exec("UPDATE memory_versions SET summary = '', details = '', tags = '[]', conditions = '' WHERE memory_id = ?", id).Error; err != nil {
		return err
	}
	if err := tx.Exec(`UPDATE sources SET excerpt = '' WHERE id IN (SELECT source_id FROM memory_versions WHERE memory_id = ?)
		AND NOT EXISTS (SELECT 1 FROM memory_versions v JOIN memories m ON m.id = v.memory_id
			WHERE v.source_id = sources.id AND m.state <> 'deleted')`, id).Error; err != nil {
		return err
	}
	return tx.Exec("UPDATE audit SET summary = '' WHERE reference IN (SELECT reference FROM deliveries WHERE memory_id = ?)", id).Error
}

func splitChunks(content string, maxRunes int) []string {
	content = strings.TrimSpace(content)
	if len([]rune(content)) <= maxRunes {
		return []string{content}
	}
	var chunks []string
	var cur strings.Builder
	for _, para := range strings.Split(content, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if cur.Len() > 0 && len([]rune(cur.String()))+len([]rune(para))+2 > maxRunes {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		if len([]rune(para)) > maxRunes {
			runes := []rune(para)
			for i := 0; i < len(runes); i += maxRunes {
				chunks = append(chunks, string(runes[i:min(len(runes), i+maxRunes)]))
			}
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(para)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

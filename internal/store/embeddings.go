package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"gorm.io/gorm"
)

type unembeddedRow struct {
	MemoryID   string
	Version    int
	Kind       string
	Summary    string
	Conditions string
	Tags       string
}

// EmbeddingStats returns (embeddedCount, totalConfirmedCount) for the currently active Embedding model.
func (s *Store) EmbeddingStats(ctx context.Context) (int, int) {
	var total int
	_ = s.db.WithContext(ctx).Raw(`SELECT count(*) FROM memories WHERE state = 'confirmed' AND enabled = 1`).Row().Scan(&total)
	emb := s.models.Providers().Embedder
	if emb == nil {
		return 0, total
	}
	var embedded int
	_ = s.db.WithContext(ctx).Raw(`SELECT count(*) FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id AND e.version = m.version AND e.model = ?
		WHERE m.state = 'confirmed' AND m.enabled = 1`, emb.ModelID()).Row().Scan(&embedded)
	return embedded, total
}

// EmbedNext backfills vectors for up to 16 active memories missing embeddings for the
// currently active Embedding model (e.g. after configuring or switching models in Web UI).
func (s *Store) EmbedNext(ctx context.Context) error {
	emb := s.models.Providers().Embedder
	if emb == nil {
		return nil
	}
	modelID := emb.ModelID()
	var batch []unembeddedRow
	err := s.db.WithContext(ctx).Raw(`SELECT m.id AS memory_id, m.version, m.kind, v.summary, v.conditions, v.tags
		FROM memories m
		JOIN memory_versions v ON v.memory_id = m.id AND v.version = m.version
		LEFT JOIN memory_embeddings e ON e.memory_id = m.id AND e.version = m.version AND e.model = ?
		WHERE m.state = 'confirmed' AND m.enabled = 1 AND e.memory_id IS NULL
		ORDER BY m.created_at DESC LIMIT 16`, modelID).Scan(&batch).Error
	if err != nil || len(batch) == 0 {
		return err
	}

	texts := make([]string, len(batch))
	for i, row := range batch {
		var tags []string
		_ = json.Unmarshal([]byte(row.Tags), &tags)
		texts[i] = domain.EmbeddingText(row.Kind, row.Summary, row.Conditions, tags)
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	vectors, err := emb.Embed(callCtx, texts)
	cancel()
	if err != nil {
		return err
	}
	if len(vectors) != len(batch) {
		return nil
	}

	nowMS := s.now().UnixMilli()
	return s.Transaction(ctx, func(tx *gorm.DB) error {
		for i, row := range batch {
			// Verify memory was not deleted while embedding was in flight.
			var alive int
			if err := tx.Raw(`SELECT count(*) FROM memories WHERE id = ? AND version = ? AND state = 'confirmed'`,
				row.MemoryID, row.Version).Row().Scan(&alive); err != nil {
				return err
			}
			if alive == 0 {
				continue
			}
			blob := model.EncodeVector(vectors[i])
			if err := tx.Exec(`INSERT INTO memory_embeddings(memory_id, version, model, dims, vector, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(memory_id, version, model) DO UPDATE SET dims = excluded.dims, vector = excluded.vector, updated_at = excluded.updated_at`,
				row.MemoryID, row.Version, modelID, len(vectors[i]), blob, nowMS).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// EmbedMemory eagerly computes and stores the vector for one memory version if an Embedder is active.
func (s *Store) EmbedMemory(ctx context.Context, memoryID string, version int) error {
	emb := s.models.Providers().Embedder
	if emb == nil {
		return nil
	}
	var row unembeddedRow
	err := s.db.WithContext(ctx).Raw(`SELECT m.id AS memory_id, m.version, m.kind, v.summary, v.conditions, v.tags
		FROM memories m JOIN memory_versions v ON v.memory_id = m.id AND v.version = ?
		WHERE m.id = ? AND m.state = 'confirmed'`, version, memoryID).Scan(&row).Error
	if err != nil || row.MemoryID == "" {
		return err
	}
	var tags []string
	_ = json.Unmarshal([]byte(row.Tags), &tags)
	text := domain.EmbeddingText(row.Kind, row.Summary, row.Conditions, tags)

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	vectors, err := emb.Embed(callCtx, []string{text})
	cancel()
	if err != nil || len(vectors) != 1 {
		return err
	}
	blob := model.EncodeVector(vectors[0])
	return s.db.WithContext(ctx).Exec(`INSERT INTO memory_embeddings(memory_id, version, model, dims, vector, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(memory_id, version, model) DO UPDATE SET dims = excluded.dims, vector = excluded.vector, updated_at = excluded.updated_at`,
		memoryID, version, emb.ModelID(), len(vectors[0]), blob, s.now().UnixMilli()).Error
}

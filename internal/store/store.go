package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/policy"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	databaseApplicationID = 0x4d4e4556 // "MNEV"
	databaseUserVersion   = 4
)

// AuditRecord captures the structured payload written to the immutable audit table.
type AuditRecord struct {
	Strategy     string
	Status       string
	Reference    string
	Version      int
	Summary      string
	ModelOutcome string
	ModelMS      int64
	MatchContext string
}

// Store owns the SQLite persistence layer and coordinates vector recall,
// Jev-gated LLM pipelines, and file-backed strategy evolution.
type Store struct {
	db     *gorm.DB
	policy *policy.Manager
	models *model.Registry
	now    func() time.Time
}

// Option customizes Store construction.
type Option func(*Store)

// WithClock overrides the store's clock (for deterministic testing).
func WithClock(fn func() time.Time) Option {
	return func(s *Store) {
		if fn != nil {
			s.now = fn
		}
	}
}

// Open opens or initializes the Mnevo SQLite database at dbPath.
func Open(ctx context.Context, dbPath string, policyMgr *policy.Manager, modelReg *model.Registry, opts ...Option) (*Store, error) {
	if policyMgr == nil {
		return nil, errors.New("policy manager is required")
	}
	if modelReg == nil {
		modelReg = model.NewStaticRegistry(model.Providers{})
	}
	uri := url.URL{
		Scheme:   "file",
		Path:     dbPath,
		RawQuery: "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
	}
	db, err := gorm.Open(
		gormsqlite.New(gormsqlite.Config{DriverName: "sqlite", DSN: uri.String()}),
		&gorm.Config{
			SkipDefaultTransaction: true,
			Logger:                 logger.Default.LogMode(logger.Silent),
		},
	)
	if err != nil {
		return nil, err
	}
	pool, err := db.DB()
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)

	s := &Store{
		db:     db,
		policy: policyMgr,
		models: modelReg,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}

	err = s.Transaction(ctx, func(tx *gorm.DB) error {
		var appID, userVer int
		if err := tx.Raw("PRAGMA application_id").Row().Scan(&appID); err != nil {
			return err
		}
		if err := tx.Raw("PRAGMA user_version").Row().Scan(&userVer); err != nil {
			return err
		}
		if appID == databaseApplicationID && userVer == databaseUserVersion {
			return nil
		}
		var populated bool
		if err := tx.Raw("SELECT EXISTS (SELECT 1 FROM sqlite_schema)").Row().Scan(&populated); err != nil {
			return err
		}
		if appID != 0 || populated {
			return errors.New("database is incompatible with the current schema; use -home with a new directory")
		}
		if err := tx.Exec(schemaSQL).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", databaseApplicationID)).Error; err != nil {
			return err
		}
		return tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", databaseUserVersion)).Error
	})
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying SQLite connection pool.
func (s *Store) Close() error {
	pool, err := s.db.DB()
	if err != nil {
		return err
	}
	return pool.Close()
}

// DB exposes *gorm.DB for controlled inspection in tests.
func (s *Store) DB() *gorm.DB { return s.db }

// Policy returns the file-backed strategy manager.
func (s *Store) Policy() *policy.Manager { return s.policy }

// Models returns the live hot-reloadable model registry.
func (s *Store) Models() *model.Registry { return s.models }

// SetClock updates the clock function (primarily for tests).
func (s *Store) SetClock(fn func() time.Time) { s.now = fn }

// Now returns the current store timestamp.
func (s *Store) Now() time.Time { return s.now() }

// Transaction runs fn inside a SQLite transaction bound to ctx.
func (s *Store) Transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(fn)
}

// ModelConfigView returns the redacted model config enriched with vector indexing progress.
func (s *Store) ModelConfigView(ctx context.Context) domain.ModelConfigView {
	view := s.models.View()
	embedded, total := s.EmbeddingStats(ctx)
	view.EmbeddedMemories = embedded
	view.TotalMemories = total
	return view
}

// Audit appends an immutable decision or mutation record to the audit log.
func (s *Store) Audit(ctx context.Context, db *gorm.DB, operation string, t domain.Task, rec AuditRecord) error {
	if db == nil {
		db = s.db
	}
	if rec.Strategy == "" {
		rec.Strategy = s.policy.Active().Label()
	}
	return db.WithContext(ctx).Exec(`INSERT INTO audit
		(at, operation, session_id, task_id, strategy, trigger_event, result, reference, version, summary, model_outcome, model_ms, match_context)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.now().UnixMilli(), operation, t.SessionID, t.ID, rec.Strategy, t.Event, rec.Status,
		rec.Reference, rec.Version, rec.Summary, rec.ModelOutcome, rec.ModelMS, rec.MatchContext).Error
}

// AuditTrail returns the most recent page of audit entries.
func (s *Store) AuditTrail(ctx context.Context) ([]domain.AuditEntry, error) {
	return s.AuditPage(ctx, 0)
}

// AuditPage returns up to 250 audit entries older than before (or latest if before == 0).
func (s *Store) AuditPage(ctx context.Context, before int64) ([]domain.AuditEntry, error) {
	rows, err := s.db.WithContext(ctx).Raw(`SELECT id, at, operation, session_id, task_id,
		strategy, trigger_event, result, reference, version, summary, model_outcome, model_ms, match_context
		FROM audit WHERE ? = 0 OR id < ? ORDER BY id DESC LIMIT 250`, before, before).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []domain.AuditEntry
	remaining := domain.MaxMessage - 4096
	for rows.Next() {
		var entry domain.AuditEntry
		if err := rows.Scan(
			&entry.ID, &entry.At, &entry.Operation, &entry.SessionID, &entry.TaskID,
			&entry.Strategy, &entry.Trigger, &entry.Result, &entry.Reference,
			&entry.Version, &entry.Summary, &entry.ModelOutcome, &entry.ModelMS, &entry.MatchContext,
		); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		remaining -= len(encoded) + 1
		if remaining < 0 {
			break
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// CompactMaintenance prunes expired operational records while preserving audit logs and summaries.
func (s *Store) CompactMaintenance(ctx context.Context) error {
	pol := s.policy.Active()
	cutoff := s.now().AddDate(0, 0, -int(pol.Value("feedback.retention_days"))).UnixMilli()
	return s.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM feedback_effects WHERE created_at < ?", cutoff).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM feedback WHERE id IN (
			SELECT f.id FROM feedback f JOIN deliveries d ON d.reference = f.reference
			WHERE f.state IN ('recorded','jev_skipped','unconfirmed') AND d.expires < ? LIMIT 500)`, cutoff).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM deliveries WHERE reference IN (
			SELECT d.reference FROM deliveries d
			WHERE d.expires < ? AND NOT EXISTS (SELECT 1 FROM feedback f WHERE f.reference = d.reference)
			LIMIT 500)`, cutoff).Error; err != nil {
			return err
		}
		return tx.Exec(`DELETE FROM formation WHERE id IN (
			SELECT id FROM formation WHERE state <> 'pending' AND received_at < ? LIMIT 500)`, cutoff).Error
	})
}

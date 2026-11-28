package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/coxlong/mnevo/internal/domain"
	"gorm.io/gorm"
)

// BeginTurn starts a fresh task for sessionID and atomically revokes any prior
// turn's active delivery references for that session in SQLite.
func (s *Store) BeginTurn(ctx context.Context, sessionID, event string) (domain.Task, error) {
	if sessionID == "" {
		sessionID = "default"
	}
	task := domain.Task{
		SessionID: sessionID,
		ID:        domain.Token(),
		Event:     event,
	}
	nowMS := s.now().UnixMilli()
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE deliveries SET revoked = 1 WHERE session_id = ? AND revoked = 0`, sessionID).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO sessions(session_id, active_task_id, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET active_task_id = excluded.active_task_id, updated_at = excluded.updated_at`,
			sessionID, task.ID, nowMS).Error
	})
	return task, err
}

// ActiveTask returns the currently active task for sessionID (empty ID if none).
func (s *Store) ActiveTask(ctx context.Context, sessionID, event string) (domain.Task, error) {
	if sessionID == "" {
		return domain.Task{}, nil
	}
	var taskID string
	err := s.db.WithContext(ctx).Raw(`SELECT active_task_id FROM sessions WHERE session_id = ?`, sessionID).
		Row().Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{SessionID: sessionID, Event: event}, nil
	}
	if err != nil {
		return domain.Task{}, err
	}
	return domain.Task{SessionID: sessionID, ID: taskID, Event: event}, nil
}

// EndTurn revokes all active delivery references for sessionID in SQLite and clears its active task.
func (s *Store) EndTurn(ctx context.Context, sessionID, event string) (domain.Task, error) {
	if sessionID == "" {
		return domain.Task{}, nil
	}
	var ended domain.Task
	nowMS := s.now().UnixMilli()
	err := s.Transaction(ctx, func(tx *gorm.DB) error {
		var taskID string
		err := tx.Raw(`SELECT active_task_id FROM sessions WHERE session_id = ?`, sessionID).Row().Scan(&taskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ended = domain.Task{SessionID: sessionID, ID: taskID, Event: event}
		if err := tx.Exec(`UPDATE deliveries SET revoked = 1 WHERE session_id = ? AND revoked = 0`, sessionID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO sessions(session_id, active_task_id, updated_at) VALUES (?, '', ?)
			ON CONFLICT(session_id) DO UPDATE SET active_task_id = '', updated_at = excluded.updated_at`,
			sessionID, nowMS).Error; err != nil {
			return err
		}
		if ended.ID != "" {
			return s.Audit(ctx, tx, event, ended, AuditRecord{Status: "ended"})
		}
		return nil
	})
	return ended, err
}

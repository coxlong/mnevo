package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"gorm.io/gorm"
)

const transcriptTailBytes = 512 << 10

type transcriptRecord struct {
	Type             string `json:"type"`
	SessionID        string `json:"sessionId"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

func (r transcriptRecord) content() (text string, hasTools bool) {
	if json.Unmarshal(r.Message.Content, &text) == nil {
		return strings.TrimSpace(text), false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text := strings.TrimSpace(b.Text); text != "" {
				parts = append(parts, text)
			}
		case "tool_use", "tool_result":
			hasTools = true
		}
	}
	return strings.Join(parts, "\n"), hasTools
}

// previousAssistantReply reads a bounded transcript tail independently of the formation cursor.
func previousAssistantReply(path, sessionID, prompt string) string {
	if path == "" || sessionID == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	start := max(int64(0), info.Size()-transcriptTailBytes)
	tail, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return ""
	}
	if start > 0 {
		_, tail, _ = bytes.Cut(tail, []byte("\n"))
	}
	lines := bytes.Split(tail, []byte("\n"))
	skippedCurrent := false
	for i := len(lines) - 1; i >= 0; i-- {
		var r transcriptRecord
		if json.Unmarshal(lines[i], &r) != nil || r.IsSidechain || r.IsMeta || r.IsCompactSummary ||
			(r.SessionID != "" && r.SessionID != sessionID) {
			continue
		}
		switch r.Type {
		case "assistant":
			if r.Message.StopReason == "tool_use" || r.Message.StopReason == "pause_turn" {
				continue
			}
			if text, hasTools := r.content(); text != "" && !hasTools {
				return domain.TruncateRunes(text, 1500)
			}
		case "user":
			text, hasTools := r.content()
			if text == "" || hasTools {
				continue
			}
			// The submitted prompt may already have been appended when the hook runs.
			if !skippedCurrent && text == strings.TrimSpace(prompt) {
				skippedCurrent = true
				continue
			}
			return ""
		}
	}
	return ""
}

type transcriptCursor struct {
	path   string
	offset int64
}

func loadTranscriptCursor(db *gorm.DB, sessionID string) (transcriptCursor, error) {
	var cursor transcriptCursor
	err := db.Raw("SELECT transcript_path, last_offset FROM transcript_cursors WHERE session_id = ?", sessionID).
		Row().Scan(&cursor.path, &cursor.offset)
	if errors.Is(err, sql.ErrNoRows) {
		return transcriptCursor{}, nil
	}
	return cursor, err
}

// EnqueueTranscriptDelta queues complete transcript records in bounded batches.
// Each batch and its cursor update commit together.
func (s *Store) EnqueueTranscriptDelta(ctx context.Context, sessionID, taskID, path string) (bool, error) {
	if sessionID == "" || strings.TrimSpace(path) == "" {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer file.Close()

	cursor, err := loadTranscriptCursor(s.db.WithContext(ctx), sessionID)
	if err != nil {
		return false, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, err
	}
	task := domain.Task{SessionID: sessionID, ID: taskID}
	queued := false
	for {
		start := cursor.offset
		if cursor.path != path || start > info.Size() {
			start = 0
		}
		text, end, err := readTranscriptBatch(ctx, file, start, info.Size())
		if err != nil || end == start {
			return queued, err
		}

		var committed bool
		var current transcriptCursor
		err = s.Transaction(ctx, func(tx *gorm.DB) error {
			var err error
			current, err = loadTranscriptCursor(tx, sessionID)
			if err != nil || current != cursor {
				return err
			}
			if text != "" {
				id := domain.Digest(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", sessionID, path, start, end, text))
				if err := s.insertFormation(tx, id, task, "transcript", path, text, ""); err != nil {
					return err
				}
			}
			if err := tx.Exec(`INSERT INTO transcript_cursors(session_id, transcript_path, last_offset, updated_at)
				VALUES (?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET
				transcript_path = excluded.transcript_path, last_offset = excluded.last_offset, updated_at = excluded.updated_at`,
				sessionID, path, end, s.now().UnixMilli()).Error; err != nil {
				return err
			}
			committed = true
			return nil
		})
		if err != nil {
			return queued, err
		}
		if !committed {
			if current.path != path || current.offset >= info.Size() {
				return queued, nil
			}
			cursor = current
			continue
		}
		cursor = transcriptCursor{path: path, offset: end}
		queued = queued || text != ""
	}
}

func readTranscriptBatch(ctx context.Context, file *os.File, start, size int64) (string, int64, error) {
	var batch strings.Builder
	var runes int
	end := start
	reader := bufio.NewReader(io.NewSectionReader(file, start, size-start))
	for {
		if err := ctx.Err(); err != nil {
			return "", start, err
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // Leave a partial JSONL record for the next event.
		}
		if err != nil {
			return "", start, err
		}
		if msg := extractTranscriptLine(line); msg != "" {
			length := utf8.RuneCountInString(msg) + 1
			if runes+length > model.MaxExtractionRunes {
				break
			}
			batch.WriteString(msg)
			batch.WriteByte('\n')
			runes += length
		}
		end += int64(len(line))
	}
	return strings.TrimSpace(batch.String()), end, nil
}

func extractTranscriptLine(line []byte) string {
	var rec transcriptRecord
	if json.Unmarshal(line, &rec) != nil {
		return ""
	}
	role := rec.Message.Role
	if role != "user" && role != "assistant" {
		if rec.Type == "user" || rec.Type == "assistant" {
			role = rec.Type
		} else {
			return ""
		}
	}
	text, _ := rec.content()
	if text == "" || strings.HasPrefix(text, "记住：") {
		return ""
	}
	return role + ": " + domain.TruncateRunes(text, 1500)
}

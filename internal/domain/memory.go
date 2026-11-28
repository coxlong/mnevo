package domain

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Memory primary kinds.
const (
	KindFact       = "fact"
	KindEpisode    = "episode"
	KindProcedure  = "procedure"
	KindPreference = "preference"
	KindGuideline  = "guideline"
)

// Memory lifecycle states.
const (
	StateConfirmed = "confirmed"
	StateDisputed  = "disputed"
	StateInvalid   = "invalid"
	StateDeleted   = "deleted"
)

// ValidMemoryKind reports whether kind is one of the five unified memory types.
func ValidMemoryKind(kind string) bool {
	switch kind {
	case KindFact, KindEpisode, KindProcedure, KindPreference, KindGuideline:
		return true
	default:
		return false
	}
}

// MemoryDraft is the structured representation of a memory for creation, extraction, or revision.
type MemoryDraft struct {
	Kind       string   `json:"type"`
	Summary    string   `json:"summary"`
	Details    string   `json:"details,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Conditions string   `json:"conditions"`
	ExpiresAt  int64    `json:"expires_at_ms,omitempty"`
}

// Normalize canonicalizes whitespace and tags and validates length bounds.
func (d *MemoryDraft) Normalize() bool {
	if !ValidMemoryKind(d.Kind) {
		return false
	}
	d.Summary = strings.TrimSpace(d.Summary)
	d.Details = strings.TrimSpace(d.Details)
	d.Conditions = strings.TrimSpace(d.Conditions)

	var ok bool
	if d.Tags, ok = NormalizeTags(d.Tags); !ok {
		return false
	}
	return d.Summary != "" &&
		utf8.RuneCountInString(d.Summary) <= 240 &&
		utf8.RuneCountInString(d.Conditions) <= 480 && len(d.Conditions) <= 1440 &&
		utf8.RuneCountInString(d.Details) <= 4096 && len(d.Details) <= 12288 &&
		d.ExpiresAt >= 0
}

// NormalizeTags lowercases, trims, validates, sorts, and deduplicates tags.
func NormalizeTags(raw []string) ([]string, bool) {
	if len(raw) > 12 {
		return nil, false
	}
	if len(raw) == 0 {
		return []string{}, true
	}
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		t := strings.ToLower(strings.TrimSpace(tag))
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > 48 || len(t) > 128 {
			return nil, false
		}
		out = append(out, t)
	}
	slices.Sort(out)
	return slices.Compact(out), true
}

// MemoryFingerprint returns a canonical deduplication digest for a normalized MemoryDraft.
func MemoryFingerprint(d MemoryDraft) string {
	normSummary := strings.ToLower(strings.Join(strings.Fields(d.Summary), " "))
	normCond := strings.ToLower(strings.Join(strings.Fields(d.Conditions), " "))
	return Digest(d.Kind + "\x00" + normSummary + "\x00" + normCond)
}

// EmbeddingText produces the canonical text fed to the Embedding model for a memory.
func EmbeddingText(kind, summary, conditions string, tags []string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteString(": ")
	b.WriteString(summary)
	if conditions != "" {
		b.WriteString("\n适用条件: ")
		b.WriteString(conditions)
	}
	if len(tags) > 0 {
		b.WriteString("\n标签: ")
		b.WriteString(strings.Join(tags, ", "))
	}
	return b.String()
}

// MemoryRecord is the read projection of a memory version and its provenance.
type MemoryRecord struct {
	ID           string   `json:"id"`
	Kind         string   `json:"type"`
	State        string   `json:"state"`
	Enabled      bool     `json:"enabled"`
	Version      int      `json:"version"`
	SourceID     string   `json:"source_id"`
	Origin       string   `json:"origin"`
	Location     string   `json:"location"`
	Summary      string   `json:"summary"`
	Details      string   `json:"details,omitempty"`
	Tags         []string `json:"tags"`
	Conditions   string   `json:"conditions"`
	ExpiresAt    int64    `json:"expires_at_ms,omitempty"`
	Strength     float64  `json:"strength"`
	ReinforcedAt int64    `json:"reinforced_at_ms"`
	Embedded     bool     `json:"embedded"`
}

// TruncateRunes returns at most maxRunes Unicode code points.
func TruncateRunes(s string, maxRunes int) string {
	for i := range s {
		if maxRunes == 0 {
			return s[:i]
		}
		maxRunes--
	}
	return s
}

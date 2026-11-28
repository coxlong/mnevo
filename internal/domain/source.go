package domain

import "strings"

// SourceRecord is the provenance backing one or more memory versions.
type SourceRecord struct {
	ID        string `json:"id"`
	Origin    string `json:"origin"`
	Location  string `json:"location"`
	Excerpt   string `json:"excerpt,omitempty"`
	CreatedAt int64  `json:"created_at_ms"`
}

// ImportInput carries raw text or document content for Jev-gated LLM memory import.
type ImportInput struct {
	Location string `json:"location"`
	Content  string `json:"content"`
}

// Valid reports whether ImportInput has non-empty location and content within bounds.
func (u ImportInput) Valid() bool {
	loc := strings.TrimSpace(u.Location)
	content := strings.TrimSpace(u.Content)
	return loc != "" && len(loc) <= 256 && content != "" && len(content) <= 128<<10
}

package domain

import "strings"

// Feedback kinds submitted by Agents against delivered memory references.
const (
	FeedbackHelpful       = "helpful"
	FeedbackNotApplicable = "not_applicable"
	FeedbackOutdated      = "outdated"
	FeedbackCorrection    = "correction"
)

// FeedbackInput represents a single idempotent Agent feedback submission.
type FeedbackInput struct {
	Reference string `json:"reference"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Evidence  string `json:"evidence,omitempty"`
	Key       string `json:"idempotency_key"`
}

// Valid reports whether all required fields are well-formed and within bounds.
func (f FeedbackInput) Valid() bool {
	switch f.Kind {
	case FeedbackHelpful, FeedbackNotApplicable, FeedbackOutdated, FeedbackCorrection:
	default:
		return false
	}
	return f.Reference != "" && len(f.Reference) <= 64 &&
		strings.TrimSpace(f.Reason) != "" && len(f.Reason) <= 4096 &&
		strings.TrimSpace(f.Key) != "" && len(f.Key) <= 128 &&
		len(f.Evidence) <= 4096
}

package model

import (
	"context"

	"github.com/coxlong/mnevo/internal/domain"
)

// MaxExtractionRunes bounds one input to the chat model's memory extractor.
const MaxExtractionRunes = 12000

// Default provider endpoints when custom URLs are omitted.
const (
	OpenRouterDecisionURL  = "https://openrouter.ai/api/v1/systemone"
	OpenRouterChatURL      = "https://openrouter.ai/api/v1"
	OpenRouterEmbeddingURL = "https://openrouter.ai/api/v1"
)

// DecisionGate is the unified System-1 (Jev) binary probability gate used across
// recall, auto-extraction/import, feedback verification, and meta-learning diagnosis.
type DecisionGate interface {
	Decide(ctx context.Context, instructions string, state any) (float64, error)
}

// RecallInput separates the current user request from the preceding assistant answer.
type RecallInput struct {
	Prompt         string `json:"user_prompt"`
	PreviousAnswer string `json:"previous_assistant_answer"`
}

type RecallQuery struct {
	Query    string   `json:"query"`
	Keywords []string `json:"keywords"`
}

// RecallRewriter turns a contextual follow-up into a standalone retrieval query.
type RecallRewriter interface {
	RewriteRecall(ctx context.Context, instructions string, input RecallInput) (RecallQuery, error)
}

// Embedder computes L2-normalized semantic vectors for memories and queries.
type Embedder interface {
	ModelID() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// MemoryExtractor distills natural language, documents, or transcripts into structured memories.
type MemoryExtractor interface {
	Extract(ctx context.Context, prompt, text string) ([]domain.MemoryDraft, error)
}

// FeedbackVerifyInput supplies current memory + feedback reason/evidence to the LLM verifier.
type FeedbackVerifyInput struct {
	MemoryType       string   `json:"memory_type"`
	CurrentSummary   string   `json:"current_summary"`
	CurrentDetails   string   `json:"current_details,omitempty"`
	CurrentCondition string   `json:"current_conditions"`
	CurrentTags      []string `json:"current_tags,omitempty"`
	FeedbackKind     string   `json:"feedback_kind"`
	FeedbackReason   string   `json:"feedback_reason"`
	FeedbackEvidence string   `json:"feedback_evidence,omitempty"`
}

// FeedbackVerifyResult is the strict JSON verdict from the LLM verifier.
type FeedbackVerifyResult struct {
	ShouldRevise *bool    `json:"should_revise"`
	Reason       string   `json:"reason"`
	Summary      string   `json:"summary,omitempty"`
	Details      string   `json:"details,omitempty"`
	Conditions   string   `json:"conditions,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// FeedbackVerifier verifies whether feedback requires updating a memory version.
type FeedbackVerifier interface {
	Verify(ctx context.Context, prompt string, input FeedbackVerifyInput) (FeedbackVerifyResult, error)
}

// MetaObservationSample summarizes one audited observation for LLM root-cause diagnosis.
type MetaObservationSample struct {
	ObservationID string `json:"observation_id"`
	TaskID        string `json:"task_id"`
	MemoryID      string `json:"memory_id,omitempty"`
	Evidence      string `json:"evidence"`
	MatchContext  string `json:"match_context,omitempty"`
	ModelOutcome  string `json:"model_outcome,omitempty"`
	Summary       string `json:"summary,omitempty"`
}

// MetaDiagnosisInput feeds verified diagnostic samples + learnable policy subset to the LLM.
type MetaDiagnosisInput struct {
	ProblemKind       string                          `json:"problem_kind"`
	PolicyVersion     int                             `json:"policy_version"`
	Samples           []MetaObservationSample         `json:"samples"`
	LearnableSettings map[string]domain.Setting       `json:"learnable_settings"`
	LearnablePrompts  map[string]domain.PromptSetting `json:"learnable_prompts"`
}

// MetaDiagnosisResult is the structured root-cause proposal from the LLM diagnoser.
type MetaDiagnosisResult struct {
	TargetType  string  `json:"target_type"` // "setting" | "prompt" | "advice"
	TargetKey   string  `json:"target_key"`
	AfterValue  float64 `json:"after_value,omitempty"`
	AfterPrompt string  `json:"after_prompt,omitempty"`
	Diagnosis   string  `json:"diagnosis"`
	Impact      string  `json:"impact"`
	Risk        string  `json:"risk"`
}

// MetaDiagnoser analyzes batches of verified diagnostic evidence to propose strategy fixes.
type MetaDiagnoser interface {
	Diagnose(ctx context.Context, prompt string, input MetaDiagnosisInput) (MetaDiagnosisResult, error)
}

// Providers holds the active set of configured external model clients.
type Providers struct {
	Embedder  Embedder
	Gate      DecisionGate
	Rewriter  RecallRewriter
	Extractor MemoryExtractor
	Verifier  FeedbackVerifier
	Diagnoser MetaDiagnoser
}

// RecallReady reports whether both Embedding and Jev are configured for recall.
func (p Providers) RecallReady() bool {
	return p.Embedder != nil && p.Gate != nil
}

// AutoExtractReady reports whether Jev, LLM, and Embedding are all configured.
func (p Providers) AutoExtractReady() bool {
	return p.Gate != nil && p.Extractor != nil && p.Embedder != nil
}

package domain

import "time"

// Service-wide runtime budgets and wire limits.
const (
	DefaultStrategy    = "settings-v1"
	RecallBudget       = 600 * time.Millisecond
	IPCBudget          = 3 * time.Second
	LongOpBudget       = 60 * time.Second
	ReferenceTTL       = 30 * time.Minute
	DecisionBudget     = 220 * time.Millisecond
	VerificationBudget = 10 * time.Second
	MaxMessage         = 256 << 10
)

// Task identifies the execution context of an Agent turn.
type Task struct {
	SessionID string
	ID        string
	Event     string
}

// HookRequest is sent by `mnevo hook` to POST /v1/hooks.
type HookRequest struct {
	SessionID      string `json:"session_id"`
	Event          string `json:"event"`
	Prompt         string `json:"prompt,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// DeliveredMemory is a single memory item injected into an Agent turn.
type DeliveredMemory struct {
	Kind       string   `json:"type"`
	Layer      string   `json:"layer"`
	Summary    string   `json:"summary"`
	Conditions string   `json:"conditions"`
	Tags       []string `json:"tags,omitempty"`
	Reference  string   `json:"reference"`
	Version    int      `json:"version"`
}

// RecallResult is the response from recall and POST /v1/hooks.
type RecallResult struct {
	Status       string            `json:"status"`
	Strategy     string            `json:"strategy,omitempty"`
	Receipt      string            `json:"receipt,omitempty"`
	Recalls      []DeliveredMemory `json:"recalls,omitempty"`
	ModelOutcome string            `json:"model_outcome,omitempty"`
	ModelMS      int64             `json:"model_ms,omitempty"`
}

// MemoryDetail is returned by GET /v1/references/{ref} (MCP get_memory).
type MemoryDetail struct {
	Status     string    `json:"status"`
	Reference  string    `json:"reference,omitempty"`
	Version    int       `json:"version,omitempty"`
	Kind       string    `json:"type,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Details    string    `json:"details,omitempty"`
	Conditions string    `json:"conditions,omitempty"`
	Strategy   string    `json:"strategy,omitempty"`
	Evidence   *Evidence `json:"evidence,omitempty"`
}

// Evidence identifies the provenance backing a memory version.
type Evidence struct {
	ID       string `json:"id"`
	Origin   string `json:"origin"`
	Location string `json:"location"`
	Excerpt  string `json:"excerpt"`
}

// ReceiptResult is returned by async enqueue endpoints (feedback, propose_memory).
type ReceiptResult struct {
	Status  string `json:"status"`
	Receipt string `json:"receipt,omitempty"`
	Message string `json:"message,omitempty"`
}

// ImportResult is returned by POST /v1/memories/import.
type ImportResult struct {
	Status   string         `json:"status"`
	Message  string         `json:"message,omitempty"`
	Count    int            `json:"count,omitempty"`
	ModelMS  int64          `json:"model_ms,omitempty"`
	Memories []MemoryRecord `json:"memories,omitempty"`
}

// HealthStatus is returned by GET /v1/health.
type HealthStatus struct {
	Status           string `json:"status"`
	Version          string `json:"version"`
	PID              int    `json:"pid"`
	HomeDir          string `json:"home_dir"`
	WebURL           string `json:"web_url,omitempty"`
	PublicURL        string `json:"public_url,omitempty"`
	RecallReady      bool   `json:"recall_ready"`
	AutoExtractReady bool   `json:"auto_extract_ready"`
	EmbeddedMemories int    `json:"embedded_memories"`
	TotalMemories    int    `json:"total_memories"`
}

// AuditEntry records an auditable decision or mutation in Mnevo.
type AuditEntry struct {
	ID           int64  `json:"id"`
	ModelMS      int64  `json:"model_ms,omitempty"`
	MatchContext string `json:"match_context,omitempty"`
	At           int64  `json:"at_ms"`
	Operation    string `json:"operation"`
	SessionID    string `json:"session_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	Strategy     string `json:"strategy"`
	Trigger      string `json:"trigger_event,omitempty"`
	Result       string `json:"result"`
	Reference    string `json:"reference,omitempty"`
	Version      int    `json:"version,omitempty"`
	Summary      string `json:"summary,omitempty"`
	ModelOutcome string `json:"model_outcome,omitempty"`
}

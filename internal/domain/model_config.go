package domain

// ModelEndpointView is the redacted view of a model provider safe for Web/CLI display.
// Raw API keys are never returned after saving.
type ModelEndpointView struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	Model     string `json:"model"`
	HasKey    bool   `json:"has_key"`
	KeyMasked string `json:"key_masked,omitempty"`
}

// ModelEndpointInput carries an admin update for a single model provider.
// Leaving APIKey empty (with ClearKey=false) preserves any previously saved key.
type ModelEndpointInput struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	ClearKey bool   `json:"clear_key,omitempty"`
}

// WebConfig holds optional public/reverse-proxy access settings for the management Web UI.
type WebConfig struct {
	PublicURL string `json:"public_url,omitempty"`
}

// ModelConfigView summarizes all runtime model providers, web entry config, and vector indexing progress.
type ModelConfigView struct {
	Web              WebConfig         `json:"web,omitempty"`
	Embedding        ModelEndpointView `json:"embedding"`
	Jev              ModelEndpointView `json:"jev"`
	LLM              ModelEndpointView `json:"llm"`
	EmbeddedMemories int               `json:"embedded_memories"`
	TotalMemories    int               `json:"total_memories"`
	RecallReady      bool              `json:"recall_ready"`
	AutoExtractReady bool              `json:"auto_extract_ready"`
}

package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
)

// EndpointConfig stores persisted settings for one external model endpoint.
type EndpointConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key,omitempty"`
}

// RuntimeConfig holds all environment/connection settings persisted in ~/.mnevo/config.json (0600).
type RuntimeConfig struct {
	Web       domain.WebConfig `json:"web,omitempty"`
	Embedding EndpointConfig   `json:"embedding"`
	Jev       EndpointConfig   `json:"jev"`
	LLM       EndpointConfig   `json:"llm"`
}

// Registry provides thread-safe, hot-reloadable access to active external model providers
// and optional Web public URL configuration.
type Registry struct {
	mu         sync.RWMutex
	configPath string
	httpClient *http.Client
	cfg        RuntimeConfig
	providers  Providers
	lastMod    time.Time
	lastSize   int64
}

// NewRegistry initializes a Registry from environment variables, overlaid by config.json if present.
func NewRegistry(configPath string, httpClient *http.Client) (*Registry, error) {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	cfg := configFromEnv()
	var lastMod time.Time
	var lastSize int64
	if configPath != "" {
		if info, statErr := os.Stat(configPath); statErr == nil {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return nil, err
			}
			var persisted RuntimeConfig
			if err := json.Unmarshal(data, &persisted); err != nil {
				return nil, fmt.Errorf("parse config.json: %w", err)
			}
			if persisted.Web.PublicURL != "" {
				norm, err := ValidatePublicURL(persisted.Web.PublicURL)
				if err != nil {
					return nil, fmt.Errorf("config.json web.public_url: %w", err)
				}
				persisted.Web.PublicURL = norm
			}
			cfg = persisted
			lastMod = info.ModTime()
			lastSize = info.Size()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	}
	providers, err := buildProviders(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	return &Registry{
		configPath: configPath,
		httpClient: httpClient,
		cfg:        cfg,
		providers:  providers,
		lastMod:    lastMod,
		lastSize:   lastSize,
	}, nil
}

// NewStaticRegistry builds an in-memory Registry for tests.
func NewStaticRegistry(p Providers) *Registry {
	return &Registry{
		httpClient: defaultHTTPClient(),
		providers:  p,
	}
}

func (r *Registry) reloadIfChanged() {
	if r == nil || r.configPath == "" {
		return
	}
	info, err := os.Stat(r.configPath)
	if err != nil {
		return
	}
	r.mu.RLock()
	unchanged := info.ModTime().Equal(r.lastMod) && info.Size() == r.lastSize
	r.mu.RUnlock()
	if unchanged {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	info, err = os.Stat(r.configPath)
	if err != nil || (info.ModTime().Equal(r.lastMod) && info.Size() == r.lastSize) {
		return
	}
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		return
	}
	var disk RuntimeConfig
	if err := json.Unmarshal(data, &disk); err != nil {
		return
	}
	normPub, err := ValidatePublicURL(disk.Web.PublicURL)
	if err != nil {
		return
	}
	disk.Web.PublicURL = normPub
	providers, err := buildProviders(disk, r.httpClient)
	if err != nil {
		return
	}
	r.cfg = disk
	r.providers = providers
	r.lastMod = info.ModTime()
	r.lastSize = info.Size()
}

// Providers returns a thread-safe snapshot of currently active model clients.
func (r *Registry) Providers() Providers {
	if r == nil {
		return Providers{}
	}
	r.reloadIfChanged()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers
}

// PublicURL returns the configured external Web UI URL (empty if loopback-only).
func (r *Registry) PublicURL() string {
	if r == nil {
		return ""
	}
	r.reloadIfChanged()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Web.PublicURL
}

// UpdateWebConfig validates, persists (mode 0600), and hot-reloads web.public_url.
func (r *Registry) UpdateWebConfig(in domain.WebConfig) (domain.WebConfig, error) {
	norm, err := ValidatePublicURL(in.PublicURL)
	if err != nil {
		return domain.WebConfig{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.cfg
	next.Web = domain.WebConfig{PublicURL: norm}
	if err := r.saveLocked(next); err != nil {
		return domain.WebConfig{}, err
	}
	r.cfg = next
	return next.Web, nil
}

// SetProviders mutates active providers in memory (primarily for tests).
func (r *Registry) SetProviders(fn func(*Providers)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.providers)
}

// View returns a redacted summary of all model endpoints and web config safe for Web UI and CLI output.
func (r *Registry) View() domain.ModelConfigView {
	if r == nil {
		return domain.ModelConfigView{}
	}
	r.reloadIfChanged()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return domain.ModelConfigView{
		Web:              r.cfg.Web,
		Embedding:        redactEndpoint(r.cfg.Embedding, OpenRouterEmbeddingURL),
		Jev:              redactEndpoint(r.cfg.Jev, OpenRouterDecisionURL),
		LLM:              redactEndpoint(r.cfg.LLM, OpenRouterChatURL),
		RecallReady:      r.providers.RecallReady(),
		AutoExtractReady: r.providers.AutoExtractReady(),
	}
}

// UpdateEndpoint validates, persists (mode 0600), and hot-reloads one provider ("embedding", "jev", or "llm").
func (r *Registry) UpdateEndpoint(kind string, in domain.ModelEndpointInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := r.cfg
	var target *EndpointConfig
	var defaultURL string
	switch kind {
	case "embedding":
		target, defaultURL = &next.Embedding, OpenRouterEmbeddingURL
	case "jev":
		target, defaultURL = &next.Jev, OpenRouterDecisionURL
	case "llm":
		target, defaultURL = &next.LLM, OpenRouterChatURL
	default:
		return fmt.Errorf("unknown model role %q", kind)
	}

	previousURL := target.URL
	target.Enabled = in.Enabled
	target.Model = strings.TrimSpace(in.Model)
	target.URL = strings.TrimSpace(in.URL)
	if target.URL == "" && target.Enabled {
		target.URL = defaultURL
	}
	newKey := strings.TrimSpace(in.APIKey)
	switch {
	case in.ClearKey:
		target.APIKey = ""
	case newKey != "":
		target.APIKey = newKey
	case target.APIKey != "" && effectiveEndpointURL(previousURL, defaultURL) != effectiveEndpointURL(target.URL, defaultURL):
		return errors.New("更换模型地址时，请输入该地址的 API Key 或清除已保存的密钥")
	}

	providers, err := buildProviders(next, r.httpClient)
	if err != nil {
		return err
	}
	if err := r.saveLocked(next); err != nil {
		return err
	}
	r.cfg = next
	r.providers = providers
	return nil
}

func effectiveEndpointURL(configured, fallback string) string {
	if configured == "" {
		return fallback
	}
	return configured
}

// TestConnection performs a minimal live round-trip against the configured provider for kind.
func (r *Registry) TestConnection(ctx context.Context, kind string) (int64, error) {
	p := r.Providers()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now()
	switch kind {
	case "embedding":
		if p.Embedder == nil {
			return 0, errors.New("Embedding model is not configured")
		}
		vecs, err := p.Embedder.Embed(callCtx, []string{"mnevo connectivity check"})
		if err == nil && (len(vecs) != 1 || len(vecs[0]) == 0) {
			err = errors.New("empty embedding vector returned")
		}
		return max(1, time.Since(started).Milliseconds()), err
	case "jev":
		if p.Gate == nil {
			return 0, errors.New("System One / Jev is not configured")
		}
		_, err := p.Gate.Decide(callCtx, domain.DefaultPrompts()["jev_recall"].Value, map[string]string{
			"task":    "connectivity check",
			"summary": "Connectivity ping for Mnevo Jev provider.",
		})
		return max(1, time.Since(started).Milliseconds()), err
	case "llm":
		if p.Extractor == nil {
			return 0, errors.New("LLM is not configured")
		}
		_, err := p.Extractor.Extract(callCtx, domain.DefaultPrompts()["memory_extract"].Value,
			"Always put concise conclusions at the top of weekly reports.")
		return max(1, time.Since(started).Milliseconds()), err
	default:
		return 0, fmt.Errorf("unknown model role %q", kind)
	}
}

func (r *Registry) saveLocked(cfg RuntimeConfig) error {
	if r.configPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := r.configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	target := filepath.Clean(r.configPath)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if info, err := os.Stat(target); err == nil {
		r.lastMod = info.ModTime()
		r.lastSize = info.Size()
	}
	return nil
}

func configFromEnv() RuntimeConfig {
	var cfg RuntimeConfig
	if raw := strings.TrimSpace(os.Getenv("MNEVO_WEB_PUBLIC_URL")); raw != "" {
		if norm, err := ValidatePublicURL(raw); err == nil {
			cfg.Web.PublicURL = norm
		}
	}
	if m := strings.TrimSpace(os.Getenv("MNEVO_EMBED_MODEL")); m != "" {
		u := strings.TrimSpace(os.Getenv("MNEVO_EMBED_URL"))
		if u == "" {
			u = OpenRouterEmbeddingURL
		}
		k := strings.TrimSpace(os.Getenv("MNEVO_EMBED_KEY"))
		if parsed, err := ValidateEndpoint(u); err == nil && k == "" && parsed.Host == "openrouter.ai" {
			k = os.Getenv("OPENROUTER_API_KEY")
		}
		cfg.Embedding = EndpointConfig{Enabled: true, URL: u, Model: m, APIKey: k}
	}
	if m := strings.TrimSpace(os.Getenv("MNEVO_JEV_MODEL")); m != "" {
		u := strings.TrimSpace(os.Getenv("MNEVO_JEV_URL"))
		if u == "" {
			u = OpenRouterDecisionURL
		}
		k := strings.TrimSpace(os.Getenv("MNEVO_JEV_KEY"))
		if parsed, err := ValidateEndpoint(u); err == nil && k == "" {
			switch parsed.Host {
			case "openrouter.ai":
				k = os.Getenv("OPENROUTER_API_KEY")
			case "api.typesafe.ai":
				k = os.Getenv("TYPESAFE_API_KEY")
			}
		}
		cfg.Jev = EndpointConfig{Enabled: true, URL: u, Model: m, APIKey: k}
	}
	if m := strings.TrimSpace(os.Getenv("MNEVO_LLM_MODEL")); m != "" {
		u := strings.TrimSpace(os.Getenv("MNEVO_LLM_URL"))
		if u == "" {
			u = OpenRouterChatURL
		}
		k := strings.TrimSpace(os.Getenv("MNEVO_LLM_KEY"))
		if parsed, err := ValidateEndpoint(u); err == nil && k == "" && parsed.Host == "openrouter.ai" {
			k = os.Getenv("OPENROUTER_API_KEY")
		}
		cfg.LLM = EndpointConfig{Enabled: true, URL: u, Model: m, APIKey: k}
	}
	return cfg
}

func buildProviders(cfg RuntimeConfig, httpClient *http.Client) (Providers, error) {
	var p Providers

	if cfg.Embedding.Enabled {
		if cfg.Embedding.Model == "" {
			return Providers{}, errors.New("Embedding requires a model name when enabled")
		}
		u := effectiveEndpointURL(cfg.Embedding.URL, OpenRouterEmbeddingURL)
		parsed, err := ValidateEndpoint(u)
		if err != nil {
			return Providers{}, fmt.Errorf("embedding url: %w", err)
		}
		if cfg.Embedding.APIKey == "" && parsed.Host == "openrouter.ai" {
			return Providers{}, errors.New("configured OpenRouter Embedding requires an API key")
		}
		p.Embedder = NewOpenAIEmbedder(httpClient, u, cfg.Embedding.Model, cfg.Embedding.APIKey)
	}

	if cfg.Jev.Enabled {
		if cfg.Jev.Model == "" {
			return Providers{}, errors.New("System One / Jev requires a model name when enabled")
		}
		u := effectiveEndpointURL(cfg.Jev.URL, OpenRouterDecisionURL)
		parsed, err := ValidateEndpoint(u)
		if err != nil {
			return Providers{}, fmt.Errorf("jev url: %w", err)
		}
		if cfg.Jev.APIKey == "" && (parsed.Host == "openrouter.ai" || parsed.Host == "api.typesafe.ai") {
			return Providers{}, errors.New("configured System One provider requires an API key")
		}
		p.Gate = NewSystemOne(httpClient, u, cfg.Jev.Model, cfg.Jev.APIKey)
	}

	if cfg.LLM.Enabled {
		if cfg.LLM.Model == "" {
			return Providers{}, errors.New("LLM requires a model name when enabled")
		}
		u := effectiveEndpointURL(cfg.LLM.URL, OpenRouterChatURL)
		parsed, err := ValidateEndpoint(u)
		if err != nil {
			return Providers{}, fmt.Errorf("llm url: %w", err)
		}
		if cfg.LLM.APIKey == "" && parsed.Host == "openrouter.ai" {
			return Providers{}, errors.New("configured OpenRouter LLM requires an API key")
		}
		chat := NewOpenAIChat(httpClient, u, cfg.LLM.Model, cfg.LLM.APIKey)
		p.Rewriter = chat
		p.Extractor = chat
		p.Verifier = chat
		p.Diagnoser = chat
	}

	return p, nil
}

func redactEndpoint(ep EndpointConfig, fallbackURL string) domain.ModelEndpointView {
	view := domain.ModelEndpointView{
		Enabled: ep.Enabled,
		URL:     effectiveEndpointURL(ep.URL, fallbackURL),
		Model:   ep.Model,
		HasKey:  ep.APIKey != "",
	}
	if ep.APIKey != "" {
		if len(ep.APIKey) <= 8 {
			view.KeyMasked = "****"
		} else {
			view.KeyMasked = ep.APIKey[:4] + "…" + ep.APIKey[len(ep.APIKey)-4:]
		}
	}
	return view
}

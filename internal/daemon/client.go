package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
)

// APIError represents a non-2xx JSON response returned by the local Mnevo HTTP API.
type APIError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" && e.Status != "" {
		return fmt.Sprintf("%s: %s", e.Status, e.Message)
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Status != "" {
		return e.Status
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// Client talks to the local Mnevo daemon over HTTP/1.1 on ~/.mnevo/mnevo.sock
// with automatic HTTP Keep-Alive connection reuse and optional on-demand auto-start.
type Client struct {
	homeDir     string
	socketPath  string
	useAdminKey bool
	autoStart   bool
	mu          sync.RWMutex
	adminKey    string
	transport   *http.Transport
	httpClient  *http.Client
}

// NewClient constructs a least-privilege Agent client (never sends X-Mnevo-Key).
func NewClient(homeDir string, autoStart bool) *Client {
	return newClient(homeDir, "", false, autoStart)
}

// NewAdminClient constructs an administrative CLI client that authenticates with admin.key.
func NewAdminClient(homeDir, adminKey string, autoStart bool) *Client {
	return newClient(homeDir, adminKey, true, autoStart)
}

func newClient(homeDir, adminKey string, useAdminKey, autoStart bool) *Client {
	socketPath := filepath.Join(homeDir, "mnevo.sock")
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{
		homeDir:     homeDir,
		socketPath:  socketPath,
		useAdminKey: useAdminKey,
		autoStart:   autoStart,
		adminKey:    adminKey,
		transport:   transport,
		httpClient:  &http.Client{Transport: transport},
	}
}

// Close releases idle Unix-socket HTTP connections.
func (c *Client) Close() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// Health queries GET /v1/health without auto-starting the daemon.
func (c *Client) Health(ctx context.Context) (domain.HealthStatus, error) {
	var out domain.HealthStatus
	err := c.doRaw(ctx, http.MethodGet, "/v1/health", nil, &out)
	return out, err
}

// EnsureRunning verifies the daemon is reachable; if not and autoStart is enabled,
// spawns a detached background `mnevo -home <dir> serve -web 127.0.0.1:8765 -web-fallback`
// process and waits briefly for readiness.
func (c *Client) EnsureRunning(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	_, err := c.Health(probeCtx)
	cancel()
	if err == nil {
		c.refreshAdminKey()
		return nil
	}
	if !c.autoStart {
		return err
	}
	if err := spawnDetachedDaemon(c.homeDir); err != nil {
		return err
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pCtx, pCancel := context.WithTimeout(ctx, 150*time.Millisecond)
		_, hErr := c.Health(pCtx)
		pCancel()
		if hErr == nil {
			c.refreshAdminKey()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	return errors.New("timed out waiting for background mnevo daemon")
}

func (c *Client) refreshAdminKey() {
	if !c.useAdminKey {
		return
	}
	c.mu.RLock()
	hasKey := c.adminKey != ""
	c.mu.RUnlock()
	if hasKey {
		return
	}
	keyPath := filepath.Join(c.homeDir, "admin.key")
	if data, err := os.ReadFile(keyPath); err == nil {
		trimmed := strings.TrimSpace(string(data))
		if len(trimmed) == 64 {
			c.mu.Lock()
			c.adminKey = trimmed
			c.mu.Unlock()
		}
	}
}

// DoJSON sends an HTTP JSON request over the Unix socket with a single round-trip
// on the warm path and automatic lazy daemon startup on dial failure when autoStart is enabled.
func (c *Client) DoJSON(ctx context.Context, method, path string, body any, dst any) error {
	return c.DoJSONWithBudget(ctx, 0, method, path, body, dst)
}

// DoJSONWithBudget executes an HTTP JSON call with a per-attempt timeout budget while giving
// cold-start daemon spawning its own bounded readiness window.
func (c *Client) DoJSONWithBudget(ctx context.Context, budget time.Duration, method, path string, body any, dst any) error {
	runAttempt := func() error {
		attemptCtx := ctx
		var cancel context.CancelFunc
		if budget > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, budget)
			defer cancel()
		}
		return c.doRaw(attemptCtx, method, path, body, dst)
	}

	err := runAttempt()
	if err == nil || !c.autoStart || !isDialError(err) {
		return err
	}
	if startErr := c.EnsureRunning(ctx); startErr != nil {
		return startErr
	}
	return runAttempt()
}

func isDialError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, os.ErrNotExist)
}

func (c *Client) doRaw(ctx context.Context, method, path string, body any, dst any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://mnevo"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.useAdminKey {
		c.mu.RLock()
		key := c.adminKey
		c.mu.RUnlock()
		if key != "" {
			req.Header.Set("X-Mnevo-Key", key)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, domain.MaxMessage+1))
	if err != nil {
		return err
	}
	if len(raw) > domain.MaxMessage {
		return errors.New("response exceeds maximum message size")
	}
	if resp.StatusCode >= 400 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		var errBody struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &errBody) == nil {
			apiErr.Status = errBody.Status
			apiErr.Message = errBody.Message
		}
		return apiErr
	}
	if dst != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("HTTP %d decode: %w", resp.StatusCode, err)
		}
	}
	return nil
}

// --- Typed high-level client helpers used by Hook, MCP, and CLI ---

func (c *Client) PostHook(ctx context.Context, req domain.HookRequest) (domain.RecallResult, error) {
	var out domain.RecallResult
	err := c.DoJSONWithBudget(ctx, domain.RecallBudget, http.MethodPost, "/v1/hooks", req, &out)
	return out, err
}

func (c *Client) GetReference(ctx context.Context, ref string) (domain.MemoryDetail, error) {
	var out domain.MemoryDetail
	err := c.DoJSONWithBudget(ctx, domain.IPCBudget, http.MethodGet, "/v1/references/"+url.PathEscape(ref), nil, &out)
	return out, err
}

func (c *Client) SubmitFeedback(ctx context.Context, in domain.FeedbackInput) (domain.ReceiptResult, error) {
	var out domain.ReceiptResult
	err := c.DoJSONWithBudget(ctx, domain.IPCBudget, http.MethodPost, "/v1/references/"+url.PathEscape(in.Reference)+"/feedback", in, &out)
	return out, err
}

func (c *Client) ProposeMemory(ctx context.Context, d domain.MemoryDraft) (domain.ReceiptResult, error) {
	var out domain.ReceiptResult
	err := c.DoJSONWithBudget(ctx, domain.IPCBudget, http.MethodPost, "/v1/memories/propose", d, &out)
	return out, err
}

func (c *Client) ReportMiss(ctx context.Context, evidence string) (domain.ReceiptResult, error) {
	var out domain.ReceiptResult
	err := c.DoJSONWithBudget(ctx, domain.IPCBudget, http.MethodPost, "/v1/observations/miss", map[string]string{"evidence": evidence}, &out)
	return out, err
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.doRaw(ctx, http.MethodPost, "/v1/shutdown", nil, nil)
}

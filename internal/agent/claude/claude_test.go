package claude_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/mnevo/internal/agent/claude"
	"github.com/coxlong/mnevo/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stubHookClient struct {
	fn func(context.Context, domain.HookRequest) (domain.RecallResult, error)
}

func (s stubHookClient) PostHook(ctx context.Context, req domain.HookRequest) (domain.RecallResult, error) {
	return s.fn(ctx, req)
}

type stubMCPClient struct {
	getRef   func(context.Context, string) (domain.MemoryDetail, error)
	feedback func(context.Context, domain.FeedbackInput) (domain.ReceiptResult, error)
}

func (s stubMCPClient) GetReference(ctx context.Context, ref string) (domain.MemoryDetail, error) {
	if s.getRef != nil {
		return s.getRef(ctx, ref)
	}
	return domain.MemoryDetail{Status: "unavailable"}, nil
}
func (s stubMCPClient) SubmitFeedback(ctx context.Context, in domain.FeedbackInput) (domain.ReceiptResult, error) {
	if s.feedback != nil {
		return s.feedback(ctx, in)
	}
	return domain.ReceiptResult{Status: "unavailable"}, nil
}
func TestSetupAndUninstallIdempotentAndPreserveUserConfig(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), "claude-cfg")
	homeDir := filepath.Join(t.TempDir(), "my mnevo home")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Pre-populate existing user settings and MCP server.
	settingsPath := filepath.Join(claudeDir, "settings.json")
	mcpPath := filepath.Join(claudeDir, ".claude.json")
	_ = os.WriteFile(settingsPath, []byte(`{
  "model": "claude-sonnet",
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "/usr/local/bin/other-hook"}]}
    ],
    "PostToolBatch": [
      {"matcher": "Read", "hooks": [
        {"type": "command", "command": "/opt/v1/mnevo hook"},
        {"type": "command", "command": "/usr/local/bin/other-batch-hook"}
      ]}
    ]
  }
}`), 0600)
	_ = os.WriteFile(mcpPath, []byte(`{
  "theme": "dark",
  "mcpServers": {
    "github": {"type": "stdio", "command": "gh-mcp"}
  }
}`), 0600)

	// Setup twice (simulating binary move) -> idempotent, zero duplicate hook entries.
	if _, err := claude.Setup("/opt/v1/mnevo", homeDir, claudeDir); err != nil {
		t.Fatalf("Setup v1: %v", err)
	}
	res, err := claude.Setup("/opt/v2/mnevo", homeDir, claudeDir)
	if err != nil || res.Status != "ok" {
		t.Fatalf("Setup v2 = %+v, %v", res, err)
	}

	rawSettings, _ := os.ReadFile(settingsPath)
	var settings map[string]any
	if err := json.Unmarshal(rawSettings, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["model"] != "claude-sonnet" {
		t.Fatalf("user setting lost: %+v", settings)
	}
	ups := settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if len(ups) != 2 {
		t.Fatalf("expected 1 user hook + 1 mnevo hook (idempotent), got %d: %s", len(ups), string(rawSettings))
	}
	if !strings.Contains(string(rawSettings), "/usr/local/bin/other-batch-hook") {
		t.Fatalf("user's tool hook was removed with the retired Mnevo hook: %s", rawSettings)
	}
	if !strings.Contains(string(rawSettings), "/opt/v2/mnevo") || strings.Contains(string(rawSettings), "/opt/v1/mnevo") {
		t.Fatalf("expected updated v2 path in settings.json: %s", string(rawSettings))
	}
	if !strings.Contains(string(rawSettings), "'"+homeDir+"'") {
		t.Fatalf("expected space-containing homeDir quoted: %s", string(rawSettings))
	}

	rawMCP, _ := os.ReadFile(mcpPath)
	var mcpDoc map[string]any
	if err := json.Unmarshal(rawMCP, &mcpDoc); err != nil {
		t.Fatal(err)
	}
	servers := mcpDoc["mcpServers"].(map[string]any)
	if mcpDoc["theme"] != "dark" || servers["github"] == nil || servers["mnevo"] == nil {
		t.Fatalf("unexpected .claude.json: %s", string(rawMCP))
	}

	// Uninstall removes ONLY Mnevo entries, preserving user hooks & github MCP server.
	if _, err := claude.Uninstall(claudeDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	rawSettings, _ = os.ReadFile(settingsPath)
	if strings.Contains(string(rawSettings), "mnevo") || !strings.Contains(string(rawSettings), "other-hook") {
		t.Fatalf("settings.json after uninstall: %s", string(rawSettings))
	}
	rawMCP, _ = os.ReadFile(mcpPath)
	if strings.Contains(string(rawMCP), "mnevo") || !strings.Contains(string(rawMCP), "github") {
		t.Fatalf(".claude.json after uninstall: %s", string(rawMCP))
	}
}

func TestHookTranscriptForwardingPostToolSkipAndFailOpen(t *testing.T) {
	var seen domain.HookRequest
	calls := 0
	client := stubHookClient{fn: func(_ context.Context, r domain.HookRequest) (domain.RecallResult, error) {
		calls++
		seen = r
		if r.Event == "Stop" {
			return domain.RecallResult{Status: "ok"}, nil
		}
		return domain.RecallResult{
			Status: "delivered",
			Recalls: []domain.DeliveredMemory{{
				Kind: "guideline", Layer: "summary",
				Summary: "Go 接口首参传 context", Conditions: "Go 开发", Reference: strings.Repeat("a", 64),
			}},
		}, nil
	}}

	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop","transcript_path":"/tmp/t.jsonl"}`)
	if err := claude.RunHook(t.Context(), client, in, &out); err != nil {
		t.Fatal(err)
	}
	if seen.Event != "Stop" || seen.TranscriptPath != "/tmp/t.jsonl" {
		t.Fatalf("unexpected Stop request: %+v", seen)
	}

	// A stale tool hook from an earlier setup must never contact the daemon.
	out.Reset()
	in = strings.NewReader(`{"session_id":"s1","hook_event_name":"PostToolBatch","tool_calls":[{"tool_name":"mcp__mnevo__get_memory","tool_response":{"text":"hello"}}]}`)
	if err := claude.RunHook(t.Context(), client, in, &out); err != nil || calls != 1 || out.Len() != 0 {
		t.Fatalf("expected mcp__mnevo__* skipped, calls=%d out=%q err=%v", calls, out.String(), err)
	}

	out.Reset()
	in = strings.NewReader(`{"session_id":"s1","hook_event_name":"PostToolBatch","tool_calls":[{"tool_name":"Read","tool_response":{"stdout":"context timeout"}}]}`)
	if err := claude.RunHook(t.Context(), client, in, &out); err != nil || calls != 1 || out.Len() != 0 {
		t.Fatalf("expected tool batch skipped, calls=%d out=%q err=%v", calls, out.String(), err)
	}

	in = strings.NewReader(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"继续","transcript_path":"/tmp/t.jsonl"}`)
	if err := claude.RunHook(t.Context(), client, in, &out); err != nil {
		t.Fatal(err)
	}
	if seen.TranscriptPath != "/tmp/t.jsonl" || seen.Prompt != "继续" || !strings.Contains(out.String(), "记忆引用 "+strings.Repeat("a", 64)) {
		t.Fatalf("unexpected hook request or output: %+v %s", seen, out.String())
	}

	// Daemon error degrades silently (Fail-Open).
	failing := stubHookClient{fn: func(context.Context, domain.HookRequest) (domain.RecallResult, error) {
		return domain.RecallResult{}, errors.New("connection refused")
	}}
	out.Reset()
	if err := claude.RunHook(t.Context(), failing, strings.NewReader(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hi"}`), &out); err != nil || out.Len() != 0 {
		t.Fatalf("expected silent fail-open, got err=%v out=%q", err, out.String())
	}
}

func TestMCPProtocolAndTools(t *testing.T) {
	client := stubMCPClient{
		getRef: func(_ context.Context, ref string) (domain.MemoryDetail, error) {
			return domain.MemoryDetail{Status: "ok", Reference: ref, Summary: "hello"}, nil
		},
		feedback: func(_ context.Context, in domain.FeedbackInput) (domain.ReceiptResult, error) {
			return domain.ReceiptResult{Status: "accepted", Receipt: "rcpt-1"}, nil
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- claude.RunMCP(ctx, client, "dev", serverTransport)
	}()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "mnevo-test", Version: "dev"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if init := session.InitializeResult(); init.ServerInfo.Version != "dev" || init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("unexpected initialize result: %+v", init)
	}
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools := list.Tools; len(tools) != 2 || tools[0].Name != "get_memory" || tools[1].Name != "submit_memory_feedback" {
		t.Fatalf("unexpected tool list: %+v", list)
	}

	call := func(name string, args map[string]string) (*mcp.CallToolResult, error) {
		return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	expectText := func(result *mcp.CallToolResult, err error, expected string) {
		t.Helper()
		if err != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("unexpected tool result: %+v (%v)", result, err)
		}
		if text, ok := result.Content[0].(*mcp.TextContent); !ok || !strings.Contains(text.Text, expected) {
			t.Fatalf("expected %q in tool result: %+v", expected, result.Content)
		}
	}
	result, err := call("get_memory", map[string]string{"reference": "abc"})
	expectText(result, err, `"hello"`)
	_, err = call("get_memory", map[string]string{"reference": "abc", "extra": "bad"})
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected strict unknown field rejection, got: %v", err)
	}
	result, err = call("submit_memory_feedback", map[string]string{
		"reference": "abc", "kind": "helpful", "reason": "useful", "idempotency_key": "k1",
	})
	expectText(result, err, "rcpt-1")

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

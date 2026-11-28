package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/mnevo/internal/daemon"
	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/policy"
	"github.com/coxlong/mnevo/internal/store"
)

type constEmbedder struct{}

func (constEmbedder) ModelID() string { return "const-embed" }
func (constEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

type constGate float64

func (g constGate) Decide(context.Context, string, any) (float64, error) { return float64(g), nil }

type gateFunc func(context.Context, string, any) (float64, error)

func (f gateFunc) Decide(ctx context.Context, prompt string, state any) (float64, error) {
	return f(ctx, prompt, state)
}

type rewriteFunc func(context.Context, string, model.RecallInput) (model.RecallQuery, error)

func (f rewriteFunc) RewriteRecall(ctx context.Context, prompt string, input model.RecallInput) (model.RecallQuery, error) {
	return f(ctx, prompt, input)
}

type recordingEmbedder struct {
	constEmbedder
	texts []string
}

func (e *recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.texts = append(e.texts, texts...)
	return e.constEmbedder.Embed(ctx, texts)
}

type stubExtract struct{}

func (stubExtract) Extract(_ context.Context, _, text string) ([]domain.MemoryDraft, error) {
	return []domain.MemoryDraft{{Kind: "preference", Summary: strings.TrimSpace(text), Tags: []string{"偏好"}, Conditions: "通用"}}, nil
}

func openTestService(t *testing.T, opts ...daemon.Option) (string, *store.Store, *daemon.Service) {
	t.Helper()
	return openServiceWithProviders(t, model.Providers{
		Embedder: constEmbedder{}, Gate: constGate(0.90), Extractor: stubExtract{},
	}, opts...)
}

func openServiceWithProviders(t *testing.T, providers model.Providers, opts ...daemon.Option) (string, *store.Store, *daemon.Service) {
	t.Helper()
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	polMgr, err := policy.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reg := model.NewStaticRegistry(providers)
	st, err := store.Open(t.Context(), filepath.Join(dir, "memory.sqlite"), polMgr, reg, store.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	allOpts := append([]daemon.Option{daemon.WithBuildInfo("v-test", dir)}, opts...)
	svc := daemon.NewService(st, "secret-admin-key", allOpts...)
	return dir, st, svc
}

func TestUnifiedHTTPServerTransportIsolationAndLifecycle(t *testing.T) {
	dir, st, svc := openTestService(t)
	socket := filepath.Join(dir, "mnevo.sock")

	// Occupy a port to also verify -web-fallback automatically falls back to an ephemeral port.
	busyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busyLn.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- svc.Serve(ctx, socket, busyLn.Addr().String(), true)
	}()

	agentClient := daemon.NewClient(dir, false)
	defer agentClient.Close()
	adminClient := daemon.NewAdminClient(dir, "secret-admin-key", false)
	defer adminClient.Close()

	// Wait for unified server to bind both Unix socket and fallback TCP Web listener.
	var health domain.HealthStatus
	for range 100 {
		h, err := agentClient.Health(t.Context())
		if err == nil {
			health = h
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if health.Status != "ok" || health.WebURL == "" || !health.RecallReady {
		t.Fatalf("unexpected health = %+v", health)
	}

	// 1. Transport isolation: TCP listener serves Web UI (/), NEVER /v1/* REST API!
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, health.WebURL+"/v1/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("TCP /v1/health status = %d, want 404", resp.StatusCode)
	}
	webReq, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, health.WebURL+"/", nil)
	webReq.SetBasicAuth("mnevo", "secret-admin-key")
	webResp, err := http.DefaultClient.Do(webReq)
	if err != nil {
		t.Fatal(err)
	}
	webResp.Body.Close()
	if webResp.StatusCode != http.StatusOK {
		t.Fatalf("TCP Web UI status = %d, want 200", webResp.StatusCode)
	}

	// 2. Unix socket Admin auth: agentClient (without key) MUST be rejected with 401 on /v1/memories.
	var dummy map[string]any
	err = agentClient.DoJSON(t.Context(), http.MethodGet, "/v1/memories", nil, &dummy)
	var apiErr *daemon.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without admin key, got %v", err)
	}

	// 3. Natural-language 记住： via POST /v1/hooks -> background worker forms & embeds -> recall delivers!
	acc, err := agentClient.PostHook(t.Context(), domain.HookRequest{
		SessionID: "sess-1", Event: "UserPromptSubmit", Prompt: "记住：回复优先给出简明结论",
	})
	if err != nil || acc.Status != "accepted" || acc.Receipt == "" {
		t.Fatalf("remember hook = %+v, %v", acc, err)
	}
	for range 40 {
		mems, _ := st.ListMemories(t.Context(), "简明结论", "")
		if len(mems) == 1 && mems[0].Embedded {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	deliv, err := agentClient.PostHook(t.Context(), domain.HookRequest{
		SessionID: "sess-1", Event: "UserPromptSubmit", Prompt: "请帮我总结报告",
	})
	if err != nil || deliv.Status != "delivered" || len(deliv.Recalls) != 1 {
		t.Fatalf("recall hook = %+v, %v", deliv, err)
	}
	ref := deliv.Recalls[0].Reference

	// 4. GET /v1/references/{ref} works during active turn.
	detail, err := agentClient.GetReference(t.Context(), ref)
	if err != nil || detail.Status != "ok" || !strings.Contains(detail.Summary, "简明结论") {
		t.Fatalf("GetReference = %+v, %v", detail, err)
	}

	// 5. Stop hook revokes active reference in SQLite AND enqueues transcript delta!
	txPath := filepath.Join(dir, "tx.jsonl")
	_ = os.WriteFile(txPath, []byte(`{"type":"user","message":{"role":"user","content":"上线前必须跑竞态检测"}}`+"\n"), 0600)
	stopRes, err := agentClient.PostHook(t.Context(), domain.HookRequest{
		SessionID: "sess-1", Event: "Stop", TranscriptPath: txPath,
	})
	if err != nil || stopRes.Status != "ok" {
		t.Fatalf("Stop hook = %+v, %v", stopRes, err)
	}
	revoked, err := agentClient.GetReference(t.Context(), ref)
	if err != nil || revoked.Status != "unavailable" {
		t.Fatalf("reference survived Stop: %+v, %v", revoked, err)
	}
	for range 40 {
		mems, _ := st.ListMemories(t.Context(), "竞态检测", "")
		if len(mems) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mems, _ := st.ListMemories(t.Context(), "竞态检测", "")
	if len(mems) != 1 {
		t.Fatalf("transcript auto-extract did not create memory: %+v", mems)
	}

	// 6. Admin REST shutdown cleanly stops Serve without SIGTERM.
	if err := adminClient.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("Serve returned error after Shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Serve shutdown")
	}
}

func TestRecallTimeoutAuditedCleanly(t *testing.T) {
	_, _, slowSvc := openTestService(t, daemon.WithRecallDelay(2*domain.RecallBudget))
	res, err := slowSvc.HandleHook(t.Context(), domain.HookRequest{
		SessionID: "slow", Event: "UserPromptSubmit", Prompt: "hi",
	})
	if err != nil || res.Status != "timeout" {
		t.Fatalf("slow recall = %+v, %v", res, err)
	}
}

func TestPromptHookRecallUsesPreviousAssistantAnswer(t *testing.T) {
	embedding := &recordingEmbedder{}
	var decisions, rewrites []model.RecallInput
	dir, st, svc := openServiceWithProviders(t, model.Providers{
		Embedder: embedding,
		Gate: gateFunc(func(_ context.Context, _ string, state any) (float64, error) {
			if input, ok := state.(model.RecallInput); ok {
				decisions = append(decisions, input)
				if input.Prompt == "明白了" {
					return 0.05, nil
				}
			}
			return 0.90, nil
		}),
		Rewriter: rewriteFunc(func(_ context.Context, _ string, input model.RecallInput) (model.RecallQuery, error) {
			rewrites = append(rewrites, input)
			if input.Prompt == "怎么修？" {
				return model.RecallQuery{}, errors.New("rewriter unavailable")
			}
			return model.RecallQuery{
				Query: "生产部署的数据库连接配置经验", Keywords: []string{"DATABASE_URL", "UNSEEN_TOKEN"},
			}, nil
		}),
	})
	_, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "fact", Summary: "生产部署从密钥存储加载数据库连接", Tags: []string{"部署"}, Conditions: "生产部署",
	})
	if err != nil {
		t.Fatal(err)
	}
	embedding.texts = nil
	path := filepath.Join(dir, "transcript.jsonl")
	previousTurn := strings.Join([]string{
		`{"type":"user","sessionId":"s1","message":{"content":"预发为什么失败？"}}`,
		`{"type":"assistant","sessionId":"s1","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"预发缺少 DATABASE_URL。"}]}}`,
		`{"type":"assistant","isSidechain":true,"sessionId":"s1","message":{"content":"子代理输出"}}`,
		`{"type":"assistant","sessionId":"other","message":{"content":"另一会话的回答"}}`,
		`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"正在检查"},{"type":"tool_use"}]}}`,
		`{"type":"user","sessionId":"s1","message":{"content":[{"type":"tool_result"}]}}`,
	}, "\n") + "\n"
	withCurrentPrompt := previousTurn + `{"type":"user","sessionId":"s1","message":{"content":"生产也是这样吗？"}}` + "\n"
	if err := os.WriteFile(path, []byte(withCurrentPrompt), 0600); err != nil {
		t.Fatal(err)
	}
	submit := func(event, prompt, transcript string) domain.RecallResult {
		t.Helper()
		result, err := svc.HandleHook(t.Context(), domain.HookRequest{
			SessionID: "s1", Event: event, Prompt: prompt, TranscriptPath: transcript,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := submit("UserPromptSubmit", "生产也是这样吗？", path); result.Status != "delivered" {
		t.Fatalf("follow-up did not deliver: %+v", result)
	}
	want := model.RecallInput{Prompt: "生产也是这样吗？", PreviousAnswer: "预发缺少 DATABASE_URL。"}
	if len(decisions) != 1 || decisions[0] != want || len(rewrites) != 1 || rewrites[0] != want {
		t.Fatalf("incorrect context: decisions=%+v rewrites=%+v", decisions, rewrites)
	}
	if len(embedding.texts) != 1 || embedding.texts[0] != "生产部署的数据库连接配置经验\nDATABASE_URL" {
		t.Fatalf("incorrect embedding query: %q", embedding.texts)
	}
	if result := submit("PostToolBatch", "工具已结束", path); result.Status != "no_match" || len(embedding.texts) != 1 {
		t.Fatalf("tool batch triggered recall: %+v, %q", result, embedding.texts)
	}
	// Exercise transcript state before Claude appends the newly submitted prompt.
	if err := os.WriteFile(path, []byte(previousTurn), 0600); err != nil {
		t.Fatal(err)
	}
	if result := submit("UserPromptSubmit", "明白了", path); result.ModelOutcome != "query_jev_skip" || len(embedding.texts) != 1 || len(rewrites) != 1 {
		t.Fatalf("acknowledgment triggered retrieval: %+v, %q", result, embedding.texts)
	}
	if result := submit("UserPromptSubmit", "怎么修？", path); result.Status != "delivered" || len(embedding.texts) != 2 ||
		!strings.Contains(embedding.texts[1], "怎么修？") || !strings.Contains(embedding.texts[1], "DATABASE_URL") {
		t.Fatalf("rewrite failure lost context: %+v, %q", result, embedding.texts)
	}
	if result := submit("UserPromptSubmit", "检查生产部署", ""); result.Status != "delivered" || len(embedding.texts) != 3 ||
		embedding.texts[2] != "检查生产部署" || len(decisions) != 3 || len(rewrites) != 2 {
		t.Fatalf("first-turn path should embed the original request: %+v, %q", result, embedding.texts)
	}
}

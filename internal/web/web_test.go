package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/mnevo/internal/daemon"
	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/policy"
	"github.com/coxlong/mnevo/internal/store"
	"github.com/coxlong/mnevo/internal/web"
)

type webStubEmbedder struct{}

func (webStubEmbedder) ModelID() string { return "web-embed" }
func (webStubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

type webStubGate func(string) float64

func (g webStubGate) Decide(_ context.Context, instr string, _ any) (float64, error) {
	return g(instr), nil
}

type webStubDiagnoser struct{}

func (webStubDiagnoser) Extract(_ context.Context, _, text string) ([]domain.MemoryDraft, error) {
	return []domain.MemoryDraft{{Kind: "guideline", Summary: strings.TrimSpace(text), Tags: []string{"规范"}, Conditions: "通用"}}, nil
}
func (webStubDiagnoser) Diagnose(_ context.Context, _ string, _ model.MetaDiagnosisInput) (model.MetaDiagnosisResult, error) {
	return model.MetaDiagnosisResult{
		TargetType: "setting", TargetKey: "recall.jev_threshold", AfterValue: 0.55,
		Diagnosis: "3 次误注入得分均低于 0.55", Impact: "减少误注入", Risk: "略降召回率",
	}, nil
}

func openWebTestEnv(t *testing.T, providers model.Providers) (*store.Store, *policy.Manager, *model.Registry, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	polMgr, err := policy.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	modReg, err := model.NewRegistry(filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if providers.Embedder != nil || providers.Gate != nil || providers.Extractor != nil || providers.Diagnoser != nil {
		modReg.SetProviders(func(p *model.Providers) { *p = providers })
	}
	st, err := store.Open(t.Context(), filepath.Join(dir, "memory.sqlite"), polMgr, modReg, store.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := daemon.NewService(st, "secret")
	handler := web.NewHandler(svc, "secret", "http://127.0.0.1:4242", nil)
	return st, polMgr, modReg, handler
}

// browserRequest performs a raw HTTP request without any authentication.
func browserRequest(handler http.Handler, method, targetURL, origin string, data url.Values, authenticated bool) *httptest.ResponseRecorder {
	var body string
	if data != nil {
		body = data.Encode()
	}
	req := httptest.NewRequest(method, targetURL, strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// webClient logs in once and reuses the session cookie across requests.
type webClient struct {
	t          *testing.T
	handler    http.Handler
	cookie     *http.Cookie
	cookieHost string
}

func newWebClient(t *testing.T, handler http.Handler, host string) *webClient {
	t.Helper()
	loginURL := "http://" + host + "/login"
	page := browserRequest(handler, http.MethodGet, loginURL, "", nil, false)
	if page.Code != http.StatusOK {
		t.Fatalf("login page = %d", page.Code)
	}
	csrf := extractCSRF(t, page.Body.String())
	form := url.Values{"csrf": {csrf}, "username": {"mnevo"}, "password": {"secret"}}
	login := browserRequest(handler, http.MethodPost, loginURL, "http://"+host, form, false)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login = %d, body = %s", login.Code, login.Body.String())
	}
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == "mnevo_session" && cookie.Value != "" {
			return &webClient{t: t, handler: handler, cookie: cookie, cookieHost: host}
		}
	}
	t.Fatal("session cookie missing after login")
	return nil
}

func (c *webClient) do(method, targetURL, origin string, data url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	var body string
	if data != nil {
		body = data.Encode()
	}
	req := httptest.NewRequest(method, targetURL, strings.NewReader(body))
	req.AddCookie(c.cookie)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

func (c *webClient) get(targetURL string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(http.MethodGet, targetURL, "", nil)
}

func (c *webClient) post(targetURL, origin string, data url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(http.MethodPost, targetURL, origin, data)
}

// postForm follows the PRG redirect: POST -> 303 -> GET, returning the final
// rendered page carrying the one-shot flash message.
func (c *webClient) postForm(targetURL, origin string, data url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	w := c.post(targetURL, origin, data)
	if w.Code != http.StatusSeeOther {
		return w
	}
	location := w.Header().Get("Location")
	if u, err := url.Parse(location); err == nil && u.IsAbs() {
		location = u.RequestURI()
	}
	base := "http://" + c.cookieHost
	return c.get(base + location)
}

func extractCSRF(t *testing.T, html string) string {
	t.Helper()
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(html)
	if len(match) != 2 {
		t.Fatalf("csrf token missing from HTML")
	}
	return match[1]
}

func TestWebModelConfigurationAndSmartImport(t *testing.T) {
	_, _, modReg, handler := openWebTestEnv(t, model.Providers{})
	client := newWebClient(t, handler, "127.0.0.1:4242")

	page := client.get("http://127.0.0.1:4242/?view=models")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "不注入") {
		t.Fatalf("models view = %d", page.Code)
	}
	csrf := extractCSRF(t, page.Body.String())

	for _, ep := range []struct{ target, url, model, key string }{
		{"embedding", "http://127.0.0.1:11434/v1", "nomic-embed-text", "sk-emb-12345678"},
		{"jev", "http://127.0.0.1:8000/v1/systemone", "jev-local", "sk-jev-12345678"},
	} {
		form := url.Values{
			"csrf": {csrf}, "operation": {"model"}, "action": {"set"}, "target": {ep.target},
			"enabled": {"on"}, "url": {ep.url}, "model": {ep.model}, "api_key": {ep.key},
		}
		w := client.postForm("http://127.0.0.1:4242/?view=models", "http://127.0.0.1:4242", form)
		if w.Code != 200 {
			t.Fatalf("model set %s = %d", ep.target, w.Code)
		}
	}
	if !modReg.Providers().RecallReady() {
		t.Fatal("expected hot-reloaded RecallReady after Web configuration")
	}
	after := client.get("http://127.0.0.1:4242/?view=models")
	if strings.Contains(after.Body.String(), "sk-emb-12345678") || !strings.Contains(after.Body.String(), "sk-e…5678") {
		t.Fatal("API key was not properly redacted in Web HTML")
	}

	modReg.SetProviders(func(p *model.Providers) {
		p.Embedder = webStubEmbedder{}
		p.Gate = webStubGate(func(string) float64 { return 0.90 })
		p.Extractor = webStubDiagnoser{}
	})
	impForm := url.Values{
		"csrf": {csrf}, "operation": {"memory"}, "action": {"import"},
		"location": {"style.md"}, "content": {"所有公共函数必须写清晰注释"},
	}
	impW := client.postForm("http://127.0.0.1:4242/", "http://127.0.0.1:4242", impForm)
	if impW.Code != 200 || !strings.Contains(impW.Body.String(), "所有公共函数必须写清晰注释") {
		t.Fatalf("smart import failed: %d", impW.Code)
	}
}

func TestStrategyProposalApprovalRollbackAndWeb(t *testing.T) {
	st, polMgr, _, handler := openWebTestEnv(t, model.Providers{
		Embedder: webStubEmbedder{},
		Gate: webStubGate(func(instr string) float64 {
			if strings.Contains(instr, "systemic") {
				return 0.90
			}
			return 0.35
		}),
		Diagnoser: webStubDiagnoser{},
	})
	ctx := t.Context()

	if _, _, err := st.CreateMemory(ctx, domain.MemoryDraft{
		Kind: "procedure", Summary: "导出稿件先用源表校对", Tags: []string{"导出"}, Conditions: "跨表导出",
	}); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	configured, err := polMgr.UpdateSetting("recall.jev_threshold", domain.Setting{
		Value: 0.25, Learn: true, Apply: "approval", Bounds: [2]float64{0.05, 0.90},
	}, "test", "enable")
	if err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}
	base := configured.Version

	next := func(id, want string) domain.RecallResult {
		t.Helper()
		res, err := st.Recall(ctx, domain.Task{SessionID: id, ID: id}, "导出图标")
		if err != nil || res.Status != want {
			t.Fatalf("recall(%s) = %+v, %v; want %s", id, res, err, want)
		}
		return res
	}

	for i := range 3 {
		id := fmt.Sprint("decision-", i)
		next(id, "delivered")
		for range 2 {
			ok, err := st.RecordObservation(ctx, domain.Observation{
				Kind: "false_injection", Task: id, Evidence: "与导出图标无关",
			})
			if err != nil || !ok {
				t.Fatalf("RecordObservation: %v, %v", ok, err)
			}
		}
		if err := st.Diagnose(ctx); err != nil {
			t.Fatal(err)
		}
	}

	proposals, _ := st.ListProposals(ctx)
	if len(proposals) != 1 || proposals[0].Base != base {
		t.Fatalf("proposals = %+v", proposals)
	}
	proposal := proposals[0]
	next("while-pending", "delivered")

	// Unauthenticated requests redirect to the branded login page instead of Basic Auth.
	if w := browserRequest(handler, http.MethodGet, "http://127.0.0.1:4242/?view=strategy", "", nil, false); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "login") {
		t.Fatalf("expected login redirect, got %d %s", w.Code, w.Header().Get("Location"))
	}
	client := newWebClient(t, handler, "127.0.0.1:4242")
	page := client.get("http://127.0.0.1:4242/?view=strategy")
	if page.Code != 200 || !strings.Contains(page.Body.String(), proposal.ID) {
		t.Fatal(page.Body.String())
	}
	csrf := extractCSRF(t, page.Body.String())
	form := url.Values{"csrf": {csrf}, "operation": {"strategy"}, "action": {"approve"}, "target": {proposal.ID}}
	if w := client.post("http://127.0.0.1:4242/?view=strategy", "https://other.invalid", form); w.Code != 403 {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
	next("after-rejected-origin", "delivered")

	if w := client.postForm("http://127.0.0.1:4242/?view=strategy", "http://127.0.0.1:4242", form); w.Code != 200 || !strings.Contains(w.Body.String(), "已批准") {
		t.Fatal(w.Body.String())
	}
	if polMgr.Active().Value("recall.jev_threshold") != 0.55 {
		t.Fatalf("strategy.json did not update: %v", polMgr.Active().Value("recall.jev_threshold"))
	}
	next("after-approved", "filtered")

	if _, err := polMgr.Rollback(base, "test"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	next("after-rollback", "delivered")
}

func TestWebAddFromMemoryDetailDoesNotOverwriteExisting(t *testing.T) {
	st, _, _, handler := openWebTestEnv(t, model.Providers{})
	client := newWebClient(t, handler, "127.0.0.1:4242")
	firstID, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "fact", Summary: "第一条记忆", Tags: []string{"测试"}, Conditions: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	detailPage := client.get("http://127.0.0.1:4242/?id=" + firstID)
	csrf := extractCSRF(t, detailPage.Body.String())

	form := url.Values{
		"csrf": {csrf}, "operation": {"memory"}, "action": {"add"},
		"target": {firstID},
		"kind":   {"fact"}, "summary": {"第二条独立记忆"}, "tags": {"独立"}, "conditions": {"B"},
	}
	w := client.postForm("http://127.0.0.1:4242/?id="+firstID, "http://127.0.0.1:4242", form)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	all, _ := st.ListMemories(t.Context(), "", "")
	if len(all) != 2 {
		t.Fatalf("expected 2 distinct memories, got %d", len(all))
	}
}

func TestWebAllowsLocalhostAndPublicURLSubpathWhileBlockingUnknownHost(t *testing.T) {
	_, _, _, handler := openWebTestEnv(t, model.Providers{})
	client := newWebClient(t, handler, "localhost:8765")

	// Loopback hosts (including localhost and SSH-forwarded ports) succeed by default.
	for _, target := range []string{
		"http://localhost:8765/",
		"http://localhost:18765/?view=models",
		"http://[::1]:8765/",
	} {
		if w := client.get(target); w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, w.Code)
		}
	}

	// Unconfigured external Host is strictly blocked (DNS rebinding protection).
	if w := client.get("https://proxy.example.com/mnevo/"); w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured external host = %d, want 404", w.Code)
	}

	// Configure web.public_url with subpath via Web UI (hot-reloads immediately!).
	page := client.get("http://localhost:8765/?view=models")
	csrf := extractCSRF(t, page.Body.String())
	setForm := url.Values{
		"csrf": {csrf}, "operation": {"web"}, "action": {"set"},
		"public_url": {"https://proxy.example.com/mnevo/"},
	}
	if w := client.postForm("http://localhost:8765/?view=models", "http://localhost:8765", setForm); w.Code != http.StatusOK {
		t.Fatalf("web set public_url = %d, want 200", w.Code)
	}

	// Configured external Host & subpath now succeed (both /mnevo and /mnevo/), and links are relative!
	for _, target := range []string{
		"https://proxy.example.com/mnevo",
		"https://proxy.example.com/mnevo/",
		"https://proxy.example.com/mnevo/?view=models",
	} {
		w := client.get(target)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `href="/?view=`) {
			t.Fatalf("GET %s = %d (expected 200 with relative links)", target, w.Code)
		}
	}

	// POST through reverse proxy (even when proxy rewrites Host to 127.0.0.1:8765) succeeds if Origin matches public_url!
	addForm := url.Values{
		"csrf": {csrf}, "operation": {"memory"}, "action": {"add"},
		"kind": {"fact"}, "summary": {"反代子路径新增记忆"}, "tags": {"测试"}, "conditions": {"反代"},
	}
	if w := client.postForm("http://127.0.0.1:8765/", "https://proxy.example.com", addForm); w.Code != http.StatusOK {
		t.Fatalf("proxied POST = %d, want 200", w.Code)
	}
	// Unrelated external Host & cross-origin POST remain strictly rejected!
	if w := client.get("https://evil.invalid/"); w.Code != http.StatusNotFound {
		t.Fatalf("evil Host = %d, want 404", w.Code)
	}
	if w := client.post("https://proxy.example.com/mnevo/", "https://evil.invalid", addForm); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", w.Code)
	}

	// Favicon returns 204 on both root and subpath; /v1/health still returns 404.
	for _, fav := range []string{"http://localhost:8765/favicon.ico", "https://proxy.example.com/mnevo/favicon.ico"} {
		if w := browserRequest(handler, http.MethodGet, fav, "", nil, false); w.Code != http.StatusNoContent {
			t.Fatalf("favicon %s = %d, want 204", fav, w.Code)
		}
	}
	if w := browserRequest(handler, http.MethodGet, "http://localhost:8765/v1/health", "", nil, true); w.Code != http.StatusNotFound {
		t.Fatalf("/v1/health over web handler = %d, want 404", w.Code)
	}
}

func TestWebSessionLoginLogoutAndHtmxPartial(t *testing.T) {
	_, _, _, handler := openWebTestEnv(t, model.Providers{})
	client := newWebClient(t, handler, "127.0.0.1:4242")

	// Wrong password is rejected with a friendly message (not Basic Auth).
	badForm := url.Values{"csrf": {extractCSRF(t, browserRequest(handler, http.MethodGet, "http://127.0.0.1:4242/login", "", nil, false).Body.String())},
		"username": {"mnevo"}, "password": {"wrong"}}
	if w := browserRequest(handler, http.MethodPost, "http://127.0.0.1:4242/login", "http://127.0.0.1:4242", badForm, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", w.Code)
	}

	// htmx partial: HX-Request returns only the fragment with OOB recall state.
	w := client.do(http.MethodGet, "http://127.0.0.1:4242/?view=models", "", nil)
	w.Header().Set("HX-Request", "true")
	// Redo with header set properly.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/?view=models", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(client.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "<!doctype") {
		t.Fatalf("partial render returned full document (status %d)", rec.Code)
	}
	if !strings.Contains(body, `id="page-content"`) || !strings.Contains(body, "hx-swap-oob") {
		t.Fatal("partial render missing #page-content or OOB recall state")
	}

	// Session expiry via unknown cookie redirects to login.
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/?view=memory", nil)
	req.AddCookie(&http.Cookie{Name: "mnevo_session", Value: "forged-session-id"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "login") {
		t.Fatalf("forged session = %d %s, want login redirect", rec.Code, rec.Header().Get("Location"))
	}

	// Logout destroys the session; subsequent requests redirect to login.
	page := client.get("http://127.0.0.1:4242/?view=memory")
	csrf := extractCSRF(t, page.Body.String())
	logoutForm := url.Values{"csrf": {csrf}}
	if w := client.post("http://127.0.0.1:4242/logout", "http://127.0.0.1:4242", logoutForm); w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", w.Code)
	}
	if w := client.get("http://127.0.0.1:4242/?view=memory"); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "login") {
		t.Fatalf("post-logout request = %d, want login redirect", w.Code)
	}
}

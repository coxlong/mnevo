package model_test

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonReply(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAllModelHTTPContracts(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("wrong method or credentials")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/v1/embeddings":
			return jsonReply(`{"data":[{"index":0,"embedding":[3.0,4.0]}]}`), nil
		case "/v1/systemone":
			if !strings.Contains(string(body["questions"]), `"decision"`) {
				t.Fatalf("bad System One body: %v", body)
			}
			return jsonReply(`{"answers":{"decision":{"type":"noul","noul":0.82}}}`), nil
		case "/v1/chat/completions":
			msgStr := string(body["messages"])
			switch {
			case strings.Contains(msgStr, "should_revise"):
				return jsonReply(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"should_revise\":true,\"reason\":\"updated\",\"summary\":\"new summary\",\"conditions\":\"always\",\"tags\":[\"go\"]}"}}]}`), nil
			case strings.Contains(msgStr, "target_type"):
				return jsonReply(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"target_type\":\"setting\",\"target_key\":\"recall.jev_threshold\",\"after_value\":0.45,\"diagnosis\":\"too noisy\",\"impact\":\"fewer false positives\",\"risk\":\"slight miss\"}"}}]}`), nil
			default:
				return jsonReply(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"memories\":[{\"type\":\"preference\",\"summary\":\"报告先给结论\",\"tags\":[\"报告\"],\"conditions\":\"写报告时\"}]}"}}]}`), nil
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return nil, nil
		}
	})}

	emb := model.NewOpenAIEmbedder(httpClient, "http://127.0.0.1:9999/v1", "embed-1", "test-key")
	vecs, err := emb.Embed(t.Context(), []string{"hello"})
	if err != nil || len(vecs) != 1 || math.Abs(float64(vecs[0][0])-0.6) > 1e-5 || math.Abs(float64(vecs[0][1])-0.8) > 1e-5 {
		t.Fatalf("embed = %+v, %v", vecs, err)
	}
	decoded := model.DecodeVector(model.EncodeVector(vecs[0]))
	if sim := model.CosineSimilarity(vecs[0], decoded); math.Abs(sim-1.0) > 1e-5 {
		t.Fatalf("round-trip cosine = %f", sim)
	}

	gate := model.NewSystemOne(httpClient, "http://127.0.0.1:9999/v1/systemone", "jev-1", "test-key")
	p, err := gate.Decide(t.Context(), "test?", map[string]string{"a": "b"})
	if err != nil || p != 0.82 {
		t.Fatalf("decide = %f, %v", p, err)
	}

	chat := model.NewOpenAIChat(httpClient, "http://127.0.0.1:9999/v1", "llm-1", "test-key")
	drafts, err := chat.Extract(t.Context(), "extract memories", "报告先给结论")
	if err != nil || len(drafts) != 1 || drafts[0].Summary != "报告先给结论" {
		t.Fatalf("extract = %+v, %v", drafts, err)
	}
	ver, err := chat.Verify(t.Context(), "return should_revise", model.FeedbackVerifyInput{CurrentSummary: "old"})
	if err != nil || ver.ShouldRevise == nil || !*ver.ShouldRevise || ver.Summary != "new summary" {
		t.Fatalf("verify = %+v, %v", ver, err)
	}
	diag, err := chat.Diagnose(t.Context(), "return target_type", model.MetaDiagnosisInput{ProblemKind: "false_injection"})
	if err != nil || diag.TargetType != "setting" || diag.AfterValue != 0.45 {
		t.Fatalf("diagnose = %+v, %v", diag, err)
	}
}

func TestRegistryPersistenceRedactionAndHotReload(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	reg, err := model.NewRegistry(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers().RecallReady() {
		t.Fatal("fresh registry should not be recall ready")
	}

	if err := reg.UpdateEndpoint("embedding", domain.ModelEndpointInput{
		Enabled: true, URL: "http://127.0.0.1:11434/v1", Model: "nomic-embed-text", APIKey: "sk-secret-12345678",
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateEndpoint("jev", domain.ModelEndpointInput{
		Enabled: true, URL: "http://127.0.0.1:8000/v1/systemone", Model: "jev-local",
	}); err != nil {
		t.Fatal(err)
	}
	if !reg.Providers().RecallReady() {
		t.Fatal("expected RecallReady after enabling embedding and jev")
	}

	// Updating model with blank APIKey preserves saved secret and masks it in View().
	if err := reg.UpdateEndpoint("embedding", domain.ModelEndpointInput{
		Enabled: true, URL: "http://127.0.0.1:11434/v1", Model: "bge-m3",
	}); err != nil {
		t.Fatal(err)
	}
	view := reg.View()
	if !view.Embedding.HasKey || !strings.Contains(view.Embedding.KeyMasked, "…") || strings.Contains(view.Embedding.KeyMasked, "secret") {
		t.Fatalf("unexpected masked view: %+v", view.Embedding)
	}

	info, err := os.Stat(cfgPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config.json mode = %v, %v", info, err)
	}

	// Reopening from disk restores live providers.
	reopened, err := model.NewRegistry(cfgPath, nil)
	if err != nil || !reopened.Providers().RecallReady() || reopened.Providers().Embedder.ModelID() != "bge-m3" {
		t.Fatalf("reopened failed: %v", err)
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, u := range []string{"https://openrouter.ai/api/v1", "http://127.0.0.1:11434/v1", "http://localhost:8000/v1"} {
		if _, err := model.ValidateEndpoint(u); err != nil {
			t.Fatalf("expected valid %q: %v", u, err)
		}
	}
	for _, u := range []string{"http://evil.example/v1", "https://user:pass@openrouter.ai/v1", "https://openrouter.ai/v1?x=1"} {
		if _, err := model.ValidateEndpoint(u); err == nil {
			t.Fatalf("expected error for %q", u)
		}
	}
}

func TestEndpointChangeRequiresExplicitKeyDecision(t *testing.T) {
	const oldURL = "http://127.0.0.1:8001/systemone"
	const newURL = "http://127.0.0.1:8002/systemone"
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	var destination, authorization string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		destination, authorization = r.URL.String(), r.Header.Get("Authorization")
		return jsonReply(`{"answers":{"decision":{"type":"noul","noul":0.82}}}`), nil
	})}
	reg, err := model.NewRegistry(cfgPath, client)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.ModelEndpointInput{Enabled: true, URL: oldURL, Model: "jev-local", APIKey: "test-old-key"}
	if err := reg.UpdateEndpoint("jev", input); err != nil {
		t.Fatal(err)
	}
	input.URL, input.APIKey = newURL, ""
	if err := reg.UpdateEndpoint("jev", input); err == nil {
		t.Fatal("changed destination with implicit key reuse")
	}
	if _, err := reg.TestConnection(t.Context(), "jev"); err != nil || destination != oldURL || authorization != "Bearer test-old-key" {
		t.Fatalf("rejected change altered the provider: destination=%s err=%v", destination, err)
	}
	input.ClearKey = true
	if err := reg.UpdateEndpoint("jev", input); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.TestConnection(t.Context(), "jev"); err != nil || destination != newURL || authorization != "" {
		t.Fatalf("new destination still using saved credentials: destination=%s err=%v", destination, err)
	}
	input.URL, input.APIKey, input.ClearKey = oldURL, "test-new-key", false
	if err := reg.UpdateEndpoint("jev", input); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.TestConnection(t.Context(), "jev"); err != nil || destination != oldURL || authorization != "Bearer test-new-key" {
		t.Fatalf("explicit replacement key was not applied: destination=%s err=%v", destination, err)
	}
}

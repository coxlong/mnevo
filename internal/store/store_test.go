package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/policy"
	"github.com/coxlong/mnevo/internal/store"
)

// topicEmbedder maps domain keywords into clean 4D orthogonal basis vectors so tests
// deterministically exercise real cosine similarity without network calls.
type topicEmbedder struct {
	id string
	fn func(context.Context, []string) ([][]float32, error)
}

func (e *topicEmbedder) ModelID() string {
	if e.id == "" {
		return "test-embed-v1"
	}
	return e.id
}

func (e *topicEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.fn != nil {
		return e.fn(ctx, texts)
	}
	out := make([][]float32, len(texts))
	for i, s := range texts {
		lower := strings.ToLower(s)
		v := []float32{0.05, 0.05, 0.05, 0.05}
		if strings.Contains(lower, "go") || strings.Contains(lower, "context") || strings.Contains(lower, "编程") {
			v[0] += 1.0
		}
		if strings.Contains(lower, "退款") || strings.Contains(lower, "支付") || strings.Contains(lower, "优惠券") {
			v[1] += 1.0
		}
		if strings.Contains(lower, "周报") || strings.Contains(lower, "结论") || strings.Contains(lower, "报告") {
			v[2] += 1.0
		}
		if strings.Contains(lower, "缓存") || strings.Contains(lower, "索引") || strings.Contains(lower, "排障") {
			v[3] += 1.0
		}
		out[i] = model.NormalizeVector(v)
	}
	return out, nil
}

type gateFunc func(context.Context, string, any) (float64, error)

func (f gateFunc) Decide(ctx context.Context, instr string, state any) (float64, error) {
	return f(ctx, instr, state)
}

type extractFunc func(context.Context, string, string) ([]domain.MemoryDraft, error)

func (f extractFunc) Extract(ctx context.Context, prompt, text string) ([]domain.MemoryDraft, error) {
	return f(ctx, prompt, text)
}

type verifyFunc func(context.Context, string, model.FeedbackVerifyInput) (model.FeedbackVerifyResult, error)

func (f verifyFunc) Verify(ctx context.Context, prompt string, in model.FeedbackVerifyInput) (model.FeedbackVerifyResult, error) {
	return f(ctx, prompt, in)
}

type diagnoseFunc func(context.Context, string, model.MetaDiagnosisInput) (model.MetaDiagnosisResult, error)

func (f diagnoseFunc) Diagnose(ctx context.Context, prompt string, in model.MetaDiagnosisInput) (model.MetaDiagnosisResult, error) {
	return f(ctx, prompt, in)
}

func openTestEnv(t *testing.T, providers model.Providers) (*store.Store, *policy.Manager, *model.Registry) {
	t.Helper()
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	polMgr, err := policy.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	modReg := model.NewStaticRegistry(providers)
	st, err := store.Open(t.Context(), filepath.Join(dir, "memory.sqlite"), polMgr, modReg, store.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, polMgr, modReg
}

func TestRecallSkipsWhenModelsUnconfigured(t *testing.T) {
	st, _, modReg := openTestEnv(t, model.Providers{})
	id, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "guideline", Summary: "Go 方法首参必须传 context", Tags: []string{"go"}, Conditions: "Go 编程",
	})
	if err != nil || id == "" {
		t.Fatalf("CreateMemory = %q, %v", id, err)
	}

	// No Embedding / Jev configured -> MUST NOT inject (zero local fallback).
	rec, err := st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t1"}, "写 Go context 代码")
	if err != nil || rec.Status != "no_match" || rec.ModelOutcome != "models_not_configured" {
		t.Fatalf("expected models_not_configured no_match, got %+v, %v", rec, err)
	}

	// Only Embedding without Jev -> still MUST NOT inject.
	modReg.SetProviders(func(p *model.Providers) { p.Embedder = &topicEmbedder{} })
	if err := st.EmbedNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec, err = st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t2"}, "写 Go context 代码")
	if err != nil || rec.Status != "no_match" || rec.ModelOutcome != "models_not_configured" {
		t.Fatalf("expected models_not_configured without Jev, got %+v, %v", rec, err)
	}
}

func TestCrossDomainVectorRecallDecayJevGateAndSessionRevocation(t *testing.T) {
	st, _, modReg := openTestEnv(t, model.Providers{
		Embedder: &topicEmbedder{},
		Gate: gateFunc(func(_ context.Context, _ string, _ any) (float64, error) {
			return 0.90, nil
		}),
	})

	for _, d := range []domain.MemoryDraft{
		{Kind: "guideline", Summary: "Go 接口方法首参统一使用 context.Context", Tags: []string{"go", "编程规范"}, Conditions: "Go 后端开发"},
		{Kind: "fact", Summary: "部分退款时优惠券按实付金额比例分摊扣减", Tags: []string{"支付", "退款"}, Conditions: "退款结算业务", Details: "见结算规则第 3 章"},
		{Kind: "episode", Summary: "冷启动缓存为空需重建离线索引", Tags: []string{"缓存", "索引"}, Conditions: "冷启动排障"},
	} {
		if _, _, err := st.CreateMemory(t.Context(), d); err != nil {
			t.Fatalf("CreateMemory = %v", err)
		}
	}

	// BeginTurn creates a fresh SQL-backed task.
	task, err := st.BeginTurn(t.Context(), "s1", "UserPromptSubmit")
	if err != nil || task.ID == "" {
		t.Fatalf("BeginTurn = %+v, %v", task, err)
	}
	rec, err := st.Recall(t.Context(), task, "用 Go 实现支付退款接口的详情逻辑")
	if err != nil || rec.Status != "delivered" || len(rec.Recalls) != 2 {
		t.Fatalf("expected 2 cross-domain recalls, got %+v, %v", rec, err)
	}
	ref := rec.Recalls[0].Reference

	// MCP get_memory reads active reference cleanly from SQLite.
	got, err := st.GetReference(t.Context(), ref)
	if err != nil || got.Status != "ok" || got.Evidence == nil {
		t.Fatalf("GetReference = %+v, %v", got, err)
	}

	// EndTurn (Stop) revokes all deliveries for that session in SQLite immediately.
	if _, err := st.EndTurn(t.Context(), "s1", "Stop"); err != nil {
		t.Fatalf("EndTurn = %v", err)
	}
	if revoked, _ := st.GetReference(t.Context(), ref); revoked.Status != "unavailable" {
		t.Fatalf("reference survived EndTurn: %+v", revoked)
	}

	// Time decay reduces episode score below minimum after 180 days (episode half-life = 30d).
	future := st.Now().Add(180 * 24 * time.Hour)
	st.SetClock(func() time.Time { return future })
	if faded, _ := st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t-fade"}, "排查缓存索引问题"); faded.Status != "no_match" {
		t.Fatalf("expected decayed episode no_match, got %+v", faded)
	}

	// Jev low score filters; Jev error MUST NOT fall back to local injection!
	modReg.SetProviders(func(p *model.Providers) {
		p.Gate = gateFunc(func(context.Context, string, any) (float64, error) { return 0.10, nil })
	})
	if flt, _ := st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t-flt"}, "Go 编程规范"); flt.Status != "filtered" {
		t.Fatalf("expected filtered, got %+v", flt)
	}
	modReg.SetProviders(func(p *model.Providers) {
		p.Gate = gateFunc(func(context.Context, string, any) (float64, error) { return 0, errors.New("jev down") })
	})
	if errRes, _ := st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t-err"}, "Go 编程规范"); errRes.Status != "no_match" || errRes.ModelOutcome != "jev_error" {
		t.Fatalf("expected no_match on jev_error without fallback, got %+v", errRes)
	}
}

func TestRecallEvaluatesOnlyTopCandidatesInStableOrder(t *testing.T) {
	var evaluated []string
	st, pol, _ := openTestEnv(t, model.Providers{
		Embedder: &topicEmbedder{},
		Gate: gateFunc(func(_ context.Context, _ string, state any) (float64, error) {
			evaluated = append(evaluated, state.(map[string]any)["summary"].(string))
			return 0.1, nil
		}),
	})
	k := int(pol.Active().Value("recall.jev_candidates"))
	var ids []string
	summaries := make(map[string]string)
	for i := range k + 3 {
		summary := fmt.Sprintf("Go guideline %d", i)
		id, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
			Kind: "guideline", Summary: summary, Conditions: "Go 后端开发",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		summaries[id] = summary
	}
	slices.Sort(ids)
	want := make([]string, k)
	for i, id := range ids[:k] {
		want[i] = summaries[id]
	}
	result, err := st.Recall(t.Context(), domain.Task{SessionID: "session", ID: "task"}, "Go 开发")
	if err != nil || result.Status != "filtered" || !slices.Equal(evaluated, want) {
		t.Fatalf("recall = %+v, %v, evaluated = %v, want %v", result, err, evaluated, want)
	}
}

func TestRecallDoesNotDeliverAfterTurnEndsDuringJev(t *testing.T) {
	st, _, models := openTestEnv(t, model.Providers{Embedder: &topicEmbedder{}})
	if _, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "guideline", Summary: "Go 方法首参使用 context.Context", Conditions: "Go 后端开发",
	}); err != nil {
		t.Fatal(err)
	}
	task, err := st.BeginTurn(t.Context(), "session", "UserPromptSubmit")
	if err != nil {
		t.Fatal(err)
	}
	var ended domain.Task
	var endErr error
	models.SetProviders(func(p *model.Providers) {
		p.Gate = gateFunc(func(ctx context.Context, _ string, _ any) (float64, error) {
			ended, endErr = st.EndTurn(ctx, task.SessionID, "Stop")
			return 0.9, endErr
		})
	})
	result, err := st.Recall(t.Context(), task, "Go context 接口")
	if err != nil || endErr != nil || ended.ID != task.ID {
		t.Fatalf("recall = %v, end turn = %+v, %v", err, ended, endErr)
	}
	if result.Status != "no_match" || len(result.Recalls) != 0 {
		t.Fatalf("ended task received memory: %+v", result)
	}
}

func TestJevGatedImportAndTranscriptAutoExtract(t *testing.T) {
	llmCalls := 0
	st, _, _ := openTestEnv(t, model.Providers{
		Embedder: &topicEmbedder{},
		Gate: gateFunc(func(_ context.Context, _ string, state any) (float64, error) {
			m := state.(map[string]any)
			excerpt := fmt.Sprint(m["excerpt"])
			if strings.Contains(excerpt, "你好") || strings.Contains(excerpt, "天气") {
				return 0.05, nil // chit-chat stopped at Jev!
			}
			return 0.90, nil
		}),
		Extractor: extractFunc(func(_ context.Context, _ string, text string) ([]domain.MemoryDraft, error) {
			llmCalls++
			if strings.Contains(text, "周报") {
				return []domain.MemoryDraft{{
					Kind: "preference", Summary: "周报开头先给三条核心结论", Tags: []string{"周报", "结论"}, Conditions: "撰写周报时",
				}}, nil
			}
			return []domain.MemoryDraft{{
				Kind: "fact", Summary: "退款优惠券按实付比例扣减", Tags: []string{"退款", "支付"}, Conditions: "退款结算",
			}}, nil
		}),
	})

	// 1. Chit-chat document import is stopped by Jev -> 0 LLM calls!
	skipRes, err := st.ImportDocument(t.Context(), domain.ImportInput{Location: "chat.txt", Content: "你好，今天天气真不错"})
	if err != nil || skipRes.Status != "jev_skipped" || llmCalls != 0 {
		t.Fatalf("chit-chat import = %+v, llmCalls=%d, err=%v", skipRes, llmCalls, err)
	}

	// 2. Substantive document import passes Jev -> LLM extracts -> directly confirmed + embedded!
	impRes, err := st.ImportDocument(t.Context(), domain.ImportInput{Location: "rules.md", Content: "退款时优惠券必须按实付金额比例扣减"})
	if err != nil || impRes.Status != "ok" || llmCalls != 1 {
		t.Fatalf("import = %+v, llmCalls=%d, err=%v", impRes, llmCalls, err)
	}

	// 3. Claude Code transcript incremental auto-extraction.
	transcriptFile := filepath.Join(t.TempDir(), "session.jsonl")
	line1 := `{"type":"user","message":{"role":"user","content":"你好"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"你好，有什么可以帮你？"}]}}` + "\n"
	if err := os.WriteFile(transcriptFile, []byte(line1), 0600); err != nil {
		t.Fatal(err)
	}
	queued, err := st.EnqueueTranscriptDelta(t.Context(), "sess-1", "turn-1", transcriptFile)
	if err != nil || !queued {
		t.Fatalf("enqueue turn 1 = %v, %v", queued, err)
	}
	if err := st.FormNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if llmCalls != 1 {
		t.Fatalf("expected Jev to block chit-chat transcript from calling LLM, llmCalls=%d", llmCalls)
	}

	line2 := `{"type":"user","message":{"role":"user","content":"以后写周报开头先给三条核心结论"}}` + "\n"
	f, _ := os.OpenFile(transcriptFile, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(line2)
	_ = f.Close()
	queued, err = st.EnqueueTranscriptDelta(t.Context(), "sess-1", "turn-2", transcriptFile)
	if err != nil || !queued {
		t.Fatalf("enqueue turn 2 = %v, %v", queued, err)
	}
	if err := st.FormNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if llmCalls != 2 {
		t.Fatalf("expected LLM called once for durable turn, llmCalls=%d", llmCalls)
	}

	rec, err := st.Recall(t.Context(), domain.Task{SessionID: "sess-2", ID: "turn-3"}, "帮我写本周周报结论")
	if err != nil || rec.Status != "delivered" || len(rec.Recalls) == 0 || !strings.Contains(rec.Recalls[0].Summary, "核心结论") {
		t.Fatalf("auto-extracted memory not recalled: %+v, %v", rec, err)
	}

	queued, err = st.EnqueueTranscriptDelta(t.Context(), "sess-1", "turn-2", transcriptFile)
	if err != nil || queued {
		t.Fatalf("expected no delta, got %v, %v", queued, err)
	}
}

func TestEmbedNextBackfillsAfterModelSwitch(t *testing.T) {
	st, _, modReg := openTestEnv(t, model.Providers{})
	if _, _, err := st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "fact", Summary: "Go 代码禁止忽略 error", Tags: []string{"go"}, Conditions: "Go 开发",
	}); err != nil {
		t.Fatal(err)
	}
	if emb, total := st.EmbeddingStats(t.Context()); emb != 0 || total != 1 {
		t.Fatalf("stats before = %d/%d", emb, total)
	}

	modReg.SetProviders(func(p *model.Providers) {
		p.Embedder = &topicEmbedder{id: "embed-v2"}
		p.Gate = gateFunc(func(context.Context, string, any) (float64, error) { return 0.9, nil })
	})
	if err := st.EmbedNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if emb, total := st.EmbeddingStats(t.Context()); emb != 1 || total != 1 {
		t.Fatalf("stats after backfill = %d/%d", emb, total)
	}
	if rec, _ := st.Recall(t.Context(), domain.Task{SessionID: "s1", ID: "t1"}, "检查 Go 错误处理"); rec.Status != "delivered" {
		t.Fatalf("backfilled memory not recalled: %+v", rec)
	}
}

func TestFeedbackJevGatedVerificationAndIdempotency(t *testing.T) {
	llmCalls := 0
	yes := true
	st, _, _ := openTestEnv(t, model.Providers{
		Embedder: &topicEmbedder{},
		Gate: gateFunc(func(_ context.Context, _ string, state any) (float64, error) {
			m := state.(map[string]any)
			if strings.Contains(fmt.Sprint(m["feedback_reason"]), "噪音") {
				return 0.05, nil
			}
			return 0.92, nil
		}),
		Verifier: verifyFunc(func(_ context.Context, _ string, _ model.FeedbackVerifyInput) (model.FeedbackVerifyResult, error) {
			llmCalls++
			return model.FeedbackVerifyResult{
				ShouldRevise: &yes, Reason: "verified update",
				Summary: "Go 接口首参用 context 并设置超时", Conditions: "Go 后端代码", Tags: []string{"go", "context"},
			}, nil
		}),
	})

	_, _, _ = st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "guideline", Summary: "Go 接口首参传 context", Tags: []string{"go"}, Conditions: "Go 后端",
	})
	task := domain.Task{SessionID: "s1", ID: "t1"}
	rec, err := st.Recall(t.Context(), task, "写 Go context 接口")
	if err != nil || rec.Status != "delivered" || len(rec.Recalls) != 1 {
		t.Fatalf("recall = %+v, %v", rec, err)
	}
	ref := rec.Recalls[0].Reference

	// Concurrent idempotent submission returns same receipt; conflict rejected.
	f := domain.FeedbackInput{Reference: ref, Kind: "correction", Reason: "噪音反馈", Key: "k1"}
	var wg sync.WaitGroup
	receipts := make([]string, 6)
	for i := range receipts {
		wg.Go(func() {
			r, _ := st.SubmitFeedback(t.Context(), f)
			receipts[i] = r.Receipt
		})
	}
	wg.Wait()
	for _, id := range receipts {
		if id == "" || id != receipts[0] {
			t.Fatalf("idempotency broken: %v", receipts)
		}
	}
	conflict := f
	conflict.Reason = "different"
	if r, _ := st.SubmitFeedback(t.Context(), conflict); r.Status != "conflict" {
		t.Fatalf("expected conflict, got %+v", r)
	}

	// Verify noisy correction -> blocked by Jev -> 0 LLM calls!
	if err := st.VerifyNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if llmCalls != 0 {
		t.Fatalf("expected Jev to block noisy correction, llmCalls=%d", llmCalls)
	}

	// Substantive correction -> passes Jev -> LLM revises to v2 & re-embeds!
	sub, err := st.SubmitFeedback(t.Context(), domain.FeedbackInput{
		Reference: ref, Kind: "correction", Reason: "规范已补充必须设置超时", Key: "k2",
	})
	if err != nil || sub.Status != "accepted" {
		t.Fatalf("submit k2 = %+v, %v", sub, err)
	}
	if err := st.VerifyNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if llmCalls != 1 {
		t.Fatalf("expected 1 LLM verify call, got %d", llmCalls)
	}
	if old, _ := st.GetReference(t.Context(), ref); old.Status != "unavailable" {
		t.Fatalf("superseded ref still readable: %+v", old)
	}
	v2, _ := st.Recall(t.Context(), domain.Task{SessionID: "s2", ID: "t2"}, "写 Go context 接口")
	if len(v2.Recalls) != 1 || v2.Recalls[0].Version != 2 || !strings.Contains(v2.Recalls[0].Summary, "设置超时") {
		t.Fatalf("expected v2 recall, got %+v", v2)
	}
}

func TestMetaLearningJevToLLMDiagnosisApprovalAndRollback(t *testing.T) {
	jevRecallProb := 0.35
	diagLLMCalls := 0
	st, polMgr, _ := openTestEnv(t, model.Providers{
		Embedder: &topicEmbedder{},
		Gate: gateFunc(func(_ context.Context, instr string, _ any) (float64, error) {
			if strings.Contains(instr, "systemic") {
				return 0.88, nil
			}
			return jevRecallProb, nil
		}),
		Diagnoser: diagnoseFunc(func(_ context.Context, _ string, in model.MetaDiagnosisInput) (model.MetaDiagnosisResult, error) {
			diagLLMCalls++
			if len(in.Samples) != 3 || !in.LearnableSettings["recall.jev_threshold"].Learn {
				t.Fatalf("unexpected diagnosis input: %+v", in)
			}
			return model.MetaDiagnosisResult{
				TargetType: "setting",
				TargetKey:  "recall.jev_threshold",
				AfterValue: 0.50,
				Diagnosis:  "3 次误注入的 jev_p 均为 0.35，建议提高 recall.jev_threshold 至 0.50",
				Impact:     "减少边界相关记忆的误注入",
				Risk:       "轻微降低召回覆盖率",
			}, nil
		}),
	})

	_, _, _ = st.CreateMemory(t.Context(), domain.MemoryDraft{
		Kind: "guideline", Summary: "Go 代码规范", Tags: []string{"go"}, Conditions: "Go 后端",
	})
	pol, err := polMgr.UpdateSetting("recall.jev_threshold", domain.Setting{
		Value: 0.25, Learn: true, Apply: "approval", Bounds: [2]float64{0.05, 0.90},
	}, "test", "enable learning")
	if err != nil || pol.Version != 2 {
		t.Fatalf("UpdateSetting = %+v, %v", pol, err)
	}

	for i := range 3 {
		tid := fmt.Sprintf("task-%d", i)
		r, _ := st.Recall(t.Context(), domain.Task{SessionID: tid, ID: tid}, "Go 代码")
		if r.Status != "delivered" {
			t.Fatalf("expected delivered on v2, got %+v", r)
		}
		ok, err := st.RecordObservation(t.Context(), domain.Observation{
			Kind: "false_injection", Task: tid, Evidence: "与当前任务无关",
		})
		if err != nil || !ok {
			t.Fatalf("RecordObservation = %v, %v", ok, err)
		}
	}

	if err := st.Diagnose(t.Context()); err != nil {
		t.Fatal(err)
	}
	if diagLLMCalls != 1 {
		t.Fatalf("expected 1 LLM diagnosis call, got %d", diagLLMCalls)
	}
	props, _ := st.ListProposals(t.Context())
	if len(props) != 1 || props[0].AfterVal != 0.50 {
		t.Fatalf("proposals = %+v", props)
	}

	if r, _ := st.Recall(t.Context(), domain.Task{SessionID: "pending", ID: "pending"}, "Go 代码"); r.Status != "delivered" {
		t.Fatalf("pending changed runtime: %+v", r)
	}

	status, activePol, err := st.ResolveProposal(t.Context(), props[0].ID, true, "test")
	if err != nil || status != "approved" || activePol.Version != 3 || polMgr.Active().Value("recall.jev_threshold") != 0.50 {
		t.Fatalf("ResolveProposal = %q, %+v, %v", status, activePol, err)
	}

	if r, _ := st.Recall(t.Context(), domain.Task{SessionID: "after", ID: "after"}, "Go 代码"); r.Status != "filtered" {
		t.Fatalf("expected filtered after approval, got %+v", r)
	}

	rbPol, err := polMgr.Rollback(2, "test")
	if err != nil || rbPol.Version != 4 {
		t.Fatalf("Rollback = %+v, %v", rbPol, err)
	}
	if r, _ := st.Recall(t.Context(), domain.Task{SessionID: "rb", ID: "rb"}, "Go 代码"); r.Status != "delivered" {
		t.Fatalf("expected delivered after rollback, got %+v", r)
	}
}

func TestDeleteScrubsEmbeddingsAndOpenPreservesIncompatibleDB(t *testing.T) {
	st, _, _ := openTestEnv(t, model.Providers{Embedder: &topicEmbedder{}})
	draft := domain.MemoryDraft{
		Kind: "fact", Summary: "秘密测试数据", Tags: []string{"go"}, Conditions: "测试",
	}
	id, _, err := st.CreateMemory(t.Context(), draft)
	if err != nil {
		t.Fatal(err)
	}
	if emb, _ := st.EmbeddingStats(t.Context()); emb != 1 {
		t.Fatalf("expected 1 embedding, got %d", emb)
	}
	if ok, err := st.SetMemoryState(t.Context(), id, domain.StateDeleted); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	var cnt int
	_ = st.DB().Raw("SELECT count(*) FROM memory_embeddings").Row().Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("deleted memory still has %d embeddings", cnt)
	}
	var fingerprint sql.NullString
	if err := st.DB().Raw("SELECT fingerprint FROM memories WHERE id = ?", id).Row().Scan(&fingerprint); err != nil || fingerprint.Valid {
		t.Fatalf("deletion kept the fingerprint: %+v, %v", fingerprint, err)
	}
	// Simulate a tombstone persisted by an older build.
	if err := st.DB().Exec("UPDATE memories SET fingerprint = ? WHERE id = ?", domain.MemoryFingerprint(draft), id).Error; err != nil {
		t.Fatal(err)
	}
	newID, version, err := st.CreateMemory(t.Context(), draft)
	if err != nil || newID == id || version != 1 {
		t.Fatalf("creating the same memory again: new=%v version=%d err=%v", newID != id, version, err)
	}
	if rows, err := st.ListMemories(t.Context(), "", newID); err != nil || len(rows) != 1 || rows[0].Summary != draft.Summary {
		t.Fatalf("new memory is not visible: %+v, %v", rows, err)
	}

	dir := t.TempDir()
	badDB := filepath.Join(dir, "memory.sqlite")
	db, _ := sql.Open("sqlite", badDB)
	_, _ = db.Exec("CREATE TABLE keep_me(x TEXT); INSERT INTO keep_me VALUES ('safe')")
	_ = db.Close()
	polMgr, _ := policy.Open(dir, nil)
	if _, err := store.Open(t.Context(), badDB, polMgr, nil); err == nil {
		t.Fatal("expected incompatible DB error")
	}
}

func TestTranscriptBatchRecoveryAndPartialRecord(t *testing.T) {
	st, _, _ := openTestEnv(t, model.Providers{})
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := fmt.Sprintf(`{"type":"user","message":{"content":%q}}`, strings.Repeat("字", 1500)) + "\n"
	prefix := strings.Repeat(line, 20)
	tail := `{"type":"assistant","message":{"content":"tail marker"}}` + "\n"
	partial := `{"type":"assistant","message":{"content":"partial`
	if err := os.WriteFile(path, []byte(prefix+tail+partial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec(`CREATE TRIGGER reject_tail BEFORE INSERT ON formation
		WHEN instr(NEW.raw_text, 'tail marker') > 0 BEGIN SELECT RAISE(ABORT, 'queue unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if queued, err := st.EnqueueTranscriptDelta(ctx, "session", "task", path); !queued || err == nil {
		t.Fatalf("expected a saved prefix and a failed tail: %v, %v", queued, err)
	}
	var offset int
	if err := st.DB().Raw("SELECT last_offset FROM transcript_cursors WHERE session_id = 'session'").Row().Scan(&offset); err != nil ||
		offset <= 0 || offset >= len(prefix+tail) {
		t.Fatalf("cursor advanced past failed insert: %d, %v", offset, err)
	}
	if err := st.DB().Exec("DROP TRIGGER reject_tail").Error; err != nil {
		t.Fatal(err)
	}
	var retry sync.WaitGroup
	for range 4 {
		retry.Go(func() {
			if _, err := st.EnqueueTranscriptDelta(ctx, "session", "task", path); err != nil {
				t.Errorf("failed to retry the tail: %v", err)
			}
		})
	}
	retry.Wait()
	completed := prefix + tail + partial + " record\"}}\n"
	if err := os.WriteFile(path, []byte(completed), 0600); err != nil {
		t.Fatal(err)
	}
	if queued, err := st.EnqueueTranscriptDelta(ctx, "session", "task", path); !queued || err != nil {
		t.Fatalf("failed to queue the completed record: %v, %v", queued, err)
	}
	if queued, err := st.EnqueueTranscriptDelta(ctx, "session", "retry", path); queued || err != nil {
		t.Fatalf("retry incorrectly queued a duplicate: %v, %v", queued, err)
	}
	var jobs []string
	if err := st.DB().Raw("SELECT raw_text FROM formation ORDER BY rowid").Scan(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	all := strings.Join(jobs, "\n")
	if strings.Count(all, "user: ") != 20 || strings.Count(all, "tail marker") != 1 ||
		strings.Count(all, "partial record") != 1 {
		t.Fatalf("missing or repeated transcript records across %d jobs", len(jobs))
	}
}

func TestProposalResolutionRespectsNewPolicyAndRecoversPublication(t *testing.T) {
	st, polMgr, _ := openTestEnv(t, model.Providers{})
	ctx := t.Context()
	const key = "recall.jev_threshold"
	item := domain.Setting{Value: 0.25, Learn: true, Apply: "auto", Bounds: [2]float64{0.15, 0.40}}
	setPolicy := func() domain.Policy {
		t.Helper()
		p, err := polMgr.UpdateSetting(key, item, "admin", "set learning rules")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	insertProposal := func(id string, base int) {
		t.Helper()
		if err := st.DB().Exec(`INSERT INTO proposals(id, base_version, kind, target_type, target_key, before_val, after_val)
			VALUES (?, ?, 'false_injection', 'setting', ?, 0.25, 0.35)`, id, base, key).Error; err != nil {
			t.Fatal(err)
		}
	}

	insertProposal("stale", setPolicy().Version)
	item.Learn, item.Apply, item.Bounds[1] = false, "approval", 0.30
	restricted := setPolicy()
	if status, p, err := st.ResolveProposal(ctx, "stale", true, "preauthorized"); err != nil ||
		status != "stale" || p.Version != restricted.Version || p.Settings[key] != item {
		t.Fatalf("old proposal changed the administrator's settings: %s, %v", status, err)
	}

	item.Learn, item.Bounds[1] = true, 0.40
	base := setPolicy().Version
	insertProposal("recover", base)
	if err := st.DB().Exec(`CREATE TRIGGER reject_decision BEFORE UPDATE ON proposals
		WHEN NEW.id = 'recover' BEGIN SELECT RAISE(ABORT, 'decision unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ResolveProposal(ctx, "recover", true, "approver"); err == nil {
		t.Fatal("expected SQLite to fail after publishing the strategy")
	}
	if p := polMgr.Active(); p.Version != base+1 || p.Value(key) != 0.35 {
		t.Fatalf("expected file publication before SQLite failure, got version %d, value %v", p.Version, p.Value(key))
	}
	if err := st.DB().Exec("DROP TRIGGER reject_decision").Error; err != nil {
		t.Fatal(err)
	}
	if status, p, err := st.ResolveProposal(ctx, "recover", false, "rejecter"); err != nil ||
		status != "conflict" || p.Version != base+1 {
		t.Fatalf("retry did not settle existing publication: %s, version=%d, %v", status, p.Version, err)
	}
	var savedStatus, savedActor string
	var published int
	if err := st.DB().Raw("SELECT status, actor, published FROM proposals WHERE id = 'recover'").
		Row().Scan(&savedStatus, &savedActor, &published); err != nil ||
		savedStatus != "approved" || savedActor != "approver" || published != base+1 {
		t.Fatalf("wrong recovered decision: %s by %s at v%d, %v", savedStatus, savedActor, published, err)
	}
}

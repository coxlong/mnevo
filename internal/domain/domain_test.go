package domain_test

import (
	"testing"

	"github.com/coxlong/mnevo/internal/domain"
)

func TestMemoryDraftNormalizeAndFingerprint(t *testing.T) {
	d := domain.MemoryDraft{
		Kind:       domain.KindGuideline,
		Summary:    "  Go 接口方法必须首参传 context.Context  ",
		Tags:       []string{"Go", " 编程规范 ", "go"},
		Conditions: "编写 Go 后端代码时",
	}
	if !d.Normalize() {
		t.Fatal("expected valid draft")
	}
	if len(d.Tags) != 2 || d.Tags[0] != "go" || d.Tags[1] != "编程规范" {
		t.Fatalf("unexpected normalized tags: %+v", d.Tags)
	}
	d2 := d
	d2.Summary = "go   接口方法必须首参传 context.context"
	if domain.MemoryFingerprint(d) != domain.MemoryFingerprint(d2) {
		t.Fatal("expected whitespace/case-insensitive fingerprint match")
	}
	bad := domain.MemoryDraft{Kind: "invalid", Summary: "x"}
	if bad.Normalize() {
		t.Fatal("invalid kind accepted")
	}
}

func TestValidatePolicyCrossInvariants(t *testing.T) {
	p := domain.DefaultPolicy(1)
	if err := domain.ValidatePolicy(p); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}

	// embedding_ms + jev_ms >= deadline_ms must fail.
	bad := domain.DefaultPolicy(1)
	s := bad.Settings["recall.deadline_ms"]
	s.Value = 300 // 180 + 220 >= 300
	bad.Settings["recall.deadline_ms"] = s
	if err := domain.ValidatePolicy(bad); err == nil {
		t.Fatal("expected error when embedding_ms + jev_ms >= deadline_ms")
	}

	// Prompts must never allow auto-apply.
	badPrompt := domain.DefaultPolicy(1)
	pr := badPrompt.Prompts["jev_recall"]
	pr.Apply = "auto"
	badPrompt.Prompts["jev_recall"] = pr
	if err := domain.ValidatePolicy(badPrompt); err == nil {
		t.Fatal("expected error when prompt apply == auto")
	}

	// Narrow-band recall.jev_threshold allows auto-apply; wide-band rejected.
	narrow := domain.Setting{Value: 0.30, Learn: true, Apply: "auto", Bounds: [2]float64{0.20, 0.45}}
	if err := domain.ValidateSetting("recall.jev_threshold", narrow); err != nil {
		t.Fatalf("valid narrow auto setting rejected: %v", err)
	}
	wide := domain.Setting{Value: 0.30, Learn: true, Apply: "auto", Bounds: [2]float64{0.10, 0.80}}
	if err := domain.ValidateSetting("recall.jev_threshold", wide); err == nil {
		t.Fatal("wide auto setting should be rejected")
	}
}

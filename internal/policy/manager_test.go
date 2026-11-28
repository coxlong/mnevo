package policy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/policy"
)

func TestFileBackedPolicyHistoryRollbackAndHotReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	mgr, err := policy.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	// Initial v1 file and archive exist with 0600 mode.
	for _, p := range []string{
		filepath.Join(dir, "strategy.json"),
		filepath.Join(dir, "strategy.history", "v1.json"),
	} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file %s stat = %+v, %v", p, info, err)
		}
	}
	if v := mgr.Active().Version; v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}

	// Update setting -> v2.
	p2, err := mgr.UpdateSetting("recall.jev_threshold", domain.Setting{
		Value: 0.45, Learn: true, Apply: "approval", Bounds: [2]float64{0.10, 0.90},
	}, "admin", "tighten jev")
	if err != nil || p2.Version != 2 || p2.Value("recall.jev_threshold") != 0.45 {
		t.Fatalf("v2 = %+v, %v", p2, err)
	}

	// Update prompt -> v3.
	p3, err := mgr.UpdatePrompt("jev_recall", domain.PromptSetting{
		Value: "Custom recall relevance prompt", Learn: true, Apply: "approval",
	}, "admin", "tune prompt")
	if err != nil || p3.Version != 3 || p3.Prompt("jev_recall") != "Custom recall relevance prompt" {
		t.Fatalf("v3 = %+v, %v", p3, err)
	}

	// Rollback to v1 -> publishes v4 restoring values while keeping current Learn=true guardrail!
	p4, err := mgr.Rollback(1, "admin")
	if err != nil || p4.Version != 4 {
		t.Fatalf("rollback = %+v, %v", p4, err)
	}
	if p4.Value("recall.jev_threshold") != 0.25 || !p4.Settings["recall.jev_threshold"].Learn {
		t.Fatalf("rollback did not restore value while keeping learn flag: %+v", p4.Settings["recall.jev_threshold"])
	}
	if p4.Prompt("jev_recall") != domain.DefaultPrompts()["jev_recall"].Value || !p4.Prompts["jev_recall"].Learn {
		t.Fatalf("rollback did not restore prompt while keeping learn flag: %+v", p4.Prompts["jev_recall"])
	}

	// Direct manual edit on disk hot-reloads transparently and bumps version.
	edited := mgr.Active()
	item := edited.Settings["recall.embedding_min"]
	item.Value = 0.55
	edited.Settings["recall.embedding_min"] = item
	raw, _ := json.MarshalIndent(edited, "", "  ")
	if err := os.WriteFile(mgr.FilePath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime advances beyond previous stat.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(mgr.FilePath(), future, future)

	reloaded := mgr.Active()
	if reloaded.Version != 5 || reloaded.Value("recall.embedding_min") != 0.55 {
		t.Fatalf("disk hot-reload failed: version=%d val=%v", reloaded.Version, reloaded.Value("recall.embedding_min"))
	}

	hist, err := mgr.History()
	if err != nil || len(hist) != 5 || hist[0].Version != 5 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

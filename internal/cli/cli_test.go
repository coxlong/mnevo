package cli_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/coxlong/mnevo/internal/cli"
	"github.com/coxlong/mnevo/internal/domain"
)

func TestParseAdminCommand(t *testing.T) {
	cmd, err := cli.ParseAdminCommand([]string{"memory", "add", `{"type":"fact","summary":"s","tags":["go"],"conditions":"c"}`})
	if err != nil || cmd.Method != http.MethodPost || cmd.Path != "/v1/memories" {
		t.Fatalf("memory add = %+v, %v", cmd, err)
	}
	if d, ok := cmd.Body.(domain.MemoryDraft); !ok || d.Summary != "s" {
		t.Fatalf("unexpected draft body = %#v", cmd.Body)
	}

	cmd, err = cli.ParseAdminCommand([]string{"model", "set", "embedding", `{"enabled":true,"url":"http://127.0.0.1:11434/v1","model":"bge-m3"}`})
	if err != nil || cmd.Method != http.MethodPut || cmd.Path != "/v1/models/embedding" {
		t.Fatalf("model set = %+v, %v", cmd, err)
	}

	cmd, err = cli.ParseAdminCommand([]string{"strategy", "set-prompt", "jev_recall", `{"value":"check relevance","learn":true,"apply":"approval"}`})
	if err != nil || cmd.Method != http.MethodPut || cmd.Path != "/v1/strategy/prompts/jev_recall" {
		t.Fatalf("strategy set-prompt = %+v, %v", cmd, err)
	}

	docPath := filepath.Join(t.TempDir(), "notes.md")
	_ = os.WriteFile(docPath, []byte("Always check errors."), 0600)
	cmd, err = cli.ParseAdminCommand([]string{"import", docPath})
	if err != nil || cmd.Method != http.MethodPost || cmd.Path != "/v1/memories/import" || cmd.Timeout != domain.LongOpBudget {
		t.Fatalf("import = %+v, %v", cmd, err)
	}

	cmd, err = cli.ParseAdminCommand([]string{"web", "set", "https://example.com/mnevo"})
	if err != nil || cmd.Method != http.MethodPut || cmd.Path != "/v1/web" {
		t.Fatalf("web set = %+v, %v", cmd, err)
	}

	if _, err := cli.ParseAdminCommand([]string{"model", "unknown"}); err == nil {
		t.Fatal("expected unknown model action error")
	}
}

func TestSetupUninstallAndStatusStopped(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	claudeDir := filepath.Join(t.TempDir(), "claude")

	if err := cli.Run([]string{"-home", homeDir, "setup", "-claude-dir", claudeDir}); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Fatalf("settings.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, ".claude.json")); err != nil {
		t.Fatalf(".claude.json missing: %v", err)
	}
	if err := cli.Run([]string{"-home", homeDir, "status"}); err != nil {
		t.Fatalf("status stopped failed: %v", err)
	}
	if err := cli.Run([]string{"-home", homeDir, "uninstall", "-claude-dir", claudeDir}); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
}

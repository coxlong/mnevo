package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/coxlong/mnevo/internal/agent/claude"
	"github.com/coxlong/mnevo/internal/daemon"
	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
	"github.com/coxlong/mnevo/internal/policy"
	"github.com/coxlong/mnevo/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is overridden at link time via -ldflags "-X github.com/coxlong/mnevo/internal/cli.Version=...".
var Version = "dev"

// Run executes the Mnevo CLI entrypoint for the given argv slice (excluding os.Args[0]).
func Run(args []string) error {
	// Fast path for Claude Code Hook & MCP adapters: `mnevo hook [HOME_DIR]` / `mnevo mcp [HOME_DIR]`.
	if len(args) >= 1 && len(args) <= 2 && (args[0] == "hook" || args[0] == "mcp") {
		override := ""
		if len(args) == 2 {
			override = args[1]
		}
		dir, err := resolveHomeDir(override)
		if err != nil {
			return err
		}
		return runAdapter(args[0], dir)
	}

	flags := flag.NewFlagSet("mnevo", flag.ContinueOnError)
	homeFlag := flags.String("home", "", "private mnevo home directory (default ~/.mnevo)")
	stateFlag := flags.String("state", "", "alias for -home")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: mnevo [-home DIR] {setup|uninstall|status|stop|serve|import|memory|model|web|strategy|audit|version} ...")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return errors.New("command required")
	}

	if args[0] == "version" {
		printVersion()
		return nil
	}

	override := *homeFlag
	if override == "" {
		override = *stateFlag
	}
	dir, err := resolveHomeDir(override)
	if err != nil {
		return err
	}
	socket := filepath.Join(dir, "mnevo.sock")

	switch args[0] {
	case "hook", "mcp":
		return runAdapter(args[0], dir)
	case "setup":
		return runSetup(dir, args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "status":
		return runStatus(dir)
	case "stop":
		return runStop(dir)
	case "serve":
		return runServe(dir, socket, args[1:])
	case "import", "audit", "memory", "model", "web", "strategy":
		return runAdmin(dir, args)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func autoStartEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("MNEVO_NO_AUTOSTART")))
	return v != "1" && v != "true" && v != "yes"
}

func runAdapter(mode, dir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := daemon.NewClient(dir, autoStartEnabled())
	defer client.Close()
	if mode == "hook" {
		return claude.RunHook(ctx, client, os.Stdin, os.Stdout)
	}
	return claude.RunMCP(ctx, client, Version, &mcp.StdioTransport{MaxLineLength: domain.MaxMessage})
}

func runSetup(dir string, args []string) error {
	opts := flag.NewFlagSet("setup", flag.ContinueOnError)
	claudeDir := opts.String("claude-dir", "", "optional Claude Code config directory override")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return errors.New("usage: mnevo setup [-claude-dir DIR]")
	}
	res, err := claude.Setup("", dir, *claudeDir)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func runUninstall(args []string) error {
	opts := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	claudeDir := opts.String("claude-dir", "", "optional Claude Code config directory override")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return errors.New("usage: mnevo uninstall [-claude-dir DIR]")
	}
	res, err := claude.Uninstall(*claudeDir)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func runStatus(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), domain.IPCBudget)
	defer cancel()
	client := daemon.NewClient(dir, false)
	defer client.Close()
	health, err := client.Health(ctx)
	if err != nil {
		return printJSON(map[string]any{
			"status":   "stopped",
			"home_dir": dir,
		})
	}
	return printJSON(health)
}

func runStop(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	key, err := readAdminKey(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return printJSON(map[string]any{"status": "stopped", "home_dir": dir})
		}
		return err
	}
	client := daemon.NewAdminClient(dir, key, false)
	defer client.Close()
	if err := client.Shutdown(ctx); err != nil {
		// If daemon is already unreachable, report stopped idempotently.
		if _, hErr := client.Health(ctx); hErr != nil {
			return printJSON(map[string]any{"status": "stopped", "home_dir": dir})
		}
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pCtx, pCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		_, hErr := client.Health(pCtx)
		pCancel()
		if hErr != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return printJSON(map[string]any{"status": "stopped", "home_dir": dir})
}

func runServe(dir, socket string, args []string) error {
	opts := flag.NewFlagSet("serve", flag.ContinueOnError)
	webAddr := opts.String("web", "", "optional 127.0.0.1:PORT management page")
	webFallback := opts.Bool("web-fallback", false, "fall back to an ephemeral 127.0.0.1 port if -web is busy")
	delay := opts.Duration("test-recall-delay", 0, "simulate slow recall for integration testing")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 || *delay < 0 {
		return errors.New("invalid serve arguments")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lock, key, err := ensureStateDirAndKey(dir)
	if err != nil {
		return err
	}
	defer lock.Close()

	polMgr, err := policy.Open(dir, time.Now)
	if err != nil {
		return err
	}
	modelReg, err := model.NewRegistry(filepath.Join(dir, "config.json"), nil)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(dir, "memory.sqlite"), polMgr, modelReg)
	if err != nil {
		return err
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := daemon.NewService(
		st,
		key,
		daemon.WithBuildInfo(Version, dir),
		daemon.WithRecallDelay(*delay),
		daemon.WithLogger(logger),
	)
	return svc.Serve(ctx, socket, *webAddr, *webFallback)
}

func runAdmin(dir string, args []string) error {
	cmd, err := ParseAdminCommand(args)
	if err != nil {
		return err
	}
	key, err := readAdminKey(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	client := daemon.NewAdminClient(dir, key, autoStartEnabled())
	defer client.Close()

	var result map[string]any
	if err := client.DoJSONWithBudget(context.Background(), cmd.Timeout, cmd.Method, cmd.Path, cmd.Body, &result); err != nil {
		return err
	}
	if st, _ := result["status"].(string); st != "" && st != "ok" && st != "jev_skipped" && st != "no_memories" {
		if msg, _ := result["message"].(string); msg != "" {
			return fmt.Errorf("%s: %s", st, msg)
		}
		return errors.New(st)
	}
	return printJSON(result)
}

func printVersion() {
	commit := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				commit = " (" + s.Value[:7] + ")"
			}
		}
	}
	fmt.Printf("mnevo %s%s\n", Version, commit)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func resolveHomeDir(override string) (string, error) {
	if override == "" {
		override = os.Getenv("MNEVO_HOME")
	}
	if override == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		override = filepath.Join(home, ".mnevo")
	}
	return filepath.Abs(override)
}

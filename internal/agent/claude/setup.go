package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var hookEvents = []string{
	"UserPromptSubmit",
	"Stop",
	"StopFailure",
	"SessionEnd",
}

const retiredHookEvent = "PostToolBatch"

// SetupResult describes the Claude Code configuration paths updated by Setup or Uninstall.
type SetupResult struct {
	Status       string `json:"status"`
	Binary       string `json:"binary,omitempty"`
	HomeDir      string `json:"home_dir,omitempty"`
	SettingsPath string `json:"settings_path"`
	MCPPath      string `json:"mcp_path"`
}

// ResolveClaudePaths returns the Claude Code hooks settings.json and user .claude.json paths.
// When claudeDir (or CLAUDE_CONFIG_DIR) is set, both files reside inside that directory;
// otherwise defaults to ~/.claude/settings.json and ~/.claude.json.
func ResolveClaudePaths(claudeDir string) (settingsPath, mcpPath string, err error) {
	dir := strings.TrimSpace(claudeDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(abs, "settings.json"), filepath.Join(abs, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json"), nil
}

// Setup idempotently registers Mnevo's Hook commands and stdio MCP server into Claude Code's
// configuration files while preserving all existing user settings, hooks, and other MCP servers.
func Setup(binaryPath, homeDir, claudeDir string) (SetupResult, error) {
	if binaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return SetupResult{}, err
		}
		binaryPath = exe
	}
	absBin, err := filepath.Abs(binaryPath)
	if err != nil {
		return SetupResult{}, err
	}
	absHome, err := filepath.Abs(homeDir)
	if err != nil {
		return SetupResult{}, err
	}
	settingsPath, mcpPath, err := ResolveClaudePaths(claudeDir)
	if err != nil {
		return SetupResult{}, err
	}

	// 1. Merge Mnevo hooks into settings.json.
	settingsDoc, err := readJSONMap(settingsPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read %s: %w", settingsPath, err)
	}
	hooksMap, ok := settingsDoc["hooks"].(map[string]any)
	if !ok || hooksMap == nil {
		hooksMap = map[string]any{}
	}
	hookCmd := shellQuote(absBin) + " hook " + shellQuote(absHome)
	mnevoEntry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": hookCmd,
				"timeout": 5,
			},
		},
	}
	for _, ev := range hookEvents {
		existing := filterOutMnevoHookGroups(hooksMap[ev])
		hooksMap[ev] = append(existing, mnevoEntry)
	}
	removeMnevoHooks(hooksMap, retiredHookEvent)
	settingsDoc["hooks"] = hooksMap
	if err := writeJSONAtomic(settingsPath, settingsDoc); err != nil {
		return SetupResult{}, fmt.Errorf("write %s: %w", settingsPath, err)
	}

	// 2. Merge Mnevo stdio MCP server into .claude.json.
	mcpDoc, err := readJSONMap(mcpPath)
	if err != nil {
		return SetupResult{}, fmt.Errorf("read %s: %w", mcpPath, err)
	}
	servers, ok := mcpDoc["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = map[string]any{}
	}
	servers["mnevo"] = map[string]any{
		"type":    "stdio",
		"command": absBin,
		"args":    []string{"mcp", absHome},
	}
	mcpDoc["mcpServers"] = servers
	if err := writeJSONAtomic(mcpPath, mcpDoc); err != nil {
		return SetupResult{}, fmt.Errorf("write %s: %w", mcpPath, err)
	}

	return SetupResult{
		Status:       "ok",
		Binary:       absBin,
		HomeDir:      absHome,
		SettingsPath: settingsPath,
		MCPPath:      mcpPath,
	}, nil
}

// Uninstall removes Mnevo's Hook commands and MCP server entry from Claude Code's configuration
// while preserving all other user settings.
func Uninstall(claudeDir string) (SetupResult, error) {
	settingsPath, mcpPath, err := ResolveClaudePaths(claudeDir)
	if err != nil {
		return SetupResult{}, err
	}

	if _, err := os.Stat(settingsPath); err == nil {
		settingsDoc, err := readJSONMap(settingsPath)
		if err != nil {
			return SetupResult{}, fmt.Errorf("read %s: %w", settingsPath, err)
		}
		if hooksMap, ok := settingsDoc["hooks"].(map[string]any); ok && hooksMap != nil {
			removeMnevoHooks(hooksMap, hookEvents...)
			removeMnevoHooks(hooksMap, retiredHookEvent)
			if len(hooksMap) == 0 {
				delete(settingsDoc, "hooks")
			} else {
				settingsDoc["hooks"] = hooksMap
			}
			if err := writeJSONAtomic(settingsPath, settingsDoc); err != nil {
				return SetupResult{}, fmt.Errorf("write %s: %w", settingsPath, err)
			}
		}
	}

	if _, err := os.Stat(mcpPath); err == nil {
		mcpDoc, err := readJSONMap(mcpPath)
		if err != nil {
			return SetupResult{}, fmt.Errorf("read %s: %w", mcpPath, err)
		}
		if servers, ok := mcpDoc["mcpServers"].(map[string]any); ok && servers != nil {
			if _, exists := servers["mnevo"]; exists {
				delete(servers, "mnevo")
				if len(servers) == 0 {
					delete(mcpDoc, "mcpServers")
				} else {
					mcpDoc["mcpServers"] = servers
				}
				if err := writeJSONAtomic(mcpPath, mcpDoc); err != nil {
					return SetupResult{}, fmt.Errorf("write %s: %w", mcpPath, err)
				}
			}
		}
	}

	return SetupResult{
		Status:       "ok",
		SettingsPath: settingsPath,
		MCPPath:      mcpPath,
	}, nil
}

func removeMnevoHooks(hooksMap map[string]any, events ...string) {
	for _, event := range events {
		remaining := filterOutMnevoHookGroups(hooksMap[event])
		if len(remaining) == 0 {
			delete(hooksMap, event)
		} else {
			hooksMap[event] = remaining
		}
	}
}

func filterOutMnevoHookGroups(raw any) []any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	kept := make([]any, 0, len(items))
	for _, item := range items {
		group, ok := item.(map[string]any)
		if !ok {
			kept = append(kept, item)
			continue
		}
		hooks, ok := group["hooks"].([]any)
		if !ok || len(hooks) == 0 {
			kept = append(kept, item)
			continue
		}
		var remaining []any
		for _, h := range hooks {
			entry, _ := h.(map[string]any)
			cmd, _ := entry["command"].(string)
			if !isMnevoHookCommand(cmd) {
				remaining = append(remaining, h)
			}
		}
		if len(remaining) > 0 {
			group["hooks"] = remaining
			kept = append(kept, group)
		}
	}
	return kept
}

func isMnevoHookCommand(cmd string) bool {
	tokens := splitShellTokens(strings.TrimSpace(cmd))
	if len(tokens) < 2 {
		return false
	}
	if tokens[0] == "exec" && len(tokens) >= 3 {
		tokens = tokens[1:]
	}
	return filepath.Base(tokens[0]) == "mnevo" && tokens[1] == "hook"
}

func splitShellTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	hasToken := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(ch)
			}
		case inDouble:
			if ch == '"' {
				inDouble = false
			} else {
				cur.WriteByte(ch)
			}
		case ch == '\'':
			inSingle = true
			hasToken = true
		case ch == '"':
			inDouble = true
			hasToken = true
		case ch == ' ' || ch == '\t':
			if hasToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteByte(ch)
			hasToken = true
		}
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		isSafe := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '-' || c == '_' || c == ':'
		if !isSafe {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func readJSONMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func writeJSONAtomic(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, encoded, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

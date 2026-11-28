package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
)

// Manager manages the file-backed strategy (~/.mnevo/strategy.json) and its
// immutable version archive (~/.mnevo/strategy.history/vN.json).
// It supports direct human edits to strategy.json via transparent hot-reload as well
// as atomic versioned publishes and rollbacks from Web/CLI/meta-learning.
type Manager struct {
	mu         sync.RWMutex
	filePath   string
	historyDir string
	now        func() time.Time
	active     domain.Policy
	modTime    time.Time
	size       int64
}

// Open initializes or loads strategy.json inside dir (creating v1 defaults if absent).
func Open(dir string, now func() time.Time) (*Manager, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	histDir := filepath.Join(dir, "strategy.history")
	if err := os.MkdirAll(histDir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{
		filePath:   filepath.Join(dir, "strategy.json"),
		historyDir: histDir,
		now:        now,
	}

	if _, err := os.Stat(m.filePath); errors.Is(err, os.ErrNotExist) {
		initial := domain.DefaultPolicy(now().UnixMilli())
		if err := m.writeAtomicLocked(initial); err != nil {
			return nil, err
		}
		m.active = initial
		m.recordFileStatLocked()
		return m, nil
	} else if err != nil {
		return nil, err
	}

	loaded, err := readPolicyFile(m.filePath)
	if err != nil {
		return nil, fmt.Errorf("load strategy.json: %w", err)
	}
	if loaded.Version <= 0 {
		loaded.Version = 1
	}
	if err := domain.ValidatePolicy(loaded); err != nil {
		return nil, fmt.Errorf("invalid strategy.json: %w", err)
	}
	// Ensure current version is also archived in strategy.history.
	histFile := m.versionPath(loaded.Version)
	if _, err := os.Stat(histFile); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON0600(histFile, loaded); err != nil {
			return nil, err
		}
	}
	m.active = loaded
	m.recordFileStatLocked()
	return m, nil
}

// Active returns the currently active Policy, transparently hot-reloading strategy.json
// if it was edited directly on disk.
func (m *Manager) Active() domain.Policy {
	m.mu.RLock()
	info, err := os.Stat(m.filePath)
	unchanged := err == nil && info.ModTime().Equal(m.modTime) && info.Size() == m.size
	cached := clonePolicy(m.active)
	m.mu.RUnlock()
	if unchanged || err != nil {
		return cached
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.reloadLocked()
	return clonePolicy(m.active)
}

func (m *Manager) reloadLocked() error {
	info, err := os.Stat(m.filePath)
	if err != nil || (info.ModTime().Equal(m.modTime) && info.Size() == m.size) {
		return nil
	}
	loaded, err := readPolicyFile(m.filePath)
	if err == nil {
		err = domain.ValidatePolicy(loaded)
	}
	if err != nil {
		slog.Warn("ignoring invalid manual edit to strategy.json; keeping active version",
			"version", m.active.Version, "error", err)
		return nil
	}
	if loaded.Version <= m.active.Version {
		loaded.Version = m.active.Version + 1
		if loaded.Actor == "" {
			loaded.Actor = "manual-file-edit"
		}
		if loaded.Reason == "" {
			loaded.Reason = "edited strategy.json on disk"
		}
		loaded.UpdatedAt = m.now().UnixMilli()
		err = m.writeAtomicLocked(loaded)
	} else {
		err = writeJSON0600(m.versionPath(loaded.Version), loaded)
	}
	if err != nil {
		slog.Warn("failed to archive manual strategy change", "version", loaded.Version, "error", err)
		return err
	}
	m.active = loaded
	m.recordFileStatLocked()
	return nil
}

// LoadVersion loads a specific archived version (or Active() when version == 0).
func (m *Manager) LoadVersion(version int) (domain.Policy, error) {
	if version <= 0 {
		return m.Active(), nil
	}
	return readPolicyFile(m.versionPath(version))
}

// History returns all archived strategy revisions sorted newest-first.
func (m *Manager) History() ([]domain.PolicyRevision, error) {
	entries, err := os.ReadDir(m.historyDir)
	if err != nil {
		return nil, err
	}
	var revisions []domain.PolicyRevision
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "v") || !strings.HasSuffix(name, ".json") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "v"), ".json")
		if _, err := strconv.Atoi(numStr); err != nil {
			continue
		}
		p, err := readPolicyFile(filepath.Join(m.historyDir, name))
		if err != nil {
			continue
		}
		revisions = append(revisions, domain.PolicyRevision{
			Version:   p.Version,
			Actor:     p.Actor,
			Reason:    p.Reason,
			UpdatedAt: p.UpdatedAt,
		})
	}
	slices.SortFunc(revisions, func(a, b domain.PolicyRevision) int {
		if a.Version > b.Version {
			return -1
		}
		if a.Version < b.Version {
			return 1
		}
		return 0
	})
	return revisions, nil
}

// UpdateSetting validates and publishes a single numeric setting update.
func (m *Manager) UpdateSetting(key string, item domain.Setting, actor, reason string) (domain.Policy, error) {
	return m.Mutate(actor, reason, func(p *domain.Policy) error {
		if err := domain.ValidateSetting(key, item); err != nil {
			return err
		}
		p.Settings[key] = item
		return nil
	})
}

// UpdatePrompt validates and publishes a single prompt update.
func (m *Manager) UpdatePrompt(key string, item domain.PromptSetting, actor, reason string) (domain.Policy, error) {
	return m.Mutate(actor, reason, func(p *domain.Policy) error {
		if err := domain.ValidatePrompt(key, item); err != nil {
			return err
		}
		p.Prompts[key] = item
		return nil
	})
}

// Rollback restores all numeric values and prompt values from targetVersion while
// preserving current learn/apply/bounds guardrails, publishing as a new version.
func (m *Manager) Rollback(targetVersion int, actor string) (domain.Policy, error) {
	if targetVersion <= 0 {
		return domain.Policy{}, errors.New("invalid rollback version")
	}
	old, err := m.LoadVersion(targetVersion)
	if err != nil {
		return domain.Policy{}, err
	}
	return m.Mutate(actor, fmt.Sprintf("restore values from version %d", targetVersion), func(p *domain.Policy) error {
		for k, prior := range old.Settings {
			cur := p.Settings[k]
			if prior.Value < cur.Bounds[0] || prior.Value > cur.Bounds[1] {
				return fmt.Errorf("%s: historical value %v outside current bounds %v", k, prior.Value, cur.Bounds)
			}
			cur.Value = prior.Value
			p.Settings[k] = cur
		}
		for k, prior := range old.Prompts {
			cur := p.Prompts[k]
			cur.Value = prior.Value
			p.Prompts[k] = cur
		}
		return nil
	})
}

// Mutate atomically loads active policy, applies fn, validates, archives, and replaces strategy.json.
func (m *Manager) Mutate(actor, reason string, fn func(*domain.Policy) error) (domain.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadLocked(); err != nil {
		return domain.Policy{}, err
	}

	next := clonePolicy(m.active)
	if err := fn(&next); err != nil {
		return domain.Policy{}, err
	}
	if err := domain.ValidatePolicy(next); err != nil {
		return domain.Policy{}, err
	}
	next.Version = m.active.Version + 1
	next.Actor = actor
	next.Reason = reason
	next.UpdatedAt = m.now().UnixMilli()

	if err := m.writeAtomicLocked(next); err != nil {
		return domain.Policy{}, err
	}
	m.active = next
	m.recordFileStatLocked()
	return clonePolicy(next), nil
}

// FilePath returns the absolute path to strategy.json.
func (m *Manager) FilePath() string { return m.filePath }

func (m *Manager) versionPath(version int) string {
	return filepath.Join(m.historyDir, fmt.Sprintf("v%d.json", version))
}

func (m *Manager) writeAtomicLocked(p domain.Policy) error {
	if err := writeJSON0600(m.versionPath(p.Version), p); err != nil {
		return err
	}
	return writeJSON0600(m.filePath, p)
}

func (m *Manager) recordFileStatLocked() {
	if info, err := os.Stat(m.filePath); err == nil {
		m.modTime = info.ModTime()
		m.size = info.Size()
	}
}

func readPolicyFile(path string) (domain.Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Policy{}, err
	}
	var p domain.Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return domain.Policy{}, err
	}
	return p, nil
}

func writeJSON0600(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func clonePolicy(p domain.Policy) domain.Policy {
	out := p
	out.Settings = maps.Clone(p.Settings)
	out.Prompts = maps.Clone(p.Prompts)
	return out
}

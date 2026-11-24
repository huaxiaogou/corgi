package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// AllEfforts lists standard thinking effort levels supported across agent CLIs.
var AllEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// ModelNameRe validates CLI-safe model and effort flags.
var ModelNameRe = regexp.MustCompile(`^[A-Za-z0-9._:/@\[\]-]{0,120}$`)

// Adapter encapsulates CLI-specific differences for Claude Code, Codex, Antigravity, and CodeBuddy.
type Adapter interface {
	Kind() string
	DisplayName() string
	LaunchArgs(modelName, effort string, bypassPerm bool) []string
	ResumeArgs(sessionID string) []string
	PreTrustWorkspace(home, cwd string) error
	DiscoverModels(ctx context.Context, home string) (*model.ModelCatalog, error)
	LocateTranscript(home, cwd, sessionID string, since time.Time) string
	ParseTranscriptLine(line []byte, imgDir string) ([]*model.Message, bool)
	ReadContextUsage(transcriptPath string, claudeWindow int) model.ContextUsage
	LookupTitle(home, sessionID, terminalTitle string) string
}

// NormalizeKind canonicalizes user-facing agent kind names to herdr kinds.
func NormalizeKind(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "claude", "claude-code":
		return "claude"
	case "codex":
		return "codex"
	case "agy", "antigravity", "antigravity-cli":
		return "agy"
	case "codebuddy", "cbc", "codebuddy-code":
		return "codebuddy"
	default:
		return ""
	}
}

// Registry manages all supported AgentAdapters and instance-scoped caches.
type Registry struct {
	adapters map[string]Adapter

	mu          sync.Mutex
	modelCache  map[string]cachedCatalog
	installedAt time.Time
	installed   []string
}

type cachedCatalog struct {
	at      time.Time
	catalog *model.ModelCatalog
}

// NewRegistry returns a Registry populated with Claude, Codex, Antigravity, and CodeBuddy adapters.
func NewRegistry() *Registry {
	r := &Registry{
		adapters:   map[string]Adapter{},
		modelCache: map[string]cachedCatalog{},
	}
	for _, a := range []Adapter{&ClaudeAdapter{}, &CodexAdapter{}, &AgyAdapter{}, &CodeBuddyAdapter{}} {
		r.adapters[a.Kind()] = a
	}
	return r
}

// Get returns the adapter for raw kind (accepts aliases like "antigravity" or "cbc").
func (r *Registry) Get(rawKind string) (Adapter, error) {
	k := NormalizeKind(rawKind)
	a, ok := r.adapters[k]
	if !ok {
		return nil, fmt.Errorf("unsupported agent kind %q", rawKind)
	}
	return a, nil
}

// SupportedKinds returns canonical supported kinds in display order.
func (r *Registry) SupportedKinds() []string {
	return []string{"claude", "codex", "agy", "codebuddy"}
}

// DetectInstalled returns canonical kinds installed on the local machine (cached for 1 minute).
func (r *Registry) DetectInstalled(home string, refresh bool) []string {
	r.mu.Lock()
	if !refresh && r.installed != nil && time.Since(r.installedAt) < time.Minute {
		cp := append([]string(nil), r.installed...)
		r.mu.Unlock()
		return cp
	}
	r.mu.Unlock()

	found := []string{}
	for _, k := range r.SupportedKinds() {
		if herdr.FindBin(k, home) != "" {
			found = append(found, k)
		}
	}

	r.mu.Lock()
	r.installed = found
	r.installedAt = time.Now()
	r.mu.Unlock()
	return append([]string(nil), found...)
}

// CheckInstalled validates that kind exists on the local host.
func (r *Registry) CheckInstalled(home, rawKind string) error {
	a, err := r.Get(rawKind)
	if err != nil {
		return err
	}
	if herdr.FindBin(a.Kind(), home) == "" {
		return fmt.Errorf("本机未安装 %s（找不到 %s 命令）", a.DisplayName(), a.Kind())
	}
	return nil
}

// Models discovers available models for rawKind with 10-minute instance caching.
func (r *Registry) Models(ctx context.Context, home, rawKind string, refresh bool) (*model.ModelCatalog, error) {
	a, err := r.Get(rawKind)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	c, ok := r.modelCache[a.Kind()]
	r.mu.Unlock()
	if ok && !refresh && time.Since(c.at) < 10*time.Minute {
		return c.catalog, nil
	}
	cat, err := a.DiscoverModels(ctx, home)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.modelCache[a.Kind()] = cachedCatalog{at: time.Now(), catalog: cat}
	r.mu.Unlock()
	return cat, nil
}

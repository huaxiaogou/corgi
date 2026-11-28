package updater

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	defaultReleaseRepoURL       = "https://github.com/huaxiaogou/corgi"
	RelayUpdateTagHeader        = "X-Corgi-Update-Tag"
	RelayUpdateSHAHeader        = "X-Corgi-Update-SHA256"
	selfUpdateTimeout           = 2*time.Minute + 30*time.Second
	remoteUpdateForwardTimeout  = 2*time.Minute + 50*time.Second
	updateLatestAttemptTimeout  = 20 * time.Second
	updateFileAttemptTimeout    = 30 * time.Second
	updateBinAttemptTimeout     = 2 * time.Minute
	updateIdleReadTimeout       = 25 * time.Second
	updateMaxAttempts           = 3
	defaultUpdateRetryBase      = time.Second
	maxUpdateBinarySize         = int64(128 << 20)
	autoUpdateAttemptsKVKey     = "auto_update_attempts"
	defaultNodeReconnectTimeout = 25 * time.Second
	autoUpdateRetryCap          = 24 * time.Hour
)

var supportedReleasePlatforms = map[string]bool{
	"linux/amd64":  true,
	"linux/arm64":  true,
	"darwin/amd64": true,
	"darwin/arm64": true,
}

// FleetProvider abstracts live Hub/Node machine discovery and tunnel clients.
type FleetProvider interface {
	Machines() []model.Machine
	RemoteMachineByID(id string) (model.Machine, *http.Client, bool)
	SubscribeChanges() (<-chan struct{}, func())
}

// DeviceLister lists paired machine metadata (including offline machines).
type DeviceLister interface {
	List(ctx context.Context) ([]*model.Device, error)
}

// KVStore persists bounded auto-update attempt records.
type KVStore interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, val string) error
}

// RuntimeGuard coordinates idle reservation and post-upgrade events with core.Engine.
type RuntimeGuard interface {
	Settings() model.Settings
	PostEvent(ctx context.Context, thread, text, ref, tone string)
	TryReserveHubRestart(ctx context.Context) (func(), bool, string)
	RecordPendingAutoUpdateNotice(ctx context.Context, target string, machines []string) error
}

// Config wires runtime dependencies and dynamic config getters into a Manager.
type Config struct {
	Version              func() string
	ServerCtx            func() context.Context
	ReleaseRepoURL       func() string
	ExecutablePath       func() string
	ExecFn               func() func(string)
	RetryBaseDelay       func() time.Duration
	NodeReconnectTimeout func() time.Duration

	Fleet              FleetProvider
	Devices            DeviceLister
	KV                 KVStore
	Runtime            RuntimeGuard
	Logger             *slog.Logger
	OnStateChanged     func()
	FlushBeforeRestart func(ctx context.Context) error
}

// OutdatedMachine summarizes one online machine running an older release version.
type OutdatedMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// VersionMachine summarizes one Hub or paired Node machine in /api/version.
type VersionMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Local   bool   `json:"local"`
	Online  bool   `json:"online"`
}

// FleetUpdateView is the JSON snapshot exposed in GET /api/version and POST /api/update/fleet.
type FleetUpdateView struct {
	Busy         bool              `json:"busy"`
	Scope        string            `json:"scope,omitempty"` // fleet | auto
	Phase        string            `json:"phase"`           // idle | updating | reconnecting | restarting | done | failed
	Target       string            `json:"target,omitempty"`
	Total        int               `json:"total"`
	Index        int               `json:"index"`
	CurrentID    string            `json:"current_id,omitempty"`
	CurrentName  string            `json:"current_name,omitempty"`
	CompletedIDs []string          `json:"completed_ids,omitempty"`
	PendingIDs   []string          `json:"pending_ids,omitempty"`
	FailedID     string            `json:"failed_id,omitempty"`
	FailedName   string            `json:"failed_name,omitempty"`
	Error        string            `json:"error,omitempty"`
	Errors       map[string]string `json:"errors,omitempty"`
}

// VersionSnapshot is the response payload for GET /api/version.
type VersionSnapshot struct {
	Version  string            `json:"version"`
	Latest   string            `json:"latest"`
	Outdated []OutdatedMachine `json:"outdated"`
	Machines []VersionMachine  `json:"machines"`
	Update   FleetUpdateView   `json:"update"`
}

// LocalUpdateResponse holds the HTTP status, JSON body, and optional post-flush exec callback
// for synchronous local or relayed self-updates.
type LocalUpdateResponse struct {
	Status     int
	Payload    map[string]any
	AfterFlush func()
}

// UpdateError classifies release download, verification, or local disk errors.
type UpdateError struct {
	Code      string
	Relayable bool
	Msg       string
	Err       error
}

func (e *UpdateError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "升级失败"
}

func (e *UpdateError) Unwrap() error { return e.Err }

type updateTarget struct {
	ID    string
	Name  string
	Local bool
}

type fleetUpdateState struct {
	view     FleetUpdateView
	plan     []updateTarget
	pending  []updateTarget
	upgraded []string
}

// Manager owns release lookup, binary verification, Hub-relayed node updates,
// rolling fleet updates, and bounded overnight auto-updates.
type Manager struct {
	cfg Config

	updateOnce    sync.Once
	updateMu      sync.Mutex
	latestTag     string
	latestChecked time.Time
	latestBusy    bool

	selfUpdateMu  sync.Mutex
	fleetUpdateMu sync.Mutex
	fleetUpdate   *fleetUpdateState
}

// NewManager creates an isolated update Manager with zero package-level mutable state.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg}
}

func (m *Manager) version() string {
	if m.cfg.Version != nil {
		return m.cfg.Version()
	}
	return ""
}

func (m *Manager) serverCtx() context.Context {
	if m.cfg.ServerCtx != nil {
		if ctx := m.cfg.ServerCtx(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

func (m *Manager) repoBaseURL() string {
	if m.cfg.ReleaseRepoURL != nil {
		if u := strings.TrimSpace(m.cfg.ReleaseRepoURL()); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return defaultReleaseRepoURL
}

func (m *Manager) effectiveRetryBaseDelay() time.Duration {
	if m.cfg.RetryBaseDelay != nil {
		if d := m.cfg.RetryBaseDelay(); d > 0 {
			return d
		}
	}
	return defaultUpdateRetryBase
}

func (m *Manager) effectiveNodeReconnectTimeout() time.Duration {
	if m.cfg.NodeReconnectTimeout != nil {
		if d := m.cfg.NodeReconnectTimeout(); d > 0 {
			return d
		}
	}
	return defaultNodeReconnectTimeout
}

func parseSemver(v string) []int {
	v, ok := strings.CutPrefix(strings.TrimSpace(v), "v")
	if !ok {
		return nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return nil
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// IsOutdatedVersion reports whether semver string have is strictly older than latest.
func IsOutdatedVersion(have, latest string) bool {
	a, b := parseSemver(have), parseSemver(latest)
	if a == nil || b == nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func parsePlatformOSArch(raw string) (goos, goarch string, ok bool) {
	parts := strings.Split(strings.TrimSpace(strings.ToLower(raw)), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	key := parts[0] + "/" + parts[1]
	if !supportedReleasePlatforms[key] {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// SanitizeNodeExecArgs strips consumed one-time pairing flags (--code, --pair-only) so
// foreground-started nodes cleanly re-exec using their persisted ~/.corgi/node.json.
func SanitizeNodeExecArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--code" || a == "-code":
			if i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(a, "--code=") || strings.HasPrefix(a, "-code="):
			continue
		case a == "--pair-only" || a == "-pair-only" || strings.HasPrefix(a, "--pair-only=") || strings.HasPrefix(a, "-pair-only="):
			continue
		}
		out = append(out, a)
	}
	return out
}

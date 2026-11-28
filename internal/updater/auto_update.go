package updater

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// TryAutoUpdate implements core.AutoUpdater. It runs only when Settings.AutoUpdate is true,
// the Hub runs a release semver build, and at least one outdated online machine has not been
// attempted for the target tag within the rolling 24-hour cap.
func (m *Manager) TryAutoUpdate(ctx context.Context) {
	if m.cfg.Runtime == nil || m.cfg.KV == nil || !m.cfg.Runtime.Settings().AutoUpdate {
		return
	}
	if parseSemver(m.version()) == nil || m.IsFleetBusy() {
		return
	}

	lctx, cancel := context.WithTimeout(ctx, updateLatestAttemptTimeout*2)
	latest, _, err := m.fetchLatestTagErr(lctx)
	cancel()
	if err != nil || latest == "" {
		return
	}
	m.recordLatestTag(latest)

	now := time.Now()
	attempts := m.LoadAutoUpdateAttempts(ctx)
	eligible := map[string]bool{}
	for _, mc := range m.CollectVersionMachines(ctx) {
		if !(mc.Local || mc.Online) || !IsOutdatedVersion(mc.Version, latest) {
			continue
		}
		key := autoUpdateAttemptKey(mc.ID, latest)
		if prevAt, ok := attempts[key]; ok && now.Sub(prevAt) < autoUpdateRetryCap {
			continue
		}
		eligible[mc.ID] = true
	}
	if len(eligible) == 0 {
		return
	}

	// Persist attempt timestamps BEFORE starting so crashes or failed upgrades never hot-loop.
	for id := range eligible {
		attempts[autoUpdateAttemptKey(id, latest)] = now
	}
	m.saveAutoUpdateAttempts(ctx, attempts, now)

	if m.cfg.Logger != nil {
		m.cfg.Logger.Info("starting opt-in overnight auto-update", "target", latest, "machines", len(eligible))
	}
	_, _, _ = m.StartFleetUpdate(ctx, "auto", func(id string) bool {
		return eligible[id]
	})
}

func autoUpdateAttemptKey(machineID, tag string) string {
	if strings.TrimSpace(machineID) == "" {
		return "hub@" + tag
	}
	return machineID + "@" + tag
}

// LoadAutoUpdateAttempts reads the persisted 24h auto-update attempt table from KV.
func (m *Manager) LoadAutoUpdateAttempts(ctx context.Context) map[string]time.Time {
	out := map[string]time.Time{}
	if m.cfg.KV == nil {
		return out
	}
	raw, ok, err := m.cfg.KV.GetKV(ctx, autoUpdateAttemptsKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func (m *Manager) saveAutoUpdateAttempts(ctx context.Context, attempts map[string]time.Time, now time.Time) {
	if m.cfg.KV == nil {
		return
	}
	pruned := make(map[string]time.Time, len(attempts))
	for k, t := range attempts {
		if now.Sub(t) < 7*24*time.Hour {
			pruned[k] = t
		}
	}
	if b, err := json.Marshal(pruned); err == nil {
		_ = m.cfg.KV.SetKV(ctx, autoUpdateAttemptsKVKey, string(b))
	}
}

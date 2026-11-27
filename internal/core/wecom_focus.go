package core

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	// WeComFocusKVPrefix namespaces all persisted WeCom direct-chat focus records in SQLite kv.
	WeComFocusKVPrefix = "wecom_focus:"
	// DefaultWeComFocusTTL is the built-in inactivity timeout after which a WeCom direct-chat
	// focus automatically returns to Corgi main chat.
	DefaultWeComFocusTTL = 60 * time.Minute
)

// WeComFocus stores a user's persisted WeCom direct-chat binding to one Worker.
type WeComFocus struct {
	Worker     string    `json:"worker"`
	Title      string    `json:"title,omitempty"`
	Machine    string    `json:"machine,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Session    string    `json:"session,omitempty"`
	WorkItemID string    `json:"work_item_id,omitempty"`
	EnteredAt  time.Time `json:"entered_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// WeComFocusLoadResult describes the outcome of loading a persisted WeCom focus entry.
type WeComFocusLoadResult struct {
	Focus         *WeComFocus
	Expired       bool
	ExpiredReason string // "ttl" | "legacy" | "ended"
	ExpiredWorker string
	ExpiredTitle  string
	TTL           time.Duration
}

func (e *Engine) wecomFocusTTL() time.Duration {
	return DefaultWeComFocusTTL
}

// LoadWeComFocus loads the active WeCom focus for key, enforcing TTL, legacy timestamp upgrades,
// and target Worker liveness. Stale or ended entries are lazily removed from SQLite.
func (e *Engine) LoadWeComFocus(ctx context.Context, key string) WeComFocusLoadResult {
	return e.loadWeComFocusAt(ctx, key, time.Now(), true)
}

func (e *Engine) loadWeComFocusAt(ctx context.Context, key string, now time.Time, notifyOnCleanup bool) WeComFocusLoadResult {
	ttl := e.wecomFocusTTL()
	res := WeComFocusLoadResult{TTL: ttl}
	key = strings.TrimSpace(key)
	if e.store == nil || key == "" {
		return res
	}
	raw, ok, err := e.store.GetKV(ctx, key)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return res
	}
	var f WeComFocus
	if json.Unmarshal([]byte(raw), &f) != nil || strings.TrimSpace(f.Worker) == "" {
		_ = e.store.DeleteKV(ctx, key)
		if notifyOnCleanup {
			e.notifyChanged()
		}
		return res
	}
	f.Worker = strings.TrimSpace(f.Worker)
	if f.UpdatedAt.IsZero() {
		_ = e.store.DeleteKV(ctx, key)
		if notifyOnCleanup {
			e.notifyChanged()
		}
		res.Expired = true
		res.ExpiredReason = "legacy"
		res.ExpiredWorker = f.Worker
		res.ExpiredTitle = strings.TrimSpace(f.Title)
		return res
	}
	if now.Sub(f.UpdatedAt) >= ttl {
		_ = e.store.DeleteKV(ctx, key)
		if notifyOnCleanup {
			e.notifyChanged()
		}
		res.Expired = true
		res.ExpiredReason = "ttl"
		res.ExpiredWorker = f.Worker
		res.ExpiredTitle = strings.TrimSpace(f.Title)
		return res
	}
	if w, wErr := e.store.GetWorker(ctx, f.Worker); wErr == nil && w != nil && w.Ended {
		_ = e.store.DeleteKV(ctx, key)
		if notifyOnCleanup {
			e.notifyChanged()
		}
		res.Expired = true
		res.ExpiredReason = "ended"
		res.ExpiredWorker = f.Worker
		res.ExpiredTitle = orStr(strings.TrimSpace(f.Title), strings.TrimSpace(w.Title))
		return res
	}
	if f.EnteredAt.IsZero() {
		f.EnteredAt = f.UpdatedAt
	}
	res.Focus = &f
	return res
}

// SaveWeComFocus writes or clears a WeCom direct-chat focus entry with fresh timestamps
// and notifies Web SSE subscribers.
func (e *Engine) SaveWeComFocus(ctx context.Context, key string, f *WeComFocus) error {
	key = strings.TrimSpace(key)
	if e.store == nil || key == "" {
		return nil
	}
	if f == nil || strings.TrimSpace(f.Worker) == "" {
		_, err := e.ClearWeComFocus(ctx, key)
		return err
	}
	cp := *f
	cp.Worker = strings.TrimSpace(cp.Worker)
	now := time.Now()
	if cp.EnteredAt.IsZero() {
		if raw, ok, err := e.store.GetKV(ctx, key); err == nil && ok && strings.TrimSpace(raw) != "" {
			var prev WeComFocus
			if json.Unmarshal([]byte(raw), &prev) == nil && prev.Worker == cp.Worker && !prev.EnteredAt.IsZero() {
				cp.EnteredAt = prev.EnteredAt
			}
		}
		if cp.EnteredAt.IsZero() {
			cp.EnteredAt = now
		}
	}
	cp.UpdatedAt = now
	raw, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	if err := e.store.SetKV(ctx, key, string(raw)); err != nil {
		return err
	}
	e.notifyChanged()
	return nil
}

// TouchWeComFocus refreshes UpdatedAt when key still points to workerName.
// It is a no-op if the user already switched away or returned to Corgi main chat.
func (e *Engine) TouchWeComFocus(ctx context.Context, key, workerName string) {
	key = strings.TrimSpace(key)
	workerName = strings.TrimSpace(workerName)
	if e.store == nil || key == "" || workerName == "" {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, key)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var f WeComFocus
	if json.Unmarshal([]byte(raw), &f) != nil || strings.TrimSpace(f.Worker) != workerName {
		return
	}
	now := time.Now()
	if f.EnteredAt.IsZero() {
		f.EnteredAt = now
	}
	f.UpdatedAt = now
	if b, err := json.Marshal(&f); err == nil {
		_ = e.store.SetKV(ctx, key, string(b))
	}
}

// ClearWeComFocus removes one persisted WeCom focus entry and notifies subscribers if it existed.
func (e *Engine) ClearWeComFocus(ctx context.Context, key string) (bool, error) {
	key = strings.TrimSpace(key)
	if e.store == nil || key == "" {
		return false, nil
	}
	_, existed, err := e.store.GetKV(ctx, key)
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}
	if err := e.store.DeleteKV(ctx, key); err != nil {
		return false, err
	}
	e.notifyChanged()
	return true, nil
}

// ClearAllWeComFocuses removes every persisted wecom_focus:* entry (used by Web one-click reset).
func (e *Engine) ClearAllWeComFocuses(ctx context.Context) (int, error) {
	if e.store == nil {
		return 0, nil
	}
	items, err := e.store.ListKVByPrefix(ctx, WeComFocusKVPrefix)
	if err != nil {
		return 0, err
	}
	removed := 0
	for k := range items {
		if err := e.store.DeleteKV(ctx, k); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		e.notifyChanged()
	}
	return removed, nil
}

// ClearWeComFocusForWorker clears all persisted wecom_focus:* entries pointing to workerName.
func (e *Engine) ClearWeComFocusForWorker(ctx context.Context, workerName string) int {
	workerName = strings.TrimSpace(workerName)
	if e.store == nil || workerName == "" {
		return 0
	}
	return e.clearWeComFocusMatching(ctx, func(f *WeComFocus) bool {
		return strings.TrimSpace(f.Worker) == workerName
	})
}

// ClearWeComFocusForWorkItem clears all persisted wecom_focus:* entries pointing to a closed
// WorkItem or any Worker associated with it.
func (e *Engine) ClearWeComFocusForWorkItem(ctx context.Context, workItemID string, workers []string) int {
	workItemID = strings.TrimSpace(workItemID)
	if e.store == nil || (workItemID == "" && len(workers) == 0) {
		return 0
	}
	return e.clearWeComFocusMatching(ctx, func(f *WeComFocus) bool {
		if workItemID != "" && strings.TrimSpace(f.WorkItemID) == workItemID {
			return true
		}
		return slices.Contains(workers, strings.TrimSpace(f.Worker))
	})
}

func (e *Engine) clearWeComFocusMatching(ctx context.Context, match func(*WeComFocus) bool) int {
	items, err := e.store.ListKVByPrefix(ctx, WeComFocusKVPrefix)
	if err != nil || len(items) == 0 {
		return 0
	}
	removed := 0
	for k, raw := range items {
		var f WeComFocus
		if json.Unmarshal([]byte(raw), &f) != nil {
			continue
		}
		if match(&f) {
			if err := e.store.DeleteKV(ctx, k); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		e.notifyChanged()
	}
	return removed
}

func wecomFocusScope(key string) string {
	rest := strings.TrimPrefix(strings.TrimSpace(key), WeComFocusKVPrefix)
	if strings.Contains(rest, ":") {
		return "group"
	}
	return "single"
}

// ListActiveWeComFocuses returns all valid, non-expired WeCom direct-chat focus views,
// lazily deleting expired, legacy, or ended entries without re-triggering SSE loops.
func (e *Engine) ListActiveWeComFocuses(ctx context.Context) []*model.WeComFocusView {
	if e.store == nil {
		return nil
	}
	items, err := e.store.ListKVByPrefix(ctx, WeComFocusKVPrefix)
	if err != nil || len(items) == 0 {
		return nil
	}
	now := time.Now()
	ttl := e.wecomFocusTTL()
	out := make([]*model.WeComFocusView, 0, len(items))
	for k := range items {
		res := e.loadWeComFocusAt(ctx, k, now, false)
		if res.Focus == nil {
			continue
		}
		f := res.Focus
		title := strings.TrimSpace(f.Title)
		machine := strings.TrimSpace(f.Machine)
		kind := strings.TrimSpace(f.Kind)
		workItemID := strings.TrimSpace(f.WorkItemID)
		if w, wErr := e.store.GetWorker(ctx, f.Worker); wErr == nil && w != nil {
			if title == "" {
				title = strings.TrimSpace(w.Title)
			}
			if machine == "" {
				machine = workerMachineID(w)
			}
			if kind == "" {
				kind = w.Kind
			}
			if workItemID == "" {
				workItemID = w.WorkItemID
			}
		}
		if title == "" {
			title = f.Worker
		}
		out = append(out, &model.WeComFocusView{
			Key:        k,
			Scope:      wecomFocusScope(k),
			Worker:     f.Worker,
			Title:      title,
			Machine:    e.MachineDisplayName(machine),
			Kind:       kind,
			Session:    f.Session,
			WorkItemID: workItemID,
			EnteredAt:  f.EnteredAt,
			UpdatedAt:  f.UpdatedAt,
			ExpiresAt:  f.UpdatedAt.Add(ttl),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ActiveWeComFocus returns the most recent active WeCom focus matching thread
// (single-chat focus for chat:main, or group-scoped focus for wecom:<chatid>).
func (e *Engine) ActiveWeComFocus(ctx context.Context, thread string) *model.WeComFocusView {
	active := e.ListActiveWeComFocuses(ctx)
	if len(active) == 0 {
		return nil
	}
	if chatID, ok := strings.CutPrefix(strings.TrimSpace(thread), "wecom:"); ok && chatID != "" {
		wantPrefix := WeComFocusKVPrefix + chatID + ":"
		for _, v := range active {
			if strings.HasPrefix(v.Key, wantPrefix) {
				return v
			}
		}
		return nil
	}
	for _, v := range active {
		if v.Scope == "single" {
			return v
		}
	}
	return nil
}

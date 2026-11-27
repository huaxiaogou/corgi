package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const pendingAutoUpdateNoticeKVKey = "pending_auto_update_notice"

type pendingAutoUpdateNotice struct {
	Target   string    `json:"target"`
	Machines []string  `json:"machines,omitempty"`
	At       time.Time `json:"at"`
}

func (e *Engine) isHubUpdating() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hubUpdating
}

// TryReserveHubRestart verifies that the Hub is completely idle (no Dream, Handover,
// active Router turn, queued Router trigger, or queued Web prompt) and claims an
// exclusive reservation blocking new Router/Dream/Handover transitions until release()
// is invoked (or the process replaces itself via syscall.Exec).
func (e *Engine) TryReserveHubRestart(ctx context.Context) (func(), bool, string) {
	if !e.isAPIRouterMode() && e.fleet != nil {
		ags, scanned, err := e.fleet.AgentsWithScanned(ctx)
		if err != nil || !scanned[""] {
			return nil, false, "本地 herdr 状态暂不可用"
		}
		for _, a := range ags {
			if a.Name != nil && *a.Name == model.RouterName && !strings.Contains(a.PaneID, "@") {
				if a.AgentStatus != "idle" && a.AgentStatus != "done" {
					return nil, false, "Corgi 正在处理任务"
				}
			}
		}
	}

	if reason := e.checkRouterQueuesIdle(ctx); reason != "" {
		return nil, false, reason
	}

	e.mu.Lock()
	switch {
	case e.hubUpdating:
		e.mu.Unlock()
		return nil, false, "Hub 正在执行升级"
	case e.dream != nil:
		e.mu.Unlock()
		return nil, false, "Corgi 正在做梦整理记忆"
	case e.handover != nil:
		e.mu.Unlock()
		return nil, false, "Corgi 正在交接会话"
	case e.apiTurn != nil || e.routerLaunching || e.routerDispatching || !e.routerAskedAt.IsZero():
		e.mu.Unlock()
		return nil, false, "Corgi 正在回复或启动中"
	case e.settings.Paused:
		e.mu.Unlock()
		return nil, false, "Corgi 当前已暂停"
	}
	e.hubUpdating = true
	e.mu.Unlock()

	release := func() {
		e.mu.Lock()
		e.hubUpdating = false
		e.mu.Unlock()
	}

	// Phase-2 recheck after claiming e.hubUpdating to close any TOCTOU window with concurrent chat ingress.
	if reason := e.checkRouterQueuesIdle(ctx); reason != "" {
		release()
		return nil, false, reason
	}
	return release, true, ""
}

func (e *Engine) checkRouterQueuesIdle(ctx context.Context) string {
	if e.store == nil {
		return ""
	}
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil {
		return "读取待处理队列失败"
	}
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			return "有待处理的消息队列"
		}
	}
	prompts, err := e.store.ListQueuedPrompts(ctx, "")
	if err != nil {
		return "读取待发送对话失败"
	}
	for _, p := range prompts {
		if strings.HasPrefix(p.Scope, routerQueuePrefix) {
			return "有排队待发送的对话"
		}
	}
	return ""
}

// IsAutoUpdateWindowEligible checks Bounded B2 eligibility for opt-in overnight auto-updates:
//   - AutoUpdate must be explicitly enabled.
//   - If Dream is enabled, tonight's scheduled Dream must have completed ("ok") or quietly skipped
//     ("skipped"), and current time must still be inside that overnight window (within 3h of DreamAt).
//   - If Dream is "off", current time must be in [04:00, 06:00) local time.
func (e *Engine) IsAutoUpdateWindowEligible(ctx context.Context, now time.Time) bool {
	e.mu.Lock()
	enabled := e.settings.AutoUpdate
	paused := e.settings.Paused
	dreamH, dreamM, dreamOn := e.dreamAtLocked()
	e.mu.Unlock()
	if !enabled || paused {
		return false
	}
	if !dreamOn {
		start := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
		end := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, now.Location())
		return !now.Before(start) && now.Before(end)
	}
	slotTime := time.Date(now.Year(), now.Month(), now.Day(), dreamH, dreamM, 0, 0, now.Location())
	cutoff := slotTime.Add(3 * time.Hour)
	if now.Before(slotTime) || !now.Before(cutoff) {
		return false
	}
	slot, ok := e.readNightlyDreamSlot(ctx)
	if !ok || slot.Date != slotTime.Format("2006-01-02") {
		return false
	}
	return slot.Outcome == "ok" || slot.Outcome == "skipped"
}

func (e *Engine) checkAutoUpdate(ctx context.Context, live map[string]liveInfo, hasRouterQueued bool, now time.Time) {
	e.mu.Lock()
	updater := e.autoUpdater
	enabled := e.settings.AutoUpdate
	lastCheck := e.lastAutoUpdateCheck
	busy := e.dream != nil || e.handover != nil || e.hubUpdating || e.apiTurn != nil || e.routerLaunching || e.routerDispatching || !e.routerAskedAt.IsZero() || e.settings.Paused
	runCtx := e.runCtx
	e.mu.Unlock()
	if updater == nil || !enabled || busy || hasRouterQueued || now.Sub(lastCheck) < time.Minute {
		return
	}
	if li, alive := live[model.RouterName]; alive && li.status != "idle" && li.status != "done" {
		return
	}
	if !e.IsAutoUpdateWindowEligible(ctx, now) {
		return
	}
	if e.checkRouterQueuesIdle(ctx) != "" {
		return
	}

	e.mu.Lock()
	e.lastAutoUpdateCheck = now
	e.mu.Unlock()

	if runCtx == nil {
		runCtx = context.Background()
	}
	go updater.TryAutoUpdate(runCtx)
}

// RecordPendingAutoUpdateNotice records an expected post-restart auto-update confirmation
// so the newly exec'd Hub binary can announce the confirmed upgrade once it boots on target.
func (e *Engine) RecordPendingAutoUpdateNotice(ctx context.Context, target string, machines []string) error {
	if e.store == nil || strings.TrimSpace(target) == "" {
		return nil
	}
	b, err := json.Marshal(pendingAutoUpdateNotice{
		Target:   strings.TrimSpace(target),
		Machines: machines,
		At:       time.Now(),
	})
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, pendingAutoUpdateNoticeKVKey, string(b))
}

// EmitPendingAutoUpdateNotice checks on Hub startup whether an overnight auto-update just
// completed onto the expected version, clears the marker, and emits a single chat:main event.
func (e *Engine) EmitPendingAutoUpdateNotice(ctx context.Context) {
	if e.store == nil {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, pendingAutoUpdateNoticeKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var notice pendingAutoUpdateNotice
	if err := json.Unmarshal([]byte(raw), &notice); err != nil {
		_ = e.store.DeleteKV(ctx, pendingAutoUpdateNoticeKVKey)
		return
	}
	e.mu.Lock()
	curVer := e.version
	e.mu.Unlock()
	if curVer == "" {
		return
	}
	// Stale (>12h) marker or confirmed match: clear either way; only emit success when running target version.
	if curVer == notice.Target {
		_ = e.store.DeleteKV(ctx, pendingAutoUpdateNoticeKVKey)
		detail := "Hub"
		if len(notice.Machines) > 0 {
			detail = strings.Join(notice.Machines, "、")
		}
		e.PostEvent(ctx, model.MainThread, fmt.Sprintf("凌晨空闲自动更新已完成，已升级至 %s（%s）", notice.Target, detail), "", "info")
		return
	}
	if time.Since(notice.At) > 12*time.Hour {
		_ = e.store.DeleteKV(ctx, pendingAutoUpdateNoticeKVKey)
	}
}

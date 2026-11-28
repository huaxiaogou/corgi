package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// Run drives the periodic Super-Agent scheduling loop until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	e.reconcileAPIRouterInflight(ctx)
	e.EmitPendingAutoUpdateNotice(ctx)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	defer e.llm.CloseIdleConnections()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wakeCh:
			e.tick(ctx)
		case <-tick.C:
			e.tick(ctx)
		}
	}
}

type liveInfo struct {
	pane    string
	status  string
	ready   bool
	kind    string
	session string
}

func isHubSystemName(name string) bool {
	return name == model.RouterName || name == model.DreamName
}

func isHubLocalPane(paneID string) bool {
	return paneID != "" && !strings.Contains(paneID, "@")
}

func (e *Engine) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	// 1. Fire due timers and periodically expire >7d stale pending actions.
	now := time.Now()
	e.mu.Lock()
	needExpire := now.Sub(e.lastPendingExpire) >= 10*time.Minute
	if needExpire {
		e.lastPendingExpire = now
	}
	e.mu.Unlock()
	if needExpire {
		if n, err := e.store.ExpireStalePendingActions(ctx, model.PendingActionMaxAge); err == nil && n > 0 {
			e.notifyChanged()
		}
	}
	if e.expireStaleConfigConfirms(ctx) > 0 {
		e.notifyChanged()
	}
	if timers, err := e.store.ListTimers(ctx); err == nil {
		for _, tm := range timers {
			if tm.Enabled && !tm.NextRun.After(now) {
				fired := now
				tm.LastRun = &fired
				AdvanceTimer(tm, now)
				_ = e.store.UpsertTimer(ctx, tm)
				_, _ = e.Ingest(ctx, "timer", tm.Thread, tm.ID, tm.Text, "", tm.Target, tm.ID)
			}
		}
	}

	var (
		rawAgents []model.Agent
		scanned   map[string]bool
		fleetOK   bool
	)
	if e.fleet != nil {
		// Cap fleet scan so a stalled local herdr never exhausts the tick deadline
		// needed by API Dream finalization and API Router SQLite delivery.
		scanCtx, scanCancel := context.WithTimeout(ctx, 8*time.Second)
		var fErr error
		rawAgents, scanned, fErr = e.fleet.AgentsWithScanned(scanCtx)
		scanCancel()
		if fErr == nil || len(scanned) > 0 {
			fleetOK = true
		}
	}
	live := map[string]liveInfo{}
	if fleetOK {
		for i := range rawAgents {
			a := &rawAgents[i]
			if (a.Name == nil || *a.Name == "") && a.PaneID != "" && a.Agent != nil && *a.Agent != "" {
				fallback := deriveFallbackWorkerName(*a.Agent, a.PaneID)
				a.Name = &fallback
				e.fleet.RememberName(a.PaneID, fallback)
			}
			if a.Name == nil || *a.Name == "" {
				continue
			}
			name := *a.Name
			if isHubSystemName(name) && !isHubLocalPane(a.PaneID) {
				continue
			}
			li := liveInfo{pane: a.PaneID, status: a.AgentStatus, ready: a.Ready}
			if a.Agent != nil {
				li.kind = *a.Agent
			}
			if a.Session != nil {
				li.session = extractSessionID(a.Session.Kind, a.Session.Value)
			}
			live[name] = li
		}

		// Auto-adopt any live non-system agent session into Corgi's managed workers table
		// so no orphan unmanaged sessions ever exist.
		e.adoptUnmanagedAgents(ctx, rawAgents)

		// 2. Sync worker states, detect bare-shell failures, create Actions on blocked dialogs or quota errors.
		e.syncWorkers(ctx, live, scanned, now)
	}

	queued, _ := e.store.ListQueuedTriggers(ctx)
	hasRouterQueued := false
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			hasRouterQueued = true
			break
		}
	}

	// 3. Drive or trigger nightly memory consolidation (Dream).
	e.checkDream(ctx, hasRouterQueued)

	if e.Settings().Paused || e.isHubUpdating() {
		return
	}

	// 4. Drain idle main-chat queue, route/deliver triggers, and drain idle single-agent queues.
	e.drainRouterPrompts(ctx)
	e.deliverQueuedWithFleet(ctx, live, scanned, fleetOK)
	if fleetOK {
		e.drainPanePrompts(ctx, rawAgents)
	}

	// 5. Watchdog for stalled/thrashing workers, zero-token gated idle patrol / morning digest,
	//    and lossless background hibernation GC for idle Corgi-dispatched workers.
	if fleetOK {
		e.checkStalls(ctx, live, now)
		e.checkWorkerThrash(ctx, live, now)
		e.checkIdlePatrol(ctx, live, now)
		e.checkWorkerGC(ctx, rawAgents, live, scanned, now)
	}

	// 6. Optional overnight idle auto-update after tonight's scheduled Dream succeeds/skips.
	e.checkAutoUpdate(ctx, hasRouterQueued, now)
}

func deriveFallbackWorkerName(rawKind, paneID string) string {
	k := agent.NormalizeKind(rawKind)
	if k == "" {
		k = "agent"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(paneID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == '@' || r == '-' || r == '_' {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 16 {
		slug = slug[:16]
	}
	if slug == "" {
		return k + "-live"
	}
	return k + "-" + slug
}

func workerMachineID(w *model.Worker) string {
	if w == nil {
		return ""
	}
	if w.PaneID != "" {
		if _, m := fleet.SplitID(w.PaneID); m != "" {
			return m
		}
	}
	return strings.TrimSpace(w.Machine)
}

// RestartRouter cancels any in-flight native API Router turn and resets backoff/auth state.
func (e *Engine) RestartRouter(ctx context.Context) error {
	e.cancelAndSettleAPIRouterTurn(ctx, "", true)
	e.mu.Lock()
	e.router.resetFailuresLocked(false)
	e.mu.Unlock()
	e.wake()
	e.notifyChanged()
	return nil
}

func (e *Engine) checkStalls(ctx context.Context, live map[string]liveInfo, now time.Time) {
	stallDur := time.Duration(e.Settings().StallMinutes) * time.Minute
	if stallDur <= 0 {
		return
	}
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	for _, w := range workers {
		if w.Ended {
			continue
		}
		li, ok := live[w.Name]
		if !ok || li.status != "working" || e.hasDirectTurn(w.Name) {
			e.mu.Lock()
			delete(e.lastScreen, w.Name)
			e.mu.Unlock()
			continue
		}
		rr, err := e.fleet.Read(ctx, li.pane, "detection", "text", 0)
		if err != nil {
			continue
		}
		h := agent.ProgressHash(rr.Text)

		e.mu.Lock()
		mk, seen := e.lastScreen[w.Name]
		if !seen || mk.hash != h {
			e.lastScreen[w.Name] = screenMark{hash: h, since: now}
			e.mu.Unlock()
			continue
		}
		stalled := !mk.told && now.Sub(mk.since) >= stallDur
		if stalled {
			mk.told = true
			e.lastScreen[w.Name] = mk
		}
		e.mu.Unlock()

		if stalled {
			_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
				fmt.Sprintf("watchdog: worker %s screen has not changed for %d minutes. Inspect it, nudge it, or switch approach.", w.Name, int(stallDur.Minutes())),
				"", model.RouterName, w.Name)
		}
	}
}

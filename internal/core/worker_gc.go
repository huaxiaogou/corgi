package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	defaultWorkerGCInterval          = 2 * time.Minute
	defaultWorkerGCGracePeriod       = 2 * time.Minute
	defaultWorkerGCActivityCooldown  = 2 * time.Minute
	defaultWorkerGCClosedIdle        = 15 * time.Minute
	defaultWorkerGCFaultAge          = 30 * time.Minute
	defaultWorkerGCCapIdleCooldown   = 15 * time.Minute
	defaultWorkerGCFailBackoff       = 10 * time.Minute
	defaultWorkerGCRecentCloseWindow = 15 * time.Second
	workerGCMaxPerCycle              = 2
)

type workerGCCandidate struct {
	name        string
	paneID      string
	workspaceID string
	machine     string
	session     string // strictly pane-bound from herdr (never cwd-guessed)
	rule        string // P1 | P2 | P3 | P4
	reason      string
	idleDur     time.Duration
}

func (e *Engine) effectiveWorkerIdle(w *model.Worker, now time.Time) (time.Duration, bool) {
	if w == nil || w.IdleSince == nil {
		return 0, false
	}
	base := *w.IdleSince
	if act := e.lastWorkerActivity(w.Name); !act.IsZero() && act.After(base) {
		base = act
	}
	if base.After(now) {
		return 0, true
	}
	return now.Sub(base), true
}

func (e *Engine) recordWorkerGCFailBackoff(workerName string, now time.Time) {
	if workerName == "" {
		return
	}
	e.gc.mu.Lock()
	if e.gc.failBackoff == nil {
		e.gc.failBackoff = map[string]time.Time{}
	}
	e.gc.failBackoff[workerName] = now
	e.gc.mu.Unlock()
}

func (e *Engine) inWorkerGCFailBackoff(workerName string, now time.Time) bool {
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	at, ok := e.gc.failBackoff[workerName]
	return ok && now.Sub(at) < defaultWorkerGCFailBackoff
}

// checkWorkerGC runs a fast, non-blocking selection pass every 2 minutes from tick()
// and hands up to workerGCMaxPerCycle hibernation candidates to a single-flight background goroutine.
func (e *Engine) checkWorkerGC(ctx context.Context, rawAgents []model.Agent, live map[string]liveInfo, scanned map[string]bool, now time.Time) {
	if e.fleet == nil || e.store == nil {
		return
	}
	st := e.Settings()
	if st.EffectiveWorkerIdleGCMinutes() <= 0 {
		return
	}
	if st.Paused || e.isHubUpdating() {
		return
	}
	grace := e.getWorkerGCGracePeriod()
	if grace > 0 && !e.gc.startedAt.IsZero() && now.Sub(e.gc.startedAt) < grace {
		return
	}

	interval := e.getWorkerGCInterval()
	e.gc.mu.Lock()
	if e.gc.running || (!e.gc.lastScan.IsZero() && now.Sub(e.gc.lastScan) < interval) {
		e.gc.mu.Unlock()
		return
	}
	e.gc.lastScan = now
	e.gc.mu.Unlock()

	cands := e.selectWorkerGCCandidates(ctx, st, rawAgents, live, scanned, now)
	if len(cands) == 0 {
		return
	}
	if len(cands) > workerGCMaxPerCycle {
		cands = cands[:workerGCMaxPerCycle]
	}

	parent := e.backgroundRunCtx()
	if parent == nil {
		parent = context.Background()
	}

	e.gc.mu.Lock()
	if e.gc.running {
		e.gc.mu.Unlock()
		return
	}
	e.gc.running = true
	e.gc.mu.Unlock()

	go func() {
		defer func() {
			e.gc.mu.Lock()
			e.gc.running = false
			e.gc.mu.Unlock()
		}()
		for _, cand := range cands {
			if parent.Err() != nil {
				return
			}
			hctx, cancel := context.WithTimeout(parent, 30*time.Second)
			e.hibernateWorker(hctx, cand)
			cancel()
		}
	}()
}

func (e *Engine) selectWorkerGCCandidates(
	ctx context.Context,
	st model.Settings,
	rawAgents []model.Agent,
	live map[string]liveInfo,
	scanned map[string]bool,
	now time.Time,
) []workerGCCandidate {
	idleMin := st.EffectiveWorkerIdleGCMinutes()
	if idleMin <= 0 {
		return nil
	}
	idleThreshold := time.Duration(idleMin) * time.Minute
	maxLive := st.EffectiveWorkerMaxLivePerMachine()
	grace := e.getWorkerGCGracePeriod()
	actCooldown := e.getWorkerGCActivityCooldown()

	workers, err := e.store.ListWorkers(ctx)
	if err != nil || len(workers) == 0 {
		return nil
	}

	// Build machine grace map from Fleet.
	machineSince := map[string]time.Time{}
	for _, m := range e.fleet.Machines() {
		machineSince[m.ID] = m.Since
	}

	// Map exact live agents by paneID for workspace verification.
	agentByPane := make(map[string]model.Agent, len(rawAgents))
	for _, a := range rawAgents {
		if a.PaneID != "" {
			agentByPane[a.PaneID] = a
		}
	}

	// Gather active blocking sets once per cycle.
	queuedWorkers := map[string]bool{}
	if qTrigs, err := e.store.ListQueuedTriggers(ctx); err == nil {
		for _, t := range qTrigs {
			for _, tgt := range t.Targets {
				queuedWorkers[tgt] = true
			}
		}
	}
	queuedPanes := map[string]bool{}
	if qPrompts, err := e.store.ListQueuedPrompts(ctx, ""); err == nil {
		for _, p := range qPrompts {
			if !strings.HasPrefix(p.Scope, routerQueuePrefix) {
				queuedPanes[p.Scope] = true
			}
		}
	}
	blockedWorkers := map[string]bool{}
	blockedWorkItems := map[string]bool{}
	markAction := func(a *model.Action) {
		if a == nil {
			return
		}
		if a.TargetWorker != "" {
			blockedWorkers[a.TargetWorker] = true
		}
		if a.WorkItemID != "" {
			blockedWorkItems[a.WorkItemID] = true
		}
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, a := range pending {
			markAction(a)
		}
	}
	if snoozed, err := e.store.ListSnoozedActions(ctx); err == nil {
		for _, a := range snoozed {
			if a.SnoozeUntil == nil || a.SnoozeUntil.After(now) {
				markAction(a)
			}
		}
	}
	focusedWorkers := map[string]bool{}
	for _, fv := range e.ListActiveWeComFocuses(ctx) {
		if fv != nil && fv.Worker != "" {
			focusedWorkers[fv.Worker] = true
		}
	}

	// Cache work items by ID (include closed items so P1 can check ClosedAt).
	workItems := map[string]*model.WorkItem{}
	if allWI, err := e.store.ListWorkItems(ctx, true); err == nil {
		for _, wi := range allWI {
			if wi != nil {
				workItems[wi.ID] = wi
			}
		}
	}

	type capPoolItem struct {
		cand workerGCCandidate
	}
	var p1, p2, p3 []workerGCCandidate
	selected := map[string]bool{}
	liveCorgiCountByMachine := map[string]int{}
	capPoolByMachine := map[string][]capPoolItem{}

	for _, w := range workers {
		if w == nil || w.Ended || w.Hibernating || !isCorgiOwnedWorker(w) {
			continue
		}
		machine := workerMachineID(w)
		li, alive := live[w.Name]
		if !alive || li.pane == "" || li.pane != w.PaneID {
			continue
		}
		ag, ok := agentByPane[w.PaneID]
		if !ok || w.WorkspaceID == "" || ag.WorkspaceID != w.WorkspaceID {
			continue
		}
		// Count every live Corgi-owned worker on this machine (including Keep/working) toward soft cap.
		liveCorgiCountByMachine[machine]++

		if w.Keep {
			continue
		}
		if scanned != nil && !scanned[machine] {
			continue
		}
		if grace > 0 {
			if since, ok := machineSince[machine]; ok && !since.IsZero() && now.Sub(since) < grace {
				continue
			}
		}
		if e.inWorkerGCFailBackoff(w.Name, now) {
			continue
		}
		if e.isWorkerGCClosing(w.Name) || e.isHealingBypassWorker(w.Name, w.PaneID) {
			continue
		}
		if e.hasDirectTurn(w.Name) || e.hasWorkerPendingCheck(w.Name, now) || e.hasWorkerStartupCheck(w.Name) {
			continue
		}
		if queuedWorkers[w.Name] || queuedPanes[w.PaneID] {
			continue
		}
		if blockedWorkers[w.Name] || (w.WorkItemID != "" && blockedWorkItems[w.WorkItemID]) {
			continue
		}
		if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
			continue
		}
		if focusedWorkers[w.Name] {
			continue
		}
		e.gc.mu.Lock()
		busyOp := e.gc.workerOps[w.Name] > 0
		lastAct := e.gc.workerActivity[w.Name]
		e.gc.mu.Unlock()
		if busyOp {
			continue
		}
		if actCooldown > 0 && !lastAct.IsZero() && now.Sub(lastAct) < actCooldown {
			continue
		}

		// Strictly pane-bound session from herdr (or previously persisted pane-bound session on w).
		sess := li.session
		if sess == "" {
			sess = w.Session
		}

		// P2: Proven initial startup-failed worker shell (never completed a turn).
		if isWorkerFaulted(w) {
			if w.StartupFailed {
				faultBase := w.CreatedAt
				if w.FaultAt != nil && !w.FaultAt.IsZero() {
					faultBase = *w.FaultAt
				}
				if !faultBase.IsZero() && now.Sub(faultBase) >= defaultWorkerGCFaultAge {
					p2 = append(p2, workerGCCandidate{
						name:        w.Name,
						paneID:      w.PaneID,
						workspaceID: w.WorkspaceID,
						machine:     machine,
						session:     sess,
						rule:        "P2",
						reason:      fmt.Sprintf("首次启动故障闲置 %s", formatIdleDuration(now.Sub(faultBase))),
						idleDur:     now.Sub(faultBase),
					})
					selected[w.Name] = true
				}
			}
			continue
		}

		// P1 / P3 / P4 require idle or done status.
		if li.status != "idle" && li.status != "done" {
			continue
		}
		effIdle, hasIdle := e.effectiveWorkerIdle(w, now)
		if !hasIdle {
			continue
		}

		// P1: Closed WorkItem + idle >= 15m.
		if w.WorkItemID != "" {
			if wi := workItems[w.WorkItemID]; wi != nil && wi.ClosedAt != nil && effIdle >= defaultWorkerGCClosedIdle {
				p1 = append(p1, workerGCCandidate{
					name:        w.Name,
					paneID:      w.PaneID,
					workspaceID: w.WorkspaceID,
					machine:     machine,
					session:     sess,
					rule:        "P1",
					reason:      fmt.Sprintf("关联事项 %s 已关闭且空闲 %s", wi.ID, formatIdleDuration(effIdle)),
					idleDur:     effIdle,
				})
				selected[w.Name] = true
				continue
			}
		}

		// P3 & P4 strictly require a resumable pane-bound session ID.
		if sess == "" {
			continue
		}

		// P3: Routine idle hibernation.
		if effIdle >= idleThreshold {
			p3 = append(p3, workerGCCandidate{
				name:        w.Name,
				paneID:      w.PaneID,
				workspaceID: w.WorkspaceID,
				machine:     machine,
				session:     sess,
				rule:        "P3",
				reason:      fmt.Sprintf("持续空闲 %s", formatIdleDuration(effIdle)),
				idleDur:     effIdle,
			})
			selected[w.Name] = true
			continue
		}

		// Eligible pool for P4 per-machine LRU cap (requires 15m cooldown).
		if maxLive > 0 && effIdle >= defaultWorkerGCCapIdleCooldown {
			capPoolByMachine[machine] = append(capPoolByMachine[machine], capPoolItem{
				cand: workerGCCandidate{
					name:        w.Name,
					paneID:      w.PaneID,
					workspaceID: w.WorkspaceID,
					machine:     machine,
					session:     sess,
					rule:        "P4",
					reason:      fmt.Sprintf("单机并发超上限（上限 %d，已空闲 %s）", maxLive, formatIdleDuration(effIdle)),
					idleDur:     effIdle,
				},
			})
		}
	}

	sortOldestFirst := func(list []workerGCCandidate) {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].idleDur != list[j].idleDur {
				return list[i].idleDur > list[j].idleDur
			}
			return list[i].name < list[j].name
		})
	}
	sortOldestFirst(p1)
	sortOldestFirst(p2)
	sortOldestFirst(p3)

	// Account for workers already selected by P1-P3 before triggering P4 LRU eviction.
	selectedByMachine := map[string]int{}
	for _, list := range [][]workerGCCandidate{p1, p2, p3} {
		for _, c := range list {
			selectedByMachine[c.machine]++
		}
	}

	var p4 []workerGCCandidate
	if maxLive > 0 {
		machines := make([]string, 0, len(capPoolByMachine))
		for m := range capPoolByMachine {
			machines = append(machines, m)
		}
		sort.Strings(machines)
		for _, m := range machines {
			excess := liveCorgiCountByMachine[m] - selectedByMachine[m] - maxLive
			if excess <= 0 {
				continue
			}
			pool := capPoolByMachine[m]
			sort.SliceStable(pool, func(i, j int) bool {
				if pool[i].cand.idleDur != pool[j].cand.idleDur {
					return pool[i].cand.idleDur > pool[j].cand.idleDur
				}
				return pool[i].cand.name < pool[j].cand.name
			})
			for i := 0; i < len(pool) && i < excess; i++ {
				if !selected[pool[i].cand.name] {
					p4 = append(p4, pool[i].cand)
					selected[pool[i].cand.name] = true
				}
			}
		}
	}

	out := make([]workerGCCandidate, 0, len(p1)+len(p2)+len(p3)+len(p4))
	out = append(out, p1...)
	out = append(out, p2...)
	out = append(out, p3...)
	out = append(out, p4...)
	return out
}

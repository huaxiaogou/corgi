package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
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

// isCorgiOwnedWorker reports whether w was explicitly dispatched by Corgi/Router.
// Manually opened ("you"), adopted ("adopted"), legacy empty (""), and unknown origins are exempt.
func isCorgiOwnedWorker(w *model.Worker) bool {
	if w == nil || isHubSystemName(w.Name) {
		return false
	}
	return w.CreatedBy == model.RouterName || w.CreatedBy == "router"
}

func (e *Engine) getWorkerGCInterval() time.Duration {
	e.mu.Lock()
	d := e.workerGCInterval
	e.mu.Unlock()
	if d > 0 {
		return d
	}
	return defaultWorkerGCInterval
}

func (e *Engine) getWorkerGCGracePeriod() time.Duration {
	e.mu.Lock()
	d := e.workerGCGracePeriod
	e.mu.Unlock()
	if d != 0 {
		if d < 0 {
			return 0
		}
		return d
	}
	return defaultWorkerGCGracePeriod
}

func (e *Engine) getWorkerGCActivityCooldown() time.Duration {
	e.mu.Lock()
	d := e.workerGCActivityCooldown
	e.mu.Unlock()
	if d != 0 {
		if d < 0 {
			return 0
		}
		return d
	}
	return defaultWorkerGCActivityCooldown
}

// beginWorkerOp registers an in-flight operation on workerName, preventing concurrent GC
// hibernation from closing its workspace while the operation is active.
// Returns false if GC is already closing workerName.
func (e *Engine) beginWorkerOp(workerName string) bool {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gcClosing[workerName] {
		return false
	}
	if e.workerOps == nil {
		e.workerOps = map[string]int{}
	}
	if e.workerActivity == nil {
		e.workerActivity = map[string]time.Time{}
	}
	e.workerOps[workerName]++
	e.workerActivity[workerName] = time.Now()
	return true
}

// endWorkerOp releases a previously acquired beginWorkerOp reservation and refreshes activity.
func (e *Engine) endWorkerOp(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := e.workerOps[workerName]; n <= 1 {
		delete(e.workerOps, workerName)
	} else {
		e.workerOps[workerName] = n - 1
	}
	if e.workerActivity == nil {
		e.workerActivity = map[string]time.Time{}
	}
	e.workerActivity[workerName] = time.Now()
}

// beginWorkerOpWithoutActivity registers an internal read/sync guard (such as syncWorkers)
// that blocks concurrent GC claiming without artificially resetting the worker's idle clock.
func (e *Engine) beginWorkerOpWithoutActivity(workerName string) bool {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gcClosing[workerName] {
		return false
	}
	if e.workerOps == nil {
		e.workerOps = map[string]int{}
	}
	e.workerOps[workerName]++
	return true
}

func (e *Engine) endWorkerOpWithoutActivity(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := e.workerOps[workerName]; n <= 1 {
		delete(e.workerOps, workerName)
	} else {
		e.workerOps[workerName] = n - 1
	}
}

func (e *Engine) touchWorkerActivity(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.mu.Lock()
	if e.workerActivity == nil {
		e.workerActivity = map[string]time.Time{}
	}
	e.workerActivity[workerName] = time.Now()
	e.mu.Unlock()
}

func (e *Engine) lastWorkerActivity(workerName string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.workerActivity[workerName]
}

func (e *Engine) isWorkerGCClosing(workerName string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gcClosing[workerName]
}

func (e *Engine) isPaneGCClosing(paneID string) bool {
	if paneID == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gcClosingPane[paneID] != ""
}

func (e *Engine) waitWorkerGCIdle(ctx context.Context, workerName string, maxWait time.Duration) bool {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return true
	}
	if !e.isWorkerGCClosing(workerName) {
		return true
	}
	deadline := time.Now().Add(maxWait)
	for e.isWorkerGCClosing(workerName) {
		if time.Now().After(deadline) {
			return false
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return true
}

func (e *Engine) recordRecentlyClosedPane(paneID string, now time.Time) {
	if paneID == "" {
		return
	}
	e.mu.Lock()
	if e.recentlyClosedPanes == nil {
		e.recentlyClosedPanes = map[string]time.Time{}
	}
	for pid, at := range e.recentlyClosedPanes {
		if now.Sub(at) > defaultWorkerGCRecentCloseWindow {
			delete(e.recentlyClosedPanes, pid)
		}
	}
	e.recentlyClosedPanes[paneID] = now
	e.mu.Unlock()
}

func (e *Engine) isRecentlyClosedPane(paneID string, now time.Time) bool {
	if paneID == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.recentlyClosedPanes[paneID]
	if !ok {
		return false
	}
	if now.Sub(at) > defaultWorkerGCRecentCloseWindow {
		delete(e.recentlyClosedPanes, paneID)
		return false
	}
	return true
}

func (e *Engine) workerNameForPane(ctx context.Context, paneID string) string {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" || e.store == nil {
		return ""
	}
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if w != nil && !w.Ended && w.PaneID == paneID {
				return w.Name
			}
		}
	}
	return ""
}

func (e *Engine) beginWorkerOpByTarget(ctx context.Context, target, paneID string) (string, bool) {
	name := ""
	if paneID != "" {
		if e.isPaneGCClosing(paneID) {
			return "", false
		}
		name = e.workerNameForPane(ctx, paneID)
	}
	if name == "" && target != "" && !strings.Contains(target, ":") && !strings.Contains(target, "@") {
		if e.store != nil {
			if w, err := e.store.GetWorker(ctx, target); err == nil && w != nil && !w.Ended {
				name = w.Name
			}
		}
	}
	if name == "" {
		return "", true
	}
	if !e.beginWorkerOp(name) {
		return "", false
	}
	return name, true
}

// SendPaneText sends raw text to paneID while guarding against concurrent GC hibernation
// and refreshing the owning worker's activity clock.
func (e *Engine) SendPaneText(ctx context.Context, paneID, text string, enter bool) error {
	if e.fleet == nil {
		return fmt.Errorf("fleet not configured")
	}
	name, ok := e.beginWorkerOpByTarget(ctx, "", paneID)
	if !ok {
		return fmt.Errorf("worker is hibernating; retry or resume session")
	}
	if name != "" {
		defer e.endWorkerOp(name)
	}
	if err := e.fleet.SendText(ctx, paneID, text, enter); err != nil {
		return err
	}
	if name != "" {
		e.resetWorkerIdleByName(ctx, name)
	}
	return nil
}

// SendPaneKeys sends raw key events to paneID while guarding against concurrent GC hibernation
// and refreshing the owning worker's activity clock.
func (e *Engine) SendPaneKeys(ctx context.Context, paneID string, keys ...string) error {
	if e.fleet == nil {
		return fmt.Errorf("fleet not configured")
	}
	name, ok := e.beginWorkerOpByTarget(ctx, "", paneID)
	if !ok {
		return fmt.Errorf("worker is hibernating; retry or resume session")
	}
	if name != "" {
		defer e.endWorkerOp(name)
	}
	if err := e.fleet.SendKeys(ctx, paneID, keys...); err != nil {
		return err
	}
	if name != "" {
		e.resetWorkerIdleByName(ctx, name)
	}
	return nil
}

func (e *Engine) resetWorkerIdleByName(ctx context.Context, name string) {
	if name == "" || e.store == nil {
		return
	}
	if wk, err := e.store.GetWorker(ctx, name); err == nil && wk != nil && !wk.Ended && !wk.Hibernating {
		if wk.IdleSince != nil || wk.IdlePatrolCount > 0 || wk.SnoozeUntil != nil {
			resetWorkerIdle(wk)
			_ = e.store.UpsertWorker(ctx, wk)
		}
	}
}

func (e *Engine) collectWorkerSubscribers(ctx context.Context, workerName string) []string {
	if workerName == "" || e.store == nil {
		return nil
	}
	threads, err := e.store.ListThreads(ctx, 200)
	if err != nil {
		return nil
	}
	var subs []string
	for _, th := range threads {
		if th != nil && th.ID != "" && th.ID != model.MainThread && slices.Contains(th.Subscribers, workerName) {
			subs = append(subs, th.ID)
		}
	}
	return subs
}

func (e *Engine) buildWorkerEndedSession(ctx context.Context, wk *model.Worker, histCwd string, subs []string, now time.Time) *model.EndedSession {
	if wk == nil || wk.Session == "" || e.store == nil {
		return nil
	}
	eid, _, err := e.store.NextID(ctx, "h")
	if err != nil {
		return nil
	}
	return &model.EndedSession{
		ID:                  eid,
		Kind:                wk.Kind,
		Machine:             wk.Machine,
		Cwd:                 orStr(histCwd, wk.Cwd),
		Name:                wk.Name,
		Title:               orStr(wk.Title, wk.Name),
		Session:             wk.Session,
		WorkItemID:          wk.WorkItemID,
		Model:               wk.Model,
		Effort:              wk.Effort,
		Constraint:          wk.Constraint,
		Mode:                wk.Mode,
		ReadonlyEnforcement: wk.ReadonlyEnforcement,
		CreatedBy:           wk.CreatedBy,
		Thread:              wk.Thread,
		Keep:                wk.Keep,
		RepoRoot:            wk.RepoRoot,
		RepoCommonDir:       wk.RepoCommonDir,
		WorktreePath:        wk.WorktreePath,
		WorktreeBranch:      wk.WorktreeBranch,
		WorktreeBase:        wk.WorktreeBase,
		Subscribers:         append([]string(nil), subs...),
		EndedAt:             now,
	}
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
	e.mu.Lock()
	if e.gcFailBackoff == nil {
		e.gcFailBackoff = map[string]time.Time{}
	}
	e.gcFailBackoff[workerName] = now
	e.mu.Unlock()
}

func (e *Engine) inWorkerGCFailBackoff(workerName string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.gcFailBackoff[workerName]
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
	if grace > 0 && !e.startedAt.IsZero() && now.Sub(e.startedAt) < grace {
		return
	}

	interval := e.getWorkerGCInterval()
	e.mu.Lock()
	if e.gcRunning || (!e.lastGCScan.IsZero() && now.Sub(e.lastGCScan) < interval) {
		e.mu.Unlock()
		return
	}
	e.lastGCScan = now
	e.mu.Unlock()

	cands := e.selectWorkerGCCandidates(ctx, st, rawAgents, live, scanned, now)
	if len(cands) == 0 {
		return
	}
	if len(cands) > workerGCMaxPerCycle {
		cands = cands[:workerGCMaxPerCycle]
	}

	e.mu.Lock()
	if e.gcRunning {
		e.mu.Unlock()
		return
	}
	e.gcRunning = true
	parent := e.runCtx
	if parent == nil {
		parent = context.Background()
	}
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			e.gcRunning = false
			e.mu.Unlock()
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
		e.mu.Lock()
		busyOp := e.workerOps[w.Name] > 0
		lastAct := e.workerActivity[w.Name]
		e.mu.Unlock()
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

func (e *Engine) hibernateWorker(ctx context.Context, cand workerGCCandidate) {
	if e.fleet == nil || e.store == nil || cand.name == "" {
		return
	}

	// Atomically claim exclusive GC closing slot when zero active workerOps exist.
	e.mu.Lock()
	if e.gcClosing[cand.name] || e.workerOps[cand.name] > 0 {
		e.mu.Unlock()
		return
	}
	if e.gcClosing == nil {
		e.gcClosing = map[string]bool{}
	}
	if e.gcClosingPane == nil {
		e.gcClosingPane = map[string]string{}
	}
	e.gcClosing[cand.name] = true
	if cand.paneID != "" {
		e.gcClosingPane[cand.paneID] = cand.name
	}
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		delete(e.gcClosing, cand.name)
		if cand.paneID != "" && e.gcClosingPane[cand.paneID] == cand.name {
			delete(e.gcClosingPane, cand.paneID)
		}
		e.mu.Unlock()
	}()

	now := time.Now()
	st := e.Settings()
	if st.EffectiveWorkerIdleGCMinutes() <= 0 || st.Paused || e.isHubUpdating() {
		return
	}

	// Re-read fresh worker state and re-verify all safety vetoes before touching herdr.
	wk, err := e.store.GetWorker(ctx, cand.name)
	if err != nil || wk == nil || wk.Ended || wk.Hibernating || wk.Keep || !isCorgiOwnedWorker(wk) {
		return
	}
	if wk.PaneID == "" || wk.PaneID != cand.paneID || wk.WorkspaceID == "" || wk.WorkspaceID != cand.workspaceID {
		return
	}
	if e.isHealingBypassWorker(wk.Name, wk.PaneID) ||
		e.hasDirectTurn(wk.Name) || e.hasWorkerPendingCheck(wk.Name, now) || e.hasWorkerStartupCheck(wk.Name) {
		return
	}
	if actCooldown := e.getWorkerGCActivityCooldown(); actCooldown > 0 {
		if lastAct := e.lastWorkerActivity(wk.Name); !lastAct.IsZero() && now.Sub(lastAct) < actCooldown {
			return
		}
	}
	if wk.SnoozeUntil != nil && wk.SnoozeUntil.After(now) {
		return
	}
	if qTrigs, qErr := e.store.ListQueuedTriggers(ctx); qErr == nil {
		for _, t := range qTrigs {
			if slices.Contains(t.Targets, wk.Name) {
				return
			}
		}
	}
	if qPrompts, qErr := e.store.ListQueuedPrompts(ctx, wk.PaneID); qErr == nil && len(qPrompts) > 0 {
		return
	}
	if pending, pErr := e.store.ListPendingActions(ctx); pErr == nil {
		for _, a := range pending {
			if a.TargetWorker == wk.Name || (wk.WorkItemID != "" && a.WorkItemID == wk.WorkItemID) {
				return
			}
		}
	}
	if snoozed, sErr := e.store.ListSnoozedActions(ctx); sErr == nil {
		for _, a := range snoozed {
			if (a.SnoozeUntil == nil || a.SnoozeUntil.After(now)) &&
				(a.TargetWorker == wk.Name || (wk.WorkItemID != "" && a.WorkItemID == wk.WorkItemID)) {
				return
			}
		}
	}
	for _, fv := range e.ListActiveWeComFocuses(ctx) {
		if fv != nil && fv.Worker == wk.Name {
			return
		}
	}

	// Re-verify rule preconditions on fresh worker state.
	if cand.rule == "P2" {
		if !isWorkerFaulted(wk) || !wk.StartupFailed {
			return
		}
	} else {
		if isWorkerFaulted(wk) {
			return
		}
		effIdle, ok := e.effectiveWorkerIdle(wk, now)
		if !ok {
			return
		}
		switch cand.rule {
		case "P1":
			if wk.WorkItemID == "" {
				return
			}
			wi, wiErr := e.store.GetWorkItem(ctx, wk.WorkItemID)
			if wiErr != nil || wi == nil || wi.ClosedAt == nil || effIdle < defaultWorkerGCClosedIdle {
				return
			}
		case "P3":
			if effIdle < time.Duration(st.EffectiveWorkerIdleGCMinutes())*time.Minute {
				return
			}
		case "P4":
			if effIdle < defaultWorkerGCCapIdleCooldown {
				return
			}
		}
	}

	// Screen check BEFORE final pane check: if screen read fails or background task is active, abort conservatively.
	rr, rErr := e.fleet.Read(ctx, wk.PaneID, "recent", "text", 20)
	if rErr != nil {
		e.logger.Warn("worker gc: screen read failed, skipping hibernation", "worker", wk.Name, "pane", wk.PaneID, "err", rErr)
		return
	}
	if agent.HasBackgroundTask(rr.Text) {
		return
	}

	// Strictly use pane-bound session ID (never cwd-guessed lookupRemoteSession).
	if cand.session != "" && wk.Session == "" {
		wk.Session = cand.session
	}
	if (cand.rule == "P3" || cand.rule == "P4") && wk.Session == "" {
		return
	}

	// Query ONLY target machine's pane.list immediately before closing to verify:
	// (a) wk.WorkspaceID contains strictly 1 pane matching wk.PaneID (never kill sibling user panes),
	// (b) for P1/P3/P4 the pane is still idle/done (never interrupt a newly started foreground turn).
	machine := workerMachineID(wk)
	panes, pErr := e.fleet.PanesOnMachine(ctx, machine)
	if pErr != nil {
		e.logger.Warn("worker gc: pane.list failed, aborting hibernation", "worker", wk.Name, "machine", machine, "err", pErr)
		e.recordWorkerGCFailBackoff(wk.Name, now)
		return
	}
	wsPaneCount := 0
	var targetPane *model.Pane
	for i := range panes {
		if panes[i].WorkspaceID == wk.WorkspaceID {
			wsPaneCount++
			if panes[i].ID == wk.PaneID {
				targetPane = &panes[i]
			}
		}
	}
	if wsPaneCount != 1 || targetPane == nil {
		e.logger.Info("worker gc: skipped non-exclusive workspace", "worker", wk.Name, "workspace", wk.WorkspaceID, "pane_count", wsPaneCount)
		return
	}
	if cand.rule == "P2" {
		if targetPane.AgentStatus == "working" {
			return
		}
	} else {
		if targetPane.AgentStatus != "idle" && targetPane.AgentStatus != "done" {
			return
		}
	}

	// Re-read fresh worker right before persisting Hibernating=true so a concurrent Keep=true
	// toggle during the screen/pane RPC is never clobbered.
	freshWk, fErr := e.store.GetWorker(ctx, cand.name)
	if fErr != nil || freshWk == nil || freshWk.Ended || freshWk.Hibernating || freshWk.Keep ||
		!isCorgiOwnedWorker(freshWk) || freshWk.PaneID != cand.paneID || freshWk.WorkspaceID != cand.workspaceID {
		return
	}
	if cand.session != "" && freshWk.Session == "" {
		freshWk.Session = cand.session
	}
	if (cand.rule == "P3" || cand.rule == "P4") && freshWk.Session == "" {
		return
	}
	wk = freshWk

	// Persist Hibernating=true BEFORE issuing CloseWorkspace so any crash or lost RPC reply
	// reconciles quietly in syncWorkers instead of marking the worker "gone" and alerting Router.
	wk.Hibernating = true
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return
	}

	// Close workspace immediately (no stopCodexTurn since GC only closes already-idle/faulted panes).
	closeErr := e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	if closeErr != nil && !isHerdrNotFoundErr(closeErr) {
		var he *herdr.Error
		if errors.As(closeErr, &he) {
			// Structured herdr rejection: workspace was NOT closed; revert Hibernating cleanly.
			e.logger.Warn("worker gc: close workspace rejected, keeping worker live", "worker", wk.Name, "workspace", wk.WorkspaceID, "err", closeErr)
			wk.Hibernating = false
			_ = e.store.UpsertWorker(ctx, wk)
			e.recordWorkerGCFailBackoff(wk.Name, time.Now())
			return
		}
		// Ambiguous transport/timeout error (workspace may already have closed on the Node):
		// leave Hibernating=true so syncWorkers reconciles cleanly on the next confirmed machine scan.
		e.logger.Warn("worker gc: close workspace transport error, deferring to sync reconciliation", "worker", wk.Name, "workspace", wk.WorkspaceID, "err", closeErr)
		e.recordWorkerGCFailBackoff(wk.Name, time.Now())
		return
	}

	e.finalizeHibernatedWorker(ctx, wk, cand.reason, time.Now())
}

func isHerdrNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var he *herdr.Error
	if errors.As(err, &he) {
		return strings.Contains(strings.ToLower(he.Code), "not_found")
	}
	return false
}

// finalizeHibernatedWorker archives the worker's session (preserving worktree path and metadata),
// marks the worker Ended=true, cleans up live subscriptions/dialogs/turn watermarks, and emits
// one quiet info event. Never deletes worktree directories.
func (e *Engine) finalizeHibernatedWorker(ctx context.Context, wk *model.Worker, reason string, now time.Time) {
	if wk == nil || e.store == nil {
		return
	}
	if wk.PaneID != "" {
		e.recordRecentlyClosedPane(wk.PaneID, now)
	}
	subs := e.collectWorkerSubscribers(ctx, wk.Name)
	if wk.WorkItemID != "" && wk.WorktreeBranch != "" {
		_ = e.store.RecordWorkItemBranch(ctx, wk.WorkItemID, wk.WorktreeBranch)
	}
	// Save rich EndedSession BEFORE clearing Hibernating/marking Ended so concurrent WatchEndings
	// sees the completed archive atomically.
	if es := e.buildWorkerEndedSession(ctx, wk, wk.Cwd, subs, now); es != nil {
		_ = e.store.SaveEndedSession(ctx, es, store.EndedSessionRetentionLimit)
	}

	wk.Ended = true
	wk.Hibernating = false
	wk.Status = "done"
	wk.FaultError = ""
	wk.FaultPaneID = ""
	wk.FaultAt = nil
	wk.StartupFailed = false
	_ = e.store.UpsertWorker(ctx, wk)

	if len(subs) > 0 {
		if threads, tErr := e.store.ListThreads(ctx, 200); tErr == nil {
			for _, th := range threads {
				if slices.Contains(th.Subscribers, wk.Name) {
					th.Subscribers = removeStr(th.Subscribers, wk.Name)
					_ = e.store.UpsertThread(ctx, th)
				}
			}
		}
	}
	_ = e.store.ExpireWorkerActions(ctx, wk.Name)
	e.ClearWeComFocusForWorker(ctx, wk.Name)
	if wk.PaneID != "" {
		_ = e.store.DeleteQueuedPromptsByScope(ctx, wk.PaneID)
		e.setQueuePaused(wk.PaneID, false)
		e.clearConstraintDelivered(wk.PaneID)
	}
	e.clearWorkerTurnState(wk.Name)

	note := "上下文与目录已保留，可随时续接"
	if wk.Session == "" {
		note = "进程已回收"
	}
	if reason != "" {
		e.addEvent(ctx, wk.Thread, fmt.Sprintf("已自动休眠 %s（%s，%s）", wk.Name, reason, note), wk.Name, "info")
	} else {
		e.addEvent(ctx, wk.Thread, fmt.Sprintf("已自动休眠 %s（%s）", wk.Name, note), wk.Name, "info")
	}
	e.notifyChanged()
}

// reconcileHibernatingWorker resolves a worker left in Hibernating=true after a Hub crash
// or lost CloseWorkspace reply once a fresh scan of its host machine completes.
func (e *Engine) reconcileHibernatingWorker(ctx context.Context, w *model.Worker, alive bool, scanned map[string]bool, now time.Time) {
	if w == nil || !w.Hibernating || e.isWorkerGCClosing(w.Name) {
		return
	}
	wkMachine := workerMachineID(w)
	if scanned != nil && !scanned[wkMachine] {
		return
	}
	if !alive {
		e.finalizeHibernatedWorker(ctx, w, "休眠对账完成", now)
		return
	}
	// Pane is still alive: clear Hibernating flag and back off so we don't thrash.
	w.Hibernating = false
	_ = e.store.UpsertWorker(ctx, w)
	e.recordWorkerGCFailBackoff(w.Name, now)
	e.notifyChanged()
}

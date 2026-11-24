package core

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// Run drives the periodic Super-Agent scheduling loop until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	if err := e.memory.WriteRouterDocs(e.binPath, e.Settings()); err != nil {
		e.logger.Warn("write router docs", "err", err)
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
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

func (e *Engine) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	// 1. Fire due timers.
	now := time.Now()
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

	rawAgents, err := e.fleet.Agents(ctx)
	if err != nil {
		return // herdr temporarily unreachable; preserve queued triggers
	}
	live := map[string]liveInfo{}
	for _, a := range rawAgents {
		if a.Name == nil || *a.Name == "" {
			continue
		}
		li := liveInfo{pane: a.PaneID, status: a.AgentStatus, ready: a.Ready}
		if a.Agent != nil {
			li.kind = *a.Agent
		}
		if a.Session != nil {
			li.session = extractSessionID(a.Session.Kind, a.Session.Value)
		}
		live[*a.Name] = li
	}

	// 2. Sync worker states, detect bare-shell failures, create Actions on blocked dialogs or quota errors.
	e.syncWorkers(ctx, live, now)

	// 3. Guard Router against trust blocks, transient API errors, and stalled turns.
	e.handleRouterLifecycle(ctx, live, now)

	queued, _ := e.store.ListQueuedTriggers(ctx)
	hasQueued := len(queued) > 0

	// 4. Drive or trigger nightly memory consolidation (Dream) and context handover.
	e.checkDream(ctx, live, hasQueued)
	e.checkHandover(ctx, live, hasQueued)

	if e.Settings().Paused || e.Dreaming() || e.handingOver() {
		return
	}

	// 5. Route and deliver queued triggers.
	e.deliverQueued(ctx, live)

	// 6. Watchdog for stalled workers.
	e.checkStalls(ctx, live, now)
}

func (e *Engine) syncWorkers(ctx context.Context, live map[string]liveInfo, now time.Time) {
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	for _, w := range workers {
		if w.Ended {
			// Restore worker if herdr brought its exact pane back after a daemon restart.
			if li, ok := live[w.Name]; ok && w.Status == "gone" && w.PaneID != "" && li.pane == w.PaneID {
				w.Ended = false
				w.Status = li.status
				_ = e.store.UpsertWorker(ctx, w)
				_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
					fmt.Sprintf("worker %s is back (herdr restored its pane; status %s).", w.Name, li.status), "", model.RouterName, w.Name)
				e.addEvent(ctx, w.Thread, w.Name+" 回来了（herdr 恢复了其窗格）", w.Name, "info")
				e.notifyChanged()
			}
			continue
		}
		li, alive := live[w.Name]
		if !alive {
			if w.Status != "starting" && w.Status != "failed" && time.Since(w.CreatedAt) > 20*time.Second {
				w.Ended = true
				w.Status = "gone"
				_ = e.store.UpsertWorker(ctx, w)
				_ = e.store.ExpireWorkerActions(ctx, w.Name)
				_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
					fmt.Sprintf("worker %s exited (pane gone).", w.Name), "", model.RouterName, w.Name)
				e.addEvent(ctx, w.Thread, w.Name+" 已退出", w.Name, "info")
				e.notifyChanged()
			}
			continue
		}
		if e.checkWorkerBareShell(ctx, w, li, now) {
			continue
		}
		if li.session != "" && li.session != w.Session {
			w.Session = li.session
		}
		prev := w.Status
		if li.status == prev && li.pane == w.PaneID {
			continue
		}
		w.PaneID = li.pane
		w.Status = li.status
		_ = e.store.UpsertWorker(ctx, w)
		e.notifyChanged()

		switch {
		case li.status == "blocked":
			e.mu.Lock()
			recentAns := now.Sub(e.lastAnswer[li.pane]) < 2*time.Second
			e.mu.Unlock()
			if !recentAns {
				e.handleWorkerBlocked(ctx, w)
			}
		case prev == "working" && (li.status == "done" || li.status == "idle"):
			_ = e.store.ExpireWorkerActions(ctx, w.Name)
			if now.Sub(w.ReportAt) > 90*time.Second {
				e.handleWorkerTurnDone(ctx, w)
			}
		}
	}
}

func (e *Engine) handleWorkerBlocked(ctx context.Context, w *model.Worker) {
	rr, err := e.fleet.Read(ctx, w.PaneID, "detection", "text", 0)
	if err != nil {
		return
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) == 0 {
		return
	}
	aid, _, err := e.store.NextID(ctx, "a")
	if err != nil {
		return
	}
	act := &model.Action{
		ID:           aid,
		Kind:         model.ActionKindWorkerDialog,
		Thread:       w.Thread,
		TargetWorker: w.Name,
		PaneID:       w.PaneID,
		Title:        ap.Title,
		Summary:      ap.Detail,
		Command:      ap.Command,
		Choices:      approvalToChoices(ap),
		Status:       model.ActionPending,
		CreatedAt:    time.Now(),
	}
	if err := e.store.InsertAction(ctx, act); err != nil {
		return
	}
	e.addEvent(ctx, w.Thread, w.Name+" 等你确认："+ap.Title, w.Name, "blocked")
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s is BLOCKED on a dialog (%s). Answer routine dialogs with `corgi answer %s <choice>`, or leave high-risk choices to the human.", w.Name, ap.Title, w.Name),
		"", model.RouterName, w.Name)

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionCreated(ctx, act)
	sink.OnStateChanged()
}

func (e *Engine) handleWorkerTurnDone(ctx context.Context, w *model.Worker) {
	screen := ""
	if rr, err := e.fleet.Read(ctx, w.PaneID, "recent", "text", 40); err == nil {
		screen = rr.Text
	}
	if qErr := agent.ExtractQuotaError(screen, w.LastReport); qErr != "" {
		_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s stopped on an agent/provider error (not finished): %s\nHand the job to another kind of agent and inform the human.", w.Name, qErr),
			"", model.RouterName, w.Name)
		e.addEvent(ctx, w.Thread, w.Name+" 出错了："+qErr, w.Name, "error")
		return
	}
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s finished a turn.", w.Name), "", model.RouterName, w.Name)
	e.addEvent(ctx, w.Thread, w.Name+" 完成了一轮", w.Name, "done")
}

func (e *Engine) deliverQueued(ctx context.Context, live map[string]liveInfo) {
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil || len(queued) == 0 {
		return
	}
	batches := map[string][]*model.Trigger{}
	for _, t := range queued {
		if len(t.Targets) == 0 {
			if t.Source != "worker" && t.Source != "chat" {
				if th, err := e.store.GetThread(ctx, t.Thread); err == nil {
					for _, sub := range th.Subscribers {
						if w, err := e.store.GetWorker(ctx, sub); err == nil && !w.Ended && sub != t.From {
							t.Targets = append(t.Targets, sub)
						}
					}
				}
			}
			if len(t.Targets) == 0 {
				t.Targets = []string{model.RouterName}
			}
		}
		for _, target := range t.Targets {
			batches[target] = append(batches[target], t)
		}
	}

	for target, list := range batches {
		li, alive := live[target]
		if !alive {
			if target == model.RouterName {
				e.ensureRouter(ctx)
			} else {
				e.rerouteDeadWorkerTriggers(ctx, target, list)
			}
			continue
		}
		if li.status == "blocked" || (!li.ready && li.status == "unknown") {
			continue
		}
		// Human chat/wecom messages interrupt Router mid-turn; system events wait for idle.
		if li.status == "working" {
			if target == model.RouterName {
				list = filterHumanTriggers(list)
			} else {
				list = filterWorkerSteeringTriggers(list)
			}
			if len(list) == 0 {
				continue
			}
		}
		e.deliverBatch(ctx, target, list)
	}
}

func filterHumanTriggers(list []*model.Trigger) []*model.Trigger {
	var out []*model.Trigger
	for _, t := range list {
		if t.Source == "chat" || t.Source == "wecom" {
			out = append(out, t)
		}
	}
	return out
}

func filterWorkerSteeringTriggers(list []*model.Trigger) []*model.Trigger {
	var out []*model.Trigger
	for _, t := range list {
		if t.Source == "router" || t.Source == "wecom" {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) rerouteDeadWorkerTriggers(ctx context.Context, target string, list []*model.Trigger) {
	if w, err := e.store.GetWorker(ctx, target); err == nil && !w.Ended && time.Since(w.CreatedAt) < 2*time.Minute {
		return // still booting
	}
	for _, t := range list {
		t.Targets = removeStr(t.Targets, target)
		if !slices.Contains(t.Targets, model.RouterName) {
			t.Targets = append(t.Targets, model.RouterName)
		}
		_ = e.store.UpdateTrigger(ctx, t)
	}
}

func (e *Engine) deliverBatch(ctx context.Context, target string, list []*model.Trigger) {
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if len(list) > 20 {
		list = list[:20]
	}

	e.mu.Lock()
	fresh := target == model.RouterName && e.routerFresh
	chars := e.settings.ReplyChars
	e.mu.Unlock()

	var b strings.Builder
	if fresh {
		b.WriteString(e.memory.SessionStartPreamble())
		b.WriteString("\n")
	}
	if len(list) == 1 {
		b.WriteString("[corgi] 1 new event\n")
	} else {
		fmt.Fprintf(&b, "[corgi] %d new events (batched)\n", len(list))
	}
	for _, t := range list {
		fmt.Fprintf(&b, "\n#%s · %s · %s", t.ID, t.Thread, t.Source)
		if t.From != "" {
			fmt.Fprintf(&b, " · from %s", t.From)
		}
		if t.Policy == "candidate" {
			b.WriteString(" · candidate (act only if a rule or memory covers it)")
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		body := strings.TrimSpace(t.Text)
		if r := []rune(body); len(r) > 700 { // Inline & Spill
			body = string(r[:700]) + fmt.Sprintf(" … [spilled; run `%s show %s`]", e.binPath, t.ID)
		}
		b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
	}
	if target == model.RouterName {
		fmt.Fprintf(&b, "\nReply with `%s reply`, usually ≤%d characters.", e.binPath, chars)
	}

	if err := e.fleet.Prompt(ctx, target, b.String()); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) && (he.Code == "agent_blocked" || he.Code == "agent_not_ready") {
			return
		}
		e.logger.Warn("deliver trigger batch failed", "target", target, "err", err)
		return
	}

	e.mu.Lock()
	if fresh {
		e.routerFresh = false
	}
	if target == model.RouterName && e.routerAskedAt.IsZero() {
		e.routerAskedAt = time.Now()
	}
	e.mu.Unlock()

	for _, t := range list {
		t.Targets = removeStr(t.Targets, target)
		if len(t.Targets) == 0 {
			t.Status = "delivered"
		}
		_ = e.store.UpdateTrigger(ctx, t)
	}
	e.notifyChanged()
}

// ensureRouter launches the resident Router session on the hub machine if absent.
func (e *Engine) ensureRouter(ctx context.Context) {
	e.mu.Lock()
	if e.routerLaunching {
		e.mu.Unlock()
		return
	}
	e.routerLaunching = true
	st := e.settings
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			e.routerLaunching = false
			e.mu.Unlock()
			e.notifyChanged()
		}()
		bg, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		kind := agent.NormalizeKind(st.RouterKind)
		if kind == "" {
			kind = "claude"
		}
		ad, err := e.agents.Get(kind)
		if err != nil {
			e.logger.Error("router adapter invalid", "kind", kind, "err", err)
			return
		}
		if err := e.memory.WriteRouterDocs(e.binPath, st); err != nil {
			e.logger.Warn("write router docs", "err", err)
		}
		if !st.AskPermissions {
			_ = ad.PreTrustWorkspace(e.home, e.memory.RouterDir())
		}
		args := ad.LaunchArgs(st.RouterModel, st.RouterEffort, !st.AskPermissions)
		_, paneID, err := e.fleet.CreateWorkspace(bg, "", e.memory.RouterDir(), model.RouterLabel)
		if err != nil {
			e.logger.Error("create router workspace failed", "err", err)
			return
		}
		if err := e.fleet.StartAgent(bg, paneID, kind, model.RouterName, args); err != nil {
			var he *herdr.Error
			if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
				e.logger.Error("start router agent failed", "err", err)
				return
			}
		}
		e.fleet.RememberName(paneID, model.RouterName)
		e.fleet.RememberRun(paneID, st.RouterModel, st.RouterEffort)

		e.mu.Lock()
		e.routerFresh = true
		e.routerLaunchAt = time.Now()
		e.routerRun = &[2]string{st.RouterModel, st.RouterEffort}
		e.mu.Unlock()
	}()
}

// RestartRouter closes the current Router workspace so the next tick spawns a fresh session.
func (e *Engine) RestartRouter(ctx context.Context) error {
	if ws, err := e.fleet.Workspaces(ctx); err == nil {
		for _, w := range ws {
			if w.Label == model.RouterLabel {
				_ = e.fleet.CloseWorkspace(ctx, w.ID)
			}
		}
	}
	e.mu.Lock()
	e.routerFresh = true
	e.routerAskedAt = time.Time{}
	e.mu.Unlock()
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
		if !ok || li.status != "working" {
			e.mu.Lock()
			delete(e.lastScreen, w.Name)
			e.mu.Unlock()
			continue
		}
		rr, err := e.fleet.Read(ctx, li.pane, "recent", "text", 30)
		if err != nil {
			continue
		}
		sum := sha1.Sum([]byte(rr.Text))
		h := hex.EncodeToString(sum[:8])

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

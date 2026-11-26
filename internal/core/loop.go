package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// Run drives the periodic Super-Agent scheduling loop until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	if changed, err := e.memory.WriteRouterDocsChanged(e.binPath, e.Settings()); err != nil {
		e.logger.Warn("write router docs", "err", err)
	} else if changed {
		e.queueRouterDocReread(ctx)
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
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
		li := liveInfo{pane: a.PaneID, status: a.AgentStatus, ready: a.Ready}
		if a.Agent != nil {
			li.kind = *a.Agent
		}
		if a.Session != nil {
			li.session = extractSessionID(a.Session.Kind, a.Session.Value)
		}
		if *a.Name == model.RouterName {
			li.status = e.stabilizeRouterStatus(a.PaneID, li.kind, li.status, now)
		}
		live[*a.Name] = li
	}
	if _, alive := live[model.RouterName]; !alive {
		e.clearRouterLive()
	}

	// Auto-adopt any live non-system agent session into Corgi's managed workers table
	// so no orphan unmanaged sessions ever exist.
	e.adoptUnmanagedAgents(ctx, rawAgents)

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

	// 5. Drain idle main-chat queue, route/deliver triggers, and drain idle single-agent queues.
	e.drainRouterPrompts(ctx, live)
	e.deliverQueued(ctx, live)
	e.drainPanePrompts(ctx, rawAgents)

	// 6. Watchdog for stalled workers and zero-token gated idle patrol / morning digest.
	e.checkStalls(ctx, live, now)
	e.checkIdlePatrol(ctx, live, now)
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

// adoptUnmanagedAgents ensures every live non-system coding agent across all connected machines
// is tracked in Corgi's managed workers table so it participates in status counts, dialog cards,
// and end-of-turn reporting.
func (e *Engine) adoptUnmanagedAgents(ctx context.Context, rawAgents []model.Agent) {
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	byName := map[string]*model.Worker{}
	byPane := map[string]*model.Worker{}
	for _, w := range workers {
		byName[w.Name] = w
		if !w.Ended && w.PaneID != "" {
			byPane[w.PaneID] = w
		}
	}

	changed := false
	for _, a := range rawAgents {
		if a.Name == nil || *a.Name == "" || a.PaneID == "" || a.Agent == nil {
			continue
		}
		name := *a.Name
		if name == model.RouterName || name == model.DreamName {
			continue
		}
		if byPane[a.PaneID] != nil {
			continue
		}
		if prev := byName[name]; prev != nil {
			if !prev.Ended {
				continue
			}
			// Leave "gone" workers with matching PaneID to syncWorkers' own restoration branch.
			if prev.Status == "gone" && prev.PaneID == a.PaneID {
				continue
			}
		}

		kind := agent.NormalizeKind(*a.Agent)
		if kind == "" {
			continue
		}
		_, machine := fleet.SplitID(a.PaneID)
		cwd := ""
		if a.Cwd != nil {
			cwd = *a.Cwd
		}
		sess := ""
		if a.Session != nil {
			sess = extractSessionID(a.Session.Kind, a.Session.Value)
		}
		taskDesc := strings.TrimSpace(a.TerminalTitle)
		if taskDesc == "" {
			taskDesc = "项目会话 · " + orStr(cwd, name)
		}
		st := a.AgentStatus
		if st == "" {
			st = "idle"
		}
		wk := &model.Worker{
			Name:        name,
			Kind:        kind,
			Machine:     machine,
			Cwd:         cwd,
			Task:        oneLineClip(taskDesc, 200),
			Thread:      model.MainThread,
			PaneID:      a.PaneID,
			WorkspaceID: a.WorkspaceID,
			CreatedBy:   "you",
			CreatedAt:   time.Now(),
			Status:      st,
			Session:     sess,
		}
		if e.fleet != nil {
			if run, ok := e.fleet.Run(a.PaneID); ok {
				wk.Run = run
				wk.Model, wk.Effort = run[0], run[1]
			}
		}
		if uErr := e.store.UpsertWorker(ctx, wk); uErr == nil {
			byName[name] = wk
			byPane[a.PaneID] = wk
			changed = true
		}
	}
	if changed {
		e.notifyChanged()
	}
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
				e.invalidateBypassCheck(w.PaneID)
				_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
					fmt.Sprintf("worker %s is back (herdr restored its pane; status %s).", w.Name, li.status), "", model.RouterName, w.Name)
				e.addEvent(ctx, w.Thread, w.Name+" 回来了（herdr 恢复了其窗格）", w.Name, "info")
				e.notifyChanged()
			}
			continue
		}
		li, alive := live[w.Name]
		if !alive {
			if e.isHealingBypassWorker(w.Name, w.PaneID) {
				continue
			}
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
		if e.isHealingBypassWorker(w.Name, li.pane) {
			continue
		}
		if e.checkWorkerBareShell(ctx, w, li, now) {
			continue
		}
		sessionChanged := false
		if li.session != "" && li.session != w.Session {
			w.Session = li.session
			sessionChanged = true
		}
		// herdr reports "idle" while an agent sits on a first-run wizard or folder trust prompt.
		if li.status == "idle" && e.shouldCheckIdleDialog(li.pane, now) {
			if rr, rErr := e.fleet.Read(ctx, li.pane, "detection", "text", 0); rErr == nil && agent.ScreenAsks(rr.Text) {
				li.status = "blocked"
				live[w.Name] = li
			}
		}
		prev := w.Status
		if prev == "working" && (li.status == "done" || li.status == "idle") {
			e.invalidateBypassCheck(li.pane)
		}
		if li.status == "idle" || li.status == "done" {
			e.maybeHealWorkerBypass(ctx, w, li, now)
		}
		idleChanged := false
		if li.status == "idle" || li.status == "done" {
			if w.IdleSince == nil {
				t := now
				if (prev == "idle" || prev == "done") && !w.ReportAt.IsZero() && w.ReportAt.Before(now) {
					t = w.ReportAt
				}
				w.IdleSince = &t
				idleChanged = true
			}
		} else if w.IdleSince != nil || w.IdlePatrolCount > 0 || w.SnoozeUntil != nil {
			resetWorkerIdle(w)
			idleChanged = true
		}
		if li.status == prev && li.pane == w.PaneID {
			if idleChanged || sessionChanged {
				_ = e.store.UpsertWorker(ctx, w)
			}
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

func (e *Engine) shouldCheckIdleDialog(paneID string, now time.Time) bool {
	if paneID == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Sub(e.idleCheckAt[paneID]) < 4500*time.Millisecond {
		return false
	}
	e.idleCheckAt[paneID] = now
	return true
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
	if e.answerWorkerTrust(ctx, w, ap) {
		return
	}
	if e.answerRestoredAgyEditDialog(ctx, w, ap) {
		return
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, p := range pending {
			if p.Kind == model.ActionKindWorkerDialog && p.TargetWorker == w.Name && p.Title == ap.Title {
				return
			}
		}
	}
	aid, _, err := e.store.NextID(ctx, "a")
	if err != nil {
		return
	}
	act := &model.Action{
		ID:           aid,
		Kind:         model.ActionKindWorkerDialog,
		Category:     "blocked",
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
	summary := e.lastWorkerSummary(ctx, w)
	choices := formatChoicesText(ap)
	e.addEvent(ctx, w.Thread, w.Name+" 等你确认："+ap.Title, w.Name, "blocked")
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s is BLOCKED on a terminal dialog (also shown as a card in chat).\nScreen:\n%s%s\nIf the answer follows from the task or is routine, answer it yourself: `%s answer %s <choice>`. Leave high-risk choices to the human.",
			w.Name, summary, choices, e.binPath, w.Name),
		"", model.RouterName, w.Name)

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionCreated(ctx, act)
	sink.OnStateChanged()
}

func (e *Engine) handleWorkerTurnDone(ctx context.Context, w *model.Worker) {
	summary := e.lastWorkerSummary(ctx, w)
	screen := ""
	if rr, err := e.fleet.Read(ctx, w.PaneID, "recent", "text", 40); err == nil {
		screen = rr.Text
	}
	checkAgainst := summary
	if w.LastReport != "" {
		checkAgainst += "\n" + w.LastReport
	}
	if qErr := agent.ExtractQuotaError(screen, checkAgainst); qErr != "" {
		_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
			fmt.Sprintf("worker %s stopped on an agent/provider error (not finished): %s\nLast message before it:\n%s\nHand the job to another kind of agent or computer and inform the human in one line.", w.Name, qErr, summary),
			"", model.RouterName, w.Name)
		e.addEvent(ctx, w.Thread, w.Name+" 出错了："+qErr, w.Name, "error")
		return
	}
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name,
		fmt.Sprintf("worker %s finished a turn. Last message:\n%s", w.Name, summary), "", model.RouterName, w.Name)
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
		if li.status == "blocked" || (!li.ready && li.status == "unknown") || e.isHealingBypassWorker(target, li.pane) {
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
		e.deliverBatch(ctx, target, li.pane, list)
	}
}

func filterHumanTriggers(list []*model.Trigger) []*model.Trigger {
	var out []*model.Trigger
	for _, t := range list {
		if t.Steer || t.Source == "wecom" {
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

func (e *Engine) deliverBatch(ctx context.Context, target, paneID string, list []*model.Trigger) {
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
			showBin := e.binPath
			if target != model.RouterName || showBin == "" {
				showBin = "corgi"
			}
			body = string(r[:700]) + fmt.Sprintf(" … [spilled; run `%s show %s`]", showBin, t.ID)
		}
		b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
	}
	if target == model.RouterName {
		fmt.Fprintf(&b, "\nReply with `%s reply` (usually ≤%d Chinese chars / English words; for 2+ items use `- ` Markdown bullets via stdin heredoc `<<'EOF'`, never pack into one semicolon-separated line).", e.binPath, chars)
	}

	promptTarget := target
	if paneID != "" {
		promptTarget = paneID
	}
	if err := e.fleet.Prompt(ctx, promptTarget, b.String()); err != nil {
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
	if target == model.RouterName {
		if e.routerAskedAt.IsZero() {
			e.routerAskedAt = time.Now()
		}
		e.routerRepliedAt = time.Time{}
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
	if e.fleet == nil {
		return
	}
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
		if e.agents.CheckInstalled(e.home, kind) != nil {
			installed := e.agents.DetectInstalled(e.home, true)
			if len(installed) == 0 {
				e.logger.Error("no agent CLI installed on hub for router", "configured", kind)
				return
			}
			e.logger.Warn("configured router agent not installed on hub, falling back", "configured", kind, "fallback", installed[0])
			kind = installed[0]
		}
		ad, err := e.agents.Get(kind)
		if err != nil {
			e.logger.Error("router adapter invalid", "kind", kind, "err", err)
			return
		}
		if err := e.memory.WriteRouterDocs(e.binPath, st); err != nil {
			e.logger.Warn("write router docs", "err", err)
		}
		e.memory.MarkRouterDocsRead()
		e.mu.Lock()
		rereadID := e.rereadTrigID
		e.rereadTrigID = ""
		e.mu.Unlock()
		if rereadID != "" {
			if tr, err := e.store.GetTrigger(bg, rereadID); err == nil && tr.Status == "queued" {
				tr.Status = "ignored"
				_ = e.store.UpdateTrigger(bg, tr)
			}
		}
		if !st.AskPermissions {
			_ = ad.PreTrustWorkspace(e.home, e.memory.RouterDir())
		}
		args := ad.LaunchArgs(st.RouterModel, st.RouterEffort, !st.AskPermissions)
		e.closeWorkspacesByLabel(bg, model.RouterLabel)
		wsID, paneID, err := e.fleet.CreateWorkspace(bg, "", e.memory.RouterDir(), model.RouterLabel)
		if err != nil {
			e.logger.Error("create router workspace failed", "err", err)
			return
		}
		if err := e.fleet.StartAgent(bg, paneID, kind, model.RouterName, args); err != nil {
			var he *herdr.Error
			if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
				e.logger.Error("start router agent failed, closing workspace", "kind", kind, "err", err)
				_ = e.fleet.CloseWorkspace(bg, wsID)
				return
			}
		}
		e.fleet.RememberName(paneID, model.RouterName)
		e.fleet.RememberRun(paneID, st.RouterModel, st.RouterEffort)

		runPair := [2]string{st.RouterModel, st.RouterEffort}
		if b, err := json.Marshal(runPair); err == nil {
			_ = e.store.SetKV(bg, "router_run", string(b))
		}

		e.mu.Lock()
		e.routerFresh = true
		e.routerLaunchAt = time.Now()
		e.routerRun = &runPair
		e.routerStallAlerted = false
		e.routerRetryPending = false
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
	_ = e.store.SetKV(ctx, "router_run", "")
	e.mu.Lock()
	e.routerFresh = true
	e.routerAskedAt = time.Time{}
	e.routerRepliedAt = time.Time{}
	e.routerRetries = 0
	e.routerRetryPending = false
	e.routerNudged = false
	e.routerStallAlerted = false
	e.routerRun = nil
	e.routerLastAlive = false
	e.routerLastPane = ""
	e.routerLastStatus = ""
	e.routerLastSeenAt = time.Time{}
	e.routerWorkingAt = time.Time{}
	e.routerIdleSince = time.Time{}
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

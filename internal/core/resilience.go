package core

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

var (
	uuidRe          = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	trustHintRe     = regexp.MustCompile(`(?i)(trust (this|the contents)|do you trust|hooks? need review|信任此目录|信任该目录)`)
	routerRetryWait = []time.Duration{20 * time.Second, time.Minute, 3 * time.Minute}
)

const (
	routerRetryMark        = "[corgi retry"
	routerErrDedupDefault  = 10 * time.Minute
	routerMinSilentDefault = 1 * time.Second
)

// extractSessionID normalizes herdr's agent_session into a resumable session ID.
func extractSessionID(kind, value string) string {
	if kind != "path" {
		return value
	}
	ids := uuidRe.FindAllString(filepath.Base(value), -1)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

// closeWorkspacesByLabel closes all hub-local workspaces matching label.
func (e *Engine) closeWorkspacesByLabel(ctx context.Context, label string) {
	ws, err := e.fleet.Workspaces(ctx)
	if err != nil {
		return
	}
	for _, w := range ws {
		if w.Label == label && !strings.Contains(w.ID, "@") {
			_ = e.fleet.CloseWorkspace(ctx, w.ID)
		}
	}
}

// trustOwnDir auto-answers a workspace-trust or own-hooks dialog inside an unattended
// Router/Dream session when bypass permissions are enabled (rate-limited to once per 15s).
func (e *Engine) trustOwnDir(ctx context.Context, paneID string) {
	if paneID == "" {
		return
	}
	e.mu.Lock()
	if time.Since(e.lastTrust[paneID]) < 15*time.Second {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	rr, err := e.fleet.Read(ctx, paneID, "detection", "text", 0)
	if err != nil {
		return
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) < 2 {
		return
	}
	if !trustHintRe.MatchString(ap.Title + "\n" + ap.Detail + "\n" + ap.Raw) {
		return
	}

	e.mu.Lock()
	e.lastTrust[paneID] = time.Now()
	e.mu.Unlock()

	key := ap.Choices[0].Key
	cursor := ap.Choices[0].Cursor
	if ap.Style == "cursor" {
		key = ""
	}
	_ = e.sendDialogAnswer(ctx, paneID, key, cursor)
}

// handleRouterLifecycle guards the resident Router against folder-trust blocks,
// transient API/provider errors (progressive async backoff 20s -> 1m -> 3m), and stalled turns.
func (e *Engine) handleRouterLifecycle(ctx context.Context, live map[string]liveInfo, now time.Time) {
	li, alive := live[model.RouterName]
	if !alive {
		e.mu.Lock()
		e.routerPrevStatus = ""
		delete(e.lastScreen, model.RouterName)
		e.mu.Unlock()
		return
	}

	st := e.Settings()
	if !st.AskPermissions && (li.status == "blocked" || (li.status == "idle" && now.Sub(e.routerLaunchAt) < 2*time.Minute)) {
		e.trustOwnDir(ctx, li.pane)
	}

	e.mu.Lock()
	prev := e.routerPrevStatus
	e.routerPrevStatus = li.status
	askedAt := e.routerAskedAt
	retries := e.routerRetries
	nudged := e.routerNudged
	stallAlerted := e.routerStallAlerted
	busySpecial := e.handover != nil || e.dream != nil
	e.mu.Unlock()

	if busySpecial {
		return
	}
	if !st.AskPermissions && (li.status == "idle" || li.status == "done") {
		if e.maybeHealRouterBypass(ctx, li, now) {
			return
		}
	}

	// 1. Detect Router turn completion without a reply -> inspect screen after our last prompt.
	if prev == "working" && (li.status == "idle" || li.status == "done") {
		e.mu.Lock()
		delete(e.lastScreen, model.RouterName)
		e.routerNudged = false
		e.routerStallAlerted = false
		e.mu.Unlock()

		if !askedAt.IsZero() {
			e.inspectRouterSilentTurn(ctx, li.pane, retries)
		}
		return
	}

	// 2. Router Watchdog: respect StallMinutes (minimum 2m).
	// First strike sends Esc + 2s wait + nudge; second strike alerts the human without destructively restarting Router.
	if li.status == "working" && st.StallMinutes > 0 {
		stallDur := time.Duration(st.StallMinutes) * time.Minute
		if stallDur < 2*time.Minute {
			stallDur = 2 * time.Minute
		}
		rr, err := e.fleet.Read(ctx, li.pane, "detection", "text", 0)
		if err != nil {
			return
		}
		h := agent.ProgressHash(rr.Text)

		e.mu.Lock()
		mk, seen := e.lastScreen[model.RouterName]
		if !seen || mk.hash != h {
			e.lastScreen[model.RouterName] = screenMark{hash: h, since: now}
			e.routerStallAlerted = false
			e.mu.Unlock()
			return
		}
		shouldNudge := !nudged && !mk.told && now.Sub(mk.since) >= stallDur
		shouldAlert := nudged && !stallAlerted && now.Sub(mk.since) >= stallDur
		if shouldNudge {
			mk.told = true
			mk.since = now // reset clock for 2nd-strike alert
			e.lastScreen[model.RouterName] = mk
			e.routerNudged = true
		} else if shouldAlert {
			e.routerStallAlerted = true
		}
		e.mu.Unlock()

		if shouldNudge {
			_ = e.fleet.SendKeys(ctx, li.pane, "esc")
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			_ = e.fleet.Prompt(ctx, model.RouterName,
				"[corgi watchdog] Your previous turn appeared stalled. Resume handling pending events or inform the human briefly.")
			e.addEvent(ctx, model.MainThread, "Corgi 大脑似乎卡住了，已尝试自动轻推恢复", model.RouterName, "info")
			e.notifyChanged()
		} else if shouldAlert {
			e.logger.Warn("router remained stalled after watchdog nudge; alerting human without restarting")
			e.addEvent(ctx, model.MainThread, "Corgi 大脑持续无响应，请打开大脑终端查看或在顶部手动重启（为避免丢失上下文未自动强杀）", model.RouterName, "error")
			e.notifyChanged()
		}
	}
}

func (e *Engine) routerRetrySchedule() []time.Duration {
	e.mu.Lock()
	custom := e.routerRetryDelays
	e.mu.Unlock()
	if len(custom) > 0 {
		return custom
	}
	return routerRetryWait
}

func formatRetryWait(d time.Duration) string {
	if d < time.Minute {
		secs := int(d.Seconds())
		if secs < 1 {
			secs = 1
		}
		return fmt.Sprintf("%d 秒", secs)
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

// extractPostPromptScreen slices screen to only lines printed after our most recent prompt marker,
// preventing stale error strings in earlier scrollback from triggering false retries.
func extractPostPromptScreen(screen string) string {
	lines := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	start := 0
	for i, line := range lines {
		if strings.Contains(line, "corgi reply") ||
			strings.Contains(line, routerRetryMark) ||
			strings.Contains(line, "[corgi watchdog]") ||
			strings.Contains(line, "[corgi] ") {
			start = i + 1
		}
	}
	if start >= len(lines) {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

func (e *Engine) inspectRouterSilentTurn(ctx context.Context, paneID string, retries int) {
	e.mu.Lock()
	askedAt := e.routerAskedAt
	minAge := e.routerMinSilentAge
	e.mu.Unlock()
	if minAge == 0 {
		minAge = routerMinSilentDefault
	}
	if minAge > 0 && !askedAt.IsZero() && time.Since(askedAt) < minAge {
		return
	}

	screen := ""
	if rr, err := e.fleet.Read(ctx, paneID, "recent", "text", 80); err == nil {
		screen = extractPostPromptScreen(rr.Text)
	}
	qErr := agent.ExtractQuotaError(screen, "")
	if qErr == "" {
		e.mu.Lock()
		e.routerAskedAt = time.Time{}
		e.routerRetries = 0
		e.routerRetryPending = false
		sink := e.sink
		e.mu.Unlock()
		if sink != nil {
			sink.OnRouterTurnEnd(ctx)
		}
		return
	}

	delays := e.routerRetrySchedule()
	if retries < len(delays) {
		wait := delays[retries]
		attempt := retries + 1
		e.mu.Lock()
		e.routerAskedAt = time.Time{}
		e.routerRetries = attempt
		e.routerRetryPending = true
		parentCtx := e.runCtx
		e.mu.Unlock()
		e.logger.Warn("router hit provider error, scheduling backoff retry",
			"attempt", attempt, "max", len(delays), "wait", wait, "err", qErr)
		e.addEvent(ctx, model.MainThread,
			fmt.Sprintf("Corgi 大脑遇到 API 错误（%s），%s后自动重试（%d/%d）", oneLineClip(qErr, 80), formatRetryWait(wait), attempt, len(delays)),
			model.RouterName, "info")
		e.notifyChanged()
		go e.runRouterBackoffRetry(parentCtx, wait, attempt, len(delays), qErr)
		return
	}

	now := time.Now()
	e.mu.Lock()
	e.routerAskedAt = time.Time{}
	e.routerRetries = 0
	e.routerRetryPending = false
	dedupWin := e.routerErrDedupWindow
	if dedupWin <= 0 {
		dedupWin = routerErrDedupDefault
	}
	suppressed := e.routerLastErr == qErr && !e.routerLastErrAt.IsZero() && now.Sub(e.routerLastErrAt) < dedupWin
	if !suppressed {
		e.routerLastErr = qErr
		e.routerLastErrAt = now
	}
	e.mu.Unlock()
	if suppressed {
		return
	}
	e.addEvent(ctx, model.MainThread,
		fmt.Sprintf("Corgi 大脑调用出错（已自动重试 %d 次仍失败）：%s", len(delays), qErr),
		model.RouterName, "error")
	e.notifyChanged()
}

func (e *Engine) runRouterBackoffRetry(parentCtx context.Context, wait time.Duration, attempt, maxAttempts int, qErr string) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-parentCtx.Done():
		e.mu.Lock()
		e.routerRetryPending = false
		e.mu.Unlock()
		return
	case <-timer.C:
	}

	e.mu.Lock()
	e.routerRetryPending = false
	busy := !e.routerAskedAt.IsZero() || e.routerLaunching || e.handover != nil || e.dream != nil || e.settings.Paused
	e.mu.Unlock()
	if busy {
		return
	}

	pctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	prompt := fmt.Sprintf("%s %d/%d] Previous turn stopped on a provider error (%s). It may have passed — please retry handling the latest events.",
		routerRetryMark, attempt, maxAttempts, qErr)
	if err := e.fleet.Prompt(pctx, model.RouterName, prompt); err != nil {
		e.logger.Warn("router backoff retry prompt failed", "attempt", attempt, "err", err)
		return
	}
	e.mu.Lock()
	if e.routerAskedAt.IsZero() {
		e.routerAskedAt = time.Now()
	}
	e.mu.Unlock()
}

// checkWorkerBareShell detects a spawned worker whose CLI crashed or dropped back to a bare shell.
// Returns true when the worker was marked failed and handled.
func (e *Engine) checkWorkerBareShell(ctx context.Context, w *model.Worker, li liveInfo, now time.Time) bool {
	if li.kind != "" {
		e.mu.Lock()
		delete(e.noAgent, w.Name)
		e.mu.Unlock()
		return false
	}

	e.mu.Lock()
	since, seen := e.noAgent[w.Name]
	if !seen {
		e.noAgent[w.Name] = now
		since = now
	}
	failed := now.Sub(since) > 30*time.Second && now.Sub(w.CreatedAt) > 45*time.Second
	if failed {
		delete(e.noAgent, w.Name)
	}
	e.mu.Unlock()

	if !failed {
		return true // still in grace window
	}

	w.Ended = true
	w.Status = "failed"
	w.PaneID = li.pane
	_ = e.store.UpsertWorker(ctx, w)
	_ = e.store.ExpireWorkerActions(ctx, w.Name)

	// Clear queued triggers solely targeting this dead worker so they don't hang forever.
	if queued, err := e.store.ListQueuedTriggers(ctx); err == nil {
		for _, t := range queued {
			if len(t.Targets) == 1 && t.Targets[0] == w.Name {
				t.Status = "ignored"
				_ = e.store.UpdateTrigger(ctx, t)
			}
		}
	}

	tail := ""
	if rr, err := e.fleet.Read(ctx, li.pane, "recent", "text", 12); err == nil {
		tail = oneLineClip(rr.Text, 240)
	}
	msg := fmt.Sprintf("worker %s failed to start (pane dropped to bare shell). Retry with another agent, model, or computer.", w.Name)
	if tail != "" {
		msg += "\nLast terminal lines: " + tail
	}
	_, _ = e.Ingest(ctx, "worker", w.Thread, w.Name, msg, "", model.RouterName, w.Name)
	e.addEvent(ctx, w.Thread, w.Name+" 启动失败（已退回裸终端）", w.Name, "error")
	e.notifyChanged()
	return true
}

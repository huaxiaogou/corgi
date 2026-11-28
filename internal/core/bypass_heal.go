package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

const (
	defaultBypassCheckInterval     = 60 * time.Second
	defaultBypassHealFailBackoff   = 10 * time.Minute
	defaultBypassExitPoll          = 350 * time.Millisecond
	defaultBypassExitTimeout       = 6 * time.Second
	defaultBypassStartRetryTimeout = 12 * time.Second
)

func (e *Engine) getBypassCheckInterval() time.Duration {
	if e.bypassCheckInterval > 0 {
		return e.bypassCheckInterval
	}
	return defaultBypassCheckInterval
}

func (e *Engine) getBypassExitPoll() time.Duration {
	if e.bypassExitPoll > 0 {
		return e.bypassExitPoll
	}
	return defaultBypassExitPoll
}

func (e *Engine) getBypassExitTimeout() time.Duration {
	if e.bypassExitTimeout > 0 {
		return e.bypassExitTimeout
	}
	return defaultBypassExitTimeout
}

func (e *Engine) getBypassStartRetryTimeout() time.Duration {
	if e.bypassStartRetryTimeout > 0 {
		return e.bypassStartRetryTimeout
	}
	return defaultBypassStartRetryTimeout
}

func (e *Engine) isHealingBypassPane(paneID string) bool {
	if paneID == "" {
		return false
	}
	e.mu.Lock()
	_, ok := e.healingBypass[paneID]
	e.mu.Unlock()
	return ok
}

func (e *Engine) isHealingBypassWorker(workerName, paneID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if paneID != "" {
		if _, ok := e.healingBypass[paneID]; ok {
			return true
		}
	}
	if workerName != "" {
		for _, name := range e.healingBypass {
			if name == workerName {
				return true
			}
		}
	}
	return false
}

func (e *Engine) invalidateBypassCheck(paneID string) {
	if paneID == "" {
		return
	}
	e.mu.Lock()
	delete(e.bypassCheckedAt, paneID)
	delete(e.bypassHealedPID, paneID)
	e.mu.Unlock()
}

// maybeHealWorkerBypass detects when herdr restored a managed worker pane without its
// bypass-permissions flag (e.g. bare `agy --conversation <id>` after a herdr server restart)
// while AskPermissions is false, and seamlessly relaunches the same session in-place when idle.
func (e *Engine) maybeHealWorkerBypass(ctx context.Context, w *model.Worker, li liveInfo, now time.Time) {
	if e.fleet == nil || w == nil || w.Ended || w.Hibernating || w.PaneID == "" || w.Session == "" || li.kind == "" {
		return
	}
	if e.isWorkerGCClosing(w.Name) {
		return
	}
	if e.Settings().AskPermissions {
		return
	}
	if li.status != "idle" && li.status != "done" {
		return
	}
	kind := agent.NormalizeKind(orStr(w.Kind, li.kind))
	if kind == "" {
		return
	}

	paneID := li.pane
	if paneID == "" {
		paneID = w.PaneID
	}
	checkEvery := e.getBypassCheckInterval()

	e.mu.Lock()
	if _, active := e.healingBypass[paneID]; active {
		e.mu.Unlock()
		return
	}
	if lastFail := e.bypassHealFailAt[paneID]; !lastFail.IsZero() && now.Sub(lastFail) < defaultBypassHealFailBackoff {
		e.mu.Unlock()
		return
	}
	if lastCheck := e.bypassCheckedAt[paneID]; !lastCheck.IsZero() && now.Sub(lastCheck) < checkEvery {
		e.mu.Unlock()
		return
	}
	e.bypassCheckedAt[paneID] = now
	e.mu.Unlock()

	info, err := e.fleet.ProcessInfo(ctx, paneID)
	if err != nil {
		return
	}
	need, pid := agent.NeedsBypassHeal(kind, info)
	if !need {
		return
	}

	// Never disturb a worker with an active pending dialog card.
	if pending, pErr := e.store.ListPendingActions(ctx); pErr == nil {
		for _, a := range pending {
			if a.Kind == model.ActionKindWorkerDialog && (a.TargetWorker == w.Name || a.PaneID == paneID) {
				return
			}
		}
	}

	e.mu.Lock()
	if _, active := e.healingBypass[paneID]; active {
		e.mu.Unlock()
		return
	}
	if pid != 0 && e.bypassHealedPID[paneID] == pid {
		e.mu.Unlock()
		return
	}
	e.healingBypass[paneID] = w.Name
	if pid != 0 {
		e.bypassHealedPID[paneID] = pid
	}
	e.mu.Unlock()

	wkCopy := *w
	wkCopy.PaneID = paneID
	wkCopy.Kind = kind
	go e.healWorkerBypassInPlace(wkCopy, pid)
}

func (e *Engine) healWorkerBypassInPlace(w model.Worker, targetPID uint32) {
	defer func() {
		e.mu.Lock()
		delete(e.healingBypass, w.PaneID)
		e.mu.Unlock()
		e.wake()
	}()

	hctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Re-check Settings and live agent state right before exiting so we never interrupt a brand-new turn.
	st := e.Settings()
	if st.AskPermissions {
		return
	}
	if ags, err := e.fleet.Agents(hctx); err == nil {
		stillIdle := false
		for _, a := range ags {
			if a.PaneID == w.PaneID {
				stillIdle = a.AgentStatus == "idle" || a.AgentStatus == "done"
				break
			}
		}
		if !stillIdle {
			e.invalidateBypassCheck(w.PaneID)
			return
		}
	}
	info, err := e.fleet.ProcessInfo(hctx, w.PaneID)
	if err != nil {
		e.recordBypassHealFailure(w.PaneID)
		return
	}
	need, curPID := agent.NeedsBypassHeal(w.Kind, info)
	if !need || (targetPID != 0 && curPID != 0 && curPID != targetPID) {
		return
	}

	ad, err := e.agents.Get(w.Kind)
	if err != nil {
		e.recordBypassHealFailure(w.PaneID)
		return
	}

	e.preTrustWorkspace(hctx, w.Machine, w.Kind, w.Cwd)

	// Cleanly exit the restored CLI back to its interactive shell prompt inside the exact same pane.
	if err := e.exitAgentToShell(hctx, w.PaneID); err != nil {
		e.logger.Warn("bypass self-heal: agent did not return to shell", "worker", w.Name, "pane", w.PaneID, "err", err)
		e.recordBypassHealFailure(w.PaneID)
		return
	}

	modelName, effort := w.Model, w.Effort
	if w.Kind == agent.NormalizeKind(st.WorkerKind) {
		if modelName == "" {
			modelName = st.WorkerModel
		}
		if effort == "" {
			effort = st.WorkerEffort
		}
	}
	args := append(ad.ResumeArgs(w.Session), ad.LaunchArgs(modelName, effort, true)...)
	if w.Kind == "claude" {
		workerBin := e.binPath
		if w.Machine != "" || workerBin == "" {
			workerBin = "corgi"
		}
		args = append(args, "--allowedTools", fmt.Sprintf("Bash(%s report:*),Bash(corgi report:*)", workerBin))
	}

	// Retry StartAgent briefly while herdr releases the previous agent_name on the same pane.
	startDeadline := time.Now().Add(e.getBypassStartRetryTimeout())
	var startErr error
	for {
		startErr = e.fleet.StartAgent(hctx, w.PaneID, w.Kind, w.Name, args)
		if startErr == nil {
			break
		}
		var he *herdr.Error
		if errors.As(startErr, &he) && he.Code == "agent_not_ready" {
			startErr = nil
			break
		}
		if !isRetryableInPlaceStartErr(startErr) || time.Now().After(startDeadline) {
			break
		}
		select {
		case <-hctx.Done():
			startErr = hctx.Err()
			break
		case <-time.After(e.getBypassExitPoll()):
		}
		if hctx.Err() != nil {
			break
		}
	}
	if startErr != nil {
		e.logger.Warn("bypass self-heal: failed to relaunch agent in-place", "worker", w.Name, "pane", w.PaneID, "err", startErr)
		e.recordBypassHealFailure(w.PaneID)
		return
	}

	e.fleet.RememberName(w.PaneID, w.Name)
	e.fleet.RememberRun(w.PaneID, modelName, effort)

	e.mu.Lock()
	e.bypassCheckedAt[w.PaneID] = time.Now()
	delete(e.bypassHealFailAt, w.PaneID)
	e.mu.Unlock()

	e.logger.Info("healed worker bypass permissions in-place", "worker", w.Name, "kind", w.Kind, "pane", w.PaneID, "session", w.Session)
	e.addEvent(hctx, w.Thread, fmt.Sprintf("已自动为 %s 恢复免审批模式（herdr 重启后自愈）", w.Name), w.Name, "info")
	e.notifyChanged()
}

func (e *Engine) exitAgentToShell(ctx context.Context, paneID string) error {
	_ = e.fleet.SendText(ctx, paneID, "/exit", true)
	if e.waitForPaneShell(ctx, paneID, e.getBypassExitTimeout()) {
		return nil
	}
	// Fallback for CLIs where /exit is not recognized or requires EOF/SIGINT.
	_ = e.fleet.SendKeys(ctx, paneID, "ctrl+d")
	if e.waitForPaneShell(ctx, paneID, e.getBypassExitTimeout()/2) {
		return nil
	}
	_ = e.fleet.SendKeys(ctx, paneID, "ctrl+c")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(e.getBypassExitPoll()):
	}
	_ = e.fleet.SendKeys(ctx, paneID, "ctrl+c")
	if e.waitForPaneShell(ctx, paneID, e.getBypassExitTimeout()/2) {
		return nil
	}
	return fmt.Errorf("pane %s did not return to shell prompt", paneID)
}

func (e *Engine) waitForPaneShell(ctx context.Context, paneID string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultBypassExitTimeout
	}
	poll := e.getBypassExitPoll()
	deadline := time.Now().Add(timeout)
	for {
		info, err := e.fleet.ProcessInfo(ctx, paneID)
		if err == nil && info != nil && info.IsAtShellPrompt() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

func (e *Engine) recordBypassHealFailure(paneID string) {
	e.mu.Lock()
	e.bypassHealFailAt[paneID] = time.Now()
	e.mu.Unlock()
}

func isRetryableInPlaceStartErr(err error) bool {
	var he *herdr.Error
	return errors.As(err, &he) && (he.Code == "agent_name_taken" || he.Code == "agent_pane_busy")
}

// answerRestoredAgyEditDialog auto-accepts an Antigravity file-edit confirmation prompt when
// AskPermissions is false AND pane.process_info confirms the pane lost --dangerously-skip-permissions
// (for example, mid-turn after a herdr restart). It also invalidates the bypass check cache so
// the worker is healed to full bypass mode as soon as the turn finishes.
func (e *Engine) answerRestoredAgyEditDialog(ctx context.Context, w *model.Worker, ap model.Approval) bool {
	if e.fleet == nil || w == nil || w.PaneID == "" || e.Settings().AskPermissions || len(ap.Choices) < 2 {
		return false
	}
	if agent.NormalizeKind(w.Kind) != "agy" {
		return false
	}
	titleLow := strings.ToLower(ap.Title)
	fullLow := strings.ToLower(ap.Title + "\n" + ap.Detail + "\n" + ap.Raw)
	if !strings.Contains(titleLow, "accept this file edit") || !strings.Contains(fullLow, "shift+tab to auto-approve file edits") {
		return false
	}
	firstLabel := strings.ToLower(ap.Choices[0].Label)
	if !strings.Contains(firstLabel, "yes") && !strings.Contains(firstLabel, "accept") {
		return false
	}

	info, err := e.fleet.ProcessInfo(ctx, w.PaneID)
	if err != nil {
		return false
	}
	need, _ := agent.NeedsBypassHeal("agy", info)
	if !need {
		return false
	}

	e.mu.Lock()
	e.lastAnswer[w.PaneID] = time.Now()
	delete(e.bypassCheckedAt, w.PaneID)
	e.mu.Unlock()

	key := ap.Choices[0].Key
	cursor := 1
	if ap.Style == "cursor" {
		key = ""
	}
	if err := e.sendDialogAnswer(ctx, w.PaneID, key, cursor); err != nil {
		return false
	}
	e.logger.Info("auto-accepted file edit on restored agy pane missing bypass flag", "worker", w.Name, "pane", w.PaneID)
	return true
}

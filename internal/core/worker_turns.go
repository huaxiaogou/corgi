package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

const (
	workerPendingCheckWindow          = 90 * time.Second
	defaultWorkerStartupProbeDelay    = 2 * time.Second
	defaultWorkerStartupProbeInterval = 3 * time.Second
	defaultWorkerStartupCheckWindow   = 25 * time.Second
)

// workerTurnWatermark tracks deterministic causal watermarks for one live Worker instance:
// Queued (latestIssued) -> Delivered (to PTY) -> Observed (in transcript USER_INPUT header),
// the highest assistant byte offset already surfaced to Router to prevent stale replays,
// and post-dispatch startup health checks that catch immediate executor/config crashes.
type workerTurnWatermark struct {
	paneID              string
	session             string
	issuedSeqs          map[int64]bool
	latestIssuedSeq     int64
	latestIssuedID      string
	deliveredSeq        int64
	deliveredID         string
	observedSeq         int64
	observedID          string
	lastNotifiedAt      int64
	lastUserAt          int64
	pendingCheck        bool
	pendingSince        time.Time
	startupCheckActive  bool
	startupBatchIDs     []string
	startupPromptText   string
	startupPreScreen    string
	startupPreUserAt    int64
	startupPreAssistAt  int64
	startupPreReportAt  time.Time
	startupDispatchedAt time.Time
	startupLastProbeAt  time.Time
	startupAlertedSeq   int64
	reportedUserAt      int64
	reportedSeq         int64
	reportedAt          time.Time
	reportedArtID       string
	reportedIsProgress  bool
}

func (e *Engine) getOrResetWorkerTurnLocked(name, paneID, session string) *workerTurnWatermark {
	if e.workerTurns == nil {
		e.workerTurns = map[string]*workerTurnWatermark{}
	}
	wm := e.workerTurns[name]
	paneChanged := wm != nil && paneID != "" && wm.paneID != "" && wm.paneID != paneID
	sessChanged := wm != nil && session != "" && wm.session != "" && wm.session != session
	if wm == nil || paneChanged || sessChanged {
		wm = &workerTurnWatermark{
			paneID:     paneID,
			session:    session,
			issuedSeqs: map[int64]bool{},
		}
		e.workerTurns[name] = wm
		return wm
	}
	if wm.paneID == "" && paneID != "" {
		wm.paneID = paneID
	}
	if wm.session == "" && session != "" {
		wm.session = session
	}
	return wm
}

func (e *Engine) recordWorkerIssued(wk *model.Worker, trigID string, trigSeq int64) {
	if wk == nil || wk.Name == "" || trigSeq <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, wk.PaneID, wk.Session)
	wm.issuedSeqs[trigSeq] = true
	if trigSeq > wm.latestIssuedSeq {
		wm.latestIssuedSeq = trigSeq
		wm.latestIssuedID = trigID
	}
}

func (e *Engine) recordWorkerDelivered(workerName, paneID string, list []*model.Trigger) {
	if workerName == "" || len(list) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(workerName, paneID, "")
	for _, t := range list {
		if t == nil || t.Seq <= 0 {
			continue
		}
		wm.issuedSeqs[t.Seq] = true
		if t.Seq > wm.latestIssuedSeq {
			wm.latestIssuedSeq = t.Seq
			wm.latestIssuedID = t.ID
		}
		if t.Seq > wm.deliveredSeq {
			wm.deliveredSeq = t.Seq
			wm.deliveredID = t.ID
		}
	}
	wm.pendingCheck = false
	wm.pendingSince = time.Time{}
}

func (e *Engine) clearWorkerPendingCheck(workerName string) {
	if workerName == "" {
		return
	}
	e.mu.Lock()
	if wm := e.workerTurns[workerName]; wm != nil {
		wm.pendingCheck = false
		wm.pendingSince = time.Time{}
	}
	e.mu.Unlock()
}

func (e *Engine) clearWorkerTurnState(workerName string) {
	if workerName == "" {
		return
	}
	e.mu.Lock()
	delete(e.workerTurns, workerName)
	e.mu.Unlock()
}

func (e *Engine) hasWorkerPendingCheck(workerName string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.workerTurns[workerName]
	if wm == nil || !wm.pendingCheck {
		return false
	}
	if !wm.pendingSince.IsZero() && now.Sub(wm.pendingSince) > workerPendingCheckWindow {
		wm.pendingCheck = false
		wm.pendingSince = time.Time{}
		return false
	}
	return true
}

// observeWorkerTranscript updates the worker's Observed trigger watermark and handles transcript
// rotation/truncation safely. Only trigger seqs actually issued/delivered to this worker instance
// are accepted as observed watermarks.
func (e *Engine) observeWorkerTranscript(wk *model.Worker, snap workerTurnSnapshot) {
	if wk == nil || wk.Name == "" || !snap.FromTranscript {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, wk.PaneID, wk.Session)
	if snap.LastUserAt > 0 {
		if wm.lastUserAt > 0 && snap.LastUserAt < wm.lastUserAt {
			// Transcript rotated or truncated: reset byte-offset baseline cleanly.
			wm.lastNotifiedAt = 0
		}
		wm.lastUserAt = snap.LastUserAt
	}
	if snap.LastUserSeq > 0 {
		known := wm.issuedSeqs[snap.LastUserSeq] ||
			(wm.deliveredSeq > 0 && snap.LastUserSeq <= wm.deliveredSeq) ||
			len(wm.issuedSeqs) == 0
		if known && snap.LastUserSeq > wm.observedSeq {
			wm.observedSeq = snap.LastUserSeq
			wm.observedID = snap.LastUserTID
		}
	}
}

// shouldEmitWorkerTurnSummary gates automatic `handleWorkerTurnDone` notifications for Router-managed
// workers: suppresses stale pre-user replies (retaining pendingCheck so delayed flushes are still
// caught on subsequent idle ticks) and deduplicates already-notified assistant offsets.
func (e *Engine) shouldEmitWorkerTurnSummary(wk *model.Worker, snap workerTurnSnapshot, now time.Time) bool {
	if wk == nil || wk.Name == "" {
		return false
	}
	e.observeWorkerTranscript(wk, snap)
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, wk.PaneID, wk.Session)
	if !snap.FromTranscript {
		wm.pendingCheck = false
		wm.pendingSince = time.Time{}
		return true
	}
	if !snap.AfterLastUser || strings.TrimSpace(snap.Text) == "" || snap.Text == "(no output captured)" {
		if !wm.pendingCheck {
			wm.pendingCheck = true
			wm.pendingSince = now
		}
		return false
	}
	if snap.AssistantAt > 0 && snap.AssistantAt <= wm.lastNotifiedAt {
		wm.pendingCheck = false
		wm.pendingSince = time.Time{}
		return false
	}
	// Suppress duplicate automatic turn-done notifications when the worker already filed an
	// explicit non-progress `corgi report` for this exact turn (e.g. closing prose written
	// seconds after `corgi report`). New turns, progress reports, and newer Router deliveries
	// are never suppressed.
	if !wm.reportedAt.IsZero() && !wm.reportedIsProgress && wm.deliveredSeq <= wm.reportedSeq {
		sameTranscriptTurn := snap.FromTranscript && snap.LastUserAt > 0 && wm.reportedUserAt > 0 && snap.LastUserAt == wm.reportedUserAt
		sameRecentReport := now.Sub(wm.reportedAt) <= 90*time.Second && (snap.LastUserAt == 0 || wm.reportedUserAt == 0 || snap.LastUserAt <= wm.reportedUserAt)
		if sameTranscriptTurn || sameRecentReport {
			if snap.AssistantAt > wm.lastNotifiedAt {
				wm.lastNotifiedAt = snap.AssistantAt
			}
			wm.pendingCheck = false
			wm.pendingSince = time.Time{}
			return false
		}
	}
	if snap.AssistantAt > 0 {
		wm.lastNotifiedAt = snap.AssistantAt
	}
	wm.pendingCheck = false
	wm.pendingSince = time.Time{}
	return true
}

func isProgressReportText(text string) bool {
	trimmed := strings.TrimSpace(text)
	low := strings.ToLower(trimmed)
	return strings.HasPrefix(low, "progress:") ||
		strings.HasPrefix(trimmed, "进度：") ||
		strings.HasPrefix(trimmed, "进度:") ||
		strings.HasPrefix(trimmed, "进度更新：") ||
		strings.HasPrefix(trimmed, "进度更新:")
}

// recordWorkerExplicitReport records an explicit `corgi report` for the current turn so
// closing assistant prose written seconds later does not emit a duplicate turn-done delivery.
func (e *Engine) recordWorkerExplicitReport(wk *model.Worker, snap workerTurnSnapshot, artID string, isProgress bool, now time.Time) {
	if wk == nil || wk.Name == "" {
		return
	}
	if snap.FromTranscript {
		e.observeWorkerTranscript(wk, snap)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, wk.PaneID, wk.Session)
	if snap.FromTranscript && snap.AfterLastUser && snap.AssistantAt > wm.lastNotifiedAt {
		wm.lastNotifiedAt = snap.AssistantAt
	}
	wm.reportedAt = now
	wm.reportedArtID = strings.TrimSpace(artID)
	wm.reportedIsProgress = isProgress
	if snap.LastUserAt > 0 {
		wm.reportedUserAt = snap.LastUserAt
	} else if wm.lastUserAt > 0 {
		wm.reportedUserAt = wm.lastUserAt
	}
	seq := wm.deliveredSeq
	if snap.LastUserSeq > seq {
		seq = snap.LastUserSeq
	}
	wm.reportedSeq = seq
	wm.pendingCheck = false
	wm.pendingSince = time.Time{}
}

func (e *Engine) markWorkerAssistantNotified(wk *model.Worker, snap workerTurnSnapshot) {
	if wk == nil || wk.Name == "" || !snap.FromTranscript {
		return
	}
	e.observeWorkerTranscript(wk, snap)
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, wk.PaneID, wk.Session)
	if snap.AfterLastUser && snap.AssistantAt > wm.lastNotifiedAt {
		wm.lastNotifiedAt = snap.AssistantAt
	}
	wm.pendingCheck = false
	wm.pendingSince = time.Time{}
}

// formatWorkerTurnStatusNote emits a concise, strictly factual causal watermark line ONLY when
// newer Router instructions for wk remain queued in Hub or delivered-but-not-yet-observed in wk's
// transcript. Returns "" in the normal caught-up case.
func (e *Engine) formatWorkerTurnStatusNote(ctx context.Context, wk *model.Worker, snap workerTurnSnapshot) string {
	if wk == nil || wk.Name == "" {
		return ""
	}
	e.observeWorkerTranscript(wk, snap)

	var queuedIDs []string
	var maxQueuedSeq int64
	if e.store != nil {
		if queued, err := e.store.ListQueuedTriggers(ctx); err == nil {
			for _, t := range queued {
				if t != nil && slices.Contains(t.Targets, wk.Name) {
					queuedIDs = append(queuedIDs, "#"+t.ID)
					if t.Seq > maxQueuedSeq {
						maxQueuedSeq = t.Seq
					}
				}
			}
		}
	}

	e.mu.Lock()
	wm := e.workerTurns[wk.Name]
	var obsID, delID string
	var obsSeq, delSeq int64
	if wm != nil {
		obsID, obsSeq = wm.observedID, wm.observedSeq
		delID, delSeq = wm.deliveredID, wm.deliveredSeq
	}
	e.mu.Unlock()

	var parts []string
	if len(queuedIDs) > 0 {
		if obsID != "" {
			parts = append(parts, fmt.Sprintf("已观察到 Worker 读入 #%s", obsID))
		}
		parts = append(parts, fmt.Sprintf("更新补充 %s 正在 Hub 队列待派发（Worker 空闲后将自动合批送入）", strings.Join(queuedIDs, "、")))
	} else if snap.FromTranscript && delSeq > 0 && obsSeq > 0 && obsSeq < delSeq && delID != "" {
		parts = append(parts, fmt.Sprintf("已观察到 Worker 读入 #%s", obsID))
		parts = append(parts, fmt.Sprintf("更新补充 #%s 已投递但尚未在日志中观测到读入，本报告归属未确认", delID))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n[turn_status: " + strings.Join(parts, "；") + "]"
}

func isWorkerFaulted(w *model.Worker) bool {
	if w == nil || w.Ended || strings.TrimSpace(w.FaultError) == "" {
		return false
	}
	return w.FaultPaneID == "" || w.PaneID == "" || w.FaultPaneID == w.PaneID
}

func (e *Engine) startupCheckTimings() (probeDelay, probeInterval, checkWindow time.Duration) {
	e.mu.Lock()
	probeDelay = e.workerStartupProbeDelay
	probeInterval = e.workerStartupProbeInterval
	checkWindow = e.workerStartupCheckWindow
	e.mu.Unlock()
	if probeDelay <= 0 {
		probeDelay = defaultWorkerStartupProbeDelay
	}
	if probeInterval <= 0 {
		probeInterval = defaultWorkerStartupProbeInterval
	}
	if checkWindow <= 0 {
		checkWindow = defaultWorkerStartupCheckWindow
	}
	return probeDelay, probeInterval, checkWindow
}

// armWorkerStartupCheck arms post-dispatch observation for a freshly delivered Worker batch
// so immediate executor crashes (e.g. `plan model not specified` returning straight to idle)
// or silent non-starts are caught within seconds instead of sitting idle forever.
func (e *Engine) armWorkerStartupCheck(wk *model.Worker, paneID string, list []*model.Trigger, promptText, preScreen string, preSnap workerTurnSnapshot, now time.Time) {
	if wk == nil || wk.Name == "" || len(list) == 0 {
		return
	}
	batchIDs := make([]string, 0, len(list))
	for _, t := range list {
		if t != nil && t.ID != "" {
			batchIDs = append(batchIDs, "#"+t.ID)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.getOrResetWorkerTurnLocked(wk.Name, orStr(paneID, wk.PaneID), wk.Session)
	wm.startupCheckActive = true
	wm.startupBatchIDs = batchIDs
	wm.startupPromptText = promptText
	wm.startupPreScreen = preScreen
	wm.startupPreUserAt = preSnap.LastUserAt
	wm.startupPreAssistAt = preSnap.AssistantAt
	wm.startupPreReportAt = wk.ReportAt
	wm.startupDispatchedAt = now
	wm.startupLastProbeAt = time.Time{}
}

func (e *Engine) disarmWorkerStartupCheck(workerName string) {
	if workerName == "" {
		return
	}
	e.mu.Lock()
	if wm := e.workerTurns[workerName]; wm != nil {
		wm.startupCheckActive = false
		wm.startupPromptText = ""
		wm.startupPreScreen = ""
	}
	e.mu.Unlock()
}

func (e *Engine) hasWorkerStartupCheck(workerName string) bool {
	if workerName == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	wm := e.workerTurns[workerName]
	return wm != nil && wm.startupCheckActive
}

func (e *Engine) markWorkerFaulted(ctx context.Context, w *model.Worker, paneID, errMsg string, initialStartup bool) {
	if w == nil || strings.TrimSpace(errMsg) == "" {
		return
	}
	now := time.Now()
	w.Status = "failed"
	w.FaultError = strings.TrimSpace(errMsg)
	w.FaultPaneID = orStr(paneID, w.PaneID)
	w.FaultAt = &now
	if initialStartup && !w.HadTurn && w.Session == "" && strings.TrimSpace(w.LastReport) == "" && w.ReportAt.IsZero() {
		w.StartupFailed = true
	}
	_ = e.store.UpsertWorker(ctx, w)
}

// checkWorkerStartupHealth inspects a recently-dispatched Worker that has remained idle/done
// without entering `working`. It verifies transcript/report progress first, then diffs post-dispatch
// terminal lines to surface fatal executor/provider errors immediately and pause faulty instances.
func (e *Engine) checkWorkerStartupHealth(ctx context.Context, w *model.Worker, li liveInfo, now time.Time) {
	if w == nil || w.Name == "" {
		return
	}
	probeDelay, probeInterval, checkWindow := e.startupCheckTimings()

	e.mu.Lock()
	wm := e.workerTurns[w.Name]
	if wm == nil || !wm.startupCheckActive {
		e.mu.Unlock()
		return
	}
	paneID := orStr(li.pane, w.PaneID)
	if wm.paneID != "" && paneID != "" && wm.paneID != paneID {
		wm.startupCheckActive = false
		e.mu.Unlock()
		return
	}
	deliveredSeq := wm.deliveredSeq
	deliveredID := wm.deliveredID
	batchIDs := append([]string(nil), wm.startupBatchIDs...)
	promptText := wm.startupPromptText
	preScreen := wm.startupPreScreen
	preUserAt := wm.startupPreUserAt
	preAssistAt := wm.startupPreAssistAt
	preReportAt := wm.startupPreReportAt
	dispatchedAt := wm.startupDispatchedAt
	lastProbeAt := wm.startupLastProbeAt
	alertedSeq := wm.startupAlertedSeq
	e.mu.Unlock()

	if alertedSeq >= deliveredSeq && deliveredSeq > 0 {
		e.disarmWorkerStartupCheck(w.Name)
		return
	}

	// 1. Explicit worker report proves execution started.
	if !w.ReportAt.IsZero() && w.ReportAt.After(preReportAt) {
		e.disarmWorkerStartupCheck(w.Name)
		return
	}

	// 2. Transcript progress proves the agent ingested or answered our batch.
	snap := e.lastWorkerTurnSnapshot(ctx, w)
	if snap.FromTranscript && (snap.LastUserAt > preUserAt || snap.AssistantAt > preAssistAt || (deliveredSeq > 0 && snap.LastUserSeq >= deliveredSeq)) {
		e.disarmWorkerStartupCheck(w.Name)
		if snap.AfterLastUser && snap.AssistantAt > preAssistAt {
			e.handleWorkerTurnDone(ctx, w)
		}
		return
	}

	elapsed := now.Sub(dispatchedAt)
	timedOut := elapsed >= checkWindow
	if !timedOut {
		if elapsed < probeDelay {
			return
		}
		if !lastProbeAt.IsZero() && now.Sub(lastProbeAt) < probeInterval {
			return
		}
	}

	e.mu.Lock()
	if cur := e.workerTurns[w.Name]; cur != nil && cur.startupCheckActive {
		cur.startupLastProbeAt = now
	}
	e.mu.Unlock()

	// 3. Diff post-dispatch terminal output for newly printed fatal executor / provider errors.
	marker := ""
	if deliveredID != "" {
		marker = "#" + deliveredID
	}
	var screenTail string
	if e.fleet != nil && paneID != "" {
		if rr, err := e.fleet.Read(ctx, paneID, "recent", "text", 40); err == nil {
			delta := agent.ExtractPostDispatchDelta(preScreen, rr.Text, marker, promptText)
			screenTail = compactScreenTail(rr.Text, 12, 600)
			if errMsg := agent.ExtractQuotaError(delta, ""); errMsg != "" {
				e.mu.Lock()
				if cur := e.workerTurns[w.Name]; cur != nil {
					if cur.startupAlertedSeq >= deliveredSeq && deliveredSeq > 0 {
						cur.startupCheckActive = false
						e.mu.Unlock()
						return
					}
					cur.startupAlertedSeq = deliveredSeq
					cur.startupCheckActive = false
					cur.startupPromptText = ""
					cur.startupPreScreen = ""
				}
				e.mu.Unlock()

				e.clearWorkerPendingCheck(w.Name)
				if dt := e.popDirectTurn(w.Name); dt != nil && dt.callback != nil {
					res := DirectTurnResult{
						WorkerName:   w.Name,
						Machine:      workerMachineID(w),
						MachineLabel: e.MachineDisplayName(workerMachineID(w)),
						Kind:         w.Kind,
						QuotaError:   errMsg,
					}
					go dt.callback(context.WithoutCancel(ctx), res)
				}
				e.markWorkerFaulted(ctx, w, paneID, errMsg, preUserAt == 0 && preAssistAt == 0 && preReportAt.IsZero())
				batchDesc := strings.Join(batchIDs, ", ")
				wiTag := ""
				if w.WorkItemID != "" {
					wiTag = fmt.Sprintf(" [work_item: %s]", w.WorkItemID)
				}
				idemKey := fmt.Sprintf("worker-startup-fault:%s:%s:%d", w.Name, paneID, deliveredSeq)
				var msg string
				if e.isAPIRouterMode() {
					msg = fmt.Sprintf("worker %s failed immediately after dispatch (%s)%s: %s\nThis worker instance is now PAUSED from accepting new sends. Call manage_work(action=\"stop_worker\", target=%q) and dispatch to a healthy worker/model/computer, then inform the human in one line.",
						w.Name, orStr(batchDesc, marker), wiTag, errMsg, w.Name)
				} else {
					msg = fmt.Sprintf("worker %s failed immediately after dispatch (%s)%s: %s\nThis worker instance is now PAUSED from accepting new sends. Run `%s stop %s` and dispatch to a healthy worker/model/computer, then inform the human in one line.",
						w.Name, orStr(batchDesc, marker), wiTag, errMsg, orStr(e.binPath, "corgi"), w.Name)
				}
				_, _ = e.IngestWithMeta(ctx, "worker", w.Thread, w.Name, msg, idemKey, model.RouterName, w.Name, TriggerMeta{WorkItemID: w.WorkItemID})
				e.addEvent(ctx, w.Thread, w.Name+" 启动失败（已暂停接活）："+oneLineClip(errMsg, 100), w.Name, "error")
				e.notifyChanged()
				return
			}
		}
	}

	// 4. Deadline expired with neither execution proof nor fatal screen error: notify once honestly.
	if !timedOut {
		return
	}
	e.mu.Lock()
	if cur := e.workerTurns[w.Name]; cur != nil {
		if cur.startupAlertedSeq >= deliveredSeq && deliveredSeq > 0 {
			cur.startupCheckActive = false
			e.mu.Unlock()
			return
		}
		cur.startupAlertedSeq = deliveredSeq
		cur.startupCheckActive = false
		cur.startupPromptText = ""
		cur.startupPreScreen = ""
	}
	e.mu.Unlock()

	batchDesc := strings.Join(batchIDs, ", ")
	wiTag := ""
	if w.WorkItemID != "" {
		wiTag = fmt.Sprintf(" [work_item: %s]", w.WorkItemID)
	}
	idemKey := fmt.Sprintf("worker-startup-unconfirmed:%s:%s:%d", w.Name, paneID, deliveredSeq)
	var msg string
	if e.isAPIRouterMode() {
		msg = fmt.Sprintf("worker %s has not confirmed starting batch (%s)%s after %ds (pane remains %s). Inspect with read_detail(kind=\"worker\", target=%q) or call manage_work(action=\"stop_worker\", target=%q) before deciding next steps; avoid blindly resending to the same pane.",
			w.Name, orStr(batchDesc, marker), wiTag, int(checkWindow.Seconds()), orStr(li.status, "idle"), w.Name, w.Name)
	} else {
		msg = fmt.Sprintf("worker %s has not confirmed starting batch (%s)%s after %ds (pane remains %s). Inspect with `%s show %s` or stop and re-dispatch; avoid blindly resending to the same pane.",
			w.Name, orStr(batchDesc, marker), wiTag, int(checkWindow.Seconds()), orStr(li.status, "idle"), orStr(e.binPath, "corgi"), w.Name)
	}
	if screenTail != "" {
		msg += "\nRecent pane output:\n" + screenTail
	}
	_, _ = e.IngestWithMeta(ctx, "worker", w.Thread, w.Name, msg, idemKey, model.RouterName, w.Name, TriggerMeta{WorkItemID: w.WorkItemID})
	e.addEvent(ctx, w.Thread, w.Name+" 派发后未确认启动，已通知大脑核查", w.Name, "error")
	e.notifyChanged()
}

// compactScreenTail extracts up to maxLines trailing non-empty lines from a terminal screen
// snapshot (bounded to maxRunes) so Router/humans can see real CLI errors on startup failures.
func compactScreenTail(screen string, maxLines, maxRunes int) string {
	raw := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		// Skip pure box-drawing separator bars.
		if strings.Trim(t, "─━═-_= ") == "" {
			continue
		}
		lines = append(lines, t)
	}
	if len(lines) == 0 {
		return ""
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	joined := strings.Join(lines, "\n")
	if maxRunes > 0 {
		r := []rune(joined)
		if len(r) > maxRunes {
			joined = "…\n" + string(r[len(r)-maxRunes:])
		}
	}
	return joined
}

func (e *Engine) rerouteFaultedWorkerTriggers(ctx context.Context, w *model.Worker, list []*model.Trigger) {
	if w == nil || len(list) == 0 {
		return
	}
	var ids []string
	var maxSeq int64
	for _, t := range list {
		if t == nil {
			continue
		}
		ids = append(ids, "#"+t.ID)
		if t.Seq > maxSeq {
			maxSeq = t.Seq
		}
		_ = e.store.AckTriggerTarget(ctx, t.ID, w.Name)
	}
	wiTag := ""
	if w.WorkItemID != "" {
		wiTag = fmt.Sprintf(" [work_item: %s]", w.WorkItemID)
	}
	idemKey := fmt.Sprintf("worker-fault-paused:%s:%s:%d", w.Name, w.FaultPaneID, maxSeq)
	var msg string
	if e.isAPIRouterMode() {
		msg = fmt.Sprintf("worker %s is PAUSED due to prior fatal error (%s); skipped delivering queued batch (%s)%s. Call manage_work(action=\"stop_worker\", target=%q) and re-dispatch to a healthy worker/model.",
			w.Name, oneLineClip(w.FaultError, 160), strings.Join(ids, ", "), wiTag, w.Name)
	} else {
		msg = fmt.Sprintf("worker %s is PAUSED due to prior fatal error (%s); skipped delivering queued batch (%s)%s. Run `%s stop %s` and re-dispatch to a healthy worker/model.",
			w.Name, oneLineClip(w.FaultError, 160), strings.Join(ids, ", "), wiTag, orStr(e.binPath, "corgi"), w.Name)
	}
	_, _ = e.IngestWithMeta(ctx, "worker", w.Thread, w.Name, msg, idemKey, model.RouterName, w.Name, TriggerMeta{WorkItemID: w.WorkItemID})
	e.notifyChanged()
}

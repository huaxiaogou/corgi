package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/gitwt"
	"github.com/kiperjing/corgi/internal/model"
)

const (
	autoVerifyTimeout    = 4 * time.Minute
	autoVerifyMaxBounces = 1
)

type workerAutoVerify struct {
	gen               uint64
	implementTurn     bool
	bounces           int
	minTriggerSeq     int64 // completion triggers with Seq <= minTriggerSeq belong to a prior turn
	inFlight          bool
	inFlightGen       uint64
	inFlightTriggerID string
	inFlightCancel    context.CancelFunc
}

type autoVerifyJob struct {
	triggerID  string
	workerName string
	thread     string
	workItemID string
	paneID     string
	session    string
	baseCommit string
	gen        uint64
	bounces    int
}

// markWorkerImplementDispatched starts a fresh auto-verify generation for an explicit
// mode="implement" dispatch/resume, canceling any prior in-flight verification for workerName.
func (e *Engine) markWorkerImplementDispatched(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	var cancel context.CancelFunc
	e.mu.Lock()
	if e.autoVerify == nil {
		e.autoVerify = map[string]*workerAutoVerify{}
	}
	if e.autoVerifyReleased == nil {
		e.autoVerifyReleased = map[string]bool{}
	}
	st := e.autoVerify[workerName]
	if st == nil {
		st = &workerAutoVerify{}
		e.autoVerify[workerName] = st
	}
	st.gen++
	st.implementTurn = true
	st.bounces = 0
	st.minTriggerSeq = 0
	if st.inFlight {
		cancel = st.inFlightCancel
		if st.inFlightTriggerID != "" {
			e.markAutoVerifyReleasedLocked(st.inFlightTriggerID)
		}
		st.inFlight = false
		st.inFlightCancel = nil
		st.inFlightTriggerID = ""
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		e.wake()
	}
}

func (e *Engine) setWorkerAutoVerifyMinSeq(workerName string, seq int64) {
	if workerName == "" || seq <= 0 {
		return
	}
	e.mu.Lock()
	if st := e.autoVerify[workerName]; st != nil && seq > st.minTriggerSeq {
		st.minTriggerSeq = seq
	}
	e.mu.Unlock()
}

// invalidateWorkerAutoVerify increments the worker's auto-verify generation, cancels any
// in-flight verification, releases any held trigger, and optionally clears implementTurn.
func (e *Engine) invalidateWorkerAutoVerify(workerName string, clearImplement bool) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	var cancel context.CancelFunc
	var releasedAny bool
	e.mu.Lock()
	st := e.autoVerify[workerName]
	if st != nil {
		st.gen++
		if clearImplement {
			st.implementTurn = false
		}
		st.bounces = 0
		st.minTriggerSeq = 0
		if st.inFlight {
			cancel = st.inFlightCancel
			if st.inFlightTriggerID != "" {
				e.markAutoVerifyReleasedLocked(st.inFlightTriggerID)
				releasedAny = true
			}
			st.inFlight = false
			st.inFlightCancel = nil
			st.inFlightTriggerID = ""
		}
	}
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cancel != nil || releasedAny {
		e.wake()
	}
}

// invalidateAllAutoVerify cancels every active API auto-verify job (used when switching api -> pty).
func (e *Engine) invalidateAllAutoVerify() {
	var cancels []context.CancelFunc
	e.mu.Lock()
	for _, st := range e.autoVerify {
		if st == nil {
			continue
		}
		st.gen++
		st.implementTurn = false
		st.bounces = 0
		st.minTriggerSeq = 0
		if st.inFlight {
			if st.inFlightCancel != nil {
				cancels = append(cancels, st.inFlightCancel)
			}
			if st.inFlightTriggerID != "" {
				e.markAutoVerifyReleasedLocked(st.inFlightTriggerID)
			}
			st.inFlight = false
			st.inFlightCancel = nil
			st.inFlightTriggerID = ""
		}
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if len(cancels) > 0 {
		e.wake()
	}
}

// invalidateWorkerAutoVerifyByTarget resolves a pane ID or worker name to invalidate any
// active auto-verify state on external pane input, with zero DB work when no verify state exists.
func (e *Engine) invalidateWorkerAutoVerifyByTarget(ctx context.Context, target, paneID string) {
	e.mu.Lock()
	hasAny := len(e.autoVerify) > 0
	e.mu.Unlock()
	if !hasAny {
		return
	}
	if t := strings.TrimSpace(target); t != "" {
		e.invalidateWorkerAutoVerify(t, true)
	}
	paneID = strings.TrimSpace(paneID)
	if paneID == "" || e.store == nil {
		return
	}
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if w != nil && !w.Ended && w.PaneID == paneID {
				e.invalidateWorkerAutoVerify(w.Name, true)
			}
		}
	}
}

func (e *Engine) markAutoVerifyReleasedLocked(triggerID string) {
	if triggerID == "" {
		return
	}
	if e.autoVerifyReleased == nil {
		e.autoVerifyReleased = map[string]bool{}
	}
	if len(e.autoVerifyReleased) >= 512 {
		clear(e.autoVerifyReleased)
	}
	e.autoVerifyReleased[triggerID] = true
}

func isBlockedReportText(s string) bool {
	u := strings.ToUpper(strings.TrimSpace(s))
	return strings.HasPrefix(u, "BLOCKED:") || strings.HasPrefix(u, "BLOCKED：")
}

// isAutoVerifyCandidateTrigger returns true ONLY for worker completion reports (explicit
// `corgi report` excluding BLOCKED:/progress reports) or idle turn-done summaries.
func isAutoVerifyCandidateTrigger(t *model.Trigger) bool {
	if t == nil || t.Source != "worker" || strings.TrimSpace(t.From) == "" {
		return false
	}
	txt := strings.TrimSpace(t.Text)
	if strings.HasPrefix(txt, "[auto_verify:") {
		return false
	}
	reportPrefix := "report from " + t.From + ":\n"
	if body, ok := strings.CutPrefix(txt, reportPrefix); ok {
		if isBlockedReportText(body) || isProgressReportText(body) {
			return false
		}
		return true
	}
	turnPrefix := "worker " + t.From + " produced a new update"
	if rest, ok := strings.CutPrefix(txt, turnPrefix); ok {
		if idx := strings.Index(rest, "Last message:\n"); idx >= 0 {
			msg := rest[idx+len("Last message:\n"):]
			if isBlockedReportText(msg) {
				return false
			}
		}
		return true
	}
	return false
}

// filterAutoVerifyTriggers intercepts eligible worktree implement worker completion triggers
// right before API Router delivery, launching at most one background VerifyWorker per worker
// while allowing all other triggers (e.g. human chat or unrelated workers) to proceed immediately.
func (e *Engine) filterAutoVerifyTriggers(ctx context.Context, live map[string]liveInfo, list []*model.Trigger) []*model.Trigger {
	if len(list) == 0 || e.store == nil || e.Settings().AskPermissions {
		return list
	}
	out := make([]*model.Trigger, 0, len(list))
	for _, t := range list {
		if !isAutoVerifyCandidateTrigger(t) {
			out = append(out, t)
			continue
		}
		workerName := strings.TrimSpace(t.From)
		e.mu.Lock()
		released := e.autoVerifyReleased[t.ID]
		st := e.autoVerify[workerName]
		inFlight := st != nil && st.inFlight
		implTurn := st != nil && st.implementTurn
		minSeq := int64(0)
		if st != nil {
			minSeq = st.minTriggerSeq
		}
		e.mu.Unlock()

		if released {
			out = append(out, t)
			continue
		}
		if inFlight {
			// Hold all completion triggers for this worker while verification is running.
			continue
		}
		if !implTurn {
			out = append(out, t)
			continue
		}
		if minSeq > 0 && t.Seq > 0 && t.Seq <= minSeq {
			// Stale completion trigger from before the current implement dispatch or bounce.
			out = append(out, t)
			continue
		}
		if e.hasDirectTurn(workerName) {
			out = append(out, t)
			continue
		}

		wk, err := e.store.GetWorker(ctx, workerName)
		if err != nil || wk == nil || wk.Ended || isWorkerFaulted(wk) ||
			strings.TrimSpace(wk.WorktreePath) == "" || strings.TrimSpace(wk.WorktreeBase) == "" {
			out = append(out, t)
			continue
		}
		if li, alive := live[workerName]; alive && li.status == "blocked" {
			out = append(out, t)
			continue
		}
		wiID := orStr(strings.TrimSpace(t.WorkItemID), strings.TrimSpace(wk.WorkItemID))
		if wiID == "" || (strings.TrimSpace(wk.WorkItemID) != "" && wiID != strings.TrimSpace(wk.WorkItemID)) {
			out = append(out, t)
			continue
		}
		wi, wiErr := e.store.GetWorkItem(ctx, wiID)
		if wiErr != nil || wi == nil || wi.ClosedAt != nil || wi.Scope != "implement" {
			out = append(out, t)
			continue
		}

		// Re-check durable queued status to avoid reserving an already-acked trigger from a stale snapshot.
		if fresh, fErr := e.store.GetTrigger(ctx, t.ID); fErr != nil || fresh == nil || fresh.Status != "queued" {
			continue
		}

		// Reserve single in-flight verify slot under e.mu.
		e.mu.Lock()
		if e.autoVerifyReleased[t.ID] {
			e.mu.Unlock()
			out = append(out, t)
			continue
		}
		st = e.autoVerify[workerName]
		if st == nil || !st.implementTurn || (st.minTriggerSeq > 0 && t.Seq > 0 && t.Seq <= st.minTriggerSeq) {
			e.mu.Unlock()
			out = append(out, t)
			continue
		}
		if st.inFlight {
			e.mu.Unlock()
			continue
		}
		rootCtx := e.runCtx
		if rootCtx == nil {
			rootCtx = context.Background()
		}
		vctx, cancel := context.WithTimeout(rootCtx, autoVerifyTimeout)
		st.inFlight = true
		st.inFlightGen = st.gen
		st.inFlightTriggerID = t.ID
		st.inFlightCancel = cancel
		job := autoVerifyJob{
			triggerID:  t.ID,
			workerName: wk.Name,
			thread:     orStr(strings.TrimSpace(t.Thread), orStr(wk.Thread, model.MainThread)),
			workItemID: wi.ID,
			paneID:     wk.PaneID,
			session:    wk.Session,
			baseCommit: strings.TrimSpace(wk.WorktreeBase),
			gen:        st.gen,
			bounces:    st.bounces,
		}
		e.mu.Unlock()

		go e.runAutoVerify(vctx, cancel, job)
	}
	return out
}

func (e *Engine) runAutoVerify(ctx context.Context, cancel context.CancelFunc, job autoVerifyJob) {
	defer cancel()
	bg := context.Background()

	e.mu.Lock()
	verifyFn := e.autoVerifyFn
	e.mu.Unlock()
	if verifyFn == nil {
		verifyFn = e.VerifyWorker
	}

	res, err := verifyFn(ctx, VerifyWorkerRequest{
		Worker:      job.workerName,
		BaseCommit:  job.baseCommit,
		SkipIfClean: true,
	})

	// Check API mode, generation, and live worker identity BEFORE applying any verdict or bounce.
	isValid := func() bool {
		e.mu.Lock()
		apiMode := e.settings.EffectiveRouterMode() == "api"
		st := e.autoVerify[job.workerName]
		ok := apiMode && st != nil && st.inFlight && st.inFlightGen == job.gen && st.gen == job.gen && st.inFlightTriggerID == job.triggerID
		e.mu.Unlock()
		return ok
	}
	finishRelease := func(prefix string, clearImpl bool) {
		if prefix != "" && e.store != nil {
			_ = e.store.PrependTriggerText(bg, job.triggerID, prefix)
		}
		e.mu.Lock()
		if st := e.autoVerify[job.workerName]; st != nil && st.inFlightTriggerID == job.triggerID {
			st.inFlight = false
			st.inFlightCancel = nil
			st.inFlightTriggerID = ""
			if clearImpl && st.gen == job.gen {
				st.implementTurn = false
			}
		}
		e.markAutoVerifyReleasedLocked(job.triggerID)
		e.mu.Unlock()
		e.wake()
	}

	if !isValid() {
		finishRelease("", false)
		return
	}
	if e.hasDirectTurn(job.workerName) {
		finishRelease("", true)
		return
	}
	wk, wErr := e.store.GetWorker(bg, job.workerName)
	if wErr != nil || wk == nil || wk.Ended || isWorkerFaulted(wk) ||
		wk.PaneID != job.paneID || wk.WorkItemID != job.workItemID ||
		(job.session != "" && wk.Session != "" && wk.Session != job.session) {
		finishRelease("", true)
		return
	}

	if err != nil {
		if !isValid() {
			finishRelease("", false)
			return
		}
		finishRelease(fmt.Sprintf("[auto_verify: FAIL · 自动验证执行失败：%s]", oneLineClip(err.Error(), 180)), true)
		return
	}
	if res == nil || !res.BaseChecked || res.Clean {
		finishRelease("", true)
		return
	}
	if len(res.Checks) == 0 {
		finishRelease("[auto_verify: SKIPPED · 未检测到构建/测试命令]", true)
		return
	}
	if res.OK {
		finishRelease(fmt.Sprintf("[auto_verify: PASS · %s]", summarizeCheckNames(res.Checks)), true)
		return
	}

	// Checks failed. Bounce back automatically at most once without waking Router.
	if job.bounces < autoVerifyMaxBounces {
		bounce := formatVerifyBouncePrompt(res.VerifyResult)
		bounceTrig, sErr := e.enqueueAutoVerifyBounce(bg, job, bounce, isValid)
		if sErr != nil {
			if !isValid() {
				finishRelease("", false)
				return
			}
			finishRelease(fmt.Sprintf("[auto_verify: FAIL · %s（自动打回失败：%s）]",
				summarizeFailedChecks(res.VerifyResult), oneLineClip(sErr.Error(), 120)), true)
			return
		}
		// Ack the held trigger BEFORE clearing inFlight so no concurrent tick can re-reserve it.
		_ = e.store.AckTriggerTarget(bg, job.triggerID, model.RouterName)
		e.mu.Lock()
		st := e.autoVerify[job.workerName]
		stillValid := st != nil && st.inFlight && st.gen == job.gen && st.inFlightTriggerID == job.triggerID
		if stillValid {
			st.bounces++
			if bounceTrig != nil && bounceTrig.Seq > st.minTriggerSeq {
				st.minTriggerSeq = bounceTrig.Seq
			}
			st.inFlight = false
			st.inFlightCancel = nil
			st.inFlightTriggerID = ""
		} else if st != nil && st.inFlightTriggerID == job.triggerID {
			st.inFlight = false
			st.inFlightCancel = nil
			st.inFlightTriggerID = ""
		}
		e.markAutoVerifyReleasedLocked(job.triggerID)
		e.mu.Unlock()
		if stillValid {
			e.addEvent(bg, job.thread, fmt.Sprintf("#%s 自动验证未通过，已自动打回修复 (%d/%d)：%s",
				job.workerName, job.bounces+1, autoVerifyMaxBounces, summarizeFailedChecks(res.VerifyResult)), job.workerName, "info")
			e.notifyChanged()
		}
		e.wake()
		return
	}

	// Already bounced once and still failing: escalate to Router with concise failure summary at the top.
	finishRelease(fmt.Sprintf("[auto_verify: FAIL · 已自动打回 %d 次仍未通过 · %s]",
		job.bounces, summarizeFailedChecks(res.VerifyResult)), true)
}

// enqueueAutoVerifyBounce enqueues a repair prompt strictly for the exact live worker instance
// captured in job (never adopting a replacement worker or mutating work-item ownership).
func (e *Engine) enqueueAutoVerifyBounce(ctx context.Context, job autoVerifyJob, bounce string, isValid func() bool) (*model.Trigger, error) {
	if !isValid() || e.hasDirectTurn(job.workerName) {
		return nil, fmt.Errorf("verification invalidated")
	}
	wk, err := e.store.GetWorker(ctx, job.workerName)
	if err != nil || wk == nil || wk.Ended || isWorkerFaulted(wk) {
		return nil, fmt.Errorf("worker %q is no longer active", job.workerName)
	}
	if wk.PaneID != job.paneID || wk.WorkItemID != job.workItemID ||
		(job.session != "" && wk.Session != "" && wk.Session != job.session) {
		return nil, fmt.Errorf("worker %q instance changed", job.workerName)
	}
	if !isValid() {
		return nil, fmt.Errorf("verification invalidated")
	}
	if wk.IdleSince != nil || wk.IdlePatrolCount > 0 || wk.SnoozeUntil != nil {
		resetWorkerIdle(wk)
		_ = e.store.UpsertWorker(ctx, wk)
	}
	trig, err := e.IngestWithMeta(ctx, "router", job.thread, model.RouterName, bounce, "", wk.Name, wk.Name, TriggerMeta{WorkItemID: job.workItemID})
	if err != nil {
		return nil, err
	}
	if trig != nil {
		e.recordWorkerIssued(wk, trig.ID, trig.Seq)
	}
	return trig, nil
}

func summarizeCheckNames(checks []gitwt.VerifyCheck) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.Name != "" {
			names = append(names, c.Name)
		} else if c.Command != "" {
			names = append(names, c.Command)
		}
	}
	if len(names) == 0 {
		return "checks"
	}
	return strings.Join(names, ", ")
}

func summarizeFailedChecks(res gitwt.VerifyResult) string {
	var failed []string
	for _, c := range res.Checks {
		if c.Passed {
			continue
		}
		label := orStr(c.Name, c.Command)
		if out := oneLineClip(c.Output, 160); out != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", label, out))
		} else {
			failed = append(failed, label+" 失败")
		}
	}
	if len(failed) == 0 {
		if res.Note != "" {
			return oneLineClip(res.Note, 180)
		}
		return "构建/测试未通过"
	}
	return oneLineClip(strings.Join(failed, "；"), 300)
}

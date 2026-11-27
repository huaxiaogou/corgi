package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const workerPendingCheckWindow = 90 * time.Second

// workerTurnWatermark tracks deterministic causal watermarks for one live Worker instance:
// Queued (latestIssued) -> Delivered (to PTY) -> Observed (in transcript USER_INPUT header),
// plus the highest assistant byte offset already surfaced to Router to prevent stale replays.
type workerTurnWatermark struct {
	paneID          string
	session         string
	issuedSeqs      map[int64]bool
	latestIssuedSeq int64
	latestIssuedID  string
	deliveredSeq    int64
	deliveredID     string
	observedSeq     int64
	observedID      string
	lastNotifiedAt  int64
	lastUserAt      int64
	pendingCheck    bool
	pendingSince    time.Time
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
	if snap.AssistantAt > 0 {
		wm.lastNotifiedAt = snap.AssistantAt
	}
	wm.pendingCheck = false
	wm.pendingSince = time.Time{}
	return true
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

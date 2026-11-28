package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

// workerGCState encapsulates worker operation reservations, activity timestamps,
// and background hibernation GC state behind its own mutex (never nested with Engine.mu).
type workerGCState struct {
	mu                  sync.Mutex
	startedAt           time.Time
	lastScan            time.Time
	running             bool
	closing             map[string]bool
	closingPane         map[string]string
	workerOps           map[string]int
	workerActivity      map[string]time.Time
	failBackoff         map[string]time.Time
	recentlyClosedPanes map[string]time.Time
	interval            time.Duration
	gracePeriod         time.Duration
	activityCooldown    time.Duration
}

func newWorkerGCState(startedAt time.Time) workerGCState {
	return workerGCState{
		startedAt:           startedAt,
		closing:             map[string]bool{},
		closingPane:         map[string]string{},
		workerOps:           map[string]int{},
		workerActivity:      map[string]time.Time{},
		failBackoff:         map[string]time.Time{},
		recentlyClosedPanes: map[string]time.Time{},
	}
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
	e.gc.mu.Lock()
	d := e.gc.interval
	e.gc.mu.Unlock()
	if d > 0 {
		return d
	}
	return defaultWorkerGCInterval
}

func (e *Engine) getWorkerGCGracePeriod() time.Duration {
	e.gc.mu.Lock()
	d := e.gc.gracePeriod
	e.gc.mu.Unlock()
	if d != 0 {
		if d < 0 {
			return 0
		}
		return d
	}
	return defaultWorkerGCGracePeriod
}

func (e *Engine) getWorkerGCActivityCooldown() time.Duration {
	e.gc.mu.Lock()
	d := e.gc.activityCooldown
	e.gc.mu.Unlock()
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
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	if e.gc.closing[workerName] {
		return false
	}
	if e.gc.workerOps == nil {
		e.gc.workerOps = map[string]int{}
	}
	if e.gc.workerActivity == nil {
		e.gc.workerActivity = map[string]time.Time{}
	}
	e.gc.workerOps[workerName]++
	e.gc.workerActivity[workerName] = time.Now()
	return true
}

// endWorkerOp releases a previously acquired beginWorkerOp reservation and refreshes activity.
func (e *Engine) endWorkerOp(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	if n := e.gc.workerOps[workerName]; n <= 1 {
		delete(e.gc.workerOps, workerName)
	} else {
		e.gc.workerOps[workerName] = n - 1
	}
	if e.gc.workerActivity == nil {
		e.gc.workerActivity = map[string]time.Time{}
	}
	e.gc.workerActivity[workerName] = time.Now()
}

// beginWorkerOpWithoutActivity registers an internal read/sync guard (such as syncWorkers)
// that blocks concurrent GC claiming without artificially resetting the worker's idle clock.
func (e *Engine) beginWorkerOpWithoutActivity(workerName string) bool {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return true
	}
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	if e.gc.closing[workerName] {
		return false
	}
	if e.gc.workerOps == nil {
		e.gc.workerOps = map[string]int{}
	}
	e.gc.workerOps[workerName]++
	return true
}

func (e *Engine) endWorkerOpWithoutActivity(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	if n := e.gc.workerOps[workerName]; n <= 1 {
		delete(e.gc.workerOps, workerName)
	} else {
		e.gc.workerOps[workerName] = n - 1
	}
}

func (e *Engine) touchWorkerActivity(workerName string) {
	workerName = strings.TrimSpace(workerName)
	if workerName == "" {
		return
	}
	e.gc.mu.Lock()
	if e.gc.workerActivity == nil {
		e.gc.workerActivity = map[string]time.Time{}
	}
	e.gc.workerActivity[workerName] = time.Now()
	e.gc.mu.Unlock()
}

func (e *Engine) lastWorkerActivity(workerName string) time.Time {
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	return e.gc.workerActivity[workerName]
}

func (e *Engine) isWorkerGCClosing(workerName string) bool {
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	return e.gc.closing[workerName]
}

func (e *Engine) isPaneGCClosing(paneID string) bool {
	if paneID == "" {
		return false
	}
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	return e.gc.closingPane[paneID] != ""
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
	e.gc.mu.Lock()
	if e.gc.recentlyClosedPanes == nil {
		e.gc.recentlyClosedPanes = map[string]time.Time{}
	}
	for pid, at := range e.gc.recentlyClosedPanes {
		if now.Sub(at) > defaultWorkerGCRecentCloseWindow {
			delete(e.gc.recentlyClosedPanes, pid)
		}
	}
	e.gc.recentlyClosedPanes[paneID] = now
	e.gc.mu.Unlock()
}

func (e *Engine) isRecentlyClosedPane(paneID string, now time.Time) bool {
	if paneID == "" {
		return false
	}
	e.gc.mu.Lock()
	defer e.gc.mu.Unlock()
	at, ok := e.gc.recentlyClosedPanes[paneID]
	if !ok {
		return false
	}
	if now.Sub(at) > defaultWorkerGCRecentCloseWindow {
		delete(e.gc.recentlyClosedPanes, paneID)
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

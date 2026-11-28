package core

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

var (
	uuidRe      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	trustHintRe = regexp.MustCompile(`(?i)(trust (this|the contents)|do you trust|hooks? need review|信任此目录|信任该目录)`)
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

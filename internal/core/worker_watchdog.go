package core

import (
	"context"
	"fmt"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	watchdogMinWorkingAge  = 5 * time.Minute
	watchdogSampleInterval = 60 * time.Second
	watchdogLongRunAge     = 20 * time.Minute
	watchdogTrailingFails  = 4
	watchdogWindowFails    = 4
)

type watchdogMark struct {
	pane            string
	workingSince    time.Time
	lastSampleAt    time.Time
	prevHit         bool
	prevFailAt      int64
	alertedUserAt   int64
	alertedRunSince time.Time
}

type watchdogDecision struct {
	alert   bool
	reason  string
	message string
}

// evalThrashSample applies deterministic two-sample confirmation so transient errors or
// repeated reads of the same historical failures never trigger false alerts.
func evalThrashSample(workerName string, mk *watchdogMark, stats *model.ToolStats, now time.Time) watchdogDecision {
	if mk == nil || stats == nil {
		return watchdogDecision{}
	}
	if mk.alreadyAlerted(stats.LastUserAt) {
		return watchdogDecision{}
	}
	hit := stats.TrailingFailed >= watchdogTrailingFails || (stats.N >= watchdogWindowFails && stats.Failed >= watchdogWindowFails)
	if !hit {
		mk.prevHit = false
		mk.prevFailAt = 0
		return watchdogDecision{}
	}
	// Require two consecutive samples to hit AND a strictly advancing failure offset
	// so re-reading the same stalled error burst cannot trigger twice.
	if mk.prevHit && stats.LastFailureAt > mk.prevFailAt {
		mins := int(now.Sub(mk.workingSince).Minutes())
		if mins < 1 {
			mins = 1
		}
		var detail string
		if stats.TrailingFailed >= watchdogTrailingFails {
			detail = fmt.Sprintf("末尾连续 %d 次工具失败（最近 %d 次结果共失败 %d 次）", stats.TrailingFailed, stats.N, stats.Failed)
		} else {
			detail = fmt.Sprintf("最近 %d 次工具结果共失败 %d 次（疑似修改-失败反复空转）", stats.N, stats.Failed)
		}
		return watchdogDecision{
			alert:  true,
			reason: "thrash",
			message: fmt.Sprintf(
				"watchdog: worker %s has been working for %d minutes with %s, and new failures appeared since the last check. Inspect it, send steering, switch approach, or ask the human.",
				workerName, mins, detail,
			),
		}
	}
	mk.prevHit = true
	mk.prevFailAt = stats.LastFailureAt
	return watchdogDecision{}
}

func (mk *watchdogMark) alreadyAlerted(lastUserAt int64) bool {
	if mk == nil {
		return false
	}
	if lastUserAt > 0 && mk.alertedUserAt == lastUserAt {
		return true
	}
	return !mk.alertedRunSince.IsZero() && mk.alertedRunSince.Equal(mk.workingSince)
}

func (mk *watchdogMark) markAlerted(lastUserAt int64) {
	if mk == nil {
		return
	}
	if lastUserAt > 0 {
		mk.alertedUserAt = lastUserAt
	}
	mk.alertedRunSince = mk.workingSince
}

// checkWorkerThrash runs alongside checkStalls inside Engine.tick. It incurs zero transcript/RPC
// overhead until a Worker has been continuously working for >= 5 minutes, samples at most one
// due Worker per tick (every 60s per Worker), and skips Workers in active WeCom direct chats.
func (e *Engine) checkWorkerThrash(ctx context.Context, live map[string]liveInfo, now time.Time) {
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}

	var candidate *model.Worker
	var candDueAt time.Time
	var longRunWorker *model.Worker
	var longRunMins int

	e.mu.Lock()
	if e.watchdogMarks == nil {
		e.watchdogMarks = map[string]*watchdogMark{}
	}
	for _, w := range workers {
		if w == nil || w.Ended {
			delete(e.watchdogMarks, w.Name)
			continue
		}
		li, ok := live[w.Name]
		if !ok || li.status != "working" {
			delete(e.watchdogMarks, w.Name)
			continue
		}
		mk := e.watchdogMarks[w.Name]
		if mk == nil || mk.pane != li.pane {
			mk = &watchdogMark{pane: li.pane, workingSince: now}
			e.watchdogMarks[w.Name] = mk
		}
		// Never interrupt an active direct WeCom Q&A turn.
		if len(e.directTurns[w.Name]) > 0 {
			continue
		}
		if mk.alreadyAlerted(0) {
			continue
		}
		// 1. Check 20-minute neutral long-run reminder for Router-spawned workers without recent report.
		if w.CreatedBy == model.RouterName {
			since := mk.workingSince
			if w.ReportAt.After(since) {
				since = w.ReportAt
			}
			if now.Sub(since) >= watchdogLongRunAge && longRunWorker == nil {
				longRunWorker = w
				longRunMins = int(now.Sub(since).Minutes())
			}
		}
		// 2. Pick at most one due worker (working >= 5m and lastSampleAt >= 60s ago) per tick.
		if now.Sub(mk.workingSince) >= watchdogMinWorkingAge {
			if mk.lastSampleAt.IsZero() || now.Sub(mk.lastSampleAt) >= watchdogSampleInterval {
				if candidate == nil || mk.lastSampleAt.Before(candDueAt) {
					candidate = w
					candDueAt = mk.lastSampleAt
				}
			}
		}
	}
	e.mu.Unlock()

	if candidate != nil {
		stats, ok := e.workerTurnToolStats(ctx, candidate)
		if e.hasDirectTurn(candidate.Name) {
			return
		}
		var dec watchdogDecision
		var userAt int64
		e.mu.Lock()
		if mk := e.watchdogMarks[candidate.Name]; mk != nil {
			mk.lastSampleAt = now
			if ok && stats != nil {
				userAt = stats.LastUserAt
				dec = evalThrashSample(candidate.Name, mk, stats, now)
			}
		}
		e.mu.Unlock()
		if dec.alert {
			if _, err := e.Ingest(ctx, "worker", candidate.Thread, candidate.Name, dec.message, "", model.RouterName, candidate.Name); err == nil {
				e.mu.Lock()
				if mk := e.watchdogMarks[candidate.Name]; mk != nil {
					mk.markAlerted(userAt)
				}
				e.mu.Unlock()
				e.addEvent(ctx, candidate.Thread, candidate.Name+" 疑似连续报错空转，已提醒 Corgi 检查", candidate.Name, "info")
			}
			return
		}
	}

	if longRunWorker != nil && !e.hasDirectTurn(longRunWorker.Name) {
		msg := fmt.Sprintf(
			"watchdog: worker %s has been running continuously for %d minutes without a progress report. Check whether it is making progress or needs steering.",
			longRunWorker.Name, longRunMins,
		)
		if _, err := e.Ingest(ctx, "worker", longRunWorker.Thread, longRunWorker.Name, msg, "", model.RouterName, longRunWorker.Name); err == nil {
			e.mu.Lock()
			if mk := e.watchdogMarks[longRunWorker.Name]; mk != nil {
				mk.markAlerted(0)
			}
			e.mu.Unlock()
			e.addEvent(ctx, longRunWorker.Thread, fmt.Sprintf("%s 已连续运行 %d 分钟未汇报，已提醒 Corgi 巡检", longRunWorker.Name, longRunMins), longRunWorker.Name, "info")
		}
	}
}

package core

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const routerLoopFallbackPoll = 10 * time.Second

func (e *Engine) wakeRouter() {
	if e == nil || e.routerWakeCh == nil {
		return
	}
	select {
	case e.routerWakeCh <- struct{}{}:
	default:
	}
}

// runAPIRouterLoop drives the in-process native API Brain independently from tick()'s
// Fleet/Worker polling loop so inbound human messages and worker reports reach the LLM
// in milliseconds without waiting on remote node RPC scans.
func (e *Engine) runAPIRouterLoop(ctx context.Context) {
	fallback := time.NewTicker(routerLoopFallbackPoll)
	defer fallback.Stop()

	e.stepAPIRouter(ctx)
	for {
		var (
			backoffTimer *time.Timer
			backoffC     <-chan time.Time
		)
		e.mu.Lock()
		until := e.router.backoffUntil
		paused := e.router.authPaused
		e.mu.Unlock()
		if !paused && !until.IsZero() {
			wait := time.Until(until)
			if wait <= 0 {
				wait = 50 * time.Millisecond
			}
			backoffTimer = time.NewTimer(wait)
			backoffC = backoffTimer.C
		}

		select {
		case <-ctx.Done():
			if backoffTimer != nil {
				backoffTimer.Stop()
			}
			return
		case <-e.routerWakeCh:
			if backoffTimer != nil {
				backoffTimer.Stop()
			}
			e.stepAPIRouter(ctx)
		case <-backoffC:
			e.stepAPIRouter(ctx)
		case <-fallback.C:
			if backoffTimer != nil {
				backoffTimer.Stop()
			}
			e.stepAPIRouter(ctx)
		}
	}
}

func (e *Engine) stepAPIRouter(parent context.Context) {
	if e.store == nil || e.Settings().Paused || e.isHubUpdating() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	e.drainRouterPrompts(ctx)
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil || len(queued) == 0 {
		return
	}
	var routerList []*model.Trigger
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
			_ = e.store.ReplaceTriggerTargets(ctx, t.ID, t.Targets)
		}
		if slices.Contains(t.Targets, model.RouterName) {
			routerList = append(routerList, t)
		}
	}
	if len(routerList) > 0 {
		e.deliverAPIRouter(ctx, routerList)
	}
}

func isHumanPriorityTrigger(t *model.Trigger) bool {
	if t == nil {
		return false
	}
	if (t.Source != "chat" && t.Source != "wecom") || t.Policy != "respond" {
		return false
	}
	fromLow := strings.ToLower(strings.TrimSpace(t.From))
	return fromLow != "machine" && fromLow != "router" && fromLow != model.RouterName
}

// selectNextAPIRouterTrigger prioritizes live human messages strictly ahead of queued
// background events (idle patrol, worker progress, timers, webhooks), preserving FIFO
// by Seq within each priority tier.
func selectNextAPIRouterTrigger(list []*model.Trigger) *model.Trigger {
	if len(list) == 0 {
		return nil
	}
	best := list[0]
	bestHuman := isHumanPriorityTrigger(best)
	for _, cand := range list[1:] {
		if cand == nil {
			continue
		}
		candHuman := isHumanPriorityTrigger(cand)
		if candHuman && !bestHuman {
			best, bestHuman = cand, true
			continue
		}
		if candHuman == bestHuman && (best == nil || cand.Seq < best.Seq) {
			best = cand
		}
	}
	return best
}

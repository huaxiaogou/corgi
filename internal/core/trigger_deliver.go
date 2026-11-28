package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func (e *Engine) deliverQueued(ctx context.Context, live map[string]liveInfo, scanned map[string]bool) {
	e.deliverQueuedWithFleet(ctx, live, scanned, true)
}

func (e *Engine) deliverQueuedWithFleet(ctx context.Context, live map[string]liveInfo, scanned map[string]bool, fleetOK bool) {
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
			_ = e.store.ReplaceTriggerTargets(ctx, t.ID, t.Targets)
		}
		for _, target := range t.Targets {
			batches[target] = append(batches[target], t)
		}
	}

	for target, list := range batches {
		if target == model.RouterName {
			e.deliverAPIRouter(ctx, list)
			continue
		}
		if !fleetOK {
			continue
		}
		if e.isDeliveringWorker(target) {
			continue
		}
		li, alive := live[target]
		if !alive {
			e.rerouteDeadWorkerTriggers(ctx, target, list, scanned)
			continue
		}
		if li.status == "blocked" || (!li.ready && li.status == "unknown") || e.isHealingBypassWorker(target, li.pane) {
			continue
		}
		if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && isWorkerFaulted(wk) && (wk.FaultPaneID == "" || wk.FaultPaneID == li.pane) {
			e.rerouteFaultedWorkerTriggers(ctx, wk, list)
			continue
		}
		if li.status == "working" {
			list = filterWorkerSteeringTriggers(list)
			if len(list) == 0 {
				continue
			}
		}
		if e.triggersHaveHumanMedia(ctx, list) {
			if e.tryBeginWorkerDelivery(target) {
				batchCopy := append([]*model.Trigger(nil), list...)
				if rootCtx := e.backgroundRunCtx(); rootCtx != nil {
					go e.deliverBatchAsync(rootCtx, target, li.pane, batchCopy)
				} else {
					e.deliverBatchAsync(ctx, target, li.pane, batchCopy)
				}
			}
			continue
		}
		e.deliverBatch(ctx, target, li.pane, list)
	}
}

func (e *Engine) isTrustedHumanMediaTrigger(ctx context.Context, t *model.Trigger) bool {
	if t == nil || len(t.Media) == 0 {
		return false
	}
	if (t.Source != "chat" && t.Source != "wecom") || t.Policy != "respond" {
		return false
	}
	fromLow := strings.ToLower(strings.TrimSpace(t.From))
	if fromLow == "machine" || fromLow == "router" || fromLow == model.RouterName {
		return false
	}
	if t.Ref != "" && e.store != nil {
		en, err := e.store.GetEntry(ctx, t.Ref)
		if err != nil || en == nil || (strings.TrimSpace(en.Channel) != "" && !isHumanChannel(en.Channel)) {
			return false
		}
	}
	return true
}

func (e *Engine) triggersHaveHumanMedia(ctx context.Context, list []*model.Trigger) bool {
	for _, t := range list {
		if e.isTrustedHumanMediaTrigger(ctx, t) {
			return true
		}
	}
	return false
}

func (e *Engine) isDeliveringWorker(target string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deliveringWorkers[target]
}

func (e *Engine) tryBeginWorkerDelivery(target string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.deliveringWorkers == nil {
		e.deliveringWorkers = map[string]bool{}
	}
	if e.deliveringWorkers[target] {
		return false
	}
	e.deliveringWorkers[target] = true
	return true
}

func (e *Engine) endWorkerDelivery(target string) {
	e.mu.Lock()
	delete(e.deliveringWorkers, target)
	e.mu.Unlock()
}

func (e *Engine) backgroundRunCtx() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runCtx
}

func (e *Engine) deliverBatchAsync(parent context.Context, target, paneID string, list []*model.Trigger) {
	defer e.endWorkerDelivery(target)
	dctx, cancel := context.WithTimeout(parent, stageBatchTimeout+20*time.Second)
	defer cancel()
	e.deliverBatch(dctx, target, paneID, list)
}

func filterWorkerSteeringTriggers(list []*model.Trigger) []*model.Trigger {
	// While a Worker is busy, queue routine Router instructions on Hub so multiple rapid
	// follow-ups coalesce cleanly by Seq on the next idle boundary (instead of sitting unseen
	// in a CLI PTY input buffer). Only explicit Steer triggers bypass the busy queue.
	var out []*model.Trigger
	for _, t := range list {
		if t.Steer {
			out = append(out, t)
		}
	}
	return out
}

func (e *Engine) rerouteDeadWorkerTriggers(ctx context.Context, target string, list []*model.Trigger, scanned map[string]bool) {
	if w, err := e.store.GetWorker(ctx, target); err == nil && !w.Ended {
		if time.Since(w.CreatedAt) < 2*time.Minute {
			return // still booting
		}
		if scanned != nil && !scanned[workerMachineID(w)] {
			return // worker's machine was not scanned on this tick; keep trigger targeting this worker
		}
	}
	for _, t := range list {
		_ = e.store.RerouteTriggerTarget(ctx, t.ID, target, model.RouterName)
	}
}

func (e *Engine) deliverBatch(ctx context.Context, target, paneID string, list []*model.Trigger) {
	if !e.beginWorkerOp(target) {
		return
	}
	defer e.endWorkerOp(target)
	// Re-verify queued triggers after acquiring delivery ownership to avoid re-sending a stale tick snapshot.
	if e.store != nil {
		if freshQueued, err := e.store.ListQueuedTriggers(ctx); err == nil {
			wanted := make(map[string]bool, len(list))
			for _, t := range list {
				if t != nil {
					wanted[t.ID] = true
				}
			}
			var liveList []*model.Trigger
			for _, ft := range freshQueued {
				if wanted[ft.ID] && slices.Contains(ft.Targets, target) {
					liveList = append(liveList, ft)
				}
			}
			list = liveList
		}
	}
	if len(list) == 0 {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if len(list) > 20 {
		list = list[:20]
	}

	var targetWorker *model.Worker
	var workerConstraint string
	if wk, wErr := e.store.GetWorker(ctx, target); wErr == nil && wk != nil {
		targetWorker = wk
		workerConstraint = strings.TrimSpace(wk.Constraint)
	}

	budget := newStageBudget(ctx)
	defer budget.close()

	type readyTrigger struct {
		trig *model.Trigger
		body string
	}
	ready := make([]readyTrigger, 0, len(list))
	for _, t := range list {
		body := strings.TrimSpace(t.Text)
		if r := []rune(body); len(r) > 700 { // Inline & Spill
			body = string(r[:700]) + fmt.Sprintf(" … [spilled; run `corgi show %s`]", t.ID)
		}
		if targetWorker != nil && e.isTrustedHumanMediaTrigger(ctx, t) {
			stagedBody, _, sErr := e.stageMediaForDirectTextWithBudget(budget, workerMachineID(targetWorker), body, t.Media)
			if sErr != nil {
				e.logger.Warn("stage trigger media for worker failed", "target", target, "trigger", t.ID, "err", sErr)
				if isPermanentStageError(sErr) {
					e.addEvent(ctx, t.Thread, fmt.Sprintf("发往 #%s 的消息附件无法同步（已跳过）：%v", target, sErr), target, "error")
					_ = e.store.AckTriggerTarget(ctx, t.ID, target)
					e.notifyChanged()
				}
				continue
			}
			body = stagedBody
		}
		ready = append(ready, readyTrigger{trig: t, body: body})
	}
	if len(ready) == 0 {
		return
	}

	var b strings.Builder
	if workerConstraint != "" {
		b.WriteString(FormatConstraintHeader(workerConstraint))
		b.WriteString("\n\n")
	}
	if len(ready) == 1 {
		b.WriteString("[corgi] 1 new event\n")
	} else {
		fmt.Fprintf(&b, "[corgi] %d new events (batched)\n", len(ready))
	}
	delivered := make([]*model.Trigger, 0, len(ready))
	for _, rt := range ready {
		t := rt.trig
		delivered = append(delivered, t)
		fmt.Fprintf(&b, "\n#%s · %s · %s", t.ID, t.Thread, t.Source)
		if t.From != "" {
			fmt.Fprintf(&b, " · from %s", t.From)
		}
		if t.Policy == "candidate" {
			b.WriteString(" · candidate (act only if a rule or memory covers it)")
		}
		fmt.Fprintf(&b, " · %s\n", t.At.Format("15:04"))
		b.WriteString("  " + strings.ReplaceAll(rt.body, "\n", "\n  ") + "\n")
	}

	promptTarget := target
	if paneID != "" {
		promptTarget = paneID
	}
	var preScreen string
	var preSnap workerTurnSnapshot
	if targetWorker != nil {
		if rr, rErr := e.fleet.Read(ctx, promptTarget, "recent", "text", 30); rErr == nil {
			preScreen = rr.Text
		}
		preSnap = e.lastWorkerTurnSnapshot(ctx, targetWorker)
	}
	promptBody := b.String()
	if err := e.fleet.Prompt(ctx, promptTarget, promptBody); err != nil {
		var he *herdr.Error
		if errors.As(err, &he) && (he.Code == "agent_blocked" || he.Code == "agent_not_ready") {
			return
		}
		e.logger.Warn("deliver trigger batch failed", "target", target, "err", err)
		return
	}

	if workerConstraint != "" && paneID != "" {
		e.markConstraintDelivered(paneID)
	}
	e.recordWorkerDelivered(target, paneID, delivered)
	e.resetWorkerIdleByName(ctx, target)
	if targetWorker != nil {
		e.armWorkerStartupCheck(targetWorker, paneID, delivered, promptBody, preScreen, preSnap, time.Now())
	}

	for _, t := range delivered {
		_ = e.store.AckTriggerTarget(ctx, t.ID, target)
	}
	e.notifyChanged()
}

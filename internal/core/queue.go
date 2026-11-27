package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

const routerQueuePrefix = "router:"

// RouterQueueScope returns the durable queue scope key for a main Corgi chat thread.
func RouterQueueScope(thread string) string {
	return routerQueuePrefix + orStr(strings.TrimSpace(thread), model.MainThread)
}

func (e *Engine) wake() {
	if e == nil || e.wakeCh == nil {
		return
	}
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}

func (e *Engine) isQueuePaused(scope string) bool {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	return e.pausedQueues[scope]
}

func (e *Engine) setQueuePaused(scope string, paused bool) {
	e.queueMu.Lock()
	if paused {
		e.pausedQueues[scope] = true
	} else {
		delete(e.pausedQueues, scope)
	}
	e.queueMu.Unlock()
}

func (e *Engine) markDrainCooldown(scope string) {
	e.queueMu.Lock()
	e.drainCooldown[scope] = time.Now()
	e.queueMu.Unlock()
}

func (e *Engine) clearDrainCooldown(scope string) {
	e.queueMu.Lock()
	delete(e.drainCooldown, scope)
	e.queueMu.Unlock()
}

func (e *Engine) inDrainCooldown(scope string, d time.Duration) bool {
	e.queueMu.Lock()
	defer e.queueMu.Unlock()
	t, ok := e.drainCooldown[scope]
	return ok && time.Since(t) < d
}

// resolvePaneTarget resolves a worker name or pane ID into a canonical pane ID and live status.
func (e *Engine) resolvePaneTarget(ctx context.Context, target string) (paneID, status string) {
	paneID = target
	if e.fleet == nil {
		return paneID, ""
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return paneID, ""
	}
	for _, a := range agents {
		if a.PaneID == target || (a.Name != nil && *a.Name == target) {
			return a.PaneID, a.AgentStatus
		}
	}
	return paneID, ""
}

// SubmitPanePrompt sends a prompt immediately when idle, or enqueues it durably when working.
// mode may be ""/"auto", "queue", or "steer".
func (e *Engine) SubmitPanePrompt(ctx context.Context, target, text, mode string) (bool, *model.QueuedPrompt, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return false, nil, fmt.Errorf("prompt is required")
	}
	paneID, status := e.resolvePaneTarget(ctx, target)
	mode = strings.ToLower(strings.TrimSpace(mode))

	if mode == "steer" {
		if e.fleet == nil {
			return false, nil, fmt.Errorf("fleet not configured")
		}
		sendText, hadConstraint := e.decorateDirectPrompt(ctx, paneID, text)
		if err := e.fleet.Prompt(ctx, paneID, sendText); err != nil {
			return false, nil, err
		}
		if hadConstraint {
			e.markConstraintDelivered(paneID)
		}
		e.setQueuePaused(paneID, false)
		e.markDrainCooldown(paneID)
		e.notifyChanged()
		return false, nil, nil
	}

	existing, _ := e.store.ListQueuedPrompts(ctx, paneID)
	shouldQueue := mode == "queue" || len(existing) > 0 || status == "working"
	if shouldQueue {
		id, seq, err := e.store.NextID(ctx, "q")
		if err != nil {
			return false, nil, err
		}
		item := &model.QueuedPrompt{
			ID:        id,
			Seq:       seq,
			Scope:     paneID,
			PaneID:    paneID,
			Text:      text,
			CreatedAt: time.Now(),
		}
		if err := e.store.InsertQueuedPrompt(ctx, item); err != nil {
			return false, nil, err
		}
		e.notifyChanged()
		e.wake()
		return true, item, nil
	}

	if e.fleet == nil {
		return false, nil, fmt.Errorf("fleet not configured")
	}
	sendText, hadConstraint := e.decorateDirectPrompt(ctx, paneID, text)
	if err := e.fleet.Prompt(ctx, paneID, sendText); err != nil {
		return false, nil, err
	}
	if hadConstraint {
		e.markConstraintDelivered(paneID)
	}
	e.setQueuePaused(paneID, false)
	e.markDrainCooldown(paneID)
	e.notifyChanged()
	return false, nil, nil
}

// SteerPanePrompt pops a single queued prompt and immediately soft-steers the pane.
func (e *Engine) SteerPanePrompt(ctx context.Context, target, qid string) (*model.QueuedPrompt, error) {
	paneID, _ := e.resolvePaneTarget(ctx, target)
	item, err := e.store.GetQueuedPrompt(ctx, qid)
	if err != nil {
		return nil, err
	}
	if item.Scope != paneID && item.Scope != target {
		return nil, store.ErrNotFound
	}
	if e.fleet == nil {
		return nil, fmt.Errorf("fleet not configured")
	}
	sendText, hadConstraint := e.decorateDirectPrompt(ctx, paneID, item.Text)
	if err := e.fleet.Prompt(ctx, paneID, sendText); err != nil {
		return nil, err
	}
	if hadConstraint {
		e.markConstraintDelivered(paneID)
	}
	_ = e.store.DeleteQueuedPrompt(ctx, item.ID)
	e.setQueuePaused(item.Scope, false)
	e.markDrainCooldown(item.Scope)
	e.notifyChanged()
	return item, nil
}

// StopPane sends Esc to immediately interrupt the pane's current step, pausing auto-drain if queued items exist.
func (e *Engine) StopPane(ctx context.Context, target string) (bool, error) {
	paneID, _ := e.resolvePaneTarget(ctx, target)
	if e.fleet == nil {
		return false, fmt.Errorf("fleet not configured")
	}
	if err := e.fleet.SendKeys(ctx, paneID, "esc"); err != nil {
		return false, err
	}
	items, _ := e.store.ListQueuedPrompts(ctx, paneID)
	paused := len(items) > 0
	if paused {
		e.setQueuePaused(paneID, true)
	}
	e.notifyChanged()
	return paused, nil
}

// SubmitChatSay submits a human chat message to Corgi Router, queueing it while Router is busy.
// mode may be ""/"auto", "queue", or "steer".
func (e *Engine) SubmitChatSay(ctx context.Context, req SayRequest, mode string) (bool, *model.QueuedPrompt, *model.Entry, *model.Trigger, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return false, nil, nil, nil, fmt.Errorf("消息内容不能为空")
	}
	thread := orStr(strings.TrimSpace(req.Thread), model.MainThread)
	req.Thread = thread
	if req.Channel == "" {
		req.Channel = "web"
	}
	scope := RouterQueueScope(thread)
	mode = strings.ToLower(strings.TrimSpace(mode))
	clientMsgID := strings.TrimSpace(req.ClientMsgID)
	idemKey := normalizeSayIdemKey(req)

	// Deduplicate retries BEFORE deciding queue vs immediate delivery so a client retry
	// after router status flips never produces both a queued item and a direct entry.
	if idemKey != "" {
		if existingTrig, err := e.store.GetTriggerByIdemKey(ctx, idemKey); err == nil && existingTrig != nil {
			var existingEntry *model.Entry
			if existingTrig.Ref != "" {
				existingEntry, _ = e.store.GetEntry(ctx, existingTrig.Ref)
			}
			return false, nil, existingEntry, existingTrig, nil
		}
	}
	if clientMsgID != "" {
		if existingItem, err := e.store.FindQueuedPromptByClientMsgID(ctx, scope, clientMsgID); err == nil && existingItem != nil {
			return true, existingItem, nil, nil, nil
		}
	}

	if mode == "steer" {
		e.setQueuePaused(scope, false)
		entry, trig, err := e.sayWithSteer(ctx, req, true)
		if err == nil {
			e.markDrainCooldown(scope)
			e.wake()
		}
		return false, nil, entry, trig, err
	}

	if req.Channel == "web" {
		existing, _ := e.store.ListQueuedPrompts(ctx, scope)
		routerWorking := e.isRouterWorking(ctx)
		if mode == "queue" || len(existing) > 0 || routerWorking {
			id, seq, err := e.store.NextID(ctx, "q")
			if err != nil {
				return false, nil, nil, nil, err
			}
			item := &model.QueuedPrompt{
				ID:          id,
				Seq:         seq,
				Scope:       scope,
				Thread:      thread,
				Text:        text,
				ReEntryID:   req.ReEntryID,
				Machine:     req.Machine,
				Kind:        req.Kind,
				Model:       req.Model,
				Effort:      req.Effort,
				ProjectID:   req.ProjectID,
				ClientMsgID: clientMsgID,
				CreatedAt:   time.Now(),
			}
			if err := e.store.InsertQueuedPrompt(ctx, item); err != nil {
				return false, nil, nil, nil, err
			}
			e.notifyChanged()
			e.wake()
			return true, item, nil, nil, nil
		}
	}

	e.setQueuePaused(scope, false)
	entry, trig, err := e.sayWithSteer(ctx, req, false)
	if err == nil {
		e.markDrainCooldown(scope)
		e.wake()
	}
	return false, nil, entry, trig, err
}

// SteerChatPrompt pops one queued main-chat message, records it in chat history, and soft-steers Router immediately.
func (e *Engine) SteerChatPrompt(ctx context.Context, thread, qid string) (*model.Entry, *model.Trigger, error) {
	scope := RouterQueueScope(thread)
	item, err := e.store.GetQueuedPrompt(ctx, qid)
	if err != nil {
		return nil, nil, err
	}
	if item.Scope != scope {
		return nil, nil, store.ErrNotFound
	}
	entry, trig, err := e.sayWithSteer(ctx, SayRequest{
		Thread:      item.Thread,
		Channel:     "web",
		From:        "you",
		Text:        item.Text,
		ReEntryID:   item.ReEntryID,
		Machine:     item.Machine,
		Kind:        item.Kind,
		Model:       item.Model,
		Effort:      item.Effort,
		ProjectID:   item.ProjectID,
		ClientMsgID: item.ClientMsgID,
	}, true)
	if err != nil {
		return nil, nil, err
	}
	_ = e.store.DeleteQueuedPrompt(ctx, item.ID)
	e.setQueuePaused(scope, false)
	e.markDrainCooldown(scope)
	e.notifyChanged()
	e.wake()
	return entry, trig, nil
}

// StopRouter sends Esc to Corgi Router's terminal pane and pauses router auto-drain if messages are queued.
func (e *Engine) StopRouter(ctx context.Context, thread string) (bool, error) {
	if e.fleet == nil {
		return false, fmt.Errorf("fleet not configured")
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return false, err
	}
	paneID := ""
	for _, a := range agents {
		if a.Name != nil && *a.Name == model.RouterName {
			paneID = a.PaneID
			break
		}
	}
	if paneID == "" {
		return false, fmt.Errorf("Corgi 会话当前未运行")
	}
	if err := e.fleet.SendKeys(ctx, paneID, "esc"); err != nil {
		return false, err
	}
	e.mu.Lock()
	e.routerWorkingAt = time.Time{}
	e.routerIdleSince = time.Time{}
	e.mu.Unlock()
	scope := RouterQueueScope(thread)
	items, _ := e.store.ListQueuedPrompts(ctx, scope)
	paused := len(items) > 0
	if paused {
		e.setQueuePaused(scope, true)
	}
	e.notifyChanged()
	return paused, nil
}

// CancelQueuedPrompt deletes one queued prompt from either a pane or main-chat scope.
func (e *Engine) CancelQueuedPrompt(ctx context.Context, scope, qid string) error {
	item, err := e.store.GetQueuedPrompt(ctx, qid)
	if err != nil {
		return err
	}
	if scope != "" && item.Scope != scope {
		return store.ErrNotFound
	}
	if err := e.store.DeleteQueuedPrompt(ctx, item.ID); err != nil {
		return err
	}
	if rem, _ := e.store.ListQueuedPrompts(ctx, item.Scope); len(rem) == 0 {
		e.setQueuePaused(item.Scope, false)
	}
	e.notifyChanged()
	return nil
}

// ResumeQueue clears a paused queue and wakes the scheduling loop so the next item drains promptly.
func (e *Engine) ResumeQueue(scope string) {
	e.setQueuePaused(scope, false)
	e.clearDrainCooldown(scope)
	e.notifyChanged()
	e.wake()
}

// PaneQueuesSnapshot returns all queued single-agent pane prompts grouped by pane_id plus paused flags.
func (e *Engine) PaneQueuesSnapshot(ctx context.Context) (map[string][]*model.QueuedPrompt, map[string]bool) {
	all, err := e.store.ListQueuedPrompts(ctx, "")
	if err != nil || len(all) == 0 {
		return nil, nil
	}
	queues := map[string][]*model.QueuedPrompt{}
	for _, p := range all {
		if strings.HasPrefix(p.Scope, routerQueuePrefix) {
			continue
		}
		queues[p.Scope] = append(queues[p.Scope], p)
	}
	if len(queues) == 0 {
		return nil, nil
	}
	var paused map[string]bool
	e.queueMu.Lock()
	for scope := range queues {
		if e.pausedQueues[scope] {
			if paused == nil {
				paused = map[string]bool{}
			}
			paused[scope] = true
		}
	}
	e.queueMu.Unlock()
	return queues, paused
}

// ChatQueueSnapshot returns queued main-chat prompts for one thread plus whether its queue is paused.
func (e *Engine) ChatQueueSnapshot(ctx context.Context, thread string) ([]*model.QueuedPrompt, bool) {
	scope := RouterQueueScope(thread)
	items, err := e.store.ListQueuedPrompts(ctx, scope)
	if err != nil || len(items) == 0 {
		return nil, false
	}
	return items, e.isQueuePaused(scope)
}

func (e *Engine) isRouterWorking(ctx context.Context) bool {
	if e.fleet == nil {
		return false
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.Name != nil && *a.Name == model.RouterName {
			kind := ""
			if a.Agent != nil {
				kind = *a.Agent
			}
			return e.stabilizeRouterStatus(a.PaneID, kind, a.AgentStatus, time.Now()) == "working"
		}
	}
	return false
}

// drainRouterPrompts promotes at most one queued web chat prompt into a real Entry+Trigger
// once Router is idle and has no pending triggers waiting.
func (e *Engine) drainRouterPrompts(ctx context.Context, live map[string]liveInfo) {
	li, alive := live[model.RouterName]
	if alive && li.status == "working" {
		e.queueMu.Lock()
		for k := range e.drainCooldown {
			if strings.HasPrefix(k, routerQueuePrefix) {
				delete(e.drainCooldown, k)
			}
		}
		e.queueMu.Unlock()
		return
	}
	if alive && (li.status == "blocked" || (!li.ready && li.status == "unknown")) {
		return
	}
	e.mu.Lock()
	if e.routerLaunching || e.hubUpdating || e.routerDispatching {
		e.mu.Unlock()
		return
	}
	e.routerDispatching = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.routerDispatching = false
		e.mu.Unlock()
	}()

	if queued, err := e.store.ListQueuedTriggers(ctx); err == nil {
		for _, t := range queued {
			if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
				return // wait until already-queued router triggers finish first
			}
		}
	}

	all, err := e.store.ListQueuedPrompts(ctx, "")
	if err != nil || len(all) == 0 {
		return
	}
	for _, item := range all {
		if !strings.HasPrefix(item.Scope, routerQueuePrefix) {
			continue
		}
		if e.isQueuePaused(item.Scope) || e.inDrainCooldown(item.Scope, 5*time.Second) {
			continue
		}
		if _, _, err := e.sayWithSteer(ctx, SayRequest{
			Thread:      item.Thread,
			Channel:     "web",
			From:        "you",
			Text:        item.Text,
			ReEntryID:   item.ReEntryID,
			Machine:     item.Machine,
			Kind:        item.Kind,
			Model:       item.Model,
			Effort:      item.Effort,
			ProjectID:   item.ProjectID,
			ClientMsgID: item.ClientMsgID,
		}, false); err != nil {
			e.logger.Warn("drain router queued prompt failed", "id", item.ID, "err", err)
			return
		}
		_ = e.store.DeleteQueuedPrompt(ctx, item.ID)
		e.markDrainCooldown(item.Scope)
		e.notifyChanged()
		return // one queued turn at a time
	}
}

// drainPanePrompts pops and delivers at most one queued prompt per idle single-agent pane.
func (e *Engine) drainPanePrompts(ctx context.Context, rawAgents []model.Agent) {
	statusByPane := map[string]string{}
	for _, a := range rawAgents {
		if a.PaneID == "" {
			continue
		}
		statusByPane[a.PaneID] = a.AgentStatus
		if a.AgentStatus == "working" {
			e.clearDrainCooldown(a.PaneID)
		}
	}

	all, err := e.store.ListQueuedPrompts(ctx, "")
	if err != nil || len(all) == 0 {
		return
	}
	byPane := map[string][]*model.QueuedPrompt{}
	for _, p := range all {
		if strings.HasPrefix(p.Scope, routerQueuePrefix) {
			continue
		}
		byPane[p.Scope] = append(byPane[p.Scope], p)
	}
	for paneID, items := range byPane {
		if len(items) == 0 || e.isQueuePaused(paneID) || e.inDrainCooldown(paneID, 5*time.Second) || e.isHealingBypassPane(paneID) {
			continue
		}
		st, alive := statusByPane[paneID]
		if !alive || (st != "idle" && st != "done") {
			continue
		}
		first := items[0]
		sendText, hadConstraint := e.decorateDirectPrompt(ctx, paneID, first.Text)
		if err := e.fleet.Prompt(ctx, paneID, sendText); err != nil {
			var he *herdr.Error
			if errors.As(err, &he) && (he.Code == "agent_blocked" || he.Code == "agent_not_ready") {
				continue
			}
			e.logger.Warn("drain pane queued prompt failed", "pane", paneID, "id", first.ID, "err", err)
			continue
		}
		if hadConstraint {
			e.markConstraintDelivered(paneID)
		}
		_ = e.store.DeleteQueuedPrompt(ctx, first.ID)
		e.markDrainCooldown(paneID)
		e.notifyChanged()
	}
}

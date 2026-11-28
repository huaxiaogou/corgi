package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	apiRouterInflightKVKey             = "api_router_inflight"
	apiRouterTurnDeadline              = 420 * time.Second
	apiRouterStepDeadline              = 210 * time.Second
	maxAPIRouterSteps                  = 8
	maxAPIRouterToolCalls              = 12
	maxAPIRouterBareTextRetries        = 2
	maxAPIToolOutputRunes              = 4000
	maxSameAPITriggerFails             = 3
	turnFlagMutated             uint32 = 1 << iota
	turnFlagReplied
	turnFlagDispatched
	turnFlagNoAction
	turnFlagAcked
	turnFlagStopped
)

type routerState struct {
	turn                                     *apiRouterTurn
	turnGen, configEpoch                     uint64
	backoffUntil                             time.Time
	failCount, sameTriggerFails              int
	lastFailTriggerID, lastErr, pendingAckID string
	authPaused                               bool
}

func (s *routerState) resetFailuresLocked(bumpEpoch bool) {
	if bumpEpoch {
		s.configEpoch++
	}
	s.backoffUntil, s.failCount, s.lastFailTriggerID, s.sameTriggerFails, s.authPaused, s.lastErr = time.Time{}, 0, "", 0, false, ""
}

type apiRouterTurn struct {
	gen, configEpoch  uint64
	thread, triggerID string
	cancel            context.CancelFunc
	done              chan struct{}
	startedAt         time.Time
	flags             atomic.Uint32
	mu                sync.Mutex
	summaryOps        []string
}

func (t *apiRouterTurn) hasFlag(bit uint32) bool { return t.flags.Load()&bit != 0 }
func (t *apiRouterTurn) markMutated(op string) {
	t.flags.Or(turnFlagMutated)
	if op != "" {
		t.mu.Lock()
		t.summaryOps = append(t.summaryOps, op)
		t.mu.Unlock()
	}
}
func (t *apiRouterTurn) opsText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return orStr(strings.Join(t.summaryOps, "；"), "已执行变更")
}

type apiRouterInflight struct {
	TriggerID  string    `json:"trigger_id"`
	Thread     string    `json:"thread"`
	Op         string    `json:"op"`
	ReservedID string    `json:"reserved_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

func (e *Engine) cancelAndSettleAPIRouterTurn(ctx context.Context, thread string, ackTrigger bool) {
	e.mu.Lock()
	turn := e.router.turn
	if turn == nil || (thread != "" && turn.thread != thread) {
		e.mu.Unlock()
		return
	}
	if ackTrigger {
		turn.flags.Or(turnFlagStopped)
	}
	cancel, done, trigID, turnThread, sink := turn.cancel, turn.done, turn.triggerID, turn.thread, e.sink
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	if ackTrigger && trigID != "" && e.store != nil {
		e.ackAPITriggerOnce(context.Background(), turn, &model.Trigger{ID: trigID})
		if turn.hasFlag(turnFlagAcked) {
			e.clearAPIRouterInflightIfMatch(context.Background(), trigID)
		}
		sink.OnAPIRouterTurnEnd(context.Background(), turnThread, trigID, "cancelled")
	}
}

func (e *Engine) recordAPIRouterInflight(ctx context.Context, trig *model.Trigger, op, reservedID string) error {
	if e.store == nil || trig == nil {
		return nil
	}
	b, _ := json.Marshal(apiRouterInflight{TriggerID: trig.ID, Thread: orStr(trig.Thread, model.MainThread), Op: op, ReservedID: reservedID, StartedAt: time.Now()})
	if err := e.store.SetKV(ctx, apiRouterInflightKVKey, string(b)); err != nil {
		return fmt.Errorf("persist api router inflight: %w", err)
	}
	return nil
}

func (e *Engine) clearAPIRouterInflightIfMatch(ctx context.Context, triggerID string) {
	var m apiRouterInflight
	if e.store != nil && triggerID != "" {
		if raw, ok, err := e.store.GetKV(ctx, apiRouterInflightKVKey); err == nil && ok && json.Unmarshal([]byte(raw), &m) == nil && m.TriggerID == triggerID {
			_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)
		}
	}
}

// reconcileAPIRouterInflight enforces Strategy 2B: AckTriggerTarget must succeed before DeleteKV or next turn.
func (e *Engine) reconcileAPIRouterInflight(ctx context.Context) bool {
	if e.store == nil {
		return true
	}
	raw, ok, err := e.store.GetKV(ctx, apiRouterInflightKVKey)
	if err != nil {
		e.logger.Warn("reconcile api router inflight read failed", "err", err)
		return false
	}
	var m apiRouterInflight
	hasKV := ok && strings.TrimSpace(raw) != ""
	if hasKV && (json.Unmarshal([]byte(raw), &m) != nil || strings.TrimSpace(m.TriggerID) == "") {
		_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)
		hasKV = false
	}
	e.mu.Lock()
	pendingID := e.router.pendingAckID
	e.mu.Unlock()
	targetID := condStr(hasKV, m.TriggerID, pendingID)
	if targetID == "" {
		return true
	}
	if err := e.store.AckTriggerTarget(ctx, targetID, model.RouterName); err != nil {
		e.logger.Warn("reconcile api router ack failed; retaining guard", "trigger", targetID, "op", m.Op, "err", err)
		return false
	}
	e.mu.Lock()
	if e.router.pendingAckID == targetID {
		e.router.pendingAckID = ""
		e.router.resetFailuresLocked(false)
	}
	e.mu.Unlock()
	if hasKV {
		_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)
		if pendingID != targetID {
			note := fmt.Sprintf("上次大脑回合（%s）在 Hub 重启时中断，为防重复执行已跳过自动重放", m.Op)
			if wk, wErr := e.store.GetWorker(ctx, m.ReservedID); m.ReservedID != "" && wErr == nil && wk != nil {
				note = fmt.Sprintf("上次大脑回合在派发 #%s 后因 Hub 重启中断，该 Worker 已保留（状态：%s），未重复派发", wk.Name, wk.Status)
			} else if en, eErr := e.store.GetEntry(ctx, m.ReservedID); m.ReservedID != "" && eErr == nil && en != nil {
				note = fmt.Sprintf("上次大脑回复（%s）已入库后因 Hub 重启中断，未重复发送", en.ID)
			}
			e.addEvent(ctx, orStr(m.Thread, model.MainThread), note, targetID, "info")
			e.notifyChanged()
		}
	}
	return true
}

func (e *Engine) deliverAPIRouter(ctx context.Context, list []*model.Trigger) {
	if len(list) == 0 {
		return
	}
	e.mu.Lock()
	busy := e.router.turn != nil || e.hubUpdating || e.routerDispatching || e.settings.Paused
	e.mu.Unlock()
	if busy || !e.reconcileAPIRouterInflight(ctx) {
		return
	}
	now := time.Now()
	e.mu.Lock()
	if e.router.turn != nil || e.hubUpdating || e.routerDispatching || e.settings.Paused ||
		e.router.authPaused || e.router.pendingAckID != "" || (!e.router.backoffUntil.IsZero() && now.Before(e.router.backoffUntil)) {
		e.mu.Unlock()
		return
	}
	slices.SortFunc(list, func(a, b *model.Trigger) int { return int(a.Seq - b.Seq) })
	trig := *list[0]
	trig.Targets, trig.Media = append([]string(nil), list[0].Targets...), append([]model.MediaRef(nil), list[0].Media...)
	rootCtx := e.runCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	turnCtx, cancel := context.WithTimeout(rootCtx, apiRouterTurnDeadline)
	e.router.turnGen++
	turn := &apiRouterTurn{gen: e.router.turnGen, configEpoch: e.router.configEpoch, thread: orStr(strings.TrimSpace(trig.Thread), model.MainThread), triggerID: trig.ID, cancel: cancel, done: make(chan struct{}), startedAt: now}
	e.router.turn = turn
	e.mu.Unlock()
	e.notifyChanged()
	go e.runAPIRouterTurn(turnCtx, turn, &trig)
}

func (e *Engine) ackAPITriggerOnce(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger) {
	if turn == nil || trig == nil || e.store == nil || turn.hasFlag(turnFlagAcked) {
		return
	}
	if err := e.store.AckTriggerTarget(ctx, trig.ID, model.RouterName); err != nil {
		e.mu.Lock()
		e.router.pendingAckID = trig.ID
		e.mu.Unlock()
		e.logger.Warn("ack api trigger failed; blocking replay", "trigger", trig.ID, "err", err)
		return
	}
	turn.flags.Or(turnFlagAcked)
	e.mu.Lock()
	e.router.pendingAckID = condStr(e.router.pendingAckID == trig.ID, "", e.router.pendingAckID)
	if e.router.configEpoch == turn.configEpoch {
		e.router.resetFailuresLocked(false)
	}
	e.mu.Unlock()
}

func (e *Engine) runAPIRouterTurn(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger) {
	bg, sealReason := context.Background(), "silent"
	defer func() {
		turn.cancel()
		if !turn.hasFlag(turnFlagMutated) || turn.hasFlag(turnFlagAcked) {
			e.clearAPIRouterInflightIfMatch(bg, turn.triggerID)
		}
		e.mu.Lock()
		if e.router.turn == turn {
			e.router.turn = nil
		}
		sink := e.sink
		e.mu.Unlock()
		close(turn.done)
		if !turn.hasFlag(turnFlagReplied) && sealReason != "" {
			sink.OnAPIRouterTurnEnd(bg, turn.thread, turn.triggerID, sealReason)
		}
		e.notifyChanged()
		e.wake()
	}()
	if e.store != nil {
		fresh, err := e.store.GetTrigger(ctx, trig.ID)
		if err != nil || fresh == nil || fresh.Status != "queued" || (len(fresh.Targets) > 0 && !slices.Contains(fresh.Targets, model.RouterName)) {
			sealReason = ""
			return
		}
		trig = fresh
	}
	cfg, modelName, effort, cacheKey, err := e.resolveActiveRouterAPIConfig(ctx)
	if err != nil {
		sealReason = e.handleAPIRouterFailure(bg, turn, trig, 0, modelName, effort, err)
		return
	}
	turnMeta, allowSilent := e.resolveTurnConsentMeta(ctx, trig), canAPIRouterSilence(trig)
	messages, tools := e.buildAPIRouterMessages(ctx, trig, turnMeta), apiRouterToolDefs()
	totalCalls, bareRetries, nudgeIdx, forceTools, toolChoiceOK, exhausted := 0, 0, -1, false, true, false
	var progress []string
	for step := 0; step < maxAPIRouterSteps; step++ {
		req := LLMChatRequest{Model: modelName, Messages: messages, Tools: tools, ReasoningEffort: effort, PromptCacheKey: cacheKey, Stream: true, StreamOptions: &LLMStreamOptions{IncludeUsage: true}}
		if forceTools && toolChoiceOK {
			req.ToolChoice = "required"
		}
		stepCtx, stepCancel := context.WithTimeout(ctx, apiRouterStepDeadline)
		resp, callErr := e.llm.ChatCompletion(stepCtx, cfg, req)
		if callErr != nil && req.ToolChoice != nil && errors.Is(callErr, ErrLLMBadRequest) {
			toolChoiceOK, req.ToolChoice = false, nil
			e.logger.Warn("api router provider rejected tool_choice=required, retrying without tool_choice", "trigger", trig.ID, "model", modelName, "err", callErr)
			resp, callErr = e.llm.ChatCompletion(stepCtx, cfg, req)
		}
		stepCancel()
		if callErr != nil {
			if turn.hasFlag(turnFlagStopped) {
				e.ackAPITriggerOnce(bg, turn, trig)
				sealReason = "cancelled"
			} else if ctx.Err() != nil && !turn.hasFlag(turnFlagMutated) && errors.Is(ctx.Err(), context.Canceled) {
				sealReason = ""
			} else {
				sealReason = e.handleAPIRouterFailure(bg, turn, trig, step+1, modelName, effort, callErr)
			}
			return
		}
		choice := resp.Choices[0].Message
		e.logLLMStep("api router step", "trigger", trig.ID, step+1, modelName, effort, resp, choice.ToolCalls)
		if len(choice.ToolCalls) == 0 {
			if !turn.hasFlag(turnFlagReplied) && bareRetries < maxAPIRouterBareTextRetries && step < maxAPIRouterSteps-1 {
				bareRetries, forceTools = bareRetries+1, true
				if nudgeIdx < 0 || nudgeIdx != len(messages)-1 {
					messages, nudgeIdx = append(messages, LLMMessage{Role: "user"}), len(messages)
				}
				messages[nudgeIdx].Content = condStr(allowSilent, "[系统内部校验 · 非用户发言] 上一步未调用工具，内容未发送给用户。需通知用户请调用 reply_user，否则调用 no_action(reason)。", "[系统内部校验 · 非用户发言] 上一步未调用工具，内容未发送给用户。本轮必须调用工具，回复请走 reply_user。")
				continue
			}
			if !turn.hasFlag(turnFlagReplied) && !turn.hasFlag(turnFlagDispatched) && !allowSilent {
				e.addEvent(bg, turn.thread, "大脑本轮未通过工具发出有效答复，已中止以避免误发，请重试", trig.ID, "error")
				sealReason = "error"
			}
			break
		}
		forceTools, nudgeIdx = false, -1
		messages = append(messages, LLMMessage{Role: "assistant", Content: choice.Content, ReasoningContent: choice.ReasoningContent, ToolCalls: choice.ToolCalls})
		earlyExit, stepHadErr := false, false
		for _, tc := range choice.ToolCalls {
			totalCalls++
			isClosing := tc.Function.Name == "reply_user" || tc.Function.Name == "no_action"
			var toolOut string
			if totalCalls > maxAPIRouterToolCalls && !isClosing {
				toolOut, stepHadErr = toolErr(condStr(allowSilent, "本轮工具调用次数已达上限（12次），请立刻用 reply_user 汇总结论，或调用 no_action 静默结束。", "本轮工具调用次数已达上限（12次），请立刻用 reply_user 汇总结论。")), true
			} else if isClosing && stepHadErr {
				toolOut = toolErr("同一步前置工具执行失败，请先查看报错并处理后再决定回复或结束。")
			} else {
				e.pushToolProgress(ctx, bg, turn, tc, &progress)
				if toolOut, earlyExit = e.execAPIRouterTool(ctx, turn, trig, turnMeta, tc); strings.Contains(toolOut, `"ok":false`) {
					stepHadErr = true
					e.logger.Warn("api router tool rejected", "trigger", trig.ID, "step", step+1, "tool", tc.Function.Name, "result", oneLineClip(toolOut, 240))
				}
			}
			messages = append(messages, LLMMessage{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: clipRunes(toolOut, maxAPIToolOutputRunes, "...[truncated]")})
			if earlyExit || ctx.Err() != nil {
				break
			}
		}
		if earlyExit {
			break
		}
		if step == maxAPIRouterSteps-2 && !turn.hasFlag(turnFlagReplied) && ctx.Err() == nil {
			messages, forceTools = append(messages, LLMMessage{Role: "user", Content: condStr(allowSilent, "[系统内部提醒 · 非用户发言] 已进入本轮最后一步，请立即调用 reply_user 汇报结论，或调用 no_action(reason) 静默结束。", "[系统内部提醒 · 非用户发言] 已进入本轮最后一步，请立即调用 reply_user 向用户给出明确答复，勿再继续拉取详情。")}), true
		}
		exhausted = step == maxAPIRouterSteps-1
	}
	e.ackAPITriggerOnce(bg, turn, trig)
	switch {
	case turn.hasFlag(turnFlagReplied):
		sealReason = ""
	case turn.hasFlag(turnFlagDispatched):
		sealReason = "dispatched"
	case sealReason != "error" && !allowSilent && !turn.hasFlag(turnFlagNoAction) && (!exhausted || !turn.hasFlag(turnFlagStopped)):
		e.addEvent(bg, turn.thread, condStr(!exhausted, "大脑本轮未能完成有效回复，已结束本轮，请重试", condStr(turn.hasFlag(turnFlagMutated), fmt.Sprintf("已完成部分操作（%s），但达到最大步数（%d 步）仍未发出最终回复，请确认后续安排", turn.opsText(), maxAPIRouterSteps), fmt.Sprintf("本轮已达最大工具步数（%d 步）仍未发出最终回复，已结束本轮，请重试或补充说明", maxAPIRouterSteps))), trig.ID, "error")
		sealReason = "error"
	}
}

func (e *Engine) handleAPIRouterFailure(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, step int, modelName, effort string, err error) string {
	msg := err.Error()
	e.logger.Warn("api router turn failed", "trigger", trig.ID, "thread", turn.thread, "step", step, "model", modelName, "effort", effort, "mutated", turn.hasFlag(turnFlagMutated), "err", msg)
	if turn.hasFlag(turnFlagMutated) || errors.Is(err, ErrLLMBadRequest) {
		e.ackAPITriggerOnce(ctx, turn, trig)
		if !turn.hasFlag(turnFlagReplied) {
			e.addEvent(ctx, turn.thread, condStr(turn.hasFlag(turnFlagMutated), fmt.Sprintf("已完成部分操作（%s），后续模型调用中断：%s", turn.opsText(), oneLineClip(msg, 120)), "本条消息模型请求被拒绝（已跳过以免阻塞后续对话）："+oneLineClip(msg, 140)), trig.ID, "error")
		}
		return "error"
	}
	e.mu.Lock()
	if e.router.configEpoch != turn.configEpoch {
		e.mu.Unlock()
		return "error"
	}
	if errors.Is(err, ErrLLMAuth) {
		prev := e.router.lastErr
		e.router.authPaused, e.router.lastErr = true, msg
		e.mu.Unlock()
		if prev != msg {
			e.addEvent(ctx, turn.thread, "原生 API 大脑鉴权失败（已暂停重试，请检查设置中的 Key）："+oneLineClip(msg, 140), trig.ID, "error")
		}
		return "error"
	}
	if e.router.lastFailTriggerID != trig.ID {
		e.router.lastFailTriggerID, e.router.sameTriggerFails = trig.ID, 0
	}
	e.router.sameTriggerFails++
	if e.router.sameTriggerFails >= maxSameAPITriggerFails {
		e.router.lastErr = msg
		e.mu.Unlock()
		e.ackAPITriggerOnce(ctx, turn, trig)
		e.addEvent(ctx, turn.thread, fmt.Sprintf("本条消息连续 %d 次调用模型失败（已跳过以免阻塞后续对话，可重新发送）：%s", maxSameAPITriggerFails, oneLineClip(msg, 120)), trig.ID, "error")
		return "error"
	}
	fails := e.router.failCount + 1
	backoff := min(5*time.Second<<min(fails-1, 5), 2*time.Minute)
	e.router.failCount, e.router.backoffUntil, e.router.lastErr = fails, time.Now().Add(backoff), msg
	e.mu.Unlock()
	if fails == 1 || fails == 3 {
		e.addEvent(ctx, turn.thread, fmt.Sprintf("原生 API 大脑暂不可用（%s 后自动重试）：%s", backoff.Round(time.Second), oneLineClip(msg, 120)), trig.ID, "error")
	}
	return "error"
}

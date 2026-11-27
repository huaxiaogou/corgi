package channel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

type wecomDecisionCardState struct {
	TaskID      string
	CardType    string
	Expanded    bool
	Preselected []string
	PushCount   int
}

func (b *Bus) cloneDecisionCardState(actionID string) *wecomDecisionCardState {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.wecomCardStates[actionID]
	if st == nil {
		return nil
	}
	cp := *st
	cp.Preselected = append([]string(nil), st.Preselected...)
	return &cp
}

func (b *Bus) saveDecisionCardState(actionID string, fn func(st *wecomDecisionCardState)) *wecomDecisionCardState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wecomCardStates == nil {
		b.wecomCardStates = map[string]*wecomDecisionCardState{}
	}
	st := b.wecomCardStates[actionID]
	if st == nil {
		st = &wecomDecisionCardState{}
		b.wecomCardStates[actionID] = st
	}
	if fn != nil {
		fn(st)
	}
	cp := *st
	cp.Preselected = append([]string(nil), st.Preselected...)
	return &cp
}

func (b *Bus) decisionCardOptions(ctx context.Context, a *model.Action, taskID string, st *wecomDecisionCardState, remainingAfter int) wecom.DecisionCardOptions {
	pos, total := 0, 0
	if b.engine != nil && a != nil && a.Status == model.ActionPending {
		pos, total = b.engine.ActionDecisionPosition(ctx, a.Thread, a.ID)
	}
	consoleURL := ""
	if b.engine != nil && a != nil && a.Status == model.ActionPending && needsConsoleLink(a) {
		consoleURL = b.engine.EffectivePublicURL()
	}
	opts := wecom.DecisionCardOptions{
		TaskID:         taskID,
		QueuePos:       pos,
		QueueTotal:     total,
		ConsoleURL:     consoleURL,
		AllowSnooze:    a != nil && a.Kind != model.ActionKindWorkerDialog,
		RemainingAfter: remainingAfter,
	}
	if st != nil {
		opts.Expanded = st.Expanded
		opts.Preselected = append([]string(nil), st.Preselected...)
	}
	return opts
}

func needsConsoleLink(a *model.Action) bool {
	if a == nil {
		return false
	}
	if utf8.RuneCountInString(strings.TrimSpace(a.Summary)) > 112 {
		return true
	}
	for _, ch := range a.Choices {
		if strings.TrimSpace(ch.Detail) != "" || utf8.RuneCountInString(strings.TrimSpace(ch.Label)) > 24 {
			return true
		}
	}
	return false
}

// pushWeComDecisionCard pushes a native Scheme 3 WeCom template_card immediately for any real
// decision action (never blocked by older pending cards).
func (b *Bus) pushWeComDecisionCard(ctx context.Context, thread, chatID string, action *model.Action, freshTaskID bool) bool {
	if action == nil || !action.IsDecision() || b.wecom == nil || !b.wecom.Online() {
		return false
	}
	prev := b.cloneDecisionCardState(action.ID)
	nextPush := 1
	if prev != nil {
		nextPush = prev.PushCount + 1
	}
	taskID := action.ID
	if freshTaskID || nextPush > 1 || (!action.CreatedAt.IsZero() && time.Since(action.CreatedAt) > 30*time.Second) {
		taskID = fmt.Sprintf("%s_r%d_%d", action.ID, nextPush, time.Now().Unix()%100000)
	}

	opts := b.decisionCardOptions(ctx, action, taskID, prev, 0)
	cardType, err := b.wecom.SendDecisionCard(ctx, chatID, action, opts)
	if err != nil {
		b.logger.Warn("push decision card to wecom failed", "action", action.ID, "err", err)
		return false
	}

	action.PushedToWeCom = true
	if b.engine != nil {
		_ = b.engine.MarkActionPushedToWeCom(ctx, action.ID)
	}
	b.saveDecisionCardState(action.ID, func(st *wecomDecisionCardState) {
		st.TaskID = taskID
		st.CardType = cardType
		st.PushCount = nextPush
	})
	return true
}

func (b *Bus) handleDecisionCardClick(ctx context.Context, ev wecom.CardClickEvent) {
	if b.engine == nil {
		return
	}
	st := b.cloneDecisionCardState(ev.ActionID)
	act, err := b.engine.GetAction(ctx, ev.ActionID)
	if err != nil || act == nil {
		b.logger.Warn("wecom card click action lookup failed", "action", ev.ActionID, "err", err)
		return
	}
	cardType := resolveEventCardType(ev, st, act)

	// 1. Stale click on an already settled card -> refresh locked receipt in-place without double-recording.
	if act.Status != model.ActionPending {
		b.logger.Info("refreshing already-resolved wecom action card", "action", act.ID, "status", act.Status, "resolved_by", act.ResolvedBy)
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, st, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(act, cardType, opts))
		}
		return
	}

	// 2. Vote interaction callbacks (checkbox/radio + explicit submit, inline fold/expand, snooze).
	if cardType == "vote_interaction" || ev.ChoiceKey == "snooze_apply" || len(ev.SelectedKeys) > 0 {
		b.handleVoteCardInteraction(ctx, ev, act, st)
		return
	}

	// 3. Button interaction callbacks (fast 1-2 option single-select and snooze picker).
	b.handleButtonCardInteraction(ctx, ev, act, st)
}

func (b *Bus) handleButtonCardInteraction(ctx context.Context, ev wecom.CardClickEvent, act *model.Action, st *wecomDecisionCardState) {
	switch ev.ChoiceKey {
	case "snooze_menu":
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, st, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildSnoozeCard(act, "button_interaction", opts))
		}
		return
	case "back":
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, st, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildDecisionCard(act, opts))
		}
		return
	case "snooze:1h":
		b.applyWeComSnooze(ctx, ev, act, st, "button_interaction", time.Now().Add(time.Hour))
		return
	case "snooze:tmr":
		b.applyWeComSnooze(ctx, ev, act, st, "button_interaction", time.Time{})
		return
	case "dismiss":
		b.applyWeComDismiss(ctx, ev, act, st, "button_interaction")
		return
	}
	b.commitWeComResolution(ctx, ev, act, st, "button_interaction", []string{ev.ChoiceKey})
}

func (b *Bus) handleVoteCardInteraction(ctx context.Context, ev wecom.CardClickEvent, act *model.Action, st *wecomDecisionCardState) {
	if ev.ChoiceKey == "snooze_apply" ||
		slices.Contains(ev.SelectedKeys, wecom.SentinelSnooze1H) ||
		slices.Contains(ev.SelectedKeys, wecom.SentinelSnoozeTomorrow) ||
		slices.Contains(ev.SelectedKeys, wecom.SentinelDismiss) ||
		slices.Contains(ev.SelectedKeys, wecom.SentinelBack) {
		switch {
		case slices.Contains(ev.SelectedKeys, wecom.SentinelSnooze1H):
			b.applyWeComSnooze(ctx, ev, act, st, "vote_interaction", time.Now().Add(time.Hour))
		case slices.Contains(ev.SelectedKeys, wecom.SentinelSnoozeTomorrow):
			b.applyWeComSnooze(ctx, ev, act, st, "vote_interaction", time.Time{})
		case slices.Contains(ev.SelectedKeys, wecom.SentinelDismiss):
			b.applyWeComDismiss(ctx, ev, act, st, "vote_interaction")
		default:
			if b.wecom != nil {
				opts := b.decisionCardOptions(ctx, act, ev.TaskID, st, 0)
				_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildDecisionCard(act, opts))
			}
		}
		return
	}

	var incomingReal []string
	var wantMore, wantFold, wantSnooze bool
	for _, k := range ev.SelectedKeys {
		switch k {
		case wecom.SentinelMore:
			wantMore = true
		case wecom.SentinelFold:
			wantFold = true
		case wecom.SentinelSnooze:
			wantSnooze = true
		default:
			incomingReal = append(incomingReal, k)
		}
	}

	wasExpanded := st != nil && st.Expanded
	var prevSel []string
	if st != nil {
		prevSel = st.Preselected
	}
	merged := mergeVoteSelections(act, prevSel, incomingReal, wasExpanded)

	switch {
	case wantMore || wantFold:
		updated := b.saveDecisionCardState(act.ID, func(s *wecomDecisionCardState) {
			s.CardType = "vote_interaction"
			s.Expanded = wantMore
			s.Preselected = merged
		})
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, updated, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildDecisionCard(act, opts))
		}
		return

	case wantSnooze:
		updated := b.saveDecisionCardState(act.ID, func(s *wecomDecisionCardState) {
			s.CardType = "vote_interaction"
			s.Preselected = merged
		})
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, updated, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildSnoozeCard(act, "vote_interaction", opts))
		}
		return
	}

	if len(merged) == 0 {
		if b.wecom != nil {
			opts := b.decisionCardOptions(ctx, act, ev.TaskID, st, 0)
			_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildDecisionCard(act, opts))
		}
		return
	}
	b.commitWeComResolution(ctx, ev, act, st, "vote_interaction", merged)
}

func (b *Bus) commitWeComResolution(ctx context.Context, ev wecom.CardClickEvent, act *model.Action, st *wecomDecisionCardState, cardType string, choices []string) {
	resolved, err := b.engine.ResolveActionChoices(ctx, act.ID, "wecom", choices)
	if err != nil {
		if errors.Is(err, store.ErrActionAlreadyResolved) {
			existing, gErr := b.engine.GetAction(ctx, act.ID)
			if gErr != nil || existing == nil {
				return
			}
			if b.wecom != nil {
				opts := b.decisionCardOptions(ctx, existing, ev.TaskID, st, 0)
				_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(existing, cardType, opts))
			}
			return
		}
		b.logger.Warn("wecom card resolution failed", "action", act.ID, "choices", choices, "err", err)
		return
	}
	b.logger.Info("resolved action from wecom card click", "action", resolved.ID, "choice_key", resolved.ChosenKey, "choice_label", resolved.ChosenLabel)
	remaining := len(b.engine.ListPendingDecisionsForThread(ctx, resolved.Thread))
	if b.wecom != nil {
		opts := b.decisionCardOptions(ctx, resolved, ev.TaskID, st, remaining)
		if err := b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(resolved, cardType, opts)); err != nil {
			b.logger.Warn("update wecom template card failed", "action", resolved.ID, "req_id", ev.ReqID, "err", err)
		}
	}
}

func (b *Bus) applyWeComSnooze(ctx context.Context, ev wecom.CardClickEvent, act *model.Action, st *wecomDecisionCardState, cardType string, until time.Time) {
	snoozed, err := b.engine.SnoozeActionBy(ctx, act.ID, "wecom", until)
	if err != nil {
		if errors.Is(err, store.ErrActionAlreadyResolved) {
			if existing, gErr := b.engine.GetAction(ctx, act.ID); gErr == nil && existing != nil && b.wecom != nil {
				opts := b.decisionCardOptions(ctx, existing, ev.TaskID, st, 0)
				_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(existing, cardType, opts))
			}
			return
		}
		b.logger.Warn("wecom card snooze failed", "action", act.ID, "err", err)
		return
	}
	remaining := len(b.engine.ListPendingDecisionsForThread(ctx, snoozed.Thread))
	if b.wecom != nil {
		opts := b.decisionCardOptions(ctx, snoozed, ev.TaskID, st, remaining)
		_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(snoozed, cardType, opts))
	}
}

func (b *Bus) applyWeComDismiss(ctx context.Context, ev wecom.CardClickEvent, act *model.Action, st *wecomDecisionCardState, cardType string) {
	dismissed, err := b.engine.DismissActionBy(ctx, act.ID, "wecom")
	if err != nil {
		if errors.Is(err, store.ErrActionAlreadyResolved) {
			if existing, gErr := b.engine.GetAction(ctx, act.ID); gErr == nil && existing != nil && b.wecom != nil {
				opts := b.decisionCardOptions(ctx, existing, ev.TaskID, st, 0)
				_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(existing, cardType, opts))
			}
			return
		}
		b.logger.Warn("wecom card dismiss failed", "action", act.ID, "err", err)
		return
	}
	remaining := len(b.engine.ListPendingDecisionsForThread(ctx, dismissed.Thread))
	if b.wecom != nil {
		opts := b.decisionCardOptions(ctx, dismissed, ev.TaskID, st, remaining)
		_ = b.wecom.UpdateTemplateCard(ctx, ev.ReqID, wecom.BuildResolvedDecisionCard(dismissed, cardType, opts))
	}
}

// handleWeComDecisionListCommand handles `/d`, `待拍板`, and `/d <编号>`: lists all real pending
// decisions (excluding router_check) and re-surfaces the requested (or top) decision card on demand.
func (b *Bus) handleWeComDecisionListCommand(ctx context.Context, thread, chatID, reqID, query string) {
	if b.engine == nil {
		return
	}
	pending := b.engine.ListPendingDecisionsForThread(ctx, thread)
	if len(pending) == 0 {
		b.sendInstantWeComText(ctx, chatID, reqID, "当前没有待拍板事项。")
		return
	}

	targetIdx := 0
	if q := strings.TrimSpace(query); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > len(pending) {
			b.sendInstantWeComText(ctx, chatID, reqID, fmt.Sprintf("编号无效，请输入 `1`–`%d`（或直接发 `/d` 查看完整清单）。", len(pending)))
			return
		}
		targetIdx = n - 1
		if b.pushWeComDecisionCard(ctx, thread, chatID, pending[targetIdx], true) {
			return
		}
		b.sendInstantWeComText(ctx, chatID, reqID, fmt.Sprintf("**待拍板 #%d**：%s", n, firstNonEmptyStr(pending[targetIdx].Title, pending[targetIdx].Summary)))
		return
	}

	pushed := b.pushWeComDecisionCard(ctx, thread, chatID, pending[0], true)
	if len(pending) == 1 && pushed {
		return
	}
	limit := min(len(pending), 10)
	lines := make([]string, 0, limit+3)
	lines = append(lines, fmt.Sprintf("**待拍板清单（共 %d 项）**", len(pending)))
	for i := 0; i < limit; i++ {
		prefix := fmt.Sprintf("%d.", i+1)
		if i == 0 && pushed {
			prefix = "1. [已唤起卡片]"
		}
		lines = append(lines, fmt.Sprintf("%s %s", prefix, firstNonEmptyStr(pending[i].Title, pending[i].Summary)))
	}
	if len(pending) > limit {
		lines = append(lines, fmt.Sprintf("… 另有 %d 项待拍板", len(pending)-limit))
	}
	lines = append(lines, "\n发 `/d <编号>` 可直接重新唤起任意一项的拍板卡。")
	b.sendInstantWeComText(ctx, chatID, reqID, strings.Join(lines, "\n"))
}

func resolveEventCardType(ev wecom.CardClickEvent, st *wecomDecisionCardState, act *model.Action) string {
	if ct := strings.TrimSpace(ev.CardType); ct != "" {
		return ct
	}
	if st != nil && strings.TrimSpace(st.CardType) != "" {
		return st.CardType
	}
	if len(ev.SelectedKeys) > 0 || ev.ChoiceKey == "snooze_apply" {
		return "vote_interaction"
	}
	if act != nil && !act.IsFastSingle() && ev.ChoiceKey == "submit" {
		return "vote_interaction"
	}
	return "button_interaction"
}

func mergeVoteSelections(a *model.Action, prevSelected, incomingReal []string, wasExpanded bool) []string {
	if a == nil {
		return incomingReal
	}
	if !a.IsMulti() {
		if len(incomingReal) > 0 {
			return []string{incomingReal[0]}
		}
		if len(prevSelected) > 0 && !wasExpanded {
			return []string{prevSelected[0]}
		}
		return nil
	}
	out := make([]string, 0, len(incomingReal)+len(prevSelected))
	seen := map[string]bool{}
	for _, k := range incomingReal {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if !wasExpanded && len(a.Choices) > wecom.DecisionFoldLimit {
		hiddenKeys := map[string]bool{}
		for i := wecom.DecisionFoldLimit; i < len(a.Choices); i++ {
			k := strings.TrimSpace(a.Choices[i].Key)
			if k == "" {
				k = strconv.Itoa(i + 1)
			}
			hiddenKeys[k] = true
		}
		for _, k := range prevSelected {
			if hiddenKeys[k] && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

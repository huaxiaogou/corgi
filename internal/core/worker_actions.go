package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

// GetAction loads one Action by ID from the durable store.
func (e *Engine) GetAction(ctx context.Context, actionID string) (*model.Action, error) {
	return e.store.GetAction(ctx, actionID)
}

// ResolveAction atomically resolves a pending Action from Web, WeCom, Router, or CLI.
func (e *Engine) ResolveAction(ctx context.Context, actionID, resolvedBy, choiceInput string) (*model.Action, error) {
	return e.resolveAction(ctx, actionID, resolvedBy, choiceInput, false, false)
}

// ResolveActionCustom atomically resolves a pending decision card with a human-written custom answer,
// bypassing preset choice fuzzy matching so custom text is never overwritten by a preset option.
func (e *Engine) ResolveActionCustom(ctx context.Context, actionID, resolvedBy, customText string) (*model.Action, error) {
	return e.resolveAction(ctx, actionID, resolvedBy, customText, true, false)
}

// ResolveActionCustomInPlace atomically resolves a pending decision card with a human-written custom answer
// while preserving explicit Web in-place intent for the recorded user entry.
func (e *Engine) ResolveActionCustomInPlace(ctx context.Context, actionID, resolvedBy, customText string, inPlace bool) (*model.Action, error) {
	return e.resolveAction(ctx, actionID, resolvedBy, customText, true, inPlace)
}

func (e *Engine) resolveAction(ctx context.Context, actionID, resolvedBy, choiceInput string, custom, inPlace bool) (*model.Action, error) {
	trimmed := strings.TrimSpace(choiceInput)
	if trimmed == "" {
		return nil, fmt.Errorf("empty action choice")
	}
	act, err := e.store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if act.Status != model.ActionPending {
		return nil, store.ErrActionAlreadyResolved
	}
	if act.Kind == model.ActionKindConfigConfirm {
		return e.resolveConfigConfirmAction(ctx, actionID, resolvedBy, choiceInput, custom)
	}
	var (
		chosenKey    string
		chosenLabel  string
		chosenCursor int
	)
	if custom {
		if act.Kind == model.ActionKindWorkerDialog {
			return nil, fmt.Errorf("terminal dialog does not accept custom free-text input")
		}
		chosenKey = "custom"
		chosenLabel = trimmed
	} else if act.Kind == model.ActionKindRouterCheck && len(act.Choices) == 0 &&
		(strings.EqualFold(trimmed, "ack") || strings.EqualFold(trimmed, "seen") || trimmed == "已看") {
		chosenKey = "ack"
		chosenLabel = "已看"
	} else if k, lbl, cur, ok := matchStrictActionChoice(act, trimmed); ok {
		chosenKey, chosenLabel, chosenCursor = k, lbl, cur
	} else if act.Kind == model.ActionKindWorkerDialog {
		// Allow label-word match for AnswerWorker CLI convenience, but never send unmatched arbitrary text to a live terminal.
		var matched bool
		chosenKey, chosenLabel, chosenCursor, matched = matchLabelSubstringChoice(act, trimmed)
		if !matched {
			return nil, fmt.Errorf("invalid choice %q for terminal dialog %s", trimmed, act.ID)
		}
	} else if len(act.Choices) > 0 {
		return nil, fmt.Errorf("invalid choice %q for action %s", trimmed, act.ID)
	} else {
		chosenKey, chosenLabel, chosenCursor = matchActionChoice(act, trimmed)
	}
	resolved, err := e.store.ResolveAction(ctx, actionID, resolvedBy, chosenKey, chosenLabel)
	if err != nil {
		return nil, err
	}

	switch resolved.Kind {
	case model.ActionKindWorkerDialog:
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := e.sendDialogAnswer(dctx, resolved.PaneID, chosenKey, chosenCursor)
		cancel()
		if err != nil {
			e.logger.Warn("failed sending dialog answer to worker", "worker", resolved.TargetWorker, "err", err)
		}
		e.mu.Lock()
		e.lastAnswer[resolved.PaneID] = time.Now()
		e.mu.Unlock()
		e.addEventWithAction(ctx, resolved.Thread, fmt.Sprintf("已为 %s 选择：%s（via %s）", resolved.TargetWorker, chosenLabel, resolvedBy), resolved.TargetWorker, "info", resolved.ID)
	case model.ActionKindRouterCheck:
		// Silent acknowledgement ("✓ 已看") clears the reminder without waking Router.
		if custom || chosenKey != "ack" {
			promptCtx := ""
			if custom {
				promptCtx = buildCustomDecisionPromptContext(resolved)
			}
			_, _, _ = e.Say(ctx, SayRequest{
				Thread:        resolved.Thread,
				Channel:       resolvedBy,
				From:          resolvedBy,
				Text:          chosenLabel,
				ReEntryID:     resolved.EntryID,
				InPlace:       custom && inPlace,
				ActionID:      resolved.ID,
				PromptContext: promptCtx,
			})
		}
	case model.ActionKindRouterChoice, model.ActionKindPatrolDigest:
		promptCtx := ""
		if custom {
			promptCtx = buildCustomDecisionPromptContext(resolved)
		}
		_, _, _ = e.Say(ctx, SayRequest{
			Thread:        resolved.Thread,
			Channel:       resolvedBy,
			From:          resolvedBy,
			Text:          chosenLabel,
			ReEntryID:     resolved.EntryID,
			InPlace:       custom && inPlace,
			ActionID:      resolved.ID,
			PromptContext: promptCtx,
		})
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, resolved)
	sink.OnStateChanged()
	return resolved, nil
}

func buildCustomDecisionPromptContext(a *model.Action) string {
	q := oneLineClip(orStr(a.Summary, a.Title), 240)
	var b strings.Builder
	fmt.Fprintf(&b, "[In reply to decision card: %q]", q)
	if len(a.Choices) > 0 {
		opts := make([]string, 0, len(a.Choices))
		for i, c := range a.Choices {
			opts = append(opts, fmt.Sprintf("%d. %s", i+1, c.Label))
		}
		fmt.Fprintf(&b, "\n[Preset options were: %s — human provided a custom answer instead]", strings.Join(opts, " | "))
	}
	return b.String()
}

// DismissAction atomically dismisses a pending reminder/choice without sending a reply to Router.
func (e *Engine) DismissAction(ctx context.Context, actionID string) (*model.Action, error) {
	return e.DismissActionBy(ctx, actionID, "")
}

// DismissActionBy atomically dismisses a pending action and records the originating channel.
func (e *Engine) DismissActionBy(ctx context.Context, actionID, channel string) (*model.Action, error) {
	existing, err := e.store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Kind == model.ActionKindConfigConfirm {
		return e.dismissConfigConfirmAction(ctx, actionID, channel)
	}
	act, err := e.store.DismissActionBy(ctx, actionID, channel)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, act)
	sink.OnStateChanged()
	return act, nil
}

// AnswerWorker answers a blocked worker's live terminal dialog by choice key, label words, or "esc".
func (e *Engine) AnswerWorker(ctx context.Context, workerName, choiceInput, by string) (string, error) {
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended || wk.PaneID == "" {
		return "", fmt.Errorf("no running worker %q", workerName)
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, a := range pending {
			if a.Kind == model.ActionKindWorkerDialog && a.TargetWorker == workerName {
				res, err := e.ResolveAction(ctx, a.ID, by, choiceInput)
				if err != nil {
					return "", err
				}
				return res.ChosenLabel, nil
			}
		}
	}
	rr, err := e.fleet.Read(ctx, wk.PaneID, "detection", "text", 0)
	if err != nil {
		return "", err
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) == 0 && !strings.EqualFold(strings.TrimSpace(choiceInput), "esc") {
		return "", fmt.Errorf("worker %s is not showing a dialog", workerName)
	}
	tmp := &model.Action{Choices: approvalToChoices(ap)}
	key, label, cursor := matchActionChoice(tmp, choiceInput)
	if err := e.sendDialogAnswer(ctx, wk.PaneID, key, cursor); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.lastAnswer[wk.PaneID] = time.Now()
	e.mu.Unlock()
	e.addEvent(ctx, wk.Thread, fmt.Sprintf("替 %s 选了：%s", workerName, label), workerName, "info")
	return label, nil
}

func (e *Engine) sendDialogAnswer(ctx context.Context, paneID, key string, cursor int) error {
	if paneID == "" {
		return nil
	}
	if strings.EqualFold(key, "esc") {
		return e.fleet.SendKeys(ctx, paneID, "esc")
	}
	if cursor > 0 && key == "" {
		if rr, err := e.fleet.Read(ctx, paneID, "detection", "text", 0); err == nil {
			ap := agent.ParseApproval(rr.Text)
			if cursor-1 < len(ap.Choices) {
				return e.fleet.SendKeys(ctx, paneID, agent.MoveKeys(ap, cursor-1)...)
			}
		}
		keys := make([]string, 0, cursor)
		for i := 1; i < cursor; i++ {
			keys = append(keys, "down")
		}
		keys = append(keys, "enter")
		return e.fleet.SendKeys(ctx, paneID, keys...)
	}
	return e.fleet.SendText(ctx, paneID, key, true)
}

func matchLabelSubstringChoice(a *model.Action, raw string) (key, label string, cursor int, ok bool) {
	low := strings.ToLower(strings.TrimSpace(raw))
	if low == "" {
		return "", "", 0, false
	}
	for i, c := range a.Choices {
		if strings.Contains(strings.ToLower(c.Label), low) {
			return c.Key, c.Label, i + 1, true
		}
	}
	return "", "", 0, false
}

func matchActionChoice(a *model.Action, raw string) (key, label string, cursor int) {
	q := strings.TrimSpace(raw)
	if strings.EqualFold(q, "esc") {
		return "esc", "取消 (Esc)", 0
	}
	low := strings.ToLower(q)
	for i, c := range a.Choices {
		if (c.Key != "" && strings.EqualFold(c.Key, q)) || (c.Label != "" && strings.EqualFold(c.Label, q)) {
			return c.Key, c.Label, i + 1
		}
	}
	for i, c := range a.Choices {
		if c.Value != "" && strings.EqualFold(c.Value, q) {
			return c.Key, c.Label, i + 1
		}
	}
	if idx, err := strconv.Atoi(q); err == nil && idx >= 1 && idx <= len(a.Choices) {
		c := a.Choices[idx-1]
		return c.Key, c.Label, idx
	}
	for i, c := range a.Choices {
		if low != "" && strings.Contains(strings.ToLower(c.Label), low) {
			return c.Key, c.Label, i + 1
		}
	}
	return q, q, 0
}

func approvalToChoices(ap model.Approval) []model.ActionChoice {
	out := make([]model.ActionChoice, 0, len(ap.Choices))
	for _, c := range ap.Choices {
		key := c.Key
		if ap.Style == "cursor" {
			key = ""
		}
		out = append(out, model.ActionChoice{Key: key, Label: c.Label, Value: strconv.Itoa(c.Cursor)})
	}
	return out
}

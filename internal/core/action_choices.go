package core

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

// maxActionOptions caps selectable options at 16 so WeCom vote_interaction (20-option hard ceiling)
// can always render every choice plus up to 3 utility items (expand/fold, snooze, console link)
// without ever silently truncating tail options.
const maxActionOptions = 16

// ActionPriority ranks pending decisions identically across Web and WeCom.
func ActionPriority(a *model.Action) int {
	if a == nil {
		return 9
	}
	if a.Kind == model.ActionKindWorkerDialog || a.Category == "blocked" {
		return 0
	}
	if a.Category == "worktree" {
		return 1
	}
	if a.Kind == model.ActionKindPatrolDigest || a.Kind == model.ActionKindRouterChoice {
		return 2
	}
	return 3
}

// ListPendingDecisionsForThread returns ONLY real pending decisions (Action.IsDecision() == true,
// excluding lightweight router_check reminders) in thread, ordered by priority then creation time.
func (e *Engine) ListPendingDecisionsForThread(ctx context.Context, thread string) []*model.Action {
	all, err := e.store.ListPendingActions(ctx)
	if err != nil || len(all) == 0 {
		return nil
	}
	thread = strings.TrimSpace(thread)
	out := make([]*model.Action, 0, len(all))
	for _, a := range all {
		if a == nil || a.Status != model.ActionPending || !a.IsDecision() {
			continue
		}
		if thread != "" && a.Thread != thread {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := ActionPriority(out[i]), ActionPriority(out[j])
		if pi != pj {
			return pi < pj
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ActionDecisionPosition returns the 1-based position of actionID among pending real decisions
// in thread and the total pending decision count.
func (e *Engine) ActionDecisionPosition(ctx context.Context, thread, actionID string) (pos, total int) {
	list := e.ListPendingDecisionsForThread(ctx, thread)
	total = len(list)
	for i, a := range list {
		if a.ID == actionID {
			return i + 1, total
		}
	}
	return 0, total
}

// ResolveActionChoices strictly validates and atomically resolves a decision card with 1+ choice keys.
//   - Single-select actions accept EXACTLY 1 valid choice key.
//   - Multi-select actions accept >= 1 valid distinct choice keys (rejecting worker_dialog and router_check).
//   - Any unrecognized choice key is rejected instead of silently echoing arbitrary strings.
func (e *Engine) ResolveActionChoices(ctx context.Context, actionID, resolvedBy string, choiceInputs []string) (*model.Action, error) {
	cleaned := make([]string, 0, len(choiceInputs))
	for _, raw := range choiceInputs {
		if s := strings.TrimSpace(raw); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("empty action choices")
	}
	act, err := e.store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if act.Status != model.ActionPending {
		return nil, store.ErrActionAlreadyResolved
	}
	if !act.IsMulti() {
		if len(cleaned) != 1 {
			return nil, fmt.Errorf("single-select action %s accepts exactly one choice (got %d)", act.ID, len(cleaned))
		}
		if len(act.Choices) > 0 {
			if _, _, _, ok := matchStrictActionChoice(act, cleaned[0]); !ok {
				return nil, fmt.Errorf("invalid choice %q for action %s", cleaned[0], act.ID)
			}
		}
		return e.ResolveAction(ctx, actionID, resolvedBy, cleaned[0])
	}
	if act.Kind == model.ActionKindWorkerDialog || act.Kind == model.ActionKindRouterCheck {
		return nil, fmt.Errorf("action kind %s does not support multiple selections", act.Kind)
	}
	if len(act.Choices) == 0 {
		return nil, fmt.Errorf("action %s has no selectable choices", act.ID)
	}

	seen := map[string]bool{}
	keys := make([]string, 0, len(cleaned))
	labels := make([]string, 0, len(cleaned))
	for _, raw := range cleaned {
		k, lbl, _, ok := matchStrictActionChoice(act, raw)
		if !ok {
			return nil, fmt.Errorf("invalid choice %q for action %s", raw, act.ID)
		}
		dedupKey := k
		if dedupKey == "" {
			dedupKey = lbl
		}
		if dedupKey == "" || seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		keys = append(keys, k)
		labels = append(labels, lbl)
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("no valid action choices matched")
	}

	chosenKey := strings.Join(keys, ",")
	chosenLabel := fmt.Sprintf("已选 %d 项：%s", len(labels), strings.Join(labels, "、"))
	resolved, err := e.store.ResolveActionWithKeys(ctx, actionID, resolvedBy, chosenKey, chosenLabel, keys)
	if err != nil {
		return nil, err
	}

	if resolved.Kind == model.ActionKindRouterChoice || resolved.Kind == model.ActionKindPatrolDigest {
		_, _, _ = e.Say(ctx, SayRequest{
			Thread:        resolved.Thread,
			Channel:       resolvedBy,
			From:          resolvedBy,
			Text:          chosenLabel,
			ReEntryID:     resolved.EntryID,
			ActionID:      resolved.ID,
			PromptContext: buildMultiDecisionPromptContext(resolved, labels),
		})
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, resolved)
	sink.OnStateChanged()
	return resolved, nil
}

func buildMultiDecisionPromptContext(a *model.Action, labels []string) string {
	q := oneLineClip(orStr(a.Summary, a.Title), 240)
	return fmt.Sprintf("[In reply to multi-select decision card: %q]\n[Selected %d/%d option(s): %s]",
		q, len(labels), len(a.Choices), strings.Join(labels, " | "))
}

// matchStrictActionChoice matches raw strictly against a real choice on a (or "esc" on worker_dialog).
// Unlike legacy fuzzy fallback, it returns ok=false on unrecognized keys so invalid inputs are rejected.
func matchStrictActionChoice(a *model.Action, raw string) (key, label string, cursor int, ok bool) {
	if a == nil {
		return "", "", 0, false
	}
	q := strings.TrimSpace(raw)
	if q == "" {
		return "", "", 0, false
	}
	if strings.EqualFold(q, "esc") && a.Kind == model.ActionKindWorkerDialog {
		return "esc", "取消 (Esc)", 0, true
	}
	for i, c := range a.Choices {
		if (c.Key != "" && strings.EqualFold(c.Key, q)) || (c.Label != "" && strings.EqualFold(c.Label, q)) {
			k := c.Key
			if k == "" && a.Kind != model.ActionKindWorkerDialog {
				k = strconv.Itoa(i + 1)
			}
			return k, c.Label, i + 1, true
		}
	}
	for i, c := range a.Choices {
		if c.Value != "" && strings.EqualFold(c.Value, q) {
			k := c.Key
			if k == "" && a.Kind != model.ActionKindWorkerDialog {
				k = strconv.Itoa(i + 1)
			}
			return k, c.Label, i + 1, true
		}
	}
	if idx, err := strconv.Atoi(q); err == nil && idx >= 1 && idx <= len(a.Choices) {
		c := a.Choices[idx-1]
		k := c.Key
		if k == "" && a.Kind != model.ActionKindWorkerDialog {
			k = strconv.Itoa(idx)
		}
		return k, c.Label, idx, true
	}
	return "", "", 0, false
}

// normalizeStructuredChoices validates structured ActionChoice or plain option inputs without silent truncation.
func normalizeStructuredChoices(raw []model.ActionChoice, fallbackOpts []string) ([]model.ActionChoice, []string, error) {
	if len(raw) == 0 {
		opts, err := cleanOptions(fallbackOpts)
		if err != nil {
			return nil, nil, err
		}
		if len(opts) == 0 {
			return nil, nil, nil
		}
		choices := make([]model.ActionChoice, 0, len(opts))
		for i, label := range opts {
			choices = append(choices, model.ActionChoice{
				Key:   strconv.Itoa(i + 1),
				Label: label,
			})
		}
		return choices, opts, nil
	}
	choices := make([]model.ActionChoice, 0, len(raw))
	opts := make([]string, 0, len(raw))
	seenKeys := map[string]bool{}
	for i, ch := range raw {
		label := strings.Join(strings.Fields(ch.Label), " ")
		if label == "" {
			continue
		}
		if n := len([]rune(label)); n > maxOptionRunes {
			return nil, nil, fmt.Errorf("option %d exceeds %d characters (%d)", i+1, maxOptionRunes, n)
		}
		if len(choices) >= maxActionOptions {
			return nil, nil, fmt.Errorf("too many options (max %d supported without truncation)", maxActionOptions)
		}
		detail := strings.Join(strings.Fields(ch.Detail), " ")
		if n := len([]rune(detail)); n > maxOptionRunes {
			detail = oneLineClip(detail, maxOptionRunes)
		}
		key := strings.TrimSpace(ch.Key)
		if key == "" || seenKeys[key] {
			key = strconv.Itoa(len(choices) + 1)
		}
		seenKeys[key] = true
		choices = append(choices, model.ActionChoice{
			Key:         key,
			Label:       label,
			Detail:      detail,
			Value:       strings.TrimSpace(ch.Value),
			Recommended: ch.Recommended,
		})
		opts = append(opts, label)
	}
	return choices, opts, nil
}

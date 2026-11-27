package wecom

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

// BuildResolvedDecisionCard constructs an in-place locked receipt template_card matching the
// originating card_type (button_interaction or vote_interaction) with replace_text set cleanly.
func BuildResolvedDecisionCard(a *model.Action, cardType string, opts DecisionCardOptions) map[string]any {
	if a == nil {
		a = &model.Action{ID: opts.TaskID, Status: model.ActionResolved}
	}
	if cardType == "vote_interaction" {
		return buildResolvedVoteCard(a, opts)
	}
	return buildResolvedButtonCard(a, opts)
}

func buildResolvedVoteCard(a *model.Action, opts DecisionCardOptions) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(opts.TaskID, a.ID))
	sourceDesc, statusText, _, mainDesc, chosen, replaceText := resolveCardSummaryFields(a, opts)
	title, _, _, _ := shapeCardCopy(a, len(a.Choices))

	chosenSet := map[string]bool{}
	for _, k := range a.ChosenKeys {
		if t := strings.TrimSpace(k); t != "" {
			chosenSet[t] = true
		}
	}
	if k := strings.TrimSpace(a.ChosenKey); k != "" && !strings.Contains(k, ",") {
		chosenSet[k] = true
	}

	choices := effectiveChoices(a)
	optList := make([]map[string]any, 0, min(len(choices), voteMaxOptions))
	matched := 0
	for i, ch := range choices {
		if len(optList) >= voteMaxOptions {
			break
		}
		k := choiceKey(ch, i)
		isChecked := a.Status == model.ActionResolved && (chosenSet[k] || (len(chosenSet) == 0 && ch.Label == chosen))
		if isChecked {
			matched++
		}
		optList = append(optList, map[string]any{
			"id":         k,
			"text":       formatVoteOptionText(ch),
			"is_checked": isChecked,
		})
	}
	if len(optList) == 0 || (matched == 0 && a.Status != model.ActionResolved) {
		summaryOpt := statusText
		if a.Status == model.ActionSnoozed && a.SnoozeUntil != nil {
			summaryOpt = "已暂缓至 " + a.SnoozeUntil.Format("01-02 15:04")
		} else if chosen != "" {
			summaryOpt = chosen
		}
		optList = []map[string]any{
			{"id": "result", "text": clipRunes(summaryOpt, voteOptionMaxRunes), "is_checked": a.Status == model.ActionResolved},
		}
	}

	var descParts []string
	if mainDesc != "" {
		descParts = append(descParts, mainDesc)
	}
	if a.Status == model.ActionResolved && chosen != "" {
		descParts = append(descParts, "已选："+chosen)
	}
	if opts.RemainingAfter > 0 {
		descParts = append(descParts, fmt.Sprintf("剩余 %d 项待拍板（发 /d 查看）", opts.RemainingAfter))
	}

	mode := 0
	if a.IsMulti() {
		mode = 1
	}
	return map[string]any{
		"card_type": "vote_interaction",
		"source": map[string]any{
			"desc":       sourceDesc,
			"desc_color": 1,
		},
		"main_title": map[string]any{
			"title": title,
			"desc":  clipRunes(strings.Join(descParts, " · "), voteMainDescMaxRunes),
		},
		"checkbox": map[string]any{
			"question_key": sanitizeTaskID(a.ID),
			"mode":         mode,
			"disable":      true,
			"option_list":  optList,
		},
		"submit_button": map[string]any{
			"text": clipRunes(statusText, cardButtonTextMaxRunes),
			"key":  tid + ":resolved",
		},
		"replace_text": replaceText,
		"task_id":      tid,
	}
}

func buildResolvedButtonCard(a *model.Action, opts DecisionCardOptions) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(opts.TaskID, a.ID))
	sourceDesc, statusText, btnText, mainDesc, chosen, replaceText := resolveCardSummaryFields(a, opts)
	title, _, body, _ := shapeCardCopy(a, len(a.Choices))

	expired := a.Status == model.ActionExpired
	snoozed := a.Status == model.ActionSnoozed
	isEsc := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "esc")
	isCustom := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "custom")
	channelName := formatResolvedChannel(firstNonEmpty(a.ResolvedChannel, a.ResolvedBy))

	horiz := []map[string]any{
		{"keyname": "状态", "value": statusText},
	}
	if snoozed && a.SnoozeUntil != nil {
		horiz = append(horiz, map[string]any{
			"keyname": "再次提醒",
			"value":   a.SnoozeUntil.Format("01-02 15:04"),
		})
	}
	if !snoozed && !expired && chosen != "" {
		keyName := "已选择"
		if isCustom {
			keyName = "自定义回答"
		} else if isEsc {
			keyName = "操作结果"
		}
		horiz = append(horiz, map[string]any{
			"keyname": keyName,
			"value":   clipRunes(chosen, cardHorizValueMaxRunes),
		})
	}
	if channelName != "" && !expired && !snoozed {
		horiz = append(horiz, map[string]any{
			"keyname": "处理渠道",
			"value":   clipRunes(channelName, cardHorizValueMaxRunes),
		})
	}
	if w := collapseSpaces(a.TargetWorker); w != "" && len(horiz) < cardHorizMaxItems {
		horiz = append(horiz, map[string]any{
			"keyname": "执行会话",
			"value":   clipRunes(w, cardHorizValueMaxRunes),
		})
	}
	if opts.RemainingAfter > 0 && len(horiz) < cardHorizMaxItems {
		horiz = append(horiz, map[string]any{
			"keyname": "剩余待拍板",
			"value":   fmt.Sprintf("还有 %d 项 · 发 /d 查看", opts.RemainingAfter),
		})
	}

	card := map[string]any{
		"card_type": "button_interaction",
		"source": map[string]any{
			"desc":       sourceDesc,
			"desc_color": 1,
		},
		"main_title": map[string]any{
			"title": title,
			"desc":  clipRunes(mainDesc, cardMainDescMaxRunes),
		},
		"horizontal_content_list": horiz,
		"task_id":                 tid,
		"replace_text":            replaceText,
		"button_list": []map[string]any{
			{
				"text":  btnText,
				"style": 2,
				"key":   tid + ":resolved",
			},
		},
	}
	if body != "" {
		card["sub_title_text"] = body
	}
	if len([]rune(chosen)) > cardHorizValueMaxRunes {
		card["quote_area"] = map[string]any{
			"type":       0,
			"title":      "已选方案",
			"quote_text": clipRunes(chosen, cardQuoteTextMaxRunes),
		}
	} else if cmd := strings.TrimSpace(a.Command); cmd != "" {
		card["quote_area"] = map[string]any{
			"type":       0,
			"title":      "关联命令",
			"quote_text": clipRunes(cmd, cardQuoteTextMaxRunes),
		}
	}
	return card
}

func resolveCardSummaryFields(a *model.Action, opts DecisionCardOptions) (sourceDesc, statusText, btnText, mainDesc, chosen, replaceText string) {
	expired := a.Status == model.ActionExpired
	snoozed := a.Status == model.ActionSnoozed
	isEsc := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "esc")
	isAck := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "ack")

	sourceDesc = "Corgi · 已拍板"
	statusText = "已拍板"
	btnText = "已拍板"
	switch {
	case snoozed:
		sourceDesc = "Corgi · 已暂缓"
		statusText = "已暂缓"
		btnText = "已暂缓"
	case expired:
		if a.ResolvedBy == "dismiss" {
			sourceDesc = "Corgi · 已关闭"
			statusText = "已关闭"
			btnText = "已关闭"
		} else {
			sourceDesc = "Corgi · 已失效"
			statusText = "已失效"
			btnText = "已失效"
		}
	case isEsc:
		sourceDesc = "Corgi · 已取消"
		statusText = "已取消"
		btnText = "已取消"
	case isAck:
		sourceDesc = "Corgi · 已确认"
		statusText = "已看"
		btnText = "已看"
	}

	channelName := formatResolvedChannel(firstNonEmpty(a.ResolvedChannel, a.ResolvedBy))
	mainDesc = statusText
	switch {
	case snoozed:
		if a.SnoozeUntil != nil {
			mainDesc = "已暂缓 · " + a.SnoozeUntil.Format("01-02 15:04") + " 再提醒"
		} else {
			mainDesc = "已暂缓提醒"
		}
	case expired:
		mainDesc = statusText + " · 无需再处理"
	case channelName != "":
		mainDesc = statusText + " · " + channelName
	}
	if opts.RemainingAfter == 0 && opts.QueueTotal > 1 {
		mainDesc = statusText + " · 待拍板已清空"
	}

	chosen = collapseSpaces(firstNonEmpty(a.ChosenLabel, a.ChosenKey))
	replaceText = statusText
	switch {
	case snoozed && a.SnoozeUntil != nil:
		replaceText = clipRunes("暂缓至 "+a.SnoozeUntil.Format("01-02 15:04"), cardReplaceTextMaxRunes)
	case !expired && !snoozed && !isEsc && !isAck && chosen != "":
		replaceText = clipRunes("已选："+chosen, cardReplaceTextMaxRunes)
	}
	return sourceDesc, statusText, btnText, mainDesc, chosen, replaceText
}

func effectiveChoices(a *model.Action) []model.ActionChoice {
	if a == nil {
		return nil
	}
	choices := a.Choices
	if len(choices) == 0 && a.Kind == model.ActionKindRouterCheck {
		choices = []model.ActionChoice{{Key: "ack", Label: "已看"}}
	}
	return choices
}

func choiceKey(ch model.ActionChoice, idx int) string {
	if k := strings.TrimSpace(ch.Key); k != "" {
		return k
	}
	return strconv.Itoa(idx + 1)
}

func formatVoteOptionText(ch model.ActionChoice) string {
	label := collapseSpaces(stripMarkdownPlainText(ch.Label))
	if ch.Recommended && !strings.Contains(label, "推荐") {
		if len([]rune(label))+4 <= voteOptionMaxRunes {
			label += "（推荐）"
		}
	}
	return clipRunes(label, voteOptionMaxRunes)
}

func formatVoteDescLines(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return strings.TrimRightFunc(strings.Join(out, "\n"), isTrimEdgeRune)
}

func formatPendingSourceDesc(opts DecisionCardOptions) string {
	if opts.QueueTotal > 1 {
		if opts.QueuePos > 0 {
			return fmt.Sprintf("Corgi · 待拍板 %d/%d", opts.QueuePos, opts.QueueTotal)
		}
		return fmt.Sprintf("Corgi · 待拍板（共 %d 项）", opts.QueueTotal)
	}
	return "Corgi · 待拍板"
}

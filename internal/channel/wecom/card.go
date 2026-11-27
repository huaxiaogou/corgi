package wecom

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	cardMainTitleMaxRunes      = 26
	cardMainDescMaxRunes       = 30
	cardSubTitleMaxRunes       = 112
	cardHorizValueMaxRunes     = 30
	cardButtonTextMaxRunes     = 10
	cardButtonDirectMaxRunes   = 8
	cardCompactButtonThreshold = 3
	cardMaxButtons             = 6
	cardQuoteTextMaxRunes      = 280
	cardReplaceTextMaxRunes    = 16
	cardTaskIDMaxBytes         = 128
)

// BuildActionCard constructs a WeCom button_interaction template_card for a pending Action.
// It structures title, body, command, full option descriptions, and button labels within WeCom's
// field limits so long messages and choices stay readable instead of getting truncated.
func BuildActionCard(a *model.Action) map[string]any {
	if a == nil {
		a = &model.Action{ID: "action", Title: "待拍板决策"}
	}
	choices := a.Choices
	if len(choices) == 0 && a.Kind == model.ActionKindRouterCheck {
		choices = []model.ActionChoice{{Key: "ack", Label: "已看"}}
	}
	if len(choices) > cardMaxButtons {
		choices = choices[:cardMaxButtons]
	}

	compactButtons := shouldUseCompactButtons(choices)
	expandOptions := compactButtons || shouldExpandOptions(choices)
	buttons := make([]map[string]any, 0, len(choices))
	var horiz []map[string]any
	var longOptionLines []string

	for i, ch := range choices {
		style := 2 // secondary outline
		if i == 0 {
			style = 1 // primary highlight for recommended option
		}
		key := strings.TrimSpace(ch.Key)
		if key == "" {
			key = strconv.Itoa(i + 1)
		}
		btnText := formatButtonText(i+1, ch.Label, expandOptions)
		if compactButtons {
			btnText = formatCompactButtonText(i + 1)
		}
		buttons = append(buttons, map[string]any{
			"text":  btnText,
			"style": style,
			"key":   a.ID + ":" + key,
		})
		if expandOptions {
			optText := formatChoiceFullText(ch)
			horiz = append(horiz, map[string]any{
				"keyname": fmt.Sprintf("选项 %d", i+1),
				"value":   clipRunes(optText, cardHorizValueMaxRunes),
			})
			if len([]rune(optText)) > cardHorizValueMaxRunes {
				longOptionLines = append(longOptionLines, fmt.Sprintf("%d. %s", i+1, optText))
			}
		}
	}

	title, desc, body, overflow := shapeCardCopy(a, len(choices))
	sourceDesc := "Corgi · 待拍板"
	if a.Kind == model.ActionKindRouterCheck {
		sourceDesc = "Corgi · 审阅提醒"
	}
	card := map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": sourceDesc},
		"main_title": map[string]any{
			"title": title,
			"desc":  desc,
		},
		"task_id":     sanitizeTaskID(a.ID),
		"button_list": buttons,
	}
	if body != "" {
		card["sub_title_text"] = body
	}
	if len(horiz) > 0 {
		card["horizontal_content_list"] = horiz
	}
	if qa := buildQuoteArea(overflow, strings.TrimSpace(a.Command), longOptionLines); qa != nil {
		card["quote_area"] = qa
	}
	return card
}

// BuildResolvedActionCard constructs an in-place updated button_interaction template_card
// for a resolved or expired Action. Keeping card_type="button_interaction" with the matching
// task_id and replace_text satisfies WeCom's aibot_respond_update_msg schema without requiring
// an external card_action URL.
func BuildResolvedActionCard(a *model.Action, taskID string) map[string]any {
	if a == nil {
		a = &model.Action{ID: taskID, Status: model.ActionResolved}
	}
	tid := sanitizeTaskID(firstNonEmpty(taskID, a.ID))

	expired := a.Status == model.ActionExpired
	snoozed := a.Status == model.ActionSnoozed
	isEsc := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "esc")
	isCustom := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "custom")
	isAck := a.Status == model.ActionResolved && strings.EqualFold(strings.TrimSpace(a.ChosenKey), "ack")

	sourceDesc := "Corgi · 已拍板"
	statusText := "已拍板"
	btnText := "已拍板"
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

	title, _, body, _ := shapeCardCopy(a, len(a.Choices))
	channelName := formatResolvedChannel(a.ResolvedBy)
	mainDesc := statusText
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

	chosen := collapseSpaces(firstNonEmpty(a.ChosenLabel, a.ChosenKey))
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
	if w := collapseSpaces(a.TargetWorker); w != "" {
		horiz = append(horiz, map[string]any{
			"keyname": "执行会话",
			"value":   clipRunes(w, cardHorizValueMaxRunes),
		})
	}

	replaceText := statusText
	switch {
	case snoozed && a.SnoozeUntil != nil:
		replaceText = clipRunes("暂缓至 "+a.SnoozeUntil.Format("01-02 15:04"), cardReplaceTextMaxRunes)
	case !expired && !snoozed && !isEsc && !isAck && chosen != "":
		replaceText = clipRunes("已选："+chosen, cardReplaceTextMaxRunes)
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

// SendActionCard proactively pushes an interactive decision card via aibot_send_msg and checks its ACK.
func (c *Client) SendActionCard(ctx context.Context, chatID string, a *model.Action) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.callRPC(sctx, "aibot_send_msg", map[string]any{
		"chatid":        chatID,
		"msgtype":       "template_card",
		"template_card": BuildActionCard(a),
	})
	if err != nil {
		return fmt.Errorf("send wecom action card %s: %w", actionID(a), err)
	}
	return nil
}

// UpdateCardResolved refreshes an interacted template card in-place using aibot_respond_update_msg.
func (c *Client) UpdateCardResolved(ctx context.Context, reqID string, a *model.Action) error {
	return c.UpdateCardResolvedWithTask(ctx, reqID, actionID(a), a)
}

// UpdateCardResolvedWithTask refreshes an interacted template card in-place, preserving the exact
// task_id echoed in the callback event and verifying WeCom's ACK response.
func (c *Client) UpdateCardResolvedWithTask(ctx context.Context, reqID, taskID string, a *model.Action) error {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.callRPCWithReqID(uctx, reqID, "aibot_respond_update_msg", map[string]any{
		"response_type": "update_template_card",
		"template_card": BuildResolvedActionCard(a, taskID),
	})
	if err != nil {
		return fmt.Errorf("update wecom template card %s: %w", firstNonEmpty(taskID, actionID(a)), err)
	}
	return nil
}

// WorkerPickOption describes one selectable matter/worker shown in WeCom's switch-conversation card.
type WorkerPickOption struct {
	Name        string
	Title       string
	Machine     string
	Kind        string
	Status      string
	HasWorkItem bool
	Ended       bool
}

// BuildWorkerPickerCard builds a clean, paginated WeCom button_interaction card for switching
// into a Worker direct chat or returning to Corgi main chat. Rows use compact numeric keys
// ("1.".."N.") paired with full Chinese matter titles, and buttons use crisp "选 1".."选 N"
// labels + in-place pagination ("上一页"/"下一页") so options never truncate or drop off.
func BuildWorkerPickerCard(taskID, currentWorker, currentTitle string, opts []WorkerPickOption, page int) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(taskID, fmt.Sprintf("nav:pick:%d", time.Now().UnixNano())))
	currentWorker = strings.TrimSpace(currentWorker)
	currentTitle = strings.TrimSpace(currentTitle)

	// Calculate page windows respecting WeCom's 6-button max and <=5 readable rows per card.
	hasBackMain := currentWorker != ""
	type pageSlice struct {
		start int
		end   int
	}
	var pages []pageSlice
	if len(opts) == 0 {
		pages = []pageSlice{{0, 0}}
	} else {
		// Fit all items on 1 page if they fit within available button slots (max 5 rows for clean mobile height).
		singleMax := 5
		if hasBackMain && singleMax > cardMaxButtons-1 {
			singleMax = cardMaxButtons - 1
		}
		if len(opts) <= singleMax {
			pages = []pageSlice{{0, len(opts)}}
		} else {
			pos := 0
			for pos < len(opts) {
				isFirst := pos == 0
				// Reserve 1 slot for 回主对话 if focused, 1 for 上一页 if !isFirst, and 1 for 下一页 if more remain.
				fixedNav := 0
				if hasBackMain {
					fixedNav++
				}
				if !isFirst {
					fixedNav++
				}
				capWithoutNext := cardMaxButtons - fixedNav
				if capWithoutNext > 5 {
					capWithoutNext = 5
				}
				if pos+capWithoutNext >= len(opts) {
					pages = append(pages, pageSlice{pos, len(opts)})
					break
				}
				capWithNext := cardMaxButtons - fixedNav - 1
				if capWithNext > 5 {
					capWithNext = 5
				}
				if capWithNext < 1 {
					capWithNext = 1
				}
				pages = append(pages, pageSlice{pos, pos + capWithNext})
				pos += capWithNext
			}
		}
	}

	if page < 0 {
		page = 0
	}
	if page >= len(pages) {
		page = len(pages) - 1
	}
	win := pages[page]
	pageOpts := opts[win.start:win.end]

	curLabel := "Corgi 主对话"
	if currentWorker != "" {
		curLabel = firstNonEmpty(currentTitle, "#"+currentWorker)
	}
	var desc string
	if len(pages) > 1 {
		desc = fmt.Sprintf("当前：%s · %d/%d页", clipRunes(curLabel, 14), page+1, len(pages))
	} else {
		desc = fmt.Sprintf("当前：%s · 共%d项", clipRunes(curLabel, 16), len(opts))
	}

	buttons := make([]map[string]any, 0, cardMaxButtons)
	horiz := make([]map[string]any, 0, len(pageOpts))

	if hasBackMain {
		buttons = append(buttons, map[string]any{
			"text":  "回主对话",
			"style": 1,
			"key":   "nav:main",
		})
	}
	for i, opt := range pageOpts {
		num := win.start + i + 1
		style := 2
		if !hasBackMain && i == 0 && page == 0 {
			style = 1
		}
		buttons = append(buttons, map[string]any{
			"text":  fmt.Sprintf("选 %d", num),
			"style": style,
			"key":   "nav:focus:" + opt.Name,
		})
		label := strings.TrimSpace(opt.Title)
		if label == "" {
			label = opt.Name
		}
		suffix := ""
		switch {
		case opt.Ended:
			suffix = " · 可恢复"
		case opt.Status == "working":
			suffix = " · 思考中"
		case opt.Status == "blocked":
			suffix = " · 待确认"
		}
		val := clipRunes(label, cardHorizValueMaxRunes-len([]rune(suffix))) + suffix
		horiz = append(horiz, map[string]any{
			"keyname": fmt.Sprintf("%d.", num),
			"value":   val,
		})
	}
	if page > 0 {
		buttons = append(buttons, map[string]any{
			"text":  "← 上一页",
			"style": 2,
			"key":   fmt.Sprintf("nav:page:%d", page-1),
		})
	}
	if page+1 < len(pages) {
		buttons = append(buttons, map[string]any{
			"text":  "下一页 →",
			"style": 2,
			"key":   fmt.Sprintf("nav:page:%d", page+1),
		})
	}

	card := map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": "Corgi · 切换对话"},
		"main_title": map[string]any{
			"title": "选择要直聊的事项 / 会话",
			"desc":  clipRunes(desc, cardMainDescMaxRunes),
		},
		"task_id":     tid,
		"button_list": buttons,
	}
	if len(horiz) > 0 {
		card["horizontal_content_list"] = horiz
	}
	return card
}

// BuildNavResultCard collapses an interacted navigation picker card in-place.
func BuildNavResultCard(taskID, title, statusText, detail string) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(taskID, "nav:done"))
	card := map[string]any{
		"card_type": "button_interaction",
		"source": map[string]any{
			"desc":       "Corgi · 切换对话",
			"desc_color": 1,
		},
		"main_title": map[string]any{
			"title": clipRunes(firstNonEmpty(title, "对话对象已切换"), cardMainTitleMaxRunes),
			"desc":  clipRunes(statusText, cardMainDescMaxRunes),
		},
		"task_id":      tid,
		"replace_text": clipRunes(statusText, cardReplaceTextMaxRunes),
		"button_list": []map[string]any{
			{
				"text":  clipRunes(firstNonEmpty(statusText, "已切换"), cardButtonTextMaxRunes),
				"style": 2,
				"key":   tid + ":done",
			},
		},
	}
	if strings.TrimSpace(detail) != "" {
		card["sub_title_text"] = clipRunes(detail, cardSubTitleMaxRunes)
	}
	return card
}

// SendWorkerPickerCard sends an interactive paginated Worker/matter selection card to chatID.
func (c *Client) SendWorkerPickerCard(ctx context.Context, chatID, currentWorker, currentTitle string, opts []WorkerPickOption, page int) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	taskID := fmt.Sprintf("nav:pick:%d", time.Now().UnixNano())
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.callRPC(sctx, "aibot_send_msg", map[string]any{
		"chatid":        chatID,
		"msgtype":       "template_card",
		"template_card": BuildWorkerPickerCard(taskID, currentWorker, currentTitle, opts, page),
	})
	if err != nil {
		return fmt.Errorf("send wecom worker picker card: %w", err)
	}
	return nil
}

// UpdateWorkerPickerCard replaces an existing Worker picker card in-place for pagination.
func (c *Client) UpdateWorkerPickerCard(ctx context.Context, reqID, taskID, currentWorker, currentTitle string, opts []WorkerPickOption, page int) error {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.callRPCWithReqID(uctx, reqID, "aibot_respond_update_msg", map[string]any{
		"response_type": "update_template_card",
		"template_card": BuildWorkerPickerCard(taskID, currentWorker, currentTitle, opts, page),
	})
	if err != nil {
		return fmt.Errorf("update wecom worker picker card %s: %w", taskID, err)
	}
	return nil
}

// BuildDirectFocusCard renders an interactive card with live action buttons (no replace_text lock)
// shown right after entering direct chat so the human can return to Corgi or switch with one tap.
func BuildDirectFocusCard(taskID, title, workerName, detail string) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(taskID, fmt.Sprintf("nav:focus:%d", time.Now().UnixNano())))
	handle := strings.TrimSpace(workerName)
	desc := "直聊中"
	if handle != "" {
		desc = "直聊中 · #" + handle
	}
	card := map[string]any{
		"card_type": "button_interaction",
		"source": map[string]any{
			"desc":       "Corgi · 直聊中",
			"desc_color": 1,
		},
		"main_title": map[string]any{
			"title": clipRunes(firstNonEmpty(title, "#"+handle, "直聊会话"), cardMainTitleMaxRunes),
			"desc":  clipRunes(desc, cardMainDescMaxRunes),
		},
		"task_id": tid,
		"button_list": []map[string]any{
			{
				"text":  "返回 Corgi 主对话",
				"style": 1,
				"key":   "nav:main",
			},
			{
				"text":  "切换其他",
				"style": 2,
				"key":   "nav:picker",
			},
		},
	}
	if strings.TrimSpace(detail) != "" {
		card["sub_title_text"] = clipRunes(detail, cardSubTitleMaxRunes)
	}
	return card
}

// SendDirectFocusCard pushes a live direct-chat control card with one-tap return/switch buttons.
func (c *Client) SendDirectFocusCard(ctx context.Context, chatID, title, workerName, detail string) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	taskID := fmt.Sprintf("nav:focus:%d", time.Now().UnixNano())
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.callRPC(sctx, "aibot_send_msg", map[string]any{
		"chatid":        chatID,
		"msgtype":       "template_card",
		"template_card": BuildDirectFocusCard(taskID, title, workerName, detail),
	})
	if err != nil {
		return fmt.Errorf("send wecom direct focus card: %w", err)
	}
	return nil
}

// UpdateDirectFocusCard updates a picker card in-place into a live direct-chat control card.
func (c *Client) UpdateDirectFocusCard(ctx context.Context, reqID, taskID, title, workerName, detail string) error {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.callRPCWithReqID(uctx, reqID, "aibot_respond_update_msg", map[string]any{
		"response_type": "update_template_card",
		"template_card": BuildDirectFocusCard(taskID, title, workerName, detail),
	})
	if err != nil {
		return fmt.Errorf("update wecom direct focus card %s: %w", taskID, err)
	}
	return nil
}

// UpdateNavCard updates a navigation template card in-place after click.
func (c *Client) UpdateNavCard(ctx context.Context, reqID, taskID, title, statusText, detail string) error {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.callRPCWithReqID(uctx, reqID, "aibot_respond_update_msg", map[string]any{
		"response_type": "update_template_card",
		"template_card": BuildNavResultCard(taskID, title, statusText, detail),
	})
	if err != nil {
		return fmt.Errorf("update wecom nav card %s: %w", taskID, err)
	}
	return nil
}

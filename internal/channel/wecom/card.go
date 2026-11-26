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
	card := map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": "Corgi · 待拍板"},
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
	case !expired && !snoozed && !isEsc && chosen != "":
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

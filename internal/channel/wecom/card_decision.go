package wecom

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	// DecisionFoldLimit matches Scheme 3: show first 4 choices inline before offering "+ 展开其余 N 个选项".
	DecisionFoldLimit    = 4
	voteOptionMaxRunes   = 17
	voteMainDescMaxRunes = 140
	voteMaxOptions       = 20
	cardHorizMaxItems    = 6

	// Sentinel keys/IDs for WeCom in-place decision card interactions.
	SentinelMore           = "__more__"
	SentinelFold           = "__fold__"
	SentinelSnooze         = "__snooze__"
	SentinelBack           = "__back__"
	SentinelSnooze1H       = "__snooze_1h__"
	SentinelSnoozeTomorrow = "__snooze_tmr__"
	SentinelDismiss        = "__dismiss__"
)

// DecisionCardOptions carries dynamic pending-count metadata, inline expand state, preselected keys,
// and snooze toggle when constructing a native WeCom Scheme 3 decision card.
type DecisionCardOptions struct {
	TaskID         string
	QueuePos       int      // 1-based position among pending decisions (<=0 omits position)
	QueueTotal     int      // total pending real decisions in thread (<=1 omits counter)
	Expanded       bool     // true after user expands >4 options inline
	Preselected    []string // choice keys preserved across inline expand/fold
	ConsoleURL     string   // optional authenticated Web console URL (shown on button_interaction when configured)
	AllowSnooze    bool     // offer "稍后处理" affordance (false for live terminal worker_dialog)
	RemainingAfter int      // remaining pending decisions after settling (shown on receipt)
}

// BuildDecisionCard constructs a Scheme 3 WeCom template_card for a pending Action:
//   - 1-2 option single-select -> native button_interaction for fast 1-tap decision (+ 稍后处理).
//   - >2 option single-select OR multi-select -> native vote_interaction (radio/checkbox + explicit submit),
//     folding >4 options inline with "+ 展开其余 N 个选项" (zero nested scroll, zero silent tail truncation).
func BuildDecisionCard(a *model.Action, opts DecisionCardOptions) map[string]any {
	if a == nil {
		a = &model.Action{ID: "action", Title: "待拍板决策"}
	}
	choices := effectiveChoices(a)
	if !a.IsMulti() && len(choices) <= 2 {
		return buildFastButtonDecisionCard(a, choices, opts)
	}
	return buildVoteDecisionCard(a, choices, opts)
}

// BuildSnoozeCard constructs an in-place defer picker preserving the original card_type so
// aibot_respond_update_msg stays schema-safe across both button_interaction and vote_interaction.
func BuildSnoozeCard(a *model.Action, cardType string, opts DecisionCardOptions) map[string]any {
	if a == nil {
		a = &model.Action{ID: "action", Title: "待拍板决策"}
	}
	tid := sanitizeTaskID(firstNonEmpty(opts.TaskID, a.ID))
	title, _, _, _ := shapeCardCopy(a, len(a.Choices))

	if cardType == "vote_interaction" {
		return map[string]any{
			"card_type": "vote_interaction",
			"source":    map[string]any{"desc": formatPendingSourceDesc(opts)},
			"main_title": map[string]any{
				"title": clipRunes("稍后处理 · "+title, cardMainTitleMaxRunes),
				"desc":  "选择提醒时间或暂不处理，原卡将变为已暂缓回执",
			},
			"checkbox": map[string]any{
				"question_key": sanitizeTaskID(a.ID),
				"mode":         0,
				"option_list": []map[string]any{
					{"id": SentinelSnooze1H, "text": "1 小时后再提醒", "is_checked": true},
					{"id": SentinelSnoozeTomorrow, "text": "明天 09:00 再提醒", "is_checked": false},
					{"id": SentinelDismiss, "text": "暂不处理（跳过）", "is_checked": false},
					{"id": SentinelBack, "text": "← 返回原选项", "is_checked": false},
				},
			},
			"submit_button": map[string]any{
				"text": "确认",
				"key":  a.ID + ":snooze_apply",
			},
			"task_id": tid,
		}
	}

	return map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": "Corgi · 稍后处理"},
		"main_title": map[string]any{
			"title": title,
			"desc":  "选择提醒时间或返回继续拍板",
		},
		"sub_title_text": "暂缓后本卡将原位更新为回执，发 /d 可随时查看待拍板清单。",
		"task_id":        tid,
		"button_list": []map[string]any{
			{"text": "1小时后提醒", "style": 1, "key": a.ID + ":snooze:1h"},
			{"text": "明天09:00", "style": 2, "key": a.ID + ":snooze:tmr"},
			{"text": "暂不处理", "style": 2, "key": a.ID + ":dismiss"},
			{"text": "← 返回", "style": 2, "key": a.ID + ":back"},
		},
	}
}

func buildFastButtonDecisionCard(a *model.Action, choices []model.ActionChoice, opts DecisionCardOptions) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(opts.TaskID, a.ID))
	expandOptions := shouldExpandOptions(choices)
	buttons := make([]map[string]any, 0, len(choices)+1)
	var horiz []map[string]any
	var longOptionLines []string

	for i, ch := range choices {
		style := 2
		if i == 0 {
			style = 1
		}
		key := choiceKey(ch, i)
		buttons = append(buttons, map[string]any{
			"text":  formatButtonText(i+1, ch.Label, expandOptions),
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
	if opts.AllowSnooze && a.Kind != model.ActionKindWorkerDialog && len(buttons) < cardMaxButtons {
		buttons = append(buttons, map[string]any{
			"text":  "稍后处理",
			"style": 2,
			"key":   a.ID + ":snooze_menu",
		})
	}
	if strings.TrimSpace(opts.ConsoleURL) != "" && len(horiz) < cardHorizMaxItems {
		horiz = append(horiz, map[string]any{
			"type":    1,
			"keyname": "完整说明",
			"value":   "打开控制台查看 ↗",
			"url":     strings.TrimSpace(opts.ConsoleURL),
		})
	}

	title, desc, body, overflow := shapeCardCopy(a, len(choices))
	if opts.QueueTotal > 1 && strings.TrimSpace(a.TargetWorker) == "" && len(choices) == 2 {
		desc = clipRunes(fmt.Sprintf("2 项快拍 · 当前共 %d 项待拍板", opts.QueueTotal), cardMainDescMaxRunes)
	} else if len(choices) == 2 && strings.TrimSpace(a.TargetWorker) == "" {
		desc = "2 项单选 · 点击即拍板"
	}

	card := map[string]any{
		"card_type": "button_interaction",
		"source":    map[string]any{"desc": formatPendingSourceDesc(opts)},
		"main_title": map[string]any{
			"title": title,
			"desc":  clipRunes(desc, cardMainDescMaxRunes),
		},
		"task_id":     tid,
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

func buildVoteDecisionCard(a *model.Action, choices []model.ActionChoice, opts DecisionCardOptions) map[string]any {
	tid := sanitizeTaskID(firstNonEmpty(opts.TaskID, a.ID))
	isMulti := a.IsMulti()
	mode := 0
	if isMulti {
		mode = 1
	}

	reservedUtility := 0
	if opts.AllowSnooze && a.Kind != model.ActionKindWorkerDialog {
		reservedUtility++
	}
	hasOverflow := len(choices) > DecisionFoldLimit
	if hasOverflow {
		reservedUtility++
	}
	maxVisibleChoices := max(voteMaxOptions-reservedUtility, 1)

	visible := choices
	hiddenCount := 0
	hiddenSelected := 0
	overflowOmitted := 0
	if hasOverflow && !opts.Expanded {
		visible = choices[:DecisionFoldLimit]
		hidden := choices[DecisionFoldLimit:]
		hiddenCount = len(hidden)
		for i, ch := range hidden {
			if slices.Contains(opts.Preselected, choiceKey(ch, DecisionFoldLimit+i)) {
				hiddenSelected++
			}
		}
	} else if len(visible) > maxVisibleChoices {
		overflowOmitted = len(visible) - maxVisibleChoices
		visible = visible[:maxVisibleChoices]
	}

	optList := make([]map[string]any, 0, len(visible)+reservedUtility)
	for i, ch := range visible {
		k := choiceKey(ch, i)
		optList = append(optList, map[string]any{
			"id":         k,
			"text":       formatVoteOptionText(ch),
			"is_checked": slices.Contains(opts.Preselected, k),
		})
	}
	if hasOverflow && !opts.Expanded {
		moreText := fmt.Sprintf("+ 展开其余 %d 个选项", hiddenCount)
		if hiddenSelected > 0 {
			moreText = fmt.Sprintf("+ 展开其余 %d 项（已选%d）", hiddenCount, hiddenSelected)
		}
		optList = append(optList, map[string]any{
			"id":         SentinelMore,
			"text":       clipRunes(moreText, voteOptionMaxRunes),
			"is_checked": false,
		})
	} else if hasOverflow && opts.Expanded {
		optList = append(optList, map[string]any{
			"id":         SentinelFold,
			"text":       "▴ 收起至前 4 项",
			"is_checked": false,
		})
	}
	if opts.AllowSnooze && a.Kind != model.ActionKindWorkerDialog {
		optList = append(optList, map[string]any{
			"id":         SentinelSnooze,
			"text":       "◷ 稍后处理",
			"is_checked": false,
		})
	}

	title, _, body, _ := shapeCardCopy(a, len(choices))
	var descParts []string
	if isMulti {
		descParts = append(descParts, fmt.Sprintf("多选（%d 项）· 勾选后点下方确认", len(choices)))
	} else {
		descParts = append(descParts, fmt.Sprintf("单选（%d 项）· 选定后点下方确认", len(choices)))
	}
	if overflowOmitted > 0 {
		descParts = append(descParts, fmt.Sprintf("另有 %d 项请前往 Web 控制台选择", overflowOmitted))
	}
	if w := strings.TrimSpace(a.TargetWorker); w != "" {
		descParts = append(descParts, "会话："+w)
	}
	if body != "" {
		descParts = append(descParts, collapseSpaces(body))
	}
	if cmd := strings.TrimSpace(a.Command); cmd != "" {
		descParts = append(descParts, "命令："+collapseSpaces(cmd))
	}
	submitText := clipRunes(firstNonEmpty(a.ConfirmLabel, "确认提交"), cardButtonTextMaxRunes)

	return map[string]any{
		"card_type": "vote_interaction",
		"source":    map[string]any{"desc": formatPendingSourceDesc(opts)},
		"main_title": map[string]any{
			"title": title,
			"desc":  clipRunes(strings.Join(descParts, " · "), voteMainDescMaxRunes),
		},
		"checkbox": map[string]any{
			"question_key": sanitizeTaskID(a.ID),
			"mode":         mode,
			"option_list":  optList,
		},
		"submit_button": map[string]any{
			"text": submitText,
			"key":  a.ID + ":submit",
		},
		"task_id": tid,
	}
}

// SendDecisionCard pushes a Scheme 3 interactive decision card via aibot_send_msg and checks its ACK.
func (c *Client) SendDecisionCard(ctx context.Context, chatID string, a *model.Action, opts DecisionCardOptions) (string, error) {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return "", nil
	}
	card := BuildDecisionCard(a, opts)
	cardType, _ := card["card_type"].(string)

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.callRPC(sctx, "aibot_send_msg", map[string]any{
		"chatid":        chatID,
		"msgtype":       "template_card",
		"template_card": card,
	})
	if err != nil {
		return "", fmt.Errorf("send wecom decision card %s: %w", actionID(a), err)
	}
	return cardType, nil
}

// UpdateTemplateCard updates an interacted template_card in-place using aibot_respond_update_msg.
func (c *Client) UpdateTemplateCard(ctx context.Context, reqID string, card map[string]any) error {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" || len(card) == 0 {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.callRPCWithReqID(uctx, reqID, "aibot_respond_update_msg", map[string]any{
		"response_type": "update_template_card",
		"template_card": card,
	})
	if err != nil {
		tid, _ := card["task_id"].(string)
		return fmt.Errorf("update wecom template card %s: %w", tid, err)
	}
	return nil
}

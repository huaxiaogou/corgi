package wecom

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestBuildDecisionCardScheme3RoutingAndFolding(t *testing.T) {
	// 1. 1-2 option single-select -> button_interaction fast 1-tap (+ 稍后处理).
	fast := &model.Action{
		ID:      "a10",
		Kind:    model.ActionKindRouterChoice,
		Title:   "是否发布 v0.1.64？",
		Summary: "测试全部通过，是否现在发布？",
		Choices: []model.ActionChoice{
			{Key: "1", Label: "立即发布"},
			{Key: "2", Label: "暂缓"},
		},
	}
	card := BuildDecisionCard(fast, DecisionCardOptions{AllowSnooze: true, QueuePos: 1, QueueTotal: 3})
	if card["card_type"] != "button_interaction" {
		t.Fatalf("expected button_interaction for 2-option single, got %v", card["card_type"])
	}
	btns := card["button_list"].([]map[string]any)
	if len(btns) != 3 || btns[2]["key"] != "a10:snooze_menu" {
		t.Fatalf("expected 2 choices + snooze_menu button, got %+v", btns)
	}

	// 2. worker_dialog omits snooze button.
	dlg := &model.Action{
		ID:      "a11",
		Kind:    model.ActionKindWorkerDialog,
		Title:   "确认运行命令",
		Choices: []model.ActionChoice{{Key: "1", Label: "允许"}, {Key: "2", Label: "拒绝"}},
	}
	dlgCard := BuildDecisionCard(dlg, DecisionCardOptions{AllowSnooze: true})
	if len(dlgCard["button_list"].([]map[string]any)) != 2 {
		t.Fatalf("worker_dialog must not render snooze button: %+v", dlgCard["button_list"])
	}

	// 3. >2 option single-select -> vote_interaction (mode=0).
	threeSingle := &model.Action{
		ID:      "a12",
		Kind:    model.ActionKindRouterChoice,
		Title:   "选择发布策略",
		Choices: []model.ActionChoice{{Key: "1", Label: "蓝绿", Recommended: true}, {Key: "2", Label: "灰度"}, {Key: "3", Label: "暂缓"}},
	}
	voteSingle := BuildDecisionCard(threeSingle, DecisionCardOptions{AllowSnooze: true})
	if voteSingle["card_type"] != "vote_interaction" {
		t.Fatalf("expected vote_interaction for 3-option single, got %v", voteSingle["card_type"])
	}
	cb := voteSingle["checkbox"].(map[string]any)
	if cb["mode"] != 0 {
		t.Fatalf("expected single-select mode=0, got %v", cb["mode"])
	}

	// 4. Multi-select with 16 choices folds at 4 and expands all 16 without truncation.
	choices16 := make([]model.ActionChoice, 16)
	for i := range choices16 {
		choices16[i] = model.ActionChoice{Key: fmt.Sprintf("%d", i+1), Label: fmt.Sprintf("检查项%d", i+1)}
	}
	multi16 := &model.Action{
		ID:           "a16",
		Kind:         model.ActionKindRouterChoice,
		Mode:         model.ActionModeMulti,
		ConfirmLabel: "提交勾选",
		Title:        "发布前检查清单",
		Choices:      choices16,
	}
	folded := BuildDecisionCard(multi16, DecisionCardOptions{
		AllowSnooze: true,
		Expanded:    false,
		Preselected: []string{"1", "10", "16"},
	})
	fcb := folded["checkbox"].(map[string]any)
	if fcb["mode"] != 1 {
		t.Fatalf("expected multi-select mode=1, got %v", fcb["mode"])
	}
	fOpts := fcb["option_list"].([]map[string]any)
	// 4 visible + SentinelMore + SentinelSnooze = 6
	if len(fOpts) != 6 || fOpts[4]["id"] != SentinelMore || !strings.Contains(fOpts[4]["text"].(string), "已选2") {
		t.Fatalf("unexpected folded options: %+v", fOpts)
	}
	subBtn := folded["submit_button"].(map[string]any)
	if subBtn["text"] != "提交勾选" {
		t.Fatalf("expected custom submit label, got %+v", subBtn)
	}

	expanded := BuildDecisionCard(multi16, DecisionCardOptions{
		AllowSnooze: true,
		Expanded:    true,
		Preselected: []string{"1", "10", "16"},
	})
	eOpts := expanded["checkbox"].(map[string]any)["option_list"].([]map[string]any)
	// 16 real + SentinelFold + SentinelSnooze = 18 <= 20 (zero truncation!)
	if len(eOpts) != 18 || eOpts[15]["id"] != "16" || eOpts[15]["is_checked"] != true || eOpts[16]["id"] != SentinelFold {
		t.Fatalf("expected all 16 choices + fold + snooze without truncation, got len=%d tail=%+v", len(eOpts), eOpts[14:])
	}
}

func TestBuildSnoozeAndResolvedDecisionCardsNoUndo(t *testing.T) {
	now := time.Now()
	act := &model.Action{
		ID:              "a20",
		Kind:            model.ActionKindRouterChoice,
		Mode:            model.ActionModeMulti,
		Title:           "检查项",
		Status:          model.ActionResolved,
		ResolvedBy:      "wecom",
		ResolvedChannel: "wecom",
		ChosenKey:       "1,3",
		ChosenKeys:      []string{"1", "3"},
		ChosenLabel:     "已选 2 项：A、C",
		ResolvedAt:      &now,
		Choices: []model.ActionChoice{
			{Key: "1", Label: "A"},
			{Key: "2", Label: "B"},
			{Key: "3", Label: "C"},
		},
	}

	snoozeVote := BuildSnoozeCard(act, "vote_interaction", DecisionCardOptions{TaskID: "a20"})
	if snoozeVote["card_type"] != "vote_interaction" {
		t.Fatalf("expected vote_interaction snooze card, got %v", snoozeVote["card_type"])
	}
	snoozeBtn := BuildSnoozeCard(act, "button_interaction", DecisionCardOptions{TaskID: "a20"})
	if snoozeBtn["card_type"] != "button_interaction" {
		t.Fatalf("expected button_interaction snooze card, got %v", snoozeBtn["card_type"])
	}

	voteReceipt := BuildResolvedDecisionCard(act, "vote_interaction", DecisionCardOptions{TaskID: "a20", RemainingAfter: 2})
	if voteReceipt["card_type"] != "vote_interaction" || voteReceipt["replace_text"] == "" {
		t.Fatalf("unexpected vote receipt: %+v", voteReceipt)
	}
	rawVote, _ := json.Marshal(voteReceipt)
	if strings.Contains(string(rawVote), "undo") || strings.Contains(string(rawVote), "撤销") {
		t.Fatalf("vote receipt must not contain undo affordance: %s", rawVote)
	}
	if cb := voteReceipt["checkbox"].(map[string]any); cb["disable"] != true {
		t.Fatalf("vote receipt checkbox must be disabled: %+v", cb)
	}

	btnReceipt := BuildResolvedDecisionCard(act, "button_interaction", DecisionCardOptions{TaskID: "a20"})
	rawBtn, _ := json.Marshal(btnReceipt)
	if btnReceipt["replace_text"] == "" || strings.Contains(string(rawBtn), "undo") || strings.Contains(string(rawBtn), "撤销") {
		t.Fatalf("button receipt must be locked with replace_text and no undo: %s", rawBtn)
	}
}

func TestParseVoteInteractionSelectedItemsFormats(t *testing.T) {
	// Format A: snake_case array
	rawA := json.RawMessage(`{
		"chatid": "u1",
		"chattype": "single",
		"from": {"userid": "u1"},
		"event": {
			"eventtype": "template_card_event",
			"template_card_event": {
				"card_type": "vote_interaction",
				"task_id": "a99",
				"event_key": "a99:submit",
				"selected_items": [{"question_key": "a99", "option_ids": ["1", "3"]}]
			}
		}
	}`)
	evA, ok := ParseCardEventCallback("req-1", rawA)
	if !ok || evA.CardType != "vote_interaction" || evA.ActionID != "a99" || len(evA.SelectedKeys) != 2 || evA.SelectedKeys[0] != "1" || evA.SelectedKeys[1] != "3" {
		t.Fatalf("unexpected vote event A: %+v ok=%v", evA, ok)
	}

	// Format B: PascalCase nested wrapper
	rawB := json.RawMessage(`{
		"chatid": "u1",
		"from": {"userid": "u1"},
		"event": {
			"EventType": "template_card_event",
			"CardType": "vote_interaction",
			"TaskId": "a99_r2",
			"EventKey": "a99:submit",
			"SelectedItems": {"SelectedItem": [{"QuestionKey": "a99", "OptionIds": {"OptionId": ["2", "__more__"]}}]}
		}
	}`)
	evB, ok := ParseCardEventCallback("req-2", rawB)
	if !ok || evB.ActionID != "a99" || evB.TaskID != "a99_r2" || len(evB.SelectedKeys) != 2 || evB.SelectedKeys[1] != SentinelMore {
		t.Fatalf("unexpected vote event B: %+v ok=%v", evB, ok)
	}
}

func TestBuildVoteDecisionCardCleanTypographyNoMarkdownOrRedundantPrefix(t *testing.T) {
	act := &model.Action{
		ID:      "a3700",
		Kind:    model.ActionKindRouterChoice,
		Title:   "**wi3524 直聊身份** · 合入部署",
		Summary: "分支 `corgi/agy-3637` **已写码完成、验收全绿**。\n- 未合入 / 未部署\n- 请选择推进方式：",
		Choices: []model.ActionChoice{
			{Key: "1", Label: "**合入并构建部署+核验**"},
			{Key: "2", Label: "只合入+构建，暂不部署"},
			{Key: "3", Label: "先看完整报告 / diff"},
		},
	}
	card := BuildDecisionCard(act, DecisionCardOptions{AllowSnooze: true})
	mt := card["main_title"].(map[string]any)
	title := mt["title"].(string)
	desc := mt["desc"].(string)

	if title != "wi3524 直聊身份 · 合入部署" {
		t.Fatalf("expected clean explicit title without markdown, got %q", title)
	}
	for _, bad := range []string{"**", "`", "单选（", "选定后点下方确认"} {
		if strings.Contains(desc, bad) || strings.Contains(title, bad) {
			t.Fatalf("vote card must not contain %q; title=%q desc=%q", bad, title, desc)
		}
	}
	if !strings.Contains(desc, "分支 corgi/agy-3637 已写码完成、验收全绿") {
		t.Fatalf("expected clean summary in desc, got %q", desc)
	}
	opts := card["checkbox"].(map[string]any)["option_list"].([]map[string]any)
	if opts[0]["text"] != "合入并构建部署+核验" {
		t.Fatalf("expected markdown stripped from option text, got %q", opts[0]["text"])
	}
}

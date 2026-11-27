package wecom

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildDirectFocusCardHasLiveButtonsAndNoReplaceText(t *testing.T) {
	card := BuildDirectFocusCard("nav:focus:test", "已切入直聊 · 会话身份提醒", "agy-3637", "MacBook · agy · 直接发消息即可，60 分钟未直聊自动回 Corgi。")

	if card["card_type"] != "button_interaction" {
		t.Fatalf("unexpected card_type: %v", card["card_type"])
	}
	if _, hasReplace := card["replace_text"]; hasReplace {
		t.Fatalf("direct focus card must not lock buttons with replace_text: %+v", card)
	}
	buttons, ok := card["button_list"].([]map[string]any)
	if !ok || len(buttons) != 2 {
		t.Fatalf("expected 2 live buttons, got %+v", card["button_list"])
	}
	if buttons[0]["key"] != "nav:main" || buttons[0]["text"] != "返回 Corgi 主对话" {
		t.Fatalf("unexpected primary return button: %+v", buttons[0])
	}
	if buttons[1]["key"] != "nav:picker" || buttons[1]["text"] != "切换其他" {
		t.Fatalf("unexpected secondary switch button: %+v", buttons[1])
	}
	raw, _ := json.Marshal(card)
	if strings.Contains(string(raw), "不经过 Corgi") || strings.Contains(string(raw), "不经 Corgi") {
		t.Fatalf("card JSON violated copy redline: %s", string(raw))
	}
}

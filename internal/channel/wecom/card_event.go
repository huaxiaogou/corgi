package wecom

import (
	"encoding/json"
	"strings"
)

type cardEventPayload struct {
	TaskID          string          `json:"task_id"`
	TaskIDPascal    string          `json:"TaskId"`
	EventKey        string          `json:"event_key"`
	EventKeyPascal  string          `json:"EventKey"`
	ResponseCode    string          `json:"response_code"`
	ResponseCodePas string          `json:"ResponseCode"`
	CardType        string          `json:"card_type"`
	CardTypePascal  string          `json:"CardType"`
	SelectedItems   json.RawMessage `json:"selected_items"`
	SelectedPascal  json.RawMessage `json:"SelectedItems"`
}

// ParseCardEventCallback parses button_interaction and vote_interaction template_card events.
// It accepts both nested event.template_card_event and flattened event payloads,
// in either snake_case or PascalCase, including vote_interaction selected_items,
// while ignoring non-card events (e.g. enter_chat).
func ParseCardEventCallback(reqID string, raw json.RawMessage) (CardClickEvent, bool) {
	var b struct {
		ChatID            string            `json:"chatid"`
		ChatType          string            `json:"chattype"`
		ResponseCode      string            `json:"response_code"`
		ResponseCodePas   string            `json:"ResponseCode"`
		TaskID            string            `json:"task_id"`
		TaskIDPascal      string            `json:"TaskId"`
		EventKey          string            `json:"event_key"`
		EventKeyPascal    string            `json:"EventKey"`
		CardType          string            `json:"card_type"`
		CardTypePascal    string            `json:"CardType"`
		SelectedItems     json.RawMessage   `json:"selected_items"`
		SelectedPascal    json.RawMessage   `json:"SelectedItems"`
		TemplateCardEvent *cardEventPayload `json:"template_card_event"`
		From              struct {
			UserID  string `json:"userid"`
			UserID2 string `json:"user_id"`
		} `json:"from"`
		Event struct {
			EventType         string            `json:"eventtype"`
			EventTypeAlt      string            `json:"event_type"`
			EventTypePascal   string            `json:"EventType"`
			TaskID            string            `json:"task_id"`
			TaskIDPascal      string            `json:"TaskId"`
			EventKey          string            `json:"event_key"`
			EventKeyPascal    string            `json:"EventKey"`
			CardType          string            `json:"card_type"`
			CardTypePascal    string            `json:"CardType"`
			ResponseCode      string            `json:"response_code"`
			ResponseCodePas   string            `json:"ResponseCode"`
			SelectedItems     json.RawMessage   `json:"selected_items"`
			SelectedPascal    json.RawMessage   `json:"SelectedItems"`
			TemplateCardEvent *cardEventPayload `json:"template_card_event"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return CardClickEvent{}, false
	}
	eventType := firstNonEmpty(b.Event.EventType, b.Event.EventTypeAlt, b.Event.EventTypePascal)
	if eventType != "" && !strings.EqualFold(eventType, "template_card_event") {
		return CardClickEvent{}, false
	}
	fromUser := firstNonEmpty(b.From.UserID, b.From.UserID2)
	if fromUser == "" {
		return CardClickEvent{}, false
	}

	nested := b.Event.TemplateCardEvent
	if nested == nil {
		nested = b.TemplateCardEvent
	}
	var nestedTaskID, nestedEventKey, nestedRespCode, nestedCardType string
	var rawSelected json.RawMessage
	if nested != nil {
		nestedTaskID = firstNonEmpty(nested.TaskID, nested.TaskIDPascal)
		nestedEventKey = firstNonEmpty(nested.EventKey, nested.EventKeyPascal)
		nestedRespCode = firstNonEmpty(nested.ResponseCode, nested.ResponseCodePas)
		nestedCardType = firstNonEmpty(nested.CardType, nested.CardTypePascal)
		rawSelected = firstRawJSON(nested.SelectedItems, nested.SelectedPascal)
	}
	rawSelected = firstRawJSON(rawSelected, b.Event.SelectedItems, b.Event.SelectedPascal, b.SelectedItems, b.SelectedPascal)

	taskID := firstNonEmpty(nestedTaskID, b.Event.TaskID, b.Event.TaskIDPascal, b.TaskID, b.TaskIDPascal)
	eventKey := firstNonEmpty(nestedEventKey, b.Event.EventKey, b.Event.EventKeyPascal, b.EventKey, b.EventKeyPascal)
	respCode := firstNonEmpty(nestedRespCode, b.Event.ResponseCode, b.Event.ResponseCodePas, b.ResponseCode, b.ResponseCodePas)
	cardType := firstNonEmpty(nestedCardType, b.Event.CardType, b.Event.CardTypePascal, b.CardType, b.CardTypePascal)

	questionKey, selectedKeys := parseSelectedItems(rawSelected)
	if taskID == "" && eventKey == "" && questionKey == "" {
		return CardClickEvent{}, false
	}

	actionID, choiceKey := parseEventKey(firstNonEmpty(questionKey, taskID), eventKey)
	actionID = strings.TrimSpace(actionID)
	choiceKey = strings.TrimSpace(choiceKey)
	if choiceKey == "" && len(selectedKeys) > 0 {
		choiceKey = "submit"
	}
	if actionID == "" || (choiceKey == "" && len(selectedKeys) == 0) {
		return CardClickEvent{}, false
	}

	chatID := strings.TrimSpace(b.ChatID)
	if chatID == "" {
		chatID = fromUser
	}
	return CardClickEvent{
		ReqID:        reqID,
		ChatType:     strings.TrimSpace(b.ChatType),
		ChatID:       chatID,
		FromUser:     fromUser,
		CardType:     strings.TrimSpace(cardType),
		TaskID:       firstNonEmpty(taskID, actionID),
		ActionID:     actionID,
		ChoiceKey:    choiceKey,
		QuestionKey:  questionKey,
		SelectedKeys: selectedKeys,
		ResponseCode: respCode,
	}, true
}

func firstRawJSON(vals ...json.RawMessage) json.RawMessage {
	for _, v := range vals {
		t := strings.TrimSpace(string(v))
		if t != "" && t != "null" {
			return v
		}
	}
	return nil
}

type rawSelectedItem struct {
	QuestionKey       string          `json:"question_key"`
	QuestionKeyPascal string          `json:"QuestionKey"`
	OptionIDs         json.RawMessage `json:"option_ids"`
	OptionIDsPascal   json.RawMessage `json:"OptionIds"`
}

func parseSelectedItems(raw json.RawMessage) (questionKey string, optionIDs []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var items []rawSelectedItem
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal(raw, &items)
	} else {
		var wrapper struct {
			SelectedItem       []rawSelectedItem `json:"selected_item"`
			SelectedItemPascal []rawSelectedItem `json:"SelectedItem"`
		}
		if json.Unmarshal(raw, &wrapper) == nil {
			items = append(items, wrapper.SelectedItem...)
			items = append(items, wrapper.SelectedItemPascal...)
		}
	}
	seen := map[string]bool{}
	for _, it := range items {
		if questionKey == "" {
			questionKey = firstNonEmpty(it.QuestionKey, it.QuestionKeyPascal)
		}
		for _, id := range parseOptionIDs(firstRawJSON(it.OptionIDs, it.OptionIDsPascal)) {
			if id != "" && !seen[id] {
				seen[id] = true
				optionIDs = append(optionIDs, id)
			}
		}
	}
	return questionKey, optionIDs
}

func parseOptionIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []string
		if json.Unmarshal(raw, &arr) == nil {
			return cleanOptionIDList(arr)
		}
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return []string{strings.TrimSpace(s)}
		}
		return nil
	}
	var obj struct {
		OptionID       json.RawMessage `json:"option_id"`
		OptionIDPascal json.RawMessage `json:"OptionId"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	inner := firstRawJSON(obj.OptionID, obj.OptionIDPascal)
	if len(inner) == 0 {
		return nil
	}
	return parseOptionIDs(inner)
}

func cleanOptionIDList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func extractCallbackEventType(raw json.RawMessage) string {
	var b struct {
		Event struct {
			EventType       string `json:"eventtype"`
			EventTypeAlt    string `json:"event_type"`
			EventTypePascal string `json:"EventType"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	return firstNonEmpty(b.Event.EventType, b.Event.EventTypeAlt, b.Event.EventTypePascal)
}

func parseEventKey(taskID, eventKey string) (actionID, choiceKey string) {
	if a, k, ok := strings.Cut(strings.TrimSpace(eventKey), ":"); ok {
		return a, k
	}
	return strings.TrimSpace(taskID), strings.TrimSpace(eventKey)
}

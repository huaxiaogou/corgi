package wecom

import (
	"encoding/json"
	"strings"
)

type cardEventPayload struct {
	TaskID          string `json:"task_id"`
	TaskIDPascal    string `json:"TaskId"`
	EventKey        string `json:"event_key"`
	EventKeyPascal  string `json:"EventKey"`
	ResponseCode    string `json:"response_code"`
	ResponseCodePas string `json:"ResponseCode"`
	CardType        string `json:"card_type"`
	CardTypePascal  string `json:"CardType"`
}

// ParseCardEventCallback parses a button_interaction template_card click event.
// It accepts both nested event.template_card_event and flattened event payloads,
// in either snake_case or PascalCase, while ignoring non-card events (e.g. enter_chat).
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
			ResponseCode      string            `json:"response_code"`
			ResponseCodePas   string            `json:"ResponseCode"`
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
	var nestedTaskID, nestedEventKey, nestedRespCode string
	if nested != nil {
		nestedTaskID = firstNonEmpty(nested.TaskID, nested.TaskIDPascal)
		nestedEventKey = firstNonEmpty(nested.EventKey, nested.EventKeyPascal)
		nestedRespCode = firstNonEmpty(nested.ResponseCode, nested.ResponseCodePas)
	}

	taskID := firstNonEmpty(nestedTaskID, b.Event.TaskID, b.Event.TaskIDPascal, b.TaskID, b.TaskIDPascal)
	eventKey := firstNonEmpty(nestedEventKey, b.Event.EventKey, b.Event.EventKeyPascal, b.EventKey, b.EventKeyPascal)
	respCode := firstNonEmpty(nestedRespCode, b.Event.ResponseCode, b.Event.ResponseCodePas, b.ResponseCode, b.ResponseCodePas)
	if taskID == "" && eventKey == "" {
		return CardClickEvent{}, false
	}

	actionID, choiceKey := parseEventKey(taskID, eventKey)
	actionID = strings.TrimSpace(actionID)
	choiceKey = strings.TrimSpace(choiceKey)
	if actionID == "" || choiceKey == "" {
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
		TaskID:       firstNonEmpty(taskID, actionID),
		ActionID:     actionID,
		ChoiceKey:    choiceKey,
		ResponseCode: respCode,
	}, true
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

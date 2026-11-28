package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	maxToolInput  = 300
	maxToolDetail = 4096
)

// parseToolArgs decodes Antigravity tool call arguments, unwrapping double-encoded JSON strings.
func parseToolArgs(raw json.RawMessage) (map[string]any, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, ""
	}
	var m map[string]any
	if json.Unmarshal(trimmed, &m) == nil && m != nil {
		return m, ""
	}
	var encoded string
	if json.Unmarshal(trimmed, &encoded) == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			return nil, ""
		}
		if json.Unmarshal([]byte(encoded), &m) == nil && m != nil {
			return m, ""
		}
		return nil, encoded
	}
	return nil, string(trimmed)
}

func buildAgyToolMessage(name string, raw json.RawMessage) *model.Message {
	args, rawStr := parseToolArgs(raw)
	if args == nil {
		msg := &model.Message{
			Kind:  "tool",
			Name:  name,
			Input: clipText(oneLineToolText(rawStr), maxToolInput),
		}
		if strings.Contains(rawStr, "\n") || len([]rune(rawStr)) > maxToolInput {
			msg.Detail = clipText(rawStr, maxToolDetail)
		}
		return msg
	}

	switch name {
	case "invoke_subagent":
		return buildInvokeSubagentMsg(name, args)
	case "swarm":
		if strings.EqualFold(firstArgString(args, "action", "Action"), "add") {
			return buildSwarmAddMsg(name, args)
		}
		return buildSwarmOtherMsg(name, args)
	case "update_steps":
		return buildUpdateStepsMsg(name, args)
	case "send_message":
		to := firstArgString(args, "Recipient", "recipient")
		body := firstArgString(args, "Message", "message")
		in := strings.TrimSpace(fmt.Sprintf("→ %s: %s", to, oneLineToolText(body)))
		return &model.Message{
			Kind:   "tool",
			Name:   name,
			Input:  clipText(in, maxToolInput),
			Detail: clipText(formatToolDetail(args), maxToolDetail),
		}
	case "manage_subagents", "manage_task":
		act := firstArgString(args, "Action", "action")
		target := firstArgString(args, "TaskId", "task_id")
		if target == "" {
			target = strings.Join(argStringSlice(args, "ConversationIds"), ", ")
		}
		in := strings.TrimSpace(act + " " + target)
		return &model.Message{
			Kind:   "tool",
			Name:   name,
			Input:  clipText(in, maxToolInput),
			Detail: clipText(formatToolDetail(args), maxToolDetail),
		}
	case "schedule":
		when := firstArgString(args, "CronExpression", "cron_expression")
		if when == "" {
			if sec, ok := args["DurationSeconds"]; ok {
				when = fmt.Sprintf("%vs", sec)
			}
		}
		prompt := oneLineToolText(firstArgString(args, "Prompt", "prompt"))
		in := strings.TrimSpace(strings.Trim(when+" · "+prompt, " ·"))
		return &model.Message{
			Kind:   "tool",
			Name:   name,
			Input:  clipText(in, maxToolInput),
			Detail: clipText(formatToolDetail(args), maxToolDetail),
		}
	case "ask_question":
		return buildAskQuestionMsg(name, args)
	case "search_web":
		q := firstArgString(args, "query", "Query")
		dom := firstArgString(args, "domain", "Domain")
		in := q
		if dom != "" {
			in = strings.TrimSpace(q + " (site:" + dom + ")")
		}
		return &model.Message{
			Kind:   "tool",
			Name:   name,
			Input:  clipText(oneLineToolText(in), maxToolInput),
			Detail: clipText(formatToolDetail(args), maxToolDetail),
		}
	default:
		in := firstArgString(args, "CommandLine", "Path", "path", "query", "toolSummary", "Description", "ToolName", "Uri", "command", "url")
		detail := clipText(formatToolDetail(args), maxToolDetail)
		if in == "" {
			in = oneLineToolText(detail)
		}
		msg := &model.Message{
			Kind:  "tool",
			Name:  name,
			Input: clipText(oneLineToolText(in), maxToolInput),
		}
		if detail != "" && detail != msg.Input {
			msg.Detail = detail
		}
		return msg
	}
}

func buildInvokeSubagentMsg(name string, args map[string]any) *model.Message {
	typ := firstArgString(args, "TypeName", "type", "subagent_type")
	role := firstArgString(args, "Role", "role", "description")
	prompt := firstArgString(args, "Prompt", "prompt")
	mod := firstArgString(args, "Model", "model")
	if strings.EqualFold(mod, "inherit") {
		mod = ""
	}
	in := oneLineToolText(role)
	if in == "" {
		in = oneLineToolText(prompt)
	}
	return &model.Message{
		Kind:  "tool",
		Name:  name,
		Input: clipText(in, maxToolInput),
		Subagent: &model.SubagentCall{
			Type:   typ,
			Role:   role,
			Prompt: clipText(prompt, maxToolDetail),
			Model:  mod,
		},
	}
}

func buildSwarmAddMsg(name string, args map[string]any) *model.Message {
	role := firstArgString(args, "name", "Name")
	brief := firstArgString(args, "brief", "Brief")
	mod := firstArgString(args, "model", "Model")
	if strings.EqualFold(mod, "inherit") {
		mod = ""
	}
	in := oneLineToolText(role)
	if b := oneLineToolText(brief); b != "" {
		if in != "" {
			in += " · " + b
		} else {
			in = b
		}
	}
	return &model.Message{
		Kind:  "tool",
		Name:  name,
		Input: clipText(in, maxToolInput),
		Subagent: &model.SubagentCall{
			Type:   "swarm",
			Role:   role,
			Prompt: clipText(brief, maxToolDetail),
			Model:  mod,
		},
	}
}

func buildSwarmOtherMsg(name string, args map[string]any) *model.Message {
	act := firstArgString(args, "action", "Action")
	target := firstArgString(args, "name", "title", "model")
	in := strings.TrimSpace(act + " " + target)
	return &model.Message{
		Kind:   "tool",
		Name:   name,
		Input:  clipText(in, maxToolInput),
		Detail: clipText(formatToolDetail(args), maxToolDetail),
	}
}

func buildUpdateStepsMsg(name string, args map[string]any) *model.Message {
	rawSteps, _ := args["steps"].([]any)
	var (
		doneCount int
		active    string
		lines     []string
	)
	for _, it := range rawSteps {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		title := firstArgString(m, "title", "Title")
		status := strings.ToLower(firstArgString(m, "status", "Status"))
		glyph := "○"
		switch status {
		case "done":
			doneCount++
			glyph = "✓"
		case "in_progress":
			glyph = "▶"
			if active == "" {
				active = title
			}
		case "failed":
			glyph = "✕"
		case "skipped":
			glyph = "–"
		}
		lines = append(lines, fmt.Sprintf("%s %s", glyph, title))
	}
	in := fmt.Sprintf("%d/%d 完成", doneCount, len(lines))
	if active != "" {
		in += " · " + active
	}
	return &model.Message{
		Kind:   "tool",
		Name:   name,
		Input:  clipText(in, maxToolInput),
		Detail: clipText(strings.Join(lines, "\n"), maxToolDetail),
	}
}

func buildAskQuestionMsg(name string, args map[string]any) *model.Message {
	qs, _ := args["questions"].([]any)
	var firstQ string
	var lines []string
	for i, it := range qs {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		q := firstArgString(m, "question", "Question")
		if firstQ == "" {
			firstQ = q
		}
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, q))
		for _, opt := range argStringSlice(m, "options") {
			lines = append(lines, "   - "+opt)
		}
	}
	in := oneLineToolText(firstQ)
	if len(qs) > 1 && in != "" {
		in = fmt.Sprintf("(%d) %s", len(qs), in)
	}
	return &model.Message{
		Kind:   "tool",
		Name:   name,
		Input:  clipText(in, maxToolInput),
		Detail: clipText(strings.Join(lines, "\n"), maxToolDetail),
	}
}

func formatToolDetail(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		v := formatToolValue(args[k])
		if v == "" {
			continue
		}
		if strings.Contains(v, "\n") {
			lines = append(lines, k+":\n  "+strings.ReplaceAll(v, "\n", "\n  "))
		} else {
			lines = append(lines, k+": "+v)
		}
	}
	return strings.Join(lines, "\n")
}

func formatToolValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		b, err := json.MarshalIndent(x, "", "  ")
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func firstArgString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func argStringSlice(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func oneLineToolText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

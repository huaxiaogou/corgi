package wecom

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/kiperjing/corgi/internal/model"
)

func shapeCardCopy(a *model.Action, choiceCount int) (title, desc, body, overflow string) {
	rawTitle := collapseSpaces(a.Title)
	rawSummary := normalizeMultiline(a.Summary)

	sameAsSummary := rawSummary != "" && (rawTitle == "" || isClippedPrefix(rawTitle, rawSummary))
	switch {
	case sameAsSummary:
		if !strings.Contains(rawSummary, "\n") && len([]rune(rawSummary)) <= cardMainTitleMaxRunes {
			title = rawSummary
		} else if head, rest, ok := splitLeadHeading(rawSummary, cardMainTitleMaxRunes); ok {
			title, body = head, rest
		} else {
			title, body = defaultActionHeading(a.Kind, a.Category), rawSummary
		}
	case rawSummary == "" && len([]rune(rawTitle)) > cardMainTitleMaxRunes:
		if head, rest, ok := splitLeadHeading(rawTitle, cardMainTitleMaxRunes); ok {
			title, body = head, rest
		} else {
			title, body = defaultActionHeading(a.Kind, a.Category), rawTitle
		}
	default:
		title = clipRunes(rawTitle, cardMainTitleMaxRunes)
		if title == "" {
			title = defaultActionHeading(a.Kind, a.Category)
		}
		body = rawSummary
	}

	switch {
	case a.Kind == model.ActionKindRouterCheck:
		if w := strings.TrimSpace(a.TargetWorker); w != "" {
			desc = clipRunes("会话："+w+" · 待你查收", cardMainDescMaxRunes)
		} else {
			desc = "轻量审阅提醒"
		}
	case strings.TrimSpace(a.TargetWorker) != "":
		desc = clipRunes("会话："+strings.TrimSpace(a.TargetWorker)+" · 请拍板", cardMainDescMaxRunes)
	case choiceCount == 1:
		desc = "确认后将继续推进"
	case choiceCount > 1:
		desc = fmt.Sprintf("共 %d 个选项 · 请点击拍板", choiceCount)
	default:
		desc = "请确认下一步操作"
	}

	if body != "" {
		body, overflow = splitTextBudget(body, cardSubTitleMaxRunes)
	}
	return title, desc, body, overflow
}

func shouldUseCompactButtons(choices []model.ActionChoice) bool {
	return len(choices) >= cardCompactButtonThreshold
}

func formatCompactButtonText(num int) string {
	return fmt.Sprintf("选%d", num)
}

func shouldExpandOptions(choices []model.ActionChoice) bool {
	for _, ch := range choices {
		if len([]rune(collapseSpaces(ch.Label))) > cardButtonDirectMaxRunes || strings.TrimSpace(ch.Detail) != "" {
			return true
		}
	}
	return false
}

func formatChoiceFullText(ch model.ActionChoice) string {
	label := collapseSpaces(ch.Label)
	detail := collapseSpaces(ch.Detail)
	if detail == "" {
		return label
	}
	if label == "" {
		return detail
	}
	return label + "：" + detail
}

func formatButtonText(num int, label string, numbered bool) string {
	clean := collapseSpaces(label)
	if clean == "" {
		clean = fmt.Sprintf("选项%d", num)
	}
	if !numbered {
		return clipRunes(clean, cardButtonTextMaxRunes)
	}
	clean = stripMatchingIndexPrefix(clean, num)
	combined := fmt.Sprintf("%d. %s", num, clean)
	if len([]rune(combined)) <= cardButtonTextMaxRunes {
		return combined
	}
	return clipRunes(combined, cardButtonTextMaxRunes)
}

func stripMatchingIndexPrefix(s string, num int) string {
	n := strconv.Itoa(num)
	if !strings.HasPrefix(s, n) {
		return s
	}
	rest := s[len(n):]
	for _, sep := range []string{". ", ".", "、", ") ", ")", "：", ":", " "} {
		if after, ok := strings.CutPrefix(rest, sep); ok {
			if trimmed := strings.TrimSpace(after); trimmed != "" {
				return trimmed
			}
		}
	}
	return s
}

func buildQuoteArea(overflow, command string, longOptionLines []string) map[string]any {
	if overflow == "" && command == "" && len(longOptionLines) == 0 {
		return nil
	}

	title := "补充信息"
	text := ""
	switch {
	case overflow == "" && command != "" && len(longOptionLines) == 0:
		title = "待执行命令"
		text = command
	case overflow == "" && command == "" && len(longOptionLines) > 0:
		title = "完整选项说明"
		text = strings.Join(longOptionLines, "\n")
	case overflow != "" && command == "" && len(longOptionLines) == 0:
		title = "完整说明"
		text = overflow
	default:
		var parts []string
		if overflow != "" {
			parts = append(parts, "续："+overflow)
		}
		if command != "" {
			parts = append(parts, "命令："+command)
		}
		if len(longOptionLines) > 0 {
			parts = append(parts, "完整选项：\n"+strings.Join(longOptionLines, "\n"))
		}
		text = strings.Join(parts, "\n")
	}
	return map[string]any{
		"type":       0,
		"title":      title,
		"quote_text": clipRunes(text, cardQuoteTextMaxRunes),
	}
}

func defaultActionHeading(kind, category string) string {
	switch kind {
	case model.ActionKindRouterCheck:
		return "待确认事项"
	case model.ActionKindWorkerDialog:
		return "终端操作确认"
	case model.ActionKindPatrolDigest:
		switch strings.ToLower(strings.TrimSpace(category)) {
		case "morning":
			return "晨间待办简报"
		case "stale":
			return "陈旧会话归档"
		case "patrol":
			return "待办巡检提醒"
		default:
			return "巡检摘要确认"
		}
	default:
		return "待拍板决策"
	}
}

func formatResolvedChannel(resolvedBy string) string {
	switch strings.ToLower(strings.TrimSpace(resolvedBy)) {
	case "wecom":
		return "企业微信"
	case "web":
		return "Web 控制台"
	case "router":
		return "Corgi 路由"
	case "cli":
		return "命令行"
	case "dismiss":
		return "已忽略"
	default:
		return strings.TrimSpace(resolvedBy)
	}
}

func isClippedPrefix(title, summary string) bool {
	flatSummary := collapseSpaces(summary)
	cleanTitle := strings.TrimRight(collapseSpaces(title), "….")
	if cleanTitle == "" || flatSummary == "" {
		return false
	}
	return strings.HasPrefix(flatSummary, cleanTitle)
}

func splitLeadHeading(summary string, maxRunes int) (head, rest string, ok bool) {
	if first, after, found := strings.Cut(summary, "\n"); found {
		h := strings.TrimRight(strings.TrimSpace(first), "。；：:")
		r := strings.TrimSpace(after)
		if h != "" && r != "" && len([]rune(h)) <= maxRunes {
			return h, r, true
		}
	}
	runes := []rune(summary)
	limit := min(len(runes), maxRunes)
	for i := 0; i < limit; i++ {
		switch runes[i] {
		case '。', '！', '？', '；', '!', '?':
			h := strings.TrimSpace(string(runes[:i]))
			if runes[i] == '？' || runes[i] == '?' {
				h = strings.TrimSpace(string(runes[:i+1]))
			}
			r := strings.TrimSpace(string(runes[i+1:]))
			if len([]rune(h)) >= 4 && len([]rune(h)) <= maxRunes && r != "" {
				return h, r, true
			}
		}
	}
	return "", "", false
}

func splitTextBudget(s string, maxRunes int) (head, tail string) {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= maxRunes {
		return string(runes), ""
	}
	// Prefer a clean break at newline or sentence boundary in the latter third of the budget.
	best := -1
	minSplit := maxRunes * 2 / 3
	for i := maxRunes - 1; i >= minSplit; i-- {
		switch runes[i] {
		case '\n', '。', '！', '？', '；', '，', ';', '!', '?':
			best = i + 1
		}
		if best > 0 {
			break
		}
	}
	if best > 0 {
		h := strings.TrimSpace(string(runes[:best]))
		t := strings.TrimSpace(string(runes[best:]))
		if h != "" && t != "" {
			return h, t
		}
	}
	h := strings.TrimRightFunc(string(runes[:maxRunes-1]), isTrimEdgeRune) + "…"
	t := strings.TrimSpace(string(runes[maxRunes-1:]))
	return h, t
}

func sanitizeTaskID(id string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(id) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '@' {
			if b.Len()+1 > cardTaskIDMaxBytes {
				break
			}
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "corgi-action"
	}
	return b.String()
}

func actionID(a *model.Action) string {
	if a == nil {
		return ""
	}
	return a.ID
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeMultiline(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func clipRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	head := strings.TrimRightFunc(string(runes[:maxRunes-1]), isTrimEdgeRune)
	if head == "" {
		head = string(runes[:maxRunes-1])
	}
	return head + "…"
}

func isTrimEdgeRune(r rune) bool {
	return unicode.IsSpace(r) || r == ',' || r == '，' || r == '、' || r == ':' || r == '：' || r == ';' || r == '；'
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// FormatActionResolvedMarkdown builds a concise WeCom status-sync receipt for an Action resolved,
// dismissed, or snoozed outside WeCom (e.g. Web console). WeCom Smart Bot WebSocket does not allow
// mutating an unclicked template_card in-place without an inbound callback req_id, so this receipt
// immediately communicates the settled state in the chat thread.
func FormatActionResolvedMarkdown(a *model.Action) string {
	if a == nil {
		return ""
	}
	question := clipRunes(collapseSpaces(firstNonEmpty(a.Title, a.Summary)), 80)
	worker := collapseSpaces(a.TargetWorker)
	channel := formatResolvedChannel(a.ResolvedBy)
	chosen := clipRunes(collapseSpaces(firstNonEmpty(a.ChosenLabel, a.ChosenKey)), 180)

	var head string
	switch a.Status {
	case model.ActionSnoozed:
		if a.SnoozeUntil != nil {
			head = fmt.Sprintf("**已暂缓**：将于 %s 再次提醒（无需在上方卡片重复操作）", a.SnoozeUntil.Format("01-02 15:04"))
		} else {
			head = "**已暂缓**：已暂缓该待办提醒"
		}
	case model.ActionExpired:
		if a.ResolvedBy == "dismiss" {
			head = "**已关闭**：已忽略该待办（无需在上方卡片重复操作）"
		} else {
			head = "**已失效**：该待办已关闭"
		}
	default:
		source := ""
		if channel != "" {
			source = "（" + channel + "）"
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(a.ChosenKey), "ack"):
			head = fmt.Sprintf("**已确认%s**：已看", source)
		case strings.EqualFold(strings.TrimSpace(a.ChosenKey), "reply"):
			head = fmt.Sprintf("**已回复%s**：%s", source, chosen)
		case strings.EqualFold(strings.TrimSpace(a.ChosenKey), "esc"):
			if worker != "" {
				head = fmt.Sprintf("**已取消%s** · 已向 `%s` 发送取消 (Esc)", source, worker)
			} else {
				head = fmt.Sprintf("**已取消%s**：已取消 (Esc)", source)
			}
		case strings.EqualFold(strings.TrimSpace(a.ChosenKey), "custom"):
			head = fmt.Sprintf("**已拍板 · 自定义回答%s**：%s", source, chosen)
		case worker != "":
			head = fmt.Sprintf("**已拍板%s** · `%s`：%s", source, worker, chosen)
		default:
			head = fmt.Sprintf("**已拍板%s**：%s", source, chosen)
		}
	}
	if question != "" {
		return head + "\n> 原问题：" + question
	}
	return head
}

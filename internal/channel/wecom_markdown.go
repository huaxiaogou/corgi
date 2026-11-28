package channel

import (
	"fmt"
	"regexp"
	"strings"
)

const wecomDetailsMaxRunes = 300

var (
	wecomCalloutRe      = regexp.MustCompile(`^>\s*\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*(.*)$`)
	wecomMarkRe         = regexp.MustCompile(`==([^=\n]+)==`)
	wecomKbdRe          = regexp.MustCompile(`(?i)<kbd>([^<\n]+)</kbd>`)
	wecomInlineCodeRe   = regexp.MustCompile("`[^`\n]+`")
	wecomDetailsOpenRe  = regexp.MustCompile(`(?i)^<details(?:\s[^>]*)?>`)
	wecomDetailsCloseRe = regexp.MustCompile(`(?i)</details>`)
	wecomSummaryRe      = regexp.MustCompile(`(?is)<summary[^>]*>(.*?)</summary>`)
)

// formatMarkdownForWeCom downgrades Web rich-Markdown extensions into clean WeCom-compatible
// Markdown across both Router replies and Worker direct-chat replies while keeping ordinary
// fenced code blocks intact.
func formatMarkdownForWeCom(text, publicURL string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))

	for i := 0; i < len(lines); {
		line := lines[i]
		if ch, count, info, ok := parseFenceOpen(line); ok {
			lang := strings.ToLower(strings.Fields(info + " ")[0])
			var body []string
			j := i + 1
			for j < len(lines) && !isFenceClose(lines[j], ch, count) {
				body = append(body, lines[j])
				j++
			}
			nextIdx := j
			if j < len(lines) {
				nextIdx = j + 1
			}
			switch lang {
			case "metrics":
				out = append(out, downgradeMetricsForWeCom(body)...)
			case "steps":
				out = append(out, downgradeStepsForWeCom(body)...)
			case "mermaid", "svg", "html-card":
				out = append(out, downgradeVisualFenceForWeCom(lang, publicURL))
			default:
				out = append(out, lines[i:nextIdx]...)
			}
			i = nextIdx
			continue
		}

		// Multi-line or single-line <details> block outside fences.
		if wecomDetailsOpenRe.MatchString(strings.TrimSpace(line)) {
			chunk := []string{line}
			j := i
			for !wecomDetailsCloseRe.MatchString(lines[j]) && j+1 < len(lines) {
				j++
				chunk = append(chunk, lines[j])
			}
			out = append(out, downgradeDetailsForWeCom(strings.Join(chunk, "\n"), publicURL))
			i = j + 1
			continue
		}

		// Callout header: > [!NOTE|TIP|IMPORTANT|WARNING|CAUTION]
		if m := wecomCalloutRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			label := wecomCalloutLabel(m[1])
			rest := strings.TrimSpace(m[2])
			if rest != "" {
				out = append(out, fmt.Sprintf("> **%s**：%s", label, downgradeInlineForWeCom(rest)))
			} else {
				out = append(out, fmt.Sprintf("> **%s**", label))
			}
			i++
			continue
		}

		out = append(out, downgradeInlineForWeCom(line))
		i++
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func parseFenceOpen(line string) (byte, int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 {
		return 0, 0, "", false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, "", false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	info := strings.TrimSpace(trimmed[n:])
	if ch == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, "", false
	}
	return ch, n, info, true
}

func isFenceClose(line string, ch byte, minCount int) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < minCount {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != ch {
			return false
		}
	}
	return true
}

func downgradeMetricsForWeCom(lines []string) []string {
	var out []string
	for _, raw := range lines {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, "|")
		for k := range parts {
			parts[k] = strings.TrimSpace(parts[k])
		}
		switch {
		case len(parts) >= 3 && parts[2] != "":
			out = append(out, fmt.Sprintf("- **%s**：%s（%s）", parts[0], parts[1], parts[2]))
		case len(parts) >= 2:
			out = append(out, fmt.Sprintf("- **%s**：%s", parts[0], parts[1]))
		default:
			out = append(out, "- "+parts[0])
		}
	}
	return out
}

func downgradeStepsForWeCom(lines []string) []string {
	var out []string
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		s = strings.TrimLeft(s, "-*• ")
		switch {
		case strings.HasPrefix(strings.ToLower(s), "[x]"):
			out = append(out, "✓ "+strings.TrimSpace(s[3:]))
		case strings.HasPrefix(s, "[~]"):
			out = append(out, "▶ "+strings.TrimSpace(s[3:]))
		case strings.HasPrefix(s, "[!]"):
			out = append(out, "⚠ "+strings.TrimSpace(s[3:]))
		case strings.HasPrefix(s, "[ ]"):
			out = append(out, "○ "+strings.TrimSpace(s[3:]))
		default:
			out = append(out, "○ "+s)
		}
	}
	return out
}

func downgradeVisualFenceForWeCom(lang, publicURL string) string {
	kind := "图表/卡片"
	if lang == "mermaid" || lang == "svg" {
		kind = "图表"
	}
	u := strings.TrimSpace(publicURL)
	if u != "" {
		return fmt.Sprintf("> 已生成%s，请在 [Web 控制台](%s) 查看", kind, u)
	}
	return fmt.Sprintf("> 已生成%s，请在 Web 控制台查看", kind)
}

func downgradeDetailsForWeCom(raw, publicURL string) string {
	title := "详细内容"
	body := raw
	if m := wecomSummaryRe.FindStringSubmatchIndex(raw); m != nil {
		if t := strings.TrimSpace(raw[m[2]:m[3]]); t != "" {
			title = t
		}
		body = raw[:m[0]] + raw[m[1]:]
	}
	body = wecomDetailsOpenRe.ReplaceAllString(strings.TrimSpace(body), "")
	body = wecomDetailsCloseRe.ReplaceAllString(body, "")
	body = formatMarkdownForWeCom(strings.TrimSpace(body), publicURL)
	if body == "" {
		return "**" + title + "**"
	}
	runes := []rune(body)
	if len(runes) > wecomDetailsMaxRunes {
		body = strings.TrimSpace(string(runes[:wecomDetailsMaxRunes])) + "…（完整内容请在 Web 控制台查看）"
	}
	return "**" + title + "**\n" + body
}

func wecomCalloutLabel(kind string) string {
	switch strings.ToUpper(kind) {
	case "TIP":
		return "提示"
	case "IMPORTANT":
		return "重要"
	case "WARNING":
		return "警告"
	case "CAUTION":
		return "警示"
	default:
		return "注意"
	}
}

func downgradeInlineForWeCom(line string) string {
	if !strings.Contains(line, "==") && !strings.Contains(strings.ToLower(line), "<kbd>") {
		return line
	}
	// Preserve inline `code` spans untouched while cleaning ==mark== and <kbd>.
	locs := wecomInlineCodeRe.FindAllStringIndex(line, -1)
	if len(locs) == 0 {
		return replaceWeComInlineTags(line)
	}
	var b strings.Builder
	pos := 0
	for _, loc := range locs {
		b.WriteString(replaceWeComInlineTags(line[pos:loc[0]]))
		b.WriteString(line[loc[0]:loc[1]])
		pos = loc[1]
	}
	b.WriteString(replaceWeComInlineTags(line[pos:]))
	return b.String()
}

func replaceWeComInlineTags(s string) string {
	s = wecomMarkRe.ReplaceAllString(s, "**$1**")
	s = wecomKbdRe.ReplaceAllString(s, "`$1`")
	return s
}

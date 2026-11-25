package agent

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/kiperjing/corgi/internal/model"
)

var (
	numChoiceRe     = regexp.MustCompile(`^\s*([›>•❯▶▸●➜→]?)\s*(\d+)[.)]\s+(.+)$`)
	cursorChoiceRe  = regexp.MustCompile(`^(\s*)([›>❯▶▸➜→])\s+(.+)$`)
	checkboxLineRe  = regexp.MustCompile(`^\s*([›>❯▶▸➜→])?\s*(\[[ xX✓]\])\s+(.+)$`)
	horizTokenRe    = regexp.MustCompile(`(?:\[([A-Za-z][A-Za-z0-9 _/-]{1,20})\]|([›>❯▶▸➜→])\s+([A-Za-z][A-Za-z0-9/-]{1,20})(?:\s{2,}|$))`)
	primaryBtnRe    = regexp.MustCompile(`(?i)^(done|confirm|continue|next|ok|accept|allow|yes|finish|submit)$`)
	ruleOrLogoRe    = regexp.MustCompile(`^[-─━═╭╮╰╯┌┐└┘│┃|╌┄┈▄▀█▌▐░▒▓\s]+$`)
	footerHintRe    = regexp.MustCompile(`(?i)(↑/↓|navigate\s*·|enter\s+(to\s+)?(confirm|toggle|select|continue)|esc\s+to\s+(cancel|interrupt|back)|tab\s+to\s+amend|\bauto\s*·\s*[a-z0-9])`)
	dialogActionRe  = regexp.MustCompile(`(?i)(↑/↓\s*navigate|enter\s+(to\s+)?(confirm|toggle|select|continue)|esc\s+to\s+(cancel|back)|tab\s+to\s+amend)`)
	idleChromeRe    = regexp.MustCompile("(?i)(bypass permissions on|manual mode on|accept edits on|plan mode on|\\? for shortcuts|← for agents|↵\\s*send|⏎\\s*send|shift\\+tab to cycle|⇧tab to cycle|^\\s*[❯>›]\\s*$|^\\s*[❯>›]\\s+try\\s+\")")
	bgFooterRe      = regexp.MustCompile(`(?i)\b[1-9]\d*\s+(background\s+(task|shell|job)s?\b|task\(s\)|shells?\s+running\b)`)
	quotaErrRe      = regexp.MustCompile(`(?i)(usage limit|limit reached|hit your( \w+)? limit|reached your( \w+)? limit|quota exceeded|exceeded your( current)? quota|resource.?exhausted|daily limit|rate.?limit(ed)?\b|credit balance|out of credits|please run /login|not logged in|invalid api key|api error|overloaded|unexpected status \d{3}|try again (at|in) )`)
	confirmHintRe   = regexp.MustCompile(`(?i)(do you want to|allow|proceed|trust (this|the files|the contents)|do you trust|confirm|press enter|esc to cancel|y/n|yes,?\s*(i trust|allow|proceed)|choose your color scheme|terms of service|select login method)`)
	activityGlyphRe = regexp.MustCompile(`^[✢✳✶✻✽✹●⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s*(.+)$`)
)

const maxBelowMenu = 4

type horizButton struct {
	label   string
	focused bool
}

// ScreenAsks reports whether a terminal screen is currently displaying an interactive confirmation dialog.
func ScreenAsks(screen string) bool {
	a := ParseApproval(screen)
	return len(a.Choices) >= 2
}

// cleanSplitColumn strips side-by-side preview panes (e.g. `light | Here's the change: |` or `│ preview │`)
// while leaving normal single-column commands (like `cat a | grep b` which do not end in `|`) intact.
func cleanSplitColumn(raw string) string {
	line := strings.TrimRight(raw, " \t\r")
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	// Strip outer Unicode vertical frame borders if present.
	if strings.HasPrefix(trimmed, "│") || strings.HasPrefix(trimmed, "┃") {
		idx := strings.IndexAny(line, "│┃")
		line = line[idx+len("│"):]
	}
	if strings.HasSuffix(line, "│") || strings.HasSuffix(line, "┃") {
		if last := strings.LastIndexAny(line, "│┃"); last >= 0 {
			line = strings.TrimRight(line[:last], " ")
		}
	}
	// Split on inner Unicode column divider.
	if mid := strings.IndexAny(line, "│┃"); mid >= 0 {
		line = strings.TrimRight(line[:mid], " ")
	}
	// Split on ASCII side-by-side preview box (`option | preview |`).
	if strings.HasSuffix(strings.TrimSpace(line), "|") {
		if bar := strings.Index(line, " |"); bar >= 0 {
			line = strings.TrimRight(line[:bar], " ")
		} else if strings.HasPrefix(strings.TrimSpace(line), "|") {
			return ""
		}
	}
	return line
}

func parseHorizontalButtons(line string) ([]horizButton, bool) {
	t := strings.TrimSpace(line)
	if t == "" || checkboxLineRe.MatchString(line) || numChoiceRe.MatchString(line) || strings.HasPrefix(t, "-") || strings.HasPrefix(t, "•") {
		return nil, false
	}
	matches := horizTokenRe.FindAllStringSubmatch(line, -1)
	if len(matches) < 2 {
		return nil, false
	}
	out := make([]horizButton, 0, len(matches))
	for _, m := range matches {
		if m[1] != "" {
			out = append(out, horizButton{label: strings.TrimSpace(m[1]), focused: false})
		} else if m[3] != "" {
			out = append(out, horizButton{label: strings.TrimSpace(m[3]), focused: true})
		}
	}
	if len(out) < 2 {
		return nil, false
	}
	return out, true
}

func verticalMoveKeys(curIdx, targetIdx int) []string {
	var keys []string
	for d := curIdx; d < targetIdx; d++ {
		keys = append(keys, "down")
	}
	for d := curIdx; d > targetIdx; d-- {
		keys = append(keys, "up")
	}
	return append(keys, "enter")
}

func horizontalMoveKeys(curIdx, targetIdx int) []string {
	var keys []string
	for d := curIdx; d < targetIdx; d++ {
		keys = append(keys, "right")
	}
	for d := curIdx; d > targetIdx; d-- {
		keys = append(keys, "left")
	}
	return append(keys, "enter")
}

// hasIdleInputPrompt checks the bottom non-empty lines for normal CLI input box chrome
// (e.g. `> ... ↵ send`, `bypass permissions on`, `? for shortcuts`) so historical markdown lists
// or quoted dialogs in scrollback above an idle prompt are never mistaken for an active dialog.
func hasIdleInputPrompt(screen string) bool {
	tail := nonEmptyTail(screen, 6)
	hasChrome := false
	for _, l := range tail {
		if dialogActionRe.MatchString(l) {
			return false
		}
		if idleChromeRe.MatchString(l) {
			hasChrome = true
		}
	}
	return hasChrome
}

// ParseApproval extracts structured question/choices and exact navigation keys from a terminal dialog.
func ParseApproval(screen string) model.Approval {
	var rawLines []string
	hasDialogFooter := false
	for _, r := range strings.Split(screen, "\n") {
		cleaned := cleanSplitColumn(r)
		t := strings.TrimSpace(cleaned)
		if t == "" {
			continue
		}
		if dialogActionRe.MatchString(t) {
			hasDialogFooter = true
		}
		if footerHintRe.MatchString(t) {
			continue
		}
		rawLines = append(rawLines, cleaned)
	}
	if len(rawLines) > 32 {
		rawLines = rawLines[len(rawLines)-32:]
	}
	trimmedLines := make([]string, len(rawLines))
	for i, l := range rawLines {
		trimmedLines[i] = strings.TrimSpace(l)
	}
	rawJoined := strings.Join(trimmedLines, "\n")
	out := model.Approval{Raw: rawJoined}
	if len(rawLines) == 0 || hasIdleInputPrompt(screen) {
		return out
	}

	var choices []model.Choice
	style := ""
	firstChoiceIdx := -1
	lastChoiceIdx := -1

	// 1. First check for horizontal wizard button bars (e.g. `[Previous]  [Done]` or `> Previous  [Done]`),
	// optionally paired with an agreement checkbox (`> [x] Yes, I agree...`) above the buttons.
	if confirmHintRe.MatchString(rawJoined) {
		for i := len(rawLines) - 1; i >= 0; i-- {
			btns, ok := parseHorizontalButtons(rawLines[i])
			if !ok {
				continue
			}
			firstChoiceIdx = i
			lastChoiceIdx = i
			style = "cursor"

			// Look upward for an optional checkbox control above the button bar.
			cbIdx := -1
			cbFocused := false
			cbMark := ""
			cbText := ""
			for j := i - 1; j >= 0; j-- {
				if ruleOrLogoRe.MatchString(trimmedLines[j]) {
					break
				}
				if m := checkboxLineRe.FindStringSubmatch(rawLines[j]); m != nil {
					cbIdx = j
					cbFocused = m[1] != ""
					cbMark = m[2]
					parts := []string{strings.TrimSpace(m[3])}
					for k := j + 1; k < i; k++ {
						tk := trimmedLines[k]
						if tk == "" || strings.HasPrefix(strings.ToLower(tk), "links:") || strings.HasPrefix(tk, "- ") || ruleOrLogoRe.MatchString(tk) {
							break
						}
						parts = append(parts, tk)
					}
					cbText = clipText(cbMark+" "+strings.Join(parts, " "), 96)
					break
				}
			}
			if cbIdx >= 0 && cbIdx < firstChoiceIdx {
				firstChoiceIdx = cbIdx
			}

			curBtn := -1
			for bIdx, b := range btns {
				if b.focused {
					curBtn = bIdx
					break
				}
			}

			if cbIdx >= 0 {
				// Wizard with checkbox + horizontal button bar:
				// Surface the primary confirm button (e.g. Done) first so one tap completes setup,
				// followed by the checkbox toggle and secondary navigation buttons.
				type orderedBtn struct {
					idx int
					btn horizButton
				}
				var primary, secondary []orderedBtn
				for bIdx, b := range btns {
					if primaryBtnRe.MatchString(b.label) {
						primary = append(primary, orderedBtn{idx: bIdx, btn: b})
					} else {
						secondary = append(secondary, orderedBtn{idx: bIdx, btn: b})
					}
				}
				if len(primary) == 0 && len(secondary) > 0 {
					last := secondary[len(secondary)-1]
					primary = []orderedBtn{last}
					secondary = secondary[:len(secondary)-1]
				}

				btnKeys := func(targetBtn int) []string {
					if cbFocused || curBtn < 0 {
						keys := []string{"down"}
						for r := 0; r < targetBtn; r++ {
							keys = append(keys, "right")
						}
						return append(keys, "enter")
					}
					return horizontalMoveKeys(curBtn, targetBtn)
				}

				for _, ob := range primary {
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(len(choices) + 1),
						Label:  ob.btn.label,
						Keys:   btnKeys(ob.idx),
						Cursor: len(choices) + 1,
						Active: ob.btn.focused,
					})
				}
				cbKeys := []string{"enter"}
				if !cbFocused && curBtn >= 0 {
					cbKeys = []string{"up", "enter"}
				}
				choices = append(choices, model.Choice{
					Key:    strconv.Itoa(len(choices) + 1),
					Label:  cbText,
					Keys:   cbKeys,
					Cursor: len(choices) + 1,
					Active: cbFocused,
				})
				for _, ob := range secondary {
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(len(choices) + 1),
						Label:  ob.btn.label,
						Keys:   btnKeys(ob.idx),
						Cursor: len(choices) + 1,
						Active: ob.btn.focused,
					})
				}
			} else {
				// Pure horizontal button bar (no checkbox above).
				if curBtn < 0 {
					curBtn = len(btns) - 1
				}
				for bIdx, b := range btns {
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(bIdx + 1),
						Label:  b.label,
						Keys:   horizontalMoveKeys(curBtn, bIdx),
						Cursor: bIdx + 1,
						Active: bIdx == curBtn,
					})
				}
			}
			break
		}
	}

	// 2. Numbered or vertical cursor menus (e.g. permission prompts, folder trust, color scheme picker).
	if len(choices) < 2 {
		choices = nil
		style = ""
		firstChoiceIdx = -1
		lastChoiceIdx = -1
		cursorTextIndent := -1

		for i, l := range rawLines {
			t := trimmedLines[i]
			if ruleOrLogoRe.MatchString(t) {
				continue
			}
			if m := numChoiceRe.FindStringSubmatch(l); m != nil {
				lbl := strings.TrimSpace(m[3])
				// Skip long markdown prose bullets like `1. **Heading**：...` unless marked with an active cursor.
				if m[1] == "" && (len([]rune(lbl)) > 96 || strings.Contains(lbl, "**")) {
					continue
				}
				if firstChoiceIdx < 0 {
					firstChoiceIdx = i
				}
				lastChoiceIdx = i
				style = "number"
				choices = append(choices, model.Choice{
					Key:    m[2],
					Label:  lbl,
					Cursor: len(choices) + 1,
					Active: m[1] != "",
				})
				continue
			}
			if len(choices) == 0 {
				// Do not mistake a standalone checkbox `[x] ...` for a vertical cursor list.
				if checkboxLineRe.MatchString(l) {
					continue
				}
				if m := cursorChoiceRe.FindStringSubmatch(l); m != nil && confirmHintRe.MatchString(strings.Join(trimmedLines[:i+1], "\n")) {
					firstChoiceIdx = i
					lastChoiceIdx = i
					style = "cursor"
					// Track column where option text starts (e.g. "  > terminal" -> indent 4) so
					// deeper-indented wrapped paragraph lines are not mistaken for sibling options.
					cursorTextIndent = len(m[1]) + len(m[2]) + 1
					// Also collect any sibling options immediately above the cursor line (when cursor moved down!).
					var above []string
					for j := i - 1; j >= 0; j-- {
						tj := trimmedLines[j]
						if tj == "" || ruleOrLogoRe.MatchString(tj) || strings.HasSuffix(tj, "?") || strings.HasSuffix(tj, "？") || strings.HasSuffix(tj, ":") || confirmHintRe.MatchString(tj) {
							break
						}
						ind := len(rawLines[j]) - len(strings.TrimLeft(rawLines[j], " "))
						if ind >= 1 && ind <= cursorTextIndent+1 && len(tj) <= 80 {
							above = append([]string{tj}, above...)
							firstChoiceIdx = j
						} else {
							break
						}
					}
					for _, prevOpt := range above {
						choices = append(choices, model.Choice{
							Key:    strconv.Itoa(len(choices) + 1),
							Label:  prevOpt,
							Cursor: len(choices) + 1,
							Active: false,
						})
					}
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(len(choices) + 1),
						Label:  strings.TrimSpace(m[3]),
						Cursor: len(choices) + 1,
						Active: true,
					})
				}
			} else if style == "cursor" {
				low := strings.ToLower(t)
				if strings.HasPrefix(low, "esc") || strings.HasPrefix(low, "enter") || strings.HasPrefix(low, "↑") || strings.HasPrefix(low, "links:") {
					break
				}
				ind := len(l) - len(strings.TrimLeft(l, " "))
				if t != "" && ind <= cursorTextIndent+1 && len(t) <= 80 && len(choices) < 9 {
					lastChoiceIdx = i
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(len(choices) + 1),
						Label:  t,
						Cursor: len(choices) + 1,
					})
				} else {
					break
				}
			}
		}

		// Populate vertical arrow+Enter Keys on cursor menus AND on numbered menus that show a cursor
		// (e.g. Codex folder trust `› 1. Yes`, which requires Enter rather than a bare digit).
		curIdx := -1
		for i, c := range choices {
			if c.Active {
				curIdx = i
				break
			}
		}
		// Require a cursor, interactive footer, or confirmation hint for cursorless numbered menus
		// so plain numbered markdown lists are never mistaken for blocking prompts.
		if style == "number" && curIdx < 0 && !hasDialogFooter && !confirmHintRe.MatchString(rawJoined) {
			choices = nil
		}
		if curIdx >= 0 && style == "cursor" {
			for i := range choices {
				choices[i].Keys = verticalMoveKeys(curIdx, i)
			}
		}
	}

	// Bottom-anchor guard: a live blocking dialog sits at the bottom of the screen with at most
	// a few footer/status lines below it; scrollback menus buried above normal output are ignored.
	if len(choices) >= 2 && lastChoiceIdx >= 0 {
		below := 0
		for i := lastChoiceIdx + 1; i < len(rawLines); i++ {
			if !ruleOrLogoRe.MatchString(trimmedLines[i]) {
				below++
			}
		}
		if below > maxBelowMenu {
			return model.Approval{Raw: out.Raw}
		}
	}

	if len(choices) < 2 {
		return model.Approval{Raw: out.Raw}
	}
	out.Choices = choices
	out.Style = style

	var detailLines []string
	for i := firstChoiceIdx - 1; i >= 0; i-- {
		t := trimmedLines[i]
		if t == "" || ruleOrLogoRe.MatchString(t) {
			continue
		}
		if out.Title == "" && (strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") || strings.HasSuffix(t, ":") || confirmHintRe.MatchString(t)) {
			out.Title = clipText(t, 120)
		} else if out.Command == "" && looksLikeCommand(t) {
			out.Command = clipText(t, 180)
		} else if len(detailLines) < 3 {
			detailLines = append([]string{t}, detailLines...)
		}
	}
	if len(detailLines) > 0 {
		out.Detail = clipText(strings.Join(detailLines, " "), 220)
	}
	if out.Title == "" {
		out.Title = "Agent 等待确认"
	}
	return out
}

// MoveKeys returns the exact key sequence to select targetIdx (0-based) on an Approval dialog.
func MoveKeys(ap model.Approval, targetIdx int) []string {
	if targetIdx < 0 || targetIdx >= len(ap.Choices) {
		return []string{"enter"}
	}
	if len(ap.Choices[targetIdx].Keys) > 0 {
		return append([]string(nil), ap.Choices[targetIdx].Keys...)
	}
	activeIdx := 0
	for i, c := range ap.Choices {
		if c.Active {
			activeIdx = i
			break
		}
	}
	return verticalMoveKeys(activeIdx, targetIdx)
}

// ProgressHash hashes a terminal screen after stripping animated spinner glyphs,
// spinner/status lines, and elapsed-time/token digits so Watchdog reliably detects
// a hung turn even while CLI spinners and elapsed-second counters keep ticking.
func ProgressHash(screen string) string {
	var b strings.Builder
	for _, l := range strings.Split(screen, "\n") {
		low := strings.ToLower(strings.TrimSpace(l))
		if strings.Contains(low, "esc to interrupt") ||
			strings.Contains(low, "esc to cancel") ||
			strings.Contains(low, "task running for a while") ||
			strings.Contains(low, "ctrl+o to expand") ||
			(strings.Contains(low, "streaming") && strings.Contains(low, "tokens")) ||
			isAnimatedStatusLine(l) {
			continue
		}
		b.WriteString(strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) || isSpinnerRune(r) {
				return -1
			}
			return r
		}, l))
		b.WriteByte('\n')
	}
	sum := sha1.Sum([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:8])
}

func isSpinnerRune(r rune) bool {
	if r >= 0x2800 && r <= 0x28ff { // Braille spinner animation frames
		return true
	}
	switch r {
	case '✢', '✳', '✶', '✻', '✽', '✹', '◐', '◓', '◑', '◒', '◴', '◷', '◶', '◵':
		return true
	}
	return false
}

func isAnimatedStatusLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	first := rune(0)
	for _, r := range trimmed {
		first = r
		break
	}
	if !isSpinnerRune(first) {
		return false
	}
	low := strings.ToLower(trimmed)
	for _, kw := range []string{"thinking", "pondering", "working", "streaming", "brewing", "churning", "running", "connecting", "retrying", "waiting"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// ActivityLine extracts the current working step and spinner metadata from the bottom of an agent terminal.
func ActivityLine(screen string) (tail, meta string) {
	lines := nonEmptyTail(screen, 12)
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || l == ">" || l == "❯" || strings.HasPrefix(l, "─") {
			continue
		}
		if m := activityGlyphRe.FindStringSubmatch(l); m != nil {
			body := strings.TrimSpace(m[1])
			if open := strings.LastIndexByte(body, '('); open > 0 && strings.HasSuffix(body, ")") {
				return clipText(strings.TrimSpace(body[:open]), 100), clipText(body[open+1:len(body)-1], 80)
			}
			return clipText(body, 120), ""
		}
		low := strings.ToLower(l)
		if strings.Contains(low, "esc to interrupt") || strings.Contains(low, "streaming ·") {
			return clipText(l, 120), ""
		}
	}
	return "", ""
}

func looksLikeCommand(line string) bool {
	prefixes := []string{"$ ", "go ", "npm ", "git ", "rm ", "mv ", "cp ", "curl ", "docker ", "kubectl ", "make ", "pytest ", "python ", "bash ", "sh "}
	low := strings.ToLower(strings.TrimSpace(line))
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// HasBackgroundTask checks the terminal footer for active background tasks/timers.
func HasBackgroundTask(screen string) bool {
	for _, l := range nonEmptyTail(screen, 3) {
		if bgFooterRe.MatchString(l) {
			return true
		}
	}
	return false
}

// ExtractQuotaError scans the bottom of a terminal for provider rate limits / auth errors,
// ignoring lines whose matched error phrase already appears in the assistant's own summary.
func ExtractQuotaError(screen, summary string) string {
	lowSummary := strings.ToLower(summary)
	for _, l := range nonEmptyTail(screen, 12) {
		cleaned := strings.Trim(l, "⎿⏺●•✗✘⚠■│ ")
		if match := quotaErrRe.FindString(cleaned); match != "" {
			if !strings.Contains(lowSummary, strings.ToLower(match)) {
				return clipText(cleaned, 200)
			}
		}
	}
	return ""
}

// TailLines returns the last maxLines non-empty lines of screen joined by newline.
func TailLines(screen string, maxLines int) string {
	return strings.Join(nonEmptyTail(screen, maxLines), "\n")
}

var imageExtMap = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// ExtractImage decodes a base64 image once into imgDir and returns a compact reference token.
func ExtractImage(mediaType, b64, imgDir string) string {
	ext, ok := imageExtMap[mediaType]
	if !ok || b64 == "" || imgDir == "" {
		return "[image]"
	}
	sum := sha1.Sum([]byte(b64))
	name := hex.EncodeToString(sum[:10]) + ext
	target := filepath.Join(imgDir, name)
	if _, err := os.Stat(target); err != nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return "[image]"
		}
		if err := os.MkdirAll(imgDir, 0o700); err != nil {
			return "[image]"
		}
		tmp := target + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) != nil || os.Rename(tmp, target) != nil {
			_ = os.Remove(tmp)
			return "[image]"
		}
	}
	return "[image:" + target + "]"
}

func nonEmptyTail(s string, maxLines int) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	return out
}

func clipText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

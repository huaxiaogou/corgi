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
	numChoiceRe     = regexp.MustCompile(`^\s*([›>•❯▶]?)\s*(\d+)[.)]\s+(.+)$`)
	cursorChoiceRe  = regexp.MustCompile(`^\s*([›>❯▶])\s+(.+)$`)
	bracketBtnRe    = regexp.MustCompile(`\[([A-Za-z0-9][A-Za-z0-9 _/-]{1,24})\]`)
	bgFooterRe      = regexp.MustCompile(`(?i)\b[1-9]\d*\s+(background\s+(task|shell|job)s?\b|task\(s\)|shells?\s+running\b)`)
	quotaErrRe      = regexp.MustCompile(`(?i)(usage limit|limit reached|hit your( \w+)? limit|reached your( \w+)? limit|quota exceeded|exceeded your( current)? quota|resource.?exhausted|daily limit|rate.?limit(ed)?\b|credit balance|out of credits|please run /login|not logged in|invalid api key|api error|overloaded|unexpected status \d{3}|try again (at|in) )`)
	confirmHintRe   = regexp.MustCompile(`(?i)(do you want to|allow|proceed|trust (this|the files|the contents)|do you trust|confirm|press enter|esc to cancel|y/n|yes,?\s*(i trust|allow|proceed)|choose your color scheme|terms of service|select login method)`)
	activityGlyphRe = regexp.MustCompile(`^[✢✳✶✻✽✹●⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s*(.+)$`)
)

// ScreenAsks reports whether a terminal screen is currently displaying an interactive confirmation dialog.
func ScreenAsks(screen string) bool {
	a := ParseApproval(screen)
	return len(a.Choices) >= 2
}

// ParseApproval extracts structured question/choices from a terminal confirmation dialog.
func ParseApproval(screen string) model.Approval {
	lines := nonEmptyTail(screen, 28)
	out := model.Approval{Raw: strings.Join(lines, "\n")}
	if len(lines) == 0 {
		return out
	}

	var choices []model.Choice
	style := ""
	firstChoiceIdx := -1
	for i, l := range lines {
		if m := numChoiceRe.FindStringSubmatch(l); m != nil {
			if firstChoiceIdx < 0 {
				firstChoiceIdx = i
			}
			style = "number"
			choices = append(choices, model.Choice{
				Key:    m[2],
				Label:  strings.TrimSpace(m[3]),
				Cursor: len(choices) + 1,
				Active: m[1] != "",
			})
			continue
		}
		if len(choices) == 0 {
			if m := cursorChoiceRe.FindStringSubmatch(l); m != nil && confirmHintRe.MatchString(strings.Join(lines[:i+1], "\n")) {
				firstChoiceIdx = i
				style = "cursor"
				choices = append(choices, model.Choice{
					Key:    "1",
					Label:  strings.TrimSpace(m[2]),
					Cursor: 1,
					Active: true,
				})
			}
		} else if style == "cursor" {
			t := strings.TrimSpace(l)
			low := strings.ToLower(t)
			if t != "" && !strings.HasPrefix(low, "esc") && !strings.HasPrefix(low, "enter") && !strings.HasPrefix(low, "↑") && len(choices) < 6 {
				choices = append(choices, model.Choice{
					Key:    strconv.Itoa(len(choices) + 1),
					Label:  t,
					Cursor: len(choices) + 1,
				})
			}
		}
	}

	// Recognize horizontal wizard buttons like "[Previous]  [Done]" on onboarding/ToS dialogs.
	if len(choices) < 2 && confirmHintRe.MatchString(out.Raw) {
		for i := len(lines) - 1; i >= 0; i-- {
			matches := bracketBtnRe.FindAllStringSubmatch(lines[i], -1)
			if len(matches) >= 2 {
				firstChoiceIdx = i
				style = "cursor"
				for idx, bm := range matches {
					lbl := strings.TrimSpace(bm[1])
					choices = append(choices, model.Choice{
						Key:    strconv.Itoa(idx + 1),
						Label:  lbl,
						Cursor: idx + 1,
						Active: idx == len(matches)-1,
					})
				}
				break
			}
		}
	}

	if len(choices) < 2 {
		return model.Approval{Raw: out.Raw}
	}
	out.Choices = choices
	out.Style = style

	for i := firstChoiceIdx - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "─") || strings.HasPrefix(t, "═") || strings.HasPrefix(t, "╭") || strings.HasPrefix(t, "╰") {
			continue
		}
		if out.Title == "" && (strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") || confirmHintRe.MatchString(t)) {
			out.Title = clipText(t, 120)
		} else if out.Command == "" && looksLikeCommand(t) {
			out.Command = clipText(t, 180)
		} else if out.Detail == "" {
			out.Detail = clipText(t, 160)
		}
	}
	if out.Title == "" {
		out.Title = "Agent 等待确认"
	}
	return out
}

// MoveKeys computes the exact key sequence (up/down/left/right + enter) to pick targetIdx (0-based) on a cursor menu.
func MoveKeys(ap model.Approval, targetIdx int) []string {
	if targetIdx < 0 || targetIdx >= len(ap.Choices) {
		return []string{"enter"}
	}
	activeIdx := 0
	for i, c := range ap.Choices {
		if c.Active {
			activeIdx = i
			break
		}
	}
	var keys []string
	for activeIdx < targetIdx {
		keys = append(keys, "down")
		activeIdx++
	}
	for activeIdx > targetIdx {
		keys = append(keys, "up")
		activeIdx--
	}
	return append(keys, "enter")
}

// ProgressHash hashes a terminal screen after stripping spinner lines and elapsed-time/token digits
// so Watchdog accurately detects a hung turn whose spinner timer keeps ticking.
func ProgressHash(screen string) string {
	var b strings.Builder
	for _, l := range strings.Split(screen, "\n") {
		low := strings.ToLower(l)
		if strings.Contains(low, "esc to interrupt") ||
			strings.Contains(low, "esc to cancel") ||
			strings.Contains(low, "task running for a while") ||
			(strings.Contains(low, "streaming") && strings.Contains(low, "tokens")) {
			continue
		}
		b.WriteString(strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return -1
			}
			return r
		}, l))
		b.WriteByte('\n')
	}
	sum := sha1.Sum([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:8])
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

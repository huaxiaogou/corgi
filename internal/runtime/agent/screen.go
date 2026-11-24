package agent

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

var (
	numChoiceRe    = regexp.MustCompile(`^\s*([›>•❯▶]?)\s*(\d+)[.)]\s+(.+)$`)
	cursorChoiceRe = regexp.MustCompile(`^\s*([›>❯▶])\s+(.+)$`)
	bgFooterRe     = regexp.MustCompile(`(?i)\b[1-9]\d*\s+(background\s+(task|shell|job)s?\b|task\(s\)|shells?\s+running\b)`)
	quotaErrRe     = regexp.MustCompile(`(?i)(usage limit|limit reached|hit your( \w+)? limit|reached your( \w+)? limit|quota exceeded|exceeded your( current)? quota|resource.?exhausted|daily limit|rate.?limit(ed)?\b|credit balance|out of credits|please run /login|not logged in|invalid api key|api error|overloaded|unexpected status \d{3})`)
	confirmHintRe  = regexp.MustCompile(`(?i)(do you want to|allow|proceed|trust (this|the contents)|confirm|press enter|esc to cancel|y/n|yes,?\s*allow)`)
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
			active := m[1] != ""
			style = "number"
			choices = append(choices, model.Choice{
				Key:    m[2],
				Label:  strings.TrimSpace(m[3]),
				Cursor: len(choices) + 1,
				Active: active,
			})
			continue
		}
		if len(choices) == 0 {
			if m := cursorChoiceRe.FindStringSubmatch(l); m != nil && confirmHintRe.MatchString(strings.Join(lines[:i], "\n")) {
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
			if t != "" && !strings.HasPrefix(strings.ToLower(t), "esc") && len(choices) < 6 {
				choices = append(choices, model.Choice{
					Key:    strconv.Itoa(len(choices) + 1),
					Label:  t,
					Cursor: len(choices) + 1,
				})
			}
		}
	}

	if len(choices) < 2 {
		return model.Approval{Raw: out.Raw}
	}
	out.Choices = choices
	out.Style = style

	// Pull title & optional command preview from lines above the choices.
	for i := firstChoiceIdx - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "─") || strings.HasPrefix(t, "═") {
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
	lines := nonEmptyTail(screen, 3)
	for _, l := range lines {
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

// TranscriptReader incrementally tails a JSONL transcript file using an Adapter.
type TranscriptReader struct {
	path    string
	adapter Adapter
	imgDir  string
	offset  int64
	msgs    []*model.Message
}

// NewTranscriptReader initializes an incremental reader for path.
func NewTranscriptReader(path string, adapter Adapter, imgDir string) *TranscriptReader {
	return &TranscriptReader{path: path, adapter: adapter, imgDir: imgDir}
}

// Poll reads newly appended complete JSONL lines and returns whether new messages arrived.
func (tr *TranscriptReader) Poll() bool {
	f, err := os.Open(tr.path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == tr.offset {
		return false
	}
	if fi.Size() < tr.offset {
		tr.offset = 0
		tr.msgs = nil
	}
	if _, err := f.Seek(tr.offset, io.SeekStart); err != nil {
		return false
	}
	rd := bufio.NewReaderSize(f, 512*1024)
	changed := false
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break // wait for complete trailing newline on next poll
		}
		tr.offset += int64(len(line))
		if msgs, ok := tr.adapter.ParseTranscriptLine(line, tr.imgDir); ok {
			tr.msgs = append(tr.msgs, msgs...)
			changed = true
		}
	}
	const maxKeep = 120
	if len(tr.msgs) > maxKeep {
		tr.msgs = append([]*model.Message(nil), tr.msgs[len(tr.msgs)-maxKeep:]...)
	}
	return changed
}

// Messages returns a shallow snapshot of buffered messages.
func (tr *TranscriptReader) Messages() []*model.Message {
	return append([]*model.Message(nil), tr.msgs...)
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

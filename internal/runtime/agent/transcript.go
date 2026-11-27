package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	maxViewMessages = 60
	earlierPageSize = 100
)

// corgiHeaderTriggerRe matches ONLY Corgi's own deliverBatch event header lines (`#t1234 · thread · source`),
// avoiding false matches against arbitrary #tID mentions inside quoted prose or reports.
var corgiHeaderTriggerRe = regexp.MustCompile(`(?m)^[ \t]*#(t(\d+))[ \t]*·`)

// TurnState summarizes the structured latest-turn boundary observed in a JSONL transcript.
type TurnState struct {
	Text          string
	AfterLastUser bool
	AssistantAt   int64
	LastUserAt    int64
	LastUserTID   string
	LastUserSeq   int64
}

// ExtractCorgiHeaderMaxTrigger parses the highest Corgi event header trigger ID (e.g. "t3323", 3323)
// embedded inside a structured user prompt delivered by Engine.deliverBatch.
func ExtractCorgiHeaderMaxTrigger(userText string) (string, int64) {
	if strings.TrimSpace(userText) == "" {
		return "", 0
	}
	matches := corgiHeaderTriggerRe.FindAllStringSubmatch(userText, -1)
	var bestID string
	var bestSeq int64
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		seq, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || seq <= 0 {
			continue
		}
		if seq > bestSeq {
			bestSeq = seq
			bestID = m[1]
		}
	}
	return bestID, bestSeq
}

// TranscriptReader incrementally tails a JSONL session file with tail-window fast open and pagination.
type TranscriptReader struct {
	path    string
	adapter Adapter
	imgDir  string
	offset  int64
	began   int64
	limit   int64
	partial bool
	dropped int
	msgs    []*model.Message
}

// NewTranscriptReader initializes an incremental reader for path.
func NewTranscriptReader(path string, adapter Adapter, imgDir string) *TranscriptReader {
	return &TranscriptReader{path: path, adapter: adapter, imgDir: imgDir}
}

// Path returns the underlying transcript file path.
func (tr *TranscriptReader) Path() string { return tr.path }

// OpenTail reads only the tail window of a large JSONL session so multi-megabyte logs open immediately.
func (tr *TranscriptReader) OpenTail() {
	fi, err := os.Stat(tr.path)
	if err != nil {
		return
	}
	for win := int64(1 << 20); ; win *= 4 {
		*tr = *NewTranscriptReader(tr.path, tr.adapter, tr.imgDir)
		if win >= fi.Size() {
			tr.Poll()
			return
		}
		tr.offset = fi.Size() - win
		tr.partial = true
		tr.Poll()
		if len(tr.msgs) >= maxViewMessages/2 {
			return
		}
	}
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
		*tr = *NewTranscriptReader(tr.path, tr.adapter, tr.imgDir)
	}
	if _, err := f.Seek(tr.offset, io.SeekStart); err != nil {
		return false
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	if tr.partial {
		skip, err := rd.ReadBytes('\n')
		if err != nil {
			return false
		}
		tr.offset += int64(len(skip))
		tr.partial = false
		tr.began = tr.offset
	}
	changed := false
	for tr.limit == 0 || tr.offset < tr.limit {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break
		}
		lineStart := tr.offset
		tr.offset += int64(len(line))
		if parsed, ok := tr.adapter.ParseTranscriptLine(line, tr.imgDir); ok {
			for _, m := range parsed {
				m.At = lineStart
				tr.msgs = append(tr.msgs, m)
			}
			changed = true
		}
	}
	if len(tr.msgs) > maxViewMessages*2 && tr.limit == 0 {
		trim := len(tr.msgs) - maxViewMessages
		tr.dropped += trim
		tr.msgs = append([]*model.Message(nil), tr.msgs[trim:]...)
	}
	return changed
}

// View returns up to maxViewMessages most recent buffered messages.
func (tr *TranscriptReader) View() []*model.Message {
	if len(tr.msgs) > maxViewMessages {
		return tr.msgs[len(tr.msgs)-maxViewMessages:]
	}
	return tr.msgs
}

// Messages returns a shallow copy of View() for callers expecting the slice snapshot.
func (tr *TranscriptReader) Messages() []*model.Message {
	return append([]*model.Message(nil), tr.View()...)
}

// EarlierCursor returns the byte offset before View()[0], or 0 when View() already starts at byte 0.
func (tr *TranscriptReader) EarlierCursor() int64 {
	view := tr.View()
	if len(view) == 0 || (tr.began == 0 && len(view) == len(tr.msgs) && tr.dropped == 0) {
		return 0
	}
	return view[0].At
}

// LastAssistant returns the latest assistant text in the transcript (clipped to maxRunes), or "".
func (tr *TranscriptReader) LastAssistant(maxRunes int) string {
	for i := len(tr.msgs) - 1; i >= 0; i-- {
		if tr.msgs[i].Kind == "assistant" && tr.msgs[i].Text != "" {
			return clipText(tr.msgs[i].Text, maxRunes)
		}
	}
	return ""
}

// LastAssistantAfterLastUser returns the latest assistant text written strictly after the most
// recent user message in the transcript. If the transcript currently ends on a user turn (or
// interim thinking/tool steps) with no assistant reply yet, it returns ("", false).
func (tr *TranscriptReader) LastAssistantAfterLastUser(maxRunes int) (string, bool) {
	st := tr.InspectLastTurn(maxRunes)
	if !st.AfterLastUser {
		return "", false
	}
	return st.Text, true
}

// InspectLastTurn scans buffered messages backward to report whether a non-empty assistant reply
// exists strictly after the latest user turn, along with exact byte offsets and the highest Corgi
// event header trigger ID consumed in that user turn. When no post-user assistant text exists yet,
// Text/AssistantAt fall back to the latest earlier assistant for display while AfterLastUser is false.
func (tr *TranscriptReader) InspectLastTurn(maxRunes int) TurnState {
	var st TurnState
	seenUser := false
	for i := len(tr.msgs) - 1; i >= 0; i-- {
		m := tr.msgs[i]
		if m == nil {
			continue
		}
		switch m.Kind {
		case "assistant":
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			if !seenUser && !st.AfterLastUser {
				st.Text = clipText(m.Text, maxRunes)
				st.AssistantAt = m.At
				st.AfterLastUser = true
			} else if seenUser && st.Text == "" {
				st.Text = clipText(m.Text, maxRunes)
				st.AssistantAt = m.At
			}
		case "user":
			if !seenUser {
				seenUser = true
				st.LastUserAt = m.At
				st.LastUserTID, st.LastUserSeq = ExtractCorgiHeaderMaxTrigger(m.Text)
			}
		}
		if seenUser && st.Text != "" {
			break
		}
	}
	return st
}

// ComputeTurnToolStats reads at most the trailing maxTailBytes (default 1 MiB) of a JSONL
// transcript without expanding the read window and summarizes up to the last 8 tool results
// since the most recent user message for thrash detection.
func ComputeTurnToolStats(path string, adapter Adapter, maxTailBytes int64) *model.ToolStats {
	if path == "" || adapter == nil {
		return nil
	}
	if maxTailBytes <= 0 {
		maxTailBytes = 1 << 20
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return &model.ToolStats{}
	}
	var offset int64
	partial := false
	if fi.Size() > maxTailBytes {
		offset = fi.Size() - maxTailBytes
		partial = true
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil
		}
	}
	rd := bufio.NewReaderSize(f, 1<<16)
	if partial {
		skip, err := rd.ReadBytes('\n')
		if err != nil {
			return &model.ToolStats{}
		}
		offset += int64(len(skip))
	}
	var msgs []*model.Message
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			// Skip incomplete trailing line still being flushed to disk.
			if line[len(line)-1] != '\n' {
				break
			}
			lineStart := offset
			offset += int64(len(line))
			if parsed, ok := adapter.ParseTranscriptLine(line, ""); ok {
				for _, m := range parsed {
					m.At = lineStart
					msgs = append(msgs, m)
				}
			}
		}
		if err != nil {
			break
		}
	}
	return SummarizeTurnToolStats(msgs, 8)
}

// SummarizeTurnToolStats scans msgs backward until the most recent user turn, collecting up to
// windowSize tool results (messages with Kind=="tool" and non-empty Output or Error==true).
func SummarizeTurnToolStats(msgs []*model.Message, windowSize int) *model.ToolStats {
	if windowSize <= 0 {
		windowSize = 8
	}
	stats := &model.ToolStats{}
	trailingBroken := false
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil {
			continue
		}
		if m.Kind == "user" {
			stats.LastUserAt = m.At
			break
		}
		if stats.N >= windowSize || m.Kind != "tool" {
			continue
		}
		// Count only actual tool results (not tool_use / function_call invocations).
		if strings.TrimSpace(m.Output) == "" && !m.Error {
			continue
		}
		stats.N++
		if m.Error {
			stats.Failed++
			if stats.LastFailureAt == 0 {
				stats.LastFailureAt = m.At
			}
			if !trailingBroken {
				stats.TrailingFailed++
			}
		} else {
			trailingBroken = true
		}
	}
	return stats
}

// EarlierMessages loads up to earlierPageSize messages starting strictly before byte offset `before`.
func EarlierMessages(path string, adapter Adapter, imgDir string, before int64) ([]*model.Message, int64) {
	fi, err := os.Stat(path)
	if err != nil || before <= 0 {
		return []*model.Message{}, 0
	}
	end := min(fi.Size(), before+(1<<20))
	for win := int64(4 << 20); ; win *= 4 {
		tr := NewTranscriptReader(path, adapter, imgDir)
		if start := before - win; start > 0 {
			tr.offset = start
			tr.partial = true
		}
		tr.limit = end
		tr.Poll()
		var keep []*model.Message
		for _, m := range tr.msgs {
			if m.At < before {
				keep = append(keep, m)
			}
		}
		atTop := tr.began == 0
		if len(keep) > earlierPageSize {
			keep = keep[len(keep)-earlierPageSize:]
			return keep, keep[0].At
		}
		if atTop {
			if keep == nil {
				keep = []*model.Message{}
			}
			return keep, 0
		}
	}
}

// MsgSync computes incremental patches for a single SSE viewer.
type MsgSync struct {
	sent map[int]string
}

// Patch returns only the changed tail of tr.View() since the previous call, or nil when unchanged.
func (ms *MsgSync) Patch(tr *TranscriptReader) *model.MsgPatch {
	view := tr.View()
	base := tr.dropped + len(tr.msgs) - len(view)
	enc := make([]string, len(view))
	for i, msg := range view {
		b, _ := json.Marshal(msg)
		enc[i] = string(b)
	}
	from := base + len(view)
	if ms.sent == nil {
		from = base
	} else {
		for i, s := range enc {
			if ms.sent[base+i] != s {
				from = base + i
				break
			}
		}
		if from == base+len(view) && len(ms.sent) == len(view) {
			if _, ok := ms.sent[base]; ok {
				return nil
			}
		}
	}
	ms.sent = make(map[int]string, len(enc))
	for i, s := range enc {
		ms.sent[base+i] = s
	}
	return &model.MsgPatch{
		Base:    base,
		From:    from,
		Msgs:    append([]*model.Message{}, view[from-base:]...),
		Earlier: tr.EarlierCursor(),
	}
}

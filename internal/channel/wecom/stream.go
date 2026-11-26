package wecom

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// StreamMaxBytes is WeCom's hard per-frame UTF-8 byte ceiling for aibot_respond_msg stream content.
	StreamMaxBytes = 20480
	// StreamMaxDuration leaves a 30s safety margin before WeCom's 10-minute passive stream auto-seal.
	StreamMaxDuration = 9*time.Minute + 30*time.Second
	// DefaultStreamThrottle coalesces rapid transcript updates into ~1s full-snapshot frames.
	DefaultStreamThrottle = time.Second
)

var (
	thinkOpenRe  = regexp.MustCompile(`(?i)<\s*think\s*>`)
	thinkCloseRe = regexp.MustCompile(`(?i)<\s*/\s*think\s*>`)
	streamSeq    atomic.Uint64
)

// SanitizeThinkTags escapes literal <think>/<\/think> markers inside user or model text
// so WeCom's collapsible thinking parser never splits or breaks prematurely.
func SanitizeThinkTags(s string) string {
	s = thinkOpenRe.ReplaceAllString(s, "&lt;think&gt;")
	s = thinkCloseRe.ReplaceAllString(s, "&lt;/think&gt;")
	return s
}

// FormatStreamContent builds a WeCom stream frame payload with strictly closed <think>...</think>
// tags within StreamMaxBytes UTF-8 bytes, prioritizing the final body and keeping the latest tail
// of thinking progress when trimming is required.
func FormatStreamContent(think, body string) string {
	think = strings.TrimSpace(SanitizeThinkTags(think))
	body = strings.TrimSpace(SanitizeThinkTags(body))
	if think == "" && body == "" {
		return ""
	}
	if think == "" {
		return clipUTF8Bytes(body, StreamMaxBytes, "\n\n…（内容过长，完整报告见预览链接或附件）")
	}

	const openTag = "<think>"
	const closeTag = "</think>"
	tagOverhead := len(openTag) + len(closeTag)
	if body != "" {
		tagOverhead++ // separating newline
	}

	if body != "" {
		const bodySuffix = "\n\n…（内容过长，完整报告见预览链接或附件）"
		// Reserve at least a compact slice for thinking when body is huge, unless body itself needs almost all space.
		maxBodyBytes := StreamMaxBytes - tagOverhead - 512
		if maxBodyBytes < StreamMaxBytes/2 {
			maxBodyBytes = StreamMaxBytes - tagOverhead
		}
		if len(body) > maxBodyBytes {
			body = clipUTF8Bytes(body, maxBodyBytes, bodySuffix)
		}
	}

	thinkBudget := StreamMaxBytes - tagOverhead - len(body)
	if thinkBudget <= 0 {
		return clipUTF8Bytes(body, StreamMaxBytes, "…")
	}
	if len(think) > thinkBudget {
		think = clipUTF8BytesTail(think, thinkBudget, "…\n")
	}
	if think == "" {
		return body
	}
	if body == "" {
		return openTag + think + closeTag
	}
	return openTag + think + closeTag + "\n" + body
}

func clipUTF8Bytes(s string, maxBytes int, suffix string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if len(suffix) >= maxBytes {
		return trimUTF8Prefix(s, maxBytes)
	}
	return strings.TrimRight(trimUTF8Prefix(s, maxBytes-len(suffix)), " \t\n") + suffix
}

func clipUTF8BytesTail(s string, maxBytes int, prefix string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if len(prefix) >= maxBytes {
		return trimUTF8Suffix(s, maxBytes)
	}
	tail := trimUTF8Suffix(s, maxBytes-len(prefix))
	if idx := strings.IndexByte(tail, '\n'); idx > 0 && idx < len(tail)/2 {
		tail = tail[idx+1:]
	}
	tail = strings.TrimLeft(tail, " \t\n")
	if tail == "" {
		return ""
	}
	return prefix + tail
}

func trimUTF8Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func trimUTF8Suffix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	i := len(s) - maxBytes
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// StreamSendFunc writes one full-snapshot aibot_respond_msg stream frame.
type StreamSendFunc func(ctx context.Context, reqID, streamID, content string, finish bool) error

// StreamSession serializes all progress and final frames for a single passive WeCom req_id.
type StreamSession struct {
	reqID       string
	streamID    string
	chatID      string
	startedAt   time.Time
	minInterval time.Duration
	send        StreamSendFunc

	mu         sync.Mutex
	finished   bool
	lastSent   string
	lastSentAt time.Time
	lastThink  string
}

// NewStreamSession creates a passive stream bound to reqID with a unique stream_id.
func NewStreamSession(reqID, chatID string, send StreamSendFunc) *StreamSession {
	n := streamSeq.Add(1)
	return &StreamSession{
		reqID:       strings.TrimSpace(reqID),
		streamID:    fmt.Sprintf("stream_%d_%d", time.Now().Unix(), n),
		chatID:      strings.TrimSpace(chatID),
		startedAt:   time.Now(),
		minInterval: DefaultStreamThrottle,
		send:        send,
	}
}

func (s *StreamSession) ReqID() string        { return s.reqID }
func (s *StreamSession) StreamID() string     { return s.streamID }
func (s *StreamSession) ChatID() string       { return s.chatID }
func (s *StreamSession) StartedAt() time.Time { return s.startedAt }

// SetMinInterval adjusts progress frame coalescing (useful in unit tests).
func (s *StreamSession) SetMinInterval(d time.Duration) {
	s.mu.Lock()
	s.minInterval = d
	s.mu.Unlock()
}

// Finished reports whether the stream has already sent its terminal finish:true frame.
func (s *StreamSession) Finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// SendProgress writes an interim finish:false frame, respecting throttling and deduplication.
func (s *StreamSession) SendProgress(ctx context.Context, think, body string, force bool) error {
	if s == nil || s.send == nil || s.reqID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil
	}
	if strings.TrimSpace(think) != "" {
		s.lastThink = think
	} else if s.lastThink != "" {
		think = s.lastThink
	}
	content := FormatStreamContent(think, body)
	if content == "" || content == s.lastSent {
		return nil
	}
	if !force && !s.lastSentAt.IsZero() && s.minInterval > 0 && time.Since(s.lastSentAt) < s.minInterval {
		return nil
	}
	if err := s.send(ctx, s.reqID, s.streamID, content, false); err != nil {
		return err
	}
	s.lastSent = content
	s.lastSentAt = time.Now()
	return nil
}

// Finish seals the stream at most once with finish:true.
// If think is empty, it preserves the latest collected thinking block so WeCom keeps the
// collapsible "已深度思考 >" header above the final Markdown answer.
func (s *StreamSession) Finish(ctx context.Context, think, body string) (bool, error) {
	if s == nil || s.send == nil || s.reqID == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return false, nil
	}
	s.finished = true
	if strings.TrimSpace(think) == "" {
		think = s.lastThink
	}
	content := FormatStreamContent(think, body)
	if content == "" {
		content = FormatStreamContent("已完成处理", "已处理。")
	}
	err := s.send(ctx, s.reqID, s.streamID, content, true)
	if err == nil {
		s.lastSent = content
		s.lastSentAt = time.Now()
	}
	return true, err
}

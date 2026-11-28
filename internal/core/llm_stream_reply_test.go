package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestReplyUserTextStreamExtractor(t *testing.T) {
	cases := []struct {
		name      string
		chunks    []string
		byteByOne bool
		want      string
	}{
		{
			name:   "text as first key",
			chunks: []string{`{"text":"你好，`, `世界！\n第二行"}`},
			want:   "你好，世界！\n第二行",
		},
		{
			name:   "text after work_id, choices array, and bool fields",
			chunks: []string{`{"work_id":"wi_9","check":false,"choices":["方案A","方案B"],"text":"请选择`, `执行方案：\"A\" 或 \"B\""}`},
			want:   "请选择执行方案：\"A\" 或 \"B\"",
		},
		{
			name:      "unicode escapes and emoji surrogate pair split 1 byte at a time",
			chunks:    []string{`{"title":"t","text":"\u4f60\u597d \uD83D\uDE80 \t\\ok"}`},
			byteByOne: true,
			want:      "你好 🚀 \t\\ok",
		},
		{
			name:   "ignores nested object text key before top-level text",
			chunks: []string{`{"meta":{"text":"nested-ignore","arr":["a",{"text":"also-ignore"}]},"text":"top-level-real"}`},
			want:   "top-level-real",
		},
		{
			name:   "handles escaped quotes inside skipped string before text",
			chunks: []string{`{"title":"包含\"引号\"的标题","text":"真正回复内容"}`},
			want:   "真正回复内容",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p replyUserTextStreamExtractor
			if tc.byteByOne {
				full := strings.Join(tc.chunks, "")
				for i := 0; i < len(full); i++ {
					p.Feed(full[i : i+1])
				}
			} else {
				for _, ch := range tc.chunks {
					p.Feed(ch)
				}
			}
			if got := p.Text(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type liveRecordingSink struct {
	recordingAPISink
	liveMu     sync.Mutex
	liveEvents []model.RouterLiveView
}

func (s *liveRecordingSink) OnAPIRouterLive(_ context.Context, live model.RouterLiveView) {
	s.liveMu.Lock()
	s.liveEvents = append(s.liveEvents, live)
	s.liveMu.Unlock()
}

func TestAPIRouterLiveProgressAndStreamingReplyDraft(t *testing.T) {
	var (
		mu   sync.Mutex
		step int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		step++
		cur := step
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if cur == 1 {
			// Step 1: read_detail tool call -> emits live progress step.
			chunks := []string{
				`data: {"id":"s1","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"tc1","type":"function","function":{"name":"manage_context","arguments":"{\"action\":\"inspect\"}"}}]}}]}`,
				`data: {"id":"s1","model":"hy4","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
				`data: [DONE]`,
			}
			for _, c := range chunks {
				_, _ = w.Write([]byte(c + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		// Step 2: chunked reply_user arguments spaced >110ms apart so live draft frames emit.
		chunks := []string{
			`data: {"id":"s2","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"tc2","type":"function","function":{"name":"reply_user","arguments":"{\"text\":\"第一段流式草稿"}}]}}]}`,
			`data: {"id":"s2","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"，继续输出第二段。\"}"}}]}}]}`,
			`data: {"id":"s2","model":"hy4","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
			`data: [DONE]`,
		}
		for i, c := range chunks {
			if i == 1 {
				time.Sleep(120 * time.Millisecond)
			}
			_, _ = w.Write([]byte(c + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	sink := &liveRecordingSink{}
	eng.SetSink(sink)

	ctx := context.Background()
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "流式测试"})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	sink.liveMu.Lock()
	events := append([]model.RouterLiveView(nil), sink.liveEvents...)
	sink.liveMu.Unlock()

	sawProgress, sawDraft, sawInactive := false, false, false
	for _, ev := range events {
		if len(ev.Progress) > 0 {
			sawProgress = true
		}
		if strings.Contains(ev.DraftText, "第一段流式草稿") {
			sawDraft = true
		}
		if !ev.Active {
			sawInactive = true
		}
	}
	if !sawProgress || !sawDraft || !sawInactive {
		t.Fatalf("expected progress=%v draft=%v inactive=%v; got events=%+v", sawProgress, sawDraft, sawInactive, events)
	}
}

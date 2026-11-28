package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

type capturedStreamFrame struct {
	reqID    string
	streamID string
	content  string
	finish   bool
}

func openWeComStreamTestEnv(t *testing.T) (*core.Engine, *Bus, *[]capturedStreamFrame, *[]string, *sync.Mutex) {
	eng, _, bus, streams, markdowns, mu := openWeComStreamTestEnvWithStore(t)
	return eng, bus, streams, markdowns, mu
}

func openWeComStreamTestEnvWithStore(t *testing.T) (*core.Engine, *store.Store, *Bus, *[]capturedStreamFrame, *[]string, *sync.Mutex) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng, err := core.NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	bus := NewBus(eng, nil, nil)
	var mu sync.Mutex
	var streams []capturedStreamFrame
	var markdowns []string
	bus.sendStreamFn = func(_ context.Context, reqID, streamID, content string, finish bool) error {
		mu.Lock()
		streams = append(streams, capturedStreamFrame{reqID: reqID, streamID: streamID, content: content, finish: finish})
		mu.Unlock()
		return nil
	}
	bus.sendFn = func(_ context.Context, _, md string) error {
		mu.Lock()
		markdowns = append(markdowns, md)
		mu.Unlock()
		return nil
	}
	bus.sendMarkdownACK = bus.sendFn
	return eng, st, bus, &streams, &markdowns, &mu
}

func TestWeComRouterStreamSealsOnReplyWithoutDuplicateMarkdown(t *testing.T) {
	eng, bus, streams, markdowns, mu := openWeComStreamTestEnv(t)
	ctx := context.Background()

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-router-1",
		MsgID:    "msg-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "帮我看下今天线上延迟",
	})

	mu.Lock()
	if len(*streams) != 1 || (*streams)[0].finish || !strings.Contains((*streams)[0].content, "<think>") {
		t.Fatalf("expected immediate initial thinking stream frame, got %+v", *streams)
	}
	mu.Unlock()

	// Router replies via OnReply -> should seal the passive stream and NOT send duplicate markdown.
	bus.OnReply(ctx, &model.Entry{
		ID:        "e1",
		Thread:    model.MainThread,
		Text:      "已派给 MacBook 上的 agy 排查。",
		WorkerRef: &model.EntryWorkerRef{Name: "latency-check", Machine: "mac-1"},
	}, nil, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(*markdowns) != 0 {
		t.Fatalf("expected 0 duplicate active markdown sends, got %+v", *markdowns)
	}
	if len(*streams) != 2 {
		t.Fatalf("expected 2 stream frames (init + finish), got %d: %+v", len(*streams), *streams)
	}
	fin := (*streams)[1]
	if !fin.finish || fin.reqID != "req-router-1" || !strings.Contains(fin.content, "已派给 MacBook 上的 agy 排查。") || strings.Contains(fin.content, "[#latency-check]") {
		t.Fatalf("expected clean Corgi final stream without [#worker] prefix, got: %+v", fin)
	}
	_ = eng
}

func TestWeComConsecutiveMessagesKeepIndependentFIFOStreams(t *testing.T) {
	_, bus, streams, markdowns, mu := openWeComStreamTestEnv(t)
	ctx := context.Background()

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-1",
		MsgID:    "msg-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "原型什么进展了",
	})
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-2",
		MsgID:    "msg-2",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "太湖最新方案是什么",
	})

	// Neither stream is prematurely sealed with "已收到补充消息，将在下一条合并回复".
	mu.Lock()
	if len(*streams) != 2 || (*streams)[0].finish || (*streams)[1].finish {
		t.Fatalf("expected 2 open interim streams without premature merge notice, got %+v", *streams)
	}
	mu.Unlock()

	// Router replies independently to question 1 then question 2 -> strict 1-to-1 FIFO stream sealing!
	bus.OnReply(ctx, &model.Entry{ID: "e1", Thread: model.MainThread, Text: "原型制作中"}, nil, nil)
	bus.OnReply(ctx, &model.Entry{ID: "e2", Thread: model.MainThread, Text: "太湖推荐方案 A"}, nil, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(*markdowns) != 0 {
		t.Fatalf("expected 0 fallback markdowns when 2 streams seal 1-to-1, got %+v", *markdowns)
	}
	if len(*streams) != 4 {
		t.Fatalf("expected 4 frames (req-1 init, req-2 init, req-1 fin, req-2 fin), got %d: %+v", len(*streams), *streams)
	}
	if (*streams)[2].reqID != "req-1" || !(*streams)[2].finish || !strings.Contains((*streams)[2].content, "原型制作中") {
		t.Fatalf("expected first reply to seal req-1, got %+v", (*streams)[2])
	}
	if (*streams)[3].reqID != "req-2" || !(*streams)[3].finish || !strings.Contains((*streams)[3].content, "太湖推荐方案 A") {
		t.Fatalf("expected second reply to seal req-2, got %+v", (*streams)[3])
	}
}

func TestWeComFocusReturnMainAndPickerFallback(t *testing.T) {
	_, bus, streams, _, mu := openWeComStreamTestEnv(t)
	ctx := context.Background()

	key := wecomFocusKey("single", "U1", "U1")
	bus.saveWeComFocus(ctx, key, &wecomFocus{Worker: "alpha", Machine: "mac-1"})
	if f := bus.loadWeComFocus(ctx, key); f == nil || f.Worker != "alpha" {
		t.Fatalf("expected persisted focus alpha, got %+v", f)
	}

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-help",
		MsgID:    "msg-help",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "/help",
	})
	mu.Lock()
	if len(*streams) != 1 || !(*streams)[0].finish || !strings.Contains((*streams)[0].content, "直聊中") || !strings.Contains((*streams)[0].content, "/back") {
		t.Fatalf("expected instant /help showing active worker + /back, got %+v", *streams)
	}
	if strings.Contains((*streams)[0].content, "不经过 Corgi") || strings.Contains((*streams)[0].content, "不经 Corgi") {
		t.Fatalf("/help text violated copy redline: %q", (*streams)[0].content)
	}
	mu.Unlock()

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-back",
		MsgID:    "msg-back",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "回主会话",
	})

	if f := bus.loadWeComFocus(ctx, key); f != nil {
		t.Fatalf("expected focus cleared after 回主对话, got %+v", f)
	}
	mu.Lock()
	if len(*streams) != 2 || !(*streams)[1].finish || !strings.Contains((*streams)[1].content, "Corgi 主对话") {
		t.Fatalf("expected instant sealed confirmation for 回主会话, got %+v", *streams)
	}
	mu.Unlock()
}

func TestApplyMsgPatchAndFormatProgressThink(t *testing.T) {
	msgMap := map[int]*model.Message{}
	applyMsgPatch(msgMap, &model.MsgPatch{
		Base: 10,
		From: 10,
		Msgs: []*model.Message{
			{Kind: "user", Text: "旧问题"},
			{Kind: "assistant", Text: "旧回答"},
		},
	})
	baseline := 12
	applyMsgPatch(msgMap, &model.MsgPatch{
		Base: 10,
		From: 12,
		Msgs: []*model.Message{
			{Kind: "user", Text: "新问题"},
			{Kind: "thinking", Text: "先查数据库慢查询日志"},
			{Kind: "tool", Name: "run_command", Input: "go test ./..."},
			{Kind: "tool", Name: "result", Output: "ok"},
		},
	})

	delta := orderedMessagesSince(msgMap, baseline)
	think := formatProgressThink(sliceAfterLastUser(delta, 0), "默认占位")
	if !strings.Contains(think, "先查数据库慢查询日志") || !strings.Contains(think, "执行命令：go test ./...") {
		t.Fatalf("unexpected formatted progress think: %q", think)
	}
	if strings.Contains(think, "旧问题") || strings.Contains(think, "旧回答") {
		t.Fatalf("old history leaked into progress think: %q", think)
	}
}

func TestRouterStreamNotPrematurelySealedAndCapturesFinalThink(t *testing.T) {
	_, bus, streams, markdowns, mu := openWeComStreamTestEnv(t)
	ctx := context.Background()

	// Write a temporary CodeBuddy transcript with prior history + a fast turn's reasoning & Bash description.
	logPath := filepath.Join(t.TempDir(), "router.jsonl")
	initial := `{"type":"message","role":"user","content":[{"type":"input_text","text":"old question"}]}` + "\n" +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}` + "\n"
	if err := os.WriteFile(logPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-single-bubble",
		MsgID:    "msg-single-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "当前有哪些事项",
	})
	// Duplicate MsgID retry must be ignored (no second stream!).
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-single-bubble-dup",
		MsgID:    "msg-single-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "当前有哪些事项",
	})

	// Attach the transcript reader baseline as Hub would, then append fast-turn reasoning + Bash call.
	bus.mu.Lock()
	var st *activeWeComStream
	if q := bus.routerStreams[model.MainThread]; len(q) > 0 {
		st = q[0]
	}
	bus.mu.Unlock()
	if st == nil {
		t.Fatal("expected active router stream")
	}
	st.pollMu.Lock()
	st.locateFn = func(context.Context) (string, agent.Adapter) { return logPath, &agent.CodeBuddyAdapter{} }
	st.pollMu.Unlock()
	st.attachLocalTranscript(ctx, true)

	turnLines := `{"type":"message","role":"user","content":[{"type":"input_text","text":"当前有哪些事项"}]}` + "\n" +
		`{"type":"function_call","name":"Bash","arguments":{"command":"/usr/local/bin/corgi list","description":"查看当前事项与状态"}}` + "\n" +
		`{"type":"reasoning","content":[],"rawContent":[{"type":"reasoning_text","text":"整理 4 项待处理事项给用户"}]}` + "\n"
	if err := os.WriteFile(logPath, []byte(initial+turnLines), 0o600); err != nil {
		t.Fatal(err)
	}

	bus.OnReply(ctx, &model.Entry{
		ID:     "e-single",
		Thread: model.MainThread,
		Text:   "当前有 4 项待处理。",
	}, nil, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(*markdowns) != 0 {
		t.Fatalf("expected 0 fallback markdown bubbles, got %+v", *markdowns)
	}
	if len(*streams) != 2 {
		t.Fatalf("expected exactly 2 frames (init + sealed final), got %d: %+v", len(*streams), *streams)
	}
	fin := (*streams)[1]
	if !fin.finish || fin.reqID != "req-single-bubble" {
		t.Fatalf("unexpected final frame metadata: %+v", fin)
	}
	if !strings.Contains(fin.content, "• 查看当前事项与状态") || !strings.Contains(fin.content, "整理 4 项待处理事项给用户") {
		t.Fatalf("expected real tool description + reasoning in final <think>, got: %q", fin.content)
	}
	if !strings.Contains(fin.content, "</think>\n当前有 4 项待处理。") {
		t.Fatalf("expected final answer right after </think>, got: %q", fin.content)
	}
}

func TestLongSessionBeyond60MessagesStillStreamsLiveThink(t *testing.T) {
	_, bus, _, _, _ := openWeComStreamTestEnv(t)
	ctx := context.Background()

	// Seed 80 historical messages (> TranscriptReader's 60-message sliding View window).
	logPath := filepath.Join(t.TempDir(), "long-worker.jsonl")
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hist q"}]}` + "\n")
		sb.WriteString(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hist a"}]}` + "\n")
	}
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	st := &activeWeComStream{
		session:   wecom.NewStreamSession("req-long", "U1", bus.sendStreamFn),
		initThink: "初始占位",
		locateFn:  func(context.Context) (string, agent.Adapter) { return logPath, &agent.CodeBuddyAdapter{} },
	}
	st.attachLocalTranscript(ctx, true)

	// Before any new turn arrives, think must stay at fallback (no leaked history).
	if think, _ := st.pollLocalThink(ctx, false); think != "初始占位" {
		t.Fatalf("expected fallback before new turn, got: %q", think)
	}

	// Append a brand-new user turn + reasoning + tool call beyond the 60-message window.
	sb.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"太湖站点可以复用吗"}]}` + "\n")
	sb.WriteString(`{"type":"reasoning","content":[],"rawContent":[{"type":"reasoning_text","text":"正在核对太湖应用令牌和站点复用机制"}]}` + "\n")
	sb.WriteString(`{"type":"function_call","name":"Read","arguments":{"file_path":"/data/workbench/app/docs/self-hosting.md"}}` + "\n")
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	think, polled := st.pollLocalThink(ctx, false)
	if !polled {
		t.Fatal("expected pollLocalThink to detect newly appended lines")
	}
	if !strings.Contains(think, "正在核对太湖应用令牌和站点复用机制") || !strings.Contains(think, "读取文件：/data/workbench/app/docs/self-hosting.md") {
		t.Fatalf("expected live reasoning + tool call beyond 60-msg window, got: %q", think)
	}
	if strings.Contains(think, "hist q") || strings.Contains(think, "hist a") {
		t.Fatalf("historical messages leaked into think: %q", think)
	}
}

func TestWeComQuoteMainChatWhileFocusedRoutesOnceToRouter(t *testing.T) {
	_, st, bus, streams, _, mu := openWeComStreamTestEnvWithStore(t)
	ctx := context.Background()

	// Seed a plain main-chat Corgi reply (no WorkerRef) and focus user on worker "taihu-plan".
	if err := st.InsertEntry(ctx, &model.Entry{
		ID:     "e-main-1",
		Thread: model.MainThread,
		Role:   "corgi",
		Text:   "当前有 **4 项**待处理事项，其中太湖方案待审阅。",
	}); err != nil {
		t.Fatal(err)
	}
	focusKey := wecomFocusKey("single", "U1", "U1")
	bus.saveWeComFocus(ctx, focusKey, &wecomFocus{Worker: "taihu-plan", Title: "太湖方案"})

	// While focused on taihu-plan, user quotes the plain main-chat Corgi reply (even without markdown
	// and with WeCom truncation) -> should route ONCE to Corgi Router without clearing saved focus.
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:     "req-quote-main",
		MsgID:     "msg-quote-main",
		ChatType:  "single",
		ChatID:    "U1",
		FromUser:  "U1",
		Text:      "帮我把第 2 项先归档",
		QuoteText: "当前有 4 项待处理事项，其中太湖方案...",
	})

	// Focus must remain intact on taihu-plan.
	if f := bus.loadWeComFocus(ctx, focusKey); f == nil || f.Worker != "taihu-plan" {
		t.Fatalf("expected saved focus taihu-plan untouched, got %+v", f)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*streams) != 1 || (*streams)[0].reqID != "req-quote-main" || !strings.Contains((*streams)[0].content, "正在思考并分析你的请求") {
		t.Fatalf("expected message routed to Corgi Router stream, got %+v", *streams)
	}

	// Verify direct-worker prompt formatting carries quote context cleanly.
	got := formatWeComDirectPrompt("  第 2 步  改成只读模式  ", "按这个改")
	if got != "[引用：第 2 步 改成只读模式]\n按这个改" {
		t.Fatalf("unexpected direct quote prompt: %q", got)
	}
	if plain := formatWeComDirectPrompt("", "直接说"); plain != "直接说" {
		t.Fatalf("unexpected plain direct prompt: %q", plain)
	}
}

func TestWeComFocusTTLAutoReturnLegacyAndEnded(t *testing.T) {
	_, st, bus, streams, _, mu := openWeComStreamTestEnvWithStore(t)
	ctx := context.Background()
	now := time.Now()
	focusKey := wecomFocusKey("single", "U1", "U1")

	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Seed an ended worker for the ended-worker test case.
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "stopped-w", Kind: "agy", Cwd: "/tmp", Thread: model.MainThread,
		Status: "done", Ended: true, Title: "已停任务", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name            string
		seedKV          string
		msgID           string
		reqID           string
		wantNoticeSub   string
		wantNoNoticeSub bool
	}{
		{
			name: "expired 60m TTL auto-returns to Corgi and prepends one-time notice",
			seedKV: mustJSON(core.WeComFocus{
				Worker:    "taihu-plan",
				Title:     "太湖方案",
				EnteredAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-65 * time.Minute),
			}),
			msgID:         "msg-ttl-1",
			reqID:         "req-ttl-1",
			wantNoticeSub: "> 已超过 60m 未直聊，已自动回到 Corgi 主对话（原直聊 #taihu-plan）",
		},
		{
			name:            "legacy focus without UpdatedAt silently expires to Corgi",
			seedKV:          `{"worker":"legacy-w","title":"旧直聊"}`,
			msgID:           "msg-legacy-1",
			reqID:           "req-legacy-1",
			wantNoNoticeSub: true,
		},
		{
			name: "ended worker focus auto-returns to Corgi without reviving worker",
			seedKV: mustJSON(core.WeComFocus{
				Worker:    "stopped-w",
				Title:     "已停任务",
				EnteredAt: now.Add(-5 * time.Minute),
				UpdatedAt: now.Add(-1 * time.Minute),
			}),
			msgID:         "msg-ended-1",
			reqID:         "req-ended-1",
			wantNoticeSub: "> 原直聊会话 #stopped-w 已结束，已自动回到 Corgi 主对话",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			*streams = nil
			mu.Unlock()

			if err := st.SetKV(ctx, focusKey, tc.seedKV); err != nil {
				t.Fatal(err)
			}
			bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
				ReqID:    tc.reqID,
				MsgID:    tc.msgID,
				ChatType: "single",
				ChatID:   "U1",
				FromUser: "U1",
				Text:     "现在整体进展怎么样？",
			})

			// Focus KV must be cleaned.
			if f := bus.loadWeComFocus(ctx, focusKey); f != nil {
				t.Fatalf("expected expired focus cleared, got %+v", f)
			}

			// First Corgi reply seals the router stream with the auto-return notice (if any).
			bus.OnReply(ctx, &model.Entry{ID: "e-" + tc.msgID, Thread: model.MainThread, Text: "整体正常。"}, nil, nil)

			mu.Lock()
			got := append([]capturedStreamFrame(nil), (*streams)...)
			mu.Unlock()
			if len(got) != 2 || !got[1].finish {
				t.Fatalf("expected 2 router stream frames (init + finish), got %+v", got)
			}
			if tc.wantNoticeSub != "" && !strings.Contains(got[1].content, tc.wantNoticeSub) {
				t.Fatalf("expected notice %q in %q", tc.wantNoticeSub, got[1].content)
			}
			if tc.wantNoNoticeSub && strings.Contains(got[1].content, "已自动回到") {
				t.Fatalf("expected silent legacy upgrade, got %q", got[1].content)
			}
			if !strings.Contains(got[1].content, "整体正常。") {
				t.Fatalf("missing Corgi reply text in %q", got[1].content)
			}

			// Second Corgi reply must NOT repeat the notice.
			if gotNotice := bus.takePendingFocusNotice(model.MainThread); gotNotice != "" {
				t.Fatalf("expected pending focus notice consumed on first reply, still got %q", gotNotice)
			}
		})
	}
}

func TestFormatDirectReplyBodyHeaderFooterTruncationAndRedline(t *testing.T) {
	cases := []struct {
		name              string
		title             string
		worker            string
		machine           string
		fullText          string
		previewURL        string
		quotaErr          string
		inFocus           bool
		curFocus          string
		maxBytes          int
		wantHeader        string
		wantFooter        string
		wantPreviewLink   bool
		wantTruncatedNote bool
	}{
		{
			name:       "focus mode with distinct title",
			title:      "会话身份提醒",
			worker:     "agy-3637",
			machine:    "MacBook",
			fullText:   "已更新前端顶栏和提示条。",
			inFocus:    true,
			curFocus:   "agy-3637",
			wantHeader: "↳ 直聊 · 会话身份提醒（#agy-3637 · MacBook）",
			wantFooter: "> **↳ 直聊中 · 会话身份提醒（#agy-3637）** · 发 /back 回 Corgi · /w 切换",
		},
		{
			name:       "one-shot quote while in main chat",
			title:      "",
			worker:     "api-profiler",
			machine:    "Hub",
			fullText:   "慢查询已定位到 orders 表。",
			inFocus:    false,
			curFocus:   "",
			wantHeader: "↳ 直聊 · #api-profiler（Hub）",
			wantFooter: "> ↳ 单次直问 · #api-profiler · 当前仍在 Corgi 主对话",
		},
		{
			name:       "one-shot quote of another worker while focused",
			title:      "",
			worker:     "worker-b",
			machine:    "Hub",
			fullText:   "B 已核对。",
			inFocus:    false,
			curFocus:   "worker-a",
			wantHeader: "↳ 直聊 · #worker-b（Hub）",
			wantFooter: "> ↳ 单次直问 · #worker-b · 当前焦点仍在 #worker-a（发 /back 回 Corgi）",
		},
		{
			name:              "over-budget body clips middle prose while preserving header, preview link, and footer",
			title:             "长报告直聊",
			worker:            "report-w",
			machine:           "Hub",
			fullText:          strings.Repeat("这是一段超长直聊回复内容，用于验证字节裁剪后尾注绝不丢失。", 500),
			previewURL:        "https://corgi.example/p/tok123",
			inFocus:           true,
			curFocus:          "report-w",
			maxBytes:          1200,
			wantHeader:        "↳ 直聊 · 长报告直聊（#report-w · Hub）",
			wantFooter:        "> **↳ 直聊中 · 长报告直聊（#report-w）** · 发 /back 回 Corgi · /w 切换",
			wantPreviewLink:   true,
			wantTruncatedNote: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatDirectReplyBody(tc.title, tc.worker, tc.machine, tc.fullText, tc.previewURL, tc.quotaErr, tc.inFocus, tc.curFocus, tc.maxBytes)
			if !strings.HasPrefix(out, tc.wantHeader) {
				t.Fatalf("header mismatch:\ngot:  %q\nwant prefix: %q", out, tc.wantHeader)
			}
			if !strings.HasSuffix(out, tc.wantFooter) {
				t.Fatalf("footer mismatch:\ngot:  %q\nwant suffix: %q", out, tc.wantFooter)
			}
			if tc.maxBytes > 0 && len(out) > tc.maxBytes {
				t.Fatalf("output len=%d exceeded maxBytes=%d", len(out), tc.maxBytes)
			}
			if tc.wantPreviewLink && !strings.Contains(out, "🔗 [在线预览 · 完整报告]("+tc.previewURL+")") {
				t.Fatalf("preview link lost during truncation: %q", out)
			}
			if tc.wantTruncatedNote && !strings.Contains(out, "内容过长") {
				t.Fatalf("expected truncation notice in %q", out)
			}
			if strings.Contains(out, "不经过 Corgi") || strings.Contains(out, "不经 Corgi") {
				t.Fatalf("direct reply violated copy redline: %q", out)
			}
			// Round-trip verify: quoting either the header or footer extracts the exact worker handle.
			if gotTag := core.ExtractQuoteWorkerTag(tc.wantHeader); gotTag != tc.worker {
				t.Fatalf("ExtractQuoteWorkerTag(header %q) = %q, want %q", tc.wantHeader, gotTag, tc.worker)
			}
			if gotTag := core.ExtractQuoteWorkerTag(tc.wantFooter); gotTag != tc.worker {
				t.Fatalf("ExtractQuoteWorkerTag(footer %q) = %q, want %q", tc.wantFooter, gotTag, tc.worker)
			}
		})
	}
}

func TestWeComQuoteCorgiWorkerRefReportNeverLeaksToWorker(t *testing.T) {
	_, st, bus, streams, _, mu := openWeComStreamTestEnvWithStore(t)
	ctx := context.Background()

	// Seed a Corgi reply with WorkerRef (report forwarded by Corgi).
	if err := st.InsertEntry(ctx, &model.Entry{
		ID:        "e-report-1",
		Thread:    model.MainThread,
		Role:      "corgi",
		Text:      "api-profiler 查完了，建议给 orders 表加联合索引。",
		WorkerRef: &model.EntryWorkerRef{Name: "api-profiler"},
	}); err != nil {
		t.Fatal(err)
	}
	focusKey := wecomFocusKey("single", "U1", "U1")
	bus.saveWeComFocus(ctx, focusKey, &wecomFocus{Worker: "other-w", Title: "另一个任务"})

	// 1. While focused on other-w, quoting Corgi's WorkerRef report routes ONCE to Corgi Router
	// without losing focus on other-w.
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:     "req-quote-report",
		MsgID:     "msg-quote-report",
		ChatType:  "single",
		ChatID:    "U1",
		FromUser:  "U1",
		Text:      "把 api-profiler 停掉吧",
		QuoteText: "api-profiler 查完了，建议给 orders 表加联合索引。",
	})
	if f := bus.loadWeComFocus(ctx, focusKey); f == nil || f.Worker != "other-w" {
		t.Fatalf("expected focus other-w preserved, got %+v", f)
	}
	mu.Lock()
	if len(*streams) != 1 || (*streams)[0].reqID != "req-quote-report" || !strings.Contains((*streams)[0].content, "正在思考并分析你的请求") {
		t.Fatalf("expected Corgi report quote routed to Router, got %+v", *streams)
	}
	*streams = nil
	mu.Unlock()

	// 2. Even quoting a historical legacy "[#api-profiler] ..." message in main chat goes to Corgi Router.
	bus.saveWeComFocus(ctx, focusKey, nil)
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:     "req-legacy-bracket",
		MsgID:     "msg-legacy-bracket",
		ChatType:  "single",
		ChatID:    "U1",
		FromUser:  "U1",
		Text:      "帮我归档这个",
		QuoteText: "[#api-profiler] 发现缺索引",
	})
	mu.Lock()
	defer mu.Unlock()
	if len(*streams) != 1 || (*streams)[0].reqID != "req-legacy-bracket" || !strings.Contains((*streams)[0].content, "正在思考并分析你的请求") {
		t.Fatalf("expected legacy [#worker] quote routed to Corgi Router, got %+v", *streams)
	}
}

func TestDirectWorkerThinkPrefixAndLocalRemoteTimeoutConsistent(t *testing.T) {
	eng, _, bus, streams, _, mu := openWeComStreamTestEnvWithStore(t)
	ctx := context.Background()

	// 1. Think prefix [直聊 #worker] is preserved on line 1 even after real tool/thinking lines arrive.
	logPath := filepath.Join(t.TempDir(), "worker.jsonl")
	content := `{"type":"message","role":"user","content":[{"type":"input_text","text":"查日志"}]}` + "\n" +
		`{"type":"function_call","name":"Bash","arguments":{"command":"tail -n 20 app.log","description":"查看最近日志"}}` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	st, watchCtx := bus.openWorkerStream(ctx, model.MainThread, "U1", "req-think", "log-worker", "日志排查", true, "log-worker")
	if st == nil || watchCtx == nil {
		t.Fatal("expected openWorkerStream to succeed")
	}
	st.pollMu.Lock()
	st.locateFn = func(context.Context) (string, agent.Adapter) { return logPath, &agent.CodeBuddyAdapter{} }
	st.pollMu.Unlock()
	st.attachLocalTranscript(ctx, false)
	think, _ := st.pollLocalThink(ctx, false)
	if !strings.HasPrefix(think, "[直聊 #log-worker]\n") || !strings.Contains(think, "查看最近日志") {
		t.Fatalf("expected [直聊 #log-worker] preserved above tool steps, got %q", think)
	}
	st.abort()

	// 2. Local worker timeout seal carries dedicated direct header + footer.
	mu.Lock()
	*streams = nil
	mu.Unlock()
	localSt, localCtx := bus.openWorkerStream(ctx, model.MainThread, "U1", "req-local-to", "local-w", "本地直聊", true, "local-w")
	localTimer := time.NewTimer(10 * time.Millisecond)
	bus.watchLocalWorkerTranscript(localCtx, localTimer, localSt, "local-w", "", nil)
	localTimer.Stop()

	mu.Lock()
	var localFin *capturedStreamFrame
	for i := range *streams {
		if (*streams)[i].finish {
			localFin = &(*streams)[i]
		}
	}
	mu.Unlock()
	if localFin == nil || !strings.Contains(localFin.content, "↳ 直聊 · #local-w 仍在后台处理中，完成后会自动发给你。") ||
		!strings.Contains(localFin.content, "↳ 直聊中 · 本地直聊（#local-w）") {
		t.Fatalf("unexpected local timeout seal: %+v", localFin)
	}

	// 3. Remote worker timeout seals (both nil-fleet and offline-remote paths) carry identical direct header + footer.
	for _, withFleet := range []bool{false, true} {
		if withFleet {
			eng.SetFleet(fleet.NewFleet(nil, "hub", t.TempDir(), "test", []string{"claude"}))
		} else {
			eng.SetFleet(nil)
		}
		mu.Lock()
		*streams = nil
		mu.Unlock()
		remSt, remCtx := bus.openWorkerStream(ctx, model.MainThread, "U1", "req-remote-to", "remote-w", "远程直聊", true, "remote-w")
		remTimer := time.NewTimer(10 * time.Millisecond)
		bus.watchRemoteWorkerSSE(remCtx, remTimer, remSt, "remote-w", "mac-offline", "w1:p1@mac-offline", remSt.initThink)
		remTimer.Stop()

		mu.Lock()
		var remFin *capturedStreamFrame
		for i := range *streams {
			if (*streams)[i].finish {
				remFin = &(*streams)[i]
			}
		}
		mu.Unlock()
		if remFin == nil || !strings.Contains(remFin.content, "↳ 直聊 · #remote-w 仍在后台处理中，完成后会自动发给你。") ||
			!strings.Contains(remFin.content, "↳ 直聊中 · 远程直聊（#remote-w）") {
			t.Fatalf("unexpected remote timeout seal (withFleet=%v): %+v", withFleet, remFin)
		}
	}
}

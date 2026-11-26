package channel

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
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
	return eng, bus, &streams, &markdowns, &mu
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
	if !fin.finish || fin.reqID != "req-router-1" || !strings.Contains(fin.content, "[#latency-check] 已派给 MacBook 上的 agy 排查。") {
		t.Fatalf("unexpected final stream frame: %+v", fin)
	}
	_ = eng
}

func TestWeComConsecutiveMessagesSealPreviousRouterStream(t *testing.T) {
	_, bus, streams, _, mu := openWeComStreamTestEnv(t)
	ctx := context.Background()

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-1",
		MsgID:    "msg-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "第一问",
	})
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-2",
		MsgID:    "msg-2",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "补充一句",
	})

	mu.Lock()
	defer mu.Unlock()
	if len(*streams) != 3 {
		t.Fatalf("expected 3 frames (req-1 init, req-1 seal, req-2 init), got %d: %+v", len(*streams), *streams)
	}
	if (*streams)[1].reqID != "req-1" || !(*streams)[1].finish {
		t.Fatalf("expected req-1 to be sealed before opening req-2, got %+v", (*streams)[1])
	}
	if (*streams)[2].reqID != "req-2" || (*streams)[2].finish {
		t.Fatalf("expected req-2 open interim frame, got %+v", (*streams)[2])
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
		ReqID:    "req-back",
		MsgID:    "msg-back",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "回主对话",
	})

	if f := bus.loadWeComFocus(ctx, key); f != nil {
		t.Fatalf("expected focus cleared after 回主对话, got %+v", f)
	}
	mu.Lock()
	if len(*streams) != 1 || !(*streams)[0].finish || !strings.Contains((*streams)[0].content, "Corgi 主对话") {
		t.Fatalf("expected instant sealed confirmation for 回主对话, got %+v", *streams)
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

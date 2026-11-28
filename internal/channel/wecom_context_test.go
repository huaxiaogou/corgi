package channel

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestWeComContextCommands(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := core.NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx := context.Background()
	id, seq, _ := st.NextID(ctx, "rt")
	_, err = st.AppendRouterContextTurn(ctx, &model.RouterContextTurn{
		ID: id, Seq: seq, Thread: model.MainThread, Epoch: 1, Kind: "turn",
		UserText: "用户问", AssistantText: "大脑答", EstTokens: 2500, CreatedAt: time.Now(),
	}, 3000, 2400)
	if err != nil {
		t.Fatalf("seed context turn: %v", err)
	}

	bus := NewBus(eng, nil, nil)
	var (
		mu     sync.Mutex
		frames []string
	)
	bus.sendStreamFn = func(_ context.Context, _, _, content string, finish bool) error {
		if finish {
			mu.Lock()
			frames = append(frames, content)
			mu.Unlock()
		}
		return nil
	}

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		MsgID: "m1", ReqID: "r1", ChatType: "single", FromUser: "owner", Text: "/ctx",
	})
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		MsgID: "m2", ReqID: "r2", ChatType: "single", FromUser: "owner", Text: "/ctx clear",
	})

	mu.Lock()
	got := append([]string(nil), frames...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 instant replies, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "大脑工作上下文") || !strings.Contains(got[0], "缓存命中 80%") {
		t.Fatalf("unexpected /ctx reply: %q", got[0])
	}
	if !strings.Contains(got[1], "已清空大脑工作上下文") {
		t.Fatalf("unexpected /ctx clear reply: %q", got[1])
	}
	info, err := eng.InspectRouterContext(ctx, model.MainThread)
	if err != nil || info.Summary.Epoch != 2 || info.Summary.ActiveTurns != 0 {
		t.Fatalf("expected cleared epoch 2 with 0 active turns, got %+v (err=%v)", info, err)
	}
}

func TestWeComOnAPIRouterLiveStreamsDraftBody(t *testing.T) {
	bus := &Bus{
		routerStreams: map[string][]*activeWeComStream{},
		liveSubs:      map[chan model.RouterLiveView]struct{}{},
	}
	var (
		mu     sync.Mutex
		frames []string
	)
	bus.sendStreamFn = func(_ context.Context, _, _, content string, finish bool) error {
		mu.Lock()
		frames = append(frames, content)
		mu.Unlock()
		return nil
	}
	st := bus.startRouterStream(context.Background(), model.MainThread, "owner", "req-live-1")
	if st == nil {
		t.Fatalf("expected active stream")
	}
	st.session.SetMinInterval(0)
	st.setRefIDs("e1", "t1")

	liveCh, unsub := bus.SubscribeRouterLive()
	defer unsub()

	bus.OnAPIRouterLive(context.Background(), model.RouterLiveView{
		Active:    true,
		Thread:    model.MainThread,
		TriggerID: "t1",
		Progress:  []string{"• 正在查阅事项详情…"},
		DraftText: "正在流式输出回复正文",
	})

	select {
	case got := <-liveCh:
		if got.DraftText != "正在流式输出回复正文" {
			t.Fatalf("unexpected SSE live payload: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for SubscribeRouterLive event")
	}

	mu.Lock()
	gotFrames := append([]string(nil), frames...)
	mu.Unlock()
	if len(gotFrames) < 2 {
		t.Fatalf("expected initial frame + draft stream frame, got %v", gotFrames)
	}
	last := gotFrames[len(gotFrames)-1]
	if !strings.Contains(last, "正在查阅事项详情") || !strings.Contains(last, "正在流式输出回复正文") {
		t.Fatalf("expected WeCom stream frame to contain <think> progress and streaming body, got %q", last)
	}
	st.abort()
}

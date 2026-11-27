package channel

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestBusFlushBeforeRestartWaitsForInFlightAndPreservesTransientFailure(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "v0.1.41", []string{"claude"})
	eng, err := core.NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := eng.Settings()
	cfg.NotifyLevel = "normal"
	_ = eng.SaveConfig(ctx, &cfg, nil)

	bus := NewBus(eng, nil, nil)
	bus.flushDelay = 1 * time.Millisecond

	inSend := make(chan struct{}, 1)
	releaseSend := make(chan struct{})
	var sendCount atomic.Int32
	var mu sync.Mutex
	var bodies []string

	bus.sendFn = func(_ context.Context, _ string, markdown string) error {
		n := sendCount.Add(1)
		if n == 1 {
			inSend <- struct{}{}
			<-releaseSend
			return errors.New("transient wecom network blip")
		}
		mu.Lock()
		bodies = append(bodies, markdown)
		mu.Unlock()
		return nil
	}

	bus.OnReply(ctx, &model.Entry{Thread: model.MainThread, Text: "重要回复不能丢"}, nil, nil)
	time.Sleep(5 * time.Millisecond)

	// Start periodic flush in background and wait until it is mid-send.
	go bus.FlushDue(ctx, false)
	<-inSend

	// Call FlushBeforeRestart concurrently -> must block until in-flight send finishes,
	// AND retry the preserved failed batch so it succeeds before returning!
	done := make(chan error, 1)
	go func() {
		done <- bus.FlushBeforeRestart(ctx)
	}()

	select {
	case <-done:
		t.Fatal("FlushBeforeRestart returned before in-flight send finished")
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseSend)
	if err := <-done; err != nil {
		t.Fatalf("expected FlushBeforeRestart retry to succeed, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0] != "重要回复不能丢" {
		t.Fatalf("expected preserved batch delivered cleanly, got %+v", bodies)
	}
}

func TestDeliveryGatePreservesCausalOrderAcrossWeComWSDisconnect(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := core.NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := eng.Settings()
	cfg.NotifyLevel = "normal"
	_ = eng.SaveConfig(ctx, &cfg, nil)

	bus := NewBus(eng, nil, nil)
	bus.flushDelay = 20 * time.Second

	var (
		mu         sync.Mutex
		wsOnline   = true
		deliveries []string
	)
	bus.sendStreamFn = func(_ context.Context, reqID, _ string, content string, finish bool) error {
		mu.Lock()
		online := wsOnline
		mu.Unlock()
		if !online {
			return errors.New("wecom websocket not connected")
		}
		if finish {
			mu.Lock()
			deliveries = append(deliveries, "stream:"+reqID+":"+content)
			mu.Unlock()
		}
		return nil
	}
	bus.sendFn = func(_ context.Context, _ string, markdown string) error {
		mu.Lock()
		online := wsOnline
		mu.Unlock()
		if !online {
			return errors.New("wecom websocket not connected")
		}
		mu.Lock()
		deliveries = append(deliveries, "md:"+markdown)
		mu.Unlock()
		return nil
	}
	bus.sendMarkdownACK = bus.sendFn

	// 1. User sends message #1 (opens passive stream req-1).
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-1",
		MsgID:    "m-1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "第一个好 上下滑转起来",
	})

	// 2. Simulate 2s WS disconnect right as Router replies e3324 -> seal fails, must queue at FIFO head!
	mu.Lock()
	wsOnline = false
	mu.Unlock()
	bus.OnReply(ctx, &model.Entry{ID: "e3324", Thread: model.MainThread, Text: "第一条确认 e3324"}, nil, nil)

	// 3. WS reconnects; user sends message #2 (opens passive stream req-2), then Router replies e3328.
	mu.Lock()
	wsOnline = true
	mu.Unlock()
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-2",
		MsgID:    "m-2",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "而且不需要折叠",
	})
	bus.OnReply(ctx, &model.Entry{ID: "e3328", Thread: model.MainThread, Text: "第二条确认 e3328"}, nil, nil)

	mu.Lock()
	got := append([]string(nil), deliveries...)
	mu.Unlock()

	// Strict causal order: e3324 MUST arrive BEFORE e3328, each exactly once!
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 ordered deliveries (e3324 then e3328), got %d: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "md:") || !strings.Contains(got[0], "第一条确认 e3324") {
		t.Fatalf("expected e3324 delivered FIRST via active markdown, got %q", got[0])
	}
	if !strings.HasPrefix(got[1], "stream:req-2:") || !strings.Contains(got[1], "第二条确认 e3328") {
		t.Fatalf("expected e3328 delivered SECOND via its stream, got %q", got[1])
	}

	// 4. Verify ACK timeout (context.DeadlineExceeded) is treated as unknown and NOT blindly replayed.
	bus.sendFn = func(_ context.Context, _ string, _ string) error {
		return context.DeadlineExceeded
	}
	bus.sendMarkdownACK = bus.sendFn
	bus.OnReply(ctx, &model.Entry{ID: "e-timeout", Thread: model.MainThread, Text: "超时不盲重放"}, nil, nil)
	if bus.hasOutboxFor("U1") || bus.hasOutboxFor("") {
		t.Fatal("expected unknown/deadline-exceeded delivery NOT to be re-queued for blind replay")
	}
}

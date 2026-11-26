package channel

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

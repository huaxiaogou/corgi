package channel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestOutboxCoalescingAndQuietSuppression(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := core.NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu   sync.Mutex
		sent []struct{ chat, body string }
	)
	bus := NewBus(eng, nil, nil)
	bus.flushDelay = 10 * time.Millisecond
	bus.sendFn = func(_ context.Context, chatID, markdown string) error {
		mu.Lock()
		sent = append(sent, struct{ chat, body string }{chatID, markdown})
		mu.Unlock()
		return nil
	}

	ctx := context.Background()

	// 1. Quiet mode + Web actively seen suppresses redundant main-thread plain replies.
	bus.RecordWebSeen()
	bus.OnReply(ctx, &model.Entry{Thread: model.MainThread, Text: "安静模式下网页在线应被抑制"}, nil)
	time.Sleep(15 * time.Millisecond)
	bus.FlushDue(ctx, false)
	mu.Lock()
	if len(sent) != 0 {
		t.Fatalf("expected quiet suppression while web seen, got %+v", sent)
	}
	mu.Unlock()

	// 2. Group thread replies still deliver and coalesce multiple rapid messages into one push.
	bus.OnReply(ctx, &model.Entry{Thread: "wecom:grp-1", Text: "第一条进展"}, nil)
	bus.OnReply(ctx, &model.Entry{Thread: "wecom:grp-1", Text: "第二条进展"}, nil)
	time.Sleep(15 * time.Millisecond)
	bus.FlushDue(ctx, false)
	mu.Lock()
	if len(sent) != 1 || sent[0].chat != "grp-1" || !strings.Contains(sent[0].body, "第一条进展") || !strings.Contains(sent[0].body, "第二条进展") {
		t.Fatalf("expected coalesced group push, got %+v", sent)
	}
	sent = nil
	mu.Unlock()

	// 3. Normal mode delivers main-thread replies even while Web is open.
	cur := eng.Settings()
	cur.NotifyLevel = "normal"
	if err := eng.SaveConfig(ctx, &cur, nil); err != nil {
		t.Fatal(err)
	}
	bus.OnReply(ctx, &model.Entry{Thread: model.MainThread, Text: "常规模式直推"}, nil)
	bus.FlushDue(ctx, true)
	mu.Lock()
	if len(sent) != 1 || sent[0].chat != "" || sent[0].body != "常规模式直推" {
		t.Fatalf("expected normal mode main-thread push, got %+v", sent)
	}
	mu.Unlock()
}

func TestWebGroupSyncReportAttachmentAndWorkerTag(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := core.NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	reportPath := filepath.Join(dir, "perf-report.md")
	if err := os.WriteFile(reportPath, []byte("# Report\nAll checks green.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		msgs  []struct{ chat, body string }
		files []struct{ chat, name, body string }
	)
	bus := NewBus(eng, nil, nil)
	bus.SetPaths(filepath.Join(dir, "uploads"), dir)
	bus.flushDelay = 10 * time.Millisecond
	bus.sendFn = func(_ context.Context, chatID, markdown string) error {
		mu.Lock()
		msgs = append(msgs, struct{ chat, body string }{chatID, markdown})
		mu.Unlock()
		return nil
	}
	bus.sendFileFn = func(_ context.Context, chatID, filename string, data []byte) error {
		mu.Lock()
		files = append(files, struct{ chat, name, body string }{chatID, filename, string(data)})
		mu.Unlock()
		return nil
	}

	ctx := context.Background()
	// 1. Web message posted in a wecom group thread mirrors back to WeCom group.
	bus.OnUserMessage(ctx, &model.Entry{Channel: "web", Thread: "wecom:grp-88", Text: "请把单测也跑一遍"})

	// 2. Group reply referencing a worker + local .md file gets [#worker] prefix and pushes .md attachment.
	bus.OnReply(ctx, &model.Entry{
		Thread: "wecom:grp-88",
		Ref:    "api-profiler",
		Text:   "剖析完成，详单见 " + reportPath,
	}, nil)
	bus.FlushDue(ctx, true)

	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 2 || msgs[0].chat != "grp-88" || !strings.Contains(msgs[0].body, "**[Web]**") {
		t.Fatalf("unexpected mirrored messages: %+v", msgs)
	}
	if !strings.HasPrefix(msgs[1].body, "[#api-profiler] ") {
		t.Fatalf("expected [#api-profiler] prefix in group reply, got %q", msgs[1].body)
	}
	if len(files) != 1 || files[0].chat != "grp-88" || files[0].name != "perf-report.md" || !strings.Contains(files[0].body, "All checks green") {
		t.Fatalf("expected report attachment pushed to WeCom, got %+v", files)
	}
}

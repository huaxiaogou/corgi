package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kiperjing/corgi/internal/channel/wecom"
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

func TestHandleWeComCardClickResolvesAndUpdatesCard(t *testing.T) {
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

	type wsFrame struct {
		Cmd     string         `json:"cmd"`
		Headers map[string]any `json:"headers"`
		Body    map[string]any `json:"body"`
	}
	sentCh := make(chan wsFrame, 4)
	updateCh := make(chan wsFrame, 4)
	serverConnCh := make(chan *websocket.Conn, 1)

	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		select {
		case serverConnCh <- conn:
		default:
		}
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var f wsFrame
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			reqID, _ := f.Headers["req_id"].(string)
			switch f.Cmd {
			case "aibot_send_msg":
				sentCh <- f
				ack, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"errmsg":  "ok",
				})
				_ = conn.Write(r.Context(), websocket.MessageText, ack)
			case "aibot_respond_update_msg":
				updateCh <- f
				ack, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"errmsg":  "ok",
				})
				_ = conn.Write(r.Context(), websocket.MessageText, ack)
			}
		}
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bus *Bus
	wc := wecom.NewClient(wecom.Config{
		GatewayURL: "ws" + strings.TrimPrefix(wsSrv.URL, "http"),
		BotID:      "bot-1",
		Secret:     "sec-1",
		AllowUsers: []string{"owner_01"},
		DefaultTo:  "owner_01",
	}, wecom.Handlers{
		OnCardClick: func(c context.Context, ev wecom.CardClickEvent) { bus.HandleWeComCardClick(c, ev) },
	}, nil)
	bus = NewBus(eng, wc, nil)
	go wc.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !wc.Online() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if !wc.Online() {
		t.Fatal("wecom client did not come online")
	}
	srvConn := <-serverConnCh

	// 1. Reply with long summary + 3 choices triggers immediate WeCom template card push
	// with full options in horizontal_content_list and compact buttons (选1/选2/选3).
	longSummary := "登录接口并发刷新 token 存在竞态。建议先加分布式锁并补充并发回归测试，确认按哪种方案推进？"
	opts := []string{
		"先加分布式锁并补齐并发回归测试（推荐）",
		"仅记录告警日志，暂不改动登录核心链路",
		"暂不推进",
	}
	_, act, _, err := eng.Reply(ctx, model.MainThread, longSummary, "", opts, false)
	if err != nil || act == nil {
		t.Fatalf("eng.Reply failed: act=%v err=%v", act, err)
	}

	select {
	case f := <-sentCh:
		tc, _ := f.Body["template_card"].(map[string]any)
		if tc["card_type"] != "button_interaction" || tc["task_id"] != act.ID {
			t.Fatalf("unexpected pushed card: %+v", tc)
		}
		if _, ok := tc["horizontal_content_list"]; !ok {
			t.Fatalf("expected expanded options in horizontal_content_list, got %+v", tc)
		}
		rawBtns, _ := tc["button_list"].([]any)
		if len(rawBtns) != 3 {
			t.Fatalf("expected 3 compact buttons, got %+v", tc["button_list"])
		}
		b0, _ := rawBtns[0].(map[string]any)
		b1, _ := rawBtns[1].(map[string]any)
		b2, _ := rawBtns[2].(map[string]any)
		if b0["text"] != "选1" || b1["text"] != "选2" || b2["text"] != "选3" {
			t.Fatalf("expected compact buttons 选1/选2/选3, got %+v", rawBtns)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pushed action card")
	}

	// 2. User clicks option 2 via aibot_event_callback -> Action resolved in DB + card updated in-place.
	click1, _ := json.Marshal(map[string]any{
		"cmd":     "aibot_event_callback",
		"headers": map[string]any{"req_id": "cb-click-1"},
		"body": map[string]any{
			"chattype": "single",
			"from":     map[string]any{"userid": "owner_01"},
			"event": map[string]any{
				"eventtype": "template_card_event",
				"template_card_event": map[string]any{
					"task_id":   act.ID,
					"event_key": act.ID + ":2",
				},
			},
		},
	})
	if err := srvConn.Write(ctx, websocket.MessageText, click1); err != nil {
		t.Fatalf("write click1 callback: %v", err)
	}

	select {
	case upd := <-updateCh:
		if upd.Headers["req_id"] != "cb-click-1" {
			t.Fatalf("expected req_id cb-click-1, got %+v", upd.Headers)
		}
		if upd.Body["response_type"] != "update_template_card" {
			t.Fatalf("unexpected response_type: %+v", upd.Body)
		}
		tc, _ := upd.Body["template_card"].(map[string]any)
		src, _ := tc["source"].(map[string]any)
		if tc["card_type"] != "button_interaction" || tc["task_id"] != act.ID || src["desc"] != "Corgi · 已拍板" {
			t.Fatalf("unexpected updated template_card: %+v", tc)
		}
		if rt, _ := tc["replace_text"].(string); !strings.Contains(rt, "仅记录告警") {
			t.Fatalf("expected replace_text to reflect chosen option, got %q", rt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for card update after click1")
	}

	gotAct, err := st.GetAction(ctx, act.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if gotAct.Status != model.ActionResolved || gotAct.ResolvedBy != "wecom" || gotAct.ChosenKey != "2" || gotAct.ChosenLabel != opts[1] || gotAct.ResolvedAt == nil {
		t.Fatalf("action was not properly recorded as resolved: %+v", gotAct)
	}
	entries, _, err := st.ListEntries(ctx, model.MainThread, 10)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	userReplies := 0
	for _, e := range entries {
		if e.Role == "user" && e.Channel == "wecom" && e.Text == opts[1] {
			userReplies++
			if e.ActionID != act.ID || e.Re == nil || e.Re.Entry != act.EntryID {
				t.Fatalf("wecom choice entry missing receipt linkage (action_id=%q re=%+v, want action=%q entry=%q)",
					e.ActionID, e.Re, act.ID, act.EntryID)
			}
		}
	}
	if userReplies != 1 {
		t.Fatalf("expected 1 recorded user reply entry for choice, got %d (entries=%+v)", userReplies, entries)
	}
	snap := eng.Snapshot(ctx, model.MainThread)
	if len(snap.Actions) != 0 {
		t.Fatalf("expected 0 pending actions in Snapshot after WeCom click, got %+v", snap.Actions)
	}
	if ra := snap.ResolvedActions[act.ID]; ra == nil || ra.Status != model.ActionResolved || ra.ResolvedBy != "wecom" || len(ra.Choices) != 3 {
		t.Fatalf("expected Web Snapshot ResolvedActions[%s] synced with WeCom click, got %+v", act.ID, ra)
	}

	// 3. Re-clicking an already-resolved card still refreshes the stale card in-place without double-recording.
	click2, _ := json.Marshal(map[string]any{
		"cmd":     "aibot_event_callback",
		"headers": map[string]any{"req_id": "cb-click-2"},
		"body": map[string]any{
			"chattype": "single",
			"from":     map[string]any{"userid": "owner_01"},
			"event": map[string]any{
				"eventtype": "template_card_event",
				"task_id":   act.ID,
				"event_key": act.ID + ":1",
			},
		},
	})
	if err := srvConn.Write(ctx, websocket.MessageText, click2); err != nil {
		t.Fatalf("write click2 callback: %v", err)
	}

	select {
	case upd := <-updateCh:
		if upd.Headers["req_id"] != "cb-click-2" {
			t.Fatalf("expected req_id cb-click-2 on stale card refresh, got %+v", upd.Headers)
		}
		tc, _ := upd.Body["template_card"].(map[string]any)
		if rt, _ := tc["replace_text"].(string); !strings.Contains(rt, "仅记录告警") {
			t.Fatalf("stale refresh should preserve original choice 2, got %q", rt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale card refresh on click2")
	}

	entries2, _, _ := st.ListEntries(ctx, model.MainThread, 10)
	if len(entries2) != len(entries) {
		t.Fatalf("second click must not create duplicate reply entries: before=%d after=%d", len(entries), len(entries2))
	}
}

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
	bus.OnReply(ctx, &model.Entry{Thread: model.MainThread, Text: "安静模式下网页在线应被抑制"}, nil, nil)
	time.Sleep(15 * time.Millisecond)
	bus.FlushDue(ctx, false)
	mu.Lock()
	if len(sent) != 0 {
		t.Fatalf("expected quiet suppression while web seen, got %+v", sent)
	}
	mu.Unlock()

	// 2. Group thread replies still deliver and coalesce multiple rapid messages into one push.
	bus.OnReply(ctx, &model.Entry{Thread: "wecom:grp-1", Text: "第一条进展"}, nil, nil)
	bus.OnReply(ctx, &model.Entry{Thread: "wecom:grp-1", Text: "第二条进展"}, nil, nil)
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
	bus.OnReply(ctx, &model.Entry{Thread: model.MainThread, Text: "常规模式直推"}, nil, nil)
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

	// 2. Group reply referencing a worker + explicit file attachment gets [#worker] prefix and pushes file.
	bus.OnReply(ctx, &model.Entry{
		Thread: "wecom:grp-88",
		Ref:    "api-profiler",
		Text:   "剖析完成，详单见附件",
		Files:  []string{reportPath},
	}, nil, nil)
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

func TestWebActionResolutionSyncsToWeComAndStaleCardRefresh(t *testing.T) {
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
	cardCh := make(chan wsFrame, 8)
	mdCh := make(chan wsFrame, 8)
	updateCh := make(chan wsFrame, 8)
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
				if f.Body["msgtype"] == "template_card" {
					cardCh <- f
				} else if f.Body["msgtype"] == "markdown" {
					mdCh <- f
				}
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

	// 1. User is away from Web -> card pushes to WeCom (PushedToWeCom=true).
	// Then user resolves from Web -> WeCom immediately gets Markdown sync receipt.
	_, act1, _, err := eng.Reply(ctx, model.MainThread, "是否启动灰度发布？", "", []string{"立即灰度", "暂缓发布"}, false)
	if err != nil || act1 == nil {
		t.Fatalf("eng.Reply 1 failed: %v", err)
	}
	<-cardCh
	// Even if user now opens Web to resolve it, pushed cards still send a sync receipt to WeCom.
	bus.RecordWebSeen()
	if _, err := eng.ResolveAction(ctx, act1.ID, "web", "2"); err != nil {
		t.Fatalf("ResolveAction from web failed: %v", err)
	}
	select {
	case m := <-mdCh:
		md, _ := m.Body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		if !strings.Contains(content, "Web 控制台") || !strings.Contains(content, "暂缓发布") || !strings.Contains(content, "是否启动灰度发布") {
			t.Fatalf("unexpected web-resolve sync markdown: %q", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WeCom sync markdown after Web resolve")
	}

	// 2. Clicking the stale WeCom card later refreshes it in-place with original Web choice without double-recording.
	staleClick, _ := json.Marshal(map[string]any{
		"cmd":     "aibot_event_callback",
		"headers": map[string]any{"req_id": "cb-stale-web-1"},
		"body": map[string]any{
			"chattype": "single",
			"from":     map[string]any{"userid": "owner_01"},
			"event": map[string]any{
				"eventtype": "template_card_event",
				"template_card_event": map[string]any{
					"task_id":   act1.ID,
					"event_key": act1.ID + ":1",
				},
			},
		},
	})
	if err := srvConn.Write(ctx, websocket.MessageText, staleClick); err != nil {
		t.Fatalf("write stale click: %v", err)
	}
	select {
	case upd := <-updateCh:
		tc, _ := upd.Body["template_card"].(map[string]any)
		mt, _ := tc["main_title"].(map[string]any)
		if rt, _ := tc["replace_text"].(string); !strings.Contains(rt, "暂缓发布") {
			t.Fatalf("stale card refresh should show original Web choice, got replace_text=%q", rt)
		}
		if desc, _ := mt["desc"].(string); !strings.Contains(desc, "Web 控制台") {
			t.Fatalf("stale card refresh should show Web 控制台, got desc=%q", desc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale card refresh")
	}

	// 3. Web custom answer + dismiss + snooze all sync immediately to WeCom when card was pushed.
	bus.mu.Lock()
	bus.webSeenAt = time.Time{} // simulate away from Web when cards are created
	bus.mu.Unlock()
	_, act2, _, err := eng.Reply(ctx, model.MainThread, "多用户接入方向？", "", []string{"个人用", "共享服务"}, false)
	if err != nil || act2 == nil {
		t.Fatalf("eng.Reply 2 failed: %v", err)
	}
	<-cardCh
	if _, err := eng.ResolveActionCustom(ctx, act2.ID, "web", "先研究单实例隔离"); err != nil {
		t.Fatalf("ResolveActionCustom failed: %v", err)
	}
	select {
	case m := <-mdCh:
		md, _ := m.Body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		if !strings.Contains(content, "自定义回答") || !strings.Contains(content, "先研究单实例隔离") {
			t.Fatalf("unexpected custom-answer sync markdown: %q", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for custom-answer WeCom sync")
	}

	_, act3, _, err := eng.Reply(ctx, model.MainThread, "是否归档旧会话？", "", []string{"归档", "保留"}, false)
	if err != nil || act3 == nil {
		t.Fatalf("eng.Reply 3 failed: %v", err)
	}
	<-cardCh
	if _, err := eng.SnoozeAction(ctx, act3.ID, time.Date(2026, 11, 27, 9, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("SnoozeAction failed: %v", err)
	}
	select {
	case m := <-mdCh:
		md, _ := m.Body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		if !strings.Contains(content, "已暂缓") || !strings.Contains(content, "11-27 09:00") {
			t.Fatalf("unexpected snooze sync markdown: %q", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snooze WeCom sync")
	}

	// Clicking stale snoozed WeCom card renders "Corgi · 已暂缓", never "已拍板".
	snoozedClick, _ := json.Marshal(map[string]any{
		"cmd":     "aibot_event_callback",
		"headers": map[string]any{"req_id": "cb-stale-snooze"},
		"body": map[string]any{
			"chattype": "single",
			"from":     map[string]any{"userid": "owner_01"},
			"event": map[string]any{
				"eventtype": "template_card_event",
				"template_card_event": map[string]any{
					"task_id":   act3.ID,
					"event_key": act3.ID + ":1",
				},
			},
		},
	})
	if err := srvConn.Write(ctx, websocket.MessageText, snoozedClick); err != nil {
		t.Fatalf("write snoozed click: %v", err)
	}
	select {
	case upd := <-updateCh:
		tc, _ := upd.Body["template_card"].(map[string]any)
		src, _ := tc["source"].(map[string]any)
		if src["desc"] != "Corgi · 已暂缓" || tc["replace_text"] != "暂缓至 11-27 09:00" {
			t.Fatalf("stale snoozed card mismatch: %+v", tc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stale snoozed card refresh")
	}

	// 4. Group thread Web resolution sends exactly ONE receipt (no duplicate from OnUserMessage).
	_, grpAct, _, err := eng.Reply(ctx, "wecom:grp-99", "群任务确认？", "", []string{"通过", "打回"}, false)
	if err != nil || grpAct == nil {
		t.Fatalf("eng.Reply group failed: %v", err)
	}
	<-cardCh
	if _, err := eng.ResolveAction(ctx, grpAct.ID, "web", "1"); err != nil {
		t.Fatalf("ResolveAction group failed: %v", err)
	}
	select {
	case m := <-mdCh:
		if m.Body["chatid"] != "grp-99" {
			t.Fatalf("expected group chatid grp-99, got %+v", m.Body)
		}
		md, _ := m.Body["markdown"].(map[string]any)
		if content, _ := md["content"].(string); !strings.Contains(content, "已拍板（Web 控制台）") || !strings.Contains(content, "通过") {
			t.Fatalf("unexpected group sync markdown: %q", content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for group WeCom sync")
	}
	select {
	case dup := <-mdCh:
		t.Fatalf("expected single sync message for group Web resolve, got duplicate: %+v", dup.Body)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWeComWebActiveSuppressionAndPreciseReceipts(t *testing.T) {
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
	cardCh := make(chan wsFrame, 8)
	mdCh := make(chan wsFrame, 8)

	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
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
			if f.Cmd == "aibot_send_msg" {
				if f.Body["msgtype"] == "template_card" {
					cardCh <- f
				} else if f.Body["msgtype"] == "markdown" {
					mdCh <- f
				}
				ack, _ := json.Marshal(map[string]any{"headers": map[string]any{"req_id": reqID}, "errcode": 0, "errmsg": "ok"})
				_ = conn.Write(r.Context(), websocket.MessageText, ack)
			}
		}
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wc := wecom.NewClient(wecom.Config{
		GatewayURL: "ws" + strings.TrimPrefix(wsSrv.URL, "http"),
		BotID:      "bot-1",
		Secret:     "sec-1",
		AllowUsers: []string{"owner_01"},
		DefaultTo:  "owner_01",
	}, wecom.Handlers{}, nil)
	bus := NewBus(eng, wc, nil)
	go wc.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !wc.Online() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if !wc.Online() {
		t.Fatal("wecom client did not come online")
	}

	// 1. User is actively viewing Web in quiet mode -> action cards (--opt, --check, worker_dialog) are suppressed on WeCom!
	bus.RecordWebSeen()
	_, optAct, _, err := eng.Reply(ctx, model.MainThread, "是否继续？", "", []string{"继续", "停止"}, false)
	if err != nil || optAct == nil {
		t.Fatalf("Reply(--opt): %v", err)
	}
	_, chkAct, _, err := eng.Reply(ctx, model.MainThread, "请抽空看一下报告", "", nil, true)
	if err != nil || chkAct == nil {
		t.Fatalf("Reply(--check): %v", err)
	}
	dlgAct := &model.Action{
		ID: "a_dlg_quiet", Kind: model.ActionKindWorkerDialog, Thread: model.MainThread,
		TargetWorker: "w1", Title: "Approve?", Choices: []model.ActionChoice{{Key: "1", Label: "Yes"}},
		Status: model.ActionPending, CreatedAt: time.Now(),
	}
	_ = st.InsertAction(ctx, dlgAct)
	bus.OnActionCreated(ctx, dlgAct)

	select {
	case unexpected := <-cardCh:
		t.Fatalf("expected card suppressed while Web is active, got %+v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}

	// Resolving unpushed actions on Web must NOT send phantom receipts to WeCom!
	if _, err := eng.ResolveAction(ctx, optAct.ID, "web", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, chkAct.ID, "web", "ack"); err != nil {
		t.Fatal(err)
	}
	select {
	case phantom := <-mdCh:
		t.Fatalf("expected zero WeCom receipts for unpushed cards, got %+v", phantom)
	case <-time.After(100 * time.Millisecond):
	}

	// 2. User recently messaged via WeCom (wecomRecent) -> cards push immediately even while Web is open!
	bus.mu.Lock()
	bus.wecomActiveAt = time.Now()
	bus.mu.Unlock()
	_, chk2, _, err := eng.Reply(ctx, model.MainThread, "企微会话中的待看提醒", "", nil, true)
	if err != nil || chk2 == nil {
		t.Fatalf("Reply(--check wecomRecent): %v", err)
	}
	select {
	case f := <-cardCh:
		tc, _ := f.Body["template_card"].(map[string]any)
		btns, _ := tc["button_list"].([]any)
		if len(btns) != 1 || btns[0].(map[string]any)["text"] != "已看" {
			t.Fatalf("expected single 已看 button for router_check card, got %+v", btns)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected check card pushed to WeCom when wecomRecent is active")
	}
	persisted, _ := st.GetAction(ctx, chk2.ID)
	if !persisted.PushedToWeCom {
		t.Fatalf("expected PushedToWeCom persisted=true, got %+v", persisted)
	}
}

package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

type capturedWeComFrame struct {
	Cmd     string
	ReqID   string
	MsgType string
	Card    map[string]any
}

func startMockWeComGateway(t *testing.T) (*wecom.Client, func() []capturedWeComFrame, func()) {
	t.Helper()
	var (
		mu     sync.Mutex
		frames []capturedWeComFrame
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var f struct {
				Cmd     string         `json:"cmd"`
				Headers map[string]any `json:"headers"`
				Body    map[string]any `json:"body"`
			}
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			reqID, _ := f.Headers["req_id"].(string)
			if f.Cmd == "aibot_send_msg" || f.Cmd == "aibot_respond_update_msg" {
				msgType, _ := f.Body["msgtype"].(string)
				card, _ := f.Body["template_card"].(map[string]any)
				mu.Lock()
				frames = append(frames, capturedWeComFrame{
					Cmd:     f.Cmd,
					ReqID:   reqID,
					MsgType: msgType,
					Card:    card,
				})
				mu.Unlock()
				resp, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"errmsg":  "ok",
				})
				_ = conn.Write(r.Context(), websocket.MessageText, resp)
			}
		}
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())
	cl := wecom.NewClient(wecom.Config{
		GatewayURL: wsURL,
		BotID:      "bot-test",
		Secret:     "sec-test",
		AllowUsers: []string{"owner_01"},
		DefaultTo:  "owner_01",
	}, wecom.Handlers{}, nil)
	go cl.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !cl.Online() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if !cl.Online() {
		cancel()
		srv.Close()
		t.Fatal("mock wecom client failed to come online")
	}

	getFrames := func() []capturedWeComFrame {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedWeComFrame(nil), frames...)
	}
	cleanup := func() {
		cancel()
		srv.Close()
	}
	return cl, getFrames, cleanup
}

func TestWeComScheme3NoQueueDeadlockVoteExpandSnoozeStaleAndDCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := core.NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	wc, getFrames, cleanup := startMockWeComGateway(t)
	defer cleanup()

	bus := NewBus(eng, wc, nil)
	var (
		mdMu sync.Mutex
		mds  []string
	)
	bus.sendFn = func(_ context.Context, _, markdown string) error {
		mdMu.Lock()
		mds = append(mds, markdown)
		mdMu.Unlock()
		return nil
	}
	bus.sendStreamFn = nil // force instant command replies through sendFn for assertion

	// 1. Two independent real decisions push BOTH cards immediately (no single-card queue latch deadlock!).
	_, act1, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread:  model.MainThread,
		Text:    "决策一：是否上线？",
		Options: []string{"立即上线", "暂缓"},
	})
	if err != nil || act1 == nil {
		t.Fatalf("reply act1: %v", err)
	}
	_, act2, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread:  model.MainThread,
		Text:    "决策二：勾选发布检查项",
		Mode:    model.ActionModeMulti,
		Options: []string{"检查1", "检查2", "检查3", "检查4", "检查5", "检查6"},
	})
	if err != nil || act2 == nil {
		t.Fatalf("reply act2: %v", err)
	}
	// Plus a router_check reminder which must NOT push a card and must NOT appear in /d.
	_, chkAct, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
		Thread: model.MainThread,
		Text:   "阶段报告已就绪，供查收。",
		Check:  true,
	})
	if err != nil || chkAct == nil {
		t.Fatalf("reply chkAct: %v", err)
	}

	frames := getFrames()
	if len(frames) != 2 {
		t.Fatalf("expected 2 immediate decision cards (no queue latch, no router_check card), got %d: %+v", len(frames), frames)
	}
	if frames[0].Card["card_type"] != "button_interaction" || frames[1].Card["card_type"] != "vote_interaction" {
		t.Fatalf("unexpected card types: %v, %v", frames[0].Card["card_type"], frames[1].Card["card_type"])
	}

	// 2. Vote interaction inline expand preserving preselected key, then multi-select submit -> locked receipt.
	bus.HandleWeComCardClick(ctx, wecom.CardClickEvent{
		ReqID:        "req-expand",
		ChatID:       "owner_01",
		FromUser:     "owner_01",
		CardType:     "vote_interaction",
		TaskID:       act2.ID,
		ActionID:     act2.ID,
		ChoiceKey:    "submit",
		SelectedKeys: []string{"1", wecom.SentinelMore},
	})
	frames = getFrames()
	last := frames[len(frames)-1]
	if last.Cmd != "aibot_respond_update_msg" || last.ReqID != "req-expand" {
		t.Fatalf("expected in-place expand update, got %+v", last)
	}
	eOpts := last.Card["checkbox"].(map[string]any)["option_list"].([]any)
	// 6 choices + fold + snooze = 8
	if len(eOpts) != 8 {
		t.Fatalf("expected 8 expanded vote options, got %d", len(eOpts))
	}

	// Now submit keys 1 and 6.
	bus.HandleWeComCardClick(ctx, wecom.CardClickEvent{
		ReqID:        "req-submit",
		ChatID:       "owner_01",
		FromUser:     "owner_01",
		CardType:     "vote_interaction",
		TaskID:       act2.ID,
		ActionID:     act2.ID,
		ChoiceKey:    "submit",
		SelectedKeys: []string{"1", "6"},
	})
	gotAct2, _ := eng.GetAction(ctx, act2.ID)
	if gotAct2.Status != model.ActionResolved || len(gotAct2.ChosenKeys) != 2 || gotAct2.ChosenKeys[0] != "1" || gotAct2.ChosenKeys[1] != "6" {
		t.Fatalf("expected act2 resolved with [1,6], got %+v", gotAct2)
	}
	frames = getFrames()
	last = frames[len(frames)-1]
	if last.Cmd != "aibot_respond_update_msg" || last.Card["card_type"] != "vote_interaction" || last.Card["replace_text"] == "" {
		t.Fatalf("expected locked vote_interaction receipt, got %+v", last)
	}

	// 3. Stale click on act2 after resolution refreshes receipt in-place without error or double-submit.
	bus.HandleWeComCardClick(ctx, wecom.CardClickEvent{
		ReqID:        "req-stale",
		ChatID:       "owner_01",
		FromUser:     "owner_01",
		CardType:     "vote_interaction",
		TaskID:       act2.ID,
		ActionID:     act2.ID,
		ChoiceKey:    "submit",
		SelectedKeys: []string{"2"},
	})
	frames = getFrames()
	if frames[len(frames)-1].ReqID != "req-stale" || frames[len(frames)-1].Card["replace_text"] == "" {
		t.Fatalf("expected stale click receipt refresh, got %+v", frames[len(frames)-1])
	}

	// 4. `/d` lists only remaining real decision (act1) and re-surfaces it (excludes chkAct!).
	beforeCount := len(getFrames())
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		MsgID:    "m-d1",
		ChatType: "single",
		ChatID:   "owner_01",
		FromUser: "owner_01",
		Text:     "/d",
	})
	frames = getFrames()
	if len(frames) != beforeCount+1 || frames[len(frames)-1].Cmd != "aibot_send_msg" {
		t.Fatalf("expected /d to re-surface act1 card, got delta=%d", len(frames)-beforeCount)
	}

	// 5. Button snooze flow -> in-place snoozed receipt AND no extra markdown receipt (ResolvedChannel == wecom).
	mdMu.Lock()
	mdBefore := len(mds)
	mdMu.Unlock()
	bus.HandleWeComCardClick(ctx, wecom.CardClickEvent{
		ReqID:     "req-snooze-menu",
		ChatID:    "owner_01",
		FromUser:  "owner_01",
		CardType:  "button_interaction",
		TaskID:    act1.ID,
		ActionID:  act1.ID,
		ChoiceKey: "snooze_menu",
	})
	bus.HandleWeComCardClick(ctx, wecom.CardClickEvent{
		ReqID:     "req-snooze-1h",
		ChatID:    "owner_01",
		FromUser:  "owner_01",
		CardType:  "button_interaction",
		TaskID:    act1.ID,
		ActionID:  act1.ID,
		ChoiceKey: "snooze:1h",
	})
	gotAct1, _ := eng.GetAction(ctx, act1.ID)
	if gotAct1.Status != model.ActionSnoozed || gotAct1.ResolvedChannel != "wecom" {
		t.Fatalf("expected act1 snoozed via wecom, got %+v", gotAct1)
	}
	mdMu.Lock()
	mdAfter := len(mds)
	mdMu.Unlock()
	if mdAfter != mdBefore {
		t.Fatalf("expected zero duplicate markdown receipt for wecom-initiated snooze, got %d new messages", mdAfter-mdBefore)
	}

	// 6. `/d` with 0 remaining real decisions (even though chkAct is still pending) reports empty.
	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		MsgID:    "m-d2",
		ChatType: "single",
		ChatID:   "owner_01",
		FromUser: "owner_01",
		Text:     "/d",
	})
	mdMu.Lock()
	lastMD := mds[len(mds)-1]
	mdMu.Unlock()
	if !strings.Contains(lastMD, "当前没有待拍板事项") {
		t.Fatalf("expected /d to ignore router_check and report empty, got %q", lastMD)
	}
}

package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func openInPlaceTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng, err := NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return eng, st
}

func appendCorgiEntry(t *testing.T, st *store.Store, thread, text string) *model.Entry {
	t.Helper()
	ctx := context.Background()
	id, seq, err := st.NextID(ctx, "e")
	if err != nil {
		t.Fatalf("next id: %v", err)
	}
	en := &model.Entry{
		ID:      id,
		Seq:     seq,
		Role:    "corgi",
		Thread:  thread,
		Channel: "web",
		Text:    text,
		At:      time.Now(),
	}
	if err := st.InsertEntry(ctx, en); err != nil {
		t.Fatalf("insert entry: %v", err)
	}
	return en
}

func TestSayInPlaceValidationAndFallback(t *testing.T) {
	eng, st := openInPlaceTestEngine(t)
	ctx := context.Background()

	targetMain := appendCorgiEntry(t, st, model.MainThread, "主线程早期方案分析")
	targetOther := appendCorgiEntry(t, st, "wecom:group-9", "其他线程历史消息")

	cases := []struct {
		name        string
		req         SayRequest
		wantInPlace bool
	}{
		{
			name: "web valid same-thread target persists in_place=true",
			req: SayRequest{
				Thread:    model.MainThread,
				Channel:   "web",
				From:      "you",
				Text:      "这里补充一个约束条件",
				ReEntryID: targetMain.ID,
				InPlace:   true,
			},
			wantInPlace: true,
		},
		{
			name: "missing target entry falls back to false",
			req: SayRequest{
				Thread:    model.MainThread,
				Channel:   "web",
				From:      "you",
				Text:      "引用了不存在的条目",
				ReEntryID: "e999999",
				InPlace:   true,
			},
			wantInPlace: false,
		},
		{
			name: "cross-thread target falls back to false",
			req: SayRequest{
				Thread:    model.MainThread,
				Channel:   "web",
				From:      "you",
				Text:      "跨线程引用不得原位",
				ReEntryID: targetOther.ID,
				InPlace:   true,
			},
			wantInPlace: false,
		},
		{
			name: "wecom channel always forces in_place=false",
			req: SayRequest{
				Thread:    model.MainThread,
				Channel:   "wecom",
				From:      "you",
				Text:      "企微引用必须普通落盘",
				ReEntryID: targetMain.ID,
				InPlace:   true,
			},
			wantInPlace: false,
		},
		{
			name: "web explicit false remains false",
			req: SayRequest{
				Thread:    model.MainThread,
				Channel:   "web",
				From:      "you",
				Text:      "普通底部引用回复",
				ReEntryID: targetMain.ID,
				InPlace:   false,
			},
			wantInPlace: false,
		},
		{
			name: "empty re_entry_id falls back to false",
			req: SayRequest{
				Thread:  model.MainThread,
				Channel: "web",
				From:    "you",
				Text:    "无引用目标不得原位",
				InPlace: true,
			},
			wantInPlace: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, _, err := eng.Say(ctx, tc.req)
			if err != nil {
				t.Fatalf("Say failed: %v", err)
			}
			if entry.InPlace != tc.wantInPlace {
				t.Fatalf("entry.InPlace = %v, want %v", entry.InPlace, tc.wantInPlace)
			}
			got, err := st.GetEntry(ctx, entry.ID)
			if err != nil {
				t.Fatalf("GetEntry: %v", err)
			}
			if got.InPlace != tc.wantInPlace {
				t.Fatalf("persisted entry.InPlace = %v, want %v", got.InPlace, tc.wantInPlace)
			}
		})
	}
}

func TestChatQueueFourPathsPreserveInPlace(t *testing.T) {
	eng, st := openInPlaceTestEngine(t)
	ctx := context.Background()
	target := appendCorgiEntry(t, st, model.MainThread, "需要补充说明的历史报告")

	// Path 1: Immediate SubmitChatSay when Router is idle.
	queued, _, immediateEntry, _, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "路径1：空闲立即原位补充",
		ReEntryID: target.ID,
		InPlace:   true,
	}, "auto")
	if err != nil || queued || immediateEntry == nil {
		t.Fatalf("path 1 unexpected: queued=%v entry=%v err=%v", queued, immediateEntry, err)
	}
	if !immediateEntry.InPlace {
		t.Fatalf("path 1 expected InPlace=true")
	}

	// Path 2: Enqueue via SubmitChatSay(mode="queue") stores InPlace on QueuedPrompt.
	queued, qSteer, _, _, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "路径2/3：排队后插队原位补充",
		ReEntryID: target.ID,
		InPlace:   true,
	}, "queue")
	if err != nil || !queued || qSteer == nil {
		t.Fatalf("path 2 steer item failed: queued=%v item=%v err=%v", queued, qSteer, err)
	}
	if !qSteer.InPlace {
		t.Fatalf("path 2 expected QueuedPrompt.InPlace=true")
	}

	queued, qDrain, _, _, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "路径2/4：排队后自动出队原位补充",
		ReEntryID: target.ID,
		InPlace:   true,
	}, "queue")
	if err != nil || !queued || qDrain == nil || !qDrain.InPlace {
		t.Fatalf("path 2 drain item failed: queued=%v item=%+v err=%v", queued, qDrain, err)
	}

	// Path 3: SteerChatPrompt preserves InPlace on created Entry.
	steeredEntry, _, err := eng.SteerChatPrompt(ctx, model.MainThread, qSteer.ID)
	if err != nil {
		t.Fatalf("SteerChatPrompt: %v", err)
	}
	if !steeredEntry.InPlace || steeredEntry.Re == nil || steeredEntry.Re.Entry != target.ID {
		t.Fatalf("path 3 expected InPlace=true with re=%s, got %+v", target.ID, steeredEntry)
	}

	// Clear queued triggers & cooldown so Path 4 (drainRouterPrompts) can promote qDrain.
	trigs, err := st.ListQueuedTriggers(ctx)
	if err != nil {
		t.Fatalf("ListQueuedTriggers: %v", err)
	}
	for _, tr := range trigs {
		tr.Status = "delivered"
		if err := st.UpdateTrigger(ctx, tr); err != nil {
			t.Fatalf("UpdateTrigger: %v", err)
		}
	}
	eng.clearDrainCooldown(RouterQueueScope(model.MainThread))

	// Path 4: drainRouterPrompts promotes queued item with InPlace preserved.
	eng.drainRouterPrompts(ctx)
	entries, _, err := st.ListEntries(ctx, model.MainThread, 50)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	var drained *model.Entry
	for _, en := range entries {
		if en.Text == "路径2/4：排队后自动出队原位补充" {
			drained = en
			break
		}
	}
	if drained == nil {
		t.Fatalf("path 4 drained entry not found")
	}
	if !drained.InPlace || drained.Re == nil || drained.Re.Entry != target.ID {
		t.Fatalf("path 4 expected InPlace=true with re=%s, got %+v", target.ID, drained)
	}

	// Router history context still reads all entries in seq order cleanly.
	hist := eng.buildThreadHistoryText(ctx, model.MainThread, nil)
	idx1 := strings.Index(hist, "路径1：空闲立即原位补充")
	idx3 := strings.Index(hist, "路径2/3：排队后插队原位补充")
	idx4 := strings.Index(hist, "路径2/4：排队后自动出队原位补充")
	if idx1 < 0 || idx3 < 0 || idx4 < 0 || !(idx1 < idx3 && idx3 < idx4) {
		t.Fatalf("expected seq-ordered history containing all in-place replies, got:\n%s", hist)
	}
}

func TestResolveActionCustomInPlaceAndAtomicConflict(t *testing.T) {
	eng, st := openInPlaceTestEngine(t)
	ctx := context.Background()

	out, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread:  model.MainThread,
		Text:    "请选择发布策略",
		Options: []string{"灰度发布", "全量发布"},
	})
	if err != nil || out.Action == nil {
		t.Fatalf("ReplyDetailed: out=%+v err=%v", out, err)
	}

	resolved, err := eng.ResolveActionCustomInPlace(ctx, out.Action.ID, "web", "先发灰度并观察 15 分钟", true)
	if err != nil {
		t.Fatalf("ResolveActionCustomInPlace: %v", err)
	}
	if resolved.Status != model.ActionResolved || resolved.ChosenKey != "custom" {
		t.Fatalf("unexpected resolved action: %+v", resolved)
	}

	// Second resolution must still hit atomic ErrActionAlreadyResolved (409).
	if _, err := eng.ResolveActionCustomInPlace(ctx, out.Action.ID, "web", "重复提交", true); !errors.Is(err, store.ErrActionAlreadyResolved) {
		t.Fatalf("expected ErrActionAlreadyResolved, got %v", err)
	}

	entries, _, err := st.ListEntries(ctx, model.MainThread, 20)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	var customEntry *model.Entry
	for _, en := range entries {
		if en.Role == "user" && en.ActionID == out.Action.ID {
			customEntry = en
			break
		}
	}
	if customEntry == nil {
		t.Fatalf("expected user entry bound to action %s", out.Action.ID)
	}
	if !customEntry.InPlace || customEntry.Re == nil || customEntry.Re.Entry != out.Entry.ID {
		t.Fatalf("expected InPlace=true targeting %s, got %+v", out.Entry.ID, customEntry)
	}

	// WeCom custom resolution must still force InPlace=false even if flag is true.
	out2, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread:  model.MainThread,
		Text:    "另一项待拍板",
		Options: []string{"A", "B"},
	})
	if err != nil || out2.Action == nil {
		t.Fatalf("ReplyDetailed 2: %v", err)
	}
	if _, err := eng.ResolveActionCustomInPlace(ctx, out2.Action.ID, "wecom", "企微自定义答复", true); err != nil {
		t.Fatalf("wecom ResolveActionCustomInPlace: %v", err)
	}
	entries2, _, err := st.ListEntries(ctx, model.MainThread, 30)
	if err != nil {
		t.Fatalf("ListEntries 2: %v", err)
	}
	for _, en := range entries2 {
		if en.Role == "user" && en.ActionID == out2.Action.ID {
			if en.InPlace {
				t.Fatalf("expected wecom action entry InPlace=false, got true")
			}
			return
		}
	}
	t.Fatalf("expected wecom custom action entry")
}

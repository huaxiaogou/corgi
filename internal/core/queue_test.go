package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func newMockHerdrFleet(t *testing.T, handler func(method string, params map[string]any) (any, *herdr.Error)) *fleet.Fleet {
	t.Helper()
	return fleet.NewFleet(newFakeHerdrClient(handler), "hub", t.TempDir(), "test", []string{"agy", "claude"})
}

func TestPaneQueueSteerStopAndSequentialDrain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu      sync.Mutex
		status  = "working"
		prompts []string
		keys    [][]string
	)
	fl := newMockHerdrFleet(t, func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			name := "agy-1"
			kind := "agy"
			return map[string]any{
				"agents": []map[string]any{{
					"name":              name,
					"pane_id":           "w1:p1",
					"workspace_id":      "w1",
					"agent":             kind,
					"agent_status":      status,
					"interactive_ready": true,
				}},
			}, nil
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
			return map[string]any{"ok": true}, nil
		case "pane.send_keys":
			raw := params["keys"].([]any)
			var ks []string
			for _, x := range raw {
				ks = append(ks, x.(string))
			}
			keys = append(keys, ks)
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})

	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. While working, default submissions queue up independently.
	q1, it1, err := eng.SubmitPanePrompt(ctx, "w1:p1", "先跑测试", "auto")
	if err != nil || !q1 || it1 == nil {
		t.Fatalf("expected first prompt queued: q=%v item=%v err=%v", q1, it1, err)
	}
	q2, it2, err := eng.SubmitPanePrompt(ctx, "w1:p1", "还要发版", "auto")
	if err != nil || !q2 || it2 == nil {
		t.Fatalf("expected second prompt queued: q=%v item=%v err=%v", q2, it2, err)
	}
	q3, it3, err := eng.SubmitPanePrompt(ctx, "w1:p1", "临时插队改样式", "auto")
	if err != nil || !q3 || it3 == nil {
		t.Fatalf("expected third prompt queued: q=%v item=%v err=%v", q3, it3, err)
	}

	// 2. Steer ONLY #3 mid-turn; #1 and #2 stay queued in order.
	if _, err := eng.SteerPanePrompt(ctx, "w1:p1", it3.ID); err != nil {
		t.Fatalf("SteerPanePrompt: %v", err)
	}
	mu.Lock()
	if len(prompts) != 1 || prompts[0] != "临时插队改样式" {
		t.Fatalf("expected immediate steer of #3, got %v", prompts)
	}
	mu.Unlock()

	queues, _ := eng.PaneQueuesSnapshot(ctx)
	if len(queues["w1:p1"]) != 2 || queues["w1:p1"][0].ID != it1.ID || queues["w1:p1"][1].ID != it2.ID {
		t.Fatalf("unexpected remaining queue: %+v", queues["w1:p1"])
	}

	// 3. Stop sends Esc and pauses auto-drain while preserving queued items.
	paused, err := eng.StopPane(ctx, "w1:p1")
	if err != nil || !paused {
		t.Fatalf("expected StopPane paused=true, got paused=%v err=%v", paused, err)
	}
	mu.Lock()
	status = "idle"
	mu.Unlock()
	rawAgents, _ := fl.Agents(ctx)
	eng.drainPanePrompts(ctx, rawAgents)
	mu.Lock()
	if len(prompts) != 1 || len(keys) != 1 || keys[0][0] != "esc" {
		t.Fatalf("expected paused queue not to auto-drain after Stop; prompts=%v keys=%v", prompts, keys)
	}
	mu.Unlock()

	// 4. Resuming drains strictly ONE item (#1), holding #2 in cooldown until next turn finishes.
	eng.ResumeQueue("w1:p1")
	eng.drainPanePrompts(ctx, rawAgents)
	eng.drainPanePrompts(ctx, rawAgents)
	mu.Lock()
	if len(prompts) != 2 || prompts[1] != "先跑测试" {
		t.Fatalf("expected only #1 drained on idle, got %v", prompts)
	}
	status = "working"
	mu.Unlock()

	// Observing working clears cooldown; next idle drains #2.
	rawAgents, _ = fl.Agents(ctx)
	eng.drainPanePrompts(ctx, rawAgents)
	mu.Lock()
	status = "done"
	mu.Unlock()
	rawAgents, _ = fl.Agents(ctx)
	eng.drainPanePrompts(ctx, rawAgents)
	mu.Lock()
	if len(prompts) != 3 || prompts[2] != "还要发版" {
		t.Fatalf("expected #2 drained after turn 1 finished, got %v", prompts)
	}
	mu.Unlock()
}

func TestMainChatQueueSteerAndDrain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu      sync.Mutex
		status  = "working"
		prompts []string
	)
	fl := newMockHerdrFleet(t, func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			name := model.RouterName
			kind := "claude"
			return map[string]any{
				"agents": []map[string]any{{
					"name":              name,
					"pane_id":           "w0:p1",
					"workspace_id":      "w0",
					"agent":             kind,
					"agent_status":      status,
					"interactive_ready": true,
				}},
			}, nil
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})

	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// While Router is working, Web chat submissions queue cleanly without polluting entries.
	q1, it1, entry1, _, err := eng.SubmitChatSay(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "第一条排队"}, "auto")
	if err != nil || !q1 || it1 == nil || entry1 != nil {
		t.Fatalf("expected web chat queued while router working: q=%v item=%v entry=%v err=%v", q1, it1, entry1, err)
	}
	q2, it2, _, _, err := eng.SubmitChatSay(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "第二条插队引导"}, "auto")
	if err != nil || !q2 || it2 == nil {
		t.Fatalf("expected second web chat queued: q=%v item=%v err=%v", q2, it2, err)
	}

	// Steer #2 mid-turn -> creates Entry + Trigger(Steer=true) and delivers while Router is still working!
	if _, trig, err := eng.SteerChatPrompt(ctx, model.MainThread, it2.ID); err != nil || trig == nil || !trig.Steer {
		t.Fatalf("SteerChatPrompt failed: trig=%+v err=%v", trig, err)
	}
	live := map[string]liveInfo{model.RouterName: {pane: "w0:p1", status: "working", ready: true, kind: "claude"}}
	eng.deliverQueued(ctx, live, map[string]bool{"": true})
	mu.Lock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "第二条插队引导") {
		t.Fatalf("expected steered #2 delivered mid-turn, got %v", prompts)
	}
	mu.Unlock()

	// Cancel remaining #1 leaves zero queued items.
	if err := eng.CancelQueuedPrompt(ctx, RouterQueueScope(model.MainThread), it1.ID); err != nil {
		t.Fatalf("CancelQueuedPrompt: %v", err)
	}
	snap := eng.Snapshot(ctx, model.MainThread)
	if len(snap.QueuedChat) != 0 {
		t.Fatalf("expected empty chat queue after cancel, got %+v", snap.QueuedChat)
	}
}

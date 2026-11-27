package core

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

// TestRemoteNodeCorgiPaneNeverHijacksHubRouter reproduces the exact production stall where
// a leftover blocked pane named "corgi" on a connected remote Node (e.g. a local dev Hub)
// overwrote Hub's live idle Router in live["corgi"] and Fleet.names["corgi"], stalling all
// trigger delivery until the remote Node disconnected.
func TestRemoteNodeCorgiPaneNeverHijacksHubRouter(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var hubPrompts atomic.Int32
	var remotePrompts atomic.Int32

	hubHerdr := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "agent.list":
			return map[string]any{
				"agents": []map[string]any{
					{
						"pane_id":           "w1F:p1",
						"workspace_id":      "w1F",
						"name":              model.RouterName,
						"agent":             "codebuddy",
						"agent_status":      "idle",
						"interactive_ready": true,
					},
				},
			}, nil
		case "pane.read":
			return map[string]any{"text": "claude-opus-5.5 high · bypass permissions on"}, nil
		case "agent.prompt":
			hubPrompts.Add(1)
			return map[string]any{"ok": true}, nil
		case "pane.process_info":
			return map[string]any{"pid": 123, "argv": []string{"codebuddy", "-y"}}, nil
		default:
			return map[string]any{}, nil
		}
	})

	remoteHerdr := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "agent.list":
			// Simulate remote node responding slightly later so it appends AFTER Hub in merged slice.
			time.Sleep(15 * time.Millisecond)
			return map[string]any{
				"agents": []map[string]any{
					{
						"pane_id":           "wR:p1",
						"workspace_id":      "wR",
						"name":              model.RouterName,
						"agent":             "claude",
						"agent_status":      "blocked",
						"interactive_ready": false,
					},
				},
			}, nil
		case "pane.read":
			return map[string]any{"text": "WARNING: Claude Code running in Bypass Permissions mode\n❯ No, exit\n  Yes, I accept"}, nil
		case "agent.prompt":
			remotePrompts.Add(1)
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})

	fl := fleet.NewFleet(hubHerdr, "hub", dir, "test", []string{"codebuddy"})
	fl.Attach(&fleet.RemoteMachine{
		Info:  model.Machine{ID: "mac-node", Name: "MacBook", Online: true},
		Herdr: remoteHerdr,
	})

	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Enqueue a WeCom user message targeting Router.
	_, trig, err := eng.Say(ctx, SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "user1",
		Text:    "都什么情况了",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run one Engine tick: Hub Router must NOT be hijacked by remote blocked wR:p1@mac-node.
	eng.tick(ctx)

	if hubPrompts.Load() != 1 {
		t.Fatalf("expected Hub Router w1F:p1 to receive prompt, got hubPrompts=%d remotePrompts=%d",
			hubPrompts.Load(), remotePrompts.Load())
	}
	if remotePrompts.Load() != 0 {
		t.Fatalf("remote Node's stale corgi pane must never receive Hub prompts, got %d", remotePrompts.Load())
	}
	gotTrig, err := st.GetTrigger(ctx, trig.ID)
	if err != nil || gotTrig.Status != "delivered" {
		t.Fatalf("expected trigger delivered immediately, got %+v err=%v", gotTrig, err)
	}

	// Snapshot & direct Prompt by RouterName must also stay bound strictly to Hub w1F:p1.
	snap := eng.Snapshot(ctx, model.MainThread)
	if !snap.Router.Alive || snap.Router.PaneID != "w1F:p1" || snap.Router.Status != "idle" {
		t.Fatalf("Snapshot().Router was hijacked: %+v", snap.Router)
	}
	if err := fl.Prompt(ctx, model.RouterName, "ping"); err != nil {
		t.Fatalf("fl.Prompt(RouterName): %v", err)
	}
	if hubPrompts.Load() != 2 || remotePrompts.Load() != 0 {
		t.Fatalf("Fleet name lookup for corgi was hijacked: hub=%d remote=%d", hubPrompts.Load(), remotePrompts.Load())
	}
}

package core

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func TestIdleRestoredWorkerHealsBypassInPlace(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu          sync.Mutex
		phase       = "restored" // restored -> shell -> healed
		startCalls  int
		startedArgs []string
		startedName string
		sentTexts   []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			if phase == "shell" {
				return map[string]any{"agents": []any{}}, nil
			}
			return map[string]any{"agents": []any{
				map[string]any{
					"pane_id":           "w7:p1",
					"workspace_id":      "w7",
					"name":              "agy-85",
					"agent":             "agy",
					"agent_status":      "idle",
					"interactive_ready": true,
					"agent_session": map[string]any{
						"kind":  "id",
						"value": "032e20e3-5dfe-4496-ba33-b1519947f4a0",
					},
				},
			}}, nil
		case "pane.process_info":
			switch phase {
			case "restored":
				return map[string]any{"process_info": map[string]any{
					"pane_id":                     "w7:p1",
					"shell_pid":                   64071,
					"foreground_process_group_id": 67818,
					"foreground_processes": []any{
						map[string]any{
							"pid":     67818,
							"name":    "agy",
							"argv0":   "agy",
							"argv":    []string{"agy", "--conversation", "032e20e3-5dfe-4496-ba33-b1519947f4a0"},
							"cmdline": "agy --conversation 032e20e3-5dfe-4496-ba33-b1519947f4a0",
						},
					},
				}}, nil
			case "shell":
				return map[string]any{"process_info": map[string]any{
					"pane_id":                     "w7:p1",
					"shell_pid":                   64071,
					"foreground_process_group_id": 64071,
					"foreground_processes": []any{
						map[string]any{"pid": 64071, "name": "zsh", "argv0": "-zsh", "argv": []string{"-zsh"}},
					},
				}}, nil
			default:
				return map[string]any{"process_info": map[string]any{
					"pane_id":                     "w7:p1",
					"shell_pid":                   64071,
					"foreground_process_group_id": 98058,
					"foreground_processes": []any{
						map[string]any{
							"pid":  98058,
							"name": "agy",
							"argv": []string{"agy", "--conversation", "032e20e3-5dfe-4496-ba33-b1519947f4a0", "--model", "gemini-4-neon-xhigh", "--dangerously-skip-permissions"},
						},
					},
				}}, nil
			}
		case "pane.send_text":
			txt, _ := params["text"].(string)
			sentTexts = append(sentTexts, txt)
			if txt == "/exit" {
				phase = "shell"
			}
			return map[string]any{}, nil
		case "pane.send_keys":
			return map[string]any{}, nil
		case "agent.start":
			startCalls++
			if startCalls == 1 {
				// Simulate herdr briefly holding the old agent name right after /exit.
				return nil, &herdr.Error{Code: "agent_name_taken", Message: "agent name agy-85 is already used"}
			}
			startedName, _ = params["name"].(string)
			if rawArgs, ok := params["args"].([]any); ok {
				startedArgs = nil
				for _, a := range rawArgs {
					if s, ok := a.(string); ok {
						startedArgs = append(startedArgs, s)
					}
				}
			}
			phase = "healed"
			return map[string]any{"type": "agent_started"}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.bypassCheckInterval = 10 * time.Millisecond
	eng.bypassExitPoll = 5 * time.Millisecond
	eng.bypassExitTimeout = 500 * time.Millisecond
	eng.bypassStartRetryTimeout = 500 * time.Millisecond

	ctx := context.Background()
	wk := &model.Worker{
		Name:      "agy-85",
		Kind:      "agy",
		Cwd:       dir,
		Thread:    model.MainThread,
		PaneID:    "w7:p1",
		Status:    "idle",
		Model:     "gemini-4-neon",
		Effort:    "xhigh",
		Session:   "032e20e3-5dfe-4496-ba33-b1519947f4a0",
		CreatedAt: time.Now().Add(-5 * time.Minute),
	}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}

	// Trigger tick -> should heal agy-85 asynchronously in-place.
	live := map[string]liveInfo{
		"agy-85": {pane: "w7:p1", status: "idle", ready: true, kind: "agy", session: wk.Session},
	}
	eng.syncWorkers(ctx, live, map[string]bool{"": true}, time.Now())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := phase == "healed" && !eng.isHealingBypassPane("w7:p1")
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	mu.Lock()
	gotPhase, gotCalls, gotName := phase, startCalls, startedName
	gotArgs := append([]string(nil), startedArgs...)
	gotTexts := append([]string(nil), sentTexts...)
	mu.Unlock()

	if gotPhase != "healed" || gotCalls != 2 || gotName != "agy-85" {
		t.Fatalf("expected in-place heal after 1 name_taken retry, got phase=%q calls=%d name=%q texts=%v", gotPhase, gotCalls, gotName, gotTexts)
	}
	for _, want := range []string{"--conversation", wk.Session, "--model", "gemini-4-neon-xhigh", "--dangerously-skip-permissions"} {
		if !slices.Contains(gotArgs, want) {
			t.Fatalf("relaunched args %v missing %q", gotArgs, want)
		}
	}

	// Subsequent syncWorkers check must NOT re-trigger heal now that bypass flag is present.
	time.Sleep(20 * time.Millisecond)
	eng.syncWorkers(ctx, live, map[string]bool{"": true}, time.Now())
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if startCalls != 2 {
		t.Fatalf("expected no extra agent.start after heal, got %d", startCalls)
	}
	mu.Unlock()
}

func TestWorkingAndAskPermissionsWorkersAreNotInterrupted(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu        sync.Mutex
		exitCount int
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.process_info":
			return map[string]any{"process_info": map[string]any{
				"pane_id":                     "w7:p1",
				"shell_pid":                   100,
				"foreground_process_group_id": 200,
				"foreground_processes": []any{
					map[string]any{"pid": 200, "name": "agy", "argv": []string{"agy", "--conversation", "sess-1"}},
				},
			}}, nil
		case "pane.send_text":
			exitCount++
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.bypassCheckInterval = 5 * time.Millisecond

	ctx := context.Background()
	wk := &model.Worker{
		Name: "agy-work", Kind: "agy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w7:p1", Status: "working", Session: "sess-1", CreatedAt: time.Now().Add(-5 * time.Minute),
	}
	_ = st.UpsertWorker(ctx, wk)

	// 1. Working worker must NEVER be interrupted.
	eng.syncWorkers(ctx, map[string]liveInfo{
		"agy-work": {pane: "w7:p1", status: "working", ready: true, kind: "agy", session: "sess-1"},
	}, map[string]bool{"": true}, time.Now())
	time.Sleep(40 * time.Millisecond)

	// 2. AskPermissions == true must NEVER auto-heal idle worker either.
	cfg := eng.Settings()
	cfg.AskPermissions = true
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	eng.syncWorkers(ctx, map[string]liveInfo{
		"agy-work": {pane: "w7:p1", status: "idle", ready: true, kind: "agy", session: "sess-1"},
	}, map[string]bool{"": true}, time.Now())
	time.Sleep(40 * time.Millisecond)

	mu.Lock()
	if exitCount != 0 {
		t.Fatalf("expected 0 exit attempts for working/AskPermissions worker, got %d", exitCount)
	}
	mu.Unlock()
}

func TestAnswerRestoredAgyEditDialog(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu          sync.Mutex
		sentAnswers []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{
				"text": "  shift+tab to auto-approve file edits\nAccept this file edit?\n> 1. Yes, accept this change\n  2. No, reject this change\n  ↑/↓ Navigate · f full diff\nesc to cancel",
			}}, nil
		case "pane.process_info":
			return map[string]any{"process_info": map[string]any{
				"pane_id":                     "w7:p1",
				"shell_pid":                   100,
				"foreground_process_group_id": 200,
				"foreground_processes": []any{
					map[string]any{"pid": 200, "name": "agy", "argv": []string{"agy", "--conversation", "sess-85"}},
				},
			}}, nil
		case "pane.send_text":
			if txt, ok := params["text"].(string); ok {
				sentAnswers = append(sentAnswers, txt)
			}
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude", "agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.bypassCheckInterval = 5 * time.Millisecond

	ctx := context.Background()
	wk := &model.Worker{
		Name: "agy-85", Kind: "agy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w7:p1", Status: "blocked", Session: "sess-85",
	}
	_ = st.UpsertWorker(ctx, wk)

	// Mid-turn file-edit dialog on restored agy pane missing bypass is auto-accepted without creating a pending card.
	eng.handleWorkerBlocked(ctx, wk)
	pending, err := st.ListPendingActions(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected 0 pending actions after auto-accepting restored agy file edit, got %+v (err=%v)", pending, err)
	}
	mu.Lock()
	if len(sentAnswers) != 1 || sentAnswers[0] != "1" {
		t.Fatalf("expected choice '1' sent to w7:p1, got %v", sentAnswers)
	}
	mu.Unlock()
}

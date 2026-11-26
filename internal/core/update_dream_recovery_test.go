package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

// TestUnscannedRemoteMachinePreservesWorkerApprovalsAndTriggers verifies that a brief
// Node disconnect/restart does NOT mark remote workers gone, expire their pending approvals,
// or reroute their queued triggers to the Router.
func TestUnscannedRemoteMachinePreservesWorkerApprovalsAndTriggers(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "v0.1.41", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	wk := &model.Worker{
		Name:      "remote-dev",
		Kind:      "codex",
		Machine:   "mbp-1",
		Cwd:       "/Users/me/proj",
		Thread:    model.MainThread,
		PaneID:    "w3:p1@mbp-1",
		Status:    "blocked",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}
	act := &model.Action{
		ID:           "a1",
		Kind:         model.ActionKindWorkerDialog,
		Thread:       model.MainThread,
		TargetWorker: wk.Name,
		PaneID:       wk.PaneID,
		Title:        "确认执行命令",
		Status:       model.ActionPending,
		CreatedAt:    time.Now(),
	}
	if err := st.InsertAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	trig := &model.Trigger{
		ID:      "t1",
		Seq:     1,
		Source:  "router",
		Thread:  model.MainThread,
		Text:    "继续跑测试",
		At:      time.Now(),
		Policy:  "respond",
		Status:  "queued",
		Targets: []string{wk.Name},
	}
	if err := st.InsertTrigger(ctx, trig); err != nil {
		t.Fatal(err)
	}

	// Only Hub ("") was scanned; mbp-1 is temporarily offline/restarting.
	scanned := map[string]bool{"": true}
	emptyLive := map[string]liveInfo{}
	eng.syncWorkers(ctx, emptyLive, scanned, time.Now())
	eng.deliverQueued(ctx, emptyLive, scanned)

	gotWk, err := st.GetWorker(ctx, wk.Name)
	if err != nil || gotWk.Ended || gotWk.Status == "gone" {
		t.Fatalf("remote worker should stay intact when mbp-1 was not scanned: wk=%+v err=%v", gotWk, err)
	}
	pending, err := st.ListPendingActions(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "a1" {
		t.Fatalf("pending worker approval should survive unscanned tick: %+v err=%v", pending, err)
	}
	gotTrig, err := st.GetTrigger(ctx, "t1")
	if err != nil || len(gotTrig.Targets) != 1 || gotTrig.Targets[0] != wk.Name {
		t.Fatalf("queued trigger should stay on remote worker while mbp-1 is unscanned: %+v err=%v", gotTrig, err)
	}

	// Now mbp-1 IS scanned successfully and the pane is truly gone -> transitions cleanly.
	scanned["mbp-1"] = true
	eng.syncWorkers(ctx, emptyLive, scanned, time.Now())
	eng.deliverQueued(ctx, emptyLive, scanned)

	gotWk, _ = st.GetWorker(ctx, wk.Name)
	if !gotWk.Ended || gotWk.Status != "gone" {
		t.Fatalf("expected worker gone once mbp-1 scanned without pane, got %+v", gotWk)
	}
	pending, _ = st.ListPendingActions(ctx)
	if len(pending) != 0 {
		t.Fatalf("expected worker_dialog action expired once mbp-1 confirmed gone, got %+v", pending)
	}
	gotTrig, _ = st.GetTrigger(ctx, "t1")
	if len(gotTrig.Targets) != 1 || gotTrig.Targets[0] != model.RouterName {
		t.Fatalf("expected trigger rerouted to router once confirmed gone, got %+v", gotTrig.Targets)
	}
}

// TestDreamRestartRecoveryAcrossEngineInstances exercises restarting a Hub Engine mid-Dream:
// 1) Pane already exited after writing fresh journal -> recovers exactly once, emits 1 event & 1 skill card set,
//    commits last_dream + nightly_dream_slot atomically, preserves an already-running Router.
// 2) Second Engine restart / replay does NOT duplicate events or cards.
func TestDreamRestartRecoveryAcrossEngineInstances(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu         sync.Mutex
		closedWS   []string
		routerLive = true
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.list":
			ws := []map[string]any{
				{"workspace_id": "ws-dream-1", "label": "corgi · dream · run-test-1"},
			}
			if routerLive {
				ws = append(ws, map[string]any{"workspace_id": "ws-router", "label": model.RouterLabel})
			}
			return map[string]any{"workspaces": ws}, nil
		case "workspace.close":
			if id, _ := params["workspace_id"].(string); id != "" {
				closedWS = append(closedWS, id)
			}
			return map[string]any{}, nil
		case "agent.list":
			ags := []map[string]any{}
			if routerLive {
				ags = append(ags, map[string]any{"pane_id": "w0:p1", "name": model.RouterName, "agent": "claude", "agent_status": "idle", "ready": true})
			}
			return map[string]any{"agents": ags}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "v0.1.41", []string{"claude"})
	eng1, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	runAt := time.Now().Add(-3 * time.Minute)
	d1 := &dreamRun{
		id:          "run-test-1",
		at:          runAt,
		observedAt:  runAt,
		started:     true,
		reason:      "nightly",
		workspaceID: "ws-dream-1",
		paneID:      "w1:p1",
	}
	if err := eng1.saveActiveDreamMarker(ctx, d1); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(eng1.memory.DreamsDir(), journalName(runAt)+".md")
	journalBody := "精简了 2 条重复指令并整理了笔记。\n可做成 skill：版本发布检查\n"
	if err := os.WriteFile(journalPath, []byte(journalBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate Hub restart: construct Engine 2 against the same SQLite DB.
	eng2, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !eng2.Dreaming() {
		t.Fatal("expected NewEngine to eagerly hydrate active_dream before first tick")
	}
	if err := eng2.StartDream(ctx, "manual"); err == nil {
		t.Fatal("expected manual StartDream rejected while recovered Dream is still active")
	}

	// First tick sees corgi-dream gone + fresh journal -> finishes cleanly once.
	eng2.checkDream(ctx, map[string]liveInfo{
		model.RouterName: {pane: "w0:p1", status: "idle", ready: true, kind: "claude"},
	}, false)

	if eng2.Dreaming() {
		t.Fatal("expected recovered Dream finalized on first tick")
	}
	if _, ok, _ := st.GetKV(ctx, activeDreamKVKey); ok {
		t.Fatal("expected active_dream KV removed atomically")
	}
	if eng2.LastDream(ctx).IsZero() {
		t.Fatal("expected last_dream updated")
	}
	slot, ok := eng2.readNightlyDreamSlot(ctx)
	if !ok || slot.Outcome != "ok" || slot.Date != runAt.Format("2006-01-02") {
		t.Fatalf("unexpected nightly_dream_slot: ok=%v slot=%+v", ok, slot)
	}

	// Replay finishDream with same runID must be a strict no-op (no duplicate events/cards).
	eng2.finishDream(ctx, d1, true)

	entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
	awakeCount := 0
	for _, en := range entries {
		if strings.Contains(en.Text, "Corgi 醒了") {
			awakeCount++
		}
	}
	if awakeCount != 1 {
		t.Fatalf("expected exactly 1 awake event, got %d", awakeCount)
	}
	acts, _ := st.ListPendingActions(ctx)
	if len(acts) != 1 {
		t.Fatalf("expected exactly 1 skill suggestion card, got %d", len(acts))
	}
	mu.Lock()
	for _, id := range closedWS {
		if id == "ws-router" {
			t.Fatalf("recovery must NOT close an already-running Router workspace: %v", closedWS)
		}
	}
	mu.Unlock()
}

// TestAutoUpdateBoundedB2AndReservation verifies Bounded B2 scheduling rules,
// exclusive Hub restart reservation against Dream/queues, and post-restart notice confirmation.
func TestAutoUpdateBoundedB2AndReservation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		if method == "agent.list" {
			return map[string]any{"agents": []map[string]any{
				{"pane_id": "w0:p1", "name": model.RouterName, "agent": "claude", "agent_status": "idle", "ready": true},
			}}, nil
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "v0.1.40", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.Local

	// 1. Default AutoUpdate == false -> never eligible.
	t0415 := time.Date(2026, 11, 27, 4, 15, 0, 0, loc)
	if eng.IsAutoUpdateWindowEligible(ctx, t0415) {
		t.Fatal("expected AutoUpdate ineligible when setting is false")
	}

	// Enable AutoUpdate with Dream at 04:00.
	cfg := eng.Settings()
	cfg.AutoUpdate = true
	cfg.DreamAt = "04:00"
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	// Without tonight's nightly_dream_slot -> ineligible.
	if eng.IsAutoUpdateWindowEligible(ctx, t0415) {
		t.Fatal("expected ineligible before tonight's nightly Dream succeeds or skips")
	}

	// Nightly failure -> ineligible.
	eng.recordNightlyDreamSlot(ctx, "2026-11-27", "failed", t0415)
	if eng.IsAutoUpdateWindowEligible(ctx, t0415) {
		t.Fatal("expected ineligible after failed nightly Dream")
	}

	// Nightly ok -> eligible inside 3h window (04:00..07:00), ineligible after 07:00 cutoff.
	eng.recordNightlyDreamSlot(ctx, "2026-11-27", "ok", t0415)
	if !eng.IsAutoUpdateWindowEligible(ctx, t0415) {
		t.Fatal("expected eligible after nightly Dream ok inside overnight cutoff")
	}
	t0705 := time.Date(2026, 11, 27, 7, 5, 0, 0, loc)
	if eng.IsAutoUpdateWindowEligible(ctx, t0705) {
		t.Fatal("expected ineligible past 3h overnight cutoff")
	}

	// Dream off -> eligible in [04:00, 06:00), ineligible at 06:01.
	cfg.DreamAt = "off"
	_ = eng.SaveConfig(ctx, &cfg, nil)
	if !eng.IsAutoUpdateWindowEligible(ctx, t0415) {
		t.Fatal("expected eligible at 04:15 when dream_at=off")
	}
	if eng.IsAutoUpdateWindowEligible(ctx, time.Date(2026, 11, 27, 6, 1, 0, 0, loc)) {
		t.Fatal("expected ineligible at 06:01 when dream_at=off")
	}

	// 2. TryReserveHubRestart blocks StartDream while held, and rejects when Web chat queue is non-empty.
	release, ok, reason := eng.TryReserveHubRestart(ctx)
	if !ok {
		t.Fatalf("expected idle reservation to succeed, got reason=%q", reason)
	}
	if err := eng.StartDream(ctx, "manual"); err == nil {
		t.Fatal("expected StartDream blocked while Hub restart reservation is held")
	}
	release()

	// Enqueue a Router web prompt -> TryReserveHubRestart must reject.
	if err := st.InsertQueuedPrompt(ctx, &model.QueuedPrompt{
		ID: "q1", Scope: RouterQueueScope(model.MainThread), Thread: model.MainThread, Text: "等待发送", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := eng.TryReserveHubRestart(ctx); ok {
		t.Fatal("expected reservation rejected while Router web prompt is queued")
	}
	_ = st.DeleteQueuedPrompt(ctx, "q1")

	// 3. Post-restart notice emits once booted on target version.
	if err := eng.RecordPendingAutoUpdateNotice(ctx, "v0.1.41", []string{"Hub", "macbook"}); err != nil {
		t.Fatal(err)
	}
	eng.SetVersion("v0.1.41")
	eng.EmitPendingAutoUpdateNotice(ctx)
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 10)
	found := false
	for _, en := range entries {
		if strings.Contains(en.Text, "v0.1.41") && strings.Contains(en.Text, "macbook") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected confirmed auto-update notice in chat:main, got %+v", entries)
	}
	if _, exists, _ := st.GetKV(ctx, pendingAutoUpdateNoticeKVKey); exists {
		t.Fatal("expected pending_auto_update_notice deleted after emission")
	}
}

package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestActiveHoursParsingAndWindow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	if !InActiveHours("09:00-22:30", time.Date(2026, 11, 25, 9, 0, 0, 0, loc)) {
		t.Fatal("09:00 should be inside 09:00-22:30")
	}
	if InActiveHours("09:00-22:30", time.Date(2026, 11, 25, 22, 30, 0, 0, loc)) {
		t.Fatal("22:30 should be outside 09:00-22:30")
	}
	if InActiveHours("09:00-22:30", time.Date(2026, 11, 25, 3, 15, 0, 0, loc)) {
		t.Fatal("03:15 should be outside 09:00-22:30")
	}
	if !InActiveHours("24h", time.Date(2026, 11, 25, 3, 15, 0, 0, loc)) {
		t.Fatal("24h should always be active")
	}
	next := NextMorningStart("09:00-22:30", time.Date(2026, 11, 25, 17, 30, 0, 0, loc))
	want := time.Date(2026, 11, 26, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("NextMorningStart = %v, want %v", next, want)
	}
}

func TestIdlePatrolSameDayBackoffAndQuietNight(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"codebuddy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.FixedZone("CST", 8*3600)
	day := time.Date(2026, 11, 25, 17, 0, 0, 0, loc)
	// Mark morning digest done for today so we isolate same-day backoff behavior.
	_ = st.SetKV(ctx, kvLastMorningDigestDay, day.Format("2006-01-02"))

	idleAt := day
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name:       "taihu-plan",
		Kind:       "codebuddy",
		Cwd:        "/root",
		Thread:     model.MainThread,
		PaneID:     "wM:p1",
		CreatedBy:  "corgi",
		CreatedAt:  day.Add(-10 * time.Minute),
		Status:     "idle",
		IdleSince:  &idleAt,
		LastReport: "方案已完成，3 条路径待你拍板",
	})
	live := map[string]liveInfo{
		model.RouterName: {pane: "wN:p1", status: "idle", ready: true, kind: "codebuddy"},
		"taihu-plan":     {pane: "wM:p1", status: "idle", ready: true, kind: "codebuddy"},
	}

	// 1. Before 30m: zero-token skip.
	eng.runIdlePatrol(ctx, live, day.Add(20*time.Minute))
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected 0 triggers at 20m idle, got %d", len(trs))
	}

	// 2. At 30m: first patrol fires (1 merged trigger).
	eng.runIdlePatrol(ctx, live, day.Add(30*time.Minute))
	trs, _ := st.ListQueuedTriggers(ctx)
	if len(trs) != 1 || !strings.Contains(trs[0].Text, "[corgi idle-patrol]") || !strings.Contains(trs[0].Text, "attempt=1/2") {
		t.Fatalf("expected 1st idle-patrol trigger at 30m, got %+v", trs)
	}
	// Mark trigger delivered to simulate Router handling it without creating an Action.
	trs[0].Status = "delivered"
	_ = st.UpdateTrigger(ctx, trs[0])

	// 3. At 2h (only 1.5h since 1st patrol < 4h backoff): must stay silent.
	eng.runIdlePatrol(ctx, live, day.Add(2*time.Hour))
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected 0 triggers before 4h backoff, got %d", len(trs))
	}

	// 4. At 4h30m (21:30, inside activeHours, 4h after 1st patrol): 2nd patrol fires.
	eng.runIdlePatrol(ctx, live, day.Add(4*time.Hour+30*time.Minute))
	trs, _ = st.ListQueuedTriggers(ctx)
	if len(trs) != 1 || !strings.Contains(trs[0].Text, "attempt=2/2") {
		t.Fatalf("expected 2nd idle-patrol trigger after 4h backoff, got %+v", trs)
	}
	trs[0].Status = "delivered"
	_ = st.UpdateTrigger(ctx, trs[0])

	// 5. Same-day cap reached (attempt=2): even at 22:25 or next day inside quiet night 23:30, no 3rd same-day patrol.
	eng.runIdlePatrol(ctx, live, day.Add(6*time.Hour+30*time.Minute)) // 23:30 night
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected 0 triggers during quiet night, got %d", len(trs))
	}
}

func TestMorningDigestCarryoverKeepExceptionAndDigestReplacement(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"codebuddy", "agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.FixedZone("CST", 8*3600)
	nextMorning := time.Date(2026, 11, 26, 9, 5, 0, 0, loc)

	fourDaysAgo := nextMorning.Add(-96 * time.Hour)
	yesterdayEve := nextMorning.Add(-15 * time.Hour)

	// 1. Stale Corgi worker (>3d), NOT kept -> should appear in stale archival list.
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "old-job-1", Kind: "codebuddy", Cwd: "/root", Thread: model.MainThread, PaneID: "w1:p1",
		CreatedBy: "corgi", CreatedAt: fourDaysAgo, Status: "idle", IdleSince: &fourDaysAgo,
	})
	// 2. Stale Corgi worker (>3d), marked KEEP -> MUST be exempt!
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "kept-ref", Kind: "codebuddy", Cwd: "/root", Thread: model.MainThread, PaneID: "w2:p1",
		CreatedBy: "corgi", CreatedAt: fourDaysAgo, Status: "idle", IdleSince: &fourDaysAgo, Keep: true,
	})
	// 3. Stale manual session (by=you) -> MUST NEVER appear in stale archival list!
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "manual-agy", Kind: "agy", Cwd: "/Users/dev/corgi", Thread: model.MainThread, PaneID: "w3:p1",
		CreatedBy: "you", CreatedAt: fourDaysAgo, Status: "idle", IdleSince: &fourDaysAgo,
	})
	// 4. Carryover worktree branch from yesterday -> priority WORKTREE item.
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "wt-fix", Kind: "agy", Cwd: "/Users/dev/wt", Thread: model.MainThread, PaneID: "w4:p1",
		CreatedBy: "corgi", CreatedAt: yesterdayEve, Status: "idle", IdleSince: &yesterdayEve,
		WorktreeBranch: "corgi/wt-fix",
	})

	live := map[string]liveInfo{
		model.RouterName: {pane: "wN:p1", status: "idle", ready: true, kind: "codebuddy"},
		"old-job-1":      {pane: "w1:p1", status: "idle", ready: true, kind: "codebuddy"},
		"kept-ref":       {pane: "w2:p1", status: "idle", ready: true, kind: "codebuddy"},
		"manual-agy":     {pane: "w3:p1", status: "idle", ready: true, kind: "agy"},
		"wt-fix":         {pane: "w4:p1", status: "idle", ready: true, kind: "agy"},
	}

	eng.runIdlePatrol(ctx, live, nextMorning)
	trs, _ := st.ListQueuedTriggers(ctx)
	if len(trs) != 1 || !strings.Contains(trs[0].Text, "[corgi morning-digest]") {
		t.Fatalf("expected 1 morning-digest trigger, got %+v", trs)
	}
	txt := trs[0].Text
	if !strings.Contains(txt, "corgi/wt-fix") {
		t.Fatalf("expected worktree item in morning digest:\n%s", txt)
	}
	if !strings.Contains(txt, "old-job-1") {
		t.Fatalf("expected stale Corgi session old-job-1 in morning digest:\n%s", txt)
	}
	if strings.Contains(txt, "kept-ref") {
		t.Fatalf("KEEP session kept-ref should be exempt from morning digest:\n%s", txt)
	}
	if strings.Contains(txt, "Stale Corgi sessions") && strings.Contains(strings.Split(txt, "Stale Corgi sessions")[1], "manual-agy") {
		t.Fatalf("manual session manual-agy must never appear in stale archival list:\n%s", txt)
	}

	// Verify Digest card auto-replacement (never stacks multiple patrol digest cards) + Snooze wake-up!
	_, act1, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread, Text: "早报 1", Options: []string{"处理", "稍后"}, Digest: true, Worker: "wt-fix",
	})
	if err != nil || act1 == nil || act1.Kind != model.ActionKindPatrolDigest {
		t.Fatalf("first digest reply failed: act=%+v err=%v", act1, err)
	}
	_, act2, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread, Text: "早报 2（覆盖旧卡）", Options: []string{"合并", "留着"}, Digest: true, Worker: "wt-fix",
	})
	if err != nil || act2 == nil {
		t.Fatalf("second digest reply failed: %v", err)
	}
	pending, _ := st.ListPendingActions(ctx)
	if len(pending) != 1 || pending[0].ID != act2.ID {
		t.Fatalf("expected only latest digest card pending, got %+v", pending)
	}

	// Snooze act2 until nextMorning+1h -> disappears from pending, wakes up when due.
	wakeAt := nextMorning.Add(time.Hour)
	if _, err := eng.SnoozeAction(ctx, act2.ID, wakeAt); err != nil {
		t.Fatalf("SnoozeAction failed: %v", err)
	}
	if p, _ := st.ListPendingActions(ctx); len(p) != 0 {
		t.Fatalf("expected 0 pending while snoozed, got %d", len(p))
	}
	woken, err := st.WakeDueSnoozedActions(ctx, wakeAt)
	if err != nil || len(woken) != 1 || woken[0].ID != act2.ID {
		t.Fatalf("expected snoozed action to wake at due time, got woken=%+v err=%v", woken, err)
	}
}

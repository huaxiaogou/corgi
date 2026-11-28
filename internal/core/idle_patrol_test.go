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

func TestIdlePatrolGlobalCooldownCoalescingPendingGuardAndHumanActivity(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 11, 26, 10, 26, 0, 0, loc)
	_ = st.SetKV(ctx, kvLastMorningDigestDay, base.Format("2006-01-02"))

	// Seed 3 staggered workers finishing at 10:26, 10:32, 10:33 (reproducing back-to-back patrol storm).
	t1 := base
	t2 := base.Add(6 * time.Minute)
	t3 := base.Add(7 * time.Minute)
	for _, item := range []struct {
		name string
		pane string
		at   *time.Time
	}{
		{"chat-inline-preview-plan", "wD:p1", &t1},
		{"personal-agent-inbox-research", "wF:p1", &t2},
		{"agent-flow-plan", "wG:p1", &t3},
	} {
		_ = st.UpsertWorker(ctx, &model.Worker{
			Name: item.name, Kind: "agy", Cwd: dir, Thread: model.MainThread, PaneID: item.pane,
			CreatedBy: "corgi", CreatedAt: base.Add(-1 * time.Hour), Status: "idle", IdleSince: item.at,
			LastReport: "待验收",
		})
	}
	live := map[string]liveInfo{
		model.RouterName:                {pane: "wV:p1", status: "idle", ready: true, kind: "codebuddy"},
		"chat-inline-preview-plan":      {pane: "wD:p1", status: "idle", ready: true, kind: "agy"},
		"personal-agent-inbox-research": {pane: "wF:p1", status: "idle", ready: true, kind: "agy"},
		"agent-flow-plan":               {pane: "wG:p1", status: "idle", ready: true, kind: "agy"},
	}

	// 1. Recent human message at 10:32 defers patrol at 10:56 (24m < 30m).
	_ = st.InsertEntry(ctx, &model.Entry{
		ID: "e-user-1", Seq: 1, Role: "user", Thread: model.MainThread, Channel: "web",
		Text: "继续", At: base.Add(6 * time.Minute),
	})
	eng.runIdlePatrol(ctx, live, base.Add(30*time.Minute)) // 10:56
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected patrol deferred while human messaged <30m ago, got %d", len(trs))
	}

	// 2. At 11:03 (31m after human message): fires ONCE and coalesces all 3 workers into one batch!
	at1103 := base.Add(37 * time.Minute)
	eng.runIdlePatrol(ctx, live, at1103)
	trs, _ := st.ListQueuedTriggers(ctx)
	if len(trs) != 1 {
		t.Fatalf("expected exactly 1 coalesced patrol trigger, got %d", len(trs))
	}
	for _, name := range []string{"chat-inline-preview-plan", "personal-agent-inbox-research", "agent-flow-plan"} {
		if !strings.Contains(trs[0].Text, name) {
			t.Fatalf("coalesced patrol missing %q:\n%s", name, trs[0].Text)
		}
	}
	trs[0].Status = "delivered"
	_ = st.UpdateTrigger(ctx, trs[0])

	// 3. Even if a 4th worker finishes and crosses 30m 32 seconds later, 2h global cooldown blocks spam!
	t4 := at1103.Add(-29 * time.Minute)
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "media-eval-plan", Kind: "agy", Cwd: dir, Thread: model.MainThread, PaneID: "wH:p1",
		CreatedBy: "corgi", CreatedAt: base.Add(-1 * time.Hour), Status: "idle", IdleSince: &t4,
	})
	live["media-eval-plan"] = liveInfo{pane: "wH:p1", status: "idle", ready: true, kind: "agy"}

	eng.runIdlePatrol(ctx, live, at1103.Add(2*time.Minute)) // 11:05 (< 2h global cooldown)
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected 0 triggers inside 2h global patrol cooldown, got %d", len(trs))
	}

	// 4. After 2h global cooldown (13:05), if an unresolved patrol_digest card is already pending today, still skip!
	_, act, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread, Text: "单条待办", Options: []string{"批准", "暂缓"}, Digest: true,
	})
	if err != nil || act == nil || act.Category != "patrol" {
		t.Fatalf("expected default digest category 'patrol', got act=%+v err=%v", act, err)
	}
	_, _ = eng.DismissAction(ctx, act.ID)
	sameDayAct := &model.Action{
		ID: "a-same-day", Kind: model.ActionKindPatrolDigest, Thread: model.MainThread,
		Category: "patrol", Title: "单条待办", Status: model.ActionPending, CreatedAt: at1103,
		Choices: []model.ActionChoice{{Key: "1", Label: "批准"}},
	}
	if err := st.InsertAction(ctx, sameDayAct); err != nil {
		t.Fatal(err)
	}
	eng.runIdlePatrol(ctx, live, at1103.Add(2*time.Hour+5*time.Minute))
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected 0 triggers while today's patrol_digest is still pending, got %d", len(trs))
	}

	// 5. Once dismissed, after global cooldown passes, media-eval-plan can finally be patrolled cleanly.
	_, _ = eng.DismissAction(ctx, sameDayAct.ID)
	eng.runIdlePatrol(ctx, live, at1103.Add(2*time.Hour+6*time.Minute))
	trs, _ = st.ListQueuedTriggers(ctx)
	if len(trs) != 1 || !strings.Contains(trs[0].Text, "media-eval-plan") {
		t.Fatalf("expected media-eval-plan patrolled after cooldown & digest clear, got %+v", trs)
	}
}

func TestMorningDigestWorkItemCenteredAndHibernatedWorkers(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.FixedZone("CST", 8*3600)
	nextMorning := time.Date(2026, 11, 28, 9, 5, 0, 0, loc)
	yesterday := nextMorning.Add(-14 * time.Hour)

	// 1. Open WorkItem whose worker was already hibernated overnight by v0.1.76 Worker GC (Ended=true)!
	if err := st.CreateWorkItem(ctx, &model.WorkItem{
		ID: "wi-flow", Thread: model.MainThread, Title: "物言 Agent 内部数据流可视化方案",
		NextStep: "交互原型已交付，等待审阅反馈",
		Workers:  []string{"agent-flow-plan"}, CreatedAt: yesterday, UpdatedAt: yesterday,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.SetWorkItemScope(ctx, "wi-flow", "prototype")
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "agent-flow-plan", Kind: "agy", Thread: model.MainThread, WorkItemID: "wi-flow",
		CreatedBy: "corgi", Ended: true, Status: "done", CreatedAt: yesterday,
	})

	// 2. Open implement WorkItem whose hibernated EndedSession still carries an unmerged branch.
	if err := st.CreateWorkItem(ctx, &model.WorkItem{
		ID: "wi-impl", Thread: model.MainThread, Title: "企微通知体验优化",
		NextStep: "分支已跑通测试，待拍板合入",
		Workers:  []string{"agy-impl"}, CreatedAt: yesterday, UpdatedAt: yesterday,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.SetWorkItemScope(ctx, "wi-impl", "implement")
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "agy-impl", Kind: "agy", Thread: model.MainThread, WorkItemID: "wi-impl",
		CreatedBy: "corgi", Ended: true, Status: "done", CreatedAt: yesterday,
	})
	_ = st.SaveEndedSession(ctx, &model.EndedSession{
		ID: "h-impl", Kind: "agy", Name: "agy-impl", WorkItemID: "wi-impl",
		WorktreeBranch: "corgi/agy-impl", Session: "sess-impl", EndedAt: yesterday.Add(2 * time.Hour),
	}, store.EndedSessionRetentionLimit)

	// 3. Closed WorkItem with 2 leftover live workers still holding worktree branches -> MUST be excluded!
	closedAt := nextMorning.Add(-20 * time.Minute)
	if err := st.CreateWorkItem(ctx, &model.WorkItem{
		ID: "wi-closed", Thread: model.MainThread, Title: "会话直聊身份提醒",
		Workers:   []string{"agy-3637", "identity-prototype"},
		CreatedAt: yesterday, UpdatedAt: closedAt, ClosedAt: &closedAt, CloseNote: "已发布 v0.1.75",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = st.SetWorkItemScope(ctx, "wi-closed", "implement")
	for _, name := range []string{"agy-3637", "identity-prototype"} {
		_ = st.UpsertWorker(ctx, &model.Worker{
			Name: name, Kind: "agy", Thread: model.MainThread, PaneID: "w-" + name + ":p1",
			WorkItemID: "wi-closed", WorktreeBranch: "corgi/" + name,
			CreatedBy: "corgi", Status: "idle", IdleSince: &yesterday, CreatedAt: yesterday,
		})
	}

	live := map[string]liveInfo{
		model.RouterName:     {pane: "wR:p1", status: "idle", ready: true, kind: "codebuddy"},
		"agy-3637":           {pane: "w-agy-3637:p1", status: "idle", ready: true, kind: "agy"},
		"identity-prototype": {pane: "w-identity-prototype:p1", status: "idle", ready: true, kind: "agy"},
	}

	eng.runIdlePatrol(ctx, live, nextMorning)
	trs, _ := st.ListQueuedTriggers(ctx)
	if len(trs) != 1 || !strings.Contains(trs[0].Text, "[corgi morning-digest]") {
		t.Fatalf("expected 1 morning-digest trigger, got %+v", trs)
	}
	txt := trs[0].Text
	if !strings.Contains(txt, "物言 Agent 内部数据流可视化方案") {
		t.Fatalf("expected hibernated open WorkItem surfaced in morning digest:\n%s", txt)
	}
	if !strings.Contains(txt, "企微通知体验优化") || !strings.Contains(txt, "corgi/agy-impl") {
		t.Fatalf("expected open implement WorkItem + branch surfaced in morning digest:\n%s", txt)
	}
	if strings.Contains(txt, "agy-3637") || strings.Contains(txt, "identity-prototype") || strings.Contains(txt, "会话直聊身份提醒") {
		t.Fatalf("closed WorkItem workers/worktrees MUST NOT appear in morning digest:\n%s", txt)
	}

	// Same-day idle patrol must also skip closed WorkItem workers!
	trs[0].Status = "delivered"
	_ = st.UpdateTrigger(ctx, trs[0])
	_ = st.DeleteKV(ctx, kvLastIdlePatrolAt)
	eng.runIdlePatrol(ctx, live, nextMorning.Add(3*time.Hour))
	trs2, _ := st.ListQueuedTriggers(ctx)
	if len(trs2) != 0 {
		t.Fatalf("expected same-day patrol to skip closed WorkItem workers, got %+v", trs2)
	}
}

func TestIdlePatrolSkipsWhenNativeAPIRouterWorking(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"codex"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := time.FixedZone("CST", 8*3600)
	day := time.Date(2026, 11, 25, 17, 0, 0, 0, loc)
	_ = st.SetKV(ctx, kvLastMorningDigestDay, day.Format("2006-01-02"))

	idleAt := day
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name:       "codex-idle",
		Kind:       "codex",
		Cwd:        "/root",
		Thread:     model.MainThread,
		PaneID:     "wM:p1",
		CreatedBy:  "corgi",
		CreatedAt:  day.Add(-10 * time.Minute),
		Status:     "idle",
		IdleSince:  &idleAt,
		LastReport: "等待确认",
	})
	// Note: live map has NO model.RouterName pane in native API mode!
	live := map[string]liveInfo{
		"codex-idle": {pane: "wM:p1", status: "idle", ready: true, kind: "codex"},
	}

	// Simulate an active in-process native API Router turn (trigger already acked by mutating tool).
	eng.mu.Lock()
	eng.router.turn = &apiRouterTurn{thread: model.MainThread, triggerID: "t_active"}
	eng.mu.Unlock()

	eng.runIdlePatrol(ctx, live, day.Add(45*time.Minute))
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("expected idle patrol skipped while native API router turn is active, got %d", len(trs))
	}

	// Once turn completes, patrol fires normally.
	eng.mu.Lock()
	eng.router.turn = nil
	eng.mu.Unlock()

	eng.runIdlePatrol(ctx, live, day.Add(46*time.Minute))
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 1 {
		t.Fatalf("expected idle patrol to fire once native API router is idle, got %d", len(trs))
	}
}

package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

func strPtr(s string) *string { return &s }

func TestWorkerGC_RulesP1P2P3P4AndKillSwitch(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu       sync.Mutex
		panes    = []map[string]any{}
		closedWS = []string{}
		screen   = "ready >\n"
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.list":
			return map[string]any{"panes": panes}, nil
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": screen}}, nil
		case "workspace.close":
			if id, ok := params["workspace_id"].(string); ok {
				closedWS = append(closedWS, id)
			}
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude", "codex", "agy", "codebuddy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.gc.gracePeriod = -1
	eng.gc.activityCooldown = -1

	ctx := context.Background()
	now := time.Now()
	idle20m := now.Add(-20 * time.Minute)
	idle130m := now.Add(-130 * time.Minute)
	idle35m := now.Add(-35 * time.Minute)
	idle25m := now.Add(-25 * time.Minute)
	closedAt := now.Add(-30 * time.Minute)
	faultAt35m := now.Add(-35 * time.Minute)

	// Closed WorkItem for P1.
	_ = st.CreateWorkItem(ctx, &model.WorkItem{
		ID: "wi-closed", Thread: model.MainThread, Title: "已完结任务",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: closedAt, ClosedAt: &closedAt,
	})

	// P1 candidate: closed work item + idle 20m + worktree preserved.
	wtDir := filepath.Join(dir, "fake-wt-p1")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "claude-p1", Kind: "claude", Cwd: wtDir, Thread: model.MainThread,
		PaneID: "w1:p1", WorkspaceID: "w1", CreatedBy: "corgi", CreatedAt: now.Add(-2 * time.Hour),
		Status: "idle", IdleSince: &idle20m, Session: "sess-p1", WorkItemID: "wi-closed",
		RepoRoot: dir, WorktreePath: wtDir, WorktreeBranch: "corgi/claude-p1",
	})

	// P2 candidate: proven initial startup-failed empty shell (StartupFailed=true, HadTurn=false).
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "agy-p2", Kind: "agy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w2:p1", WorkspaceID: "w2", CreatedBy: "corgi", CreatedAt: faultAt35m,
		Status: "failed", FaultError: "Failed to start a conversation", FaultPaneID: "w2:p1",
		FaultAt: &faultAt35m, StartupFailed: true,
	})
	// Non-P2 faulted worker: established worker that already ran a turn (StartupFailed=false, HadTurn=true).
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "codebuddy-established", Kind: "codebuddy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w3:p1", WorkspaceID: "w3", CreatedBy: "corgi", CreatedAt: now.Add(-3 * time.Hour),
		Status: "failed", FaultError: "quota exceeded", FaultPaneID: "w3:p1",
		FaultAt: &faultAt35m, HadTurn: true, StartupFailed: false,
	})

	// P3 candidate: routine idle >= 120m across codex with pane-bound session.
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "codex-p3", Kind: "codex", Cwd: dir, Thread: model.MainThread,
		PaneID: "w4:p1", WorkspaceID: "w4", CreatedBy: "router", CreatedAt: now.Add(-3 * time.Hour),
		Status: "done", IdleSince: &idle130m, Session: "sess-p3",
	})
	// Sessionless non-closed worker: MUST be skipped by P3 even if idle >= 120m.
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "codebuddy-nosess", Kind: "codebuddy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w5:p1", WorkspaceID: "w5", CreatedBy: "corgi", CreatedAt: now.Add(-3 * time.Hour),
		Status: "idle", IdleSince: &idle130m,
	})

	// P4 candidates: idle 35m and 25m (below 120m P3 threshold, above 15m P4 cooldown).
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "agy-cap-old", Kind: "agy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w6:p1", WorkspaceID: "w6", CreatedBy: "corgi", CreatedAt: now.Add(-1 * time.Hour),
		Status: "idle", IdleSince: &idle35m, Session: "sess-cap-old",
	})
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "agy-cap-new", Kind: "agy", Cwd: dir, Thread: model.MainThread,
		PaneID: "w7:p1", WorkspaceID: "w7", CreatedBy: "corgi", CreatedAt: now.Add(-1 * time.Hour),
		Status: "idle", IdleSince: &idle25m, Session: "sess-cap-new",
	})

	rawAgents := []model.Agent{
		{Name: strPtr("claude-p1"), PaneID: "w1:p1", WorkspaceID: "w1", Agent: strPtr("claude"), AgentStatus: "idle", Session: &model.AgentSession{Kind: "id", Value: "sess-p1"}},
		{Name: strPtr("agy-p2"), PaneID: "w2:p1", WorkspaceID: "w2", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("codebuddy-established"), PaneID: "w3:p1", WorkspaceID: "w3", Agent: strPtr("codebuddy"), AgentStatus: "idle"},
		{Name: strPtr("codex-p3"), PaneID: "w4:p1", WorkspaceID: "w4", Agent: strPtr("codex"), AgentStatus: "done", Session: &model.AgentSession{Kind: "id", Value: "sess-p3"}},
		{Name: strPtr("codebuddy-nosess"), PaneID: "w5:p1", WorkspaceID: "w5", Agent: strPtr("codebuddy"), AgentStatus: "idle"},
		{Name: strPtr("agy-cap-old"), PaneID: "w6:p1", WorkspaceID: "w6", Agent: strPtr("agy"), AgentStatus: "idle", Session: &model.AgentSession{Kind: "id", Value: "sess-cap-old"}},
		{Name: strPtr("agy-cap-new"), PaneID: "w7:p1", WorkspaceID: "w7", Agent: strPtr("agy"), AgentStatus: "idle", Session: &model.AgentSession{Kind: "id", Value: "sess-cap-new"}},
	}
	live := map[string]liveInfo{
		"claude-p1":             {pane: "w1:p1", status: "idle", ready: true, kind: "claude", session: "sess-p1"},
		"agy-p2":                {pane: "w2:p1", status: "idle", ready: true, kind: "agy"},
		"codebuddy-established": {pane: "w3:p1", status: "idle", ready: true, kind: "codebuddy"},
		"codex-p3":              {pane: "w4:p1", status: "done", ready: true, kind: "codex", session: "sess-p3"},
		"codebuddy-nosess":      {pane: "w5:p1", status: "idle", ready: true, kind: "codebuddy"},
		"agy-cap-old":           {pane: "w6:p1", status: "idle", ready: true, kind: "agy", session: "sess-cap-old"},
		"agy-cap-new":           {pane: "w7:p1", status: "idle", ready: true, kind: "agy", session: "sess-cap-new"},
	}
	scanned := map[string]bool{"": true}

	// 1. Kill switch (-1) disables ALL P1-P4 rules.
	sOff := eng.Settings()
	sOff.WorkerIdleGCMinutes = -1
	sOff.WorkerMaxLivePerMachine = 2
	if cands := eng.selectWorkerGCCandidates(ctx, sOff, rawAgents, live, scanned, now); len(cands) != 0 {
		t.Fatalf("expected 0 candidates when WorkerIdleGCMinutes=-1, got %+v", cands)
	}

	// 2. Active GC with maxLive=3: total 7 live Corgi workers - 3 (P1+P2+P3) = 4 remaining > cap 3 => 1 P4 eviction (oldest: agy-cap-old).
	sOn := eng.Settings()
	sOn.WorkerIdleGCMinutes = 120
	sOn.WorkerMaxLivePerMachine = 3
	cands := eng.selectWorkerGCCandidates(ctx, sOn, rawAgents, live, scanned, now)
	gotOrder := make([]string, 0, len(cands))
	for _, c := range cands {
		gotOrder = append(gotOrder, c.rule+":"+c.name)
	}
	wantOrder := []string{"P1:claude-p1", "P2:agy-p2", "P3:codex-p3", "P4:agy-cap-old"}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("GC selection order = %v, want %v", gotOrder, wantOrder)
	}

	// 3. Execute hibernation on P1 and verify worktree directory is NEVER deleted & session archived cleanly.
	mu.Lock()
	panes = []map[string]any{
		{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "idle"},
	}
	mu.Unlock()
	eng.hibernateWorker(ctx, cands[0])

	mu.Lock()
	if !slices.Equal(closedWS, []string{"w1"}) {
		t.Fatalf("expected workspace w1 closed, got %v", closedWS)
	}
	mu.Unlock()
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("hibernation must NEVER delete worktree dir %s: %v", wtDir, err)
	}
	wAfter, err := st.GetWorker(ctx, "claude-p1")
	if err != nil || !wAfter.Ended || wAfter.Hibernating || wAfter.Status != "done" || wAfter.WorktreePath != wtDir {
		t.Fatalf("unexpected worker state after hibernation: %+v (err=%v)", wAfter, err)
	}
	ended, _ := st.ListEndedSessions(ctx, store.EndedSessionRetentionLimit)
	if len(ended) != 1 || ended[0].Session != "sess-p1" || ended[0].WorktreePath != wtDir || ended[0].CreatedBy != "corgi" {
		t.Fatalf("expected rich EndedSession archived, got %+v", ended)
	}
}

func TestWorkerGC_SafetyVetoes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu       sync.Mutex
		panes    = []map[string]any{}
		closedWS = []string{}
		screen   = "ready >\n"
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.list":
			return map[string]any{"panes": panes}, nil
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": screen}}, nil
		case "workspace.close":
			if id, ok := params["workspace_id"].(string); ok {
				closedWS = append(closedWS, id)
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
	eng.gc.gracePeriod = -1
	eng.gc.activityCooldown = -1

	ctx := context.Background()
	now := time.Now()
	oldIdle := now.Add(-4 * time.Hour)

	addIdleWorker := func(name, by, ws, pane, sess, wiID string, keep bool) {
		_ = st.UpsertWorker(ctx, &model.Worker{
			Name: name, Kind: "agy", Cwd: dir, Thread: model.MainThread,
			PaneID: pane, WorkspaceID: ws, CreatedBy: by, CreatedAt: oldIdle,
			Status: "idle", IdleSince: &oldIdle, Session: sess, WorkItemID: wiID, Keep: keep,
		})
	}
	addIdleWorker("manual-you", "you", "w1", "w1:p1", "s1", "", false)
	addIdleWorker("adopted-w", "adopted", "w2", "w2:p1", "s2", "", false)
	addIdleWorker("legacy-empty", "", "w3", "w3:p1", "s3", "", false)
	addIdleWorker("kept-w", "corgi", "w4", "w4:p1", "s4", "", true)
	addIdleWorker("pending-act-w", "corgi", "w5", "w5:p1", "s5", "wi-act", false)
	addIdleWorker("snoozed-act-w", "corgi", "w6", "w6:p1", "s6", "wi-snz", false)
	addIdleWorker("wecom-focus-w", "corgi", "w7", "w7:p1", "s7", "", false)
	addIdleWorker("queued-prompt-w", "corgi", "w8", "w8:p1", "s8", "", false)
	addIdleWorker("eligible-w", "corgi", "w9", "w9:p1", "s9", "", false)

	_ = st.InsertAction(ctx, &model.Action{
		ID: "a-pend", Kind: model.ActionKindRouterChoice, Thread: model.MainThread,
		WorkItemID: "wi-act", Title: "待拍板", Status: model.ActionPending, CreatedAt: now,
	})
	snzUntil := now.Add(2 * time.Hour)
	_ = st.InsertAction(ctx, &model.Action{
		ID: "a-snz", Kind: model.ActionKindRouterChoice, Thread: model.MainThread,
		WorkItemID: "wi-snz", Title: "暂缓中", Status: model.ActionSnoozed, SnoozeUntil: &snzUntil, CreatedAt: now,
	})
	_ = eng.SaveWeComFocus(ctx, WeComFocusKVPrefix+"user1", &WeComFocus{Worker: "wecom-focus-w"})
	_ = st.InsertQueuedPrompt(ctx, &model.QueuedPrompt{
		ID: "q1", Seq: 1, Scope: "w8:p1", PaneID: "w8:p1", Text: "follow up", CreatedAt: now,
	})

	rawAgents := []model.Agent{
		{Name: strPtr("manual-you"), PaneID: "w1:p1", WorkspaceID: "w1", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("adopted-w"), PaneID: "w2:p1", WorkspaceID: "w2", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("legacy-empty"), PaneID: "w3:p1", WorkspaceID: "w3", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("kept-w"), PaneID: "w4:p1", WorkspaceID: "w4", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("pending-act-w"), PaneID: "w5:p1", WorkspaceID: "w5", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("snoozed-act-w"), PaneID: "w6:p1", WorkspaceID: "w6", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("wecom-focus-w"), PaneID: "w7:p1", WorkspaceID: "w7", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("queued-prompt-w"), PaneID: "w8:p1", WorkspaceID: "w8", Agent: strPtr("agy"), AgentStatus: "idle"},
		{Name: strPtr("eligible-w"), PaneID: "w9:p1", WorkspaceID: "w9", Agent: strPtr("agy"), AgentStatus: "idle"},
	}
	live := map[string]liveInfo{}
	for _, a := range rawAgents {
		live[*a.Name] = liveInfo{pane: a.PaneID, status: "idle", ready: true, kind: "agy"}
	}

	cands := eng.selectWorkerGCCandidates(ctx, eng.Settings(), rawAgents, live, map[string]bool{"": true}, now)
	if len(cands) != 1 || cands[0].name != "eligible-w" {
		t.Fatalf("expected ONLY eligible-w selected, got %+v", cands)
	}
	cand := cands[0]

	// 1. Background task on screen vetoes hibernation.
	mu.Lock()
	screen = "Done.\n2 background tasks running\n"
	panes = []map[string]any{{"pane_id": "w9:p1", "workspace_id": "w9", "agent_status": "idle"}}
	mu.Unlock()
	eng.hibernateWorker(ctx, cand)
	if len(closedWS) != 0 {
		t.Fatalf("background task should veto hibernation, closed=%v", closedWS)
	}

	// 2. Multi-pane workspace (user shell pane in same workspace) vetoes hibernation.
	mu.Lock()
	screen = "ready >\n"
	panes = []map[string]any{
		{"pane_id": "w9:p1", "workspace_id": "w9", "agent_status": "idle"},
		{"pane_id": "w9:p2", "workspace_id": "w9", "agent_status": ""},
	}
	mu.Unlock()
	eng.hibernateWorker(ctx, cand)
	if len(closedWS) != 0 {
		t.Fatalf("multi-pane workspace must NEVER be closed by GC, closed=%v", closedWS)
	}

	// 3. Foreground turn started right before close (pane.list shows working) vetoes hibernation.
	mu.Lock()
	panes = []map[string]any{
		{"pane_id": "w9:p1", "workspace_id": "w9", "agent_status": "working"},
	}
	mu.Unlock()
	eng.hibernateWorker(ctx, cand)
	if len(closedWS) != 0 {
		t.Fatalf("working pane must NEVER be interrupted by GC, closed=%v", closedWS)
	}

	// 4. Active beginWorkerOp reservation blocks GC cleanly.
	mu.Lock()
	panes = []map[string]any{
		{"pane_id": "w9:p1", "workspace_id": "w9", "agent_status": "idle"},
	}
	mu.Unlock()
	if !eng.beginWorkerOp("eligible-w") {
		t.Fatal("expected beginWorkerOp to succeed")
	}
	eng.hibernateWorker(ctx, cand)
	eng.endWorkerOp("eligible-w")
	if len(closedWS) != 0 {
		t.Fatalf("active workerOp must block GC, closed=%v", closedWS)
	}
}

func TestWorkerGC_HibernatingCrashReconciliation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Now()
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "w-gone", Kind: "claude", Cwd: dir, Thread: model.MainThread,
		PaneID: "w1:p1", WorkspaceID: "w1", CreatedBy: "corgi", CreatedAt: now.Add(-time.Hour),
		Status: "idle", Session: "sess-gone", Hibernating: true,
	})
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "w-still-live", Kind: "claude", Cwd: dir, Thread: model.MainThread,
		PaneID: "w2:p1", WorkspaceID: "w2", CreatedBy: "corgi", CreatedAt: now.Add(-time.Hour),
		Status: "idle", Session: "sess-live", Hibernating: true,
	})

	// 1. Unscanned machine must NOT finalize or clear Hibernating.
	eng.syncWorkers(ctx, map[string]liveInfo{}, map[string]bool{"": false}, now)
	if w, _ := st.GetWorker(ctx, "w-gone"); !w.Hibernating || w.Ended {
		t.Fatalf("unscanned machine should leave Hibernating untouched, got %+v", w)
	}

	// 2. Fresh scan: w-gone disappeared (quietly finalized, NO Router alert); w-still-live is alive (Hibernating cleared).
	eng.syncWorkers(ctx, map[string]liveInfo{
		"w-still-live": {pane: "w2:p1", status: "idle", ready: true, kind: "claude", session: "sess-live"},
	}, map[string]bool{"": true}, now)

	wGone, _ := st.GetWorker(ctx, "w-gone")
	if !wGone.Ended || wGone.Hibernating || wGone.Status != "done" {
		t.Fatalf("expected w-gone quietly finalized as Ended+done, got %+v", wGone)
	}
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("reconciled hibernated worker must NOT alert Router with gone trigger, got %+v", trs)
	}
	ended, _ := st.ListEndedSessions(ctx, store.EndedSessionRetentionLimit)
	if len(ended) != 1 || ended[0].Session != "sess-gone" {
		t.Fatalf("expected sess-gone archived during reconciliation, got %+v", ended)
	}

	wAlive, _ := st.GetWorker(ctx, "w-still-live")
	if wAlive.Ended || wAlive.Hibernating {
		t.Fatalf("expected live worker to clear Hibernating and stay active, got %+v", wAlive)
	}
}

func TestWorkerGC_LosslessHibernateAndResumeMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Create fake worktree directory so resolveResumeCwd finds it intact.
	wtDir := filepath.Join(dir, "wt-feature")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		createdCwd  string
		startedArgs []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.list":
			return map[string]any{"panes": []any{
				map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "idle"},
			}}, nil
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": "ready >\n"}}, nil
		case "workspace.close":
			return map[string]any{}, nil
		case "workspace.create":
			if c, ok := params["cwd"].(string); ok {
				createdCwd = c
			}
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w99"},
				"root_pane": map[string]any{"pane_id": "w99:p1"},
			}, nil
		case "agent.start":
			if raw, ok := params["args"].([]any); ok {
				startedArgs = nil
				for _, a := range raw {
					if s, ok := a.(string); ok {
						startedArgs = append(startedArgs, s)
					}
				}
			}
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.gc.gracePeriod = -1
	eng.gc.activityCooldown = -1

	ctx := context.Background()
	now := time.Now()
	oldIdle := now.Add(-3 * time.Hour)
	_ = st.CreateWorkItem(ctx, &model.WorkItem{
		ID: "wi-open", Thread: "wecom:grp1", Title: "功能开发",
		CreatedAt: oldIdle, UpdatedAt: oldIdle,
	})
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "feat-dev", Kind: "claude", Machine: "", Cwd: wtDir, Thread: "wecom:grp1",
		Title: "功能开发会话", PaneID: "w1:p1", WorkspaceID: "w1", CreatedBy: "corgi",
		CreatedAt: oldIdle, Status: "idle", IdleSince: &oldIdle, Session: "sess-feat",
		WorkItemID: "wi-open", Constraint: "implement",
		RepoRoot: dir, RepoCommonDir: filepath.Join(dir, ".git"),
		WorktreePath: wtDir, WorktreeBranch: "corgi/feat-dev", WorktreeBase: "abc123",
	})
	if err := eng.Subscribe(ctx, "feat-dev", "wecom:grp1", false); err != nil {
		t.Fatal(err)
	}

	// 1. Hibernate worker.
	eng.hibernateWorker(ctx, workerGCCandidate{
		name: "feat-dev", paneID: "w1:p1", workspaceID: "w1", machine: "",
		session: "sess-feat", rule: "P3", reason: "持续空闲 3h", idleDur: 3 * time.Hour,
	})

	// 2. Verify WatchEndings' SaveEndedSessionIfAbsent and ForgetEndedSessionIfLive NEVER overwrite or delete the rich archive.
	_ = st.SaveEndedSessionIfAbsent(ctx, &model.EndedSession{
		ID: "h-weak", Kind: "claude", Machine: "", Cwd: "/wrong", Name: "feat-dev",
		Session: "sess-feat", EndedAt: time.Now(),
	}, store.EndedSessionRetentionLimit)
	_ = st.ForgetEndedSessionIfLive(ctx, "claude", "", "sess-feat", "feat-dev")

	ended, _ := st.ListEndedSessions(ctx, store.EndedSessionRetentionLimit)
	if len(ended) != 1 || ended[0].WorktreePath != wtDir || ended[0].CreatedBy != "corgi" || !slices.Equal(ended[0].Subscribers, []string{"wecom:grp1"}) {
		t.Fatalf("rich archive must survive WatchEndings calls intact, got %+v", ended)
	}
	archiveID := ended[0].ID

	// 3. Resume via Web History path (EndedSessionID) and verify ALL metadata + worktree + subscription restored.
	resumed, didResume, err := eng.ResumeWorkerSession(ctx, ResumeWorkerRequest{EndedSessionID: archiveID})
	if err != nil || !didResume {
		t.Fatalf("ResumeWorkerSession failed: didResume=%v err=%v", didResume, err)
	}
	if resumed.CreatedBy != "corgi" || resumed.Thread != "wecom:grp1" || resumed.Title != "功能开发会话" ||
		resumed.WorktreePath != wtDir || resumed.WorktreeBranch != "corgi/feat-dev" || resumed.WorktreeBase != "abc123" ||
		resumed.RepoRoot != dir || resumed.WorkItemID != "wi-open" || resumed.Constraint != "implement" {
		t.Fatalf("resume lost metadata: %+v", resumed)
	}
	mu.Lock()
	if createdCwd != wtDir {
		t.Fatalf("expected resumed workspace cwd=%s, got %s", wtDir, createdCwd)
	}
	if !strings.Contains(strings.Join(startedArgs, " "), "sess-feat") {
		t.Fatalf("expected resume args to contain sess-feat, got %v", startedArgs)
	}
	mu.Unlock()
	th, err := st.GetThread(ctx, "wecom:grp1")
	if err != nil || !slices.Contains(th.Subscribers, "feat-dev") {
		t.Fatalf("expected thread subscription restored after resume, got %+v (err=%v)", th, err)
	}

	// 4. Verify Spawn --resume preserves Hub-local machine ("") even when default WorkerMachine is remote,
	//    and preserves worktree metadata & CreatedBy.
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "hub-worker", Kind: "claude", Machine: "", Cwd: wtDir, Thread: model.MainThread,
		Title: "本机任务", CreatedBy: "corgi", CreatedAt: oldIdle, Status: "done", Ended: true,
		Session: "sess-hub-1", Keep: true, RepoRoot: dir, WorktreePath: wtDir,
		WorktreeBranch: "corgi/hub-worker", WorktreeBase: "def456",
	})
	eng.mu.Lock()
	eng.settings.WorkerMachine = "offline-remote-node"
	eng.mu.Unlock()
	spawned, err := eng.Spawn(ctx, SpawnRequest{
		Resume: "hub-worker",
		Task:   "继续处理",
	})
	if err != nil {
		t.Fatalf("Spawn --resume should preserve Hub-local machine instead of default remote WorkerMachine: %v", err)
	}
	if spawned.Machine != "" || spawned.CreatedBy != "corgi" || !spawned.Keep ||
		spawned.WorktreePath != wtDir || spawned.WorktreeBranch != "corgi/hub-worker" || spawned.WorktreeBase != "def456" {
		t.Fatalf("Spawn --resume lost metadata: %+v", spawned)
	}
}

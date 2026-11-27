package core

import (
	"context"
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

func TestBusyWorkerQueuesRoutineRouterTellsAndCoalesces(t *testing.T) {
	list := []*model.Trigger{
		{ID: "t3323", Seq: 3323, Source: "router", Text: "加入上下滑循环"},
		{ID: "t3327", Seq: 3327, Source: "router", Text: "不要默认折叠"},
		{ID: "t3328", Seq: 3328, Source: "wecom", Text: "普通追问"},
	}
	if got := filterWorkerSteeringTriggers(list); len(got) != 0 {
		t.Fatalf("expected routine router/wecom triggers to stay queued while worker is busy, got %+v", got)
	}
	list = append(list, &model.Trigger{ID: "t3329", Seq: 3329, Source: "router", Steer: true, Text: "显式插话"})
	got := filterWorkerSteeringTriggers(list)
	if len(got) != 1 || got[0].ID != "t3329" {
		t.Fatalf("expected only explicit Steer trigger to bypass busy worker queue, got %+v", got)
	}
}

func TestWorkerTurnWatermarkSuppressesStaleAndHonestStatusTagging(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wk := &model.Worker{Name: "agy-85", PaneID: "w7:p1@mac", Session: "sess-1", Thread: model.MainThread}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}

	// Issue #t3323 (delivered) and #t3327 (still queued in Hub while worker is busy).
	t1 := &model.Trigger{ID: "t3323", Seq: 3323, Source: "router", Thread: model.MainThread, Targets: []string{wk.Name}, Status: "delivered"}
	eng.recordWorkerIssued(wk, t1.ID, t1.Seq)
	eng.recordWorkerDelivered(wk.Name, wk.PaneID, []*model.Trigger{t1})

	t2, err := eng.Ingest(ctx, "router", model.MainThread, model.RouterName, "补充不要折叠", "", wk.Name, wk.Name)
	if err != nil {
		t.Fatal(err)
	}
	eng.recordWorkerIssued(wk, t2.ID, t2.Seq)

	now := time.Now()
	// 1. Replay 17:03:02 fake idle (AfterLastUser=false, fallback text from 16:47) -> MUST suppress and arm pendingCheck.
	staleSnap := workerTurnSnapshot{
		Text:           "16:47 旧版 4 套折叠原型",
		FromTranscript: true,
		AfterLastUser:  false,
		AssistantAt:    500,
		LastUserAt:     1200,
		LastUserTID:    "t3323",
		LastUserSeq:    3323,
	}
	if eng.shouldEmitWorkerTurnSummary(wk, staleSnap, now) {
		t.Fatal("expected pre-user stale assistant reply to be suppressed")
	}
	if !eng.hasWorkerPendingCheck(wk.Name, now.Add(2*time.Second)) {
		t.Fatal("expected pendingCheck armed after empty-body idle flicker")
	}

	// 2. While #t2 is still queued in Hub and Worker observed #t3323, turn status note must state factual gap honestly.
	note := eng.formatWorkerTurnStatusNote(ctx, wk, staleSnap)
	if !strings.Contains(note, "#t3323") || !strings.Contains(note, "#"+t2.ID) || !strings.Contains(note, "Hub 队列待派发") {
		t.Fatalf("unexpected queued turn status note: %q", note)
	}

	// 3. Delayed flush writes real new assistant output at offset 2100 -> emits once, clears pendingCheck, dedupes repeat.
	freshSnap := workerTurnSnapshot{
		Text:           "已完成无限上下滑滚牌原型",
		FromTranscript: true,
		AfterLastUser:  true,
		AssistantAt:    2100,
		LastUserAt:     1200,
		LastUserTID:    "t3323",
		LastUserSeq:    3323,
	}
	if !eng.shouldEmitWorkerTurnSummary(wk, freshSnap, now.Add(3*time.Second)) {
		t.Fatal("expected fresh post-user assistant output to emit")
	}
	if eng.hasWorkerPendingCheck(wk.Name, now.Add(4*time.Second)) {
		t.Fatal("expected pendingCheck cleared after emitting fresh output")
	}
	if eng.shouldEmitWorkerTurnSummary(wk, freshSnap, now.Add(5*time.Second)) {
		t.Fatal("expected identical assistant offset to be deduplicated")
	}

	// 4. Same text in a brand-new turn (higher AssistantAt) MUST still emit (no global content hash false suppression).
	nextTurnSnap := workerTurnSnapshot{
		Text:           "已完成无限上下滑滚牌原型",
		FromTranscript: true,
		AfterLastUser:  true,
		AssistantAt:    3400,
		LastUserAt:     2800,
		LastUserTID:    t2.ID,
		LastUserSeq:    t2.Seq,
	}
	if !eng.shouldEmitWorkerTurnSummary(wk, nextTurnSnap, now.Add(10*time.Second)) {
		t.Fatal("expected identical text at a higher turn offset to emit")
	}

	// 5. Once queued trigger is delivered AND observed in transcript, status note clears to empty string.
	t2.Targets = nil
	t2.Status = "delivered"
	_ = st.UpdateTrigger(ctx, t2)
	eng.recordWorkerDelivered(wk.Name, wk.PaneID, []*model.Trigger{t2})
	if cleanNote := eng.formatWorkerTurnStatusNote(ctx, wk, nextTurnSnap); cleanNote != "" {
		t.Fatalf("expected empty turn status note when caught up, got %q", cleanNote)
	}
}

func TestWorkerStartupCheckCatchesImmediateExecutorCrashAndPausesInstance(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	chrome := "────────────────────────────────\n>\n────────────────────────────────\n? for shortcuts"
	var (
		mu          sync.Mutex
		screenText  = "⚠ failed to construct executor: plan model not specified; you must specify a valid model\nError ID: ac007c06-17\n" + chrome
		promptCalls int
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{"pane_id": "wK:p1", "source": "recent", "text": screenText}}, nil
		case "agent.prompt":
			promptCalls++
			txt, _ := params["text"].(string)
			screenText = strings.Join([]string{
				"Error ID: ac007c06-17",
				"────────────────────────────────",
				"> " + txt,
				"⚠ failed to construct executor: plan model not specified; you must specify a valid model",
				"Error ID: ac007c06-19",
				chrome,
			}, "\n")
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.workerStartupProbeDelay = 10 * time.Millisecond
	eng.workerStartupProbeInterval = 10 * time.Millisecond
	eng.workerStartupCheckWindow = 500 * time.Millisecond

	ctx := context.Background()
	wk := &model.Worker{
		Name:       "repo-state-check",
		Kind:       "agy",
		PaneID:     "wK:p1",
		Thread:     model.MainThread,
		Status:     "idle",
		WorkItemID: "wi3420",
		CreatedAt:  time.Now().Add(-time.Hour),
	}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachWorkerAndUpsert(ctx, wk, "", "wi3420", "排查已看提醒交互", "等待检查"); err != nil {
		t.Fatal(err)
	}

	trig, _, err := eng.SendToWorkerWithWork(ctx, wk.Name, model.MainThread, "继续 wi3420 只读排查，禁止改代码。", "wi3420")
	if err != nil {
		t.Fatalf("SendToWorkerWithWork: %v", err)
	}

	live := map[string]liveInfo{
		wk.Name: {pane: "wK:p1", kind: "agy", status: "idle", ready: true},
	}
	scanned := map[string]bool{"": true}

	// Deliver batch to the broken idle pane; pane stays idle (never transitions to working!).
	eng.deliverQueued(ctx, live, scanned)
	if promptCalls != 1 {
		t.Fatalf("expected 1 prompt call, got %d", promptCalls)
	}
	if !eng.hasWorkerStartupCheck(wk.Name) {
		t.Fatal("expected startup check armed right after delivery")
	}

	// Simulate ticks after probeDelay while worker remains idle.
	now := time.Now().Add(50 * time.Millisecond)
	eng.syncWorkers(ctx, live, scanned, now)
	eng.syncWorkers(ctx, live, scanned, now.Add(50*time.Millisecond))

	// 1. Worker must be marked failed + faulted and remain failed across steady idle ticks.
	gotWk, err := st.GetWorker(ctx, wk.Name)
	if err != nil {
		t.Fatal(err)
	}
	if gotWk.Status != "failed" || !strings.Contains(gotWk.FaultError, "plan model not specified") || gotWk.FaultPaneID != "wK:p1" {
		t.Fatalf("expected faulted worker state persisted, got %+v", gotWk)
	}

	// 2. Exactly ONE Router alert must be emitted carrying worker, trigger ID, WorkItemID, and error.
	queued, err := st.ListQueuedTriggers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var alerts []*model.Trigger
	for _, q := range queued {
		if q.Source == "worker" && q.From == wk.Name {
			alerts = append(alerts, q)
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("expected exactly 1 startup fault alert to Router, got %d: %+v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0].Text, "#"+trig.ID) || !strings.Contains(alerts[0].Text, "wi3420") || !strings.Contains(alerts[0].Text, "plan model not specified") {
		t.Fatalf("alert missing key evidence: %q", alerts[0].Text)
	}

	// 3. Subsequent SendToWorkerWithWork to the faulted instance must be rejected cleanly.
	if _, _, sendErr := eng.SendToWorkerWithWork(ctx, wk.Name, model.MainThread, "再试一次", "wi3420"); sendErr == nil || !strings.Contains(sendErr.Error(), "paused") {
		t.Fatalf("expected send to faulted worker rejected, got %v", sendErr)
	}

	// 4. Pre-existing queued trigger targeting the faulted worker must be rerouted, NEVER sent to PTY.
	_, err = eng.Ingest(ctx, "router", model.MainThread, model.RouterName, "旧队列消息", "", wk.Name, wk.Name)
	if err != nil {
		t.Fatal(err)
	}
	eng.deliverQueued(ctx, live, scanned)
	if promptCalls != 1 {
		t.Fatalf("expected faulted worker NOT to receive another prompt, got %d calls", promptCalls)
	}

	// 5. If the worker later genuinely enters working (recovered), fault clears automatically.
	liveWorking := map[string]liveInfo{
		wk.Name: {pane: "wK:p1", kind: "agy", status: "working", ready: true},
	}
	eng.syncWorkers(ctx, liveWorking, scanned, now.Add(time.Second))
	recoveredWk, _ := st.GetWorker(ctx, wk.Name)
	if recoveredWk.Status != "working" || recoveredWk.FaultError != "" {
		t.Fatalf("expected fault cleared on genuine working transition, got %+v", recoveredWk)
	}
}

func TestWorkerStartupCheckZeroFalsePositivesOnOldScrollbackAndBenignWarning(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	chrome := "────────────────────────────────\n>\n────────────────────────────────\n? for shortcuts"
	var (
		mu         sync.Mutex
		screenText = "⚠ failed to construct executor: plan model not specified\nError ID: old-1\n" + chrome
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{"pane_id": "w9:p1", "source": "recent", "text": screenText}}, nil
		case "agent.prompt":
			txt, _ := params["text"].(string)
			screenText = strings.Join([]string{
				"Error ID: old-1",
				"> " + txt,
				"⚠ Warning: model gpt-6-luna deprecated, falling back to gpt-6-mini",
				"  └ using fallback model for this request",
				chrome,
			}, "\n")
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.workerStartupProbeDelay = 10 * time.Millisecond
	eng.workerStartupProbeInterval = 10 * time.Millisecond
	eng.workerStartupCheckWindow = 80 * time.Millisecond

	ctx := context.Background()
	wk := &model.Worker{Name: "healthy-slow", Kind: "agy", PaneID: "w9:p1", Thread: model.MainThread, Status: "idle", CreatedAt: time.Now().Add(-time.Minute)}
	_ = st.UpsertWorker(ctx, wk)

	_, _, err = eng.SendToWorkerWithWork(ctx, wk.Name, model.MainThread, "执行分析任务", "")
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]liveInfo{wk.Name: {pane: "w9:p1", kind: "agy", status: "idle", ready: true}}
	scanned := map[string]bool{"": true}
	eng.deliverQueued(ctx, live, scanned)

	// Probe before timeout: old error & benign warning must NOT trigger fault.
	eng.syncWorkers(ctx, live, scanned, time.Now().Add(30*time.Millisecond))
	gotWk, _ := st.GetWorker(ctx, wk.Name)
	if gotWk.FaultError != "" || gotWk.Status == "failed" {
		t.Fatalf("expected zero false positive fault on old scrollback + benign warning, got %+v", gotWk)
	}

	// Probe after checkWindow: emits one unconfirmed-startup diagnostic WITHOUT faulting the worker.
	eng.syncWorkers(ctx, live, scanned, time.Now().Add(150*time.Millisecond))
	gotWk, _ = st.GetWorker(ctx, wk.Name)
	if gotWk.FaultError != "" {
		t.Fatalf("expected timeout notice NOT to mark FaultError, got %+v", gotWk)
	}
	queued, _ := st.ListQueuedTriggers(ctx)
	var unconfirmed int
	for _, q := range queued {
		if q.Source == "worker" && q.From == wk.Name && strings.Contains(q.Text, "has not confirmed starting") {
			unconfirmed++
		}
	}
	if unconfirmed != 1 {
		t.Fatalf("expected 1 unconfirmed-startup notice, got %d", unconfirmed)
	}
}

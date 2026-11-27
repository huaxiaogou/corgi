package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
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

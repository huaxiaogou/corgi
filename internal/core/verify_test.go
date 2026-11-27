package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestSingleSourceLedgerPreambleAndUpFrontBudget(t *testing.T) {
	dir := t.TempDir()
	mm := NewMemoryManager(dir)
	if err := mm.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Seed a stale legacy file under memory/tasks/private/.
	stale := filepath.Join(mm.MemDir(), "tasks", "private", "closed-legacy-task.md")
	if err := os.WriteFile(stale, []byte(strings.Repeat("stale legacy notes\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}

	preamble := mm.SessionStartPreamble()
	if strings.Contains(preamble, "closed-legacy-task.md") || strings.Contains(preamble, "open task notes") {
		t.Fatalf("SessionStartPreamble must not enumerate legacy tasks/private files as open tasks: %s", preamble)
	}
	if !strings.Contains(preamble, "sole authoritative open-task ledger") {
		t.Fatalf("SessionStartPreamble missing sole ledger note: %s", preamble)
	}
	if sz := mm.UpFrontSize(); sz > 2048 {
		t.Fatalf("UpFrontSize counted legacy tasks/private file: got %d bytes", sz)
	}
}

func TestSnapshotSlimsEndedWorkersPreservingActiveAndStore(t *testing.T) {
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

	longTask := strings.Repeat("任务详情", 200)   // 800 runes
	longReport := strings.Repeat("汇报正文", 120) // 480 runes
	now := time.Now()
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "w-active", Thread: model.MainThread, Kind: "claude", Cwd: dir,
		Status: "working", Task: longTask, LastReport: longReport, CreatedAt: now, Ended: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "w-ended", Thread: model.MainThread, Kind: "claude", Cwd: dir,
		Status: "stopped", Task: longTask, LastReport: longReport, CreatedAt: now.Add(-time.Minute), Ended: true,
	}); err != nil {
		t.Fatal(err)
	}

	snap := eng.Snapshot(ctx, model.MainThread)
	var gotActive, gotEnded *model.Worker
	for _, w := range snap.Workers {
		switch w.Name {
		case "w-active":
			gotActive = w
		case "w-ended":
			gotEnded = w
		}
	}
	if gotActive == nil || gotEnded == nil {
		t.Fatalf("missing workers in snapshot: %+v", snap.Workers)
	}
	if gotActive.Task != longTask || gotActive.LastReport != longReport {
		t.Fatalf("active worker must remain lossless in Snapshot")
	}
	if len([]rune(gotEnded.Task)) > 405 || len([]rune(gotEnded.LastReport)) > 245 {
		t.Fatalf("ended worker not slimmed: taskRunes=%d reportRunes=%d", len([]rune(gotEnded.Task)), len([]rune(gotEnded.LastReport)))
	}
	// Store record itself must stay 100% lossless.
	stored, err := st.GetWorker(ctx, "w-ended")
	if err != nil || stored.Task != longTask || stored.LastReport != longReport {
		t.Fatalf("store record was mutated or missing: %+v err=%v", stored, err)
	}
}

func TestVerifyWorkerBounceOnFailure(t *testing.T) {
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

	workDir := t.TempDir()
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "w-verify", Thread: model.MainThread, Kind: "claude", Cwd: workDir,
		Status: "idle", Task: "implement feature", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Passing custom check.
	passOut, err := eng.VerifyWorker(ctx, VerifyWorkerRequest{
		Worker: "w-verify",
		Cmd:    "echo all-good",
		Send:   true,
	})
	if err != nil {
		t.Fatalf("VerifyWorker pass: %v", err)
	}
	if !passOut.OK || passOut.SentToWorker {
		t.Fatalf("expected OK=true & SentToWorker=false, got %+v", passOut)
	}

	// Failing custom check with --send should bounce prompt straight back to w-verify.
	failOut, err := eng.VerifyWorker(ctx, VerifyWorkerRequest{
		Worker: "w-verify",
		Cmd:    "echo 'boom failure' && exit 1",
		Send:   true,
	})
	if err != nil {
		t.Fatalf("VerifyWorker fail+send: %v", err)
	}
	if failOut.OK || !failOut.SentToWorker || failOut.TriggerID == "" {
		t.Fatalf("expected OK=false & SentToWorker=true with TriggerID, got %+v", failOut)
	}
	trigs, _ := st.ListQueuedTriggers(ctx)
	found := false
	for _, tr := range trigs {
		if tr.ID == failOut.TriggerID && strings.Contains(tr.Text, "boom failure") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected bounced failure trigger queued for worker, triggers=%+v", trigs)
	}
}

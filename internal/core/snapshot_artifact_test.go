package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestMultipleStandaloneWorkerReportSnapshotsSucceed(t *testing.T) {
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
	w1 := &model.Worker{Name: "worker-alpha", Thread: model.MainThread, Machine: "mac-1"}
	w2 := &model.Worker{Name: "worker-beta", Thread: model.MainThread, Machine: "hub"}
	if err := st.UpsertWorker(ctx, w1); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorker(ctx, w2); err != nil {
		t.Fatal(err)
	}

	// Capture multiple distinct standalone report snapshots across workers — must never collide on UNIQUE(entry_id, ordinal).
	rep1, err := eng.Report(ctx, w1.Name, "", strings.Repeat("第一份完整分析报告内容。", 60))
	if err != nil || rep1 == nil || rep1.ArtifactID == "" {
		t.Fatalf("first report failed: trig=%+v err=%v", rep1, err)
	}
	rep2, err := eng.Report(ctx, w1.Name, "", strings.Repeat("第二份更新后的完整报告。", 60))
	if err != nil || rep2 == nil || rep2.ArtifactID == "" {
		t.Fatalf("second report failed: trig=%+v err=%v", rep2, err)
	}
	rep3, err := eng.Report(ctx, w2.Name, "", strings.Repeat("另一个任务的长调研报告。", 60))
	if err != nil || rep3 == nil || rep3.ArtifactID == "" {
		t.Fatalf("third report failed: trig=%+v err=%v", rep3, err)
	}
	if rep1.ArtifactID == rep2.ArtifactID || rep2.ArtifactID == rep3.ArtifactID {
		t.Fatalf("expected 3 distinct artifact IDs, got %q %q %q", rep1.ArtifactID, rep2.ArtifactID, rep3.ArtifactID)
	}

	// Plain confirmation with ONLY `--worker` (no `--re <report_tID>`) MUST attach 0 artifacts!
	confirmRes, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "收到，我已让 worker-alpha 继续处理下一轮。",
		Worker: w1.Name,
	})
	if err != nil {
		t.Fatalf("confirmation ReplyDetailed failed: %v", err)
	}
	if len(confirmRes.Entry.Artifacts) != 0 {
		t.Fatalf("plain --worker confirmation must NOT attach stale report snapshots, got %+v", confirmRes.Entry.Artifacts)
	}

	// Explicit `--re <rep1.ID>` attaches EXACTLY rep1's snapshot (not newer rep2!) and relinks EntryID cleanly.
	res, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "第一份报告已整理，请审阅。",
		Worker: w1.Name,
		ReID:   rep1.ID,
	})
	if err != nil {
		t.Fatalf("ReplyDetailed with --re failed: %v", err)
	}
	if len(res.Entry.Artifacts) != 1 || res.Entry.Artifacts[0].ID != rep1.ArtifactID {
		t.Fatalf("expected exact snapshot %s for --re %s, got %+v", rep1.ArtifactID, rep1.ID, res.Entry.Artifacts)
	}
	art, err := st.GetArtifact(ctx, rep1.ArtifactID)
	if err != nil {
		t.Fatalf("GetArtifact(%s) failed: %v", rep1.ArtifactID, err)
	}
	if art.EntryID != res.Entry.ID {
		t.Fatalf("expected standalone artifact relinked to reply entry %q, got %q", res.Entry.ID, art.EntryID)
	}
}

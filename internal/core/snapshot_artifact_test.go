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
	id1 := eng.captureWorkerReportSnapshot(ctx, w1, strings.Repeat("第一份完整分析报告内容。", 30), 100)
	id2 := eng.captureWorkerReportSnapshot(ctx, w1, strings.Repeat("第二份更新后的完整报告。", 30), 100)
	id3 := eng.captureWorkerReportSnapshot(ctx, w2, strings.Repeat("另一个任务的长调研报告。", 30), 100)
	if id1 == "" || id2 == "" || id3 == "" || id1 == id2 || id2 == id3 {
		t.Fatalf("expected 3 distinct standalone artifact IDs, got id1=%q id2=%q id3=%q", id1, id2, id3)
	}

	// Auto-attaching the latest standalone snapshot to a Router reply should relink its EntryID to the reply entry.
	res, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "worker-alpha 报告已完成。",
		Worker: w1.Name,
	})
	if err != nil {
		t.Fatalf("ReplyDetailed failed: %v", err)
	}
	art, err := st.GetArtifact(ctx, id2)
	if err != nil {
		t.Fatalf("GetArtifact(%s) failed: %v", id2, err)
	}
	if art.EntryID != res.Entry.ID {
		t.Fatalf("expected standalone artifact relinked to reply entry %q, got %q", res.Entry.ID, art.EntryID)
	}
}

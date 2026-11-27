package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "corgi.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenAndReopenMigrationIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "corgi.db")
	st1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	ctx := context.Background()
	id, seq, err := st1.NextID(ctx, "e")
	if err != nil || id != "e1" || seq != 1 {
		t.Fatalf("unexpected NextID: %q %d %v", id, seq, err)
	}
	_ = st1.Close()

	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	id2, seq2, err := st2.NextID(ctx, "e")
	if err != nil || id2 != "e2" || seq2 != 2 {
		t.Fatalf("sequence did not persist: %q %d %v", id2, seq2, err)
	}
}

func TestEntriesPaginationPinAndSearch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	samples := []struct {
		role string
		text string
	}{
		{"user", "帮我排查订单慢查询问题"},
		{"corgi", "已派 api-profiler 去查 orders 联合索引"},
		{"event", "worker api-profiler started"},
		{"user", "顺便补齐 order_service 单元测试"},
		{"corgi", "订单表缺少联合索引导致 P99 飙到 2.4s"},
	}
	var ids []string
	for _, sm := range samples {
		id, seq, err := st.NextID(ctx, "e")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		err = st.InsertEntry(ctx, &model.Entry{
			ID: id, Seq: seq, Role: sm.role, Thread: model.MainThread,
			Channel: "web", Text: sm.text, At: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	got, more, err := st.ListEntries(ctx, model.MainThread, 3)
	if err != nil || !more || len(got) != 3 {
		t.Fatalf("ListEntries got len=%d more=%v err=%v", len(got), more, err)
	}
	if got[0].ID != ids[2] || got[2].ID != ids[4] {
		t.Fatalf("unexpected order: %s .. %s", got[0].ID, got[2].ID)
	}

	earlier, more2, err := st.ListEntriesBefore(ctx, model.MainThread, got[0].Seq, 5)
	if err != nil || more2 || len(earlier) != 2 {
		t.Fatalf("ListEntriesBefore got len=%d more=%v err=%v", len(earlier), more2, err)
	}

	if err := st.SetEntryPinned(ctx, ids[4], true); err != nil {
		t.Fatal(err)
	}
	pinned, err := st.ListPinnedEntries(ctx, 10)
	if err != nil || len(pinned) != 1 || pinned[0].ID != ids[4] {
		t.Fatalf("ListPinnedEntries unexpected: %+v %v", pinned, err)
	}

	res, err := st.SearchEntries(ctx, model.MainThread, "订单 索引", 10)
	if err != nil || res.Total != 1 || res.Results[0].ID != ids[4] {
		t.Fatalf("SearchEntries unexpected: %+v %v", res, err)
	}
}

func TestTriggerIdempotency(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	id1, seq1, _ := st.NextID(ctx, "t")
	tr1 := &model.Trigger{
		ID: id1, Seq: seq1, Source: "wecom", Thread: model.MainThread,
		From: "user1", Text: "hello", At: time.Now(), Policy: "respond",
		Status: "queued", IdemKey: "wecom:msg-100",
	}
	if err := st.InsertTrigger(ctx, tr1); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	id2, seq2, _ := st.NextID(ctx, "t")
	tr2 := *tr1
	tr2.ID, tr2.Seq = id2, seq2
	if err := st.InsertTrigger(ctx, &tr2); !errors.Is(err, ErrDuplicateTrigger) {
		t.Fatalf("expected ErrDuplicateTrigger, got %v", err)
	}
}

func TestResolveActionAtomicNoDoubleSubmit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	id, _, _ := st.NextID(ctx, "a")
	act := &model.Action{
		ID: id, Kind: model.ActionKindRouterChoice, Thread: model.MainThread,
		Title: "建立联合索引？", Status: model.ActionPending, CreatedAt: time.Now(),
		Choices: []model.ActionChoice{{Key: "1", Label: "生成迁移脚本"}},
	}
	if err := st.InsertAction(ctx, act); err != nil {
		t.Fatal(err)
	}

	// Simulate simultaneous resolution from Web and WeCom.
	const callers = 8
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int32
		dups      atomic.Int32
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		src := "web"
		if i%2 == 1 {
			src = "wecom"
		}
		go func(by string) {
			defer wg.Done()
			_, err := st.ResolveAction(ctx, id, by, "1", "生成迁移脚本")
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, ErrActionAlreadyResolved):
				dups.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(src)
	}
	wg.Wait()

	if succeeded.Load() != 1 || dups.Load() != callers-1 {
		t.Fatalf("want 1 success and %d duplicates, got %d / %d", callers-1, succeeded.Load(), dups.Load())
	}
}

func TestDeviceLifecycle(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	d := &model.Device{
		ID: "macbook", Kind: "machine", Name: "MacBook Pro", OS: "darwin/arm64",
		TokenHash: "deadbeef1234", CreatedAt: time.Now(), LastSeen: time.Now(),
	}
	if err := st.UpsertDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	found, err := st.FindDeviceByHash(ctx, "deadbeef1234")
	if err != nil || found.ID != "macbook" {
		t.Fatalf("FindDeviceByHash failed: %+v %v", found, err)
	}
	if _, err := st.DeleteDevice(ctx, "macbook"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FindDeviceByHash(ctx, "deadbeef1234"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestListKVByPrefixEscapesWildcards(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	seed := map[string]string{
		"wecom_focus:u1":      `{"worker":"alpha"}`,
		"wecom_focus:grp1:u2": `{"worker":"beta"}`,
		"wecomXfocus:u3":      `{"worker":"wrong-underscore"}`,
		"wecom%focus:u4":      `{"worker":"percent-key"}`,
		"wecom123focus:u5":    `{"worker":"wrong-percent"}`,
		"other:key":           `val`,
	}
	for k, v := range seed {
		if err := st.SetKV(ctx, k, v); err != nil {
			t.Fatalf("SetKV(%q): %v", k, err)
		}
	}

	cases := []struct {
		name     string
		prefix   string
		wantKeys []string
	}{
		{
			name:     "exact underscore prefix excludes wildcard matches",
			prefix:   "wecom_focus:",
			wantKeys: []string{"wecom_focus:grp1:u2", "wecom_focus:u1"},
		},
		{
			name:     "percent character in prefix is escaped literally",
			prefix:   "wecom%focus:",
			wantKeys: []string{"wecom%focus:u4"},
		},
		{
			name:     "unmatched prefix returns empty map",
			prefix:   "missing_prefix:",
			wantKeys: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ListKVByPrefix(ctx, tc.prefix)
			if err != nil {
				t.Fatalf("ListKVByPrefix(%q) err: %v", tc.prefix, err)
			}
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("ListKVByPrefix(%q) = %+v, want keys %v", tc.prefix, got, tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if got[k] != seed[k] {
					t.Fatalf("ListKVByPrefix(%q)[%q] = %q, want %q", tc.prefix, k, got[k], seed[k])
				}
			}
		})
	}
}

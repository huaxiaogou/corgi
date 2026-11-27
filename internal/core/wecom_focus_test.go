package core

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
)

func TestWeComFocusTTLAndLifecycleTable(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()
	now := time.Now()

	// Seed one live worker and one ended worker.
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "live-w", Kind: "agy", Cwd: "/tmp", Thread: model.MainThread,
		Status: "idle", PaneID: "w1:p1", Title: "直聊联调", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "ended-w", Kind: "agy", Cwd: "/tmp", Thread: model.MainThread,
		Status: "done", Ended: true, Title: "已停任务", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		name          string
		key           string
		rawKV         string
		wantActive    bool
		wantWorker    string
		wantExpired   bool
		wantReason    string
		wantDeletedKV bool
	}{
		{
			name: "fresh focus within 60m TTL stays active",
			key:  "wecom_focus:u-fresh",
			rawKV: mustJSON(WeComFocus{
				Worker:    "live-w",
				Title:     "直聊联调",
				EnteredAt: now.Add(-20 * time.Minute),
				UpdatedAt: now.Add(-15 * time.Minute),
			}),
			wantActive: true,
			wantWorker: "live-w",
		},
		{
			name: "expired focus past 60m TTL auto-cleans",
			key:  "wecom_focus:u-ttl",
			rawKV: mustJSON(WeComFocus{
				Worker:    "live-w",
				Title:     "直聊联调",
				EnteredAt: now.Add(-90 * time.Minute),
				UpdatedAt: now.Add(-61 * time.Minute),
			}),
			wantExpired:   true,
			wantReason:    "ttl",
			wantDeletedKV: true,
		},
		{
			name:          "legacy record without UpdatedAt auto-expires cleanly",
			key:           "wecom_focus:u-legacy",
			rawKV:         `{"worker":"live-w","title":"旧焦点"}`,
			wantExpired:   true,
			wantReason:    "legacy",
			wantDeletedKV: true,
		},
		{
			name: "focus pointing to ended worker auto-cleans",
			key:  "wecom_focus:u-ended",
			rawKV: mustJSON(WeComFocus{
				Worker:    "ended-w",
				Title:     "已停任务",
				EnteredAt: now.Add(-5 * time.Minute),
				UpdatedAt: now.Add(-1 * time.Minute),
			}),
			wantExpired:   true,
			wantReason:    "ended",
			wantDeletedKV: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := eng.SetKV(ctx, tc.key, tc.rawKV); err != nil {
				t.Fatal(err)
			}
			got := eng.loadWeComFocusAt(ctx, tc.key, now, false)
			if tc.wantActive {
				if got.Focus == nil || got.Focus.Worker != tc.wantWorker || got.Expired {
					t.Fatalf("expected active focus %q, got %+v", tc.wantWorker, got)
				}
			} else {
				if got.Focus != nil || got.Expired != tc.wantExpired || got.ExpiredReason != tc.wantReason {
					t.Fatalf("expected expired=%v reason=%q, got %+v", tc.wantExpired, tc.wantReason, got)
				}
			}
			if tc.wantDeletedKV {
				if _, ok, _ := eng.GetKV(ctx, tc.key); ok {
					t.Fatalf("expected stale KV %q deleted lazily", tc.key)
				}
			}
		})
	}
}

func TestWeComFocusCleanupOnStopSyncGoneAndCloseWorkItem(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Minute)

	// 1. StopWorker clears only focuses pointing to that worker.
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "w-stop", Kind: "claude", Cwd: "/tmp", Thread: model.MainThread, Status: "idle", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "w-other", Kind: "claude", Cwd: "/tmp", Thread: model.MainThread, Status: "idle", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:u1", &WeComFocus{Worker: "w-stop", Title: "停机测试"})
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:u2", &WeComFocus{Worker: "w-other", Title: "保留焦点"})

	if err := eng.StopWorker(ctx, "w-stop"); err != nil {
		t.Fatalf("StopWorker failed: %v", err)
	}
	if got := eng.LoadWeComFocus(ctx, "wecom_focus:u1"); got.Focus != nil {
		t.Fatalf("expected focus for stopped worker cleared, got %+v", got)
	}
	if got := eng.LoadWeComFocus(ctx, "wecom_focus:u2"); got.Focus == nil || got.Focus.Worker != "w-other" {
		t.Fatalf("expected unrelated focus preserved, got %+v", got)
	}

	// 2. syncWorkers pane-gone clears focus pointing to exited worker.
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "w-gone", Kind: "claude", Cwd: "/tmp", Thread: model.MainThread,
		Status: "idle", PaneID: "w3:p1", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:u-gone", &WeComFocus{Worker: "w-gone", Title: "后台退出"})
	eng.syncWorkers(ctx, map[string]liveInfo{}, map[string]bool{"": true}, time.Now())
	if _, ok, _ := eng.GetKV(ctx, "wecom_focus:u-gone"); ok {
		t.Fatal("expected focus cleared when syncWorkers marks worker gone")
	}

	// 3. CloseWorkItem clears focuses associated with the closed matter's workers.
	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "w-wi", Kind: "agy", Cwd: "/tmp", Thread: model.MainThread,
		Status: "idle", PaneID: "w4:p1", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "结项清理测试", "待合并", "w-wi", "")
	if err != nil {
		t.Fatalf("SetWorkItem failed: %v", err)
	}
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:u-wi", &WeComFocus{Worker: "w-wi", WorkItemID: wi.ID, Title: wi.Title})
	if _, err := eng.CloseWorkItem(ctx, wi.ID, "已完成"); err != nil {
		t.Fatalf("CloseWorkItem failed: %v", err)
	}
	if _, ok, _ := eng.GetKV(ctx, "wecom_focus:u-wi"); ok {
		t.Fatal("expected focus cleared when CloseWorkItem closes its work item")
	}
}

func TestSnapshotWeComFocusExposureAndFiltering(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()
	now := time.Now()

	if err := eng.store.UpsertWorker(ctx, &model.Worker{
		Name: "active-w", Kind: "agy", Cwd: "/tmp", Thread: model.MainThread,
		Status: "idle", PaneID: "w2:p1", Title: "会话身份提醒", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Seed one expired single focus and one active single focus + one active group focus.
	expiredRaw, _ := json.Marshal(WeComFocus{
		Worker:    "active-w",
		Title:     "已过期",
		EnteredAt: now.Add(-2 * time.Hour),
		UpdatedAt: now.Add(-70 * time.Minute),
	})
	_ = eng.SetKV(ctx, "wecom_focus:u-expired", string(expiredRaw))
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:u-owner", &WeComFocus{
		Worker: "active-w", Title: "会话身份提醒", Kind: "agy",
	})
	_ = eng.SaveWeComFocus(ctx, "wecom_focus:grp-99:u-owner", &WeComFocus{
		Worker: "active-w", Title: "群里直聊", Kind: "agy",
	})

	snapMain := eng.Snapshot(ctx, model.MainThread)
	if snapMain.WeComFocus == nil || snapMain.WeComFocus.Key != "wecom_focus:u-owner" || snapMain.WeComFocus.Scope != "single" {
		t.Fatalf("expected active single WeComFocus in chat:main snapshot, got %+v", snapMain.WeComFocus)
	}
	if snapMain.WeComFocus.Title != "会话身份提醒" || snapMain.WeComFocus.Machine != "Hub" || !snapMain.WeComFocus.ExpiresAt.After(snapMain.WeComFocus.UpdatedAt) {
		t.Fatalf("unexpected single WeComFocus metadata: %+v", snapMain.WeComFocus)
	}
	// Expired KV should have been cleaned lazily during Snapshot.
	if _, ok, _ := eng.GetKV(ctx, "wecom_focus:u-expired"); ok {
		t.Fatal("expected expired focus lazily removed during Snapshot")
	}

	snapGrp := eng.Snapshot(ctx, "wecom:grp-99")
	if snapGrp.WeComFocus == nil || snapGrp.WeComFocus.Key != "wecom_focus:grp-99:u-owner" || snapGrp.WeComFocus.Scope != "group" {
		t.Fatalf("expected matching group WeComFocus in wecom:grp-99 snapshot, got %+v", snapGrp.WeComFocus)
	}

	// ClearAllWeComFocuses clears all active entries cleanly.
	removed, err := eng.ClearAllWeComFocuses(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("ClearAllWeComFocuses = (%d, %v), want (2, nil)", removed, err)
	}
	if snapAfter := eng.Snapshot(ctx, model.MainThread); snapAfter.WeComFocus != nil {
		t.Fatalf("expected nil WeComFocus after clear, got %+v", snapAfter.WeComFocus)
	}
}

func TestGroupSpawnDoesNotAutoSubscribeUnlessExplicit(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()
	cwd := t.TempDir()
	eng.SetFleet(fleet.NewFleet(nil, "hub", cwd, "test", []string{"claude"}))
	groupThread := "wecom:grp-no-autosub"

	// 1. Default group Spawn (Subscribe == nil) must NOT subscribe the worker to the group thread.
	_, _ = eng.Spawn(ctx, SpawnRequest{
		Kind:      "claude",
		Cwd:       cwd,
		Name:      "grp-default-w",
		Thread:    groupThread,
		Task:      "排查群里日志",
		Worktree:  "off",
		Ephemeral: true,
	})
	if th, err := eng.store.GetThread(ctx, groupThread); err == nil && slices.Contains(th.Subscribers, "grp-default-w") {
		t.Fatalf("expected default group spawn NOT to auto-subscribe, got subscribers=%v", th.Subscribers)
	}

	// 2. Explicit Subscribe=true DOES subscribe the worker to the group thread.
	yes := true
	_, _ = eng.Spawn(ctx, SpawnRequest{
		Kind:      "claude",
		Cwd:       cwd,
		Name:      "grp-optin-w",
		Thread:    groupThread,
		Task:      "显式订阅群聊",
		Subscribe: &yes,
		Worktree:  "off",
		Ephemeral: true,
	})
	th, err := eng.store.GetThread(ctx, groupThread)
	if err != nil || !slices.Contains(th.Subscribers, "grp-optin-w") {
		t.Fatalf("expected explicit Subscribe=true to subscribe worker, got th=%+v err=%v", th, err)
	}
}

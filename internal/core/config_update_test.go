package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

type fakeFleetUpdater struct {
	mu           sync.Mutex
	checkRes     UpdateCheckResult
	checkErr     error
	triggerRes   UpdateTriggerResult
	triggerErr   error
	triggerCalls atomic.Int32
	lastExpected string
}

func (f *fakeFleetUpdater) TryAutoUpdate(context.Context) {}

func (f *fakeFleetUpdater) CheckUpdate(context.Context) (UpdateCheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkRes, f.checkErr
}

func (f *fakeFleetUpdater) TriggerFleetUpdate(_ context.Context, expectedTarget string) (UpdateTriggerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerCalls.Add(1)
	f.lastExpected = expectedTarget
	if f.triggerErr != nil {
		return UpdateTriggerResult{}, f.triggerErr
	}
	return f.triggerRes, nil
}

func TestManageConfigCheckAndTriggerUpdateL2(t *testing.T) {
	ctx := context.Background()
	eng, st, _, _ := newTestAPIEngine(t, nil)

	fu := &fakeFleetUpdater{
		checkRes: UpdateCheckResult{
			CurrentVersion: "v0.1.78",
			LatestVersion:  "v0.1.79",
			HasUpdate:      true,
			Outdated: []UpdateMachineStatus{
				{ID: "hub", Name: "hub", Version: "v0.1.78", Local: true, Online: true, Outdated: true},
				{ID: "mac-1", Name: "Mac", Version: "v0.1.77", Online: true, Outdated: true},
			},
		},
		triggerRes: UpdateTriggerResult{
			Started:    true,
			Target:     "v0.1.79",
			PendingIDs: []string{"mac-1", "hub"},
			Phase:      "updating",
		},
	}
	eng.SetAutoUpdater(fu)

	// 1. Group thread rejects both check_update and trigger_update.
	grpTurn := &apiRouterTurn{thread: "wecom:grp", triggerID: "t-grp-upd"}
	if out, _ := eng.toolManageConfig(ctx, grpTurn, &model.Trigger{ID: "t-grp-upd", Thread: "wecom:grp"}, &turnConsentMeta{isHuman: true}, `{"action":"check_update"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected group check_update rejected, got %s", out)
	}

	// 2. Main thread non-human turn allows read-only check_update, rejects trigger_update.
	bgTrig := &model.Trigger{ID: "t-bg-upd", Thread: model.MainThread, Source: "system", Policy: "respond", From: "corgi", Ref: "idle-patrol"}
	bgMeta := eng.resolveTurnConsentMeta(ctx, bgTrig)
	bgTurn := &apiRouterTurn{thread: model.MainThread, triggerID: bgTrig.ID}
	chkOut, stop := eng.toolManageConfig(ctx, bgTurn, bgTrig, bgMeta, `{"action":"check_update"}`)
	if stop || !strings.Contains(chkOut, `"v0.1.79"`) || !strings.Contains(chkOut, `"has_update":true`) {
		t.Fatalf("expected read-only check_update ok, got stop=%v out=%s", stop, chkOut)
	}
	if fu.triggerCalls.Load() != 0 {
		t.Fatalf("check_update must be strictly read-only")
	}
	if out, _ := eng.toolManageConfig(ctx, bgTurn, bgTrig, bgMeta, `{"action":"trigger_update"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected non-human trigger_update rejected, got %s", out)
	}

	// 3. Up-to-date or busy returns status directly without issuing redundant confirmation card.
	turn, trig, meta := newHumanConfigTurn(t, eng, "一键升级")
	fu.mu.Lock()
	fu.checkRes.HasUpdate = false
	fu.mu.Unlock()
	if out, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"trigger_update"}`); stop || !strings.Contains(out, `"already_up_to_date":true`) {
		t.Fatalf("expected up-to-date response without card, got stop=%v out=%s", stop, out)
	}
	fu.mu.Lock()
	fu.checkRes.HasUpdate = true
	fu.checkRes.Busy = true
	fu.mu.Unlock()
	if out, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"trigger_update"}`); stop || !strings.Contains(out, `"busy":true`) {
		t.Fatalf("expected busy response without card, got stop=%v out=%s", stop, out)
	}

	// 4. Genuine update available: issues L2 confirm card with Cancel recommended & Strategy 2B earlyExit.
	fu.mu.Lock()
	fu.checkRes.Busy = false
	fu.mu.Unlock()
	cardOut, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"trigger_update"}`)
	if !stop || !strings.Contains(cardOut, `"pending_confirm":true`) || !strings.Contains(cardOut, `"trigger_update"`) {
		t.Fatalf("expected L2 trigger_update confirm card, got stop=%v out=%s", stop, cardOut)
	}
	var card struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(cardOut), &card)
	if fu.triggerCalls.Load() != 0 {
		t.Fatalf("upgrade must NOT trigger before human confirmation")
	}

	// 5. Concurrent confirmation clicks trigger upgrade EXACTLY ONCE with pinned expectedTarget.
	var (
		wg      sync.WaitGroup
		okCount atomic.Int32
		dupCnt  atomic.Int32
	)
	for i := range 6 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ch := condStr(idx%2 == 0, "web", "wecom")
			_, err := eng.ResolveAction(ctx, card.Action, ch, "1")
			if err == nil {
				okCount.Add(1)
			} else if errors.Is(err, store.ErrActionAlreadyResolved) {
				dupCnt.Add(1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if okCount.Load() != 1 || dupCnt.Load() != 5 {
		t.Fatalf("expected 1 ok + 5 ErrActionAlreadyResolved, got ok=%d dup=%d", okCount.Load(), dupCnt.Load())
	}
	if fu.triggerCalls.Load() != 1 || fu.lastExpected != "v0.1.79" {
		t.Fatalf("expected exactly 1 TriggerFleetUpdate(v0.1.79), got calls=%d target=%q", fu.triggerCalls.Load(), fu.lastExpected)
	}

	// 6. Drift/error on trigger is reported cleanly in receipt without false success.
	fu.mu.Lock()
	fu.triggerErr = fmt.Errorf("最新版本已变更为 v0.1.80（确认目标为 v0.1.79）")
	fu.mu.Unlock()
	turn2, trig2, meta2 := newHumanConfigTurn(t, eng, "再次升级")
	cardOut2, _ := eng.toolManageConfig(ctx, turn2, trig2, meta2, `{"action":"trigger_update"}`)
	var card2 struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(cardOut2), &card2)
	if _, err := eng.ResolveAction(ctx, card2.Action, "wecom", "1"); err != nil {
		t.Fatalf("confirm on drift error returned unexpected err: %v", err)
	}
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 10)
	foundDrift := false
	for _, en := range entries {
		if strings.Contains(en.Text, "v0.1.80") && strings.Contains(en.Text, "未启动一键升级") {
			foundDrift = true
			break
		}
	}
	if !foundDrift {
		t.Fatalf("expected version-drift failure receipt in timeline")
	}
}

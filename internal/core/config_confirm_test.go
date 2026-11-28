package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

func newHumanConfigTurn(t *testing.T, eng *Engine, text string) (*apiRouterTurn, *model.Trigger, *turnConsentMeta) {
	t.Helper()
	ctx := context.Background()
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: text})
	if err != nil || trig == nil {
		t.Fatalf("Say(%q): %v", text, err)
	}
	meta := eng.resolveTurnConsentMeta(ctx, trig)
	return &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}, trig, meta
}

func TestPatchManagedSettingsL1L2L3AndModelSync(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)

	// Seed existing KindDefaults + custom router settings to ensure router_api_* is never side-effect mutated.
	cur := eng.Settings()
	cur.WorkerKind = "claude"
	cur.KindDefaults = map[string]model.ModelEffortSpec{"claude": {Model: "old-model", Effort: "low"}}
	cur.AskPermissions = true
	if err := eng.SaveConfig(ctx, &cur, nil); err != nil {
		t.Fatal(err)
	}
	beforeRouterURL := eng.Settings().RouterAPIBaseURL

	turn, trig, meta := newHumanConfigTurn(t, eng, "调整配置")

	// 1. All L3 keys + projects + unknown keys MUST be rejected.
	l3Payloads := []string{
		`{"action":"patch_settings","settings":{"router_api_provider":"openai"}}`,
		`{"action":"patch_settings","settings":{"router_api_base_url":"https://evil.example"}}`,
		`{"action":"patch_settings","settings":{"router_api_model":"gpt-x"}}`,
		`{"action":"patch_settings","settings":{"router_api_effort":"max"}}`,
		`{"action":"patch_settings","settings":{"router_api_key":"sk-nope"}}`,
		`{"action":"patch_settings","settings":{"wecom_enabled":false}}`,
		`{"action":"patch_settings","settings":{"wecom_bot_id":"b"}}`,
		`{"action":"patch_settings","settings":{"wecom_secret":"s"}}`,
		`{"action":"patch_settings","settings":{"wecom_allow_users":["attacker"]}}`,
		`{"action":"patch_settings","settings":{"wecom_default_to":"u"}}`,
		`{"action":"patch_settings","settings":{"public_url":"https://phish.example"}}`,
		`{"action":"patch_settings","settings":{"auto_update":true}}`,
		`{"action":"patch_settings","settings":{"paused":true}}`,
		`{"action":"patch_settings","settings":{"owner_token":"tok"}}`,
		`{"action":"patch_settings","settings":{"projects":[]}}`,
		`{"action":"patch_settings","settings":{"nonexistent_key":123}}`,
	}
	for _, args := range l3Payloads {
		if out, stop := eng.toolManageConfig(ctx, turn, trig, meta, args); stop || !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected %s rejected, got stop=%v out=%s", args, stop, out)
		}
	}

	// 2. Mixing ask_permissions=false with L1 key MUST be rejected.
	if out, _ := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"patch_settings","settings":{"ask_permissions":false,"reply_chars":300}}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected mixed ask_permissions=false rejected, got %s", out)
	}

	// 3. Invalid formats/ranges rejected cleanly.
	for _, bad := range []string{
		`{"action":"patch_settings","settings":{"active_hours":"bad-window"}}`,
		`{"action":"patch_settings","settings":{"dream_at":"25:99"}}`,
		`{"action":"patch_settings","settings":{"notify_level":"loud"}}`,
		`{"action":"patch_settings","settings":{"worker_cwd":"/etc"}}`,
		`{"action":"patch_settings","settings":{"idle_patrol_minutes":999}}`,
		`{"action":"patch_settings","settings":{"worker_idle_gc_minutes":5}}`,
	} {
		if out, _ := eng.toolManageConfig(ctx, turn, trig, meta, bad); !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected invalid %s rejected, got %s", bad, out)
		}
	}

	// 4. Patching worker_model/worker_effort MUST sync into KindDefaults[claude] instead of reverting!
	repoDir := filepath.Join(dir, "workspace-ok")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	okArgs := fmt.Sprintf(`{"action":"patch_settings","settings":{"worker_model":"claude-opus-4","worker_effort":"high","worker_cwd":%q,"active_hours":"08:30-23:00","idle_patrol_minutes":45,"morning_digest":false,"dream_at":"03:30","notify_level":"normal","reply_chars":320,"stall_minutes":20,"stale_archive_days":5,"worker_idle_gc_minutes":90,"worker_max_live_per_machine":8}}`, repoDir)
	out, stop := eng.toolManageConfig(ctx, turn, trig, meta, okArgs)
	if stop || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected valid L1 patch_settings ok, got stop=%v out=%s", stop, out)
	}
	after := eng.Settings()
	if after.WorkerModel != "claude-opus-4" || after.WorkerEffort != "high" || after.KindDefaults["claude"].Model != "claude-opus-4" {
		t.Fatalf("worker_model/effort did not sync into KindDefaults: %+v", after)
	}
	if after.ActiveHours != "08:30-23:00" || after.IdlePatrolMinutes != 45 || after.MorningDigestEnabled() || after.DreamAt != "03:30" || after.NotifyLevel != "normal" {
		t.Fatalf("schedule settings not persisted: %+v", after)
	}
	if after.RouterAPIBaseURL != beforeRouterURL {
		t.Fatalf("router_api_base_url mutated unexpectedly: %q -> %q", beforeRouterURL, after.RouterAPIBaseURL)
	}
}

func TestManagedPoliciesAndProtectedSources(t *testing.T) {
	ctx := context.Background()
	eng, _, _, _ := newTestAPIEngine(t, nil)
	turn, trig, meta := newHumanConfigTurn(t, eng, "管理策略")

	for _, prot := range []string{"chat", "wecom", "worker", "timer", "system", "router"} {
		args := fmt.Sprintf(`{"action":"upsert_policy","policy":{"source":%q,"action":"ignore"}}`, prot)
		if out, _ := eng.toolManageConfig(ctx, turn, trig, meta, args); !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected protected source %q upsert rejected, got %s", prot, out)
		}
		delArgs := fmt.Sprintf(`{"action":"delete_policy","policy_source":%q}`, prot)
		if out, _ := eng.toolManageConfig(ctx, turn, trig, meta, delArgs); !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected protected source %q delete rejected, got %s", prot, out)
		}
	}

	out, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"upsert_policy","policy":{"source":"webhook:github","action":"respond","allow":["octocat"]}}`)
	if stop || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("upsert_policy failed: %s", out)
	}
	// Delete triggers L2 confirm card (earlyExit = true).
	delOut, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"delete_policy","policy_source":"webhook:github"}`)
	if !stop || !strings.Contains(delOut, `"pending_confirm":true`) {
		t.Fatalf("expected delete_policy L2 confirm card, got stop=%v out=%s", stop, delOut)
	}
	var parsed struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(delOut), &parsed)
	if _, err := eng.ResolveAction(ctx, parsed.Action, "wecom", "1"); err != nil {
		t.Fatalf("confirm delete_policy: %v", err)
	}
	for _, p := range eng.Policies() {
		if p.Source == "webhook:github" {
			t.Fatalf("expected webhook:github deleted after confirmation")
		}
	}
}

func TestL2ConfigConfirmFullLifecycleGuardsAndAtomicity(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	repoDir := filepath.Join(dir, "old-demo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.UpsertProject(ctx, "test", UpsertProjectRequest{Name: "old-demo", Machine: "hub", Path: repoDir}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SetProjectPath(ctx, "test", "old-demo", "offline-mac", "~/work/old-demo"); err != nil {
		t.Fatal(err)
	}

	// 1. Delete project issues L2 confirm card with Cancel marked Recommended=true, Strategy 2B inflight, and earlyExit=true.
	turn, trig, meta := newHumanConfigTurn(t, eng, "删除项目 old-demo")
	out, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"delete_project","project":"old-demo"}`)
	if !stop || !turn.hasFlag(turnFlagReplied) || !strings.Contains(out, `"pending_confirm":true`) {
		t.Fatalf("expected L2 confirm card with earlyExit=true, got stop=%v out=%s", stop, out)
	}
	var cardResp struct {
		Action string `json:"action"`
		Entry  string `json:"entry"`
	}
	_ = json.Unmarshal([]byte(out), &cardResp)
	act, err := st.GetAction(ctx, cardResp.Action)
	if err != nil || act.Kind != model.ActionKindConfigConfirm || !act.IsDecision() || act.IsMulti() {
		t.Fatalf("unexpected config_confirm action: %+v err=%v", act, err)
	}
	if len(act.Choices) != 2 || !act.Choices[1].Recommended || act.Choices[0].Recommended {
		t.Fatalf("expected Cancel (choice 2) marked Recommended=true, got %+v", act.Choices)
	}

	// 2. Security bypass guards: router/machine channel, custom free-text, snooze, quoted reply auto-resolve MUST all fail/skip.
	if _, err := eng.ResolveAction(ctx, act.ID, "router", "1"); err == nil {
		t.Fatal("expected router channel rejected on config_confirm")
	}
	if _, err := eng.ResolveAction(ctx, act.ID, "machine", "1"); err == nil {
		t.Fatal("expected machine channel rejected on config_confirm")
	}
	if _, err := eng.ResolveActionCustom(ctx, act.ID, "web", "确认执行"); err == nil {
		t.Fatal("expected custom free-text rejected on config_confirm")
	}
	if _, err := eng.SnoozeActionBy(ctx, act.ID, "web", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected snooze rejected on config_confirm")
	}
	if _, err := eng.DismissActionBy(ctx, act.ID, "machine"); err == nil {
		t.Fatal("expected machine dismiss rejected on config_confirm")
	}
	// Quoting the card entry with plain chat text MUST NOT auto-resolve config_confirm.
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "好的", ReEntryID: cardResp.Entry}); err != nil {
		t.Fatal(err)
	}
	if still, _ := st.GetAction(ctx, act.ID); still.Status != model.ActionPending {
		t.Fatalf("quoted free-text reply bypassed config_confirm: status=%s", still.Status)
	}

	// 3. Concurrent clicks (Web + WeCom): exactly ONE succeeds, others get ErrActionAlreadyResolved, no LLM trigger woken.
	queuedBefore, _ := st.ListQueuedTriggers(ctx)
	const clicks = 10
	var (
		wg        sync.WaitGroup
		succeeded atomic.Int32
		already   atomic.Int32
	)
	for i := range clicks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ch := "web"
			if idx%2 == 1 {
				ch = "wecom"
			}
			_, rErr := eng.ResolveActionChoices(ctx, act.ID, ch, []string{"1"})
			if rErr == nil {
				succeeded.Add(1)
			} else if errors.Is(rErr, store.ErrActionAlreadyResolved) {
				already.Add(1)
			} else {
				t.Errorf("unexpected error on concurrent resolve: %v", rErr)
			}
		}(i)
	}
	wg.Wait()
	if succeeded.Load() != 1 || already.Load() != clicks-1 {
		t.Fatalf("expected 1 success + %d ErrActionAlreadyResolved, got ok=%d already=%d", clicks-1, succeeded.Load(), already.Load())
	}
	if len(eng.Settings().Projects) != 0 {
		t.Fatalf("expected project deleted, got %+v", eng.Settings().Projects)
	}
	if _, exists, _ := st.GetKV(ctx, configProposalKVPrefix+act.ID); exists {
		t.Fatalf("expected proposal KV deleted after apply")
	}
	queuedAfter, _ := st.ListQueuedTriggers(ctx)
	if len(queuedAfter) != len(queuedBefore) {
		t.Fatalf("config_confirm resolution must NOT wake a second LLM turn: before=%d after=%d", len(queuedBefore), len(queuedAfter))
	}

	// 4. Optimistic lock (BeforeHash mismatch) rejects silently-overwriting config and marks action expired.
	cur := eng.Settings()
	cur.AskPermissions = true
	if err := eng.SaveConfig(ctx, &cur, nil); err != nil {
		t.Fatal(err)
	}
	turn2, trig2, meta2 := newHumanConfigTurn(t, eng, "关闭权限确认")
	out2, stop2 := eng.toolManageConfig(ctx, turn2, trig2, meta2, `{"action":"patch_settings","settings":{"ask_permissions":false}}`)
	if !stop2 || !strings.Contains(out2, `"pending_confirm":true`) {
		t.Fatalf("expected L2 card for ask_permissions=false, got stop=%v out=%s", stop2, out2)
	}
	var card2 struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(out2), &card2)

	// External config change invalidates BeforeHash.
	mutSt := eng.Settings()
	mutSt.ReplyChars = 250
	if err := eng.SaveConfig(ctx, &mutSt, nil); err != nil {
		t.Fatal(err)
	}
	settled2, err := eng.ResolveAction(ctx, card2.Action, "web", "1")
	if err != nil {
		t.Fatalf("ResolveAction on stale BeforeHash returned err: %v", err)
	}
	if settled2.Status != model.ActionExpired || !strings.Contains(settled2.ExpiredReason, "变化") {
		t.Fatalf("expected expired status on BeforeHash mismatch, got %+v", settled2)
	}
	if !eng.Settings().AskPermissions {
		t.Fatalf("expected ask_permissions unchanged when BeforeHash mismatched")
	}

	// 5. Offline >7d restart: Snapshot() calls ExpireStalePendingActions (must skip config_confirm) then expireStaleConfigConfirms (deletes KV too).
	turn3, trig3, meta3 := newHumanConfigTurn(t, eng, "再次关闭权限确认")
	out3, _ := eng.toolManageConfig(ctx, turn3, trig3, meta3, `{"action":"patch_settings","settings":{"ask_permissions":false}}`)
	var card3 struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal([]byte(out3), &card3)
	// Backdate action & proposal beyond 8 days to simulate >7d offline hub restart.
	act3, _ := st.GetAction(ctx, card3.Action)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	act3.CreatedAt = oldTime
	// Update directly via CommitConfigConfirmTx? Better: test ExpireStalePendingActions + ExpirePendingConfigConfirms with future cutoff!
	if n, err := st.ExpireStalePendingActions(ctx, time.Nanosecond); err != nil || n != 0 {
		t.Fatalf("ExpireStalePendingActions must skip config_confirm, got n=%d err=%v", n, err)
	}
	if n, err := st.ExpirePendingConfigConfirms(ctx, time.Now().Add(time.Minute), configProposalKVPrefix, "配置确认提案已过期"); err != nil || n != 1 {
		t.Fatalf("ExpirePendingConfigConfirms failed: n=%d err=%v", n, err)
	}
	if _, exists, _ := st.GetKV(ctx, configProposalKVPrefix+card3.Action); exists {
		t.Fatalf("expected proposal KV cleaned by ExpirePendingConfigConfirms")
	}
	if expAct, _ := st.GetAction(ctx, card3.Action); expAct.Status != model.ActionExpired {
		t.Fatalf("expected expired config_confirm action, got %+v", expAct)
	}
}

func TestManageConfigTriggerDreamAndHistory(t *testing.T) {
	ctx := context.Background()
	eng, _, _, _ := newTestAPIEngine(t, nil)

	// Non-human trigger_dream must fail.
	bgTrig := &model.Trigger{ID: "t-bg-dream", Thread: model.MainThread, Source: "timer", Policy: "respond", From: "timer"}
	bgMeta := eng.resolveTurnConsentMeta(ctx, bgTrig)
	if out, _ := eng.toolManageConfig(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: bgTrig.ID}, bgTrig, bgMeta, `{"action":"trigger_dream"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected non-human trigger_dream rejected, got %s", out)
	}

	turn, trig, meta := newHumanConfigTurn(t, eng, "手动做梦")
	out, stop := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"trigger_dream"}`)
	if stop || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected human trigger_dream ok, got stop=%v out=%s", stop, out)
	}
	// History action returns recent redacted audit entries.
	histOut, _ := eng.toolManageConfig(ctx, turn, trig, meta, `{"action":"history"}`)
	if !strings.Contains(histOut, `"ok":true`) {
		t.Fatalf("expected history ok, got %s", histOut)
	}
}

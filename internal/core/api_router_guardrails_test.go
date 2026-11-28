package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestAPIRouterStrategy1ANonHumanGuards(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	// Attach a fake fleet so allowed dispatches actually reach Spawn and we can inspect WorktreeBranch.
	var (
		mu         sync.Mutex
		spawnCalls int
		promptSent int
	)
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.create":
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w1"},
				"root_pane": map[string]any{"pane_id": "w1:p1"},
			}, nil
		case "agent.start":
			spawnCalls++
			return map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "session": "s1", "pid": 1234}, nil
		case "agent.prompt":
			promptSent++
			return map[string]any{"ok": true}, nil
		case "agent.list":
			return map[string]any{"agents": []map[string]any{{
				"name": "claude-active", "pane_id": "w1:p9", "workspace_id": "w1",
				"agent": "claude", "agent_status": "idle", "interactive_ready": true,
			}}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	// Seed an active worker for steer_worker / resume checks.
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "claude-active", Kind: "claude", Thread: model.MainThread, PaneID: "w1:p9", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}

	// Create an Action resolved by "router" (non-human) and verify spoofed web Entry still yields isHuman=false.
	_, actRouter, _, err := eng.Reply(ctx, model.MainThread, "是否推进？", "", []string{"推进", "取消"}, false)
	if err != nil || actRouter == nil {
		t.Fatalf("create router action: act=%v err=%v", actRouter, err)
	}
	if _, err := eng.ResolveAction(ctx, actRouter.ID, "router", "1"); err != nil {
		t.Fatalf("resolve by router: %v", err)
	}
	spoofEntry := &model.Entry{ID: "e-spoof", Thread: model.MainThread, Role: "user", Channel: "web", ActionID: actRouter.ID, Text: "推进", At: time.Now()}
	if err := st.InsertEntry(ctx, spoofEntry); err != nil {
		t.Fatal(err)
	}
	spoofTrig := &model.Trigger{ID: "t-spoof", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "owner", Ref: spoofEntry.ID, Text: "推进"}

	// Create a human-resolved Action on "web" channel to contrast.
	_, actHuman, _, err := eng.Reply(ctx, model.MainThread, "确认方案？", "", []string{"执行实现", "暂缓"}, false)
	if err != nil || actHuman == nil {
		t.Fatalf("create human action: act=%v err=%v", actHuman, err)
	}
	if _, err := eng.ResolveAction(ctx, actHuman.ID, "web", "1"); err != nil {
		t.Fatalf("resolve by human web: %v", err)
	}
	humanActEntry := &model.Entry{ID: "e-human-act", Thread: model.MainThread, Role: "user", Channel: "web", ActionID: actHuman.ID, Text: "执行实现", At: time.Now()}
	if err := st.InsertEntry(ctx, humanActEntry); err != nil {
		t.Fatal(err)
	}
	humanActTrig := &model.Trigger{ID: "t-human-act", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "owner", Ref: humanActEntry.ID, Text: "执行实现"}

	nonHumanCases := []struct {
		name string
		trig *model.Trigger
	}{
		{"worker", &model.Trigger{ID: "t-nh-w", Thread: model.MainThread, Source: "worker", Policy: "respond", From: "claude-active", Text: "done"}},
		{"timer", &model.Trigger{ID: "t-nh-tm", Thread: model.MainThread, Source: "timer", Policy: "respond", From: "timer", Text: "check"}},
		{"system", &model.Trigger{ID: "t-nh-sys", Thread: model.MainThread, Source: "system", Policy: "respond", From: "corgi", Ref: "idle-patrol", Text: "patrol"}},
		{"webhook_candidate", &model.Trigger{ID: "t-nh-wh", Thread: model.MainThread, Source: "webhook:ci", Policy: "candidate", From: "ci", Text: "failed"}},
		{"machine_chat", &model.Trigger{ID: "t-nh-m", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "machine", Text: "run"}},
		{"spoofed_router_action", spoofTrig},
	}

	for _, tc := range nonHumanCases {
		t.Run(tc.name, func(t *testing.T) {
			meta := eng.resolveTurnConsentMeta(ctx, tc.trig)
			if meta.isHuman {
				t.Fatalf("expected isHuman=false for %s", tc.name)
			}
			turn := &apiRouterTurn{thread: model.MainThread, triggerID: tc.trig.ID}
			for _, badArgs := range []string{
				`{"mode":"implement","goal":"改代码"}`,
				`{"mode":"prototype","goal":"画原型"}`,
				`{"mode":"readonly","allow_release":true,"goal":"发布"}`,
				`{"mode":"readonly","resume":"claude-active","goal":"续聊"}`,
			} {
				if out := eng.toolDispatchWork(ctx, turn, tc.trig, meta, badArgs); !strings.Contains(out, `"ok":false`) {
					t.Fatalf("%s: expected dispatch_work(%s) rejected, got %s", tc.name, badArgs, out)
				}
			}
			if out := eng.toolSteerWorker(ctx, turn, tc.trig, meta, `{"worker":"claude-active","message":"继续"}`); !strings.Contains(out, `"ok":false`) {
				t.Fatalf("%s: expected steer_worker rejected, got %s", tc.name, out)
			}
		})
	}

	// Non-human readonly dispatch MUST succeed and force worktree="off" even if model requested worktree="on".
	nhTrig := nonHumanCases[0].trig
	nhMeta := eng.resolveTurnConsentMeta(ctx, nhTrig)
	outRO := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: nhTrig.ID}, nhTrig, nhMeta, `{"mode":"readonly","worktree":"on","goal":"只读排查日志"}`)
	if !strings.Contains(outRO, `"ok":true`) || !strings.Contains(outRO, `"branch":""`) {
		t.Fatalf("expected non-human readonly dispatch to succeed with forced worktree=off, got %s", outRO)
	}

	// Human Action-resolved turn yields isHuman=true, approvedAction!=nil, and allows steer_worker.
	hMeta := eng.resolveTurnConsentMeta(ctx, humanActTrig)
	if !hMeta.isHuman || hMeta.approvedAction == nil || hMeta.approvedAction.ID != actHuman.ID {
		t.Fatalf("expected human action turn isHuman=true & approvedAction=%s, got %+v", actHuman.ID, hMeta)
	}
	steerOut := eng.toolSteerWorker(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: humanActTrig.ID}, humanActTrig, hMeta, `{"worker":"claude-active","message":"按方案执行"}`)
	if !strings.Contains(steerOut, `"ok":true`) {
		t.Fatalf("expected human steer_worker to succeed, got %s", steerOut)
	}
}

func TestAPIRouterProjectCwdReplyAttachmentAndTimerTarget(t *testing.T) {
	ctx := context.Background()
	eng, st, _, _ := newTestAPIEngine(t, nil)

	// 1. Explicit project missing path for target machine fails clearly instead of falling back to home.
	cfg := eng.Settings()
	cfg.Projects = []model.ProjectWorkspace{{
		ID: "demo", Name: "DemoRepo", Paths: map[string]string{"gpu-1": "/srv/demo"},
	}}
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "调研 demo"})
	if err != nil {
		t.Fatal(err)
	}
	meta := eng.resolveTurnConsentMeta(ctx, trig)
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
	outProj := eng.toolDispatchWork(ctx, turn, trig, meta, `{"mode":"readonly","project":"demo","goal":"看结构"}`)
	if !strings.Contains(outProj, `"ok":false`) || !strings.Contains(outProj, "未配置机器 hub 的路径") {
		t.Fatalf("expected missing project×machine path error, got %s", outProj)
	}

	// 2. reply_user attachment promise without files/snapshot is rejected by ReplyDetailed.
	outAtt, exited := eng.toolReplyUser(ctx, turn, trig, `{"text":"完整报告附件已发，请查收附件"}`)
	if exited || !strings.Contains(outAtt, `"ok":false`) || !strings.Contains(outAtt, "attachment") {
		t.Fatalf("expected attachment-promise rejection from ReplyDetailed, exited=%v out=%s", exited, outAtt)
	}

	// 3. manage_timer ignores any extra target arg and always binds to model.RouterName.
	outTm := eng.toolManageTimer(ctx, turn, trig, `{"action":"add","spec":"in 15m","text":"巡检","target":"claude-1"}`)
	if !strings.Contains(outTm, `"ok":true`) {
		t.Fatalf("expected add timer ok, got %s", outTm)
	}
	timers, err := st.ListTimers(ctx)
	if err != nil || len(timers) != 1 || timers[0].Target != model.RouterName {
		t.Fatalf("expected timer hardcoded to %q, got %+v err=%v", model.RouterName, timers, err)
	}
}

func TestAPIRouterStrategy2BInflightKVAndReconcileOrder(t *testing.T) {
	ctx := context.Background()
	eng, st, _, _ := newTestAPIEngine(t, nil)

	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我处理一下"})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.recordAPIRouterInflight(ctx, trig, "dispatch_work", "claude-7"); err != nil {
		t.Fatalf("record inflight: %v", err)
	}

	// 1. If AckTriggerTarget fails during startup reconciliation (canceled ctx), KV marker MUST be retained.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	eng.reconcileAPIRouterInflight(canceledCtx)
	if raw, ok, err := st.GetKV(ctx, apiRouterInflightKVKey); err != nil || !ok || !strings.Contains(raw, trig.ID) {
		t.Fatalf("expected inflight KV retained on ACK failure, got ok=%v raw=%q err=%v", ok, raw, err)
	}
	if got, _ := st.GetTrigger(ctx, trig.ID); got == nil || got.Status != "queued" {
		t.Fatalf("expected trigger still queued after failed ACK, got %+v", got)
	}

	// 2. Once ACK succeeds, trigger is delivered AND KV marker is deleted.
	eng.reconcileAPIRouterInflight(ctx)
	if raw, ok, _ := st.GetKV(ctx, apiRouterInflightKVKey); ok && raw != "" {
		t.Fatalf("expected inflight KV cleared after successful ACK, got %q", raw)
	}
	if got, _ := st.GetTrigger(ctx, trig.ID); got == nil || got.Status != "delivered" {
		t.Fatalf("expected trigger delivered after reconcile, got %+v", got)
	}

	// 3. KV persistence failure aborts reply_user / dispatch_work / steer_worker BEFORE side effects.
	eng2, st2, sink2, dir2 := newTestAPIEngine(t, nil)
	var spawnCalled int
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		if method == "agent.start" || method == "agent.prompt" {
			spawnCalled++
		}
		return map[string]any{}, nil
	})
	eng2.fleet = fleet.NewFleet(hc, "hub", dir2, "test", []string{"claude"})
	if err := st2.UpsertWorker(ctx, &model.Worker{
		Name: "claude-live", Kind: "claude", Thread: model.MainThread, PaneID: "w1:p1", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	_, trig2, err := eng2.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "发消息"})
	if err != nil {
		t.Fatal(err)
	}
	meta2 := eng2.resolveTurnConsentMeta(ctx, trig2)
	turn2 := &apiRouterTurn{thread: model.MainThread, triggerID: trig2.ID}
	// Close DB to force SetKV failure before side effects.
	_ = st2.Close()
	outRep, exited := eng2.toolReplyUser(ctx, turn2, trig2, `{"text":"不应发送"}`)
	if exited || !strings.Contains(outRep, `"ok":false`) || !strings.Contains(outRep, "防重标记失败") {
		t.Fatalf("expected reply_user fail-fast on KV error, exited=%v out=%s", exited, outRep)
	}
	if outDisp := eng2.toolDispatchWork(ctx, turn2, trig2, meta2, `{"mode":"readonly","goal":"不应派发"}`); !strings.Contains(outDisp, `"ok":false`) {
		t.Fatalf("expected dispatch_work fail-fast on DB/KV error, got %s", outDisp)
	}
	if outRes := eng2.toolDispatchWork(ctx, turn2, trig2, meta2, `{"mode":"readonly","resume":"claude-live","goal":"不应续聊"}`); !strings.Contains(outRes, `"ok":false`) || !strings.Contains(outRes, "防重标记失败") {
		t.Fatalf("expected resume dispatch_work fail-fast on KV error, got %s", outRes)
	}
	if outSteer := eng2.toolSteerWorker(ctx, turn2, trig2, meta2, `{"worker":"claude-live","message":"不应追加"}`); !strings.Contains(outSteer, `"ok":false`) {
		t.Fatalf("expected steer_worker fail-fast on DB/KV error, got %s", outSteer)
	}
	sink2.mu.Lock()
	repN := len(sink2.replies)
	sink2.mu.Unlock()
	if repN != 0 || spawnCalled != 0 {
		t.Fatalf("expected zero side effects when KV write fails, replies=%d spawns=%d", repN, spawnCalled)
	}
}

func TestAPIRouterCircuitBreakerCountsMultiStepFailures(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqCount++
		mu.Unlock()
		// Step 1 succeeds with a read_detail tool call; Step 2 (has role=="tool") always returns 503.
		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasToolResult = true
				break
			}
		}
		if !hasToolResult {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c-step1", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_rd", "type": "function",
							"function": map[string]any{"name": "read_detail", "arguments": `{"kind":"work_item","target":"missing"}`},
						}},
					},
				}},
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"step2 503"}}`))
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "二步失败毒消息"})
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= maxSameAPITriggerFails; attempt++ {
		eng.mu.Lock()
		eng.router.backoffUntil = time.Time{}
		eng.mu.Unlock()
		eng.tick(ctx)
		waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "multi-step fail settled")
	}

	got, _ := st.GetTrigger(ctx, trig.ID)
	if got == nil || got.Status != "delivered" {
		t.Fatalf("expected poison trigger circuit-broken after %d turns even when step 1 succeeded, got %+v", maxSameAPITriggerFails, got)
	}
	mu.Lock()
	gotReqs := reqCount
	mu.Unlock()
	if gotReqs != maxSameAPITriggerFails*2 {
		t.Fatalf("expected %d requests (%d turns x 2 steps), got %d", maxSameAPITriggerFails*2, maxSameAPITriggerFails, gotReqs)
	}
}

func TestAPIRouterBareSilentTextNotAcceptedOnOptionalNotify(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		n := reqCount
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c-silent-text", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "[SILENT]"},
				}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c-noact", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_na", "type": "function",
						"function": map[string]any{"name": "no_action", "arguments": `{"reason":"确认无需打扰"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, sink, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	trig, err := eng.Ingest(ctx, "system", model.MainThread, "corgi", "[corgi morning-digest]", "", model.RouterName, "morning-digest")
	if err != nil {
		t.Fatal(err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "optional notify turn settled")

	mu.Lock()
	gotReqs := reqCount
	mu.Unlock()
	if gotReqs != 2 {
		t.Fatalf("expected bare [SILENT] text to trigger nudge + no_action tool call (2 requests), got %d", gotReqs)
	}
	ends := sink.ends()
	if len(ends) == 0 || ends[len(ends)-1].reason != "silent" {
		t.Fatalf("expected silent seal after no_action, got %+v", ends)
	}
}

func TestMemoryManagerOSRootSandbox(t *testing.T) {
	dir := t.TempDir()
	mem := NewMemoryManager(dir)
	if err := mem.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Legacy messages.md on disk is preserved (never deleted), hidden from ListMemoryFiles, and blocked from read/write.
	legacyMsg := filepath.Join(mem.MemDir(), "messages.md")
	if err := os.WriteFile(legacyMsg, []byte("legacy log"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mem.Ensure(); err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if _, err := os.Stat(legacyMsg); err != nil {
		t.Fatalf("legacy messages.md should not be deleted by Ensure: %v", err)
	}
	for _, mf := range mem.ListFiles() {
		if mf.Name == "messages.md" {
			t.Fatalf("legacy messages.md must be hidden from ListMemoryFiles")
		}
	}
	if _, err := mem.ReadFile("messages.md"); err == nil {
		t.Fatal("expected ReadFile(messages.md) rejected")
	}

	// Table-driven write whitelist validation.
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"instructions.md", true},
		{"notes/project-foo.md", true},
		{"messages.md", false},
		{"../escape.md", false},
		{"/etc/passwd", false},
		{"notes/sub/nested.md", false},
		{"tasks/todo.md", false},
		{"notes/.hidden.md", false},
		{"notes/plain.txt", false},
		{"dreams/2026-01-01.md", false},
	} {
		err := mem.SaveFile(tc.path, "# ok\n")
		if tc.ok && err != nil {
			t.Fatalf("SaveFile(%q) unexpectedly failed: %v", tc.path, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("SaveFile(%q) expected failure, succeeded", tc.path)
		}
	}

	// Write size limit (>32KB) and Read size limit (>256KB).
	if err := mem.SaveFile("notes/big.md", strings.Repeat("a", maxMemoryWriteBytes+1)); err == nil {
		t.Fatal("expected write >32KB rejected")
	}
	bigReadPath := filepath.Join(mem.MemDir(), "notes", "oversize.md")
	if err := os.WriteFile(bigReadPath, make([]byte, maxMemoryReadBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.ReadFile("notes/oversize.md"); err == nil {
		t.Fatal("expected read >256KB rejected")
	}

	// Symlink escape AND intra-root symlink rejected on both read and write.
	outside := filepath.Join(t.TempDir(), "outside.md")
	_ = os.WriteFile(outside, []byte("secret"), 0o600)
	_ = os.Symlink(outside, filepath.Join(mem.MemDir(), "notes", "ext-link.md"))
	_ = os.Symlink("../instructions.md", filepath.Join(mem.MemDir(), "notes", "intra-link.md"))
	for _, sym := range []string{"notes/ext-link.md", "notes/intra-link.md"} {
		if _, err := mem.ReadFile(sym); err == nil {
			t.Fatalf("expected symlink ReadFile(%q) rejected", sym)
		}
		if err := mem.SaveFile(sym, "overwrite"); err == nil {
			t.Fatalf("expected symlink SaveFile(%q) rejected", sym)
		}
	}

	// Dream journal atomic write & confined read.
	if err := mem.SaveDreamJournal("2026-11-28-0400.md", "摘要\n\n- 明细\n"); err != nil {
		t.Fatalf("SaveDreamJournal: %v", err)
	}
	if raw, err := mem.ReadFile("dreams/2026-11-28-0400.md"); err != nil || !strings.Contains(string(raw), "摘要") {
		t.Fatalf("ReadFile dream journal failed: raw=%q err=%v", string(raw), err)
	}
	if entries, err := os.ReadDir(filepath.Join(mem.MemDir(), "notes")); err == nil {
		for _, de := range entries {
			if strings.HasPrefix(de.Name(), ".corgi-tmp-") {
				t.Fatalf("leftover temp file in notes: %s", de.Name())
			}
		}
	}
}

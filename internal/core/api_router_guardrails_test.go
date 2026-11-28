package core

import (
	"context"
	"database/sql"
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
	_ "modernc.org/sqlite"
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

func TestStructuredTriggerDispatchOverrideReplacesHint(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	projDir := filepath.Join(dir, "demo-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := eng.Settings()
	cfg.WorkerKind = "claude"
	cfg.Projects = []model.ProjectWorkspace{{ID: "demo", Name: "Demo", Paths: map[string]string{"hub": projDir}}}
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		createdCwd  string
		startedCmd  string
		startedArgs []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.create":
			createdCwd, _ = params["cwd"].(string)
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w-ov"},
				"root_pane": map[string]any{"pane_id": "w-ov:p1"},
			}, nil
		case "agent.start":
			startedCmd, _ = params["agent"].(string)
			if raw, ok := params["args"].([]any); ok {
				for _, a := range raw {
					if s, ok := a.(string); ok {
						startedArgs = append(startedArgs, s)
					}
				}
			}
			return map[string]any{"pane_id": "w-ov:p1", "workspace_id": "w-ov", "session": "s-ov", "pid": 4242}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude", "codex"})

	// 1. Say persists structured DispatchOverride and does NOT pollute trig.Text with buildDispatchHint.
	_, trig, err := eng.Say(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "排查一下登录报错",
		Kind:      "codex",
		Model:     "gpt-5.4-mini",
		Effort:    "high",
		ProjectID: "demo",
	})
	if err != nil {
		t.Fatalf("Say: %v", err)
	}
	if strings.Contains(trig.Text, "用户本次指定运行参数") {
		t.Fatalf("trig.Text must not contain legacy buildDispatchHint text, got %q", trig.Text)
	}
	reloaded, err := st.GetTrigger(ctx, trig.ID)
	if err != nil || reloaded.Dispatch == nil || reloaded.Dispatch.Kind != "codex" || reloaded.Dispatch.Model != "gpt-5.4-mini" || reloaded.Dispatch.Effort != "high" || reloaded.Dispatch.ProjectID != "demo" {
		t.Fatalf("unexpected persisted DispatchOverride: %+v err=%v", reloaded, err)
	}

	// 2. Live tail state exposes structured override fields cleanly.
	meta := eng.resolveTurnConsentMeta(ctx, reloaded)
	tail := eng.buildLiveTailState(ctx, model.MainThread, reloaded, meta)
	if !strings.Contains(tail, "dispatch_kind=codex") || !strings.Contains(tail, "dispatch_project=demo") {
		t.Fatalf("expected structured dispatch override in live tail state, got: %s", tail)
	}

	// 3. toolDispatchWork automatically applies persisted DispatchOverride (kind/model/effort/project->cwd) on Spawn.
	out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: reloaded.ID}, reloaded, meta, `{"mode":"readonly","goal":"只读排查登录报错"}`)
	if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"kind":"codex"`) || !strings.Contains(out, `"model":"gpt-5.4-mini"`) {
		t.Fatalf("expected dispatch_work to inherit structured Trigger.Dispatch, got %s", out)
	}
	mu.Lock()
	gotCwd := createdCwd
	mu.Unlock()
	if gotCwd != projDir {
		t.Fatalf("expected Spawn cwd=%q from Trigger.Dispatch.ProjectID, got %q (cmd=%q args=%v)", projDir, gotCwd, startedCmd, startedArgs)
	}
}

func execSQLiteTestStmt(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open test sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func TestCodexReviewRegressions(t *testing.T) {
	ctx := context.Background()

	// 1. BLOCKER: Real AckTriggerTarget failure (GetKV succeeds, UPDATE triggers fails) blocks replay across
	//    startup reconcile, live mutated turn, and 3x circuit breaker; recovery ACKs cleanly with side effects == 1.
	t.Run("ack failure blocks replay until sqlite recovers", func(t *testing.T) {
		var (
			mu       sync.Mutex
			llmCalls int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			llmCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c-ack", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_rep", "type": "function",
							"function": map[string]any{"name": "reply_user", "arguments": `{"text":"只发一次"}`},
						}},
					},
				}},
			})
		}))
		defer srv.Close()

		eng, st, sink, _ := newTestAPIEngine(t, srv)
		_, t1, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "启动恢复测试"})
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.recordAPIRouterInflight(ctx, t1, "dispatch_work", "claude-1"); err != nil {
			t.Fatal(err)
		}
		execSQLiteTestStmt(t, st.DBPath(), `CREATE TRIGGER fail_trig_ack BEFORE UPDATE ON triggers BEGIN SELECT RAISE(ABORT, 'ack denied'); END;`)

		// Startup reconcile reads KV successfully, hits REAL AckTriggerTarget failure -> retains KV & blocks deliverAPIRouter.
		if ok := eng.reconcileAPIRouterInflight(ctx); ok {
			t.Fatal("expected reconcileAPIRouterInflight=false when AckTriggerTarget fails")
		}
		if raw, ok, err := st.GetKV(ctx, apiRouterInflightKVKey); err != nil || !ok || !strings.Contains(raw, t1.ID) {
			t.Fatalf("expected KV marker retained on real ACK failure, ok=%v raw=%q err=%v", ok, raw, err)
		}
		eng.deliverAPIRouter(ctx, []*model.Trigger{t1})
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		if llmCalls != 0 {
			t.Fatalf("expected 0 LLM calls while startup inflight ACK is failing, got %d", llmCalls)
		}
		mu.Unlock()

		// Drop trigger -> next deliverAPIRouter retries ACK, clears KV, and NEVER replays t1!
		execSQLiteTestStmt(t, st.DBPath(), `DROP TRIGGER fail_trig_ack;`)
		eng.deliverAPIRouter(ctx, []*model.Trigger{t1})
		waitUntil(t, 3*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "reconcile settled")
		if got, _ := st.GetTrigger(ctx, t1.ID); got == nil || got.Status != "delivered" {
			t.Fatalf("expected t1 delivered after SQLite recovered, got %+v", got)
		}
		if raw, ok, _ := st.GetKV(ctx, apiRouterInflightKVKey); ok && raw != "" {
			t.Fatalf("expected KV marker cleared after ACK recovery, got %q", raw)
		}
		mu.Lock()
		if llmCalls != 0 {
			t.Fatalf("expected 0 LLM calls for reconciled t1, got %d", llmCalls)
		}
		mu.Unlock()

		// Live turn executes reply_user (side effect = 1) while ACK fails -> next tick MUST NOT replay reply_user!
		_, t2, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "实时回合ACK失败"})
		if err != nil {
			t.Fatal(err)
		}
		execSQLiteTestStmt(t, st.DBPath(), `CREATE TRIGGER fail_trig_ack BEFORE UPDATE ON triggers BEGIN SELECT RAISE(ABORT, 'ack denied'); END;`)
		eng.deliverAPIRouter(ctx, []*model.Trigger{t2})
		waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "live mutated turn settled")
		// Tick again while ACK still fails -> MUST NOT invoke LLM a second time!
		eng.tick(ctx)
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		callsBeforeRecover := llmCalls
		mu.Unlock()
		sink.mu.Lock()
		repBeforeRecover := len(sink.replies)
		sink.mu.Unlock()
		if callsBeforeRecover != 1 || repBeforeRecover != 1 {
			t.Fatalf("expected strictly 1 LLM call & 1 reply while ACK failing, got calls=%d replies=%d", callsBeforeRecover, repBeforeRecover)
		}
		// Recover SQLite -> tick ACKs t2 cleanly, side effect count remains strictly 1!
		execSQLiteTestStmt(t, st.DBPath(), `DROP TRIGGER fail_trig_ack;`)
		eng.tick(ctx)
		waitUntil(t, 3*time.Second, func() bool {
			g, _ := st.GetTrigger(ctx, t2.ID)
			return g != nil && g.Status == "delivered"
		}, "t2 acked after recovery")
		mu.Lock()
		callsAfterRecover := llmCalls
		mu.Unlock()
		sink.mu.Lock()
		repAfterRecover := len(sink.replies)
		sink.mu.Unlock()
		if callsAfterRecover != 1 || repAfterRecover != 1 {
			t.Fatalf("expected side effects == 1 after ACK recovery, got calls=%d replies=%d", callsAfterRecover, repAfterRecover)
		}
	})

	// 1b. BLOCKER: 3x circuit breaker with ACK failure MUST NOT reset counter and exceed 3 LLM calls.
	t.Run("circuit breaker with ack failure never exceeds 3 attempts", func(t *testing.T) {
		var (
			mu       sync.Mutex
			llmCalls int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			llmCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"503"}}`))
		}))
		defer srv.Close()

		eng, st, _, _ := newTestAPIEngine(t, srv)
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "毒消息ACK失败"})
		if err != nil {
			t.Fatal(err)
		}
		execSQLiteTestStmt(t, st.DBPath(), `CREATE TRIGGER fail_trig_ack BEFORE UPDATE ON triggers BEGIN SELECT RAISE(ABORT, 'ack denied'); END;`)
		for i := 0; i < 5; i++ {
			eng.mu.Lock()
			eng.router.backoffUntil = time.Time{}
			eng.mu.Unlock()
			eng.tick(ctx)
			waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "cb attempt settled")
		}
		mu.Lock()
		gotCalls := llmCalls
		mu.Unlock()
		if gotCalls != maxSameAPITriggerFails {
			t.Fatalf("expected strictly %d LLM attempts despite ACK failure, got %d", maxSameAPITriggerFails, gotCalls)
		}
		execSQLiteTestStmt(t, st.DBPath(), `DROP TRIGGER fail_trig_ack;`)
		eng.tick(ctx)
		if got, _ := st.GetTrigger(ctx, trig.ID); got == nil || got.Status != "delivered" {
			t.Fatalf("expected poison trigger ACKed on SQLite recovery, got %+v", got)
		}
	})

	// 2. MAJOR/MINOR MemoryManager: non-nil empty JSON [], anchored root rejects symlink memory/dreams dir, exact 32KiB boundary.
	t.Run("memory non-nil json, anchored root symlink block, exact 32KB limit", func(t *testing.T) {
		dir := t.TempDir()
		mem := NewMemoryManager(dir)
		if err := mem.Ensure(); err != nil {
			t.Fatal(err)
		}
		// Empty dreams serializes as [] (never null -> prevents MemoryTab crash before first dream).
		b, err := json.Marshal(MemorySnapshot{Dir: mem.MemDir(), Files: mem.ListFiles(), Dreams: mem.ListDreams()})
		if err != nil || !strings.Contains(string(b), `"dreams":[]`) {
			t.Fatalf("expected dreams:[] JSON array, got %s err=%v", string(b), err)
		}
		// Exact 32 KiB final byte limit (with trailing \n normalization).
		if err := mem.SaveFile("notes/exact-over.md", strings.Repeat("a", maxMemoryWriteBytes)); err == nil {
			t.Fatal("expected 32768 chars + appended newline (32769 bytes) rejected")
		}
		if err := mem.SaveFile("notes/exact-ok.md", strings.Repeat("a", maxMemoryWriteBytes-1)); err != nil {
			t.Fatalf("expected 32767 chars + appended newline (32768 bytes) accepted, got %v", err)
		}
		// Replacing memory/ or dreams/ itself with a symlink pointing outside corgiDir MUST be rejected.
		outsideDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(outsideDir, "secret.md"), []byte("outside"), 0o600)
		symBase := t.TempDir()
		_ = os.MkdirAll(filepath.Join(symBase, "router"), 0o700)
		_ = os.Symlink(outsideDir, filepath.Join(symBase, "router", "memory"))
		_ = os.Symlink(outsideDir, filepath.Join(symBase, "router", "dreams"))
		symMem := NewMemoryManager(symBase)
		if err := symMem.Ensure(); err == nil {
			t.Fatal("expected Ensure to reject symlinked memory/dreams directories")
		}
		if _, err := symMem.ReadFile("secret.md"); err == nil {
			t.Fatal("expected ReadFile to reject symlinked memory directory")
		}
		if err := symMem.SaveFile("notes/a.md", "hi"); err == nil {
			t.Fatal("expected SaveFile to reject symlinked memory directory")
		}
		if err := symMem.SaveDreamJournal("2026-11-28-0400.md", "hi"); err == nil {
			t.Fatal("expected SaveDreamJournal to reject symlinked dreams directory")
		}
	})

	// 3. MAJOR Cross-thread isolation: steer_worker rejects foreign work_id; progress never leaks foreign WorkItem title.
	t.Run("cross-thread steer_worker work_id and progress isolation", func(t *testing.T) {
		eng, st, _, _ := newTestAPIEngine(t, nil)
		foreignWI, err := eng.SetWorkItem(ctx, "new", "wecom:group-secret", "机密外线程事项标题", "待办", "", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = st.UpsertWorker(ctx, &model.Worker{Name: "claude-main", Kind: "claude", Thread: model.MainThread, PaneID: "w1:p1", Status: "idle"})
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "继续"})
		if err != nil {
			t.Fatal(err)
		}
		meta := eng.resolveTurnConsentMeta(ctx, trig)
		turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
		if out := eng.toolSteerWorker(ctx, turn, trig, meta, `{"worker":"claude-main","message":"hi","work_id":"`+foreignWI.ID+`"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "不属于当前会话线程") {
			t.Fatalf("expected foreign work_id rejected in steer_worker, got %s", out)
		}
		prog := eng.formatAPIToolProgress(ctx, model.MainThread, LLMToolCall{
			Function: LLMToolFunctionCall{Name: "read_detail", Arguments: `{"kind":"work_item","target":"` + foreignWI.ID + `"}`},
		})
		if strings.Contains(prog, "机密外线程事项标题") {
			t.Fatalf("progress leaked foreign-thread WorkItem title: %q", prog)
		}
	})

	// 4. MAJOR Composer override wins over LLM args, preserves hub/queued picks, and auto-matched project fails without home fallback.
	t.Run("composer override priority, queued prompt, and auto-matched project cwd", func(t *testing.T) {
		eng, _, _, dir := newTestAPIEngine(t, nil)
		projDir := filepath.Join(dir, "corgi-repo")
		_ = os.MkdirAll(projDir, 0o755)
		cfg := eng.Settings()
		cfg.WorkerMachine = "gpu-remote"
		cfg.WorkerKind = "claude"
		cfg.Projects = []model.ProjectWorkspace{
			{ID: "corgi", Name: "Corgi", Paths: map[string]string{"hub": projDir}},
			{ID: "remoteonly", Name: "RemoteOnly", Paths: map[string]string{"gpu-remote": "/srv/remoteonly"}},
		}
		if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
			t.Fatal(err)
		}
		var createdCwd string
		hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
			if method == "workspace.create" {
				createdCwd, _ = params["cwd"].(string)
				return map[string]any{"workspace": map[string]any{"workspace_id": "w-c"}, "root_pane": map[string]any{"pane_id": "w-c:p1"}}, nil
			}
			return map[string]any{}, nil
		})
		eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude", "codex"})

		// Enqueue while router busy, then promote via SteerChatPrompt -> preserves explicit machine="hub" + kind/model/effort/project.
		eng.mu.Lock()
		eng.router.turn = &apiRouterTurn{thread: model.MainThread, triggerID: "t0", done: make(chan struct{})}
		eng.mu.Unlock()
		queued, item, _, _, err := eng.SubmitChatSay(ctx, SayRequest{
			Thread: model.MainThread, Channel: "web", From: "you", Text: "检查代码",
			Machine: "hub", Kind: "codex", Model: "gpt-5.4-mini", Effort: "high", ProjectID: "corgi",
		}, "queue")
		if err != nil || !queued || item == nil {
			t.Fatalf("expected queued chat prompt: queued=%v item=%v err=%v", queued, item, err)
		}
		eng.mu.Lock()
		eng.router.turn = nil
		eng.mu.Unlock()
		_, promotedTrig, err := eng.SteerChatPrompt(ctx, model.MainThread, item.ID)
		if err != nil || promotedTrig.Dispatch == nil || promotedTrig.Dispatch.Machine != "hub" {
			t.Fatalf("expected promoted trigger preserving machine=hub, got %+v err=%v", promotedTrig, err)
		}
		// LLM passes conflicting machine/kind/project -> human's Trigger.Dispatch MUST win!
		meta := eng.resolveTurnConsentMeta(ctx, promotedTrig)
		out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: promotedTrig.ID}, promotedTrig, meta,
			`{"mode":"readonly","machine":"gpu-remote","kind":"claude","project":"remoteonly","goal":"只读核查"}`)
		if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"kind":"codex"`) || !strings.Contains(out, `"model":"gpt-5.4-mini"`) || createdCwd != projDir {
			t.Fatalf("expected Composer override to beat conflicting LLM args, out=%s cwd=%q", out, createdCwd)
		}

		// Auto-matched project (omitted from args.Project, mentioned in goal) missing hub path MUST fail explicitly.
		_, autoTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "看看 RemoteOnly 仓库", Machine: "hub"})
		if err != nil {
			t.Fatal(err)
		}
		outAuto := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: autoTrig.ID}, autoTrig, eng.resolveTurnConsentMeta(ctx, autoTrig),
			`{"mode":"readonly","goal":"排查 RemoteOnly 仓库结构"}`)
		if !strings.Contains(outAuto, `"ok":false`) || !strings.Contains(outAuto, "未配置机器 hub 的路径") {
			t.Fatalf("expected auto-matched project missing hub path to fail explicitly, got %s", outAuto)
		}
	})

	// 5. MINOR: Real Action triggers generated by ResolveAction strictly check ResolvedChannel (no ResolvedBy fallback).
	t.Run("real action trigger derives isHuman strictly from ResolvedChannel", func(t *testing.T) {
		eng, st, _, _ := newTestAPIEngine(t, nil)
		_, actRouter, _, _ := eng.Reply(ctx, model.MainThread, "自动确认？", "", []string{"是", "否"}, false)
		if _, err := eng.ResolveAction(ctx, actRouter.ID, "router", "1"); err != nil {
			t.Fatal(err)
		}
		_, actWeb, _, _ := eng.Reply(ctx, model.MainThread, "人工确认？", "", []string{"是", "否"}, false)
		if _, err := eng.ResolveAction(ctx, actWeb.ID, "web", "1"); err != nil {
			t.Fatal(err)
		}
		trs, _ := st.ListQueuedTriggers(ctx)
		var routerTrig, webTrig *model.Trigger
		for _, tr := range trs {
			if tr.Ref != "" {
				if en, err := st.GetEntry(ctx, tr.Ref); err == nil && en != nil {
					if en.ActionID == actRouter.ID {
						routerTrig = tr
					} else if en.ActionID == actWeb.ID {
						webTrig = tr
					}
				}
			}
		}
		if routerTrig == nil || eng.resolveTurnConsentMeta(ctx, routerTrig).isHuman {
			t.Fatalf("expected real router-resolved Action trigger to have isHuman=false, trig=%+v", routerTrig)
		}
		if webTrig == nil || !eng.resolveTurnConsentMeta(ctx, webTrig).isHuman {
			t.Fatalf("expected real web-resolved Action trigger to have isHuman=true, trig=%+v", webTrig)
		}
		// Legacy Action with empty ResolvedChannel and ResolvedBy="web" must NOT be trusted as human.
		legacyAct := &model.Action{ID: "a-legacy-ch", Kind: model.ActionKindRouterChoice, Thread: model.MainThread, Title: "旧卡", Status: model.ActionResolved, ResolvedBy: "web", ResolvedChannel: "", ChosenKey: "1", ChosenLabel: "是", CreatedAt: time.Now()}
		_ = st.InsertAction(ctx, legacyAct)
		_ = st.InsertEntry(ctx, &model.Entry{ID: "e-legacy-ch", Thread: model.MainThread, Role: "user", Channel: "web", ActionID: legacyAct.ID, Text: "是", At: time.Now()})
		legacyTrig := &model.Trigger{ID: "t-legacy-ch", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "owner", Ref: "e-legacy-ch", Text: "是"}
		if eng.resolveTurnConsentMeta(ctx, legacyTrig).isHuman {
			t.Fatal("expected Action with empty ResolvedChannel rejected as non-human")
		}
	})
}

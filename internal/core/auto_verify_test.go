package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/gitwt"
	"github.com/kiperjing/corgi/internal/model"
)

func seedImplementWorker(t *testing.T, eng *Engine, name, wtPath, baseCommit, scope string) (*model.Worker, *model.WorkItem) {
	t.Helper()
	ctx := context.Background()
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "自动验收测试项", "", "", "")
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	if scope != "" {
		if _, err := eng.store.SetWorkItemScope(ctx, wi.ID, scope); err != nil {
			t.Fatalf("set scope: %v", err)
		}
		wi.Scope = scope
	}
	wk := &model.Worker{
		Name:           name,
		Kind:           "codex",
		Thread:         model.MainThread,
		PaneID:         "w1:p1",
		Session:        "sess-1",
		Status:         "idle",
		Cwd:            wtPath,
		WorktreePath:   wtPath,
		WorktreeBranch: "corgi/" + name,
		WorktreeBase:   baseCommit,
		WorkItemID:     wi.ID,
		CreatedAt:      time.Now(),
	}
	if err := eng.store.UpsertWorker(ctx, wk); err != nil {
		t.Fatalf("upsert worker: %v", err)
	}
	return wk, wi
}

func TestAutoVerifyScenariosTable(t *testing.T) {
	cases := []struct {
		name           string
		res            *VerifyWorkerResponse
		verifyErr      error
		wantPrefix     string
		wantNoPrefix   bool
		wantRouterWake bool
	}{
		{
			name:           "clean_zero_changes_passthrough",
			res:            &VerifyWorkerResponse{VerifyResult: gitwt.VerifyResult{OK: true, BaseChecked: true, Clean: true}},
			wantNoPrefix:   true,
			wantRouterWake: true,
		},
		{
			name:           "old_node_without_base_checked_passthrough",
			res:            &VerifyWorkerResponse{VerifyResult: gitwt.VerifyResult{OK: true, BaseChecked: false}},
			wantNoPrefix:   true,
			wantRouterWake: true,
		},
		{
			name:           "zero_checks_skipped",
			res:            &VerifyWorkerResponse{VerifyResult: gitwt.VerifyResult{OK: true, BaseChecked: true, Clean: false, ChecksPassed: true}},
			wantPrefix:     "[auto_verify: SKIPPED · 未检测到构建/测试命令]",
			wantRouterWake: true,
		},
		{
			name: "pass_prepends_header_before_truncation",
			res: &VerifyWorkerResponse{VerifyResult: gitwt.VerifyResult{
				OK: true, BaseChecked: true, ChecksPassed: true,
				Checks: []gitwt.VerifyCheck{
					{Name: "go-vet", Command: "go vet ./...", Passed: true},
					{Name: "go-test", Command: "go test ./...", Passed: true},
				},
			}},
			wantPrefix:     "[auto_verify: PASS · go-vet, go-test]",
			wantRouterWake: true,
		},
		{
			name:           "verify_transport_error_escalates",
			verifyErr:      context.DeadlineExceeded,
			wantPrefix:     "[auto_verify: FAIL · 自动验证执行失败：",
			wantRouterWake: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				routerTexts []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []LLMMessage `json:"messages"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if len(req.Messages) > 0 {
					if s, ok := req.Messages[len(req.Messages)-1].Content.(string); ok {
						mu.Lock()
						routerTexts = append(routerTexts, s)
						mu.Unlock()
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-av", "model": "hy4",
					"choices": []map[string]any{{
						"index": 0, "finish_reason": "tool_calls",
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id": "call_av", "type": "function",
								"function": map[string]any{"name": "reply_user", "arguments": `{"purpose":"result","text":"已完成验收汇报"}`},
							}},
						},
					}},
				})
			}))
			defer srv.Close()

			eng, st, _, dir := newTestAPIEngine(t, srv.URL+"/v1")
			ctx := context.Background()
			wk, _ := seedImplementWorker(t, eng, "codex-1", dir, "abc1234", "implement")
			eng.markWorkerImplementDispatched(wk.Name)

			var verifyCalls int
			eng.autoVerifyFn = func(_ context.Context, req VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
				verifyCalls++
				if !req.SkipIfClean || req.BaseCommit != "abc1234" {
					t.Errorf("expected SkipIfClean=true and BaseCommit=abc1234, got %+v", req)
				}
				return tc.res, tc.verifyErr
			}

			// Emit a long report (>2200 runes) to verify PASS/SKIPPED/FAIL sits BEFORE the 2000-rune truncation.
			longReport := "完成功能改造：" + strings.Repeat("甲", 2300)
			trig, err := eng.Report(ctx, wk.Name, wk.PaneID, longReport)
			if err != nil {
				t.Fatalf("Report: %v", err)
			}
			origArtID := trig.ArtifactID
			origWI := trig.WorkItemID

			// Tick 1 intercepts and launches background verify.
			eng.tick(ctx)
			waitUntil(t, 3*time.Second, func() bool {
				eng.mu.Lock()
				defer eng.mu.Unlock()
				return eng.autoVerifyReleased[trig.ID]
			}, "auto verify completion")

			// Tick 2 delivers verified trigger to API Router.
			eng.tick(ctx)
			waitUntil(t, 3*time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(routerTexts) >= 1
			}, "router delivery")
			waitUntil(t, 3*time.Second, func() bool {
				eng.mu.Lock()
				defer eng.mu.Unlock()
				return eng.apiTurn == nil
			}, "api turn settled")

			updatedTrig, err := st.GetTrigger(ctx, trig.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updatedTrig.ArtifactID != origArtID || updatedTrig.WorkItemID != origWI {
				t.Fatalf("expected ArtifactID=%q WorkItemID=%q preserved, got %q %q", origArtID, origWI, updatedTrig.ArtifactID, updatedTrig.WorkItemID)
			}
			mu.Lock()
			gotPrompt := routerTexts[0]
			mu.Unlock()

			if tc.wantNoPrefix {
				if strings.Contains(updatedTrig.Text, "[auto_verify:") {
					t.Fatalf("expected no auto_verify prefix, got %q", updatedTrig.Text[:min(120, len(updatedTrig.Text))])
				}
			} else {
				if !strings.HasPrefix(updatedTrig.Text, tc.wantPrefix) {
					t.Fatalf("expected trigger text to start with %q, got %q", tc.wantPrefix, updatedTrig.Text[:min(160, len(updatedTrig.Text))])
				}
				if !strings.Contains(gotPrompt, tc.wantPrefix) {
					t.Fatalf("expected Router prompt (before 2000-rune clip) to contain %q", tc.wantPrefix)
				}
			}
			if verifyCalls != 1 {
				t.Fatalf("expected exactly 1 verify call, got %d", verifyCalls)
			}
		})
	}
}

func TestAutoVerifyFirstFailBouncesSecondFailEscalates(t *testing.T) {
	var (
		mu          sync.Mutex
		routerCalls int
		routerBody  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []LLMMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		routerCalls++
		if len(req.Messages) > 0 {
			routerBody, _ = req.Messages[len(req.Messages)-1].Content.(string)
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-bounce", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_bounce", "type": "function",
						"function": map[string]any{"name": "reply_user", "arguments": `{"purpose":"answer","text":"验收失败，需进一步处理"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, _, dir := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()
	wk, _ := seedImplementWorker(t, eng, "codex-b1", dir, "base111", "implement")
	eng.markWorkerImplementDispatched(wk.Name)

	var verifyCalls int
	eng.autoVerifyFn = func(_ context.Context, _ VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
		verifyCalls++
		return &VerifyWorkerResponse{
			VerifyResult: gitwt.VerifyResult{
				OK:           false,
				BaseChecked:  true,
				ChecksPassed: false,
				Checks: []gitwt.VerifyCheck{
					{Name: "go-test", Command: "go test ./...", Passed: false, Output: "FAIL TestAdd: got -1 want 5"},
				},
			},
		}, nil
	}

	// First completion -> fails verify -> auto-bounces to worker (1/1), acks Router trigger, does NOT wake Router.
	t1, err := eng.Report(ctx, wk.Name, wk.PaneID, "第一轮改好了")
	if err != nil {
		t.Fatal(err)
	}
	eng.tick(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		tr, _ := st.GetTrigger(ctx, t1.ID)
		return tr != nil && tr.Status == "delivered"
	}, "first trigger acked on bounce")

	// Confirm Router was NOT woken and a bounce trigger was queued for the worker.
	eng.tick(ctx)
	mu.Lock()
	rc1 := routerCalls
	mu.Unlock()
	if rc1 != 0 {
		t.Fatalf("expected 0 router calls on first bounce, got %d", rc1)
	}
	queued, _ := st.ListQueuedTriggers(ctx)
	foundBounce := false
	for _, q := range queued {
		if len(q.Targets) == 1 && q.Targets[0] == wk.Name && strings.Contains(q.Text, "自动验证") {
			foundBounce = true
			_ = st.AckTriggerTarget(ctx, q.ID, wk.Name) // simulate worker receiving bounce
		}
	}
	if !foundBounce {
		t.Fatalf("expected bounce trigger queued for worker %s, got %+v", wk.Name, queued)
	}
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
	foundBounceEvent := false
	for _, en := range entries {
		if en.Role == "event" && strings.Contains(en.Text, "已自动打回修复 (1/1)") {
			foundBounceEvent = true
		}
	}
	if !foundBounceEvent {
		t.Fatalf("expected quiet bounce event (1/1), got %+v", entries)
	}

	// Second completion -> fails verify again -> MUST NOT bounce again; escalates to Router with [auto_verify: FAIL ...].
	t2, err := eng.Report(ctx, wk.Name, wk.PaneID, "第二轮修完了")
	if err != nil {
		t.Fatal(err)
	}
	eng.tick(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.autoVerifyReleased[t2.ID]
	}, "second failure released")
	eng.tick(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return routerCalls == 1
	}, "router woken on second failure")
	waitUntil(t, 3*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.apiTurn == nil
	}, "second turn settled")

	mu.Lock()
	body := routerBody
	mu.Unlock()
	if !strings.Contains(body, "[auto_verify: FAIL · 已自动打回 1 次仍未通过") {
		t.Fatalf("expected escalated FAIL header in Router prompt, got %q", body)
	}
	if verifyCalls != 2 {
		t.Fatalf("expected 2 verify calls total, got %d", verifyCalls)
	}
}

func TestAutoVerifyGateExclusions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-excl", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_excl", "type": "function",
						"function": map[string]any{"name": "reply_user", "arguments": `{"purpose":"answer","text":"已收到汇报"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, _, dir := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()

	var verifyCalls int
	eng.autoVerifyFn = func(_ context.Context, _ VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
		verifyCalls++
		return &VerifyWorkerResponse{VerifyResult: gitwt.VerifyResult{OK: true, BaseChecked: true}}, nil
	}

	// 1. Sticky implement WorkItem, but worker was NOT dispatched in implement mode (e.g. readonly worker).
	wReadonly, _ := seedImplementWorker(t, eng, "codex-ro", dir, "base1", "implement")
	eng.invalidateWorkerAutoVerify(wReadonly.Name, true)
	tr1, _ := eng.Report(ctx, wReadonly.Name, wReadonly.PaneID, "只读分析完成")
	out1 := eng.filterAutoVerifyTriggers(ctx, nil, []*model.Trigger{tr1})
	if len(out1) != 1 {
		t.Fatal("expected readonly worker on sticky implement work_item to bypass auto-verify")
	}

	// 2. Implement worker sending BLOCKED: report or progress report -> must bypass auto-verify.
	wImpl, _ := seedImplementWorker(t, eng, "codex-im", dir, "base2", "implement")
	eng.markWorkerImplementDispatched(wImpl.Name)
	trBlocked, _ := eng.Report(ctx, wImpl.Name, wImpl.PaneID, "BLOCKED: 需要线上数据库权限 · tried: 本地验证")
	trProg1, _ := eng.Report(ctx, wImpl.Name, wImpl.PaneID, "进度：正在运行单元测试")
	trProg2, _ := eng.Report(ctx, wImpl.Name, wImpl.PaneID, "PROGRESS: compiling binary")
	out2 := eng.filterAutoVerifyTriggers(ctx, nil, []*model.Trigger{trBlocked, trProg1, trProg2})
	if len(out2) != 3 {
		t.Fatalf("expected BLOCKED and progress reports to bypass auto-verify, got %d", len(out2))
	}

	// 3. Implement worker without WorktreePath -> must bypass auto-verify.
	wNoWT, _ := seedImplementWorker(t, eng, "codex-nowt", "", "base3", "implement")
	eng.markWorkerImplementDispatched(wNoWT.Name)
	trNoWT, _ := eng.Report(ctx, wNoWT.Name, wNoWT.PaneID, "完成了")
	out3 := eng.filterAutoVerifyTriggers(ctx, nil, []*model.Trigger{trNoWT})
	if len(out3) != 1 {
		t.Fatal("expected worker without WorktreePath to bypass auto-verify")
	}

	// 4. AskPermissions enabled -> must bypass automatic background command execution.
	cfg := eng.Settings()
	cfg.AskPermissions = true
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	trAsk, _ := eng.Report(ctx, wImpl.Name, wImpl.PaneID, "改完了")
	out4 := eng.filterAutoVerifyTriggers(ctx, nil, []*model.Trigger{trAsk})
	if len(out4) != 1 {
		t.Fatal("expected AskPermissions=true to bypass automatic background verify")
	}
	cfg.AskPermissions = false
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	// 5. Read-only lookup of an already-live worker via ResumeWorkerSession must NOT clear implementTurn.
	if liveWk, resumed, err := eng.ResumeWorkerSession(ctx, ResumeWorkerRequest{Name: wImpl.Name}); err != nil || resumed || liveWk == nil {
		t.Fatalf("ResumeWorkerSession live lookup: wk=%v resumed=%v err=%v", liveWk, resumed, err)
	}
	eng.mu.Lock()
	stillImpl := eng.autoVerify[wImpl.Name] != nil && eng.autoVerify[wImpl.Name].implementTurn
	eng.mu.Unlock()
	if !stillImpl {
		t.Fatal("expected live ResumeWorkerSession lookup NOT to clear implementTurn")
	}

	if verifyCalls != 0 {
		t.Fatalf("expected 0 auto-verify executions for excluded cases, got %d", verifyCalls)
	}
}

func TestAutoVerifyExplicitReportPlusIdleSingleDeliveryPreservesArtifact(t *testing.T) {
	var (
		mu          sync.Mutex
		routerCalls int
		routerBody  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []LLMMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		routerCalls++
		if len(req.Messages) > 0 {
			routerBody, _ = req.Messages[len(req.Messages)-1].Content.(string)
		}
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-art", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_art", "type": "function",
						"function": map[string]any{"name": "reply_user", "arguments": `{"purpose":"result","text":"已完成交付"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, _, dir := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()
	wk, wi := seedImplementWorker(t, eng, "codex-art", dir, "base-art", "implement")
	eng.markWorkerImplementDispatched(wk.Name)
	eng.autoVerifyFn = func(_ context.Context, _ VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
		return &VerifyWorkerResponse{
			VerifyResult: gitwt.VerifyResult{
				OK: true, BaseChecked: true, ChecksPassed: true,
				Checks: []gitwt.VerifyCheck{{Name: "go-test", Passed: true}},
			},
		}, nil
	}

	// Explicit long report (>500 runes) creates a full report Artifact snapshot on Hub.
	longText := "详细完成报告：\n" + strings.Repeat("修复边界条件与单测覆盖；", 60)
	trig, err := eng.Report(ctx, wk.Name, wk.PaneID, longText)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if trig.ArtifactID == "" {
		t.Fatal("expected Report >500 runes to create an ArtifactID snapshot")
	}

	// Simulate worker going idle seconds later with closing prose in transcript -> must be suppressed.
	snap := workerTurnSnapshot{
		FromTranscript: true,
		AfterLastUser:  true,
		Text:           "已经通过 corgi report 汇报完毕。",
		AssistantAt:    time.Now().Unix(),
	}
	if eng.shouldEmitWorkerTurnSummary(wk, snap, time.Now()) {
		t.Fatal("expected closing idle summary to be suppressed after explicit Report")
	}

	// Run auto-verify and deliver once to Router.
	eng.tick(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.autoVerifyReleased[trig.ID]
	}, "verify pass released")
	eng.tick(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return routerCalls == 1
	}, "single router delivery")
	waitUntil(t, 3*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.apiTurn == nil
	}, "turn settled")

	saved, err := st.GetTrigger(ctx, trig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ArtifactID != trig.ArtifactID || saved.WorkItemID != wi.ID {
		t.Fatalf("expected ArtifactID=%q WorkItemID=%q preserved, got %q %q", trig.ArtifactID, wi.ID, saved.ArtifactID, saved.WorkItemID)
	}
	mu.Lock()
	body := routerBody
	calls := routerCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 router delivery, got %d", calls)
	}
	if !strings.Contains(body, "[auto_verify: PASS · go-test]") || !strings.Contains(body, "artifact="+trig.ArtifactID) {
		t.Fatalf("expected PASS tag and artifact=%s in Router prompt, got %q", trig.ArtifactID, body)
	}
}

func TestAutoVerifyConcurrentInvalidation(t *testing.T) {
	for _, action := range []string{"steer", "direct_pane", "stop", "mode_switch_pty"} {
		t.Run(action, func(t *testing.T) {
			eng, st, _, dir := newTestAPIEngine(t, "http://127.0.0.1:1/v1")
			ctx := context.Background()
			wk, _ := seedImplementWorker(t, eng, "codex-race", dir, "base999", "implement")
			eng.markWorkerImplementDispatched(wk.Name)

			inVerify := make(chan struct{})
			releaseVerify := make(chan struct{})
			var ctxCanceled bool
			var vMu sync.Mutex
			eng.autoVerifyFn = func(vctx context.Context, _ VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
				close(inVerify)
				select {
				case <-vctx.Done():
					vMu.Lock()
					ctxCanceled = true
					vMu.Unlock()
				case <-releaseVerify:
				}
				return &VerifyWorkerResponse{
					VerifyResult: gitwt.VerifyResult{
						OK: false, BaseChecked: true, ChecksPassed: false,
						Checks: []gitwt.VerifyCheck{{Name: "go-test", Passed: false, Output: "stale failure"}},
					},
				}, nil
			}

			trig, err := eng.Report(ctx, wk.Name, wk.PaneID, "报告完成")
			if err != nil {
				t.Fatal(err)
			}
			// Intercept and start in-flight verify.
			held := eng.filterAutoVerifyTriggers(ctx, nil, []*model.Trigger{trig})
			if len(held) != 0 {
				t.Fatal("expected trigger held while verify starts")
			}
			<-inVerify

			// External event happens mid-verify.
			switch action {
			case "steer":
				if _, _, err := eng.SendToWorkerWithWork(ctx, wk.Name, model.MainThread, "补充改一下日志格式", wk.WorkItemID); err != nil {
					t.Fatalf("SendToWorkerWithWork: %v", err)
				}
			case "direct_pane":
				// Queue mode works with nil fleet and still invalidates auto-verify by pane/target.
				if _, _, err := eng.SubmitPanePrompt(ctx, wk.PaneID, "直接问个问题", "queue"); err != nil {
					t.Fatalf("SubmitPanePrompt: %v", err)
				}
			case "stop":
				if err := eng.StopWorker(ctx, wk.Name); err != nil {
					t.Fatalf("StopWorker: %v", err)
				}
			case "mode_switch_pty":
				cfg := eng.Settings()
				cfg.RouterMode = "pty"
				if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
					t.Fatalf("SaveConfig pty: %v", err)
				}
			}

			close(releaseVerify)
			waitUntil(t, 3*time.Second, func() bool {
				eng.mu.Lock()
				defer eng.mu.Unlock()
				return eng.autoVerifyReleased[trig.ID] && (eng.autoVerify[wk.Name] == nil || !eng.autoVerify[wk.Name].inFlight)
			}, "invalidated verify settled")

			vMu.Lock()
			canceled := ctxCanceled
			vMu.Unlock()
			if !canceled {
				t.Fatalf("expected verify ctx canceled on %s", action)
			}

			// Verify original trigger was NOT prefixed with stale failure and NO bounce prompt was queued.
			freshTrig, err := st.GetTrigger(ctx, trig.ID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(freshTrig.Text, "[auto_verify:") {
				t.Fatalf("expected stale verify NOT to mutate trigger text, got %q", freshTrig.Text)
			}
			queued, _ := st.ListQueuedTriggers(ctx)
			for _, q := range queued {
				if strings.Contains(q.Text, "自动验证（corgi verify）未通过") {
					t.Fatalf("expected NO stale bounce prompt after %s, found %+v", action, q)
				}
			}
		})
	}
}

func TestAutoVerifyEndToEndRealGitRepo(t *testing.T) {
	// Real git repo + real gitwt.Verify (autoVerifyFn == nil).
	repo := t.TempDir()
	runGitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	writeFile := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGitCmd("init", "-b", "main")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "tester")
	writeFile("go.mod", "module example.com/e2eav\n\ngo 1.22\n")
	writeFile("calc.go", "package e2eav\n\nfunc Mul(a, b int) int { return a * b }\n")
	writeFile("calc_test.go", "package e2eav\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif Mul(3, 4) != 12 { t.Fatal(\"bad\") }\n}\n")
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "init")
	base := runGitCmd("rev-parse", "HEAD")

	var (
		mu         sync.Mutex
		routerSeen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []LLMMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			if s, ok := req.Messages[len(req.Messages)-1].Content.(string); ok {
				mu.Lock()
				routerSeen = append(routerSeen, s)
				mu.Unlock()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-e2e", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_e2e", "type": "function",
						"function": map[string]any{"name": "reply_user", "arguments": `{"purpose":"result","text":"实测验收已通过"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()
	wk, _ := seedImplementWorker(t, eng, "codex-e2e", repo, base, "implement")
	eng.markWorkerImplementDispatched(wk.Name)

	// Step 1: Introduce a bug, report -> real `go test` fails -> auto-bounce (1/1), Router stays asleep.
	writeFile("calc.go", "package e2eav\n\nfunc Mul(a, b int) int { return a + b }\n")
	t1, err := eng.Report(ctx, wk.Name, wk.PaneID, "已实现 Mul")
	if err != nil {
		t.Fatal(err)
	}
	eng.tick(ctx)
	waitUntil(t, 20*time.Second, func() bool {
		tr, _ := st.GetTrigger(ctx, t1.ID)
		return tr != nil && tr.Status == "delivered"
	}, "real git test failure bounce")

	// Clear bounce trigger targeting worker (simulating worker picking it up).
	queued, _ := st.ListQueuedTriggers(ctx)
	for _, q := range queued {
		if len(q.Targets) == 1 && q.Targets[0] == wk.Name {
			_ = st.AckTriggerTarget(ctx, q.ID, wk.Name)
		}
	}

	// Step 2: Fix bug, report -> real `go vet` + `go test` PASS -> Router receives [auto_verify: PASS · go-vet, go-test]!
	writeFile("calc.go", "package e2eav\n\nfunc Mul(a, b int) int { return a * b }\n")
	writeFile("extra.go", "package e2eav\n\nfunc Double(x int) int { return Mul(x, 2) }\n")
	t2, err := eng.Report(ctx, wk.Name, wk.PaneID, "已修复 Mul 并新增 Double")
	if err != nil {
		t.Fatal(err)
	}
	eng.tick(ctx)
	waitUntil(t, 20*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.autoVerifyReleased[t2.ID]
	}, "real git verify pass")
	eng.tick(ctx)
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(routerSeen) == 1
	}, "router delivered PASS trigger")
	waitUntil(t, 3*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.apiTurn == nil
	}, "router turn settled")

	mu.Lock()
	got := routerSeen[0]
	mu.Unlock()
	if !strings.Contains(got, "[auto_verify: PASS · go-vet, go-test]") {
		t.Fatalf("expected PASS tag in Router prompt, got %q", got)
	}
}

func TestAPIModeTriggerWordingCleanup(t *testing.T) {
	eng, st, _, _ := newTestAPIEngine(t, "http://127.0.0.1:1/v1")
	ctx := context.Background()

	// 1. buildDispatchHint
	apiHint := eng.buildDispatchHint(SayRequest{Kind: "codex", ProjectID: "corgi"})
	if strings.Contains(apiHint, "--kind") || !strings.Contains(apiHint, "kind=codex") {
		t.Fatalf("expected API key=value dispatch hint, got %q", apiHint)
	}

	// 2. formatReportArtifactNote
	apiArt := eng.formatReportArtifactNote("art-1", "codex-1")
	if strings.Contains(apiArt, "`corgi reply`") || !strings.Contains(apiArt, `reply_user(purpose="result")`) {
		t.Fatalf("expected API reply_user artifact note, got %q", apiArt)
	}

	// 3. formatWorkerWorkItemContext
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "测试标题", "下一步", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetWorkItemTitleState(ctx, wi.ID, "draft")
	wk := &model.Worker{Name: "codex-w1", Thread: model.MainThread, WorkItemID: wi.ID, FaultError: "401 unauthorized", FaultPaneID: "p1"}
	apiWI := eng.formatWorkerWorkItemContext(ctx, wk)
	if strings.Contains(apiWI, "task set") || !strings.Contains(apiWI, `manage_work(action="update_work"`) {
		t.Fatalf("expected API manage_work work_item hint, got %q", apiWI)
	}

	// 4. rerouteFaultedWorkerTriggers
	tr, _ := eng.IngestWithMeta(ctx, "router", model.MainThread, model.RouterName, "hi", "", wk.Name, wk.Name, TriggerMeta{WorkItemID: wi.ID})
	eng.rerouteFaultedWorkerTriggers(ctx, wk, []*model.Trigger{tr})
	queued, _ := st.ListQueuedTriggers(ctx)
	foundFault := false
	for _, q := range queued {
		if strings.Contains(q.Text, "PAUSED due to prior fatal error") {
			foundFault = true
			if strings.Contains(q.Text, "stop "+wk.Name+"`") || !strings.Contains(q.Text, `manage_work(action="stop_worker"`) {
				t.Fatalf("expected API manage_work fault wording, got %q", q.Text)
			}
		}
	}
	if !foundFault {
		t.Fatal("expected fault trigger to be ingested")
	}

	// 5. Switch to PTY mode and confirm legacy CLI hints are preserved 100%.
	cfg := eng.Settings()
	cfg.RouterMode = "pty"
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	if ptyHint := eng.buildDispatchHint(SayRequest{Kind: "codex"}); !strings.Contains(ptyHint, "--kind codex") {
		t.Fatalf("expected PTY CLI flag hint preserved, got %q", ptyHint)
	}
	if ptyArt := eng.formatReportArtifactNote("art-1", "codex-1"); !strings.Contains(ptyArt, "`corgi reply`") {
		t.Fatalf("expected PTY CLI artifact note preserved, got %q", ptyArt)
	}
	if ptyWI := eng.formatWorkerWorkItemContext(ctx, wk); !strings.Contains(ptyWI, "task set") {
		t.Fatalf("expected PTY CLI work_item hint preserved, got %q", ptyWI)
	}
}

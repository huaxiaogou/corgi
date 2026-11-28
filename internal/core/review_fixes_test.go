package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestAPIRouterBudgetExemptsClosingTools(t *testing.T) {
	cases := []struct {
		name         string
		optional     bool
		closingTool  string
		closingArgs  string
		wantReplies  int
		wantEndState string
	}{
		{
			name:        "reply_user succeeds after 12-call budget reached",
			optional:    false,
			closingTool: "reply_user",
			closingArgs: `{"text":"已完成排查并收敛结论。"}`,
			wantReplies: 1,
		},
		{
			name:         "no_action succeeds on optional_notify after 12-call budget reached",
			optional:     true,
			closingTool:  "no_action",
			closingArgs:  `{"reason":"巡检核对无新增需打扰事项"}`,
			wantReplies:  0,
			wantEndState: "silent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				step        int
				sawBudgetOp bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req LLMChatRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				step++
				cur := step
				for _, m := range req.Messages {
					if s, ok := m.Content.(string); ok && strings.Contains(s, "工具调用次数已达上限") {
						sawBudgetOp = true
					}
				}
				mu.Unlock()

				makeReads := func(prefix string, n int) []map[string]any {
					out := make([]map[string]any, 0, n)
					for i := 0; i < n; i++ {
						out = append(out, map[string]any{
							"id":   fmt.Sprintf("%s_%d", prefix, i),
							"type": "function",
							"function": map[string]any{
								"name":      "read_detail",
								"arguments": `{"kind":"note","target":"missing"}`,
							},
						})
					}
					return out
				}

				switch cur {
				case 1:
					// Emit 13 read_detail calls in step 1: 12 consume budget, 13th hits budget cap.
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "cmpl-b1", "model": "hy4",
						"choices": []map[string]any{{
							"index": 0, "finish_reason": "tool_calls",
							"message": map[string]any{"role": "assistant", "tool_calls": makeReads("c1", maxAPIRouterToolCalls+1)},
						}},
					})
				default:
					// Step 2: model obeys budget cap and calls closing tool (call #14 > 12).
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "cmpl-b2", "model": "hy4",
						"choices": []map[string]any{{
							"index": 0, "finish_reason": "tool_calls",
							"message": map[string]any{
								"role": "assistant",
								"tool_calls": []map[string]any{{
									"id": "c_close", "type": "function",
									"function": map[string]any{"name": tc.closingTool, "arguments": tc.closingArgs},
								}},
							},
						}},
					})
				}
			}))
			defer srv.Close()

			eng, st, sink, _ := newTestAPIEngine(t, srv)
			ctx := context.Background()
			var (
				trig *model.Trigger
				err  error
			)
			if tc.optional {
				trig, err = eng.Ingest(ctx, "system", model.MainThread, "corgi", "[corgi idle-patrol]", "", model.RouterName, "idle-patrol")
			} else {
				_, trig, err = eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我查一下全部状态"})
			}
			if err != nil {
				t.Fatal(err)
			}

			eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
			waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "budget wrap-up settled")

			mu.Lock()
			gotSteps, gotBudgetErr := step, sawBudgetOp
			mu.Unlock()
			if gotSteps != 2 || !gotBudgetErr {
				t.Fatalf("expected 2 steps with budget limit feedback on call #13, got steps=%d sawBudget=%v", gotSteps, gotBudgetErr)
			}
			gotTrig, _ := st.GetTrigger(ctx, trig.ID)
			if gotTrig == nil || gotTrig.Status != "delivered" {
				t.Fatalf("expected trigger delivered, got %+v", gotTrig)
			}
			sink.mu.Lock()
			replies := append([]*model.Entry(nil), sink.replies...)
			sink.mu.Unlock()
			if len(replies) != tc.wantReplies {
				t.Fatalf("expected %d replies, got %d (%+v)", tc.wantReplies, len(replies), replies)
			}
			if tc.wantEndState != "" {
				ends := sink.ends()
				if len(ends) == 0 || ends[len(ends)-1].reason != tc.wantEndState {
					t.Fatalf("expected turn end %q, got %+v", tc.wantEndState, ends)
				}
			}
		})
	}
}

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@corgi.local"},
		{"config", "user.name", "Corgi Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
}

func TestAPIRouterReleaseApprovalRequiresDecisionCardAndWorktreeIsolation(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	repoDir := filepath.Join(dir, "demo-repo")
	initTestGitRepo(t, repoDir)
	cfg := eng.Settings()
	cfg.WorkerKind = "claude"
	cfg.Projects = []model.ProjectWorkspace{{ID: "demo", Name: "Demo", Paths: map[string]string{"hub": repoDir}}}
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		spawnSeq int
	)
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.create":
			spawnSeq++
			wsID := fmt.Sprintf("w-%d", spawnSeq)
			return map[string]any{
				"workspace": map[string]any{"workspace_id": wsID},
				"root_pane": map[string]any{"pane_id": wsID + ":p1"},
			}, nil
		case "agent.start":
			return map[string]any{"pane_id": fmt.Sprintf("w-%d:p1", spawnSeq), "workspace_id": fmt.Sprintf("w-%d", spawnSeq), "session": fmt.Sprintf("s-%d", spawnSeq), "pid": 3000 + spawnSeq}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	findTriggerForAction := func(actID string) *model.Trigger {
		t.Helper()
		trs, err := st.ListQueuedTriggers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range trs {
			if en, eErr := st.GetEntry(ctx, tr.Ref); eErr == nil && en != nil && en.ActionID == actID {
				return tr
			}
		}
		t.Fatalf("no trigger found for action %s", actID)
		return nil
	}

	// 1. Plain human message mentioning release/deploy MUST NOT unlock allow_release=true.
	_, plainTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "帮我合入 main 并发布部署"})
	if err != nil {
		t.Fatal(err)
	}
	plainMeta := eng.resolveTurnConsentMeta(ctx, plainTrig)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"合入并发布"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "决策卡片") {
		t.Fatalf("expected plain text allow_release rejected, got %s", out)
	}

	// 2. Custom free-text reply on a decision card MUST NOT unlock allow_release=true.
	_, customAct, _, err := eng.Reply(ctx, model.MainThread, "是否发布？", "", []string{"发布上线", "暂缓"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionCustom(ctx, customAct.ID, "web", "发布上线"); err != nil {
		t.Fatal(err)
	}
	customTrig := findTriggerForAction(customAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: customTrig.ID}, customTrig, eng.resolveTurnConsentMeta(ctx, customTrig),
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"合入并发布"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected ResolveActionCustom rejected for allow_release, got %s", out)
	}

	// 3. Lightweight router_check (check=true, no choices) MUST NOT unlock allow_release=true.
	_, checkAct, _, err := eng.Reply(ctx, model.MainThread, "构建已绿，待查阅", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionCustom(ctx, checkAct.ID, "web", "帮我发布"); err != nil {
		t.Fatal(err)
	}
	checkTrig := findTriggerForAction(checkAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: checkTrig.ID}, checkTrig, eng.resolveTurnConsentMeta(ctx, checkTrig),
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"合入并发布"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected router_check rejected for allow_release, got %s", out)
	}

	// 4. Quoted plain text auto-resolving a card (chosen_key="reply") MUST NOT unlock allow_release=true.
	cardEn, quoteAct, _, err := eng.Reply(ctx, model.MainThread, "确认发版？", "", []string{"发版", "取消"}, false)
	if err != nil || quoteAct == nil {
		t.Fatalf("create quote card: %v", err)
	}
	_, quotedTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "发版吧", ReEntryID: cardEn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: quotedTrig.ID}, quotedTrig, eng.resolveTurnConsentMeta(ctx, quotedTrig),
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"合入并发布"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected quoted plain reply rejected for allow_release, got %s", out)
	}

	// 5. Ordinary human implement MUST force isolated worktree even if LLM passes worktree="off" or "auto".
	for _, reqWT := range []string{"off", "auto"} {
		_, implTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "在 demo 里修个 bug"})
		if err != nil {
			t.Fatal(err)
		}
		out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: implTrig.ID}, implTrig, eng.resolveTurnConsentMeta(ctx, implTrig),
			fmt.Sprintf(`{"mode":"implement","worktree":%q,"project":"demo","goal":"修复 demo 缺陷"}`, reqWT))
		var resp struct {
			OK     bool   `json:"ok"`
			Worker string `json:"worker"`
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil || !resp.OK || !strings.HasPrefix(resp.Branch, "corgi/") {
			t.Fatalf("worktree=%q: expected forced isolated worktree branch, got %s", reqWT, out)
		}
		wk, err := st.GetWorker(ctx, resp.Worker)
		if err != nil || !strings.Contains(wk.Constraint, "【源码实现范围】") || strings.Contains(wk.Constraint, "【已获用户发布授权】") {
			t.Fatalf("worktree=%q: unexpected worker constraint %+v err=%v", reqWT, wk, err)
		}
	}

	// 6. Real human-clicked single-select decision card choice unlocks allow_release=true & defaults worktree=off.
	_, relAct, _, err := eng.Reply(ctx, model.MainThread, "确认合入 main 并部署？", "", []string{"合入并部署", "取消"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, relAct.ID, "web", "1"); err != nil {
		t.Fatal(err)
	}
	relTrig := findTriggerForAction(relAct.ID)
	outRel := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: relTrig.ID}, relTrig, eng.resolveTurnConsentMeta(ctx, relTrig),
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"合入并发布上线"}`)
	var relResp struct {
		OK     bool   `json:"ok"`
		Worker string `json:"worker"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(outRel), &relResp); err != nil || !relResp.OK || relResp.Branch != "" {
		t.Fatalf("expected card-approved allow_release to succeed on primary checkout, got %s", outRel)
	}
	relWk, err := st.GetWorker(ctx, relResp.Worker)
	if err != nil || !strings.Contains(relWk.Constraint, "【已获用户发布授权】") {
		t.Fatalf("expected release constraint on approved worker, got %+v err=%v", relWk, err)
	}

	// 7. Real human-clicked multi-select decision card choice also unlocks allow_release=true.
	_, multiAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread, Text: "选择要执行的发布步骤", Options: []string{"合入 main", "构建部署"}, Mode: model.ActionModeMulti,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionChoices(ctx, multiAct.ID, "wecom", []string{"1", "2"}); err != nil {
		t.Fatal(err)
	}
	multiTrig := findTriggerForAction(multiAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: multiTrig.ID}, multiTrig, eng.resolveTurnConsentMeta(ctx, multiTrig),
		`{"mode":"implement","allow_release":true,"project":"demo","goal":"按多选拍板发布"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected multi-select card-approved allow_release to succeed, got %s", out)
	}
}

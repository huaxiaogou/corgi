package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
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

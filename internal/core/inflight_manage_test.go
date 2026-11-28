package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

func TestAPIRouterManageTimerAndCreateWorkInflightDedup(t *testing.T) {
	ctx := context.Background()

	// 1. Input validation rejects invalid args BEFORE persisting any inflight KV marker.
	t.Run("invalid inputs do not write inflight marker", func(t *testing.T) {
		eng, st, _, _ := newTestAPIEngine(t, nil)
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "建工作项和提醒"})
		if err != nil {
			t.Fatal(err)
		}
		turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}

		if out := eng.toolManageWork(ctx, turn, trig, `{"action":"create_work","title":"   "}`); !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected empty title rejected, got %s", out)
		}
		if out := eng.toolManageTimer(ctx, turn, trig, `{"action":"add","spec":"bad-spec","text":"提醒"}`); !strings.Contains(out, `"ok":false`) {
			t.Fatalf("expected invalid timer spec rejected, got %s", out)
		}
		if raw, ok, _ := st.GetKV(ctx, apiRouterInflightKVKey); ok && raw != "" {
			t.Fatalf("expected no inflight KV on validation failure, got %q", raw)
		}
	})

	// 2. Fail-closed: KV persistence error aborts create_work and manage_timer add before creating records.
	t.Run("kv failure aborts create_work and manage_timer add fail-closed", func(t *testing.T) {
		eng, st, _, _ := newTestAPIEngine(t, nil)
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "建工作项和提醒"})
		if err != nil {
			t.Fatal(err)
		}
		turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
		_ = st.Close()

		if out := eng.toolManageWork(ctx, turn, trig, `{"action":"create_work","title":"不应创建"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "防重标记失败") {
			t.Fatalf("expected create_work fail-closed on KV error, got %s", out)
		}
		if out := eng.toolManageTimer(ctx, turn, trig, `{"action":"add","spec":"in 30m","text":"不应创建"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "防重标记失败") {
			t.Fatalf("expected manage_timer add fail-closed on KV error, got %s", out)
		}
	})

	// 3. Real ACK failure + simulated Hub restart never replays create_work or manage_timer add.
	for _, tc := range []struct {
		name           string
		toolName       string
		toolArgs       string
		wantOp         string
		wantRecoverSub string
		verifySingle   func(t *testing.T, eng *Engine)
	}{
		{
			name:           "create_work retains inflight on ack failure and reconciles on restart without duplicate",
			toolName:       "manage_work",
			toolArgs:       `{"action":"create_work","title":"防重测试事项","next_step":"待验收"}`,
			wantOp:         "create_work",
			wantRecoverSub: "创建工作项「防重测试事项」",
			verifySingle: func(t *testing.T, eng *Engine) {
				items, err := eng.store.ListWorkItems(ctx, true)
				if err != nil || len(items) != 1 || items[0].Title != "防重测试事项" {
					t.Fatalf("expected strictly 1 work item, got %+v err=%v", items, err)
				}
			},
		},
		{
			name:           "manage_timer add retains inflight on ack failure and reconciles on restart without duplicate",
			toolName:       "manage_timer",
			toolArgs:       `{"action":"add","spec":"in 30m","text":"防重提醒内容"}`,
			wantOp:         "manage_timer_add",
			wantRecoverSub: "创建定时提醒",
			verifySingle: func(t *testing.T, eng *Engine) {
				timers, err := eng.store.ListTimers(ctx)
				if err != nil || len(timers) != 1 || timers[0].Text != "防重提醒内容" {
					t.Fatalf("expected strictly 1 timer, got %+v err=%v", timers, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				llmCalls int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				llmCalls++
				n := llmCalls
				mu.Unlock()
				if n == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "c-mut-1", "model": "hy4",
						"choices": []map[string]any{{
							"index": 0, "finish_reason": "tool_calls",
							"message": map[string]any{
								"role": "assistant",
								"tool_calls": []map[string]any{{
									"id": "call_mut", "type": "function",
									"function": map[string]any{"name": tc.toolName, "arguments": tc.toolArgs},
								}},
							},
						}},
					})
					return
				}
				// Step 2 fails (simulating mid-turn interruption/crash right after create_work / manage_timer add).
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"interrupted before reply"}}`))
			}))
			defer srv.Close()

			eng, st, _, dir := newTestAPIEngine(t, srv)
			_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我建一个"})
			if err != nil {
				t.Fatal(err)
			}
			// Simulate ACK failure (crash/DB lock right before trigger ACK).
			execSQLiteTestStmt(t, st.DBPath(), `CREATE TRIGGER fail_trig_ack BEFORE UPDATE ON triggers BEGIN SELECT RAISE(ABORT, 'ack denied'); END;`)

			eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
			waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "mutating turn settled")

			// Verify inflight marker was retained with trigger ID & reserved ID (never cleared on unacked mutation).
			raw, ok, err := st.GetKV(ctx, apiRouterInflightKVKey)
			if err != nil || !ok || !strings.Contains(raw, trig.ID) || !strings.Contains(raw, tc.wantOp) {
				t.Fatalf("expected retained inflight KV for %s/%s, got ok=%v raw=%q err=%v", trig.ID, tc.wantOp, ok, raw, err)
			}
			// Verify trigger is still queued in DB, yet next tick refuses to replay it.
			eng.tick(ctx)
			time.Sleep(50 * time.Millisecond)
			tc.verifySingle(t, eng)

			// Simulate Hub restart (fresh Engine on same SQLite DB) after SQLite recovers.
			execSQLiteTestStmt(t, st.DBPath(), `DROP TRIGGER fail_trig_ack;`)
			restarted, err := NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
			if err != nil {
				t.Fatalf("new restarted engine: %v", err)
			}
			restarted.SetLLMTransportForTest(newTestRewriteTransport(srv))

			mu.Lock()
			callsBeforeRestart := llmCalls
			mu.Unlock()

			// Startup reconcile ACKs trigger, clears KV, records preserved-item notice, and never replays LLM.
			if !restarted.reconcileAPIRouterInflight(ctx) {
				t.Fatal("expected reconcileAPIRouterInflight=true on restarted engine")
			}
			if rawAfter, okAfter, _ := st.GetKV(ctx, apiRouterInflightKVKey); okAfter && rawAfter != "" {
				t.Fatalf("expected inflight KV cleared after restart reconcile, got %q", rawAfter)
			}
			gotTrig, _ := st.GetTrigger(ctx, trig.ID)
			if gotTrig == nil || gotTrig.Status != "delivered" {
				t.Fatalf("expected trigger delivered after restart reconcile, got %+v", gotTrig)
			}
			restarted.deliverAPIRouter(ctx, []*model.Trigger{gotTrig})
			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			callsAfterRestart := llmCalls
			mu.Unlock()
			if callsAfterRestart != callsBeforeRestart {
				t.Fatalf("expected 0 replay LLM calls after restart, before=%d after=%d", callsBeforeRestart, callsAfterRestart)
			}
			tc.verifySingle(t, restarted)

			entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
			foundNotice := false
			for _, en := range entries {
				if en.Role == "event" && strings.Contains(en.Text, tc.wantRecoverSub) && strings.Contains(en.Text, "未重复创建") {
					foundNotice = true
					break
				}
			}
			if !foundNotice {
				t.Fatalf("expected restart recovery notice containing %q, entries=%+v", tc.wantRecoverSub, entries)
			}
		})
	}
}

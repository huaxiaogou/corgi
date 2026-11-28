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
)

func waitAPIRouterIdle(t *testing.T, eng *Engine, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		eng.mu.Lock()
		busy := eng.router.turn != nil
		eng.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("api router did not settle within %s", timeout)
}

func writeJSONToolResp(w http.ResponseWriter, promptTokens, cachedTokens int, calls []LLMToolCall) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":    "chatcmpl-test",
		"model": "hy4",
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":       "assistant",
				"content":    "",
				"tool_calls": calls,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": 30,
			"total_tokens":      promptTokens + 30,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": cachedTokens,
			},
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func TestAPIRouterAppendOnlyPrefixAndCacheKey(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []LLMChatRequest
		call int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode req: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		call++
		cur := call
		mu.Unlock()

		switch cur {
		case 1:
			// Turn 1 Step 1: create_work + read_detail note (tests tool trace retention + work_item tagging).
			writeJSONToolResp(w, 1200, 0, []LLMToolCall{{
				ID:   "tc_create",
				Type: "function",
				Function: LLMToolFunctionCall{
					Name:      "manage_work",
					Arguments: `{"action":"create_work","title":"登录页样式优化","next_step":"调研方案"}`,
				},
			}})
		case 2:
			// Turn 1 Step 2: reply_user
			writeJSONToolResp(w, 1500, 1100, []LLMToolCall{{
				ID:   "tc_reply1",
				Type: "function",
				Function: LLMToolFunctionCall{
					Name:      "reply_user",
					Arguments: `{"text":"已建好登录页样式优化事项并开始梳理。"}`,
				},
			}})
		case 3:
			// Turn 2: reply_user directly, observing frozen Turn 1 prefix + tool trace!
			writeJSONToolResp(w, 1800, 1450, []LLMToolCall{{
				ID:   "tc_reply2",
				Type: "function",
				Function: LLMToolFunctionCall{
					Name:      "reply_user",
					Arguments: `{"text":"好的，按你说的改用紧凑布局。"}`,
				},
			}})
		default:
			writeJSONToolResp(w, 2000, 1700, []LLMToolCall{{
				ID:   "tc_reply3",
				Type: "function",
				Function: LLMToolFunctionCall{
					Name:      "reply_user",
					Arguments: `{"text":"收到，已继续跟进。"}`,
				},
			}})
		}
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()

	_, t1, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我优化下登录页样式"})
	if err != nil {
		t.Fatalf("say 1: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{t1})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	_, t2, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "顺便用紧凑布局"})
	if err != nil {
		t.Fatalf("say 2: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{t2})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	_, t3, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "再确认下细节"})
	if err != nil {
		t.Fatalf("say 3: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{t3})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	mu.Lock()
	gotReqs := append([]LLMChatRequest(nil), reqs...)
	mu.Unlock()
	if len(gotReqs) != 4 {
		t.Fatalf("expected 4 LLM requests, got %d", len(gotReqs))
	}

	// Verify per-thread+epoch PromptCacheKey.
	wantKey := "corgi:tokenhub:hy4:v1:chat:main:e1"
	for i, req := range gotReqs {
		if req.PromptCacheKey != wantKey {
			t.Fatalf("req[%d] cache key = %q, want %q", i, req.PromptCacheKey, wantKey)
		}
	}

	// Turn 2 (req[2]) must contain frozen Turn 1 (user + assistant with tool trace) before final live user message.
	r2, r3 := gotReqs[2], gotReqs[3]
	if len(r2.Messages) < 5 {
		t.Fatalf("turn 2 expected >=5 messages (sys, mem, t1.user, t1.assistant, t2.live), got %d", len(r2.Messages))
	}
	t1User, _ := r2.Messages[2].Content.(string)
	t1Asst, _ := r2.Messages[3].Content.(string)
	if !strings.Contains(t1User, "帮我优化下登录页样式") {
		t.Fatalf("turn 1 frozen user missing prompt: %q", t1User)
	}
	if !strings.Contains(t1Asst, "[工具轨迹]") || !strings.Contains(t1Asst, "manage_work(create_work)") || !strings.Contains(t1Asst, "已建好登录页样式优化事项") {
		t.Fatalf("turn 1 frozen assistant missing tool trace or reply: %q", t1Asst)
	}

	// Turn 3 (req[3]) prefix [0..3] must be BYTE-IDENTICAL to Turn 2's [0..3] for KV Cache hits!
	for i := 0; i < 4; i++ {
		if r2.Messages[i].Role != r3.Messages[i].Role || r2.Messages[i].Content != r3.Messages[i].Content {
			t.Fatalf("KV prefix diverged at message[%d]: turn2=%+v turn3=%+v", i, r2.Messages[i], r3.Messages[i])
		}
	}

	// Verify stored usage telemetry and work_item multi-source attribution.
	info, err := eng.InspectRouterContext(ctx, model.MainThread)
	if err != nil {
		t.Fatalf("inspect context: %v", err)
	}
	if info.Summary.ActiveTurns != 3 || info.Summary.LastPromptTokens != 2000 || info.Summary.LastCachedTokens != 1700 {
		t.Fatalf("unexpected context summary: %+v", info.Summary)
	}
	if len(info.Turns) != 3 || info.Turns[0].WorkItemID == "" {
		t.Fatalf("expected turn 1 tagged with created work_item_id, got %+v", info.Turns)
	}
	// Verify pruning by work_item removes turn 1 while preserving lossless UI entries!
	wiID := info.Turns[0].WorkItemID
	pruneRes, err := eng.PruneRouterContext(ctx, PruneRouterContextRequest{
		Thread: model.MainThread, Mode: "work_item", WorkItemID: wiID,
	})
	if err != nil || pruneRes.DroppedTurns != 1 {
		t.Fatalf("prune work_item failed: res=%+v err=%v", pruneRes, err)
	}
	entries, _, err := st.ListEntries(ctx, model.MainThread, 50)
	if err != nil || len(entries) < 6 {
		t.Fatalf("lossless entries should be preserved after prune, got %d (err=%v)", len(entries), err)
	}
}

func TestAPIRouterNoActionZeroPollutionAndBgNotesPreserved(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []LLMChatRequest
		call int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		call++
		cur := call
		mu.Unlock()
		if cur == 1 {
			writeJSONToolResp(w, 800, 0, []LLMToolCall{{
				ID:       "tc_noact",
				Type:     "function",
				Function: LLMToolFunctionCall{Name: "no_action", Arguments: `{"reason":"巡检正常无需打扰"}`},
			}})
			return
		}
		writeJSONToolResp(w, 900, 0, []LLMToolCall{{
			ID:       "tc_reply",
			Type:     "function",
			Function: LLMToolFunctionCall{Name: "reply_user", Arguments: `{"text":"收到，已看到刚才直聊的结论。"}`},
		}})
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	eng.appendDirectBgNote("直聊 #claude-1：已确认根因在缓存键未分片")

	patrolTrig, err := eng.Ingest(ctx, "system", model.MainThread, "corgi", "定时巡检检查", "", model.RouterName, "idle-patrol")
	if err != nil {
		t.Fatalf("ingest patrol: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{patrolTrig})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	info, _ := eng.InspectRouterContext(ctx, model.MainThread)
	if info.Summary.ActiveTurns != 0 {
		t.Fatalf("no_action turn must not enter router_context_turns, got %d", info.Summary.ActiveTurns)
	}

	// Next human turn must still see the preserved directBgNote!
	_, humanTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "直聊结论是什么？"})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{humanTrig})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	mu.Lock()
	r2 := reqs[1]
	mu.Unlock()
	lastUser, _ := r2.Messages[len(r2.Messages)-1].Content.(string)
	if !strings.Contains(lastUser, "缓存键未分片") {
		t.Fatalf("directBgNote was lost after no_action turn: %q", lastUser)
	}
	if len(eng.peekDirectBgNotes()) != 0 {
		t.Fatalf("directBgNotes should be committed after successful human turn")
	}
}

func TestAPIRouterCompactionCASAndClearIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-comp",
			"model": "hy4",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": "- 开放事项：登录页优化仍在推进\n- 关键约束：保持黑白极简风格",
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()

	// Seed 8 active turns in epoch 1.
	for i := 1; i <= 8; i++ {
		id, seq, _ := st.NextID(ctx, "rt")
		_, err := st.AppendRouterContextTurn(ctx, &model.RouterContextTurn{
			ID:            id,
			Seq:           seq,
			Thread:        model.MainThread,
			Epoch:         1,
			Kind:          "turn",
			UserText:      "用户第 " + string(rune('0'+i)) + " 轮输入",
			AssistantText: "大脑第 " + string(rune('0'+i)) + " 轮回复",
			EstTokens:     5000,
			CreatedAt:     time.Now(),
		}, 0, 0)
		if err != nil {
			t.Fatalf("seed turn %d: %v", i, err)
		}
	}

	cfg, modelName, effort, _, err := eng.resolveActiveRouterAPIConfig(ctx)
	if err != nil {
		t.Fatalf("resolve cfg: %v", err)
	}
	if err := eng.runContextCompactionSync(ctx, model.MainThread, cfg, modelName, effort, 3, true); err != nil {
		t.Fatalf("run compaction: %v", err)
	}

	state, err := st.GetRouterContextState(ctx, model.MainThread)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.Epoch != 2 || state.CompactionCount != 1 {
		t.Fatalf("expected epoch=2 compaction_count=1, got %+v", state)
	}
	turns, err := st.ListActiveRouterContextTurns(ctx, model.MainThread, state.Epoch)
	if err != nil {
		t.Fatalf("list active turns: %v", err)
	}
	// Expect 1 summary turn + 3 preserved tail turns = 4.
	if len(turns) != 4 || turns[0].Kind != "summary" {
		t.Fatalf("expected 1 summary + 3 tail turns, got %d (first kind=%q)", len(turns), turns[0].Kind)
	}

	// Simulate stale compaction CAS mismatch when epoch already advanced (e.g. user cleared mid-compaction).
	committed, _, err := st.CommitRouterContextCompactionTx(ctx, model.MainThread, 1, turns[0].Seq, &model.RouterContextTurn{
		ID: "rt_stale", UserText: "stale", AssistantText: "stale", EstTokens: 100,
	})
	if err != nil || committed {
		t.Fatalf("stale epoch compaction CAS should be rejected: committed=%v err=%v", committed, err)
	}

	// Clear resets to empty epoch 3 and MUST NOT fall back to legacy entries!
	_, _, _ = eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "旧对话内容不应回流"})
	res, err := eng.PruneRouterContext(ctx, PruneRouterContextRequest{Thread: model.MainThread, Mode: "clear"})
	if err != nil || res.Summary.Epoch != 3 || res.Summary.ActiveTurns != 0 {
		t.Fatalf("clear failed: res=%+v err=%v", res, err)
	}
	epoch, msgs, _ := eng.buildThreadContextMessages(ctx, model.MainThread, &model.Trigger{ID: "t_new", Thread: model.MainThread, Source: "chat", Text: "新话题"}, &turnConsentMeta{isHuman: true})
	if epoch != 3 {
		t.Fatalf("expected epoch 3, got %d", epoch)
	}
	lastMsg, _ := msgs[len(msgs)-1].Content.(string)
	if strings.Contains(lastMsg, "旧对话内容不应回流") {
		t.Fatalf("cleared epoch must not fall back to legacy entries: %q", lastMsg)
	}
}

func TestAPIRouterOverflow400SelfHealing(t *testing.T) {
	var (
		mu   sync.Mutex
		call int
		lens []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		call++
		cur := call
		lens = append(lens, len(req.Messages))
		mu.Unlock()
		if cur == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 64000 tokens (context_length_exceeded)"}}`))
			return
		}
		if cur == 2 {
			// Compaction summary call or retried router call: if no tools, return summary.
			if len(req.Tools) == 0 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "sum-1", "model": "hy4",
					"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "摘要：保留核心状态"}}},
				})
				return
			}
		}
		writeJSONToolResp(w, 6000, 0, []LLMToolCall{{
			ID:       "tc_ok",
			Type:     "function",
			Function: LLMToolFunctionCall{Name: "reply_user", Arguments: `{"text":"已自动压缩上下文并完成答复。"}`},
		}})
	}))
	defer srv.Close()

	eng, st, sink, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		id, seq, _ := st.NextID(ctx, "rt")
		_, _ = st.AppendRouterContextTurn(ctx, &model.RouterContextTurn{
			ID: id, Seq: seq, Thread: model.MainThread, Epoch: 1, Kind: "turn",
			UserText: "旧消息", AssistantText: "旧回复", EstTokens: 12000, CreatedAt: time.Now(),
		}, 0, 0)
	}

	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我看下最新进展"})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitAPIRouterIdle(t, eng, 5*time.Second)

	sink.mu.Lock()
	replyCount := len(sink.replies)
	sink.mu.Unlock()
	if replyCount != 1 {
		t.Fatalf("expected 1 successful reply after overflow 400 recovery, got %d", replyCount)
	}
	mu.Lock()
	gotLens := append([]int(nil), lens...)
	mu.Unlock()
	if len(gotLens) < 2 || gotLens[len(gotLens)-1] >= gotLens[0] {
		t.Fatalf("expected retried request to have fewer messages than overflowed attempt: %v", gotLens)
	}
}

func TestAPIRouterManageContextGuardrailAndReopenWork(t *testing.T) {
	eng, _, _, _ := newTestAPIEngine(t, nil)
	ctx := context.Background()
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: "t_1"}

	// Non-human turn can inspect, but cannot clear/prune.
	outInspect := eng.toolManageContext(ctx, turn, &turnConsentMeta{isHuman: false}, `{"action":"inspect"}`)
	if !strings.Contains(outInspect, `"ok":true`) {
		t.Fatalf("inspect should succeed on background turn: %s", outInspect)
	}
	outClearBg := eng.toolManageContext(ctx, turn, &turnConsentMeta{isHuman: false}, `{"action":"clear"}`)
	if !strings.Contains(outClearBg, `"ok":false`) {
		t.Fatalf("clear must be rejected on background turn: %s", outClearBg)
	}
	outClearHuman := eng.toolManageContext(ctx, turn, &turnConsentMeta{isHuman: true}, `{"action":"clear"}`)
	if !strings.Contains(outClearHuman, `"ok":true`) {
		t.Fatalf("clear should succeed on human turn: %s", outClearHuman)
	}

	// Verify manage_work(reopen_work) works end-to-end.
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "待重开事项", "检查", "", "")
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	if _, err := eng.CloseWorkItem(ctx, wi.ID, "先关掉"); err != nil {
		t.Fatalf("close work item: %v", err)
	}
	reopenOut := eng.toolManageWork(ctx, turn, &model.Trigger{ID: "t_1"}, `{"action":"reopen_work","target":"`+wi.ID+`"}`)
	if !strings.Contains(reopenOut, `"ok":true`) {
		t.Fatalf("reopen_work failed: %s", reopenOut)
	}
}

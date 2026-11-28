package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestSelectNextAPIRouterTriggerHumanPriority(t *testing.T) {
	cases := []struct {
		name   string
		list   []*model.Trigger
		wantID string
	}{
		{
			name: "human beats earlier background worker/system/timer",
			list: []*model.Trigger{
				{ID: "t1", Seq: 1, Source: "system", Policy: "respond", From: "corgi"},
				{ID: "t2", Seq: 2, Source: "worker", Policy: "respond", From: "claude-1"},
				{ID: "t3", Seq: 3, Source: "chat", Policy: "respond", From: "you"},
			},
			wantID: "t3",
		},
		{
			name: "human triggers stay FIFO by Seq",
			list: []*model.Trigger{
				{ID: "t10", Seq: 10, Source: "wecom", Policy: "respond", From: "alice"},
				{ID: "t5", Seq: 5, Source: "chat", Policy: "respond", From: "you"},
			},
			wantID: "t5",
		},
		{
			name: "background triggers stay FIFO by Seq when no human queued",
			list: []*model.Trigger{
				{ID: "t8", Seq: 8, Source: "timer", Policy: "respond", From: "tm1"},
				{ID: "t4", Seq: 4, Source: "worker", Policy: "respond", From: "claude-1"},
			},
			wantID: "t4",
		},
		{
			name: "machine-origin chat trigger is not promoted as human",
			list: []*model.Trigger{
				{ID: "t1", Seq: 1, Source: "worker", Policy: "respond", From: "claude-1"},
				{ID: "t2", Seq: 2, Source: "chat", Policy: "respond", From: "machine"},
			},
			wantID: "t1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectNextAPIRouterTrigger(tc.list)
			if got == nil || got.ID != tc.wantID {
				t.Fatalf("got %+v, want %q", got, tc.wantID)
			}
		})
	}
}

func TestRunLLMToolLoopTransientRetryAndReasoningFallback(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream gateway timeout"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"cmpl-1","model":"hy4","choices":[{"index":0,"delta":{"reasoning":"先核对一下状态"}}]}`,
			`data: {"id":"cmpl-1","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"tc1","type":"function","function":{"name":"reply_user","arguments":"{\"text\":\"重试后成功\"}"}}]}}]}`,
			`data: {"id":"cmpl-1","model":"hy4","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":1234,"completion_tokens":45,"total_tokens":1279,"prompt_tokens_details":{"cached_tokens":900}}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	var execCount int
	res := eng.runLLMToolLoop(context.Background(), llmToolLoopConfig{
		ClientCfg:          LLMClientConfig{Provider: RouterAPIProviderTokenHub, BaseURL: TokenHubDefaultBaseURL, APIKey: "sk-test"},
		Model:              "hy4",
		Messages:           []LLMMessage{{Role: "user", Content: "hi"}},
		MaxSteps:           2,
		StepRetryBaseDelay: 5 * time.Millisecond,
		MaxStepRetries:     func(int) int { return 2 },
		ExecTool: func(_ int, tc LLMToolCall) (string, bool) {
			execCount++
			return `{"ok":true}`, true
		},
	})
	if res.Err != nil || !res.EarlyExit || execCount != 1 {
		t.Fatalf("expected retry recovery + early exit, got res=%+v execCount=%d", res, execCount)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts (1 transient 502 + 1 success), got %d", attempts.Load())
	}
	if res.LastUsage.PromptTokens != 1234 || res.LastUsage.PromptTokensDetails == nil || res.LastUsage.PromptTokensDetails.CachedTokens != 900 {
		t.Fatalf("expected LastUsage propagated, got %+v", res.LastUsage)
	}

	// Non-retryable 401 must NOT retry.
	var authAttempts atomic.Int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authAttempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer authSrv.Close()
	eng.SetLLMTransportForTest(newTestRewriteTransport(authSrv))
	resAuth := eng.runLLMToolLoop(context.Background(), llmToolLoopConfig{
		ClientCfg:          LLMClientConfig{Provider: RouterAPIProviderTokenHub, BaseURL: TokenHubDefaultBaseURL, APIKey: "sk-bad"},
		Model:              "hy4",
		Messages:           []LLMMessage{{Role: "user", Content: "hi"}},
		MaxSteps:           2,
		StepRetryBaseDelay: 5 * time.Millisecond,
		MaxStepRetries:     func(int) int { return 2 },
	})
	if resAuth.Err == nil || authAttempts.Load() != 1 {
		t.Fatalf("401 must fail immediately without retry: attempts=%d err=%v", authAttempts.Load(), resAuth.Err)
	}
}

func TestSayOnCommittedRunsBeforeWakeAndRouterLoopDeliversFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONToolResp(w, 500, 0, []LLMToolCall{{
			ID:       "tc_fast",
			Type:     "function",
			Function: LLMToolFunctionCall{Name: "reply_user", Arguments: `{"text":"毫秒直达收到"}`},
		}})
	}))
	defer srv.Close()

	eng, _, sink, _ := newTestAPIEngine(t, srv)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.runAPIRouterLoop(runCtx)

	var committedTrigID atomic.Value
	_, trig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "测试快速唤醒",
		OnCommitted: func(_ *model.Entry, tr *model.Trigger) {
			if tr != nil {
				committedTrigID.Store(tr.ID)
			}
		},
	})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	if got, _ := committedTrigID.Load().(string); got != trig.ID {
		t.Fatalf("OnCommitted trigger id = %q, want %q", got, trig.ID)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.replies)
		sink.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("independent runAPIRouterLoop did not deliver reply promptly")
}

func TestDirectBgNotesPreservedOnFailureThenCommittedOnSuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ok", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []LLMToolCall{{
						ID:       "tc_ok",
						Type:     "function",
						Function: LLMToolFunctionCall{Name: "reply_user", Arguments: `{"text":"成功读取直聊背景"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	eng.appendDirectBgNote("直聊 #codex-2：已定位内存泄漏")

	// First turn fails with 503 -> directBgNote MUST still be present!
	_, trig, _ := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "怎么样了"})
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitAPIRouterIdle(t, eng, 8*time.Second)

	if notes := eng.peekDirectBgNotes(); len(notes) != 1 {
		t.Fatalf("expected directBgNotes preserved after failed turn, got %v", notes)
	}
}

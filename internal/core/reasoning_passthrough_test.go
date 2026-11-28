package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestDeepSeekReasoningContentPassthroughInMultiStepTools(t *testing.T) {
	cases := []struct {
		name            string
		stream          bool
		reasoningChunks []string
		wantReasoning   string
		wantKeyPresent  bool
	}{
		{
			name:            "sse streaming accumulates and echoes reasoning_content across tool steps",
			stream:          true,
			reasoningChunks: []string{"第一步先读笔记，", "确认内容后再回复用户。"},
			wantReasoning:   "第一步先读笔记，确认内容后再回复用户。",
			wantKeyPresent:  true,
		},
		{
			name:            "non-streaming json echoes reasoning_content across tool steps",
			stream:          false,
			reasoningChunks: []string{"非流式思考过程"},
			wantReasoning:   "非流式思考过程",
			wantKeyPresent:  true,
		},
		{
			name:            "empty reasoning_content is omitted from assistant wire json",
			stream:          false,
			reasoningChunks: nil,
			wantReasoning:   "",
			wantKeyPresent:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu            sync.Mutex
				step          int
				step2RawBody  string
				step2Messages []map[string]any
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				step++
				cur := step
				if cur == 2 {
					step2RawBody = string(raw)
					var parsed struct {
						Messages []map[string]any `json:"messages"`
					}
					_ = json.Unmarshal(raw, &parsed)
					step2Messages = parsed.Messages
				}
				mu.Unlock()

				if cur == 1 {
					if tc.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, chunk := range tc.reasoningChunks {
							b, _ := json.Marshal(map[string]any{
								"id": "cmpl-ds-1", "model": "deepseek-reasoner",
								"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": chunk}}},
							})
							_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
						}
						_, _ = w.Write([]byte(`data: {"id":"cmpl-ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ds_1","type":"function","function":{"name":"read_detail","arguments":"{\"kind\":\"note\",\"target\":\"demo\"}"}}]}}]}` + "\n\n"))
						_, _ = w.Write([]byte(`data: {"id":"cmpl-ds-1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						return
					}
					msg := map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_ds_1", "type": "function",
							"function": map[string]any{"name": "read_detail", "arguments": `{"kind":"note","target":"demo"}`},
						}},
					}
					if len(tc.reasoningChunks) > 0 {
						msg["reasoning_content"] = strings.Join(tc.reasoningChunks, "")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "cmpl-ds-1", "model": "deepseek-reasoner",
						"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": msg}},
					})
					return
				}

				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-ds-2", "model": "deepseek-reasoner",
					"choices": []map[string]any{{
						"index": 0, "finish_reason": "tool_calls",
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id": "call_ds_2", "type": "function",
								"function": map[string]any{"name": "reply_user", "arguments": `{"text":"已核对笔记内容。"}`},
							}},
						},
					}},
				})
			}))
			defer srv.Close()

			eng, _, sink, _ := newTestAPIEngine(t, srv)
			ctx := context.Background()
			if err := eng.memory.SaveFile("notes/demo.md", "demo body"); err != nil {
				t.Fatal(err)
			}
			_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "帮我看下 demo 笔记"})
			if err != nil {
				t.Fatal(err)
			}
			eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
			waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "deepseek reasoning turn settled")

			mu.Lock()
			msgs := step2Messages
			rawBody := step2RawBody
			mu.Unlock()
			var assistantMsg map[string]any
			for _, m := range msgs {
				if m["role"] == "assistant" {
					assistantMsg = m
					break
				}
			}
			if assistantMsg == nil {
				t.Fatalf("expected assistant message in step 2 request, raw=%s", rawBody)
			}
			gotVal, exists := assistantMsg["reasoning_content"]
			if exists != tc.wantKeyPresent {
				t.Fatalf("reasoning_content key presence=%v, want %v (msg=%+v)", exists, tc.wantKeyPresent, assistantMsg)
			}
			if tc.wantKeyPresent {
				if gotStr, _ := gotVal.(string); gotStr != tc.wantReasoning {
					t.Fatalf("reasoning_content = %q, want %q", gotStr, tc.wantReasoning)
				}
			}
			sink.mu.Lock()
			repN := len(sink.replies)
			sink.mu.Unlock()
			if repN != 1 {
				t.Fatalf("expected 1 reply after step 2, got %d", repN)
			}
		})
	}
}

func TestDeepSeekReasoningContentPassthroughInAPIDream(t *testing.T) {
	var (
		mu              sync.Mutex
		step            int
		gotDreamReason  string
		sawAssistantMsg bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		step++
		cur := step
		if cur == 2 {
			for _, m := range req.Messages {
				if m.Role == "assistant" {
					sawAssistantMsg = true
					gotDreamReason = m.ReasoningContent
				}
			}
		}
		mu.Unlock()

		if cur == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"d1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"先检查 instructions.md"}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"id":"d1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"dc1","type":"function","function":{"name":"read_memory","arguments":"{\"path\":\"instructions.md\"}"}}]}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"id":"d1","model":"deepseek-reasoner","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "d2", "model": "deepseek-reasoner",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "dc2", "type": "function",
						"function": map[string]any{"name": "finish_dream", "arguments": `{"summary":"完成记忆复盘","journal":"- ok"}`},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	if err := eng.StartDream(ctx, "manual"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.dream != nil && eng.dream.apiDone
	}, "dream finished")
	eng.tick(ctx)

	mu.Lock()
	okMsg, reason := sawAssistantMsg, gotDreamReason
	mu.Unlock()
	if !okMsg || reason != "先检查 instructions.md" {
		t.Fatalf("expected dream step 2 assistant reasoning_content echoed, ok=%v got=%q", okMsg, reason)
	}
}

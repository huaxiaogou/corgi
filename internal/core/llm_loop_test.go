package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRunLLMToolLoopToolChoiceFallbackBudgetAndFinalNudge(t *testing.T) {
	t.Parallel()
	var (
		mu              sync.Mutex
		reqStep         int
		sawToolChoice   []any
		sawFinalNudge   bool
		sawBudgetErrMsg bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqStep++
		cur := reqStep
		sawToolChoice = append(sawToolChoice, req.ToolChoice)
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok {
				if strings.Contains(s, "FINAL_NUDGE_MARKER") {
					sawFinalNudge = true
				}
				if strings.Contains(s, "BUDGET_EXCEEDED_MARKER") {
					sawBudgetErrMsg = true
				}
			}
		}
		mu.Unlock()

		// Reject tool_choice=required once to verify automatic fallback.
		if req.ToolChoice != nil {
			http.Error(w, "tool_choice unsupported", http.StatusBadRequest)
			return
		}
		switch cur {
		case 1:
			// Bare text without tools -> triggers bare-text nudge + forceTools.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "s1", "model": "hy4",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "plain text"}}},
			})
		case 3:
			// Fallback retry of step 2: invoke 2 non-closing tools (2nd exceeds budget=1).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "s2", "model": "hy4",
				"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "c1", "type": "function", "function": map[string]any{"name": "probe", "arguments": `{}`}},
						{"id": "c2", "type": "function", "function": map[string]any{"name": "probe", "arguments": `{}`}},
					},
				}}},
			})
		default:
			// Step 3 (final step): closing tool succeeds.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "s3", "model": "hy4",
				"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "c3", "type": "function", "function": map[string]any{"name": "finish", "arguments": `{}`}},
					},
				}}},
			})
		}
	}))
	defer srv.Close()

	eng := &Engine{llm: newTestLLMClient(srv)}
	res := eng.runLLMToolLoop(context.Background(), llmToolLoopConfig{
		ClientCfg:          LLMClientConfig{Provider: RouterAPIProviderTokenHub, BaseURL: TokenHubDefaultBaseURL, APIKey: "sk-test"},
		Model:              "hy4",
		Tools:              []LLMToolDef{toolSpec("probe", "probe", nil), toolSpec("finish", "finish", nil)},
		Messages:           []LLMMessage{{Role: "user", Content: "go"}},
		MaxSteps:           3,
		MaxToolCalls:       1,
		MaxBareTextRetries: 1,
		BareTextForceTools: true,
		BareTextNudge:      func(int) string { return "CALL_A_TOOL" },
		FinalStepNudge:     func() (string, bool) { return "FINAL_NUDGE_MARKER", true },
		IsClosingTool:      func(name string) bool { return name == "finish" },
		ToolBudgetErrMsg:   "BUDGET_EXCEEDED_MARKER",
		ExecTool: func(_ int, tc LLMToolCall) (string, bool) {
			if tc.Function.Name == "finish" {
				return `{"ok":true}`, true
			}
			return `{"ok":true}`, false
		},
	})
	if res.Err != nil || !res.EarlyExit {
		t.Fatalf("expected EarlyExit without err, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawBudgetErrMsg || !sawFinalNudge {
		t.Fatalf("expected budget err (%v) and final nudge (%v)", sawBudgetErrMsg, sawFinalNudge)
	}
	if len(sawToolChoice) < 4 || sawToolChoice[1] != "required" || sawToolChoice[2] != nil || sawToolChoice[3] != nil {
		t.Fatalf("expected tool_choice fallback to stay disabled after 400, got %v", sawToolChoice)
	}
}

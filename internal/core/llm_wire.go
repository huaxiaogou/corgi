package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// OpenAI-compatible Chat Completions wire types (Role="system", never "developer", for TokenHub).
type (
	LLMImageURL struct {
		URL string `json:"url"`
	}
	LLMContentPart struct {
		Type     string       `json:"type"` // text | image_url
		Text     string       `json:"text,omitempty"`
		ImageURL *LLMImageURL `json:"image_url,omitempty"`
	}
	LLMToolFunctionCall struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	LLMToolCall struct {
		ID       string              `json:"id"`
		Type     string              `json:"type"` // "function"
		Function LLMToolFunctionCall `json:"function"`
	}
	LLMMessage struct {
		Role             string        `json:"role"` // system | user | assistant | tool
		Content          any           `json:"content,omitempty"`
		ReasoningContent string        `json:"reasoning_content,omitempty"`
		ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string        `json:"tool_call_id,omitempty"`
		Name             string        `json:"name,omitempty"`
	}
	LLMToolFunctionSpec struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	LLMToolDef struct {
		Type     string              `json:"type"` // "function"
		Function LLMToolFunctionSpec `json:"function"`
	}
	LLMStreamOptions struct {
		IncludeUsage bool `json:"include_usage,omitempty"`
	}
	LLMChatRequest struct {
		Model           string            `json:"model"`
		Messages        []LLMMessage      `json:"messages"`
		Tools           []LLMToolDef      `json:"tools,omitempty"`
		ToolChoice      any               `json:"tool_choice,omitempty"`
		MaxTokens       int               `json:"max_tokens,omitempty"`
		ReasoningEffort string            `json:"reasoning_effort,omitempty"`
		PromptCacheKey  string            `json:"prompt_cache_key,omitempty"`
		Stream          bool              `json:"stream,omitempty"`
		StreamOptions   *LLMStreamOptions `json:"stream_options,omitempty"`
	}
	LLMUsage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details,omitempty"`
	}
	LLMChatChoice struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string        `json:"role"`
			Content          string        `json:"content"`
			ReasoningContent string        `json:"reasoning_content,omitempty"`
			Reasoning        string        `json:"reasoning,omitempty"`
			ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	}
	LLMChatResponse struct {
		ID        string          `json:"id"`
		Model     string          `json:"model"`
		Choices   []LLMChatChoice `json:"choices"`
		Usage     LLMUsage        `json:"usage"`
		RequestID string          `json:"-"`
		Latency   time.Duration   `json:"-"`
		TTFT      time.Duration   `json:"-"`
	}
	LLMClientConfig struct {
		Provider          string
		BaseURL           string
		APIKey            string
		StreamIdleTimeout time.Duration // optional per-chunk idle timeout when Stream=true (default 60s)
	}
)

// doChatCompletionAttempt executes a single HTTP attempt without inner retry loops.
// Returns disableStream=true only when a gateway rejects stream/stream_options with HTTP 400.
func doChatCompletionAttempt(
	ctx context.Context, client *http.Client, endpoint, apiKey string,
	payload []byte, useStream bool, idleTimeout time.Duration,
) (*LLMChatResponse, bool, error) {
	start := time.Now()
	reqCtx := ctx
	var (
		attemptCancel context.CancelFunc
		idleTimer     *time.Timer
		stalled       atomic.Bool
	)
	if useStream && idleTimeout > 0 {
		reqCtx, attemptCancel = context.WithCancel(ctx)
		idleTimer = time.AfterFunc(idleTimeout, func() { stalled.Store(true); attemptCancel() })
		defer func() { idleTimer.Stop(); attemptCancel() }()
	} else if !useStream {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			reqCtx, attemptCancel = context.WithTimeout(ctx, 10*time.Minute)
			defer attemptCancel()
		}
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("build llm request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	if useStream {
		httpReq.Header.Set("Accept", "text/event-stream, application/json")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	httpReq = attachEffectiveProxyToReq(httpReq)
	resp, err := client.Do(httpReq)
	if err != nil {
		if stalled.Load() && ctx.Err() == nil {
			return nil, false, fmt.Errorf("上游响应超时（%s 无新输出）", idleTimeout.Round(time.Second))
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("llm network error: %w", err)
	}
	defer resp.Body.Close()
	reqID := sanitizeRequestID(orStr(resp.Header.Get("X-Request-Id"), resp.Header.Get("X-Trace-Id")))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil && len(rawBody) == 0 {
			return nil, false, fmt.Errorf("read llm error response: %w", readErr)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, false, fmt.Errorf("%w: 上游返回重定向 HTTP %d（已禁用跳转）", ErrLLMBadRequest, resp.StatusCode)
		}
		errMsg := extractSanitizedProviderError(rawBody, apiKey)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, false, fmt.Errorf("%w: HTTP %d (%s)", ErrLLMAuth, resp.StatusCode, errMsg)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return nil, false, fmt.Errorf("上游服务暂不可用 HTTP %d (%s)", resp.StatusCode, errMsg)
		case resp.StatusCode == http.StatusBadRequest && useStream && (strings.Contains(strings.ToLower(errMsg), "stream_options") || strings.Contains(strings.ToLower(errMsg), "stream")):
			return nil, true, fmt.Errorf("provider rejected stream options: %s", errMsg)
		default:
			return nil, false, fmt.Errorf("%w: HTTP %d (%s)", ErrLLMBadRequest, resp.StatusCode, errMsg)
		}
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		onActivity := func() {
			if idleTimer != nil {
				idleTimer.Reset(idleTimeout)
			}
		}
		onActivity()
		streamed, sErr := parseLLMStreamResponse(resp.Body, apiKey, start, onActivity)
		if sErr != nil {
			if stalled.Load() && ctx.Err() == nil {
				return nil, false, fmt.Errorf("上游流式输出卡顿（%s 无新分片）", idleTimeout.Round(time.Second))
			}
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, sErr
		}
		streamed.RequestID = reqID
		return streamed, false, nil
	}

	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	latency := time.Since(start)
	if readErr != nil {
		if stalled.Load() && ctx.Err() == nil {
			return nil, false, fmt.Errorf("上游响应超时（%s 无新输出）", idleTimeout.Round(time.Second))
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("read llm response: %w", readErr)
	}
	var parsed LLMChatResponse
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return nil, false, fmt.Errorf("decode llm response: %w", err)
	}
	parsed.RequestID, parsed.Latency, parsed.TTFT = reqID, latency, latency
	if len(parsed.Choices) == 0 {
		return nil, false, fmt.Errorf("llm returned empty choices")
	}
	for i := range parsed.Choices {
		if parsed.Choices[i].Message.ReasoningContent == "" && parsed.Choices[i].Message.Reasoning != "" {
			parsed.Choices[i].Message.ReasoningContent = parsed.Choices[i].Message.Reasoning
		}
	}
	return &parsed, false, nil
}

func parseLLMStreamResponse(body io.Reader, apiKey string, start time.Time, onActivity func()) (*LLMChatResponse, error) {
	reader := bufio.NewReaderSize(io.LimitReader(body, 16<<20), 64<<10)
	type toolAcc struct {
		id, typ    string
		name, args strings.Builder
	}
	type deltaTC struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	type streamChunk struct {
		ID    string    `json:"id"`
		Model string    `json:"model"`
		Usage *LLMUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content          string    `json:"content"`
				ReasoningContent string    `json:"reasoning_content"`
				Reasoning        string    `json:"reasoning"`
				ToolCalls        []deltaTC `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}

	var (
		out                      LLMChatResponse
		contentBuf, reasoningBuf strings.Builder
		finishReason             string
		sawDone                  bool
		sawAnyChunk              bool
		tcByIdx                  = map[int]*toolAcc{}
		lastIdx                  = -1
	)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 && onActivity != nil {
			onActivity()
		}
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			var chunk streamChunk
			if payload != "" && json.Unmarshal([]byte(payload), &chunk) == nil {
				if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
					return nil, fmt.Errorf("上游流式返回错误：%s", extractSanitizedProviderError([]byte(payload), apiKey))
				}
				if !sawAnyChunk {
					sawAnyChunk = true
					out.TTFT = time.Since(start)
				}
				out.ID = orStr(chunk.ID, out.ID)
				out.Model = orStr(chunk.Model, out.Model)
				if chunk.Usage != nil {
					out.Usage = *chunk.Usage
				}
				for _, ch := range chunk.Choices {
					if ch.Index != 0 {
						continue
					}
					finishReason = orStr(ch.FinishReason, finishReason)
					contentBuf.WriteString(ch.Delta.Content)
					reasoningBuf.WriteString(orStr(ch.Delta.ReasoningContent, ch.Delta.Reasoning))
					for _, dtc := range ch.Delta.ToolCalls {
						idx := lastIdx
						if dtc.Index != nil {
							idx = *dtc.Index
						} else if dtc.ID != "" || idx < 0 {
							idx = len(tcByIdx)
						}
						lastIdx = idx
						acc := tcByIdx[idx]
						if acc == nil {
							acc = &toolAcc{typ: "function"}
							tcByIdx[idx] = acc
						}
						acc.id = orStr(dtc.ID, acc.id)
						acc.typ = orStr(dtc.Type, acc.typ)
						acc.name.WriteString(dtc.Function.Name)
						acc.args.WriteString(dtc.Function.Arguments)
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read llm stream: %w", err)
		}
	}
	if !sawAnyChunk {
		return nil, fmt.Errorf("llm stream returned no data")
	}
	if !sawDone && finishReason == "" {
		return nil, fmt.Errorf("llm stream interrupted before completion")
	}

	var choice LLMChatChoice
	choice.FinishReason = finishReason
	choice.Message.Role = "assistant"
	choice.Message.Content = contentBuf.String()
	choice.Message.ReasoningContent = reasoningBuf.String()
	idxs := make([]int, 0, len(tcByIdx))
	for idx := range tcByIdx {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		acc := tcByIdx[idx]
		choice.Message.ToolCalls = append(choice.Message.ToolCalls, LLMToolCall{
			ID: acc.id, Type: acc.typ,
			Function: LLMToolFunctionCall{Name: acc.name.String(), Arguments: acc.args.String()},
		})
	}
	out.Choices = []LLMChatChoice{choice}
	out.Latency = time.Since(start)
	return &out, nil
}

func (e *Engine) logLLMStep(event, idKey, idVal string, step int, modelName, effort string, resp *LLMChatResponse, calls []LLMToolCall) {
	if e.logger == nil {
		return
	}
	cached, reasoning := 0, 0
	if resp.Usage.PromptTokensDetails != nil {
		cached = resp.Usage.PromptTokensDetails.CachedTokens
	}
	if resp.Usage.CompletionTokensDetails != nil {
		reasoning = resp.Usage.CompletionTokensDetails.ReasoningTokens
	}
	names := make([]string, 0, len(calls))
	for _, tc := range calls {
		names = append(names, tc.Function.Name)
	}
	e.logger.Info(event, idKey, idVal, "step", step, "model", modelName, "effort", effort,
		"ttft_ms", resp.TTFT.Milliseconds(), "latency_ms", resp.Latency.Milliseconds(),
		"prompt_tokens", resp.Usage.PromptTokens, "cached_tokens", cached,
		"completion_tokens", resp.Usage.CompletionTokens, "reasoning_tokens", reasoning,
		"tools", strings.Join(names, ","), "request_id", resp.RequestID)
}

func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 80 {
		return raw[:80]
	}
	return raw
}

func extractSanitizedProviderError(raw []byte, secret string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &parsed) == nil {
		msg = orStr(strings.TrimSpace(parsed.Error.Message), strings.TrimSpace(parsed.Message))
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if s := strings.TrimSpace(secret); s != "" {
		msg = strings.ReplaceAll(msg, s, "[REDACTED]")
	}
	return orStr(oneLineClip(msg, 160), "empty error body")
}

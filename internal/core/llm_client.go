package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	// RouterAPIProviderTokenHub targets Tencent TokenHub's OpenAI-compatible endpoint.
	RouterAPIProviderTokenHub = "tokenhub"
	// RouterAPIProviderOpenAI targets a user-configured HTTPS OpenAI-compatible endpoint.
	RouterAPIProviderOpenAI = "openai"

	// TokenHubDefaultBaseURL is the fixed official HTTPS endpoint for Tencent TokenHub.
	TokenHubDefaultBaseURL = "https://tokenhub.tencentmaas.com/v1"
	// TokenHubDefaultModel is the default model for Tencent TokenHub Router mode.
	TokenHubDefaultModel = "hy4"
	// TokenHubDefaultEffort is the default reasoning effort for hy4.
	TokenHubDefaultEffort = "high"
)

var (
	// ErrLLMAuth signals invalid/unauthorized credentials or forbidden model access (stop retrying until settings change).
	ErrLLMAuth = errors.New("llm authentication or permission error")
	// ErrLLMBadRequest signals a non-retryable 4xx request/model configuration error.
	ErrLLMBadRequest = errors.New("llm invalid request or model")
)

// LLMImageURL encodes an image content part for OpenAI-compatible multimodal Chat Completions.
type LLMImageURL struct {
	URL string `json:"url"`
}

// LLMContentPart is one element in a multimodal message content array.
type LLMContentPart struct {
	Type     string       `json:"type"` // text | image_url
	Text     string       `json:"text,omitempty"`
	ImageURL *LLMImageURL `json:"image_url,omitempty"`
}

// LLMToolFunctionCall carries the function name and JSON arguments inside an assistant tool call.
type LLMToolFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LLMToolCall represents one tool invocation emitted by the model.
type LLMToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"` // "function"
	Function LLMToolFunctionCall `json:"function"`
}

// LLMMessage is one message in an OpenAI Chat Completions conversation.
// Content may be either a plain string or []LLMContentPart.
// System prompts MUST always use Role="system" (never "developer") for TokenHub compatibility.
type LLMMessage struct {
	Role       string        `json:"role"` // system | user | assistant | tool
	Content    any           `json:"content,omitempty"`
	ToolCalls  []LLMToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// LLMToolFunctionSpec declares one callable tool's name, description, and JSON Schema parameters.
type LLMToolFunctionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// LLMToolDef is one entry in the OpenAI Chat Completions tools array.
type LLMToolDef struct {
	Type     string              `json:"type"` // "function"
	Function LLMToolFunctionSpec `json:"function"`
}

// LLMChatRequest is the JSON request body sent to POST <base_url>/chat/completions.
type LLMChatRequest struct {
	Model           string       `json:"model"`
	Messages        []LLMMessage `json:"messages"`
	Tools           []LLMToolDef `json:"tools,omitempty"`
	ToolChoice      any          `json:"tool_choice,omitempty"`
	MaxTokens       int          `json:"max_tokens,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	PromptCacheKey  string       `json:"prompt_cache_key,omitempty"`
}

// LLMUsage captures token accounting (including prompt cache hits when reported).
type LLMUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// LLMChatChoice is one candidate completion returned by the provider.
type LLMChatChoice struct {
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
	Message      struct {
		Role      string        `json:"role"`
		Content   string        `json:"content"`
		ToolCalls []LLMToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
}

// LLMChatResponse is the parsed OpenAI Chat Completions response plus transport metadata.
type LLMChatResponse struct {
	ID        string          `json:"id"`
	Model     string          `json:"model"`
	Choices   []LLMChatChoice `json:"choices"`
	Usage     LLMUsage        `json:"usage"`
	RequestID string          `json:"-"`
	Latency   time.Duration   `json:"-"`
}

// LLMClientConfig configures one pure-Go OpenAI-compatible API call.
type LLMClientConfig struct {
	Provider            string
	BaseURL             string
	APIKey              string
	AllowPrivateForTest bool // test-only hook for httptest.Server on loopback
}

// NormalizeRouterAPIProvider returns "tokenhub" or "openai".
func NormalizeRouterAPIProvider(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), RouterAPIProviderOpenAI) {
		return RouterAPIProviderOpenAI
	}
	return RouterAPIProviderTokenHub
}

// NormalizeRouterAPIBaseURL validates and canonicalizes the API base URL according to provider policy.
// TokenHub always locks to its official HTTPS endpoint; custom OpenAI-compatible providers must use HTTPS.
func NormalizeRouterAPIBaseURL(provider, raw string, allowHTTPForTest bool) (string, string, error) {
	prov := NormalizeRouterAPIProvider(provider)
	if prov == RouterAPIProviderTokenHub && !allowHTTPForTest {
		u, _ := url.Parse(TokenHubDefaultBaseURL)
		return TokenHubDefaultBaseURL, strings.ToLower(u.Scheme + "://" + u.Host), nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if prov == RouterAPIProviderTokenHub {
			u, _ := url.Parse(TokenHubDefaultBaseURL)
			return TokenHubDefaultBaseURL, strings.ToLower(u.Scheme + "://" + u.Host), nil
		}
		return "", "", fmt.Errorf("请填写 OpenAI 兼容接口的 Base URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("无效的 API Base URL：%q", trimmed)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(allowHTTPForTest && scheme == "http") {
		return "", "", fmt.Errorf("API Base URL 必须使用 https:// 协议")
	}
	if u.User != nil {
		return "", "", fmt.Errorf("API Base URL 不允许包含嵌入凭据")
	}
	if !allowHTTPForTest {
		host := strings.ToLower(u.Hostname())
		if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
			return "", "", fmt.Errorf("API Base URL 不允许指向本机或内网域名")
		}
		if ip, err := netip.ParseAddr(host); err == nil && isDisallowedLLMIP(ip) {
			return "", "", fmt.Errorf("API Base URL 不允许指向私有、回环或元数据 IP 地址")
		}
	}
	cleanPath := strings.TrimRight(u.Path, "/")
	if cleanPath == "" {
		cleanPath = "/v1"
	}
	u.Path = cleanPath
	u.RawQuery = ""
	u.Fragment = ""
	origin := scheme + "://" + strings.ToLower(u.Host)
	return u.String(), origin, nil
}

// MaskAPIKey returns a fixed-prefix masked preview that never leaks the full secret.
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) >= 8 {
		return "••••••••" + key[len(key)-4:]
	}
	return "••••••••"
}

func isDisallowedLLMIP(addr netip.Addr) bool {
	ip := addr.Unmap()
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		!ip.IsGlobalUnicast()
}

func newLLMHTTPClient(endpointURL string, allowPrivateForTest bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	var allowedProxyHost string
	if u, err := url.Parse(endpointURL); err == nil {
		if pu, pErr := http.ProxyFromEnvironment(&http.Request{URL: u}); pErr == nil && pu != nil {
			allowedProxyHost = strings.ToLower(pu.Hostname())
		}
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if allowPrivateForTest {
				return dialer.DialContext(ctx, network, addr)
			}
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Allow dialing the host operator's explicitly configured HTTPS_PROXY/HTTP_PROXY.
			if allowedProxyHost != "" && strings.EqualFold(host, allowedProxyHost) {
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var allowed []netip.Addr
			for _, ip := range ips {
				if isDisallowedLLMIP(ip) {
					return nil, fmt.Errorf("blocked outbound LLM connection to disallowed IP %s", ip.String())
				}
				allowed = append(allowed, ip)
			}
			if len(allowed) == 0 {
				return nil, fmt.Errorf("no routable IP found for %s", host)
			}
			var lastErr error
			for _, ip := range allowed {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ChatCompletion executes one OpenAI-compatible Chat Completions call with bounded retries on 429/5xx.
func ChatCompletion(ctx context.Context, cfg LLMClientConfig, reqBody LLMChatRequest) (*LLMChatResponse, error) {
	baseURL, _, err := NormalizeRouterAPIBaseURL(cfg.Provider, cfg.BaseURL, cfg.AllowPrivateForTest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLLMBadRequest, err)
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%w: 未配置 API Key", ErrLLMAuth)
	}
	if strings.TrimSpace(reqBody.Model) == "" {
		return nil, fmt.Errorf("%w: 未配置模型名称", ErrLLMBadRequest)
	}
	// TokenHub compatibility: enforce "system" role, never "developer".
	for i := range reqBody.Messages {
		if reqBody.Messages[i].Role == "developer" {
			reqBody.Messages[i].Role = "system"
		}
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("encode llm request: %w", err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/chat/completions"
	client := newLLMHTTPClient(endpoint, cfg.AllowPrivateForTest)
	defer client.CloseIdleConnections()

	const maxAttempts = 3
	backoff := 600 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := time.Now()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("build llm request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("llm network error: %w", err)
			if attempt < maxAttempts {
				if !sleepContext(ctx, backoff) {
					return nil, ctx.Err()
				}
				backoff *= 2
				continue
			}
			return nil, lastErr
		}

		reqID := sanitizeRequestID(resp.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = sanitizeRequestID(resp.Header.Get("X-Trace-Id"))
		}
		rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		latency := time.Since(start)
		if readErr != nil {
			lastErr = fmt.Errorf("read llm response: %w", readErr)
			if attempt < maxAttempts && sleepContext(ctx, backoff) {
				backoff *= 2
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, fmt.Errorf("%w: 上游返回重定向 HTTP %d（已禁用跳转）", ErrLLMBadRequest, resp.StatusCode)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: HTTP %d (%s)", ErrLLMAuth, resp.StatusCode, extractSanitizedProviderError(rawBody, apiKey))
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("上游服务暂不可用 HTTP %d (%s)", resp.StatusCode, extractSanitizedProviderError(rawBody, apiKey))
			if attempt < maxAttempts {
				if !sleepContext(ctx, backoff) {
					return nil, ctx.Err()
				}
				backoff *= 2
				continue
			}
			return nil, lastErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("%w: HTTP %d (%s)", ErrLLMBadRequest, resp.StatusCode, extractSanitizedProviderError(rawBody, apiKey))
		}

		var out LLMChatResponse
		if err := json.Unmarshal(rawBody, &out); err != nil {
			return nil, fmt.Errorf("decode llm response: %w", err)
		}
		out.RequestID = reqID
		out.Latency = latency
		if len(out.Choices) == 0 {
			return nil, fmt.Errorf("llm returned empty choices")
		}
		return &out, nil
	}
	return nil, lastErr
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > 80 {
		raw = raw[:80]
	}
	return raw
}

func extractSanitizedProviderError(raw []byte, secret string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &parsed) == nil {
		msg = strings.TrimSpace(parsed.Error.Message)
		if msg == "" {
			msg = strings.TrimSpace(parsed.Message)
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if s := strings.TrimSpace(secret); s != "" {
		msg = strings.ReplaceAll(msg, s, "[REDACTED]")
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 160 {
		msg = string(r[:160]) + "…"
	}
	if msg == "" {
		return "empty error body"
	}
	return msg
}

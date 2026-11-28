package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	RouterAPIProviderTokenHub   = "tokenhub"
	RouterAPIProviderOpenAI     = "openai"
	TokenHubDefaultBaseURL      = "https://tokenhub.tencentmaas.com/v1"
	TokenHubDefaultModel        = "hy4"
	TokenHubDefaultEffort       = "high"
	defaultLLMStreamIdleTimeout = 60 * time.Second
)

var (
	ErrLLMAuth       = errors.New("llm authentication or permission error")
	ErrLLMBadRequest = errors.New("llm invalid request or model")
)

type RouterAPISecretStatus struct {
	HasKey      bool   `json:"has_router_api_key"`
	Masked      string `json:"router_api_key_masked,omitempty"`
	BoundOrigin string `json:"router_api_key_bound_origin,omitempty"`
	OriginMatch bool   `json:"router_api_key_origin_match"`
}

type TestRouterAPIRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

type TestRouterAPIResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	RequestID string `json:"request_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Error     string `json:"error,omitempty"`
}

// LLMClient pools outbound HTTPS connections with direct SSRF checks and test RoundTripper injection.
type LLMClient struct {
	mu         sync.RWMutex
	httpClient *http.Client
}

func NewLLMClient() *LLMClient {
	c := &LLMClient{}
	c.SetTransport(nil)
	return c
}
func (c *LLMClient) SetTransport(rt http.RoundTripper) {
	if rt == nil {
		rt = newDirectLLMTransport()
	}
	hc := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c.mu.Lock()
	c.httpClient = hc
	c.mu.Unlock()
}
func (c *LLMClient) CloseIdleConnections() {
	if c == nil {
		return
	}
	c.mu.RLock()
	hc := c.httpClient
	c.mu.RUnlock()
	if hc != nil {
		hc.CloseIdleConnections()
	}
}

// ChatCompletion executes a single Chat Completions call (with one non-streaming fallback on stream_options 400).
func (c *LLMClient) ChatCompletion(ctx context.Context, cfg LLMClientConfig, reqBody LLMChatRequest) (*LLMChatResponse, error) {
	baseURL, _, err := NormalizeRouterAPIBaseURL(cfg.Provider, cfg.BaseURL)
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
	msgs := append([]LLMMessage(nil), reqBody.Messages...)
	for i := range msgs {
		if msgs[i].Role == "developer" {
			msgs[i].Role = "system"
		}
	}
	reqBody.Messages = msgs
	c.mu.RLock()
	client := c.httpClient
	c.mu.RUnlock()
	endpoint, idleTimeout := strings.TrimRight(baseURL, "/")+"/chat/completions", cfg.StreamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultLLMStreamIdleTimeout
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("encode llm request: %w", err)
	}
	out, disableStream, callErr := doChatCompletionAttempt(ctx, client, endpoint, apiKey, payload, reqBody.Stream, idleTimeout)
	if callErr != nil && disableStream && reqBody.Stream && ctx.Err() == nil {
		reqBody.Stream, reqBody.StreamOptions = false, nil
		if fb, mErr := json.Marshal(reqBody); mErr == nil {
			out, _, callErr = doChatCompletionAttempt(ctx, client, endpoint, apiKey, fb, false, idleTimeout)
		}
	}
	if callErr != nil {
		return nil, callErr
	}
	return out, nil
}

func NormalizeRouterAPIProvider(raw string) string {
	return condStr(strings.EqualFold(strings.TrimSpace(raw), RouterAPIProviderOpenAI), RouterAPIProviderOpenAI, RouterAPIProviderTokenHub)
}

func NormalizeRouterAPIBaseURL(provider, raw string) (string, string, error) {
	if NormalizeRouterAPIProvider(provider) == RouterAPIProviderTokenHub {
		return TokenHubDefaultBaseURL, "https://tokenhub.tencentmaas.com", nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("请填写 OpenAI 兼容接口的 Base URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("无效的 API Base URL：%q", trimmed)
	}
	if strings.ToLower(u.Scheme) != "https" || u.User != nil {
		return "", "", fmt.Errorf("API Base URL 必须使用 https:// 协议且不允许包含嵌入凭据")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return "", "", fmt.Errorf("API Base URL 不允许指向本机或内网域名")
	}
	if ip, err := netip.ParseAddr(host); err == nil && isDisallowedLLMIP(ip) {
		return "", "", fmt.Errorf("API Base URL 不允许指向私有、回环或元数据 IP 地址")
	}
	u.Path, u.RawQuery, u.Fragment = orStr(strings.TrimRight(u.Path, "/"), "/v1"), "", ""
	return u.String(), "https://" + strings.ToLower(u.Host), nil
}

func NormalizeTokenHubEffort(modelName, rawEffort string) string {
	ef := strings.TrimSpace(rawEffort)
	switch strings.TrimSpace(modelName) {
	case "hy4":
		return condStr(ef == "high" || ef == "no_think", ef, "high")
	case "glm-5.3", "glm-5.3-flash", "kimi-k3":
		return condStr(ef == "low" || ef == "high" || ef == "max", ef, "low")
	}
	return ef
}

func normalizeRouterAPISettings(ns *model.Settings) error {
	if ns == nil {
		return nil
	}
	ns.RouterAPIProvider = NormalizeRouterAPIProvider(ns.RouterAPIProvider)
	ns.RouterAPIModel, ns.RouterAPIEffort = strings.TrimSpace(ns.RouterAPIModel), strings.TrimSpace(ns.RouterAPIEffort)
	if ns.RouterAPIProvider == RouterAPIProviderTokenHub {
		ns.RouterAPIBaseURL, ns.RouterAPIModel = TokenHubDefaultBaseURL, orStr(ns.RouterAPIModel, TokenHubDefaultModel)
		ns.RouterAPIEffort = NormalizeTokenHubEffort(ns.RouterAPIModel, ns.RouterAPIEffort)
	}
	baseURL, _, err := NormalizeRouterAPIBaseURL(ns.RouterAPIProvider, ns.RouterAPIBaseURL)
	if err != nil {
		return err
	}
	ns.RouterAPIBaseURL = baseURL
	if ns.RouterAPIModel == "" {
		return fmt.Errorf("请填写原生 API 大脑的模型名称（如 %s）", TokenHubDefaultModel)
	}
	return nil
}

func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return condStr(len(key) >= 8, "••••••••"+key[max(len(key)-4, 0):], "••••••••")
}

func isDisallowedLLMIP(addr netip.Addr) bool {
	ip := addr.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast()
}

type llmProxyAddrCtxKey struct{}

func normalizeProxyDialAddr(pu *url.URL) string {
	if pu == nil || pu.Hostname() == "" {
		return ""
	}
	port := orStr(pu.Port(), map[string]string{"https": "443", "socks5": "1080", "socks5h": "1080"}[strings.ToLower(pu.Scheme)])
	return net.JoinHostPort(strings.ToLower(pu.Hostname()), orStr(port, "80"))
}

func attachEffectiveProxyWith(req *http.Request, proxyFn func(*http.Request) (*url.URL, error)) *http.Request {
	if pu, err := proxyFn(req); err == nil && pu != nil {
		if proxyAddr := normalizeProxyDialAddr(pu); proxyAddr != "" {
			return req.WithContext(context.WithValue(req.Context(), llmProxyAddrCtxKey{}, proxyAddr))
		}
	}
	return req
}
func attachEffectiveProxyToReq(req *http.Request) *http.Request {
	return attachEffectiveProxyWith(req, http.ProxyFromEnvironment)
}

func newDirectLLMTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 16, IdleConnTimeout: 60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: 1 * time.Second, Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if proxyAddr, _ := ctx.Value(llmProxyAddrCtxKey{}).(string); proxyAddr != "" && strings.EqualFold(addr, proxyAddr) {
				return dialer.DialContext(ctx, network, addr)
			}
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if isDisallowedLLMIP(ip) {
					return nil, fmt.Errorf("blocked outbound LLM connection to disallowed IP %s", ip)
				}
			}
			var lastErr error
			for _, ip := range ips {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
			}
			return nil, errors.Join(lastErr, fmt.Errorf("no routable IP found for %s", host))
		},
	}
}

func (e *Engine) SetLLMTransportForTest(rt http.RoundTripper) {
	if e.llm != nil {
		e.llm.SetTransport(rt)
	}
}

func (e *Engine) loadStoredRouterSecret(ctx context.Context) *model.RouterAPISecret {
	if e.store != nil {
		if sec, err := e.store.LoadRouterAPISecret(ctx); err == nil && sec != nil && strings.TrimSpace(sec.APIKey) != "" {
			return sec
		}
	}
	return nil
}

func (e *Engine) RouterAPISecretInfo(ctx context.Context) RouterAPISecretStatus {
	sec := e.loadStoredRouterSecret(ctx)
	if sec == nil {
		return RouterAPISecretStatus{}
	}
	st := e.Settings()
	_, curOrigin, err := NormalizeRouterAPIBaseURL(st.RouterAPIProvider, st.RouterAPIBaseURL)
	return RouterAPISecretStatus{HasKey: true, Masked: MaskAPIKey(sec.APIKey), BoundOrigin: sec.BoundOrigin, OriginMatch: err == nil && sec.BoundOrigin != "" && sec.BoundOrigin == curOrigin}
}

func (e *Engine) hasConfiguredRouterAPIKey(ctx context.Context, st model.Settings) bool {
	sec := e.loadStoredRouterSecret(ctx)
	_, origin, err := NormalizeRouterAPIBaseURL(st.RouterAPIProvider, st.RouterAPIBaseURL)
	return sec != nil && err == nil && sec.BoundOrigin == origin
}

func (e *Engine) SetRouterAPIKey(ctx context.Context, provider, baseURL, apiKey string) (RouterAPISecretStatus, error) {
	if apiKey = strings.TrimSpace(apiKey); apiKey == "" {
		return RouterAPISecretStatus{}, fmt.Errorf("API Key 不能为空")
	}
	st := e.Settings()
	prov, rawURL := NormalizeRouterAPIProvider(orStr(provider, st.RouterAPIProvider)), strings.TrimSpace(baseURL)
	if rawURL == "" && prov == RouterAPIProviderTokenHub {
		rawURL = st.RouterAPIBaseURL
	}
	_, origin, err := NormalizeRouterAPIBaseURL(prov, rawURL)
	if err != nil {
		return RouterAPISecretStatus{}, err
	}
	if err := e.store.SaveRouterAPISecret(ctx, model.RouterAPISecret{APIKey: apiKey, BoundOrigin: origin, UpdatedAt: time.Now()}); err != nil {
		return RouterAPISecretStatus{}, err
	}
	e.mu.Lock()
	e.router.resetFailuresLocked(true)
	e.mu.Unlock()
	e.wake()
	e.notifyChanged()
	return e.RouterAPISecretInfo(ctx), nil
}

func (e *Engine) ClearRouterAPIKey(ctx context.Context) error {
	if err := e.store.DeleteRouterAPISecret(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.router.configEpoch++
	e.mu.Unlock()
	e.notifyChanged()
	return nil
}

func resolveEndpointSpec(st model.Settings, reqProv, reqURL, reqModel, reqEffort string, explicitCustom bool) (prov, baseURL, origin, modelName, effort string, err error) {
	prov, rawURL := NormalizeRouterAPIProvider(orStr(reqProv, st.RouterAPIProvider)), strings.TrimSpace(reqURL)
	if rawURL == "" && prov == RouterAPIProviderOpenAI && !explicitCustom {
		rawURL = st.RouterAPIBaseURL
	}
	if baseURL, origin, err = NormalizeRouterAPIBaseURL(prov, rawURL); err != nil {
		return
	}
	modelName, effort = orStr(strings.TrimSpace(reqModel), strings.TrimSpace(st.RouterAPIModel)), strings.TrimSpace(reqEffort)
	if prov == RouterAPIProviderTokenHub {
		modelName = orStr(modelName, TokenHubDefaultModel)
		effort = NormalizeTokenHubEffort(modelName, effort)
	}
	return
}

func (e *Engine) resolveKeyForOrigin(ctx context.Context, explicitKey, origin string) (string, error) {
	if k := strings.TrimSpace(explicitKey); k != "" {
		return k, nil
	}
	sec := e.loadStoredRouterSecret(ctx)
	if sec == nil {
		return "", fmt.Errorf("%w: 尚未配置 API Key，请在「自动化与设置 → 模型与运行」录入并保存", ErrLLMAuth)
	}
	if sec.BoundOrigin != origin {
		return "", fmt.Errorf("%w: 目标地址（%s）与已保存 Key 绑定的源（%s）不一致，请重新填写 API Key", ErrLLMAuth, origin, sec.BoundOrigin)
	}
	return sec.APIKey, nil
}

func (e *Engine) TestRouterAPI(ctx context.Context, req TestRouterAPIRequest) TestRouterAPIResult {
	prov, baseURL, origin, modelName, effort, err := resolveEndpointSpec(e.Settings(), req.Provider, req.BaseURL, req.Model, req.Effort, strings.EqualFold(strings.TrimSpace(req.Provider), RouterAPIProviderOpenAI))
	if err != nil {
		return TestRouterAPIResult{Error: err.Error()}
	}
	apiKey, err := e.resolveKeyForOrigin(ctx, req.APIKey, origin)
	if err != nil {
		return TestRouterAPIResult{Error: err.Error()}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := e.llm.ChatCompletion(probeCtx, LLMClientConfig{Provider: prov, BaseURL: baseURL, APIKey: apiKey}, LLMChatRequest{
		Model: modelName, ReasoningEffort: effort, MaxTokens: 16,
		Messages: []LLMMessage{{Role: "system", Content: "Reply with OK."}, {Role: "user", Content: "ping"}},
	})
	if err != nil {
		return TestRouterAPIResult{Error: err.Error()}
	}
	return TestRouterAPIResult{OK: true, LatencyMS: resp.Latency.Milliseconds(), RequestID: resp.RequestID, Model: orStr(resp.Model, modelName)}
}

func (e *Engine) resolveActiveRouterAPIConfig(ctx context.Context) (LLMClientConfig, string, string, string, error) {
	st := e.Settings()
	prov, baseURL, origin, modelName, eff, err := resolveEndpointSpec(st, st.RouterAPIProvider, st.RouterAPIBaseURL, st.RouterAPIModel, st.RouterAPIEffort, false)
	if err != nil {
		return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: %v", ErrLLMBadRequest, err)
	}
	if modelName == "" {
		return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: 未配置 API 大脑模型名称", ErrLLMBadRequest)
	}
	apiKey, err := e.resolveKeyForOrigin(ctx, "", origin)
	if err != nil {
		return LLMClientConfig{}, "", "", "", err
	}
	return LLMClientConfig{Provider: prov, BaseURL: baseURL, APIKey: apiKey}, modelName, eff, fmt.Sprintf("corgi:%s:%s:v1", prov, modelName), nil
}

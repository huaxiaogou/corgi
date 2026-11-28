package core

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

type recordingAPISink struct {
	NopSink
	mu         sync.Mutex
	turnEnds   []struct{ thread, triggerID, reason string }
	progress   []struct{ thread, triggerID, think string }
	replies    []*model.Entry
	stateCount int
}

func (s *recordingAPISink) OnStateChanged() {
	s.mu.Lock()
	s.stateCount++
	s.mu.Unlock()
}

func (s *recordingAPISink) OnReply(_ context.Context, entry *model.Entry, _ *model.Action, _ []AttachmentFile) ReplyDelivery {
	s.mu.Lock()
	s.replies = append(s.replies, entry)
	s.mu.Unlock()
	return ReplyDelivery{}
}

func (s *recordingAPISink) OnAPIRouterProgress(_ context.Context, thread, triggerID, think string) {
	s.mu.Lock()
	s.progress = append(s.progress, struct{ thread, triggerID, think string }{thread, triggerID, think})
	s.mu.Unlock()
}

func (s *recordingAPISink) OnAPIRouterTurnEnd(_ context.Context, thread, triggerID, reason string) {
	s.mu.Lock()
	s.turnEnds = append(s.turnEnds, struct{ thread, triggerID, reason string }{thread, triggerID, reason})
	s.mu.Unlock()
}

func (s *recordingAPISink) ends() []struct{ thread, triggerID, reason string } {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]struct{ thread, triggerID, reason string }(nil), s.turnEnds...)
}

type llmTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f llmTestRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newTestRewriteTransport(srv *httptest.Server) http.RoundTripper {
	u, err := url.Parse(srv.URL)
	if err != nil {
		panic(err)
	}
	base := srv.Client().Transport
	return llmTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		cloned := req.Clone(req.Context())
		cloned.URL.Scheme = u.Scheme
		cloned.URL.Host = u.Host
		cloned.Host = u.Host
		return base.RoundTrip(cloned)
	})
}

func newTestLLMClient(srv *httptest.Server) *LLMClient {
	c := NewLLMClient()
	if srv != nil {
		c.SetTransport(newTestRewriteTransport(srv))
	}
	return c
}

func newTestAPIEngine(t *testing.T, srv *httptest.Server) (*Engine, *store.Store, *recordingAPISink, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corgi.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if srv != nil {
		eng.SetLLMTransportForTest(newTestRewriteTransport(srv))
	}
	sink := &recordingAPISink{}
	eng.SetSink(sink)

	cfg := eng.Settings()
	cfg.RouterAPIProvider = RouterAPIProviderTokenHub
	cfg.RouterAPIBaseURL = TokenHubDefaultBaseURL
	cfg.RouterAPIModel = "hy4"
	cfg.RouterAPIEffort = "no_think"
	if err := eng.SaveConfig(context.Background(), &cfg, nil); err != nil {
		t.Fatalf("save api config: %v", err)
	}
	if _, err := eng.SetRouterAPIKey(context.Background(), RouterAPIProviderTokenHub, TokenHubDefaultBaseURL, "sk-test-tokenhub-9876"); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	return eng, st, sink, dir
}

func TestLLMURLPolicyAndOriginBinding(t *testing.T) {
	// Reject non-HTTPS, localhost, private IP, metadata IP unconditionally.
	for _, bad := range []string{
		"http://api.example.com/v1",
		"https://localhost/v1",
		"https://127.0.0.1/v1",
		"https://169.254.169.254/latest",
		"https://10.0.0.5/v1",
		"https://user:pass@api.example.com/v1",
	} {
		if _, _, err := NormalizeRouterAPIBaseURL(RouterAPIProviderOpenAI, bad); err == nil {
			t.Fatalf("expected URL %q to be rejected", bad)
		}
	}
	// TokenHub locks to fixed HTTPS origin.
	base, origin, err := NormalizeRouterAPIBaseURL(RouterAPIProviderTokenHub, "https://evil.example.com/v1")
	if err != nil || base != TokenHubDefaultBaseURL || origin != "https://tokenhub.tencentmaas.com" {
		t.Fatalf("tokenhub lock failed: base=%q origin=%q err=%v", base, origin, err)
	}

	// Redirects must be blocked.
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/steal", http.StatusFound)
	}))
	defer redirSrv.Close()
	_, err = newTestLLMClient(redirSrv).ChatCompletion(context.Background(), LLMClientConfig{
		Provider: RouterAPIProviderOpenAI,
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "sk-secret",
	}, LLMChatRequest{
		Model:    "hy4",
		Messages: []LLMMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("expected redirect error, got %v", err)
	}

	// Origin binding: changing Base URL invalidates reuse of previously saved key.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Request-Id", "req-origin-1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "model": "hy4",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "OK"}}},
		})
	}))
	defer srv.Close()

	eng, _, _, _ := newTestAPIEngine(t, srv)
	info := eng.RouterAPISecretInfo(context.Background())
	if !info.HasKey || info.Masked != "••••••••9876" || !info.OriginMatch {
		t.Fatalf("unexpected secret status: %+v", info)
	}
	res := eng.TestRouterAPI(context.Background(), TestRouterAPIRequest{
		Provider: RouterAPIProviderOpenAI,
		BaseURL:  "https://other.example.com/v1",
		Model:    "hy4",
	})
	if res.OK || !strings.Contains(res.Error, "不一致") {
		t.Fatalf("expected origin mismatch rejection, got %+v", res)
	}
	okRes := eng.TestRouterAPI(context.Background(), TestRouterAPIRequest{
		Provider: RouterAPIProviderTokenHub,
		BaseURL:  TokenHubDefaultBaseURL,
		Model:    "hy4",
		Effort:   "no_think",
	})
	if !okRes.OK || okRes.RequestID != "req-origin-1" || calls != 1 {
		t.Fatalf("expected probe success, got %+v (calls=%d)", okRes, calls)
	}
}

func TestAPIRouterTokenHubWireFormatAndGuardrailSelfCorrection(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
		seenReqs []LLMChatRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-tokenhub-9876" {
			t.Errorf("unexpected auth header: %q", got)
		}
		var body LLMChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		reqCount++
		n := reqCount
		seenReqs = append(seenReqs, body)
		mu.Unlock()

		w.Header().Set("X-Request-Id", "req-guardrail")
		switch n {
		case 1:
			// Deliberately violate guardrail: single choice card (< 2 choices)!
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_1", "type": "function",
							"function": map[string]any{
								"name":      "reply_user",
								"arguments": `{"text":"要选哪一个？","choices":["仅一个选项"]}`,
							},
						}},
					},
				}},
			})
		case 2:
			// Self-corrected to 2 choices -> should succeed and early-exit!
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c2", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_2", "type": "function",
							"function": map[string]any{
								"name":      "reply_user",
								"arguments": `{"text":"请拍板方案","choices":["方案 A","方案 B"]}`,
							},
						}},
					},
				}},
			})
		default:
			t.Errorf("expected early exit after final reply_user, got extra request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv)
	_, trig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "web",
		From:    "you",
		Text:    "帮我出两个方案让我选",
	})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	eng.deliverAPIRouter(context.Background(), []*model.Trigger{trig})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !eng.isRouterWorking(context.Background()) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if reqCount != 2 {
		t.Fatalf("expected exactly 2 requests (guardrail reject + self-correct early exit), got %d", reqCount)
	}
	first := seenReqs[0]
	if first.ReasoningEffort != "no_think" || first.PromptCacheKey == "" {
		t.Fatalf("expected reasoning_effort=no_think and prompt_cache_key, got %+v", first)
	}
	for _, m := range first.Messages {
		if m.Role == "developer" {
			t.Fatalf("found forbidden developer role in TokenHub request")
		}
	}
	if first.Messages[0].Role != "system" {
		t.Fatalf("expected system role first, got %q", first.Messages[0].Role)
	}
	// Verify trigger delivered and Action created.
	gotTrig, err := st.GetTrigger(context.Background(), trig.ID)
	if err != nil || gotTrig.Status != "delivered" {
		t.Fatalf("expected trigger delivered, got %+v err=%v", gotTrig, err)
	}
	acts, err := st.ListPendingActions(context.Background())
	if err != nil || len(acts) != 1 || len(acts[0].Choices) != 2 {
		t.Fatalf("expected 1 pending decision action with 2 choices, got %+v err=%v", acts, err)
	}
}

func TestAPIRouterMobileConsentContract(t *testing.T) {
	eng, st, _, _ := newTestAPIEngine(t, nil)

	// Create an open prototype work item.
	wi, err := eng.SetWorkItem(context.Background(), "new", model.MainThread, "会话直聊身份提醒原型", "确认原型交互", "", "")
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	if _, err := st.SetWorkItemScope(context.Background(), wi.ID, "prototype"); err != nil {
		t.Fatalf("set scope: %v", err)
	}

	// Case 1: Unquoted human prose must NOT be guessed-bound via hardcoded Chinese keywords.
	_, proseTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "这个提示放在顶部更好",
	})
	if err != nil {
		t.Fatalf("say prose: %v", err)
	}
	proseMeta := eng.resolveTurnConsentMeta(context.Background(), proseTrig)
	if proseMeta.boundWorkItem != nil {
		t.Fatalf("expected no regex/keyword auto-binding on unquoted prose, got %+v", proseMeta.boundWorkItem)
	}

	// Case 2: Non-human worker report is structurally blocked from implement and allow_release.
	workerTrig, err := eng.Ingest(context.Background(), "worker", model.MainThread, "worker-1", "测试挂了一个用例", "", model.RouterName, "worker-1")
	if err != nil {
		t.Fatalf("ingest worker trig: %v", err)
	}
	wMeta := eng.resolveTurnConsentMeta(context.Background(), workerTrig)
	resWorker := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: workerTrig.ID}, workerTrig, wMeta, `{"mode":"implement","goal":"修复失败的单测"}`)
	if !strings.Contains(resWorker, `"ok":false`) || !strings.Contains(resWorker, "后台事件只能新派 readonly") {
		t.Fatalf("expected non-human implement block, got: %s", resWorker)
	}
	resWorkerRel := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: workerTrig.ID}, workerTrig, wMeta, `{"mode":"implement","allow_release":true,"goal":"merge and deploy"}`)
	if !strings.Contains(resWorkerRel, `"ok":false`) || !strings.Contains(resWorkerRel, "后台事件只能新派 readonly") {
		t.Fatalf("expected non-human allow_release block, got: %s", resWorkerRel)
	}

	// Case 3: Human turn with arbitrary phrasing / English / card click passes allow_release cleanly (zero regex restriction).
	_, shipTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "ship it and roll out to production",
	})
	if err != nil {
		t.Fatalf("say ship: %v", err)
	}
	sMeta := eng.resolveTurnConsentMeta(context.Background(), shipTrig)
	resBadMode := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: shipTrig.ID}, shipTrig, sMeta, `{"mode":"readonly","allow_release":true,"goal":"ship it"}`)
	if !strings.Contains(resBadMode, `"ok":false`) || !strings.Contains(resBadMode, "allow_release") {
		t.Fatalf("expected allow_release rejected on readonly mode, got: %s", resBadMode)
	}
	// With mode=implement + allow_release=true on a human turn, consent gate MUST pass (reaches fleet nil check, not regex rejection).
	resShip := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: shipTrig.ID}, shipTrig, sMeta, `{"mode":"implement","allow_release":true,"goal":"合入并走完构建→部署→健康核验"}`)
	if !strings.Contains(resShip, "Fleet 暂未连接") {
		t.Fatalf("expected human allow_release to pass consent gate and hit fleet check, got: %s", resShip)
	}

	// Case 4: Fresh explicit mobile coding request passes consent check cleanly.
	_, freshTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "把登录页按钮改成蓝色并跑单测",
	})
	if err != nil {
		t.Fatalf("say fresh coding: %v", err)
	}
	fMeta := eng.resolveTurnConsentMeta(context.Background(), freshTrig)
	resFresh := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: freshTrig.ID}, freshTrig, fMeta, `{"mode":"implement","goal":"把登录页按钮改成蓝色并跑单测"}`)
	if !strings.Contains(resFresh, "Fleet 暂未连接") {
		t.Fatalf("fresh explicit mobile coding request was wrongly blocked by consent gate: %s", resFresh)
	}
}

func TestAPIRouterStopExactStreamSealAndCrashRecovery(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	eng, st, sink, _ := newTestAPIEngine(t, srv)
	_, t1, err := eng.Say(context.Background(), SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "第一条问题"})
	if err != nil {
		t.Fatalf("say t1: %v", err)
	}
	_, t2, err := eng.Say(context.Background(), SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "第二条问题"})
	if err != nil {
		t.Fatalf("say t2: %v", err)
	}

	eng.deliverAPIRouter(context.Background(), []*model.Trigger{t1, t2})
	<-started

	// Stop should cancel ONLY t1, ack t1, seal ONLY t1's stream, and leave t2 queued!
	if _, err := eng.StopRouter(context.Background(), model.MainThread); err != nil {
		t.Fatalf("stop router: %v", err)
	}
	got1, _ := st.GetTrigger(context.Background(), t1.ID)
	got2, _ := st.GetTrigger(context.Background(), t2.ID)
	if got1.Status != "delivered" {
		t.Fatalf("expected stopped t1 to be acked/delivered, got %s", got1.Status)
	}
	if got2.Status != "queued" {
		t.Fatalf("expected second queued trigger t2 to stay queued, got %s", got2.Status)
	}
	ends := sink.ends()
	if len(ends) == 0 || ends[0].triggerID != t1.ID || ends[0].reason != "cancelled" {
		t.Fatalf("expected exact cancelled seal for t1, got %+v", ends)
	}

	// Test crash recovery marker on Hub startup.
	if err := eng.recordAPIRouterInflight(context.Background(), t2, "dispatch_work", "claude-99"); err != nil {
		t.Fatalf("record inflight: %v", err)
	}
	eng.reconcileAPIRouterInflight(context.Background())
	got2After, _ := st.GetTrigger(context.Background(), t2.ID)
	if got2After.Status != "delivered" {
		t.Fatalf("expected inflight trigger t2 to be acked on startup recovery, got %s", got2After.Status)
	}
	if raw, ok, _ := st.GetKV(context.Background(), apiRouterInflightKVKey); ok && raw != "" {
		t.Fatalf("expected inflight marker cleared after recovery, got %q", raw)
	}
}

func TestAPIRouterTrustedImagesAndProtectedPaths(t *testing.T) {
	eng, st, _, dir := newTestAPIEngine(t, nil)

	// Protected DB path checks (including -wal and symlink).
	dbPath := st.DBPath()
	if !IsProtectedHubDBPath(dbPath, dbPath) || !IsProtectedHubDBPath(dbPath, dbPath+"-wal") {
		t.Fatalf("expected DB and WAL paths to be protected")
	}
	linkPath := filepath.Join(dir, "alias.bin")
	if err := os.Symlink(dbPath, linkPath); err == nil {
		if !IsProtectedHubDBPath(dbPath, linkPath) {
			t.Fatalf("expected symlink to DB to be protected")
		}
	}

	// Trusted upload image is loaded; arbitrary outside image is rejected.
	upDir := filepath.Join(dir, "uploads", "20261127")
	_ = os.MkdirAll(upDir, 0o700)
	trustedImg := filepath.Join(upDir, "ok.png")
	outsideImg := filepath.Join(dir, "outside.png")
	writeTinyPNG(t, trustedImg)
	writeTinyPNG(t, outsideImg)

	trig := &model.Trigger{
		ID:     "t99",
		Thread: model.MainThread,
		Media: []model.MediaRef{
			{Path: outsideImg, Name: "outside.png"},
			{Path: trustedImg, Name: "ok.png"},
		},
	}
	parts := eng.loadTrustedTriggerImages(context.Background(), trig, nil)
	if len(parts) != 1 || parts[0].ImageURL == nil || !strings.HasPrefix(parts[0].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("expected only the 1 trusted upload image to load, got %+v", parts)
	}

	// project_file and project_git are removed from read_detail; note reads through os.Root sandbox.
	turn := &apiRouterTurn{thread: model.MainThread}
	for _, kind := range []string{"project_file", "project_git"} {
		out := eng.toolReadDetail(context.Background(), turn, `{"kind":"`+kind+`","target":"demo:README.md"}`)
		if !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "不支持的 read_detail kind") {
			t.Fatalf("expected %s rejected as unsupported, got %s", kind, out)
		}
	}
	if err := eng.memory.SaveFile("notes/demo.md", "hello corgi note"); err != nil {
		t.Fatalf("save note: %v", err)
	}
	okRead := eng.toolReadDetail(context.Background(), turn, `{"kind":"note","target":"demo"}`)
	if !strings.Contains(okRead, "hello corgi note") {
		t.Fatalf("expected note content, got %s", okRead)
	}
	badRead := eng.toolReadDetail(context.Background(), turn, `{"kind":"note","target":"../corgi.db"}`)
	if !strings.Contains(badRead, `"ok":false`) {
		t.Fatalf("expected note traversal rejection, got %s", badRead)
	}
}

func writeTinyPNG(t *testing.T, p string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
}

func TestAPIRouterReviewRegressions(t *testing.T) {
	// 1. Provider error echoing credential must redact the secret (both HTTP error and SSE HTTP 200 error).
	secret := "sk-super-secret-echo-9999"
	redacted := extractSanitizedProviderError([]byte(`{"error":{"message":"invalid key sk-super-secret-echo-9999 rejected"}}`), secret)
	if strings.Contains(redacted, secret) || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected secret redacted, got %q", redacted)
	}
	sseErrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"invalid key " + secret + " in stream\"}}\n\n"))
	}))
	defer sseErrSrv.Close()
	_, sseErr := newTestLLMClient(sseErrSrv).ChatCompletion(context.Background(), LLMClientConfig{
		Provider: RouterAPIProviderTokenHub, BaseURL: TokenHubDefaultBaseURL, APIKey: secret,
	}, LLMChatRequest{Model: "hy4", Stream: true, Messages: []LLMMessage{{Role: "user", Content: "hi"}}})
	if sseErr == nil || strings.Contains(sseErr.Error(), secret) || !strings.Contains(sseErr.Error(), "[REDACTED]") {
		t.Fatalf("expected SSE stream error redacted, got %v", sseErr)
	}

	// 1b. Direct SSRF check must NOT exempt a destination unless the request's effective proxy resolves to it.
	directTr := newDirectLLMTransport()
	directReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	directReq = attachEffectiveProxyWith(directReq, func(*http.Request) (*url.URL, error) { return nil, nil })
	if _, dErr := directTr.DialContext(directReq.Context(), "tcp", "127.0.0.1:18080"); dErr == nil || !strings.Contains(dErr.Error(), "disallowed IP") {
		t.Fatalf("expected direct HTTPS dial to unused proxy IP blocked by SSRF, got %v", dErr)
	}
	proxyURL, _ := url.Parse("http://127.0.0.1:18080")
	proxiedReq, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	proxiedReq = attachEffectiveProxyWith(proxiedReq, func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	if got, _ := proxiedReq.Context().Value(llmProxyAddrCtxKey{}).(string); got != "127.0.0.1:18080" {
		t.Fatalf("expected effective HTTPS_PROXY addr attached to request context, got %q", got)
	}
	if _, otherErr := directTr.DialContext(proxiedReq.Context(), "tcp", "127.0.0.1:19999"); otherErr == nil || !strings.Contains(otherErr.Error(), "disallowed IP") {
		t.Fatalf("expected non-proxy loopback IP still blocked by SSRF even when proxy is set, got %v", otherErr)
	}

	// 2. Testing or saving blank custom provider URL must NOT reuse previous saved URL.
	eng, st, _, _ := newTestAPIEngine(t, nil)
	if _, err := eng.SetRouterAPIKey(context.Background(), RouterAPIProviderOpenAI, "", "sk-new"); err == nil {
		t.Fatalf("expected error saving key for custom provider with blank base URL")
	}
	testRes := eng.TestRouterAPI(context.Background(), TestRouterAPIRequest{
		Provider: RouterAPIProviderOpenAI,
		BaseURL:  "",
		APIKey:   "sk-new",
		Model:    "gpt-4o",
	})
	if testRes.OK {
		t.Fatalf("expected test failure for blank custom provider URL")
	}

	// 3. Machine-originated chat trigger must NOT pass human implement consent gate.
	_, machineTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "machine",
		From:    "machine",
		Text:    "把登录按钮改成蓝色",
	})
	if err != nil {
		t.Fatalf("say machine: %v", err)
	}
	mMeta := eng.resolveTurnConsentMeta(context.Background(), machineTrig)
	if mMeta.isHuman {
		t.Fatalf("machine trigger should never be marked human")
	}

	// 4. Router-only timer persists model.RouterName as target so subscribed workers don't intercept it.
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: machineTrig.ID}
	tmOut := eng.toolManageTimer(context.Background(), turn, machineTrig, `{"action":"add","spec":"in 30m","text":"提醒我验收"}`)
	if !strings.Contains(tmOut, `"ok":true`) {
		t.Fatalf("add timer failed: %s", tmOut)
	}
	timers, err := st.ListTimers(context.Background())
	if err != nil || len(timers) != 1 || timers[0].Target != model.RouterName {
		t.Fatalf("expected timer target=%q, got %+v err=%v", model.RouterName, timers, err)
	}

	// 5. HTTP 400 (ErrLLMBadRequest) finishes the offending trigger instead of pausing the entire router.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"image input not supported"}}`))
	}))
	defer badSrv.Close()
	eng400, st400, _, _ := newTestAPIEngine(t, badSrv)
	_, badTrig, err := eng400.Say(context.Background(), SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "看图"})
	if err != nil {
		t.Fatalf("say bad400: %v", err)
	}
	eng400.deliverAPIRouter(context.Background(), []*model.Trigger{badTrig})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !eng400.isRouterWorking(context.Background()) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	gotBad, _ := st400.GetTrigger(context.Background(), badTrig.ID)
	if gotBad.Status != "delivered" {
		t.Fatalf("expected HTTP 400 trigger to be skipped/delivered instead of wedging queue, got %s", gotBad.Status)
	}
	eng400.mu.Lock()
	paused := eng400.router.authPaused
	eng400.mu.Unlock()
	if paused {
		t.Fatalf("HTTP 400 input error must NOT set router.authPaused")
	}

	// 6. TokenHub effort normalization clamps unsupported efforts per model.
	if got := NormalizeTokenHubEffort("hy4", "max"); got != "high" {
		t.Fatalf("hy4 max should normalize to high, got %q", got)
	}
	if got := NormalizeTokenHubEffort("hy4", "no_think"); got != "no_think" {
		t.Fatalf("hy4 no_think should stay no_think, got %q", got)
	}
	if got := NormalizeTokenHubEffort("glm-5.3", "no_think"); got != "low" {
		t.Fatalf("glm-5.3 no_think should normalize to low, got %q", got)
	}
	if got := NormalizeTokenHubEffort("glm-5.3", "max"); got != "max" {
		t.Fatalf("glm-5.3 max should stay max, got %q", got)
	}
}

func TestLLMClientSSEStreamingToolsStallAndFallback(t *testing.T) {
	// 1. SSE streaming with reasoning_content (excluded from Content), multi-chunk tool_calls,
	//    usage (cached + reasoning tokens), and active chunks exceeding StreamIdleTimeout.
	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Errorf("expected stream=true & stream_options.include_usage=true, got %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-sse-1")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"深度思考1"}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"reasoning_content":"深度思考2"}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"reasoning_content":"深度思考3"}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"reply_user","arguments":""}}]}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":\"好"}}]}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"了\"}"}}]}}]}`,
			`{"id":"cmpl-s1","model":"hy4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":45,"total_tokens":145,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":30}}}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			// 45ms * 7 = 315ms total (> 140ms StreamIdleTimeout) — active streaming must NOT stall!
			time.Sleep(45 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer sseSrv.Close()

	resp, err := newTestLLMClient(sseSrv).ChatCompletion(context.Background(), LLMClientConfig{
		Provider:          RouterAPIProviderTokenHub,
		BaseURL:           TokenHubDefaultBaseURL,
		APIKey:            "sk-sse-test",
		StreamIdleTimeout: 140 * time.Millisecond,
	}, LLMChatRequest{
		Model:           "hy4",
		ReasoningEffort: "high",
		Stream:          true,
		StreamOptions:   &LLMStreamOptions{IncludeUsage: true},
		Messages:        []LLMMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("active SSE streaming failed: %v", err)
	}
	if resp.RequestID != "req-sse-1" || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("unexpected SSE response meta: %+v", resp)
	}
	if resp.Choices[0].Message.Content != "" {
		t.Fatalf("reasoning_content must NOT leak into Message.Content, got %q", resp.Choices[0].Message.Content)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 || resp.Choices[0].Message.ToolCalls[0].Function.Name != "reply_user" ||
		resp.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"text":"好了"}` {
		t.Fatalf("unexpected assembled tool calls: %+v", resp.Choices[0].Message.ToolCalls)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 64 ||
		resp.Usage.CompletionTokensDetails == nil || resp.Usage.CompletionTokensDetails.ReasoningTokens != 30 {
		t.Fatalf("unexpected usage details: %+v", resp.Usage)
	}

	// 2. Stalled SSE stream (0 chunks for > StreamIdleTimeout) aborts fast on a single attempt (no inner retry loop).
	var stallAttempts int
	stallSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stallAttempts++
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"s-stall\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"卡住前\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer stallSrv.Close()

	t0 := time.Now()
	_, err = newTestLLMClient(stallSrv).ChatCompletion(context.Background(), LLMClientConfig{
		Provider:          RouterAPIProviderTokenHub,
		BaseURL:           TokenHubDefaultBaseURL,
		APIKey:            "sk-stall-test",
		StreamIdleTimeout: 100 * time.Millisecond,
	}, LLMChatRequest{
		Model:    "hy4",
		Stream:   true,
		Messages: []LLMMessage{{Role: "user", Content: "test stall"}},
	})
	if err == nil || !strings.Contains(err.Error(), "卡顿") || stallAttempts != 1 || time.Since(t0) > 800*time.Millisecond {
		t.Fatalf("expected single-attempt fast stall abort, got attempts=%d elapsed=%s err=%v", stallAttempts, time.Since(t0), err)
	}

	// 3. Custom OpenAI gateway returning HTTP 400 on stream_options falls back cleanly to non-streaming.
	var fbAttempts int
	fbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fbAttempts++
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter: stream_options"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-fb", "model": "custom",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok-fallback"}}},
		})
	}))
	defer fbSrv.Close()

	resp3, err := newTestLLMClient(fbSrv).ChatCompletion(context.Background(), LLMClientConfig{
		Provider: RouterAPIProviderOpenAI,
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "sk-fb",
	}, LLMChatRequest{
		Model:         "custom",
		Stream:        true,
		StreamOptions: &LLMStreamOptions{IncludeUsage: true},
		Messages:      []LLMMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil || fbAttempts != 2 || resp3.Choices[0].Message.Content != "ok-fallback" {
		t.Fatalf("stream_options fallback failed: attempts=%d err=%v resp=%+v", fbAttempts, err, resp3)
	}
}

func TestAPIRouterSameTriggerCircuitBreakerUnblocksQueue(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []LLMMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last, _ := req.Messages[len(req.Messages)-1].Content.(string)
		// Inspect ONLY the current incoming trigger section (history block may mention earlier messages).
		triggerSection := last
		if idx := strings.LastIndex(last, "## 本轮待处理事件"); idx >= 0 {
			triggerSection = last[idx:]
		}
		mu.Lock()
		reqCount++
		mu.Unlock()
		if strings.Contains(triggerSection, "卡死消息") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream timeout"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-next", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_next", "type": "function",
						"function": map[string]any{
							"name":      "reply_user",
							"arguments": `{"text":"后续消息已正常回复"}`,
						},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, sink, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()

	_, trigBad, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "卡死消息"})
	if err != nil {
		t.Fatal(err)
	}
	_, trigGood, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "正常后续消息"})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate 3 consecutive failures on trigBad (clearing backoff between ticks to keep test fast).
	for attempt := 1; attempt <= maxSameAPITriggerFails; attempt++ {
		eng.mu.Lock()
		eng.router.backoffUntil = time.Time{}
		eng.mu.Unlock()
		eng.tick(ctx)
		waitUntil(t, 5*time.Second, func() bool {
			return !eng.isRouterWorking(ctx)
		}, "bad trigger attempt settled")
	}

	gotBad, _ := st.GetTrigger(ctx, trigBad.ID)
	if gotBad.Status != "delivered" {
		t.Fatalf("expected poison trigger skipped after %d consecutive failures, got status=%s", maxSameAPITriggerFails, gotBad.Status)
	}

	// Next tick immediately delivers trigGood without any backoff delay!
	eng.tick(ctx)
	waitUntil(t, 5*time.Second, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.replies) >= 1
	}, "good trigger delivered after circuit break")
	waitUntil(t, 3*time.Second, func() bool {
		return !eng.isRouterWorking(ctx)
	}, "good trigger settled")

	gotGood, _ := st.GetTrigger(ctx, trigGood.ID)
	if gotGood.Status != "delivered" {
		t.Fatalf("expected trigGood delivered, got %s", gotGood.Status)
	}
	sink.mu.Lock()
	replyText := sink.replies[0].Text
	sink.mu.Unlock()
	if !strings.Contains(replyText, "后续消息已正常回复") {
		t.Fatalf("unexpected reply text for trigGood: %q", replyText)
	}
}

func TestAPIModeNoPTYTranscriptLeakAndCleanHistoryAndProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-prog", "model": "hy4",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "c1", "type": "function", "function": map[string]any{
							"name":      "reply_user",
							"arguments": `{"text":"一切正常"}`,
						}},
					},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, sink, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()

	// 1. Seed prior assistant reply + routine noise events and verify history preserves
	// reply content without internal "[eXXXX]" prefixes and filters noise.
	if _, _, _, err := eng.Reply(ctx, model.MainThread, "之前的正常回复内容", "", nil, false); err != nil {
		t.Fatal(err)
	}
	eng.addEvent(ctx, model.MainThread, "agy-3750 完成了一轮", "agy-3750", "done")
	eng.addEvent(ctx, model.MainThread, "已自动休眠 agy-3637（持续空闲 2h）", "agy-3637", "info")
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "you", Text: "检查一下"})
	if err != nil {
		t.Fatal(err)
	}
	histText := eng.buildThreadHistoryText(ctx, model.MainThread, trig)
	if histText == "" {
		t.Fatalf("expected non-empty structured thread history block")
	}
	if strings.Contains(histText, "[e") {
		t.Fatalf("history must not carry internal [eXXXX] prefix, got %q", histText)
	}
	if !strings.Contains(histText, "之前的正常回复内容") {
		t.Fatalf("expected prior assistant reply preserved in history, got %q", histText)
	}
	if strings.Contains(histText, "完成了一轮") || strings.Contains(histText, "已自动休眠") {
		t.Fatalf("expected routine internal events filtered out of history, got %q", histText)
	}

	// 3. Running an API turn pushes clean Chinese step progress to ChannelSink.OnAPIRouterProgress.
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitUntil(t, 5*time.Second, func() bool {
		return !eng.isRouterWorking(ctx)
	}, "api progress turn settled")

	sink.mu.Lock()
	progs := append([]struct{ thread, triggerID, think string }(nil), sink.progress...)
	sink.mu.Unlock()
	if len(progs) == 0 || !strings.Contains(progs[0].think, "正在整理回复") || progs[0].triggerID != trig.ID {
		t.Fatalf("expected Chinese API tool progress for %s, got %+v", trig.ID, progs)
	}
}

func TestAPIRouterMaxStepsExhaustedNeverSilentlySwallows(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
		lastMsgs []LLMMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqCount++
		n := reqCount
		lastMsgs = req.Messages
		mu.Unlock()

		// Stubbornly keep calling read_detail on every step without calling reply_user.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-loop", "model": "kimi-k3",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_" + string(rune('a'+n)), "type": "function",
						"function": map[string]any{
							"name":      "read_detail",
							"arguments": `{"kind":"work_item","target":"missing"}`,
						},
					}},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, st, sink, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "帮我合入部署"})
	if err != nil {
		t.Fatal(err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitUntil(t, 5*time.Second, func() bool {
		return !eng.isRouterWorking(ctx)
	}, "max steps turn settled")

	mu.Lock()
	gotSteps := reqCount
	finalMsgs := lastMsgs
	mu.Unlock()
	if gotSteps != maxAPIRouterSteps {
		t.Fatalf("expected %d steps, got %d", maxAPIRouterSteps, gotSteps)
	}
	// Verify wrap-up reminder was injected before the last step.
	foundNudge := false
	for _, m := range finalMsgs {
		if s, ok := m.Content.(string); ok && strings.Contains(s, "最后一步") {
			foundNudge = true
			break
		}
	}
	if !foundNudge {
		t.Fatalf("expected wrap-up nudge before final step, got %+v", finalMsgs)
	}

	// Verify explicit error event was emitted and stream sealed with "error" (NEVER "silent").
	ends := sink.ends()
	if len(ends) == 0 || ends[len(ends)-1].reason != "error" {
		t.Fatalf("expected error seal on max-step exhaustion, got %+v", ends)
	}
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 10)
	foundErrEv := false
	for _, en := range entries {
		if en.Role == "event" && strings.Contains(en.Text, "最大工具步数") {
			foundErrEv = true
			break
		}
	}
	if !foundErrEv {
		t.Fatalf("expected visible error event on max-step exhaustion, entries=%+v", entries)
	}
}

func TestAPIRouterBareTextNudgeAndExplicitCardTitle(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		step := reqCount
		mu.Unlock()
		switch step {
		case 1:
			// Simulate lazy hallucination with zero tool calls.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-bare", "model": "kimi-k3",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "已停掉并重新派发替换 Worker"},
				}},
			})
		default:
			// After nudge, model calls reply_user properly with explicit card title.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-tool", "model": "kimi-k3",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_ok", "type": "function",
							"function": map[string]any{
								"name":      "reply_user",
								"arguments": `{"title":"wi3524 直聊身份 · 合入部署","text":"分支已验收全绿，请确认推进方式。","choices":["合入并部署","只合入构建"]}`,
							},
						}},
					},
				}},
			})
		}
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv)
	ctx := context.Background()
	trig, err := eng.Ingest(ctx, "worker", model.MainThread, "agy-3713", "worker failed to start", "", model.RouterName, "agy-3713")
	if err != nil {
		t.Fatal(err)
	}
	eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
	waitUntil(t, 5*time.Second, func() bool {
		return !eng.isRouterWorking(ctx)
	}, "nudged bare text turn settled")

	mu.Lock()
	gotSteps := reqCount
	mu.Unlock()
	if gotSteps != 2 {
		t.Fatalf("expected bare text nudged on step 1 and resolved via tool on step 2, got %d requests", gotSteps)
	}
	acts, err := st.ListPendingActions(ctx)
	if err != nil || len(acts) != 1 || acts[0].Title != "wi3524 直聊身份 · 合入部署" {
		t.Fatalf("expected explicit card title preserved on Action, got %+v err=%v", acts, err)
	}
}

func TestAPIRouterSilenceBoundaryZeroBareTextLeakAndNoAction(t *testing.T) {
	// 1. Table-driven verification of deterministic product silence boundary.
	policyCases := []struct {
		name string
		trig *model.Trigger
		want bool
	}{
		{name: "human wecom", trig: &model.Trigger{Source: "wecom", Policy: "respond", Text: "还在跑，"}, want: false},
		{name: "human web chat", trig: &model.Trigger{Source: "chat", Policy: "respond", Text: "如何了"}, want: false},
		{name: "worker final report", trig: &model.Trigger{Source: "worker", Policy: "respond", From: "agy-1", Text: "report from agy-1:\n发布闭环已完成"}, want: false},
		{name: "worker error", trig: &model.Trigger{Source: "worker", Policy: "respond", From: "agy-1", Text: "worker agy-1 stopped on an agent/provider error"}, want: false},
		{name: "user timer", trig: &model.Trigger{Source: "timer", Policy: "respond", Text: "提醒开会"}, want: false},
		{name: "worker progress report", trig: &model.Trigger{Source: "worker", Policy: "respond", From: "agy-1", Text: "report from agy-1:\n进度：正在构建"}, want: true},
		{name: "system morning digest", trig: &model.Trigger{Source: "system", Policy: "respond", From: "corgi", Ref: "morning-digest", Text: "[corgi morning-digest]"}, want: true},
		{name: "system idle patrol", trig: &model.Trigger{Source: "system", Policy: "respond", From: "corgi", Ref: "idle-patrol", Text: "[corgi idle-patrol]"}, want: true},
		{name: "system dream anomaly", trig: &model.Trigger{Source: "system", Policy: "respond", From: "dream", Ref: "d-1", Text: "dream report"}, want: true},
		{name: "system dream empty ref rejected", trig: &model.Trigger{Source: "system", Policy: "respond", From: "dream", Ref: "", Text: "dream report"}, want: false},
		{name: "system unknown ref rejected", trig: &model.Trigger{Source: "system", Policy: "respond", From: "corgi", Ref: "other", Text: "other"}, want: false},
		{name: "candidate webhook", trig: &model.Trigger{Source: "webhook:ci", Policy: "candidate", Text: "build ping"}, want: true},
		{name: "candidate non-webhook rejected", trig: &model.Trigger{Source: "chat", Policy: "candidate", Text: "ping"}, want: false},
	}
	for _, tc := range policyCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canAPIRouterSilence(tc.trig); got != tc.want {
				t.Fatalf("canAPIRouterSilence(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	// 2. Persistent bare-text hallucination ("收到。后续我会...") on human turn MUST NEVER leak to user/DB.
	t.Run("bare text retries never leak to user", func(t *testing.T) {
		var (
			mu          sync.Mutex
			reqCount    int
			toolChoices []any
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req LLMChatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			reqCount++
			toolChoices = append(toolChoices, req.ToolChoice)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-echo", "model": "kimi-k3",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "收到。后续我会先实际调用工具并确认返回 ok:true；需要回复时统一走 reply_user。"},
				}},
			})
		}))
		defer srv.Close()

		eng, st, sink, _ := newTestAPIEngine(t, srv)
		ctx := context.Background()
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "如何了"})
		if err != nil {
			t.Fatal(err)
		}
		eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
		waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "bare text turn settled")

		mu.Lock()
		gotReqs := reqCount
		gotTC := append([]any(nil), toolChoices...)
		mu.Unlock()
		if gotReqs != 1+maxAPIRouterBareTextRetries {
			t.Fatalf("expected %d attempts (1 + %d retries), got %d", 1+maxAPIRouterBareTextRetries, maxAPIRouterBareTextRetries, gotReqs)
		}
		if gotTC[0] != nil || gotTC[1] != "required" {
			t.Fatalf("expected tool_choice nil on step 1 then required on retry, got %+v", gotTC)
		}
		sink.mu.Lock()
		replyCount := len(sink.replies)
		sink.mu.Unlock()
		if replyCount != 0 {
			t.Fatalf("bare text MUST NEVER be delivered to user, got %d replies", replyCount)
		}
		entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
		foundErrEv := false
		for _, en := range entries {
			if en.Role == "corgi" {
				t.Fatalf("bare text meta-reply MUST NEVER persist as corgi entry, got %q", en.Text)
			}
			if en.Role == "event" && strings.Contains(en.Text, "未通过工具发出有效答复") {
				foundErrEv = true
			}
		}
		if !foundErrEv {
			t.Fatalf("expected explicit error event after bare-text retries exhausted, entries=%+v", entries)
		}
		ends := sink.ends()
		if len(ends) == 0 || ends[len(ends)-1].reason != "error" {
			t.Fatalf("expected error seal (never silent), got %+v", ends)
		}
	})

	// 3. Empty content + forbidden [SILENT] / no_action on human message are rejected; 400 on tool_choice="required" downgrades cleanly.
	t.Run("human turn rejects silent and downgrades unsupported tool_choice", func(t *testing.T) {
		var (
			mu   sync.Mutex
			step int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req LLMChatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ToolChoice == "required" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"tool_choice required not supported"}}`))
				return
			}
			mu.Lock()
			step++
			cur := step
			mu.Unlock()
			switch cur {
			case 1:
				// Empty output on must-reply turn -> must retry, never silently swallow!
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-empty", "model": "hy4",
					"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": ""}}},
				})
			case 2:
				// Calling no_action on a human message -> tool rejects!
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-noact", "model": "hy4",
					"choices": []map[string]any{{
						"index": 0, "finish_reason": "tool_calls",
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id": "call_na", "type": "function",
								"function": map[string]any{"name": "no_action", "arguments": `{"reason":"无需回复"}`},
							}},
						},
					}},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-ok", "model": "hy4",
					"choices": []map[string]any{{
						"index": 0, "finish_reason": "tool_calls",
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{{
								"id": "call_rep", "type": "function",
								"function": map[string]any{"name": "reply_user", "arguments": `{"text":"物言数据流方案仍在等你审阅。"}`},
							}},
						},
					}},
				})
			}
		}))
		defer srv.Close()

		eng, _, sink, _ := newTestAPIEngine(t, srv)
		ctx := context.Background()
		_, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "还在跑，"})
		if err != nil {
			t.Fatal(err)
		}
		eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
		waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "human turn settled")

		sink.mu.Lock()
		replies := append([]*model.Entry(nil), sink.replies...)
		sink.mu.Unlock()
		if len(replies) != 1 || !strings.Contains(replies[0].Text, "物言数据流方案") {
			t.Fatalf("expected human message answered after silent rejection, got %+v", replies)
		}
	})

	// 4. Optional background patrol cleanly finishes via explicit no_action(reason) with silent seal.
	t.Run("system patrol closes silently via no_action tool", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-patrol-na", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_na_ok", "type": "function",
							"function": map[string]any{"name": "no_action", "arguments": `{"reason":"所有开放事项近期已同步，无新变化"}`},
						}},
					},
				}},
			})
		}))
		defer srv.Close()

		eng, st, sink, _ := newTestAPIEngine(t, srv)
		ctx := context.Background()
		trig, err := eng.Ingest(ctx, "system", model.MainThread, "corgi", "[corgi morning-digest] Daily morning review", "", model.RouterName, "morning-digest")
		if err != nil {
			t.Fatal(err)
		}
		eng.deliverAPIRouter(ctx, []*model.Trigger{trig})
		waitUntil(t, 5*time.Second, func() bool { return !eng.isRouterWorking(ctx) }, "patrol no_action settled")

		gotTrig, _ := st.GetTrigger(ctx, trig.ID)
		if gotTrig == nil || gotTrig.Status != "delivered" {
			t.Fatalf("expected patrol trigger acked, got %+v", gotTrig)
		}
		ends := sink.ends()
		if len(ends) == 0 || ends[len(ends)-1].reason != "silent" {
			t.Fatalf("expected silent seal from no_action, got %+v", ends)
		}
		sink.mu.Lock()
		replyCount := len(sink.replies)
		sink.mu.Unlock()
		if replyCount != 0 {
			t.Fatalf("expected 0 user replies on no_action, got %d", replyCount)
		}
	})

	// 5. Tool progress bubbles resolve WorkItem titles instead of raw worker IDs.
	t.Run("tool progress uses WorkItem title instead of worker slug", func(t *testing.T) {
		eng, st, _, _ := newTestAPIEngine(t, nil)
		ctx := context.Background()
		wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "会话直聊身份提醒", "等待验收", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertWorker(ctx, &model.Worker{
			Name: "agy-3637", Kind: "agy", Thread: model.MainThread, WorkItemID: wi.ID, Status: "idle",
		}); err != nil {
			t.Fatal(err)
		}
		got := eng.formatAPIToolProgress(ctx, model.MainThread, LLMToolCall{
			Function: LLMToolFunctionCall{Name: "read_detail", Arguments: `{"kind":"worker","target":"agy-3637"}`},
		})
		if !strings.Contains(got, "「会话直聊身份提醒」") || strings.Contains(got, "agy-3637") {
			t.Fatalf("expected WorkItem-centered progress without worker slug, got %q", got)
		}
	})
}

func TestAPIModeWorkerTurnQuietSettleAndApprovalNoWake(t *testing.T) {
	eng, st, _, _ := newTestAPIEngine(t, nil)
	eng.mu.Lock()
	eng.workerTurnSettleDelay = 20 * time.Second
	eng.mu.Unlock()

	ctx := context.Background()
	wk := &model.Worker{Name: "agy-settle", Kind: "agy", Thread: model.MainThread, PaneID: "w1:p1", Session: "s1", Status: "idle"}
	_ = st.UpsertWorker(ctx, wk)

	base := time.Now()
	snap1 := workerTurnSnapshot{FromTranscript: true, AfterLastUser: true, Text: "正在构建产物…", AssistantAt: 100}

	// 1. Immediate pause does NOT emit; arms settle window.
	if eng.shouldEmitWorkerTurnSummary(wk, snap1, base) {
		t.Fatal("expected initial mid-run pause to be debounced in API mode")
	}
	if !eng.hasWorkerPendingCheck(wk.Name, base.Add(5*time.Second)) {
		t.Fatal("expected pendingCheck armed during quiet-settle window")
	}
	// Still before settleDelay -> suppressed.
	if eng.shouldEmitWorkerTurnSummary(wk, snap1, base.Add(15*time.Second)) {
		t.Fatal("expected still debounced before settleDelay elapsed")
	}
	// If worker resumes working, clearWorkerPendingCheck cancels settle cleanly.
	eng.clearWorkerPendingCheck(wk.Name)
	if eng.hasWorkerPendingCheck(wk.Name, base.Add(16*time.Second)) {
		t.Fatal("expected settle cancelled when worker resumed working")
	}

	// 2. Worker finishes without calling `corgi report` and stays idle past settleDelay -> emits ONCE!
	snap2 := workerTurnSnapshot{FromTranscript: true, AfterLastUser: true, Text: "全部改造已完成并跑通测试", AssistantAt: 200}
	if eng.shouldEmitWorkerTurnSummary(wk, snap2, base.Add(30*time.Second)) {
		t.Fatal("expected first observation of snap2 to start settle timer")
	}
	if !eng.shouldEmitWorkerTurnSummary(wk, snap2, base.Add(55*time.Second)) {
		t.Fatal("expected settled turn to emit once settleDelay elapsed")
	}
	// Duplicate re-check for same AssistantAt must NOT emit twice.
	if eng.shouldEmitWorkerTurnSummary(wk, snap2, base.Add(60*time.Second)) {
		t.Fatal("expected duplicate settled turn suppressed")
	}

	// 3. In API Router mode, handleWorkerBlocked creates the interactive approval Action and
	// WorkItem-centered event directly WITHOUT waking the API Router.
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		if method == "pane.read" {
			return map[string]any{"read": map[string]any{
				"text": "Run dangerous shell command?\n> 1. Allow once\n  2. Deny\nesc to cancel",
			}}, nil
		}
		return map[string]any{}, nil
	})
	eng.fleet = fleet.NewFleet(hc, "hub", t.TempDir(), "test", []string{"claude"})
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "数据库迁移方案", "等待审批", "", "")
	if err != nil {
		t.Fatal(err)
	}
	wkBlk := &model.Worker{
		Name: "claude-blk", Kind: "claude", Thread: model.MainThread, PaneID: "w9:p1",
		WorkItemID: wi.ID, Status: "blocked",
	}
	_ = st.UpsertWorker(ctx, wkBlk)
	eng.handleWorkerBlocked(ctx, wkBlk)

	acts, err := st.ListPendingActions(ctx)
	if err != nil || len(acts) != 1 || acts[0].Kind != model.ActionKindWorkerDialog {
		t.Fatalf("expected 1 worker dialog action created directly, got %+v err=%v", acts, err)
	}
	if trs, _ := st.ListQueuedTriggers(ctx); len(trs) != 0 {
		t.Fatalf("API mode handleWorkerBlocked must NOT wake API Router redundantly, got %+v", trs)
	}
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 10)
	foundWIEv := false
	for _, en := range entries {
		if en.Role == "event" && strings.Contains(en.Text, "「数据库迁移方案」 等你确认") {
			foundWIEv = true
		}
	}
	if !foundWIEv {
		t.Fatalf("expected WorkItem-centered blocked event, got %+v", entries)
	}
}

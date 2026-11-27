package core

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

type recordingAPISink struct {
	NopSink
	mu         sync.Mutex
	turnEnds   []struct{ thread, triggerID, reason string }
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

func newTestAPIEngine(t *testing.T, baseURL string) (*Engine, *store.Store, *recordingAPISink, string) {
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
	eng.SetAllowPrivateLLMForTest(true)
	sink := &recordingAPISink{}
	eng.SetSink(sink)

	cfg := eng.Settings()
	cfg.RouterMode = "api"
	cfg.RouterAPIProvider = RouterAPIProviderTokenHub
	cfg.RouterAPIBaseURL = baseURL
	cfg.RouterAPIModel = "hy4"
	cfg.RouterAPIEffort = "no_think"
	if err := eng.SaveConfig(context.Background(), &cfg, nil); err != nil {
		t.Fatalf("save api config: %v", err)
	}
	if _, err := eng.SetRouterAPIKey(context.Background(), RouterAPIProviderTokenHub, baseURL, "sk-test-tokenhub-9876"); err != nil {
		t.Fatalf("set api key: %v", err)
	}
	return eng, st, sink, dir
}

func TestLLMURLPolicyAndOriginBinding(t *testing.T) {
	// Reject non-HTTPS, localhost, private IP, metadata IP when test bypass is off.
	for _, bad := range []string{
		"http://api.example.com/v1",
		"https://localhost/v1",
		"https://127.0.0.1/v1",
		"https://169.254.169.254/latest",
		"https://10.0.0.5/v1",
		"https://user:pass@api.example.com/v1",
	} {
		if _, _, err := NormalizeRouterAPIBaseURL(RouterAPIProviderOpenAI, bad, false); err == nil {
			t.Fatalf("expected URL %q to be rejected", bad)
		}
	}
	// TokenHub locks to fixed HTTPS origin.
	base, origin, err := NormalizeRouterAPIBaseURL(RouterAPIProviderTokenHub, "https://evil.example.com/v1", false)
	if err != nil || base != TokenHubDefaultBaseURL || origin != "https://tokenhub.tencentmaas.com" {
		t.Fatalf("tokenhub lock failed: base=%q origin=%q err=%v", base, origin, err)
	}

	// Redirects must be blocked.
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/steal", http.StatusFound)
	}))
	defer redirSrv.Close()
	_, err = ChatCompletion(context.Background(), LLMClientConfig{
		Provider:            RouterAPIProviderOpenAI,
		BaseURL:             redirSrv.URL + "/v1",
		APIKey:              "sk-secret",
		AllowPrivateForTest: true,
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

	eng, _, _, _ := newTestAPIEngine(t, srv.URL+"/v1")
	info := eng.RouterAPISecretInfo(context.Background())
	if !info.HasKey || info.Masked != "••••••••9876" || !info.OriginMatch {
		t.Fatalf("unexpected secret status: %+v", info)
	}
	res := eng.TestRouterAPI(context.Background(), TestRouterAPIRequest{
		Provider: RouterAPIProviderOpenAI,
		BaseURL:  "http://127.0.0.1:1/v1",
		Model:    "hy4",
	})
	if res.OK || !strings.Contains(res.Error, "不一致") {
		t.Fatalf("expected origin mismatch rejection, got %+v", res)
	}
	okRes := eng.TestRouterAPI(context.Background(), TestRouterAPIRequest{
		Provider: RouterAPIProviderTokenHub,
		BaseURL:  srv.URL + "/v1",
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
			// Deliberately violate guardrail: purpose="answer" with choices!
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
								"arguments": `{"purpose":"answer","text":"要选哪一个？","choices":["A","B"]}`,
							},
						}},
					},
				}},
			})
		case 2:
			// Self-corrected to purpose="attention" -> should succeed and early-exit!
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
								"arguments": `{"purpose":"attention","text":"请拍板方案","choices":["方案 A","方案 B"]}`,
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

	eng, st, _, _ := newTestAPIEngine(t, srv.URL+"/v1")
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
	eng, st, _, _ := newTestAPIEngine(t, "http://127.0.0.1:9/v1")

	// Create an open prototype work item (simulating Incident 1: e3548).
	wi, err := eng.SetWorkItem(context.Background(), "new", model.MainThread, "会话直聊身份提醒原型", "确认原型交互", "", "")
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	if _, err := st.SetWorkItemScope(context.Background(), wi.ID, "prototype"); err != nil {
		t.Fatalf("set scope: %v", err)
	}

	// Case 1: Human gives prototype feedback WITHOUT naming work_id -> model tries dispatch_work(mode="implement")
	// even omitting work_id -> MUST be blocked by Go pre-binding!
	_, protoTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "这个提示放在顶部更好",
	})
	if err != nil {
		t.Fatalf("say proto feedback: %v", err)
	}
	meta := eng.resolveTurnConsentMeta(context.Background(), protoTrig)
	if meta.prototypeBlockedWorkID != wi.ID {
		t.Fatalf("expected pre-bound prototype work item %s, got %q", wi.ID, meta.prototypeBlockedWorkID)
	}
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: protoTrig.ID}
	resStr := eng.toolDispatchWork(context.Background(), turn, protoTrig, meta, `{"mode":"implement","goal":"修改 Corgi 顶部栏源码"}`)
	if !strings.Contains(resStr, `"ok":false`) || !strings.Contains(resStr, "原型/只读阶段") {
		t.Fatalf("expected prototype transition block, got: %s", resStr)
	}

	// Case 2: Non-human worker report tries to call implement or verify(send_on_fail=true) -> MUST be blocked!
	workerTrig, err := eng.Ingest(context.Background(), "worker", model.MainThread, "worker-1", "测试挂了一个用例", "", model.RouterName, "worker-1")
	if err != nil {
		t.Fatalf("ingest worker trig: %v", err)
	}
	wMeta := eng.resolveTurnConsentMeta(context.Background(), workerTrig)
	resWorker := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: workerTrig.ID}, workerTrig, wMeta, `{"mode":"implement","goal":"修复失败的单测"}`)
	if !strings.Contains(resWorker, `"ok":false`) || !strings.Contains(resWorker, "非人类直接指令") {
		t.Fatalf("expected non-human implement block, got: %s", resWorker)
	}

	// Case 3: Unauthorized merge/push/deploy (Incident 2: e3268) -> MUST be blocked even on fresh human turn!
	_, mergeTrig, err := eng.Say(context.Background(), SayRequest{
		Thread:  model.MainThread,
		Channel: "wecom",
		From:    "owner",
		Text:    "看看这个分支情况",
	})
	if err != nil {
		t.Fatalf("say merge check: %v", err)
	}
	mMeta := eng.resolveTurnConsentMeta(context.Background(), mergeTrig)
	resMerge := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: mergeTrig.ID}, mergeTrig, mMeta, `{"mode":"implement","goal":"执行 git merge main 并推送到远端"}`)
	if !strings.Contains(resMerge, `"ok":false`) || !strings.Contains(resMerge, "高风险发布操作") {
		t.Fatalf("expected release op block, got: %s", resMerge)
	}

	// Case 4: Fresh explicit mobile coding request (unrelated to prototype) passes consent check!
	// (Note: e.fleet is nil in unit test so Spawn itself fails on fleet check AFTER passing the consent gate.)
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
	if fMeta.prototypeBlockedWorkID != "" {
		t.Fatalf("fresh unrelated coding request should not bind to prototype work item, got %q", fMeta.prototypeBlockedWorkID)
	}
	resFresh := eng.toolDispatchWork(context.Background(), &apiRouterTurn{thread: model.MainThread, triggerID: freshTrig.ID}, freshTrig, fMeta, `{"mode":"implement","goal":"把登录页按钮改成蓝色并跑单测"}`)
	if strings.Contains(resFresh, "原型/只读阶段") || strings.Contains(resFresh, "非人类直接指令") {
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

	eng, st, sink, _ := newTestAPIEngine(t, srv.URL+"/v1")
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
	eng.recordAPIRouterInflight(context.Background(), t2, "dispatch_work", "claude-99")
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
	eng, st, _, dir := newTestAPIEngine(t, "http://127.0.0.1:9/v1")

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

	// Bounded project_file read_detail allows files inside configured project root and blocks traversal.
	projDir := filepath.Join(dir, "demo-repo")
	_ = os.MkdirAll(projDir, 0o755)
	_ = os.WriteFile(filepath.Join(projDir, "README.md"), []byte("hello corgi api"), 0o644)
	cfg := eng.Settings()
	cfg.Projects = []model.ProjectWorkspace{{
		ID:    "demo",
		Name:  "Demo",
		Paths: map[string]string{"hub": projDir},
	}}
	if err := eng.SaveConfig(context.Background(), &cfg, nil); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	turn := &apiRouterTurn{thread: model.MainThread}
	okRead := eng.toolReadDetail(context.Background(), turn, `{"kind":"project_file","target":"demo:README.md"}`)
	if !strings.Contains(okRead, "hello corgi api") {
		t.Fatalf("expected project_file content, got %s", okRead)
	}
	badRead := eng.toolReadDetail(context.Background(), turn, `{"kind":"project_file","target":"demo:../corgi.db"}`)
	if !strings.Contains(badRead, `"ok":false`) {
		t.Fatalf("expected traversal rejection, got %s", badRead)
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
	// 1. Provider error echoing credential must redact the secret.
	secret := "sk-super-secret-echo-9999"
	redacted := extractSanitizedProviderError([]byte(`{"error":{"message":"invalid key sk-super-secret-echo-9999 rejected"}}`), secret)
	if strings.Contains(redacted, secret) || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected secret redacted, got %q", redacted)
	}

	// 2. Testing or saving blank custom provider URL must NOT reuse previous saved URL.
	eng, st, _, _ := newTestAPIEngine(t, "http://127.0.0.1:9/v1")
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
	eng400, st400, _, _ := newTestAPIEngine(t, badSrv.URL+"/v1")
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
	paused := eng400.apiAuthPaused
	eng400.mu.Unlock()
	if paused {
		t.Fatalf("HTTP 400 input error must NOT set apiAuthPaused")
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

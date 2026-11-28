package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	apiRouterInflightKVKey      = "api_router_inflight"
	apiRouterTurnDeadline       = 420 * time.Second
	apiRouterStepDeadline       = 210 * time.Second
	maxAPIRouterSteps           = 8
	maxAPIRouterToolCalls       = 12
	maxAPIRouterBareTextRetries = 2
	maxAPIToolOutputBytes       = 8192
	maxSameAPITriggerFails      = 3
)

type apiRouterTurn struct {
	gen       uint64
	thread    string
	triggerID string
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time

	mu             sync.Mutex
	mutated        bool
	replied        bool
	dispatched     bool
	noAction       bool
	noActionReason string
	acked          bool
	stopped        bool
	summaryOps     []string
}

func (t *apiRouterTurn) markMutated(opSummary string) {
	t.mu.Lock()
	t.mutated = true
	if opSummary != "" {
		t.summaryOps = append(t.summaryOps, opSummary)
	}
	t.mu.Unlock()
}

func (t *apiRouterTurn) hasMutated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mutated
}

func (t *apiRouterTurn) markReplied() {
	t.mu.Lock()
	t.replied = true
	t.mu.Unlock()
}

func (t *apiRouterTurn) hasReplied() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.replied
}

func (t *apiRouterTurn) markDispatched() {
	t.mu.Lock()
	t.dispatched = true
	t.mu.Unlock()
}

func (t *apiRouterTurn) hasDispatched() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dispatched
}

func (t *apiRouterTurn) markNoAction(reason string) {
	t.mu.Lock()
	t.noAction = true
	t.noActionReason = strings.TrimSpace(reason)
	t.mu.Unlock()
}

func (t *apiRouterTurn) hasNoAction() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.noAction
}

func (t *apiRouterTurn) markAcked() {
	t.mu.Lock()
	t.acked = true
	t.mu.Unlock()
}

func (t *apiRouterTurn) isAcked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acked
}

func (t *apiRouterTurn) markStopped() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
}

func (t *apiRouterTurn) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

func (t *apiRouterTurn) opsText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.summaryOps, "；")
}

type apiRouterInflight struct {
	TriggerID  string    `json:"trigger_id"`
	Thread     string    `json:"thread"`
	Op         string    `json:"op"`
	ReservedID string    `json:"reserved_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// RouterAPISecretStatus exposes non-sensitive status metadata for the stored API Router secret.
type RouterAPISecretStatus struct {
	HasKey      bool   `json:"has_router_api_key"`
	Masked      string `json:"router_api_key_masked,omitempty"`
	BoundOrigin string `json:"router_api_key_bound_origin,omitempty"`
	OriginMatch bool   `json:"router_api_key_origin_match"`
}

// TestRouterAPIRequest configures an on-demand connectivity probe (may test unsaved form values).
type TestRouterAPIRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

// TestRouterAPIResult reports the sanitized result of an API Router connectivity probe.
type TestRouterAPIResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	RequestID string `json:"request_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SetAllowPrivateLLMForTest enables loopback/HTTP test servers inside Go unit tests only.
func (e *Engine) SetAllowPrivateLLMForTest(allow bool) {
	e.mu.Lock()
	e.apiAllowPrivateForTest = allow
	e.mu.Unlock()
}

func (e *Engine) isAPIRouterMode() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings.EffectiveRouterMode() == "api"
}

// NormalizeTokenHubEffort ensures known Tencent TokenHub models only receive their
// provider-supported reasoning_effort values (e.g. hy4: high|no_think; glm-5.3/kimi-k3: low|high|max).
func NormalizeTokenHubEffort(modelName, rawEffort string) string {
	m := strings.TrimSpace(modelName)
	ef := strings.TrimSpace(rawEffort)
	switch m {
	case "hy4":
		if ef == "high" || ef == "no_think" {
			return ef
		}
		return "high"
	case "glm-5.3", "glm-5.3-flash", "kimi-k3":
		if ef == "low" || ef == "high" || ef == "max" {
			return ef
		}
		return "low"
	default:
		return ef
	}
}

func normalizeRouterAPISettings(ns *model.Settings, allowPrivateForTest bool) error {
	if ns == nil {
		return nil
	}
	ns.RouterMode = ns.EffectiveRouterMode()
	ns.RouterAPIProvider = NormalizeRouterAPIProvider(ns.RouterAPIProvider)
	ns.RouterAPIModel = strings.TrimSpace(ns.RouterAPIModel)
	ns.RouterAPIEffort = strings.TrimSpace(ns.RouterAPIEffort)
	if ns.RouterAPIProvider == RouterAPIProviderTokenHub {
		if !allowPrivateForTest || strings.TrimSpace(ns.RouterAPIBaseURL) == "" {
			ns.RouterAPIBaseURL = TokenHubDefaultBaseURL
		}
		if ns.RouterAPIModel == "" {
			ns.RouterAPIModel = TokenHubDefaultModel
		}
		ns.RouterAPIEffort = NormalizeTokenHubEffort(ns.RouterAPIModel, ns.RouterAPIEffort)
	}
	if ns.RouterMode == "api" || strings.TrimSpace(ns.RouterAPIBaseURL) != "" {
		baseURL, _, err := NormalizeRouterAPIBaseURL(ns.RouterAPIProvider, ns.RouterAPIBaseURL, allowPrivateForTest)
		if err != nil {
			if ns.RouterMode == "api" {
				return err
			}
			// Keep invalid custom URL out when still in PTY mode only if non-empty.
			return err
		}
		ns.RouterAPIBaseURL = baseURL
	}
	if ns.RouterMode == "api" && ns.RouterAPIModel == "" {
		return fmt.Errorf("请填写原生 API 大脑的模型名称（如 %s）", TokenHubDefaultModel)
	}
	return nil
}

// RouterAPISecretInfo returns the non-sensitive status of the saved API Router key.
func (e *Engine) RouterAPISecretInfo(ctx context.Context) RouterAPISecretStatus {
	if e.store == nil {
		return RouterAPISecretStatus{}
	}
	sec, err := e.store.LoadRouterAPISecret(ctx)
	if err != nil || sec == nil || strings.TrimSpace(sec.APIKey) == "" {
		return RouterAPISecretStatus{}
	}
	st := e.Settings()
	e.mu.Lock()
	allowTest := e.apiAllowPrivateForTest
	e.mu.Unlock()
	_, curOrigin, urlErr := NormalizeRouterAPIBaseURL(st.RouterAPIProvider, st.RouterAPIBaseURL, allowTest)
	return RouterAPISecretStatus{
		HasKey:      true,
		Masked:      MaskAPIKey(sec.APIKey),
		BoundOrigin: sec.BoundOrigin,
		OriginMatch: urlErr == nil && sec.BoundOrigin != "" && sec.BoundOrigin == curOrigin,
	}
}

func (e *Engine) hasConfiguredRouterAPIKey(ctx context.Context, st model.Settings) bool {
	if e.store == nil {
		return false
	}
	sec, err := e.store.LoadRouterAPISecret(ctx)
	if err != nil || sec == nil || strings.TrimSpace(sec.APIKey) == "" {
		return false
	}
	e.mu.Lock()
	allowTest := e.apiAllowPrivateForTest
	e.mu.Unlock()
	_, origin, err := NormalizeRouterAPIBaseURL(st.RouterAPIProvider, st.RouterAPIBaseURL, allowTest)
	return err == nil && sec.BoundOrigin == origin
}

// SetRouterAPIKey persists a new API Router credential bound to the normalized provider/baseURL origin.
func (e *Engine) SetRouterAPIKey(ctx context.Context, provider, baseURL, apiKey string) (RouterAPISecretStatus, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return RouterAPISecretStatus{}, fmt.Errorf("API Key 不能为空")
	}
	e.mu.Lock()
	allowTest := e.apiAllowPrivateForTest
	st := e.settings
	e.mu.Unlock()

	prov := strings.TrimSpace(provider)
	if prov == "" {
		prov = st.RouterAPIProvider
	}
	prov = NormalizeRouterAPIProvider(prov)
	rawURL := strings.TrimSpace(baseURL)
	// Never silently fall back to an older saved URL when caller explicitly chose custom provider with blank URL.
	if rawURL == "" && prov == RouterAPIProviderTokenHub {
		rawURL = st.RouterAPIBaseURL
	}
	_, origin, err := NormalizeRouterAPIBaseURL(prov, rawURL, allowTest)
	if err != nil {
		return RouterAPISecretStatus{}, err
	}
	if err := e.store.SaveRouterAPISecret(ctx, model.RouterAPISecret{
		APIKey:      apiKey,
		BoundOrigin: origin,
		UpdatedAt:   time.Now(),
	}); err != nil {
		return RouterAPISecretStatus{}, err
	}
	e.mu.Lock()
	e.apiConfigEpoch++
	e.apiAuthPaused = false
	e.apiBackoffUntil = time.Time{}
	e.apiFailCount = 0
	e.apiLastFailTriggerID = ""
	e.apiSameTriggerFails = 0
	e.apiLastErr = ""
	e.mu.Unlock()
	e.wake()
	e.notifyChanged()
	return e.RouterAPISecretInfo(ctx), nil
}

// ClearRouterAPIKey deletes the stored API Router secret.
func (e *Engine) ClearRouterAPIKey(ctx context.Context) error {
	if err := e.store.DeleteRouterAPISecret(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.apiConfigEpoch++
	e.mu.Unlock()
	e.notifyChanged()
	return nil
}

// TestRouterAPI sends a small synthetic Chat Completions request (with 15s deadline) using either
// the supplied API key or the stored key if its bound origin matches the target Base URL.
func (e *Engine) TestRouterAPI(ctx context.Context, req TestRouterAPIRequest) TestRouterAPIResult {
	e.mu.Lock()
	allowTest := e.apiAllowPrivateForTest
	st := e.settings
	e.mu.Unlock()

	explicitCustom := strings.EqualFold(strings.TrimSpace(req.Provider), RouterAPIProviderOpenAI)
	prov := NormalizeRouterAPIProvider(orStr(req.Provider, st.RouterAPIProvider))
	rawURL := strings.TrimSpace(req.BaseURL)
	if rawURL == "" && prov == RouterAPIProviderOpenAI && !explicitCustom {
		rawURL = st.RouterAPIBaseURL
	}
	baseURL, origin, err := NormalizeRouterAPIBaseURL(prov, rawURL, allowTest)
	if err != nil {
		return TestRouterAPIResult{OK: false, Error: err.Error()}
	}
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = orStr(st.RouterAPIModel, TokenHubDefaultModel)
	}
	effort := strings.TrimSpace(req.Effort)
	if prov == RouterAPIProviderTokenHub {
		effort = NormalizeTokenHubEffort(modelName, effort)
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && e.store != nil {
		sec, sErr := e.store.LoadRouterAPISecret(ctx)
		if sErr == nil && sec != nil && strings.TrimSpace(sec.APIKey) != "" {
			if sec.BoundOrigin != origin {
				return TestRouterAPIResult{
					OK:    false,
					Error: fmt.Sprintf("目标地址（%s）与已保存 Key 绑定的源（%s）不一致，请重新填写 API Key", origin, sec.BoundOrigin),
				}
			}
			apiKey = sec.APIKey
		}
	}
	if apiKey == "" {
		return TestRouterAPIResult{OK: false, Error: "请先输入或保存 API Key"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := ChatCompletion(probeCtx, LLMClientConfig{
		Provider:            prov,
		BaseURL:             baseURL,
		APIKey:              apiKey,
		AllowPrivateForTest: allowTest,
	}, LLMChatRequest{
		Model:           modelName,
		ReasoningEffort: effort,
		MaxTokens:       16,
		Messages: []LLMMessage{
			{Role: "system", Content: "Reply with OK."},
			{Role: "user", Content: "ping"},
		},
	})
	if err != nil {
		return TestRouterAPIResult{OK: false, Error: err.Error()}
	}
	return TestRouterAPIResult{
		OK:        true,
		LatencyMS: resp.Latency.Milliseconds(),
		RequestID: resp.RequestID,
		Model:     orStr(resp.Model, modelName),
	}
}

func (e *Engine) resolveActiveRouterAPIConfig(ctx context.Context) (LLMClientConfig, string, string, string, error) {
	e.mu.Lock()
	st := e.settings
	allowTest := e.apiAllowPrivateForTest
	e.mu.Unlock()

	prov := NormalizeRouterAPIProvider(st.RouterAPIProvider)
	baseURL, origin, err := NormalizeRouterAPIBaseURL(prov, st.RouterAPIBaseURL, allowTest)
	if err != nil {
		return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: %v", ErrLLMBadRequest, err)
	}
	modelName := strings.TrimSpace(st.RouterAPIModel)
	if modelName == "" {
		if prov == RouterAPIProviderTokenHub {
			modelName = TokenHubDefaultModel
		} else {
			return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: 未配置 API 大脑模型名称", ErrLLMBadRequest)
		}
	}
	sec, err := e.store.LoadRouterAPISecret(ctx)
	if err != nil || sec == nil || strings.TrimSpace(sec.APIKey) == "" {
		return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: 尚未配置 API Key，请在「自动化与设置 → 模型与运行」录入并保存", ErrLLMAuth)
	}
	if sec.BoundOrigin != origin {
		return LLMClientConfig{}, "", "", "", fmt.Errorf("%w: API Base URL 已变更为 %s，请重新录入对应的 API Key", ErrLLMAuth, origin)
	}
	eff := strings.TrimSpace(st.RouterAPIEffort)
	if prov == RouterAPIProviderTokenHub {
		eff = NormalizeTokenHubEffort(modelName, eff)
	}
	cacheKey := fmt.Sprintf("corgi:%s:%s:v1", prov, modelName)
	return LLMClientConfig{
		Provider:            prov,
		BaseURL:             baseURL,
		APIKey:              sec.APIKey,
		AllowPrivateForTest: allowTest,
	}, modelName, eff, cacheKey, nil
}

// prepareRouterModeSwitch serializes transitions between "pty" and "api", settling active work
// and closing ONLY the Hub-local PTY Router workspace when switching into API mode.
func (e *Engine) prepareRouterModeSwitch(ctx context.Context, prevMode, nextMode string) error {
	e.mu.Lock()
	if e.dream != nil {
		e.mu.Unlock()
		return fmt.Errorf("Corgi 正在做梦整理记忆，请等待完成后再切换大脑模式")
	}
	if e.handover != nil {
		e.mu.Unlock()
		return fmt.Errorf("当前 PTY 会话正在交接记忆，请几秒后再切换大脑模式")
	}
	e.mu.Unlock()

	if prevMode == "api" && nextMode == "pty" {
		e.cancelAndSettleAPIRouterTurn(ctx, "", false)
		return nil
	}
	if prevMode == "pty" && nextMode == "api" {
		// Wait briefly if a PTY Router launch is mid-flight so we don't orphan a fresh workspace.
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			e.mu.Lock()
			launching := e.routerLaunching
			e.mu.Unlock()
			if !launching {
				break
			}
			if !sleepContext(ctx, 150*time.Millisecond) {
				return ctx.Err()
			}
		}
		if e.fleet != nil {
			ws, err := e.fleet.Workspaces(ctx)
			if err == nil {
				for _, w := range ws {
					if w.Label == model.RouterLabel && isHubLocalPane(w.ID) {
						if cErr := e.fleet.CloseWorkspace(ctx, w.ID); cErr != nil {
							return fmt.Errorf("关闭旧 PTY 大脑工作区 %s 失败: %w", w.ID, cErr)
						}
					}
				}
			}
		}
		e.clearRouterLive()
	}
	return nil
}

// cancelAndSettleAPIRouterTurn cancels the active API turn (scoped to thread when non-empty),
// waits briefly for it to exit, and when ackTrigger is true (explicit user Stop) marks it
// stopped and acknowledges its trigger so Stop never replays it.
func (e *Engine) cancelAndSettleAPIRouterTurn(ctx context.Context, thread string, ackTrigger bool) {
	e.mu.Lock()
	turn := e.apiTurn
	if turn == nil || (thread != "" && turn.thread != thread) {
		e.mu.Unlock()
		return
	}
	if ackTrigger {
		turn.markStopped()
	}
	cancel := turn.cancel
	done := turn.done
	trigID := turn.triggerID
	turnThread := turn.thread
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
	}
	if ackTrigger && trigID != "" && e.store != nil {
		if err := e.store.AckTriggerTarget(context.Background(), trigID, model.RouterName); err == nil {
			turn.markAcked()
			e.clearAPIRouterInflightIfMatch(context.Background(), trigID)
		}
		e.mu.Lock()
		sink := e.sink
		e.mu.Unlock()
		sink.OnAPIRouterTurnEnd(context.Background(), turnThread, trigID, "cancelled")
	}
}

func (e *Engine) clearAPIRouterInflightIfMatch(ctx context.Context, triggerID string) {
	if e.store == nil || triggerID == "" {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, apiRouterInflightKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var m apiRouterInflight
	if json.Unmarshal([]byte(raw), &m) == nil && m.TriggerID == triggerID {
		_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)
	}
}

// reconcileAPIRouterInflight inspects any leftover inflight API Router marker on Hub startup,
// prevents blind replay of a mid-mutation trigger across restarts, and emits one clear recovery event.
func (e *Engine) reconcileAPIRouterInflight(ctx context.Context) {
	if e.store == nil {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, apiRouterInflightKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var m apiRouterInflight
	if json.Unmarshal([]byte(raw), &m) != nil || strings.TrimSpace(m.TriggerID) == "" {
		_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)
		return
	}
	_ = e.store.AckTriggerTarget(ctx, m.TriggerID, model.RouterName)
	_ = e.store.DeleteKV(ctx, apiRouterInflightKVKey)

	thread := orStr(m.Thread, model.MainThread)
	note := fmt.Sprintf("上次大脑回合（%s）在 Hub 重启时中断，为防重复执行已跳过自动重放", m.Op)
	if m.ReservedID != "" {
		if wk, wErr := e.store.GetWorker(ctx, m.ReservedID); wErr == nil && wk != nil {
			note = fmt.Sprintf("上次大脑回合在派发 #%s 后因 Hub 重启中断，该 Worker 已保留（状态：%s），未重复派发", wk.Name, wk.Status)
		} else if en, eErr := e.store.GetEntry(ctx, m.ReservedID); eErr == nil && en != nil {
			note = fmt.Sprintf("上次大脑回复（%s）已入库后因 Hub 重启中断，未重复发送", en.ID)
		}
	}
	e.addEvent(ctx, thread, note, m.TriggerID, "info")
	e.notifyChanged()
}

// deliverAPIRouter reserves the single Hub-wide API Router slot for the oldest eligible trigger
// (strictly 1 trigger per turn) and executes it asynchronously under Engine.Run's root context.
func (e *Engine) deliverAPIRouter(ctx context.Context, list []*model.Trigger) {
	if len(list) == 0 {
		return
	}
	now := time.Now()
	e.mu.Lock()
	if e.apiTurn != nil || e.hubUpdating || (e.dream != nil && e.dream.mode != "api") || e.routerDispatching || e.settings.Paused {
		e.mu.Unlock()
		return
	}
	if e.apiAuthPaused || (!e.apiBackoffUntil.IsZero() && now.Before(e.apiBackoffUntil)) {
		e.mu.Unlock()
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	trig := *list[0]
	trig.Targets = append([]string(nil), list[0].Targets...)
	trig.Media = append([]model.MediaRef(nil), list[0].Media...)

	rootCtx := e.runCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	turnCtx, cancel := context.WithTimeout(rootCtx, apiRouterTurnDeadline)
	e.apiTurnGen++
	turn := &apiRouterTurn{
		gen:       e.apiTurnGen,
		thread:    orStr(strings.TrimSpace(trig.Thread), model.MainThread),
		triggerID: trig.ID,
		cancel:    cancel,
		done:      make(chan struct{}),
		startedAt: now,
	}
	e.apiTurn = turn
	e.mu.Unlock()

	e.notifyChanged()
	go e.runAPIRouterTurn(turnCtx, turn, &trig)
}

func (e *Engine) recordAPIRouterInflight(ctx context.Context, trig *model.Trigger, op, reservedID string) {
	if e.store == nil || trig == nil {
		return
	}
	b, err := json.Marshal(apiRouterInflight{
		TriggerID:  trig.ID,
		Thread:     orStr(trig.Thread, model.MainThread),
		Op:         op,
		ReservedID: reservedID,
		StartedAt:  time.Now(),
	})
	if err == nil {
		_ = e.store.SetKV(ctx, apiRouterInflightKVKey, string(b))
	}
}

func (e *Engine) ackAPITriggerOnce(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger) {
	if turn == nil || trig == nil || e.store == nil || turn.isAcked() {
		return
	}
	if err := e.store.AckTriggerTarget(ctx, trig.ID, model.RouterName); err == nil {
		turn.markAcked()
	} else {
		e.logger.Warn("ack api trigger failed", "trigger", trig.ID, "err", err)
	}
}

func (e *Engine) runAPIRouterTurn(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger) {
	bg := context.Background()
	sealReason := "silent"
	defer func() {
		turn.cancel()
		// Retain the inflight recovery marker if a mutation happened whose ACK failed.
		if !turn.hasMutated() || turn.isAcked() {
			e.clearAPIRouterInflightIfMatch(bg, turn.triggerID)
		}

		e.mu.Lock()
		if e.apiTurn == turn {
			e.apiTurn = nil
		}
		sink := e.sink
		e.mu.Unlock()

		close(turn.done)
		if !turn.hasReplied() && sealReason != "" {
			sink.OnAPIRouterTurnEnd(bg, turn.thread, turn.triggerID, sealReason)
		}
		e.notifyChanged()
		e.wake()
	}()

	// Re-check durable status after reserving the slot so a stale tick list cannot run an already-acked trigger twice.
	if e.store != nil {
		fresh, gErr := e.store.GetTrigger(ctx, trig.ID)
		if gErr != nil || fresh == nil || fresh.Status != "queued" || (len(fresh.Targets) > 0 && !slices.Contains(fresh.Targets, model.RouterName)) {
			sealReason = ""
			return
		}
		trig = fresh
	}

	e.mu.Lock()
	configEpoch := e.apiConfigEpoch
	e.mu.Unlock()

	cfg, modelName, effort, cacheKey, err := e.resolveActiveRouterAPIConfig(ctx)
	if err != nil {
		e.handleAPIRouterFailure(bg, turn, trig, configEpoch, 0, modelName, effort, err)
		sealReason = "error"
		return
	}

	turnMeta := e.resolveTurnConsentMeta(ctx, trig)
	messages := e.buildAPIRouterMessages(ctx, trig, turnMeta)
	tools := apiRouterToolDefs()
	totalCalls := 0
	var progressLines []string
	pushProgress := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if len(progressLines) == 0 || progressLines[len(progressLines)-1] != "• "+line {
			progressLines = append(progressLines, "• "+line)
			if len(progressLines) > 6 {
				progressLines = progressLines[len(progressLines)-6:]
			}
		}
		e.mu.Lock()
		sink := e.sink
		e.mu.Unlock()
		sink.OnAPIRouterProgress(bg, turn.thread, trig.ID, strings.Join(progressLines, "\n"))
	}

	allowSilent := canAPIRouterSilence(trig)
	exhaustedSteps := false
	bareTextRetries := 0
	forceToolChoice := false
	toolChoiceSupported := true
	nudgeMsgIdx := -1
	for step := 0; step < maxAPIRouterSteps; step++ {
		if ctx.Err() != nil {
			if turn.isStopped() {
				e.ackAPITriggerOnce(bg, turn, trig)
				sealReason = "cancelled"
			} else if !turn.hasMutated() {
				// Mode switch or shutdown cancellation before any mutation: preserve trigger in queue.
				sealReason = ""
			} else {
				e.handleAPIRouterFailure(bg, turn, trig, configEpoch, step+1, modelName, effort, ctx.Err())
				sealReason = "error"
			}
			return
		}

		req := LLMChatRequest{
			Model:           modelName,
			Messages:        messages,
			Tools:           tools,
			ReasoningEffort: effort,
			PromptCacheKey:  cacheKey,
			Stream:          true,
			StreamOptions:   &LLMStreamOptions{IncludeUsage: true},
		}
		if forceToolChoice && toolChoiceSupported {
			req.ToolChoice = "required"
		}
		stepCtx, stepCancel := context.WithTimeout(ctx, apiRouterStepDeadline)
		resp, err := ChatCompletion(stepCtx, cfg, req)
		if err != nil && forceToolChoice && toolChoiceSupported && errors.Is(err, ErrLLMBadRequest) {
			// Gracefully downgrade if a custom OpenAI-compatible gateway rejects tool_choice="required".
			toolChoiceSupported = false
			req.ToolChoice = nil
			e.logger.Warn("api router provider rejected tool_choice=required, retrying without tool_choice",
				"trigger", trig.ID, "model", modelName, "err", err)
			resp, err = ChatCompletion(stepCtx, cfg, req)
		}
		stepCancel()
		if err != nil {
			if turn.isStopped() {
				e.ackAPITriggerOnce(bg, turn, trig)
				sealReason = "cancelled"
				return
			}
			if ctx.Err() != nil && !turn.hasMutated() && errors.Is(ctx.Err(), context.Canceled) {
				sealReason = ""
				return
			}
			e.handleAPIRouterFailure(bg, turn, trig, configEpoch, step+1, modelName, effort, err)
			sealReason = "error"
			return
		}

		e.mu.Lock()
		if e.apiConfigEpoch == configEpoch {
			e.apiFailCount = 0
			e.apiLastFailTriggerID = ""
			e.apiSameTriggerFails = 0
			e.apiBackoffUntil = time.Time{}
			e.apiAuthPaused = false
			e.apiLastErr = ""
		}
		e.mu.Unlock()

		cached := 0
		if resp.Usage.PromptTokensDetails != nil {
			cached = resp.Usage.PromptTokensDetails.CachedTokens
		}
		reasoning := 0
		if resp.Usage.CompletionTokensDetails != nil {
			reasoning = resp.Usage.CompletionTokensDetails.ReasoningTokens
		}
		choice := resp.Choices[0].Message
		toolNames := make([]string, 0, len(choice.ToolCalls))
		for _, tc := range choice.ToolCalls {
			toolNames = append(toolNames, tc.Function.Name)
		}
		e.logger.Info("api router step",
			"trigger", trig.ID, "thread", turn.thread, "step", step+1,
			"model", modelName, "effort", effort,
			"ttft_ms", resp.TTFT.Milliseconds(), "latency_ms", resp.Latency.Milliseconds(),
			"prompt_tokens", resp.Usage.PromptTokens, "cached_tokens", cached,
			"completion_tokens", resp.Usage.CompletionTokens, "reasoning_tokens", reasoning,
			"tools", strings.Join(toolNames, ","),
			"request_id", resp.RequestID)

		if len(choice.ToolCalls) == 0 {
			plain := strings.TrimSpace(choice.Content)
			if turn.hasReplied() {
				break
			}
			// Backward-compatible silence ONLY for triggers explicitly allowed to be silent.
			if allowSilent && isDeliberateSilenceText(trig, plain) {
				e.logger.Info("api router deliberate silent text",
					"trigger", trig.ID, "thread", turn.thread, "step", step+1, "model", modelName)
				turn.markNoAction("deliberate silent text")
				break
			}
			// Never deliver bare text directly to the user or silently swallow empty output.
			if bareTextRetries < maxAPIRouterBareTextRetries && step < maxAPIRouterSteps-1 {
				bareTextRetries++
				forceToolChoice = true
				e.logger.Warn("api router bare or empty output intercepted",
					"trigger", trig.ID, "thread", turn.thread, "step", step+1,
					"model", modelName, "empty", plain == "", "allow_silent", allowSilent, "attempt", bareTextRetries)
				nudgeText := "[系统内部校验 · 非用户发言] 上一步未调用工具，内容未发送给用户。禁止回复“收到”或提及任何内部工具名；本轮事件必须答复用户，请直接调用所需的查询/操作工具，或调用 reply_user 以事项名称为主语向用户答复。"
				if allowSilent {
					nudgeText = "[系统内部校验 · 非用户发言] 上一步未调用工具，内容未发送给用户。禁止回复“收到”或提及任何内部工具名；若需通知用户请调用 reply_user，若确认无需打扰请调用 no_action(reason)。"
				}
				// Replace in place if retrying consecutively; never append the rejected bare-text assistant draft.
				if nudgeMsgIdx >= 0 && nudgeMsgIdx == len(messages)-1 {
					messages[nudgeMsgIdx].Content = nudgeText
				} else {
					messages = append(messages, LLMMessage{Role: "user", Content: nudgeText})
					nudgeMsgIdx = len(messages) - 1
				}
				continue
			}
			if !turn.hasDispatched() {
				e.logger.Warn("api router bare/empty retries exhausted without tool call",
					"trigger", trig.ID, "thread", turn.thread, "model", modelName,
					"allow_silent", allowSilent, "empty", plain == "")
				if !allowSilent {
					e.addEvent(bg, turn.thread, "大脑本轮未通过工具发出有效答复，已中止以避免误发，请重试", trig.ID, "error")
					sealReason = "error"
				} else {
					turn.markNoAction("no tool call after retries on optional event")
				}
			}
			break
		}

		forceToolChoice = false
		nudgeMsgIdx = -1
		messages = append(messages, LLMMessage{
			Role:      "assistant",
			Content:   choice.Content,
			ToolCalls: choice.ToolCalls,
		})

		earlyExit := false
		stepHadErr := false
		for _, tc := range choice.ToolCalls {
			totalCalls++
			var toolOut string
			if totalCalls > maxAPIRouterToolCalls {
				toolOut = `{"ok":false,"error":"本轮工具调用次数已达上限（12次），请立刻用 reply_user 汇总结论。"}`
				stepHadErr = true
			} else if (tc.Function.Name == "reply_user" || tc.Function.Name == "no_action") && stepHadErr {
				toolOut = toolErr("同一步前置工具执行失败，请先查看报错并处理后再决定回复或结束。")
			} else {
				pushProgress(e.formatAPIToolProgress(ctx, tc))
				var doneTurn bool
				toolOut, doneTurn = e.execAPIRouterTool(ctx, turn, trig, turnMeta, tc)
				if strings.Contains(toolOut, `"ok":false`) {
					stepHadErr = true
					e.logger.Warn("api router tool rejected",
						"trigger", trig.ID, "thread", turn.thread, "step", step+1,
						"tool", tc.Function.Name, "result", oneLineClip(toolOut, 240))
				}
				if doneTurn {
					earlyExit = true
				}
			}
			if len(toolOut) > maxAPIToolOutputBytes {
				toolOut = toolOut[:maxAPIToolOutputBytes] + "...[truncated]"
			}
			messages = append(messages, LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    toolOut,
			})
			if earlyExit || ctx.Err() != nil {
				break
			}
		}
		if earlyExit {
			break
		}
		if step == maxAPIRouterSteps-2 && !turn.hasReplied() && ctx.Err() == nil {
			wrapMsg := "[系统内部提醒 · 非用户发言] 已进入本轮最后一步，请立即调用 reply_user 向用户给出明确答复或当前进展，勿再继续拉取详情。"
			if allowSilent {
				wrapMsg = "[系统内部提醒 · 非用户发言] 已进入本轮最后一步，请立即调用 reply_user 汇报结论，或调用 no_action(reason) 静默结束。"
			}
			messages = append(messages, LLMMessage{
				Role:    "user",
				Content: wrapMsg,
			})
			forceToolChoice = true
		}
		if step == maxAPIRouterSteps-1 {
			exhaustedSteps = true
		}
	}

	e.ackAPITriggerOnce(bg, turn, trig)
	if turn.hasReplied() {
		sealReason = ""
	} else if turn.hasDispatched() {
		sealReason = "dispatched"
	} else if turn.hasNoAction() {
		sealReason = "silent"
	} else if exhaustedSteps && !turn.isStopped() && sealReason != "error" && !allowSilent {
		e.logger.Warn("api router max steps exhausted without reply",
			"trigger", trig.ID, "thread", turn.thread, "model", modelName, "effort", effort,
			"mutated", turn.hasMutated(), "ops", turn.opsText())
		notice := fmt.Sprintf("本轮已达最大工具步数（%d 步）仍未发出最终回复，已结束本轮，请重试或补充说明", maxAPIRouterSteps)
		if turn.hasMutated() {
			notice = fmt.Sprintf("已完成部分操作（%s），但达到最大步数（%d 步）仍未发出最终回复，请确认后续安排", orStr(turn.opsText(), "已执行变更"), maxAPIRouterSteps)
		}
		e.addEvent(bg, turn.thread, notice, trig.ID, "error")
		sealReason = "error"
	} else if sealReason != "error" {
		if allowSilent {
			sealReason = "silent"
		} else {
			e.addEvent(bg, turn.thread, "大脑本轮未能完成有效回复，已结束本轮，请重试", trig.ID, "error")
			sealReason = "error"
		}
	}
}

// canAPIRouterSilence defines the deterministic product boundary for silence:
// ONLY background system patrols/digests/triage, candidate webhooks, or explicit
// progress-only worker reports may conclude silently via no_action.
// Human messages, worker final reports, worker errors, and user timers MUST reply.
func canAPIRouterSilence(trig *model.Trigger) bool {
	if trig == nil {
		return false
	}
	if trig.Policy == "candidate" {
		return true
	}
	if trig.Source == "system" {
		return true
	}
	if trig.Source == "worker" {
		txt := strings.TrimSpace(trig.Text)
		prefix := "report from " + strings.TrimSpace(trig.From) + ":\n"
		if body, ok := strings.CutPrefix(txt, prefix); ok && isProgressReportText(body) {
			return true
		}
	}
	return false
}

func (e *Engine) formatAPIToolProgress(ctx context.Context, tc LLMToolCall) string {
	var raw map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &raw)
	strArg := func(k string) string {
		if v, ok := raw[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	matterLabel := func(workID, workerName string) string {
		if title := e.workItemTitleFor(ctx, workID, workerName); title != "" {
			return "「" + oneLineClip(title, 28) + "」"
		}
		return ""
	}
	switch tc.Function.Name {
	case "reply_user":
		return "正在整理回复…"
	case "no_action":
		return "正在完成核对…"
	case "read_detail":
		kind := strArg("kind")
		target := strArg("target")
		switch kind {
		case "worker":
			if lbl := matterLabel("", target); lbl != "" {
				return "正在查看" + lbl + "最新进展…"
			}
			return "正在查看执行进展…"
		case "work_item":
			if lbl := matterLabel(target, ""); lbl != "" {
				return "正在查阅" + lbl + "事项详情…"
			}
			return "正在查阅事项详情…"
		case "project_file":
			return "正在查阅项目文件…"
		case "project_git":
			return "正在查看项目代码状态…"
		case "note":
			return "正在查阅记忆笔记…"
		}
		return "正在查阅任务详情…"
	case "dispatch_work":
		lbl := matterLabel(strArg("work_id"), strArg("resume"))
		if lbl == "" {
			if t := strArg("title"); t != "" {
				lbl = "「" + oneLineClip(t, 28) + "」"
			}
		}
		if strArg("resume") != "" {
			if lbl != "" {
				return "正在续接推进" + lbl + "…"
			}
			return "正在续接执行任务…"
		}
		if lbl != "" {
			return "正在安排推进" + lbl + "…"
		}
		return "正在派发执行任务…"
	case "steer_worker":
		if lbl := matterLabel(strArg("work_id"), strArg("worker")); lbl != "" {
			return "正在向" + lbl + "发送跟进指令…"
		}
		return "正在发送跟进指令…"
	case "manage_work":
		switch strArg("action") {
		case "stop_worker":
			if lbl := matterLabel("", strArg("target")); lbl != "" {
				return "正在停止" + lbl + "的执行进程…"
			}
			return "正在停止执行进程…"
		case "close_work":
			if lbl := matterLabel(strArg("target"), ""); lbl != "" {
				return "正在关闭事项" + lbl + "…"
			}
			return "正在归档事项…"
		}
		return "正在更新事项状态…"
	case "manage_timer":
		return "正在更新定时提醒…"
	case "save_memory":
		return "正在更新记忆笔记…"
	default:
		return "正在处理中…"
	}
}

func isDeliberateSilenceText(trig *model.Trigger, text string) bool {
	if trig == nil {
		return false
	}
	upper := strings.ToUpper(strings.TrimSpace(text))
	if upper == "[SILENT]" || upper == "SILENT" || upper == "NO_REPLY" || upper == "[NO_REPLY]" {
		return true
	}
	return trig.Policy == "candidate" && (upper == "IGNORE" || upper == "[IGNORE]")
}

func (e *Engine) handleAPIRouterFailure(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, configEpoch uint64, step int, modelName, effort string, err error) {
	msg := err.Error()
	elapsedMS := int64(0)
	if turn != nil && !turn.startedAt.IsZero() {
		elapsedMS = time.Since(turn.startedAt).Milliseconds()
	}
	e.logger.Warn("api router turn failed",
		"trigger", trig.ID, "thread", turn.thread, "step", step,
		"model", modelName, "effort", effort, "elapsed_ms", elapsedMS,
		"mutated", turn.hasMutated(), "err", msg)

	if turn.hasMutated() {
		// Never replay after a side-effect already landed; surface concise partial progress.
		e.ackAPITriggerOnce(ctx, turn, trig)
		if !turn.hasReplied() {
			ops := turn.opsText()
			summary := fmt.Sprintf("已完成部分操作（%s），后续模型调用中断：%s", orStr(ops, "已执行变更"), oneLineClip(msg, 120))
			e.addEvent(ctx, turn.thread, summary, trig.ID, "error")
		}
		return
	}

	if errors.Is(err, ErrLLMAuth) {
		e.mu.Lock()
		if e.apiConfigEpoch != configEpoch {
			e.mu.Unlock()
			return // stale auth failure from a superseded key/config; do not pause the new key
		}
		prevErr := e.apiLastErr
		e.apiAuthPaused = true
		e.apiLastErr = msg
		e.mu.Unlock()
		if prevErr != msg {
			e.addEvent(ctx, turn.thread, "原生 API 大脑鉴权失败（已暂停重试，请检查设置中的 Key）："+oneLineClip(msg, 140), trig.ID, "error")
		}
		return
	}

	if errors.Is(err, ErrLLMBadRequest) {
		// Finish input-specific 400 errors (e.g. image on text-only model) so one bad message never wedges the queue.
		e.ackAPITriggerOnce(ctx, turn, trig)
		e.addEvent(ctx, turn.thread, "本条消息模型请求被拒绝（已跳过以免阻塞后续对话）："+oneLineClip(msg, 140), trig.ID, "error")
		return
	}

	e.mu.Lock()
	if e.apiConfigEpoch != configEpoch {
		e.mu.Unlock()
		return
	}
	if e.apiLastFailTriggerID == trig.ID {
		e.apiSameTriggerFails++
	} else {
		e.apiLastFailTriggerID = trig.ID
		e.apiSameTriggerFails = 1
	}
	if e.apiSameTriggerFails >= maxSameAPITriggerFails {
		e.apiSameTriggerFails = 0
		e.apiLastFailTriggerID = ""
		e.apiFailCount = 0
		e.apiBackoffUntil = time.Time{}
		e.apiLastErr = msg
		e.mu.Unlock()
		e.ackAPITriggerOnce(ctx, turn, trig)
		e.addEvent(ctx, turn.thread, fmt.Sprintf("本条消息连续 %d 次调用模型失败（已跳过以免阻塞后续对话，可重新发送）：%s", maxSameAPITriggerFails, oneLineClip(msg, 120)), trig.ID, "error")
		return
	}
	e.apiFailCount++
	fails := e.apiFailCount
	backoff := 5 * time.Second
	for i := 1; i < fails && backoff < 2*time.Minute; i++ {
		backoff *= 2
	}
	if backoff > 2*time.Minute {
		backoff = 2 * time.Minute
	}
	e.apiBackoffUntil = time.Now().Add(backoff)
	e.apiLastErr = msg
	e.mu.Unlock()
	if fails == 1 || fails == 3 {
		e.addEvent(ctx, turn.thread, fmt.Sprintf("原生 API 大脑暂不可用（%s 后自动重试）：%s", backoff.Round(time.Second), oneLineClip(msg, 120)), trig.ID, "error")
	}
}

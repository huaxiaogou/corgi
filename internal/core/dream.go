package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

const (
	dreamSkillsPrefix   = "可做成 skill："
	dreamFixPrefix      = "可改进 skill："
	activeDreamKVKey    = "active_dream"
	nightlyDreamSlotKey = "nightly_dream_slot"
)

type activeDreamMarker struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	Reason      string    `json:"reason"`
	Mode        string    `json:"mode,omitempty"` // "api" | "pty" (empty = legacy pty)
	ArchivedRef string    `json:"archived_ref,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	PaneID      string    `json:"pane_id,omitempty"`
	PromptSent  bool      `json:"prompt_sent,omitempty"`
}

type nightlyDreamSlot struct {
	Date    string    `json:"date"`    // YYYY-MM-DD of scheduled dream slot
	Outcome string    `json:"outcome"` // ok | skipped | failed
	At      time.Time `json:"at"`
}

type dreamRun struct {
	id             string
	at             time.Time
	observedAt     time.Time
	mode           string
	started        bool
	worked         bool
	recovered      bool
	apiDone        bool
	apiOK          bool
	apiErr         string
	apiErrIsAuth   bool
	step           int
	lastTool       string
	lastProgressAt time.Time
	cancel         context.CancelFunc
	idleAt         time.Time
	reason         string
	blockedAt      time.Time
	told           bool
	archivedRef    string
	workspaceID    string
	paneID         string
}

func dreamWorkspaceLabel(runID string) string {
	if strings.TrimSpace(runID) == "" {
		return model.DreamLabel
	}
	return model.DreamLabel + " · " + runID
}

func isDreamWorkspaceLabel(label string) bool {
	return label == model.DreamLabel || strings.HasPrefix(label, model.DreamLabel+" · ")
}

func (e *Engine) loadActiveDreamMarker(ctx context.Context) {
	if e.store == nil {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, activeDreamKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var m activeDreamMarker
	if err := json.Unmarshal([]byte(raw), &m); err != nil || strings.TrimSpace(m.ID) == "" || m.At.IsZero() {
		return
	}
	e.mu.Lock()
	e.dream = &dreamRun{
		id:          m.ID,
		at:          m.At,
		observedAt:  time.Now(),
		mode:        m.Mode,
		started:     m.PromptSent,
		recovered:   true,
		reason:      m.Reason,
		archivedRef: m.ArchivedRef,
		workspaceID: m.WorkspaceID,
		paneID:      m.PaneID,
	}
	e.mu.Unlock()
	if e.logger != nil {
		e.logger.Info("loaded active dream marker for restart recovery", "id", m.ID, "mode", m.Mode, "at", m.At.Format(time.RFC3339), "prompt_sent", m.PromptSent)
	}
}

func (e *Engine) saveActiveDreamMarker(ctx context.Context, d *dreamRun) error {
	if e.store == nil || d == nil || d.id == "" {
		return nil
	}
	e.mu.Lock()
	m := activeDreamMarker{
		ID:          d.id,
		At:          d.at,
		Reason:      d.reason,
		Mode:        d.mode,
		ArchivedRef: d.archivedRef,
		WorkspaceID: d.workspaceID,
		PaneID:      d.paneID,
		PromptSent:  d.started,
	}
	e.mu.Unlock()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, activeDreamKVKey, string(b))
}

func (e *Engine) recordNightlyDreamSlot(ctx context.Context, slotDate, outcome string, at time.Time) {
	if e.store == nil {
		return
	}
	b, err := json.Marshal(nightlyDreamSlot{Date: slotDate, Outcome: outcome, At: at})
	if err != nil {
		return
	}
	_ = e.store.SetKV(ctx, nightlyDreamSlotKey, string(b))
}

func (e *Engine) readNightlyDreamSlot(ctx context.Context) (nightlyDreamSlot, bool) {
	if e.store == nil {
		return nightlyDreamSlot{}, false
	}
	raw, ok, err := e.store.GetKV(ctx, nightlyDreamSlotKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return nightlyDreamSlot{}, false
	}
	var s nightlyDreamSlot
	if err := json.Unmarshal([]byte(raw), &s); err != nil || s.Date == "" {
		return nightlyDreamSlot{}, false
	}
	return s, true
}

// Dreaming reports whether an isolated memory-consolidation Dream session is active.
func (e *Engine) Dreaming() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dream != nil
}

// isBlockingDream reports whether an active Dream session suspends the main Router loop
// (true for PTY Dream which closes the local PTY Router; false for async in-process API Dream).
func (e *Engine) isBlockingDream() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dream != nil && e.dream.mode != "api"
}

// LastDream returns the timestamp of the last completed dream run (zero if never).
func (e *Engine) LastDream(ctx context.Context) time.Time {
	raw, ok, err := e.store.GetKV(ctx, "last_dream")
	if err != nil || !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *Engine) setLastDream(ctx context.Context, t time.Time) {
	_ = e.store.SetKV(ctx, "last_dream", t.Format(time.RFC3339))
}

func (e *Engine) dreamAtLocked() (h, m int, ok bool) {
	v := strings.TrimSpace(e.settings.DreamAt)
	if v == "" {
		v = "04:00"
	}
	if strings.EqualFold(v, "off") {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 4, 0, true
	}
	return h, m, true
}

// NextDream computes the next scheduled nightly dream time (zero when disabled).
func (e *Engine) NextDream(ctx context.Context, now time.Time) time.Time {
	e.mu.Lock()
	h, m, ok := e.dreamAtLocked()
	e.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	last := e.LastDream(ctx)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !last.Before(t) || now.After(t.Add(3*time.Hour)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func (e *Engine) hasActivitySince(ctx context.Context, since time.Time) (bool, error) {
	entries, _, err := e.store.ListEntries(ctx, "", 60)
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i]
		if !since.IsZero() && en.At.Before(since) {
			return false, nil
		}
		if en.Role == "user" || en.Role == "corgi" {
			return true, nil
		}
	}
	return false, nil
}

// checkDream drives an active dream session or initiates the scheduled nightly dream.
func (e *Engine) checkDream(ctx context.Context, live map[string]liveInfo, hasQueued bool) {
	e.checkDreamWithLocal(ctx, live, hasQueued, true)
}

func (e *Engine) checkDreamWithLocal(ctx context.Context, live map[string]liveInfo, hasQueued bool, localScanned bool) {
	e.mu.Lock()
	d := e.dream
	e.mu.Unlock()
	if d != nil {
		e.driveDream(ctx, d, live, localScanned)
		return
	}
	now := time.Now()
	next := e.NextDream(ctx, now)
	if next.IsZero() || now.Before(next) || now.After(next.Add(3*time.Hour)) {
		return
	}
	li, alive := live[model.RouterName]
	e.mu.Lock()
	apiMode := e.settings.EffectiveRouterMode() == "api"
	busy := e.handover != nil || e.hubUpdating || e.routerDispatching || e.routerLaunching || e.apiTurn != nil || e.settings.Paused || hasQueued || !e.routerAskedAt.IsZero() ||
		(!apiMode && alive && li.status != "idle" && li.status != "done")
	e.mu.Unlock()
	if busy || (!apiMode && !localScanned) {
		return
	}
	active, err := e.hasActivitySince(ctx, e.LastDream(ctx))
	if err != nil {
		e.logger.Warn("dream activity check failed", "err", err)
		return
	}
	if !active {
		e.setLastDream(ctx, now) // quiet day: skip waking LLM, zero tokens spent
		e.recordNightlyDreamSlot(ctx, next.Format("2006-01-02"), "skipped", now)
		return
	}
	_ = e.StartDream(ctx, "nightly")
}

// StartDream starts an isolated Dream session (in-process API Dream in api mode, or local PTY
// Dream in pty mode) to review recent history, consolidate memory/, and propose mature skills.
func (e *Engine) StartDream(ctx context.Context, reason string) error {
	e.apiSwitchMu.Lock()
	defer e.apiSwitchMu.Unlock()

	now := time.Now()
	e.mu.Lock()
	st := e.settings
	mode := st.EffectiveRouterMode()
	if e.dream != nil || e.handover != nil || e.hubUpdating || (mode != "api" && e.apiTurn != nil) {
		e.mu.Unlock()
		return fmt.Errorf("Corgi 正在整理记忆、交接会话或升级中")
	}
	if mode != "api" && e.fleet == nil {
		e.mu.Unlock()
		return fmt.Errorf("本机终端服务未连接，无法启动 PTY 做梦")
	}
	d := &dreamRun{
		id:             now.Format("20060102-150405.000000000"),
		at:             now,
		observedAt:     now,
		lastProgressAt: now,
		mode:           mode,
		reason:         reason,
	}
	e.dream = d
	e.mu.Unlock()

	// Persist active dream intent BEFORE any workspace/Router side effects so a crash
	// or restart can deterministically reconcile this exact run.
	if err := e.saveActiveDreamMarker(ctx, d); err != nil {
		e.mu.Lock()
		if e.dream == d {
			e.dream = nil
		}
		e.mu.Unlock()
		return fmt.Errorf("记录做梦状态失败：%w", err)
	}

	e.addEvent(ctx, model.MainThread, "Corgi 睡一会儿，复盘近期对话与日志、整理记忆（做梦）", "", "info")
	e.notifyChanged()

	if mode == "api" {
		go e.runAPIDream(d, st)
	} else {
		go e.launchDreamAsync(d, st)
	}
	return nil
}

func (e *Engine) launchDreamAsync(d *dreamRun, st model.Settings) {
	bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fail := func(err error) {
		e.logger.Warn("dream launch failed", "id", d.id, "err", err)
		e.closeDreamWorkspaces(bg, d)
		updates := map[string]string{}
		if d.reason == "nightly" {
			now := time.Now()
			updates["last_dream"] = now.Format(time.RFC3339)
			if b, jerr := json.Marshal(nightlyDreamSlot{Date: d.at.Format("2006-01-02"), Outcome: "failed", At: now}); jerr == nil {
				updates[nightlyDreamSlotKey] = string(b)
			}
		}
		if e.store != nil && d.id != "" {
			_, _ = e.store.CommitActiveDreamFinish(bg, d.id, updates)
		}
		e.mu.Lock()
		if e.dream == d {
			e.dream = nil
		}
		e.mu.Unlock()
		e.addEvent(bg, model.MainThread, "整理记忆未能启动："+err.Error(), "", "error")
		e.notifyChanged()
	}

	// Archive current Router session so writeDreamInput includes it.
	if live, err := e.fleet.Agents(bg); err == nil {
		for _, a := range live {
			if a.Name != nil && *a.Name == model.RouterName && isHubLocalPane(a.PaneID) && a.Session != nil {
				sid := extractSessionID(a.Session.Kind, a.Session.Value)
				if sid != "" {
					kind := agent.NormalizeKind(st.RouterKind)
					if a.Agent != nil && *a.Agent != "" {
						if nk := agent.NormalizeKind(*a.Agent); nk != "" {
							kind = nk
						}
					}
					eid, _, _ := e.store.NextID(bg, "h")
					_ = e.store.SaveEndedSession(bg, &model.EndedSession{
						ID: eid, Kind: kind, Cwd: e.memory.RouterDir(), Name: model.RouterName,
						Session: sid, EndedAt: time.Now(),
						Title: "Corgi · 做梦前的会话（" + time.Now().Format("01-02 15:04") + "）",
					}, 40)
					e.mu.Lock()
					if e.dream == d {
						d.archivedRef = "h:" + eid
					}
					e.mu.Unlock()
					_ = e.saveActiveDreamMarker(bg, d)
				}
				break
			}
		}
	}

	e.closeWorkspacesByLabel(bg, model.RouterLabel)
	e.closeDreamWorkspaces(bg, nil)

	if err := e.memory.WriteRouterDocs(e.binPath, st); err != nil {
		e.logger.Warn("write router docs before dream", "err", err)
	}
	if err := e.writeDreamInput(bg); err != nil {
		fail(err)
		return
	}

	kind := agent.NormalizeKind(st.RouterKind)
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		fail(err)
		return
	}
	if !st.AskPermissions {
		_ = ad.PreTrustWorkspace(e.home, e.memory.RouterDir())
	}

	// Prefer the stronger Worker model/effort for nightly judgment when sharing the same agent kind.
	dreamModel, dreamEffort := st.RouterModel, st.RouterEffort
	if st.WorkerModel != "" && agent.NormalizeKind(st.WorkerKind) == kind {
		dreamModel, dreamEffort = st.WorkerModel, st.WorkerEffort
	}
	args := ad.LaunchArgs(dreamModel, dreamEffort, !st.AskPermissions)

	wsID, paneID, err := e.fleet.CreateWorkspace(bg, "", e.memory.RouterDir(), dreamWorkspaceLabel(d.id))
	if err != nil {
		fail(err)
		return
	}
	e.mu.Lock()
	if e.dream == d {
		d.workspaceID = wsID
		d.paneID = paneID
	}
	e.mu.Unlock()
	_ = e.saveActiveDreamMarker(bg, d)

	if err := e.fleet.StartAgent(bg, paneID, kind, model.DreamName, args); err != nil {
		var he *herdr.Error
		if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			_ = e.fleet.CloseWorkspace(bg, wsID)
			fail(err)
			return
		}
	}
	e.fleet.RememberName(paneID, model.DreamName)

	prompt := strings.NewReplacer(
		"{{DATE}}", journalName(d.at),
		"{{BIN}}", e.binPath,
		"{{BUDGET}}", fmt.Sprint(UpFrontBudgetBytes>>10),
	).Replace(dreamPromptTemplate)

	deadline := time.Now().Add(90 * time.Second)
	for {
		err := e.fleet.Prompt(bg, model.DreamName, prompt)
		var he *herdr.Error
		if err == nil {
			break
		}
		if !errors.As(err, &he) || (he.Code != "agent_not_ready" && he.Code != "agent_not_found") || time.Now().After(deadline) {
			e.closeDreamWorkspaces(context.Background(), d)
			fail(err)
			return
		}
		select {
		case <-bg.Done():
			fail(bg.Err())
			return
		case <-time.After(2 * time.Second):
		}
	}

	e.mu.Lock()
	if e.dream == d {
		e.dream.started = true
	}
	e.mu.Unlock()
	_ = e.saveActiveDreamMarker(bg, d)
}

func (e *Engine) findDreamLive(d *dreamRun, live map[string]liveInfo) (liveInfo, bool) {
	if li, ok := live[model.DreamName]; ok {
		return li, true
	}
	e.mu.Lock()
	wantPane := d.paneID
	e.mu.Unlock()
	if wantPane != "" {
		for _, li := range live {
			if li.pane == wantPane {
				return li, true
			}
		}
	}
	return liveInfo{}, false
}

func (e *Engine) driveDream(ctx context.Context, d *dreamRun, live map[string]liveInfo, localScanned bool) {
	e.mu.Lock()
	mode := d.mode
	started := d.started
	recovered := d.recovered
	observedAt := d.observedAt
	apiDone := d.apiDone
	apiOK := d.apiOK
	e.mu.Unlock()
	if observedAt.IsZero() {
		observedAt = d.at
	}

	journal := filepath.Join(e.memory.DreamsDir(), journalName(d.at)+".md")
	journalFresh := false
	if fi, err := os.Stat(journal); err == nil && !fi.ModTime().Before(d.at) && fi.Size() > 0 {
		journalFresh = true
	}

	if mode == "api" {
		if recovered {
			e.finishDream(ctx, d, journalFresh)
			return
		}
		if apiDone {
			e.finishDream(ctx, d, apiOK)
			return
		}
		// Async API Dream has no arbitrary wall-clock cap; runAPIDream monitors SSE stream
		// liveness via idle watchdog and sets apiDone on completion or fault.
		return
	}

	if !localScanned || e.fleet == nil {
		if recovered && e.fleet == nil {
			e.finishDream(ctx, d, journalFresh)
		}
		return
	}

	li, alive := e.findDreamLive(d, live)
	if !started {
		if recovered {
			// Crashed between StartDream and saving PromptSent: reconcile safely against local herdr.
			if alive {
				if li.status == "working" {
					e.mu.Lock()
					d.started = true
					d.worked = true
					e.mu.Unlock()
					_ = e.saveActiveDreamMarker(ctx, d)
					goto statusCheck
				}
				if journalFresh {
					e.finishDream(ctx, d, true)
					return
				}
				if time.Since(observedAt) > 90*time.Second {
					e.finishDream(ctx, d, false)
				}
				return
			}
			// Verify local herdr workspace list before concluding pane is gone.
			if _, err := e.fleet.LocalWorkspaces(ctx); err != nil {
				return
			}
			e.finishDream(ctx, d, journalFresh)
			return
		}
		if time.Since(d.at) > 5*time.Minute {
			e.finishDream(ctx, d, false)
		}
		return
	}

	if alive && li.status == "blocked" {
		if !e.Settings().AskPermissions {
			e.trustOwnDir(ctx, li.pane)
		}
		e.mu.Lock()
		if d.blockedAt.IsZero() {
			d.blockedAt = time.Now()
		} else if !d.told && time.Since(d.blockedAt) > time.Minute {
			d.told = true
			e.mu.Unlock()
			e.addEvent(ctx, model.MainThread, "Corgi 整理记忆时停在一个确认上，请打开查看", model.DreamName, "blocked")
			e.notifyChanged()
			goto statusCheck
		}
		e.mu.Unlock()
	} else {
		e.mu.Lock()
		d.blockedAt = time.Time{}
		e.mu.Unlock()
	}

statusCheck:
	e.mu.Lock()
	switch {
	case alive && li.status == "working":
		d.worked = true
		d.idleAt = time.Time{}
	case alive && (li.status == "idle" || li.status == "done") && d.idleAt.IsZero():
		d.idleAt = time.Now()
	}
	worked, idleAt := d.worked, d.idleAt
	e.mu.Unlock()

	// Measure timeout from observedAt so Hub downtime during restart does not prematurely abort a still-running Dream.
	wait := time.Since(observedAt)
	switch {
	case worked && !idleAt.IsZero() && time.Since(idleAt) > 6*time.Second:
		e.finishDream(ctx, d, true)
	case !worked && journalFresh && !idleAt.IsZero() && time.Since(idleAt) > 10*time.Second:
		e.finishDream(ctx, d, true)
	case !alive && (worked || recovered), wait > 25*time.Minute, !worked && !idleAt.IsZero() && time.Since(idleAt) > 2*time.Minute:
		e.finishDream(ctx, d, worked || journalFresh)
	}
}

func (e *Engine) closeDreamWorkspaces(ctx context.Context, d *dreamRun) {
	if e.fleet == nil {
		return
	}
	closed := map[string]bool{}
	if d != nil {
		e.mu.Lock()
		wsID := d.workspaceID
		runID := d.id
		e.mu.Unlock()
		if wsID != "" {
			_ = e.fleet.CloseWorkspace(ctx, wsID)
			closed[wsID] = true
		}
		if ws, err := e.fleet.LocalWorkspaces(ctx); err == nil {
			wantLabel := dreamWorkspaceLabel(runID)
			for _, w := range ws {
				if !closed[w.ID] && (w.Label == wantLabel || (runID == "" && isDreamWorkspaceLabel(w.Label))) {
					_ = e.fleet.CloseWorkspace(ctx, w.ID)
					closed[w.ID] = true
				}
			}
		}
		return
	}
	// When d == nil (cleanup before a fresh Dream launch), close any legacy or leftover local Dream workspaces.
	if ws, err := e.fleet.LocalWorkspaces(ctx); err == nil {
		for _, w := range ws {
			if isDreamWorkspaceLabel(w.Label) {
				_ = e.fleet.CloseWorkspace(ctx, w.ID)
			}
		}
	}
}

func (e *Engine) isLocalRouterAlive(ctx context.Context) bool {
	if e.fleet == nil {
		return false
	}
	ags, _, err := e.fleet.AgentsWithScanned(ctx)
	if err != nil {
		return false
	}
	for _, a := range ags {
		if a.Name != nil && *a.Name == model.RouterName && !strings.Contains(a.PaneID, "@") {
			return true
		}
	}
	return false
}

func (e *Engine) finishDream(ctx context.Context, d *dreamRun, ok bool) {
	now := time.Now()
	kvUpdates := map[string]string{}
	if ok || d.reason == "nightly" {
		// Always advance last_dream on nightly runs so a failed dream cannot spin in a 2-minute loop.
		// For a late recovery (>3h after the original slot), use the original run time so it does not consume a newer night.
		watermark := now
		if d.recovered && time.Since(d.at) > 3*time.Hour {
			watermark = d.at
		}
		kvUpdates["last_dream"] = watermark.Format(time.RFC3339)
	}
	if d.reason == "nightly" {
		outcome := "failed"
		if ok {
			outcome = "ok"
		}
		if b, err := json.Marshal(nightlyDreamSlot{Date: d.at.Format("2006-01-02"), Outcome: outcome, At: now}); err == nil {
			kvUpdates[nightlyDreamSlotKey] = string(b)
		}
	}

	if d.id != "" && e.store != nil {
		committed, err := e.store.CommitActiveDreamFinish(ctx, d.id, kvUpdates)
		if err != nil {
			e.logger.Warn("commit dream finish failed, will retry", "id", d.id, "err", err)
			return
		}
		if !committed {
			e.mu.Lock()
			if e.dream == d {
				e.dream = nil
			}
			e.mu.Unlock()
			return
		}
	} else {
		e.mu.Lock()
		if e.dream != d {
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
		if e.store != nil {
			for k, v := range kvUpdates {
				_ = e.store.SetKV(ctx, k, v)
			}
		}
	}

	if d.mode != "api" {
		e.closeDreamWorkspaces(ctx, d)
	}
	summary, skills, fixes := "", "", ""
	if ok {
		journal := filepath.Join(e.memory.DreamsDir(), journalName(d.at)+".md")
		summary = parseDreamSummary(journal)
		skills = parseDreamPrefixLine(journal, dreamSkillsPrefix)
		fixes = parseDreamPrefixLine(journal, dreamFixPrefix)
	}

	e.mu.Lock()
	if e.dream == d {
		e.dream = nil
	}
	e.routerLaunchAt = time.Time{}
	archivedRef := d.archivedRef
	apiErr := d.apiErr
	apiErrIsAuth := d.apiErrIsAuth
	stepCount := d.step
	lastTool := d.lastTool
	recovered := d.recovered
	runCtx := e.runCtx
	e.mu.Unlock()

	switch {
	case !ok && apiErr != "":
		e.addEvent(ctx, model.MainThread, "本次整理记忆未能完成（"+apiErr+"），Corgi 先醒过来了", archivedRef, "error")
	case !ok:
		e.addEvent(ctx, model.MainThread, "本次整理记忆未能完成，Corgi 先醒过来了", archivedRef, "error")
	case summary != "":
		e.addEvent(ctx, model.MainThread, "Corgi 醒了。记忆整理："+summary, archivedRef, "info")
	default:
		e.addEvent(ctx, model.MainThread, "Corgi 醒了，记忆已整理完毕", archivedRef, "info")
	}

	// Report Dream anomalies to the Brain (Router) so it can decide whether to handle
	// automatically (schedule retry / note / ignore) or ask the user to intervene.
	if !ok && !apiErrIsAuth && (runCtx == nil || runCtx.Err() == nil) {
		canNotifyBrain := d.mode != "api" || e.hasConfiguredRouterAPIKey(ctx, e.Settings())
		if canNotifyBrain && e.store != nil {
			errDesc := orStr(apiErr, "未落盘做梦日志")
			if recovered && apiErr == "" {
				errDesc = "Hub 重启导致后台做梦中断，未完成落盘"
			}
			elapsed := time.Since(d.at).Round(time.Second)
			if elapsed < 0 {
				elapsed = 0
			}
			progress := fmt.Sprintf("第 %d 步", stepCount)
			if lastTool != "" {
				progress += "（最后工具：" + lastTool + "）"
			}
			report := fmt.Sprintf(
				"[做梦异常报告] 后台记忆整理（模式=%s，触发=%s，已运行=%s，进度=%s）未能正常完成。\n"+
					"异常原因：%s\n"+
					"请你作为大脑综合判断：\n"+
					"- 若属临时网络/上游波动且无需打扰主人，可自行决定设提醒延后重跑或直接输出 [SILENT] 忽略；\n"+
					"- 若需要主人介入（如模型异常、配额/配置问题、记忆文件异常或连续失败），请用 reply_user 简洁向主人说明原因与建议方案（必要时发 attention 决策卡）。",
				orStr(d.mode, "pty"), orStr(d.reason, "auto"), elapsed, progress, errDesc,
			)
			_, _ = e.Ingest(ctx, "system", model.MainThread, "dream", report, "", model.RouterName, d.id)
		}
	}

	if skills != "" {
		_, _, _, _ = e.Reply(ctx, model.MainThread,
			"整理记忆时发现这些流程已经做熟，可沉淀为 skill 供所有 Agent 复用："+skills+"。要不要做成 skill？",
			"", []string{"做成个人 skill", "放到项目仓库", "先不用"}, false)
	}
	if fixes != "" {
		_, _, _, _ = e.Reply(ctx, model.MainThread,
			"整理记忆时发现已有 skill 存在卡点："+fixes+"。要不要派 Agent 去修？",
			"", []string{"派人去修", "先不用"}, false)
	}
	e.notifyChanged()
	if !e.isAPIRouterMode() && !e.isLocalRouterAlive(ctx) {
		e.ensureRouter(ctx)
	}
}

// writeDreamInput writes dream/input.md summarizing chats and archived Router logs since last dream.
func (e *Engine) writeDreamInput(ctx context.Context) error {
	since := e.LastDream(ctx)
	var b strings.Builder
	sinceStr := "never"
	if !since.IsZero() {
		sinceStr = since.Format("2006-01-02 15:04")
	}
	fmt.Fprintf(&b, "# What happened since the last dream (%s)\n\n", sinceStr)

	b.WriteString("## Authoritative Open Work Items (Hub SQLite)\n\n")
	if openWork, err := e.store.ListWorkItems(ctx, false); err != nil {
		fmt.Fprintf(&b, "(error reading work_items: %v)\n", err)
	} else if len(openWork) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, wi := range openWork {
			fmt.Fprintf(&b, "- %s [%s] %s", wi.ID, wi.Thread, wi.Title)
			if wi.NextStep != "" {
				fmt.Fprintf(&b, " | next: %s", wi.NextStep)
			}
			if len(wi.Workers) > 0 {
				fmt.Fprintf(&b, " | workers: %s", strings.Join(wi.Workers, ","))
			}
			if len(wi.Branches) > 0 {
				fmt.Fprintf(&b, " | branches: %s", strings.Join(wi.Branches, ","))
			}
			if len(wi.Artifacts) > 0 {
				fmt.Fprintf(&b, " | delivered_files: %d", len(wi.Artifacts))
			}
			b.WriteByte('\n')
		}
	}
	if pendingActs, err := e.store.ListPendingActions(ctx); err == nil && len(pendingActs) > 0 {
		b.WriteString("\n## Pending User Actions\n\n")
		for _, a := range pendingActs {
			fmt.Fprintf(&b, "- %s (%s) %s\n", a.ID, a.Kind, oneLineClip(a.Title, 120))
		}
	}
	b.WriteString("\n## Chat & event log\n\n")

	entries, _, _ := e.store.ListEntries(ctx, "", 500)
	var lines []string
	for _, en := range entries {
		if !since.IsZero() && en.At.Before(since) {
			continue
		}
		who := en.Role
		switch en.Role {
		case "user":
			who = "human"
		case "corgi":
			who = "Corgi"
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] %s: %s",
			en.At.Format("01-02 15:04"), en.Thread, who, oneLineClip(en.Text, 600)))
	}
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n\n## Corgi's own sessions (reasoning & commands)\n")

	ended, _ := e.store.ListEndedSessions(ctx, 40)
	var routerSess []*model.EndedSession
	for _, h := range ended {
		if h.Name == model.RouterName && (since.IsZero() || h.EndedAt.After(since)) {
			routerSess = append(routerSess, h)
		}
	}
	sort.Slice(routerSess, func(i, j int) bool { return routerSess[i].EndedAt.Before(routerSess[j].EndedAt) })

	const budget = 240 << 10
	var parts []string
	for _, h := range routerSess {
		if e.agents == nil {
			break
		}
		ad, err := e.agents.Get(h.Kind)
		if err != nil {
			continue
		}
		path := ad.LocateTranscript(e.home, h.Cwd, h.Session, h.EndedAt.Add(-24*time.Hour))
		if path == "" {
			continue
		}
		tr := agent.NewTranscriptReader(path, ad, "")
		tr.Poll()
		var p strings.Builder
		fmt.Fprintf(&p, "\n### %s (ended %s)\n\n", h.Title, h.EndedAt.Format("01-02 15:04"))
		for _, m := range tr.Messages() {
			switch m.Kind {
			case "user":
				fmt.Fprintf(&p, "IN: %s\n\n", oneLineClip(m.Text, 1200))
			case "assistant":
				fmt.Fprintf(&p, "CORGI: %s\n\n", oneLineClip(m.Text, 1200))
			case "tool":
				fmt.Fprintf(&p, "TOOL %s: %s → %s\n\n", m.Name, oneLineClip(m.Input, 260), oneLineClip(m.Output, 260))
			}
		}
		parts = append(parts, p.String())
	}

	total, keep := 0, 0
	for i := len(parts) - 1; i >= 0; i-- {
		if total+len(parts[i]) > budget && keep > 0 {
			break
		}
		total += len(parts[i])
		keep++
	}
	if keep < len(parts) {
		fmt.Fprintf(&b, "\n(%d older sessions omitted for budget.)\n", len(parts)-keep)
	}
	for _, p := range parts[len(parts)-keep:] {
		if len(p) > budget {
			p = "…(truncated)…\n" + p[len(p)-budget:]
		}
		b.WriteString(p)
	}
	if len(parts) == 0 {
		b.WriteString("\n(no archived router transcripts found)\n")
	}

	dir := filepath.Join(e.memory.RouterDir(), "dream")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir dream dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "input.md"), []byte(b.String()), 0o600)
}

func journalName(at time.Time) string { return at.Format("2006-01-02-1504") }

func parseDreamPrefixLine(path, prefix string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for i, l := range strings.Split(string(b), "\n") {
		if i > 8 {
			break
		}
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), ">-* "))
		if s, ok := strings.CutPrefix(l, prefix); ok {
			return strings.TrimRight(strings.TrimSpace(s), "。.")
		}
	}
	return ""
}

func parseDreamSummary(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, dreamSkillsPrefix) || strings.HasPrefix(t, dreamFixPrefix) {
			continue
		}
		t = strings.TrimSpace(strings.TrimLeft(t, ">-* "))
		if t != "" {
			return oneLineClip(t, 160)
		}
	}
	return ""
}

const dreamPromptTemplate = `[corgi] Dream. You are Corgi's memory keeper running while Corgi sleeps. Read CORGI.md for context and memory/ layout. Inputs in this directory:
- memory/: Corgi's layered memory.
- dream/input.md: recent conversations and Corgi's archived session transcripts since the last dream.
- dreams/: earlier dream journals, if any.
Run ` + "`{{BIN}} list`" + ` to check which workers and timers are still active.

Consolidate memory/ and keep up-front files (instructions.md, notes/index.md) under ~{{BUDGET}} KB combined:
1. Single-source open-task ledger: Hub SQLite work_items is the sole open-task ledger. Do NOT create or maintain open task checklists under tasks/private/ or tasks/groups/. If legacy notes still exist under tasks/private/ or tasks/groups/ whose Hub work_item is explicitly CLOSED (absent from "Authoritative Open Work Items" in dream/input.md and ` + "`{{BIN}} list`" + `) AND no active worker, unmerged branch, or pending user action remains, move them to tasks/done/ (distilling any reusable knowledge into notes/). Moving a .md file NEVER closes a Hub SQLite work_item; never treat a local prototype/build without confirmed delivery as done.
2. Rewrite instructions.md cleanly: one standing preference/rule per line (≤80 Chinese chars); move procedures/history into notes/ with a pointer ("见 notes/x.md"). Remove recurring schedules already backed by an active timer + notes/ workflow; merge duplicates.
3. Capture cross-session patterns seen more than once (repeated corrections, implicit preferences, recurring mistakes) into instructions.md or notes/, marked "(inferred)".
4. File reusable engineering findings (data paths, working commands, pitfalls, agent/model track records in notes/agents.md) into notes/<topic>.md and keep notes/index.md accurate. Store pointers, not copies.
5. Skills review:
   - If a reusable workflow in notes/ has been used ≥3 times, stabilized, and has no skill yet, list it on a line starting "可做成 skill：" separated by 、.
   - If an existing skill caused workarounds or failures in logs, list it on a line starting "可改进 skill：" as "name（修复点）" separated by 、.
   Place these lines directly under the journal's first summary line (omit if none).
6. Preserve every explicit human instruction unless superseded by a newer one. memory/messages.md is hub-managed (read-only).

Write dreams/{{DATE}}.md:
- Line 1: one Chinese sentence summarizing what you changed (shown to the human).
- Next lines (optional): skill suggestion lines above.
- Remaining (<30 lines): concise log of edits and noticed patterns.

Edit files only, then stop.`

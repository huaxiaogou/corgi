package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

const autoUpdateAttemptsKVKey = "auto_update_attempts"

var (
	nodeReconnectTimeout = 25 * time.Second
	autoUpdateRetryCap   = 24 * time.Hour
)

type updateTarget struct {
	ID    string
	Name  string
	Local bool
}

// FleetUpdateView is the JSON snapshot exposed in GET /api/version and POST /api/update/fleet.
type FleetUpdateView struct {
	Busy         bool              `json:"busy"`
	Scope        string            `json:"scope,omitempty"` // fleet | auto
	Phase        string            `json:"phase"`           // idle | updating | reconnecting | restarting | done | failed
	Target       string            `json:"target,omitempty"`
	Total        int               `json:"total"`
	Index        int               `json:"index"`
	CurrentID    string            `json:"current_id,omitempty"`
	CurrentName  string            `json:"current_name,omitempty"`
	CompletedIDs []string          `json:"completed_ids,omitempty"`
	PendingIDs   []string          `json:"pending_ids,omitempty"`
	FailedID     string            `json:"failed_id,omitempty"`
	FailedName   string            `json:"failed_name,omitempty"`
	Error        string            `json:"error,omitempty"`
	Errors       map[string]string `json:"errors,omitempty"`
}

type fleetUpdateState struct {
	view     FleetUpdateView
	plan     []updateTarget
	pending  []updateTarget
	upgraded []string
}

func (s *Server) isFleetUpdateBusy() bool {
	s.fleetUpdateMu.Lock()
	defer s.fleetUpdateMu.Unlock()
	return s.fleetUpdate != nil && s.fleetUpdate.view.Busy
}

func cloneFleetUpdateView(v FleetUpdateView) FleetUpdateView {
	if len(v.CompletedIDs) > 0 {
		v.CompletedIDs = append([]string(nil), v.CompletedIDs...)
	} else {
		v.CompletedIDs = nil
	}
	if len(v.PendingIDs) > 0 {
		v.PendingIDs = append([]string(nil), v.PendingIDs...)
	} else {
		v.PendingIDs = nil
	}
	if len(v.Errors) > 0 {
		cp := make(map[string]string, len(v.Errors))
		for k, val := range v.Errors {
			cp[k] = val
		}
		v.Errors = cp
	} else {
		v.Errors = nil
	}
	return v
}

func (s *Server) fleetUpdateSnapshot() FleetUpdateView {
	s.fleetUpdateMu.Lock()
	defer s.fleetUpdateMu.Unlock()
	if s.fleetUpdate == nil {
		return FleetUpdateView{Phase: "idle"}
	}
	return cloneFleetUpdateView(s.fleetUpdate.view)
}

func (s *Server) notifyFleetUpdateChanged() {
	if s.Bus != nil {
		s.Bus.OnStateChanged()
	}
}

// handleFleetUpdate starts (or returns status of) the Hub-owned sequential fleet upgrade:
// all outdated online remote Nodes first (with Hub relay fallback & reconnect confirmation),
// followed by the Hub itself last.
func (s *Server) handleFleetUpdate(w http.ResponseWriter, r *http.Request) {
	view, status, err := s.startFleetUpdate(r.Context(), "fleet", nil)
	if err != nil {
		if status <= 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "update": view})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "update": view})
}

func (s *Server) startFleetUpdate(reqCtx context.Context, scope string, allowFilter func(id string) bool) (FleetUpdateView, int, error) {
	view, status, _, err := s.startFleetUpdateWithTarget(reqCtx, scope, "", allowFilter)
	return view, status, err
}

func (s *Server) startFleetUpdateWithTarget(reqCtx context.Context, scope, expectedTarget string, allowFilter func(id string) bool) (FleetUpdateView, int, bool, error) {
	s.fleetUpdateMu.Lock()
	if s.fleetUpdate != nil && s.fleetUpdate.view.Busy {
		snap := cloneFleetUpdateView(s.fleetUpdate.view)
		s.fleetUpdateMu.Unlock()
		return snap, http.StatusOK, false, nil
	}
	prev := s.fleetUpdate
	s.fleetUpdateMu.Unlock()

	ctx, cancel := context.WithTimeout(s.serverCtx(), updateLatestAttemptTimeout*2)
	latest, _, err := s.fetchLatestTagErr(ctx)
	cancel()
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号"
		}
		return s.fleetUpdateSnapshot(), http.StatusBadGateway, false, errors.New("查不到最新版本：" + msg)
	}
	if exp := strings.TrimSpace(expectedTarget); exp != "" && latest != exp {
		return s.fleetUpdateSnapshot(), http.StatusConflict, false, fmt.Errorf("最新版本已变更为 %s（确认目标为 %s），未执行升级，请重新检查并确认", latest, exp)
	}
	s.recordLatestTag(latest)

	targets := s.buildFleetUpdatePlan(reqCtx, latest, prev, allowFilter)
	if len(targets) == 0 {
		s.fleetUpdateMu.Lock()
		if s.fleetUpdate != nil && s.fleetUpdate.view.Busy {
			snap := cloneFleetUpdateView(s.fleetUpdate.view)
			s.fleetUpdateMu.Unlock()
			return snap, http.StatusOK, false, nil
		}
		s.fleetUpdate = &fleetUpdateState{
			view: FleetUpdateView{Phase: "done", Scope: scope, Target: latest},
		}
		snap := cloneFleetUpdateView(s.fleetUpdate.view)
		s.fleetUpdateMu.Unlock()
		return snap, http.StatusOK, false, nil
	}

	pendingIDs := make([]string, 0, len(targets))
	for _, t := range targets {
		pendingIDs = append(pendingIDs, t.ID)
	}

	s.fleetUpdateMu.Lock()
	if s.fleetUpdate != nil && s.fleetUpdate.view.Busy {
		snap := cloneFleetUpdateView(s.fleetUpdate.view)
		s.fleetUpdateMu.Unlock()
		return snap, http.StatusOK, false, nil
	}
	completed := []string(nil)
	if prev != nil && prev.view.Phase == "failed" && prev.view.Target == latest {
		completed = append([]string(nil), prev.view.CompletedIDs...)
	}
	st := &fleetUpdateState{
		plan:    targets,
		pending: append([]updateTarget(nil), targets...),
		view: FleetUpdateView{
			Busy:         true,
			Scope:        scope,
			Phase:        "updating",
			Target:       latest,
			Total:        len(targets),
			Index:        0,
			CompletedIDs: completed,
			PendingIDs:   pendingIDs,
			Errors:       map[string]string{},
		},
	}
	s.fleetUpdate = st
	snap := cloneFleetUpdateView(st.view)
	s.fleetUpdateMu.Unlock()

	s.notifyFleetUpdateChanged()
	go s.runFleetUpdate(st)
	return snap, http.StatusOK, true, nil
}

// CheckUpdate implements core.FleetUpdater as a strictly read-only version inspection
// reusing fetchLatestTagErr, collectVersionMachines, and fleetUpdateSnapshot.
func (s *Server) CheckUpdate(ctx context.Context) (core.UpdateCheckResult, error) {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	latest, _, err := s.fetchLatestTagErr(tctx)
	cancel()
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号"
		}
		return core.UpdateCheckResult{}, errors.New("检查更新失败：" + msg)
	}
	machines := s.collectVersionMachines(ctx)
	snap := s.fleetUpdateSnapshot()
	res := core.UpdateCheckResult{
		CurrentVersion: s.Version,
		LatestVersion:  latest,
		Busy:           snap.Busy,
		Phase:          snap.Phase,
		Error:          snap.Error,
		Machines:       make([]core.UpdateMachineStatus, 0, len(machines)),
	}
	for _, m := range machines {
		outdated := (m.Local || m.Online) && isOutdatedVersion(m.Version, latest)
		item := core.UpdateMachineStatus{
			ID:       m.ID,
			Name:     m.Name,
			Version:  m.Version,
			OS:       m.OS,
			Local:    m.Local,
			Online:   m.Online,
			Outdated: outdated,
		}
		res.Machines = append(res.Machines, item)
		if outdated {
			res.Outdated = append(res.Outdated, item)
		}
	}
	res.HasUpdate = len(res.Outdated) > 0
	return res, nil
}

// TriggerFleetUpdate implements core.FleetUpdater by delegating to the existing Hub-owned
// sequential fleet upgrade while validating expectedTarget against the live release tag.
func (s *Server) TriggerFleetUpdate(ctx context.Context, expectedTarget string) (core.UpdateTriggerResult, error) {
	view, _, started, err := s.startFleetUpdateWithTarget(ctx, "fleet", expectedTarget, nil)
	if err != nil {
		return core.UpdateTriggerResult{}, err
	}
	return core.UpdateTriggerResult{
		Started:    started,
		Busy:       !started && view.Busy,
		AlreadyUp:  !started && !view.Busy && view.Phase == "done",
		Target:     view.Target,
		PendingIDs: append([]string(nil), view.PendingIDs...),
		Phase:      view.Phase,
	}, nil
}

func (s *Server) buildFleetUpdatePlan(ctx context.Context, latest string, prev *fleetUpdateState, allowFilter func(id string) bool) []updateTarget {
	machines := s.collectVersionMachines(ctx)
	byID := map[string]versionMachine{}
	for _, m := range machines {
		byID[m.ID] = m
	}

	var out []updateTarget
	seen := map[string]bool{}

	// If retrying a failed fleet run for the same target tag, keep unfinished IDs (even if
	// a previously failed Node is momentarily offline) so Retry never skips ahead to the Hub.
	if prev != nil && prev.view.Phase == "failed" && prev.view.Target == latest && len(prev.pending) > 0 {
		for _, t := range prev.pending {
			if allowFilter != nil && !allowFilter(t.ID) {
				continue
			}
			m, exists := byID[t.ID]
			if exists && !isOutdatedVersion(m.Version, latest) && (m.Local || m.Online) {
				continue // already confirmed on latest
			}
			name := t.Name
			if exists && m.Name != "" {
				name = m.Name
			}
			out = append(out, updateTarget{ID: t.ID, Name: name, Local: t.Local})
			seen[t.ID] = true
		}
	}

	for _, m := range machines {
		if seen[m.ID] {
			continue
		}
		if !(m.Local || m.Online) || !isOutdatedVersion(m.Version, latest) {
			continue
		}
		if allowFilter != nil && !allowFilter(m.ID) {
			continue
		}
		out = append(out, updateTarget{ID: m.ID, Name: m.Name, Local: m.Local})
		seen[m.ID] = true
	}

	// Strictly remote Nodes first (sorted by name), Hub last.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local != out[j].Local {
			return !out[i].Local
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) runFleetUpdate(st *fleetUpdateState) {
	totalTimeout := time.Duration(len(st.plan))*remoteUpdateForwardTimeout + selfUpdateTimeout
	ctx, cancel := context.WithTimeout(s.serverCtx(), totalTimeout)
	defer cancel()

	cache := newRelayRunCache()
	defer cache.Cleanup()

	target := st.view.Target
	scope := st.view.Scope
	upgradedNames := []string{}

	for i, tgt := range st.plan {
		s.fleetUpdateMu.Lock()
		st.view.Phase = "updating"
		st.view.Index = i + 1
		st.view.CurrentID = tgt.ID
		st.view.CurrentName = tgt.Name
		st.view.Error = ""
		delete(st.view.Errors, tgt.ID)
		s.fleetUpdateMu.Unlock()
		s.notifyFleetUpdateChanged()

		if !tgt.Local {
			if err := s.runFleetRemoteStep(ctx, st, tgt, target, cache); err != nil {
				s.failFleetUpdate(st, tgt, err.Error())
				return
			}
			upgradedNames = append(upgradedNames, tgt.Name)
			s.completeFleetTarget(st, tgt.ID)
			continue
		}

		// Hub step (always last). Clean relay temp files before staging/exec.
		cache.Cleanup()
		if err := s.runFleetHubStep(ctx, st, tgt, target, scope, upgradedNames); err != nil {
			s.failFleetUpdate(st, tgt, err.Error())
			return
		}
		return
	}

	// Fleet run finished updating remote Nodes only (Hub was already on latest).
	s.fleetUpdateMu.Lock()
	st.view.Busy = false
	st.view.Phase = "done"
	st.view.CurrentID = ""
	st.view.CurrentName = ""
	st.view.PendingIDs = nil
	st.pending = nil
	s.fleetUpdateMu.Unlock()
	s.notifyFleetUpdateChanged()

	if scope == "auto" && s.Engine != nil && len(upgradedNames) > 0 {
		s.Engine.PostEvent(ctx, model.MainThread,
			fmt.Sprintf("凌晨空闲自动更新已完成，已升级至 %s（%s）", target, strings.Join(upgradedNames, "、")),
			"", "info")
	}
}

func (s *Server) runFleetRemoteStep(ctx context.Context, st *fleetUpdateState, tgt updateTarget, target string, cache *relayRunCache) error {
	if s.Fleet == nil {
		return fmt.Errorf("节点 %s 不在线", tgt.Name)
	}
	// Subscribe BEFORE issuing the update request so a rapid reconnect cannot be missed.
	ch, unsub := s.Fleet.SubscribeChanges()
	defer unsub()

	m, hc, ok := s.Fleet.RemoteMachineByID(tgt.ID)
	if !ok {
		return fmt.Errorf("节点 %s 当前离线，请确认其在线后重试", tgt.Name)
	}
	if !isOutdatedVersion(m.Version, target) {
		return nil
	}

	stepCtx, cancel := context.WithTimeout(ctx, remoteUpdateForwardTimeout)
	res, status, err := s.updateRemoteNode(stepCtx, m, hc, target, cache)
	cancel()

	if err != nil {
		// On transport EOF/timeout/connection drop (status == 0), the Node may already have
		// restarted onto target before replying; reconcile briefly before declaring failure.
		if status == 0 {
			reconcileWait := min(8*time.Second, nodeReconnectTimeout)
			if confirmed := s.waitNodeConfirmVersion(ctx, tgt.ID, target, ch, reconcileWait); confirmed {
				return nil
			}
		}
		return err
	}
	if upToDate, _ := res["up_to_date"].(bool); upToDate {
		if ver, _ := res["version"].(string); ver == "" || !isOutdatedVersion(ver, target) {
			return nil
		}
	}

	s.fleetUpdateMu.Lock()
	st.view.Phase = "reconnecting"
	s.fleetUpdateMu.Unlock()
	s.notifyFleetUpdateChanged()

	if !s.waitNodeConfirmVersion(ctx, tgt.ID, target, ch, nodeReconnectTimeout) {
		return fmt.Errorf("节点 %s 已下发升级，但在 %ds 内未以 %s 重新上线，已暂停后续 Hub 升级（请稍后刷新或重试）",
			tgt.Name, int(nodeReconnectTimeout.Seconds()), target)
	}
	return nil
}

func (s *Server) waitNodeConfirmVersion(ctx context.Context, machineID, target string, ch <-chan struct{}, wait time.Duration) bool {
	if s.Fleet == nil {
		return false
	}
	check := func() bool {
		m, _, ok := s.Fleet.RemoteMachineByID(machineID)
		return ok && parseSemver(m.Version) != nil && !isOutdatedVersion(m.Version, target)
	}
	if check() {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return check()
		case <-timer.C:
			return check()
		case <-ch:
			if check() {
				return true
			}
		case <-poll.C:
			if check() {
				return true
			}
		}
	}
}

func (s *Server) runFleetHubStep(ctx context.Context, st *fleetUpdateState, tgt updateTarget, target, scope string, upgradedRemote []string) error {
	if !s.selfUpdateMu.TryLock() {
		return errors.New("Hub 升级锁被占用，请稍后重试")
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			s.selfUpdateMu.Unlock()
		}
	}()

	if parseSemver(s.Version) == nil {
		return fmt.Errorf("Hub 为开发编译版（%s），不支持在线覆盖升级", s.Version)
	}
	if !isOutdatedVersion(s.Version, target) {
		s.completeFleetTarget(st, tgt.ID)
		s.fleetUpdateMu.Lock()
		st.view.Busy = false
		st.view.Phase = "done"
		st.view.CurrentID = ""
		st.view.CurrentName = ""
		st.view.PendingIDs = nil
		st.pending = nil
		s.fleetUpdateMu.Unlock()
		s.notifyFleetUpdateChanged()
		return nil
	}

	exe, err := s.resolveExecutable()
	if err != nil {
		return fmt.Errorf("定位 Hub 可执行文件失败：%w", err)
	}

	stageCtx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	tmpPath, err := s.stageReleaseBinary(stageCtx, target, exe, false)
	cancel()
	if err != nil {
		return err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	// For unattended auto-updates, acquire Engine's exclusive idle reservation BEFORE os.Rename
	// so new user messages, Dreams, or Handovers cleanly yield the upgrade without mutating disk.
	var releaseReserve func()
	if scope == "auto" && s.Engine != nil {
		rel, ok, reason := s.Engine.TryReserveHubRestart(ctx)
		if !ok {
			return fmt.Errorf("已跳过本次静默重启（%s）", reason)
		}
		releaseReserve = rel
	}

	if scope == "auto" && s.Engine != nil {
		allNames := append([]string{"Hub"}, upgradedRemote...)
		_ = s.Engine.RecordPendingAutoUpdateNotice(ctx, target, allNames)
	}

	if err := os.Rename(tmpPath, exe); err != nil {
		if releaseReserve != nil {
			releaseReserve()
		}
		return fmt.Errorf("覆盖 Hub 可执行文件失败：%w", err)
	}
	cleanupTmp = false

	s.fleetUpdateMu.Lock()
	st.view.Phase = "restarting"
	s.fleetUpdateMu.Unlock()
	s.notifyFleetUpdateChanged()

	if s.Logger != nil {
		s.Logger.Info("fleet update replaced Hub binary, restarting in place", "from", s.Version, "to", target, "scope", scope)
	}
	keepLocked = true
	s.triggerExecAsync(exe, false, func() {
		if releaseReserve != nil {
			releaseReserve()
		}
		s.selfUpdateMu.Unlock()
		s.failFleetUpdate(st, tgt, "新版本已写入，但原地重启失败，请手动重启 Hub 服务")
	})
	return nil
}

func (s *Server) completeFleetTarget(st *fleetUpdateState, id string) {
	s.fleetUpdateMu.Lock()
	st.view.CompletedIDs = append(st.view.CompletedIDs, id)
	nextPending := make([]updateTarget, 0, len(st.pending))
	nextPendingIDs := make([]string, 0, len(st.pending))
	for _, p := range st.pending {
		if p.ID == id {
			continue
		}
		nextPending = append(nextPending, p)
		nextPendingIDs = append(nextPendingIDs, p.ID)
	}
	st.pending = nextPending
	st.view.PendingIDs = nextPendingIDs
	s.fleetUpdateMu.Unlock()
	s.notifyFleetUpdateChanged()
}

func (s *Server) failFleetUpdate(st *fleetUpdateState, tgt updateTarget, msg string) {
	s.fleetUpdateMu.Lock()
	st.view.Busy = false
	st.view.Phase = "failed"
	st.view.FailedID = tgt.ID
	st.view.FailedName = tgt.Name
	st.view.Error = msg
	if st.view.Errors == nil {
		st.view.Errors = map[string]string{}
	}
	st.view.Errors[tgt.ID] = msg
	s.fleetUpdateMu.Unlock()
	s.notifyFleetUpdateChanged()
}

// ---------------------------------------------------------------- Remote Node Update & Hub Relay

type relayAsset struct {
	asset  string
	sha256 string
	size   int64
	path   string
}

type relayRunCache struct {
	dir     string
	entries map[string]relayAsset
}

func newRelayRunCache() *relayRunCache {
	return &relayRunCache{entries: map[string]relayAsset{}}
}

func (c *relayRunCache) Cleanup() {
	if c == nil {
		return
	}
	for k, ent := range c.entries {
		_ = os.Remove(ent.path)
		delete(c.entries, k)
	}
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
		c.dir = ""
	}
}

func (c *relayRunCache) getOrFetch(ctx context.Context, s *Server, tag, goos, goarch string) (relayAsset, *os.File, error) {
	key := tag + ":" + goos + "/" + goarch
	if ent, ok := c.entries[key]; ok {
		f, err := os.Open(ent.path)
		if err == nil {
			if fi, serr := f.Stat(); serr == nil && fi.Size() == ent.size {
				return ent, f, nil
			}
			_ = f.Close()
		}
		_ = os.Remove(ent.path)
		delete(c.entries, key)
	}
	if c.dir == "" {
		dir, err := os.MkdirTemp("", "corgi-relay-*")
		if err != nil {
			return relayAsset{}, nil, err
		}
		c.dir = dir
	}
	asset := "corgi-" + goos + "-" + goarch
	wantSHA, usedIPv4, err := s.downloadChecksums(ctx, tag, asset, false)
	if err != nil {
		return relayAsset{}, nil, err
	}
	tmpPath, err := s.downloadAssetToDir(ctx, tag, asset, c.dir, wantSHA, usedIPv4, 0o600)
	if err != nil {
		return relayAsset{}, nil, err
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return relayAsset{}, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return relayAsset{}, nil, err
	}
	ent := relayAsset{asset: asset, sha256: wantSHA, size: fi.Size(), path: tmpPath}
	c.entries[key] = ent
	return ent, f, nil
}

func (s *Server) machineOS(ctx context.Context, m model.Machine) string {
	if strings.TrimSpace(m.OS) != "" {
		return m.OS
	}
	if s.Auth != nil {
		if devs, err := s.Auth.List(ctx); err == nil {
			for _, d := range devs {
				if d.ID == m.ID && d.Kind == "machine" && strings.TrimSpace(d.OS) != "" {
					return d.OS
				}
			}
		}
	}
	return ""
}

func isLegacyRelayableMessage(msg string) bool {
	for _, nonLocal := range []string{"开发编译版", "升级正在进行中", "定位当前可执行文件", "无写权限", "磁盘空间不足", "写入新版本文件失败", "无法运行", "自检输出不匹配"} {
		if strings.Contains(msg, nonLocal) {
			return false
		}
	}
	return true
}

func (s *Server) updateRemoteNode(ctx context.Context, m model.Machine, hc *http.Client, targetTag string, cache *relayRunCache) (map[string]any, int, error) {
	// Use a request-scoped client sharing the tunnel Transport so we never mutate shared hc.Timeout.
	reqClient := &http.Client{Transport: hc.Transport}
	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/update", nil)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("构造节点 %s 升级请求失败：%w", m.Name, err)
	}
	resp, err := reqClient.Do(outReq)
	if err != nil {
		return nil, 0, fmt.Errorf("转发到 %s 失败：%s", m.Name, formatUpdateError(err))
	}
	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("节点 %s 升级连接中断（可能正在重启），请稍候刷新查看版本", m.Name)
	}

	if resp.StatusCode == http.StatusOK {
		var out map[string]any
		if json.Unmarshal(rawBody, &out) != nil || out == nil {
			out = map[string]any{"ok": true}
		}
		return out, http.StatusOK, nil
	}

	var errRes struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Relayable *bool  `json:"relayable"`
	}
	errMsg := strings.TrimSpace(string(rawBody))
	if json.Unmarshal(rawBody, &errRes) == nil && strings.TrimSpace(errRes.Error) != "" {
		errMsg = strings.TrimSpace(errRes.Error)
	}
	if errMsg == "" {
		errMsg = resp.Status
	}

	shouldRelay := resp.StatusCode == http.StatusBadGateway &&
		((errRes.Relayable != nil && *errRes.Relayable) || (errRes.Relayable == nil && isLegacyRelayableMessage(errMsg)))
	goos, goarch, hasPlatform := parsePlatformOSArch(s.machineOS(ctx, m))
	if shouldRelay && hasPlatform {
		return s.relayRemoteNodeUpdate(ctx, m, reqClient, goos, goarch, targetTag, errMsg, cache)
	}
	return nil, resp.StatusCode, errors.New(errMsg)
}

func (s *Server) relayRemoteNodeUpdate(ctx context.Context, m model.Machine, reqClient *http.Client, goos, goarch, targetTag, directErr string, cache *relayRunCache) (map[string]any, int, error) {
	tag := targetTag
	if tag == "" {
		var err error
		tag, _, err = s.fetchLatestTagErr(ctx)
		if err != nil || tag == "" {
			return nil, http.StatusBadGateway, fmt.Errorf("%s；Hub 中转查不到最新版本：%s", directErr, formatUpdateError(err))
		}
		s.recordLatestTag(tag)
	}
	if parseSemver(m.Version) != nil && !isOutdatedVersion(m.Version, tag) {
		return map[string]any{"ok": true, "version": m.Version, "up_to_date": true, "relayed": true}, http.StatusOK, nil
	}
	if cache == nil {
		cache = newRelayRunCache()
		defer cache.Cleanup()
	}
	ent, f, err := cache.getOrFetch(ctx, s, tag, goos, goarch)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("%s；Hub 中转下载 %s (%s/%s) 失败：%s", directErr, tag, goos, goarch, formatUpdateError(err))
	}
	defer f.Close()

	pushReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/update/relay", f)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("构造中转升级请求失败：%w", err)
	}
	pushReq.ContentLength = ent.size
	pushReq.Header.Set("Content-Type", "application/octet-stream")
	pushReq.Header.Set(relayUpdateTagHeader, tag)
	pushReq.Header.Set(relayUpdateSHAHeader, ent.sha256)

	resp, err := reqClient.Do(pushReq)
	if err != nil {
		return nil, 0, fmt.Errorf("Hub 中转推送 %s 到 %s 失败：%s", tag, m.Name, formatUpdateError(err))
	}
	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("Hub 中转推送 %s 到 %s 时连接中断，请稍候刷新查看版本", tag, m.Name)
	}

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		verHint := "版本较旧"
		if strings.TrimSpace(m.Version) != "" {
			verHint = fmt.Sprintf("当前版本（%s）较旧", strings.TrimSpace(m.Version))
		}
		return nil, http.StatusBadGateway, fmt.Errorf("%s；已尝试由 Hub 中转分发 %s，但节点 %s %s，暂不支持接收 Hub 中转升级，请先在该节点手动运行一次 install.sh 后再使用一键更新", directErr, tag, m.Name, verHint)
	}
	if resp.StatusCode == http.StatusOK {
		var out map[string]any
		if json.Unmarshal(rawBody, &out) != nil || out == nil {
			out = map[string]any{"ok": true, "version": tag}
		}
		out["relayed"] = true
		return out, http.StatusOK, nil
	}

	var nodeRes struct {
		Error string `json:"error"`
	}
	nodeErr := strings.TrimSpace(string(rawBody))
	if json.Unmarshal(rawBody, &nodeRes) == nil && strings.TrimSpace(nodeRes.Error) != "" {
		nodeErr = strings.TrimSpace(nodeRes.Error)
	}
	if nodeErr == "" {
		nodeErr = resp.Status
	}
	return nil, resp.StatusCode, fmt.Errorf("Hub 中转分发失败：%s", nodeErr)
}

// ---------------------------------------------------------------- Opt-In Overnight Auto-Update (Bounded B2)

// TryAutoUpdate implements core.AutoUpdater. It runs only when Settings.AutoUpdate is true,
// the Hub runs a release semver build, and at least one outdated online machine has not been
// attempted for the target tag within the rolling 24-hour cap.
func (s *Server) TryAutoUpdate(ctx context.Context) {
	if s.Engine == nil || s.Store == nil || !s.Engine.Settings().AutoUpdate {
		return
	}
	if parseSemver(s.Version) == nil || s.isFleetUpdateBusy() {
		return
	}

	lctx, cancel := context.WithTimeout(ctx, updateLatestAttemptTimeout*2)
	latest, _, err := s.fetchLatestTagErr(lctx)
	cancel()
	if err != nil || latest == "" {
		return
	}
	s.recordLatestTag(latest)

	now := time.Now()
	attempts := s.loadAutoUpdateAttempts(ctx)
	eligible := map[string]bool{}
	for _, m := range s.collectVersionMachines(ctx) {
		if !(m.Local || m.Online) || !isOutdatedVersion(m.Version, latest) {
			continue
		}
		key := autoUpdateAttemptKey(m.ID, latest)
		if prevAt, ok := attempts[key]; ok && now.Sub(prevAt) < autoUpdateRetryCap {
			continue
		}
		eligible[m.ID] = true
	}
	if len(eligible) == 0 {
		return
	}

	// Persist attempt timestamps BEFORE starting so crashes or failed upgrades never hot-loop.
	for id := range eligible {
		attempts[autoUpdateAttemptKey(id, latest)] = now
	}
	s.saveAutoUpdateAttempts(ctx, attempts, now)

	if s.Logger != nil {
		s.Logger.Info("starting opt-in overnight auto-update", "target", latest, "machines", len(eligible))
	}
	_, _, _ = s.startFleetUpdate(ctx, "auto", func(id string) bool {
		return eligible[id]
	})
}

func autoUpdateAttemptKey(machineID, tag string) string {
	if strings.TrimSpace(machineID) == "" {
		return "hub@" + tag
	}
	return machineID + "@" + tag
}

func (s *Server) loadAutoUpdateAttempts(ctx context.Context) map[string]time.Time {
	out := map[string]time.Time{}
	if s.Store == nil {
		return out
	}
	raw, ok, err := s.Store.GetKV(ctx, autoUpdateAttemptsKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func (s *Server) saveAutoUpdateAttempts(ctx context.Context, attempts map[string]time.Time, now time.Time) {
	if s.Store == nil {
		return
	}
	pruned := make(map[string]time.Time, len(attempts))
	for k, t := range attempts {
		if now.Sub(t) < 7*24*time.Hour {
			pruned[k] = t
		}
	}
	if b, err := json.Marshal(pruned); err == nil {
		_ = s.Store.SetKV(ctx, autoUpdateAttemptsKVKey, string(b))
	}
}

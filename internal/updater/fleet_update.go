package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

// IsFleetBusy reports whether a Hub-owned sequential fleet upgrade is currently running.
func (m *Manager) IsFleetBusy() bool {
	m.fleetUpdateMu.Lock()
	defer m.fleetUpdateMu.Unlock()
	return m.fleetUpdate != nil && m.fleetUpdate.view.Busy
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

// FleetSnapshot returns a defensive copy of the current fleet upgrade progress.
func (m *Manager) FleetSnapshot() FleetUpdateView {
	m.fleetUpdateMu.Lock()
	defer m.fleetUpdateMu.Unlock()
	if m.fleetUpdate == nil {
		return FleetUpdateView{Phase: "idle"}
	}
	return cloneFleetUpdateView(m.fleetUpdate.view)
}

// ResetFleetState clears any completed/failed fleet update snapshot back to idle (if not busy).
func (m *Manager) ResetFleetState() {
	m.fleetUpdateMu.Lock()
	if m.fleetUpdate != nil && !m.fleetUpdate.view.Busy {
		m.fleetUpdate = nil
	}
	m.fleetUpdateMu.Unlock()
}

func (m *Manager) notifyChanged() {
	if m.cfg.OnStateChanged != nil {
		m.cfg.OnStateChanged()
	}
}

// StartFleetUpdate launches (or returns status of) the Hub-owned sequential fleet upgrade:
// all outdated online remote Nodes first, followed by the Hub itself last.
func (m *Manager) StartFleetUpdate(reqCtx context.Context, scope string, allowFilter func(id string) bool) (FleetUpdateView, int, error) {
	view, status, _, err := m.startFleetUpdateWithTarget(reqCtx, scope, "", allowFilter)
	return view, status, err
}

func (m *Manager) startFleetUpdateWithTarget(reqCtx context.Context, scope, expectedTarget string, allowFilter func(id string) bool) (FleetUpdateView, int, bool, error) {
	m.fleetUpdateMu.Lock()
	if m.fleetUpdate != nil && m.fleetUpdate.view.Busy {
		snap := cloneFleetUpdateView(m.fleetUpdate.view)
		m.fleetUpdateMu.Unlock()
		return snap, http.StatusOK, false, nil
	}
	prev := m.fleetUpdate
	m.fleetUpdateMu.Unlock()

	ctx, cancel := context.WithTimeout(m.serverCtx(), updateLatestAttemptTimeout*2)
	latest, _, err := m.fetchLatestTagErr(ctx)
	cancel()
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号"
		}
		return m.FleetSnapshot(), http.StatusBadGateway, false, errors.New("查不到最新版本：" + msg)
	}
	if exp := strings.TrimSpace(expectedTarget); exp != "" && latest != exp {
		return m.FleetSnapshot(), http.StatusConflict, false, fmt.Errorf("最新版本已变更为 %s（确认目标为 %s），未执行升级，请重新检查并确认", latest, exp)
	}
	m.recordLatestTag(latest)

	targets := m.buildFleetUpdatePlan(reqCtx, latest, prev, allowFilter)
	if len(targets) == 0 {
		m.fleetUpdateMu.Lock()
		if m.fleetUpdate != nil && m.fleetUpdate.view.Busy {
			snap := cloneFleetUpdateView(m.fleetUpdate.view)
			m.fleetUpdateMu.Unlock()
			return snap, http.StatusOK, false, nil
		}
		m.fleetUpdate = &fleetUpdateState{
			view: FleetUpdateView{Phase: "done", Scope: scope, Target: latest},
		}
		snap := cloneFleetUpdateView(m.fleetUpdate.view)
		m.fleetUpdateMu.Unlock()
		return snap, http.StatusOK, false, nil
	}

	pendingIDs := make([]string, 0, len(targets))
	for _, t := range targets {
		pendingIDs = append(pendingIDs, t.ID)
	}

	m.fleetUpdateMu.Lock()
	if m.fleetUpdate != nil && m.fleetUpdate.view.Busy {
		snap := cloneFleetUpdateView(m.fleetUpdate.view)
		m.fleetUpdateMu.Unlock()
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
	m.fleetUpdate = st
	snap := cloneFleetUpdateView(st.view)
	m.fleetUpdateMu.Unlock()

	m.notifyChanged()
	go m.runFleetUpdate(st)
	return snap, http.StatusOK, true, nil
}

// CheckUpdate implements core.FleetUpdater as a strictly read-only version inspection.
func (m *Manager) CheckUpdate(ctx context.Context) (core.UpdateCheckResult, error) {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	latest, _, err := m.fetchLatestTagErr(tctx)
	cancel()
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号"
		}
		return core.UpdateCheckResult{}, errors.New("检查更新失败：" + msg)
	}
	machines := m.CollectVersionMachines(ctx)
	snap := m.FleetSnapshot()
	res := core.UpdateCheckResult{
		CurrentVersion: m.version(),
		LatestVersion:  latest,
		Busy:           snap.Busy,
		Phase:          snap.Phase,
		Error:          snap.Error,
		Machines:       make([]core.UpdateMachineStatus, 0, len(machines)),
	}
	for _, mc := range machines {
		outdated := (mc.Local || mc.Online) && IsOutdatedVersion(mc.Version, latest)
		item := core.UpdateMachineStatus{
			ID:       mc.ID,
			Name:     mc.Name,
			Version:  mc.Version,
			OS:       mc.OS,
			Local:    mc.Local,
			Online:   mc.Online,
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

// TriggerFleetUpdate implements core.FleetUpdater while validating expectedTarget against the live release tag.
func (m *Manager) TriggerFleetUpdate(ctx context.Context, expectedTarget string) (core.UpdateTriggerResult, error) {
	view, _, started, err := m.startFleetUpdateWithTarget(ctx, "fleet", expectedTarget, nil)
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

func (m *Manager) buildFleetUpdatePlan(ctx context.Context, latest string, prev *fleetUpdateState, allowFilter func(id string) bool) []updateTarget {
	machines := m.CollectVersionMachines(ctx)
	byID := map[string]VersionMachine{}
	for _, mc := range machines {
		byID[mc.ID] = mc
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
			mc, exists := byID[t.ID]
			if exists && !IsOutdatedVersion(mc.Version, latest) && (mc.Local || mc.Online) {
				continue
			}
			name := t.Name
			if exists && mc.Name != "" {
				name = mc.Name
			}
			out = append(out, updateTarget{ID: t.ID, Name: name, Local: t.Local})
			seen[t.ID] = true
		}
	}

	for _, mc := range machines {
		if seen[mc.ID] {
			continue
		}
		if !(mc.Local || mc.Online) || !IsOutdatedVersion(mc.Version, latest) {
			continue
		}
		if allowFilter != nil && !allowFilter(mc.ID) {
			continue
		}
		out = append(out, updateTarget{ID: mc.ID, Name: mc.Name, Local: mc.Local})
		seen[mc.ID] = true
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

func (m *Manager) runFleetUpdate(st *fleetUpdateState) {
	totalTimeout := time.Duration(len(st.plan))*remoteUpdateForwardTimeout + selfUpdateTimeout
	ctx, cancel := context.WithTimeout(m.serverCtx(), totalTimeout)
	defer cancel()

	cache := newRelayRunCache()
	defer cache.Cleanup()

	target := st.view.Target
	scope := st.view.Scope
	upgradedNames := []string{}

	for i, tgt := range st.plan {
		m.fleetUpdateMu.Lock()
		st.view.Phase = "updating"
		st.view.Index = i + 1
		st.view.CurrentID = tgt.ID
		st.view.CurrentName = tgt.Name
		st.view.Error = ""
		delete(st.view.Errors, tgt.ID)
		m.fleetUpdateMu.Unlock()
		m.notifyChanged()

		if !tgt.Local {
			if err := m.runFleetRemoteStep(ctx, st, tgt, target, cache); err != nil {
				m.failFleetUpdate(st, tgt, err.Error())
				return
			}
			upgradedNames = append(upgradedNames, tgt.Name)
			m.completeFleetTarget(st, tgt.ID)
			continue
		}

		// Hub step (always last). Clean relay temp files before staging/exec.
		cache.Cleanup()
		if err := m.runFleetHubStep(ctx, st, tgt, target, scope, upgradedNames); err != nil {
			m.failFleetUpdate(st, tgt, err.Error())
			return
		}
		return
	}

	// Fleet run finished updating remote Nodes only (Hub was already on latest).
	m.fleetUpdateMu.Lock()
	st.view.Busy = false
	st.view.Phase = "done"
	st.view.CurrentID = ""
	st.view.CurrentName = ""
	st.view.PendingIDs = nil
	st.pending = nil
	m.fleetUpdateMu.Unlock()
	m.notifyChanged()

	if scope == "auto" && m.cfg.Runtime != nil && len(upgradedNames) > 0 {
		m.cfg.Runtime.PostEvent(ctx, model.MainThread,
			fmt.Sprintf("凌晨空闲自动更新已完成，已升级至 %s（%s）", target, strings.Join(upgradedNames, "、")),
			"", "info")
	}
}

func (m *Manager) runFleetRemoteStep(ctx context.Context, st *fleetUpdateState, tgt updateTarget, target string, cache *relayRunCache) error {
	if m.cfg.Fleet == nil {
		return fmt.Errorf("节点 %s 不在线", tgt.Name)
	}
	// Subscribe BEFORE issuing the update request so a rapid reconnect cannot be missed.
	ch, unsub := m.cfg.Fleet.SubscribeChanges()
	defer unsub()

	mc, hc, ok := m.cfg.Fleet.RemoteMachineByID(tgt.ID)
	if !ok {
		return fmt.Errorf("节点 %s 当前离线，请确认其在线后重试", tgt.Name)
	}
	if !IsOutdatedVersion(mc.Version, target) {
		return nil
	}

	stepCtx, cancel := context.WithTimeout(ctx, remoteUpdateForwardTimeout)
	res, status, err := m.updateRemoteNode(stepCtx, mc, hc, target, cache)
	cancel()

	reconnectWait := m.effectiveNodeReconnectTimeout()
	if err != nil {
		// On transport EOF/timeout/connection drop (status == 0), the Node may already have
		// restarted onto target before replying; reconcile briefly before declaring failure.
		if status == 0 {
			reconcileWait := min(8*time.Second, reconnectWait)
			if confirmed := m.waitNodeConfirmVersion(ctx, tgt.ID, target, ch, reconcileWait); confirmed {
				return nil
			}
		}
		return err
	}
	if upToDate, _ := res["up_to_date"].(bool); upToDate {
		if ver, _ := res["version"].(string); ver == "" || !IsOutdatedVersion(ver, target) {
			return nil
		}
	}

	m.fleetUpdateMu.Lock()
	st.view.Phase = "reconnecting"
	m.fleetUpdateMu.Unlock()
	m.notifyChanged()

	if !m.waitNodeConfirmVersion(ctx, tgt.ID, target, ch, reconnectWait) {
		return fmt.Errorf("节点 %s 已下发升级，但在 %ds 内未以 %s 重新上线，已暂停后续 Hub 升级（请稍后刷新或重试）",
			tgt.Name, int(reconnectWait.Seconds()), target)
	}
	return nil
}

func (m *Manager) waitNodeConfirmVersion(ctx context.Context, machineID, target string, ch <-chan struct{}, wait time.Duration) bool {
	if m.cfg.Fleet == nil {
		return false
	}
	check := func() bool {
		mc, _, ok := m.cfg.Fleet.RemoteMachineByID(machineID)
		return ok && parseSemver(mc.Version) != nil && !IsOutdatedVersion(mc.Version, target)
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

func (m *Manager) runFleetHubStep(ctx context.Context, st *fleetUpdateState, tgt updateTarget, target, scope string, upgradedRemote []string) error {
	if !m.selfUpdateMu.TryLock() {
		return errors.New("Hub 升级锁被占用，请稍后重试")
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			m.selfUpdateMu.Unlock()
		}
	}()

	curVer := m.version()
	if parseSemver(curVer) == nil {
		return fmt.Errorf("Hub 为开发编译版（%s），不支持在线覆盖升级", curVer)
	}
	if !IsOutdatedVersion(curVer, target) {
		m.completeFleetTarget(st, tgt.ID)
		m.fleetUpdateMu.Lock()
		st.view.Busy = false
		st.view.Phase = "done"
		st.view.CurrentID = ""
		st.view.CurrentName = ""
		st.view.PendingIDs = nil
		st.pending = nil
		m.fleetUpdateMu.Unlock()
		m.notifyChanged()
		return nil
	}

	exe, err := m.resolveExecutable()
	if err != nil {
		return fmt.Errorf("定位 Hub 可执行文件失败：%w", err)
	}

	stageCtx, cancel := context.WithTimeout(ctx, selfUpdateTimeout)
	tmpPath, err := m.stageReleaseBinary(stageCtx, target, exe, false)
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
	if scope == "auto" && m.cfg.Runtime != nil {
		rel, ok, reason := m.cfg.Runtime.TryReserveHubRestart(ctx)
		if !ok {
			return fmt.Errorf("已跳过本次静默重启（%s）", reason)
		}
		releaseReserve = rel
	}

	if scope == "auto" && m.cfg.Runtime != nil {
		allNames := append([]string{"Hub"}, upgradedRemote...)
		_ = m.cfg.Runtime.RecordPendingAutoUpdateNotice(ctx, target, allNames)
	}

	if err := os.Rename(tmpPath, exe); err != nil {
		if releaseReserve != nil {
			releaseReserve()
		}
		return fmt.Errorf("覆盖 Hub 可执行文件失败：%w", err)
	}
	cleanupTmp = false

	m.fleetUpdateMu.Lock()
	st.view.Phase = "restarting"
	m.fleetUpdateMu.Unlock()
	m.notifyChanged()

	if m.cfg.Logger != nil {
		m.cfg.Logger.Info("fleet update replaced Hub binary, restarting in place", "from", curVer, "to", target, "scope", scope)
	}
	keepLocked = true
	m.triggerExecAsync(exe, false, func() {
		if releaseReserve != nil {
			releaseReserve()
		}
		m.selfUpdateMu.Unlock()
		m.failFleetUpdate(st, tgt, "新版本已写入，但原地重启失败，请手动重启 Hub 服务")
	})
	return nil
}

func (m *Manager) completeFleetTarget(st *fleetUpdateState, id string) {
	m.fleetUpdateMu.Lock()
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
	m.fleetUpdateMu.Unlock()
	m.notifyChanged()
}

func (m *Manager) failFleetUpdate(st *fleetUpdateState, tgt updateTarget, msg string) {
	m.fleetUpdateMu.Lock()
	st.view.Busy = false
	st.view.Phase = "failed"
	st.view.FailedID = tgt.ID
	st.view.FailedName = tgt.Name
	st.view.Error = msg
	if st.view.Errors == nil {
		st.view.Errors = map[string]string{}
	}
	st.view.Errors[tgt.ID] = msg
	m.fleetUpdateMu.Unlock()
	m.notifyChanged()
}

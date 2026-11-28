package updater

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// CollectVersionMachines returns Hub + paired Nodes sorted local-first, online-first, then by name.
func (m *Manager) CollectVersionMachines(ctx context.Context) []VersionMachine {
	curVer := m.version()
	byID := map[string]VersionMachine{}
	if m.cfg.Fleet != nil {
		for _, mc := range m.cfg.Fleet.Machines() {
			v := mc.Version
			online := mc.Online
			if mc.Local {
				v = curVer
				online = true
			}
			byID[mc.ID] = VersionMachine{
				ID:      mc.ID,
				Name:    mc.Name,
				Version: v,
				OS:      mc.OS,
				Local:   mc.Local,
				Online:  online,
			}
		}
	}
	if m.cfg.Devices != nil {
		if devs, err := m.cfg.Devices.List(ctx); err == nil {
			for _, d := range devs {
				if d == nil || d.Kind != "machine" {
					continue
				}
				if _, exists := byID[d.ID]; !exists {
					byID[d.ID] = VersionMachine{
						ID:     d.ID,
						Name:   d.Name,
						OS:     d.OS,
						Local:  false,
						Online: false,
					}
				}
			}
		}
	}
	out := make([]VersionMachine, 0, len(byID))
	for _, mc := range byID {
		out = append(out, mc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local != out[j].Local {
			return out[i].Local
		}
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// VersionSnapshot builds the full /api/version response payload.
func (m *Manager) VersionSnapshot(ctx context.Context, soon bool) VersionSnapshot {
	latest := m.LatestReleaseTag(soon)
	machines := m.CollectVersionMachines(ctx)
	outdated := []OutdatedMachine{}
	for _, mc := range machines {
		if (mc.Local || mc.Online) && IsOutdatedVersion(mc.Version, latest) {
			outdated = append(outdated, OutdatedMachine{ID: mc.ID, Name: mc.Name, Version: mc.Version})
		}
	}
	return VersionSnapshot{
		Version:  m.version(),
		Latest:   latest,
		Outdated: outdated,
		Machines: machines,
		Update:   m.FleetSnapshot(),
	}
}

// LocalSelfUpdate downloads, verifies, and atomically replaces the current executable.
// Caller must invoke resp.AfterFlush() after writing and flushing the HTTP response.
func (m *Manager) LocalSelfUpdate(isNode bool) LocalUpdateResponse {
	if !isNode && m.IsFleetBusy() {
		return LocalUpdateResponse{
			Status:  http.StatusConflict,
			Payload: map[string]any{"error": "全端升级正在进行中，请稍候"},
		}
	}
	if !m.selfUpdateMu.TryLock() {
		return LocalUpdateResponse{
			Status:  http.StatusConflict,
			Payload: map[string]any{"error": "升级正在进行中，请稍候", "code": "busy", "relayable": false},
		}
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			m.selfUpdateMu.Unlock()
		}
	}()

	curVer := m.version()
	if parseSemver(curVer) == nil {
		return LocalUpdateResponse{
			Status: http.StatusBadRequest,
			Payload: map[string]any{
				"error":     "当前进程为开发编译版（" + curVer + "），不支持在线覆盖升级",
				"code":      "dev_build",
				"relayable": false,
			},
		}
	}

	ctx, cancel := context.WithTimeout(m.serverCtx(), selfUpdateTimeout)
	defer cancel()

	latest, usedIPv4, err := m.fetchLatestTagErr(ctx)
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号（请检查这台电脑能否访问 GitHub）"
		}
		return LocalUpdateResponse{
			Status: http.StatusBadGateway,
			Payload: map[string]any{
				"error":     "查不到最新版本：" + msg,
				"code":      "upstream_net",
				"relayable": true,
			},
		}
	}
	m.recordLatestTag(latest)

	if !IsOutdatedVersion(curVer, latest) {
		return LocalUpdateResponse{
			Status:  http.StatusOK,
			Payload: map[string]any{"ok": true, "version": curVer, "up_to_date": true},
		}
	}

	exe, err := m.resolveExecutable()
	if err != nil {
		return LocalUpdateResponse{
			Status: http.StatusInternalServerError,
			Payload: map[string]any{
				"error":     "定位当前可执行文件失败：" + err.Error(),
				"code":      "local_io",
				"relayable": false,
			},
		}
	}

	tmpPath, err := m.stageReleaseBinary(ctx, latest, exe, usedIPv4)
	if err != nil {
		return updateErrorResponse(err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Remove(tmpPath)
		return LocalUpdateResponse{
			Status: http.StatusBadGateway,
			Payload: map[string]any{
				"error":     "写入新版本文件失败：" + err.Error(),
				"code":      "local_io",
				"relayable": false,
			},
		}
	}

	if m.cfg.Logger != nil {
		m.cfg.Logger.Info("self-updated binary, restarting in place", "from", curVer, "to", latest, "exe", exe, "node", isNode)
	}
	keepLocked = true
	return LocalUpdateResponse{
		Status:  http.StatusOK,
		Payload: map[string]any{"ok": true, "from": curVer, "version": latest},
		AfterFlush: func() {
			m.triggerExecAsync(exe, isNode, func() {
				m.selfUpdateMu.Unlock()
			})
		},
	}
}

// ApplyRelayedBinary verifies a Hub-relayed binary stream, atomically replaces the executable,
// and returns a post-flush exec trigger.
func (m *Manager) ApplyRelayedBinary(body io.Reader, contentLen int64, tag, wantSHA string) LocalUpdateResponse {
	if !m.selfUpdateMu.TryLock() {
		return LocalUpdateResponse{
			Status:  http.StatusConflict,
			Payload: map[string]any{"error": "升级正在进行中，请稍候", "code": "busy"},
		}
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			m.selfUpdateMu.Unlock()
		}
	}()

	curVer := m.version()
	if parseSemver(curVer) == nil {
		return LocalUpdateResponse{
			Status:  http.StatusBadRequest,
			Payload: map[string]any{"error": "当前进程为开发编译版（" + curVer + "），不支持中转升级"},
		}
	}
	tag = strings.TrimSpace(tag)
	wantSHA = strings.ToLower(strings.TrimSpace(wantSHA))
	if parseSemver(tag) == nil {
		return LocalUpdateResponse{
			Status:  http.StatusBadRequest,
			Payload: map[string]any{"error": "中转升级版本号无效"},
		}
	}
	if !isValidSHA256Hex(wantSHA) {
		return LocalUpdateResponse{
			Status:  http.StatusBadRequest,
			Payload: map[string]any{"error": "中转升级校验值格式无效"},
		}
	}
	if !IsOutdatedVersion(curVer, tag) {
		return LocalUpdateResponse{
			Status:  http.StatusOK,
			Payload: map[string]any{"ok": true, "version": curVer, "up_to_date": true, "relayed": true},
		}
	}

	exe, err := m.resolveExecutable()
	if err != nil {
		return LocalUpdateResponse{
			Status:  http.StatusInternalServerError,
			Payload: map[string]any{"error": "定位当前可执行文件失败：" + err.Error()},
		}
	}

	ctx, cancel := context.WithTimeout(m.serverCtx(), selfUpdateTimeout)
	defer cancel()

	tmpPath, err := stageStreamedBinary(ctx, body, contentLen, tag, wantSHA, exe)
	if err != nil {
		return updateErrorResponse(err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Remove(tmpPath)
		return LocalUpdateResponse{
			Status:  http.StatusBadGateway,
			Payload: map[string]any{"error": "写入新版本文件失败：" + err.Error(), "code": "local_io"},
		}
	}

	if m.cfg.Logger != nil {
		m.cfg.Logger.Info("applied relayed binary, restarting in place", "from", curVer, "to", tag, "exe", exe)
	}
	keepLocked = true
	return LocalUpdateResponse{
		Status:  http.StatusOK,
		Payload: map[string]any{"ok": true, "from": curVer, "version": tag, "relayed": true},
		AfterFlush: func() {
			m.triggerExecAsync(exe, true, func() {
				m.selfUpdateMu.Unlock()
			})
		},
	}
}

func updateErrorResponse(err error) LocalUpdateResponse {
	var ue *UpdateError
	if errors.As(err, &ue) {
		status := http.StatusBadGateway
		if ue.Code == "dev_build" {
			status = http.StatusBadRequest
		} else if ue.Code == "busy" {
			status = http.StatusConflict
		}
		return LocalUpdateResponse{
			Status: status,
			Payload: map[string]any{
				"error":     ue.Error(),
				"code":      ue.Code,
				"relayable": ue.Relayable,
			},
		}
	}
	return LocalUpdateResponse{
		Status: http.StatusBadGateway,
		Payload: map[string]any{
			"error":     "升级失败：" + formatUpdateError(err),
			"code":      "unknown",
			"relayable": false,
		},
	}
}

func (m *Manager) triggerExecAsync(targetExe string, isNode bool, onExecFail func()) {
	var execFn func(string)
	if m.cfg.ExecFn != nil {
		execFn = m.cfg.ExecFn()
	}
	go func() {
		if !isNode && m.cfg.FlushBeforeRestart != nil {
			fctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			_ = m.cfg.FlushBeforeRestart(fctx)
			cancel()
		}
		if execFn != nil {
			execFn(targetExe)
			return
		}
		time.Sleep(800 * time.Millisecond)
		args := append([]string(nil), os.Args...)
		if isNode {
			args = SanitizeNodeExecArgs(args)
		}
		if err := syscall.Exec(targetExe, args, os.Environ()); err != nil {
			if m.cfg.Logger != nil {
				m.cfg.Logger.Error("syscall.Exec failed after upgrade, keeping current process alive", "exe", targetExe, "err", err)
			}
			if onExecFail != nil {
				onExecFail()
			}
		}
	}()
}

func (m *Manager) resolveExecutable() (string, error) {
	if m.cfg.ExecutablePath != nil {
		if p := m.cfg.ExecutablePath(); p != "" {
			return p, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
		return resolved, nil
	}
	return exe, nil
}

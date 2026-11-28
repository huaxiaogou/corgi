package core

import (
	"context"
	"fmt"
	"strings"
)

// UpdateMachineStatus is one machine's version snapshot for manage_config(check_update).
type UpdateMachineStatus struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	OS       string `json:"os,omitempty"`
	Local    bool   `json:"local,omitempty"`
	Online   bool   `json:"online"`
	Outdated bool   `json:"outdated"`
}

// UpdateCheckResult is the read-only release/version check snapshot reused from the Hub update engine.
type UpdateCheckResult struct {
	CurrentVersion string                `json:"current_version"`
	LatestVersion  string                `json:"latest_version"`
	HasUpdate      bool                  `json:"has_update"`
	Busy           bool                  `json:"busy"`
	Phase          string                `json:"phase,omitempty"`
	Error          string                `json:"error,omitempty"`
	Outdated       []UpdateMachineStatus `json:"outdated,omitempty"`
	Machines       []UpdateMachineStatus `json:"machines,omitempty"`
}

// UpdateTriggerResult reports the outcome of launching an authenticated fleet upgrade.
type UpdateTriggerResult struct {
	Started    bool     `json:"started"`
	AlreadyUp  bool     `json:"already_up_to_date,omitempty"`
	Busy       bool     `json:"busy,omitempty"`
	Target     string   `json:"target,omitempty"`
	PendingIDs []string `json:"pending_ids,omitempty"`
	Phase      string   `json:"phase,omitempty"`
}

// FleetUpdater is optionally implemented by AutoUpdater (web.Server) so internal/core can
// reuse the existing update engine without breaking the one-way package DAG.
type FleetUpdater interface {
	CheckUpdate(ctx context.Context) (UpdateCheckResult, error)
	TriggerFleetUpdate(ctx context.Context, expectedTarget string) (UpdateTriggerResult, error)
}

type triggerUpdatePayload struct {
	TargetVersion  string   `json:"target_version"`
	CurrentVersion string   `json:"current_version,omitempty"`
	OutdatedNames  []string `json:"outdated_names,omitempty"`
}

func (e *Engine) fleetUpdater() (FleetUpdater, error) {
	e.mu.Lock()
	u := e.autoUpdater
	e.mu.Unlock()
	if u == nil {
		return nil, fmt.Errorf("升级服务暂不可用")
	}
	fu, ok := u.(FleetUpdater)
	if !ok {
		return nil, fmt.Errorf("当前升级模块不支持对话内检查/触发升级")
	}
	return fu, nil
}

// CheckFleetUpdate executes a strictly read-only version check via the existing Hub update engine.
func (e *Engine) CheckFleetUpdate(ctx context.Context) (UpdateCheckResult, error) {
	fu, err := e.fleetUpdater()
	if err != nil {
		return UpdateCheckResult{}, err
	}
	return fu.CheckUpdate(ctx)
}

// TriggerFleetUpdate starts the existing Hub-owned sequential fleet upgrade for expectedTarget.
func (e *Engine) TriggerFleetUpdate(ctx context.Context, expectedTarget string) (UpdateTriggerResult, error) {
	fu, err := e.fleetUpdater()
	if err != nil {
		return UpdateTriggerResult{}, err
	}
	return fu.TriggerFleetUpdate(ctx, strings.TrimSpace(expectedTarget))
}

func (e *Engine) executeConfirmedFleetUpdate(ctx context.Context, targetVersion, resolvedBy string) (string, bool) {
	res, err := e.TriggerFleetUpdate(ctx, targetVersion)
	if err != nil {
		return fmt.Sprintf("未启动一键升级（目标 %s）：%s", targetVersion, err.Error()), true
	}
	if res.Busy {
		return fmt.Sprintf("全端升级已在进行中（目标 %s，阶段：%s），本次未重复启动。", orStr(res.Target, targetVersion), orStr(res.Phase, "updating")), false
	}
	if res.AlreadyUp {
		return fmt.Sprintf("所有在线设备均已是最新版本 %s，无需升级。", orStr(res.Target, targetVersion)), false
	}
	return fmt.Sprintf("已确认并启动一键升级至 %s（待升级 %d 台在线设备，via %s），将按顺序升级远端节点并最终自动重启 Hub。", orStr(res.Target, targetVersion), len(res.PendingIDs), resolvedBy), false
}

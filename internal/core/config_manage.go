package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

const (
	configAuditRingKVKey = "config_audit_ring"
	maxConfigAuditRing   = 30
)

// CloneSettings returns a deep copy of Settings so callers can never race on nested maps/slices/pointers.
func CloneSettings(st model.Settings) model.Settings {
	cp := st
	if st.MorningDigest != nil {
		v := *st.MorningDigest
		cp.MorningDigest = &v
	}
	if st.KindDefaults != nil {
		cp.KindDefaults = make(map[string]model.ModelEffortSpec, len(st.KindDefaults))
		for k, v := range st.KindDefaults {
			cp.KindDefaults[k] = v
		}
	}
	if st.MachineKindOverrides != nil {
		cp.MachineKindOverrides = make(map[string]map[string]model.ModelEffortSpec, len(st.MachineKindOverrides))
		for m, byKind := range st.MachineKindOverrides {
			inner := make(map[string]model.ModelEffortSpec, len(byKind))
			for k, v := range byKind {
				inner[k] = v
			}
			cp.MachineKindOverrides[m] = inner
		}
	}
	if st.Projects != nil {
		cp.Projects = make([]model.ProjectWorkspace, len(st.Projects))
		for i, p := range st.Projects {
			pc := p
			if p.Aliases != nil {
				pc.Aliases = append([]string(nil), p.Aliases...)
			}
			if p.Paths != nil {
				pc.Paths = make(map[string]string, len(p.Paths))
				for mk, mp := range p.Paths {
					pc.Paths[mk] = mp
				}
			}
			cp.Projects[i] = pc
		}
	}
	if st.WeComAllowUsers != nil {
		cp.WeComAllowUsers = append([]string(nil), st.WeComAllowUsers...)
	}
	return cp
}

// SetOnSettingsApplied registers a sequential post-commit hook (e.g. WeCom live config reload)
// invoked strictly in commit order after SQLite persistence and memory publication.
func (e *Engine) SetOnSettingsApplied(fn func(model.Settings)) {
	e.mu.Lock()
	e.onSettingsApplied = fn
	e.mu.Unlock()
}

func (e *Engine) withConfigWriteLock(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case e.configWriteSem <- struct{}{}:
		defer func() { <-e.configWriteSem }()
		return fn()
	case <-ctx.Done():
		return fmt.Errorf("等待配置写通道取消：%w", ctx.Err())
	}
}

func (e *Engine) normalizeSettingsForSave(prev, ns model.Settings, preserveRouterAPI bool) (model.Settings, bool, error) {
	ns = CloneSettings(ns)
	if ns.WeComSecret == "••••••••" {
		ns.WeComSecret = prev.WeComSecret
	}
	if preserveRouterAPI {
		ns.RouterAPIProvider = prev.RouterAPIProvider
		ns.RouterAPIBaseURL = prev.RouterAPIBaseURL
		ns.RouterAPIModel = prev.RouterAPIModel
		ns.RouterAPIEffort = prev.RouterAPIEffort
	}
	if agent.NormalizeKind(ns.WorkerKind) == "" {
		ns.WorkerKind = prev.WorkerKind
	}
	if ns.ReplyChars <= 0 {
		ns.ReplyChars = model.DefaultReplyChars
	}
	if ns.StallMinutes <= 0 {
		ns.StallMinutes = 12
	}
	if ns.IdlePatrolMinutes < 0 {
		ns.IdlePatrolMinutes = -1
	} else if ns.IdlePatrolMinutes == 0 {
		ns.IdlePatrolMinutes = 30
	} else if ns.IdlePatrolMinutes > 480 {
		ns.IdlePatrolMinutes = 480
	}
	ns.ActiveHours = NormalizeActiveHours(ns.ActiveHours)
	if ns.StaleArchiveDays < 0 {
		ns.StaleArchiveDays = -1
	} else if ns.StaleArchiveDays == 0 {
		ns.StaleArchiveDays = 3
	} else if ns.StaleArchiveDays > 60 {
		ns.StaleArchiveDays = 60
	}
	if ns.WorkerIdleGCMinutes < 0 {
		ns.WorkerIdleGCMinutes = -1
	} else if ns.WorkerIdleGCMinutes == 0 {
		ns.WorkerIdleGCMinutes = 120
	} else if ns.WorkerIdleGCMinutes < 15 {
		ns.WorkerIdleGCMinutes = 15
	} else if ns.WorkerIdleGCMinutes > 1440 {
		ns.WorkerIdleGCMinutes = 1440
	}
	if ns.WorkerMaxLivePerMachine < 0 {
		ns.WorkerMaxLivePerMachine = -1
	} else if ns.WorkerMaxLivePerMachine == 0 {
		ns.WorkerMaxLivePerMachine = 6
	} else if ns.WorkerMaxLivePerMachine > 50 {
		ns.WorkerMaxLivePerMachine = 50
	}
	if ns.NotifyLevel != "normal" {
		ns.NotifyLevel = "quiet"
	}
	if ns.DreamAt != "" && !strings.EqualFold(ns.DreamAt, "off") {
		var dh, dm int
		if _, err := fmt.Sscanf(ns.DreamAt, "%d:%d", &dh, &dm); err != nil || dh < 0 || dh > 23 || dm < 0 || dm > 59 {
			ns.DreamAt = "04:00"
		} else {
			ns.DreamAt = fmt.Sprintf("%02d:%02d", dh, dm)
		}
	}
	if ns.WorkerCwd == "" {
		ns.WorkerCwd = e.home
	}
	normPubURL, urlErr := NormalizePublicURL(ns.PublicURL)
	if urlErr != nil {
		return model.Settings{}, false, urlErr
	}
	ns.PublicURL = normPubURL
	NormalizeDispatchConfig(&ns)
	if !preserveRouterAPI {
		if err := normalizeRouterAPISettings(&ns); err != nil {
			return model.Settings{}, false, err
		}
	}
	apiChanged := prev.RouterAPIProvider != ns.RouterAPIProvider ||
		prev.RouterAPIBaseURL != ns.RouterAPIBaseURL ||
		prev.RouterAPIModel != ns.RouterAPIModel ||
		prev.RouterAPIEffort != ns.RouterAPIEffort
	return ns, apiChanged, nil
}

func sanitizePoliciesForSave(pols []model.Policy) []model.Policy {
	valid := make([]model.Policy, 0, len(pols))
	for _, p := range pols {
		src := strings.TrimSpace(p.Source)
		if src != "" && (p.Action == "respond" || p.Action == "candidate" || p.Action == "ignore") {
			cp := model.Policy{Source: src, Action: p.Action}
			for _, u := range p.Allow {
				if u = strings.TrimSpace(u); u != "" {
					cp.Allow = append(cp.Allow, u)
				}
			}
			valid = append(valid, cp)
		}
	}
	return valid
}

func (e *Engine) buildAuditRingValue(ctx context.Context, actor, action, summary string) (string, error) {
	var ring []model.ConfigAuditEntry
	if e.store != nil {
		raw, ok, err := e.store.GetKV(ctx, configAuditRingKVKey)
		if err != nil {
			return "", fmt.Errorf("读取配置审计历史失败：%w", err)
		}
		if ok && strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &ring); err != nil {
				return "", fmt.Errorf("解析配置审计历史失败，已中止覆盖：%w", err)
			}
		}
	}
	ring = append(ring, model.ConfigAuditEntry{
		At:      time.Now(),
		Actor:   orStr(strings.TrimSpace(actor), "owner"),
		Action:  orStr(strings.TrimSpace(action), "save_config"),
		Summary: oneLineClip(strings.TrimSpace(summary), 200),
	})
	if len(ring) > maxConfigAuditRing {
		ring = ring[len(ring)-maxConfigAuditRing:]
	}
	b, err := json.Marshal(ring)
	if err != nil {
		return "", fmt.Errorf("编码配置审计历史失败：%w", err)
	}
	return string(b), nil
}

// ListConfigHistory returns up to limit recent redacted configuration audit entries (newest last).
func (e *Engine) ListConfigHistory(ctx context.Context, limit int) []model.ConfigAuditEntry {
	if e.store == nil {
		return []model.ConfigAuditEntry{}
	}
	raw, ok, err := e.store.GetKV(ctx, configAuditRingKVKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return []model.ConfigAuditEntry{}
	}
	var ring []model.ConfigAuditEntry
	if err := json.Unmarshal([]byte(raw), &ring); err != nil {
		return []model.ConfigAuditEntry{}
	}
	if limit > 0 && len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	return ring
}

func (e *Engine) commitConfigUnlocked(ctx context.Context, st *model.Settings, pols []model.Policy, preserveRouterAPI bool, actor, action, summary string, writeEvent bool) (model.Settings, []model.Policy, error) {
	prevSt, prevPols := e.Settings(), e.Policies()
	kvUpdates := map[string]string{}
	var (
		nextSt     = prevSt
		nextPols   = prevPols
		apiChanged bool
	)
	if st != nil {
		norm, changed, err := e.normalizeSettingsForSave(prevSt, *st, preserveRouterAPI)
		if err != nil {
			return prevSt, prevPols, err
		}
		b, err := json.Marshal(norm)
		if err != nil {
			return prevSt, prevPols, fmt.Errorf("encode settings: %w", err)
		}
		nextSt, apiChanged = norm, changed
		kvUpdates["settings"] = string(b)
	}
	if pols != nil {
		cleanPols := sanitizePoliciesForSave(pols)
		b, err := json.Marshal(cleanPols)
		if err != nil {
			return prevSt, prevPols, fmt.Errorf("encode policies: %w", err)
		}
		nextPols = cleanPols
		kvUpdates["policies"] = string(b)
	}
	if len(kvUpdates) == 0 {
		return prevSt, prevPols, nil
	}
	if summary != "" {
		auditVal, err := e.buildAuditRingValue(ctx, actor, action, summary)
		if err != nil {
			return prevSt, prevPols, err
		}
		kvUpdates[configAuditRingKVKey] = auditVal
	}
	if err := e.store.CommitKVTx(ctx, kvUpdates); err != nil {
		return prevSt, prevPols, fmt.Errorf("persist config tx: %w", err)
	}

	var resetBypass bool
	e.mu.Lock()
	if st != nil {
		resetBypass = e.settings.AskPermissions && !nextSt.AskPermissions
		e.settings = CloneSettings(nextSt)
		if apiChanged {
			e.router.resetFailuresLocked(true)
		}
	}
	if pols != nil {
		e.policies = ClonePolicies(nextPols)
	}
	sink := e.sink
	onApplied := e.onSettingsApplied
	e.mu.Unlock()
	if resetBypass {
		e.bypass.resetCaches()
	}

	if st != nil && onApplied != nil {
		onApplied(CloneSettings(nextSt))
	}
	if e.logger != nil {
		e.logger.Info("config updated", "actor", actor, "action", action, "summary", oneLineClip(summary, 160))
	}
	if writeEvent && summary != "" {
		e.addEvent(ctx, model.MainThread, "配置已更新："+oneLineClip(summary, 180), "", "info")
	}
	if apiChanged {
		e.wake()
	}
	sink.OnStateChanged()
	return CloneSettings(nextSt), ClonePolicies(nextPols), nil
}

// SaveConfig persists updated Settings and/or Policies atomically under the single config writer.
func (e *Engine) SaveConfig(ctx context.Context, st *model.Settings, pols []model.Policy) error {
	return e.withConfigWriteLock(ctx, func() error {
		summary := "通过控制台更新配置"
		if st != nil && pols == nil {
			summary = "通过控制台保存系统设置"
		} else if st == nil && pols != nil {
			summary = "通过控制台保存过滤策略"
		}
		_, _, err := e.commitConfigUnlocked(ctx, st, pols, false, "web", "save_config", summary, false)
		return err
	})
}

// ApproveWeComUser atomically appends uid to WeComAllowUsers inside the single config writer,
// preventing concurrent approval lost-updates and out-of-order WeCom allowlist reloads.
func (e *Engine) ApproveWeComUser(ctx context.Context, uid string) (model.Settings, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return model.Settings{}, fmt.Errorf("user_id is required")
	}
	var out model.Settings
	err := e.withConfigWriteLock(ctx, func() error {
		cur := e.Settings()
		exists := false
		for _, u := range cur.WeComAllowUsers {
			if strings.TrimSpace(u) == uid {
				exists = true
				break
			}
		}
		if !exists {
			cur.WeComAllowUsers = append(cur.WeComAllowUsers, uid)
		}
		next, _, err := e.commitConfigUnlocked(ctx, &cur, nil, true, "web", "wecom_approve", "批准企微白名单用户 "+uid, false)
		if err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}

func (e *Engine) updateManagedConfig(ctx context.Context, actor, action string, writeEvent bool, mutate func(st *model.Settings, pols *[]model.Policy) (stChanged, polsChanged bool, summary string, err error)) (model.Settings, []model.Policy, error) {
	var (
		outSt   model.Settings
		outPols []model.Policy
	)
	err := e.withConfigWriteLock(ctx, func() error {
		st, pols := e.Settings(), e.Policies()
		stChanged, polsChanged, summary, err := mutate(&st, &pols)
		if err != nil {
			return err
		}
		if !stChanged && !polsChanged {
			outSt, outPols = st, pols
			return nil
		}
		var stPtr *model.Settings
		var polsArg []model.Policy
		if stChanged {
			stPtr = &st
		}
		if polsChanged {
			polsArg = pols
		}
		nextSt, nextPols, err := e.commitConfigUnlocked(ctx, stPtr, polsArg, true, actor, action, summary, writeEvent)
		if err != nil {
			return err
		}
		outSt, outPols = nextSt, nextPols
		return nil
	})
	return outSt, outPols, err
}

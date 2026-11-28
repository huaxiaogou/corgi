package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// PatchSettingsResult captures the outcome of a validated manage_config(patch_settings) call.
type PatchSettingsResult struct {
	NeedsConfirm  bool           `json:"needs_confirm,omitempty"`
	BeforeHash    string         `json:"-"`
	NoOp          bool           `json:"no_op,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	AppliedFields map[string]any `json:"applied_fields,omitempty"`
}

// PatchManagedSettings validates and applies L1 operational settings patches under the single config writer,
// hard-rejects L3/unknown keys, and flags ask_permissions=false transitions for L2 confirmation.
func (e *Engine) PatchManagedSettings(ctx context.Context, actor string, rawPatch map[string]json.RawMessage) (PatchSettingsResult, error) {
	if len(rawPatch) == 0 {
		return PatchSettingsResult{}, fmt.Errorf("settings 不能为空对象")
	}
	var askPermTarget *bool
	for k, raw := range rawPatch {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			return PatchSettingsResult{}, fmt.Errorf("settings 包含空字段名")
		}
		if IsL3SettingKey(key) {
			return PatchSettingsResult{}, fmt.Errorf("字段 %s 属于高敏感通道/系统凭据配置（L3），大脑禁止修改，请在 Web 控制台设置页手动操作", key)
		}
		if key == "projects" {
			return PatchSettingsResult{}, fmt.Errorf("项目工作区请使用 manage_config 的 upsert_project / set_project_path / delete_project / remove_project_path 管理")
		}
		if !isManagedL1SettingKey(key) {
			return PatchSettingsResult{}, fmt.Errorf("不支持通过 manage_config 修改的设置字段：%s", key)
		}
		if key == "ask_permissions" {
			var b bool
			if err := decodeStrictJSON(raw, &b); err != nil {
				return PatchSettingsResult{}, fmt.Errorf("ask_permissions 必须是布尔值")
			}
			askPermTarget = &b
		}
	}

	if askPermTarget != nil && !*askPermTarget {
		if len(rawPatch) > 1 {
			return PatchSettingsResult{}, fmt.Errorf("关闭权限确认（ask_permissions=false）属高危变更，必须单独提交以触发确认卡片，禁止与其他字段混传")
		}
		stSnap, _, beforeHash := e.configSnapshot()
		if !stSnap.AskPermissions {
			return PatchSettingsResult{
				NoOp:          true,
				Summary:       "ask_permissions 当前已为 false，无需重复修改",
				AppliedFields: map[string]any{"ask_permissions": false},
			}, nil
		}
		return PatchSettingsResult{NeedsConfirm: true, BeforeHash: beforeHash}, nil
	}

	var res PatchSettingsResult
	_, _, err := e.updateManagedConfig(ctx, actor, "patch_settings", true, func(st *model.Settings, _ *[]model.Policy) (bool, bool, string, error) {
		appliedKeys := make([]string, 0, len(rawPatch))
		// Apply worker_kind and worker_machine before worker_cwd/worker_model/worker_effort.
		orderedKeys := slices.Sorted(maps.Keys(rawPatch))
		orderedKeys = prioritizeKey(orderedKeys, "kind_defaults")
		orderedKeys = prioritizeKey(orderedKeys, "worker_kind")
		orderedKeys = prioritizeKey(orderedKeys, "worker_machine")

		var (
			patchedModel  *string
			patchedEffort *string
			explicitKD    = map[string][2]bool{}
		)
		for _, origKey := range orderedKeys {
			key := strings.ToLower(strings.TrimSpace(origKey))
			raw := rawPatch[origKey]
			if err := e.applyOneManagedSetting(ctx, st, key, raw, &patchedModel, &patchedEffort, explicitKD); err != nil {
				return false, false, "", err
			}
			appliedKeys = append(appliedKeys, key)
		}
		if patchedModel != nil || patchedEffort != nil {
			effKind := agent.NormalizeKind(st.WorkerKind)
			if effKind != "" {
				if st.KindDefaults == nil {
					st.KindDefaults = map[string]model.ModelEffortSpec{}
				}
				spec := st.KindDefaults[effKind]
				if patchedModel != nil {
					if explicitKD[effKind][0] && spec.Model != *patchedModel {
						return false, false, "", fmt.Errorf("同时修改了 worker_model 与 kind_defaults[%s].model 且值不一致", effKind)
					}
					spec.Model = *patchedModel
				}
				if patchedEffort != nil {
					if explicitKD[effKind][1] && spec.Effort != *patchedEffort {
						return false, false, "", fmt.Errorf("同时修改了 worker_effort 与 kind_defaults[%s].effort 且值不一致", effKind)
					}
					spec.Effort = *patchedEffort
				}
				if spec.Model == "" && spec.Effort == "" {
					delete(st.KindDefaults, effKind)
				} else {
					st.KindDefaults[effKind] = spec
				}
			}
		}
		NormalizeDispatchConfig(st)
		summary := "更新配置项：" + strings.Join(appliedKeys, ", ")
		res = PatchSettingsResult{
			Summary:       summary,
			AppliedFields: snapshotAppliedFields(*st, appliedKeys),
		}
		return true, false, summary, nil
	})
	if err != nil {
		return PatchSettingsResult{}, err
	}
	return res, nil
}

func isManagedL1SettingKey(k string) bool {
	switch k {
	case "worker_machine", "worker_kind", "worker_model", "worker_effort", "worker_cwd",
		"kind_defaults", "machine_kind_overrides",
		"active_hours", "idle_patrol_minutes", "stall_minutes", "stale_archive_days",
		"worker_idle_gc_minutes", "worker_max_live_per_machine", "reply_chars",
		"router_context_budget_tokens",
		"morning_digest", "dream_at", "notify_level", "ask_permissions":
		return true
	default:
		return false
	}
}

func prioritizeKey(keys []string, first string) []string {
	idx := slices.IndexFunc(keys, func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), first) })
	if idx <= 0 {
		return keys
	}
	out := make([]string, 0, len(keys))
	out = append(out, keys[idx])
	out = append(out, keys[:idx]...)
	out = append(out, keys[idx+1:]...)
	return out
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func decodeStrictJSON(raw json.RawMessage, dst any) error {
	if isJSONNull(raw) {
		return fmt.Errorf("不允许 null")
	}
	return json.Unmarshal(raw, dst)
}

func decodeOptionalString(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	if hasControlRune(s) {
		return "", fmt.Errorf("包含非法控制字符")
	}
	return strings.TrimSpace(s), nil
}

func (e *Engine) applyOneManagedSetting(ctx context.Context, st *model.Settings, key string, raw json.RawMessage, patchedModel, patchedEffort **string, explicitKD map[string][2]bool) error {
	switch key {
	case "worker_machine":
		v, err := decodeOptionalString(raw)
		if err != nil {
			return fmt.Errorf("worker_machine 无效：%w", err)
		}
		if v == "" || strings.EqualFold(v, "hub") || strings.EqualFold(v, "local") {
			st.WorkerMachine = ""
		} else {
			_, targetID := e.resolveMachineKeyForConfig(ctx, v)
			st.WorkerMachine = targetID
		}
	case "worker_kind":
		var v string
		if err := decodeStrictJSON(raw, &v); err != nil {
			return fmt.Errorf("worker_kind 必须是字符串")
		}
		norm := agent.NormalizeKind(v)
		if norm == "" {
			return fmt.Errorf("不支持的 worker_kind：%s（支持 claude / codex / agy / codebuddy）", v)
		}
		st.WorkerKind = norm
	case "worker_model":
		v, err := decodeOptionalString(raw)
		if err != nil {
			return fmt.Errorf("worker_model 无效：%w", err)
		}
		if v != "" && !agent.ModelNameRe.MatchString(v) {
			return fmt.Errorf("worker_model 格式无效：%s", v)
		}
		st.WorkerModel = v
		*patchedModel = &v
	case "worker_effort":
		v, err := decodeOptionalString(raw)
		if err != nil {
			return fmt.Errorf("worker_effort 无效：%w", err)
		}
		if v != "" && !agent.ModelNameRe.MatchString(v) {
			return fmt.Errorf("worker_effort 格式无效：%s", v)
		}
		st.WorkerEffort = v
		*patchedEffort = &v
	case "worker_cwd":
		v, err := decodeOptionalString(raw)
		if err != nil {
			return fmt.Errorf("worker_cwd 无效：%w", err)
		}
		var (
			clean string
			vErr  error
		)
		if st.WorkerMachine == "" {
			clean, vErr = ValidateLocalWorkerCwd(e.home, e.corgiDir, e.dbPath(), v)
		} else {
			clean, vErr = ValidateWorkerCwd(v, "")
		}
		if vErr != nil {
			return vErr
		}
		st.WorkerCwd = orStr(clean, "~")
	case "kind_defaults":
		var patch map[string]model.ModelEffortSpec
		if err := decodeStrictJSON(raw, &patch); err != nil {
			return fmt.Errorf("kind_defaults 必须是对象：%w", err)
		}
		if st.KindDefaults == nil {
			st.KindDefaults = map[string]model.ModelEffortSpec{}
		}
		for rk, spec := range patch {
			nk := agent.NormalizeKind(rk)
			if nk == "" {
				return fmt.Errorf("kind_defaults 包含不支持的 CLI 类型：%s", rk)
			}
			m, ef := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.Effort)
			if m != "" && !agent.ModelNameRe.MatchString(m) {
				return fmt.Errorf("kind_defaults[%s].model 格式无效：%s", nk, m)
			}
			if ef != "" && !agent.ModelNameRe.MatchString(ef) {
				return fmt.Errorf("kind_defaults[%s].effort 格式无效：%s", nk, ef)
			}
			explicitKD[nk] = [2]bool{true, true}
			if m == "" && ef == "" {
				delete(st.KindDefaults, nk)
			} else {
				st.KindDefaults[nk] = model.ModelEffortSpec{Model: m, Effort: ef}
			}
		}
	case "machine_kind_overrides":
		var patch map[string]map[string]model.ModelEffortSpec
		if err := decodeStrictJSON(raw, &patch); err != nil {
			return fmt.Errorf("machine_kind_overrides 必须是嵌套对象：%w", err)
		}
		if st.MachineKindOverrides == nil {
			st.MachineKindOverrides = map[string]map[string]model.ModelEffortSpec{}
		}
		for rm, byKind := range patch {
			mk, _ := e.resolveMachineKeyForConfig(ctx, rm)
			if st.MachineKindOverrides[mk] == nil {
				st.MachineKindOverrides[mk] = map[string]model.ModelEffortSpec{}
			}
			for rk, spec := range byKind {
				nk := agent.NormalizeKind(rk)
				if nk == "" {
					return fmt.Errorf("machine_kind_overrides[%s] 包含不支持的 CLI 类型：%s", mk, rk)
				}
				m, ef := strings.TrimSpace(spec.Model), strings.TrimSpace(spec.Effort)
				if m != "" && !agent.ModelNameRe.MatchString(m) {
					return fmt.Errorf("machine_kind_overrides[%s][%s].model 格式无效：%s", mk, nk, m)
				}
				if ef != "" && !agent.ModelNameRe.MatchString(ef) {
					return fmt.Errorf("machine_kind_overrides[%s][%s].effort 格式无效：%s", mk, nk, ef)
				}
				if m == "" && ef == "" {
					delete(st.MachineKindOverrides[mk], nk)
				} else {
					st.MachineKindOverrides[mk][nk] = model.ModelEffortSpec{Model: m, Effort: ef}
				}
			}
		}
	case "active_hours":
		var v string
		if err := decodeStrictJSON(raw, &v); err != nil {
			return fmt.Errorf("active_hours 必须是字符串")
		}
		if _, _, _, ok := ParseActiveHours(v); !ok {
			return fmt.Errorf("active_hours 格式无效：%s（示例：09:00-22:30 或 24h）", v)
		}
		st.ActiveHours = NormalizeActiveHours(v)
	case "idle_patrol_minutes":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || (v != -1 && (v < 1 || v > 480)) {
			return fmt.Errorf("idle_patrol_minutes 必须为 -1（关闭）或 1–480")
		}
		st.IdlePatrolMinutes = v
	case "stall_minutes":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || v < 1 || v > 240 {
			return fmt.Errorf("stall_minutes 必须为 1–240")
		}
		st.StallMinutes = v
	case "stale_archive_days":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || (v != -1 && (v < 1 || v > 60)) {
			return fmt.Errorf("stale_archive_days 必须为 -1（关闭）或 1–60")
		}
		st.StaleArchiveDays = v
	case "worker_idle_gc_minutes":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || (v != -1 && (v < 15 || v > 1440)) {
			return fmt.Errorf("worker_idle_gc_minutes 必须为 -1（关闭）或 15–1440")
		}
		st.WorkerIdleGCMinutes = v
	case "worker_max_live_per_machine":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || (v != -1 && (v < 1 || v > 50)) {
			return fmt.Errorf("worker_max_live_per_machine 必须为 -1（不限）或 1–50")
		}
		st.WorkerMaxLivePerMachine = v
	case "reply_chars":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || v < 40 || v > 4000 {
			return fmt.Errorf("reply_chars 必须为 40–4000")
		}
		st.ReplyChars = v
	case "router_context_budget_tokens":
		var v int
		if err := decodeStrictJSON(raw, &v); err != nil || v < model.MinRouterContextBudgetTokens || v > model.MaxRouterContextBudgetTokens {
			return fmt.Errorf("router_context_budget_tokens 必须为 %d–%d", model.MinRouterContextBudgetTokens, model.MaxRouterContextBudgetTokens)
		}
		st.RouterContextBudgetTokens = v
	case "morning_digest":
		var v bool
		if err := decodeStrictJSON(raw, &v); err != nil {
			return fmt.Errorf("morning_digest 必须是布尔值")
		}
		st.MorningDigest = &v
	case "dream_at":
		var v string
		if err := decodeStrictJSON(raw, &v); err != nil {
			return fmt.Errorf("dream_at 必须是字符串")
		}
		v = strings.TrimSpace(v)
		if strings.EqualFold(v, "off") {
			st.DreamAt = "off"
		} else {
			var dh, dm int
			if _, err := fmt.Sscanf(v, "%d:%d", &dh, &dm); err != nil || dh < 0 || dh > 23 || dm < 0 || dm > 59 {
				return fmt.Errorf("dream_at 格式无效：%s（请使用 HH:MM 或 off）", v)
			}
			st.DreamAt = fmt.Sprintf("%02d:%02d", dh, dm)
		}
	case "notify_level":
		var v string
		if err := decodeStrictJSON(raw, &v); err != nil {
			return fmt.Errorf("notify_level 必须是字符串")
		}
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "quiet" && v != "normal" {
			return fmt.Errorf("notify_level 仅支持 quiet 或 normal")
		}
		st.NotifyLevel = v
	case "ask_permissions":
		var v bool
		if err := decodeStrictJSON(raw, &v); err != nil || !v {
			return fmt.Errorf("ask_permissions 直写仅允许设为 true")
		}
		st.AskPermissions = true
	}
	return nil
}

package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const maxManagedPolicies = 100

// UpsertManagedPolicy validates and upserts an external source policy under the single config writer.
func (e *Engine) UpsertManagedPolicy(ctx context.Context, actor string, pol model.Policy) (model.Policy, error) {
	src := strings.TrimSpace(pol.Source)
	if src == "" || len(src) > 64 || hasControlRune(src) {
		return model.Policy{}, fmt.Errorf("policy.source 不能为空、不能超过 64 字符或包含控制字符")
	}
	if IsProtectedPolicySource(src) {
		return model.Policy{}, fmt.Errorf("内置来源 %q 受保护，禁止通过 manage_config 修改", src)
	}
	act := strings.ToLower(strings.TrimSpace(pol.Action))
	if act != "respond" && act != "candidate" && act != "ignore" {
		return model.Policy{}, fmt.Errorf("policy.action 必须是 respond、candidate 或 ignore")
	}
	var allow []string
	for _, u := range pol.Allow {
		if hasControlRune(u) {
			return model.Policy{}, fmt.Errorf("policy.allow 包含非法控制字符")
		}
		if u = strings.TrimSpace(u); u != "" && !slices.Contains(allow, u) {
			allow = append(allow, u)
		}
	}
	clean := model.Policy{Source: src, Action: act, Allow: allow}
	_, _, err := e.updateManagedConfig(ctx, actor, "upsert_policy", true, func(_ *model.Settings, pols *[]model.Policy) (bool, bool, string, error) {
		list := *pols
		for i := range list {
			if list[i].Source == src {
				list[i] = clean
				*pols = list
				return false, true, fmt.Sprintf("更新过滤策略 %s -> %s", src, act), nil
			}
		}
		if len(list) >= maxManagedPolicies {
			return false, false, "", fmt.Errorf("过滤策略数量已达上限（%d 条）", maxManagedPolicies)
		}
		*pols = append(list, clean)
		return false, true, fmt.Sprintf("新增过滤策略 %s -> %s", src, act), nil
	})
	return clean, err
}

func applyDeletePolicy(pols *[]model.Policy, source string) (string, error) {
	src := strings.TrimSpace(source)
	if IsProtectedPolicySource(src) {
		return "", fmt.Errorf("内置来源 %q 受保护，禁止删除", src)
	}
	list := *pols
	for i := range list {
		if list[i].Source == src {
			*pols = append(list[:i], list[i+1:]...)
			return fmt.Sprintf("删除过滤策略 %s", src), nil
		}
	}
	return "", fmt.Errorf("未找到过滤策略：%s", src)
}

func snapshotAppliedFields(st model.Settings, keys []string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		switch k {
		case "worker_machine":
			out[k] = orStr(st.WorkerMachine, "hub")
		case "worker_kind":
			out[k] = st.WorkerKind
		case "worker_model":
			out[k] = st.WorkerModel
		case "worker_effort":
			out[k] = st.WorkerEffort
		case "worker_cwd":
			out[k] = st.WorkerCwd
		case "kind_defaults":
			out[k] = st.KindDefaults
		case "machine_kind_overrides":
			out[k] = st.MachineKindOverrides
		case "active_hours":
			out[k] = st.EffectiveActiveHours()
		case "idle_patrol_minutes":
			out[k] = st.IdlePatrolMinutes
		case "stall_minutes":
			out[k] = st.StallMinutes
		case "stale_archive_days":
			out[k] = st.StaleArchiveDays
		case "worker_idle_gc_minutes":
			out[k] = st.WorkerIdleGCMinutes
		case "worker_max_live_per_machine":
			out[k] = st.WorkerMaxLivePerMachine
		case "reply_chars":
			out[k] = st.ReplyChars
		case "router_context_budget_tokens":
			out[k] = st.EffectiveRouterContextBudgetTokens()
		case "morning_digest":
			out[k] = st.MorningDigestEnabled()
		case "dream_at":
			out[k] = orStr(st.DreamAt, "04:00")
		case "notify_level":
			out[k] = st.NotifyLevel
		case "ask_permissions":
			out[k] = st.AskPermissions
		}
	}
	return out
}

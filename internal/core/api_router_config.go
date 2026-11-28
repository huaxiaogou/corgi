package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

func condStr(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func configRouterToolDef() LLMToolDef {
	return toolSpec("manage_config", "查看或修改 Hub 运营配置、项目工作区、外部过滤策略、检查/一键升级或手动触发整理记忆（仅限主线程；写操作强制仅限人类主对话回合；删除项目/路径/策略、关闭 ask_permissions 或触发一键升级会自动下发确认卡片；严禁修改大脑 API 与企微凭据）。", map[string]any{
		"action": propEnum([]string{
			"get", "history", "check_update",
			"upsert_project", "set_project_path", "delete_project", "remove_project_path",
			"patch_settings", "upsert_policy", "delete_policy", "trigger_dream", "trigger_update",
		}, "操作类型"),
		"section":       propEnum([]string{"all", "projects", "dispatch", "schedule", "policies", "machines"}, "action=get 时的只读视图分区（默认 all，自动脱敏）"),
		"project":       prop("string", "项目 ID 或名称（project 相关操作使用）"),
		"name":          prop("string", "项目展示名（upsert_project 新建时必填）"),
		"aliases":       prop("[]string", "项目匹配别名列表"),
		"note":          prop("string", "项目备注/默认开发约定（≤500字）"),
		"machine":       prop("string", "机器 ID 或名称，本机填 hub"),
		"path":          prop("string", "机器上的代码仓库绝对路径（或 ~/...）"),
		"paths":         map[string]any{"type": "object", "description": "upsert_project 可选批量路径映射（仅增/改，不删既有映射）"},
		"settings":      map[string]any{"type": "object", "description": "patch_settings 的部分更新对象（仅允许运营白名单字段，严禁 router_api_*/wecom_*/public_url/auto_update/paused）"},
		"policy":        map[string]any{"type": "object", "description": "upsert_policy 的策略对象 {source, action(respond|candidate|ignore), allow([]string)}"},
		"policy_source": prop("string", "delete_policy 目标策略 source（如 webhook:github）"),
	}, "action")
}

func requireHumanConfigTurn(turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta) string {
	if turn == nil || turn.thread != model.MainThread {
		return "非主线程（如企微群聊）禁止查看或修改全局配置"
	}
	if trig == nil || meta == nil || !meta.isHuman {
		return "只有主线程人类对话回合可以修改系统配置（后台巡检、定时器、Worker 汇报或 Webhook 均禁止改配置）"
	}
	en := meta.sourceEntry
	if en == nil || en.Role != "user" || orStr(en.Thread, model.MainThread) != model.MainThread || !isHumanChannel(en.Channel) {
		return "无法验证可信人类消息来源，已拒绝修改配置"
	}
	if strings.TrimSpace(en.ActionID) != "" && meta.approvedAction == nil {
		return "卡片触发来源未经有效人类渠道确认，已拒绝修改配置"
	}
	return ""
}

func (e *Engine) toolManageConfig(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta, rawArgs string) (string, bool) {
	if turn == nil || turn.thread != model.MainThread {
		return toolErr("企微群聊或非主线程禁止访问或修改全局配置"), false
	}
	var args struct {
		Action       string                     `json:"action"`
		Section      string                     `json:"section"`
		Project      string                     `json:"project"`
		Name         string                     `json:"name"`
		Aliases      []string                   `json:"aliases"`
		Note         *string                    `json:"note"`
		Machine      string                     `json:"machine"`
		Path         string                     `json:"path"`
		Paths        map[string]string          `json:"paths"`
		Settings     map[string]json.RawMessage `json:"settings"`
		Policy       *model.Policy              `json:"policy"`
		PolicySource string                     `json:"policy_source"`
		PolicyID     string                     `json:"policy_id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("manage_config 参数 JSON 无效：" + err.Error()), false
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	switch action {
	case "get":
		return toolJSON(map[string]any{"ok": true, "config": e.BuildRedactedConfigView(ctx, args.Section)}), false
	case "history":
		return toolJSON(map[string]any{"ok": true, "history": e.ListConfigHistory(ctx, maxConfigAuditRing)}), false
	case "check_update":
		res, err := e.CheckFleetUpdate(ctx)
		if err != nil {
			return toolErr(err.Error()), false
		}
		return toolJSON(map[string]any{"ok": true, "update": res}), false
	case "upsert_project":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		req := UpsertProjectRequest{
			Project: args.Project, Name: args.Name, Aliases: args.Aliases,
			Machine: args.Machine, Path: args.Path, Paths: args.Paths,
		}
		if args.Note != nil {
			req.HasNote, req.Note = true, *args.Note
		}
		res, err := e.UpsertProject(ctx, "router", req)
		if err != nil {
			return toolErr(err.Error()), false
		}
		turn.markMutated("已更新项目 " + res.Project.Name)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		out := map[string]any{"ok": true, "created": res.Created, "project": res.Project}
		if len(res.Warnings) > 0 {
			out["warnings"] = res.Warnings
		}
		return toolJSON(out), false
	case "set_project_path":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		res, err := e.SetProjectPath(ctx, "router", args.Project, args.Machine, args.Path)
		if err != nil {
			return toolErr(err.Error()), false
		}
		turn.markMutated("已设置项目路径 " + res.Project.Name)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		out := map[string]any{"ok": true, "project": res.Project}
		if len(res.Warnings) > 0 {
			out["warnings"] = res.Warnings
		}
		return toolJSON(out), false
	case "delete_project":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		st, _, beforeHash := e.configSnapshot()
		idx := findProjectIndex(st.Projects, args.Project)
		if idx < 0 {
			return toolErr("未找到已配置的项目：" + strings.TrimSpace(args.Project)), false
		}
		p := st.Projects[idx]
		return e.sendConfigConfirmCard(ctx, turn, trig, beforeHash, configOpDeleteProject,
			"确认删除项目："+p.Name,
			fmt.Sprintf("即将删除项目工作区「%s」（ID: %s，含 %d 条机器路径映射）。", p.Name, p.ID, len(p.Paths)),
			deleteProjectPayload{Project: p.ID})
	case "remove_project_path":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		st, _, beforeHash := e.configSnapshot()
		idx := findProjectIndex(st.Projects, args.Project)
		if idx < 0 {
			return toolErr("未找到已配置的项目：" + strings.TrimSpace(args.Project)), false
		}
		mk, _ := e.resolveMachineKeyForConfig(ctx, args.Machine)
		p := st.Projects[idx]
		oldPath, ok := p.Paths[mk]
		if !ok {
			return toolErr(fmt.Sprintf("项目 %s 未配置机器 %s 的路径", p.Name, mk)), false
		}
		return e.sendConfigConfirmCard(ctx, turn, trig, beforeHash, configOpRemoveProjectPath,
			fmt.Sprintf("确认移除 %s 在 %s 的路径", p.Name, mk),
			fmt.Sprintf("即将移除项目「%s」在机器 %s 上的路径映射：%s。", p.Name, mk, oldPath),
			removeProjectPathPayload{Project: p.ID, Machine: mk})
	case "patch_settings":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		res, err := e.PatchManagedSettings(ctx, "router", args.Settings)
		if err != nil {
			return toolErr(err.Error()), false
		}
		if res.NeedsConfirm {
			return e.sendConfigConfirmCard(ctx, turn, trig, res.BeforeHash, configOpDisableAskPermissions,
				"确认关闭权限确认（ask_permissions）",
				"即将将全站 ask_permissions 从 true 调整为 false（允许 Coding CLI 自动执行操作而不再弹窗确认）。",
				map[string]any{"ask_permissions": false})
		}
		if !res.NoOp {
			turn.markMutated(res.Summary)
			e.ackAPITriggerOnce(context.Background(), turn, trig)
		}
		return toolJSON(map[string]any{"ok": true, "no_op": res.NoOp, "summary": res.Summary, "applied_fields": res.AppliedFields}), false
	case "upsert_policy":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		if args.Policy == nil {
			return toolErr("upsert_policy 必须提供 policy 对象 {source, action, allow}"), false
		}
		saved, err := e.UpsertManagedPolicy(ctx, "router", *args.Policy)
		if err != nil {
			return toolErr(err.Error()), false
		}
		turn.markMutated("已更新过滤策略 " + saved.Source)
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "policy": saved}), false
	case "delete_policy":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		src := orStr(strings.TrimSpace(args.PolicySource), strings.TrimSpace(args.PolicyID))
		if src == "" {
			return toolErr("delete_policy 必须提供 policy_source"), false
		}
		if IsProtectedPolicySource(src) {
			return toolErr(fmt.Sprintf("内置来源 %q 受保护，禁止删除", src)), false
		}
		_, pols, beforeHash := e.configSnapshot()
		found := false
		for _, p := range pols {
			if p.Source == src {
				found = true
				break
			}
		}
		if !found {
			return toolErr("未找到过滤策略：" + src), false
		}
		return e.sendConfigConfirmCard(ctx, turn, trig, beforeHash, configOpDeletePolicy,
			"确认删除过滤策略："+src,
			fmt.Sprintf("即将删除外部来源过滤策略「%s」（删除后该来源将回退为默认 candidate 处理）。", src),
			deletePolicyPayload{Source: src})
	case "trigger_dream":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		if err := e.StartDream(ctx, "manual"); err != nil {
			return toolErr(err.Error()), false
		}
		turn.markMutated("已启动后台记忆整理（做梦）")
		e.ackAPITriggerOnce(context.Background(), turn, trig)
		return toolJSON(map[string]any{"ok": true, "started": true}), false
	case "trigger_update":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		_, _, beforeHash := e.configSnapshot()
		checkRes, err := e.CheckFleetUpdate(ctx)
		if err != nil {
			return toolErr(err.Error()), false
		}
		if checkRes.Busy {
			return toolJSON(map[string]any{"ok": true, "busy": true, "update": checkRes}), false
		}
		if !checkRes.HasUpdate {
			return toolJSON(map[string]any{"ok": true, "already_up_to_date": true, "update": checkRes}), false
		}
		names := make([]string, 0, len(checkRes.Outdated))
		for _, m := range checkRes.Outdated {
			names = append(names, fmt.Sprintf("%s(%s)", orStr(m.Name, m.ID), orStr(m.Version, "unknown")))
		}
		return e.sendConfigConfirmCard(ctx, turn, trig, beforeHash, configOpTriggerUpdate,
			"确认一键升级至 "+checkRes.LatestVersion,
			fmt.Sprintf("即将把 %d 台在线设备从当前版本升级至 %s：%s（先逐台升级远端节点，最后 Hub 自动重启）。", len(checkRes.Outdated), checkRes.LatestVersion, strings.Join(names, "、")),
			triggerUpdatePayload{TargetVersion: checkRes.LatestVersion, CurrentVersion: checkRes.CurrentVersion, OutdatedNames: names})
	default:
		return toolErr("不支持的 manage_config action：" + args.Action), false
	}
}

// BuildRedactedConfigView returns a strict whitelist view of Hub operational configuration.
// Plaintext credentials (Router API Key, WeCom Secret, Owner Token, Hook Key) are NEVER included.
func (e *Engine) BuildRedactedConfigView(ctx context.Context, section string) map[string]any {
	st, pols := e.Settings(), e.Policies()
	sec := strings.ToLower(strings.TrimSpace(section))
	if sec == "" {
		sec = "all"
	}
	dispatchView := map[string]any{
		"worker_machine":         orStr(st.WorkerMachine, "hub"),
		"worker_kind":            st.WorkerKind,
		"worker_model":           st.WorkerModel,
		"worker_effort":          st.WorkerEffort,
		"worker_cwd":             st.WorkerCwd,
		"kind_defaults":          st.KindDefaults,
		"machine_kind_overrides": st.MachineKindOverrides,
		"ask_permissions":        st.AskPermissions,
	}
	scheduleView := map[string]any{
		"active_hours":                st.EffectiveActiveHours(),
		"idle_patrol_minutes":         st.IdlePatrolMinutes,
		"stall_minutes":               st.StallMinutes,
		"stale_archive_days":          st.StaleArchiveDays,
		"worker_idle_gc_minutes":      st.WorkerIdleGCMinutes,
		"worker_max_live_per_machine": st.WorkerMaxLivePerMachine,
		"reply_chars":                 st.ReplyChars,
		"morning_digest":              st.MorningDigestEnabled(),
		"dream_at":                    orStr(st.DreamAt, "04:00"),
		"notify_level":                orStr(st.NotifyLevel, "quiet"),
		"paused":                      st.Paused,
		"auto_update":                 st.AutoUpdate,
	}
	machinesView := []map[string]any{}
	if e.fleet != nil {
		for _, m := range e.fleet.Machines() {
			machinesView = append(machinesView, map[string]any{
				"id":     condStr(m.Local, "hub", m.ID),
				"name":   m.Name,
				"local":  m.Local,
				"online": m.Online,
				"os":     m.OS,
				"agents": m.Agents,
			})
		}
	}
	switch sec {
	case "projects":
		return map[string]any{"section": "projects", "projects": st.Projects}
	case "dispatch":
		return map[string]any{"section": "dispatch", "dispatch": dispatchView}
	case "schedule":
		return map[string]any{"section": "schedule", "schedule": scheduleView}
	case "policies":
		return map[string]any{"section": "policies", "policies": pols}
	case "machines":
		return map[string]any{"section": "machines", "machines": machinesView}
	default:
		secInfo := e.RouterAPISecretInfo(ctx)
		return map[string]any{
			"section":  "all",
			"projects": st.Projects,
			"dispatch": dispatchView,
			"schedule": scheduleView,
			"policies": pols,
			"machines": machinesView,
			"status": map[string]any{
				"router_api_provider": st.RouterAPIProvider,
				"router_api_model":    st.RouterAPIModel,
				"router_api_effort":   st.RouterAPIEffort,
				"has_router_api_key":  secInfo.HasKey,
				"wecom_enabled":       st.WeComEnabled,
				"wecom_configured":    strings.TrimSpace(st.WeComBotID) != "" && strings.TrimSpace(st.WeComSecret) != "",
				"wecom_allow_count":   len(st.WeComAllowUsers),
			},
		}
	}
}

package core

import (
	"context"
	"encoding/json"
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
	return toolSpec("manage_config", "查看或修改 Hub 运营配置与项目工作区（仅限主线程；写操作强制仅限人类主对话回合；严禁触碰大脑 API 凭据与企微密钥）。", map[string]any{
		"action":  propEnum([]string{"get", "history", "upsert_project", "set_project_path"}, "操作类型"),
		"section": propEnum([]string{"all", "projects", "dispatch", "schedule", "policies", "machines"}, "action=get 时的只读视图分区（默认 all，自动脱敏）"),
		"project": prop("string", "项目 ID 或名称（upsert_project / set_project_path 使用）"),
		"name":    prop("string", "项目展示名（upsert_project 新建时必填）"),
		"aliases": prop("[]string", "项目匹配别名列表"),
		"note":    prop("string", "项目备注/默认开发约定（≤500字）"),
		"machine": prop("string", "机器 ID 或名称，本机填 hub"),
		"path":    prop("string", "机器上的代码仓库绝对路径（或 ~/...）"),
		"paths":   map[string]any{"type": "object", "description": "upsert_project 可选批量路径映射（仅增/改，不删既有映射）"},
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
		Action  string            `json:"action"`
		Section string            `json:"section"`
		Project string            `json:"project"`
		Name    string            `json:"name"`
		Aliases []string          `json:"aliases"`
		Note    *string           `json:"note"`
		Machine string            `json:"machine"`
		Path    string            `json:"path"`
		Paths   map[string]string `json:"paths"`
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
	case "upsert_project":
		if msg := requireHumanConfigTurn(turn, trig, meta); msg != "" {
			return toolErr(msg), false
		}
		req := UpsertProjectRequest{
			Project: args.Project,
			Name:    args.Name,
			Aliases: args.Aliases,
			Machine: args.Machine,
			Path:    args.Path,
			Paths:   args.Paths,
		}
		if args.Note != nil {
			req.HasNote = true
			req.Note = *args.Note
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

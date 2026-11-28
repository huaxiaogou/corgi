package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// LaunchManagedRequest describes a user-initiated agent launch from the `+` modal or History/Card resume.
type LaunchManagedRequest struct {
	Kind                string
	Machine             string
	Cwd                 string
	Name                string
	Label               string
	Model               string
	Effort              string
	Prompt              string
	SessionID           string
	WorkItemID          string
	ProjectID           string
	Worktree            string // auto (default) | on | off
	Constraint          string
	Mode                string
	ReadonlyEnforcement string
	ExtraArgs           []string
	CreatedBy           string
	Thread              string
	Keep                bool
	RepoRoot            string
	RepoCommonDir       string
	WorktreePath        string
	WorktreeBranch      string
	WorktreeBase        string
	Subscribers         []string
}

// LaunchManaged starts an interactive agent on any connected machine and registers it immediately
// as a Corgi-managed Worker with full turn reporting, dialog card protection, and Router visibility.
func (e *Engine) LaunchManaged(ctx context.Context, req LaunchManagedRequest) (*model.Worker, error) {
	kind := agent.NormalizeKind(req.Kind)
	ad, err := e.agents.Get(kind)
	if err != nil {
		return nil, err
	}
	machine := e.fleet.ResolveMachine(req.Machine)
	mc, ok := e.fleet.Machine(machine)
	if !ok {
		return nil, fmt.Errorf("电脑不在线：%s", orStr(req.Machine, machine))
	}
	machine = mc.ID

	have := append([]string(nil), mc.Agents...)
	if mc.Local {
		for _, k := range e.agents.DetectInstalled(e.home, false) {
			if !slices.Contains(have, k) {
				have = append(have, k)
			}
		}
		if len(have) == 0 {
			if chkErr := e.agents.CheckInstalled(e.home, kind); chkErr != nil {
				return nil, chkErr
			}
		}
	}
	if len(have) > 0 && !slices.Contains(have, kind) {
		return nil, fmt.Errorf("%s 未安装 %s（可用：%s）", mc.Name, kind, strings.Join(have, ", "))
	}

	targetHome := mc.Home
	if mc.Local && targetHome == "" {
		targetHome = e.home
	}
	cwd := expandInHome(targetHome, orStr(strings.TrimSpace(req.Cwd), "~"))
	if mc.Local && cwd != "" {
		if fi, statErr := os.Stat(cwd); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("目录不存在: %s", cwd)
		}
	}

	explicitName := strings.TrimSpace(req.Name) != ""
	name := strings.TrimSpace(req.Name)
	if name == "" {
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", kind, seq)
	}
	if !workerNameRe.MatchString(name) || name == model.RouterName || name == model.DreamName {
		return nil, fmt.Errorf("名称需以小写字母开头，仅含字母/数字/-/_")
	}
	if existing, gErr := e.store.GetWorker(ctx, name); gErr == nil && !existing.Ended {
		if explicitName {
			return nil, fmt.Errorf("受管会话 %s 已在运行中", name)
		}
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", name, seq)
	}

	e.wtMu.Lock()
	var wtPrep *worktreePrep
	if req.SessionID == "" {
		effCwd, prep, wErr := e.prepareWorktree(ctx, machine, cwd, name, req.Worktree)
		if wErr != nil {
			e.wtMu.Unlock()
			return nil, wErr
		}
		cwd = effCwd
		wtPrep = prep
	}
	e.wtMu.Unlock()

	st := e.Settings()
	if !st.AskPermissions {
		e.preTrustWorkspace(ctx, machine, kind, cwd)
	}

	effModel, effEffort, modelSource := ResolveWorkerModel(st, machine, kind, req.Model, req.Effort)
	workerBin := e.binPath
	if !mc.Local || workerBin == "" {
		workerBin = "corgi"
	}
	workerMode := strings.ToLower(strings.TrimSpace(req.Mode))
	readonly := workerMode == "readonly"
	roEnforcement := strings.TrimSpace(req.ReadonlyEnforcement)
	if readonly && roEnforcement == "" {
		roEnforcement = string(ad.ReadonlyEnforcement())
	}
	// Codex `resume <id>` is a subcommand and must precede launch flags.
	args := append(ad.ResumeArgs(req.SessionID), ad.LaunchArgsWithOptions(agent.LaunchOptions{
		Model:      effModel,
		Effort:     effEffort,
		BypassPerm: !st.AskPermissions,
		Readonly:   readonly,
	})...)
	if kind == "claude" {
		args = append(args, "--allowedTools", fmt.Sprintf("Bash(%s report:*),Bash(corgi report:*)", workerBin))
	}
	args = append(args, req.ExtraArgs...)

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "corgi · " + name
	}
	wsID, paneID, err := e.fleet.CreateWorkspace(ctx, machine, cwd, label)
	if err != nil {
		return nil, err
	}
	sErr := e.fleet.StartAgent(ctx, paneID, kind, name, args)
	var he *herdr.Error
	blockedOnStart := sErr != nil && errors.As(sErr, &he) && he.Code == "agent_not_ready"
	if sErr != nil && !blockedOnStart {
		_ = e.fleet.CloseWorkspace(ctx, wsID)
		return nil, sErr
	}

	e.fleet.RememberName(paneID, name)
	e.fleet.RememberRun(paneID, effModel, effEffort)

	prompt := strings.TrimSpace(req.Prompt)
	taskDesc := prompt
	if taskDesc == "" {
		taskDesc = "项目会话 · " + cwd
	}
	status := "idle"
	if blockedOnStart {
		status = "blocked"
	} else if prompt != "" {
		status = "starting"
	}

	constraint, cErr := NormalizeConstraint(req.Constraint)
	if cErr != nil {
		return nil, cErr
	}
	thread := strings.TrimSpace(req.Thread)
	if thread == "" {
		thread = model.MainThread
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = "you"
	}
	savedTitle := strings.TrimSpace(req.Label)
	if savedTitle == "corgi · "+name {
		savedTitle = ""
	}
	wk := &model.Worker{
		Name:                name,
		Kind:                kind,
		Machine:             machine,
		Cwd:                 cwd,
		Task:                taskDesc,
		Thread:              thread,
		PaneID:              paneID,
		WorkspaceID:         wsID,
		CreatedBy:           createdBy,
		CreatedAt:           time.Now(),
		Title:               savedTitle,
		Keep:                req.Keep,
		Model:               effModel,
		Effort:              effEffort,
		ModelSource:         modelSource,
		Run:                 &[2]string{effModel, effEffort},
		Status:              status,
		Session:             req.SessionID,
		WorkItemID:          strings.TrimSpace(req.WorkItemID),
		Constraint:          constraint,
		Mode:                workerMode,
		ReadonlyEnforcement: roEnforcement,
	}
	if proj := ResolveProject(st.Projects, req.ProjectID, cwd); proj != nil {
		wk.ProjectID = proj.ID
		wk.ProjectName = proj.Name
	}
	if wtPrep != nil {
		wk.RepoRoot = wtPrep.RepoRoot
		wk.RepoCommonDir = wtPrep.CommonDir
		wk.WorktreePath = wtPrep.WorktreePath
		wk.WorktreeBranch = wtPrep.WorktreeBranch
		wk.WorktreeBase = wtPrep.BaseCommit
	} else if req.SessionID != "" {
		wk.RepoRoot = req.RepoRoot
		wk.RepoCommonDir = req.RepoCommonDir
		wk.WorktreePath = req.WorktreePath
		wk.WorktreeBranch = req.WorktreeBranch
		wk.WorktreeBase = req.WorktreeBase
	}
	if wk.WorkItemID != "" {
		if _, err := e.store.AttachWorkerAndUpsert(ctx, wk, wk.WorkItemID, "", oneLineClip(taskDesc, 90), ""); err != nil {
			if err := e.store.UpsertWorker(ctx, wk); err != nil {
				return nil, err
			}
		}
	} else if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return nil, err
	}
	for _, subThread := range req.Subscribers {
		if subThread != "" && subThread != model.MainThread {
			_ = e.Subscribe(ctx, wk.Name, subThread, false)
		}
	}
	e.touchWorkerActivity(wk.Name)

	verb := "新建受管任务"
	if req.SessionID != "" {
		verb = "续接受管会话"
	}
	wtTag := ""
	if wk.WorktreeBranch != "" {
		wtTag = " · 自动 Worktree " + wk.WorktreeBranch
	}
	e.addEvent(ctx, model.MainThread, fmt.Sprintf("%s %s（%s%s · %s%s）", verb, name, kind, machineTag(mc.Name, mc.Local), cwd, wtTag), name, "spawn")
	e.notifyChanged()

	if prompt != "" {
		brief := fmt.Sprintf("[corgi task · worker=%s · thread=%s]\n%s\n\nHow this works:\n- Report completion with `%s report \"outcome summary & file paths\"`.\n- If blocked on a decision only the human can make or repeated failures, report `BLOCKED: <need> · tried: <attempts> · recommendation: <choice>`.%s",
			name, model.MainThread, prompt, workerBin, worktreePromptNote(wtPrep))
		_, _ = e.Ingest(ctx, "router", model.MainThread, "you", brief, "", name, name)
	}
	return wk, nil
}

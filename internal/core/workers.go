package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func (e *Engine) Spawn(ctx context.Context, req SpawnRequest) (*model.Worker, error) {
	if strings.TrimSpace(req.Task) == "" {
		return nil, fmt.Errorf("task is required")
	}
	st := e.Settings()
	reqConstraint, err := NormalizeConstraint(req.Constraint)
	if err != nil {
		return nil, err
	}
	var (
		sessionID        string
		resumeWorkID     string
		resumeBranch     string
		resumeMachineSet bool
		resumeCreatedBy  string
		resumeTitle      string
		resumeKeep       bool
		resumeMode       string
		resumeWtPrep     *worktreePrep
		resumeSubs       []string
		workerConstraint = reqConstraint
	)
	if req.Resume != "" {
		old, err := e.store.GetWorker(ctx, req.Resume)
		if err != nil {
			return nil, fmt.Errorf("cannot resume worker %q: %w", req.Resume, err)
		}
		if !old.Ended {
			return nil, fmt.Errorf("worker %q is still running; use `send` instead", req.Resume)
		}
		if old.Session == "" {
			return nil, fmt.Errorf("worker %q has no saved session id to resume; spawn a fresh worker", req.Resume)
		}
		oldConstraint, _ := NormalizeConstraint(old.Constraint)
		if reqConstraint != "" && reqConstraint != oldConstraint {
			return nil, fmt.Errorf("cannot change constraint when resuming worker %q (existing: %q); spawn a new worker for a different scope", req.Resume, oldConstraint)
		}
		workerConstraint = oldConstraint
		req.Kind, req.Cwd, req.Machine, sessionID = old.Kind, old.Cwd, old.Machine, old.Session
		resumeMachineSet = true
		resumeWorkID = old.WorkItemID
		resumeBranch = old.WorktreeBranch
		resumeCreatedBy = old.CreatedBy
		resumeTitle = old.Title
		resumeKeep = old.Keep
		resumeMode = old.Mode
		fellBack := false
		if old.RepoRoot != "" {
			req.Cwd, fellBack = e.resolveResumeCwd(ctx, old.Machine, old.Cwd, old.RepoRoot)
		}
		if old.RepoRoot != "" || old.WorktreePath != "" || old.WorktreeBranch != "" {
			wtPath := old.WorktreePath
			wtBranch := old.WorktreeBranch
			if fellBack {
				wtPath = ""
				wtBranch = ""
			}
			resumeWtPrep = &worktreePrep{
				RepoRoot:       old.RepoRoot,
				CommonDir:      old.RepoCommonDir,
				WorktreePath:   wtPath,
				WorktreeBranch: wtBranch,
				BaseCommit:     old.WorktreeBase,
			}
		}
		if req.Subscribe == nil {
			if ended, eErr := e.store.ListEndedSessions(ctx, store.EndedSessionRetentionLimit); eErr == nil {
				for _, es := range ended {
					if es.Session == old.Session && es.Machine == old.Machine {
						resumeSubs = append([]string(nil), es.Subscribers...)
						break
					}
				}
			}
		}
		if req.Name == "" {
			req.Name = old.Name
		}
		if req.Thread == "" {
			req.Thread = old.Thread
		}
	}
	kind := agent.NormalizeKind(req.Kind)
	if kind == "" {
		kind = agent.NormalizeKind(st.WorkerKind)
	}
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		return nil, err
	}
	machine := st.WorkerMachine
	if resumeMachineSet {
		machine = e.fleet.ResolveMachine(req.Machine)
	} else if req.Machine != "" {
		machine = e.fleet.ResolveMachine(req.Machine)
	}
	mc, ok := e.fleet.Machine(machine)
	if !ok {
		return nil, fmt.Errorf("machine %q is not connected", orStr(req.Machine, machine))
	}
	machine = mc.ID

	// Pre-flight check that the target computer has the requested agent CLI installed.
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
		return nil, fmt.Errorf("%s has no %s installed (available: %s); pick another computer or agent kind",
			mc.Name, kind, strings.Join(have, ", "))
	}

	thread := strings.TrimSpace(req.Thread)
	if thread == "" {
		thread = model.MainThread
	}

	// Capture latest human message in this thread so fresh Router spawns carry original phrasing
	// and constraints verbatim without lossy telephone summarization.
	origUserReq := strings.TrimSpace(req.OriginalRequest)
	if origUserReq == "" && sessionID == "" {
		if recent, _, rErr := e.store.ListEntries(ctx, thread, 12); rErr == nil {
			for i := len(recent) - 1; i >= 0; i-- {
				if recent[i].Role == "user" && strings.TrimSpace(recent[i].Text) != "" && time.Since(recent[i].At) <= 20*time.Minute {
					origUserReq = strings.TrimSpace(recent[i].Text)
					break
				}
			}
		}
	}

	proj := ResolveProject(st.Projects, req.ProjectID, req.Task, origUserReq)
	cwd := strings.TrimSpace(req.Cwd)
	if (cwd == "" || cwd == "~") && proj != nil {
		if projPath := ProjectPathForMachine(proj, machine); projPath != "" {
			cwd = projPath
		}
	}
	if cwd == "" && (mc.ID == st.WorkerMachine || (mc.Local && st.WorkerMachine == "")) {
		cwd = st.WorkerCwd
	}
	if sessionID == "" && cwd != "" && !isManagedWorktreePath(cwd, e.corgiDir) {
		var vErr error
		if mc.Local {
			_, vErr = ValidateLocalWorkerCwd(e.home, e.corgiDir, e.dbPath(), cwd)
		} else {
			_, vErr = ValidateWorkerCwd(cwd, "")
		}
		if vErr != nil {
			return nil, fmt.Errorf("invalid worker cwd: %w", vErr)
		}
	}
	targetHome := mc.Home
	if mc.Local && targetHome == "" {
		targetHome = e.home
	}
	cwd = expandInHome(targetHome, cwd)
	if sessionID == "" && cwd != "" && !isManagedWorktreePath(cwd, e.corgiDir) &&
		(targetHome == "" || filepath.Clean(cwd) != filepath.Clean(targetHome)) {
		var vErr error
		if mc.Local {
			_, vErr = ValidateLocalWorkerCwd(targetHome, e.corgiDir, e.dbPath(), cwd)
		} else {
			_, vErr = ValidateWorkerCwd(cwd, "")
		}
		if vErr != nil {
			return nil, fmt.Errorf("invalid resolved worker cwd: %w", vErr)
		}
	}
	if mc.Local && cwd != "" {
		if fi, statErr := os.Stat(cwd); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("directory not found on %s: %s", mc.Name, cwd)
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		_, seq, _ := e.store.NextID(ctx, "w")
		name = fmt.Sprintf("%s-%d", kind, seq)
	}
	if !workerNameRe.MatchString(name) || name == model.RouterName {
		return nil, fmt.Errorf("invalid worker name %q", name)
	}
	if existing, gErr := e.store.GetWorker(ctx, name); gErr == nil && !existing.Ended {
		return nil, fmt.Errorf("worker %s is already running", name)
	}

	modelName, effort, modelSource := ResolveWorkerModel(st, machine, kind, req.Model, req.Effort)

	// Resolve work-item assignment before acquiring wtMu/tx so NextID never contends with an open tx.
	targetWorkID := strings.TrimSpace(req.WorkID)
	if !req.Ephemeral && targetWorkID == "" && resumeWorkID != "" {
		if existingWI, wiErr := e.store.GetWorkItem(ctx, resumeWorkID); wiErr == nil && existingWI.ClosedAt == nil {
			targetWorkID = existingWI.ID
		}
	}
	var newWorkID string
	if !req.Ephemeral && targetWorkID == "" {
		var idErr error
		newWorkID, _, idErr = e.store.NextID(ctx, "wi")
		if idErr != nil {
			return nil, idErr
		}
	}

	e.wtMu.Lock()
	var wtPrep *worktreePrep
	wtMode := strings.TrimSpace(req.Worktree)
	if wtMode == "" {
		switch strings.ToLower(strings.TrimSpace(req.Mode)) {
		case "readonly", "prototype":
			wtMode = "off"
		}
	}
	if sessionID == "" {
		effCwd, prep, wErr := e.prepareWorktree(ctx, machine, cwd, name, wtMode)
		if wErr != nil {
			e.wtMu.Unlock()
			return nil, wErr
		}
		cwd = effCwd
		wtPrep = prep
	} else if resumeWtPrep != nil {
		wtPrep = resumeWtPrep
	}

	createdBy := orStr(req.By, orStr(resumeCreatedBy, model.RouterName))
	workerMode := strings.ToLower(strings.TrimSpace(orStr(req.Mode, resumeMode)))
	readonly := workerMode == "readonly"
	roEnforcement := ""
	if readonly {
		roEnforcement = string(ad.ReadonlyEnforcement())
	}
	wk := &model.Worker{
		Name:                name,
		Kind:                kind,
		Machine:             machine,
		Cwd:                 cwd,
		Task:                req.Task,
		Thread:              thread,
		CreatedBy:           createdBy,
		CreatedAt:           time.Now(),
		Title:               resumeTitle,
		Keep:                resumeKeep,
		Model:               modelName,
		Effort:              effort,
		ModelSource:         modelSource,
		Run:                 &[2]string{modelName, effort},
		Status:              "starting",
		Session:             sessionID,
		Constraint:          workerConstraint,
		Mode:                workerMode,
		ReadonlyEnforcement: roEnforcement,
	}
	if proj != nil {
		wk.ProjectID = proj.ID
		wk.ProjectName = proj.Name
	}
	if wtPrep != nil {
		wk.RepoRoot = wtPrep.RepoRoot
		wk.RepoCommonDir = wtPrep.CommonDir
		wk.WorktreePath = wtPrep.WorktreePath
		wk.WorktreeBranch = wtPrep.WorktreeBranch
		wk.WorktreeBase = wtPrep.BaseCommit
	}
	var createdNewWI bool
	var createdDraftTitle bool
	if req.Ephemeral {
		err = e.store.UpsertWorker(ctx, wk)
	} else {
		wiTitle, isDraft := DeriveWorkItemTitle(req.WorkTitle, req.Task, origUserReq)
		createdNewWI = targetWorkID == "" && newWorkID != ""
		createdDraftTitle = createdNewWI && isDraft
		_, err = e.store.AttachWorkerAndUpsert(ctx, wk, targetWorkID, newWorkID, wiTitle, "")
	}
	e.wtMu.Unlock()
	if err != nil {
		return nil, err
	}
	if createdDraftTitle && wk.WorkItemID != "" {
		_ = e.store.SetWorkItemTitleState(ctx, wk.WorkItemID, store.WorkItemTitleDraft)
	}
	if !req.Ephemeral && resumeBranch != "" && wk.WorkItemID != "" {
		_ = e.store.RecordWorkItemBranch(ctx, wk.WorkItemID, resumeBranch)
	}
	sub := false
	if req.Subscribe != nil {
		sub = *req.Subscribe
	}
	if sub {
		_ = e.Subscribe(ctx, name, thread, false)
	}
	for _, subThread := range resumeSubs {
		if subThread != "" && subThread != model.MainThread {
			_ = e.Subscribe(ctx, name, subThread, false)
		}
	}
	e.touchWorkerActivity(name)
	verb := "派出"
	if sessionID != "" {
		verb = "续接"
	}
	wtTag := ""
	if wk.WorktreeBranch != "" {
		wtTag = " · Worktree " + wk.WorktreeBranch
	}
	modelTag := ""
	if modelName != "" {
		modelTag = " · " + modelName
		if effort != "" {
			modelTag += "/" + effort
		}
	}
	projTag := ""
	if wk.ProjectName != "" {
		projTag = " · " + wk.ProjectName
	}
	roTag := ""
	if readonly {
		if lbl := agent.ReadonlyEnforcementLabel(agent.ReadonlyEnforcement(roEnforcement)); lbl != "" {
			roTag = " · 只读(" + lbl + ")"
		}
	}
	e.addEvent(ctx, thread, fmt.Sprintf("%s %s（%s%s%s%s%s%s）", verb, name, kind, modelTag, machineTag(mc.Name, mc.Local), projTag, wtTag, roTag), name, "spawn")
	e.notifyChanged()

	workerBin := e.binPath
	if !mc.Local || workerBin == "" {
		workerBin = "corgi"
	}
	// Note: Codex `resume <id>` is a subcommand and MUST precede launch flags.
	args := append(ad.ResumeArgs(sessionID), ad.LaunchArgsWithOptions(agent.LaunchOptions{
		Model:      modelName,
		Effort:     effort,
		BypassPerm: !st.AskPermissions,
		Readonly:   readonly,
	})...)
	if kind == "claude" {
		args = append(args, "--allowedTools", fmt.Sprintf("Bash(%s report:*),Bash(corgi report:*)", workerBin))
	}

	started := make(chan error, 1)
	go func() {
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if !st.AskPermissions {
			e.preTrustWorkspace(lctx, machine, kind, cwd)
		}
		wsID, paneID, cErr := e.fleet.CreateWorkspace(lctx, machine, cwd, "corgi · "+name)
		if cErr != nil {
			e.failSpawnedWorker(lctx, wk, "", "", fmt.Errorf("create workspace: %w", cErr), started)
			return
		}
		sErr := e.fleet.StartAgent(lctx, paneID, kind, name, args)
		var he *herdr.Error
		blockedOnStart := sErr != nil && errors.As(sErr, &he) && he.Code == "agent_not_ready"
		if sErr != nil && !blockedOnStart {
			_ = e.fleet.CloseWorkspace(lctx, wsID)
			e.failSpawnedWorker(lctx, wk, paneID, wsID, fmt.Errorf("start agent %s: %w", name, sErr), started)
			return
		}

		e.fleet.RememberName(paneID, name)
		e.fleet.RememberRun(paneID, modelName, effort)

		wk.PaneID = paneID
		wk.WorkspaceID = wsID
		wk.Status = "idle"
		if blockedOnStart {
			wk.Status = "blocked"
		}
		_ = e.store.UpsertWorker(lctx, wk)
		if sessionID != "" {
			_ = e.store.ForgetEndedSession(lctx, kind, machine, sessionID)
		}
		e.notifyChanged()

		select {
		case started <- nil:
		default:
		}

		var extraCtx strings.Builder
		if origUserReq != "" && !strings.Contains(req.Task, origUserReq) && (req.By == "" || req.By == model.RouterName) {
			extraCtx.WriteString("\n\n[Human original message — preserve all constraints & nuances]\n")
			extraCtx.WriteString(oneLineClip(origUserReq, 600))
		}
		if proj != nil && proj.Note != "" {
			extraCtx.WriteString("\n\n[Project workspace note · " + proj.Name + "]\n")
			extraCtx.WriteString(proj.Note)
		}
		brief := fmt.Sprintf("[corgi task · worker=%s · thread=%s]\n%s%s\n\nHow this works:\n- Report completion with `%s report \"outcome summary & file paths\"`.\n- If blocked on a decision only the human can make or repeated failures, report `BLOCKED: <need> · tried: <attempts> · recommendation: <choice>`.%s",
			name, thread, strings.TrimSpace(req.Task), extraCtx.String(), workerBin, worktreePromptNote(wtPrep))
		_, _ = e.IngestWithMeta(lctx, "router", thread, model.RouterName, brief, "", name, name, TriggerMeta{WorkItemID: wk.WorkItemID})
	}()

	select {
	case sErr := <-started:
		if sErr != nil {
			return nil, sErr
		}
		return wk, nil
	case <-time.After(8 * time.Second):
		return wk, nil
	case <-ctx.Done():
		return wk, nil
	}
}

// CloseManagedWorkspace cleanly stops any active Worker owning workspaceID (preserving its session
// in History) and closes the underlying herdr workspace.
func (e *Engine) CloseManagedWorkspace(ctx context.Context, workspaceID string) error {
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if !w.Ended && w.WorkspaceID == workspaceID {
				return e.StopWorker(ctx, w.Name)
			}
		}
	}
	if e.fleet != nil {
		return e.fleet.CloseWorkspace(ctx, workspaceID)
	}
	return nil
}

func (e *Engine) failSpawnedWorker(ctx context.Context, wk *model.Worker, paneID, wsID string, err error, started chan<- error) {
	wk.PaneID = paneID
	wk.WorkspaceID = wsID
	wk.Ended = true
	wk.Status = "failed"
	_ = e.store.UpsertWorker(ctx, wk)
	e.addEvent(ctx, wk.Thread, wk.Name+" 启动失败："+err.Error(), wk.Name, "error")
	e.notifyChanged()
	select {
	case started <- err:
	default:
		_, _ = e.Ingest(ctx, "worker", wk.Thread, wk.Name,
			fmt.Sprintf("worker %s failed to start: %v", wk.Name, err), "", model.RouterName, wk.Name)
	}
}

func buildModeConstraint(mode string, allowRelease bool, extra string) string {
	var prefix string
	switch mode {
	case "readonly":
		prefix = "【只读范围】仅允许阅读、分析、调研和输出报告，严禁修改仓库源码、严禁 git commit/merge/push。"
	case "prototype":
		prefix = "【原型范围】仅允许产出或更新原型/设计文档/HTML 草图，严禁修改产品源码或执行 git merge/push。"
	case "implement":
		if allowRelease {
			prefix = "【已获用户发布授权】按本次明确目标执行合入、构建、部署与核验，严格遵守任务说明中的范围与检查步骤。"
		} else {
			prefix = "【源码实现范围】仅在当前隔离分支完成本次改动与所请求的测试；严禁擅自 git merge 到主分支、git push 或部署上线。"
		}
	}
	if extra == "" {
		return prefix
	}
	return prefix + "\n" + extra
}

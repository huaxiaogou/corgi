package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
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
	targetHome := mc.Home
	if mc.Local && targetHome == "" {
		targetHome = e.home
	}
	cwd = expandInHome(targetHome, cwd)
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
	wk := &model.Worker{
		Name:        name,
		Kind:        kind,
		Machine:     machine,
		Cwd:         cwd,
		Task:        req.Task,
		Thread:      thread,
		CreatedBy:   createdBy,
		CreatedAt:   time.Now(),
		Title:       resumeTitle,
		Keep:        resumeKeep,
		Model:       modelName,
		Effort:      effort,
		ModelSource: modelSource,
		Run:         &[2]string{modelName, effort},
		Status:      "starting",
		Session:     sessionID,
		Constraint:  workerConstraint,
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
	if strings.EqualFold(strings.TrimSpace(req.Mode), "implement") {
		e.markWorkerImplementDispatched(wk.Name)
	} else {
		e.invalidateWorkerAutoVerify(wk.Name, true)
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
	e.addEvent(ctx, thread, fmt.Sprintf("%s %s（%s%s%s%s%s）", verb, name, kind, modelTag, machineTag(mc.Name, mc.Local), projTag, wtTag), name, "spawn")
	e.notifyChanged()

	workerBin := e.binPath
	if !mc.Local || workerBin == "" {
		workerBin = "corgi"
	}
	// Note: Codex `resume <id>` is a subcommand and MUST precede launch flags.
	args := append(ad.ResumeArgs(sessionID), ad.LaunchArgs(modelName, effort, !st.AskPermissions)...)
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
		if bTrig, bErr := e.IngestWithMeta(lctx, "router", thread, model.RouterName, brief, "", name, name, TriggerMeta{WorkItemID: wk.WorkItemID}); bErr == nil && bTrig != nil && strings.EqualFold(strings.TrimSpace(req.Mode), "implement") {
			e.setWorkerAutoVerifyMinSeq(wk.Name, bTrig.Seq)
		}
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

// LaunchManagedRequest describes a user-initiated agent launch from the `+` modal or History/Card resume.
type LaunchManagedRequest struct {
	Kind           string
	Machine        string
	Cwd            string
	Name           string
	Label          string
	Model          string
	Effort         string
	Prompt         string
	SessionID      string
	WorkItemID     string
	ProjectID      string
	Worktree       string // auto (default) | on | off
	Constraint     string
	ExtraArgs      []string
	CreatedBy      string
	Thread         string
	Keep           bool
	RepoRoot       string
	RepoCommonDir  string
	WorktreePath   string
	WorktreeBranch string
	WorktreeBase   string
	Subscribers    []string
}

// ResumeWorkerRequest identifies a specific historical worker session to reopen for direct follow-up.
type ResumeWorkerRequest struct {
	EndedSessionID string `json:"ended_session_id,omitempty"`
	Name           string `json:"name"`
	Machine        string `json:"machine,omitempty"`
	MachineSet     bool   `json:"-"`
	Session        string `json:"session,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	WorkItemID     string `json:"work_item_id,omitempty"`
	Constraint     string `json:"constraint,omitempty"`
}

// ResumeWorkerSession returns the existing live pane if the exact worker session is already running,
// or reopens the archived session on its original machine while preserving its WorkItem linkage,
// ownership, worktree metadata, and thread subscriptions.
func (e *Engine) ResumeWorkerSession(ctx context.Context, req ResumeWorkerRequest) (*model.Worker, bool, error) {
	name := strings.TrimSpace(req.Name)
	sess := strings.TrimSpace(req.Session)
	machineSet := req.MachineSet || strings.TrimSpace(req.Machine) != ""
	machine := strings.TrimSpace(req.Machine)
	if machine != "" && e.fleet != nil {
		machine = e.fleet.ResolveMachine(machine)
	}

	kind := agent.NormalizeKind(req.Kind)
	cwd := strings.TrimSpace(req.Cwd)
	workID := strings.TrimSpace(req.WorkItemID)
	reqConstraint, cErr := NormalizeConstraint(req.Constraint)
	if cErr != nil {
		return nil, false, cErr
	}
	var (
		modelName, effort, title, savedConstraint string
		createdBy, thread                         string
		keep                                      bool
		repoRoot, repoCommonDir                   string
		worktreePath, worktreeBranch, wtBase      string
		subscribers                               []string
		exactEnded                                *model.EndedSession
	)

	// 0. If an exact EndedSession archive ID was requested (e.g. Web History click), load it first
	// so machine/session/metadata match that exact archive row without ambiguity.
	if eid := strings.TrimSpace(req.EndedSessionID); eid != "" {
		es, err := e.store.GetEndedSession(ctx, eid)
		if err != nil {
			return nil, false, fmt.Errorf("历史会话不存在")
		}
		exactEnded = es
		if name == "" {
			name = es.Name
		}
		if sess == "" {
			sess = es.Session
		}
		if kind == "" {
			kind = agent.NormalizeKind(es.Kind)
		}
		if !machineSet {
			machine = es.Machine
			machineSet = true
		}
		if cwd == "" {
			cwd = es.Cwd
		}
		if workID == "" {
			workID = es.WorkItemID
		}
		modelName, effort, title, savedConstraint = es.Model, es.Effort, es.Title, es.Constraint
		createdBy, thread, keep = es.CreatedBy, es.Thread, es.Keep
		repoRoot, repoCommonDir = es.RepoRoot, es.RepoCommonDir
		worktreePath, worktreeBranch, wtBase = es.WorktreePath, es.WorktreeBranch, es.WorktreeBase
		subscribers = append([]string(nil), es.Subscribers...)
	}

	// Wait briefly if GC is currently finalizing hibernation of this worker so we resume cleanly.
	if name != "" {
		e.waitWorkerGCIdle(ctx, name, 3*time.Second)
	}

	// 1. Check live workers first by exact session match, then by name+machine.
	if all, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range all {
			if w.Ended || w.Hibernating || w.PaneID == "" {
				continue
			}
			if sess != "" && w.Session == sess && (!machineSet || w.Machine == machine) {
				return w, false, nil
			}
		}
		if name != "" {
			for _, w := range all {
				if w.Ended || w.Hibernating || w.PaneID == "" {
					continue
				}
				if w.Name == name && (!machineSet || w.Machine == machine) && (sess == "" || w.Session == "" || w.Session == sess) {
					return w, false, nil
				}
			}
		}
	}

	// 2. Recover saved metadata from Worker table and EndedSessions.
	if name != "" {
		if old, err := e.store.GetWorker(ctx, name); err == nil {
			machineOK := !machineSet || old.Machine == machine
			if machineOK && (sess == "" || old.Session == sess || old.Session == "") {
				if sess == "" {
					sess = old.Session
				}
				if kind == "" {
					kind = agent.NormalizeKind(old.Kind)
				}
				if !machineSet {
					machine = old.Machine
					machineSet = true
				}
				if cwd == "" {
					cwd = old.Cwd
				}
				if workID == "" {
					workID = old.WorkItemID
				}
				if modelName == "" {
					modelName = old.Model
				}
				if effort == "" {
					effort = old.Effort
				}
				if title == "" {
					title = old.Title
				}
				if savedConstraint == "" {
					savedConstraint = old.Constraint
				}
				if createdBy == "" {
					createdBy = old.CreatedBy
				}
				if thread == "" {
					thread = old.Thread
				}
				if !keep {
					keep = old.Keep
				}
				if repoRoot == "" {
					repoRoot = old.RepoRoot
				}
				if repoCommonDir == "" {
					repoCommonDir = old.RepoCommonDir
				}
				if worktreePath == "" {
					worktreePath = old.WorktreePath
				}
				if worktreeBranch == "" {
					worktreeBranch = old.WorktreeBranch
				}
				if wtBase == "" {
					wtBase = old.WorktreeBase
				}
			}
		}
	}
	if exactEnded == nil {
		if ended, err := e.store.ListEndedSessions(ctx, store.EndedSessionRetentionLimit); err == nil {
			for _, es := range ended {
				matchSess := sess != "" && es.Session == sess && (!machineSet || es.Machine == machine)
				matchName := sess == "" && name != "" && es.Name == name && (!machineSet || es.Machine == machine)
				if !matchSess && !matchName {
					continue
				}
				if sess == "" {
					sess = es.Session
				}
				if kind == "" {
					kind = agent.NormalizeKind(es.Kind)
				}
				if !machineSet {
					machine = es.Machine
					machineSet = true
				}
				if cwd == "" {
					cwd = es.Cwd
				}
				if workID == "" {
					workID = es.WorkItemID
				}
				if modelName == "" {
					modelName = es.Model
				}
				if effort == "" {
					effort = es.Effort
				}
				if title == "" {
					title = es.Title
				}
				if name == "" {
					name = es.Name
				}
				if savedConstraint == "" {
					savedConstraint = es.Constraint
				}
				if createdBy == "" {
					createdBy = es.CreatedBy
				}
				if thread == "" {
					thread = es.Thread
				}
				if !keep {
					keep = es.Keep
				}
				if repoRoot == "" {
					repoRoot = es.RepoRoot
				}
				if repoCommonDir == "" {
					repoCommonDir = es.RepoCommonDir
				}
				if worktreePath == "" {
					worktreePath = es.WorktreePath
				}
				if worktreeBranch == "" {
					worktreeBranch = es.WorktreeBranch
				}
				if wtBase == "" {
					wtBase = es.WorktreeBase
				}
				if len(subscribers) == 0 && len(es.Subscribers) > 0 {
					subscribers = append([]string(nil), es.Subscribers...)
				}
				break
			}
		}
	}
	savedConstraint, _ = NormalizeConstraint(savedConstraint)
	if reqConstraint != "" && reqConstraint != savedConstraint {
		return nil, false, fmt.Errorf("cannot change constraint when resuming worker %q (existing: %q); start a new session for a different scope", orStr(name, sess), savedConstraint)
	}

	if sess == "" {
		return nil, false, fmt.Errorf("未找到可恢复的会话记录")
	}
	if kind == "" {
		return nil, false, fmt.Errorf("无法确定该会话对应的 Agent 类型")
	}
	if repoRoot != "" {
		resolvedCwd, fellBack := e.resolveResumeCwd(ctx, machine, cwd, repoRoot)
		cwd = resolvedCwd
		if fellBack {
			worktreePath = ""
			worktreeBranch = ""
		}
	}
	if workID != "" {
		if wi, err := e.store.GetWorkItem(ctx, workID); err != nil || wi.ClosedAt != nil {
			workID = ""
		}
	}

	wk, err := e.LaunchManaged(ctx, LaunchManagedRequest{
		Kind:           kind,
		Machine:        machine,
		Cwd:            cwd,
		Name:           name,
		Label:          title,
		Model:          modelName,
		Effort:         effort,
		SessionID:      sess,
		WorkItemID:     workID,
		Constraint:     savedConstraint,
		CreatedBy:      createdBy,
		Thread:         thread,
		Keep:           keep,
		RepoRoot:       repoRoot,
		RepoCommonDir:  repoCommonDir,
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		WorktreeBase:   wtBase,
		Subscribers:    subscribers,
	})
	if err != nil {
		return nil, false, err
	}
	_ = e.store.ForgetEndedSession(ctx, kind, machine, sess)
	return wk, true, nil
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
	// Codex `resume <id>` is a subcommand and must precede launch flags.
	args := append(ad.ResumeArgs(req.SessionID), ad.LaunchArgs(effModel, effEffort, !st.AskPermissions)...)
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
		Name:        name,
		Kind:        kind,
		Machine:     machine,
		Cwd:         cwd,
		Task:        taskDesc,
		Thread:      thread,
		PaneID:      paneID,
		WorkspaceID: wsID,
		CreatedBy:   createdBy,
		CreatedAt:   time.Now(),
		Title:       savedTitle,
		Keep:        req.Keep,
		Model:       effModel,
		Effort:      effEffort,
		ModelSource: modelSource,
		Run:         &[2]string{effModel, effEffort},
		Status:      status,
		Session:     req.SessionID,
		WorkItemID:  strings.TrimSpace(req.WorkItemID),
		Constraint:  constraint,
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
	e.invalidateWorkerAutoVerify(wk.Name, true)

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

// SendToWorker queues a steering message for an active worker, or adopts a live
// manually-started agent matching workerName so the Router can delegate directly to it.
func (e *Engine) SendToWorker(ctx context.Context, workerName, thread, text string) (*model.Trigger, error) {
	trig, _, err := e.SendToWorkerWithWork(ctx, workerName, thread, text, "")
	return trig, err
}

// SendToWorkerWithWork steers an active or newly-adopted worker while ensuring durable work-item linkage:
// explicit workID attaches to that item; otherwise inherits the worker's open item, requires disambiguation
// when multiple open items reference the worker, or creates a new item if unassigned.
func (e *Engine) SendToWorkerWithWork(ctx context.Context, workerName, thread, text, workID string) (*model.Trigger, string, error) {
	return e.SendToWorkerWithWorkTitle(ctx, workerName, thread, text, workID, "")
}

// SendToWorkerWithWorkTitle steers an active or newly-adopted worker while ensuring durable
// work-item linkage, using explicit workTitle when creating a brand-new work item.
func (e *Engine) SendToWorkerWithWorkTitle(ctx context.Context, workerName, thread, text, workID, workTitle string) (*model.Trigger, string, error) {
	return e.sendToWorkerInternal(ctx, workerName, thread, text, workID, workTitle, false, false)
}

func (e *Engine) sendToWorkerInternal(ctx context.Context, workerName, thread, text, workID, workTitle string, markImplement, keepAutoVerifyGen bool) (*model.Trigger, string, error) {
	e.waitWorkerGCIdle(ctx, workerName, 3*time.Second)
	if !e.beginWorkerOp(workerName) {
		return nil, "", fmt.Errorf("worker %q is hibernating; retry or resume session", workerName)
	}
	defer e.endWorkerOp(workerName)

	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended || wk.Hibernating {
		adopted, adoptErr := e.adoptLiveAgent(ctx, workerName, thread, text)
		if adoptErr != nil {
			return nil, "", fmt.Errorf("no running worker or live agent named %q", workerName)
		}
		wk = adopted
	}
	if isWorkerFaulted(wk) {
		if e.isAPIRouterMode() {
			return nil, "", fmt.Errorf("worker %q is paused due to fatal error (%s); call manage_work(action=\"stop_worker\", target=%q) and dispatch to a healthy worker/model", wk.Name, oneLineClip(wk.FaultError, 120), wk.Name)
		}
		return nil, "", fmt.Errorf("worker %q is paused due to fatal error (%s); run `%s stop %s` and dispatch to a healthy worker/model", wk.Name, oneLineClip(wk.FaultError, 120), orStr(e.binPath, "corgi"), wk.Name)
	}
	if thread == "" {
		thread = wk.Thread
	}
	prevBranch := wk.WorktreeBranch
	if wk.IdleSince != nil || wk.IdlePatrolCount > 0 || wk.SnoozeUntil != nil {
		resetWorkerIdle(wk)
	}

	targetWorkID := strings.TrimSpace(workID)
	if targetWorkID == "" && wk.WorkItemID != "" {
		if existingWI, wiErr := e.store.GetWorkItem(ctx, wk.WorkItemID); wiErr == nil && existingWI.ClosedAt == nil {
			targetWorkID = existingWI.ID
		}
	}
	if targetWorkID == "" {
		matches, mErr := e.store.FindOpenWorkItemsByWorker(ctx, wk.Name)
		if mErr != nil {
			return nil, "", mErr
		}
		if len(matches) == 1 {
			targetWorkID = matches[0].ID
		} else if len(matches) > 1 {
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return nil, "", fmt.Errorf("worker %q belongs to multiple open work items (%s); specify --work <id>", wk.Name, strings.Join(ids, ", "))
		}
	}
	var newWorkID string
	if targetWorkID == "" {
		var idErr error
		newWorkID, _, idErr = e.store.NextID(ctx, "wi")
		if idErr != nil {
			return nil, "", idErr
		}
	}
	wiTitle, isDraft := DeriveWorkItemTitle(workTitle, text, "")
	wi, err := e.store.AttachWorkerAndUpsert(ctx, wk, targetWorkID, newWorkID, wiTitle, "")
	if err != nil {
		return nil, "", err
	}
	if targetWorkID == "" && isDraft && wi != nil {
		_ = e.store.SetWorkItemTitleState(ctx, wi.ID, store.WorkItemTitleDraft)
	}
	if prevBranch != "" && wi != nil {
		_ = e.store.RecordWorkItemBranch(ctx, wi.ID, prevBranch)
	}
	if markImplement {
		e.markWorkerImplementDispatched(wk.Name)
	} else if !keepAutoVerifyGen {
		e.invalidateWorkerAutoVerify(wk.Name, true)
	}
	trig, err := e.IngestWithMeta(ctx, "router", thread, model.RouterName, text, "", wk.Name, wk.Name, TriggerMeta{WorkItemID: wk.WorkItemID})
	if err != nil {
		return nil, "", err
	}
	if trig != nil {
		if markImplement {
			e.setWorkerAutoVerifyMinSeq(wk.Name, trig.Seq)
		}
		e.recordWorkerIssued(wk, trig.ID, trig.Seq)
	}
	return trig, wk.WorkItemID, nil
}

func (e *Engine) adoptLiveAgent(ctx context.Context, target, thread, task string) (*model.Worker, error) {
	if e.fleet == nil {
		return nil, fmt.Errorf("fleet unavailable")
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		name := ""
		if a.Name != nil {
			name = *a.Name
		}
		if name == "" || name == model.RouterName || name == model.DreamName {
			continue
		}
		if name != target && a.PaneID != target {
			continue
		}
		_, machine := fleet.SplitID(a.PaneID)
		kind := ""
		if a.Agent != nil {
			kind = agent.NormalizeKind(*a.Agent)
		}
		cwd := ""
		if a.Cwd != nil {
			cwd = *a.Cwd
		}
		if thread == "" {
			thread = model.MainThread
		}
		wk := &model.Worker{
			Name:        name,
			Kind:        kind,
			Machine:     machine,
			Cwd:         cwd,
			Task:        oneLineClip(task, 200),
			Thread:      thread,
			PaneID:      a.PaneID,
			WorkspaceID: a.WorkspaceID,
			CreatedBy:   "adopted",
			CreatedAt:   time.Now(),
			Status:      orStr(a.AgentStatus, "idle"),
		}
		if err := e.store.UpsertWorker(ctx, wk); err != nil {
			return nil, err
		}
		e.fleet.RememberName(a.PaneID, name)
		e.addEvent(ctx, thread, fmt.Sprintf("接管已有会话 %s（%s%s）", name, kind, machineTag(machine, machine == "")), name, "spawn")
		e.notifyChanged()
		return wk, nil
	}
	return nil, fmt.Errorf("live agent %q not found", target)
}

// Report receives a worker's progress or completion report and routes it to the Router.
func (e *Engine) Report(ctx context.Context, workerName, paneID, text string) (*model.Trigger, error) {
	var wk *model.Worker
	if workerName != "" {
		wk, _ = e.store.GetWorker(ctx, workerName)
	}
	if wk == nil && paneID != "" {
		if all, err := e.store.ListWorkers(ctx); err == nil {
			for _, w := range all {
				local, _ := fleet.SplitID(w.PaneID)
				if !w.Ended && (w.PaneID == paneID || local == paneID) {
					wk = w
					break
				}
			}
		}
	}
	thread := model.MainThread
	from := orStr(workerName, "worker")
	artID := ""
	artNote := ""
	turnNote := ""
	if wk != nil {
		from = wk.Name
		thread = wk.Thread
		wk.LastReport = text
		wk.ReportAt = time.Now()
		wk.HadTurn = true
		wk.FaultError = ""
		wk.FaultPaneID = ""
		wk.FaultAt = nil
		wk.StartupFailed = false
		_ = e.store.UpsertWorker(ctx, wk)
		e.disarmWorkerStartupCheck(wk.Name)
		if id := e.captureWorkerReportSnapshot(ctx, wk, text, 500); id != "" {
			artID = id
			artNote = e.formatReportArtifactNote(id, wk.Name)
		}
		snap := e.lastWorkerTurnSnapshot(ctx, wk)
		turnNote = e.formatWorkerTurnStatusNote(ctx, wk, snap)
		e.recordWorkerExplicitReport(wk, snap, artID, isProgressReportText(text), wk.ReportAt)
	}
	workID := ""
	wiNote := ""
	if wk != nil {
		workID = wk.WorkItemID
		wiNote = e.formatWorkerWorkItemContext(ctx, wk)
	}
	subject := e.formatWorkItemSubject(ctx, workID, from, from)
	e.addEvent(ctx, thread, fmt.Sprintf("%s 汇报：%s", subject, oneLineClip(text, 120)), from, "done")
	trig, err := e.IngestWithMeta(ctx, "worker", thread, from, fmt.Sprintf("report from %s:\n%s%s%s%s", from, text, artNote, turnNote, wiNote), "", model.RouterName, from, TriggerMeta{
		WorkItemID: workID,
		ArtifactID: artID,
	})
	return trig, err
}

func (e *Engine) formatReportArtifactNote(artID, workerName string) string {
	if e.isAPIRouterMode() {
		return fmt.Sprintf("\n[full_report_artifact: %s — full untruncated report snapshotted on Hub; pass re_id=<this trigger_id> and worker=%q on reply_user(purpose=\"result\") (when not using files) to attach this exact snapshot]", artID, workerName)
	}
	return fmt.Sprintf("\n[full_report_artifact: %s — full untruncated report snapshotted on Hub; pass --re <this event #tID> --worker %s on `corgi reply` (when not using --file) to attach this exact snapshot]", artID, workerName)
}

// Subscribe subscribes or unsubscribes a worker to a thread's incoming events.
func (e *Engine) Subscribe(ctx context.Context, workerName, thread string, off bool) error {
	if thread == "" || thread == model.MainThread {
		return fmt.Errorf("chat:main always stays with Corgi router")
	}
	th, err := e.store.GetThread(ctx, thread)
	if err != nil {
		th = &model.Thread{ID: thread, Subscribers: []string{}, LastAt: time.Now()}
	}
	if off {
		th.Subscribers = removeStr(th.Subscribers, workerName)
	} else if !slices.Contains(th.Subscribers, workerName) {
		th.Subscribers = append(th.Subscribers, workerName)
	}
	th.LastAt = time.Now()
	return e.store.UpsertThread(ctx, th)
}

// StopWorker closes a worker's herdr workspace while preserving its session ID for --resume.
func (e *Engine) StopWorker(ctx context.Context, workerName string) error {
	e.waitWorkerGCIdle(ctx, workerName, 3*time.Second)
	if !e.beginWorkerOp(workerName) {
		return fmt.Errorf("worker %q is currently hibernating; retry shortly", workerName)
	}
	defer e.endWorkerOp(workerName)

	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil {
		return fmt.Errorf("worker %q not found", workerName)
	}
	e.invalidateWorkerAutoVerify(wk.Name, true)
	if wk.Session == "" && wk.PaneID != "" {
		if sess := e.lookupRemoteSession(ctx, wk.PaneID); sess != "" {
			wk.Session = sess
		}
	}
	if agent.NormalizeKind(wk.Kind) == "codex" && wk.Session != "" {
		if err := e.stopCodexTurn(ctx, wk.Machine, wk.Session); err != nil {
			e.logger.Warn("interrupt codex daemon turn failed", "worker", wk.Name, "machine", wk.Machine, "err", err)
		}
	}
	if wk.PaneID != "" && e.fleet != nil {
		// Press Esc first so any running turn aborts cleanly before closing the workspace.
		_ = e.fleet.SendKeys(ctx, wk.PaneID, "esc")
		time.Sleep(600 * time.Millisecond)
	}
	if wk.WorkspaceID != "" && e.fleet != nil {
		_ = e.fleet.CloseWorkspace(ctx, wk.WorkspaceID)
	}
	if wk.PaneID != "" {
		e.recordRecentlyClosedPane(wk.PaneID, time.Now())
	}
	subs := e.collectWorkerSubscribers(ctx, wk.Name)
	if wk.WorkItemID != "" && wk.WorktreeBranch != "" {
		_ = e.store.RecordWorkItemBranch(ctx, wk.WorkItemID, wk.WorktreeBranch)
	}
	wtNote := ""
	if wk.WorktreePath != "" && wk.RepoRoot != "" {
		if clRes, clErr := e.cleanupWorktreeOnMachine(ctx, wk.Machine, wk.RepoRoot, wk.WorktreePath, wk.WorktreeBranch, wk.WorktreeBase); clErr != nil {
			wtNote = "；Worktree 清理跳过：" + clErr.Error()
		} else {
			if clRes.RemovedDir {
				wk.WorktreePath = ""
				wk.Cwd = wk.RepoRoot
			}
			if clRes.DeletedBranch {
				wk.WorktreeBranch = ""
			}
			if clRes.Note != "" {
				wtNote = "；" + clRes.Note
			}
		}
	}
	if wk.Session != "" {
		histCwd := wk.Cwd
		if wk.WorktreePath != "" && wk.RepoRoot != "" {
			histCwd, _ = e.resolveResumeCwd(ctx, wk.Machine, wk.Cwd, wk.RepoRoot)
		}
		if es := e.buildWorkerEndedSession(ctx, wk, histCwd, subs, time.Now()); es != nil {
			_ = e.store.SaveEndedSession(ctx, es, store.EndedSessionRetentionLimit)
		}
	}
	wk.Ended = true
	wk.Hibernating = false
	wk.Status = "done"
	wk.FaultError = ""
	wk.FaultPaneID = ""
	wk.FaultAt = nil
	wk.StartupFailed = false
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return err
	}
	if threads, tErr := e.store.ListThreads(ctx, 200); tErr == nil {
		for _, th := range threads {
			if slices.Contains(th.Subscribers, wk.Name) {
				th.Subscribers = removeStr(th.Subscribers, wk.Name)
				_ = e.store.UpsertThread(ctx, th)
			}
		}
	}
	_ = e.store.ExpireWorkerActions(ctx, wk.Name)
	e.ClearWeComFocusForWorker(ctx, wk.Name)
	if wk.PaneID != "" {
		_ = e.store.DeleteQueuedPromptsByScope(ctx, wk.PaneID)
		e.setQueuePaused(wk.PaneID, false)
		e.clearConstraintDelivered(wk.PaneID)
	}
	e.clearWorkerTurnState(wk.Name)
	e.addEvent(ctx, wk.Thread, "已停止 "+wk.Name+"（会话已保留，可随时续接"+wtNote+"）", wk.Name, "info")
	e.notifyChanged()
	return nil
}

// GetAction loads one Action by ID from the durable store.
func (e *Engine) GetAction(ctx context.Context, actionID string) (*model.Action, error) {
	return e.store.GetAction(ctx, actionID)
}

// ResolveAction atomically resolves a pending Action from Web, WeCom, Router, or CLI.
func (e *Engine) ResolveAction(ctx context.Context, actionID, resolvedBy, choiceInput string) (*model.Action, error) {
	return e.resolveAction(ctx, actionID, resolvedBy, choiceInput, false)
}

// ResolveActionCustom atomically resolves a pending decision card with a human-written custom answer,
// bypassing preset choice fuzzy matching so custom text is never overwritten by a preset option.
func (e *Engine) ResolveActionCustom(ctx context.Context, actionID, resolvedBy, customText string) (*model.Action, error) {
	return e.resolveAction(ctx, actionID, resolvedBy, customText, true)
}

func (e *Engine) resolveAction(ctx context.Context, actionID, resolvedBy, choiceInput string, custom bool) (*model.Action, error) {
	trimmed := strings.TrimSpace(choiceInput)
	if trimmed == "" {
		return nil, fmt.Errorf("empty action choice")
	}
	act, err := e.store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if act.Status != model.ActionPending {
		return nil, store.ErrActionAlreadyResolved
	}
	var (
		chosenKey    string
		chosenLabel  string
		chosenCursor int
	)
	if custom {
		if act.Kind == model.ActionKindWorkerDialog {
			return nil, fmt.Errorf("terminal dialog does not accept custom free-text input")
		}
		chosenKey = "custom"
		chosenLabel = trimmed
	} else if act.Kind == model.ActionKindRouterCheck && len(act.Choices) == 0 &&
		(strings.EqualFold(trimmed, "ack") || strings.EqualFold(trimmed, "seen") || trimmed == "已看") {
		chosenKey = "ack"
		chosenLabel = "已看"
	} else if k, lbl, cur, ok := matchStrictActionChoice(act, trimmed); ok {
		chosenKey, chosenLabel, chosenCursor = k, lbl, cur
	} else if act.Kind == model.ActionKindWorkerDialog {
		// Allow label-word match for AnswerWorker CLI convenience, but never send unmatched arbitrary text to a live terminal.
		var matched bool
		chosenKey, chosenLabel, chosenCursor, matched = matchLabelSubstringChoice(act, trimmed)
		if !matched {
			return nil, fmt.Errorf("invalid choice %q for terminal dialog %s", trimmed, act.ID)
		}
	} else if len(act.Choices) > 0 {
		return nil, fmt.Errorf("invalid choice %q for action %s", trimmed, act.ID)
	} else {
		chosenKey, chosenLabel, chosenCursor = matchActionChoice(act, trimmed)
	}
	resolved, err := e.store.ResolveAction(ctx, actionID, resolvedBy, chosenKey, chosenLabel)
	if err != nil {
		return nil, err
	}

	switch resolved.Kind {
	case model.ActionKindWorkerDialog:
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := e.sendDialogAnswer(dctx, resolved.PaneID, chosenKey, chosenCursor)
		cancel()
		if err != nil {
			e.logger.Warn("failed sending dialog answer to worker", "worker", resolved.TargetWorker, "err", err)
		}
		e.mu.Lock()
		e.lastAnswer[resolved.PaneID] = time.Now()
		e.mu.Unlock()
		e.addEventWithAction(ctx, resolved.Thread, fmt.Sprintf("已为 %s 选择：%s（via %s）", resolved.TargetWorker, chosenLabel, resolvedBy), resolved.TargetWorker, "info", resolved.ID)
	case model.ActionKindRouterCheck:
		// Silent acknowledgement ("✓ 已看") clears the reminder without waking Router.
		if custom || chosenKey != "ack" {
			promptCtx := ""
			if custom {
				promptCtx = buildCustomDecisionPromptContext(resolved)
			}
			_, _, _ = e.Say(ctx, SayRequest{
				Thread:        resolved.Thread,
				Channel:       resolvedBy,
				From:          resolvedBy,
				Text:          chosenLabel,
				ReEntryID:     resolved.EntryID,
				ActionID:      resolved.ID,
				PromptContext: promptCtx,
			})
		}
	case model.ActionKindRouterChoice, model.ActionKindPatrolDigest:
		promptCtx := ""
		if custom {
			promptCtx = buildCustomDecisionPromptContext(resolved)
		}
		_, _, _ = e.Say(ctx, SayRequest{
			Thread:        resolved.Thread,
			Channel:       resolvedBy,
			From:          resolvedBy,
			Text:          chosenLabel,
			ReEntryID:     resolved.EntryID,
			ActionID:      resolved.ID,
			PromptContext: promptCtx,
		})
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, resolved)
	sink.OnStateChanged()
	return resolved, nil
}

func buildCustomDecisionPromptContext(a *model.Action) string {
	q := oneLineClip(orStr(a.Summary, a.Title), 240)
	var b strings.Builder
	fmt.Fprintf(&b, "[In reply to decision card: %q]", q)
	if len(a.Choices) > 0 {
		opts := make([]string, 0, len(a.Choices))
		for i, c := range a.Choices {
			opts = append(opts, fmt.Sprintf("%d. %s", i+1, c.Label))
		}
		fmt.Fprintf(&b, "\n[Preset options were: %s — human provided a custom answer instead]", strings.Join(opts, " | "))
	}
	return b.String()
}

// DismissAction atomically dismisses a pending reminder/choice without sending a reply to Router.
func (e *Engine) DismissAction(ctx context.Context, actionID string) (*model.Action, error) {
	return e.DismissActionBy(ctx, actionID, "")
}

// DismissActionBy atomically dismisses a pending action and records the originating channel.
func (e *Engine) DismissActionBy(ctx context.Context, actionID, channel string) (*model.Action, error) {
	act, err := e.store.DismissActionBy(ctx, actionID, channel)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, act)
	sink.OnStateChanged()
	return act, nil
}

// AnswerWorker answers a blocked worker's live terminal dialog by choice key, label words, or "esc".
func (e *Engine) AnswerWorker(ctx context.Context, workerName, choiceInput, by string) (string, error) {
	wk, err := e.store.GetWorker(ctx, workerName)
	if err != nil || wk.Ended || wk.PaneID == "" {
		return "", fmt.Errorf("no running worker %q", workerName)
	}
	if pending, err := e.store.ListPendingActions(ctx); err == nil {
		for _, a := range pending {
			if a.Kind == model.ActionKindWorkerDialog && a.TargetWorker == workerName {
				res, err := e.ResolveAction(ctx, a.ID, by, choiceInput)
				if err != nil {
					return "", err
				}
				return res.ChosenLabel, nil
			}
		}
	}
	rr, err := e.fleet.Read(ctx, wk.PaneID, "detection", "text", 0)
	if err != nil {
		return "", err
	}
	ap := agent.ParseApproval(rr.Text)
	if len(ap.Choices) == 0 && !strings.EqualFold(strings.TrimSpace(choiceInput), "esc") {
		return "", fmt.Errorf("worker %s is not showing a dialog", workerName)
	}
	tmp := &model.Action{Choices: approvalToChoices(ap)}
	key, label, cursor := matchActionChoice(tmp, choiceInput)
	if err := e.sendDialogAnswer(ctx, wk.PaneID, key, cursor); err != nil {
		return "", err
	}
	e.mu.Lock()
	e.lastAnswer[wk.PaneID] = time.Now()
	e.mu.Unlock()
	e.addEvent(ctx, wk.Thread, fmt.Sprintf("替 %s 选了：%s", workerName, label), workerName, "info")
	return label, nil
}

func (e *Engine) sendDialogAnswer(ctx context.Context, paneID, key string, cursor int) error {
	if paneID == "" {
		return nil
	}
	if strings.EqualFold(key, "esc") {
		return e.fleet.SendKeys(ctx, paneID, "esc")
	}
	if cursor > 0 && key == "" {
		if rr, err := e.fleet.Read(ctx, paneID, "detection", "text", 0); err == nil {
			ap := agent.ParseApproval(rr.Text)
			if cursor-1 < len(ap.Choices) {
				return e.fleet.SendKeys(ctx, paneID, agent.MoveKeys(ap, cursor-1)...)
			}
		}
		keys := make([]string, 0, cursor)
		for i := 1; i < cursor; i++ {
			keys = append(keys, "down")
		}
		keys = append(keys, "enter")
		return e.fleet.SendKeys(ctx, paneID, keys...)
	}
	return e.fleet.SendText(ctx, paneID, key, true)
}

func matchLabelSubstringChoice(a *model.Action, raw string) (key, label string, cursor int, ok bool) {
	low := strings.ToLower(strings.TrimSpace(raw))
	if low == "" {
		return "", "", 0, false
	}
	for i, c := range a.Choices {
		if strings.Contains(strings.ToLower(c.Label), low) {
			return c.Key, c.Label, i + 1, true
		}
	}
	return "", "", 0, false
}

func matchActionChoice(a *model.Action, raw string) (key, label string, cursor int) {
	q := strings.TrimSpace(raw)
	if strings.EqualFold(q, "esc") {
		return "esc", "取消 (Esc)", 0
	}
	low := strings.ToLower(q)
	for i, c := range a.Choices {
		if (c.Key != "" && strings.EqualFold(c.Key, q)) || (c.Label != "" && strings.EqualFold(c.Label, q)) {
			return c.Key, c.Label, i + 1
		}
	}
	for i, c := range a.Choices {
		if c.Value != "" && strings.EqualFold(c.Value, q) {
			return c.Key, c.Label, i + 1
		}
	}
	if idx, err := strconv.Atoi(q); err == nil && idx >= 1 && idx <= len(a.Choices) {
		c := a.Choices[idx-1]
		return c.Key, c.Label, idx
	}
	for i, c := range a.Choices {
		if low != "" && strings.Contains(strings.ToLower(c.Label), low) {
			return c.Key, c.Label, i + 1
		}
	}
	return q, q, 0
}

func approvalToChoices(ap model.Approval) []model.ActionChoice {
	out := make([]model.ActionChoice, 0, len(ap.Choices))
	for _, c := range ap.Choices {
		key := c.Key
		if ap.Style == "cursor" {
			key = ""
		}
		out = append(out, model.ActionChoice{Key: key, Label: c.Label, Value: strconv.Itoa(c.Cursor)})
	}
	return out
}

// AddTimer creates and persists a new timer.

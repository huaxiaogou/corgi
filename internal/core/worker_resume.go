package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

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
		savedMode, savedROEnforcement             string
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
		savedMode, savedROEnforcement = es.Mode, es.ReadonlyEnforcement
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
				if savedMode == "" {
					savedMode = old.Mode
				}
				if savedROEnforcement == "" {
					savedROEnforcement = old.ReadonlyEnforcement
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
				if savedMode == "" {
					savedMode = es.Mode
				}
				if savedROEnforcement == "" {
					savedROEnforcement = es.ReadonlyEnforcement
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
		Kind:                kind,
		Machine:             machine,
		Cwd:                 cwd,
		Name:                name,
		Label:               title,
		Model:               modelName,
		Effort:              effort,
		SessionID:           sess,
		WorkItemID:          workID,
		Constraint:          savedConstraint,
		Mode:                savedMode,
		ReadonlyEnforcement: savedROEnforcement,
		CreatedBy:           createdBy,
		Thread:              thread,
		Keep:                keep,
		RepoRoot:            repoRoot,
		RepoCommonDir:       repoCommonDir,
		WorktreePath:        worktreePath,
		WorktreeBranch:      worktreeBranch,
		WorktreeBase:        wtBase,
		Subscribers:         subscribers,
	})
	if err != nil {
		return nil, false, err
	}
	_ = e.store.ForgetEndedSession(ctx, kind, machine, sess)
	return wk, true, nil
}

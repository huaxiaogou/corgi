package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/gitwt"
	"github.com/kiperjing/corgi/internal/model"
)

type worktreePrep struct {
	RepoRoot       string
	CommonDir      string
	WorktreePath   string
	WorktreeBranch string
	BaseCommit     string
}

func (e *Engine) inspectGitOnMachine(ctx context.Context, machine, dir string) gitwt.InspectResult {
	if strings.TrimSpace(dir) == "" {
		return gitwt.InspectResult{}
	}
	if machine == "" {
		return gitwt.Inspect(ctx, dir)
	}
	if e.fleet == nil {
		return gitwt.InspectResult{}
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return gitwt.InspectResult{}
	}
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"cwd": dir})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/git/inspect", bytes.NewReader(body))
	if err != nil {
		return gitwt.InspectResult{}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return gitwt.InspectResult{}
	}
	defer resp.Body.Close()
	var res gitwt.InspectResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res
}

func (e *Engine) createWorktreeOnMachine(ctx context.Context, machine, repoRoot, name, branch, relPath string) (gitwt.CreateResult, error) {
	if machine == "" {
		return gitwt.Create(ctx, e.home, repoRoot, name, branch, relPath)
	}
	if e.fleet == nil {
		return gitwt.CreateResult{}, fmt.Errorf("machine %q is offline", machine)
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return gitwt.CreateResult{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{
		"repo_root": repoRoot,
		"name":      name,
		"branch":    branch,
		"rel_path":  relPath,
	})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/git/worktree/create", bytes.NewReader(body))
	if err != nil {
		return gitwt.CreateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return gitwt.CreateResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			return gitwt.CreateResult{}, fmt.Errorf("%s", errBody.Error)
		}
		return gitwt.CreateResult{}, fmt.Errorf("remote worktree create failed: HTTP %d", resp.StatusCode)
	}
	var res gitwt.CreateResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return gitwt.CreateResult{}, err
	}
	return res, nil
}

func (e *Engine) cleanupWorktreeOnMachine(ctx context.Context, machine, repoRoot, wtPath, branch, baseCommit string) (gitwt.CleanupResult, error) {
	if wtPath == "" || repoRoot == "" {
		return gitwt.CleanupResult{}, nil
	}
	if machine == "" {
		return gitwt.Cleanup(ctx, repoRoot, wtPath, branch, baseCommit)
	}
	if e.fleet == nil {
		return gitwt.CleanupResult{}, fmt.Errorf("machine %q is offline", machine)
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return gitwt.CleanupResult{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{
		"repo_root":     repoRoot,
		"worktree_path": wtPath,
		"branch":        branch,
		"base_commit":   baseCommit,
	})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/git/worktree/cleanup", bytes.NewReader(body))
	if err != nil {
		return gitwt.CleanupResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return gitwt.CleanupResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		if errBody.Error != "" {
			return gitwt.CleanupResult{}, fmt.Errorf("%s", errBody.Error)
		}
		return gitwt.CleanupResult{}, fmt.Errorf("remote worktree cleanup failed: HTTP %d", resp.StatusCode)
	}
	var res gitwt.CleanupResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return gitwt.CleanupResult{}, err
	}
	return res, nil
}

func pathWithinRoot(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	cp := path.Clean(p)
	cr := path.Clean(root)
	if cp == cr {
		return true
	}
	return strings.HasPrefix(cp, cr+"/")
}

// hasOtherLiveAgentInRepo checks whether any other active (non-router, non-dream) agent on the same machine
// already occupies the same Git repository (by CommonDir, RepoRoot, or Cwd prefix).
func (e *Engine) hasOtherLiveAgentInRepo(ctx context.Context, machine, commonDir, repoRoot, excludeName string) bool {
	// 1. Check active managed workers in SQLite on the same machine.
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if w.Ended || w.Name == excludeName || w.Name == model.RouterName || w.Name == model.DreamName {
				continue
			}
			if w.Machine != machine {
				continue
			}
			if (commonDir != "" && w.RepoCommonDir != "" && w.RepoCommonDir == commonDir) ||
				(repoRoot != "" && w.RepoRoot != "" && path.Clean(w.RepoRoot) == path.Clean(repoRoot)) ||
				pathWithinRoot(w.Cwd, repoRoot) {
				return true
			}
		}
	}

	// 2. Also check live herdr agents directly (covers newly adopted or manually opened sessions).
	if e.fleet == nil {
		return false
	}
	agents, err := e.fleet.Agents(ctx)
	if err != nil {
		return false
	}
	for _, a := range agents {
		if a.Name != nil && (*a.Name == excludeName || *a.Name == model.RouterName || *a.Name == model.DreamName) {
			continue
		}
		_, am := fleet.SplitID(a.PaneID)
		if am != machine {
			continue
		}
		if a.Cwd == nil || *a.Cwd == "" {
			continue
		}
		if pathWithinRoot(*a.Cwd, repoRoot) {
			return true
		}
		// If the live agent is itself inside a worktree of the same repository, compare CommonDir.
		if commonDir != "" && strings.Contains(*a.Cwd, "/.corgi/worktrees/") {
			if other := e.inspectGitOnMachine(ctx, machine, *a.Cwd); other.IsGit && other.CommonDir == commonDir {
				return true
			}
		}
	}
	return false
}

// prepareWorktree inspects cwd and automatically creates an isolated Git worktree when:
// - mode == "on" (explicitly requested), OR
// - mode == "" / "auto" AND another active agent on the same machine already occupies the same Git repo.
func (e *Engine) prepareWorktree(ctx context.Context, machine, cwd, name, mode string) (string, *worktreePrep, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	insp := e.inspectGitOnMachine(ctx, machine, cwd)
	if !insp.IsGit || !insp.HasHead {
		if mode == "on" {
			return cwd, nil, fmt.Errorf("当前目录不是有效的 Git 仓库（或尚无初始提交），无法创建独立 Worktree")
		}
		return cwd, nil, nil
	}

	base := &worktreePrep{
		RepoRoot:  insp.RepoRoot,
		CommonDir: insp.CommonDir,
	}
	if mode == "off" {
		return cwd, base, nil
	}

	needIsolate := mode == "on"
	if !needIsolate {
		needIsolate = e.hasOtherLiveAgentInRepo(ctx, machine, insp.CommonDir, insp.RepoRoot, name)
	}
	if !needIsolate {
		return cwd, base, nil
	}

	created, err := e.createWorktreeOnMachine(ctx, machine, insp.RepoRoot, name, "", insp.RelPath)
	if err != nil {
		if mode == "on" {
			return cwd, nil, err
		}
		e.logger.Warn("auto worktree creation failed, falling back to primary cwd",
			"machine", machine, "repo", insp.RepoRoot, "worker", name, "err", err)
		return cwd, base, nil
	}
	base.WorktreePath = created.WorktreePath
	base.WorktreeBranch = created.Branch
	base.BaseCommit = created.BaseCommit
	return created.EffectiveCwd, base, nil
}

func worktreePromptNote(prep *worktreePrep) string {
	if prep == nil || prep.WorktreeBranch == "" {
		return ""
	}
	return fmt.Sprintf("\n- Git Worktree Isolation: you are working in an isolated git worktree on branch `%s` (branched from `%s`). Before reporting completion, run the repository's build/test checks, commit your verified changes on this branch, and include the branch name and verification result in your report; do not modify the primary checkout.",
		prep.WorktreeBranch, prep.RepoRoot)
}

func (e *Engine) resolveResumeCwd(ctx context.Context, machine, savedCwd, repoRoot string) string {
	if savedCwd == "" || repoRoot == "" || path.Clean(savedCwd) == path.Clean(repoRoot) {
		return savedCwd
	}
	if insp := e.inspectGitOnMachine(ctx, machine, savedCwd); insp.IsGit {
		return savedCwd
	}
	// Worktree directory was already cleaned up on stop; fall back to primary repo root so resume succeeds.
	return repoRoot
}

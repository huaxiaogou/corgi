package gitwt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var unsafeSegRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// InspectResult summarizes whether a directory belongs to a Git repository.
type InspectResult struct {
	IsGit     bool   `json:"is_git"`
	RepoRoot  string `json:"repo_root,omitempty"`
	CommonDir string `json:"common_dir,omitempty"`
	RelPath   string `json:"rel_path,omitempty"`
	Branch    string `json:"branch,omitempty"`
	HasHead   bool   `json:"has_head,omitempty"`
}

// CreateResult describes a newly created isolated Git worktree.
type CreateResult struct {
	RepoRoot     string `json:"repo_root"`
	CommonDir    string `json:"common_dir"`
	WorktreePath string `json:"worktree_path"`
	EffectiveCwd string `json:"effective_cwd"`
	Branch       string `json:"branch"`
	BaseCommit   string `json:"base_commit"`
}

// CleanupResult summarizes what happened when cleaning up a worker's Git worktree.
type CleanupResult struct {
	RemovedDir      bool   `json:"removed_dir"`
	DeletedBranch   bool   `json:"deleted_branch"`
	Dirty           bool   `json:"dirty"`
	UnmergedCommits int    `json:"unmerged_commits"`
	Note            string `json:"note,omitempty"`
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

func sanitizeSeg(raw, fallback string) string {
	s := strings.Trim(unsafeSegRe.ReplaceAllString(strings.TrimSpace(raw), "-"), "-.")
	if s == "" {
		return fallback
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// Inspect checks whether dir lies inside a Git work tree and returns canonical repository identifiers.
func Inspect(ctx context.Context, dir string) InspectResult {
	cleanDir := canonicalPath(dir)
	if cleanDir == "" {
		return InspectResult{}
	}
	fi, err := os.Stat(cleanDir)
	if err != nil || !fi.IsDir() {
		return InspectResult{}
	}
	inside, err := runGit(ctx, cleanDir, "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return InspectResult{}
	}
	top, err := runGit(ctx, cleanDir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return InspectResult{}
	}
	repoRoot := canonicalPath(top)

	commonRaw, err := runGit(ctx, cleanDir, "rev-parse", "--git-common-dir")
	if err != nil || commonRaw == "" {
		commonRaw = filepath.Join(repoRoot, ".git")
	}
	if !filepath.IsAbs(commonRaw) {
		commonRaw = filepath.Join(cleanDir, commonRaw)
	}
	commonDir := canonicalPath(commonRaw)

	rel := ""
	if r, rErr := filepath.Rel(repoRoot, cleanDir); rErr == nil && r != "." && !strings.HasPrefix(r, "..") {
		rel = r
	}

	_, headErr := runGit(ctx, repoRoot, "rev-parse", "--verify", "HEAD")
	branch, _ := runGit(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")

	return InspectResult{
		IsGit:     true,
		RepoRoot:  repoRoot,
		CommonDir: commonDir,
		RelPath:   rel,
		Branch:    branch,
		HasHead:   headErr == nil,
	}
}

// Create creates an isolated worktree under <home>/.corgi/worktrees/<repo>/<name> on branch corgi/<name>.
func Create(ctx context.Context, home, repoRoot, name, branch, relPath string) (CreateResult, error) {
	insp := Inspect(ctx, repoRoot)
	if !insp.IsGit {
		return CreateResult{}, fmt.Errorf("目录不是 Git 仓库：%s", repoRoot)
	}
	if !insp.HasHead {
		return CreateResult{}, fmt.Errorf("Git 仓库尚无初始提交，无法创建 Worktree：%s", insp.RepoRoot)
	}
	baseCommit, err := runGit(ctx, insp.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return CreateResult{}, err
	}

	workerSeg := sanitizeSeg(name, "worker")
	repoSeg := sanitizeSeg(filepath.Base(insp.RepoRoot), "repo")

	wantBranch := strings.TrimSpace(branch)
	if wantBranch == "" {
		wantBranch = "corgi/" + workerSeg
	}
	finalBranch := wantBranch
	for i := 2; i < 100; i++ {
		if _, err := runGit(ctx, insp.RepoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+finalBranch); err != nil {
			break // branch does not exist yet
		}
		finalBranch = fmt.Sprintf("%s-%d", wantBranch, i)
	}

	baseDir := filepath.Join(home, ".corgi", "worktrees", repoSeg)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return CreateResult{}, fmt.Errorf("create worktrees parent dir: %w", err)
	}
	wtPath := filepath.Join(baseDir, workerSeg)
	for i := 2; i < 100; i++ {
		if _, statErr := os.Stat(wtPath); os.IsNotExist(statErr) {
			break
		}
		wtPath = filepath.Join(baseDir, fmt.Sprintf("%s-%d", workerSeg, i))
	}

	if _, err := runGit(ctx, insp.RepoRoot, "worktree", "add", "-b", finalBranch, wtPath, "HEAD"); err != nil {
		return CreateResult{}, err
	}
	wtPath = canonicalPath(wtPath)

	effCwd := wtPath
	if rel := filepath.Clean(relPath); rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
		cand := filepath.Join(wtPath, rel)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			effCwd = cand
		}
	}

	return CreateResult{
		RepoRoot:     insp.RepoRoot,
		CommonDir:    insp.CommonDir,
		WorktreePath: wtPath,
		EffectiveCwd: effCwd,
		Branch:       finalBranch,
		BaseCommit:   baseCommit,
	}, nil
}

// Cleanup safely cleans up a worktree after a worker stops:
// - Dirty working tree: keep both directory and branch so no uncommitted edits are lost.
// - Clean working tree with unmerged commits: remove directory, preserve branch for merge/review.
// - Clean working tree with no unmerged commits: remove directory and delete temporary branch.
func Cleanup(ctx context.Context, repoRoot, worktreePath, branch, baseCommit string) (CleanupResult, error) {
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(repoRoot) == "" {
		return CleanupResult{}, nil
	}
	wtPath := canonicalPath(worktreePath)
	root := canonicalPath(repoRoot)

	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		_, _ = runGit(ctx, root, "worktree", "prune")
		return CleanupResult{Note: "worktree 目录已不存在"}, nil
	}

	statusOut, err := runGit(ctx, wtPath, "status", "--porcelain")
	if err != nil {
		return CleanupResult{}, err
	}
	if strings.TrimSpace(statusOut) != "" {
		note := fmt.Sprintf("含未提交改动，已保留 worktree 目录 %s", wtPath)
		if branch != "" {
			note += fmt.Sprintf("（分支 %s）", branch)
		}
		return CleanupResult{Dirty: true, Note: note}, nil
	}

	unmerged := 0
	if branch != "" {
		if _, err := runGit(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			// Check whether branch is already an ancestor of repoRoot's current HEAD.
			if _, ancErr := runGit(ctx, root, "merge-base", "--is-ancestor", branch, "HEAD"); ancErr != nil {
				revSpec := "HEAD.." + branch
				if baseCommit != "" {
					revSpec = baseCommit + ".." + branch
				}
				if cntStr, cErr := runGit(ctx, root, "rev-list", "--count", revSpec); cErr == nil {
					unmerged, _ = strconv.Atoi(strings.TrimSpace(cntStr))
				}
				if unmerged == 0 {
					// Fallback if baseCommit was same as branch but diverged from HEAD
					if cntStr, cErr := runGit(ctx, root, "rev-list", "--count", "HEAD.."+branch); cErr == nil {
						unmerged, _ = strconv.Atoi(strings.TrimSpace(cntStr))
					}
				}
			}
		}
	}

	res := CleanupResult{UnmergedCommits: unmerged}
	if _, err := runGit(ctx, root, "worktree", "remove", wtPath); err != nil {
		return res, err
	}
	_, _ = runGit(ctx, root, "worktree", "prune")
	res.RemovedDir = true

	if branch != "" {
		if unmerged == 0 {
			if _, err := runGit(ctx, root, "branch", "-d", branch); err == nil {
				res.DeletedBranch = true
				res.Note = fmt.Sprintf("已清理空 worktree 与临时分支 %s", branch)
				return res, nil
			}
		}
		res.Note = fmt.Sprintf("已清理 worktree 目录，保留分支 %s（含 %d 个未合并提交）", branch, unmerged)
		return res, nil
	}
	res.Note = "已清理 worktree 目录"
	return res, nil
}

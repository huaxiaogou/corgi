package gitwt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	run("init")
	if err := os.MkdirAll(filepath.Join(repo, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "web", "app.ts"), []byte("export {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return repo
}

func TestInspectCreateAndCleanupPolicies(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	home := t.TempDir()

	// 1. Inspect non-git dir
	nonGit := t.TempDir()
	if got := Inspect(ctx, nonGit); got.IsGit {
		t.Fatalf("expected non-git dir IsGit=false, got %+v", got)
	}

	// 2. Inspect repo subdir preserves rel_path and common_dir
	subInsp := Inspect(ctx, filepath.Join(repo, "web"))
	if !subInsp.IsGit || !subInsp.HasHead || subInsp.RelPath != "web" || subInsp.CommonDir == "" {
		t.Fatalf("unexpected subdir inspect: %+v", subInsp)
	}

	// 3. Create worktree & Case A: clean with 0 commits -> removes dir and deletes branch
	wt1, err := Create(ctx, home, repo, "agy-1", "", "web")
	if err != nil {
		t.Fatalf("Create wt1: %v", err)
	}
	if wt1.Branch != "corgi/agy-1" || filepath.Base(wt1.EffectiveCwd) != "web" {
		t.Fatalf("unexpected wt1 result: %+v", wt1)
	}
	if inspWt := Inspect(ctx, wt1.WorktreePath); !inspWt.IsGit || inspWt.CommonDir != subInsp.CommonDir {
		t.Fatalf("worktree CommonDir mismatch: got %q want %q", inspWt.CommonDir, subInsp.CommonDir)
	}
	c1, err := Cleanup(ctx, wt1.RepoRoot, wt1.WorktreePath, wt1.Branch, wt1.BaseCommit)
	if err != nil || !c1.RemovedDir || !c1.DeletedBranch || c1.Dirty {
		t.Fatalf("expected clean empty worktree removed+branch deleted, got c1=%+v err=%v", c1, err)
	}

	// 4. Case B: clean with 1 new commit -> removes dir, preserves branch
	wt2, err := Create(ctx, home, repo, "agy-2", "", "")
	if err != nil {
		t.Fatalf("Create wt2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt2.WorktreePath, "feature.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "feature.txt"}, {"commit", "-m", "add feature"}} {
		cmd := exec.Command("git", append([]string{"-C", wt2.WorktreePath}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, string(out))
		}
	}
	c2, err := Cleanup(ctx, wt2.RepoRoot, wt2.WorktreePath, wt2.Branch, wt2.BaseCommit)
	if err != nil || !c2.RemovedDir || c2.DeletedBranch || c2.UnmergedCommits != 1 {
		t.Fatalf("expected dir removed & branch kept with 1 commit, got c2=%+v err=%v", c2, err)
	}

	// 5. Case C: dirty uncommitted file -> preserves BOTH dir and branch
	wt3, err := Create(ctx, home, repo, "agy-3", "", "")
	if err != nil {
		t.Fatalf("Create wt3: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt3.WorktreePath, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c3, err := Cleanup(ctx, wt3.RepoRoot, wt3.WorktreePath, wt3.Branch, wt3.BaseCommit)
	if err != nil || !c3.Dirty || c3.RemovedDir || c3.DeletedBranch {
		t.Fatalf("expected dirty worktree preserved, got c3=%+v err=%v", c3, err)
	}
	if _, err := os.Stat(wt3.WorktreePath); err != nil {
		t.Fatalf("expected dirty worktree dir to still exist: %v", err)
	}
}

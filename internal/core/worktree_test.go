package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func initCoreTestGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return repo
}

func TestAutoWorktreeTriggersOnConcurrentSameRepoAndCleansUpOnStop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := initCoreTestGitRepo(t)
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := newMockHerdrFleet(t, func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "agent.list":
			return map[string]any{"agents": []any{}}, nil
		case "workspace.close", "pane.send_keys":
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})

	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. First agent in the Git repo uses the primary checkout directly (no worktree needed).
	cwd1, prep1, err := eng.prepareWorktree(ctx, "", repo, "agy-1", "auto")
	if err != nil {
		t.Fatalf("prepareWorktree first: %v", err)
	}
	if prep1 == nil || prep1.WorktreeBranch != "" || cwd1 != repo {
		t.Fatalf("expected 1st agent to stay in primary repo, got cwd=%q prep=%+v", cwd1, prep1)
	}
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name:          "agy-1",
		Kind:          "agy",
		Cwd:           cwd1,
		RepoRoot:      prep1.RepoRoot,
		RepoCommonDir: prep1.CommonDir,
		Status:        "idle",
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Second concurrent agent on the SAME repo automatically triggers an isolated worktree!
	cwd2, prep2, err := eng.prepareWorktree(ctx, "", repo, "agy-2", "auto")
	if err != nil {
		t.Fatalf("prepareWorktree second: %v", err)
	}
	if prep2 == nil || prep2.WorktreeBranch != "corgi/agy-2" || prep2.WorktreePath == "" || cwd2 == repo {
		t.Fatalf("expected 2nd agent to auto-create isolated worktree, got cwd=%q prep=%+v", cwd2, prep2)
	}
	if _, err := os.Stat(prep2.WorktreePath); err != nil {
		t.Fatalf("expected worktree dir to exist: %v", err)
	}

	// 3. Stopping the 2nd worker cleanly removes its empty worktree and temporary branch.
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name:           "agy-2",
		Kind:           "agy",
		Cwd:            cwd2,
		PaneID:         "w2:p1",
		WorkspaceID:    "w2",
		RepoRoot:       prep2.RepoRoot,
		RepoCommonDir:  prep2.CommonDir,
		WorktreePath:   prep2.WorktreePath,
		WorktreeBranch: prep2.WorktreeBranch,
		WorktreeBase:   prep2.BaseCommit,
		Status:         "idle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.StopWorker(ctx, "agy-2"); err != nil {
		t.Fatalf("StopWorker: %v", err)
	}
	if _, err := os.Stat(prep2.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected clean empty worktree dir removed on StopWorker, stat err=%v", err)
	}
}

func TestReadonlyNoAutoWorktreeAndClosedWorkItemSafeCleanup(t *testing.T) {
	repo := initCoreTestGitRepo(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := newMockHerdrFleet(t, func(method string, _ map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			return map[string]any{"workspace": map[string]any{"workspace_id": "w-ro"}, "root_pane": map[string]any{"pane_id": "w-ro:p1"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Seed an active worker occupying the repo so "auto" would normally isolate.
	_, prepBase, err := eng.prepareWorktree(ctx, "", repo, "base-1", "off")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "base-1", Kind: "agy", Cwd: repo, RepoRoot: prepBase.RepoRoot, RepoCommonDir: prepBase.CommonDir, Status: "idle",
	})

	// 0. Spawn with Mode="readonly" and empty Worktree MUST default to "off" (no stray worktree branch!).
	roWk, err := eng.Spawn(ctx, SpawnRequest{
		Name: "ro-check", Kind: "agy", Cwd: repo, Task: "只读核查仓库状态", Mode: "readonly",
	})
	if err != nil {
		t.Fatalf("Spawn readonly: %v", err)
	}
	if roWk.WorktreeBranch != "" || roWk.WorktreePath != "" {
		t.Fatalf("readonly worker must NOT auto-create a worktree, got branch=%q path=%q", roWk.WorktreeBranch, roWk.WorktreePath)
	}

	// 1. Create two real worktrees belonging to a WorkItem: one clean empty, one dirty with uncommitted edits.
	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "只读与清理验收", "等待关闭", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cwdClean, prepClean, err := eng.prepareWorktree(ctx, "", repo, "clean-wt", "on")
	if err != nil {
		t.Fatalf("prepare clean worktree: %v", err)
	}
	cwdDirty, prepDirty, err := eng.prepareWorktree(ctx, "", repo, "dirty-wt", "on")
	if err != nil {
		t.Fatalf("prepare dirty worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwdDirty, "uncommitted.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "clean-wt", Kind: "agy", Thread: model.MainThread, WorkItemID: wi.ID, Ended: true, Status: "done",
		Cwd: cwdClean, RepoRoot: prepClean.RepoRoot, WorktreePath: prepClean.WorktreePath,
		WorktreeBranch: prepClean.WorktreeBranch, WorktreeBase: prepClean.BaseCommit, Session: "sess-clean",
	})
	_ = st.SaveEndedSession(ctx, &model.EndedSession{
		ID: "h-clean", Kind: "agy", Name: "clean-wt", WorkItemID: wi.ID, Session: "sess-clean",
		Cwd: cwdClean, RepoRoot: prepClean.RepoRoot, WorktreePath: prepClean.WorktreePath, WorktreeBranch: prepClean.WorktreeBranch,
	}, store.EndedSessionRetentionLimit)

	_ = st.UpsertWorker(ctx, &model.Worker{
		Name: "dirty-wt", Kind: "agy", Thread: model.MainThread, WorkItemID: wi.ID, Ended: true, Status: "done",
		Cwd: cwdDirty, RepoRoot: prepDirty.RepoRoot, WorktreePath: prepDirty.WorktreePath,
		WorktreeBranch: prepDirty.WorktreeBranch, WorktreeBase: prepDirty.BaseCommit, Session: "sess-dirty",
	})

	// 2. Closing the WorkItem cleans the clean empty worktree & updates EndedSession, while preserving dirty edits!
	if _, err := eng.CloseWorkItem(ctx, wi.ID, "已完成交付"); err != nil {
		t.Fatalf("CloseWorkItem: %v", err)
	}
	if _, err := os.Stat(prepClean.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected clean worktree removed on CloseWorkItem, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cwdDirty, "uncommitted.txt")); err != nil {
		t.Fatalf("dirty worktree MUST be preserved on CloseWorkItem, stat err=%v", err)
	}
	cleanAfter, _ := st.GetWorker(ctx, "clean-wt")
	if cleanAfter == nil || cleanAfter.WorktreePath != "" || cleanAfter.WorktreeBranch != "" || cleanAfter.Cwd != prepClean.RepoRoot {
		t.Fatalf("expected clean worker metadata updated to repo root, got %+v", cleanAfter)
	}
	ended, _ := st.ListEndedSessions(ctx, 10)
	if len(ended) == 0 || ended[0].WorktreePath != "" || ended[0].Cwd != prepClean.RepoRoot {
		t.Fatalf("expected EndedSession updated after clean worktree removal, got %+v", ended)
	}
}

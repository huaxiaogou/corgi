package gitwt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyGoRepoPassAndFail(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	_, _ = runGit(ctx, repo, "config", "user.email", "test@example.com")
	_, _ = runGit(ctx, repo, "config", "user.name", "tester")

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module example.com/demo\n\ngo 1.22\n")
	write("add.go", "package demo\n\nfunc Add(a, b int) int { return a + b }\n")
	write("add_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 { t.Fatal(\"bad\") }\n}\n")
	if _, err := runGit(ctx, repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "init"); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo})
	if err != nil {
		t.Fatalf("Verify pass case: %v", err)
	}
	if !res.OK || !res.ChecksPassed || !res.IsGit || res.Branch != "main" || res.Dirty {
		t.Fatalf("unexpected pass result: %+v", res)
	}
	if len(res.Checks) != 2 {
		t.Fatalf("expected 2 go checks (vet+test), got %d: %+v", len(res.Checks), res.Checks)
	}

	// Break test and verify fail + short-circuit output.
	write("add.go", "package demo\n\nfunc Add(a, b int) int { return a - b }\n")
	failRes, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo})
	if err != nil {
		t.Fatalf("Verify fail case returned error: %v", err)
	}
	if failRes.OK || failRes.ChecksPassed || !failRes.Dirty {
		t.Fatalf("expected failed dirty result, got %+v", failRes)
	}
	if len(failRes.Checks) != 2 || !failRes.Checks[0].Passed || failRes.Checks[1].Passed {
		t.Fatalf("expected vet pass and test fail, got %+v", failRes.Checks)
	}
}

func TestVerifyCustomCmdAndReviewVerdict(t *testing.T) {
	dir := t.TempDir()
	orig := reviewExecFn
	defer func() { reviewExecFn = orig }()

	reviewExecFn = func(_ context.Context, _, _, preferred, brief string, _ VerifyResult, _ string) (string, bool, string, error) {
		if preferred != "codex" || !strings.Contains(brief, "fix auth") {
			t.Errorf("unexpected review args: preferred=%q brief=%q", preferred, brief)
		}
		return "codex", false, "VERDICT: BLOCK\n- auth.go:12 missing nil check", nil
	}

	res, err := Verify(context.Background(), t.TempDir(), VerifyRequest{
		Dir:      dir,
		Cmd:      "echo ok",
		Review:   true,
		Reviewer: "codex",
		Brief:    "fix auth bug",
	})
	if err != nil {
		t.Fatalf("Verify with review: %v", err)
	}
	if !res.ChecksPassed || !res.ReviewRun || res.ReviewPassed || res.OK {
		t.Fatalf("expected checks pass + review block => OK=false, got %+v", res)
	}
	if !strings.Contains(res.ReviewSummary, "missing nil check") {
		t.Fatalf("unexpected review summary: %q", res.ReviewSummary)
	}

	// Passing review should flip OK=true.
	reviewExecFn = func(_ context.Context, _, _, _, _ string, _ VerifyResult, _ string) (string, bool, string, error) {
		return "codex", true, "VERDICT: PASS\n- 无阻断问题", nil
	}
	passRes, err := Verify(context.Background(), t.TempDir(), VerifyRequest{
		Dir:    dir,
		Cmd:    "echo ok",
		Review: true,
	})
	if err != nil {
		t.Fatalf("Verify passing review: %v", err)
	}
	if !passRes.OK || !passRes.ReviewPassed {
		t.Fatalf("expected OK=true on passing review, got %+v", passRes)
	}
}

func TestVerifyBaseCommitSkipIfCleanAndExactChanges(t *testing.T) {
	repo := t.TempDir()
	ctx := context.Background()

	if _, err := runGit(ctx, repo, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	_, _ = runGit(ctx, repo, "config", "user.email", "test@example.com")
	_, _ = runGit(ctx, repo, "config", "user.name", "tester")

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module example.com/basecheck\n\ngo 1.22\n")
	write("add.go", "package basecheck\n\nfunc Add(a, b int) int { return a + b }\n")
	write("add_test.go", "package basecheck\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 { t.Fatal(\"bad\") }\n}\n")
	if _, err := runGit(ctx, repo, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	base, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Clean tree relative to base: skip checks completely.
	cleanRes, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo, BaseCommit: base, SkipIfClean: true})
	if err != nil {
		t.Fatalf("clean verify err: %v", err)
	}
	if !cleanRes.BaseChecked || !cleanRes.Clean || !cleanRes.OK || len(cleanRes.Checks) != 0 {
		t.Fatalf("expected BaseChecked=true Clean=true Checks=0, got %+v", cleanRes)
	}

	// 2. Committed-only change relative to base (working tree clean): must run checks.
	write("add.go", "package basecheck\n\nfunc Add(a, b int) int { return a + b + 0 }\n")
	if _, err := runGit(ctx, repo, "add", "add.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, "commit", "-m", "noop touch"); err != nil {
		t.Fatal(err)
	}
	committedRes, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo, BaseCommit: base, SkipIfClean: true})
	if err != nil {
		t.Fatalf("committed verify err: %v", err)
	}
	if !committedRes.BaseChecked || committedRes.Clean || !committedRes.OK || len(committedRes.Checks) != 2 {
		t.Fatalf("expected committed-only change to run 2 go checks and pass, got %+v", committedRes)
	}

	// 3. Reset to base and add untracked-only failing test file: must detect and fail checks.
	if _, err := runGit(ctx, repo, "reset", "--hard", base); err != nil {
		t.Fatal(err)
	}
	write("untracked_test.go", "package basecheck\n\nimport \"testing\"\n\nfunc TestUntrackedFail(t *testing.T) { t.Fatal(\"boom\") }\n")
	untrackedRes, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo, BaseCommit: base, SkipIfClean: true})
	if err != nil {
		t.Fatalf("untracked verify err: %v", err)
	}
	if !untrackedRes.BaseChecked || untrackedRes.Clean || untrackedRes.OK || len(untrackedRes.Checks) != 2 || untrackedRes.Checks[1].Passed {
		t.Fatalf("expected untracked-only file to trigger failing go-test, got %+v", untrackedRes)
	}
	_ = os.Remove(filepath.Join(repo, "untracked_test.go"))

	// 4. Unreadable BaseCommit with SkipIfClean: BaseChecked=false, 0 checks run.
	badBaseRes, err := Verify(ctx, t.TempDir(), VerifyRequest{Dir: repo, BaseCommit: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", SkipIfClean: true})
	if err != nil {
		t.Fatalf("bad base verify err: %v", err)
	}
	if badBaseRes.BaseChecked || badBaseRes.Clean || len(badBaseRes.Checks) != 0 {
		t.Fatalf("expected unreadable base to return BaseChecked=false and skip commands, got %+v", badBaseRes)
	}
}

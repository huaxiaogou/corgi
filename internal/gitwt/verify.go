package gitwt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultVerifyTimeout = 4 * time.Minute
	maxCheckOutputRunes  = 4000
	maxDiffBytes         = 40 << 10
	maxReviewOutRunes    = 4000
)

// VerifyRequest configures a stateless workspace check + optional read-only cross-model review.
type VerifyRequest struct {
	Dir      string `json:"dir"`
	Cmd      string `json:"cmd,omitempty"`
	Review   bool   `json:"review,omitempty"`
	Reviewer string `json:"reviewer,omitempty"` // codex | claude (default: auto)
	Brief    string `json:"brief,omitempty"`
}

// VerifyCheck records one executed build/test/lint command.
type VerifyCheck struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

// VerifyResult is returned synchronously by Verify without persisting DB state.
type VerifyResult struct {
	OK            bool          `json:"ok"`
	Dir           string        `json:"dir"`
	IsGit         bool          `json:"is_git"`
	Branch        string        `json:"branch,omitempty"`
	Dirty         bool          `json:"dirty"`
	StatusShort   string        `json:"status_short,omitempty"`
	DiffStat      string        `json:"diff_stat,omitempty"`
	ChecksPassed  bool          `json:"checks_passed"`
	Checks        []VerifyCheck `json:"checks,omitempty"`
	Note          string        `json:"note,omitempty"`
	ReviewRun     bool          `json:"review_run,omitempty"`
	Reviewer      string        `json:"reviewer,omitempty"`
	ReviewPassed  bool          `json:"review_passed,omitempty"`
	ReviewSummary string        `json:"review_summary,omitempty"`
}

var reviewExecFn = runReadOnlyReview

// Verify runs git inspection, auto-detected (or explicit) repo checks, and an optional
// read-only cross-model diff review inside dir.
func Verify(ctx context.Context, home string, req VerifyRequest) (VerifyResult, error) {
	dir := canonicalPath(req.Dir)
	if dir == "" {
		return VerifyResult{}, fmt.Errorf("dir is required")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return VerifyResult{}, fmt.Errorf("目录不存在：%s", dir)
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultVerifyTimeout)
		defer cancel()
	}

	res := VerifyResult{
		Dir:          dir,
		ChecksPassed: true,
	}

	workRoot := dir
	insp := Inspect(ctx, dir)
	if insp.IsGit {
		res.IsGit = true
		res.Branch = insp.Branch
		workRoot = insp.RepoRoot
		if st, err := runGit(ctx, workRoot, "status", "--short"); err == nil {
			res.StatusShort = st
			res.Dirty = strings.TrimSpace(st) != ""
		}
		res.DiffStat = collectDiffStat(ctx, workRoot)
	}

	cmds := planVerifyCommands(workRoot, dir, req.Cmd, changedFiles(ctx, workRoot, res.IsGit))
	if len(cmds) == 0 {
		res.Note = "未自动检测到构建/测试脚本，可通过 --cmd 指定检查命令"
	}
	for _, spec := range cmds {
		chk := runVerifyCheck(ctx, spec.dir, spec.name, spec.command)
		res.Checks = append(res.Checks, chk)
		if !chk.Passed {
			res.ChecksPassed = false
			break
		}
	}

	if req.Review {
		diffText := ""
		if res.IsGit {
			diffText = collectReviewDiff(ctx, workRoot)
		}
		reviewer, passed, summary, rErr := reviewExecFn(ctx, home, workRoot, req.Reviewer, req.Brief, res, diffText)
		res.ReviewRun = true
		res.Reviewer = reviewer
		if rErr != nil {
			res.ReviewPassed = false
			res.ReviewSummary = rErr.Error()
		} else {
			res.ReviewPassed = passed
			res.ReviewSummary = clipTailRunes(strings.TrimSpace(summary), maxReviewOutRunes)
		}
	}

	res.OK = res.ChecksPassed && (!req.Review || res.ReviewPassed)
	return res, nil
}

type checkSpec struct {
	name    string
	dir     string
	command string
}

func planVerifyCommands(repoRoot, cwd, customCmd string, changed []string) []checkSpec {
	if c := strings.TrimSpace(customCmd); c != "" {
		return []checkSpec{{name: "custom", dir: cwd, command: c}}
	}
	var specs []checkSpec
	hasGo := fileExists(filepath.Join(repoRoot, "go.mod"))
	if hasGo {
		specs = append(specs,
			checkSpec{name: "go-vet", dir: repoRoot, command: "go vet ./..."},
			checkSpec{name: "go-test", dir: repoRoot, command: "go test -count=1 ./..."},
		)
	}

	webDir := filepath.Join(repoRoot, "web")
	if fileExists(filepath.Join(webDir, "package.json")) && fileExists(filepath.Join(webDir, "node_modules")) {
		webChanged := !hasGo || anyPathPrefix(changed, "web/")
		if webChanged {
			scripts := readPackageScripts(filepath.Join(webDir, "package.json"))
			if _, ok := scripts["typecheck"]; ok {
				specs = append(specs, checkSpec{name: "web-typecheck", dir: webDir, command: "npm run typecheck"})
			}
			if _, ok := scripts["build"]; ok {
				specs = append(specs, checkSpec{name: "web-build", dir: webDir, command: "npm run build"})
			}
		}
	} else if !hasGo && fileExists(filepath.Join(cwd, "package.json")) && fileExists(filepath.Join(cwd, "node_modules")) {
		scripts := readPackageScripts(filepath.Join(cwd, "package.json"))
		if _, ok := scripts["typecheck"]; ok {
			specs = append(specs, checkSpec{name: "typecheck", dir: cwd, command: "npm run typecheck"})
		}
		if _, ok := scripts["test"]; ok {
			specs = append(specs, checkSpec{name: "test", dir: cwd, command: "npm test"})
		}
		if _, ok := scripts["build"]; ok {
			specs = append(specs, checkSpec{name: "build", dir: cwd, command: "npm run build"})
		}
	}
	return specs
}

func runVerifyCheck(ctx context.Context, dir, name, command string) VerifyCheck {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil && out == "" {
		out = err.Error()
	}
	return VerifyCheck{
		Name:       name,
		Command:    command,
		Passed:     err == nil,
		DurationMS: time.Since(start).Milliseconds(),
		Output:     clipTailRunes(out, maxCheckOutputRunes),
	}
}

func changedFiles(ctx context.Context, repoRoot string, isGit bool) []string {
	if !isGit {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	addLines := func(raw string) {
		for _, line := range strings.Split(raw, "\n") {
			f := strings.TrimSpace(line)
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	if s, err := runGit(ctx, repoRoot, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(s, "\n") {
			if len(line) > 3 {
				addLines(line[3:])
			}
		}
	}
	if s, err := runGit(ctx, repoRoot, "diff", "--name-only", "HEAD"); err == nil {
		addLines(s)
	}
	if base := branchDiffBase(ctx, repoRoot); base != "" {
		if s, err := runGit(ctx, repoRoot, "diff", "--name-only", base+"..HEAD"); err == nil {
			addLines(s)
		}
	}
	return out
}

func branchDiffBase(ctx context.Context, repoRoot string) string {
	for _, ref := range []string{"main", "master", "origin/main", "origin/master"} {
		if base, err := runGit(ctx, repoRoot, "merge-base", ref, "HEAD"); err == nil && base != "" {
			head, _ := runGit(ctx, repoRoot, "rev-parse", "HEAD")
			if base != head {
				return base
			}
		}
	}
	return ""
}

func collectDiffStat(ctx context.Context, repoRoot string) string {
	var parts []string
	if s, err := runGit(ctx, repoRoot, "diff", "--stat", "HEAD"); err == nil && strings.TrimSpace(s) != "" {
		parts = append(parts, s)
	}
	if base := branchDiffBase(ctx, repoRoot); base != "" {
		if s, err := runGit(ctx, repoRoot, "diff", "--stat", base+"..HEAD"); err == nil && strings.TrimSpace(s) != "" {
			parts = append(parts, "Branch commits:\n"+s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func collectReviewDiff(ctx context.Context, repoRoot string) string {
	var parts []string
	if base := branchDiffBase(ctx, repoRoot); base != "" {
		if d, err := runGit(ctx, repoRoot, "diff", base+"..HEAD"); err == nil && strings.TrimSpace(d) != "" {
			parts = append(parts, "=== Branch diff ("+base[:min(8, len(base))]+"..HEAD) ===\n"+d)
		}
	}
	if d, err := runGit(ctx, repoRoot, "diff", "HEAD"); err == nil && strings.TrimSpace(d) != "" {
		parts = append(parts, "=== Uncommitted diff ===\n"+d)
	}
	if len(parts) == 0 {
		if d, err := runGit(ctx, repoRoot, "show", "--stat", "--patch", "-1", "HEAD"); err == nil && strings.TrimSpace(d) != "" {
			parts = append(parts, "=== Latest commit ===\n"+d)
		}
	}
	raw := strings.Join(parts, "\n\n")
	if len(raw) > maxDiffBytes {
		return raw[:maxDiffBytes] + "\n... [diff truncated]\n"
	}
	return raw
}

func runReadOnlyReview(ctx context.Context, home, workRoot, preferred, brief string, res VerifyResult, diffText string) (reviewer string, passed bool, summary string, err error) {
	chosen, bin := pickReviewerBin(home, preferred)
	if bin == "" {
		return chosen, false, "", fmt.Errorf("未找到可用的只读审查工具（codex / claude）")
	}

	prompt := buildReviewPrompt(brief, res, diffText)
	switch chosen {
	case "codex":
		outFile, fErr := os.CreateTemp("", "corgi-verify-review-*.txt")
		if fErr != nil {
			return chosen, false, "", fErr
		}
		outPath := outFile.Name()
		_ = outFile.Close()
		defer os.Remove(outPath)

		cmd := exec.CommandContext(ctx, bin, "exec", "-s", "read-only", "-C", workRoot, "-o", outPath, "-")
		cmd.Stdin = strings.NewReader(prompt)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		rawOut, _ := os.ReadFile(outPath)
		text := strings.TrimSpace(string(rawOut))
		if text == "" {
			text = strings.TrimSpace(stderr.String())
		}
		if runErr != nil && text == "" {
			return chosen, false, "", fmt.Errorf("codex review failed: %w", runErr)
		}
		return chosen, parseReviewVerdict(text, runErr == nil), text, nil
	default:
		cmd := exec.CommandContext(ctx, bin, "-p", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob")
		cmd.Dir = workRoot
		cmd.Stdin = strings.NewReader(prompt)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		text := strings.TrimSpace(stdout.String())
		if text == "" {
			text = strings.TrimSpace(stderr.String())
		}
		if runErr != nil && text == "" {
			return chosen, false, "", fmt.Errorf("claude review failed: %w", runErr)
		}
		return chosen, parseReviewVerdict(text, runErr == nil), text, nil
	}
}

func buildReviewPrompt(brief string, res VerifyResult, diffText string) string {
	var b strings.Builder
	b.WriteString("You are a concise, read-only code reviewer. Inspect the changes below for bugs, regressions, security issues, or missed requirements.\n")
	b.WriteString("Start your response with EXACTLY one line: `VERDICT: PASS` (no blocking issues) or `VERDICT: BLOCK` (blocking issues found).\n")
	b.WriteString("Then provide 1-5 concise Chinese bullet points (include file:line and concrete fix for any blocking finding).\n\n")
	if strings.TrimSpace(brief) != "" {
		b.WriteString("## Task Brief\n")
		b.WriteString(strings.TrimSpace(brief))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "## Checks Summary\nBranch: %s | Dirty: %v | ChecksPassed: %v\n", res.Branch, res.Dirty, res.ChecksPassed)
	for _, c := range res.Checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "- %s (`%s`): %s\n", c.Name, c.Command, status)
		if !c.Passed && c.Output != "" {
			b.WriteString("```\n" + clipTailRunes(c.Output, 1200) + "\n```\n")
		}
	}
	if strings.TrimSpace(res.StatusShort) != "" {
		b.WriteString("\n## git status --short\n" + res.StatusShort + "\n")
	}
	if strings.TrimSpace(diffText) != "" {
		b.WriteString("\n## Diff\n```diff\n" + diffText + "\n```\n")
	}
	return b.String()
}

func parseReviewVerdict(text string, exitOK bool) bool {
	if !exitOK {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		u := strings.ToUpper(strings.TrimSpace(strings.Trim(line, "`* ")))
		if strings.HasPrefix(u, "VERDICT:") {
			rest := strings.TrimSpace(strings.TrimPrefix(u, "VERDICT:"))
			return strings.HasPrefix(rest, "PASS")
		}
	}
	// No explicit VERDICT header: treat as pass unless clearly stating BLOCK/FAIL.
	upper := strings.ToUpper(text)
	return !strings.Contains(upper, "VERDICT: BLOCK")
}

func pickReviewerBin(home, preferred string) (name, bin string) {
	order := []string{"codex", "claude"}
	if p := strings.ToLower(strings.TrimSpace(preferred)); p == "claude" || p == "codex" {
		order = []string{p}
	}
	for _, k := range order {
		if p := findReviewBin(k, home); p != "" {
			return k, p
		}
	}
	return order[0], ""
}

func findReviewBin(name, home string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".npm-global", "bin", name),
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func readPackageScripts(pkgJSONPath string) map[string]string {
	raw, err := os.ReadFile(pkgJSONPath)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func anyPathPrefix(paths []string, prefix string) bool {
	for _, p := range paths {
		if strings.HasPrefix(filepath.ToSlash(strings.TrimSpace(p)), prefix) {
			return true
		}
	}
	return false
}

func clipTailRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return "…[truncated]\n" + string(r[len(r)-maxRunes:])
}

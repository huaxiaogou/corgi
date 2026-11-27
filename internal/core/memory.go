package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const routerReadSnapshotFile = ".corgi-read.md"

// UpFrontBudgetBytes caps the files loaded at every fresh Router session start (~10 KB).
const UpFrontBudgetBytes = 10 << 10

// MemoryFile represents one markdown file under memory/ or dreams/.
type MemoryFile struct {
	Name string    `json:"name"`
	Text string    `json:"text"`
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
}

// MemorySnapshot is returned to the Web UI memory editor.
type MemorySnapshot struct {
	Dir         string       `json:"dir"`
	Files       []MemoryFile `json:"files"`
	Dreams      []MemoryFile `json:"dreams"`
	UpFrontSize int64        `json:"up_front"`
	MsgSize     int64        `json:"messages"`
	Budget      int64        `json:"budget"`
	Dreaming    bool         `json:"dreaming"`
	LastDream   *time.Time   `json:"last_dream,omitempty"`
	NextDream   *time.Time   `json:"next_dream,omitempty"`
}

// MemoryManager owns ~/.corgi/router/ (CORGI.md, memory/, dreams/).
type MemoryManager struct {
	routerDir string
}

// NewMemoryManager creates a MemoryManager rooted at corgiDir/router.
func NewMemoryManager(corgiDir string) *MemoryManager {
	return &MemoryManager{routerDir: filepath.Join(corgiDir, "router")}
}

// RouterDir returns the router workspace path.
func (m *MemoryManager) RouterDir() string { return m.routerDir }

// MemDir returns the memory directory path.
func (m *MemoryManager) MemDir() string { return filepath.Join(m.routerDir, "memory") }

// DreamsDir returns the nightly dream journal directory.
func (m *MemoryManager) DreamsDir() string { return filepath.Join(m.routerDir, "dreams") }

// Ensure creates the layered memory directory skeleton without overwriting existing files.
func (m *MemoryManager) Ensure() error {
	dirs := []string{
		filepath.Join(m.MemDir(), "notes"),
		filepath.Join(m.MemDir(), "tasks", "private"),
		filepath.Join(m.MemDir(), "tasks", "groups"),
		filepath.Join(m.MemDir(), "tasks", "done"),
		m.DreamsDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	seeds := map[string]string{
		"instructions.md": "# 主人的长期指令与偏好\n",
		"notes/index.md":  "# 笔记索引\n\n每个主题文件一行说明。\n",
		"messages.md":     "# Corgi 近期回复（由系统自动维护）\n",
	}
	for rel, content := range seeds {
		p := filepath.Join(m.MemDir(), rel)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				return fmt.Errorf("seed %s: %w", p, err)
			}
		}
	}
	return nil
}

// WriteMessages refreshes memory/messages.md with the last 16 human & Corgi dialogue turns.
func (m *MemoryManager) WriteMessages(entries []*model.Entry) error {
	var lines []string
	for i := len(entries) - 1; i >= 0 && len(lines) < 16; i-- {
		e := entries[i]
		switch e.Role {
		case "corgi":
			lines = append(lines, fmt.Sprintf("- %s [%s] Corgi: %s",
				e.At.Format("01-02 15:04"), e.Thread, oneLineClip(e.Text, 240)))
		case "user":
			lines = append(lines, fmt.Sprintf("- %s [%s] 主人: %s",
				e.At.Format("01-02 15:04"), e.Thread, oneLineClip(e.Text, 240)))
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	body := "# 近期对话记录（系统自动维护，从旧到新）\n\n" + strings.Join(lines, "\n") + "\n"
	return os.WriteFile(filepath.Join(m.MemDir(), "messages.md"), []byte(body), 0o600)
}

// OpenTaskFiles returns relative paths under tasks/private and tasks/groups.
func (m *MemoryManager) OpenTaskFiles() []string {
	var out []string
	for _, sub := range []string{"private", "groups"} {
		matches, _ := filepath.Glob(filepath.Join(m.MemDir(), "tasks", sub, "*.md"))
		for _, p := range matches {
			rel, _ := filepath.Rel(m.MemDir(), p)
			out = append(out, filepath.ToSlash(rel))
		}
	}
	sort.Strings(out)
	return out
}

// UpFrontSize computes total bytes of up-front files (instructions and notes/index).
func (m *MemoryManager) UpFrontSize() int64 {
	var n int64
	add := func(rel string) {
		if fi, err := os.Stat(filepath.Join(m.MemDir(), rel)); err == nil {
			n += fi.Size()
		}
	}
	add("instructions.md")
	add("notes/index.md")
	return n
}

// MessagesSize returns the byte size of memory/messages.md.
func (m *MemoryManager) MessagesSize() int64 {
	if fi, err := os.Stat(filepath.Join(m.MemDir(), "messages.md")); err == nil {
		return fi.Size()
	}
	return 0
}

// ListFiles walks memory/ and returns all markdown files sorted by path.
func (m *MemoryManager) ListFiles() []MemoryFile {
	var out []MemoryFile
	root := m.MemDir()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out = append(out, MemoryFile{
			Name: filepath.ToSlash(rel),
			Text: string(b),
			Size: fi.Size(),
			Mod:  fi.ModTime(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListDreams returns up to 30 most recent dream journals (newest first).
func (m *MemoryManager) ListDreams() []MemoryFile {
	matches, _ := filepath.Glob(filepath.Join(m.DreamsDir(), "*.md"))
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	if len(matches) > 30 {
		matches = matches[:30]
	}
	out := make([]MemoryFile, 0, len(matches))
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		b, _ := os.ReadFile(p)
		out = append(out, MemoryFile{Name: filepath.Base(p), Text: string(b), Size: fi.Size(), Mod: fi.ModTime()})
	}
	return out
}

// SaveFile writes a markdown file safely inside memory/ (disallowing traversal and messages.md).
func (m *MemoryManager) SaveFile(relPath, content string) error {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relPath)))
	if !strings.HasSuffix(clean, ".md") || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || clean == "messages.md" {
		return fmt.Errorf("只允许修改 memory/ 下的 .md 文件（messages.md 为只读）")
	}
	if len(content) > 256<<10 {
		return fmt.Errorf("文件内容不能超过 256 KB")
	}
	target := filepath.Join(m.MemDir(), clean)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	normalized := strings.TrimRight(content, "\n") + "\n"
	return writeFileAtomic(target, []byte(normalized), 0o600)
}

// WriteRouterDocs writes CORGI.md and mirrors it to CLAUDE.md / AGENTS.md / GEMINI.md / CODEBUDDY.md.
func (m *MemoryManager) WriteRouterDocs(binPath string, st model.Settings) error {
	_, err := m.WriteRouterDocsChanged(binPath, st)
	return err
}

// WriteRouterDocsChanged writes CORGI.md and reports whether an already-read Router manual changed.
func (m *MemoryManager) WriteRouterDocsChanged(binPath string, st model.Settings) (bool, error) {
	if err := m.Ensure(); err != nil {
		return false, err
	}
	wMachine := strings.TrimSpace(st.WorkerMachine)
	if wMachine == "" {
		wMachine = "hub (local)"
	}
	wDefaults := []string{"machine " + wMachine, "kind " + st.WorkerKind}
	if wm := strings.TrimSpace(st.WorkerModel); wm != "" {
		wDefaults = append(wDefaults, "model "+wm)
	}
	if we := strings.TrimSpace(st.WorkerEffort); we != "" {
		wDefaults = append(wDefaults, "effort "+we)
	}
	wDefaults = append(wDefaults, "cwd "+st.WorkerCwd)
	doc := []byte(strings.NewReplacer(
		"{{BIN}}", binPath,
		"{{CHARS}}", fmt.Sprint(st.ReplyChars),
		"{{WMACHINE}}", wMachine,
		"{{WDEFAULTS}}", strings.Join(wDefaults, ", "),
		"{{WKIND}}", st.WorkerKind,
		"{{WCWD}}", st.WorkerCwd,
		"{{BUDGET}}", fmt.Sprint(UpFrontBudgetBytes>>10),
		"{{LAYOUT}}", memoryLayout,
	).Replace(routerManualTemplate))
	for _, name := range []string{"CORGI.md", "CLAUDE.md", "AGENTS.md", "GEMINI.md", "CODEBUDDY.md"} {
		if err := os.WriteFile(filepath.Join(m.routerDir, name), doc, 0o600); err != nil {
			return false, fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := m.EnsureClaudePermissions(binPath); err != nil {
		return false, fmt.Errorf("write claude permissions: %w", err)
	}
	snapPath := filepath.Join(m.routerDir, routerReadSnapshotFile)
	prev, err := os.ReadFile(snapPath)
	if err != nil || bytes.Equal(prev, doc) {
		return false, nil
	}
	_ = os.WriteFile(snapPath, doc, 0o600)
	return true, nil
}

// EnsureClaudePermissions writes a minimal .claude/settings.json allowlist inside routerDir so
// Router and Dream sessions can maintain memory/ and invoke Corgi CLI commands even when
// AskPermissions is enabled (bypass permissions turned off).
func (m *MemoryManager) EnsureClaudePermissions(binPath string) error {
	settingsDir := filepath.Join(m.routerDir, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		return err
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	cfg := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	perms, _ := cfg["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	var existing []string
	if rawAllow, ok := perms["allow"].([]any); ok {
		for _, it := range rawAllow {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				existing = append(existing, s)
			}
		}
	}
	managed := routerPermissionRules(m.routerDir, binPath)
	changed := false
	for _, rule := range managed {
		if !slices.Contains(existing, rule) {
			existing = append(existing, rule)
			changed = true
		}
	}
	if !changed {
		if _, err := os.Stat(settingsPath); err == nil {
			return nil
		}
	}
	perms["allow"] = existing
	cfg["permissions"] = perms
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0o600)
}

func routerPermissionRules(dir, binPath string) []string {
	cleanDir := filepath.Clean(dir)
	abs := "/" + strings.TrimPrefix(cleanDir, "/") // Claude Code uses //abs/path for absolute path rules
	var rules []string
	for _, p := range []string{"memory/**", "dreams/**", "memory.md", "memory-archive.md"} {
		rules = append(rules,
			"Read("+p+")", "Edit("+p+")", "Write("+p+")",
			"Read(/"+abs+"/"+p+")", "Edit(/"+abs+"/"+p+")", "Write(/"+abs+"/"+p+")",
		)
	}
	rules = append(rules, "Read(dream/**)", "Read(/"+abs+"/dream/**)")
	for _, base := range []string{"", cleanDir + "/"} {
		rules = append(rules,
			"Bash(mkdir -p "+base+"memory:*)",
			"Bash(mv "+base+"memory:*)",
			"Bash(rm "+base+"memory.md:*)",
			"Bash(rm "+base+"memory-archive.md:*)",
		)
	}
	for _, cmd := range []string{"ls", "find", "du", "wc", "cat", "head", "tail", "sort", "grep", "stat", "echo", "date"} {
		rules = append(rules, "Bash("+cmd+":*)")
	}
	rules = append(rules, "Bash(corgi:*)")
	if b := strings.TrimSpace(binPath); b != "" && b != "corgi" {
		rules = append(rules, "Bash("+b+":*)")
	}
	return rules
}

// MarkRouterDocsRead records that a newly-launched Router session has read the current CORGI.md.
func (m *MemoryManager) MarkRouterDocsRead() {
	if b, err := os.ReadFile(filepath.Join(m.routerDir, "CORGI.md")); err == nil {
		_ = os.WriteFile(filepath.Join(m.routerDir, routerReadSnapshotFile), b, 0o600)
	}
}

// SessionStartPreamble builds the preamble prepended to a fresh Router session's first turn.
func (m *MemoryManager) SessionStartPreamble() string {
	var b strings.Builder
	b.WriteString("[corgi] Session start. Read CORGI.md, memory/instructions.md, memory/notes/index.md, and memory/messages.md. Hub `work_items` (shown below and via `corgi list`) is the sole authoritative open-task ledger; always check `corgi list` before answering overall-status questions. Then handle the events below.\n")
	if sz := m.UpFrontSize(); sz > UpFrontBudgetBytes*2 {
		fmt.Fprintf(&b, "Notice: up-front memory is %d KB (target ~%d KB); tidy it after handling these events.\n",
			sz/1024, UpFrontBudgetBytes/1024)
	}
	return b.String()
}

func oneLineClip(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

const memoryLayout = `memory/
  instructions.md          the human's standing instructions and preferences (≤80 chars/line). Read every session.
  notes/index.md           one-line index per note file. Read every session.
  notes/<topic>.md         free-form deep context & reusable knowledge: project architecture, design trade-offs,
                           data locations, working commands/queries, pitfalls, result paths; machines.md for hosts;
                           agents.md for which agent/model succeeds or fails on which computer. Date mutable facts ("（截至 11-25）").
  tasks/                   legacy/historical archive only (read on demand; NEVER maintain open task lists here — Hub SQLite work_items is the sole open-task ledger).
  messages.md              your recent replies to the human (auto-maintained by the hub). Read every session.
Store pointers (paths, thread ids, URLs), not copies.`

const routerManualTemplate = `# You are Corgi — the always-on Super-Agent router

You are the single, always-on point of contact between one human (via Web/PWA and WeCom) and a team of coding agents ("workers") running across connected computers in herdr terminals. The human talks only to you; you keep the big picture and delegate execution to isolated workers.

## Each turn
1. Read the **[corgi] event** batch: Web chat, WeCom single/group messages, webhooks (GitHub, generic), timers, and worker events. Handle the full batch in one turn.
2. For each event decide: answer, dispatch, follow up, or stay silent.
3. Delegate real engineering work (coding, debugging, audits, builds, repo search) to workers — **NEVER run broad ` + "`find`" + ` / ` + "`grep`" + ` scans across local disks yourself**. Stay light (<10s per turn):
   - First run ` + "`{{BIN}} list`" + ` to see online computers (` + "`machines`" + `), managed ` + "`workers`" + `, AND live user-opened ` + "`agents`" + ` (with their ` + "`machine`" + ` and ` + "`cwd`" + `).
   - If the human already opened a matching session in the target repo (shown in ` + "`agents`" + `), reuse it via ` + "`{{BIN}} send <name> \"...\"`" + ` (which automatically adopts and delivers to it) instead of spawning a duplicate or searching the Hub disk!
   - If the target repo's machine or path is not in ` + "`memory/notes/`" + ` or ` + "`{{BIN}} list`" + `, ask the human in one short reply with 2–3 ` + "`--opt`" + ` choices (or spawn on the likely machine so the worker locates it) rather than searching the Hub filesystem yourself.
4. Reply via ` + "`{{BIN}} reply`" + ` following the communication rules below.

## Talking to the human
- Usually ≤{{CHARS}} units (1 Chinese char or 1 English word/version/path counts as 1; Markdown syntax and newlines are free), leading with the conclusion and direct answer. Go longer whenever detail or a multi-item list is needed, still leading with the conclusion. Stay silent when nothing requires human attention.
- **Formatting & lists**: Whenever listing **2 or more** items, statuses, progress updates, or next steps, **ALWAYS** format them as real multi-line Markdown bullets (` + "`- `" + ` or ` + "`1. `" + `), one item per line, sent via stdin heredoc (` + "`{{BIN}} reply [...] - <<'EOF'`" + `). **NEVER** cram multiple items into a single paragraph separated by semicolons (e.g. ` + "`1）…；2）…；3）…`" + `).
- Speak like a crisp colleague, never leak internal plumbing words (router, worker, trigger, event) to the human ("我派了一个 claude 去…", "api-profiler 查完了"); name workers so the human can open them directly.
- Include concrete findings (numbers, decisions) in the reply; whenever delivering a file (HTML prototype/report, Markdown doc, image, etc.), **ALWAYS** pass explicit absolute file paths via ` + "`--file PATH [--machine M] [--work WI]`" + ` (always use Corgi's native ` + "`reply --file`" + `, NEVER external ` + "`wecom-cli`" + `) and **DO NOT paste raw local absolute paths into the reply prose** — Hub automatically stores an immutable snapshot, renders clean preview cards in Web & 小本本, and sends both the Hub online preview link and original file attachment to WeCom.
- **Overall status questions** ("整体什么情况 / 还有什么事 / 还有哪些待办"): **ALWAYS run ` + "`{{BIN}} list`" + ` first** and report EVERY open Hub ` + "`work_item`" + ` plus any unresolved actions/unmerged branches — never answer from chat memory alone.
- When answering something other than the human's latest message (an earlier question, a worker result, a timer), pass ` + "`--re <id>`" + `.
- Messages ending with "附件：" list uploaded file paths on the target machine; pass those paths straight into the worker's task brief.

## Memory (~{{BUDGET}} KB up-front budget)
Your memory lives in ` + "`memory/`" + ` (this directory). Before context fills up, Corgi hands over into a fresh session that starts by reading the up-front files.

{{LAYOUT}}

Update memory & Hub ledger as you go:
- Standing rules go into ` + "`instructions.md`" + ` immediately (recurring schedules agreed in chat become a timer + a ` + "`notes/`" + ` workflow, never duplicated in ` + "`instructions.md`" + `).
- **Authoritative Work Ledger (Dynamic Title & Next-Step Maintenance)**: Hub SQLite ` + "`work_items`" + ` is the sole open-task ledger across compactions and restarts. Always pass a crisp 8–18 char Chinese ` + "`--title \"...\"`" + ` when creating a new task via ` + "`{{BIN}} spawn`" + ` or ` + "`{{BIN}} send`" + ` (NEVER leave raw prompt boilerplate like "新任务（用户原话）…" as the title). Whenever a worker reports a deliverable or a task evolves to a new phase (e.g. prototype -> implementation -> PR review), or when an event tags the title as ` + "`(draft)`" + `, proactively run ` + "`{{BIN}} task set WI --title \"...\" --next \"...\"`" + ` (skipping ` + "`--title`" + ` only if tagged ` + "`(user-locked title)`" + `). Run ` + "`{{BIN}} task done WI \"reason\"`" + ` ONLY after final delivery/merge is confirmed. Keep free-form deep project context and reusable findings in ` + "`notes/<topic>.md`" + ` (never maintain duplicate open-task lists under ` + "`memory/tasks/`" + `).
- Keep up-front files (` + "`instructions.md`" + `, ` + "`notes/index.md`" + `) under ~{{BUDGET}} KB combined.
- At night (or on demand), an isolated Dream session reviews recent chats & logs, consolidates memory, and proposes ripe skills in ` + "`dreams/`" + `.

## CLI Tools
- ` + "`{{BIN}} reply [--thread T] [--re ID] [--opt \"choice\"]... [--multi] [--confirm-label \"确认\"] [--machine M] [--work WI] [--file PATH]... [--check] [--digest] [--category morning|patrol|stale] [--worker W] \"text\"`" + ` (for multi-line, list, or code replies, ALWAYS use stdin heredoc: ` + "`{{BIN}} reply [...] - <<'EOF'`" + `)
  - **Strict card boundary (never cardify status inquiries)**: Whenever the human asks for status/progress ("怎么样了 / 什么进展 / 整体什么情况 / 还有什么事") or when sending routine progress summaries, Q&A, or notifications, ALWAYS use plain ` + "`reply`" + ` (**NO ` + "`--opt`" + ` and NO ` + "`--check`" + `**).
  - Attach 2–4 short ` + "`--opt`" + ` buttons (recommended choice first; add ` + "`--multi`" + ` when multiple choices can be ticked together, up to 16 options) ONLY when asking the human to make a **single concrete decision or authorization** (A vs B on one topic, approving one concrete step, or batch-archiving stale sessions). **NEVER** pack 2+ independent unrelated projects into fake mutually-exclusive buttons (e.g. "先验收已完成分支" vs "批准另一项目").
  - When delivering a file located on a remote computer (e.g. Mac node), pass ` + "`--machine <M>`" + ` alongside ` + "`--file /abs/path`" + ` (omit ` + "`--machine`" + ` for Hub-local files); pass ` + "`--work <WI>`" + ` and ` + "`--worker <W>`" + ` so the delivery card binds to that exact worker (direct follow-up button). When forwarding a worker's captured report without explicit ` + "`--file`" + `, pass ` + "`--re <report_tID>`" + ` alongside ` + "`--worker <W>`" + ` to auto-attach that specific report snapshot; plain status replies with ` + "`--worker <W>`" + ` alone will never attach old reports. Do not paste raw file paths in the reply text.
  - Pass ` + "`--check`" + ` ONLY when delivering a finished artifact/report that genuinely awaits human review (delivers as clean plain Markdown text in WeCom, and adds a lightweight ` + "`✓ 已看`" + ` reminder in Web without blocking decisions). **NEVER** pass ` + "`--check`" + ` on status inquiries, multi-item summaries, Q&A, or closed items.
  - When sending a digest card with ` + "`--digest --opt`" + `, pass ` + "`--category morning`" + ` (morning review), ` + "`--category stale`" + ` (batch archival), or ` + "`--category patrol`" + ` (midday single-item reminder); it replaces any prior unresolved digest card in-place.
- ` + "`{{BIN}} spawn [--kind K] [--machine M] [--project P] [--cwd DIR] [--model ID] [--effort E] [--thread T] [--name N] [--title \"8-18字标题\"] [--constraint \"红线\"]... [--work WI | --ephemeral] [--subscribe] [--resume W] [--worktree|--no-worktree] \"task brief\"`" + `
  Starts a worker asynchronously and returns immediately (human's defaults: {{WDEFAULTS}}). Hub resolves per-tool model defaults and configured ` + "`projects`" + ` paths (shown in ` + "`{{BIN}} list`" + `) automatically, and forwards the human's original request verbatim to the worker. When the task has strict boundaries (e.g. read-only analysis, no production/config changes, no restarts/push), pass concise ` + "`--constraint`" + ` flags (<=200 chars total): Hub persists this immutable red line on the worker session and reinforces it across follow-ups and resumes. Auto-creates a Hub ` + "`work_item`" + ` by default; pass ` + "`--work WI`" + ` to attach to an existing work item, or ` + "`--ephemeral`" + ` ONLY for one-off queries/lookups with no user deliverable. Omit ` + "`--machine`" + ` / ` + "`--kind`" + ` / ` + "`--model`" + ` / ` + "`--effort`" + ` / ` + "`--cwd`" + ` to use defaults — **always run on the human's default computer (` + "`{{WMACHINE}}`" + `)** unless the task explicitly targets files, repos, or services that exist only on another computer.
  **Automatic Git Worktree isolation**: when another live agent on the same computer already occupies the same Git repository, Corgi automatically creates an isolated Git worktree (` + "`corgi/<worker>`" + `) so parallel edits never collide (use ` + "`--worktree`" + ` to force or ` + "`--no-worktree`" + ` to opt out). When an engineering worker finishes changes on a branch, verify first with ` + "`{{BIN}} verify [--review] [--send] WORKER`" + ` and auto-fix failures before reporting verified results to the human (include the branch name; still ask before external push/merge/deploy unless already authorized).
  Write a complete, self-contained brief: goal, context, constraints, definition of done, expected deliverables.
  **Reuse memory**: when ` + "`notes/`" + ` holds relevant experience (data paths, proven commands, pitfalls), include the note's full path for a hub-local worker ("先读 /…/memory/notes/x.md"), or inline the key lines directly for a remote-machine worker.
  Carry forward agent/model/effort preferences the human already stated in the thread.
- ` + "`{{BIN}} send [--work WI] [--title \"8-18字标题\"] WORKER - <<'EOF'`" + ` (or ` + "`{{BIN}} send [--work WI] [--title \"...\"] WORKER \"short text\"`" + `) — steer an active worker or an existing live agent from ` + "`{{BIN}} list`" + ` (automatically adopts manually-opened sessions and inherits/creates its ` + "`work_item`" + `). Whenever the message contains backticks, ` + "`$()`" + `, flags (e.g. ` + "`--check`" + `), quotes, or multiple lines, **ALWAYS** use single-quoted stdin heredoc ` + "`- <<'EOF'`" + ` so Bash never eats or executes characters.
- ` + "`{{BIN}} verify [--review] [--reviewer codex|claude] [--cmd \"...\"] [--send] WORKER`" + ` — run repo checks (git status/diffstat + build/tests) and optional read-only cross-model diff review in WORKER's workspace on its host computer; with ` + "`--send`" + `, automatically bounces failed checks or blocking review findings straight back to WORKER for auto-fix (max 2 auto-fix rounds before escalating).
- ` + "`{{BIN}} task ls [--all]`" + ` / ` + "`{{BIN}} task set <WI|new> [--title \"...\"] [--next \"...\"] [--worker W] [--branch B]`" + ` / ` + "`{{BIN}} task done <WI> \"closing reason\"`" + ` — manage Hub's durable work-item ledger directly (also use ` + "`task set new`" + ` for non-worker promises).
- ` + "`{{BIN}} answer WORKER CHOICE`" + ` — answer a blocked worker's terminal dialog by choice key, label words, or ` + "`esc`" + `.
- ` + "`{{BIN}} subscribe [--off] WORKER THREAD`" + ` — route a thread's future events (e.g. GitHub PR or WeCom topic) straight to that worker; ` + "`chat:main`" + ` always stays with you.
- ` + "`{{BIN}} stop WORKER`" + ` — close a worker's terminal while keeping its session ID so ` + "`{{BIN}} spawn --resume WORKER \"next\"`" + ` can continue with full context later.
- ` + "`{{BIN}} keep [--off] WORKER`" + ` — mark (or unmark) a session as permanently pinned/exempt from stale-session archival prompts.
- ` + "`{{BIN}} show ID`" + ` — expand any spilled trigger, entry, thread, or worker (including a live worker's latest message/summary across any connected computer). Results carry further IDs: follow them hop by hop (Chain of Recall) to recover older context.
- ` + "`{{BIN}} list`" + ` — inspect open ` + "`work_items`" + `, pending actions, online machines, live manual agents (with machine & cwd), managed workers, threads, and timers.
- ` + "`{{BIN}} timer add [--thread T] [--to WORKER] \"in 30m|every 2h|daily 09:00|weekdays 09:00|weekly mon,thu 09:00|at YYYY-MM-DD HH:MM\" \"text\"`" + ` (` + "`timer ls`" + ` / ` + "`timer rm ID`" + `).
- Note: worker turn completions and blocked dialogs automatically carry their latest summary and numbered choices in the event text; never invoke local ` + "`herdr`" + ` commands directly (workers may run on remote computers).

## Handling Events
- ` + "`candidate`" + ` events (unsolicited webhooks): act only if covered by ` + "`memory/instructions.md`" + ` or prior human instruction; otherwise stay silent.
- Worker completion / reports: for code/repo changes, run ` + "`{{BIN}} verify [--review] [--send] WORKER`" + ` and bounce any failed checks or blocking findings back to the worker (up to 2 rounds) before reporting verified results to the human — never halt on an unverified branch asking the human to manually test/review first. Record reusable lessons in ` + "`notes/<topic>.md`" + ` (and ` + "`notes/index.md`" + `) and agent/model reliability notes in ` + "`notes/agents.md`" + `.
- **Worker lifecycle & Hygiene / Patrol**:
  - **Clean up finished one-off workers**: Once a worker's result has been passed on and nothing further is waiting on it (a one-off check, query, build, or merged & deployed job), run ` + "`{{BIN}} stop WORKER`" + ` so the active list holds only work in flight (` + "`stop`" + ` preserves its session ID so ` + "`{{BIN}} spawn --resume WORKER \"next\"`" + ` resumes losslessly later). Keep a worker open while a follow-up, fix, or human decision is still waiting on it.
  - Do NOT nag the human about recently finished idle workers unless something genuinely needs their decision.
  - On ` + "`[corgi idle-patrol]`" + ` or ` + "`[corgi morning-digest]`" + `:
    - Stay **completely silent** if nothing truly needs action OR if you already reported the same waiting items with no new change.
    - **NEVER** include already-healthy / completed status noise (e.g. "XX 已部署健康") in a pending digest.
    - When **2+ independent unrelated items** need attention: send **at most ONE** clean Markdown bulleted checklist via plain ` + "`{{BIN}} reply - <<'EOF'`" + ` (**NO ` + "`--opt`" + ` and NO ` + "`--check`" + `**, NO fake A/B buttons), one short line per item naming the exact worker/branch and concrete next decision.
    - Use ` + "`{{BIN}} reply --digest --category morning|patrol|stale --opt ...`" + ` **ONLY** for a single atomic decision (1 unmerged branch, 1 blocker, or 1 stale-session batch archive). Clicking one option in a digest only acts on that item — never drop other tracked items silently.
  - **NEVER auto-stop manually-opened sessions (` + "`by=you`" + `) or sessions marked ` + "`KEEP`" + `**. When offering batch archival of stale Corgi sessions, always list their names so the human can specify exceptions (e.g. "保留 X，其余归档" → run ` + "`{{BIN}} keep X`" + ` and ` + "`{{BIN}} stop`" + ` only on the rest).
- **Skills**: when the human approves turning a mature how-to into a skill, spawn a worker to author it in the requested repo/personal skill directory, then shrink the note to a pointer.
- **` + "`BLOCKED:`" + ` report vs. terminal dialog ` + "`BLOCKED`" + `**:
  - A report starting ` + "`BLOCKED:`" + ` means the worker needs information or a new approach — unblock it yourself (` + "`send`" + ` or respawn on another agent/model/machine) unless it crosses the high-risk gate below.
  - A terminal dialog ` + "`BLOCKED`" + ` event means a live CLI prompt is waiting: answer routine dialogs yourself with ` + "`answer`" + ` (trusting the workspace folder, confirming an already-requested option/model, reviewing the human's own hooks when bypass is on) and notify the human in one line. Leave high-risk prompts to the human.

## Drive It Forward (Autonomy Gate)
You own the outcome, not just the first attempt.
- When a worker fails, stalls, or hits a quota/provider error, recover proactively: retry, switch to another installed agent (` + "`claude`" + ` / ` + "`codex`" + ` / ` + "`agy`" + ` / ` + "`codebuddy`" + `), model, or computer, and inform the human in one line.
- **How** (agent, model, verification steps, and overriding machine when target files/services live only on another host) is yours to decide, while respecting the human's default computer (` + "`{{WMACHINE}}`" + `) whenever no other host is required. **What** (goal and scope) belongs to the human.
- **Ask first** (with 2–4 ` + "`--opt`" + ` buttons, recommended option first) ONLY for: deleting/overwriting data, schema/index changes, sending/publishing/pushing externally, spending money, modifying global host config outside the task, or genuine scope ambiguity.
`

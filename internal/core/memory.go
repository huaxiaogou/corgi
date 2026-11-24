package core

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

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

// WriteMessages refreshes memory/messages.md with Corgi's last 10 replies.
func (m *MemoryManager) WriteMessages(entries []*model.Entry) error {
	var lines []string
	for i := len(entries) - 1; i >= 0 && len(lines) < 10; i-- {
		e := entries[i]
		if e.Role == "corgi" {
			lines = append(lines, fmt.Sprintf("- %s [%s] %s",
				e.At.Format("01-02 15:04"), e.Thread, oneLineClip(e.Text, 240)))
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	body := "# Corgi 近期回复（系统自动维护，从旧到新）\n\n" + strings.Join(lines, "\n") + "\n"
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

// UpFrontSize computes total bytes of up-front files (instructions, notes/index, open tasks).
func (m *MemoryManager) UpFrontSize() int64 {
	var n int64
	add := func(rel string) {
		if fi, err := os.Stat(filepath.Join(m.MemDir(), rel)); err == nil {
			n += fi.Size()
		}
	}
	add("instructions.md")
	add("notes/index.md")
	for _, f := range m.OpenTaskFiles() {
		add(f)
	}
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
	return os.WriteFile(target, []byte(normalized), 0o600)
}

// WriteRouterDocs writes CORGI.md and mirrors it to CLAUDE.md / AGENTS.md / GEMINI.md / CODEBUDDY.md.
func (m *MemoryManager) WriteRouterDocs(binPath string, st model.Settings) error {
	if err := m.Ensure(); err != nil {
		return err
	}
	doc := strings.NewReplacer(
		"{{BIN}}", binPath,
		"{{CHARS}}", fmt.Sprint(st.ReplyChars),
		"{{WKIND}}", st.WorkerKind,
		"{{WCWD}}", st.WorkerCwd,
		"{{BUDGET}}", fmt.Sprint(UpFrontBudgetBytes>>10),
		"{{LAYOUT}}", memoryLayout,
	).Replace(routerManualTemplate)
	for _, name := range []string{"CORGI.md", "CLAUDE.md", "AGENTS.md", "GEMINI.md", "CODEBUDDY.md"} {
		if err := os.WriteFile(filepath.Join(m.routerDir, name), []byte(doc), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// SessionStartPreamble builds the preamble prepended to a fresh Router session's first turn.
func (m *MemoryManager) SessionStartPreamble() string {
	var b strings.Builder
	b.WriteString("[corgi] Session start. Read CORGI.md, memory/instructions.md, memory/notes/index.md, memory/messages.md")
	if tasks := m.OpenTaskFiles(); len(tasks) > 0 {
		fmt.Fprintf(&b, " and open tasks (%s)", strings.Join(tasks, ", "))
	} else {
		b.WriteString(" (no open tasks)")
	}
	b.WriteString(". Then handle the events below.\n")
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
  notes/<topic>.md         reusable knowledge: project layout, data locations, working commands/queries,
                           pitfalls, result paths; machines.md for hosts; agents.md for which agent/model
                           succeeds or fails on which computer. Date mutable facts ("（截至 11-25）").
  tasks/private/<slug>.md  open single-chat / Web tasks: goal, thread id, workers, done, left, next step.
  tasks/groups/<slug>.md   open WeCom group tasks (grouped by group topic).
  tasks/done/              finished or abandoned tasks with a closing line on how they ended.
  messages.md              your recent replies to the human (auto-maintained by the hub). Read every session.
Store pointers (paths, thread ids, URLs), not copies.`

const routerManualTemplate = `# You are Corgi — the always-on Super-Agent router

You are the single, always-on point of contact between one human (via Web/PWA and WeCom) and a team of coding agents ("workers") running across connected computers in herdr terminals. The human talks only to you; you keep the big picture and delegate execution to isolated workers.

## Each turn
1. Read the **[corgi] event** batch: Web chat, WeCom single/group messages, webhooks (GitHub, generic), timers, and worker events. Handle the full batch in one turn.
2. For each event decide: answer, dispatch, follow up, or stay silent.
3. Delegate real engineering work (coding, debugging, audits, builds) to isolated workers via ` + "`{{BIN}} spawn`" + `. Let each worker own its task end to end while you stay light on judgment and coordination.
4. Reply via ` + "`{{BIN}} reply`" + ` following the communication rules below.

## Talking to the human
- Usually ≤{{CHARS}} characters (in the human's language), leading with the conclusion and direct answer. Go longer only when asked for detail or a structured list, still leading with the conclusion. Stay silent when nothing requires human attention.
- Speak like a crisp colleague, never leak internal plumbing words (router, worker, trigger, event) to the human ("我派了一个 claude 去…", "api-profiler 查完了"); name workers so the human can open them directly.
- Include concrete findings (numbers, decisions, paths) in the reply and attach full paths to detail files so Web can preview them and WeCom can deliver them.
- When answering something other than the human's latest message (an earlier question, a worker result, a timer), pass ` + "`--re <id>`" + `.
- Messages ending with "附件：" list uploaded file paths on the target machine; pass those paths straight into the worker's task brief.

## Memory (~{{BUDGET}} KB up-front budget)
Your memory lives in ` + "`memory/`" + ` (this directory). Before context fills up, Corgi hands over into a fresh session that starts by reading the up-front files.

{{LAYOUT}}

Update memory as you go:
- Standing rules go into ` + "`instructions.md`" + ` immediately (recurring schedules agreed in chat become a timer + a ` + "`notes/`" + ` workflow, never duplicated in ` + "`instructions.md`" + `).
- Every spawned task gets a file under ` + "`tasks/private/`" + ` or ` + "`tasks/groups/`" + `, and moves to ` + "`tasks/done/`" + ` when finished.
- Keep up-front files (` + "`instructions.md`" + `, ` + "`notes/index.md`" + `, open tasks) under ~{{BUDGET}} KB combined.
- At night (or on demand), an isolated Dream session reviews recent chats & logs, consolidates memory, and proposes ripe skills in ` + "`dreams/`" + `.

## CLI Tools
- ` + "`{{BIN}} reply [--thread T] [--re ID] [--opt \"choice\"]... [--check] \"text\"`" + `
  Attach 2–4 short ` + "`--opt`" + ` buttons (recommended choice first) whenever asking the human to decide or proposing a concrete next step (distilling a skill, adjusting a timer, fixing a nearby issue). Add ` + "`--check`" + ` when no ` + "`--opt`" + ` applies yet human review/sign-off is still needed; omit for plain status updates.
- ` + "`{{BIN}} spawn [--kind K] [--machine M] [--cwd DIR] [--model ID] [--effort E] [--thread T] [--name N] [--subscribe] [--resume W] \"task brief\"`" + `
  Starts a worker asynchronously and returns immediately (defaults: kind {{WKIND}}, cwd {{WCWD}}). Run ` + "`{{BIN}} list`" + ` to see online computers.
  Write a complete, self-contained brief: goal, context, constraints, definition of done, expected deliverables.
  **Reuse memory**: when ` + "`notes/`" + ` holds relevant experience (data paths, proven commands, pitfalls), include the note's full path for a hub-local worker ("先读 /…/memory/notes/x.md"), or inline the key lines directly for a remote-machine worker.
  Carry forward agent/model/effort preferences the human already stated in the thread.
- ` + "`{{BIN}} send WORKER \"text\"`" + ` — steer an active worker immediately (even mid-turn).
- ` + "`{{BIN}} answer WORKER CHOICE`" + ` — answer a blocked worker's terminal dialog by choice key, label words, or ` + "`esc`" + `.
- ` + "`{{BIN}} subscribe [--off] WORKER THREAD`" + ` — route a thread's future events (e.g. GitHub PR or WeCom topic) straight to that worker; ` + "`chat:main`" + ` always stays with you.
- ` + "`{{BIN}} stop WORKER`" + ` — close a worker's terminal while keeping its session ID so ` + "`{{BIN}} spawn --resume WORKER \"next\"`" + ` can continue with full context later.
- ` + "`{{BIN}} show ID`" + ` — expand any spilled trigger, entry, thread, or worker. Results carry further IDs: follow them hop by hop (Chain of Recall) to recover older context.
- ` + "`{{BIN}} list`" + ` — inspect machines, workers, threads, and timers.
- ` + "`{{BIN}} timer add [--thread T] [--to WORKER] \"in 30m|every 2h|daily 09:00|weekdays 09:00|weekly mon,thu 09:00|at YYYY-MM-DD HH:MM\" \"text\"`" + ` (` + "`timer ls`" + ` / ` + "`timer rm ID`" + `).
- Inspect a worker's live terminal when needed: ` + "`herdr agent read WORKER --source recent-unwrapped --lines 80`" + `.

## Handling Events
- ` + "`candidate`" + ` events (unsolicited webhooks): act only if covered by ` + "`memory/instructions.md`" + ` or prior human instruction; otherwise stay silent.
- Worker completion / reports: verify outcome, report succinctly to the human or dispatch the next step. Record reusable lessons in ` + "`notes/<topic>.md`" + ` (and ` + "`notes/index.md`" + `) and agent/model reliability notes in ` + "`notes/agents.md`" + `.
- **Hygiene**: ` + "`stop`" + ` one-off workers once their result is handed off and nothing more is pending; keep workers alive only while awaiting follow-up or human confirmation.
- **Skills**: when the human approves turning a mature how-to into a skill, spawn a worker to author it in the requested repo/personal skill directory, then shrink the note to a pointer.
- **` + "`BLOCKED:`" + ` report vs. terminal dialog ` + "`BLOCKED`" + `**:
  - A report starting ` + "`BLOCKED:`" + ` means the worker needs information or a new approach — unblock it yourself (` + "`send`" + ` or respawn on another agent/model/machine) unless it crosses the high-risk gate below.
  - A terminal dialog ` + "`BLOCKED`" + ` event means a live CLI prompt is waiting: answer routine dialogs yourself with ` + "`answer`" + ` (trusting the workspace folder, confirming an already-requested option/model, reviewing the human's own hooks when bypass is on) and notify the human in one line. Leave high-risk prompts to the human.

## Drive It Forward (Autonomy Gate)
You own the outcome, not just the first attempt.
- When a worker fails, stalls, or hits a quota/provider error, recover proactively: retry, switch to another installed agent (` + "`claude`" + ` / ` + "`codex`" + ` / ` + "`agy`" + ` / ` + "`codebuddy`" + `), model, or computer, and inform the human in one line.
- **How** (agent, model, machine, verification steps) is yours to decide. **What** (goal and scope) belongs to the human.
- **Ask first** (with 2–4 ` + "`--opt`" + ` buttons, recommended option first) ONLY for: deleting/overwriting data, schema/index changes, sending/publishing/pushing externally, spending money, modifying global host config outside the task, or genuine scope ambiguity.
`

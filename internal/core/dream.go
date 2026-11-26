package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

const (
	dreamSkillsPrefix = "可做成 skill："
	dreamFixPrefix    = "可改进 skill："
)

type dreamRun struct {
	at          time.Time
	started     bool
	worked      bool
	idleAt      time.Time
	reason      string
	blockedAt   time.Time
	told        bool
	archivedRef string
}

// Dreaming reports whether an isolated memory-consolidation Dream session is active.
func (e *Engine) Dreaming() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dream != nil
}

// LastDream returns the timestamp of the last completed dream run (zero if never).
func (e *Engine) LastDream(ctx context.Context) time.Time {
	raw, ok, err := e.store.GetKV(ctx, "last_dream")
	if err != nil || !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *Engine) setLastDream(ctx context.Context, t time.Time) {
	_ = e.store.SetKV(ctx, "last_dream", t.Format(time.RFC3339))
}

func (e *Engine) dreamAtLocked() (h, m int, ok bool) {
	v := strings.TrimSpace(e.settings.DreamAt)
	if v == "" {
		v = "04:00"
	}
	if strings.EqualFold(v, "off") {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 4, 0, true
	}
	return h, m, true
}

// NextDream computes the next scheduled nightly dream time (zero when disabled).
func (e *Engine) NextDream(ctx context.Context, now time.Time) time.Time {
	e.mu.Lock()
	h, m, ok := e.dreamAtLocked()
	e.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	last := e.LastDream(ctx)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !last.Before(t) || now.After(t.Add(3*time.Hour)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func (e *Engine) hasActivitySince(ctx context.Context, since time.Time) bool {
	entries, _, err := e.store.ListEntries(ctx, "", 60)
	if err != nil {
		return false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i]
		if !since.IsZero() && en.At.Before(since) {
			return false
		}
		if en.Role == "user" || en.Role == "corgi" {
			return true
		}
	}
	return false
}

// checkDream drives an active dream session or initiates the scheduled nightly dream.
func (e *Engine) checkDream(ctx context.Context, live map[string]liveInfo, hasQueued bool) {
	e.mu.Lock()
	d := e.dream
	e.mu.Unlock()
	if d != nil {
		e.driveDream(ctx, d, live)
		return
	}
	now := time.Now()
	next := e.NextDream(ctx, now)
	if next.IsZero() || now.Before(next) || now.After(next.Add(3*time.Hour)) {
		return
	}
	li, alive := live[model.RouterName]
	e.mu.Lock()
	busy := e.handover != nil || e.settings.Paused || hasQueued || !e.routerAskedAt.IsZero() ||
		(alive && li.status != "idle" && li.status != "done")
	e.mu.Unlock()
	if busy {
		return
	}
	if !e.hasActivitySince(ctx, e.LastDream(ctx)) {
		e.setLastDream(ctx, now) // quiet day: skip waking LLM, zero tokens spent
		return
	}
	_ = e.StartDream(ctx, "nightly")
}

// StartDream archives the current Router session and spawns an isolated Dream session
// to review recent history, consolidate memory/, and propose mature skills.
func (e *Engine) StartDream(ctx context.Context, reason string) error {
	e.mu.Lock()
	if e.dream != nil || e.handover != nil {
		e.mu.Unlock()
		return fmt.Errorf("Corgi 正在整理记忆或交接会话")
	}
	st := e.settings
	d := &dreamRun{at: time.Now(), reason: reason}
	e.dream = d
	e.mu.Unlock()

	e.addEvent(ctx, model.MainThread, "Corgi 睡一会儿，复盘近期对话与日志、整理记忆（做梦）", "", "info")
	e.notifyChanged()

	go e.launchDreamAsync(d, st)
	return nil
}

func (e *Engine) launchDreamAsync(d *dreamRun, st model.Settings) {
	bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fail := func(err error) {
		e.logger.Warn("dream launch failed", "err", err)
		e.mu.Lock()
		e.dream = nil
		e.mu.Unlock()
		e.addEvent(bg, model.MainThread, "整理记忆未能启动："+err.Error(), "", "error")
		e.notifyChanged()
	}

	// Archive current Router session so writeDreamInput includes it.
	if live, err := e.fleet.Agents(bg); err == nil {
		for _, a := range live {
			if a.Name != nil && *a.Name == model.RouterName && a.Session != nil {
				sid := extractSessionID(a.Session.Kind, a.Session.Value)
				if sid != "" {
					kind := agent.NormalizeKind(st.RouterKind)
					if a.Agent != nil && *a.Agent != "" {
						if nk := agent.NormalizeKind(*a.Agent); nk != "" {
							kind = nk
						}
					}
					eid, _, _ := e.store.NextID(bg, "h")
					_ = e.store.SaveEndedSession(bg, &model.EndedSession{
						ID: eid, Kind: kind, Cwd: e.memory.RouterDir(), Name: model.RouterName,
						Session: sid, EndedAt: time.Now(),
						Title: "Corgi · 做梦前的会话（" + time.Now().Format("01-02 15:04") + "）",
					}, 40)
					e.mu.Lock()
					if e.dream == d {
						d.archivedRef = "h:" + eid
					}
					e.mu.Unlock()
				}
				break
			}
		}
	}

	e.closeWorkspacesByLabel(bg, model.RouterLabel)
	e.closeWorkspacesByLabel(bg, model.DreamLabel)

	if err := e.memory.WriteRouterDocs(e.binPath, st); err != nil {
		e.logger.Warn("write router docs before dream", "err", err)
	}
	if err := e.writeDreamInput(bg); err != nil {
		fail(err)
		return
	}

	kind := agent.NormalizeKind(st.RouterKind)
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		fail(err)
		return
	}
	if !st.AskPermissions {
		_ = ad.PreTrustWorkspace(e.home, e.memory.RouterDir())
	}

	// Prefer the stronger Worker model/effort for nightly judgment when sharing the same agent kind.
	dreamModel, dreamEffort := st.RouterModel, st.RouterEffort
	if st.WorkerModel != "" && agent.NormalizeKind(st.WorkerKind) == kind {
		dreamModel, dreamEffort = st.WorkerModel, st.WorkerEffort
	}
	args := ad.LaunchArgs(dreamModel, dreamEffort, !st.AskPermissions)

	wsID, paneID, err := e.fleet.CreateWorkspace(bg, "", e.memory.RouterDir(), model.DreamLabel)
	if err != nil {
		fail(err)
		return
	}
	if err := e.fleet.StartAgent(bg, paneID, kind, model.DreamName, args); err != nil {
		var he *herdr.Error
		if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			_ = e.fleet.CloseWorkspace(bg, wsID)
			fail(err)
			return
		}
	}
	e.fleet.RememberName(paneID, model.DreamName)

	prompt := strings.NewReplacer(
		"{{DATE}}", journalName(d.at),
		"{{BIN}}", e.binPath,
		"{{BUDGET}}", fmt.Sprint(UpFrontBudgetBytes>>10),
	).Replace(dreamPromptTemplate)

	deadline := time.Now().Add(90 * time.Second)
	for {
		err := e.fleet.Prompt(bg, model.DreamName, prompt)
		var he *herdr.Error
		if err == nil {
			break
		}
		if !errors.As(err, &he) || (he.Code != "agent_not_ready" && he.Code != "agent_not_found") || time.Now().After(deadline) {
			e.closeWorkspacesByLabel(context.Background(), model.DreamLabel)
			fail(err)
			return
		}
		select {
		case <-bg.Done():
			fail(bg.Err())
			return
		case <-time.After(2 * time.Second):
		}
	}

	e.mu.Lock()
	if e.dream == d {
		e.dream.started = true
	}
	e.mu.Unlock()
}

func (e *Engine) driveDream(ctx context.Context, d *dreamRun, live map[string]liveInfo) {
	e.mu.Lock()
	started := d.started
	e.mu.Unlock()
	if !started {
		if time.Since(d.at) > 5*time.Minute {
			e.finishDream(ctx, d, false)
		}
		return
	}
	li, alive := live[model.DreamName]
	if alive && li.status == "blocked" {
		if !e.Settings().AskPermissions {
			e.trustOwnDir(ctx, li.pane)
		}
		e.mu.Lock()
		if d.blockedAt.IsZero() {
			d.blockedAt = time.Now()
		} else if !d.told && time.Since(d.blockedAt) > time.Minute {
			d.told = true
			e.mu.Unlock()
			e.addEvent(ctx, model.MainThread, "Corgi 整理记忆时停在一个确认上，请打开查看", model.DreamName, "blocked")
			e.notifyChanged()
			goto statusCheck
		}
		e.mu.Unlock()
	} else {
		e.mu.Lock()
		d.blockedAt = time.Time{}
		e.mu.Unlock()
	}

statusCheck:
	e.mu.Lock()
	switch {
	case alive && li.status == "working":
		d.worked = true
		d.idleAt = time.Time{}
	case alive && (li.status == "idle" || li.status == "done") && d.idleAt.IsZero():
		d.idleAt = time.Now()
	}
	worked, idleAt := d.worked, d.idleAt
	e.mu.Unlock()

	wait := time.Since(d.at)
	journal := filepath.Join(e.memory.DreamsDir(), journalName(d.at)+".md")
	journalFresh := false
	if fi, err := os.Stat(journal); err == nil && !fi.ModTime().Before(d.at) && fi.Size() > 0 {
		journalFresh = true
	}
	switch {
	case worked && !idleAt.IsZero() && time.Since(idleAt) > 6*time.Second:
		e.finishDream(ctx, d, true)
	case !worked && journalFresh && !idleAt.IsZero() && time.Since(idleAt) > 10*time.Second:
		e.finishDream(ctx, d, true)
	case !alive && worked, wait > 25*time.Minute, !worked && !idleAt.IsZero() && time.Since(idleAt) > 2*time.Minute:
		e.finishDream(ctx, d, worked || journalFresh)
	}
}

func (e *Engine) finishDream(ctx context.Context, d *dreamRun, ok bool) {
	e.closeWorkspacesByLabel(ctx, model.DreamLabel)
	summary, skills, fixes := "", "", ""
	if ok || d.reason == "nightly" {
		// Always advance last_dream on nightly runs so a failed dream cannot spin in a 2-minute loop.
		e.setLastDream(ctx, time.Now())
	}
	if ok {
		journal := filepath.Join(e.memory.DreamsDir(), journalName(d.at)+".md")
		summary = parseDreamSummary(journal)
		skills = parseDreamPrefixLine(journal, dreamSkillsPrefix)
		fixes = parseDreamPrefixLine(journal, dreamFixPrefix)
	}

	e.mu.Lock()
	e.dream = nil
	e.routerLaunchAt = time.Time{}
	e.mu.Unlock()

	e.mu.Lock()
	archivedRef := d.archivedRef
	e.mu.Unlock()
	switch {
	case !ok:
		e.addEvent(ctx, model.MainThread, "本次整理记忆未能完成，Corgi 先醒过来了", archivedRef, "error")
	case summary != "":
		e.addEvent(ctx, model.MainThread, "Corgi 醒了。记忆整理："+summary, archivedRef, "info")
	default:
		e.addEvent(ctx, model.MainThread, "Corgi 醒了，记忆已整理完毕", archivedRef, "info")
	}

	if skills != "" {
		_, _, _, _ = e.Reply(ctx, model.MainThread,
			"整理记忆时发现这些流程已经做熟，可沉淀为 skill 供所有 Agent 复用："+skills+"。要不要做成 skill？",
			"", []string{"做成个人 skill", "放到项目仓库", "先不用"}, false)
	}
	if fixes != "" {
		_, _, _, _ = e.Reply(ctx, model.MainThread,
			"整理记忆时发现已有 skill 存在卡点："+fixes+"。要不要派 Agent 去修？",
			"", []string{"派人去修", "先不用"}, false)
	}
	e.notifyChanged()
	e.ensureRouter(ctx)
}

// writeDreamInput writes dream/input.md summarizing chats and archived Router logs since last dream.
func (e *Engine) writeDreamInput(ctx context.Context) error {
	since := e.LastDream(ctx)
	var b strings.Builder
	sinceStr := "never"
	if !since.IsZero() {
		sinceStr = since.Format("2006-01-02 15:04")
	}
	fmt.Fprintf(&b, "# What happened since the last dream (%s)\n\n## Chat & event log\n\n", sinceStr)

	entries, _, _ := e.store.ListEntries(ctx, "", 500)
	var lines []string
	for _, en := range entries {
		if !since.IsZero() && en.At.Before(since) {
			continue
		}
		who := en.Role
		switch en.Role {
		case "user":
			who = "human"
		case "corgi":
			who = "Corgi"
		}
		lines = append(lines, fmt.Sprintf("- %s [%s] %s: %s",
			en.At.Format("01-02 15:04"), en.Thread, who, oneLineClip(en.Text, 600)))
	}
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n\n## Corgi's own sessions (reasoning & commands)\n")

	ended, _ := e.store.ListEndedSessions(ctx, 40)
	var routerSess []*model.EndedSession
	for _, h := range ended {
		if h.Name == model.RouterName && (since.IsZero() || h.EndedAt.After(since)) {
			routerSess = append(routerSess, h)
		}
	}
	sort.Slice(routerSess, func(i, j int) bool { return routerSess[i].EndedAt.Before(routerSess[j].EndedAt) })

	const budget = 240 << 10
	var parts []string
	for _, h := range routerSess {
		ad, err := e.agents.Get(h.Kind)
		if err != nil {
			continue
		}
		path := ad.LocateTranscript(e.home, h.Cwd, h.Session, h.EndedAt.Add(-24*time.Hour))
		if path == "" {
			continue
		}
		tr := agent.NewTranscriptReader(path, ad, "")
		tr.Poll()
		var p strings.Builder
		fmt.Fprintf(&p, "\n### %s (ended %s)\n\n", h.Title, h.EndedAt.Format("01-02 15:04"))
		for _, m := range tr.Messages() {
			switch m.Kind {
			case "user":
				fmt.Fprintf(&p, "IN: %s\n\n", oneLineClip(m.Text, 1200))
			case "assistant":
				fmt.Fprintf(&p, "CORGI: %s\n\n", oneLineClip(m.Text, 1200))
			case "tool":
				fmt.Fprintf(&p, "TOOL %s: %s → %s\n\n", m.Name, oneLineClip(m.Input, 260), oneLineClip(m.Output, 260))
			}
		}
		parts = append(parts, p.String())
	}

	total, keep := 0, 0
	for i := len(parts) - 1; i >= 0; i-- {
		if total+len(parts[i]) > budget && keep > 0 {
			break
		}
		total += len(parts[i])
		keep++
	}
	if keep < len(parts) {
		fmt.Fprintf(&b, "\n(%d older sessions omitted for budget.)\n", len(parts)-keep)
	}
	for _, p := range parts[len(parts)-keep:] {
		if len(p) > budget {
			p = "…(truncated)…\n" + p[len(p)-budget:]
		}
		b.WriteString(p)
	}
	if len(parts) == 0 {
		b.WriteString("\n(no archived router transcripts found)\n")
	}

	dir := filepath.Join(e.memory.RouterDir(), "dream")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir dream dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "input.md"), []byte(b.String()), 0o600)
}

func journalName(at time.Time) string { return at.Format("2006-01-02-1504") }

func parseDreamPrefixLine(path, prefix string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for i, l := range strings.Split(string(b), "\n") {
		if i > 8 {
			break
		}
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), ">-* "))
		if s, ok := strings.CutPrefix(l, prefix); ok {
			return strings.TrimRight(strings.TrimSpace(s), "。.")
		}
	}
	return ""
}

func parseDreamSummary(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, dreamSkillsPrefix) || strings.HasPrefix(t, dreamFixPrefix) {
			continue
		}
		t = strings.TrimSpace(strings.TrimLeft(t, ">-* "))
		if t != "" {
			return oneLineClip(t, 160)
		}
	}
	return ""
}

const dreamPromptTemplate = `[corgi] Dream. You are Corgi's memory keeper running while Corgi sleeps. Read CORGI.md for context and memory/ layout. Inputs in this directory:
- memory/: Corgi's layered memory.
- dream/input.md: recent conversations and Corgi's archived session transcripts since the last dream.
- dreams/: earlier dream journals, if any.
Run ` + "`{{BIN}} list`" + ` to check which workers and timers are still active.

Consolidate memory/ and keep up-front files (instructions.md, notes/index.md, open tasks) under ~{{BUDGET}} KB combined:
1. Archive stale tasks: move finished, abandoned, or dead-worker tasks from tasks/private/ and tasks/groups/ to tasks/done/ with a closing line.
2. Rewrite instructions.md cleanly: one standing preference/rule per line (≤80 Chinese chars); move procedures/history into notes/ with a pointer ("见 notes/x.md"). Remove recurring schedules already backed by an active timer + notes/ workflow; merge duplicates.
3. Capture cross-session patterns seen more than once (repeated corrections, implicit preferences, recurring mistakes) into instructions.md or notes/, marked "(inferred)".
4. File reusable engineering findings (data paths, working commands, pitfalls, agent/model track records in notes/agents.md) into notes/<topic>.md and keep notes/index.md accurate. Store pointers, not copies.
5. Skills review:
   - If a reusable workflow in notes/ has been used ≥3 times, stabilized, and has no skill yet, list it on a line starting "可做成 skill：" separated by 、.
   - If an existing skill caused workarounds or failures in logs, list it on a line starting "可改进 skill：" as "name（修复点）" separated by 、.
   Place these lines directly under the journal's first summary line (omit if none).
6. Preserve every explicit human instruction unless superseded by a newer one. memory/messages.md is hub-managed (read-only).

Write dreams/{{DATE}}.md:
- Line 1: one Chinese sentence summarizing what you changed (shown to the human).
- Next lines (optional): skill suggestion lines above.
- Remaining (<30 lines): concise log of edits and noticed patterns.

Edit files only, then stop.`

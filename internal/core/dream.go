package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	dreamSkillsPrefix   = "可做成 skill："
	dreamFixPrefix      = "可改进 skill："
	activeDreamKVKey    = "active_dream"
	nightlyDreamSlotKey = "nightly_dream_slot"
)

type activeDreamMarker struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

type nightlyDreamSlot struct {
	Date    string    `json:"date"`    // YYYY-MM-DD of scheduled dream slot
	Outcome string    `json:"outcome"` // ok | skipped | failed
	At      time.Time `json:"at"`
}

type dreamRun struct {
	id, reason, apiErr, lastTool string
	at                           time.Time
	recovered, apiDone, apiOK    bool
	apiErrIsAuth                 bool
	step                         int
}

func (e *Engine) loadActiveDreamMarker(ctx context.Context) {
	if e.store == nil {
		return
	}
	raw, ok, err := e.store.GetKV(ctx, activeDreamKVKey)
	var m activeDreamMarker
	if err != nil || !ok || json.Unmarshal([]byte(raw), &m) != nil || strings.TrimSpace(m.ID) == "" || m.At.IsZero() {
		return
	}
	e.mu.Lock()
	e.dream = &dreamRun{id: m.ID, at: m.At, recovered: true, reason: m.Reason}
	e.mu.Unlock()
	if e.logger != nil {
		e.logger.Info("loaded active dream marker for restart recovery", "id", m.ID, "at", m.At.Format(time.RFC3339))
	}
}

func (e *Engine) saveActiveDreamMarker(ctx context.Context, d *dreamRun) error {
	if e.store == nil || d == nil || d.id == "" {
		return nil
	}
	b, err := json.Marshal(activeDreamMarker{ID: d.id, At: d.at, Reason: d.reason})
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, activeDreamKVKey, string(b))
}

func (e *Engine) recordNightlyDreamSlot(ctx context.Context, slotDate, outcome string, at time.Time) {
	if e.store != nil {
		if b, err := json.Marshal(nightlyDreamSlot{Date: slotDate, Outcome: outcome, At: at}); err == nil {
			_ = e.store.SetKV(ctx, nightlyDreamSlotKey, string(b))
		}
	}
}

func (e *Engine) readNightlyDreamSlot(ctx context.Context) (nightlyDreamSlot, bool) {
	if e.store == nil {
		return nightlyDreamSlot{}, false
	}
	raw, ok, err := e.store.GetKV(ctx, nightlyDreamSlotKey)
	var s nightlyDreamSlot
	if err != nil || !ok || json.Unmarshal([]byte(raw), &s) != nil || s.Date == "" {
		return nightlyDreamSlot{}, false
	}
	return s, true
}

// Dreaming reports whether an async memory-consolidation Dream session is active.
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
	t, _ := time.Parse(time.RFC3339, raw)
	return t
}

func (e *Engine) setLastDream(ctx context.Context, t time.Time) {
	_ = e.store.SetKV(ctx, "last_dream", t.Format(time.RFC3339))
}

func (e *Engine) dreamAtLocked() (h, m int, ok bool) {
	v := orStr(strings.TrimSpace(e.settings.DreamAt), "04:00")
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
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !e.LastDream(ctx).Before(t) || now.After(t.Add(3*time.Hour)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func (e *Engine) hasActivitySince(ctx context.Context, since time.Time) (bool, error) {
	entries, _, err := e.store.ListEntries(ctx, "", 60)
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i]
		if !since.IsZero() && en.At.Before(since) {
			return false, nil
		}
		if en.Role == "user" || en.Role == "corgi" {
			return true, nil
		}
	}
	return false, nil
}

// checkDream finalizes a finished/recovered API Dream or starts the scheduled nightly dream.
func (e *Engine) checkDream(ctx context.Context, hasQueued bool) {
	e.mu.Lock()
	d := e.dream
	if d != nil {
		recovered, apiDone, apiOK := d.recovered, d.apiDone, d.apiOK
		e.mu.Unlock()
		if recovered {
			fi, err := e.memory.StatDreamJournal(journalName(d.at) + ".md")
			e.finishDream(ctx, d, err == nil && !fi.ModTime().Before(d.at) && fi.Size() > 0)
			return
		}
		if apiDone {
			e.finishDream(ctx, d, apiOK)
		}
		return
	}
	busy := e.hubUpdating || e.routerDispatching || e.router.turn != nil || e.settings.Paused || hasQueued
	e.mu.Unlock()

	now := time.Now()
	next := e.NextDream(ctx, now)
	if busy || next.IsZero() || now.Before(next) || now.After(next.Add(3*time.Hour)) {
		return
	}
	active, err := e.hasActivitySince(ctx, e.LastDream(ctx))
	if err != nil {
		e.logger.Warn("dream activity check failed", "err", err)
		return
	}
	if !active {
		e.setLastDream(ctx, now)
		e.recordNightlyDreamSlot(ctx, next.Format("2006-01-02"), "skipped", now)
		return
	}
	_ = e.StartDream(ctx, "nightly")
}

// StartDream starts an in-process async API Dream to consolidate memory/ and propose mature skills.
func (e *Engine) StartDream(ctx context.Context, reason string) error {
	now := time.Now()
	e.mu.Lock()
	if e.dream != nil || e.hubUpdating {
		e.mu.Unlock()
		return fmt.Errorf("Corgi 正在整理记忆或升级中")
	}
	d := &dreamRun{id: now.Format("20060102-150405.000000000"), at: now, reason: reason}
	e.dream = d
	e.mu.Unlock()

	if err := e.saveActiveDreamMarker(ctx, d); err != nil {
		e.mu.Lock()
		if e.dream == d {
			e.dream = nil
		}
		e.mu.Unlock()
		return fmt.Errorf("记录做梦状态失败：%w", err)
	}
	e.addEvent(ctx, model.MainThread, "Corgi 睡一会儿，复盘近期对话与日志、整理记忆（做梦）", "", "info")
	e.notifyChanged()
	go e.runAPIDream(d)
	return nil
}

func (e *Engine) finishDream(ctx context.Context, d *dreamRun, ok bool) {
	now := time.Now()
	kvUpdates := map[string]string{}
	if ok || d.reason == "nightly" {
		watermark := now
		if d.recovered && time.Since(d.at) > 3*time.Hour {
			watermark = d.at
		}
		kvUpdates["last_dream"] = watermark.Format(time.RFC3339)
	}
	if d.reason == "nightly" {
		outcome := "failed"
		if ok {
			outcome = "ok"
		}
		if b, err := json.Marshal(nightlyDreamSlot{Date: d.at.Format("2006-01-02"), Outcome: outcome, At: now}); err == nil {
			kvUpdates[nightlyDreamSlotKey] = string(b)
		}
	}
	if d.id != "" && e.store != nil {
		committed, err := e.store.CommitActiveDreamFinish(ctx, d.id, kvUpdates)
		if err != nil {
			e.logger.Warn("commit dream finish failed, will retry", "id", d.id, "err", err)
			return
		}
		if !committed {
			e.mu.Lock()
			if e.dream == d {
				e.dream = nil
			}
			e.mu.Unlock()
			return
		}
	} else if e.store != nil {
		for k, v := range kvUpdates {
			_ = e.store.SetKV(ctx, k, v)
		}
	}

	summary, skills, fixes := "", "", ""
	if ok && e.memory != nil {
		if raw, err := e.memory.ReadFile("dreams/" + journalName(d.at) + ".md"); err == nil {
			summary, skills, fixes = parseDreamJournalText(string(raw))
		}
	}
	e.mu.Lock()
	if e.dream == d {
		e.dream = nil
	}
	apiErr, apiErrIsAuth := d.apiErr, d.apiErrIsAuth
	stepCount, lastTool, recovered, runCtx := d.step, d.lastTool, d.recovered, e.runCtx
	e.mu.Unlock()

	switch {
	case !ok && apiErr != "":
		e.addEvent(ctx, model.MainThread, "本次整理记忆未能完成（"+apiErr+"），Corgi 先醒过来了", "", "error")
	case !ok:
		e.addEvent(ctx, model.MainThread, "本次整理记忆未能完成，Corgi 先醒过来了", "", "error")
	case summary != "":
		e.addEvent(ctx, model.MainThread, "Corgi 醒了。记忆整理："+summary, "", "info")
	default:
		e.addEvent(ctx, model.MainThread, "Corgi 醒了，记忆已整理完毕", "", "info")
	}

	if !ok && !apiErrIsAuth && (runCtx == nil || runCtx.Err() == nil) && e.store != nil && e.hasConfiguredRouterAPIKey(ctx, e.Settings()) {
		errDesc := orStr(apiErr, "未落盘做梦日志")
		if recovered && apiErr == "" {
			errDesc = "Hub 重启导致后台做梦中断，未完成落盘"
		}
		progress := fmt.Sprintf("第 %d 步", stepCount)
		if lastTool != "" {
			progress += "（最后工具：" + lastTool + "）"
		}
		report := fmt.Sprintf(
			"[做梦异常报告] 后台记忆整理（触发=%s，已运行=%s，进度=%s）未能正常完成。\n异常原因：%s\n"+
				"请作为大脑判断：临时网络波动可设提醒或调用 no_action(reason) 忽略；需主人介入请调用 reply_user 说明。",
			orStr(d.reason, "auto"), max(time.Since(d.at).Round(time.Second), 0), progress, errDesc,
		)
		_, _ = e.Ingest(ctx, "system", model.MainThread, "dream", report, "", model.RouterName, d.id)
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
}

// buildDreamInput builds the in-memory Dream context directly from SQLite.
func (e *Engine) buildDreamInput(ctx context.Context) string {
	since := e.LastDream(ctx)
	sinceStr := "never"
	if !since.IsZero() {
		sinceStr = since.Format("2006-01-02 15:04")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# What happened since the last dream (%s)\n\n## Authoritative Open Work Items (Hub SQLite)\n\n", sinceStr)
	if openWork, err := e.store.ListWorkItems(ctx, false); err != nil {
		fmt.Fprintf(&b, "(error reading work_items: %v)\n", err)
	} else if len(openWork) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, wi := range openWork {
			fmt.Fprintf(&b, "- %s [%s] %s", wi.ID, wi.Thread, wi.Title)
			if wi.NextStep != "" {
				fmt.Fprintf(&b, " | next: %s", wi.NextStep)
			}
			if len(wi.Workers) > 0 {
				fmt.Fprintf(&b, " | workers: %s", strings.Join(wi.Workers, ","))
			}
			if len(wi.Branches) > 0 {
				fmt.Fprintf(&b, " | branches: %s", strings.Join(wi.Branches, ","))
			}
			if len(wi.Artifacts) > 0 {
				fmt.Fprintf(&b, " | delivered_files: %d", len(wi.Artifacts))
			}
			b.WriteByte('\n')
		}
	}
	if pendingActs, err := e.store.ListPendingActions(ctx); err == nil && len(pendingActs) > 0 {
		b.WriteString("\n## Pending User Actions\n\n")
		for _, a := range pendingActs {
			fmt.Fprintf(&b, "- %s (%s) %s\n", a.ID, a.Kind, oneLineClip(a.Title, 120))
		}
	}
	b.WriteString("\n## Chat & event log\n\n")
	entries, _, _ := e.store.ListEntries(ctx, "", 500)
	for _, en := range entries {
		if !since.IsZero() && en.At.Before(since) {
			continue
		}
		who := en.Role
		if en.Role == "user" {
			who = "human"
		} else if en.Role == "corgi" {
			who = "Corgi"
		}
		fmt.Fprintf(&b, "- %s [%s] %s: %s\n", en.At.Format("01-02 15:04"), en.Thread, who, oneLineClip(en.Text, 600))
	}
	return b.String()
}

func journalName(at time.Time) string { return at.Format("2006-01-02-1504") }

func parseDreamJournalText(text string) (summary, skills, fixes string) {
	for i, rawLine := range strings.Split(text, "\n") {
		t := strings.TrimSpace(rawLine)
		stripped := strings.TrimSpace(strings.TrimLeft(t, ">-* "))
		if i <= 8 {
			if s, ok := strings.CutPrefix(stripped, dreamSkillsPrefix); ok && skills == "" {
				skills = strings.TrimRight(strings.TrimSpace(s), "。.")
				continue
			}
			if s, ok := strings.CutPrefix(stripped, dreamFixPrefix); ok && fixes == "" {
				fixes = strings.TrimRight(strings.TrimSpace(s), "。.")
				continue
			}
		}
		if summary == "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, dreamSkillsPrefix) && !strings.HasPrefix(t, dreamFixPrefix) && stripped != "" {
			summary = oneLineClip(stripped, 160)
		}
	}
	return summary, skills, fixes
}

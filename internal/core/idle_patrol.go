package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	kvLastMorningDigestDay = "last_morning_digest_day"
	kvLastIdlePatrolAt     = "last_idle_patrol_at"
	patrolMaxSummaryItems  = 5
	patrolMaxStaleNames    = 10
	minGlobalPatrolGap     = 2 * time.Hour
)

// ParseActiveHours parses specs like "09:00-22:30" or "24h"/"always".
func ParseActiveHours(spec string) (always bool, startMin, endMin int, ok bool) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return false, 9 * 60, 22*60 + 30, true
	}
	low := strings.ToLower(s)
	if low == "24h" || low == "always" || low == "off" {
		return true, 9 * 60, 22*60 + 30, true
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return false, 0, 0, false
	}
	var sh, sm, eh, em int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%d:%d", &sh, &sm); err != nil {
		return false, 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d:%d", &eh, &em); err != nil {
		return false, 0, 0, false
	}
	if sh < 0 || sh > 23 || sm < 0 || sm > 59 || eh < 0 || eh > 23 || em < 0 || em > 59 {
		return false, 0, 0, false
	}
	return false, sh*60 + sm, eh*60 + em, true
}

// NormalizeActiveHours returns a canonical "HH:MM-HH:MM", "24h", or default "09:00-22:30".
func NormalizeActiveHours(spec string) string {
	always, startMin, endMin, ok := ParseActiveHours(spec)
	if !ok {
		return "09:00-22:30"
	}
	if always {
		return "24h"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", startMin/60, startMin%60, endMin/60, endMin%60)
}

// InActiveHours reports whether now falls inside the configured active hours window.
func InActiveHours(spec string, now time.Time) bool {
	always, startMin, endMin, ok := ParseActiveHours(spec)
	if !ok || always {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	if startMin <= endMin {
		return cur >= startMin && cur < endMin
	}
	return cur >= startMin || cur < endMin
}

// TodayMorningStart returns today's morning digest start time in now's timezone.
func TodayMorningStart(spec string, now time.Time) time.Time {
	_, startMin, _, ok := ParseActiveHours(spec)
	if !ok {
		startMin = 9 * 60
	}
	return time.Date(now.Year(), now.Month(), now.Day(), startMin/60, startMin%60, 0, 0, now.Location())
}

// NextMorningStart returns the next upcoming morning start time strictly after now.
func NextMorningStart(spec string, now time.Time) time.Time {
	today := TodayMorningStart(spec, now)
	if now.Before(today) {
		return today
	}
	return today.AddDate(0, 0, 1)
}

// checkIdlePatrol wakes due snoozed actions and runs rate-limited zero-token gated
// morning digest and same-day backoff idle patrol.
func (e *Engine) checkIdlePatrol(ctx context.Context, live map[string]liveInfo, now time.Time) {
	if woken, err := e.store.WakeDueSnoozedActions(ctx, now); err == nil && len(woken) > 0 {
		e.notifyChanged()
	}

	e.mu.Lock()
	if !e.lastPatrolScan.IsZero() && now.Sub(e.lastPatrolScan) < 30*time.Second {
		e.mu.Unlock()
		return
	}
	e.lastPatrolScan = now
	e.mu.Unlock()

	e.runIdlePatrol(ctx, live, now)
}

// runIdlePatrol executes the deterministic zero-token gate for morning digest and same-day backoff patrol.
func (e *Engine) runIdlePatrol(ctx context.Context, live map[string]liveInfo, now time.Time) {
	st := e.Settings()
	if !InActiveHours(st.EffectiveActiveHours(), now) {
		return
	}
	// Never interrupt Router while it is actively working or already handling queued events.
	if li, ok := live[model.RouterName]; ok && li.status == "working" {
		return
	}
	if queued, err := e.store.ListQueuedTriggers(ctx); err == nil {
		for _, t := range queued {
			if len(t.Targets) == 0 || containsStr(t.Targets, model.RouterName) {
				return
			}
		}
	}

	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	pendingActions, _ := e.store.ListPendingActions(ctx)
	pendingByWorker := map[string]bool{}
	for _, a := range pendingActions {
		if a.TargetWorker != "" {
			pendingByWorker[a.TargetWorker] = true
		}
	}

	todayStart := TodayMorningStart(st.EffectiveActiveHours(), now)
	todayKey := now.Format("2006-01-02")

	// 1. Daily Morning Digest (fires at most once per calendar day after activeHours start).
	if st.MorningDigestEnabled() && !now.Before(todayStart) {
		lastDay, _, _ := e.store.GetKV(ctx, kvLastMorningDigestDay)
		if lastDay != todayKey {
			if e.tryEmitMorningDigest(ctx, st, workers, live, pendingActions, todayStart, now) {
				_ = e.store.SetKV(ctx, kvLastMorningDigestDay, todayKey)
				_ = e.store.SetKV(ctx, kvLastIdlePatrolAt, now.UTC().Format(time.RFC3339))
				return
			}
			// Zero-token skip when everything is clean; still mark today done so we don't re-scan morning digest.
			_ = e.store.SetKV(ctx, kvLastMorningDigestDay, todayKey)
		}
	}

	// 2. Same-Day Backoff Idle Patrol (30m -> 4h, max 2 times per idle cycle), guarded by:
	//    (a) no unresolved patrol_digest card already waiting today,
	//    (b) global minimum cooldown >= max(2h, 2*firstDur) across all workers (prevents staggered storms),
	//    (c) no human chat message within the last firstDur (don't nag an actively chatting user).
	firstMin := st.EffectiveIdlePatrolMinutes()
	if firstMin <= 0 {
		return
	}
	for _, a := range pendingActions {
		if a.Kind == model.ActionKindPatrolDigest && !a.CreatedAt.Before(todayStart) {
			return
		}
	}
	firstDur := time.Duration(firstMin) * time.Minute
	globalGap := minGlobalPatrolGap
	if 2*firstDur > globalGap {
		globalGap = 2 * firstDur
	}
	if rawLast, ok, _ := e.store.GetKV(ctx, kvLastIdlePatrolAt); ok && rawLast != "" {
		if lastAt, pErr := time.Parse(time.RFC3339, rawLast); pErr == nil && now.Sub(lastAt) < globalGap {
			return
		}
	}
	if e.recentHumanMessage(ctx, model.MainThread, now, firstDur) {
		return
	}
	secondDur := 4 * time.Hour
	if 4*firstDur > secondDur {
		secondDur = 4 * firstDur
	}
	coalesceDur := firstDur / 2

	type candidate struct {
		w        *model.Worker
		priority int
		idleDur  time.Duration
		attempt  int
	}
	var due []candidate
	var nearDue []candidate
	for _, w := range workers {
		if w.Ended || w.IdleSince == nil {
			continue
		}
		li, alive := live[w.Name]
		if !alive || (li.status != "idle" && li.status != "done") {
			continue
		}
		if pendingByWorker[w.Name] {
			continue
		}
		if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
			continue
		}
		if w.IdlePatrolCount >= 2 {
			continue
		}
		// Skip cross-day workers already covered by a previous day's patrol; morning digest handles them.
		if w.IdlePatrolAt != nil && w.IdlePatrolAt.Before(todayStart) {
			continue
		}
		idleDur := now.Sub(*w.IdleSince)
		isDue := false
		isNear := false
		nextAttempt := w.IdlePatrolCount + 1
		switch w.IdlePatrolCount {
		case 0:
			isDue = idleDur >= firstDur
			isNear = !isDue && coalesceDur > 0 && idleDur >= coalesceDur
		case 1:
			isDue = w.IdlePatrolAt != nil && now.Sub(*w.IdlePatrolAt) >= secondDur
		}
		if !isDue && !isNear {
			continue
		}
		prio := 2
		if w.WorktreeBranch != "" {
			prio = 1
		} else if w.LastReport == "" && w.CreatedBy == "you" {
			prio = 3
		}
		cand := candidate{w: w, priority: prio, idleDur: idleDur, attempt: nextAttempt}
		if isDue {
			due = append(due, cand)
		} else {
			nearDue = append(nearDue, cand)
		}
	}
	if len(due) == 0 {
		return
	}
	// Coalesce siblings that finished a few minutes later into the same single patrol batch.
	due = append(due, nearDue...)

	sort.SliceStable(due, func(i, j int) bool {
		if due[i].priority != due[j].priority {
			return due[i].priority < due[j].priority
		}
		return due[i].idleDur > due[j].idleDur
	})

	// Update patrol counters & global cooldown timestamp so staggered workers never spam back-to-back.
	for _, c := range due {
		t := now
		c.w.IdlePatrolCount = c.attempt
		c.w.IdlePatrolAt = &t
		_ = e.store.UpsertWorker(ctx, c.w)
	}
	_ = e.store.SetKV(ctx, kvLastIdlePatrolAt, now.UTC().Format(time.RFC3339))

	shown := due
	extra := 0
	if len(shown) > patrolMaxSummaryItems {
		extra = len(shown) - patrolMaxSummaryItems
		shown = shown[:patrolMaxSummaryItems]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[corgi idle-patrol] %d idle session(s) reached patrol threshold (zero-token gate passed).\n", len(due))
	b.WriteString("Triage strictly:\n")
	b.WriteString("- If nothing genuinely needs a human decision, or you ALREADY reported these exact waiting items recently with zero new developments, stay COMPLETELY SILENT.\n")
	b.WriteString("- NEVER include already-healthy / no-action status items (e.g. '已部署健康'). NEVER auto-stop manual sessions (created_by=you).\n")
	b.WriteString("- If 2+ independent unrelated items still need attention: send ONE clean bulleted Markdown checklist via plain `reply - <<'EOF'` (NO `--opt`, NO fake mutually-exclusive A/B buttons like '先验收已完成分支' vs '批准B'); put one concise bullet per item naming the exact worker/branch and what decision is needed.\n")
	b.WriteString("- Use `reply --digest --category patrol --worker <name> --opt ...` ONLY when there is ONE single concrete decision (e.g. 1 unmerged branch or 1 specific approval) with clear, unambiguous button labels.\n")
	for i, c := range shown {
		w := c.w
		summary := strings.TrimSpace(w.LastReport)
		if summary == "" && w.PaneID != "" {
			summary = e.lastWorkerSummary(ctx, w)
		}
		origin := orStr(w.CreatedBy, "corgi")
		host := orStr(w.Machine, "hub")
		wt := ""
		if w.WorktreeBranch != "" {
			wt = " · worktree=" + w.WorktreeBranch
		}
		keepTag := ""
		if w.Keep {
			keepTag = " · KEEP"
		}
		fmt.Fprintf(&b, "\n%d. %s (%s · %s · by=%s · idle=%s · attempt=%d/2%s%s)\n",
			i+1, w.Name, w.Kind, host, origin, formatIdleDuration(c.idleDur), c.attempt, wt, keepTag)
		if summary != "" {
			fmt.Fprintf(&b, "   last: %s\n", oneLineClip(summary, 280))
		}
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n(+%d additional lower-priority idle sessions omitted to keep the summary compact)\n", extra)
	}
	_, _ = e.Ingest(ctx, "system", model.MainThread, "corgi", b.String(), "", model.RouterName, "idle-patrol")
}

func (e *Engine) tryEmitMorningDigest(
	ctx context.Context,
	st model.Settings,
	workers []*model.Worker,
	live map[string]liveInfo,
	pendingActions []*model.Action,
	todayStart, now time.Time,
) bool {
	staleDays := st.EffectiveStaleArchiveDays()
	staleDur := time.Duration(staleDays) * 24 * time.Hour

	type digestItem struct {
		priority int
		tag      string
		worker   *model.Worker
		line     string
	}
	var priorityItems []digestItem
	var staleWorkers []*model.Worker

	// 1. Existing unresolved pending actions (including woken snoozed cards).
	for _, a := range pendingActions {
		if a.Kind == model.ActionKindPatrolDigest && a.CreatedAt.Before(todayStart) {
			continue // old digest will be superseded by today's morning digest
		}
		prio := 2
		tag := "pending"
		if a.Kind == model.ActionKindWorkerDialog {
			prio = 0
			tag = "BLOCKED"
		}
		who := a.TargetWorker
		if who == "" {
			who = "main"
		}
		priorityItems = append(priorityItems, digestItem{
			priority: prio,
			tag:      tag,
			line:     fmt.Sprintf("[%s] %s: %s", tag, who, oneLineClip(a.Title, 140)),
		})
	}

	// 2. Carryover idle workers from before today's morning start & stale >= staleDays Corgi sessions.
	for _, w := range workers {
		if w.Ended {
			continue
		}
		li, alive := live[w.Name]
		if !alive {
			continue
		}
		if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
			continue
		}
		if li.status == "blocked" {
			priorityItems = append(priorityItems, digestItem{
				priority: 0,
				tag:      "BLOCKED",
				worker:   w,
				line:     fmt.Sprintf("[BLOCKED] %s (%s · %s) is waiting on a terminal dialog", w.Name, w.Kind, orStr(w.Machine, "hub")),
			})
			continue
		}
		if (li.status != "idle" && li.status != "done") || w.IdleSince == nil {
			continue
		}
		idleDur := now.Sub(*w.IdleSince)
		if w.WorktreeBranch != "" && w.IdleSince.Before(todayStart) {
			priorityItems = append(priorityItems, digestItem{
				priority: 1,
				tag:      "WORKTREE",
				worker:   w,
				line:     fmt.Sprintf("[WORKTREE] %s (%s · branch %s · idle %s) — verify/merge or keep", w.Name, orStr(w.Machine, "hub"), w.WorktreeBranch, formatIdleDuration(idleDur)),
			})
			continue
		}
		isStaleCorgi := staleDays > 0 && w.CreatedBy != "you" && !w.Keep && w.WorktreeBranch == "" && idleDur >= staleDur
		if isStaleCorgi {
			staleWorkers = append(staleWorkers, w)
			continue
		}
		if w.IdleSince.Before(todayStart) && !w.Keep {
			summary := strings.TrimSpace(w.LastReport)
			if summary == "" && w.PaneID != "" {
				summary = e.lastWorkerSummary(ctx, w)
			}
			priorityItems = append(priorityItems, digestItem{
				priority: 2,
				tag:      "CARRYOVER",
				worker:   w,
				line:     fmt.Sprintf("[CARRYOVER] %s (%s · by=%s · idle %s): %s", w.Name, orStr(w.Machine, "hub"), orStr(w.CreatedBy, "corgi"), formatIdleDuration(idleDur), oneLineClip(summary, 180)),
			})
		}
	}

	if len(priorityItems) == 0 && len(staleWorkers) == 0 {
		return false
	}

	sort.SliceStable(priorityItems, func(i, j int) bool { return priorityItems[i].priority < priorityItems[j].priority })

	// Mark patrolled timestamp on included workers so same-day patrol doesn't immediately re-fire.
	for _, it := range priorityItems {
		if it.worker != nil {
			t := now
			it.worker.IdlePatrolAt = &t
			if it.worker.IdlePatrolCount == 0 {
				it.worker.IdlePatrolCount = 1
			}
			_ = e.store.UpsertWorker(ctx, it.worker)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[corgi morning-digest] Daily morning review (%s). Pending/carryover: %d; stale >=%dd Corgi sessions: %d.\n",
		now.Format("2006-01-02 15:04"), len(priorityItems), staleDays, len(staleWorkers))
	b.WriteString("Formatting rules:\n")
	b.WriteString("- Include ONLY genuinely unresolved items needing human action (Top 5 max; omit healthy/finished noise). If nothing truly needs action, stay SILENT.\n")
	b.WriteString("- Always format multiple items as a clean Markdown bullet list (1 short line per item naming exact worker/branch).\n")
	b.WriteString("- If there are 2+ independent unrelated items (and no single batch-archive action), send ONE plain bulleted Markdown summary via `reply - <<'EOF'` WITHOUT `--opt` (NEVER force unrelated items into fake mutually-exclusive A/B buttons).\n")
	b.WriteString("- Use `reply --digest --category morning --opt ...` ONLY when there is a single clear decision or a batch-archive confirmation (pass `--category stale` for pure archival).\n")
	if len(priorityItems) > 0 {
		b.WriteString("\nCarryover & priority items:\n")
		limit := len(priorityItems)
		if limit > patrolMaxSummaryItems {
			limit = patrolMaxSummaryItems
		}
		for i := 0; i < limit; i++ {
			fmt.Fprintf(&b, "%d. %s\n", i+1, priorityItems[i].line)
		}
		if len(priorityItems) > limit {
			fmt.Fprintf(&b, "(+%d more carryover items omitted)\n", len(priorityItems)-limit)
		}
	}
	if len(staleWorkers) > 0 {
		names := make([]string, 0, min(len(staleWorkers), patrolMaxStaleNames))
		for i := 0; i < len(staleWorkers) && i < patrolMaxStaleNames; i++ {
			names = append(names, fmt.Sprintf("%s (%s)", staleWorkers[i].Name, formatIdleDuration(now.Sub(*staleWorkers[i].IdleSince))))
		}
		fmt.Fprintf(&b, "\nStale Corgi sessions (>=%dd inactive, non-keep, lossless --resume if stopped): %s", staleDays, strings.Join(names, ", "))
		if len(staleWorkers) > patrolMaxStaleNames {
			fmt.Fprintf(&b, " (+%d more)", len(staleWorkers)-patrolMaxStaleNames)
		}
		b.WriteString("\nOffer a single batch-archive option and remind the human they can reply with exceptions (e.g. '保留 X，其余归档' -> run `keep X` and `stop` the rest).\n")
	}

	_, _ = e.Ingest(ctx, "system", model.MainThread, "corgi", b.String(), "", model.RouterName, "morning-digest")
	return true
}

// SnoozeAction atomically snoozes a pending action until the specified time (default: next morning activeHours start).
func (e *Engine) SnoozeAction(ctx context.Context, actionID string, until time.Time) (*model.Action, error) {
	if until.IsZero() {
		until = NextMorningStart(e.Settings().EffectiveActiveHours(), time.Now())
	}
	act, err := e.store.SnoozeAction(ctx, actionID, until)
	if err != nil {
		return nil, err
	}
	if act.TargetWorker != "" {
		if wk, gErr := e.store.GetWorker(ctx, act.TargetWorker); gErr == nil && !wk.Ended {
			u := until
			wk.SnoozeUntil = &u
			_ = e.store.UpsertWorker(ctx, wk)
		}
	}
	e.addEventWithAction(ctx, act.Thread, fmt.Sprintf("已暂缓待办，将于 %s 再次提醒", until.Format("01-02 15:04")), act.TargetWorker, "info", act.ID)
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, act)
	sink.OnStateChanged()
	return act, nil
}

// SetWorkerKeep marks or unmarks a worker as permanently exempt from stale-session archival prompts.
func (e *Engine) SetWorkerKeep(ctx context.Context, workerName string, keep bool) (*model.Worker, error) {
	wk, err := e.store.GetWorker(ctx, strings.TrimSpace(workerName))
	if err != nil {
		return nil, err
	}
	wk.Keep = keep
	if err := e.store.UpsertWorker(ctx, wk); err != nil {
		return nil, err
	}
	e.notifyChanged()
	return wk, nil
}

func resetWorkerIdle(w *model.Worker) {
	w.IdleSince = nil
	w.IdlePatrolAt = nil
	w.IdlePatrolCount = 0
	w.SnoozeUntil = nil
}

func formatIdleDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m >= 15 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

func (e *Engine) recentHumanMessage(ctx context.Context, thread string, now time.Time, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	entries, _, err := e.store.ListEntries(ctx, thread, 12)
	if err != nil {
		return false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i]
		if en.Role == "user" {
			return !en.At.IsZero() && now.Sub(en.At) >= 0 && now.Sub(en.At) < window
		}
	}
	return false
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

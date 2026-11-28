package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
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
	wiByID := map[string]*model.WorkItem{}
	if allWIs, wiErr := e.store.ListWorkItems(ctx, true); wiErr == nil {
		for _, wi := range allWIs {
			if wi != nil {
				wiByID[wi.ID] = wi
			}
		}
	}
	pendingActions, _ := e.store.ListPendingActions(ctx)
	pendingByWorker := map[string]bool{}
	pendingByWI := map[string]bool{}
	for _, a := range pendingActions {
		if a.IsDecision() {
			if a.TargetWorker != "" {
				pendingByWorker[a.TargetWorker] = true
			}
			if a.WorkItemID != "" {
				pendingByWI[a.WorkItemID] = true
			}
		}
	}

	todayStart := TodayMorningStart(st.EffectiveActiveHours(), now)
	todayKey := now.Format("2006-01-02")

	// 1. Daily Morning Digest (fires at most once per calendar day after activeHours start).
	if st.MorningDigestEnabled() && !now.Before(todayStart) {
		lastDay, _, _ := e.store.GetKV(ctx, kvLastMorningDigestDay)
		if lastDay != todayKey {
			if e.tryEmitMorningDigest(ctx, st, workers, wiByID, live, pendingActions, todayStart, now) {
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
		wi       *model.WorkItem
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
		var wi *model.WorkItem
		if w.WorkItemID != "" {
			wi = wiByID[w.WorkItemID]
			// Never nag about workers belonging to an already-closed WorkItem.
			if wi != nil && wi.ClosedAt != nil {
				continue
			}
		}
		li, alive := live[w.Name]
		if !alive || (li.status != "idle" && li.status != "done") {
			continue
		}
		if pendingByWorker[w.Name] || (w.WorkItemID != "" && pendingByWI[w.WorkItemID]) {
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
		if w.WorktreeBranch != "" && !isPrototypeOrReadonlyWorkItem(wi) {
			prio = 1
		} else if w.LastReport == "" && w.CreatedBy == "you" {
			prio = 3
		}
		cand := candidate{w: w, wi: wi, priority: prio, idleDur: idleDur, attempt: nextAttempt}
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

	apiMode := e.isAPIRouterMode()
	var b strings.Builder
	fmt.Fprintf(&b, "[corgi idle-patrol] %d idle work item(s)/session(s) reached patrol threshold (zero-token gate passed).\n", len(due))
	b.WriteString("Triage strictly (WorkItem-centered):\n")
	if apiMode {
		b.WriteString("- 所有面向用户的表述必须以「事项名称」为主语，严禁把 wi/agy/codex/分支名等内部代号直接甩给用户（仅在需要用户决定未合入代码去留时附带说明）。\n")
		b.WriteString("- 严禁把清理已关闭事项或无未合入改动的闲置 worktree / 底层会话包装成待拍板卡片。\n")
		b.WriteString("- 若无真正需要用户拍板的新事项（或近期已汇报且无新进展），必须调用 no_action(reason) 结束。\n")
		b.WriteString("- 2 项以上彼此独立的事项用 reply_user(purpose=\"answer\") 发 1 条清晰 Markdown 分条清单（禁加互斥按钮）；仅当存在单一明确拍板点时用 reply_user(purpose=\"attention\", title=\"事项名称 · 决策点\", choices=[...])。\n")
	} else {
		b.WriteString("- Always speak in terms of the human-readable WorkItem title rather than raw internal worker/branch slugs.\n")
		b.WriteString("- If nothing genuinely needs a human decision, or you ALREADY reported these exact waiting items recently with zero new developments, stay COMPLETELY SILENT.\n")
		b.WriteString("- NEVER include already-healthy / no-action status items (e.g. '已部署健康'). NEVER auto-stop manual sessions (created_by=you).\n")
		b.WriteString("- If 2+ independent unrelated items still need attention: send ONE clean bulleted Markdown checklist via plain `reply - <<'EOF'` (NO `--opt`, NO fake mutually-exclusive A/B buttons).\n")
		b.WriteString("- Use `reply --digest --category patrol --worker <name> --opt ...` ONLY when there is ONE single concrete decision with clear, unambiguous button labels.\n")
	}
	for i, c := range shown {
		w := c.w
		summary := strings.TrimSpace(w.LastReport)
		if summary == "" && w.PaneID != "" {
			summary = e.lastWorkerSummary(ctx, w)
		}
		origin := orStr(w.CreatedBy, "corgi")
		host := orStr(w.Machine, "hub")
		wt := ""
		if w.WorktreeBranch != "" && !isPrototypeOrReadonlyWorkItem(c.wi) {
			wt = " · worktree=" + w.WorktreeBranch
		}
		keepTag := ""
		if w.Keep {
			keepTag = " · KEEP"
		}
		if c.wi != nil && strings.TrimSpace(c.wi.Title) != "" {
			fmt.Fprintf(&b, "\n%d. 事项「%s」(%s · 底层会话 %s · %s · %s · by=%s · idle=%s · attempt=%d/2%s%s)\n",
				i+1, oneLineClip(c.wi.Title, 60), c.wi.ID, w.Name, w.Kind, host, origin, formatIdleDuration(c.idleDur), c.attempt, wt, keepTag)
			if strings.TrimSpace(c.wi.NextStep) != "" {
				fmt.Fprintf(&b, "   next: %s\n", oneLineClip(c.wi.NextStep, 220))
			}
		} else {
			fmt.Fprintf(&b, "\n%d. %s (%s · %s · by=%s · idle=%s · attempt=%d/2%s%s)\n",
				i+1, w.Name, w.Kind, host, origin, formatIdleDuration(c.idleDur), c.attempt, wt, keepTag)
		}
		if summary != "" {
			fmt.Fprintf(&b, "   last: %s\n", oneLineClip(summary, 280))
		}
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n(+%d additional lower-priority items omitted to keep the summary compact)\n", extra)
	}
	_, _ = e.Ingest(ctx, "system", model.MainThread, "corgi", b.String(), "", model.RouterName, "idle-patrol")
}

func (e *Engine) tryEmitMorningDigest(
	ctx context.Context,
	st model.Settings,
	workers []*model.Worker,
	wiByID map[string]*model.WorkItem,
	live map[string]liveInfo,
	pendingActions []*model.Action,
	todayStart, now time.Time,
) bool {
	staleDays := st.EffectiveStaleArchiveDays()
	staleDur := time.Duration(staleDays) * 24 * time.Hour
	autoGCEnabled := st.EffectiveWorkerIdleGCMinutes() > 0

	type digestItem struct {
		priority int
		tag      string
		workers  []*model.Worker
		line     string
	}
	var priorityItems []digestItem
	var staleWorkers []*model.Worker

	workerByName := make(map[string]*model.Worker, len(workers))
	workersByWI := map[string][]*model.Worker{}
	for _, w := range workers {
		if w == nil {
			continue
		}
		workerByName[w.Name] = w
		if w.WorkItemID != "" {
			workersByWI[w.WorkItemID] = append(workersByWI[w.WorkItemID], w)
		}
	}
	endedByWI := map[string][]*model.EndedSession{}
	if endedList, err := e.store.ListEndedSessions(ctx, store.EndedSessionRetentionLimit); err == nil {
		for _, es := range endedList {
			if es != nil && es.WorkItemID != "" {
				endedByWI[es.WorkItemID] = append(endedByWI[es.WorkItemID], es)
			}
		}
	}

	resolveActionWI := func(a *model.Action) *model.WorkItem {
		if a == nil {
			return nil
		}
		if a.WorkItemID != "" {
			if wi := wiByID[a.WorkItemID]; wi != nil {
				return wi
			}
		}
		if a.TargetWorker != "" {
			if wk := workerByName[a.TargetWorker]; wk != nil && wk.WorkItemID != "" {
				return wiByID[wk.WorkItemID]
			}
		}
		return nil
	}

	pendingWIIDs := map[string]bool{}
	pendingDialogWorkers := map[string]bool{}

	// 1. Existing unresolved pending actions (including woken snoozed cards), WorkItem-centered.
	for _, a := range pendingActions {
		if a.Kind == model.ActionKindPatrolDigest && a.CreatedAt.Before(todayStart) {
			continue // old digest will be superseded by today's morning digest
		}
		wi := resolveActionWI(a)
		if wi != nil && wi.ClosedAt != nil {
			continue
		}
		prio := 1
		tag := "待拍板"
		if a.Kind == model.ActionKindWorkerDialog {
			prio = 0
			tag = "阻塞待确认"
			if a.TargetWorker != "" {
				pendingDialogWorkers[a.TargetWorker] = true
			}
		}
		if wi != nil {
			pendingWIIDs[wi.ID] = true
			priorityItems = append(priorityItems, digestItem{
				priority: prio,
				tag:      tag,
				line:     fmt.Sprintf("[%s] 事项「%s」(%s): %s", tag, oneLineClip(wi.Title, 60), wi.ID, oneLineClip(a.Title, 140)),
			})
		} else {
			who := orStr(a.TargetWorker, "主对话")
			priorityItems = append(priorityItems, digestItem{
				priority: prio,
				tag:      tag,
				line:     fmt.Sprintf("[%s] %s: %s", tag, who, oneLineClip(a.Title, 140)),
			})
		}
	}

	// 2. Live blocked workers not already covered by a pending dialog action.
	for _, w := range workers {
		if w == nil || w.Ended || pendingDialogWorkers[w.Name] {
			continue
		}
		var wi *model.WorkItem
		if w.WorkItemID != "" {
			wi = wiByID[w.WorkItemID]
			if wi != nil && wi.ClosedAt != nil {
				continue
			}
		}
		li, alive := live[w.Name]
		if !alive || li.status != "blocked" {
			continue
		}
		if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
			continue
		}
		label := w.Name
		if wi != nil && strings.TrimSpace(wi.Title) != "" {
			pendingWIIDs[wi.ID] = true
			label = fmt.Sprintf("事项「%s」(%s)", oneLineClip(wi.Title, 60), wi.ID)
		}
		priorityItems = append(priorityItems, digestItem{
			priority: 0,
			tag:      "阻塞待确认",
			workers:  []*model.Worker{w},
			line:     fmt.Sprintf("[阻塞待确认] %s 正等待终端审批确认", label),
		})
	}

	// 3. Open WorkItems carrying over from before todayStart — works seamlessly even when their
	//    underlying Workers were transparently hibernated overnight by v0.1.76 Worker GC.
	var openWIs []*model.WorkItem
	for _, wi := range wiByID {
		if wi != nil && wi.ClosedAt == nil && orStr(wi.Thread, model.MainThread) == model.MainThread {
			openWIs = append(openWIs, wi)
		}
	}
	sort.SliceStable(openWIs, func(i, j int) bool {
		return openWIs[i].UpdatedAt.After(openWIs[j].UpdatedAt)
	})
	for _, wi := range openWIs {
		if pendingWIIDs[wi.ID] {
			continue
		}
		attached := workersByWI[wi.ID]
		var (
			hasRunning     bool
			hasSnoozed     bool
			hasLiveCarry   bool
			liveIdleList   []*model.Worker
			implBranch     string
			latestSummary  string
			latestReportAt time.Time
		)
		for _, w := range attached {
			if w == nil {
				continue
			}
			if !w.Ended {
				if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
					hasSnoozed = true
				}
				if li, alive := live[w.Name]; alive {
					if li.status == "working" || li.status == "blocked" {
						hasRunning = true
					}
					if (li.status == "idle" || li.status == "done") && w.IdleSince != nil {
						liveIdleList = append(liveIdleList, w)
						if w.IdleSince.Before(todayStart) {
							hasLiveCarry = true
						}
					}
				}
			}
			if strings.EqualFold(wi.Scope, "implement") && implBranch == "" && w.WorktreeBranch != "" {
				implBranch = w.WorktreeBranch
			}
			if rep := strings.TrimSpace(w.LastReport); rep != "" && (latestSummary == "" || w.ReportAt.After(latestReportAt)) {
				latestSummary = rep
				latestReportAt = w.ReportAt
			}
		}
		if implBranch == "" && strings.EqualFold(wi.Scope, "implement") {
			for _, es := range endedByWI[wi.ID] {
				if es.WorktreeBranch != "" {
					implBranch = es.WorktreeBranch
					break
				}
			}
		}
		if hasRunning || hasSnoozed {
			continue
		}
		carriedOver := wi.UpdatedAt.Before(todayStart) || hasLiveCarry
		if !carriedOver {
			continue
		}
		detail := strings.TrimSpace(wi.NextStep)
		if detail == "" {
			detail = oneLineClip(latestSummary, 160)
		}
		if detail == "" {
			detail = "尚无后续安排，待确认是否继续推进或关闭"
		}
		scopeLabel := orStr(wi.Scope, "待定")
		branchNote := ""
		if implBranch != "" {
			branchNote = fmt.Sprintf("（含待核验代码分支 %s）", implBranch)
		}
		prio := 2
		if implBranch != "" {
			prio = 1
		}
		priorityItems = append(priorityItems, digestItem{
			priority: prio,
			tag:      "开放事项",
			workers:  liveIdleList,
			line:     fmt.Sprintf("[开放事项] 「%s」(%s · 阶段=%s)%s — %s", oneLineClip(wi.Title, 60), wi.ID, scopeLabel, branchNote, oneLineClip(detail, 180)),
		})
	}

	// 4. Unbound live carryover workers (WorkItemID == "" only; closed-WorkItem workers are strictly excluded).
	for _, w := range workers {
		if w == nil || w.Ended || w.WorkItemID != "" {
			continue
		}
		li, alive := live[w.Name]
		if !alive {
			continue
		}
		if w.SnoozeUntil != nil && w.SnoozeUntil.After(now) {
			continue
		}
		if (li.status != "idle" && li.status != "done") || w.IdleSince == nil {
			continue
		}
		idleDur := now.Sub(*w.IdleSince)
		if w.WorktreeBranch != "" && w.IdleSince.Before(todayStart) {
			priorityItems = append(priorityItems, digestItem{
				priority: 2,
				tag:      "未归属分支",
				workers:  []*model.Worker{w},
				line:     fmt.Sprintf("[未归属代码分支] %s (%s · branch %s · idle %s) — 若含未合入改动请核验或归属事项", w.Name, orStr(w.Machine, "hub"), w.WorktreeBranch, formatIdleDuration(idleDur)),
			})
			continue
		}
		// Only surface stale Corgi session archival prompts when automatic Worker GC is disabled (-1).
		isStaleCorgi := !autoGCEnabled && staleDays > 0 && w.CreatedBy != "you" && !w.Keep && w.WorktreeBranch == "" && idleDur >= staleDur
		if isStaleCorgi {
			staleWorkers = append(staleWorkers, w)
			continue
		}
		if w.IdleSince.Before(todayStart) && !w.Keep {
			summary := strings.TrimSpace(w.LastReport)
			if summary == "" && w.PaneID != "" {
				summary = e.lastWorkerSummary(ctx, w)
			}
			if summary == "" && w.CreatedBy == "you" {
				continue
			}
			priorityItems = append(priorityItems, digestItem{
				priority: 3,
				tag:      "未归属遗留",
				workers:  []*model.Worker{w},
				line:     fmt.Sprintf("[未归属遗留会话] %s (%s · by=%s · idle %s): %s", w.Name, orStr(w.Machine, "hub"), orStr(w.CreatedBy, "corgi"), formatIdleDuration(idleDur), oneLineClip(summary, 180)),
			})
		}
	}

	if len(priorityItems) == 0 && len(staleWorkers) == 0 {
		return false
	}

	sort.SliceStable(priorityItems, func(i, j int) bool { return priorityItems[i].priority < priorityItems[j].priority })

	// Mark patrolled timestamp on included live workers so same-day patrol doesn't immediately re-fire.
	for _, it := range priorityItems {
		for _, w := range it.workers {
			if w != nil && !w.Ended {
				t := now
				w.IdlePatrolAt = &t
				if w.IdlePatrolCount == 0 {
					w.IdlePatrolCount = 1
				}
				_ = e.store.UpsertWorker(ctx, w)
			}
		}
	}

	apiMode := e.isAPIRouterMode()
	var b strings.Builder
	fmt.Fprintf(&b, "[corgi morning-digest] Daily morning review (%s). Open/pending items: %d; stale >=%dd Corgi sessions: %d.\n",
		now.Format("2006-01-02 15:04"), len(priorityItems), staleDays, len(staleWorkers))
	b.WriteString("Formatting rules (WorkItem-centered):\n")
	if apiMode {
		b.WriteString("- 所有面向用户的标题与正文必须以「事项名称」为主语，严禁把 wi/agy/codex/分支名等内部代号当作主语甩给用户。\n")
		b.WriteString("- 严禁把清理已关闭事项或无未合入改动的闲置 worktree / 底层会话包装成待拍板卡片（底层进程休眠由系统自动 GC 处理）。\n")
		b.WriteString("- 仅汇报确实需要用户知晓或拍板的未结事项（最多 5 条）；若无需要打扰用户的事项，必须调用 no_action(reason) 结束。\n")
		b.WriteString("- 2 项以上彼此独立的事项用 reply_user(purpose=\"answer\") 发 1 条清晰 Markdown 分条清单（每条写清「事项名称 + 当前进展与需决策点」，禁加互斥按钮）。\n")
		b.WriteString("- 仅当存在单一明确拍板点时，才用 reply_user(purpose=\"attention\", title=\"事项名称 · 决策点\", choices=[...])。\n")
	} else {
		b.WriteString("- Always frame every item around its human-readable WorkItem title, not raw internal worker/branch slugs.\n")
		b.WriteString("- Include ONLY genuinely unresolved items needing human action (Top 5 max; omit healthy/finished noise). If nothing truly needs action, stay SILENT.\n")
		b.WriteString("- If there are 2+ independent unrelated items (and no single batch-archive action), send ONE plain bulleted Markdown summary via `reply - <<'EOF'` WITHOUT `--opt`.\n")
		b.WriteString("- Use `reply --digest --category morning --opt ...` ONLY when there is a single clear decision or a batch-archive confirmation.\n")
	}
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
		if apiMode {
			fmt.Fprintf(&b, "\nStale Corgi sessions (>=%dd inactive, auto-GC disabled, lossless resume via dispatch_work): %s", staleDays, strings.Join(names, ", "))
		} else {
			fmt.Fprintf(&b, "\nStale Corgi sessions (>=%dd inactive, auto-GC disabled, lossless --resume if stopped): %s", staleDays, strings.Join(names, ", "))
		}
		if len(staleWorkers) > patrolMaxStaleNames {
			fmt.Fprintf(&b, " (+%d more)", len(staleWorkers)-patrolMaxStaleNames)
		}
		if apiMode {
			b.WriteString("\nIf the human prefers manual archival, offer a concise batch-archive option via reply_user(purpose=\"attention\").\n")
		} else {
			b.WriteString("\nOffer a single batch-archive option and remind the human they can reply with exceptions (e.g. '保留 X，其余归档' -> run `keep X` and `stop` the rest).\n")
		}
	}

	_, _ = e.Ingest(ctx, "system", model.MainThread, "corgi", b.String(), "", model.RouterName, "morning-digest")
	return true
}

// SnoozeAction atomically snoozes a pending action until the specified time (default: next morning activeHours start).
func (e *Engine) SnoozeAction(ctx context.Context, actionID string, until time.Time) (*model.Action, error) {
	return e.SnoozeActionBy(ctx, actionID, "", until)
}

// SnoozeActionBy atomically snoozes a pending action and records the originating channel.
func (e *Engine) SnoozeActionBy(ctx context.Context, actionID, channel string, until time.Time) (*model.Action, error) {
	if until.IsZero() {
		until = NextMorningStart(e.Settings().EffectiveActiveHours(), time.Now())
	}
	act, err := e.store.SnoozeActionBy(ctx, actionID, channel, until)
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

// SetWorkerKeep marks or unmarks a worker as permanently exempt from stale-session archival and GC hibernation.
func (e *Engine) SetWorkerKeep(ctx context.Context, workerName string, keep bool) (*model.Worker, error) {
	name := strings.TrimSpace(workerName)
	e.waitWorkerGCIdle(ctx, name, 3*time.Second)
	if !e.beginWorkerOp(name) {
		return nil, fmt.Errorf("worker %q is currently hibernating; retry shortly", name)
	}
	defer e.endWorkerOp(name)

	wk, err := e.store.GetWorker(ctx, name)
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

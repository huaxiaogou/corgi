package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

const maxInjectedWorkItems = 12

// DeriveWorkItemTitle returns a clean human-readable title for a newly created WorkItem along with
// isDraft=true when the title was heuristically cleaned from raw prompt text rather than distilled
// via an explicit --title from Router/human.
func DeriveWorkItemTitle(explicitTitle, task, origUserReq string) (title string, isDraft bool) {
	if t := strings.TrimSpace(explicitTitle); t != "" {
		return oneLineClip(t, 40), false
	}
	if cleaned := cleanWorkItemDraftTitle(task); cleaned != "" {
		return cleaned, true
	}
	if cleaned := cleanWorkItemDraftTitle(origUserReq); cleaned != "" {
		return cleaned, true
	}
	fallback := oneLineClip(strings.TrimSpace(task), 28)
	if fallback == "" {
		fallback = oneLineClip(strings.TrimSpace(origUserReq), 28)
	}
	return fallback, true
}

// cleanWorkItemDraftTitle strips common dispatch boilerplate prefixes ("新任务（用户原话）：", "目标：",
// "任务：", inline paths, attachment trailers) and extracts the core first sentence as a readable
// placeholder until Router LLM refines it.
func cleanWorkItemDraftTitle(raw string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if s == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(s, "项目会话 · "); ok {
		rest = strings.TrimRight(strings.TrimSpace(rest), "/")
		if idx := strings.LastIndex(rest, "/"); idx >= 0 && idx+1 < len(rest) {
			return "项目会话 · " + rest[idx+1:]
		}
		return "项目会话 · " + rest
	}
	// Strip leading boilerplate wrappers repeatedly so combined headers like
	// "新任务（用户原话）：‘画个新的拍板卡原型’" yield the core request cleanly.
	prefixes := []string{
		"新任务（用户原话）：",
		"新任务(用户原话):",
		"新任务（用户原话）:",
		"新任务(用户原话)：",
		"新任务：",
		"新任务:",
		"背景（用户原话）：",
		"背景(用户原话):",
		"用户原话：",
		"用户原话:",
		"用户请求：",
		"用户请求:",
		"完成用户请求",
		"任务：只读分析",
		"任务：只读排查",
		"任务：",
		"任务:",
		"目标：",
		"目标:",
		"排查并修复截图所示",
		"只读排查",
		"请帮我",
		"请把",
		"请",
		"为用户所说的",
	}
	for changed := true; changed; {
		changed = false
		for _, p := range prefixes {
			if trimmed, ok := strings.CutPrefix(s, p); ok {
				s = strings.TrimSpace(trimmed)
				changed = true
			}
		}
	}
	// If the remaining text starts with a quoted user utterance, extract the quote body.
	for _, pair := range [][2]string{{"“", "”"}, {"‘", "’"}, {"『", "』"}, {"「", "」"}, {`"`, `"`}, {`'`, `'`}} {
		if after, ok := strings.CutPrefix(s, pair[0]); ok {
			if end := strings.Index(after, pair[1]); end > 0 {
				if q := strings.TrimSpace(after[:end]); q != "" {
					s = q
					break
				}
			}
		}
	}
	if idx := strings.Index(s, "目标："); idx >= 0 && idx < 90 {
		s = strings.TrimSpace(s[idx+len("目标："):])
	}
	for _, marker := range []string{"附件：", "附件:", "，附件", ", 附件"} {
		if idx := strings.Index(s, marker); idx > 0 {
			s = strings.TrimSpace(s[:idx])
		}
	}
	for _, sep := range []string{"。", "；", "\n", "（", "("} {
		if idx := strings.Index(s, sep); idx > 0 {
			s = strings.TrimSpace(s[:idx])
		}
	}
	words := strings.Fields(s)
	for i, w := range words {
		if strings.HasPrefix(w, "/") && strings.Count(w, "/") >= 2 {
			clean := strings.TrimRight(w, ".,;:，。；：")
			if idx := strings.LastIndex(clean, "/"); idx >= 0 && idx+1 < len(clean) {
				words[i] = clean[idx+1:]
			}
		}
	}
	s = strings.NewReplacer("『", "", "』", "", "「", "", "」", "", "“", "", "”", "", "‘", "", "’", "").Replace(strings.Join(words, " "))
	return oneLineClip(strings.TrimSpace(s), 28)
}

// formatWorkerWorkItemContext builds a compact single-line WorkItem status hint appended onto
// Worker reports and turn-done notifications so Router immediately sees the exact matter title
// and next_step at delivery time and can dynamically refine them as the matter evolves.
func (e *Engine) formatWorkerWorkItemContext(ctx context.Context, wk *model.Worker) string {
	if wk == nil || strings.TrimSpace(wk.WorkItemID) == "" || e.store == nil {
		return ""
	}
	wi, err := e.store.GetWorkItem(ctx, wk.WorkItemID)
	if err != nil || wi == nil || wi.ClosedAt != nil {
		return ""
	}
	state := e.store.GetWorkItemTitleState(ctx, wi.ID)
	var tag string
	switch state {
	case store.WorkItemTitleDraft:
		tag = fmt.Sprintf(" (draft — 请顺手调用 manage_work(action=\"update_work\", target=%q, title=\"...\", next_step=\"...\") 凝练8-18字标题并写明下一步)", wi.ID)
	case store.WorkItemTitleLocked:
		tag = " (user-locked title)"
	default:
		tag = fmt.Sprintf(" (阶段演进时可顺手调用 manage_work(action=\"update_work\" 或 \"close_work\", target=%q))", wi.ID)
	}
	next := oneLineClip(wi.NextStep, 80)
	if next == "" {
		next = "(空)"
	}
	return fmt.Sprintf("\n[work_item: %s · title=%q%s · next=%q]", wi.ID, oneLineClip(wi.Title, 48), tag, next)
}

// ListWorkItems returns open (or all, when includeClosed is true) durable work items.
func (e *Engine) ListWorkItems(ctx context.Context, includeClosed bool) ([]*model.WorkItem, error) {
	return e.store.ListWorkItems(ctx, includeClosed)
}

// ListClosedWorkItems returns paginated archived work items with optional thread and substring filter.
func (e *Engine) ListClosedWorkItems(ctx context.Context, thread, query string, limit, offset int) ([]*model.WorkItem, int, error) {
	return e.store.ListClosedWorkItems(ctx, thread, query, limit, offset)
}

// ClearWorkItemNextStep clears next_step on an open work item and notifies subscribers.
func (e *Engine) ClearWorkItemNextStep(ctx context.Context, id string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("work item id is required")
	}
	wi, err := e.store.ClearWorkItemNextStep(ctx, id)
	if err != nil {
		return nil, err
	}
	e.notifyChanged()
	return wi, nil
}

// ReopenWorkItem restores a closed work item back to open status and notifies subscribers.
func (e *Engine) ReopenWorkItem(ctx context.Context, id string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("work item id is required")
	}
	wi, err := e.store.ReopenWorkItem(ctx, id)
	if err != nil {
		return nil, err
	}
	e.notifyChanged()
	return wi, nil
}

// SetWorkItem creates a new work item (when id is "" or "new") or updates an existing open item.
func (e *Engine) SetWorkItem(ctx context.Context, id, thread, title, nextStep, workerName, branch string) (*model.WorkItem, error) {
	return e.SetWorkItemWithOptions(ctx, id, thread, title, nextStep, workerName, branch, false)
}

// SetWorkItemWithOptions updates or creates a work item while respecting human-locked titles:
// when userEditedTitle is true (Web manual rename), the title is locked against future automated
// Router/Dream overwrites; otherwise Router LLM is free to refine draft or evolving titles.
func (e *Engine) SetWorkItemWithOptions(ctx context.Context, id, thread, title, nextStep, workerName, branch string, userEditedTitle bool) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	nextStep = strings.TrimSpace(nextStep)
	workerName = strings.TrimSpace(workerName)
	branch = strings.TrimSpace(branch)
	if id == "" || strings.EqualFold(id, "new") {
		if title == "" {
			return nil, fmt.Errorf("title is required when creating a new work item")
		}
		newID, _, err := e.store.NextID(ctx, "wi")
		if err != nil {
			return nil, err
		}
		wi := &model.WorkItem{
			ID:       newID,
			Thread:   orStr(strings.TrimSpace(thread), model.MainThread),
			Title:    title,
			NextStep: nextStep,
		}
		if workerName != "" {
			wi.Workers = []string{workerName}
		}
		if branch != "" {
			wi.Branches = []string{branch}
		}
		if err := e.store.CreateWorkItem(ctx, wi); err != nil {
			return nil, err
		}
		if userEditedTitle {
			_ = e.store.SetWorkItemTitleState(ctx, wi.ID, store.WorkItemTitleLocked)
		}
		e.notifyChanged()
		return wi, nil
	}
	effectiveTitle := title
	if effectiveTitle != "" && !userEditedTitle && e.store.GetWorkItemTitleState(ctx, id) == store.WorkItemTitleLocked {
		effectiveTitle = ""
	}
	wi, err := e.store.UpdateWorkItemFields(ctx, id, effectiveTitle, nextStep, workerName, branch)
	if err != nil {
		return nil, err
	}
	if effectiveTitle != "" {
		if userEditedTitle {
			_ = e.store.SetWorkItemTitleState(ctx, wi.ID, store.WorkItemTitleLocked)
		} else {
			_ = e.store.SetWorkItemTitleState(ctx, wi.ID, "")
		}
	}
	e.notifyChanged()
	return wi, nil
}

// UnbindWorkerFromWorkItem detaches a mislinked worker from a work item without deleting history
// or stopping the live worker pane.
func (e *Engine) UnbindWorkerFromWorkItem(ctx context.Context, id, workerName string) (*model.WorkItem, error) {
	wi, err := e.store.UnbindWorkerFromWorkItem(ctx, id, workerName)
	if err != nil {
		return nil, err
	}
	e.notifyChanged()
	return wi, nil
}

// workItemTitleFor resolves the human-facing WorkItem title from an explicit workItemID
// or via workerName's bound WorkItemID. Returns "" when unbound.
func (e *Engine) workItemTitleFor(ctx context.Context, workItemID, workerName string) string {
	if e.store == nil {
		return ""
	}
	wid := strings.TrimSpace(workItemID)
	if wid == "" && strings.TrimSpace(workerName) != "" {
		if wk, err := e.store.GetWorker(ctx, strings.TrimSpace(workerName)); err == nil && wk != nil {
			wid = strings.TrimSpace(wk.WorkItemID)
		}
	}
	if wid == "" {
		return ""
	}
	if wi, err := e.store.GetWorkItem(ctx, wid); err == nil && wi != nil {
		return strings.TrimSpace(wi.Title)
	}
	return ""
}

// formatWorkItemSubject returns "「Title」" when a WorkItem title is available,
// falling back cleanly to fallbackLabel.
func (e *Engine) formatWorkItemSubject(ctx context.Context, workItemID, workerName, fallbackLabel string) string {
	if title := e.workItemTitleFor(ctx, workItemID, workerName); title != "" {
		return "「" + oneLineClip(title, 36) + "」"
	}
	return strings.TrimSpace(fallbackLabel)
}

// CloseWorkItem closes an open work item with an explicit closing note, expires its owned actions,
// and asynchronously cleans up any clean, already-merged worktrees left behind by already-ended workers.
func (e *Engine) CloseWorkItem(ctx context.Context, id, note string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	note = strings.TrimSpace(note)
	if id == "" {
		return nil, fmt.Errorf("work item id is required")
	}
	if note == "" {
		return nil, fmt.Errorf("closing reason is required")
	}
	wi, expired, err := e.store.CloseWorkItemWithActions(ctx, id, note)
	if err != nil {
		return nil, err
	}
	if wi != nil {
		e.ClearWeComFocusForWorkItem(ctx, wi.ID, wi.Workers)
		e.scheduleClosedWorkItemWorktreeCleanup(ctx, wi.ID)
	}
	if len(expired) > 0 {
		e.mu.Lock()
		sink := e.sink
		e.mu.Unlock()
		for _, act := range expired {
			sink.OnActionResolved(ctx, act)
		}
	}
	e.notifyChanged()
	return wi, nil
}

func (e *Engine) scheduleClosedWorkItemWorktreeCleanup(ctx context.Context, workItemID string) {
	if e.store == nil || strings.TrimSpace(workItemID) == "" {
		return
	}
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	hasTarget := false
	for _, wk := range workers {
		if wk != nil && wk.Ended && wk.WorkItemID == workItemID &&
			strings.TrimSpace(wk.WorktreePath) != "" && strings.TrimSpace(wk.RepoRoot) != "" {
			hasTarget = true
			break
		}
	}
	if !hasTarget {
		return
	}
	e.mu.Lock()
	parent := e.runCtx
	e.mu.Unlock()
	if parent == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		e.cleanupClosedWorkItemWorktrees(cctx, workItemID)
		return
	}
	go func() {
		cctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		e.cleanupClosedWorkItemWorktrees(cctx, workItemID)
	}()
}

// cleanupClosedWorkItemWorktrees safely removes ONLY clean, zero-unmerged-commit git worktrees
// belonging to already-ended workers of a closed WorkItem (never touching live workers, dirty
// trees, or branches with unmerged commits).
func (e *Engine) cleanupClosedWorkItemWorktrees(ctx context.Context, workItemID string) {
	if e.store == nil || strings.TrimSpace(workItemID) == "" || ctx.Err() != nil {
		return
	}
	workers, err := e.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	endedList, _ := e.store.ListEndedSessions(ctx, store.EndedSessionRetentionLimit)
	endedByName := map[string]*model.EndedSession{}
	for _, es := range endedList {
		if es != nil && es.Name != "" {
			if _, exists := endedByName[es.Name]; !exists {
				endedByName[es.Name] = es
			}
		}
	}

	for _, wk := range workers {
		if ctx.Err() != nil {
			return
		}
		if wk == nil || !wk.Ended || wk.WorkItemID != workItemID {
			continue
		}
		if strings.TrimSpace(wk.WorktreePath) == "" || strings.TrimSpace(wk.RepoRoot) == "" {
			continue
		}
		clRes, clErr := e.cleanupWorktreeOnMachine(ctx, wk.Machine, wk.RepoRoot, wk.WorktreePath, wk.WorktreeBranch, wk.WorktreeBase)
		if clErr != nil {
			e.logger.Warn("closed work item worktree cleanup skipped",
				"work_item", workItemID, "worker", wk.Name, "err", clErr)
			continue
		}
		if !clRes.RemovedDir && !clRes.DeletedBranch {
			continue
		}
		if clRes.RemovedDir {
			wk.WorktreePath = ""
			wk.Cwd = wk.RepoRoot
		}
		if clRes.DeletedBranch {
			wk.WorktreeBranch = ""
		}
		_ = e.store.UpsertWorker(ctx, wk)
		if es := endedByName[wk.Name]; es != nil {
			if clRes.RemovedDir {
				es.WorktreePath = ""
				es.Cwd = wk.RepoRoot
			}
			if clRes.DeletedBranch {
				es.WorktreeBranch = ""
			}
			_ = e.store.SaveEndedSession(ctx, es, store.EndedSessionRetentionLimit)
		}
		e.logger.Info("cleaned closed work item worktree",
			"work_item", workItemID, "worker", wk.Name, "note", clRes.Note)
	}
}

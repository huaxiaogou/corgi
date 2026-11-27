package core

import (
	"context"
	"fmt"
	"strings"

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
	bin := orStr(strings.TrimSpace(e.binPath), "corgi")
	var tag string
	switch state {
	case store.WorkItemTitleDraft:
		tag = fmt.Sprintf(" (draft — 请顺手用 `%s task set %s --title \"...\" --next \"...\"` 凝练8-18字标题并写明下一步)", bin, wi.ID)
	case store.WorkItemTitleLocked:
		tag = " (user-locked title)"
	default:
		tag = fmt.Sprintf(" (阶段演进时可顺手 `%s task set %s --title/--next`)", bin, wi.ID)
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

// CloseWorkItem closes an open work item with an explicit closing note and expires its owned actions.
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

// formatOpenWorkItemsSummary formats a compact Hub ledger summary injected on normal Router turns
// so internal CLI compactions can never erase visibility of active user work.
func formatOpenWorkItemsSummary(items []*model.WorkItem, binPath string) string {
	if len(items) == 0 {
		return ""
	}
	bin := strings.TrimSpace(binPath)
	if bin == "" {
		bin = "corgi"
	}
	total := len(items)
	shown := items
	var b strings.Builder
	if total > maxInjectedWorkItems {
		shown = items[:maxInjectedWorkItems]
		fmt.Fprintf(&b, "\n[open work items: showing %d of %d — run `%s list` for complete list]\n", len(shown), total, bin)
	} else {
		fmt.Fprintf(&b, "\n[open work items: %d]\n", total)
	}
	for _, wi := range shown {
		fmt.Fprintf(&b, "- %s · %s", wi.ID, oneLineClip(wi.Title, 72))
		if wi.NextStep != "" {
			fmt.Fprintf(&b, " · next: %s", oneLineClip(wi.NextStep, 80))
		}
		if len(wi.Workers) > 0 {
			fmt.Fprintf(&b, " [workers: %s]", strings.Join(wi.Workers, ","))
		}
		if len(wi.Branches) > 0 {
			fmt.Fprintf(&b, " [branches: %s]", strings.Join(wi.Branches, ","))
		}
		if len(wi.Artifacts) > 0 {
			fmt.Fprintf(&b, " [delivered: %d]", len(wi.Artifacts))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

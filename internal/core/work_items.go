package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const maxInjectedWorkItems = 12

// ListWorkItems returns open (or all, when includeClosed is true) durable work items.
func (e *Engine) ListWorkItems(ctx context.Context, includeClosed bool) ([]*model.WorkItem, error) {
	return e.store.ListWorkItems(ctx, includeClosed)
}

// SetWorkItem creates a new work item (when id is "" or "new") or updates an existing open item.
func (e *Engine) SetWorkItem(ctx context.Context, id, thread, title, nextStep, workerName, branch string) (*model.WorkItem, error) {
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
		e.notifyChanged()
		return wi, nil
	}
	wi, err := e.store.UpdateWorkItemFields(ctx, id, title, nextStep, workerName, branch)
	if err != nil {
		return nil, err
	}
	e.notifyChanged()
	return wi, nil
}

// CloseWorkItem closes an open work item with an explicit closing note.
func (e *Engine) CloseWorkItem(ctx context.Context, id, note string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	note = strings.TrimSpace(note)
	if id == "" {
		return nil, fmt.Errorf("work item id is required")
	}
	if note == "" {
		return nil, fmt.Errorf("closing reason is required")
	}
	wi, err := e.store.CloseWorkItem(ctx, id, note)
	if err != nil {
		return nil, err
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

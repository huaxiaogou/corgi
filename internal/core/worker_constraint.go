package core

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	// MaxWorkerConstraintRunes caps a Worker's immutable constraint to one clear line.
	// Exceeding the limit returns an explicit error instead of silently truncating prohibitions.
	MaxWorkerConstraintRunes = 200
	// DirectConstraintRemindInterval throttles repeating the 1-line constraint header on
	// rapid direct Web/WeCom follow-ups to the same pane.
	DirectConstraintRemindInterval = 30 * time.Minute
)

// NormalizeConstraint collapses whitespace into a single clean line and validates length.
func NormalizeConstraint(raw string) (string, error) {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if s == "" {
		return "", nil
	}
	if n := utf8.RuneCountInString(s); n > MaxWorkerConstraintRunes {
		return "", fmt.Errorf("constraint exceeds %d chars (%d chars); keep prohibitions concise instead of truncating", MaxWorkerConstraintRunes, n)
	}
	return s, nil
}

// FormatConstraintHeader renders the single-line header placed at the top of Worker prompts.
func FormatConstraintHeader(constraint string) string {
	c := strings.TrimSpace(constraint)
	if c == "" {
		return ""
	}
	return "【本会话红线】" + c
}

// findWorkerByPaneOrName looks up an active (non-ended) Worker matching paneID or name.
func (e *Engine) findWorkerByPaneOrName(ctx context.Context, paneID string) *model.Worker {
	if e.store == nil || strings.TrimSpace(paneID) == "" {
		return nil
	}
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if w != nil && !w.Ended && (w.PaneID == paneID || w.Name == paneID) {
				return w
			}
		}
	}
	return nil
}

// decorateDirectPrompt prepends the Worker's immutable constraint line when sending the first
// direct prompt to paneID (or once every DirectConstraintRemindInterval), keeping rapid Q&A clean.
// Returns (decoratedPrompt, includedConstraint).
func (e *Engine) decorateDirectPrompt(ctx context.Context, paneID, text string) (string, bool) {
	w := e.findWorkerByPaneOrName(ctx, paneID)
	if w == nil || strings.TrimSpace(w.Constraint) == "" {
		return text, false
	}
	now := time.Now()
	e.mu.Lock()
	last := e.constraintRemindAt[w.PaneID]
	e.mu.Unlock()
	if !last.IsZero() && now.Sub(last) < DirectConstraintRemindInterval {
		return text, false
	}
	return FormatConstraintHeader(w.Constraint) + "\n" + text, true
}

func (e *Engine) markConstraintDelivered(paneID string) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return
	}
	e.mu.Lock()
	if e.constraintRemindAt == nil {
		e.constraintRemindAt = map[string]time.Time{}
	}
	e.constraintRemindAt[paneID] = time.Now()
	e.mu.Unlock()
}

func (e *Engine) clearConstraintDelivered(paneID string) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return
	}
	e.mu.Lock()
	delete(e.constraintRemindAt, paneID)
	e.mu.Unlock()
}

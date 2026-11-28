package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func workItemLatestUpdateKey(id string) string { return "wi_update:" + strings.TrimSpace(id) }

type workItemLatestUpdatePayload struct {
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	EntryID string    `json:"entry_id,omitempty"`
}

// GetWorkItemLatestUpdate loads the latest routine FYI/patrol update attached to work item id
// from kv (zero schema migration, mirroring wi_scope:* and wi_title:* governance metadata).
func (s *Store) GetWorkItemLatestUpdate(ctx context.Context, id string) (text string, at *time.Time, entryID string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil, ""
	}
	raw, ok, err := s.GetKV(ctx, workItemLatestUpdateKey(id))
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return "", nil, ""
	}
	var payload workItemLatestUpdatePayload
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return strings.TrimSpace(raw), nil, ""
	}
	text = strings.TrimSpace(payload.Text)
	if text == "" {
		return "", nil, ""
	}
	if !payload.At.IsZero() {
		t := payload.At
		at = &t
	}
	return text, at, strings.TrimSpace(payload.EntryID)
}

// SetWorkItemLatestUpdate overwrites the latest routine status/patrol summary for work item id
// in kv unless a strictly newer update is already stored. Empty text clears the stored update.
func (s *Store) SetWorkItemLatestUpdate(ctx context.Context, id, text, entryID string, at time.Time) error {
	id = strings.TrimSpace(id)
	text = strings.TrimSpace(text)
	entryID = strings.TrimSpace(entryID)
	if id == "" {
		return nil
	}
	if text == "" {
		return s.DeleteKV(ctx, workItemLatestUpdateKey(id))
	}
	if at.IsZero() {
		at = time.Now()
	}
	if _, existingAt, _ := s.GetWorkItemLatestUpdate(ctx, id); existingAt != nil && existingAt.After(at) {
		return nil
	}
	raw, err := mustJSON(workItemLatestUpdatePayload{
		Text:    text,
		At:      at,
		EntryID: entryID,
	})
	if err != nil {
		return fmt.Errorf("encode work item latest update %s: %w", id, err)
	}
	return s.SetKV(ctx, workItemLatestUpdateKey(id), raw)
}

func (s *Store) hydrateWorkItemMeta(ctx context.Context, wi *model.WorkItem) {
	if wi == nil {
		return
	}
	wi.Scope = s.GetWorkItemScope(ctx, wi.ID)
	wi.LatestUpdate, wi.LatestUpdateAt, wi.LatestUpdateEntryID = s.GetWorkItemLatestUpdate(ctx, wi.ID)
}

// ArchiveActionAsFYI atomically transitions a legacy pending router_check action into an
// auto-archived FYI activity notice (status=resolved, category=fyi, resolved_by=auto_fyi)
// while preserving its original CreatedAt so it remains inspectable in the activity feed.
func (s *Store) ArchiveActionAsFYI(ctx context.Context, id, workItemID string) (*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin archive fyi action: %w", err)
	}
	defer tx.Rollback()

	var status, raw string
	err = tx.QueryRowContext(ctx, `SELECT status, data FROM actions WHERE id = ?`, id).Scan(&status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read action %s: %w", id, err)
	}
	if status != model.ActionPending {
		return nil, ErrActionAlreadyResolved
	}
	var a model.Action
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("decode action %s: %w", id, err)
	}
	if a.Kind != model.ActionKindRouterCheck || len(a.Choices) > 0 {
		return nil, fmt.Errorf("action %s is not a lightweight router_check", id)
	}
	now := time.Now()
	a.Category = model.ActionCategoryFYI
	a.Status = model.ActionResolved
	a.ResolvedBy = "auto_fyi"
	a.ResolvedChannel = "router"
	a.ChosenKey = "fyi"
	a.ChosenLabel = "纯知会"
	a.ResolvedAt = &now
	if strings.TrimSpace(a.WorkItemID) == "" && strings.TrimSpace(workItemID) != "" {
		a.WorkItemID = strings.TrimSpace(workItemID)
	}
	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode fyi action %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?, resolved_at = ?
		WHERE id = ? AND status = ?
	`, model.ActionResolved, updated, now.Unix(), id, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("archive fyi action %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrActionAlreadyResolved
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit archive fyi action %s: %w", id, err)
	}
	return &a, nil
}

// ListRecentFYIActions returns recent auto-archived FYI router_check notices (newest first)
// created at or after since so users can still inspect routine patrol updates without inflating badges.
func (s *Store) ListRecentFYIActions(ctx context.Context, thread string, since time.Time, limit int) ([]*model.Action, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	thread = strings.TrimSpace(thread)
	sinceUnix := int64(0)
	if !since.IsZero() {
		sinceUnix = since.Unix()
	}
	// Scan bounded recent resolved router_check rows (over-fetch slightly because manual "已看" acks share status=resolved).
	scanLimit := limit * 5
	if scanLimit < 60 {
		scanLimit = 60
	}
	var (
		rows *sql.Rows
		err  error
	)
	if thread != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT data FROM actions
			WHERE kind = ? AND status = ? AND thread = ? AND created_at >= ?
			ORDER BY created_at DESC
			LIMIT ?
		`, model.ActionKindRouterCheck, model.ActionResolved, thread, sinceUnix, scanLimit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT data FROM actions
			WHERE kind = ? AND status = ? AND created_at >= ?
			ORDER BY created_at DESC
			LIMIT ?
		`, model.ActionKindRouterCheck, model.ActionResolved, sinceUnix, scanLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("list recent fyi actions: %w", err)
	}
	defer rows.Close()

	out := []*model.Action{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan fyi action: %w", err)
		}
		var a model.Action
		if json.Unmarshal([]byte(raw), &a) != nil {
			continue
		}
		if !a.IsFYI() {
			continue
		}
		out = append(out, &a)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// ClearFYIActions atomically expires all active FYI activity notices in thread (or all threads when empty)
// so the user can clear the low-priority activity feed in one click without losing chat history.
func (s *Store) ClearFYIActions(ctx context.Context, thread string) (int, error) {
	thread = strings.TrimSpace(thread)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin clear fyi actions: %w", err)
	}
	defer tx.Rollback()

	var rows *sql.Rows
	if thread != "" {
		rows, err = tx.QueryContext(ctx, `
			SELECT id, data FROM actions
			WHERE kind = ? AND status = ? AND thread = ?
		`, model.ActionKindRouterCheck, model.ActionResolved, thread)
	} else {
		rows, err = tx.QueryContext(ctx, `
			SELECT id, data FROM actions
			WHERE kind = ? AND status = ?
		`, model.ActionKindRouterCheck, model.ActionResolved)
	}
	if err != nil {
		return 0, fmt.Errorf("query fyi actions to clear: %w", err)
	}
	type item struct {
		id  string
		raw string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.raw); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan fyi action to clear: %w", err)
		}
		items = append(items, it)
	}
	rErr := rows.Err()
	_ = rows.Close()
	if rErr != nil {
		return 0, rErr
	}

	now := time.Now()
	cleared := 0
	for _, it := range items {
		var a model.Action
		if json.Unmarshal([]byte(it.raw), &a) != nil || !a.IsFYI() {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedBy = "fyi_cleared"
		a.ResolvedAt = &now
		updated, mErr := mustJSON(&a)
		if mErr != nil {
			return 0, fmt.Errorf("encode cleared fyi action %s: %w", it.id, mErr)
		}
		res, uErr := tx.ExecContext(ctx, `
			UPDATE actions SET status = ?, data = ?, resolved_at = ?
			WHERE id = ? AND status = ?
		`, model.ActionExpired, updated, now.Unix(), it.id, model.ActionResolved)
		if uErr != nil {
			return 0, fmt.Errorf("clear fyi action %s: %w", it.id, uErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			cleared++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear fyi actions: %w", err)
	}
	return cleared, nil
}

// AckAllRouterChecks atomically marks all pending lightweight router_check reminders in thread
// as silently acknowledged ("已看") without touching any blocking decisions or legacy choice cards.
func (s *Store) AckAllRouterChecks(ctx context.Context, thread, channel string) ([]*model.Action, error) {
	thread = strings.TrimSpace(thread)
	if thread == "" {
		thread = model.MainThread
	}
	channel = strings.TrimSpace(channel)
	if channel == "" {
		channel = "web"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ack all router_checks: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, data FROM actions
		WHERE thread = ? AND kind = ? AND status = ?
		ORDER BY created_at ASC
	`, thread, model.ActionKindRouterCheck, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("query pending router_checks: %w", err)
	}
	type item struct {
		id  string
		raw string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.raw); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan pending router_check: %w", err)
		}
		items = append(items, it)
	}
	rErr := rows.Err()
	_ = rows.Close()
	if rErr != nil {
		return nil, rErr
	}

	now := time.Now()
	var acked []*model.Action
	for _, it := range items {
		var a model.Action
		if json.Unmarshal([]byte(it.raw), &a) != nil {
			continue
		}
		// Never touch blocking decisions or legacy choice-bearing cards.
		if a.Kind != model.ActionKindRouterCheck || a.IsDecision() || len(a.Choices) > 0 {
			continue
		}
		a.Status = model.ActionResolved
		a.ResolvedBy = channel
		a.ResolvedChannel = channel
		a.ChosenKey = "ack"
		a.ChosenLabel = "已看"
		a.ResolvedAt = &now
		updated, mErr := mustJSON(&a)
		if mErr != nil {
			return nil, fmt.Errorf("encode acked router_check %s: %w", it.id, mErr)
		}
		res, uErr := tx.ExecContext(ctx, `
			UPDATE actions SET status = ?, data = ?, resolved_at = ?
			WHERE id = ? AND status = ?
		`, model.ActionResolved, updated, now.Unix(), it.id, model.ActionPending)
		if uErr != nil {
			return nil, fmt.Errorf("ack router_check %s: %w", it.id, uErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copyAct := a
			acked = append(acked, &copyAct)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ack all router_checks: %w", err)
	}
	return acked, nil
}

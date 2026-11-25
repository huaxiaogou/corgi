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

// ---------------------------------------------------------------- Triggers

// InsertTrigger persists a new trigger, enforcing uniqueness on non-empty IdemKey.
func (s *Store) InsertTrigger(ctx context.Context, t *model.Trigger) error {
	data, err := mustJSON(t)
	if err != nil {
		return fmt.Errorf("encode trigger: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO triggers(id, seq, source, thread, status, idem_key, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.Seq, t.Source, t.Thread, t.Status, t.IdemKey, data, t.At.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: triggers.idem_key") {
			return ErrDuplicateTrigger
		}
		return fmt.Errorf("insert trigger %s: %w", t.ID, err)
	}
	return nil
}

// UpdateTrigger updates status/targets of an existing trigger.
func (s *Store) UpdateTrigger(ctx context.Context, t *model.Trigger) error {
	data, err := mustJSON(t)
	if err != nil {
		return fmt.Errorf("encode trigger: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE triggers SET status = ?, data = ? WHERE id = ?`, t.Status, data, t.ID)
	if err != nil {
		return fmt.Errorf("update trigger %s: %w", t.ID, err)
	}
	return nil
}

// GetTrigger loads one trigger by ID.
func (s *Store) GetTrigger(ctx context.Context, id string) (*model.Trigger, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM triggers WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger %s: %w", id, err)
	}
	var t model.Trigger
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("decode trigger %s: %w", id, err)
	}
	return &t, nil
}

// ListQueuedTriggers returns all triggers currently awaiting delivery (oldest first).
func (s *Store) ListQueuedTriggers(ctx context.Context) ([]*model.Trigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM triggers WHERE status = 'queued' ORDER BY seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("list queued triggers: %w", err)
	}
	defer rows.Close()
	return scanTriggers(rows)
}

// ListTriggers returns the newest limit triggers (newest first).
func (s *Store) ListTriggers(ctx context.Context, limit int) ([]*model.Trigger, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM triggers ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()
	return scanTriggers(rows)
}

// ListThreadTriggers returns recent triggers in one thread (newest first).
func (s *Store) ListThreadTriggers(ctx context.Context, thread string, limit int) ([]*model.Trigger, error) {
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM triggers WHERE thread = ? ORDER BY seq DESC LIMIT ?`, thread, limit)
	if err != nil {
		return nil, fmt.Errorf("list thread triggers: %w", err)
	}
	defer rows.Close()
	return scanTriggers(rows)
}

// ListWorkerTriggers returns recent triggers associated with workerName (newest first).
func (s *Store) ListWorkerTriggers(ctx context.Context, workerName string, limit int) ([]*model.Trigger, error) {
	if limit <= 0 {
		limit = 20
	}
	all, err := s.ListTriggers(ctx, 200)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Trigger, 0, limit)
	for _, t := range all {
		if t.Ref == workerName || t.From == workerName {
			out = append(out, t)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func scanTriggers(rows *sql.Rows) ([]*model.Trigger, error) {
	out := []*model.Trigger{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t model.Trigger
		if json.Unmarshal([]byte(raw), &t) == nil {
			out = append(out, &t)
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- Actions

// InsertAction persists a new pending Action.
func (s *Store) InsertAction(ctx context.Context, a *model.Action) error {
	data, err := mustJSON(a)
	if err != nil {
		return fmt.Errorf("encode action: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO actions(id, kind, thread, target_worker, status, data, created_at, resolved_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, 0)
	`, a.ID, a.Kind, a.Thread, a.TargetWorker, a.Status, data, a.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("insert action %s: %w", a.ID, err)
	}
	return nil
}

// GetAction returns one action by ID.
func (s *Store) GetAction(ctx context.Context, id string) (*model.Action, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM actions WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get action %s: %w", id, err)
	}
	var a model.Action
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("decode action %s: %w", id, err)
	}
	return &a, nil
}

// ListPendingActions returns all unresolved actions (oldest first).
func (s *Store) ListPendingActions(ctx context.Context) ([]*model.Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM actions WHERE status = ? ORDER BY created_at ASC`, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("list pending actions: %w", err)
	}
	defer rows.Close()
	out := []*model.Action{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var a model.Action
		if json.Unmarshal([]byte(raw), &a) == nil {
			out = append(out, &a)
		}
	}
	return out, rows.Err()
}

// ResolveAction atomically transitions a pending action to resolved.
// Returns ErrActionAlreadyResolved if another channel (Web or WeCom) resolved it first.
func (s *Store) ResolveAction(ctx context.Context, id, resolvedBy, chosenKey, chosenLabel string) (*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin resolve action: %w", err)
	}
	defer tx.Rollback()

	var raw string
	var status string
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
	now := time.Now()
	a.Status = model.ActionResolved
	a.ResolvedBy = resolvedBy
	a.ChosenKey = chosenKey
	a.ChosenLabel = chosenLabel
	a.ResolvedAt = &now
	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode action %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?, resolved_at = ?
		WHERE id = ? AND status = ?
	`, model.ActionResolved, updated, now.Unix(), id, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("update action %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrActionAlreadyResolved
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit resolve action %s: %w", id, err)
	}
	return &a, nil
}

// DismissAction atomically transitions a pending action to expired with resolved_by="dismiss".
func (s *Store) DismissAction(ctx context.Context, id string) (*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin dismiss action: %w", err)
	}
	defer tx.Rollback()

	var raw, status string
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
	now := time.Now()
	a.Status = model.ActionExpired
	a.ResolvedBy = "dismiss"
	a.ResolvedAt = &now
	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode action %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?, resolved_at = ?
		WHERE id = ? AND status = ?
	`, model.ActionExpired, updated, now.Unix(), id, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("dismiss action %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrActionAlreadyResolved
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit dismiss action %s: %w", id, err)
	}
	return &a, nil
}

// ExpireWorkerActions expires any pending worker_dialog actions for a worker that moved on or exited.
func (s *Store) ExpireWorkerActions(ctx context.Context, workerName string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, data FROM actions WHERE target_worker = ? AND status = ?`, workerName, model.ActionPending)
	if err != nil {
		return fmt.Errorf("query worker actions: %w", err)
	}
	defer rows.Close()
	type pair struct {
		id  string
		raw string
	}
	var items []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.raw); err == nil {
			items = append(items, p)
		}
	}
	rows.Close()
	now := time.Now()
	for _, p := range items {
		var a model.Action
		if json.Unmarshal([]byte(p.raw), &a) != nil {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedAt = &now
		if updated, err := mustJSON(&a); err == nil {
			s.db.ExecContext(ctx, `UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND status = ?`,
				model.ActionExpired, updated, now.Unix(), p.id, model.ActionPending)
		}
	}
	return nil
}

// SnoozeAction atomically transitions a pending action to snoozed until the given time.
func (s *Store) SnoozeAction(ctx context.Context, id string, until time.Time) (*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin snooze action: %w", err)
	}
	defer tx.Rollback()

	var raw, status string
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
	a.Status = model.ActionSnoozed
	a.SnoozeUntil = &until
	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode action %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?
		WHERE id = ? AND status = ?
	`, model.ActionSnoozed, updated, id, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("snooze action %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrActionAlreadyResolved
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit snooze action %s: %w", id, err)
	}
	return &a, nil
}

// ListSnoozedActions returns all snoozed actions awaiting re-surface.
func (s *Store) ListSnoozedActions(ctx context.Context) ([]*model.Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM actions WHERE status = ? ORDER BY created_at ASC`, model.ActionSnoozed)
	if err != nil {
		return nil, fmt.Errorf("list snoozed actions: %w", err)
	}
	defer rows.Close()
	out := []*model.Action{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var a model.Action
		if json.Unmarshal([]byte(raw), &a) == nil {
			out = append(out, &a)
		}
	}
	return out, rows.Err()
}

// WakeDueSnoozedActions moves any snoozed action whose snooze_until <= now back to pending.
func (s *Store) WakeDueSnoozedActions(ctx context.Context, now time.Time) ([]*model.Action, error) {
	snoozed, err := s.ListSnoozedActions(ctx)
	if err != nil {
		return nil, err
	}
	var woken []*model.Action
	for _, a := range snoozed {
		if a.SnoozeUntil != nil && a.SnoozeUntil.After(now) {
			continue
		}
		a.Status = model.ActionPending
		a.SnoozeUntil = nil
		updated, err := mustJSON(a)
		if err != nil {
			continue
		}
		res, err := s.db.ExecContext(ctx, `UPDATE actions SET status = ?, data = ? WHERE id = ? AND status = ?`,
			model.ActionPending, updated, a.ID, model.ActionSnoozed)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				woken = append(woken, a)
			}
		}
	}
	return woken, nil
}

// ExpireActionsByKind expires any pending actions of a specific kind in thread (e.g. replacing prior patrol digest cards).
func (s *Store) ExpireActionsByKind(ctx context.Context, thread, kind string) error {
	pending, err := s.ListPendingActions(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, a := range pending {
		if a.Kind != kind || (thread != "" && a.Thread != thread) {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedBy = "replaced"
		a.ResolvedAt = &now
		if updated, err := mustJSON(a); err == nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND status = ?`,
				model.ActionExpired, updated, now.Unix(), a.ID, model.ActionPending)
		}
	}
	return nil
}

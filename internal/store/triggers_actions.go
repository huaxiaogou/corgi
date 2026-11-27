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

// GetTriggerByIdemKey loads an existing trigger by its unique non-empty idempotency key.
func (s *Store) GetTriggerByIdemKey(ctx context.Context, idemKey string) (*model.Trigger, error) {
	idemKey = strings.TrimSpace(idemKey)
	if idemKey == "" {
		return nil, ErrNotFound
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM triggers WHERE idem_key = ?`, idemKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger by idem_key %q: %w", idemKey, err)
	}
	var t model.Trigger
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("decode trigger by idem_key %q: %w", idemKey, err)
	}
	return &t, nil
}

// CommitSayEntryAndTrigger atomically persists a human chat entry and its trigger in one SQLite
// transaction. If trig.IdemKey already exists, no duplicate entry is written and the original
// trigger + entry are returned with duplicate=true.
func (s *Store) CommitSayEntryAndTrigger(ctx context.Context, e *model.Entry, t *model.Trigger) (*model.Entry, *model.Trigger, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("begin commit say: %w", err)
	}
	defer tx.Rollback()

	trigData, err := mustJSON(t)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode trigger: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO triggers(id, seq, source, thread, status, idem_key, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.Seq, t.Source, t.Thread, t.Status, t.IdemKey, trigData, t.At.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: triggers.idem_key") && t.IdemKey != "" {
			var rawTrig string
			if qErr := tx.QueryRowContext(ctx, `SELECT data FROM triggers WHERE idem_key = ?`, t.IdemKey).Scan(&rawTrig); qErr != nil {
				return nil, nil, true, nil
			}
			var existingTrig model.Trigger
			if json.Unmarshal([]byte(rawTrig), &existingTrig) != nil {
				return nil, nil, true, nil
			}
			var existingEntry *model.Entry
			if existingTrig.Ref != "" {
				var rawEntry string
				if eErr := tx.QueryRowContext(ctx, `SELECT data FROM entries WHERE id = ?`, existingTrig.Ref).Scan(&rawEntry); eErr == nil {
					var ent model.Entry
					if json.Unmarshal([]byte(rawEntry), &ent) == nil {
						existingEntry = &ent
					}
				}
			}
			return existingEntry, &existingTrig, true, nil
		}
		return nil, nil, false, fmt.Errorf("insert trigger %s: %w", t.ID, err)
	}

	entryData, err := mustJSON(e)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entries(id, seq, thread, role, channel, text, action_id, pinned, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			text = excluded.text,
			action_id = excluded.action_id,
			pinned = excluded.pinned,
			data = excluded.data
	`, e.ID, e.Seq, e.Thread, e.Role, e.Channel, e.Text, e.ActionID, boolInt(e.Pinned), entryData, e.At.Unix()); err != nil {
		return nil, nil, false, fmt.Errorf("insert entry %s: %w", e.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, fmt.Errorf("commit say %s: %w", e.ID, err)
	}
	return e, t, false, nil
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

// ListActionsByIDs loads a map of Actions keyed by ID in a single parameterized query.
func (s *Store) ListActionsByIDs(ctx context.Context, ids []string) (map[string]*model.Action, error) {
	out := map[string]*model.Action{}
	seen := make(map[string]struct{}, len(ids))
	args := make([]any, 0, len(ids))
	placeholders := make([]string, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}
	if len(args) == 0 {
		return out, nil
	}
	query := `SELECT data FROM actions WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list actions by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var a model.Action
		if json.Unmarshal([]byte(raw), &a) == nil && a.ID != "" {
			out[a.ID] = &a
		}
	}
	return out, rows.Err()
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
	var keys []string
	if k := strings.TrimSpace(chosenKey); k != "" {
		keys = []string{k}
	}
	return s.ResolveActionWithKeys(ctx, id, resolvedBy, chosenKey, chosenLabel, keys)
}

// ResolveActionWithKeys atomically transitions a pending action to resolved while persisting
// structured multi-select keys and channel attribution.
func (s *Store) ResolveActionWithKeys(ctx context.Context, id, resolvedBy, chosenKey, chosenLabel string, chosenKeys []string) (*model.Action, error) {
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
	a.ResolvedChannel = strings.TrimSpace(resolvedBy)
	a.ChosenKey = chosenKey
	if len(chosenKeys) > 0 {
		a.ChosenKeys = append([]string(nil), chosenKeys...)
	} else {
		a.ChosenKeys = nil
	}
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
	return s.DismissActionBy(ctx, id, "")
}

// DismissActionBy atomically dismisses a pending action and records the originating channel.
func (s *Store) DismissActionBy(ctx context.Context, id, channel string) (*model.Action, error) {
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
	a.ResolvedChannel = strings.TrimSpace(channel)
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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, data FROM actions WHERE target_worker = ? AND kind = ? AND status = ?`,
		workerName, model.ActionKindWorkerDialog, model.ActionPending)
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
		if json.Unmarshal([]byte(p.raw), &a) != nil || a.Kind != model.ActionKindWorkerDialog {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedAt = &now
		if updated, err := mustJSON(&a); err == nil {
			s.db.ExecContext(ctx,
				`UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND kind = ? AND status = ?`,
				model.ActionExpired, updated, now.Unix(), p.id, model.ActionKindWorkerDialog, model.ActionPending)
		}
	}
	return nil
}

// SnoozeAction atomically transitions a pending action to snoozed until the given time.
func (s *Store) SnoozeAction(ctx context.Context, id string, until time.Time) (*model.Action, error) {
	return s.SnoozeActionBy(ctx, id, "", until)
}

// SnoozeActionBy atomically snoozes a pending action and records the originating channel.
func (s *Store) SnoozeActionBy(ctx context.Context, id, channel string, until time.Time) (*model.Action, error) {
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
	now := time.Now()
	a.Status = model.ActionSnoozed
	a.ResolvedChannel = strings.TrimSpace(channel)
	a.ResolvedAt = &now
	a.SnoozeUntil = &until
	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode action %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?, resolved_at = ?
		WHERE id = ? AND status = ?
	`, model.ActionSnoozed, updated, now.Unix(), id, model.ActionPending)
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
		a.ResolvedChannel = ""
		a.ResolvedAt = nil
		a.PushedToWeCom = false
		updated, err := mustJSON(a)
		if err != nil {
			continue
		}
		res, err := s.db.ExecContext(ctx, `UPDATE actions SET status = ?, data = ?, resolved_at = 0 WHERE id = ? AND status = ?`,
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

// MarkActionPushedToWeCom records that an Action card was delivered to WeCom so later
// Web/CLI resolutions know whether to push a WeCom synchronization receipt.
func (s *Store) MarkActionPushedToWeCom(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark action pushed: %w", err)
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRowContext(ctx, `SELECT data FROM actions WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read action %s: %w", id, err)
	}
	var a model.Action
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return fmt.Errorf("decode action %s: %w", id, err)
	}
	if a.PushedToWeCom {
		return nil
	}
	a.PushedToWeCom = true
	updated, err := mustJSON(&a)
	if err != nil {
		return fmt.Errorf("encode action %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET data = ? WHERE id = ?`, updated, id); err != nil {
		return fmt.Errorf("mark action %s pushed: %w", id, err)
	}
	return tx.Commit()
}

// ExpireStalePendingActions expires pending choice/digest actions older than maxAge (e.g. 7 days),
// preserving live terminal dialogs and explicit review check reminders.
func (s *Store) ExpireStalePendingActions(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	pending, err := s.ListPendingActions(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	cutoff := now.Add(-maxAge)
	expired := 0
	for _, a := range pending {
		if a.Kind == model.ActionKindWorkerDialog || a.Kind == model.ActionKindRouterCheck {
			continue
		}
		if a.CreatedAt.IsZero() || !a.CreatedAt.Before(cutoff) {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedBy = "expired"
		a.ResolvedAt = &now
		updated, err := mustJSON(a)
		if err != nil {
			continue
		}
		res, err := s.db.ExecContext(ctx, `UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND status = ?`,
			model.ActionExpired, updated, now.Unix(), a.ID, model.ActionPending)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				expired++
			}
		}
	}
	return expired, nil
}

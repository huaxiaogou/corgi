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

// CommitKVTx atomically applies a batch of KV upserts and deletes in a single SQLite transaction.
func (s *Store) CommitKVTx(ctx context.Context, updates map[string]string, deletes ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin commit kv tx: %w", err)
	}
	defer tx.Rollback()

	for _, k := range deletes {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, k); err != nil {
			return fmt.Errorf("delete kv %q: %w", k, err)
		}
	}
	for k, v := range updates {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
			k, v,
		); err != nil {
			return fmt.Errorf("upsert kv %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit kv tx: %w", err)
	}
	return nil
}

// CommitConfigProposalTx atomically inserts a confirmation card Entry, its Action (rejecting
// duplicate Action IDs without overwrite), and the bound KV proposal in one SQLite transaction.
func (s *Store) CommitConfigProposalTx(ctx context.Context, entry *model.Entry, action *model.Action, propKey, propJSON string) error {
	if entry == nil || action == nil || strings.TrimSpace(propKey) == "" {
		return fmt.Errorf("commit config proposal: missing entry, action, or proposal key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin config proposal tx: %w", err)
	}
	defer tx.Rollback()

	actData, err := mustJSON(action)
	if err != nil {
		return fmt.Errorf("encode config action: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO actions(id, kind, thread, target_worker, status, data, created_at, resolved_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, 0)
	`, action.ID, action.Kind, action.Thread, action.TargetWorker, action.Status, actData, action.CreatedAt.Unix()); err != nil {
		return fmt.Errorf("insert config action %s: %w", action.ID, err)
	}

	entryData, err := mustJSON(entry)
	if err != nil {
		return fmt.Errorf("encode config entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entries(id, seq, thread, role, channel, text, action_id, pinned, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID, entry.Seq, entry.Thread, entry.Role, entry.Channel, entry.Text, entry.ActionID, boolInt(entry.Pinned), entryData, entry.At.Unix()); err != nil {
		return fmt.Errorf("insert config entry %s: %w", entry.ID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		propKey, propJSON,
	); err != nil {
		return fmt.Errorf("insert config proposal %s: %w", propKey, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit config proposal tx: %w", err)
	}
	return nil
}

// ConfigConfirmTxRequest describes an atomic L2 confirmation settlement transaction.
type ConfigConfirmTxRequest struct {
	ActionID        string
	ProposalKey     string
	NextStatus      string // resolved | expired
	ResolvedBy      string
	ResolvedChannel string
	ChosenKey       string
	ChosenLabel     string
	ExpiredReason   string
	KVUpdates       map[string]string
}

// CommitConfigConfirmTx atomically verifies the Action is still pending, updates its terminal
// state, deletes the proposal KV key, and commits any resulting config/audit KV updates.
func (s *Store) CommitConfigConfirmTx(ctx context.Context, req ConfigConfirmTxRequest) (*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin config confirm tx: %w", err)
	}
	defer tx.Rollback()

	var status, raw string
	err = tx.QueryRowContext(ctx, `SELECT status, data FROM actions WHERE id = ?`, req.ActionID).Scan(&status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read config action %s: %w", req.ActionID, err)
	}
	if status != model.ActionPending {
		return nil, ErrActionAlreadyResolved
	}
	var a model.Action
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("decode config action %s: %w", req.ActionID, err)
	}
	if a.Kind != model.ActionKindConfigConfirm {
		return nil, fmt.Errorf("action %s is %s, not config_confirm", req.ActionID, a.Kind)
	}

	now := time.Now()
	a.Status = req.NextStatus
	a.ResolvedBy = req.ResolvedBy
	ch := strings.TrimSpace(req.ResolvedChannel)
	if ch == "" {
		ch = strings.TrimSpace(req.ResolvedBy)
	}
	a.ResolvedChannel = ch
	a.ChosenKey = req.ChosenKey
	if k := strings.TrimSpace(req.ChosenKey); k != "" {
		a.ChosenKeys = []string{k}
	} else {
		a.ChosenKeys = nil
	}
	a.ChosenLabel = req.ChosenLabel
	a.ExpiredReason = req.ExpiredReason
	a.ResolvedAt = &now

	updated, err := mustJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("encode config action %s: %w", req.ActionID, err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE actions SET status = ?, data = ?, resolved_at = ?
		WHERE id = ? AND status = ?
	`, a.Status, updated, now.Unix(), req.ActionID, model.ActionPending)
	if err != nil {
		return nil, fmt.Errorf("update config action %s: %w", req.ActionID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrActionAlreadyResolved
	}

	if strings.TrimSpace(req.ProposalKey) != "" {
		delRes, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, req.ProposalKey)
		if err != nil {
			return nil, fmt.Errorf("delete config proposal %s: %w", req.ProposalKey, err)
		}
		if req.NextStatus == model.ActionResolved {
			if n, _ := delRes.RowsAffected(); n != 1 {
				return nil, ErrActionAlreadyResolved
			}
		}
	}
	for k, v := range req.KVUpdates {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
			k, v,
		); err != nil {
			return nil, fmt.Errorf("upsert kv %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit config confirm tx: %w", err)
	}
	return &a, nil
}

// ExpirePendingConfigConfirms atomically expires pending config_confirm actions older than cutoff
// and removes their bound proposal KV keys in the same transaction.
func (s *Store) ExpirePendingConfigConfirms(ctx context.Context, cutoff time.Time, propPrefix, reason string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin expire config confirms: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, data FROM actions WHERE kind = ? AND status = ?`,
		model.ActionKindConfigConfirm, model.ActionPending,
	)
	if err != nil {
		return 0, fmt.Errorf("list pending config confirms: %w", err)
	}
	type item struct {
		id  string
		raw string
	}
	var list []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.raw); err == nil {
			list = append(list, it)
		}
	}
	_ = rows.Close()

	now := time.Now()
	expired := 0
	for _, it := range list {
		var a model.Action
		if json.Unmarshal([]byte(it.raw), &a) != nil {
			continue
		}
		if a.CreatedAt.IsZero() || !a.CreatedAt.Before(cutoff) {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedBy = "expired"
		a.ExpiredReason = reason
		a.ResolvedAt = &now
		updated, err := mustJSON(&a)
		if err != nil {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND status = ?`,
			model.ActionExpired, updated, now.Unix(), a.ID, model.ActionPending,
		)
		if err != nil {
			return 0, fmt.Errorf("expire config confirm %s: %w", a.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			expired++
			if propPrefix != "" {
				if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, propPrefix+a.ID); err != nil {
					return 0, fmt.Errorf("delete expired proposal %s: %w", a.ID, err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expire config confirms: %w", err)
	}
	return expired, nil
}

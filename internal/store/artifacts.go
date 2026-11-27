package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const artifactHMACSecretKVKey = "artifact_hmac_secret"

// GetOrCreateArtifactSecret returns the persistent 32-byte Hub secret used to sign public preview tokens.
func (s *Store) GetOrCreateArtifactSecret(ctx context.Context) ([]byte, error) {
	if raw, ok, err := s.GetKV(ctx, artifactHMACSecretKVKey); err == nil && ok && len(raw) == 64 {
		if b, dErr := hex.DecodeString(raw); dErr == nil && len(b) == 32 {
			return b, nil
		}
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, fmt.Errorf("generate artifact hmac secret: %w", err)
	}
	encoded := hex.EncodeToString(buf[:])
	// Use INSERT OR IGNORE so concurrent callers converge on a single secret.
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO kv(k, v) VALUES(?, ?)`, artifactHMACSecretKVKey, encoded); err != nil {
		return nil, fmt.Errorf("persist artifact hmac secret: %w", err)
	}
	raw, ok, err := s.GetKV(ctx, artifactHMACSecretKVKey)
	if err != nil || !ok {
		return nil, fmt.Errorf("reload artifact hmac secret: %w", err)
	}
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 32 {
		return nil, errors.New("corrupted artifact hmac secret in kv")
	}
	return b, nil
}

// CommitReplyTx atomically persists a reply Entry, its optional Action (replacing any prior digest card
// when expireDigestKind is non-empty), and all associated Hub Artifact snapshot metadata rows.
func (s *Store) CommitReplyTx(
	ctx context.Context,
	entry *model.Entry,
	action *model.Action,
	expireDigestKind string,
	artifacts []*model.Artifact,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin commit reply tx: %w", err)
	}
	defer tx.Rollback()

	if expireDigestKind != "" {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, data FROM actions WHERE thread = ? AND kind = ? AND status = ?`,
			entry.Thread, expireDigestKind, model.ActionPending,
		)
		if err != nil {
			return fmt.Errorf("query prior digest actions: %w", err)
		}
		type expItem struct {
			id   string
			data string
		}
		var toExpire []expItem
		for rows.Next() {
			var it expItem
			if err := rows.Scan(&it.id, &it.data); err != nil {
				rows.Close()
				return err
			}
			toExpire = append(toExpire, it)
		}
		rows.Close()
		now := time.Now()
		for _, it := range toExpire {
			var act model.Action
			if err := json.Unmarshal([]byte(it.data), &act); err != nil {
				continue
			}
			act.Status = model.ActionExpired
			act.ResolvedBy = "replaced"
			act.ResolvedAt = &now
			updated, mErr := mustJSON(&act)
			if mErr != nil {
				return mErr
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE actions SET status = ?, data = ? WHERE id = ?`,
				model.ActionExpired, updated, act.ID,
			); err != nil {
				return fmt.Errorf("expire prior digest %s: %w", act.ID, err)
			}
		}
	}

	// When inserting a new causal review reminder (--check) bound to a WorkItem or Worker,
	// atomically supersede older pending router_check reminders for that same deliverable source.
	// Real decision cards (router_choice / worker_dialog) are never auto-expired here.
	if action != nil && action.Kind == model.ActionKindRouterCheck &&
		(strings.TrimSpace(action.WorkItemID) != "" || strings.TrimSpace(action.TargetWorker) != "") {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, data FROM actions WHERE thread = ? AND kind = ? AND status = ?`,
			entry.Thread, model.ActionKindRouterCheck, model.ActionPending,
		)
		if err != nil {
			return fmt.Errorf("query prior router_check actions: %w", err)
		}
		type expCheck struct {
			id   string
			data string
		}
		var checks []expCheck
		for rows.Next() {
			var it expCheck
			if err := rows.Scan(&it.id, &it.data); err != nil {
				rows.Close()
				return err
			}
			checks = append(checks, it)
		}
		rows.Close()

		newWI := strings.TrimSpace(action.WorkItemID)
		newWk := strings.TrimSpace(action.TargetWorker)
		now := time.Now()
		for _, it := range checks {
			if it.id == action.ID {
				continue
			}
			var old model.Action
			if json.Unmarshal([]byte(it.data), &old) != nil {
				continue
			}
			oldWI := strings.TrimSpace(old.WorkItemID)
			oldWk := strings.TrimSpace(old.TargetWorker)
			sameSource := false
			if newWI != "" {
				sameSource = oldWI == newWI && (newWk == "" || oldWk == "" || oldWk == newWk)
			} else if newWk != "" {
				sameSource = oldWI == "" && oldWk == newWk
			}
			if !sameSource {
				continue
			}
			old.Status = model.ActionExpired
			old.ResolvedBy = "expired"
			old.ExpiredReason = "superseded"
			old.ResolvedAt = &now
			updated, mErr := mustJSON(&old)
			if mErr != nil {
				return mErr
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE actions SET status = ?, data = ?, resolved_at = ? WHERE id = ? AND status = ?`,
				model.ActionExpired, updated, now.Unix(), old.ID, model.ActionPending,
			); err != nil {
				return fmt.Errorf("expire superseded check %s: %w", old.ID, err)
			}
		}
	}

	if action != nil {
		actData, err := mustJSON(action)
		if err != nil {
			return fmt.Errorf("encode action: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO actions(id, thread, kind, target_worker, status, data, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				status = excluded.status,
				data = excluded.data
		`, action.ID, action.Thread, action.Kind, action.TargetWorker, action.Status, actData, action.CreatedAt.Unix()); err != nil {
			return fmt.Errorf("insert action %s: %w", action.ID, err)
		}
	}

	entryData, err := mustJSON(entry)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entries(id, seq, thread, role, channel, text, action_id, pinned, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			text = excluded.text,
			action_id = excluded.action_id,
			pinned = excluded.pinned,
			data = excluded.data
	`, entry.ID, entry.Seq, entry.Thread, entry.Role, entry.Channel, entry.Text, entry.ActionID, boolInt(entry.Pinned), entryData, entry.At.Unix()); err != nil {
		return fmt.Errorf("insert entry %s: %w", entry.ID, err)
	}

	for _, art := range artifacts {
		if art == nil {
			continue
		}
		revokedAt := int64(0)
		if art.RevokedAt != nil && !art.RevokedAt.IsZero() {
			revokedAt = art.RevokedAt.Unix()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifacts(
				id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
				source_path, source_machine, created_at, share_expires, expires_at, revoked_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			art.ID, art.EntryID, art.Ordinal, art.Thread, art.WorkItemID,
			art.Name, art.Kind, art.MIME, art.Size,
			art.SourcePath, art.SourceMachine,
			art.CreatedAt.Unix(), art.ShareExpiresAt.Unix(), art.ExpiresAt.Unix(), revokedAt,
		); err != nil {
			return fmt.Errorf("insert artifact %s: %w", art.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reply tx: %w", err)
	}
	return nil
}

// InsertArtifact persists a single standalone Hub artifact metadata row (used for worker report snapshots).
func (s *Store) InsertArtifact(ctx context.Context, art *model.Artifact) error {
	if art == nil {
		return nil
	}
	revokedAt := int64(0)
	if art.RevokedAt != nil && !art.RevokedAt.IsZero() {
		revokedAt = art.RevokedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts(
			id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
			source_path, source_machine, created_at, share_expires, expires_at, revoked_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			entry_id = CASE WHEN excluded.entry_id != '' THEN excluded.entry_id ELSE artifacts.entry_id END,
			work_item_id = CASE WHEN excluded.work_item_id != '' THEN excluded.work_item_id ELSE artifacts.work_item_id END
	`,
		art.ID, art.EntryID, art.Ordinal, art.Thread, art.WorkItemID,
		art.Name, art.Kind, art.MIME, art.Size,
		art.SourcePath, art.SourceMachine,
		art.CreatedAt.Unix(), art.ShareExpiresAt.Unix(), art.ExpiresAt.Unix(), revokedAt,
	)
	if err != nil {
		return fmt.Errorf("insert artifact %s: %w", art.ID, err)
	}
	return nil
}

// GetArtifact loads one artifact metadata record by internal ID.
func (s *Store) GetArtifact(ctx context.Context, id string) (*model.Artifact, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
		       source_path, source_machine, created_at, share_expires, expires_at, revoked_at
		FROM artifacts WHERE id = ?
	`, id)
	art, err := scanArtifact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact %s: %w", id, err)
	}
	return art, nil
}

// ListArtifactsByIDs returns a map of artifact metadata rows keyed by ID.
func (s *Store) ListArtifactsByIDs(ctx context.Context, ids []string) (map[string]*model.Artifact, error) {
	out := make(map[string]*model.Artifact)
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
		       source_path, source_machine, created_at, share_expires, expires_at, revoked_at
		FROM artifacts WHERE id IN (`+strings.Join(placeholders, ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifacts by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		art, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out[art.ID] = art
	}
	return out, rows.Err()
}

// ListArtifactsByWorkIDs returns artifact metadata rows grouped by work_item_id (newest first).
func (s *Store) ListArtifactsByWorkIDs(ctx context.Context, workIDs []string) (map[string][]*model.Artifact, error) {
	out := make(map[string][]*model.Artifact)
	if len(workIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(workIDs))
	args := make([]any, 0, len(workIDs))
	seen := make(map[string]struct{}, len(workIDs))
	for _, id := range workIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
		       source_path, source_machine, created_at, share_expires, expires_at, revoked_at
		FROM artifacts
		WHERE work_item_id IN (`+strings.Join(placeholders, ",")+`) AND revoked_at = 0
		ORDER BY created_at DESC, ordinal ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifacts by work ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		art, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out[art.WorkItemID] = append(out[art.WorkItemID], art)
	}
	return out, rows.Err()
}

// ListAllArtifacts returns all artifact records oldest-first for startup GC and quota enforcement.
func (s *Store) ListAllArtifacts(ctx context.Context) ([]*model.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_id, ordinal, thread, work_item_id, name, kind, mime, size,
		       source_path, source_machine, created_at, share_expires, expires_at, revoked_at
		FROM artifacts
		ORDER BY created_at ASC, ordinal ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all artifacts: %w", err)
	}
	defer rows.Close()
	var out []*model.Artifact
	for rows.Next() {
		art, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, art)
	}
	return out, rows.Err()
}

// RevokeArtifact marks an artifact revoked so public links immediately return unavailable.
func (s *Store) RevokeArtifact(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE artifacts SET revoked_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("revoke artifact %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type artifactScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(sc artifactScanner) (*model.Artifact, error) {
	var (
		art          model.Artifact
		createdAt    int64
		shareExpires int64
		expiresAt    int64
		revokedAt    int64
	)
	if err := sc.Scan(
		&art.ID, &art.EntryID, &art.Ordinal, &art.Thread, &art.WorkItemID,
		&art.Name, &art.Kind, &art.MIME, &art.Size,
		&art.SourcePath, &art.SourceMachine,
		&createdAt, &shareExpires, &expiresAt, &revokedAt,
	); err != nil {
		return nil, err
	}
	if createdAt > 0 {
		art.CreatedAt = time.Unix(createdAt, 0)
	}
	if shareExpires > 0 {
		art.ShareExpiresAt = time.Unix(shareExpires, 0)
	}
	if expiresAt > 0 {
		art.ExpiresAt = time.Unix(expiresAt, 0)
	}
	if revokedAt > 0 {
		t := time.Unix(revokedAt, 0)
		art.RevokedAt = &t
	}
	return &art, nil
}

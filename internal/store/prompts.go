package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kiperjing/corgi/internal/model"
)

// InsertQueuedPrompt persists one queued prompt waiting for a busy agent pane or main Corgi chat.
func (s *Store) InsertQueuedPrompt(ctx context.Context, p *model.QueuedPrompt) error {
	data, err := mustJSON(p)
	if err != nil {
		return fmt.Errorf("encode queued prompt: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO queued_prompts(id, seq, scope, data, created_at)
		VALUES(?, ?, ?, ?, ?)
	`, p.ID, p.Seq, p.Scope, data, p.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("insert queued prompt %s: %w", p.ID, err)
	}
	return nil
}

// GetQueuedPrompt loads one queued prompt by ID.
func (s *Store) GetQueuedPrompt(ctx context.Context, id string) (*model.QueuedPrompt, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM queued_prompts WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get queued prompt %s: %w", id, err)
	}
	var p model.QueuedPrompt
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("decode queued prompt %s: %w", id, err)
	}
	return &p, nil
}

// DeleteQueuedPrompt removes one queued prompt by ID.
func (s *Store) DeleteQueuedPrompt(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM queued_prompts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete queued prompt %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteQueuedPromptsByScope removes all queued prompts belonging to one scope (e.g. a closed pane).
func (s *Store) DeleteQueuedPromptsByScope(ctx context.Context, scope string) error {
	if scope == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM queued_prompts WHERE scope = ?`, scope)
	if err != nil {
		return fmt.Errorf("delete queued prompts for %s: %w", scope, err)
	}
	return nil
}

// ListQueuedPrompts returns queued prompts ordered oldest-first (filtered by scope if non-empty).
func (s *Store) ListQueuedPrompts(ctx context.Context, scope string) ([]*model.QueuedPrompt, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if scope == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM queued_prompts ORDER BY seq ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM queued_prompts WHERE scope = ? ORDER BY seq ASC`, scope)
	}
	if err != nil {
		return nil, fmt.Errorf("list queued prompts: %w", err)
	}
	defer rows.Close()
	out := []*model.QueuedPrompt{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p model.QueuedPrompt
		if json.Unmarshal([]byte(raw), &p) == nil {
			out = append(out, &p)
		}
	}
	return out, rows.Err()
}

// FindQueuedPromptByClientMsgID returns an existing queued prompt matching scope and clientMsgID, if any.
func (s *Store) FindQueuedPromptByClientMsgID(ctx context.Context, scope, clientMsgID string) (*model.QueuedPrompt, error) {
	if clientMsgID == "" {
		return nil, ErrNotFound
	}
	items, err := s.ListQueuedPrompts(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, p := range items {
		if p.ClientMsgID == clientMsgID {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

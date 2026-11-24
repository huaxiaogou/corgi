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

func mustJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- Entries

// InsertEntry inserts one conversation entry incrementally.
func (s *Store) InsertEntry(ctx context.Context, e *model.Entry) error {
	data, err := mustJSON(e)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO entries(id, seq, thread, role, channel, text, action_id, pinned, data, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			text = excluded.text,
			action_id = excluded.action_id,
			pinned = excluded.pinned,
			data = excluded.data
	`, e.ID, e.Seq, e.Thread, e.Role, e.Channel, e.Text, e.ActionID, boolInt(e.Pinned), data, e.At.Unix())
	if err != nil {
		return fmt.Errorf("insert entry %s: %w", e.ID, err)
	}
	return nil
}

// GetEntry reads one entry by ID.
func (s *Store) GetEntry(ctx context.Context, id string) (*model.Entry, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM entries WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get entry %s: %w", id, err)
	}
	var e model.Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, fmt.Errorf("decode entry %s: %w", id, err)
	}
	return &e, nil
}

// ListEntries returns the newest limit entries (ordered oldest → newest) and whether earlier rows exist.
// If thread is empty, returns entries across all threads.
func (s *Store) ListEntries(ctx context.Context, thread string, limit int) ([]*model.Entry, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if thread == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM entries ORDER BY seq DESC LIMIT ?`, limit+1)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM entries WHERE thread = ? ORDER BY seq DESC LIMIT ?`, thread, limit+1)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	return scanEntriesReversed(rows, limit)
}

// ListEntriesBefore paginates entries older than beforeSeq (ordered oldest → newest).
func (s *Store) ListEntriesBefore(ctx context.Context, thread string, beforeSeq int64, limit int) ([]*model.Entry, bool, error) {
	if limit <= 0 {
		limit = 60
	}
	var (
		rows *sql.Rows
		err  error
	)
	if thread == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM entries WHERE seq < ? ORDER BY seq DESC LIMIT ?`, beforeSeq, limit+1)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT data FROM entries WHERE thread = ? AND seq < ? ORDER BY seq DESC LIMIT ?`, thread, beforeSeq, limit+1)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list entries before %d: %w", beforeSeq, err)
	}
	defer rows.Close()
	return scanEntriesReversed(rows, limit)
}

func scanEntriesReversed(rows *sql.Rows, limit int) ([]*model.Entry, bool, error) {
	var out []*model.Entry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		var e model.Entry
		if json.Unmarshal([]byte(raw), &e) == nil {
			out = append(out, &e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []*model.Entry{}
	}
	return out, more, nil
}

// SetEntryPinned updates the pinned state of one entry.
func (s *Store) SetEntryPinned(ctx context.Context, id string, pinned bool) error {
	e, err := s.GetEntry(ctx, id)
	if err != nil {
		return err
	}
	e.Pinned = pinned
	return s.InsertEntry(ctx, e)
}

// ListPinnedEntries returns pinned entries (newest first).
func (s *Store) ListPinnedEntries(ctx context.Context, limit int) ([]*model.Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM entries WHERE pinned = 1 ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pinned entries: %w", err)
	}
	defer rows.Close()
	out := []*model.Entry{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e model.Entry
		if json.Unmarshal([]byte(raw), &e) == nil {
			out = append(out, &e)
		}
	}
	return out, rows.Err()
}

// SearchEntries performs multi-term case-insensitive search over user/corgi messages.
func (s *Store) SearchEntries(ctx context.Context, thread, query string, limit int) (model.SearchResult, error) {
	res := model.SearchResult{Query: strings.TrimSpace(query), Results: []model.SearchHit{}}
	terms := strings.Fields(strings.ToLower(res.Query))
	if len(terms) == 0 {
		return res, nil
	}
	if limit <= 0 {
		limit = 40
	}
	var conds []string
	var args []any
	conds = append(conds, `role IN ('user', 'corgi')`)
	if thread != "" {
		conds = append(conds, `thread = ?`)
		args = append(args, thread)
	}
	for _, term := range terms {
		conds = append(conds, `LOWER(text) LIKE ?`)
		args = append(args, "%"+term+"%")
	}
	q := `SELECT id, role, thread, text, created_at FROM entries WHERE ` + strings.Join(conds, " AND ") + ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("search entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			hit model.SearchHit
			txt string
			at  int64
		)
		if err := rows.Scan(&hit.ID, &hit.Role, &hit.Thread, &txt, &at); err != nil {
			return res, err
		}
		hit.At = time.Unix(at, 0)
		hit.Snippet = snippet(txt, 140)
		res.Results = append(res.Results, hit)
	}
	if len(res.Results) > limit {
		res.HasMore = true
		res.Results = res.Results[:limit]
	}
	res.Total = len(res.Results)
	return res, rows.Err()
}

func snippet(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

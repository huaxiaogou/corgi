package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

// ---------------------------------------------------------------- Workers, Threads, Timers, Devices, History

// UpsertWorker saves or updates one worker record.
func (s *Store) UpsertWorker(ctx context.Context, w *model.Worker) error {
	data, err := mustJSON(w)
	if err != nil {
		return fmt.Errorf("encode worker: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workers(name, machine, ended, data, created_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			machine = excluded.machine,
			ended = excluded.ended,
			data = excluded.data
	`, w.Name, w.Machine, boolInt(w.Ended), data, w.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert worker %s: %w", w.Name, err)
	}
	return nil
}

// GetWorker reads one worker by name.
func (s *Store) GetWorker(ctx context.Context, name string) (*model.Worker, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM workers WHERE name = ?`, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get worker %s: %w", name, err)
	}
	var w model.Worker
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return nil, fmt.Errorf("decode worker %s: %w", name, err)
	}
	return &w, nil
}

// ListWorkers returns all workers ordered newest first.
func (s *Store) ListWorkers(ctx context.Context) ([]*model.Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM workers ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()
	out := []*model.Worker{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var w model.Worker
		if json.Unmarshal([]byte(raw), &w) == nil {
			out = append(out, &w)
		}
	}
	return out, rows.Err()
}

// UpsertThread saves or updates one conversation thread.
func (s *Store) UpsertThread(ctx context.Context, th *model.Thread) error {
	data, err := mustJSON(th)
	if err != nil {
		return fmt.Errorf("encode thread: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO threads(id, channel, chat_id, last_at, data)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			channel = excluded.channel,
			chat_id = excluded.chat_id,
			last_at = excluded.last_at,
			data = excluded.data
	`, th.ID, th.Channel, th.ChatID, th.LastAt.Unix(), data)
	if err != nil {
		return fmt.Errorf("upsert thread %s: %w", th.ID, err)
	}
	return nil
}

// GetThread loads one thread by ID.
func (s *Store) GetThread(ctx context.Context, id string) (*model.Thread, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM threads WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get thread %s: %w", id, err)
	}
	var th model.Thread
	if err := json.Unmarshal([]byte(raw), &th); err != nil {
		return nil, fmt.Errorf("decode thread %s: %w", id, err)
	}
	return &th, nil
}

// ListThreads returns recent threads ordered by last activity.
func (s *Store) ListThreads(ctx context.Context, limit int) ([]*model.Thread, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM threads ORDER BY last_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	out := []*model.Thread{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var th model.Thread
		if json.Unmarshal([]byte(raw), &th) == nil {
			out = append(out, &th)
		}
	}
	return out, rows.Err()
}

// UpsertTimer saves or updates a timer.
func (s *Store) UpsertTimer(ctx context.Context, t *model.Timer) error {
	data, err := mustJSON(t)
	if err != nil {
		return fmt.Errorf("encode timer: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO timers(id, enabled, next_run, data)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			enabled = excluded.enabled,
			next_run = excluded.next_run,
			data = excluded.data
	`, t.ID, boolInt(t.Enabled), t.NextRun.Unix(), data)
	if err != nil {
		return fmt.Errorf("upsert timer %s: %w", t.ID, err)
	}
	return nil
}

// DeleteTimer deletes a timer by ID.
func (s *Store) DeleteTimer(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete timer %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTimers returns all timers ordered by next_run.
func (s *Store) ListTimers(ctx context.Context) ([]*model.Timer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM timers ORDER BY next_run ASC`)
	if err != nil {
		return nil, fmt.Errorf("list timers: %w", err)
	}
	defer rows.Close()
	out := []*model.Timer{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t model.Timer
		if json.Unmarshal([]byte(raw), &t) == nil {
			out = append(out, &t)
		}
	}
	return out, rows.Err()
}

// UpsertDevice saves a paired browser or computer node.
func (s *Store) UpsertDevice(ctx context.Context, d *model.Device) error {
	data, err := mustJSON(d)
	if err != nil {
		return fmt.Errorf("encode device: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO devices(id, kind, token_hash, data, last_seen)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			data = excluded.data,
			last_seen = excluded.last_seen
	`, d.ID, d.Kind, d.TokenHash, data, d.LastSeen.Unix())
	if err != nil {
		return fmt.Errorf("upsert device %s: %w", d.ID, err)
	}
	return nil
}

// FindDeviceByHash looks up a paired device by its SHA-256 token hash.
func (s *Store) FindDeviceByHash(ctx context.Context, hash string) (*model.Device, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM devices WHERE token_hash = ?`, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find device: %w", err)
	}
	var d model.Device
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("decode device: %w", err)
	}
	d.TokenHash = hash
	return &d, nil
}

// TouchDevice updates the last_seen timestamp of one device.
func (s *Store) TouchDevice(ctx context.Context, id string, at time.Time) error {
	var raw string
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, data FROM devices WHERE id = ?`, id).Scan(&hash, &raw)
	if err != nil {
		return err
	}
	var d model.Device
	if json.Unmarshal([]byte(raw), &d) != nil {
		return nil
	}
	d.TokenHash = hash
	d.LastSeen = at
	return s.UpsertDevice(ctx, &d)
}

// ListDevices lists all paired devices ordered by creation time.
func (s *Store) ListDevices(ctx context.Context) ([]*model.Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash, data FROM devices ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	out := []*model.Device{}
	for rows.Next() {
		var hash, raw string
		if err := rows.Scan(&hash, &raw); err != nil {
			return nil, err
		}
		var d model.Device
		if json.Unmarshal([]byte(raw), &d) == nil {
			d.TokenHash = hash
			out = append(out, &d)
		}
	}
	return out, rows.Err()
}

// DeleteDevice revokes a paired device by ID.
func (s *Store) DeleteDevice(ctx context.Context, id string) (*model.Device, error) {
	var hash, raw string
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, data FROM devices WHERE id = ?`, id).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read device %s: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete device %s: %w", id, err)
	}
	var d model.Device
	_ = json.Unmarshal([]byte(raw), &d)
	d.TokenHash = hash
	return &d, nil
}

// SaveEndedSession stores an archived session and trims old rows beyond maxKeep.
func (s *Store) SaveEndedSession(ctx context.Context, e *model.EndedSession, maxKeep int) error {
	if maxKeep <= 0 {
		maxKeep = 40
	}
	data, err := mustJSON(e)
	if err != nil {
		return fmt.Errorf("encode ended session: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM ended_sessions WHERE kind = ? AND machine = ? AND session_id = ?`,
		e.Kind, e.Machine, e.Session)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO ended_sessions(id, kind, machine, session_id, data, ended_at)
		VALUES(?, ?, ?, ?, ?, ?)
	`, e.ID, e.Kind, e.Machine, e.Session, data, e.EndedAt.Unix()); err != nil {
		return fmt.Errorf("insert ended session: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `
		DELETE FROM ended_sessions WHERE id NOT IN (
			SELECT id FROM ended_sessions ORDER BY ended_at DESC LIMIT ?
		)
	`, maxKeep)
	return nil
}

// ListEndedSessions returns archived ended sessions newest first.
func (s *Store) ListEndedSessions(ctx context.Context, limit int) ([]*model.EndedSession, error) {
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM ended_sessions ORDER BY ended_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list ended sessions: %w", err)
	}
	defer rows.Close()
	out := []*model.EndedSession{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e model.EndedSession
		if json.Unmarshal([]byte(raw), &e) == nil {
			out = append(out, &e)
		}
	}
	return out, rows.Err()
}

// DeleteEndedSession removes one archived session by ID.
func (s *Store) DeleteEndedSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ended_sessions WHERE id = ?`, id)
	return err
}

// ForgetEndedSession removes an archived session once it becomes active again.
func (s *Store) ForgetEndedSession(ctx context.Context, kind, machine, session string) error {
	if session == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM ended_sessions WHERE kind = ? AND machine = ? AND session_id = ?`,
		kind, machine, session)
	return err
}

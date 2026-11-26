package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/kiperjing/corgi/internal/model"
	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("record not found")
	// ErrActionAlreadyResolved is returned when an action was already resolved or expired.
	ErrActionAlreadyResolved = errors.New("action already resolved or expired")
	// ErrDuplicateTrigger is returned when a trigger with the same non-empty idempotency key exists.
	ErrDuplicateTrigger = errors.New("duplicate trigger")
)

const currentSchemaVersion = 4

var migrations = []struct {
	version int
	sql     string
}{
	{
		version: 1,
		sql: `
CREATE TABLE IF NOT EXISTS kv (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS entries (
	id         TEXT PRIMARY KEY,
	seq        INTEGER NOT NULL,
	thread     TEXT NOT NULL,
	role       TEXT NOT NULL,
	channel    TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL,
	action_id  TEXT NOT NULL DEFAULT '',
	pinned     INTEGER NOT NULL DEFAULT 0,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entries_thread_seq ON entries(thread, seq);
CREATE INDEX IF NOT EXISTS idx_entries_pinned ON entries(pinned, seq);

CREATE TABLE IF NOT EXISTS triggers (
	id         TEXT PRIMARY KEY,
	seq        INTEGER NOT NULL,
	source     TEXT NOT NULL,
	thread     TEXT NOT NULL,
	status     TEXT NOT NULL,
	idem_key   TEXT NOT NULL DEFAULT '',
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_triggers_status_seq ON triggers(status, seq);
CREATE INDEX IF NOT EXISTS idx_triggers_thread_seq ON triggers(thread, seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_triggers_idem ON triggers(idem_key) WHERE idem_key != '';

CREATE TABLE IF NOT EXISTS actions (
	id            TEXT PRIMARY KEY,
	kind          TEXT NOT NULL,
	thread        TEXT NOT NULL,
	target_worker TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	data          TEXT NOT NULL,
	created_at    INTEGER NOT NULL,
	resolved_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_actions_status ON actions(status, created_at);
CREATE INDEX IF NOT EXISTS idx_actions_worker ON actions(target_worker, status);

CREATE TABLE IF NOT EXISTS workers (
	name       TEXT PRIMARY KEY,
	machine    TEXT NOT NULL DEFAULT '',
	ended      INTEGER NOT NULL DEFAULT 0,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS threads (
	id       TEXT PRIMARY KEY,
	channel  TEXT NOT NULL DEFAULT '',
	chat_id  TEXT NOT NULL DEFAULT '',
	last_at  INTEGER NOT NULL,
	data     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_last_at ON threads(last_at DESC);

CREATE TABLE IF NOT EXISTS timers (
	id       TEXT PRIMARY KEY,
	enabled  INTEGER NOT NULL DEFAULT 1,
	next_run INTEGER NOT NULL,
	data     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	token_hash TEXT UNIQUE NOT NULL,
	data       TEXT NOT NULL,
	last_seen  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ended_sessions (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	machine    TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL,
	data       TEXT NOT NULL,
	ended_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ended_sessions_at ON ended_sessions(ended_at DESC);
`,
	},
	{
		version: 2,
		sql: `
CREATE TABLE IF NOT EXISTS queued_prompts (
	id         TEXT PRIMARY KEY,
	seq        INTEGER NOT NULL,
	scope      TEXT NOT NULL,
	data       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queued_prompts_scope_seq ON queued_prompts(scope, seq);
`,
	},
	{
		version: 3,
		sql: `
CREATE TABLE IF NOT EXISTS work_items (
	id             TEXT PRIMARY KEY,
	thread         TEXT NOT NULL,
	title          TEXT NOT NULL,
	next_step      TEXT NOT NULL DEFAULT '',
	workers_json   TEXT NOT NULL DEFAULT '[]',
	branches_json  TEXT NOT NULL DEFAULT '[]',
	artifacts_json TEXT NOT NULL DEFAULT '[]',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL,
	closed_at      INTEGER NOT NULL DEFAULT 0,
	close_note     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_work_items_open ON work_items(closed_at, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_work_items_thread ON work_items(thread, closed_at, updated_at DESC);
`,
	},
	{
		version: 4,
		sql: `
CREATE TABLE IF NOT EXISTS artifacts (
	id             TEXT PRIMARY KEY,
	entry_id       TEXT NOT NULL,
	ordinal        INTEGER NOT NULL DEFAULT 0,
	thread         TEXT NOT NULL DEFAULT '',
	work_item_id   TEXT NOT NULL DEFAULT '',
	name           TEXT NOT NULL,
	kind           TEXT NOT NULL,
	mime           TEXT NOT NULL DEFAULT '',
	size           INTEGER NOT NULL DEFAULT 0,
	source_path    TEXT NOT NULL DEFAULT '',
	source_machine TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	share_expires  INTEGER NOT NULL DEFAULT 0,
	expires_at     INTEGER NOT NULL DEFAULT 0,
	revoked_at     INTEGER NOT NULL DEFAULT 0,
	UNIQUE(entry_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_artifacts_work ON artifacts(work_item_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_artifacts_expires ON artifacts(expires_at);
`,
	},
}

// Store provides incremental, WAL-backed SQLite persistence for Corgi.
type Store struct {
	db    *sql.DB
	seqMu sync.Mutex
	seq   int64
}

// Open opens (or creates) the SQLite database at path and applies schema migrations.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.initSeq(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init sequence: %w", err)
	}
	return s, nil
}

// Close closes the underlying SQLite connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= ver {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("set user_version=%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", m.version, err)
		}
		ver = m.version
	}
	if ver != currentSchemaVersion {
		return fmt.Errorf("unexpected schema version %d (want %d)", ver, currentSchemaVersion)
	}
	return nil
}

func (s *Store) initSeq() error {
	var raw string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k = 'seq'`).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if raw != "" {
		s.seq, _ = strconv.ParseInt(raw, 10, 64)
	}
	return nil
}

// NextID atomically increments and persists the monotonic sequence number, returning prefix+seq and seq.
func (s *Store) NextID(ctx context.Context, prefix string) (string, int64, error) {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	next := s.seq + 1
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(k, v) VALUES('seq', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(next, 10),
	); err != nil {
		return "", 0, fmt.Errorf("persist seq: %w", err)
	}
	s.seq = next
	return prefix + strconv.FormatInt(next, 10), next, nil
}

// GetKV reads a raw string value from the kv table.
func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get kv %q: %w", key, err)
	}
	return val, true, nil
}

// SetKV upserts a raw string value into the kv table.
func (s *Store) SetKV(ctx context.Context, key, val string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		key, val,
	); err != nil {
		return fmt.Errorf("set kv %q: %w", key, err)
	}
	return nil
}

// DeleteKV removes a key from the kv table.
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, key); err != nil {
		return fmt.Errorf("delete kv %q: %w", key, err)
	}
	return nil
}

// CommitActiveDreamFinish atomically verifies that kv['active_dream'] still belongs to runID,
// removes 'active_dream', and upserts the provided kv updates in one SQLite transaction.
// Returns committed=false when another run or prior finalization already cleared active_dream.
func (s *Store) CommitActiveDreamFinish(ctx context.Context, runID string, updates map[string]string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin dream finish tx: %w", err)
	}
	defer tx.Rollback()

	var raw string
	err = tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = 'active_dream'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read active_dream: %w", err)
	}
	var marker struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &marker); err != nil || marker.ID != runID {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE k = 'active_dream'`); err != nil {
		return false, fmt.Errorf("delete active_dream: %w", err)
	}
	for k, v := range updates {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
			k, v,
		); err != nil {
			return false, fmt.Errorf("upsert kv %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit dream finish tx: %w", err)
	}
	return true, nil
}

// LoadSettings loads Settings from kv, falling back to DefaultSettings.
func (s *Store) LoadSettings(ctx context.Context, home string) (model.Settings, error) {
	st := model.DefaultSettings(home)
	raw, ok, err := s.GetKV(ctx, "settings")
	if err != nil || !ok {
		return st, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return st, fmt.Errorf("decode settings: %w", err)
	}
	if st.ReplyChars <= 0 {
		st.ReplyChars = model.DefaultReplyChars
	}
	if st.StallMinutes <= 0 {
		st.StallMinutes = 12
	}
	if st.IdlePatrolMinutes == 0 {
		st.IdlePatrolMinutes = 30
	}
	if st.ActiveHours == "" {
		st.ActiveHours = "09:00-22:30"
	}
	if st.StaleArchiveDays == 0 {
		st.StaleArchiveDays = 3
	}
	if st.NotifyLevel != "normal" {
		st.NotifyLevel = "quiet"
	}
	if st.WorkerCwd == "" {
		st.WorkerCwd = home
	}
	return st, nil
}

// SaveSettings persists Settings into kv.
func (s *Store) SaveSettings(ctx context.Context, st model.Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	return s.SetKV(ctx, "settings", string(b))
}

// LoadPolicies loads fast-path source policies from kv, falling back to defaults.
func (s *Store) LoadPolicies(ctx context.Context) ([]model.Policy, error) {
	raw, ok, err := s.GetKV(ctx, "policies")
	if err != nil || !ok {
		return model.DefaultPolicies(), err
	}
	var out []model.Policy
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return model.DefaultPolicies(), fmt.Errorf("decode policies: %w", err)
	}
	return out, nil
}

// SavePolicies persists source policies into kv.
func (s *Store) SavePolicies(ctx context.Context, policies []model.Policy) error {
	b, err := json.Marshal(policies)
	if err != nil {
		return fmt.Errorf("encode policies: %w", err)
	}
	return s.SetKV(ctx, "policies", string(b))
}

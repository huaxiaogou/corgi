package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

// ErrWorkItemClosed is returned when attempting to mutate or attach work to an already-closed work item.
var ErrWorkItemClosed = errors.New("work item already closed")

// CreateWorkItem inserts a new durable work item.
func (s *Store) CreateWorkItem(ctx context.Context, wi *model.WorkItem) error {
	now := time.Now()
	if wi.CreatedAt.IsZero() {
		wi.CreatedAt = now
	}
	if wi.UpdatedAt.IsZero() {
		wi.UpdatedAt = now
	}
	if wi.Thread == "" {
		wi.Thread = model.MainThread
	}
	workersJSON, branchesJSON, artifactsJSON, err := encodeWorkItemSlices(wi)
	if err != nil {
		return err
	}
	closedAt := int64(0)
	if wi.ClosedAt != nil && !wi.ClosedAt.IsZero() {
		closedAt = wi.ClosedAt.Unix()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO work_items(id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, wi.ID, wi.Thread, wi.Title, wi.NextStep, workersJSON, branchesJSON, artifactsJSON,
		wi.CreatedAt.Unix(), wi.UpdatedAt.Unix(), closedAt, wi.CloseNote)
	if err != nil {
		return fmt.Errorf("create work item %s: %w", wi.ID, err)
	}
	return nil
}

// GetWorkItem loads one work item by ID.
func (s *Store) GetWorkItem(ctx context.Context, id string) (*model.WorkItem, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note
		FROM work_items WHERE id = ?
	`, id)
	wi, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get work item %s: %w", id, err)
	}
	return wi, nil
}

// ListWorkItems returns open work items (or all work items when includeClosed is true), newest-updated first.
func (s *Store) ListWorkItems(ctx context.Context, includeClosed bool) ([]*model.WorkItem, error) {
	q := `
		SELECT id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note
		FROM work_items
	`
	if !includeClosed {
		q += ` WHERE closed_at = 0`
	}
	q += ` ORDER BY closed_at ASC, updated_at DESC, created_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	defer rows.Close()
	out := []*model.WorkItem{}
	for rows.Next() {
		wi, err := scanWorkItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wi)
	}
	return out, rows.Err()
}

// UpdateWorkItemFields updates mutable metadata and merges optional worker/branch references transactionally.
func (s *Store) UpdateWorkItemFields(ctx context.Context, id, title, nextStep, workerName, branch string) (*model.WorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin update work item: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if wi.ClosedAt != nil {
		return nil, ErrWorkItemClosed
	}
	if t := strings.TrimSpace(title); t != "" {
		wi.Title = t
	}
	if n := strings.TrimSpace(nextStep); n != "" {
		wi.NextStep = n
	}
	if w := strings.TrimSpace(workerName); w != "" && !slices.Contains(wi.Workers, w) {
		wi.Workers = append(wi.Workers, w)
	}
	if b := strings.TrimSpace(branch); b != "" && !slices.Contains(wi.Branches, b) {
		wi.Branches = append(wi.Branches, b)
	}
	wi.UpdatedAt = time.Now()
	if err := saveWorkItemTx(ctx, tx, wi); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update work item %s: %w", id, err)
	}
	return wi, nil
}

// CloseWorkItem marks an open work item closed with an explicit closing note.
func (s *Store) CloseWorkItem(ctx context.Context, id, note string) (*model.WorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin close work item: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if wi.ClosedAt != nil {
		return nil, ErrWorkItemClosed
	}
	now := time.Now()
	wi.ClosedAt = &now
	wi.UpdatedAt = now
	wi.CloseNote = strings.TrimSpace(note)
	if err := saveWorkItemTx(ctx, tx, wi); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit close work item %s: %w", id, err)
	}
	return wi, nil
}

// RecordWorkItemBranch transactionally merges branch into a work item's branches list (if open).
func (s *Store) RecordWorkItemBranch(ctx context.Context, id, branch string) error {
	id = strings.TrimSpace(id)
	branch = strings.TrimSpace(branch)
	if id == "" || branch == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if !slices.Contains(wi.Branches, branch) {
		wi.Branches = append(wi.Branches, branch)
		wi.UpdatedAt = time.Now()
		if err := saveWorkItemTx(ctx, tx, wi); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordWorkItemArtifact transactionally records a confirmed delivered file artifact on a work item.
func (s *Store) RecordWorkItemArtifact(ctx context.Context, id string, art model.WorkArtifact) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(art.Path) == "" {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if art.DeliveredAt.IsZero() {
		art.DeliveredAt = time.Now()
	}
	dup := false
	for _, existing := range wi.Artifacts {
		if existing.Path == art.Path && existing.Machine == art.Machine && existing.EntryID == art.EntryID {
			dup = true
			break
		}
	}
	if !dup {
		wi.Artifacts = append(wi.Artifacts, art)
		wi.UpdatedAt = time.Now()
		if err := saveWorkItemTx(ctx, tx, wi); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return wi, nil
}

// FindOpenWorkItemsByWorker returns all open work items that currently list workerName.
func (s *Store) FindOpenWorkItemsByWorker(ctx context.Context, workerName string) ([]*model.WorkItem, error) {
	open, err := s.ListWorkItems(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []*model.WorkItem
	for _, wi := range open {
		if slices.Contains(wi.Workers, workerName) {
			out = append(out, wi)
		}
	}
	return out, nil
}

// AttachWorkerAndUpsert atomically creates or updates a work item, merges the worker name and
// worktree branch inside a single SQLite transaction, sets wk.WorkItemID, and upserts the worker row.
// When workID is empty, newID (pre-allocated via NextID) and createTitle/nextStep are used to create a new item.
func (s *Store) AttachWorkerAndUpsert(ctx context.Context, wk *model.Worker, workID, newID, createTitle, nextStep string) (*model.WorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin attach worker: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	var wi *model.WorkItem
	if strings.TrimSpace(workID) != "" {
		wi, err = getWorkItemTx(ctx, tx, strings.TrimSpace(workID))
		if err != nil {
			return nil, err
		}
		if wi.ClosedAt != nil {
			return nil, fmt.Errorf("work item %s: %w", wi.ID, ErrWorkItemClosed)
		}
		if wk.Name != "" && !slices.Contains(wi.Workers, wk.Name) {
			wi.Workers = append(wi.Workers, wk.Name)
		}
		if wk.WorktreeBranch != "" && !slices.Contains(wi.Branches, wk.WorktreeBranch) {
			wi.Branches = append(wi.Branches, wk.WorktreeBranch)
		}
		wi.UpdatedAt = now
		if err := saveWorkItemTx(ctx, tx, wi); err != nil {
			return nil, err
		}
	} else {
		wi = &model.WorkItem{
			ID:        newID,
			Thread:    wk.Thread,
			Title:     strings.TrimSpace(createTitle),
			NextStep:  strings.TrimSpace(nextStep),
			CreatedAt: now,
			UpdatedAt: now,
		}
		if wi.Thread == "" {
			wi.Thread = model.MainThread
		}
		if wk.Name != "" {
			wi.Workers = []string{wk.Name}
		}
		if wk.WorktreeBranch != "" {
			wi.Branches = []string{wk.WorktreeBranch}
		}
		workersJSON, branchesJSON, artifactsJSON, encErr := encodeWorkItemSlices(wi)
		if encErr != nil {
			return nil, encErr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_items(id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '')
		`, wi.ID, wi.Thread, wi.Title, wi.NextStep, workersJSON, branchesJSON, artifactsJSON,
			wi.CreatedAt.Unix(), wi.UpdatedAt.Unix()); err != nil {
			return nil, fmt.Errorf("insert work item %s: %w", wi.ID, err)
		}
	}

	wk.WorkItemID = wi.ID
	wData, err := mustJSON(wk)
	if err != nil {
		return nil, fmt.Errorf("encode worker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workers(name, machine, ended, data, created_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			machine = excluded.machine,
			ended = excluded.ended,
			data = excluded.data
	`, wk.Name, wk.Machine, boolInt(wk.Ended), wData, wk.CreatedAt.Unix()); err != nil {
		return nil, fmt.Errorf("upsert worker %s: %w", wk.Name, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit attach worker %s: %w", wk.Name, err)
	}
	return wi, nil
}

type workItemScanner interface {
	Scan(dest ...any) error
}

func scanWorkItem(sc workItemScanner) (*model.WorkItem, error) {
	var (
		wi            model.WorkItem
		workersJSON   string
		branchesJSON  string
		artifactsJSON string
		createdAt     int64
		updatedAt     int64
		closedAt      int64
	)
	if err := sc.Scan(
		&wi.ID, &wi.Thread, &wi.Title, &wi.NextStep,
		&workersJSON, &branchesJSON, &artifactsJSON,
		&createdAt, &updatedAt, &closedAt, &wi.CloseNote,
	); err != nil {
		return nil, err
	}
	if createdAt > 0 {
		wi.CreatedAt = time.Unix(createdAt, 0)
	}
	if updatedAt > 0 {
		wi.UpdatedAt = time.Unix(updatedAt, 0)
	}
	if closedAt > 0 {
		t := time.Unix(closedAt, 0)
		wi.ClosedAt = &t
	}
	_ = json.Unmarshal([]byte(workersJSON), &wi.Workers)
	_ = json.Unmarshal([]byte(branchesJSON), &wi.Branches)
	_ = json.Unmarshal([]byte(artifactsJSON), &wi.Artifacts)
	return &wi, nil
}

func getWorkItemTx(ctx context.Context, tx *sql.Tx, id string) (*model.WorkItem, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note
		FROM work_items WHERE id = ?
	`, id)
	wi, err := scanWorkItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read work item %s: %w", id, err)
	}
	return wi, nil
}

func saveWorkItemTx(ctx context.Context, tx *sql.Tx, wi *model.WorkItem) error {
	workersJSON, branchesJSON, artifactsJSON, err := encodeWorkItemSlices(wi)
	if err != nil {
		return err
	}
	closedAt := int64(0)
	if wi.ClosedAt != nil && !wi.ClosedAt.IsZero() {
		closedAt = wi.ClosedAt.Unix()
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE work_items
		SET title = ?, next_step = ?, workers_json = ?, branches_json = ?, artifacts_json = ?, updated_at = ?, closed_at = ?, close_note = ?
		WHERE id = ?
	`, wi.Title, wi.NextStep, workersJSON, branchesJSON, artifactsJSON,
		wi.UpdatedAt.Unix(), closedAt, wi.CloseNote, wi.ID)
	if err != nil {
		return fmt.Errorf("update work item %s: %w", wi.ID, err)
	}
	return nil
}

func encodeWorkItemSlices(wi *model.WorkItem) (workersJSON, branchesJSON, artifactsJSON string, err error) {
	workers := wi.Workers
	if workers == nil {
		workers = []string{}
	}
	branches := wi.Branches
	if branches == nil {
		branches = []string{}
	}
	artifacts := wi.Artifacts
	if artifacts == nil {
		artifacts = []model.WorkArtifact{}
	}
	if workersJSON, err = mustJSON(workers); err != nil {
		return "", "", "", fmt.Errorf("encode work item workers: %w", err)
	}
	if branchesJSON, err = mustJSON(branches); err != nil {
		return "", "", "", fmt.Errorf("encode work item branches: %w", err)
	}
	if artifactsJSON, err = mustJSON(artifacts); err != nil {
		return "", "", "", fmt.Errorf("encode work item artifacts: %w", err)
	}
	return workersJSON, branchesJSON, artifactsJSON, nil
}

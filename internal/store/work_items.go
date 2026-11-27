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

// WorkItem title governance states stored in kv (zero schema migration):
//   - "draft": auto-cleaned from raw prompt; Router LLM should distill a crisp title on next milestone.
//   - "locked": explicitly hand-edited by the human in Web; automated Router/Dream updates preserve it.
//   - "": distilled by Router LLM; Router may freely refine it as the matter evolves.
const (
	WorkItemTitleDraft  = "draft"
	WorkItemTitleLocked = "locked"
)

func workItemTitleStateKey(id string) string { return "wi_title:" + strings.TrimSpace(id) }

// GetWorkItemTitleState returns "draft", "locked", or "" for work item id.
func (s *Store) GetWorkItemTitleState(ctx context.Context, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	v, ok, err := s.GetKV(ctx, workItemTitleStateKey(id))
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// SetWorkItemTitleState persists or clears ("") the title governance state for work item id.
func (s *Store) SetWorkItemTitleState(ctx context.Context, id, state string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return s.DeleteKV(ctx, workItemTitleStateKey(id))
	}
	return s.SetKV(ctx, workItemTitleStateKey(id), state)
}

func workItemScopeKey(id string) string { return "wi_scope:" + strings.TrimSpace(id) }

// GetWorkItemScope loads the optional authorization scope ("", "readonly", "prototype", "implement")
// stored in kv with zero schema migration (mirroring wi_title:* governance metadata).
func (s *Store) GetWorkItemScope(ctx context.Context, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	v, ok, err := s.GetKV(ctx, workItemScopeKey(id))
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// SetWorkItemScope updates the optional authorization scope ("", "readonly", "prototype", "implement")
// for an existing work item in kv (zero schema migration).
func (s *Store) SetWorkItemScope(ctx context.Context, id, scope string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	scope = strings.ToLower(strings.TrimSpace(scope))
	if id == "" {
		return nil, fmt.Errorf("work item id is required")
	}
	switch scope {
	case "", "readonly", "prototype", "implement":
	default:
		return nil, fmt.Errorf("invalid work item scope %q", scope)
	}
	wi, err := s.GetWorkItem(ctx, id)
	if err != nil {
		return nil, err
	}
	if scope == "" {
		if err := s.DeleteKV(ctx, workItemScopeKey(id)); err != nil {
			return nil, err
		}
	} else {
		if err := s.SetKV(ctx, workItemScopeKey(id), scope); err != nil {
			return nil, err
		}
	}
	wi.Scope = scope
	return wi, nil
}

// UnbindWorkerFromWorkItem transactionally detaches workerName from work item id (and clears
// worker.WorkItemID when it still points to id) while preserving all historical branches,
// artifacts, and the live worker pane.
func (s *Store) UnbindWorkerFromWorkItem(ctx context.Context, id, workerName string) (*model.WorkItem, error) {
	id = strings.TrimSpace(id)
	workerName = strings.TrimSpace(workerName)
	if id == "" || workerName == "" {
		return nil, fmt.Errorf("work item id and worker name are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin unbind worker: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(wi.Workers))
	changed := false
	for _, wn := range wi.Workers {
		if wn == workerName {
			changed = true
			continue
		}
		filtered = append(filtered, wn)
	}
	if changed {
		wi.Workers = filtered
		wi.UpdatedAt = time.Now()
		if err := saveWorkItemTx(ctx, tx, wi); err != nil {
			return nil, err
		}
	}

	var rawWk string
	if qErr := tx.QueryRowContext(ctx, `SELECT data FROM workers WHERE name = ?`, workerName).Scan(&rawWk); qErr == nil {
		var wk model.Worker
		if json.Unmarshal([]byte(rawWk), &wk) == nil && wk.WorkItemID == id {
			wk.WorkItemID = ""
			if wkData, mErr := mustJSON(&wk); mErr == nil {
				if _, uErr := tx.ExecContext(ctx, `UPDATE workers SET data = ? WHERE name = ?`, wkData, workerName); uErr != nil {
					return nil, fmt.Errorf("clear worker work_item_id: %w", uErr)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit unbind worker %s from %s: %w", workerName, id, err)
	}
	return wi, nil
}

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
	wi.Scope = s.GetWorkItemScope(ctx, wi.ID)
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
	out := []*model.WorkItem{}
	for rows.Next() {
		wi, err := scanWorkItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, wi)
	}
	rErr := rows.Err()
	_ = rows.Close()
	if rErr != nil {
		return nil, rErr
	}
	for _, wi := range out {
		wi.Scope = s.GetWorkItemScope(ctx, wi.ID)
	}
	return out, nil
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

// CountClosedWorkItems returns the number of closed work items, optionally scoped to a single thread.
func (s *Store) CountClosedWorkItems(ctx context.Context, thread string) (int, error) {
	thread = strings.TrimSpace(thread)
	var (
		n   int
		err error
	)
	if thread != "" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE closed_at > 0 AND thread = ?`, thread).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE closed_at > 0`).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count closed work items: %w", err)
	}
	return n, nil
}

// ListClosedWorkItems returns paginated closed work items (newest closed first), optionally filtered by thread and search query.
func (s *Store) ListClosedWorkItems(ctx context.Context, thread, query string, limit, offset int) ([]*model.WorkItem, int, error) {
	if limit <= 0 {
		limit = 15
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	thread = strings.TrimSpace(thread)
	query = strings.TrimSpace(query)

	where := []string{"closed_at > 0"}
	args := []any{}
	if thread != "" {
		where = append(where, "thread = ?")
		args = append(args, thread)
	}
	if query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(query))
		like := "%" + escaped + "%"
		where = append(where, `(LOWER(title) LIKE ? ESCAPE '\' OR LOWER(next_step) LIKE ? ESCAPE '\' OR LOWER(close_note) LIKE ? ESCAPE '\' OR LOWER(workers_json) LIKE ? ESCAPE '\' OR LOWER(branches_json) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like)
	}
	whereClause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items `+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count filtered closed work items: %w", err)
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, thread, title, next_step, workers_json, branches_json, artifacts_json, created_at, updated_at, closed_at, close_note
		FROM work_items `+whereClause+`
		ORDER BY closed_at DESC, updated_at DESC, created_at DESC
		LIMIT ? OFFSET ?
	`, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list closed work items: %w", err)
	}
	defer rows.Close()
	out := []*model.WorkItem{}
	for rows.Next() {
		wi, err := scanWorkItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, wi)
	}
	return out, total, rows.Err()
}

// ClearWorkItemNextStep clears next_step on an open work item transactionally.
func (s *Store) ClearWorkItemNextStep(ctx context.Context, id string) (*model.WorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin clear work item next_step: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if wi.ClosedAt != nil {
		return nil, ErrWorkItemClosed
	}
	wi.NextStep = ""
	wi.UpdatedAt = time.Now()
	if err := saveWorkItemTx(ctx, tx, wi); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit clear work item next_step %s: %w", id, err)
	}
	return wi, nil
}

// ReopenWorkItem clears closed_at and close_note on a closed work item so it returns to open status.
func (s *Store) ReopenWorkItem(ctx context.Context, id string) (*model.WorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reopen work item: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	wi.ClosedAt = nil
	wi.CloseNote = ""
	wi.UpdatedAt = time.Now()
	if err := saveWorkItemTx(ctx, tx, wi); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reopen work item %s: %w", id, err)
	}
	return wi, nil
}

// CloseWorkItem closes an open work item with an explicit closing note and atomically expires
// any pending or snoozed non-dialog actions owned by that work item in the same transaction.
func (s *Store) CloseWorkItem(ctx context.Context, id, note string) (*model.WorkItem, error) {
	wi, _, err := s.CloseWorkItemWithActions(ctx, id, note)
	return wi, err
}

// CloseWorkItemWithActions closes an open work item and atomically expires its owned pending and
// snoozed actions (excluding live worker_dialog prompts) within a single SQLite transaction.
func (s *Store) CloseWorkItemWithActions(ctx context.Context, id, note string) (*model.WorkItem, []*model.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin close work item: %w", err)
	}
	defer tx.Rollback()

	wi, err := getWorkItemTx(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	if wi.ClosedAt != nil {
		return nil, nil, ErrWorkItemClosed
	}
	now := time.Now()
	wi.ClosedAt = &now
	wi.UpdatedAt = now
	wi.CloseNote = strings.TrimSpace(note)
	if err := saveWorkItemTx(ctx, tx, wi); err != nil {
		return nil, nil, err
	}

	// Atomically expire pending/snoozed non-dialog actions explicitly owned by this WorkItem.
	// Clearing SnoozeUntil prevents WakeDueSnoozedActions from resurrecting cancelled cards across restarts.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, data FROM actions WHERE status IN (?, ?) ORDER BY created_at ASC`,
		model.ActionPending, model.ActionSnoozed,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("query work item actions %s: %w", id, err)
	}
	type rawAct struct {
		id   string
		data string
	}
	var candidates []rawAct
	for rows.Next() {
		var ra rawAct
		if sErr := rows.Scan(&ra.id, &ra.data); sErr != nil {
			rows.Close()
			return nil, nil, sErr
		}
		candidates = append(candidates, ra)
	}
	rows.Close()

	var expired []*model.Action
	for _, ra := range candidates {
		var a model.Action
		if json.Unmarshal([]byte(ra.data), &a) != nil {
			continue
		}
		if a.Kind == model.ActionKindWorkerDialog {
			continue
		}
		if strings.TrimSpace(a.WorkItemID) != wi.ID {
			continue
		}
		a.Status = model.ActionExpired
		a.ResolvedBy = "expired"
		a.ExpiredReason = "work_item_closed"
		a.ResolvedAt = &now
		a.SnoozeUntil = nil
		updated, mErr := mustJSON(&a)
		if mErr != nil {
			return nil, nil, mErr
		}
		res, uErr := tx.ExecContext(ctx, `
			UPDATE actions SET status = ?, data = ?, resolved_at = ?
			WHERE id = ? AND status IN (?, ?)
		`, model.ActionExpired, updated, now.Unix(), a.ID, model.ActionPending, model.ActionSnoozed)
		if uErr != nil {
			return nil, nil, fmt.Errorf("expire action %s for closed work item %s: %w", a.ID, wi.ID, uErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			cp := a
			expired = append(expired, &cp)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit close work item %s: %w", id, err)
	}
	return wi, expired, nil
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
	for i, existing := range wi.Artifacts {
		same := (art.ArtifactID != "" && existing.ArtifactID == art.ArtifactID) ||
			(existing.Path == art.Path && existing.Machine == art.Machine && existing.EntryID == art.EntryID)
		if same {
			if existing.ArtifactID == "" && art.ArtifactID != "" {
				wi.Artifacts[i].ArtifactID = art.ArtifactID
				wi.Artifacts[i].Name = art.Name
				wi.Artifacts[i].Kind = art.Kind
				wi.Artifacts[i].Size = art.Size
				wi.UpdatedAt = time.Now()
				if err := saveWorkItemTx(ctx, tx, wi); err != nil {
					return nil, err
				}
			}
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

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

// RouterContextState tracks the active Append-Only epoch and token telemetry for one thread.
type RouterContextState struct {
	Thread           string
	Epoch            int
	CompactionCount  int
	LastPromptTokens int
	LastCachedTokens int
	TotalEstTokens   int
	UpdatedAt        time.Time
}

const selectActiveContextTurnsSQL = `
SELECT id, seq, thread, epoch, kind, trigger_id, source, work_item_id, work_item_ids,
       user_text, assistant_text, est_tokens, dropped, drop_reason, created_at
FROM router_context_turns
WHERE thread = ? AND epoch = ? AND dropped = 0
ORDER BY seq ASC`

func normContextThread(thread string) string {
	if t := strings.TrimSpace(thread); t != "" {
		return t
	}
	return model.MainThread
}

func scanContextStateRow(row *sql.Row, thread string) (RouterContextState, error) {
	st := RouterContextState{Thread: thread, Epoch: 1}
	var updatedNS int64
	err := row.Scan(&st.Epoch, &st.CompactionCount, &st.LastPromptTokens, &st.LastCachedTokens, &st.TotalEstTokens, &updatedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return RouterContextState{}, fmt.Errorf("read router context state %s: %w", thread, err)
	}
	if st.Epoch < 1 {
		st.Epoch = 1
	}
	if updatedNS > 0 {
		st.UpdatedAt = time.Unix(0, updatedNS)
	}
	return st, nil
}

func readContextStateTx(ctx context.Context, tx *sql.Tx, thread string) (RouterContextState, error) {
	return scanContextStateRow(tx.QueryRowContext(ctx,
		`SELECT epoch, compaction_count, last_prompt_tokens, last_cached_tokens, total_est_tokens, updated_at
		 FROM router_context_state WHERE thread = ?`, thread), thread)
}

func upsertContextStateTx(ctx context.Context, tx *sql.Tx, st RouterContextState) error {
	if st.Epoch < 1 {
		st.Epoch = 1
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO router_context_state(thread, epoch, compaction_count, last_prompt_tokens, last_cached_tokens, total_est_tokens, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(thread) DO UPDATE SET
		   epoch = excluded.epoch, compaction_count = excluded.compaction_count,
		   last_prompt_tokens = excluded.last_prompt_tokens, last_cached_tokens = excluded.last_cached_tokens,
		   total_est_tokens = excluded.total_est_tokens, updated_at = excluded.updated_at`,
		st.Thread, st.Epoch, st.CompactionCount, st.LastPromptTokens, st.LastCachedTokens, st.TotalEstTokens, st.UpdatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("upsert router context state %s: %w", st.Thread, err)
	}
	return nil
}

func recalcActiveEstTokensTx(ctx context.Context, tx *sql.Tx, thread string, epoch int) (int, error) {
	var total int
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(est_tokens), 0) FROM router_context_turns WHERE thread = ? AND epoch = ? AND dropped = 0`,
		thread, epoch,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("recalc router context est tokens: %w", err)
	}
	return total, nil
}

func encodeWorkItemIDs(primary string, all []string) (string, string) {
	primary = strings.TrimSpace(primary)
	var out []string
	if primary != "" {
		out = append(out, primary)
	}
	for _, raw := range all {
		if id := strings.TrimSpace(raw); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if primary == "" && len(out) > 0 {
		primary = out[0]
	}
	if len(out) == 0 {
		return primary, "[]"
	}
	b, _ := json.Marshal(out)
	return primary, string(b)
}

func decodeWorkItemIDs(primary, rawJSON string) []string {
	var list []string
	if strings.TrimSpace(rawJSON) != "" {
		_ = json.Unmarshal([]byte(rawJSON), &list)
	}
	if primary = strings.TrimSpace(primary); primary != "" && !slices.Contains(list, primary) {
		list = append([]string{primary}, list...)
	}
	return list
}

func scanRouterContextTurns(rows *sql.Rows) ([]*model.RouterContextTurn, error) {
	var out []*model.RouterContextTurn
	for rows.Next() {
		var (
			t         model.RouterContextTurn
			idsJSON   string
			dropped   int
			createdNS int64
		)
		if err := rows.Scan(
			&t.ID, &t.Seq, &t.Thread, &t.Epoch, &t.Kind, &t.TriggerID, &t.Source,
			&t.WorkItemID, &idsJSON, &t.UserText, &t.AssistantText, &t.EstTokens,
			&dropped, &t.DropReason, &createdNS,
		); err != nil {
			return nil, fmt.Errorf("scan router context turn: %w", err)
		}
		t.Dropped = dropped != 0
		t.WorkItemIDs = decodeWorkItemIDs(t.WorkItemID, idsJSON)
		if createdNS > 0 {
			t.CreatedAt = time.Unix(0, createdNS)
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// GetRouterContextState loads the thread's active Append-Only context state (default epoch 1).
func (s *Store) GetRouterContextState(ctx context.Context, thread string) (RouterContextState, error) {
	thread = normContextThread(thread)
	return scanContextStateRow(s.db.QueryRowContext(ctx,
		`SELECT epoch, compaction_count, last_prompt_tokens, last_cached_tokens, total_est_tokens, updated_at
		 FROM router_context_state WHERE thread = ?`, thread), thread)
}

// ListActiveRouterContextTurns lists non-dropped frozen turns for thread+epoch ordered by seq ASC.
func (s *Store) ListActiveRouterContextTurns(ctx context.Context, thread string, epoch int) ([]*model.RouterContextTurn, error) {
	thread, epoch = normContextThread(thread), max(epoch, 1)
	rows, err := s.db.QueryContext(ctx, selectActiveContextTurnsSQL, thread, epoch)
	if err != nil {
		return nil, fmt.Errorf("list active router context turns: %w", err)
	}
	defer rows.Close()
	return scanRouterContextTurns(rows)
}

// CountActiveRouterContextTurns returns the count of non-dropped turns in thread+epoch.
func (s *Store) CountActiveRouterContextTurns(ctx context.Context, thread string, epoch int) (int, error) {
	thread, epoch = normContextThread(thread), max(epoch, 1)
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM router_context_turns WHERE thread = ? AND epoch = ? AND dropped = 0`,
		thread, epoch,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active router context turns: %w", err)
	}
	return n, nil
}

// AppendRouterContextTurn atomically appends one frozen turn into the thread's live epoch and updates usage.
func (s *Store) AppendRouterContextTurn(ctx context.Context, turn *model.RouterContextTurn, promptTokens, cachedTokens int) (RouterContextState, error) {
	if turn == nil {
		return RouterContextState{}, fmt.Errorf("nil router context turn")
	}
	thread := normContextThread(turn.Thread)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RouterContextState{}, fmt.Errorf("begin append router context turn: %w", err)
	}
	defer tx.Rollback()

	st, err := readContextStateTx(ctx, tx, thread)
	if err != nil {
		return RouterContextState{}, err
	}
	primaryWI, idsJSON := encodeWorkItemIDs(turn.WorkItemID, turn.WorkItemIDs)
	kind := strings.TrimSpace(turn.Kind)
	if kind == "" {
		kind = "turn"
	}
	createdAt := turn.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO router_context_turns(
			id, seq, thread, epoch, kind, trigger_id, source, work_item_id, work_item_ids,
			user_text, assistant_text, est_tokens, dropped, drop_reason, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?)`,
		turn.ID, turn.Seq, thread, st.Epoch, kind, turn.TriggerID, turn.Source,
		primaryWI, idsJSON, turn.UserText, turn.AssistantText, turn.EstTokens, createdAt.UnixNano(),
	); err != nil {
		return RouterContextState{}, fmt.Errorf("insert router context turn %s: %w", turn.ID, err)
	}
	if st.TotalEstTokens, err = recalcActiveEstTokensTx(ctx, tx, thread, st.Epoch); err != nil {
		return RouterContextState{}, err
	}
	if promptTokens > 0 && (turn.Epoch <= 0 || turn.Epoch == st.Epoch) {
		st.LastPromptTokens, st.LastCachedTokens = promptTokens, cachedTokens
	}
	st.UpdatedAt = time.Now()
	if err := upsertContextStateTx(ctx, tx, st); err != nil {
		return RouterContextState{}, err
	}
	if err := tx.Commit(); err != nil {
		return RouterContextState{}, fmt.Errorf("commit append router context turn: %w", err)
	}
	return st, nil
}

func (s *Store) dropContextTurnsSelect(ctx context.Context, thread, reason string, choose func([]*model.RouterContextTurn) ([]string, int)) (int, int, RouterContextState, error) {
	thread = normContextThread(thread)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("begin drop router context turns: %w", err)
	}
	defer tx.Rollback()

	st, err := readContextStateTx(ctx, tx, thread)
	if err != nil {
		return 0, 0, RouterContextState{}, err
	}
	rows, err := tx.QueryContext(ctx, selectActiveContextTurnsSQL, thread, st.Epoch)
	if err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("query active turns for drop: %w", err)
	}
	turns, sErr := scanRouterContextTurns(rows)
	rows.Close()
	if sErr != nil {
		return 0, 0, RouterContextState{}, sErr
	}
	dropIDs, freed := choose(turns)
	if len(dropIDs) == 0 {
		return 0, 0, st, nil
	}
	for _, id := range dropIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE router_context_turns SET dropped = 1, drop_reason = ? WHERE id = ? AND thread = ? AND epoch = ?`,
			reason, id, thread, st.Epoch,
		); err != nil {
			return 0, 0, RouterContextState{}, fmt.Errorf("drop router context turn %s: %w", id, err)
		}
	}
	if st.TotalEstTokens, err = recalcActiveEstTokensTx(ctx, tx, thread, st.Epoch); err != nil {
		return 0, 0, RouterContextState{}, err
	}
	st.LastPromptTokens, st.LastCachedTokens, st.UpdatedAt = 0, 0, time.Now()
	if err := upsertContextStateTx(ctx, tx, st); err != nil {
		return 0, 0, RouterContextState{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("commit drop router context turns: %w", err)
	}
	return len(dropIDs), freed, st, nil
}

// DropRouterContextTurnsByWorkItem soft-drops active turns referencing workItemID.
func (s *Store) DropRouterContextTurnsByWorkItem(ctx context.Context, thread, workItemID, reason string) (int, int, RouterContextState, error) {
	workItemID = strings.TrimSpace(workItemID)
	if workItemID == "" {
		st, err := s.GetRouterContextState(ctx, thread)
		return 0, 0, st, err
	}
	return s.dropContextTurnsSelect(ctx, thread, reason, func(turns []*model.RouterContextTurn) ([]string, int) {
		var ids []string
		freed := 0
		for _, t := range turns {
			if t.Kind != "summary" && (t.WorkItemID == workItemID || slices.Contains(t.WorkItemIDs, workItemID)) {
				ids, freed = append(ids, t.ID), freed+t.EstTokens
			}
		}
		return ids, freed
	})
}

// DropRouterContextTurnsByIDs soft-drops specific active turn IDs in thread's current epoch.
func (s *Store) DropRouterContextTurnsByIDs(ctx context.Context, thread string, ids []string, reason string) (int, int, RouterContextState, error) {
	want := make(map[string]bool, len(ids))
	for _, raw := range ids {
		if id := strings.TrimSpace(raw); id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		st, err := s.GetRouterContextState(ctx, thread)
		return 0, 0, st, err
	}
	return s.dropContextTurnsSelect(ctx, thread, reason, func(turns []*model.RouterContextTurn) ([]string, int) {
		var matched []string
		freed := 0
		for _, t := range turns {
			if want[t.ID] {
				matched, freed = append(matched, t.ID), freed+t.EstTokens
			}
		}
		return matched, freed
	})
}

// DropRouterContextTurnsBeforeSeq soft-drops active non-summary turns with seq < beforeSeq.
func (s *Store) DropRouterContextTurnsBeforeSeq(ctx context.Context, thread string, beforeSeq int64, reason string) (int, int, RouterContextState, error) {
	if beforeSeq <= 0 {
		st, err := s.GetRouterContextState(ctx, thread)
		return 0, 0, st, err
	}
	return s.dropContextTurnsSelect(ctx, thread, reason, func(turns []*model.RouterContextTurn) ([]string, int) {
		var ids []string
		freed := 0
		for _, t := range turns {
			if t.Kind != "summary" && t.Seq < beforeSeq {
				ids, freed = append(ids, t.ID), freed+t.EstTokens
			}
		}
		return ids, freed
	})
}

// DropOldestRouterContextTurns deterministically drops oldest turns until <= targetTokens while preserving keepTail.
func (s *Store) DropOldestRouterContextTurns(ctx context.Context, thread string, targetTokens, keepTail int, reason string) (int, int, RouterContextState, error) {
	targetTokens, keepTail = max(targetTokens, 0), max(keepTail, 0)
	return s.dropContextTurnsSelect(ctx, thread, reason, func(turns []*model.RouterContextTurn) ([]string, int) {
		total := 0
		for _, t := range turns {
			total += t.EstTokens
		}
		maxDrop := max(len(turns)-keepTail, 0)
		var ids []string
		freed := 0
		for i := 0; i < maxDrop && total-freed > targetTokens; i++ {
			ids, freed = append(ids, turns[i].ID), freed+turns[i].EstTokens
		}
		return ids, freed
	})
}

// ClearRouterContextThread advances thread to a fresh empty epoch without deleting UI entries.
func (s *Store) ClearRouterContextThread(ctx context.Context, thread string) (int, int, RouterContextState, error) {
	thread = normContextThread(thread)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("begin clear router context: %w", err)
	}
	defer tx.Rollback()

	st, err := readContextStateTx(ctx, tx, thread)
	if err != nil {
		return 0, 0, RouterContextState{}, err
	}
	var count, freed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(est_tokens), 0) FROM router_context_turns WHERE thread = ? AND epoch = ? AND dropped = 0`,
		thread, st.Epoch,
	).Scan(&count, &freed); err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("count active turns before clear: %w", err)
	}
	st.Epoch++
	st.TotalEstTokens, st.LastPromptTokens, st.LastCachedTokens, st.UpdatedAt = 0, 0, 0, time.Now()
	if err := upsertContextStateTx(ctx, tx, st); err != nil {
		return 0, 0, RouterContextState{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, RouterContextState{}, fmt.Errorf("commit clear router context: %w", err)
	}
	return count, freed, st, nil
}

// CommitRouterContextCompactionTx atomically advances expectedEpoch -> expectedEpoch+1,
// inserts summary at cutoffSeq, and carries over all non-dropped turns with seq > cutoffSeq.
func (s *Store) CommitRouterContextCompactionTx(ctx context.Context, thread string, expectedEpoch int, cutoffSeq int64, summary *model.RouterContextTurn) (bool, RouterContextState, error) {
	if summary == nil {
		return false, RouterContextState{}, fmt.Errorf("nil summary turn")
	}
	thread = normContextThread(thread)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, RouterContextState{}, fmt.Errorf("begin router context compaction: %w", err)
	}
	defer tx.Rollback()

	st, err := readContextStateTx(ctx, tx, thread)
	if err != nil {
		return false, RouterContextState{}, err
	}
	if st.Epoch != expectedEpoch {
		return false, st, nil
	}
	newEpoch := st.Epoch + 1
	createdAt := summary.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO router_context_turns(
			id, seq, thread, epoch, kind, trigger_id, source, work_item_id, work_item_ids,
			user_text, assistant_text, est_tokens, dropped, drop_reason, created_at
		) VALUES(?, ?, ?, ?, 'summary', '', 'compaction', '', '[]', ?, ?, ?, 0, '', ?)`,
		summary.ID, cutoffSeq, thread, newEpoch, summary.UserText, summary.AssistantText, summary.EstTokens, createdAt.UnixNano(),
	); err != nil {
		return false, RouterContextState{}, fmt.Errorf("insert compaction summary turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE router_context_turns SET epoch = ? WHERE thread = ? AND epoch = ? AND dropped = 0 AND seq > ?`,
		newEpoch, thread, expectedEpoch, cutoffSeq,
	); err != nil {
		return false, RouterContextState{}, fmt.Errorf("carry over tail turns to new epoch: %w", err)
	}
	if st.TotalEstTokens, err = recalcActiveEstTokensTx(ctx, tx, thread, newEpoch); err != nil {
		return false, RouterContextState{}, err
	}
	st.Epoch = newEpoch
	st.CompactionCount++
	st.LastPromptTokens, st.LastCachedTokens, st.UpdatedAt = 0, 0, time.Now()
	if err := upsertContextStateTx(ctx, tx, st); err != nil {
		return false, RouterContextState{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, RouterContextState{}, fmt.Errorf("commit router context compaction: %w", err)
	}
	return true, st, nil
}

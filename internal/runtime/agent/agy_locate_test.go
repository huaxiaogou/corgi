package agent

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type agyTestRow struct {
	id         string
	workspaces []string
	modTime    time.Time
	depth      int
	killed     int
	parentID   string
	agentName  string
}

func writeAgySummariesDB(t *testing.T, home string, newSchema bool, rows []agyTestRow) {
	t.Helper()
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "conversation_summaries.db")
	_ = os.Remove(dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if newSchema {
		_, err = db.Exec(`CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			workspace_uris TEXT,
			last_modified_time TEXT,
			nesting_depth INTEGER,
			killed INTEGER,
			parent_conversation_id TEXT,
			agent_name TEXT
		)`)
	} else {
		_, err = db.Exec(`CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			workspace_uris TEXT,
			last_modified_time TEXT
		)`)
	}
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range rows {
		uris := make([]string, 0, len(r.workspaces))
		for _, ws := range r.workspaces {
			u := &url.URL{Scheme: "file", Path: ws}
			uris = append(uris, u.String())
		}
		rawURIs, _ := json.Marshal(uris)
		mt := r.modTime
		if mt.IsZero() {
			mt = time.Now()
		}
		mtStr := mt.UTC().Format("2006-01-02 15:04:05")
		if newSchema {
			_, err = db.Exec(
				`INSERT INTO conversation_summaries (conversation_id, title, workspace_uris, last_modified_time, nesting_depth, killed, parent_conversation_id, agent_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				r.id, "title-"+r.id, string(rawURIs), mtStr, r.depth, r.killed, r.parentID, r.agentName,
			)
		} else {
			_, err = db.Exec(
				`INSERT INTO conversation_summaries (conversation_id, title, workspace_uris, last_modified_time) VALUES (?, ?, ?, ?)`,
				r.id, "title-"+r.id, string(rawURIs), mtStr,
			)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func writeAgyTranscript(t *testing.T, home, convID, firstLine string, withFull bool) string {
	t.Helper()
	logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(base, []byte(firstLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withFull {
		full := filepath.Join(logDir, "transcript_full.jsonl")
		if err := os.WriteFile(full, []byte(firstLine+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return normPath(full)
	}
	return normPath(base)
}

func agyFirstLine(typ, source string, createdAt time.Time, content string) string {
	b, _ := json.Marshal(struct {
		StepIndex int    `json:"step_index"`
		Source    string `json:"source"`
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
		Content   string `json:"content"`
	}{
		StepIndex: 0,
		Source:    source,
		Type:      typ,
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		Content:   content,
	})
	return string(b)
}

func TestAgyLocateExplicitSessionNeverFallsBack(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	otherPath := writeAgyTranscript(t, home, "conv-other", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>other</USER_REQUEST>"), false)
	explicitFull := writeAgyTranscript(t, home, "conv-explicit", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>mine</USER_REQUEST>"), true)
	writeAgySummariesDB(t, home, true, []agyTestRow{
		{id: "conv-other", workspaces: []string{ws}, modTime: now},
		{id: "conv-explicit", workspaces: []string{ws}, modTime: now},
	})

	ad := &AgyAdapter{}

	// Explicit session missing on disk must return empty rather than guessing conv-other.
	miss := ad.LocateTranscriptWithHint(home, ws, "conv-not-created-yet", now, LocateHint{PaneID: "p1", SinceKnown: true})
	if miss.Path != "" || miss.Verified || miss.SessionID != "" {
		t.Fatalf("missing explicit session must return empty, got %+v (other=%s)", miss, otherPath)
	}

	// Existing explicit session resolves to transcript_full.jsonl and marks Verified.
	hit := ad.LocateTranscriptWithHint(home, ws, "conv-explicit", now, LocateHint{PaneID: "p1", SinceKnown: true})
	if hit.Path != explicitFull || hit.SessionID != "conv-explicit" || !hit.Verified {
		t.Fatalf("explicit hit = %+v, want path=%s verified=true", hit, explicitFull)
	}
}

func TestAgyLocateTwoLayerFiltersAndSymlinkEscape(t *testing.T) {
	t.Parallel()
	now := time.Now()

	cases := []struct {
		name      string
		newSchema bool
	}{
		{name: "new_schema_filters_subagents", newSchema: true},
		{name: "legacy_schema_falls_back_to_first_line_filter", newSchema: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			realWS := filepath.Join(home, "dir with space", "repo")
			if err := os.MkdirAll(realWS, 0o755); err != nil {
				t.Fatal(err)
			}
			linkWS := filepath.Join(home, "repo-link")
			if err := os.Symlink(realWS, linkWS); err != nil {
				t.Fatal(err)
			}

			// Subagent 1: SYSTEM_MESSAGE first line (caught by layer 2 even under old schema).
			writeAgyTranscript(t, home, "conv-sub-sys", agyFirstLine("SYSTEM_MESSAGE", "SYSTEM", now, "subagent prompt"), false)
			// Subagent 2: nesting_depth=1 / killed=1 / agent_name="research" (caught by layer 1 in new schema; also SYSTEM source in layer 2).
			writeAgyTranscript(t, home, "conv-sub-depth", agyFirstLine("PLANNER_RESPONSE", "SYSTEM", now, "nested"), false)
			// Stale conversation born 2 hours ago, but file mtime touched to now (must be rejected by created_at).
			stalePath := writeAgyTranscript(t, home, "conv-stale", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now.Add(-2*time.Hour), "old"), false)
			_ = os.Chtimes(stalePath, now, now)
			// Valid top-level conversation born inside start window.
			goodPath := writeAgyTranscript(t, home, "conv-good", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>hello</USER_REQUEST>"), false)

			writeAgySummariesDB(t, home, tc.newSchema, []agyTestRow{
				{id: "conv-sub-sys", workspaces: []string{realWS}, modTime: now, depth: 0},
				{id: "conv-sub-depth", workspaces: []string{realWS}, modTime: now, depth: 1, killed: 1, parentID: "conv-good", agentName: "research"},
				{id: "conv-stale", workspaces: []string{realWS}, modTime: now, depth: 0},
				{id: "conv-good", workspaces: []string{realWS}, modTime: now, depth: 0},
			})

			ad := &AgyAdapter{}
			// Query via symlink path to verify normPath + fileURIToPath (%20 unescape) match.
			res := ad.LocateTranscriptWithHint(home, linkWS, "", now.Add(-2*time.Second), LocateHint{
				PaneID:     "p1",
				SinceKnown: true,
			})
			if res.Path != goodPath || res.SessionID != "conv-good" || !res.Verified {
				t.Fatalf("got %+v, want path=%s session=conv-good verified=true", res, goodPath)
			}
		})
	}
}

func TestAgyLocateSameCwdDualWorkersAndAmbiguityRefusal(t *testing.T) {
	t.Parallel()
	now := time.Now()

	t.Run("tagged_dual_workers_isolated", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ws := filepath.Join(home, "shared-repo")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		p1 := writeAgyTranscript(t, home, "conv-w1", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "[corgi task · worker=agy-alpha · thread=chat:main]\ndo alpha"), false)
		p2 := writeAgyTranscript(t, home, "conv-w2", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "[corgi task · worker=agy-beta · thread=chat:main]\ndo beta"), false)
		writeAgySummariesDB(t, home, true, []agyTestRow{
			{id: "conv-w1", workspaces: []string{ws}, modTime: now},
			{id: "conv-w2", workspaces: []string{ws}, modTime: now},
		})

		ad := &AgyAdapter{}
		r1 := ad.LocateTranscriptWithHint(home, ws, "", now.Add(-1*time.Second), LocateHint{
			PaneID:     "pane-1",
			WorkerName: "agy-alpha",
			SinceKnown: true,
		})
		r2 := ad.LocateTranscriptWithHint(home, ws, "", now.Add(-1*time.Second), LocateHint{
			PaneID:     "pane-2",
			WorkerName: "agy-beta",
			SinceKnown: true,
		})
		if r1.Path != p1 || r1.SessionID != "conv-w1" || !r1.Verified {
			t.Fatalf("worker 1 mismatch: %+v", r1)
		}
		if r2.Path != p2 || r2.SessionID != "conv-w2" || !r2.Verified {
			t.Fatalf("worker 2 mismatch: %+v", r2)
		}
		// Third pane with unknown worker name cannot steal either claimed transcript.
		r3 := ad.LocateTranscriptWithHint(home, ws, "", now.Add(-1*time.Second), LocateHint{
			PaneID:     "pane-3",
			WorkerName: "agy-gamma",
			SinceKnown: true,
		})
		if r3.Path != "" || r3.Verified {
			t.Fatalf("third worker should not steal claimed transcripts, got %+v", r3)
		}
	})

	t.Run("ambiguous_untagged_candidates_never_guess", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ws := filepath.Join(home, "shared-repo")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		writeAgyTranscript(t, home, "conv-a", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>task A</USER_REQUEST>"), false)
		writeAgyTranscript(t, home, "conv-b", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now.Add(time.Second), "<USER_REQUEST>task B</USER_REQUEST>"), false)
		writeAgySummariesDB(t, home, true, []agyTestRow{
			{id: "conv-a", workspaces: []string{ws}, modTime: now},
			{id: "conv-b", workspaces: []string{ws}, modTime: now.Add(time.Second)},
		})

		ad := &AgyAdapter{}
		res := ad.LocateTranscriptWithHint(home, ws, "", now.Add(-1*time.Second), LocateHint{
			PaneID:     "pane-x",
			SinceKnown: true,
		})
		if res.Path != "" || res.SessionID != "" || res.Verified {
			t.Fatalf("ambiguous candidates must return empty (never guess cands[0]), got %+v", res)
		}
	})

	t.Run("unknown_since_refuses_untagged_guess", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ws := filepath.Join(home, "repo")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		writeAgyTranscript(t, home, "conv-untagged", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>hi</USER_REQUEST>"), false)
		writeAgySummariesDB(t, home, true, []agyTestRow{
			{id: "conv-untagged", workspaces: []string{ws}, modTime: now},
		})
		ad := &AgyAdapter{}
		res := ad.LocateTranscriptWithHint(home, ws, "", time.Time{}, LocateHint{PaneID: "pane-1", SinceKnown: false})
		if res.Path != "" || res.Verified {
			t.Fatalf("SinceKnown=false without exact worker tag must return empty, got %+v", res)
		}
	})
}

func TestAgyClaimLifecycleReleaseReconcileAndGeneration(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p1 := writeAgyTranscript(t, home, "conv-1", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, "<USER_REQUEST>one</USER_REQUEST>"), false)
	writeAgySummariesDB(t, home, true, []agyTestRow{
		{id: "conv-1", workspaces: []string{ws}, modTime: now},
	})

	ad := &AgyAdapter{}
	since1 := now.Add(-2 * time.Second)
	r1 := ad.LocateTranscriptWithHint(home, ws, "", since1, LocateHint{PaneID: "pane-1", SinceKnown: true})
	if r1.Path != p1 || !r1.Verified {
		t.Fatalf("pane-1 initial claim failed: %+v", r1)
	}

	// While pane-1 holds the claim, pane-2 cannot take it.
	if blocked := ad.LocateTranscriptWithHint(home, ws, "", since1, LocateHint{PaneID: "pane-2", SinceKnown: true}); blocked.Path != "" {
		t.Fatalf("pane-2 must not steal pane-1 claim, got %+v", blocked)
	}

	// ReconcileLivePanes without pane-1 drops the stale claim.
	ad.ReconcileLivePanes(map[string]bool{"pane-2": true})
	r2 := ad.LocateTranscriptWithHint(home, ws, "", since1, LocateHint{PaneID: "pane-2", SinceKnown: true})
	if r2.Path != p1 || !r2.Verified {
		t.Fatalf("pane-2 should claim after ReconcileLivePanes freed pane-1, got %+v", r2)
	}

	// ReleasePane frees pane-2 immediately.
	ad.ReleasePane("pane-2")
	r3 := ad.LocateTranscriptWithHint(home, ws, "", since1, LocateHint{PaneID: "pane-3", SinceKnown: true})
	if r3.Path != p1 || !r3.Verified {
		t.Fatalf("pane-3 should claim after ReleasePane freed pane-2, got %+v", r3)
	}

	// Generation (startedAt) change on pane-3 invalidates old claim if old transcript predates new start window.
	laterSince := now.Add(5 * time.Minute)
	rGen := ad.LocateTranscriptWithHint(home, ws, "", laterSince, LocateHint{PaneID: "pane-3", SinceKnown: true})
	if rGen.Path != "" || rGen.Verified {
		t.Fatalf("generation change must release stale claim and refuse old transcript, got %+v", rGen)
	}
}

func TestAgyFirstLineOversizedPrefixFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	huge := "[corgi task · worker=agy-big · thread=chat:main]\n" + strings.Repeat("A", 80<<10)
	p := writeAgyTranscript(t, home, "conv-big", agyFirstLine("USER_INPUT", "USER_EXPLICIT", now, huge), false)
	writeAgySummariesDB(t, home, true, []agyTestRow{
		{id: "conv-big", workspaces: []string{ws}, modTime: now},
	})

	ad := &AgyAdapter{}
	res := ad.LocateTranscriptWithHint(home, ws, "", now.Add(-1*time.Second), LocateHint{
		PaneID:     "pane-big",
		WorkerName: "agy-big",
		SinceKnown: true,
	})
	if res.Path != p || !res.Verified {
		t.Fatalf("oversized first line should still match bounded prefix, got %+v", res)
	}
}

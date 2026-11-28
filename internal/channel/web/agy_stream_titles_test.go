package web

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	_ "modernc.org/sqlite"
)

func seedAgyConv(t *testing.T, home, ws, convID, firstContent string, createdAt time.Time) string {
	t.Helper()
	root := filepath.Join(home, ".gemini", "antigravity-cli")
	logDir := filepath.Join(root, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"step_index": 0,
		"type":       "USER_INPUT",
		"source":     "USER_EXPLICIT",
		"created_at": createdAt.UTC().Format(time.RFC3339Nano),
		"content":    firstContent,
	})
	reply, _ := json.Marshal(map[string]any{
		"step_index": 1,
		"type":       "PLANNER_RESPONSE",
		"content":    "reply-from-" + convID,
	})
	trPath := filepath.Join(logDir, "transcript.jsonl")
	if err := os.WriteFile(trPath, append(append(line, '\n'), append(reply, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(root, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT,
		workspace_uris TEXT,
		last_modified_time TEXT,
		nesting_depth INTEGER,
		killed INTEGER,
		parent_conversation_id TEXT,
		agent_name TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	u := (&url.URL{Scheme: "file", Path: ws}).String()
	rawURIs, _ := json.Marshal([]string{u})
	_, err = db.Exec(
		`INSERT OR REPLACE INTO conversation_summaries (conversation_id, title, workspace_uris, last_modified_time, nesting_depth, killed, parent_conversation_id, agent_name) VALUES (?, ?, ?, ?, 0, 0, '', '')`,
		convID, "Title "+convID, string(rawURIs), createdAt.UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return trPath
}

func fakeHerdrForWeb(handler func(method string, params map[string]any) map[string]any) *herdr.Client {
	return herdr.NewClient(func(ctx context.Context) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			rd := bufio.NewReader(c2)
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			res := handler(req.Method, req.Params)
			if res == nil {
				res = map[string]any{}
			}
			b, _ := json.Marshal(map[string]any{"id": req.ID, "result": res})
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})
}

func TestLocalPaneSessionsBlocksUnverifiedAgySessionAndReleasesOnClose(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Two untagged conversations in same cwd -> ambiguous -> must NOT expose Session in /titles.
	seedAgyConv(t, home, ws, "conv-ambig-1", "<USER_REQUEST>a</USER_REQUEST>", now)
	seedAgyConv(t, home, ws, "conv-ambig-2", "<USER_REQUEST>b</USER_REQUEST>", now)
	// One uniquely tagged worker conversation -> Verified -> must expose Session in /titles.
	seedAgyConv(t, home, ws, "conv-tagged", "[corgi task · worker=agy-verified · thread=chat:main]\nwork", now)

	var mu sync.Mutex
	closedWS := ""
	livePanes := []any{
		map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "cwd": ws, "agent": "agy", "agent_status": "idle", "terminal_title_stripped": "Ambiguous Pane"},
		map[string]any{"pane_id": "w2:p1", "workspace_id": "w2", "cwd": ws, "agent": "agy", "agent_status": "idle", "terminal_title_stripped": "Verified Pane"},
	}
	liveAgents := []any{
		map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "name": "agy-ambig", "agent": "agy", "agent_status": "idle", "cwd": ws},
		map[string]any{"pane_id": "w2:p1", "workspace_id": "w2", "name": "agy-verified", "agent": "agy", "agent_status": "idle", "cwd": ws},
	}

	hc := fakeHerdrForWeb(func(method string, params map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.list":
			return map[string]any{"panes": livePanes}
		case "agent.list":
			return map[string]any{"agents": liveAgents}
		case "workspace.close":
			if id, ok := params["workspace_id"].(string); ok {
				closedWS = id
			}
			return map[string]any{"ok": true}
		}
		return map[string]any{}
	})

	reg := agent.NewRegistry()
	srv := &Server{
		Fleet:    fleet.NewFleet(hc, "mac-node", home, "test", []string{"agy"}),
		Agents:   reg,
		Home:     home,
		CorgiDir: home,
		NodeOnly: true,
	}
	srv.markStarted("w1:p1")
	srv.markStarted("w2:p1")

	sessions := srv.localPaneSessions(context.Background())
	if got := sessions["w1:p1"].Session; got != "" {
		t.Fatalf("unverified ambiguous agy pane must NOT expose Session, got %q", got)
	}
	if got := sessions["w2:p1"].Session; got != "conv-tagged" {
		t.Fatalf("verified agy pane Session = %q, want conv-tagged", got)
	}

	// Closing workspace w2 via NodeRoutes releases w2:p1's claim locally.
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/w2/close", nil)
	rec := httptest.NewRecorder()
	srv.NodeRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || closedWS != "w2" {
		t.Fatalf("close workspace failed: code=%d closedWS=%q body=%s", rec.Code, closedWS, rec.Body.String())
	}

	// Another pane with the same worker name can immediately claim conv-tagged after release.
	ad, _ := reg.Get("agy")
	res := agent.LocateTranscript(ad, home, ws, "", now, agent.LocateHint{
		PaneID:     "w3:p1",
		WorkerName: "agy-verified",
		SinceKnown: true,
	})
	if res.SessionID != "conv-tagged" || !res.Verified {
		t.Fatalf("expected released claim to be re-claimable, got %+v", res)
	}
}

func TestHandleStreamChatReleasesOldReaderWhenLocateReturnsEmpty(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedAgyConv(t, home, ws, "conv-live", "<USER_REQUEST>initial</USER_REQUEST>", now)

	var mu sync.Mutex
	sessionVal := "conv-live"
	reads := 0

	hc := fakeHerdrForWeb(func(method string, params map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			return map[string]any{"agents": []any{
				map[string]any{
					"pane_id":       "w1:p1",
					"workspace_id":  "w1",
					"name":          "agy-1",
					"agent":         "agy",
					"agent_status":  "idle",
					"cwd":           ws,
					"agent_session": map[string]any{"kind": "id", "value": sessionVal},
				},
			}}
		case "pane.read":
			reads++
			text := "> "
			if src, _ := params["source"].(string); src == "recent_unwrapped" {
				text = "RAW_TERMINAL_FALLBACK_LINE"
			}
			return map[string]any{"read": map[string]any{"text": text, "revision": uint64(reads)}}
		}
		return map[string]any{}
	})

	srv := &Server{
		Fleet:    fleet.NewFleet(hc, "mac-node", home, "test", []string{"agy"}),
		Agents:   agent.NewRegistry(),
		Home:     home,
		CorgiDir: home,
		NodeOnly: true,
	}

	ts := httptest.NewServer(srv.NodeRoutes())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/panes/w1:p1/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	rd := bufio.NewReader(resp.Body)
	var (
		gotInitialPatch bool
		resetCount      int
		gotFallbackLog  bool
		curEvent        string
	)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
			if curEvent == "reset" {
				resetCount++
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if curEvent == "patch" && strings.Contains(data, "reply-from-conv-live") && !gotInitialPatch {
				gotInitialPatch = true
				// Switch explicit session to a missing transcript so LocateTranscript returns "".
				mu.Lock()
				sessionVal = "conv-missing-now"
				mu.Unlock()
			}
			if curEvent == "log" && strings.Contains(data, "RAW_TERMINAL_FALLBACK_LINE") {
				gotFallbackLog = true
				cancel()
				break
			}
		}
	}

	if !gotInitialPatch {
		t.Fatal("never received initial transcript patch")
	}
	if resetCount < 2 {
		t.Fatalf("expected second reset event when transcript ownership dropped, got resetCount=%d", resetCount)
	}
	if !gotFallbackLog {
		t.Fatal("expected fallback log event after releasing old reader")
	}
}

package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func newFakeHerdrClient(handler func(method string, params map[string]any) (any, *herdr.Error)) *herdr.Client {
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
			res, herr := handler(req.Method, req.Params)
			resp := map[string]any{"id": req.ID}
			if herr != nil {
				resp["error"] = herr
			} else if res != nil {
				resp["result"] = res
			} else {
				resp["result"] = map[string]any{}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})
}

func TestDreamJournalScheduleAndSkillCards(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 11, 25, 2, 0, 0, 0, time.Local)
	next := eng.NextDream(ctx, now)
	if next.Hour() != 4 || next.Minute() != 0 || next.Day() != 25 {
		t.Fatalf("unexpected NextDream: %v", next)
	}

	// Write a synthetic dream journal with both skill creation and skill fix lines.
	runAt := time.Date(2026, 11, 25, 4, 5, 0, 0, time.Local)
	jPath := filepath.Join(eng.memory.DreamsDir(), journalName(runAt)+".md")
	journal := "# 夜间整理\n\n归档了 2 个已完成任务并精简了长期偏好。\n可做成 skill：发布回滚检查、慢 SQL 剖析。\n可改进 skill：deploy-helper（补充灰度校验）\n\n- 详情...\n"
	if err := os.WriteFile(jPath, []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}

	// Test in-memory buildDreamInput generation.
	_, _, _, _ = eng.Reply(ctx, model.MainThread, "测试消息", "", nil, false)
	dreamInput := eng.buildDreamInput(ctx)
	if !strings.Contains(dreamInput, "测试消息") {
		t.Fatalf("unexpected dream input: %s", dreamInput)
	}

	// Finish dream and verify two synchronized --opt Actions are created for skill proposals.
	d := &dreamRun{at: runAt}
	eng.mu.Lock()
	eng.dream = d
	eng.mu.Unlock()
	eng.finishDream(ctx, d, true)

	if eng.Dreaming() {
		t.Fatal("expected Dreaming()=false after finishDream")
	}
	if eng.LastDream(ctx).IsZero() {
		t.Fatal("expected last_dream persisted in kv")
	}
	acts, err := st.ListPendingActions(ctx)
	if err != nil || len(acts) != 2 {
		t.Fatalf("expected 2 skill proposal actions, got %d (err=%v)", len(acts), err)
	}
	if !strings.Contains(acts[0].Summary, "发布回滚检查") || !strings.Contains(acts[1].Summary, "deploy-helper") {
		t.Fatalf("unexpected skill action summaries: %q / %q", acts[0].Summary, acts[1].Summary)
	}
}

func TestResilienceBareShellAndDismiss(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": "zsh: command not found"}}, nil
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Bare-shell worker failure detection.
	wk := &model.Worker{
		Name: "broken-1", Kind: "claude", Cwd: dir, Thread: model.MainThread,
		PaneID: "w1:p1", CreatedAt: time.Now().Add(-2 * time.Minute), Status: "starting",
	}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}
	_, _ = eng.Ingest(ctx, "router", model.MainThread, model.RouterName, "do task", "", "broken-1", "broken-1")
	eng.mu.Lock()
	eng.noAgent["broken-1"] = time.Now().Add(-40 * time.Second)
	eng.mu.Unlock()

	if !eng.checkWorkerBareShell(ctx, wk, liveInfo{pane: "w1:p1", kind: ""}, time.Now()) {
		t.Fatal("expected checkWorkerBareShell to handle bare-shell worker")
	}
	gotW, _ := st.GetWorker(ctx, "broken-1")
	if !gotW.Ended || gotW.Status != "failed" {
		t.Fatalf("expected worker marked ended/failed, got %+v", gotW)
	}

	// 2. DismissAction atomic transition + FollowView in Snapshot.
	_, act, _, err := eng.Reply(ctx, model.MainThread, "请确认报告", "", []string{"确认通过", "退回修改"}, false)
	if err != nil || act == nil {
		t.Fatalf("Reply(--opt): %v", err)
	}
	dismissed, err := eng.DismissAction(ctx, act.ID)
	if err != nil || dismissed.Status != model.ActionExpired || dismissed.ResolvedBy != "dismiss" {
		t.Fatalf("DismissAction failed: %+v err=%v", dismissed, err)
	}
	if _, err := eng.DismissAction(ctx, act.ID); !errors.Is(err, store.ErrActionAlreadyResolved) {
		t.Fatalf("expected ErrActionAlreadyResolved on double dismiss, got %v", err)
	}

	if _, err := eng.AddTimer(ctx, "in 20m", "检查灰度指标", model.MainThread, "", "router"); err != nil {
		t.Fatal(err)
	}
	snap := eng.Snapshot(ctx, model.MainThread)
	if snap.Follow == nil || !strings.Contains(snap.Follow.NextText, "检查灰度指标") {
		t.Fatalf("expected Follow populated from timer, got %+v", snap.Follow)
	}
}

func TestWorkerAutoSummaryAndSpawnPreflight(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var trustedCalls int
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/trust":
			trustedCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/api/panes/w1:p1/last":
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "已完成多媒体上传重构并通过全部测试"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer nodeSrv.Close()

	remoteHerdr := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			if gotCwd, _ := params["cwd"].(string); gotCwd != "/Users/alice/proj" {
				return nil, &herdr.Error{Code: "bad_cwd", Message: "got cwd " + gotCwd}
			}
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w1"},
				"root_pane": map[string]any{"pane_id": "w1:p1"},
			}, nil
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": "> "}}, nil
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(nil, "hub", dir, "v0.1.14", []string{"codebuddy"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{
			ID:     "mac-1",
			Name:   "MacBook",
			Home:   "/Users/alice",
			Agents: []string{"agy", "codebuddy"},
			Online: true,
		},
		Herdr: remoteHerdr,
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				u := *req.URL
				u.Scheme = "http"
				u.Host = strings.TrimPrefix(nodeSrv.URL, "http://")
				r2 := req.Clone(req.Context())
				r2.URL = &u
				return http.DefaultTransport.RoundTrip(r2)
			}),
		},
	})

	ctx := context.Background()
	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.workerTurnSettleDelay = -1

	// 1. Pre-flight check rejects uninstalled agent on MacBook immediately.
	if _, err := eng.Spawn(ctx, SpawnRequest{
		Kind: "claude", Machine: "MacBook", Task: "test task",
	}); err == nil || !strings.Contains(err.Error(), "no claude installed") {
		t.Fatalf("expected missing-agent preflight error, got %v", err)
	}

	// 2. Spawn on MacBook expands ~/proj against MacBook's /Users/alice and calls remote /api/trust.
	wk, err := eng.Spawn(ctx, SpawnRequest{
		Kind: "agy", Machine: "MacBook", Cwd: "~/proj", Name: "mac-worker", Task: "implement feature",
	})
	if err != nil {
		t.Fatalf("Spawn on MacBook failed: %v", err)
	}
	if wk.Cwd != "/Users/alice/proj" || trustedCalls != 1 {
		t.Fatalf("expected cwd=/Users/alice/proj and trustedCalls=1, got cwd=%q trustedCalls=%d", wk.Cwd, trustedCalls)
	}

	// 3. Turn completion automatically pulls summary via /api/panes/{id}/last into Router trigger.
	wk.PaneID = "w1:p1@mac-1"
	eng.handleWorkerTurnDone(ctx, wk)
	trigs, _ := st.ListQueuedTriggers(ctx)
	foundSummary := false
	for _, tr := range trigs {
		if strings.Contains(tr.Text, "Last message:\n已完成多媒体上传重构并通过全部测试") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("expected worker turn-done trigger to embed remote summary, got %+v", trigs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

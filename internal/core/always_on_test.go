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
	"sync"
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

func TestHandoverTriggersAndArchivesSession(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Seed a Claude JSONL transcript with 140k/200k tokens (70% < 90% default rotate threshold).
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	projDir := filepath.Join(dir, ".claude", "projects", "router")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessID := "11111111-2222-3333-4444-555555555555"
	trPath := filepath.Join(projDir, sessID+".jsonl")
	trLine55 := `{"type":"assistant","message":{"usage":{"input_tokens":110000,"output_tokens":500}}}` + "\n"
	if err := os.WriteFile(trPath, []byte(trLine55), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		prompts []string
		closed  []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
		case "workspace.list":
			return map[string]any{"workspaces": []any{
				map[string]any{"workspace_id": "w-router", "label": model.RouterLabel},
			}}, nil
		case "workspace.close":
			closed = append(closed, params["workspace_id"].(string))
		}
		return map[string]any{}, nil
	})

	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.routerLaunchAt = time.Now().Add(-10 * time.Minute)
	eng.mu.Unlock()

	ctx := context.Background()
	live := map[string]liveInfo{
		model.RouterName: {pane: "w-router:p1", status: "idle", ready: true, kind: "claude", session: sessID},
	}

	// 0. At 55% (< 70% default threshold), handover must NOT trigger yet.
	eng.checkHandover(ctx, live, false)
	if eng.handingOver() {
		t.Fatal("handover must not start at 55% under default 70% threshold")
	}

	// 1. Advance transcript to 146k/200k tokens (73% >= 70% default) -> triggers handover flush prompt.
	trLine73 := `{"type":"assistant","message":{"usage":{"input_tokens":146000,"output_tokens":500}}}` + "\n"
	if err := os.WriteFile(trPath, []byte(trLine73), 0o600); err != nil {
		t.Fatal(err)
	}
	eng.checkHandover(ctx, live, false)
	if !eng.handingOver() {
		t.Fatal("expected handover to start at 73% context fill")
	}
	mu.Lock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Handover") {
		t.Fatalf("unexpected handover prompt: %v", prompts)
	}
	mu.Unlock()

	// 2. Router works on flushing memory, then goes idle after >5s -> finishes handover & archives session.
	live[model.RouterName] = liveInfo{pane: "w-router:p1", status: "working", ready: true, kind: "claude", session: sessID}
	eng.checkHandover(ctx, live, false)
	eng.mu.Lock()
	eng.handover.at = time.Now().Add(-10 * time.Second)
	eng.mu.Unlock()
	live[model.RouterName] = liveInfo{pane: "w-router:p1", status: "idle", ready: true, kind: "claude", session: sessID}
	eng.checkHandover(ctx, live, false)

	if eng.handingOver() {
		t.Fatal("expected handover to complete")
	}
	ended, err := st.ListEndedSessions(ctx, 10)
	if err != nil || len(ended) != 1 || ended[0].Session != sessID {
		t.Fatalf("expected archived router session %s, got %+v (err=%v)", sessID, ended, err)
	}
	mu.Lock()
	if len(closed) != 1 || closed[0] != "w-router" {
		t.Fatalf("expected router workspace closed on handover, got %v", closed)
	}
	mu.Unlock()
}

func TestDreamJournalScheduleAndSkillCards(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.list":
			return map[string]any{"workspaces": []any{}}, nil
		case "workspace.create":
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w2"},
				"root_pane": map[string]any{"pane_id": "w2:p1"},
			}, nil
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
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

	// Test writeDreamInput generation.
	_, _, _, _ = eng.Reply(ctx, model.MainThread, "测试消息", "", nil, false)
	if err := eng.writeDreamInput(ctx); err != nil {
		t.Fatalf("writeDreamInput: %v", err)
	}
	inBytes, err := os.ReadFile(filepath.Join(eng.memory.RouterDir(), "dream", "input.md"))
	if err != nil || !strings.Contains(string(inBytes), "测试消息") {
		t.Fatalf("unexpected dream input: err=%v body=%s", err, string(inBytes))
	}

	// Finish dream and verify two synchronized --opt Actions are created for skill proposals.
	d := &dreamRun{at: runAt, started: true, worked: true}
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
	// Wait for the async ensureRouter goroutine triggered by finishDream to settle before TempDir cleanup.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		eng.mu.Lock()
		launching := eng.routerLaunching
		eng.mu.Unlock()
		if !launching {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResilienceBareShellRouterRetryAndDismiss(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu      sync.Mutex
		prompts []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": "Error: rate limit exceeded on upstream model"}}, nil
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
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

	// 2. Router transient provider error retry (3 progressive backoff attempts, 4th stops retrying).
	eng.mu.Lock()
	eng.routerRetryDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	eng.routerMinSilentAge = -1
	eng.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		eng.mu.Lock()
		eng.routerAskedAt = time.Now().Add(-10 * time.Second)
		eng.mu.Unlock()
		eng.inspectRouterSilentTurn(ctx, "w0:p1", attempt)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(prompts)
			mu.Unlock()
			if got >= attempt+1 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	eng.mu.Lock()
	eng.routerAskedAt = time.Now().Add(-10 * time.Second)
	eng.mu.Unlock()
	eng.inspectRouterSilentTurn(ctx, "w0:p1", 3)
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	if len(prompts) != 3 || !strings.Contains(prompts[0], "retry 1/3") || !strings.Contains(prompts[1], "retry 2/3") || !strings.Contains(prompts[2], "retry 3/3") {
		t.Fatalf("unexpected retry prompts: %v", prompts)
	}
	mu.Unlock()

	// 3. DismissAction atomic transition + FollowView in Snapshot.
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

func TestWorkerAutoSummarySpawnPreflightAndDocHotReload(t *testing.T) {
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
	// Route node HTTP calls to nodeSrv.
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

	// 4. CORGI.md change queues a hot-reload trigger when Router already read previous CORGI.md.
	_ = eng.memory.WriteRouterDocs("corgi", eng.Settings())
	eng.memory.MarkRouterDocsRead()
	cfg := eng.Settings()
	cfg.ReplyChars = 180
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	trigs2, _ := st.ListQueuedTriggers(ctx)
	foundReread := false
	for _, tr := range trigs2 {
		if strings.Contains(tr.Text, "CORGI.md changed") {
			foundReread = true
		}
	}
	if !foundReread {
		t.Fatal("expected CORGI.md hot-reload trigger queued after SaveConfig")
	}
}

func TestCodeBuddyHandoverWithoutHerdrSessionAndSayPersistsMessagesMD(t *testing.T) {
	dir := t.TempDir()
	home := dir
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu      sync.Mutex
		prompts []string
	)
	cli := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		if method == "agent.prompt" {
			prompts = append(prompts, params["text"].(string))
		}
		return map[string]any{"ok": true}, nil
	})
	fl := fleet.NewFleet(cli, "hub", home, "test", []string{"codebuddy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, home, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Say persists user message immediately into memory/messages.md so Router restarts never lose it.
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "这个方案是在哪跑的"}); err != nil {
		t.Fatalf("Say: %v", err)
	}
	rawMsgs, err := os.ReadFile(filepath.Join(eng.memory.RouterDir(), "memory", "messages.md"))
	if err != nil || !strings.Contains(string(rawMsgs), "这个方案是在哪跑的") {
		t.Fatalf("expected Say to flush memory/messages.md, got err=%v content=%q", err, string(rawMsgs))
	}

	// 2. CodeBuddy Router with empty li.session still triggers handover at >=90% via turn-metrics & transcript UUID.
	sessID := "01a211cb-d92c-7bcd-8eda-a1d84c15e871"
	cbProj := filepath.Join(home, ".codebuddy", "projects", "router")
	if err := os.MkdirAll(cbProj, 0o755); err != nil {
		t.Fatal(err)
	}
	cbLines := strings.Join([]string{
		`{"type":"session-meta","cwd":"` + eng.memory.RouterDir() + `"}`,
		`{"type":"message","role":"assistant","providerData":{"messageId":"s1","requestModelId":"hy4std"}}`,
		`{"type":"message","role":"assistant","providerData":{"messageId":"s2","requestModelId":"hy4std"}}`,
		`{"type":"turn-metrics","durationMs":50000,"tokenDelta":368000}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cbProj, sessID+".jsonl"), []byte(cbLines), 0o600); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.routerLaunchAt = time.Now().Add(-10 * time.Minute)
	eng.routerAskedAt = time.Time{}
	eng.mu.Unlock()

	live := map[string]liveInfo{
		model.RouterName: {pane: "w9:p1", status: "idle", ready: true, kind: "codebuddy", session: ""},
	}
	eng.checkHandover(ctx, live, false)
	if !eng.handingOver() {
		t.Fatalf("expected CodeBuddy handover triggered even with empty li.session (usage=%+v)", eng.routerUsage)
	}
	eng.mu.Lock()
	gotSess := eng.handover.session
	eng.mu.Unlock()
	if gotSess != sessID {
		t.Fatalf("expected session ID extracted from transcript filename %q, got %q", sessID, gotSess)
	}
	mu.Lock()
	if len(prompts) == 0 || !strings.Contains(prompts[0], "[corgi] Handover.") {
		t.Fatalf("expected handover prompt sent, got %v", prompts)
	}
	mu.Unlock()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRouterStatusStabilizationAndSnapshotResilience(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu        sync.Mutex
		rawStatus = "working"
		failList  bool
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			if failList {
				return nil, &herdr.Error{Code: "timeout", Message: "transient herdr hiccup"}
			}
			return map[string]any{
				"agents": []map[string]any{{
					"name":              model.RouterName,
					"pane_id":           "w0:p1",
					"workspace_id":      "w0",
					"agent":             "codebuddy",
					"agent_status":      rawStatus,
					"interactive_ready": true,
				}},
			}, nil
		case "pane.send_keys":
			return map[string]any{"ok": true}, nil
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"codebuddy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.routerIdleGrace = 80 * time.Millisecond
	eng.mu.Unlock()

	ctx := context.Background()

	// 1. Working enters immediately.
	snap1 := eng.Snapshot(ctx, model.MainThread)
	if !snap1.Router.Alive || snap1.Router.Status != "working" || snap1.Router.Replied {
		t.Fatalf("expected working router snapshot, got %+v", snap1.Router)
	}

	// 2. Brief idle/unknown blips inside grace window keep "working" without flickering.
	mu.Lock()
	rawStatus = "idle"
	mu.Unlock()
	snap2 := eng.Snapshot(ctx, model.MainThread)
	if snap2.Router.Status != "working" {
		t.Fatalf("expected transient idle blip held as working, got %+v", snap2.Router)
	}

	// 3. Transient herdr error preserves last known alive/working view instead of dropping to asleep.
	mu.Lock()
	failList = true
	mu.Unlock()
	snapErr := eng.Snapshot(ctx, model.MainThread)
	if !snapErr.Router.Alive || snapErr.Router.Status != "working" {
		t.Fatalf("expected transient poll error to preserve alive/working router, got %+v", snapErr.Router)
	}

	// 4. Resuming working resets grace timer; calling Reply marks Replied=true while still working.
	mu.Lock()
	failList = false
	rawStatus = "working"
	mu.Unlock()
	if _, _, _, err := eng.Reply(ctx, model.MainThread, "已收到，正在处理", "", nil, false); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	snapReplied := eng.Snapshot(ctx, model.MainThread)
	if snapReplied.Router.Status != "working" || !snapReplied.Router.Replied {
		t.Fatalf("expected working with Replied=true after Reply, got %+v", snapReplied.Router)
	}

	// 5. Continuous idle past grace settles cleanly to "idle" and clears Replied.
	mu.Lock()
	rawStatus = "idle"
	mu.Unlock()
	_ = eng.Snapshot(ctx, model.MainThread) // starts idle grace clock
	time.Sleep(100 * time.Millisecond)
	snapIdle := eng.Snapshot(ctx, model.MainThread)
	if snapIdle.Router.Status != "idle" || snapIdle.Router.Replied {
		t.Fatalf("expected settled idle after grace window, got %+v", snapIdle.Router)
	}

	// 6. Blocked transitions immediately without waiting for grace; StopRouter resets working immediately.
	mu.Lock()
	rawStatus = "working"
	mu.Unlock()
	if s := eng.Snapshot(ctx, model.MainThread); s.Router.Status != "working" {
		t.Fatalf("expected working again, got %+v", s.Router)
	}
	mu.Lock()
	rawStatus = "blocked"
	mu.Unlock()
	if s := eng.Snapshot(ctx, model.MainThread); s.Router.Status != "blocked" {
		t.Fatalf("expected immediate blocked transition, got %+v", s.Router)
	}

	mu.Lock()
	rawStatus = "working"
	mu.Unlock()
	_ = eng.Snapshot(ctx, model.MainThread)
	if _, err := eng.StopRouter(ctx, model.MainThread); err != nil {
		t.Fatalf("StopRouter: %v", err)
	}
	mu.Lock()
	rawStatus = "idle"
	mu.Unlock()
	if s := eng.Snapshot(ctx, model.MainThread); s.Router.Status != "idle" {
		t.Fatalf("expected StopRouter to clear working hold immediately, got %+v", s.Router)
	}
}

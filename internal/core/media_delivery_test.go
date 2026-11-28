package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestNonHumanReadonlyDispatchZeroMediaRequests(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)
	secretUpload := writeTestUpload(t, dir, "20261128/private-spec.txt", []byte("human-secret-attachment"))

	// Seed a recent human user entry referencing secretUpload to prove non-human turns cannot pull it.
	if err := st.InsertEntry(ctx, &model.Entry{
		ID: "e-h1", Thread: model.MainThread, Role: "user", Channel: "web",
		Text:  "参考 " + secretUpload,
		Media: []model.MediaRef{{Path: secretUpload, Name: "private-spec.txt"}},
		At:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var remoteHTTPReqs atomic.Int32
	fl := fleet.NewFleet(newFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil }), "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-x", Name: "MacX", Online: true, Agents: []string{"claude"}},
		Herdr: newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
			switch method {
			case "workspace.create":
				return map[string]any{"workspace": map[string]any{"workspace_id": "wx"}, "root_pane": map[string]any{"pane_id": "wx:p1"}}, nil
			case "agent.start":
				return map[string]any{"pane_id": "wx:p1", "workspace_id": "wx", "session": "sx", "pid": 3001}, nil
			default:
				return map[string]any{}, nil
			}
		}),
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				remoteHTTPReqs.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
		},
	})
	eng.SetFleet(fl)

	for _, trig := range []*model.Trigger{
		{ID: "t-wh", Thread: model.MainThread, Source: "webhook:ci", Policy: "candidate", From: "ci", Text: "check " + secretUpload},
		{ID: "t-mach", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "machine", Text: "check " + secretUpload},
		{ID: "t-timer", Thread: model.MainThread, Source: "timer", Policy: "respond", From: "timer", Text: "check " + secretUpload},
	} {
		meta := eng.resolveTurnConsentMeta(ctx, trig)
		turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
		out := eng.toolDispatchWork(ctx, turn, trig, meta, `{"mode":"readonly","machine":"mac-x","goal":"只读检查 `+secretUpload+`"}`)
		if !strings.Contains(out, `"ok":true`) {
			t.Fatalf("expected non-human readonly dispatch allowed without staging: %s", out)
		}
	}
	if got := remoteHTTPReqs.Load(); got != 0 {
		t.Fatalf("P0 regression: non-human readonly dispatch must issue 0 file HTTP requests to remote node, got %d", got)
	}
}

func TestStageMediaSteerDirectAndAsyncDeliverBatchIsolation(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)
	srcPath := writeTestUpload(t, dir, "20261128/doc.txt", []byte("doc-body"))

	var (
		mu              sync.Mutex
		remotePrompts   []string
		stageStarted    = make(chan struct{}, 4)
		releaseStage    = make(chan struct{})
		gateStage       atomic.Bool
		postUploadCount atomic.Int32
	)
	remoteHerdr := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "agent.list":
			return map[string]any{"agents": []map[string]any{{
				"name": "w-remote", "pane_id": "wr:p1", "workspace_id": "wr",
				"agent": "claude", "agent_status": "idle", "interactive_ready": true,
			}}}, nil
		case "agent.prompt":
			mu.Lock()
			if p, ok := params["text"].(string); ok {
				remotePrompts = append(remotePrompts, p)
			}
			mu.Unlock()
			return map[string]any{"ok": true}, nil
		case "pane.read":
			return map[string]any{"text": ""}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(newFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil }), "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info:  model.Machine{ID: "mac-r", Name: "MacR", Online: true, Agents: []string{"claude"}},
		Herdr: remoteHerdr,
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/uploads" && r.Method == http.MethodHead {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if r.URL.Path == "/api/uploads" && r.Method == http.MethodPost {
					if gateStage.Load() {
						stageStarted <- struct{}{}
						<-releaseStage
					}
					postUploadCount.Add(1)
					if r.Body != nil {
						_, _ = io.Copy(io.Discard, r.Body)
					}
					dest := "/Users/r/.corgi/uploads/staged/20261128/" + r.URL.Query().Get("sha256") + "-" + r.URL.Query().Get("name")
					out, _ := json.Marshal(map[string]any{"path": dest})
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}),
		},
	})
	eng.SetFleet(fl)
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "w-remote", Kind: "claude", Machine: "mac-r", Thread: model.MainThread, PaneID: "wr:p1@mac-r", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. toolSteerWorker rewrites Hub path for remote worker.
	trig := &model.Trigger{
		ID: "t-steer", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "you",
		Text:  "请看 " + srcPath,
		Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}},
	}
	meta := eng.resolveTurnConsentMeta(ctx, trig)
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
	if out := eng.toolSteerWorker(ctx, turn, trig, meta, `{"worker":"w-remote","message":"参考 `+srcPath+` 继续"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("steer_worker failed: %s", out)
	}
	queued, _ := st.ListQueuedTriggers(ctx)
	if len(queued) == 0 || strings.Contains(queued[0].Text, srcPath) || !strings.Contains(queued[0].Text, "/Users/r/.corgi/uploads/staged/20261128/") {
		t.Fatalf("expected queued steer trigger rewritten, got %+v", queued)
	}
	_ = st.AckTriggerTarget(ctx, queued[0].ID, "w-remote")

	// 2. SendDirectWorkerMessage stages Media before SubmitPanePrompt.
	if _, _, err := eng.SendDirectWorkerMessage(ctx, DirectWorkerRequest{
		Name: "w-remote", Text: "直聊看 " + srcPath, Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}},
	}); err != nil {
		t.Fatalf("SendDirectWorkerMessage failed: %v", err)
	}
	mu.Lock()
	if len(remotePrompts) == 0 || strings.Contains(remotePrompts[0], srcPath) || !strings.Contains(remotePrompts[0], "/Users/r/.corgi/uploads/staged/20261128/") {
		t.Fatalf("expected direct worker prompt rewritten, got %+v", remotePrompts)
	}
	remotePrompts = nil
	mu.Unlock()

	// 3. Async deliverQueuedWithFleet: non-human media ignored; permanent bad trigger isolated from valid sibling; deliveringWorkers released.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	eng.mu.Lock()
	eng.runCtx = runCtx
	eng.mu.Unlock()

	live := map[string]liveInfo{"w-remote": {pane: "wr:p1@mac-r", status: "idle", ready: true, kind: "claude"}}
	for _, nh := range []*model.Trigger{
		{ID: "t-mach-1", Seq: 1001, Thread: model.MainThread, Source: "chat", Policy: "respond", Status: "queued", From: "machine", Text: "machine " + srcPath, Targets: []string{"w-remote"}, Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}}, At: time.Now()},
		{ID: "t-wh-1", Seq: 1002, Thread: model.MainThread, Source: "webhook:ci", Policy: "candidate", Status: "queued", From: "ci", Text: "ci " + srcPath, Targets: []string{"w-remote"}, Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}}, At: time.Now()},
	} {
		if err := st.InsertTrigger(ctx, nh); err != nil {
			t.Fatal(err)
		}
	}
	beforePosts := postUploadCount.Load()
	eng.deliverQueuedWithFleet(ctx, live, map[string]bool{"mac-r": true}, true)
	if postUploadCount.Load() != beforePosts {
		t.Fatalf("expected non-human triggers NOT to stage media")
	}
	mu.Lock()
	remotePrompts = nil
	mu.Unlock()

	// Batch containing 1 permanently missing file trigger + 1 valid human media trigger:
	// permanent failure is isolated and ACKed with event; valid trigger still delivers asynchronously.
	gateStage.Store(true)
	missingPath := filepath.Join(dir, "uploads", "20261128", "gone.txt")
	if err := st.InsertTrigger(ctx, &model.Trigger{
		ID: "t-bad-media", Seq: 1003, Thread: model.MainThread, Source: "chat", Policy: "respond", Status: "queued", From: "you",
		Text: "missing " + missingPath, Targets: []string{"w-remote"},
		Media: []model.MediaRef{{Path: missingPath, Name: "gone.txt"}}, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTrigger(ctx, &model.Trigger{
		ID: "t-good-media", Seq: 1004, Thread: model.MainThread, Source: "chat", Policy: "respond", Status: "queued", From: "you",
		Text: "human media " + srcPath, Targets: []string{"w-remote"},
		Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}}, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	eng.deliverQueuedWithFleet(ctx, live, map[string]bool{"mac-r": true}, true)
	<-stageStarted
	eng.deliverQueuedWithFleet(ctx, live, map[string]bool{"mac-r": true}, true)
	close(releaseStage)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		q, _ := st.ListQueuedTriggers(ctx)
		if len(q) == 0 && !eng.isDeliveringWorker("w-remote") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if eng.isDeliveringWorker("w-remote") {
		t.Fatalf("expected deliveringWorkers[w-remote] released after async delivery")
	}
	if q, _ := st.ListQueuedTriggers(ctx); len(q) != 0 {
		t.Fatalf("expected both bad (isolated) and good triggers settled, remaining: %+v", q)
	}
	mu.Lock()
	if len(remotePrompts) != 1 || strings.Contains(remotePrompts[0], srcPath) || !strings.Contains(remotePrompts[0], "/Users/r/.corgi/uploads/staged/20261128/") {
		t.Fatalf("expected exactly 1 staged prompt delivered for valid sibling trigger, got %+v", remotePrompts)
	}
	mu.Unlock()
}

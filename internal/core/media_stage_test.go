package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestStageMediaHubToNodeDedupOldNodeAndDispatchOverride(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)
	srcData := []byte("需求文档正文-v1")
	srcPath := writeTestUpload(t, dir, "20261128/abc123-需求文档.pdf", srcData)

	var (
		mu          sync.Mutex
		nodeBHeads  int
		nodeBPosts  int
		nodeBStaged = map[string]string{}
		oldHeads    int
		oldPosts    int
		spawnedOn   string
	)
	nodeBCorgi := filepath.Join(t.TempDir(), "nodeb-corgi")
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w1"},
				"root_pane": map[string]any{"pane_id": "w1:p1"},
			}, nil
		case "agent.start":
			return map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "session": "s1", "pid": 1001}, nil
		case "agent.prompt":
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-b", Name: "MacB", Online: true, Agents: []string{"claude"}},
		Herdr: newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
			mu.Lock()
			defer mu.Unlock()
			switch method {
			case "workspace.create":
				spawnedOn = "mac-b"
				return map[string]any{
					"workspace": map[string]any{"workspace_id": "wb"},
					"root_pane": map[string]any{"pane_id": "wb:p1"},
				}, nil
			case "agent.start":
				return map[string]any{"pane_id": "wb:p1", "workspace_id": "wb", "session": "sb", "pid": 2002}, nil
			case "agent.prompt":
				return map[string]any{"ok": true}, nil
			default:
				return map[string]any{}, nil
			}
		}),
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				sha := r.URL.Query().Get("sha256")
				name := r.URL.Query().Get("name")
				key := sha + ":" + name
				if r.Method == http.MethodHead && r.URL.Path == "/api/uploads" {
					nodeBHeads++
					if p, ok := nodeBStaged[key]; ok {
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{"X-Corgi-Staged-Path": {p}},
							Body:       io.NopCloser(strings.NewReader("")),
						}, nil
					}
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if r.Method == http.MethodPost && r.URL.Path == "/api/uploads" {
					nodeBPosts++
					b, _ := io.ReadAll(r.Body)
					sum := sha256.Sum256(b)
					if hex.EncodeToString(sum[:]) != sha {
						return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"sha mismatch"}`))}, nil
					}
					dest := filepath.Join(nodeBCorgi, "uploads", "staged", "20261128", sha+"-"+name)
					nodeBStaged[key] = dest
					out, _ := json.Marshal(map[string]any{"path": dest, "name": name, "size": len(b)})
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}),
		},
	})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-old", Name: "MacOld", Online: true, Agents: []string{"claude"}},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodHead {
					oldHeads++
					return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				oldPosts++
				dest := "/Users/old/.corgi/uploads/20261128/rnd-" + r.URL.Query().Get("name")
				out, _ := json.Marshal(map[string]any{"path": dest})
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
			}),
		},
	})
	eng.SetFleet(fl)

	en := &model.Entry{
		ID: "e-u1", Thread: model.MainThread, Role: "user", Channel: "web",
		Text:  "请分析 " + srcPath,
		Media: []model.MediaRef{{Path: srcPath, Name: "需求文档.pdf"}},
		At:    time.Now(),
	}
	if err := st.InsertEntry(ctx, en); err != nil {
		t.Fatal(err)
	}
	trig := &model.Trigger{
		ID: "t-u1", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "you", Ref: en.ID,
		Text:     "请分析 " + srcPath,
		Media:    en.Media,
		Dispatch: &model.DispatchOverride{Machine: "mac-b"},
	}
	meta := eng.resolveTurnConsentMeta(ctx, trig)
	turn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
	rawOut := eng.toolDispatchWork(ctx, turn, trig, meta, `{"mode":"readonly","machine":"hub","goal":"分析 `+srcPath+` 的核心需求"}`)
	if !strings.Contains(rawOut, `"ok":true`) || !strings.Contains(rawOut, `"machine":"mac-b"`) {
		t.Fatalf("expected dispatch_work staged to mac-b, got: %s", rawOut)
	}
	mu.Lock()
	if nodeBHeads != 1 || nodeBPosts != 1 || spawnedOn != "mac-b" {
		t.Fatalf("expected 1 HEAD + 1 POST on mac-b and spawnedOn=mac-b, got heads=%d posts=%d spawnedOn=%q", nodeBHeads, nodeBPosts, spawnedOn)
	}
	mu.Unlock()
	workers, _ := st.ListWorkers(ctx)
	if len(workers) == 0 {
		t.Fatalf("expected spawned worker persisted")
	}
	if strings.Contains(workers[0].Task, srcPath) || !strings.Contains(workers[0].Task, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("expected worker Task rewritten to staged path, got: %s", workers[0].Task)
	}
	queued, _ := st.ListQueuedTriggers(ctx)
	if len(queued) == 0 || strings.Contains(queued[0].Text, srcPath) || !strings.Contains(queued[0].Text, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("expected queued initial worker trigger rewritten to staged path, got: %+v", queued)
	}

	// Second staging to mac-b hits HEAD 200 -> 0 additional POST calls!
	rewritten, _, warns, err := eng.stageTurnMediaForGoal(ctx, "mac-b", "再看下 "+srcPath, "", trig, meta)
	if err != nil || len(warns) != 0 || !strings.Contains(rewritten, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("second stage dedup failed: warns=%v err=%v out=%s", warns, err, rewritten)
	}
	mu.Lock()
	if nodeBHeads != 2 || nodeBPosts != 1 {
		t.Fatalf("expected HEAD=2 and POST=1 (zero-byte dedup), got heads=%d posts=%d", nodeBHeads, nodeBPosts)
	}
	mu.Unlock()

	// Old node returning HEAD 405 seamlessly falls back to POST.
	oldText, warns, err := eng.stageMediaForDirectText(ctx, "mac-old", "处理 "+srcPath, en.Media)
	if err != nil || len(warns) != 0 || !strings.Contains(oldText, "/Users/old/.corgi/uploads/20261128/rnd-") {
		t.Fatalf("old node fallback failed: warns=%v err=%v out=%s", warns, err, oldText)
	}
	mu.Lock()
	if oldHeads != 1 || oldPosts != 1 {
		t.Fatalf("expected old node 1 HEAD + 1 POST, got heads=%d posts=%d", oldHeads, oldPosts)
	}
	mu.Unlock()
}

func TestStageMediaNodeToHubNodeToNodeAndStructuredPrecedence(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)
	payload := []byte("node-a-uploaded-binary-content")
	// Use identical path string under dir/uploads so text inference would collide unless structured source wins.
	sharedPath := filepath.Join(dir, "uploads", "20261128", "same-path.md")

	hc := newFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil })
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "node-a", Name: "NodeA", Online: true},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/uploads/read" {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/octet-stream"}},
						Body:       io.NopCloser(bytes.NewReader(payload)),
					}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}),
		},
	})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "node-b", Name: "NodeB", Online: true},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodHead {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				b, _ := io.ReadAll(r.Body)
				if !bytes.Equal(b, payload) {
					return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"corrupt payload"}`))}, nil
				}
				dest := "/Users/bob/.corgi/uploads/staged/20261128/" + r.URL.Query().Get("sha256") + "-" + r.URL.Query().Get("name")
				out, _ := json.Marshal(map[string]any{"path": dest})
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
			}),
		},
	})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "node-legacy", Name: "Legacy", Online: true},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("404 page not found"))}, nil
			}),
		},
	})
	eng.SetFleet(fl)

	// Same-machine structured ref on node-a whose path collides with Hub's corgiDir/uploads must NOT be misclassified as Hub!
	sameOut, warns, err := eng.stageMediaForDirectText(ctx, "node-a", "查看 "+sharedPath, []model.MediaRef{{Machine: "node-a", Path: sharedPath, Name: "same-path.md"}})
	if err != nil || len(warns) != 0 || !strings.Contains(sameOut, sharedPath) || strings.Contains(sameOut, "[未同步") {
		t.Fatalf("structured same-machine ref must not trigger bogus Hub inference: err=%v warns=%v out=%s", err, warns, sameOut)
	}

	// Direction 2: Node A -> Hub
	outHub, warns, err := eng.stageMediaForDirectText(ctx, "", "查看 "+sharedPath, []model.MediaRef{{Machine: "node-a", Path: sharedPath, Name: "same-path.md"}})
	if err != nil || len(warns) != 0 || strings.Contains(outHub, sharedPath) || !strings.Contains(outHub, filepath.Join(dir, "uploads", "staged")) {
		t.Fatalf("Node A -> Hub failed: warns=%v err=%v out=%s", warns, err, outHub)
	}

	// Direction 3: Node A -> Node B
	outB, warns, err := eng.stageMediaForDirectText(ctx, "node-b", "查看 "+sharedPath, []model.MediaRef{{Machine: "node-a", Path: sharedPath, Name: "same-path.md"}})
	if err != nil || len(warns) != 0 || strings.Contains(outB, sharedPath) || !strings.Contains(outB, "/Users/bob/.corgi/uploads/staged/20261128/") {
		t.Fatalf("Node A -> Node B failed: warns=%v err=%v out=%s", warns, err, outB)
	}

	// Legacy source node 404 returns clean unsupported error.
	_, _, err = eng.stageMediaForDirectText(ctx, "", "查看 "+sharedPath, []model.MediaRef{{Machine: "node-legacy", Path: sharedPath, Name: "same-path.md"}})
	if err == nil || !strings.Contains(err.Error(), "源节点版本较旧") {
		t.Fatalf("expected legacy source error, got: %v", err)
	}
}

func TestStageMediaQuotasPartialFailureAndSameMachineAppend(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)
	hc := newFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil })
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "node-b", Name: "NodeB", Online: true},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodHead {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				_, _ = io.Copy(io.Discard, r.Body)
				dest := "/Users/bob/.corgi/uploads/staged/20261128/" + r.URL.Query().Get("sha256") + "-" + r.URL.Query().Get("name")
				out, _ := json.Marshal(map[string]any{"path": dest})
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
			}),
		},
	})
	eng.SetFleet(fl)

	okFile := writeTestUpload(t, dir, "20261128/ok.txt", []byte("good"))
	missingFile := filepath.Join(dir, "uploads", "20261128", "missing.txt")

	text, warns, err := eng.stageMediaForDirectText(ctx, "node-b", "看 "+okFile+" 和 "+missingFile, []model.MediaRef{
		{Path: okFile, Name: "ok.txt"},
		{Path: missingFile, Name: "missing.txt"},
	})
	if err != nil || len(warns) != 1 || strings.Contains(text, missingFile) || !strings.Contains(text, "[未同步: missing.txt]") || !strings.Contains(text, "[未同步的附件]") {
		t.Fatalf("unexpected partial failure output: err=%v warns=%v text=%s", err, warns, text)
	}
	if _, _, err := eng.stageMediaForDirectText(ctx, "offline-node", "看 "+okFile, []model.MediaRef{{Path: okFile, Name: "ok.txt"}}); err == nil {
		t.Fatalf("expected all-failed cross-machine staging to error")
	}
	localOut, warns, err := eng.stageMediaForDirectText(ctx, "", "帮我总结这个文件", []model.MediaRef{{Path: okFile, Name: "ok.txt"}})
	if err != nil || len(warns) != 0 || !strings.Contains(localOut, "[随消息附带的文件]") || !strings.Contains(localOut, okFile) {
		t.Fatalf("expected same-machine appendix, warns=%v err=%v out=%s", warns, err, localOut)
	}

	var eleven []model.MediaRef
	for i := 0; i < 11; i++ {
		p := writeTestUpload(t, dir, "20261128/f"+string(rune('a'+i))+".txt", []byte{byte('a' + i)})
		eleven = append(eleven, model.MediaRef{Path: p, Name: filepath.Base(p)})
	}
	_, warns11, err := eng.stageMediaForDirectText(ctx, "node-b", "batch", eleven)
	if err != nil || len(warns11) != 1 || !strings.Contains(warns11[0], "最多 10 个文件") {
		t.Fatalf("expected 11th file capped with warning, err=%v warns=%v", err, warns11)
	}

	over25 := writeTestUpload(t, dir, "20261128/over25.bin", make([]byte, MaxUploadStageBytes+1))
	if _, _, err := eng.stageMediaForDirectText(ctx, "node-b", "big", []model.MediaRef{{Path: over25, Name: "over25.bin"}}); err == nil {
		t.Fatalf("expected >25 MiB file rejected")
	}
	chunk20 := make([]byte, 20<<20)
	f1 := writeTestUpload(t, dir, "20261128/b1.bin", chunk20)
	chunk20[0] = 1
	f2 := writeTestUpload(t, dir, "20261128/b2.bin", chunk20)
	chunk20[0] = 2
	f3 := writeTestUpload(t, dir, "20261128/b3.bin", chunk20)
	_, warns50, err := eng.stageMediaForDirectText(ctx, "node-b", "three", []model.MediaRef{
		{Path: f1, Name: "b1.bin"},
		{Path: f2, Name: "b2.bin"},
		{Path: f3, Name: "b3.bin"},
	})
	if err != nil || len(warns50) != 1 || !strings.Contains(warns50[0], "50 MB") {
		t.Fatalf("expected 3rd 20MB file to trip 50MB batch cap, err=%v warns=%v", err, warns50)
	}
}

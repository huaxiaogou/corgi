package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
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

func writeTestUpload(t *testing.T, corgiDir, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(corgiDir, "uploads", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUploadsRootSandbox(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corgi.db")
	if err := os.WriteFile(dbPath, []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := writeTestUpload(t, dir, "20261128/abc-report.txt", []byte("hello-upload"))
	if err := os.WriteFile(filepath.Join(dir, "uploads", "20261128", "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symOutside := filepath.Join(dir, "uploads", "20261128", "sym-out.txt")
	if err := os.Symlink(outside, symOutside); err != nil {
		t.Fatal(err)
	}
	symDB := filepath.Join(dir, "uploads", "20261128", "sym-db.db")
	if err := os.Symlink(dbPath, symDB); err != nil {
		t.Fatal(err)
	}

	f, name, sz, err := OpenUploadsFile(dir, dbPath, valid, MaxUploadStageBytes)
	if err != nil {
		t.Fatalf("expected valid upload readable: %v", err)
	}
	_ = f.Close()
	if name != "abc-report.txt" || sz != int64(len("hello-upload")) {
		t.Fatalf("unexpected open result: name=%q sz=%d", name, sz)
	}

	rejected := []string{
		filepath.Join(dir, "uploads", "20261128", "..", "..", "corgi.db"),
		dbPath,
		outside,
		symOutside,
		symDB,
		filepath.Join(dir, "uploads", "20261128", "empty.txt"),
		filepath.Join(dir, "uploads", "20261128", ".tmp-hidden"),
	}
	for _, p := range rejected {
		if f, _, _, err := OpenUploadsFile(dir, dbPath, p, MaxUploadStageBytes); err == nil {
			_ = f.Close()
			t.Fatalf("expected OpenUploadsFile to reject %q", p)
		}
	}
	if _, _, _, err := OpenUploadsFile(dir, dbPath, valid, 4); err == nil {
		t.Fatalf("expected OpenUploadsFile to reject file exceeding maxBytes")
	}

	// readLocalTrustedMedia covers uploads + wecom and blocks symlinks.
	wecomFile := filepath.Join(dir, "wecom", "img.png")
	if err := os.MkdirAll(filepath.Dir(wecomFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wecomFile, []byte("png-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := readLocalTrustedMedia(dir, dbPath, wecomFile, 1024); !ok || string(got) != "png-data" {
		t.Fatalf("expected readLocalTrustedMedia to read wecom file, ok=%v got=%q", ok, string(got))
	}
	if _, ok := readLocalTrustedMedia(dir, dbPath, symOutside, 1024); ok {
		t.Fatalf("expected readLocalTrustedMedia to block symlink")
	}

	for _, badDest := range []string{"", "relative/path", "/a/../b", "/tmp/a\nb", "/tmp/a\x00b"} {
		if err := ValidateUploadDestPath(badDest); err == nil {
			t.Fatalf("expected ValidateUploadDestPath to reject %q", badDest)
		}
	}
	if err := ValidateUploadDestPath("/Users/bob/.corgi/uploads/staged/20261128/abc-需求.pdf"); err != nil {
		t.Fatalf("expected valid dest path accepted: %v", err)
	}
}

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
		spawnedTask string
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
		Herdr: newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
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
				if p, ok := params["text"].(string); ok {
					spawnedTask = p
				}
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

	// 1. Dispatch with Trigger.Dispatch.Machine="mac-b" overriding args.Machine="hub":
	// must stage to mac-b (1 HEAD + 1 POST), pin Machine="mac-b" in Spawn, and rewrite both goal and origReq.
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
	wk := workers[0]
	if strings.Contains(wk.Task, srcPath) || !strings.Contains(wk.Task, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("expected worker Task rewritten to staged path, got: %s", wk.Task)
	}
	// Initial queued trigger embeds both Task and OriginalRequest — neither may retain the Hub path.
	queued, _ := st.ListQueuedTriggers(ctx)
	if len(queued) == 0 || strings.Contains(queued[0].Text, srcPath) || !strings.Contains(queued[0].Text, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("expected queued initial worker trigger rewritten to staged path, got: %+v", queued)
	}
	_ = spawnedTask

	// 2. Second staging to mac-b hits HEAD 200 -> 0 additional POST calls!
	rewritten, _, warns, err := eng.stageTurnMediaForGoal(ctx, "mac-b", "再看下 "+srcPath, "", trig, meta)
	if err != nil || len(warns) != 0 {
		t.Fatalf("second stage failed: warns=%v err=%v", warns, err)
	}
	if !strings.Contains(rewritten, filepath.Join(nodeBCorgi, "uploads", "staged")) {
		t.Fatalf("expected rewritten staged path on dedup hit, got %s", rewritten)
	}
	mu.Lock()
	if nodeBHeads != 2 || nodeBPosts != 1 {
		t.Fatalf("expected HEAD=2 and POST=1 (zero-byte dedup), got heads=%d posts=%d", nodeBHeads, nodeBPosts)
	}
	mu.Unlock()

	// 3. Old node returning HEAD 405 seamlessly falls back to POST and adopts returned path.
	oldText, warns, err := eng.stageMediaForDirectText(ctx, "mac-old", "处理 "+srcPath, en.Media)
	if err != nil || len(warns) != 0 {
		t.Fatalf("old node fallback failed: warns=%v err=%v", warns, err)
	}
	if !strings.Contains(oldText, "/Users/old/.corgi/uploads/20261128/rnd-") {
		t.Fatalf("expected old node fallback dest path, got %s", oldText)
	}
	mu.Lock()
	if oldHeads != 1 || oldPosts != 1 {
		t.Fatalf("expected old node 1 HEAD + 1 POST, got heads=%d posts=%d", oldHeads, oldPosts)
	}
	mu.Unlock()
}

func TestStageMediaNodeToHubAndNodeToNode(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)
	payload := []byte("node-a-uploaded-binary-content")
	nodeAPath := "/Users/alice/.corgi/uploads/20261128/a1-spec.md"

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

	// Direction 2: Node A -> Hub
	outHub, warns, err := eng.stageMediaForDirectText(ctx, "", "查看 "+nodeAPath, []model.MediaRef{{Machine: "node-a", Path: nodeAPath, Name: "spec.md"}})
	if err != nil || len(warns) != 0 {
		t.Fatalf("Node A -> Hub failed: warns=%v err=%v", warns, err)
	}
	if strings.Contains(outHub, nodeAPath) || !strings.Contains(outHub, filepath.Join(dir, "uploads", "staged")) {
		t.Fatalf("expected Hub staged path in output, got: %s", outHub)
	}

	// Direction 3: Node A -> Node B
	outB, warns, err := eng.stageMediaForDirectText(ctx, "node-b", "查看 "+nodeAPath, []model.MediaRef{{Machine: "node-a", Path: nodeAPath, Name: "spec.md"}})
	if err != nil || len(warns) != 0 {
		t.Fatalf("Node A -> Node B failed: warns=%v err=%v", warns, err)
	}
	if strings.Contains(outB, nodeAPath) || !strings.Contains(outB, "/Users/bob/.corgi/uploads/staged/20261128/") {
		t.Fatalf("expected Node B staged path in output, got: %s", outB)
	}

	// Legacy source node 404 returns clean unsupported error.
	_, _, err = eng.stageMediaForDirectText(ctx, "", "查看 "+nodeAPath, []model.MediaRef{{Machine: "node-legacy", Path: nodeAPath, Name: "spec.md"}})
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

	// Partial failure: 1 succeeds, 1 fails -> succeeds with warning, strips failed path, appends note.
	text, warns, err := eng.stageMediaForDirectText(ctx, "node-b", "看 "+okFile+" 和 "+missingFile, []model.MediaRef{
		{Path: okFile, Name: "ok.txt"},
		{Path: missingFile, Name: "missing.txt"},
	})
	if err != nil {
		t.Fatalf("expected partial failure to succeed with warnings, got err=%v", err)
	}
	if len(warns) != 1 || strings.Contains(text, missingFile) || !strings.Contains(text, "[未同步: missing.txt]") || !strings.Contains(text, "[未同步的附件]") {
		t.Fatalf("unexpected partial failure output: warns=%v text=%s", warns, text)
	}

	// All cross-machine files fail (offline target) -> returns error blocking dispatch.
	if _, _, err := eng.stageMediaForDirectText(ctx, "offline-node", "看 "+okFile, []model.MediaRef{{Path: okFile, Name: "ok.txt"}}); err == nil {
		t.Fatalf("expected all-failed cross-machine staging to error")
	}

	// Same-machine file not mentioned in goal text -> appends [随消息附带的文件].
	localOut, warns, err := eng.stageMediaForDirectText(ctx, "", "帮我总结这个文件", []model.MediaRef{{Path: okFile, Name: "ok.txt"}})
	if err != nil || len(warns) != 0 || !strings.Contains(localOut, "[随消息附带的文件]") || !strings.Contains(localOut, okFile) {
		t.Fatalf("expected same-machine appendix, warns=%v err=%v out=%s", warns, err, localOut)
	}

	// > 10 files cap: 11th skipped with warning.
	var eleven []model.MediaRef
	for i := 0; i < 11; i++ {
		p := writeTestUpload(t, dir, "20261128/f"+string(rune('a'+i))+".txt", []byte{byte('a' + i)})
		eleven = append(eleven, model.MediaRef{Path: p, Name: filepath.Base(p)})
	}
	_, warns11, err := eng.stageMediaForDirectText(ctx, "node-b", "batch", eleven)
	if err != nil || len(warns11) != 1 || !strings.Contains(warns11[0], "最多 10 个文件") {
		t.Fatalf("expected 11th file capped with warning, err=%v warns=%v", err, warns11)
	}

	// Single file > 25 MiB rejected; 3x20 MiB hits 50 MiB total cap on 3rd file.
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

func TestStageMediaSteerDirectAndAsyncDeliverBatch(t *testing.T) {
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
		t.Fatalf("expected queued steer trigger rewritten to staged path, got %+v", queued)
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

	// 3. Async deliverQueuedWithFleet: machine-origin media is ignored, human-origin media runs async without double delivery.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	eng.mu.Lock()
	eng.runCtx = runCtx
	eng.mu.Unlock()

	live := map[string]liveInfo{"w-remote": {pane: "wr:p1@mac-r", status: "idle", ready: true, kind: "claude"}}
	if err := st.InsertTrigger(ctx, &model.Trigger{
		ID: "t-mach-1", Seq: 1001, Thread: model.MainThread, Source: "chat", Policy: "respond", Status: "queued", From: "machine",
		Text: "from machine " + srcPath, Targets: []string{"w-remote"},
		Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}}, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	beforePosts := postUploadCount.Load()
	eng.deliverQueuedWithFleet(ctx, live, map[string]bool{"mac-r": true}, true)
	if postUploadCount.Load() != beforePosts {
		t.Fatalf("expected machine-origin trigger NOT to stage media")
	}
	mu.Lock()
	remotePrompts = nil
	mu.Unlock()

	gateStage.Store(true)
	tHuman := &model.Trigger{
		ID: "t-hum-1", Seq: 1002, Thread: model.MainThread, Source: "chat", Policy: "respond", Status: "queued", From: "you",
		Text: "human media " + srcPath, Targets: []string{"w-remote"},
		Media: []model.MediaRef{{Path: srcPath, Name: "doc.txt"}}, At: time.Now(),
	}
	if err := st.InsertTrigger(ctx, tHuman); err != nil {
		t.Fatal(err)
	}
	eng.deliverQueuedWithFleet(ctx, live, map[string]bool{"mac-r": true}, true)
	<-stageStarted
	// Second overlapping tick must skip w-remote because deliveringWorkers[w-remote] is active.
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
	if q, _ := st.ListQueuedTriggers(ctx); len(q) != 0 {
		t.Fatalf("expected human media trigger %s acked after async delivery, remaining: %+v", tHuman.ID, q)
	}
	mu.Lock()
	if len(remotePrompts) != 1 || strings.Contains(remotePrompts[0], srcPath) || !strings.Contains(remotePrompts[0], "/Users/r/.corgi/uploads/staged/20261128/") {
		t.Fatalf("expected exactly 1 staged prompt delivered asynchronously, got %+v", remotePrompts)
	}
	mu.Unlock()
}

func TestSweepUploadsRetentionLRUAndSymlinkSafety(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.Local)
	today := now.Format("20060102")
	yesterday := now.AddDate(0, 0, -1).Format("20060102")

	setFile := func(rel string, size int, age time.Duration) string {
		p := writeTestUpload(t, dir, rel, bytes.Repeat([]byte("x"), size))
		ts := now.Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}

	origOld := setFile("20261101/old.txt", 100, 15*24*time.Hour)
	origKeep := setFile("20261120/keep.txt", 100, 5*24*time.Hour)
	wecomOld := setFile("wecom/20261101/old.pdf", 100, 15*24*time.Hour)
	stagedOld := setFile("staged/20261118/old.bin", 100, 8*24*time.Hour)
	stagedKeep := setFile("staged/20261124/keep.bin", 100, 3*24*time.Hour)
	tmpOld := setFile(today+"/.tmp-orphan", 50, 2*time.Hour)
	tmpFresh := setFile(today+"/.tmp-active", 50, 20*time.Minute)
	activeFresh := setFile(today+"/active.bin", 500, 5*time.Minute)

	// Empty date dirs: yesterday should be pruned, today should be preserved.
	emptyOldDir := filepath.Join(dir, "uploads", yesterday)
	emptyTodayDir := filepath.Join(dir, "uploads", "staged", today)
	if err := os.MkdirAll(emptyOldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(emptyTodayDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Symlink pointing outside uploads must never follow or delete external target.
	extTarget := filepath.Join(t.TempDir(), "keep-external.txt")
	if err := os.WriteFile(extTarget, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extTarget, filepath.Join(dir, "uploads", "20261101", "sym.txt")); err != nil {
		t.Fatal(err)
	}

	stats := sweepUploadsWithLimits(context.Background(), dir, now, 650, 600)
	if stats.RemovedFiles < 5 {
		t.Fatalf("expected >=5 removed files (TTLs + orphan + LRU), got %+v", stats)
	}

	mustGone := []string{origOld, wecomOld, stagedOld, tmpOld, origKeep} // origKeep (5d old) evicted by LRU before stagedKeep (3d old)
	for _, p := range mustGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err=%v", p, err)
		}
	}
	mustExist := []string{stagedKeep, tmpFresh, activeFresh, extTarget, emptyTodayDir, filepath.Join(dir, "uploads", "wecom"), filepath.Join(dir, "uploads", "staged")}
	for _, p := range mustExist {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s preserved, err=%v", p, err)
		}
	}
	if _, err := os.Stat(emptyOldDir); !os.IsNotExist(err) {
		t.Fatalf("expected empty historical dir %s pruned", emptyOldDir)
	}
}

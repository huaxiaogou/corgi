package channel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

type sentAttachment struct {
	chat, name string
	data       []byte
}

func newTestAttachmentBus(t *testing.T, home string) (*core.Engine, *Bus, func() []sentAttachment, func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := core.NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		files []sentAttachment
	)
	bus := NewBus(eng, nil, nil)
	bus.SetPaths(filepath.Join(dir, "uploads"), home)
	bus.sendFn = func(context.Context, string, string) error { return nil }
	bus.sendFileFn = func(_ context.Context, chatID, filename string, data []byte) error {
		mu.Lock()
		files = append(files, sentAttachment{chat: chatID, name: filename, data: append([]byte(nil), data...)})
		mu.Unlock()
		return nil
	}

	readFiles := func() []sentAttachment {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]sentAttachment, len(files))
		copy(cp, files)
		return cp
	}
	resetFiles := func() {
		mu.Lock()
		files = nil
		mu.Unlock()
	}
	return eng, bus, readFiles, resetFiles
}

func TestExplicitReplyAttachmentsAnyExtensionSpacesAndNoRegexScraping(t *testing.T) {
	home := t.TempDir()
	spaceDir := filepath.Join(home, "my exports", "2026 Q4")
	corgiNotes := filepath.Join(home, ".corgi", "router", "memory", "notes")
	if err := os.MkdirAll(spaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(corgiNotes, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFixture := func(path string, content []byte) string {
		t.Helper()
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	mdPath := writeFixture(filepath.Join(home, "perf-report.md"), []byte("# Perf\nOK\n"))
	htmlPath := writeFixture(filepath.Join(home, "corgi-inbox-prototype.html"), []byte("<!doctype html><title>Inbox</title>"))
	spaceHTML := writeFixture(filepath.Join(spaceDir, "corgi inbox prototype v1.html"), []byte("<html>spaced</html>"))
	customExt := writeFixture(filepath.Join(spaceDir, "snapshot.customext123"), []byte{0x01, 0x02, 0x03, 0x04})
	extensionless := writeFixture(filepath.Join(home, "CHANGELOG"), []byte("v0.1.39 changes\n"))
	unicodeFile := writeFixture(filepath.Join(spaceDir, "巡检 报告-2026.xyz"), []byte("unicode body"))
	corgiNote := writeFixture(filepath.Join(corgiNotes, "audit.md"), []byte("# Nested Note\n"))
	exactLimitData := bytes.Repeat([]byte("x"), core.MaxReplyAttachmentBytes)
	exactLimitPath := writeFixture(filepath.Join(home, "exact-5mb.bin"), exactLimitData)

	eng, bus, readFiles, resetFiles := newTestAttachmentBus(t, home)
	ctx := context.Background()

	cases := []struct {
		name     string
		files    []string
		wantName string
		wantBody []byte
	}{
		{name: "markdown file", files: []string{mdPath}, wantName: "perf-report.md", wantBody: []byte("# Perf\nOK\n")},
		{name: "html file", files: []string{htmlPath}, wantName: "corgi-inbox-prototype.html", wantBody: []byte("<!doctype html><title>Inbox</title>")},
		{name: "path with spaces", files: []string{spaceHTML}, wantName: "corgi inbox prototype v1.html", wantBody: []byte("<html>spaced</html>")},
		{name: "arbitrary custom extension", files: []string{customExt}, wantName: "snapshot.customext123", wantBody: []byte{0x01, 0x02, 0x03, 0x04}},
		{name: "extensionless regular file", files: []string{extensionless}, wantName: "CHANGELOG", wantBody: []byte("v0.1.39 changes\n")},
		{name: "unicode filename with space and unknown ext", files: []string{unicodeFile}, wantName: "巡检 报告-2026.xyz", wantBody: []byte("unicode body")},
		{name: "nested .corgi router memory deliverable", files: []string{corgiNote}, wantName: "audit.md", wantBody: []byte("# Nested Note\n")},
		{name: "exact 5 MiB limit", files: []string{exactLimitPath}, wantName: "exact-5mb.bin", wantBody: exactLimitData},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetFiles()
			entry, _, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
				Thread: "wecom:grp-explicit",
				Text:   "附件已就绪",
				Files:  tc.files,
			})
			if err != nil {
				t.Fatalf("ReplyWithMeta failed: %v", err)
			}
			if len(entry.Files) != 1 || entry.Files[0] != tc.files[0] {
				t.Fatalf("entry.Files = %#v, want %#v", entry.Files, tc.files)
			}
			got := readFiles()
			if len(got) != 1 {
				t.Fatalf("expected 1 attachment, got %d (%+v)", len(got), got)
			}
			if got[0].chat != "grp-explicit" || got[0].name != tc.wantName || !bytes.Equal(got[0].data, tc.wantBody) {
				t.Fatalf("unexpected attachment: chat=%q name=%q bodyLen=%d", got[0].chat, got[0].name, len(got[0].data))
			}
		})
	}

	t.Run("multiple explicit files deduplicated and delivered on check card reply", func(t *testing.T) {
		resetFiles()
		entry, act, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
			Thread: "wecom:grp-explicit",
			Text:   "请审阅原型和说明",
			Files:  []string{spaceHTML, extensionless, spaceHTML},
			Check:  true,
		})
		if err != nil || act == nil {
			t.Fatalf("ReplyWithMeta(--check --file): act=%v err=%v", act, err)
		}
		if len(entry.Files) != 2 {
			t.Fatalf("expected deduplicated 2 files, got %#v", entry.Files)
		}
		got := readFiles()
		if len(got) != 2 || got[0].name != "corgi inbox prototype v1.html" || got[1].name != "CHANGELOG" {
			t.Fatalf("unexpected delivered attachments: %+v", got)
		}
	})

	t.Run("plain reply prose mentioning paths does NOT auto-scrape attachments", func(t *testing.T) {
		resetFiles()
		bus.OnReply(ctx, &model.Entry{
			Thread: "wecom:grp-explicit",
			Text:   "正文仅提及路径 " + mdPath + " 与 " + htmlPath + "，未显式传 --file",
		}, nil, nil)
		if got := readFiles(); len(got) != 0 {
			t.Fatalf("expected zero attachments without explicit Files, got %+v", got)
		}
	})

	t.Run("long reply fallback still attaches md when no explicit files", func(t *testing.T) {
		resetFiles()
		bus.OnReply(ctx, &model.Entry{
			Thread: "wecom:grp-explicit",
			Text:   strings.Repeat("超长分析正文内容", 260),
		}, nil, nil)
		got := readFiles()
		if len(got) != 1 || !strings.HasPrefix(got[0].name, "corgi-report-") || !strings.HasSuffix(got[0].name, ".md") {
			t.Fatalf("expected fallback corgi-report-*.md attachment, got %+v", got)
		}
	})
}

func TestExplicitReplyAttachmentsRejectRelativeInvalidDirectoriesAndUnsafePaths(t *testing.T) {
	home := t.TempDir()
	workDir := filepath.Join(home, "repo")
	corgiDir := filepath.Join(home, ".corgi")
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(corgiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFixture := func(path string, content []byte) string {
		t.Helper()
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	realFile := writeFixture(filepath.Join(workDir, "demo.html"), []byte("<html>demo</html>"))
	dotfile := writeFixture(filepath.Join(workDir, ".env"), []byte("SECRET=1\n"))
	corgiCfg := writeFixture(filepath.Join(corgiDir, "config.json"), []byte(`{"wecom":{"secret":"top-secret"}}`))
	corgiNode := writeFixture(filepath.Join(corgiDir, "node.json"), []byte(`{"token":"secret-token"}`))
	corgiToken := writeFixture(filepath.Join(corgiDir, "token"), []byte("master-token"))
	sshKey := writeFixture(filepath.Join(sshDir, "id_rsa"), []byte("PRIVATE KEY"))
	emptyFile := writeFixture(filepath.Join(workDir, "empty.bin"), nil)
	oversizeFile := writeFixture(filepath.Join(workDir, "oversize.bin"), bytes.Repeat([]byte("a"), core.MaxReplyAttachmentBytes+1))
	symlinkToSecret := filepath.Join(workDir, "innocent.txt")
	if err := os.Symlink(corgiCfg, symlinkToSecret); err != nil {
		t.Fatal(err)
	}

	// Create real files in cwd to ensure relative paths are rejected by validation, not ENOENT.
	relDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(relDir, "rel.html"), []byte("<html>rel</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(relDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relDir, "sub", "page.md"), []byte("# rel"), 0o600); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(relDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	eng, bus, readFiles, resetFiles := newTestAttachmentBus(t, home)
	ctx := context.Background()

	cases := []struct {
		name string
		path string
	}{
		{name: "empty path", path: ""},
		{name: "whitespace only", path: "   "},
		{name: "nul byte in path", path: realFile + "\x00.txt"},
		{name: "newline in path", path: realFile + "\n"},
		{name: "bare relative file", path: "rel.html"},
		{name: "dot-slash relative file", path: "./rel.html"},
		{name: "nested relative file", path: "sub/page.md"},
		{name: "dot-dot relative file", path: "../sub/page.md"},
		{name: "tilde shorthand non-absolute", path: "~/repo/demo.html"},
		{name: "path traversal dot-dot", path: filepath.Join(workDir, "sub") + "/../demo.html"},
		{name: "path traversal dot segment", path: workDir + "/./demo.html"},
		{name: "double slash non-canonical", path: "/" + realFile},
		{name: "trailing slash directory syntax", path: workDir + "/"},
		{name: "non-existent absolute file", path: filepath.Join(workDir, "missing-file.xyz")},
		{name: "directory path", path: workDir},
		{name: "root directory", path: "/"},
		{name: "system etc file", path: "/etc/hosts"},
		{name: "device node /dev/null", path: "/dev/null"},
		{name: "hidden dotfile", path: dotfile},
		{name: "corgi config.json secret", path: corgiCfg},
		{name: "corgi node.json secret", path: corgiNode},
		{name: "corgi master token", path: corgiToken},
		{name: "ssh directory file", path: sshKey},
		{name: "symlink escaping to blocked secret", path: symlinkToSecret},
		{name: "empty 0-byte file", path: emptyFile},
		{name: "oversized file over 5 MiB", path: oversizeFile},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetFiles()
			if _, _, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
				Thread: "wecom:grp-safe",
				Text:   "非法附件测试",
				Files:  []string{tc.path},
			}); err == nil {
				t.Fatalf("ReplyWithMeta unexpectedly accepted %q", tc.path)
			}
			// Defense-in-depth: direct Bus.OnReply must also reject without calling SendFile.
			bus.OnReply(ctx, &model.Entry{
				Thread: "wecom:grp-safe",
				Text:   "直接投递防御测试",
				Files:  []string{tc.path},
			}, nil, nil)
			if got := readFiles(); len(got) != 0 {
				t.Fatalf("Bus.OnReply unexpectedly sent attachment for %q: %+v", tc.path, got)
			}
		})
	}

	t.Run("too many explicit attachments rejected", func(t *testing.T) {
		tooMany := make([]string, core.MaxReplyAttachments+1)
		for i := range tooMany {
			tooMany[i] = realFile
		}
		if _, _, _, err := eng.ReplyWithMeta(ctx, core.ReplyRequest{
			Thread: "wecom:grp-safe",
			Text:   "过多附件",
			Files:  tooMany,
		}); err == nil {
			t.Fatal("expected error when exceeding MaxReplyAttachments")
		}
	})
}

type rewriteHostRT struct {
	base   http.RoundTripper
	target *url.URL
}

func (rt rewriteHostRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	r2.Host = rt.target.Host
	return rt.base.RoundTrip(r2)
}

func TestCrossMachineReplyAttachmentSingleFetchQuietBypassAndArtifactEvidence(t *testing.T) {
	dir := t.TempDir()
	remoteDir := t.TempDir()
	remoteHTML := filepath.Join(remoteDir, "corgi-inbox-prototype.html")
	remoteBody := []byte("<!doctype html><h1>Remote Mac Prototype</h1>")
	if err := os.WriteFile(remoteHTML, remoteBody, 0o600); err != nil {
		t.Fatal(err)
	}
	remoteSecond := filepath.Join(remoteDir, "notes.txt")
	if err := os.WriteFile(remoteSecond, []byte("second file"), 0o600); err != nil {
		t.Fatal(err)
	}

	var fetchCalls atomic.Int32
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/attachments/read" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		fetchCalls.Add(1)
		var body struct {
			Path string `json:"path"`
		}
		raw, _ := os.ReadFile(remoteHTML)
		_ = raw
		var decErr error
		if err := func() error {
			var b bytes.Buffer
			_, _ = b.ReadFrom(r.Body)
			if strings.Contains(b.String(), "corgi-inbox-prototype.html") {
				body.Path = remoteHTML
			} else if strings.Contains(b.String(), "notes.txt") {
				body.Path = remoteSecond
			} else {
				body.Path = "/etc/hosts"
			}
			return nil
		}(); err != nil {
			decErr = err
		}
		_ = decErr
		name, data, cleanPath, err := core.ReadAttachmentFile(body.Path)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + err.Error() + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Corgi-Filename", url.PathEscape(name))
		w.Header().Set("X-Corgi-Clean-Path", url.PathEscape(cleanPath))
		_, _ = w.Write(data)
	}))
	defer nodeSrv.Close()

	parsedURL, _ := url.Parse(nodeSrv.URL)
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-node", Name: "MacBook", Online: true, Agents: []string{"agy"}},
		HTTPClient: &http.Client{
			Transport: rewriteHostRT{base: nodeSrv.Client().Transport, target: parsedURL},
		},
	})
	ctx := context.Background()
	eng, err := core.NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		sent     []sentAttachment
		failName string
	)
	bus := NewBus(eng, nil, nil)
	bus.sendFn = func(context.Context, string, string) error { return nil }
	bus.sendFileFn = func(_ context.Context, chatID, filename string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if failName != "" && filename == failName {
			return errors.New("wecom errcode=850003: permission denied")
		}
		sent = append(sent, sentAttachment{chat: chatID, name: filename, data: append([]byte(nil), data...)})
		return nil
	}

	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "收纳箱原型企微交付", "发送 HTML", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate user actively looking at Web in quiet mode: explicit --file MUST still bypass suppression!
	bus.RecordWebSeen()
	res, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread:  model.MainThread,
		Text:    "原型已发到企微",
		Machine: "MacBook",
		WorkID:  wi.ID,
		Files:   []string{remoteHTML},
	})
	if err != nil {
		t.Fatalf("ReplyDetailed remote attachment failed: %v", err)
	}
	if fetchCalls.Load() != 1 {
		t.Fatalf("expected single remote fetch, got %d", fetchCalls.Load())
	}
	if res.Entry.FileMachine != "mac-node" {
		t.Fatalf("expected entry.FileMachine=mac-node, got %q", res.Entry.FileMachine)
	}
	if len(res.Delivery.Files) != 1 || res.Delivery.Files[0].Status != "delivered" {
		t.Fatalf("expected delivered outcome, got %+v", res.Delivery.Files)
	}
	mu.Lock()
	if len(sent) != 1 || sent[0].name != "corgi-inbox-prototype.html" || !bytes.Equal(sent[0].data, remoteBody) {
		t.Fatalf("unexpected remote bytes sent: %+v", sent)
	}
	mu.Unlock()

	// Verify confirmed delivery artifact was recorded on the work item.
	updatedWI, err := st.GetWorkItem(ctx, wi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedWI.Artifacts) != 1 || updatedWI.Artifacts[0].EntryID != res.Entry.ID || updatedWI.Artifacts[0].Machine != "mac-node" {
		t.Fatalf("expected artifact evidence recorded on work item, got %+v", updatedWI.Artifacts)
	}

	// Partial delivery failure: first file delivers, second fails -> caller gets exact per-file status.
	mu.Lock()
	failName = "notes.txt"
	mu.Unlock()
	res2, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread:  model.MainThread,
		Text:    "双附件测试",
		Machine: "mac-node",
		Files:   []string{remoteHTML, remoteSecond},
	})
	if err != nil {
		t.Fatalf("partial failure should still return entry + per-file outcomes, got err=%v", err)
	}
	if len(res2.Delivery.Files) != 2 || res2.Delivery.Files[0].Status != "delivered" || res2.Delivery.Files[1].Status != "failed" {
		t.Fatalf("expected [delivered, failed] per-file outcomes, got %+v", res2.Delivery.Files)
	}
	if !strings.Contains(res2.Warning, "850003") {
		t.Fatalf("expected warning mentioning failure reason, got %q", res2.Warning)
	}
}

func TestWeComPreviewLinksAcrossAllThreeDeliveryBranches(t *testing.T) {
	home := t.TempDir()
	htmlPath := filepath.Join(home, "proto.html")
	htmlBytes := []byte("<!doctype html><h1>Hello</h1>")
	if err := os.WriteFile(htmlPath, htmlBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	eng, bus, readFiles, resetFiles := newTestAttachmentBus(t, home)
	eng.SetPublicURLs([]string{"https://taihu.example/corgi"})
	ctx := context.Background()

	var (
		mu   sync.Mutex
		msgs []struct{ chat, body string }
	)
	bus.sendMarkdownACK = func(_ context.Context, chatID, md string) error {
		mu.Lock()
		msgs = append(msgs, struct{ chat, body string }{chat: chatID, body: md})
		mu.Unlock()
		return nil
	}
	readMsgs := func() []struct{ chat, body string } {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]struct{ chat, body string }, len(msgs))
		copy(cp, msgs)
		return cp
	}
	resetAll := func() {
		mu.Lock()
		msgs = nil
		mu.Unlock()
		resetFiles()
	}

	// 1. Ordinary reply with --file: bypasses 20s outbox, sends 1 markdown (prose + preview link) + raw HTML file.
	resetAll()
	res1, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: "wecom:grp-demo",
		Text:   "原型做好了",
		Files:  []string{htmlPath},
	})
	if err != nil {
		t.Fatalf("ordinary reply failed: %v", err)
	}
	if res1.Delivery.LinkStatus != "delivered" {
		t.Fatalf("want LinkStatus=delivered, got %+v", res1.Delivery)
	}
	m1 := readMsgs()
	f1 := readFiles()
	if len(m1) != 1 || !strings.Contains(m1[0].body, "原型做好了") || !strings.Contains(m1[0].body, "https://taihu.example/corgi/p/") {
		t.Fatalf("expected 1 immediate markdown with prose + preview link, got %+v", m1)
	}
	if len(f1) != 1 || f1[0].name != "proto.html" || !bytes.Equal(f1[0].data, htmlBytes) {
		t.Fatalf("expected raw html attachment alongside preview link, got %+v", f1)
	}

	// 2. Quiet-suppressed reply (active Web on chat:main) with --file: compact links-only message (NO prose) + raw file.
	resetAll()
	bus.RecordWebSeen()
	res2, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: model.MainThread,
		Text:   "这段正文在静默模式下不应重复发企微",
		Files:  []string{htmlPath},
	})
	if err != nil {
		t.Fatalf("quiet reply failed: %v", err)
	}
	if res2.Delivery.LinkStatus != "delivered" {
		t.Fatalf("quiet reply want LinkStatus=delivered, got %+v", res2.Delivery)
	}
	m2 := readMsgs()
	f2 := readFiles()
	if len(m2) != 1 || strings.Contains(m2[0].body, "不应重复发企微") || !strings.Contains(m2[0].body, "https://taihu.example/corgi/p/") {
		t.Fatalf("quiet mode should send links-only without prose, got %+v", m2)
	}
	if len(f2) != 1 || !bytes.Equal(f2[0].data, htmlBytes) {
		t.Fatalf("quiet mode should still deliver raw file, got %+v", f2)
	}

	// 3. Action card (--check) with --file on non-suppressed thread: sends compact links-only message + raw file.
	resetAll()
	res3, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: "wecom:grp-demo",
		Text:   "请验收原型",
		Check:  true,
		Files:  []string{htmlPath},
	})
	if err != nil {
		t.Fatalf("check reply failed: %v", err)
	}
	if res3.Delivery.LinkStatus != "delivered" {
		t.Fatalf("check reply want LinkStatus=delivered, got %+v", res3.Delivery)
	}
	m3 := readMsgs()
	f3 := readFiles()
	if len(m3) != 1 || !strings.Contains(m3[0].body, "https://taihu.example/corgi/p/") {
		t.Fatalf("check card reply should emit preview link message, got %+v", m3)
	}
	if len(f3) != 1 || !bytes.Equal(f3[0].data, htmlBytes) {
		t.Fatalf("check card reply should emit raw file, got %+v", f3)
	}

	// 4. Without PublicURL configured: preview link skipped cleanly, raw file STILL delivered!
	resetAll()
	eng.SetPublicURLs(nil)
	res4, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: "wecom:grp-demo",
		Text:   "无公网地址测试",
		Files:  []string{htmlPath},
	})
	if err != nil {
		t.Fatalf("no-url reply failed: %v", err)
	}
	if res4.Delivery.LinkStatus != "skipped" {
		t.Fatalf("want LinkStatus=skipped without PublicURL, got %+v", res4.Delivery)
	}
	if f4 := readFiles(); len(f4) != 1 || !bytes.Equal(f4[0].data, htmlBytes) {
		t.Fatalf("raw file should still deliver when PublicURL is unset, got %+v", f4)
	}

	// 5. Prose mentioning deliverable path without --file emits warning hint without sending any file.
	resetAll()
	res5, err := eng.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: "wecom:grp-demo",
		Text:   "原型已经保存在 " + htmlPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res5.Warning, "--file") {
		t.Fatalf("expected --file warning hint when prose mentions deliverable path, got %q", res5.Warning)
	}
	if f5 := readFiles(); len(f5) != 0 {
		t.Fatalf("prose without --file must NEVER send file, got %+v", f5)
	}
}

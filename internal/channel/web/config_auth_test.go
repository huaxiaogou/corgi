package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func TestMachineTokenForbiddenOnSettingsWeComDreamAndInspectPath(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()

	mCode, _ := srv.Auth.NewCode("machine")
	_, machineTok, err := srv.Auth.Redeem(context.Background(), mCode, "machine", "node-1", "darwin")
	if err != nil {
		t.Fatalf("redeem machine: %v", err)
	}
	bCode, _ := srv.Auth.NewCode("browser")
	_, browserTok, err := srv.Auth.Redeem(context.Background(), bCode, "browser", "owner-mac", "darwin")
	if err != nil {
		t.Fatalf("redeem browser: %v", err)
	}

	// 1. Machine token must be 403 on ALL settings/wecom/dream super endpoints.
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/super/settings", ""},
		{http.MethodPost, "/api/super/settings", `{"settings":{"ask_permissions":false,"projects":[{"name":"pwn","paths":{"hub":"/tmp"}}]}}`},
		{http.MethodPost, "/api/super/settings", `{"policies":[{"source":"webhook:ci","action":"respond"}]}`},
		{http.MethodPost, "/api/super/wecom/approve", `{"user_id":"attacker"}`},
		{http.MethodPost, "/api/super/dream", `{}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		req.Header.Set("Authorization", "Bearer "+machineTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("machine token %s %s: want 403, got %d (%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// 2. Owner browser token can inspect local project dirs, while dangerous dirs/symlinks are rejected.
	repo := filepath.Join(t.TempDir(), "my-repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	reqOK := httptest.NewRequest(http.MethodPost, "/api/path/inspect", bytes.NewBufferString(`{"path":"`+repo+`"}`))
	reqOK.Header.Set("Authorization", "Bearer "+browserTok)
	recOK := httptest.NewRecorder()
	h.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("inspect valid repo: want 200, got %d (%s)", recOK.Code, recOK.Body.String())
	}
	var insp model.PathInspectResult
	if err := json.Unmarshal(recOK.Body.Bytes(), &insp); err != nil {
		t.Fatal(err)
	}
	if !insp.Exists || !insp.IsDir || !insp.IsGit {
		t.Fatalf("unexpected inspect result: %+v", insp)
	}

	// 3. NodeRoutes (s.Store == nil) must safely inspect without panicking and still reject /etc.
	nodeSrv := &Server{
		Home:     srv.Home,
		CorgiDir: filepath.Join(srv.Home, ".corgi-node"),
		NodeOnly: true,
	}
	nh := nodeSrv.NodeRoutes()
	nReqOK := httptest.NewRequest(http.MethodPost, "/api/path/inspect", bytes.NewBufferString(`{"path":"`+repo+`"}`))
	nRecOK := httptest.NewRecorder()
	nh.ServeHTTP(nRecOK, nReqOK)
	if nRecOK.Code != http.StatusOK {
		t.Fatalf("node inspect valid repo: want 200, got %d (%s)", nRecOK.Code, nRecOK.Body.String())
	}
	nReqBad := httptest.NewRequest(http.MethodPost, "/api/path/inspect", bytes.NewBufferString(`{"path":"/etc"}`))
	nRecBad := httptest.NewRecorder()
	nh.ServeHTTP(nRecBad, nReqBad)
	if nRecBad.Code != http.StatusBadRequest {
		t.Fatalf("node inspect /etc: want 400, got %d (%s)", nRecBad.Code, nRecBad.Body.String())
	}
}

func TestFleetUpdaterCheckReadOnlyExpectedTargetAndBusyGuard(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Version = "v0.1.78"
	releaseTag := "v0.1.79"
	relSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/releases/tag/"+releaseTag, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer relSrv.Close()
	srv.ReleaseRepoURL = relSrv.URL

	// 1. CheckUpdate is strictly read-only (does not mutate cached latestTag).
	chk, err := srv.CheckUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if chk.CurrentVersion != "v0.1.78" || chk.LatestVersion != "v0.1.79" || !chk.HasUpdate || len(chk.Outdated) != 1 {
		t.Fatalf("unexpected CheckUpdate result: %+v", chk)
	}
	srv.updateMu.Lock()
	cached := srv.latestTag
	srv.updateMu.Unlock()
	if cached != "" {
		t.Fatalf("CheckUpdate must be strictly read-only, mutated latestTag=%q", cached)
	}

	// 2. TriggerFleetUpdate rejects mismatched expectedTarget (release drifted to v0.1.80).
	releaseTag = "v0.1.80"
	if _, err := srv.TriggerFleetUpdate(context.Background(), "v0.1.79"); err == nil {
		t.Fatalf("expected TriggerFleetUpdate to reject drifted target v0.1.79 vs v0.1.80")
	}

	// 3. Empty-plan race: synchronize inside fetchLatestTagErr so fleetUpdate becomes busy mid-flight
	// before empty-plan branch acquires fleetUpdateMu, proving the empty-plan busy recheck preserves busy state.
	enteredFetch := make(chan struct{})
	releaseFetch := make(chan struct{})
	raceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			close(enteredFetch)
			<-releaseFetch
			http.Redirect(w, r, "/releases/tag/v0.1.78", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer raceSrv.Close()
	srv.ReleaseRepoURL = raceSrv.URL

	type triggerOut struct {
		res core.UpdateTriggerResult
		err error
	}
	doneCh := make(chan triggerOut, 1)
	go func() {
		r, e := srv.TriggerFleetUpdate(context.Background(), "v0.1.78")
		doneCh <- triggerOut{res: r, err: e}
	}()
	<-enteredFetch
	srv.fleetUpdateMu.Lock()
	srv.fleetUpdate = &fleetUpdateState{
		view: FleetUpdateView{Busy: true, Phase: "updating", Target: "v0.1.80"},
	}
	srv.fleetUpdateMu.Unlock()
	close(releaseFetch)

	got := <-doneCh
	if got.err != nil || !got.res.Busy || got.res.Started || got.res.Target != "v0.1.80" {
		t.Fatalf("expected empty-plan busy recheck to preserve active v0.1.80 run, got res=%+v err=%v", got.res, got.err)
	}
}

func TestHandleUploadOversizedReturns413(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()
	tooBig := bytes.Repeat([]byte("a"), maxUploadBytes+16)

	// Regular upload >25 MiB must return 413 (not 500).
	req := httptest.NewRequest(http.MethodPost, "/api/uploads?name=huge.bin", bytes.NewReader(tooBig))
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected regular upload >25MB to return 413, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Staged upload >25 MiB must return 413 (not 400).
	req2 := httptest.NewRequest(http.MethodPost, "/api/uploads?scope=staged&sha256="+strings.Repeat("a", 64)+"&name=huge.bin", bytes.NewReader(tooBig))
	req2.Header.Set("Authorization", "Bearer "+master)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected staged upload >25MB to return 413, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

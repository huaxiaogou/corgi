package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

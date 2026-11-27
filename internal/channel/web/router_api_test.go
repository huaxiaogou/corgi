package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterAPIKeyOwnerAuthMaskingAndDBReadBlock(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Routes()
	dbPath := srv.Store.DBPath()

	// Create one paired machine token and one paired browser token.
	machineCode, _ := srv.Auth.NewCode("machine")
	_, machineTok, err := srv.Auth.Redeem(context.Background(), machineCode, "machine", "worker-box", "linux")
	if err != nil {
		t.Fatalf("redeem machine: %v", err)
	}
	browserCode, _ := srv.Auth.NewCode("browser")
	_, browserTok, err := srv.Auth.Redeem(context.Background(), browserCode, "browser", "Owner Mac", "darwin")
	if err != nil {
		t.Fatalf("redeem browser: %v", err)
	}

	// 1. Paired machine token MUST be forbidden from setting API key, testing API, or modifying router settings.
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/super/router/api-key", `{"provider":"tokenhub","api_key":"sk-leak-1234"}`},
		{"/api/super/router/test-api", `{"provider":"tokenhub","model":"hy4"}`},
		{"/api/super/settings", `{"settings":{"router_mode":"api","router_api_provider":"tokenhub","router_api_model":"hy4"}}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+machineTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("machine token to %s: expected 403, got %d (%s)", tc.path, rec.Code, rec.Body.String())
		}
	}

	// 2. Owner browser token saves API key; GET /api/super/settings must NEVER echo plaintext key.
	saveReq := httptest.NewRequest(http.MethodPost, "/api/super/router/api-key",
		bytes.NewBufferString(`{"provider":"tokenhub","api_key":"sk-super-secret-4321"}`))
	saveReq.Header.Set("Authorization", "Bearer "+browserTok)
	saveRec := httptest.NewRecorder()
	h.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("browser save key failed: %d %s", saveRec.Code, saveRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/super/settings", nil)
	getReq.Header.Set("Authorization", "Bearer "+browserTok)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get settings failed: %d", getRec.Code)
	}
	rawBody := getRec.Body.String()
	if strings.Contains(rawBody, "sk-super-secret-4321") {
		t.Fatalf("plaintext API key leaked in GET /api/super/settings!")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawBody), &parsed); err != nil {
		t.Fatalf("decode settings resp: %v", err)
	}
	if parsed["has_router_api_key"] != true || parsed["router_api_key_masked"] != "••••••••4321" {
		t.Fatalf("unexpected masked secret fields: %+v", parsed)
	}

	// 3. Reading the Hub SQLite DB or WAL via /api/file must be forbidden.
	for _, p := range []string{dbPath, dbPath + "-wal"} {
		fReq := httptest.NewRequest(http.MethodGet, "/api/file?path="+p, nil)
		fReq.Header.Set("Authorization", "Bearer "+browserTok)
		fRec := httptest.NewRecorder()
		h.ServeHTTP(fRec, fReq)
		if fRec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 reading protected db path %s, got %d", p, fRec.Code)
		}
	}
}

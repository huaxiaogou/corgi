package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func setupCLITestDir(t *testing.T, serverURL string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "addr"), []byte(strings.TrimPrefix(serverURL, "http://")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCmdUpdateCheckDoesNotTriggerFleetUpdate(t *testing.T) {
	var fleetCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": "v0.1.64",
				"latest":  "v0.1.65",
				"outdated": []map[string]any{
					{"id": "", "name": "hub", "version": "v0.1.64"},
				},
				"machines": []map[string]any{
					{"id": "", "name": "hub", "version": "v0.1.64", "os": "linux/amd64", "local": true, "online": true},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/update/fleet":
			fleetCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := setupCLITestDir(t, srv.URL)

	if err := Run(dir, "update", []string{"--check"}); err != nil {
		t.Fatalf("corgi update --check failed: %v", err)
	}
	if err := Run(dir, "upgrade", []string{"--check"}); err != nil {
		t.Fatalf("corgi upgrade alias --check failed: %v", err)
	}
	if fleetCalls.Load() != 0 {
		t.Fatalf("--check must never trigger fleet update")
	}
}

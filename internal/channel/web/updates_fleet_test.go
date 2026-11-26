package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
)

func TestUpdateRouteSeparationAndExactMachineIDLookup(t *testing.T) {
	srv, master := newTestServer(t)
	srv.Version = "v0.1.40"
	h := srv.Routes()

	// 1. /api/update/relay must NOT be exposed on public Hub Routes().
	req := httptest.NewRequest(http.MethodPost, "/api/update/relay", bytes.NewBufferString("fake"))
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected /api/update/relay blocked on public Routes(), got %d %s", rec.Code, rec.Body.String())
	}

	// 2. ?machine=hub must resolve strictly by exact remote device ID (never fall back to Hub self-update).
	req2 := httptest.NewRequest(http.MethodPost, "/api/update?machine=hub", nil)
	req2.Header.Set("Authorization", "Bearer "+master)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unknown remote device id 'hub', got %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestHubRelayedNodeUpdateAndTamperProtection(t *testing.T) {
	oldBase := updateRetryBaseDelay
	updateRetryBaseDelay = time.Millisecond
	defer func() { updateRetryBaseDelay = oldBase }()

	srv, master := newTestServer(t)
	srv.Version = "v0.1.40"

	assetName := "corgi-" + runtime.GOOS + "-" + runtime.GOARCH
	fakeBin := []byte("#!/bin/sh\necho \"corgi v0.1.41\"\n")
	sum := sha256.Sum256(fakeBin)
	sumHex := hex.EncodeToString(sum[:])

	var dlCount atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Header().Set("Location", "https://example.invalid/huaxiaogou/corgi/releases/tag/v0.1.41")
			w.WriteHeader(http.StatusFound)
		case "/releases/download/v0.1.41/SHA256SUMS.txt":
			_, _ = w.Write([]byte(sumHex + "  " + assetName + "\n"))
		case "/releases/download/v0.1.41/" + assetName:
			dlCount.Add(1)
			_, _ = w.Write(fakeBin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()
	srv.ReleaseRepoURL = ghSrv.URL

	// Build a real NodeRoutes server whose direct /api/update returns relayable 502 (no GitHub access),
	// while /api/update/relay executes the real handleNodeRelayUpdate handler.
	nodeExe := filepath.Join(t.TempDir(), "corgi")
	if err := os.WriteFile(nodeExe, []byte("old-node"), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeExecCh := make(chan string, 1)
	nodeSrv := &Server{
		Version:        "v0.1.40",
		NodeOnly:       true,
		ExecutablePath: nodeExe,
		ExecFn:         func(exe string) { nodeExecCh <- exe },
	}
	nodeMux := nodeSrv.NodeRoutes()

	tunnelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/update" {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":     "查不到最新版本：连接 GitHub 超时",
				"code":      "upstream_net",
				"relayable": true,
			})
			return
		}
		nodeMux.ServeHTTP(w, r)
	}))
	defer tunnelSrv.Close()

	attachFakeRemote(srv.Fleet, "devbox-1", "DevBox", "v0.1.40", runtime.GOOS+"/"+runtime.GOARCH, tunnelSrv.URL)

	// 1. Trigger single remote update -> direct fails 502 relayable -> Hub downloads & relays -> Node installs!
	req := httptest.NewRequest(http.MethodPost, "/api/update?machine=devbox-1", nil)
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("relayed update failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"relayed":true`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"version":"v0.1.41"`)) {
		t.Fatalf("unexpected relay response: %s", rec.Body.String())
	}
	select {
	case got := <-nodeExecCh:
		if got != nodeExe {
			t.Fatalf("node ExecFn got %q want %q", got, nodeExe)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected Node ExecFn invoked after relay")
	}
	gotBytes, _ := os.ReadFile(nodeExe)
	if !bytes.Equal(gotBytes, fakeBin) {
		t.Fatalf("node binary was not atomically replaced")
	}

	// 2. Tampered binary on NodeRoutes /api/update/relay must be rejected without touching binary.
	tamperExe := filepath.Join(t.TempDir(), "corgi")
	_ = os.WriteFile(tamperExe, []byte("intact"), 0o755)
	nodeSrv2 := &Server{Version: "v0.1.40", NodeOnly: true, ExecutablePath: tamperExe}
	badReq := httptest.NewRequest(http.MethodPost, "/api/update/relay", strings.NewReader("corrupted-bytes"))
	badReq.Header.Set(relayUpdateTagHeader, "v0.1.41")
	badReq.Header.Set(relayUpdateSHAHeader, sumHex)
	badRec := httptest.NewRecorder()
	nodeSrv2.NodeRoutes().ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on tampered relay SHA-256, got %d %s", badRec.Code, badRec.Body.String())
	}
	if b, _ := os.ReadFile(tamperExe); string(b) != "intact" {
		t.Fatalf("tampered relay modified executable: %q", string(b))
	}

	// 3. Legacy node (direct 502 + relay 404) returns helpful manual install hint.
	legacySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/update" {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "无法获取最新版本号"})
			return
		}
		http.NotFound(w, r)
	}))
	defer legacySrv.Close()
	attachFakeRemote(srv.Fleet, "legacy-1", "LegacyBox", "v0.1.39", runtime.GOOS+"/"+runtime.GOARCH, legacySrv.URL)

	legReq := httptest.NewRequest(http.MethodPost, "/api/update?machine=legacy-1", nil)
	legReq.Header.Set("Authorization", "Bearer "+master)
	legRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(legRec, legReq)
	if legRec.Code != http.StatusBadGateway || !strings.Contains(legRec.Body.String(), "install.sh") {
		t.Fatalf("expected manual install.sh hint for legacy node, got %d %s", legRec.Code, legRec.Body.String())
	}
}

func TestFleetUpdateNodesFirstPartialFailureAndRetry(t *testing.T) {
	oldWait := nodeReconnectTimeout
	oldDelay := updateRetryBaseDelay
	nodeReconnectTimeout = 2 * time.Second
	updateRetryBaseDelay = time.Millisecond
	defer func() {
		nodeReconnectTimeout = oldWait
		updateRetryBaseDelay = oldDelay
	}()

	srv, master := newTestServer(t)
	srv.Version = "v0.1.40"

	assetName := "corgi-" + runtime.GOOS + "-" + runtime.GOARCH
	fakeBin := []byte("#!/bin/sh\necho \"corgi v0.1.41\"\n")
	sum := sha256.Sum256(fakeBin)
	sumHex := hex.EncodeToString(sum[:])

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Header().Set("Location", "https://example.invalid/huaxiaogou/corgi/releases/tag/v0.1.41")
			w.WriteHeader(http.StatusFound)
		case "/releases/download/v0.1.41/SHA256SUMS.txt":
			_, _ = w.Write([]byte(sumHex + "  " + assetName + "\n"))
		case "/releases/download/v0.1.41/" + assetName:
			_, _ = w.Write(fakeBin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghSrv.Close()

	hubExe := filepath.Join(t.TempDir(), "corgi")
	_ = os.WriteFile(hubExe, []byte("old-hub"), 0o755)
	hubExecCh := make(chan string, 1)
	srv.ReleaseRepoURL = ghSrv.URL
	srv.ExecutablePath = hubExe
	srv.ExecFn = func(exe string) { hubExecCh <- exe }

	// Remote node fails on attempt 1 (local disk permission error -> non-relayable),
	// then succeeds on attempt 2 and re-attaches advertising v0.1.41!
	var nodeAttempts atomic.Int32
	var nodeTunnelURL string
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := nodeAttempts.Add(1)
		if n == 1 {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":     "目录无写权限",
				"code":      "local_io",
				"relayable": false,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": "v0.1.40", "version": "v0.1.41"})
		go func() {
			time.Sleep(40 * time.Millisecond)
			attachFakeRemote(srv.Fleet, "mac-1", "MacBook", "v0.1.41", runtime.GOOS+"/"+runtime.GOARCH, nodeTunnelURL)
		}()
	}))
	defer nodeSrv.Close()
	nodeTunnelURL = nodeSrv.URL

	attachFakeRemote(srv.Fleet, "mac-1", "MacBook", "v0.1.40", runtime.GOOS+"/"+runtime.GOARCH, nodeSrv.URL)

	// Run 1: Node fails -> Hub MUST NOT be updated!
	req1 := httptest.NewRequest(http.MethodPost, "/api/update/fleet", nil)
	req1.Header.Set("Authorization", "Bearer "+master)
	rec1 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("fleet update start failed: %d %s", rec1.Code, rec1.Body.String())
	}

	waitFleetIdle(t, srv, 2*time.Second)
	snap := srv.fleetUpdateSnapshot()
	if snap.Phase != "failed" || snap.FailedID != "mac-1" {
		t.Fatalf("expected fleet run failed on mac-1 before Hub, got %+v", snap)
	}
	select {
	case <-hubExecCh:
		t.Fatal("Hub ExecFn MUST NOT run when a remote Node failed")
	default:
	}

	// Run 2 (Retry): Node succeeds & re-attaches on v0.1.41 -> Hub proceeds and replaces itself!
	req2 := httptest.NewRequest(http.MethodPost, "/api/update/fleet", nil)
	req2.Header.Set("Authorization", "Bearer "+master)
	rec2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("fleet retry failed: %d %s", rec2.Code, rec2.Body.String())
	}

	select {
	case gotExe := <-hubExecCh:
		if gotExe != hubExe {
			t.Fatalf("hub ExecFn got %q want %q", gotExe, hubExe)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("expected Hub ExecFn after Node confirmed reconnect; snap=%+v", srv.fleetUpdateSnapshot())
	}
	if b, _ := os.ReadFile(hubExe); !bytes.Equal(b, fakeBin) {
		t.Fatalf("hub binary was not replaced")
	}
}

func TestTryAutoUpdateEnforces24HourAttemptCap(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Version = "v0.1.40"
	ctx := context.Background()
	cfg := srv.Engine.Settings()
	cfg.AutoUpdate = true
	_ = srv.Engine.SaveConfig(ctx, &cfg, nil)

	var latestCalls atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			latestCalls.Add(1)
			w.Header().Set("Location", "https://example.invalid/huaxiaogou/corgi/releases/tag/v0.1.41")
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer ghSrv.Close()
	srv.ReleaseRepoURL = ghSrv.URL
	srv.ExecutablePath = filepath.Join(t.TempDir(), "corgi")
	_ = os.WriteFile(srv.ExecutablePath, []byte("old"), 0o755)

	oldDelay := updateRetryBaseDelay
	updateRetryBaseDelay = time.Millisecond
	defer func() { updateRetryBaseDelay = oldDelay }()

	// First call records attempt and fails (404 on SHA256SUMS.txt).
	srv.TryAutoUpdate(ctx)
	waitFleetIdle(t, srv, 2*time.Second)

	attempts := srv.loadAutoUpdateAttempts(ctx)
	if _, ok := attempts["hub@v0.1.41"]; !ok {
		t.Fatalf("expected hub@v0.1.41 attempt recorded in KV, got %+v", attempts)
	}

	// Second call within 24h must NOT start another fleet run.
	srv.fleetUpdateMu.Lock()
	srv.fleetUpdate = nil
	srv.fleetUpdateMu.Unlock()
	srv.TryAutoUpdate(ctx)
	time.Sleep(50 * time.Millisecond)
	if snap := srv.fleetUpdateSnapshot(); snap.Phase != "idle" {
		t.Fatalf("expected second TryAutoUpdate skipped by 24h cap, got %+v", snap)
	}
}

func TestSanitizeNodeExecArgsStripsPairingCode(t *testing.T) {
	in := []string{"/usr/local/bin/corgi", "node", "--hub", "http://10.0.0.1:7788", "--code", "abc-123", "--name", "mac", "--code=xyz", "--pair-only"}
	got := sanitizeNodeExecArgs(in)
	want := []string{"/usr/local/bin/corgi", "node", "--hub", "http://10.0.0.1:7788", "--name", "mac"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeNodeExecArgs=%v want=%v", got, want)
	}
}

func attachFakeRemote(fl *fleet.Fleet, id, name, ver, osArch, targetBaseURL string) {
	// Rewrite http://node/... to targetBaseURL/... so updateRemoteNode talks to httptest.Server.
	tr := fleetTestRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := strings.TrimRight(targetBaseURL, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u, r.Body)
		if err != nil {
			return nil, err
		}
		req.Header = clone.Header.Clone()
		req.ContentLength = r.ContentLength
		return http.DefaultTransport.RoundTrip(req)
	})
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{
			ID:      id,
			Name:    name,
			OS:      osArch,
			Version: ver,
			Online:  true,
		},
		HTTPClient: &http.Client{Transport: tr},
	})
}

type fleetTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fleetTestRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitFleetIdle(t *testing.T, srv *Server, maxWait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if !srv.isFleetUpdateBusy() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	b, _ := json.Marshal(srv.fleetUpdateSnapshot())
	t.Fatalf("fleet update still busy after %s: %s", maxWait, string(b))
}

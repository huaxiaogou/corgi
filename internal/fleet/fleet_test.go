package fleet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

func TestSplitJoinID(t *testing.T) {
	cases := []struct {
		id, local, machine string
	}{
		{"w1:p1", "w1:p1", ""},
		{"w1:p1@macbook", "w1:p1", "macbook"},
		{"ws1@gpu-01", "ws1", "gpu-01"},
	}
	for _, tc := range cases {
		l, m := SplitID(tc.id)
		if l != tc.local || m != tc.machine {
			t.Fatalf("SplitID(%q) = (%q,%q), want (%q,%q)", tc.id, l, m, tc.local, tc.machine)
		}
		if got := JoinID(l, m); got != tc.id {
			t.Fatalf("JoinID(%q,%q) = %q, want %q", l, m, got, tc.id)
		}
	}
}

func TestAuthPairingAndVerify(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	am := NewAuthManager(st, "master-secret")
	ctx := context.Background()

	if master, _ := am.Verify(ctx, "master-secret"); !master {
		t.Fatal("master token should verify")
	}
	code, _ := am.NewCode("machine")
	if _, _, err := am.Redeem(ctx, code, "browser", "iphone", "ios"); err == nil {
		t.Fatal("wrong kind should fail")
	}
	dev, tok, err := am.Redeem(ctx, code, "machine", "MacBook", "darwin/arm64")
	if err != nil || dev.ID != "macbook" || tok == "" {
		t.Fatalf("redeem failed: dev=%+v err=%v", dev, err)
	}
	// Second redeem must fail (one-time code)
	if _, _, err := am.Redeem(ctx, code, "machine", "MacBook2", "darwin/arm64"); err == nil {
		t.Fatal("one-time code must not be reusable")
	}
	if master, got := am.Verify(ctx, tok); master || got == nil || got.ID != "macbook" {
		t.Fatalf("device token verification failed: master=%v dev=%+v", master, got)
	}
	if _, err := am.Revoke(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, got := am.Verify(ctx, tok); got != nil {
		t.Fatal("revoked token should fail verification")
	}
}

func TestYamuxNodeTunnelRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	am := NewAuthManager(st, "master-secret")
	fl := NewFleet(nil, "hub-host", "/home/dev", "test", []string{"claude"})

	connected := make(chan struct{}, 1)
	fl.SetOnChange(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	code, _ := am.NewCode("machine")
	dev, tok, err := am.Redeem(ctx, code, "machine", "macbook", "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/node/connect", func(w http.ResponseWriter, r *http.Request) {
		_, d := am.Verify(r.Context(), "dummy")
		_ = d
		AcceptNode(w, r, dev, fl, nil)
	})
	hubSrv := httptest.NewServer(mux)
	defer hubSrv.Close()

	nodeAPI := http.NewServeMux()
	nodeAPI.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong-from-macbook"))
	})

	go func() {
		_ = RunNodeSession(ctx, NodeSessionOpts{
			Config:  NodeConfig{Hub: hubSrv.URL, ID: dev.ID, Name: "macbook", Token: tok},
			Name:    "macbook",
			Home:    "/Users/dev",
			Version: "test",
			Agents:  []string{"claude", "agy"},
			NodeAPI: nodeAPI,
		})
	}()

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for node tunnel to connect")
	}

	hc, err := fl.HTTPClient("macbook")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	resp, err := hc.Get("http://node/ping")
	if err != nil {
		t.Fatalf("tunneled GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong-from-macbook" {
		t.Fatalf("unexpected tunneled body: %q", string(body))
	}

	ms := fl.Machines()
	if len(ms) != 2 {
		t.Fatalf("want 2 machines (hub+macbook), got %d: %+v", len(ms), ms)
	}
	var gotMac model.Machine
	for _, m := range ms {
		if m.ID == "macbook" {
			gotMac = m
		}
	}
	if !gotMac.Online || len(gotMac.Agents) != 2 {
		t.Fatalf("unexpected remote machine info: %+v", gotMac)
	}
}

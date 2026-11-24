package fleet

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartLocalUIProxy(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From", "hub")
		_, _ = io.WriteString(w, "hello:"+r.URL.Path)
	}))
	defer hub.Close()

	// Reserve a free loopback port, close it, then pass to StartLocalUIProxy.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartLocalUIProxy(ctx, addr, hub.URL, nil); err != nil {
		t.Fatalf("StartLocalUIProxy: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/api/session")
	if err != nil {
		t.Fatalf("proxy GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello:/api/session" || resp.Header.Get("X-From") != "hub" {
		t.Fatalf("unexpected proxy response: status=%d body=%q header=%q", resp.StatusCode, string(body), resp.Header.Get("X-From"))
	}
}

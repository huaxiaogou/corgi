package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// StartLocalUIProxy serves a loopback-only HTTP reverse proxy to hubURL (e.g. 127.0.0.1:17788)
// so desktop browsers on remote nodes treat the Hub UI as a secure origin for PWA install & notifications.
func StartLocalUIProxy(ctx context.Context, listenAddr, hubURL string, logger *slog.Logger) error {
	addr := strings.TrimSpace(listenAddr)
	if addr == "" || strings.EqualFold(addr, "off") {
		return nil
	}
	target, err := url.Parse(strings.TrimRight(hubURL, "/"))
	if err != nil {
		return fmt.Errorf("parse hub url for local ui: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen local ui %s: %w", addr, err)
	}

	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "Hub 暂时不可达: "+err.Error(), http.StatusBadGateway)
		},
	}

	srv := &http.Server{Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed && logger != nil {
			logger.Warn("local ui proxy stopped", "addr", addr, "err", err)
		}
	}()
	if logger != nil {
		logger.Info("local node ui proxy listening", "url", "http://"+ln.Addr().String(), "hub", target.String())
	}
	return nil
}

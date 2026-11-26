package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func parseCSPDirectives(csp string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(csp, ";") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		name, val, _ := strings.Cut(p, " ")
		out[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(val)
	}
	return out
}

func TestHTMLPreviewSecurityHeadersAndHEAD(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()

	htmlPath := filepath.Join(srv.Home, "report.html")
	htmPath := filepath.Join(srv.Home, "legacy.htm")
	svgPath := filepath.Join(srv.Home, "icon.svg")
	txtPath := filepath.Join(srv.Home, "note.txt")
	bigPath := filepath.Join(srv.Home, "big.html")
	subDir := filepath.Join(srv.Home, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmlPath, []byte("<!doctype html><h1>ok</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmPath, []byte("<p>legacy</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svgPath, []byte("<svg><script>alert(1)</script></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txtPath, []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bigPath, bytes.Repeat([]byte("a"), maxHTMLRead+128), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pair a machine token to verify it cannot access /api/file HTML preview.
	code, _ := srv.Auth.NewCode("machine")
	_, nodeTok, err := srv.Auth.Redeem(context.Background(), code, "machine", "macbook", "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		method           string
		target           string
		token            string
		wantCode         int
		wantXFO          string
		wantHTMLCSP      bool
		wantAllowScripts bool
		wantEmpty        bool
		wantTruncate     bool
	}{
		{
			name:        "GET html preview has strict sandbox CSP and SAMEORIGIN",
			method:      http.MethodGet,
			target:      "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html",
			token:       master,
			wantCode:    http.StatusOK,
			wantXFO:     "SAMEORIGIN",
			wantHTMLCSP: true,
		},
		{
			name:             "GET html preview with scripts=1 enables inline sandbox scripts only",
			method:           http.MethodGet,
			target:           "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html&scripts=1",
			token:            master,
			wantCode:         http.StatusOK,
			wantXFO:          "SAMEORIGIN",
			wantHTMLCSP:      true,
			wantAllowScripts: true,
		},
		{
			name:             "HEAD html preview with scripts=1 returns opt-in CSP and empty body",
			method:           http.MethodHead,
			target:           "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html&scripts=1",
			token:            master,
			wantCode:         http.StatusOK,
			wantXFO:          "SAMEORIGIN",
			wantHTMLCSP:      true,
			wantAllowScripts: true,
			wantEmpty:        true,
		},
		{
			name:        "GET html preview ignores non-1 scripts param",
			method:      http.MethodGet,
			target:      "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html&scripts=true",
			token:       master,
			wantCode:    http.StatusOK,
			wantXFO:     "SAMEORIGIN",
			wantHTMLCSP: true,
		},
		{
			name:        "GET htm preview allowed",
			method:      http.MethodGet,
			target:      "/api/file?path=" + url.QueryEscape(htmPath) + "&as=html",
			token:       master,
			wantCode:    http.StatusOK,
			wantXFO:     "SAMEORIGIN",
			wantHTMLCSP: true,
		},
		{
			name:        "HEAD html preview returns 200 empty body with headers",
			method:      http.MethodHead,
			target:      "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html",
			token:       master,
			wantCode:    http.StatusOK,
			wantXFO:     "SAMEORIGIN",
			wantHTMLCSP: true,
			wantEmpty:   true,
		},
		{
			name:         "HEAD oversized html flags truncation",
			method:       http.MethodHead,
			target:       "/api/file?path=" + url.QueryEscape(bigPath) + "&as=html",
			token:        master,
			wantCode:     http.StatusOK,
			wantXFO:      "SAMEORIGIN",
			wantHTMLCSP:  true,
			wantEmpty:    true,
			wantTruncate: true,
		},
		{
			name:      "HEAD missing html returns 404 DENY with empty body",
			method:    http.MethodHead,
			target:    "/api/file?path=" + url.QueryEscape(filepath.Join(srv.Home, "nope.html")) + "&as=html",
			token:     master,
			wantCode:  http.StatusNotFound,
			wantXFO:   "DENY",
			wantEmpty: true,
		},
		{
			name:     "GET svg as html rejected with 400 DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(svgPath) + "&as=html",
			token:    master,
			wantCode: http.StatusBadRequest,
			wantXFO:  "DENY",
		},
		{
			name:     "GET txt as html rejected with 400 DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(txtPath) + "&as=html",
			token:    master,
			wantCode: http.StatusBadRequest,
			wantXFO:  "DENY",
		},
		{
			name:     "GET dir as html rejected with 400 DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(subDir) + "&as=html",
			token:    master,
			wantCode: http.StatusBadRequest,
			wantXFO:  "DENY",
		},
		{
			name:     "GET html download keeps XFO DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(htmlPath) + "&download=1",
			token:    master,
			wantCode: http.StatusOK,
			wantXFO:  "DENY",
		},
		{
			name:     "GET html source JSON keeps XFO DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(htmlPath),
			token:    master,
			wantCode: http.StatusOK,
			wantXFO:  "DENY",
		},
		{
			name:     "GET /api/session keeps XFO DENY",
			method:   http.MethodGet,
			target:   "/api/session",
			token:    master,
			wantCode: http.StatusOK,
			wantXFO:  "DENY",
		},
		{
			name:     "unauthenticated html preview returns 401 DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html",
			token:    "",
			wantCode: http.StatusUnauthorized,
			wantXFO:  "DENY",
		},
		{
			name:     "machine token html preview returns 401 DENY",
			method:   http.MethodGet,
			target:   "/api/file?path=" + url.QueryEscape(htmlPath) + "&as=html",
			token:    nodeTok,
			wantCode: http.StatusUnauthorized,
			wantXFO:  "DENY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := rec.Header().Values("X-Frame-Options"); len(got) != 1 || got[0] != tc.wantXFO {
				t.Fatalf("X-Frame-Options=%v want=[%s]", got, tc.wantXFO)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options=%q want=nosniff", got)
			}
			if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("Referrer-Policy=%q want=no-referrer", got)
			}
			if tc.wantEmpty && rec.Body.Len() != 0 {
				t.Fatalf("expected empty body for HEAD, got %d bytes", rec.Body.Len())
			}
			if tc.wantTruncate && rec.Header().Get("X-Corgi-Truncated") != "1" {
				t.Fatalf("expected X-Corgi-Truncated=1, got %q", rec.Header().Get("X-Corgi-Truncated"))
			}
			if tc.wantHTMLCSP {
				if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
					t.Fatalf("Content-Type=%q want=text/html; charset=utf-8", ct)
				}
				if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
					t.Fatalf("Cache-Control=%q want=no-store", cc)
				}
				csp := rec.Header().Get("Content-Security-Policy")
				directives := parseCSPDirectives(csp)
				wantExact := map[string]string{
					"default-src":     "'none'",
					"style-src":       "'unsafe-inline' data: https:",
					"img-src":         "data: blob: https:",
					"font-src":        "data: https:",
					"media-src":       "data: blob:",
					"connect-src":     "'none'",
					"form-action":     "'none'",
					"object-src":      "'none'",
					"base-uri":        "'none'",
					"frame-ancestors": "'self'",
				}
				if tc.wantAllowScripts {
					wantExact["sandbox"] = "allow-scripts"
					wantExact["script-src"] = "'unsafe-inline'"
				} else {
					wantExact["sandbox"] = ""
					if val, exists := directives["script-src"]; exists {
						t.Fatalf("static CSP must not declare script-src, got %q", val)
					}
				}
				for name, wantVal := range wantExact {
					gotVal, ok := directives[name]
					if !ok || gotVal != wantVal {
						t.Fatalf("CSP directive %q=%q (present=%v), want %q (full CSP: %q)", name, gotVal, ok, wantVal, csp)
					}
				}
				for _, mustNotHave := range []string{
					"allow-same-origin",
					"allow-top-navigation",
					"allow-forms",
					"allow-popups",
					"allow-modals",
					"allow-downloads",
					"'unsafe-eval'",
					"http:",
				} {
					if strings.Contains(csp, mustNotHave) {
						t.Fatalf("CSP %q must not contain %q", csp, mustNotHave)
					}
				}
			}
		})
	}
}

func TestForwardedRemoteNodeHTMLPreviewHardensHeaders(t *testing.T) {
	srv, master := newTestServer(t)

	// Simulate a legacy remote node returning old permissive CSP and X-Frame-Options: DENY.
	srv.Fleet.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "macbook", Name: "macbook", Online: true},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.RawQuery, "missing.html") {
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Header:     http.Header{"Content-Type": {"application/json"}, "X-Frame-Options": {"DENY"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":"找不到文件"}`)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type":            {"text/html; charset=utf-8"},
						"Content-Security-Policy": {"sandbox allow-scripts allow-popups allow-modals"},
						"X-Frame-Options":         {"DENY"},
					},
					Body: io.NopCloser(strings.NewReader("<h1>remote</h1>")),
				}, nil
			}),
		},
	})

	h := srv.Routes()

	// 1. Remote 200 HTML response has legacy CSP overridden and single XFO=SAMEORIGIN.
	req := httptest.NewRequest(http.MethodGet, "/api/file?path=/tmp/demo.html&machine=macbook&as=html", nil)
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote html status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	if xfo := rec.Header().Values("X-Frame-Options"); len(xfo) != 1 || xfo[0] != "SAMEORIGIN" {
		t.Fatalf("remote html X-Frame-Options=%v want=[SAMEORIGIN]", xfo)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != htmlSandboxCSP {
		t.Fatalf("remote html CSP=%q want=%q", csp, htmlSandboxCSP)
	}

	// 1b. Remote 200 HTML response with scripts=1 enforces opt-in inline-script CSP only.
	reqScripts := httptest.NewRequest(http.MethodGet, "/api/file?path=/tmp/demo.html&machine=macbook&as=html&scripts=1", nil)
	reqScripts.Header.Set("Authorization", "Bearer "+master)
	recScripts := httptest.NewRecorder()
	h.ServeHTTP(recScripts, reqScripts)
	if recScripts.Code != http.StatusOK {
		t.Fatalf("remote html scripts=1 status=%d want=200 body=%s", recScripts.Code, recScripts.Body.String())
	}
	if xfo := recScripts.Header().Values("X-Frame-Options"); len(xfo) != 1 || xfo[0] != "SAMEORIGIN" {
		t.Fatalf("remote html scripts=1 X-Frame-Options=%v want=[SAMEORIGIN]", xfo)
	}
	if csp := recScripts.Header().Get("Content-Security-Policy"); csp != htmlSandboxScriptsCSP {
		t.Fatalf("remote html scripts=1 CSP=%q want=%q", csp, htmlSandboxScriptsCSP)
	}

	// 2. Remote 404 keeps single XFO=DENY.
	req404 := httptest.NewRequest(http.MethodGet, "/api/file?path=/tmp/missing.html&machine=macbook&as=html", nil)
	req404.Header.Set("Authorization", "Bearer "+master)
	rec404 := httptest.NewRecorder()
	h.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("remote missing status=%d want=404", rec404.Code)
	}
	if xfo := rec404.Header().Values("X-Frame-Options"); len(xfo) != 1 || xfo[0] != "DENY" {
		t.Fatalf("remote missing X-Frame-Options=%v want=[DENY]", xfo)
	}

	// 3. Offline node HEAD returns 503 empty body with XFO=DENY.
	reqOff := httptest.NewRequest(http.MethodHead, "/api/file?path=/tmp/demo.html&machine=offline-box&as=html", nil)
	reqOff.Header.Set("Authorization", "Bearer "+master)
	recOff := httptest.NewRecorder()
	h.ServeHTTP(recOff, reqOff)
	if recOff.Code != http.StatusServiceUnavailable || recOff.Body.Len() != 0 {
		t.Fatalf("offline HEAD status=%d bodyLen=%d want=503 empty", recOff.Code, recOff.Body.Len())
	}
	if xfo := recOff.Header().Values("X-Frame-Options"); len(xfo) != 1 || xfo[0] != "DENY" {
		t.Fatalf("offline HEAD X-Frame-Options=%v want=[DENY]", xfo)
	}
}

package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func newStaticTestHandler() http.Handler {
	bigJS := strings.Repeat("export const corgiAssetPayload = 'monochrome-static-asset-content';\n", 80)
	bigHTML := "<!doctype html><html><body>" + strings.Repeat("<p>Corgi console shell</p>", 30) + "</body></html>"
	fsys := fstest.MapFS{
		"index.html":          {Data: []byte(bigHTML)},
		"assets/index-abc.js": {Data: []byte(bigJS)},
		"assets/plain.js":     {Data: []byte(bigJS)},
		"manifest.webmanifest": {
			Data: []byte(`{"name":"Corgi","short_name":"Corgi","display":"standalone","description":"monochrome control center"}`),
		},
		"tiny.txt": {Data: []byte("ok")},
		"pic.png":  {Data: []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("0", 200))},
	}
	srv := &Server{StaticFS: fsys}
	return srv.Routes()
}

func TestStaticGzipNegotiationAndETag(t *testing.T) {
	t.Parallel()
	h := newStaticTestHandler()
	wantJS := strings.Repeat("export const corgiAssetPayload = 'monochrome-static-asset-content';\n", 80)

	// 1. Accept-Encoding: gzip serves gzipped bytes, Vary, -gz ETag, and immutable cache.
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gzip request status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	gzETag := rec.Header().Get("ETag")
	if !strings.HasSuffix(gzETag, `-gz"`) {
		t.Fatalf("gzip ETag = %q, want -gz suffix", gzETag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q, want immutable", cc)
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decoded, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil || string(decoded) != wantJS {
		t.Fatalf("decoded body mismatch: err=%v len=%d want=%d", err, len(decoded), len(wantJS))
	}

	// 2. Identity request (no header or gzip;q=0) serves raw body, non -gz ETag, still sets Vary.
	for _, ae := range []string{"", "gzip;q=0", "gzip;q=0, *;q=1", "deflate"} {
		r := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("ae=%q status=%d want 200", ae, w.Code)
		}
		if ce := w.Header().Get("Content-Encoding"); ce != "" {
			t.Fatalf("ae=%q unexpected Content-Encoding=%q", ae, ce)
		}
		if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("ae=%q Vary=%q want Accept-Encoding", ae, got)
		}
		if strings.HasSuffix(w.Header().Get("ETag"), `-gz"`) {
			t.Fatalf("ae=%q identity ETag must not have -gz suffix: %q", ae, w.Header().Get("ETag"))
		}
		if w.Body.String() != wantJS {
			t.Fatalf("ae=%q raw body mismatch", ae)
		}
	}

	// Capture identity ETag for conditional checks.
	idReq := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	idRec := httptest.NewRecorder()
	h.ServeHTTP(idRec, idReq)
	rawETag := idRec.Header().Get("ETag")

	// 3. Same-representation If-None-Match -> 304 (including multi-value).
	condGz := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	condGz.Header.Set("Accept-Encoding", "gzip")
	condGz.Header.Set("If-None-Match", `"other", `+gzETag)
	wGz := httptest.NewRecorder()
	h.ServeHTTP(wGz, condGz)
	if wGz.Code != http.StatusNotModified {
		t.Fatalf("matching gzip If-None-Match status=%d want 304", wGz.Code)
	}

	condRaw := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	condRaw.Header.Set("If-None-Match", rawETag)
	wRaw := httptest.NewRecorder()
	h.ServeHTTP(wRaw, condRaw)
	if wRaw.Code != http.StatusNotModified {
		t.Fatalf("matching raw If-None-Match status=%d want 304", wRaw.Code)
	}

	// 4. Cross-representation If-None-Match -> 200.
	cross := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	cross.Header.Set("Accept-Encoding", "gzip")
	cross.Header.Set("If-None-Match", rawETag)
	wCross := httptest.NewRecorder()
	h.ServeHTTP(wCross, cross)
	if wCross.Code != http.StatusOK || wCross.Header().Get("ETag") != gzETag {
		t.Fatalf("cross-representation status=%d etag=%q want 200 %q", wCross.Code, wCross.Header().Get("ETag"), gzETag)
	}
}

func TestStaticHeadRangeAndCacheFallbackSemantics(t *testing.T) {
	t.Parallel()
	h := newStaticTestHandler()

	// HEAD returns headers and empty body.
	headReq := httptest.NewRequest(http.MethodHead, "/assets/index-abc.js", nil)
	headReq.Header.Set("Accept-Encoding", "gzip")
	headRec := httptest.NewRecorder()
	h.ServeHTTP(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d want 200", headRec.Code)
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD body len=%d want 0", headRec.Body.Len())
	}
	if headRec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("HEAD Content-Encoding=%q want gzip", headRec.Header().Get("Content-Encoding"))
	}

	// Range request falls back to identity and returns 206 Partial Content.
	rangeReq := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	rangeReq.Header.Set("Accept-Encoding", "gzip")
	rangeReq.Header.Set("Range", "bytes=0-15")
	rangeRec := httptest.NewRecorder()
	h.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("Range status=%d want 206", rangeRec.Code)
	}
	if rangeRec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("Range must fall back to identity, got Content-Encoding=%q", rangeRec.Header().Get("Content-Encoding"))
	}
	if rangeRec.Body.String() != "export const cor" {
		t.Fatalf("Range body = %q, want %q", rangeRec.Body.String(), "export const cor")
	}

	// Missing /assets/* returns true 404 (never HTML fallback).
	missReq := httptest.NewRequest(http.MethodGet, "/assets/nonexistent.js", nil)
	missRec := httptest.NewRecorder()
	h.ServeHTTP(missRec, missReq)
	if missRec.Code != http.StatusNotFound {
		t.Fatalf("missing /assets/* status=%d want 404", missRec.Code)
	}
	if strings.Contains(strings.ToLower(missRec.Header().Get("Content-Type")), "text/html") ||
		strings.Contains(missRec.Body.String(), "<html") {
		t.Fatalf("missing /assets/* must not return HTML fallback: ct=%q body=%q", missRec.Header().Get("Content-Type"), missRec.Body.String())
	}

	// Non-hashed asset under /assets/ does not get immutable.
	plainReq := httptest.NewRequest(http.MethodGet, "/assets/plain.js", nil)
	plainRec := httptest.NewRecorder()
	h.ServeHTTP(plainRec, plainReq)
	if plainRec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unhashed asset Cache-Control=%q want no-cache", plainRec.Header().Get("Cache-Control"))
	}

	// Root "/" and SPA route return index.html with no-cache.
	for _, p := range []string{"/", "/agents/p1"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("SPA path %q status=%d want 200", p, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("SPA path %q Cache-Control=%q want no-cache", p, w.Header().Get("Cache-Control"))
		}
		if !strings.Contains(w.Body.String(), "Corgi console shell") {
			t.Fatalf("SPA path %q did not serve index.html", p)
		}
	}

	// Non-compressible asset (.png) has no gzip and no Vary.
	pngReq := httptest.NewRequest(http.MethodGet, "/pic.png", nil)
	pngReq.Header.Set("Accept-Encoding", "gzip")
	pngRec := httptest.NewRecorder()
	h.ServeHTTP(pngRec, pngReq)
	if pngRec.Code != http.StatusOK || pngRec.Header().Get("Content-Encoding") != "" || pngRec.Header().Get("Vary") != "" {
		t.Fatalf("png unexpected headers: code=%d ce=%q vary=%q", pngRec.Code, pngRec.Header().Get("Content-Encoding"), pngRec.Header().Get("Vary"))
	}
}

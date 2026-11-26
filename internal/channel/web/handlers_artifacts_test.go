package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func TestArtifactSnapshotPublicPreviewShellRenderAndSecurity(t *testing.T) {
	srv, master := newTestServer(t)
	// Mount non-nil StaticFS just like production Hub so GET / and /p/... route registration is always exercised.
	srv.StaticFS = fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><body>SPA-LOGIN</body></html>")},
	}
	srv.Engine.SetPublicURLs([]string{"https://taihu.example/corgi/"})
	h := srv.Routes()
	ctx := context.Background()

	// Create an HTML prototype + SVG file on disk, then delete them AFTER reply to prove immutable Hub snapshot!
	htmlPath := filepath.Join(srv.Home, "dashboard-prototype.html")
	htmlBody := []byte("<!doctype html><html><body><h1>Interactive</h1><script>document.title='ok'</script></body></html>")
	if err := os.WriteFile(htmlPath, htmlBody, 0o600); err != nil {
		t.Fatal(err)
	}
	svgPath := filepath.Join(srv.Home, "diagram.svg")
	svgBody := []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>")
	if err := os.WriteFile(svgPath, svgBody, 0o600); err != nil {
		t.Fatal(err)
	}

	wi, err := srv.Engine.SetWorkItem(ctx, "new", model.MainThread, "首页看板原型", "待评审", "", "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := srv.Engine.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: model.MainThread,
		Text:   "原型和架构图已就绪。",
		WorkID: wi.ID,
		Files:  []string{htmlPath, svgPath},
	})
	if err != nil {
		t.Fatalf("ReplyDetailed failed: %v", err)
	}
	if len(res.Entry.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts on Entry, got %+v", res.Entry.Artifacts)
	}
	htmlArt := res.Entry.Artifacts[0]
	svgArt := res.Entry.Artifacts[1]
	if htmlArt.Kind != "html" || svgArt.Kind != "text" {
		t.Fatalf("unexpected kinds: html=%q svg=%q", htmlArt.Kind, svgArt.Kind)
	}

	// Delete originals from disk right away — Hub snapshot must continue serving both Web & /p/!
	_ = os.Remove(htmlPath)
	_ = os.Remove(svgPath)

	// 1. Snapshot hydrates Entry.Artifacts with ShareToken and projects WorkItem.Snapshots immediately.
	snap := srv.Engine.Snapshot(ctx, model.MainThread)
	if len(snap.Entries) != 1 || len(snap.Entries[0].Artifacts) != 2 {
		t.Fatalf("snapshot entries missing artifacts: %+v", snap.Entries)
	}
	shareToken := snap.Entries[0].Artifacts[0].ShareToken
	svgToken := snap.Entries[0].Artifacts[1].ShareToken
	if shareToken == "" || !strings.HasPrefix(shareToken, htmlArt.ID+".") {
		t.Fatalf("expected hydrated share token for %s, got %q", htmlArt.ID, shareToken)
	}
	if len(snap.WorkItems) != 1 || len(snap.WorkItems[0].Snapshots) != 2 {
		t.Fatalf("expected WorkItem.Snapshots hydrated immediately after Hub commit, got %+v", snap.WorkItems)
	}

	// 2. Authenticated console routes work for master/browser, and reject machine token with 401.
	code, _ := srv.Auth.NewCode("machine")
	_, nodeTok, err := srv.Auth.Redeem(ctx, code, "machine", "mac", "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}
	nodeReq := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+htmlArt.ID+"/raw", nil)
	nodeReq.Header.Set("Authorization", "Bearer "+nodeTok)
	nodeRec := httptest.NewRecorder()
	h.ServeHTTP(nodeRec, nodeReq)
	if nodeRec.Code != http.StatusUnauthorized {
		t.Fatalf("machine token must get 401 on /api/artifacts/, got %d", nodeRec.Code)
	}

	rawAuthReq := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+htmlArt.ID+"/raw", nil)
	rawAuthReq.Header.Set("Authorization", "Bearer "+master)
	rawAuthRec := httptest.NewRecorder()
	h.ServeHTTP(rawAuthRec, rawAuthReq)
	if rawAuthRec.Code != http.StatusOK || !bytes.Equal(rawAuthRec.Body.Bytes(), htmlBody) {
		t.Fatalf("console artifact raw failed: code=%d body=%q", rawAuthRec.Code, rawAuthRec.Body.String())
	}
	if gotXFO := rawAuthRec.Header().Get("X-Frame-Options"); gotXFO != "SAMEORIGIN" {
		t.Fatalf("console artifact raw XFO want SAMEORIGIN, got %q", gotXFO)
	}
	csp := rawAuthRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "https:") {
		t.Fatalf("unexpected artifact HTML CSP: %q", csp)
	}

	metaReq := httptest.NewRequest(http.MethodGet, "/api/artifacts/"+htmlArt.ID, nil)
	metaReq.Header.Set("Authorization", "Bearer "+master)
	metaRec := httptest.NewRecorder()
	h.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusOK || !strings.Contains(metaRec.Body.String(), "Interactive") {
		t.Fatalf("console artifact meta JSON failed: code=%d body=%s", metaRec.Code, metaRec.Body.String())
	}

	// 3. Public /p/{token} redirects prefix-safely to {token}/ without auth.
	redReq := httptest.NewRequest(http.MethodGet, "/p/"+shareToken, nil)
	redRec := httptest.NewRecorder()
	h.ServeHTTP(redRec, redReq)
	if redRec.Code != http.StatusFound || redRec.Header().Get("Location") != shareToken+"/" {
		t.Fatalf("expected prefix-safe relative redirect, got code=%d loc=%q", redRec.Code, redRec.Header().Get("Location"))
	}

	// 4. Public /p/{token}/ returns trusted shell posting token to ../render (NO cookie required!).
	shellReq := httptest.NewRequest(http.MethodGet, "/p/"+shareToken+"/", nil)
	shellRec := httptest.NewRecorder()
	h.ServeHTTP(shellRec, shellReq)
	if shellRec.Code != http.StatusOK {
		t.Fatalf("public shell want 200, got %d", shellRec.Code)
	}
	if shellRec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("public shell XFO want DENY, got %q", shellRec.Header().Get("X-Frame-Options"))
	}
	if !strings.Contains(shellRec.Body.String(), `action="../render"`) || !strings.Contains(shellRec.Body.String(), `sandbox="allow-scripts"`) {
		t.Fatalf("public shell missing POST ../render sandboxed iframe:\n%s", shellRec.Body.String())
	}

	// 5. POST /p/render reads token ONLY from POST body and serves sandboxed HTML with SAMEORIGIN.
	form := url.Values{"token": {shareToken}}
	renderReq := httptest.NewRequest(http.MethodPost, "/p/render", strings.NewReader(form.Encode()))
	renderReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	renderRec := httptest.NewRecorder()
	h.ServeHTTP(renderRec, renderReq)
	if renderRec.Code != http.StatusOK || !bytes.Equal(renderRec.Body.Bytes(), htmlBody) {
		t.Fatalf("POST /p/render failed: code=%d body=%q", renderRec.Code, renderRec.Body.String())
	}
	if renderRec.Header().Get("X-Frame-Options") != "SAMEORIGIN" || renderRec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unexpected /p/render headers: xfo=%q ref=%q", renderRec.Header().Get("X-Frame-Options"), renderRec.Header().Get("Referrer-Policy"))
	}

	// Query-only token on POST /p/render must be rejected with 404.
	queryOnlyReq := httptest.NewRequest(http.MethodPost, "/p/render?token="+url.QueryEscape(shareToken), nil)
	queryOnlyRec := httptest.NewRecorder()
	h.ServeHTTP(queryOnlyRec, queryOnlyReq)
	if queryOnlyRec.Code != http.StatusNotFound {
		t.Fatalf("query-only POST /p/render want 404, got %d", queryOnlyRec.Code)
	}

	// 6. Public GET ./raw serves HTML & SVG strictly as text/plain (never executes active content!).
	for _, tok := range []string{shareToken, svgToken} {
		r := httptest.NewRequest(http.MethodGet, "/p/"+tok+"/raw", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("public /raw for %s want 200 text/plain, got %d ct=%q", tok, w.Code, w.Header().Get("Content-Type"))
		}
	}

	// 7. Public GET ./download returns exact bytes with attachment disposition.
	dlReq := httptest.NewRequest(http.MethodGet, "/p/"+shareToken+"/download", nil)
	dlRec := httptest.NewRecorder()
	h.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK || !bytes.Equal(dlRec.Body.Bytes(), htmlBody) || !strings.Contains(dlRec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("public download failed: code=%d cd=%q", dlRec.Code, dlRec.Header().Get("Content-Disposition"))
	}

	// 8. Invalid or revoked token returns 404 unavailable page (never SPA login), while GET / still serves SPA.
	for _, badPath := range []string{"/p/art_bogus.badmac/", "/p/", "/p/art_bogus.badmac/extra/nested"} {
		badReq := httptest.NewRequest(http.MethodGet, badPath, nil)
		badRec := httptest.NewRecorder()
		h.ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusNotFound || !strings.Contains(badRec.Body.String(), "已失效") || strings.Contains(badRec.Body.String(), "SPA-LOGIN") {
			t.Fatalf("bad path %s want 404 unavailable page (not SPA), got %d body=%s", badPath, badRec.Code, badRec.Body.String())
		}
	}
	spaReq := httptest.NewRequest(http.MethodGet, "/", nil)
	spaRec := httptest.NewRecorder()
	h.ServeHTTP(spaRec, spaReq)
	if spaRec.Code != http.StatusOK || !strings.Contains(spaRec.Body.String(), "SPA-LOGIN") {
		t.Fatalf("GET / want 200 SPA-LOGIN, got %d body=%s", spaRec.Code, spaRec.Body.String())
	}

	revReq := httptest.NewRequest(http.MethodPost, "/api/artifacts/"+htmlArt.ID+"/revoke", bytes.NewBufferString(`{}`))
	revReq.Header.Set("Authorization", "Bearer "+master)
	revRec := httptest.NewRecorder()
	h.ServeHTTP(revRec, revReq)
	if revRec.Code != http.StatusOK {
		t.Fatalf("revoke want 200, got %d", revRec.Code)
	}
	afterRevReq := httptest.NewRequest(http.MethodGet, "/p/"+shareToken+"/", nil)
	afterRevRec := httptest.NewRecorder()
	h.ServeHTTP(afterRevRec, afterRevReq)
	if afterRevRec.Code != http.StatusNotFound {
		t.Fatalf("revoked token want 404, got %d", afterRevRec.Code)
	}

	// 9. PublicURL settings validation & precedence (Settings.PublicURL -> flag --url).
	st := srv.Engine.Settings()
	st.PublicURL = "javascript:alert(1)"
	if err := srv.Engine.SaveConfig(ctx, &st, nil); err == nil {
		t.Fatal("expected javascript: public_url to be rejected")
	}
	st.PublicURL = "http://9.134.247.217:7788/"
	if err := srv.Engine.SaveConfig(ctx, &st, nil); err != nil {
		t.Fatalf("valid IP public_url rejected: %v", err)
	}
	if got := srv.Engine.EffectivePublicURL(); got != "http://9.134.247.217:7788" {
		t.Fatalf("EffectivePublicURL want http://9.134.247.217:7788, got %q", got)
	}
	st.PublicURL = ""
	if err := srv.Engine.SaveConfig(ctx, &st, nil); err != nil {
		t.Fatalf("clearing public_url failed: %v", err)
	}
	if got := srv.Engine.EffectivePublicURL(); got != "https://taihu.example/corgi" {
		t.Fatalf("cleared public_url should fall back to flag URL, got %q", got)
	}

	// 10. Cross-thread --work validation rejects mismatched thread.
	if _, err := srv.Engine.ReplyDetailed(ctx, core.ReplyRequest{
		Thread: "wecom:other-group",
		Text:   "跨线程挂接测试",
		WorkID: wi.ID,
	}); err == nil {
		t.Fatal("expected cross-thread --work to be rejected")
	}

	// 11. Settings API exposes effective_public_url & flag_public_url.
	setReq := httptest.NewRequest(http.MethodGet, "/api/super/settings", nil)
	setReq.Header.Set("Authorization", "Bearer "+master)
	setRec := httptest.NewRecorder()
	h.ServeHTTP(setRec, setReq)
	var setOut map[string]any
	if err := json.Unmarshal(setRec.Body.Bytes(), &setOut); err != nil {
		t.Fatal(err)
	}
	if setOut["effective_public_url"] != "https://taihu.example/corgi" || setOut["flag_public_url"] != "https://taihu.example/corgi" {
		t.Fatalf("unexpected settings public url fields: %+v", setOut)
	}
}

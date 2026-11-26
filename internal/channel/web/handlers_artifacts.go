package web

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	// Strict opaque-origin sandbox CSP for interactive self-contained HTML previews:
	// inline scripts/styles work immediately, while cookies, parent DOM, external scripts,
	// external network subresources, fetch/XHR, forms, popups, and top-navigation stay blocked.
	artifactHTMLSandboxCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline' data:; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; form-action 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'"

	// Trusted minimal public shell page CSP: permits only its tiny bootstrap script and same-origin POST to ../render.
	artifactShellCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; frame-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

	// Raw non-HTML public asset CSP (source text or raster image).
	artifactRawStaticCSP = "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; frame-ancestors 'none'"
)

func isFrameableArtifactRequest(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/p/render" {
		return true
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.HasPrefix(r.URL.Path, "/api/artifacts/") &&
		strings.HasSuffix(r.URL.Path, "/raw") {
		return true
	}
	return false
}

func setCommonArtifactSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
}

func writeSandboxedHTMLArtifact(w http.ResponseWriter, r *http.Request, data []byte) {
	setCommonArtifactSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", artifactHTMLSandboxCSP)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeArtifactDownload(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	setCommonArtifactSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(name))
	w.Header().Set("Content-Security-Policy", "sandbox")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writePublicUnavailable(w http.ResponseWriter, r *http.Request, frameable bool) {
	setCommonArtifactSecurityHeaders(w)
	if frameable {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	} else {
		w.Header().Set("X-Frame-Options", "DENY")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'")
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>预览不可用 · Corgi</title>
<style>
  body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center; background: #f7f7f6; color: #111; font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", sans-serif; padding: 24px; }
  .box { max-width: 380px; text-align: center; background: #fff; border: 1px solid #ebebea; border-radius: 16px; padding: 28px 24px; }
  h1 { margin: 0 0 8px; font-size: 16px; font-weight: 600; }
  p { margin: 0; font-size: 13px; color: #666; line-height: 1.6; }
</style>
</head>
<body>
  <div class="box">
    <h1>预览链接已失效或已过期</h1>
    <p>该分享链接已过期、已撤销或不存在。如仍需查看，请在已登录的 Corgi 控制台或小本本中打开。</p>
  </div>
</body>
</html>`))
}

func formatArtifactBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024.0)
	}
	return fmt.Sprintf("%.2f MB", float64(n)/(1024.0*1024.0))
}

// ---------------------------------------------------------------- Authenticated Console Artifact Routes

func (s *Server) handleGetArtifactMeta(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeReadFileError(w, r, http.StatusNotFound, "找不到文件")
		return
	}
	art, data, err := s.Engine.ReadArtifactForConsole(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReadFileError(w, r, http.StatusNotFound, "该快照已过期或不可用")
		return
	}
	setCommonArtifactSecurityHeaders(w)
	kind := "text"
	content := ""
	if art.Kind == "binary" || art.Kind == "image" {
		kind = "binary"
	} else {
		truncated := false
		if len(data) > maxTextRead {
			data = data[:maxTextRead]
			truncated = true
		}
		content = string(data)
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":      kind,
			"path":      art.Name,
			"name":      art.Name,
			"size":      art.Size,
			"truncated": truncated,
			"content":   content,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":      kind,
		"path":      art.Name,
		"name":      art.Name,
		"size":      art.Size,
		"truncated": false,
		"content":   "",
	})
}

func (s *Server) handleGetArtifactRaw(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeReadFileError(w, r, http.StatusNotFound, "找不到文件")
		return
	}
	art, data, err := s.Engine.ReadArtifactForConsole(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReadFileError(w, r, http.StatusNotFound, "该快照已过期或不可用")
		return
	}
	switch art.Kind {
	case "html":
		writeSandboxedHTMLArtifact(w, r, data)
	case "image":
		setCommonArtifactSecurityHeaders(w)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Type", art.MIME)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case "markdown", "text":
		setCommonArtifactSecurityHeaders(w)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		writeArtifactDownload(w, r, art.Name, data)
	}
}

func (s *Server) handleGetArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeReadFileError(w, r, http.StatusNotFound, "找不到文件")
		return
	}
	art, data, err := s.Engine.ReadArtifactForConsole(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReadFileError(w, r, http.StatusNotFound, "该快照已过期或不可用")
		return
	}
	writeArtifactDownload(w, r, art.Name, data)
}

func (s *Server) handleRevokeArtifact(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unavailable"})
		return
	}
	if err := s.Engine.RevokeArtifact(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- Public Capability Preview Routes (/p/...)

func (s *Server) handlePublicArtifactRedirect(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	if s.Engine == nil || token == "" || strings.ContainsAny(token, "/\\") {
		writePublicUnavailable(w, r, false)
		return
	}
	if _, ok := s.Engine.VerifyShareToken(r.Context(), token); !ok {
		writePublicUnavailable(w, r, false)
		return
	}
	setCommonArtifactSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "DENY")
	// Prefix-preserving relative redirect so reverse proxies with path prefixes (e.g. /corgi/p/<token>) resolve to /corgi/p/<token>/.
	w.Header().Set("Location", url.PathEscape(token)+"/")
	w.WriteHeader(http.StatusFound)
}

func (s *Server) handlePublicArtifactShell(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writePublicUnavailable(w, r, false)
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	art, data, err := s.Engine.ResolvePublicShareToken(r.Context(), token)
	if err != nil {
		writePublicUnavailable(w, r, false)
		return
	}
	setCommonArtifactSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", artifactShellCSP)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(renderPublicArtifactShellHTML(art, token, data)))
}

func (s *Server) handlePublicArtifactRender(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writePublicUnavailable(w, r, true)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		writePublicUnavailable(w, r, true)
		return
	}
	// Strictly read from POST body only (never query string).
	token := strings.TrimSpace(r.PostFormValue("token"))
	art, data, err := s.Engine.ResolvePublicShareToken(r.Context(), token)
	if err != nil || art.Kind != "html" {
		writePublicUnavailable(w, r, true)
		return
	}
	writeSandboxedHTMLArtifact(w, r, data)
}

func (s *Server) handlePublicArtifactRaw(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writePublicUnavailable(w, r, false)
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	art, data, err := s.Engine.ResolvePublicShareToken(r.Context(), token)
	if err != nil {
		writePublicUnavailable(w, r, false)
		return
	}
	setCommonArtifactSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", artifactRawStaticCSP)
	switch art.Kind {
	case "image":
		w.Header().Set("Content-Type", art.MIME)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case "html", "markdown", "text":
		// Public GET ./raw never executes HTML/SVG — serves clean source text.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		writeArtifactDownload(w, r, art.Name, data)
	}
}

func (s *Server) handlePublicArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writePublicUnavailable(w, r, false)
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	art, data, err := s.Engine.ResolvePublicShareToken(r.Context(), token)
	if err != nil {
		writePublicUnavailable(w, r, false)
		return
	}
	writeArtifactDownload(w, r, art.Name, data)
}

func (s *Server) handlePublicArtifactNotFound(w http.ResponseWriter, r *http.Request) {
	writePublicUnavailable(w, r, false)
}

func renderPublicArtifactShellHTML(art *model.Artifact, token string, data []byte) string {
	escName := html.EscapeString(art.Name)
	escSize := html.EscapeString(formatArtifactBytes(art.Size))
	escToken := html.EscapeString(token)

	sourceLink := ""
	if art.Kind == "html" || art.Kind == "markdown" || art.Kind == "text" {
		sourceLink = `<a class="btn" href="./raw" target="_blank" rel="noopener noreferrer">源码</a>`
	}

	var mainBody string
	switch art.Kind {
	case "html":
		mainBody = fmt.Sprintf(`
  <form id="rf" method="POST" action="../render" target="corgi_preview_frame" style="display:none">
    <input type="hidden" name="token" value="%s" />
  </form>
  <iframe name="corgi_preview_frame" sandbox="allow-scripts" referrerpolicy="no-referrer" title="%s"></iframe>
  <script>document.getElementById("rf").submit();</script>`, escToken, escName)
	case "image":
		mainBody = fmt.Sprintf(`
  <div class="center">
    <img src="./raw" alt="%s" />
  </div>`, escName)
	case "markdown", "text":
		text := string(data)
		if len(text) > maxTextRead {
			text = text[:maxTextRead] + "\n\n... (内容已截断，请点击右上角下载原件)"
		}
		mainBody = fmt.Sprintf(`
  <div class="doc-wrap">
    <pre class="doc">%s</pre>
  </div>`, html.EscapeString(text))
	default:
		mainBody = fmt.Sprintf(`
  <div class="center">
    <div class="card">
      <div class="card-name">%s</div>
      <div class="card-meta">%s · 二进制文件</div>
      <a class="btn primary" href="./download">下载原件</a>
    </div>
  </div>`, escName, escSize)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<meta name="referrer" content="no-referrer" />
<title>%s · Corgi</title>
<style>
  * { box-sizing: border-box; }
  html, body { margin: 0; padding: 0; height: 100%%; background: #ffffff; color: #111111; font-family: -apple-system, BlinkMacSystemFont, "Inter", "PingFang SC", sans-serif; -webkit-font-smoothing: antialiased; }
  body { display: flex; flex-direction: column; overflow: hidden; }
  header { height: 44px; padding: 0 14px; border-bottom: 1px solid #ebebea; background: #f7f7f6; display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-shrink: 0; }
  .meta { display: flex; align-items: center; gap: 8px; min-width: 0; }
  .name { font-size: 13px; font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .size { font-size: 11px; color: #8e8e88; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; flex-shrink: 0; }
  .actions { display: flex; align-items: center; gap: 6px; flex-shrink: 0; }
  .btn { display: inline-flex; align-items: center; height: 28px; padding: 0 10px; border-radius: 7px; border: 1px solid #dcdcd8; background: #ffffff; color: #333330; font-size: 11.5px; text-decoration: none; cursor: pointer; }
  .btn:hover { background: #efefed; }
  .btn.primary { background: #111111; border-color: #111111; color: #ffffff; font-weight: 500; }
  .btn.primary:hover { opacity: 0.88; }
  iframe { flex: 1; width: 100%%; border: 0; background: #ffffff; min-height: 0; }
  .center { flex: 1; overflow: auto; display: flex; align-items: center; justify-content: center; padding: 24px; background: #f7f7f6; }
  .center img { max-width: 100%%; max-height: 100%%; border-radius: 10px; border: 1px solid #ebebea; background: #fff; }
  .doc-wrap { flex: 1; overflow: auto; padding: 20px; background: #ffffff; }
  .doc { max-width: 820px; margin: 0 auto; white-space: pre-wrap; word-break: break-word; font-size: 13px; line-height: 1.7; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; color: #222; }
  .card { background: #fff; border: 1px solid #ebebea; border-radius: 14px; padding: 24px 28px; text-align: center; }
  .card-name { font-size: 14px; font-weight: 600; margin-bottom: 6px; }
  .card-meta { font-size: 12px; color: #8e8e88; margin-bottom: 16px; }
</style>
</head>
<body>
  <header>
    <div class="meta">
      <span class="name" title="%s">%s</span>
      <span class="size">%s</span>
    </div>
    <div class="actions">
      %s
      <a class="btn primary" href="./download">下载原件</a>
    </div>
  </header>%s
</body>
</html>`, escName, escName, escName, escSize, sourceLink, mainBody)
}

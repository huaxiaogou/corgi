package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

const (
	maxTextRead    = 1 << 20
	maxHTMLRead    = 5 << 20
	maxImageRead   = 20 << 20
	maxUploadBytes = 25 << 20
	// Default HTML preview policy: static opaque-origin sandbox allowing passive HTTPS static assets
	// (styles/images/fonts) while forbidding all scripts, active network, forms, same-origin, and top-nav.
	htmlSandboxCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline' data: https:; img-src data: blob: https:; font-src data: https:; media-src data: blob:; connect-src 'none'; form-action 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'"
	// Opt-in inline-script HTML preview policy (user toggled): still opaque null origin,
	// allows inline scripts and passive HTTPS styles/images/fonts only; strictly forbids external scripts,
	// http:, eval, allow-same-origin, allow-top-navigation, connect-src, forms, popups, modals, and downloads.
	htmlSandboxScriptsCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline' data: https:; img-src data: blob: https:; font-src data: https:; media-src data: blob:; connect-src 'none'; form-action 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'"
)

func isHTMLPreviewRequest(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.URL.Path == "/api/file" &&
		r.URL.Query().Get("as") == "html" &&
		r.URL.Query().Get("download") != "1"
}

func htmlPreviewCSP(r *http.Request) string {
	if r.URL.Query().Get("scripts") == "1" {
		return htmlSandboxScriptsCSP
	}
	return htmlSandboxCSP
}

func applyForwardedHTMLPreviewHeaders(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	resp.Header.Del("X-Frame-Options")
	if resp.StatusCode == http.StatusOK {
		resp.Header.Set("Content-Security-Policy", htmlPreviewCSP(r))
		resp.Header.Set("Cache-Control", "no-store")
		return
	}
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeReadFileError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("X-Frame-Options", "DENY")
	if r.Method == http.MethodHead {
		w.WriteHeader(code)
		return
	}
	writeJSON(w, code, map[string]any{"error": msg})
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	refresh := r.URL.Query().Get("refresh") != ""
	cat, err := s.Agents.Models(r.Context(), s.Home, kind, refresh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	list, err := s.Store.ListEndedSessions(r.Context(), store.EndedSessionRetentionLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) handleResumeHistory(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "history unavailable"})
		return
	}
	id := r.PathValue("id")
	target, err := s.Store.GetEndedSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "历史会话不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ad, err := s.Agents.Get(target.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if s.Engine != nil {
		wk, _, lErr := s.Engine.ResumeWorkerSession(r.Context(), core.ResumeWorkerRequest{
			EndedSessionID: id,
		})
		if lErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": lErr.Error()})
			return
		}
		s.markStarted(wk.PaneID)
		writeJSON(w, http.StatusOK, map[string]any{"pane_id": wk.PaneID, "workspace_id": wk.WorkspaceID, "name": wk.Name, "worker": wk.Name, "work_item_id": wk.WorkItemID})
		return
	}

	askPerm := false
	if s.Engine != nil {
		askPerm = s.Engine.Settings().AskPermissions
	}
	if !askPerm {
		if target.Machine == "" {
			_ = ad.PreTrustWorkspace(s.Home, target.Cwd)
		} else if s.Fleet != nil {
			if hc, err := s.Fleet.HTTPClient(target.Machine); err == nil {
				tb, _ := json.Marshal(map[string]string{"kind": ad.Kind(), "dir": target.Cwd})
				if req, rErr := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://node/api/trust", bytes.NewReader(tb)); rErr == nil {
					req.Header.Set("Content-Type", "application/json")
					if resp, dErr := hc.Do(req); dErr == nil {
						_ = resp.Body.Close()
					}
				}
			}
		}
	}
	// Note: Codex `resume <id>` is a subcommand and must precede launch flags.
	args := append(ad.ResumeArgs(target.Session), ad.LaunchArgs("", "", !askPerm)...)
	name := target.Name
	if name == "" || name == model.RouterName {
		name = ad.Kind() + "-" + fleet.RandomToken()[:4]
	}
	wsID, paneID, err := s.Fleet.CreateWorkspace(r.Context(), target.Machine, target.Cwd, target.Title)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if err := s.Fleet.StartAgent(r.Context(), paneID, ad.Kind(), name, args); err != nil {
		var he *herdr.Error
		if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
	}
	s.Fleet.RememberName(paneID, name)
	s.markStarted(paneID)
	_ = s.Store.ForgetEndedSession(r.Context(), target.Kind, target.Machine, target.Session)
	writeJSON(w, http.StatusOK, map[string]any{"pane_id": paneID, "workspace_id": wsID, "name": name})
}

func (s *Server) handleListDirs(w http.ResponseWriter, r *http.Request) {
	dir := s.expand(r.URL.Query().Get("path"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	dirs := []string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
		if len(dirs) >= 300 {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": dir, "display": s.tilde(dir), "parent": filepath.Dir(dir), "dirs": dirs,
	})
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	as := r.URL.Query().Get("as")
	download := r.URL.Query().Get("download") == "1"
	p := s.expand(rawPath)
	if s.Store != nil && core.IsProtectedHubDBPath(s.Store.DBPath(), p) {
		writeReadFileError(w, r, http.StatusForbidden, "受保护的系统数据库文件不可读取")
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeReadFileError(w, r, http.StatusNotFound, "找不到文件")
		return
	}
	if fi.IsDir() {
		writeReadFileError(w, r, http.StatusBadRequest, "这是一个目录")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeReadFileError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	defer f.Close()

	if download {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(filepath.Base(p)))
		w.Header().Set("Content-Security-Policy", "sandbox")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(w, io.LimitReader(f, 100<<20))
		return
	}
	if as == "html" {
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".html" && ext != ".htm" {
			writeReadFileError(w, r, http.StatusBadRequest, "仅支持 .html/.htm 预览")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", htmlPreviewCSP(r))
		w.Header().Set("Cache-Control", "no-store")
		if fi.Size() > maxHTMLRead {
			w.Header().Set("X-Corgi-Truncated", "1")
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxHTMLRead))
		_, _ = w.Write(b)
		return
	}
	limit := int64(maxTextRead)
	if as == "image" {
		limit = maxImageRead
	}
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	if as == "image" {
		w.Header().Set("Content-Type", http.DetectContentType(b))
		w.Header().Set("Cache-Control", "private, max-age=3600")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(b)
		return
	}
	kind := "text"
	if bytes.IndexByte(b[:min(len(b), 4096)], 0) >= 0 {
		kind, b = "binary", nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":      kind,
		"path":      s.tilde(p),
		"name":      filepath.Base(p),
		"size":      fi.Size(),
		"truncated": fi.Size() > limit,
		"content":   string(b),
	})
}

func isUploadTooLargeErr(err error) bool {
	if errors.Is(err, core.ErrUploadTooLarge) {
		return true
	}
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := core.SanitizeUploadName(r.URL.Query().Get("name"))
	day := time.Now().Format("20060102")
	body := http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if r.URL.Query().Get("scope") == "staged" {
		shaHex := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sha256")))
		if !core.IsValidSHA256Hex(shaHex) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效的 sha256 参数"})
			return
		}
		dest, actualSHA, n, deduped, err := core.StageLocalFileFromReader(s.CorgiDir, day, shaHex, name, body, maxUploadBytes)
		if err != nil {
			code := http.StatusBadRequest
			if isUploadTooLargeErr(err) {
				code = http.StatusRequestEntityTooLarge
			}
			writeJSON(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": dest, "name": name, "size": n, "sha256": actualSHA, "deduped": deduped})
		return
	}
	dest, n, err := core.WriteRegularUploadAtomic(s.CorgiDir, day, fleet.RandomToken()[:6], name, body, maxUploadBytes)
	if err != nil {
		code := http.StatusInternalServerError
		if isUploadTooLargeErr(err) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]any{"error": "上传失败或超过 25 MB 限制"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dest, "name": name, "size": n})
}

// handleReadAttachment serves a strictly validated local file (5 MiB max, safe paths only)
// for Hub-to-Node cross-machine `corgi reply --machine M --file PATH` attachment delivery.
func (s *Server) handleReadAttachment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if s.Store != nil && core.IsProtectedHubDBPath(s.Store.DBPath(), body.Path) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "受保护的系统数据库文件不可读取"})
		return
	}
	name, data, cleanPath, err := core.ReadAttachmentFile(body.Path)
	if err != nil {
		code := http.StatusBadRequest
		if os.IsNotExist(err) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Corgi-Filename", url.PathEscape(name))
	w.Header().Set("X-Corgi-Clean-Path", url.PathEscape(cleanPath))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) expand(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == "~":
		return s.Home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(s.Home, p[2:])
	case !strings.HasPrefix(p, "/"):
		return filepath.Join(s.Home, p)
	default:
		return path.Clean(p)
	}
}

func (s *Server) tilde(p string) string {
	if p == s.Home {
		return "~"
	}
	if strings.HasPrefix(p, s.Home+"/") {
		return "~" + p[len(s.Home):]
	}
	return p
}

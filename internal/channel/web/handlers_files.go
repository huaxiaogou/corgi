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
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

const (
	maxTextRead    = 1 << 20
	maxHTMLRead    = 5 << 20
	maxImageRead   = 20 << 20
	maxUploadBytes = 25 << 20
	htmlSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"
)

var unsafeUploadRe = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

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
	list, err := s.Store.ListEndedSessions(r.Context(), 40)
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
	list, err := s.Store.ListEndedSessions(r.Context(), 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var target *model.EndedSession
	for _, sess := range list {
		if sess.ID == id {
			target = sess
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "历史会话不存在"})
		return
	}
	ad, err := s.Agents.Get(target.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
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
	fi, err := os.Stat(p)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "找不到文件"})
		return
	}
	if fi.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "这是一个目录"})
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()

	if download {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(filepath.Base(p)))
		w.Header().Set("Content-Security-Policy", "sandbox")
		_, _ = io.Copy(w, io.LimitReader(f, 100<<20))
		return
	}
	if as == "html" {
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".html" && ext != ".htm" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "仅支持 .html/.htm 预览"})
			return
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxHTMLRead))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", htmlSandboxCSP)
		w.Header().Set("Cache-Control", "no-store")
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

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := sanitizeFilename(r.URL.Query().Get("name"))
	dir := filepath.Join(s.CorgiDir, "uploads", time.Now().Format("20060102"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	dest := filepath.Join(dir, fleet.RandomToken()[:6]+"-"+name)
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUploadBytes))
	_ = f.Close()
	if err != nil {
		_ = os.Remove(dest)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "上传失败或超过 25 MB 限制"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dest, "name": name, "size": n})
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	ext := unsafeUploadRe.ReplaceAllString(filepath.Ext(name), "")
	stem := strings.Trim(unsafeUploadRe.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), "-"), "-.")
	if stem == "" {
		stem = "file"
	}
	return stem + ext
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

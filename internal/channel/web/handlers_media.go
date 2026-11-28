package web

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
)

func (s *Server) handleInspectPath(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	dbPath := ""
	if s.Store != nil {
		dbPath = s.Store.DBPath()
	}
	res, err := core.InspectLocalProjectDir(s.Home, s.CorgiDir, dbPath, body.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleHeadUpload supports zero-byte cross-machine staged upload deduplication.
// Older Nodes without HEAD /api/uploads return 405, causing Hub to fall back seamlessly to POST.
func (s *Server) handleHeadUpload(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("scope") != "staged" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	shaHex := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sha256")))
	if !core.IsValidSHA256Hex(shaHex) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	name := core.SanitizeUploadName(r.URL.Query().Get("name"))
	day := time.Now().Format("20060102")
	destPath, _, ok := core.LookupStagedFile(s.CorgiDir, day, shaHex, name)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("X-Corgi-Staged-Path", destPath)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// handleReadUpload streams a strictly os.Root-confined regular file from <corgiDir>/uploads/
// up to 25 MiB without buffering the full file in memory (for Node -> Hub / Node A -> Node B staging).
func (s *Server) handleReadUpload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	dbPath := ""
	if s.Store != nil {
		dbPath = s.Store.DBPath()
	}
	f, name, _, err := core.OpenUploadsFile(s.CorgiDir, dbPath, body.Path, core.MaxUploadStageBytes)
	if err != nil {
		code := http.StatusBadRequest
		if os.IsNotExist(err) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Corgi-Filename", url.PathEscape(name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(f, core.MaxUploadStageBytes))
}

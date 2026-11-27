package web

import (
	"net/http"
	"strings"

	"github.com/kiperjing/corgi/internal/gitwt"
)

func (s *Server) handleGitInspect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	dir := s.expand(body.Cwd)
	res := gitwt.Inspect(r.Context(), dir)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepoRoot string `json:"repo_root"`
		Name     string `json:"name"`
		Branch   string `json:"branch"`
		RelPath  string `json:"rel_path"`
	}
	if !decodeJSON(w, r, &body) || strings.TrimSpace(body.RepoRoot) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "repo_root is required"})
		return
	}
	res, err := gitwt.Create(r.Context(), s.Home, s.expand(body.RepoRoot), body.Name, body.Branch, body.RelPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitWorktreeCleanup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepoRoot     string `json:"repo_root"`
		WorktreePath string `json:"worktree_path"`
		Branch       string `json:"branch"`
		BaseCommit   string `json:"base_commit"`
	}
	if !decodeJSON(w, r, &body) || strings.TrimSpace(body.WorktreePath) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "worktree_path is required"})
		return
	}
	res, err := gitwt.Cleanup(r.Context(), s.expand(body.RepoRoot), s.expand(body.WorktreePath), body.Branch, body.BaseCommit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitVerify(w http.ResponseWriter, r *http.Request) {
	var body gitwt.VerifyRequest
	if !decodeJSON(w, r, &body) || strings.TrimSpace(body.Dir) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dir is required"})
		return
	}
	body.Dir = s.expand(body.Dir)
	res, err := gitwt.Verify(r.Context(), s.Home, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

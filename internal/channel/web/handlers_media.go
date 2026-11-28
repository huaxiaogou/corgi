package web

import (
	"net/http"

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

package web

import (
	"net/http"
	"strings"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func (s *Server) handleGetRouterContext(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "engine unavailable"})
		return
	}
	thread := strings.TrimSpace(r.URL.Query().Get("thread"))
	if thread == "" {
		thread = model.MainThread
	}
	info, err := s.Engine.InspectRouterContext(r.Context(), thread)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handlePruneRouterContext(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "engine unavailable"})
		return
	}
	var req core.PruneRouterContextRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Thread) == "" {
		req.Thread = model.MainThread
	}
	res, err := s.Engine.PruneRouterContext(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

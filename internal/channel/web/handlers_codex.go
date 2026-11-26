package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/runtime/codex"
)

func (s *Server) handleCodexInterrupt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thread string `json:"thread"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	thread := strings.TrimSpace(body.Thread)
	if thread == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := codex.InterruptTurn(ctx, s.Home, thread); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

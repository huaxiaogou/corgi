package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/updater"
)

// FleetUpdateView re-exports updater.FleetUpdateView for package callers.
type FleetUpdateView = updater.FleetUpdateView

func (s *Server) getUpdater() *updater.Manager {
	s.updaterOnce.Do(func() {
		cfg := updater.Config{
			Version:              func() string { return s.Version },
			ServerCtx:            func() context.Context { return s.ServerCtx },
			ReleaseRepoURL:       func() string { return s.ReleaseRepoURL },
			ExecutablePath:       func() string { return s.ExecutablePath },
			ExecFn:               func() func(string) { return s.ExecFn },
			RetryBaseDelay:       func() time.Duration { return s.UpdateRetryBaseDelay },
			NodeReconnectTimeout: func() time.Duration { return s.NodeReconnectTimeout },
			Logger:               s.Logger,
		}
		if s.Fleet != nil {
			cfg.Fleet = s.Fleet
		}
		if s.Auth != nil {
			cfg.Devices = s.Auth
		}
		if s.Store != nil {
			cfg.KV = s.Store
		}
		if s.Engine != nil {
			cfg.Runtime = s.Engine
		}
		if s.Bus != nil {
			bus := s.Bus
			cfg.OnStateChanged = bus.OnStateChanged
			cfg.FlushBeforeRestart = bus.FlushBeforeRestart
		}
		s.updater = updater.NewManager(cfg)
	})
	return s.updater
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	soon := r.URL.Query().Get("soon") != ""
	writeJSON(w, http.StatusOK, s.getUpdater().VersionSnapshot(r.Context(), soon))
}

// handleHubUpdate handles authenticated POST /api/update on the Hub:
//   - ?all=1 launches the Hub-owned sequential fleet upgrade (delegating to handleFleetUpdate).
//   - ?machine=<exact-id> upgrades one remote Node (direct first, Hub-relayed fallback on network error).
//   - empty machine upgrades the Hub itself synchronously.
func (s *Server) handleHubUpdate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		s.handleFleetUpdate(w, r)
		return
	}
	machineID := strings.TrimSpace(r.URL.Query().Get("machine"))
	if machineID == "" {
		s.handleHubSelfUpdate(w, r)
		return
	}
	res, status, err := s.getUpdater().UpdateSingleRemote(machineID)
	if err != nil {
		if status <= 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleHubSelfUpdate handles synchronous Hub self-upgrade on POST /api/update (no machine param).
func (s *Server) handleHubSelfUpdate(w http.ResponseWriter, _ *http.Request) {
	writeLocalUpdateResponse(w, s.getUpdater().LocalSelfUpdate(false))
}

// handleNodeSelfUpdate serves POST /api/update strictly inside a Node's yamux tunnel.
func (s *Server) handleNodeSelfUpdate(w http.ResponseWriter, _ *http.Request) {
	writeLocalUpdateResponse(w, s.getUpdater().LocalSelfUpdate(true))
}

// handleNodeRelayUpdate serves POST /api/update/relay strictly inside a Node's yamux tunnel.
func (s *Server) handleNodeRelayUpdate(w http.ResponseWriter, r *http.Request) {
	tag := r.Header.Get(updater.RelayUpdateTagHeader)
	wantSHA := r.Header.Get(updater.RelayUpdateSHAHeader)
	writeLocalUpdateResponse(w, s.getUpdater().ApplyRelayedBinary(r.Body, r.ContentLength, tag, wantSHA))
}

func writeLocalUpdateResponse(w http.ResponseWriter, res updater.LocalUpdateResponse) {
	writeJSON(w, res.Status, res.Payload)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if res.AfterFlush != nil {
		res.AfterFlush()
	}
}

// handleFleetUpdate starts (or returns status of) the Hub-owned sequential fleet upgrade.
func (s *Server) handleFleetUpdate(w http.ResponseWriter, r *http.Request) {
	view, status, err := s.getUpdater().StartFleetUpdate(r.Context(), "fleet", nil)
	if err != nil {
		if status <= 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "update": view})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "update": view})
}

// CheckUpdate implements core.FleetUpdater via internal/updater.
func (s *Server) CheckUpdate(ctx context.Context) (core.UpdateCheckResult, error) {
	return s.getUpdater().CheckUpdate(ctx)
}

// TriggerFleetUpdate implements core.FleetUpdater via internal/updater.
func (s *Server) TriggerFleetUpdate(ctx context.Context, expectedTarget string) (core.UpdateTriggerResult, error) {
	return s.getUpdater().TriggerFleetUpdate(ctx, expectedTarget)
}

// TryAutoUpdate implements core.AutoUpdater via internal/updater.
func (s *Server) TryAutoUpdate(ctx context.Context) {
	s.getUpdater().TryAutoUpdate(ctx)
}

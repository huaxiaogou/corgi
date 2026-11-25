package web

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

var sessionUUIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

type machineTitleCache struct {
	at      time.Time
	loading bool
	data    map[string]model.PaneSession
}

func (s *Server) handleTrustFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		Dir  string `json:"dir"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ad, err := s.Agents.Get(body.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := ad.PreTrustWorkspace(s.Home, s.expand(body.Dir)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTitles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.localPaneSessions(ctx)})
}

func (s *Server) localPaneSessions(ctx context.Context) map[string]model.PaneSession {
	out := map[string]model.PaneSession{}
	if s.Fleet == nil || s.Agents == nil {
		return out
	}
	panes, err := s.Fleet.Panes(ctx)
	if err != nil {
		return out
	}
	agents, _ := s.Fleet.Agents(ctx)
	agentByPane := map[string]model.Agent{}
	for _, a := range agents {
		agentByPane[a.PaneID] = a
	}
	for _, p := range panes {
		localID, machine := fleet.SplitID(p.ID)
		if machine != "" {
			continue
		}
		rawKind := deref(p.Agent)
		if rawKind == "" {
			if ag, ok := agentByPane[p.ID]; ok && ag.Agent != nil {
				rawKind = *ag.Agent
			}
		}
		if rawKind == "" {
			continue
		}
		kind := agent.NormalizeKind(rawKind)
		ad, err := s.Agents.Get(kind)
		if err != nil {
			continue
		}
		sess := ""
		if ag, ok := agentByPane[p.ID]; ok && ag.Session != nil {
			sess = extractSessionToken(ag.Session.Kind, ag.Session.Value)
		}
		cwd := deref(p.ForegroundCwd)
		if cwd == "" {
			cwd = deref(p.Cwd)
		}
		if sess == "" {
			if trPath := ad.LocateTranscript(s.Home, cwd, "", s.startedAt(p.ID)); trPath != "" {
				sess = extractSessionToken("path", trPath)
			}
		}
		var run *[2]string
		if r, ok := s.Fleet.Run(p.ID); ok {
			run = r
		}
		ps := model.PaneSession{
			Title:   ad.LookupTitle(s.Home, sess, deref(p.Title)),
			Session: sess,
			Run:     run,
		}
		if ps.Title != "" || ps.Session != "" || ps.Run != nil {
			out[localID] = ps
		}
	}
	return out
}

func extractSessionToken(kind, val string) string {
	if kind != "path" && !strings.Contains(val, "/") {
		return val
	}
	if ids := sessionUUIDRe.FindAllString(val, -1); len(ids) > 0 {
		return ids[len(ids)-1]
	}
	base := strings.TrimSuffix(filepath.Base(val), ".jsonl")
	if base != "" && base != "transcript" && base != "transcript_full" {
		return base
	}
	return ""
}

func (s *Server) titlesForMachine(ctx context.Context, machine string) map[string]model.PaneSession {
	s.titleMu.Lock()
	if s.titles == nil {
		s.titles = map[string]*machineTitleCache{}
	}
	entry := s.titles[machine]
	if entry == nil {
		entry = &machineTitleCache{}
		s.titles[machine] = entry
	}
	stale := time.Since(entry.at) > 4*time.Second && !entry.loading
	if stale {
		entry.loading = true
	}
	cur := entry.data
	s.titleMu.Unlock()

	if !stale {
		return cur
	}
	refresh := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var fresh map[string]model.PaneSession
		if machine == "" {
			fresh = s.localPaneSessions(rctx)
		} else if s.Fleet != nil {
			if hc, err := s.Fleet.HTTPClient(machine); err == nil {
				req, _ := http.NewRequestWithContext(rctx, http.MethodGet, "http://node/api/titles", nil)
				if resp, dErr := hc.Do(req); dErr == nil {
					var body struct {
						Sessions map[string]model.PaneSession `json:"sessions"`
					}
					if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Sessions != nil {
						fresh = body.Sessions
					}
					_ = resp.Body.Close()
				}
			}
		}
		s.titleMu.Lock()
		entry.loading = false
		entry.at = time.Now()
		if fresh != nil {
			entry.data = fresh
		}
		s.titleMu.Unlock()
	}

	if cur == nil {
		refresh()
		s.titleMu.Lock()
		cur = entry.data
		s.titleMu.Unlock()
		return cur
	}
	go refresh()
	return cur
}

// WatchEndings observes live agent sessions across all online machines and archives
// sessions into SQLite history once their pane exits cleanly (2-tick debounce).
func (s *Server) WatchEndings(ctx context.Context) {
	if s.Store == nil || s.Fleet == nil {
		return
	}
	type tracked struct {
		sess   model.EndedSession
		missed int
	}
	prev := map[string]*tracked{}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		st := s.buildPaneState(pctx)
		cancel()
		if !st.OK {
			continue
		}
		online := map[string]bool{"": true}
		for _, m := range st.Machines {
			if m.Online {
				online[m.ID] = true
			}
		}
		nowLive := map[string]bool{}
		for _, a := range st.Agents {
			nowLive[a.PaneID] = true
			if a.Name == model.RouterName || a.WorkspaceLabel == model.RouterLabel ||
				a.Name == model.DreamName || a.WorkspaceLabel == model.DreamLabel || a.Session == "" {
				delete(prev, a.PaneID)
				continue
			}
			kind := agent.NormalizeKind(a.Kind)
			prev[a.PaneID] = &tracked{
				sess: model.EndedSession{
					Kind:    kind,
					Machine: a.Machine,
					Cwd:     a.Cwd,
					Title:   a.Topic,
					Name:    a.Name,
					Session: a.Session,
				},
			}
			_ = s.Store.ForgetEndedSession(ctx, kind, a.Machine, a.Session)
		}
		for paneID, tr := range prev {
			if nowLive[paneID] || !online[tr.sess.Machine] {
				continue
			}
			tr.missed++
			if tr.missed < 2 {
				continue
			}
			delete(prev, paneID)
			id, _, err := s.Store.NextID(ctx, "h")
			if err != nil {
				continue
			}
			tr.sess.ID = id
			tr.sess.EndedAt = time.Now()
			if tr.sess.Title == "" {
				tr.sess.Title = tr.sess.Name
			}
			_ = s.Store.SaveEndedSession(ctx, &tr.sess, 40)
		}
	}
}

func (s *Server) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "history unavailable"})
		return
	}
	_ = s.Store.DeleteEndedSession(r.Context(), r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGetTranscript(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := agent.NormalizeKind(q.Get("kind"))
	session := strings.TrimSpace(q.Get("session"))
	if kind == "" || session == "" || strings.ContainsAny(session, `/\*?[`) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid kind or session"})
		return
	}
	ad, err := s.Agents.Get(kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	trPath := ad.LocateTranscript(s.Home, s.expand(q.Get("cwd")), session, time.Time{})
	if trPath == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "这台电脑上找不到该会话的记录文件"})
		return
	}
	imgDir := filepath.Join(s.CorgiDir, "images")
	if before, _ := strconv.ParseInt(q.Get("before"), 10, 64); before > 0 {
		msgs, cur := agent.EarlierMessages(trPath, ad, imgDir, before)
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "earlier": cur})
		return
	}
	tr := agent.NewTranscriptReader(trPath, ad, imgDir)
	tr.OpenTail()
	writeJSON(w, http.StatusOK, map[string]any{"messages": tr.Messages(), "earlier": tr.EarlierCursor()})
}

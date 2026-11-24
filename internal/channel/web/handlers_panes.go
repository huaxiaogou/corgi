package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

var agentNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (s *Server) buildPaneState(ctx context.Context) model.PaneState {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	machines := s.Fleet.Machines()
	ws, err := s.Fleet.Workspaces(ctx)
	if err != nil {
		return model.PaneState{Error: err.Error(), Agents: []model.AgentView{}, Shells: []model.AgentView{}, Machines: machines}
	}
	panes, err := s.Fleet.Panes(ctx)
	if err != nil {
		return model.PaneState{Error: err.Error(), Agents: []model.AgentView{}, Shells: []model.AgentView{}, Machines: machines}
	}
	agents, _ := s.Fleet.Agents(ctx)

	labels := map[string]string{}
	order := map[string]int{}
	for _, w := range ws {
		labels[w.ID] = w.Label
		order[w.ID] = w.Number
	}
	agentMap := map[string]model.Agent{}
	for _, a := range agents {
		agentMap[a.PaneID] = a
	}

	st := model.PaneState{OK: true, Agents: []model.AgentView{}, Shells: []model.AgentView{}, Machines: machines}
	for _, p := range panes {
		cwd := deref(p.ForegroundCwd)
		if cwd == "" {
			cwd = deref(p.Cwd)
		}
		_, machine := fleet.SplitID(p.ID)
		v := model.AgentView{
			PaneID:         p.ID,
			WorkspaceID:    p.WorkspaceID,
			WorkspaceLabel: labels[p.WorkspaceID],
			Kind:           deref(p.Agent),
			Status:         p.AgentStatus,
			Cwd:            cwd,
			Title:          deref(p.Title),
			IsAgent:        p.Agent != nil,
			Machine:        machine,
		}
		if ag, ok := agentMap[p.ID]; ok {
			v.Name = deref(ag.Name)
			if ag.Session != nil {
				v.Session = ag.Session.Value
			}
		}
		if v.Name == "" {
			v.Name = v.WorkspaceLabel
		}
		if v.IsAgent {
			if r, ok := s.Fleet.Run(p.ID); ok {
				v.Run = r
			}
			v.Status = s.Fleet.EffectiveStatus(p.ID, v.Status)
			if ad, err := s.Agents.Get(v.Kind); err == nil && machine == "" {
				v.Topic = ad.LookupTitle(s.Home, v.Session, v.Title)
			}
			st.Agents = append(st.Agents, v)
		} else {
			v.Status = "shell"
			st.Shells = append(st.Shells, v)
		}
	}
	sortViews := func(list []model.AgentView) {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Machine != list[j].Machine {
				return list[i].Machine < list[j].Machine
			}
			if order[list[i].WorkspaceID] != order[list[j].WorkspaceID] {
				return order[list[i].WorkspaceID] < order[list[j].WorkspaceID]
			}
			return list[i].PaneID < list[j].PaneID
		})
	}
	sortViews(st.Agents)
	sortViews(st.Shells)
	return st
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildPaneState(r.Context()))
}

func (s *Server) handleLaunchAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string   `json:"kind"` // claude | codex | agy | codebuddy | shell
		Name   string   `json:"name"`
		Cwd    string   `json:"cwd"`
		Label  string   `json:"label"`
		Model  string   `json:"model"`
		Effort string   `json:"effort"`
		Prompt string   `json:"prompt"`
		Args   []string `json:"args"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !agent.ModelNameRe.MatchString(body.Model) || !agent.ModelNameRe.MatchString(body.Effort) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid model or effort"})
		return
	}
	cwd := s.expand(body.Cwd)
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "目录不存在: " + cwd})
		return
	}
	if body.Kind == "shell" {
		label := body.Label
		if label == "" {
			label = filepath.Base(cwd)
		}
		wsID, paneID, err := s.Fleet.CreateWorkspace(r.Context(), "", cwd, label)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pane_id": paneID, "workspace_id": wsID})
		return
	}

	ad, err := s.Agents.Get(body.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.Agents.CheckInstalled(s.Home, ad.Kind()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = ad.Kind() + "-" + strconv.FormatInt(time.Now().Unix()%10000, 10)
	}
	if !agentNameRe.MatchString(name) || name == model.RouterName {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名称需以小写字母开头，仅含字母/数字/-/_"})
		return
	}
	askPerm := false
	if s.Engine != nil {
		askPerm = s.Engine.Settings().AskPermissions
	}
	if !askPerm {
		_ = ad.PreTrustWorkspace(s.Home, cwd)
	}
	args := append(ad.LaunchArgs(body.Model, body.Effort, !askPerm), body.Args...)
	label := body.Label
	if label == "" {
		label = name
	}
	wsID, paneID, err := s.Fleet.CreateWorkspace(r.Context(), "", cwd, label)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if err := s.Fleet.StartAgent(r.Context(), paneID, ad.Kind(), name, args); err != nil {
		var he *herdr.Error
		if !(errors.As(err, &he) && he.Code == "agent_not_ready") {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "pane_id": paneID, "workspace_id": wsID})
			return
		}
	}
	s.markStarted(paneID)
	s.Fleet.RememberName(paneID, name)
	s.Fleet.RememberRun(paneID, body.Model, body.Effort)
	if p := strings.TrimSpace(body.Prompt); p != "" {
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for i := 0; i < 10; i++ {
				if s.Fleet.Prompt(bg, paneID, p) == nil {
					return
				}
				time.Sleep(1500 * time.Millisecond)
			}
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"pane_id": paneID, "workspace_id": wsID, "name": name})
}

func (s *Server) handlePromptAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if !decodeJSON(w, r, &body) || strings.TrimSpace(body.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required"})
		return
	}
	if err := s.Fleet.Prompt(r.Context(), r.PathValue("target"), body.Prompt); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSendText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Enter bool   `json:"enter"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.Fleet.SendText(r.Context(), r.PathValue("id"), body.Text, body.Enter); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSendKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []string `json:"keys"`
	}
	if !decodeJSON(w, r, &body) || len(body.Keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "keys required"})
		return
	}
	if err := s.Fleet.SendKeys(r.Context(), r.PathValue("id"), body.Keys...); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCloseWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.Fleet.CloseWorkspace(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSeenPane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pane string `json:"pane"`
	}
	if decodeJSON(w, r, &body) && body.Pane != "" {
		s.Fleet.MarkSeen(body.Pane)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStreamPane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mode := r.URL.Query().Get("mode")
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	interval := 250 * time.Millisecond
	if mode == "log" {
		interval = 700 * time.Millisecond
	}
	var lastText string
	var lastSize model.Rect
	var sizeAt time.Time
	beat := time.Now()
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		var (
			rr  model.ReadResult
			err error
		)
		if mode == "log" {
			rr, err = s.Fleet.Read(ctx, id, "recent_unwrapped", "text", 500)
		} else {
			if time.Since(sizeAt) > 2*time.Second {
				if sz, e := s.Fleet.PaneSize(ctx, id); e == nil {
					lastSize = sz
				}
				sizeAt = time.Now()
			}
			rr, err = s.Fleet.Read(ctx, id, "recent", "ansi", 400)
		}
		cancel()
		if err != nil {
			var he *herdr.Error
			if errors.As(err, &he) && strings.Contains(he.Code, "not_found") {
				b, _ := json.Marshal(map[string]any{"error": he.Message})
				_ = sse.send("gone", b)
				return
			}
		} else if rr.Text != lastText {
			lastText = rr.Text
			b, _ := json.Marshal(map[string]any{
				"text": rr.Text, "revision": rr.Revision, "cols": lastSize.Width, "rows": lastSize.Height,
			})
			if sse.send("frame", b) != nil {
				return
			}
			beat = time.Now()
		} else if time.Since(beat) > 15*time.Second {
			sse.comment()
			beat = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	var (
		reader   *agent.TranscriptReader
		lastLive string
		lastLog  string
		beat     = time.Now()
	)
	imgDir := filepath.Join(s.CorgiDir, "images")
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		agents, _ := s.Fleet.Agents(ctx)
		var cur *model.Agent
		for i := range agents {
			if agents[i].PaneID == id {
				cur = &agents[i]
				break
			}
		}
		status := "shell"
		kind := ""
		if cur != nil && cur.Agent != nil {
			kind = *cur.Agent
			status = cur.AgentStatus
			if reader == nil {
				if ad, err := s.Agents.Get(kind); err == nil {
					sess := ""
					if cur.Session != nil {
						sess = cur.Session.Value
					}
					if p := ad.LocateTranscript(s.Home, deref(cur.Cwd), sess, s.startedAt(id)); p != "" {
						reader = agent.NewTranscriptReader(p, ad, imgDir)
					}
				}
			}
		}
		if reader != nil {
			if reader.Poll() || lastLog == "" {
				lastLog = "-"
				b, _ := json.Marshal(map[string]any{"messages": reader.Messages(), "structured": true})
				if sse.send("messages", b) != nil {
					cancel()
					return
				}
				beat = time.Now()
			}
		} else if rr, err := s.Fleet.Read(ctx, id, "recent_unwrapped", "text", 300); err == nil && rr.Text != lastLog {
			lastLog = rr.Text
			b, _ := json.Marshal(map[string]any{"text": rr.Text, "structured": false})
			if sse.send("log", b) != nil {
				cancel()
				return
			}
			beat = time.Now()
		}

		live := map[string]any{"status": status, "kind": kind}
		if rr, err := s.Fleet.Read(ctx, id, "detection", "text", 0); err == nil {
			if status == "blocked" || (status == "idle" && agent.ScreenAsks(rr.Text)) {
				live["status"] = "blocked"
				live["approval"] = agent.ParseApproval(rr.Text)
			} else if (status == "done" || status == "idle") && agent.HasBackgroundTask(rr.Text) {
				live["status"] = "waiting"
			}
		}
		cancel()
		if b, err := json.Marshal(live); err == nil && string(b) != lastLive {
			lastLive = string(b)
			if sse.send("live", b) != nil {
				return
			}
			beat = time.Now()
		} else if time.Since(beat) > 15*time.Second {
			sse.comment()
			beat = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(700 * time.Millisecond):
		}
	}
}

func (s *Server) markStarted(pane string) {
	s.startMu.Lock()
	if s.started == nil {
		s.started = map[string]time.Time{}
	}
	s.started[pane] = time.Now()
	s.startMu.Unlock()
}

func (s *Server) startedAt(pane string) time.Time {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started == nil {
		return time.Time{}
	}
	return s.started[pane]
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

func (s *Server) handleSuperSnapshot(w http.ResponseWriter, r *http.Request) {
	thread := r.URL.Query().Get("thread")
	writeJSON(w, http.StatusOK, s.Engine.Snapshot(r.Context(), thread))
}

func (s *Server) handleSuperSay(w http.ResponseWriter, r *http.Request) {
	var req core.SayRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	if req.From == "" {
		req.From = "you"
	}
	entry, trig, err := s.Engine.Say(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"entry": entry.ID}
	if trig != nil {
		res["trigger"] = trig.ID
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSuperReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string   `json:"text"`
		Thread  string   `json:"thread"`
		Re      string   `json:"re"`
		Options []string `json:"options"`
		Check   bool     `json:"check"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	entry, act, warn, err := s.Engine.Reply(r.Context(), body.Thread, body.Text, body.Re, body.Options, body.Check)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"ok": true, "entry": entry.ID}
	if act != nil {
		res["action"] = act.ID
	}
	if warn != "" {
		res["warning"] = warn
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleResolveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Choice string `json:"choice"`
		By     string `json:"by"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	by := strings.TrimSpace(body.By)
	if by == "" {
		by = "web"
	}
	act, err := s.Engine.ResolveAction(r.Context(), id, by, body.Choice)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, store.ErrActionAlreadyResolved) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": act})
}

func (s *Server) handleSuperPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.Store.SetEntryPinned(r.Context(), body.ID, body.Pinned); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	s.Bus.OnStateChanged()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": body.Pinned})
}

func (s *Server) handleSuperEarlier(w http.ResponseWriter, r *http.Request) {
	thread := r.URL.Query().Get("thread")
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, more, err := s.Store.ListEntriesBefore(r.Context(), thread, before, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "more": more})
}

func (s *Server) handleSuperSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	thread := r.URL.Query().Get("thread")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := s.Store.SearchEntries(r.Context(), thread, q, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSuperSpawn(w http.ResponseWriter, r *http.Request) {
	var req core.SpawnRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	wk, err := s.Engine.Spawn(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"worker":  wk.Name,
		"kind":    wk.Kind,
		"machine": wk.Machine,
		"thread":  wk.Thread,
		"status":  wk.Status,
	})
}

func (s *Server) handleSuperSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To     string `json:"to"`
		Text   string `json:"text"`
		Thread string `json:"thread"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	trig, err := s.Engine.SendToWorker(r.Context(), body.To, body.Thread, body.Text)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"trigger": trig.ID})
}

func (s *Server) handleSuperReport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Pane   string `json:"pane"`
		Text   string `json:"text"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	trig, err := s.Engine.Report(r.Context(), body.Worker, body.Pane, body.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "trigger": trig.ID})
}

func (s *Server) handleSuperSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Thread string `json:"thread"`
		Off    bool   `json:"off"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.Engine.Subscribe(r.Context(), body.Worker, body.Thread, body.Off); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSuperStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.Engine.StopWorker(r.Context(), body.Worker); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSuperAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
		Choice string `json:"choice"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	picked, err := s.Engine.AnswerWorker(r.Context(), body.Worker, body.Choice, "router")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "picked": picked})
}

func (s *Server) handleSuperShow(w http.ResponseWriter, r *http.Request) {
	obj, err := s.Engine.Show(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "id not found"})
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) handleSuperList(w http.ResponseWriter, r *http.Request) {
	workers, _ := s.Store.ListWorkers(r.Context())
	threads, _ := s.Store.ListThreads(r.Context(), 50)
	timers, _ := s.Store.ListTimers(r.Context())
	liveAgents := []map[string]any{}
	if s.Fleet != nil {
		if ags, err := s.Fleet.Agents(r.Context()); err == nil {
			for _, a := range ags {
				name := ""
				if a.Name != nil {
					name = *a.Name
				}
				if name == "" || name == model.RouterName || name == model.DreamName {
					continue
				}
				_, machine := fleet.SplitID(a.PaneID)
				if machine == "" {
					machine = "hub"
				}
				liveAgents = append(liveAgents, map[string]any{
					"name":    name,
					"kind":    deref(a.Agent),
					"machine": machine,
					"cwd":     deref(a.Cwd),
					"status":  a.AgentStatus,
					"pane_id": a.PaneID,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machines": s.Fleet.Machines(),
		"agents":   liveAgents,
		"workers":  workers,
		"threads":  threads,
		"timers":   timers,
	})
}

func (s *Server) handleSuperTimers(w http.ResponseWriter, r *http.Request) {
	timers, err := s.Store.ListTimers(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timers": timers})
}

func (s *Server) handleSuperAddTimer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec   string `json:"spec"`
		Text   string `json:"text"`
		Thread string `json:"thread"`
		Target string `json:"target"`
		Owner  string `json:"owner"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tm, err := s.Engine.AddTimer(r.Context(), body.Spec, body.Text, body.Thread, body.Target, body.Owner)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timer": tm})
}

func (s *Server) handleSuperDeleteTimer(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteTimer(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	s.Bus.OnStateChanged()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	st := s.Engine.Settings()
	// Never return plaintext WeCom secret back to the browser; expose masked sentinel.
	masked := st
	if masked.WeComSecret != "" {
		masked.WeComSecret = "••••••••"
	}
	hookKey := ""
	if s.Auth != nil {
		hookKey = s.Auth.HookKey()
	}
	wecomErr := ""
	wecomPending := []wecom.PendingUser{}
	if s.WeCom != nil {
		wecomErr = s.WeCom.LastError()
		wecomPending = s.WeCom.PendingUsers()
	}
	var hubAgents []string
	machines := []model.Machine{}
	if s.Fleet != nil {
		machines = s.Fleet.Machines()
		for _, m := range machines {
			if m.Local {
				hubAgents = m.Agents
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":      masked,
		"policies":      s.Engine.Policies(),
		"wecom_online":  s.Bus.WeComOnline(),
		"wecom_error":   wecomErr,
		"wecom_pending": wecomPending,
		"hub_agents":    hubAgents,
		"machines":      machines,
		"hook_key":      hookKey,
	})
}

func (s *Server) handleWeComApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID  string `json:"user_id"`
		Dismiss bool   `json:"dismiss"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	uid := strings.TrimSpace(body.UserID)
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user_id is required"})
		return
	}
	if body.Dismiss {
		if s.WeCom != nil {
			s.WeCom.DismissPending(uid)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	st := s.Engine.Settings()
	exists := false
	for _, u := range st.WeComAllowUsers {
		if strings.TrimSpace(u) == uid {
			exists = true
			break
		}
	}
	if !exists {
		st.WeComAllowUsers = append(st.WeComAllowUsers, uid)
	}
	if err := s.Engine.SaveConfig(r.Context(), &st, nil); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if s.WeCom != nil {
		s.WeCom.DismissPending(uid)
		if st.WeComEnabled {
			s.WeCom.UpdateConfig(wecom.Config{
				BotID:      st.WeComBotID,
				Secret:     st.WeComSecret,
				AllowUsers: st.WeComAllowUsers,
				DefaultTo:  st.WeComDefaultTo,
			})
		}
		if s.WeCom.Online() {
			wc := s.WeCom
			go func() {
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = wc.SendMarkdown(cctx, uid, "已授权绑定 Corgi，现在可以直接在这里发消息指挥你的电脑了。")
			}()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allow_users": st.WeComAllowUsers})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings *model.Settings `json:"settings"`
		Policies []model.Policy  `json:"policies"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Settings != nil && body.Settings.WeComSecret == "••••••••" {
		body.Settings.WeComSecret = s.Engine.Settings().WeComSecret
	}
	if err := s.Engine.SaveConfig(r.Context(), body.Settings, body.Policies); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.Settings != nil && s.WeCom != nil {
		st := s.Engine.Settings()
		if st.WeComEnabled {
			s.WeCom.UpdateConfig(wecom.Config{
				BotID:      st.WeComBotID,
				Secret:     st.WeComSecret,
				AllowUsers: st.WeComAllowUsers,
				DefaultTo:  st.WeComDefaultTo,
			})
		} else {
			s.WeCom.UpdateConfig(wecom.Config{})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	mm := s.Engine.Memory()
	writeJSON(w, http.StatusOK, s.Engine.MemorySnapshot(r.Context(), s.tilde(mm.RouterDir())))
}

func (s *Server) handleSaveMemory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.Engine.Memory().SaveFile(body.Path, body.Text); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRestartRouter(w http.ResponseWriter, r *http.Request) {
	_ = s.Engine.RestartRouter(r.Context())
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

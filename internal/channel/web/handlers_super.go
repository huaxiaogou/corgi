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
	var req struct {
		core.SayRequest
		Mode string `json:"mode"` // auto | queue | steer
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Channel == "" {
		req.Channel = "web"
	}
	if req.From == "" {
		req.From = "you"
	}
	queued, item, entry, trig, err := s.Engine.SubmitChatSay(r.Context(), req.SayRequest, req.Mode)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"ok": true, "queued": queued}
	if item != nil {
		res["item"] = item
	}
	if entry != nil {
		res["entry"] = entry.ID
	}
	if trig != nil {
		res["trigger"] = trig.ID
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSteerChatQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thread string `json:"thread"`
	}
	_ = decodeJSON(w, r, &body)
	entry, trig, err := s.Engine.SteerChatPrompt(r.Context(), body.Thread, r.PathValue("qid"))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"ok": true}
	if entry != nil {
		res["entry"] = entry.ID
	}
	if trig != nil {
		res["trigger"] = trig.ID
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCancelChatQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thread string `json:"thread"`
	}
	_ = decodeJSON(w, r, &body)
	scope := core.RouterQueueScope(body.Thread)
	if err := s.Engine.CancelQueuedPrompt(r.Context(), scope, r.PathValue("qid")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleResumeChatQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thread string `json:"thread"`
	}
	_ = decodeJSON(w, r, &body)
	s.Engine.ResumeQueue(core.RouterQueueScope(body.Thread))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStopRouter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thread string `json:"thread"`
	}
	_ = decodeJSON(w, r, &body)
	paused, err := s.Engine.StopRouter(r.Context(), body.Thread)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": paused})
}

func (s *Server) handleSuperReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string   `json:"text"`
		Thread   string   `json:"thread"`
		Re       string   `json:"re"`
		Options  []string `json:"options"`
		File     string   `json:"file"`
		Files    []string `json:"files"`
		Machine  string   `json:"machine"`
		WorkID   string   `json:"work_id"`
		Work     string   `json:"work"`
		Check    bool     `json:"check"`
		Digest   bool     `json:"digest"`
		Worker   string   `json:"worker"`
		Category string   `json:"category"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	files := body.Files
	if strings.TrimSpace(body.File) != "" {
		files = append([]string{body.File}, files...)
	}
	workID := strings.TrimSpace(body.WorkID)
	if workID == "" {
		workID = strings.TrimSpace(body.Work)
	}
	out, err := s.Engine.ReplyDetailed(r.Context(), core.ReplyRequest{
		Thread:   body.Thread,
		Text:     body.Text,
		ReID:     body.Re,
		Options:  body.Options,
		Files:    files,
		Machine:  body.Machine,
		WorkID:   workID,
		Check:    body.Check,
		Digest:   body.Digest,
		Worker:   body.Worker,
		Category: body.Category,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"ok": true, "entry": out.Entry.ID}
	if out.Action != nil {
		res["action"] = out.Action.ID
	}
	if out.Warning != "" {
		res["warning"] = out.Warning
	}
	if len(out.Delivery.Files) > 0 {
		res["files"] = out.Delivery.Files
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleKeepWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keep bool `json:"keep"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wk, err := s.Engine.SetWorkerKeep(r.Context(), r.PathValue("name"), body.Keep)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "worker": wk.Name, "keep": wk.Keep})
}

func (s *Server) handleResolveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Choice string `json:"choice"`
		By     string `json:"by"`
		Custom bool   `json:"custom"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	by := strings.TrimSpace(body.By)
	if by == "" {
		by = "web"
	}
	var (
		act *model.Action
		err error
	)
	if body.Custom {
		act, err = s.Engine.ResolveActionCustom(r.Context(), id, by, body.Choice)
	} else {
		act, err = s.Engine.ResolveAction(r.Context(), id, by, body.Choice)
	}
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
	res := map[string]any{"entries": entries, "more": more}
	if resolved := s.Engine.ResolvedActionsForEntries(r.Context(), entries); len(resolved) > 0 {
		res["resolved_actions"] = resolved
	}
	writeJSON(w, http.StatusOK, res)
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
	res := map[string]any{
		"worker":  wk.Name,
		"kind":    wk.Kind,
		"machine": wk.Machine,
		"thread":  wk.Thread,
		"status":  wk.Status,
		"cwd":     wk.Cwd,
	}
	if wk.WorktreeBranch != "" {
		res["worktree_branch"] = wk.WorktreeBranch
		res["worktree_path"] = wk.WorktreePath
	}
	if wk.WorkItemID != "" {
		res["work_item_id"] = wk.WorkItemID
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleSuperSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To     string `json:"to"`
		Text   string `json:"text"`
		Thread string `json:"thread"`
		WorkID string `json:"work_id"`
		Work   string `json:"work"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	workID := strings.TrimSpace(body.WorkID)
	if workID == "" {
		workID = strings.TrimSpace(body.Work)
	}
	trig, resolvedWorkID, err := s.Engine.SendToWorkerWithWork(r.Context(), body.To, body.Thread, body.Text, workID)
	if err != nil {
		code := http.StatusNotFound
		if strings.Contains(err.Error(), "multiple open work items") || errors.Is(err, store.ErrWorkItemClosed) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"trigger": trig.ID}
	if resolvedWorkID != "" {
		res["work_item_id"] = resolvedWorkID
	}
	writeJSON(w, http.StatusAccepted, res)
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
	ctx := r.Context()
	workItems, err := s.Store.ListWorkItems(ctx, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	pendingActions, err := s.Store.ListPendingActions(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	workers, err := s.Store.ListWorkers(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	threads, err := s.Store.ListThreads(ctx, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	timers, err := s.Store.ListTimers(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	liveAgents := []map[string]any{}
	if s.Fleet != nil {
		if ags, aErr := s.Fleet.Agents(ctx); aErr == nil {
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
	machines := []model.Machine{}
	if s.Fleet != nil {
		machines = s.Fleet.Machines()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_items":      workItems,
		"pending_actions": pendingActions,
		"machines":        machines,
		"agents":          liveAgents,
		"workers":         workers,
		"threads":         threads,
		"timers":          timers,
	})
}

func (s *Server) handleSuperTasks(w http.ResponseWriter, r *http.Request) {
	includeClosed := r.URL.Query().Get("all") == "1" || strings.EqualFold(r.URL.Query().Get("all"), "true")
	items, err := s.Engine.ListWorkItems(r.Context(), includeClosed)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_items": items})
}

func (s *Server) handleSuperSetTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Thread   string `json:"thread"`
		Title    string `json:"title"`
		NextStep string `json:"next_step"`
		Worker   string `json:"worker"`
		Branch   string `json:"branch"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wi, err := s.Engine.SetWorkItem(r.Context(), body.ID, body.Thread, body.Title, body.NextStep, body.Worker, body.Branch)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, store.ErrWorkItemClosed) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "work_item": wi})
}

func (s *Server) handleSuperDoneTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wi, err := s.Engine.CloseWorkItem(r.Context(), r.PathValue("id"), body.Note)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, store.ErrWorkItemClosed) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "work_item": wi})
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
	if err := s.Engine.RestartRouter(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

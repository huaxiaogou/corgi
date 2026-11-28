package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// Never trust caller-supplied ActionID on public /api/super/say (only server-created action
	// resolution creates entries bound to an approved ActionID).
	req.ActionID = ""
	// Derive human provenance at trusted ingress instead of trusting caller-spoofed channel/from.
	master, dev := s.identity(r)
	if dev != nil && dev.Kind == "browser" {
		req.Channel = "web"
		req.From = "you"
	} else if master {
		switch strings.ToLower(strings.TrimSpace(req.Channel)) {
		case "cli":
			req.Channel = "cli"
		default:
			req.Channel = "web"
		}
		if strings.TrimSpace(req.From) == "" {
			req.From = "you"
		}
	} else {
		req.Channel = "machine"
		req.From = "machine"
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
		Text         string               `json:"text"`
		Thread       string               `json:"thread"`
		Re           string               `json:"re"`
		Options      []string             `json:"options"`
		Choices      []model.ActionChoice `json:"choices"`
		Mode         string               `json:"mode"`
		ConfirmLabel string               `json:"confirm_label"`
		File         string               `json:"file"`
		Files        []string             `json:"files"`
		Machine      string               `json:"machine"`
		WorkID       string               `json:"work_id"`
		Work         string               `json:"work"`
		Check        bool                 `json:"check"`
		Digest       bool                 `json:"digest"`
		Worker       string               `json:"worker"`
		Category     string               `json:"category"`
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
		Thread:            body.Thread,
		Text:              body.Text,
		ReID:              body.Re,
		Options:           body.Options,
		StructuredChoices: body.Choices,
		Mode:              body.Mode,
		ConfirmLabel:      body.ConfirmLabel,
		Files:             files,
		Machine:           body.Machine,
		WorkID:            workID,
		Check:             body.Check,
		Digest:            body.Digest,
		Worker:            body.Worker,
		Category:          body.Category,
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
	if len(out.Entry.Artifacts) > 0 {
		res["artifacts"] = out.Entry.Artifacts
	}
	if out.Delivery.LinkStatus != "" {
		res["link_status"] = out.Delivery.LinkStatus
	}
	if out.Delivery.LinkError != "" {
		res["link_error"] = out.Delivery.LinkError
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

func (s *Server) handleResumeWorker(w http.ResponseWriter, r *http.Request) {
	var body core.ResumeWorkerRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	wk, resumed, err := s.Engine.ResumeWorkerSession(r.Context(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if resumed && wk.PaneID != "" {
		s.markStarted(wk.PaneID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"resumed":      resumed,
		"pane_id":      wk.PaneID,
		"workspace_id": wk.WorkspaceID,
		"name":         wk.Name,
		"worker":       wk.Name,
		"work_item_id": wk.WorkItemID,
	})
}

func (s *Server) resolveActionActor(r *http.Request, rawBy string) string {
	master, dev := s.identity(r)
	if dev != nil && dev.Kind == "browser" {
		return "web"
	}
	if master {
		switch strings.ToLower(strings.TrimSpace(rawBy)) {
		case "cli", "router", "web":
			return strings.ToLower(strings.TrimSpace(rawBy))
		default:
			return "web"
		}
	}
	return "machine"
}

func (s *Server) handleResolveAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Choice  string   `json:"choice"`
		Choices []string `json:"choices"`
		By      string   `json:"by"`
		Custom  bool     `json:"custom"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	by := s.resolveActionActor(r, body.By)
	var (
		act *model.Action
		err error
	)
	if body.Custom {
		act, err = s.Engine.ResolveActionCustom(r.Context(), id, by, body.Choice)
	} else if len(body.Choices) > 0 {
		act, err = s.Engine.ResolveActionChoices(r.Context(), id, by, body.Choices)
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
	s.Engine.HydrateEntriesArtifacts(r.Context(), entries)
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
		"worker":       wk.Name,
		"kind":         wk.Kind,
		"machine":      wk.Machine,
		"thread":       wk.Thread,
		"status":       wk.Status,
		"cwd":          wk.Cwd,
		"model":        wk.Model,
		"effort":       wk.Effort,
		"model_source": wk.ModelSource,
	}
	if wk.ProjectID != "" {
		res["project_id"] = wk.ProjectID
		res["project_name"] = wk.ProjectName
	}
	if wk.WorktreeBranch != "" {
		res["worktree_branch"] = wk.WorktreeBranch
		res["worktree_path"] = wk.WorktreePath
	}
	if wk.WorkItemID != "" {
		res["work_item_id"] = wk.WorkItemID
	}
	if wk.Constraint != "" {
		res["constraint"] = wk.Constraint
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleSuperSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To        string `json:"to"`
		Text      string `json:"text"`
		Thread    string `json:"thread"`
		WorkID    string `json:"work_id"`
		Work      string `json:"work"`
		WorkTitle string `json:"work_title"`
		Title     string `json:"title"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	workID := strings.TrimSpace(body.WorkID)
	if workID == "" {
		workID = strings.TrimSpace(body.Work)
	}
	workTitle := strings.TrimSpace(body.WorkTitle)
	if workTitle == "" {
		workTitle = strings.TrimSpace(body.Title)
	}
	trig, resolvedWorkID, err := s.Engine.SendToWorkerWithWorkTitle(r.Context(), body.To, body.Thread, body.Text, workID, workTitle)
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

func (s *Server) handleSuperVerify(w http.ResponseWriter, r *http.Request) {
	var req core.VerifyWorkerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := s.Engine.VerifyWorker(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "result": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
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
		"projects":        s.Engine.Settings().Projects,
		"agents":          liveAgents,
		"workers":         workers,
		"threads":         threads,
		"timers":          timers,
	})
}

func (s *Server) handleSuperTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.EqualFold(strings.TrimSpace(q.Get("status")), "closed") {
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 15
		}
		if limit > 100 {
			limit = 100
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		if offset < 0 {
			offset = 0
		}
		items, total, err := s.Engine.ListClosedWorkItems(r.Context(), q.Get("thread"), q.Get("q"), limit, offset)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"work_items": items,
			"total":      total,
			"has_more":   offset+len(items) < total,
		})
		return
	}
	includeClosed := q.Get("all") == "1" || strings.EqualFold(q.Get("all"), "true")
	items, err := s.Engine.ListWorkItems(r.Context(), includeClosed)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"work_items": items})
}

func (s *Server) handleSuperSetTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID              string `json:"id"`
		Thread          string `json:"thread"`
		Title           string `json:"title"`
		NextStep        string `json:"next_step"`
		ClearNextStep   bool   `json:"clear_next_step"`
		Worker          string `json:"worker"`
		Branch          string `json:"branch"`
		UserEditedTitle bool   `json:"user_edited_title"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var (
		wi  *model.WorkItem
		err error
	)
	if body.ClearNextStep && strings.TrimSpace(body.ID) != "" &&
		strings.TrimSpace(body.Title) == "" &&
		strings.TrimSpace(body.NextStep) == "" &&
		strings.TrimSpace(body.Worker) == "" &&
		strings.TrimSpace(body.Branch) == "" {
		wi, err = s.Engine.ClearWorkItemNextStep(r.Context(), body.ID)
	} else {
		wi, err = s.Engine.SetWorkItemWithOptions(r.Context(), body.ID, body.Thread, body.Title, body.NextStep, body.Worker, body.Branch, body.UserEditedTitle)
		if err == nil && body.ClearNextStep && strings.TrimSpace(body.NextStep) == "" {
			wi, err = s.Engine.ClearWorkItemNextStep(r.Context(), wi.ID)
		}
	}
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

func (s *Server) handleSuperReopenTask(w http.ResponseWriter, r *http.Request) {
	wi, err := s.Engine.ReopenWorkItem(r.Context(), r.PathValue("id"))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "work_item": wi})
}

func (s *Server) handleSuperUnbindTaskWorker(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string `json:"worker"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wi, err := s.Engine.UnbindWorkerFromWorkItem(r.Context(), r.PathValue("id"), body.Worker)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
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

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可查看系统设置"})
		return
	}
	st := s.Engine.Settings()
	// Never return plaintext WeCom secret back to the browser; expose masked sentinel.
	masked := st
	if masked.WeComSecret != "" {
		masked.WeComSecret = "••••••••"
	}
	secInfo := s.Engine.RouterAPISecretInfo(r.Context())
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
		"settings":                    masked,
		"has_router_api_key":          secInfo.HasKey,
		"router_api_key_masked":       secInfo.Masked,
		"router_api_key_bound_origin": secInfo.BoundOrigin,
		"router_api_key_origin_match": secInfo.OriginMatch,
		"effective_public_url":        s.Engine.EffectivePublicURL(),
		"flag_public_url":             s.Engine.FlagPublicURL(),
		"policies":                    s.Engine.Policies(),
		"wecom_online":                s.Bus.WeComOnline(),
		"wecom_error":                 wecomErr,
		"wecom_pending":               wecomPending,
		"hub_agents":                  hubAgents,
		"machines":                    machines,
		"hook_key":                    hookKey,
	})
}

func (s *Server) handleWeComFocusClear(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅控制台主人可操作企微直聊焦点"})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
			return
		}
	}
	key := strings.TrimSpace(body.Key)
	if key != "" {
		if !strings.HasPrefix(key, core.WeComFocusKVPrefix) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid wecom focus key"})
			return
		}
		cleared, err := s.Engine.ClearWeComFocus(r.Context(), key)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": cleared})
		return
	}
	removed, err := s.Engine.ClearAllWeComFocuses(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": removed > 0, "removed": removed})
}

func (s *Server) handleWeComApprove(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可审批企微白名单"})
		return
	}
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

	st, err := s.Engine.ApproveWeComUser(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if s.WeCom != nil {
		s.WeCom.DismissPending(uid)
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
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可修改系统设置"})
		return
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetRouterAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可配置 API Key"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Clear    bool   `json:"clear"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Clear {
		if err := s.Engine.ClearRouterAPIKey(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "has_router_api_key": false})
		return
	}
	info, err := s.Engine.SetRouterAPIKey(r.Context(), body.Provider, body.BaseURL, body.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                          true,
		"has_router_api_key":          info.HasKey,
		"router_api_key_masked":       info.Masked,
		"router_api_key_bound_origin": info.BoundOrigin,
		"router_api_key_origin_match": info.OriginMatch,
	})
}

func (s *Server) handleTestRouterAPI(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可测试大脑 API"})
		return
	}
	var body core.TestRouterAPIRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	res := s.Engine.TestRouterAPI(r.Context(), body)
	code := http.StatusOK
	if !res.OK {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
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

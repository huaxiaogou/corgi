package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/store"
)

var hookSourceRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// handleWebhook ingests external JSON webhooks (GitHub or generic) into the Trigger inbox
// and acknowledges immediately with 202 Accepted (zero LLM work in the HTTP path).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	master, dev := s.identity(r)
	authed := master || dev != nil
	if !(s.Auth != nil && s.Auth.VerifyHookKey(key)) && !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid webhook key"})
		return
	}

	src := strings.ToLower(strings.TrimSpace(r.PathValue("source")))
	if !hookSourceRe.MatchString(src) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid source name"})
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed reading body"})
		return
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)

	var thread, from, text, idem string
	if ghEv := r.Header.Get("X-GitHub-Event"); ghEv != "" {
		thread, from, text = parseGitHubWebhook(ghEv, payload)
		idem = r.Header.Get("X-GitHub-Delivery")
	} else {
		thread, from, text, idem = parseGenericWebhook(src, payload, raw, r)
	}
	if strings.TrimSpace(text) == "" {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "ignored": "empty event"})
		return
	}
	if idem != "" {
		idem = "hook:" + src + ":" + idem
	}

	trig, err := s.Engine.Ingest(r.Context(), "webhook:"+src, thread, from, text, idem, "", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"ok": true}
	if trig != nil {
		res["trigger"] = trig.ID
		res["policy"] = trig.Policy
	} else {
		res["duplicate"] = true
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) handleSuperTriggers(w http.ResponseWriter, r *http.Request) {
	trigs, err := s.Store.ListTriggers(r.Context(), 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": trigs})
}

func (s *Server) handleDismissAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By string `json:"by"`
	}
	_ = decodeJSON(w, r, &body)
	by := s.resolveActionActor(r, body.By)
	act, err := s.Engine.DismissActionBy(r.Context(), r.PathValue("id"), by)
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

func (s *Server) handleSnoozeAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Until   string `json:"until"`
		Minutes int    `json:"minutes"`
		By      string `json:"by"`
	}
	_ = decodeJSON(w, r, &body)
	by := s.resolveActionActor(r, body.By)
	var until time.Time
	if s := strings.TrimSpace(body.Until); s != "" {
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			until = parsed
		}
	} else if body.Minutes > 0 {
		until = time.Now().Add(time.Duration(body.Minutes) * time.Minute)
	}
	act, err := s.Engine.SnoozeActionBy(r.Context(), r.PathValue("id"), by, until)
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

func (s *Server) handleStartDream(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "仅 Hub 管理员或已配对浏览器可手动触发整理记忆"})
		return
	}
	if err := s.Engine.StartDream(r.Context(), "manual"); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleSuperSeen(w http.ResponseWriter, _ *http.Request) {
	if s.Bus != nil {
		s.Bus.RecordWebSeen()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseGenericWebhook(src string, p map[string]any, raw []byte, r *http.Request) (thread, from, text, idem string) {
	thread = strings.TrimSpace(r.Header.Get("X-Corgi-Thread"))
	if thread == "" {
		thread = jsonStr(p["thread"])
	}
	if thread == "" {
		thread = src
	}
	if !strings.Contains(thread, ":") {
		thread = src + ":" + thread
	}
	from = jsonStr(p["from"])
	text = jsonStr(p["text"])
	if text == "" {
		text = clipHookText(string(raw), 4000)
	}
	idem = strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
	if idem == "" {
		idem = jsonStr(p["id"])
	}
	return thread, from, text, idem
}

func parseGitHubWebhook(kind string, p map[string]any) (thread, from, text string) {
	repo := jsonStr(jsonDig(p, "repository", "full_name"))
	num := firstNonEmpty(
		jsonStr(jsonDig(p, "issue", "number")),
		jsonStr(jsonDig(p, "pull_request", "number")),
		jsonStr(p["number"]),
	)
	thread = "github:" + repo
	if num != "" {
		thread += "#" + num
	}
	from = jsonStr(jsonDig(p, "sender", "login"))

	if kind == "check_run" || kind == "workflow_run" || kind == "check_suite" {
		concl := jsonStr(jsonDig(p, kind, "conclusion"))
		if concl == "" {
			return thread, from, "" // skip noisy queued/in_progress check events
		}
		name := jsonStr(jsonDig(p, kind, "name"))
		branch := jsonStr(jsonDig(p, kind, "head_branch"))
		url := jsonStr(jsonDig(p, kind, "html_url"))
		return thread, from, strings.TrimSpace(fmt.Sprintf("GitHub %s %q on %s: %s\n%s", kind, name, branch, concl, url))
	}

	title := firstNonEmpty(jsonStr(jsonDig(p, "issue", "title")), jsonStr(jsonDig(p, "pull_request", "title")))
	body := firstNonEmpty(
		jsonStr(jsonDig(p, "comment", "body")),
		jsonStr(jsonDig(p, "review", "body")),
		jsonStr(jsonDig(p, "issue", "body")),
		jsonStr(jsonDig(p, "pull_request", "body")),
	)
	url := firstNonEmpty(
		jsonStr(jsonDig(p, "comment", "html_url")),
		jsonStr(jsonDig(p, "issue", "html_url")),
		jsonStr(jsonDig(p, "pull_request", "html_url")),
	)
	action := jsonStr(p["action"])
	text = fmt.Sprintf("GitHub %s/%s on %s: %s\n%s\n%s", kind, action, thread, title, clipHookText(body, 2500), url)
	return thread, from, strings.TrimSpace(text)
}

func jsonStr(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return fmt.Sprintf("%.0f", x)
	}
	return ""
}

func jsonDig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clipHookText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

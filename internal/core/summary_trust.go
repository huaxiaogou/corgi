package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// preTrustWorkspace pre-writes folder trust on the target machine (local or remote node)
// before launching an unattended agent session when AskPermissions is false.
func (e *Engine) preTrustWorkspace(ctx context.Context, machine, kind, cwd string) {
	if cwd == "" {
		return
	}
	if machine == "" {
		if ad, err := e.agents.Get(kind); err == nil {
			if err := ad.PreTrustWorkspace(e.home, cwd); err != nil {
				e.logger.Warn("local workspace pre-trust skipped", "kind", kind, "cwd", cwd, "err", err)
			}
		}
		return
	}
	if e.fleet == nil {
		return
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"kind": kind, "dir": cwd})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/trust", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		e.logger.Warn("remote workspace pre-trust failed", "machine", machine, "kind", kind, "err", err)
		return
	}
	_ = resp.Body.Close()
}

// answerWorkerTrust auto-confirms a folder-trust dialog when AskPermissions is disabled
// and the prompt matches the worker's own startup directory.
func (e *Engine) answerWorkerTrust(ctx context.Context, w *model.Worker, ap model.Approval) bool {
	if e.Settings().AskPermissions || w.PaneID == "" || len(ap.Choices) < 2 {
		return false
	}
	full := ap.Title + "\n" + ap.Detail + "\n" + ap.Raw
	if !trustHintRe.MatchString(full) {
		return false
	}
	if w.Cwd != "" {
		base := filepath.Base(filepath.Clean(w.Cwd))
		if !strings.Contains(full, w.Cwd) && (base == "" || base == "." || base == "/" || !strings.Contains(full, base)) {
			return false
		}
	}

	e.mu.Lock()
	if time.Since(e.lastTrust[w.PaneID]) < 15*time.Second {
		e.mu.Unlock()
		return false
	}
	e.lastTrust[w.PaneID] = time.Now()
	e.lastAnswer[w.PaneID] = time.Now()
	e.mu.Unlock()

	key := ap.Choices[0].Key
	cursor := 1
	if ap.Style == "cursor" {
		key = ""
	}
	if err := e.sendDialogAnswer(ctx, w.PaneID, key, cursor); err != nil {
		return false
	}
	e.logger.Info("auto-trusted worker startup folder", "worker", w.Name, "cwd", w.Cwd)
	return true
}

// lastWorkerFullMessage fetches the worker's latest untruncated assistant message from its machine's
// transcript (via /api/panes/{id}/last?full=1 for remote nodes), returning fromTranscript=true when
// read from structured transcript logs rather than raw terminal screen fallback.
func (e *Engine) lastWorkerFullMessage(ctx context.Context, w *model.Worker) (string, bool) {
	full, fromTr, _ := e.lastWorkerFullMessageDetailed(ctx, w)
	return full, fromTr
}

// lastWorkerFullMessageDetailed additionally reports afterLastUser=true when the transcript contains
// an assistant reply written strictly after the most recent user message (false when the transcript
// still ends on the user prompt and no fresh answer has been written yet).
func (e *Engine) lastWorkerFullMessageDetailed(ctx context.Context, w *model.Worker) (string, bool, bool) {
	if w == nil || w.PaneID == "" || e.fleet == nil {
		return "(no output captured)", false, false
	}
	localPane, machine := fleet.SplitID(w.PaneID)
	if machine != "" {
		if hc, err := e.fleet.HTTPClient(machine); err == nil {
			rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			req, _ := http.NewRequestWithContext(rctx, http.MethodGet, "http://node/api/panes/"+url.PathEscape(localPane)+"/last?full=1", nil)
			resp, rErr := hc.Do(req)
			cancel()
			if rErr == nil {
				var out struct {
					Text           string `json:"text"`
					FromTranscript bool   `json:"from_transcript"`
					AfterLastUser  *bool  `json:"after_last_user"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&out)
				_ = resp.Body.Close()
				if t := strings.TrimSpace(out.Text); t != "" {
					fromTr := out.FromTranscript || len([]rune(t)) > 900
					afterUser := true
					if out.AfterLastUser != nil {
						afterUser = *out.AfterLastUser
					}
					return t, fromTr, afterUser
				}
			}
		}
	} else if ad, err := e.agents.Get(w.Kind); err == nil {
		if p := ad.LocateTranscript(e.home, w.Cwd, w.Session, w.CreatedAt); p != "" {
			tr := agent.NewTranscriptReader(p, ad, "")
			tr.OpenTail()
			if after, ok := tr.LastAssistantAfterLastUser(128000); ok {
				return after, true, true
			}
			if last := strings.TrimSpace(tr.LastAssistant(128000)); last != "" {
				return last, true, false
			}
		}
	}
	if rr, err := e.fleet.Read(ctx, w.PaneID, "detection", "text", 0); err == nil {
		if tail := agent.TailLines(rr.Text, 14); tail != "" {
			return tail, false, true
		}
	}
	return "(no output captured)", false, false
}

// workerTurnToolStats returns bounded current-turn tool result statistics for thrash detection,
// reading locally on Hub or via GET /api/panes/{id}/last?tool_stats=1 on remote Nodes.
func (e *Engine) workerTurnToolStats(ctx context.Context, w *model.Worker) (*model.ToolStats, bool) {
	if w == nil || w.PaneID == "" || e.fleet == nil {
		return nil, false
	}
	localPane, machine := fleet.SplitID(w.PaneID)
	if machine != "" {
		hc, err := e.fleet.HTTPClient(machine)
		if err != nil {
			return nil, false
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://node/api/panes/"+url.PathEscape(localPane)+"/last?tool_stats=1", nil)
		if err != nil {
			return nil, false
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, false
		}
		var out struct {
			ToolStats *model.ToolStats `json:"tool_stats"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) != nil || out.ToolStats == nil {
			return nil, false
		}
		return out.ToolStats, true
	}
	ad, err := e.agents.Get(w.Kind)
	if err != nil {
		return nil, false
	}
	p := ad.LocateTranscript(e.home, w.Cwd, w.Session, w.CreatedAt)
	if p == "" {
		return nil, false
	}
	stats := agent.ComputeTurnToolStats(p, ad, 1<<20)
	if stats == nil {
		return nil, false
	}
	return stats, true
}

// lastWorkerSummary fetches the worker's latest assistant message from its machine's transcript
// (via /api/panes/{id}/last for remote nodes), falling back to the last 14 terminal lines.
func (e *Engine) lastWorkerSummary(ctx context.Context, w *model.Worker) string {
	full, _ := e.lastWorkerFullMessage(ctx, w)
	if full == "(no output captured)" {
		return full
	}
	return clipMultiline(full, 1200)
}

// lookupRemoteSession queries a remote node's /api/titles endpoint to recover a session ID
// (especially for Antigravity) before closing a worker's workspace.
func (e *Engine) lookupRemoteSession(ctx context.Context, paneID string) string {
	localPane, machine := fleet.SplitID(paneID)
	if localPane == "" || e.fleet == nil {
		return ""
	}
	if machine == "" {
		return ""
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return ""
	}
	rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://node/api/titles", nil)
	if err != nil {
		return ""
	}
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var body struct {
		Sessions map[string]model.PaneSession `json:"sessions"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Sessions != nil {
		return body.Sessions[localPane].Session
	}
	return ""
}

func formatChoicesText(ap model.Approval) string {
	if len(ap.Choices) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nChoices:")
	for i, c := range ap.Choices {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, c.Label)
	}
	return b.String()
}

func expandInHome(home, rawPath string) string {
	p := strings.TrimSpace(rawPath)
	if home == "" {
		return p
	}
	switch {
	case p == "" || p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return path.Clean(home + "/" + p[2:])
	case !strings.HasPrefix(p, "/"):
		return path.Clean(home + "/" + p)
	default:
		return path.Clean(p)
	}
}

func clipMultiline(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + " …"
}

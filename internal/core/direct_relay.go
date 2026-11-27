package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// DirectTurnResult delivers a Worker's direct reply back to an interactive channel (e.g. WeCom)
// while Hub simultaneously snapshots long reports and records background context.
type DirectTurnResult struct {
	WorkerName       string
	Machine          string
	MachineLabel     string
	Kind             string
	FullText         string
	ReportArtifactID string
	PreviewURL       string
	QuotaError       string
}

// DirectWorkerRequest dispatches a user message straight to a live or resumable Worker.
type DirectWorkerRequest struct {
	Name       string
	Machine    string
	Session    string
	Kind       string
	Cwd        string
	WorkItemID string
	Text       string
	OnDone     func(ctx context.Context, res DirectTurnResult)
}

// WorkerPickItem is a concise summary of a live or resumable Worker for channel pickers.
type WorkerPickItem struct {
	Name        string
	Title       string
	Machine     string
	Kind        string
	Status      string
	Session     string
	WorkItemID  string
	HasWorkItem bool
	Ended       bool
}

type pendingDirectTurn struct {
	prompt   string
	prevHash string
	at       time.Time
	callback func(ctx context.Context, res DirectTurnResult)
}

func hashTurnText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "(no output captured)" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

// Fleet exposes the underlying multi-machine Fleet for channel streaming helpers.
func (e *Engine) Fleet() *fleet.Fleet { return e.fleet }

// GetKV reads a persisted key-value entry from SQLite.
func (e *Engine) GetKV(ctx context.Context, key string) (string, bool, error) {
	if e.store == nil {
		return "", false, nil
	}
	return e.store.GetKV(ctx, key)
}

// SetKV writes a persisted key-value entry to SQLite.
func (e *Engine) SetKV(ctx context.Context, key, val string) error {
	if e.store == nil {
		return nil
	}
	return e.store.SetKV(ctx, key, val)
}

// DeleteKV removes a persisted key-value entry from SQLite.
func (e *Engine) DeleteKV(ctx context.Context, key string) error {
	if e.store == nil {
		return nil
	}
	return e.store.DeleteKV(ctx, key)
}

// BuildArtifactShareURL returns the public preview URL for artID when public_url is configured.
func (e *Engine) BuildArtifactShareURL(ctx context.Context, artID string) string {
	artID = strings.TrimSpace(artID)
	if artID == "" {
		return ""
	}
	return e.BuildPreviewURL(ctx, artID)
}

// MatchesMainChatCorgiReply checks whether quoteText corresponds to a recent Corgi main-chat
// reply without a [#worker] tag so quoting a main-chat message while in Worker focus routes
// that single turn cleanly back to Corgi Router.
func (e *Engine) MatchesMainChatCorgiReply(ctx context.Context, thread, quoteText string) bool {
	if e.store == nil {
		return false
	}
	q := normalizeQuoteCompare(quoteText)
	qRunes := []rune(q)
	if len(qRunes) < 4 {
		return false
	}
	if thread == "" {
		thread = model.MainThread
	}
	entries, _, err := e.store.ListEntries(ctx, thread, 40)
	if err != nil {
		return false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i]
		if en == nil || en.Role != "corgi" {
			continue
		}
		if en.WorkerRef != nil && strings.TrimSpace(en.WorkerRef.Name) != "" {
			continue
		}
		txt := normalizeQuoteCompare(en.Text)
		if txt == "" {
			continue
		}
		if strings.Contains(txt, q) || (len([]rune(txt)) >= 6 && strings.Contains(q, txt)) {
			return true
		}
		if len(qRunes) >= 16 && strings.Contains(txt, string(qRunes[:16])) {
			return true
		}
	}
	return false
}

func normalizeQuoteCompare(s string) string {
	s = strings.TrimSpace(s)
	if start := strings.Index(s, "<think>"); start >= 0 {
		if end := strings.Index(s[start:], "</think>"); end >= 0 {
			s = s[:start] + " " + s[start+end+len("</think>"):]
		}
	}
	repl := strings.NewReplacer("**", "", "__", "", "`", "", "\r", " ")
	s = repl.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, ".…")
	return strings.TrimSpace(s)
}

// MachineDisplayName maps a machine ID ("" for Hub) to a short human-readable label.
func (e *Engine) MachineDisplayName(machineID string) string {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" || strings.EqualFold(machineID, "hub") {
		return "Hub"
	}
	if e.fleet != nil {
		for _, m := range e.fleet.Machines() {
			if m.ID == machineID {
				if strings.TrimSpace(m.Name) != "" {
					return m.Name
				}
				break
			}
		}
	}
	return machineID
}

// HasQueuedRouterTriggers reports whether any queued triggers are still waiting for Router.
func (e *Engine) HasQueuedRouterTriggers(ctx context.Context) bool {
	if e.store == nil {
		return false
	}
	queued, err := e.store.ListQueuedTriggers(ctx)
	if err != nil {
		return false
	}
	for _, t := range queued {
		if len(t.Targets) == 0 || slices.Contains(t.Targets, model.RouterName) {
			return true
		}
	}
	return false
}

// RouterContinuousIdle returns how long Router has remained continuously idle/done
// since its last working state or prompt delivery (0 while working or unknown).
func (e *Engine) RouterContinuousIdle() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.routerLastAlive || e.routerTrueIdleSince.IsZero() || e.routerLastStatus == "working" {
		return 0
	}
	return time.Since(e.routerTrueIdleSince)
}

// ListSelectableWorkers returns live and resumable Workers prioritized by open Work Items
// (小本本事项 first with their human-readable Chinese titles), then other live Workers, then
// recent ended sessions, deduplicated by name and capped to limit.
func (e *Engine) ListSelectableWorkers(ctx context.Context, limit int) []WorkerPickItem {
	if limit <= 0 {
		limit = 30
	}
	if e.store == nil {
		return nil
	}

	// Build workerName -> open WorkItem index so real tracked matters surface first with Chinese titles.
	openWorkByWorker := map[string]*model.WorkItem{}
	openWorkByID := map[string]*model.WorkItem{}
	if wis, err := e.store.ListWorkItems(ctx, false); err == nil {
		sort.Slice(wis, func(i, j int) bool { return wis[i].UpdatedAt.After(wis[j].UpdatedAt) })
		for _, wi := range wis {
			if wi == nil || wi.ClosedAt != nil {
				continue
			}
			openWorkByID[wi.ID] = wi
			for _, wn := range wi.Workers {
				wn = strings.TrimSpace(wn)
				if wn != "" && openWorkByWorker[wn] == nil {
					openWorkByWorker[wn] = wi
				}
			}
		}
	}

	type scoredItem struct {
		item       WorkerPickItem
		tier       int // 0: primary worker per open WorkItem, 1: active/blocked/failed unattached live, 2: secondary WI workers & named idle live, 3: auto-named idle live, 4: ended
		statusRank int // 0: working/starting, 1: blocked, 2: failed, 3: idle/done, 4: ended
		sortAt     time.Time
		workItemT  time.Time
	}

	pickStatusRank := func(status string, ended bool) int {
		if ended {
			return 4
		}
		switch status {
		case "working", "starting", "waiting":
			return 0
		case "blocked":
			return 1
		case "failed":
			return 2
		default:
			return 3
		}
	}

	seen := map[string]bool{
		model.RouterName: true,
		model.DreamName:  true,
	}
	var raw []scoredItem

	if workers, err := e.store.ListWorkers(ctx); err == nil {
		for _, w := range workers {
			if w == nil || w.Ended || w.PaneID == "" || seen[w.Name] {
				continue
			}
			seen[w.Name] = true
			wi := openWorkByWorker[w.Name]
			if wi == nil && w.WorkItemID != "" {
				wi = openWorkByID[w.WorkItemID]
			}
			title := deriveWorkerDisplayTitle(w.Name, w.Title, w.Task, wi)
			wid := w.WorkItemID
			if wi != nil {
				wid = wi.ID
			}
			sRank := pickStatusRank(w.Status, false)
			tier := 2
			var wiTime time.Time
			if wi != nil {
				tier = 0
				wiTime = wi.UpdatedAt
			} else if sRank <= 2 {
				// Active, blocked, or failed unattached live worker surfaces right after open matters.
				tier = 1
			} else if isGenericAutoWorkerName(w.Name) {
				tier = 3
			}
			sortT := w.CreatedAt
			if w.ReportAt.After(sortT) {
				sortT = w.ReportAt
			}
			raw = append(raw, scoredItem{
				item: WorkerPickItem{
					Name:        w.Name,
					Title:       title,
					Machine:     e.MachineDisplayName(workerMachineID(w)),
					Kind:        w.Kind,
					Status:      w.Status,
					Session:     w.Session,
					WorkItemID:  wid,
					HasWorkItem: wi != nil,
					Ended:       false,
				},
				tier:       tier,
				statusRank: sRank,
				sortAt:     sortT,
				workItemT:  wiTime,
			})
		}
	}

	if ended, err := e.store.ListEndedSessions(ctx, 40); err == nil {
		for _, es := range ended {
			name := strings.TrimSpace(es.Name)
			if name == "" || es.Session == "" || seen[name] {
				continue
			}
			seen[name] = true
			wi := openWorkByWorker[name]
			if wi == nil && es.WorkItemID != "" {
				wi = openWorkByID[es.WorkItemID]
			}
			title := deriveWorkerDisplayTitle(name, es.Title, "", wi)
			wid := es.WorkItemID
			if wi != nil {
				wid = wi.ID
			}
			tier := 4
			var wiTime time.Time
			if wi != nil {
				tier = 0
				wiTime = wi.UpdatedAt
			}
			raw = append(raw, scoredItem{
				item: WorkerPickItem{
					Name:        name,
					Title:       title,
					Machine:     e.MachineDisplayName(es.Machine),
					Kind:        es.Kind,
					Status:      "ended",
					Session:     es.Session,
					WorkItemID:  wid,
					HasWorkItem: wi != nil,
					Ended:       true,
				},
				tier:       tier,
				statusRank: 4,
				sortAt:     es.EndedAt,
				workItemT:  wiTime,
			})
		}
	}

	// Deduplicate open WorkItems on the first page: pick the single best primary worker per open
	// WorkItem for Tier 0 (live > ended, working/blocked/failed > idle, newest first), and demote
	// secondary workers of the same WorkItem to "更多会话" with an explicit worker suffix so WeCom /w
	// never spams duplicate identical matter titles across multiple pages.
	bestByWI := map[string]int{}
	for i, s := range raw {
		if !s.item.HasWorkItem || s.item.WorkItemID == "" {
			continue
		}
		wid := s.item.WorkItemID
		prevIdx, ok := bestByWI[wid]
		if !ok {
			bestByWI[wid] = i
			continue
		}
		prev := raw[prevIdx]
		better := false
		if s.item.Ended != prev.item.Ended {
			better = !s.item.Ended
		} else if s.statusRank != prev.statusRank {
			better = s.statusRank < prev.statusRank
		} else if !s.sortAt.Equal(prev.sortAt) {
			better = s.sortAt.After(prev.sortAt)
		}
		if better {
			bestByWI[wid] = i
		}
	}
	for i := range raw {
		if !raw[i].item.HasWorkItem || raw[i].item.WorkItemID == "" {
			continue
		}
		if bestByWI[raw[i].item.WorkItemID] != i {
			if raw[i].item.Ended {
				raw[i].tier = 4
			} else {
				raw[i].tier = 2
			}
			if !strings.Contains(raw[i].item.Title, raw[i].item.Name) {
				raw[i].item.Title = oneLineClip(raw[i].item.Title, 20) + " · " + raw[i].item.Name
			}
		} else if raw[i].item.Ended {
			// Primary worker for an open WorkItem whose pane is ended stays right behind live matters.
			raw[i].tier = 1
		}
	}

	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].tier != raw[j].tier {
			return raw[i].tier < raw[j].tier
		}
		if raw[i].tier == 0 && raw[i].statusRank != raw[j].statusRank {
			return raw[i].statusRank < raw[j].statusRank
		}
		if !raw[i].workItemT.IsZero() && !raw[j].workItemT.IsZero() && !raw[i].workItemT.Equal(raw[j].workItemT) {
			return raw[i].workItemT.After(raw[j].workItemT)
		}
		return raw[i].sortAt.After(raw[j].sortAt)
	})

	if len(raw) > limit {
		raw = raw[:limit]
	}
	out := make([]WorkerPickItem, 0, len(raw))
	for _, s := range raw {
		out = append(out, s.item)
	}
	return out
}

func isGenericAutoWorkerName(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{"codebuddy-", "agy-", "claude-", "codex-"} {
		if rest, ok := strings.CutPrefix(low, p); ok && len(rest) > 0 {
			allDigits := true
			for _, r := range rest {
				if r < '0' || r > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return true
			}
		}
	}
	return false
}

func deriveWorkerDisplayTitle(name, explicitTitle, task string, wi *model.WorkItem) string {
	if wi != nil && strings.TrimSpace(wi.Title) != "" {
		return strings.TrimSpace(wi.Title)
	}
	if t := strings.TrimSpace(explicitTitle); t != "" && !strings.EqualFold(t, name) {
		return oneLineClip(t, 40)
	}
	if extracted := summarizeWorkerTaskTitle(task); extracted != "" {
		return extracted
	}
	return name
}

func summarizeWorkerTaskTitle(task string) string {
	return cleanWorkItemDraftTitle(task)
}

// RouterTranscriptInfo locates the Hub Router's live JSONL transcript path and adapter.
// In native API Router mode there is no local PTY CLI transcript, so it returns ("", nil)
// to prevent WeCom streams from leaking stale historical PTY transcripts.
func (e *Engine) RouterTranscriptInfo(ctx context.Context) (string, agent.Adapter) {
	if e.isAPIRouterMode() || e.agents == nil || e.memory == nil {
		return "", nil
	}
	kind := agent.NormalizeKind(e.Settings().RouterKind)
	sess := ""
	e.mu.Lock()
	launchAt := e.routerLaunchAt
	if e.routerLastKind != "" {
		kind = agent.NormalizeKind(e.routerLastKind)
	}
	e.mu.Unlock()

	if e.fleet != nil {
		if ags, err := e.fleet.Agents(ctx); err == nil {
			for _, a := range ags {
				if a.Name != nil && *a.Name == model.RouterName {
					if a.Agent != nil && *a.Agent != "" {
						kind = agent.NormalizeKind(*a.Agent)
					}
					if a.Session != nil {
						sess = extractSessionID(a.Session.Kind, a.Session.Value)
					}
					break
				}
			}
		}
	}
	if kind == "" {
		kind = "claude"
	}
	ad, err := e.agents.Get(kind)
	if err != nil {
		return "", nil
	}
	p := ad.LocateTranscript(e.home, e.memory.RouterDir(), sess, launchAt)
	return p, ad
}

// WorkerTranscriptSource returns either a local Hub transcript path + adapter or the remote
// machine ID + pane ID so channel watchers can consume live thinking/tool progress.
func (e *Engine) WorkerTranscriptSource(ctx context.Context, workerName string) (paneID, machine, localPath string, adapter agent.Adapter) {
	if e.store == nil {
		return "", "", "", nil
	}
	w, err := e.store.GetWorker(ctx, strings.TrimSpace(workerName))
	if err != nil || w == nil || w.Ended || w.PaneID == "" {
		return "", "", "", nil
	}
	machine = workerMachineID(w)
	paneID = w.PaneID
	if machine != "" {
		return paneID, machine, "", nil
	}
	if e.agents == nil {
		return paneID, "", "", nil
	}
	ad, err := e.agents.Get(w.Kind)
	if err != nil {
		return paneID, "", "", nil
	}
	sess := w.Session
	if sess == "" && e.fleet != nil {
		if ags, err := e.fleet.Agents(ctx); err == nil {
			for _, a := range ags {
				if a.PaneID == w.PaneID && a.Session != nil {
					sess = extractSessionID(a.Session.Kind, a.Session.Value)
					break
				}
			}
		}
	}
	p := ad.LocateTranscript(e.home, w.Cwd, sess, w.CreatedAt)
	return paneID, "", p, ad
}

// SendDirectWorkerMessage resolves or resumes the target Worker, queues or sends the prompt
// via SubmitPanePrompt, and registers an optional callback for delivering the Worker's direct reply.
func (e *Engine) SendDirectWorkerMessage(ctx context.Context, req DirectWorkerRequest) (*model.Worker, bool, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, false, fmt.Errorf("消息内容不能为空")
	}
	w, resumed, err := e.ResumeWorkerSession(ctx, ResumeWorkerRequest{
		Name:       req.Name,
		Machine:    req.Machine,
		Session:    req.Session,
		Kind:       req.Kind,
		Cwd:        req.Cwd,
		WorkItemID: req.WorkItemID,
	})
	if err != nil {
		return nil, false, err
	}
	e.invalidateWorkerAutoVerify(w.Name, true)

	var slot *pendingDirectTurn
	if req.OnDone != nil {
		prevText, _ := e.lastWorkerFullMessage(ctx, w)
		slot = &pendingDirectTurn{
			prompt:   text,
			prevHash: hashTurnText(prevText),
			at:       time.Now(),
			callback: req.OnDone,
		}
		e.mu.Lock()
		if e.directTurns == nil {
			e.directTurns = map[string][]*pendingDirectTurn{}
		}
		e.directTurns[w.Name] = append(e.directTurns[w.Name], slot)
		e.mu.Unlock()
	}

	if _, _, err := e.SubmitPanePrompt(ctx, w.PaneID, text, "auto"); err != nil {
		if slot != nil {
			e.removeDirectTurn(w.Name, slot)
		}
		return w, resumed, err
	}
	return w, resumed, nil
}

func (e *Engine) hasDirectTurn(workerName string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.directTurns[workerName]) > 0
}

func (e *Engine) peekDirectTurn(workerName string) *pendingDirectTurn {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.directTurns[workerName]
	if len(q) == 0 {
		return nil
	}
	return q[0]
}

func (e *Engine) popDirectTurn(workerName string) *pendingDirectTurn {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.directTurns[workerName]
	if len(q) == 0 {
		return nil
	}
	dt := q[0]
	if len(q) == 1 {
		delete(e.directTurns, workerName)
	} else {
		e.directTurns[workerName] = q[1:]
	}
	return dt
}

func (e *Engine) removeDirectTurn(workerName string, target *pendingDirectTurn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.directTurns[workerName]
	for i, it := range q {
		if it == target {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(e.directTurns, workerName)
	} else {
		e.directTurns[workerName] = q
	}
}

func (e *Engine) appendDirectBgNote(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.directBgNotes = append(e.directBgNotes, note)
	if len(e.directBgNotes) > 8 {
		e.directBgNotes = e.directBgNotes[len(e.directBgNotes)-8:]
	}
}

func (e *Engine) drainDirectBgNotes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.directBgNotes) == 0 {
		return nil
	}
	out := append([]string(nil), e.directBgNotes...)
	e.directBgNotes = nil
	return out
}

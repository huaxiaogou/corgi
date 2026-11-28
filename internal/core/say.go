package core

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

var (
	quoteDirectMarkerRe       = regexp.MustCompile(`↳\s*(?:直聊中|直聊|单次直问)`)
	quoteDirectParenWorkerRe  = regexp.MustCompile(`[（(]#([a-z][a-z0-9_-]{0,31})(?:\s|·|[）)]|\.|…|$)`)
	quoteDirectInlineWorkerRe = regexp.MustCompile(`↳\s*(?:直聊中|直聊|单次直问)\s*·\s*#([a-z][a-z0-9_-]{0,31})(?:[^a-z0-9_-]|$)`)
)

// ExtractQuoteWorkerTag parses the target worker handle strictly from dedicated WeCom
// direct-chat markers ("↳ 直聊", "↳ 直聊中", "↳ 单次直问"). Legacy "[#worker]" tags on
// Corgi reports are intentionally ignored so quoting Corgi never leaks straight to a worker.
func ExtractQuoteWorkerTag(quotedText string) string {
	for _, line := range strings.Split(quotedText, "\n") {
		if !quoteDirectMarkerRe.MatchString(line) {
			continue
		}
		if m := quoteDirectParenWorkerRe.FindStringSubmatch(line); len(m) >= 2 {
			return m[1]
		}
		if m := quoteDirectInlineWorkerRe.FindStringSubmatch(line); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

// SayRequest represents an incoming human message from Web, WeCom, or CLI.
type SayRequest struct {
	Thread        string           `json:"thread"`
	Channel       string           `json:"channel"` // web | wecom | cli
	From          string           `json:"from"`
	Text          string           `json:"text"`
	ReEntryID     string           `json:"re"`
	InPlace       bool             `json:"in_place,omitempty"`
	ActionID      string           `json:"action_id,omitempty"`
	PromptContext string           `json:"-"`
	QuoteText     string           `json:"quote_text,omitempty"`
	Machine       string           `json:"machine,omitempty"`
	Kind          string           `json:"kind,omitempty"`
	Model         string           `json:"model,omitempty"`
	Effort        string           `json:"effort,omitempty"`
	ProjectID     string           `json:"project_id,omitempty"`
	Media         []model.MediaRef `json:"media,omitempty"`
	IdemKey       string           `json:"idem_key,omitempty"`
	ClientMsgID   string           `json:"client_msg_id,omitempty"`
}

// Say ingests a human message from Web, WeCom, or CLI.
// If QuoteText contains "[#worker]" and that worker is active, it routes directly to the worker.
func (e *Engine) Say(ctx context.Context, req SayRequest) (*model.Entry, *model.Trigger, error) {
	return e.sayWithSteer(ctx, req, false)
}

func normalizeSayIdemKey(req SayRequest) string {
	if k := strings.TrimSpace(req.IdemKey); k != "" {
		return k
	}
	if cid := strings.TrimSpace(req.ClientMsgID); cid != "" {
		return "client:" + cid
	}
	return ""
}

func (e *Engine) sayWithSteer(ctx context.Context, req SayRequest, steer bool) (*model.Entry, *model.Trigger, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, nil, fmt.Errorf("消息内容不能为空")
	}
	thread := strings.TrimSpace(req.Thread)
	if thread == "" {
		thread = model.MainThread
	}
	channel := req.Channel
	if channel == "" {
		channel = "web"
	}
	idemKey := normalizeSayIdemKey(req)

	var (
		quote        *model.Quote
		validInPlace bool
	)
	if req.ReEntryID != "" {
		if prev, err := e.store.GetEntry(ctx, req.ReEntryID); err == nil && prev != nil {
			who := "Corgi"
			if prev.Role == "user" {
				who = "你"
			}
			qText := oneLineClip(prev.Text, 240)
			if prev.Role == "user" && prev.ActionID != "" && prev.Re != nil && strings.TrimSpace(prev.Re.Text) != "" {
				qText = oneLineClip("拍板「"+prev.Text+"」｜原问："+prev.Re.Text, 260)
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: qText}
			prevThread := orStr(strings.TrimSpace(prev.Thread), model.MainThread)
			if channel == "web" && req.InPlace && prevThread == thread {
				validInPlace = true
			}
		}
	} else if req.QuoteText != "" {
		quote = &model.Quote{Who: "引用", Text: oneLineClip(req.QuoteText, 240)}
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return nil, nil, err
	}
	tid, tseq, err := e.store.NextID(ctx, "t")
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	entry := &model.Entry{
		ID:       eid,
		Seq:      eseq,
		Role:     "user",
		Thread:   thread,
		Channel:  channel,
		Text:     text,
		At:       now,
		Re:       quote,
		InPlace:  validInPlace,
		ActionID: strings.TrimSpace(req.ActionID),
		Media:    append([]model.MediaRef(nil), req.Media...),
	}

	// Build prompt payload enriched with quote context; one-off dispatch overrides are persisted structurally on Trigger.
	promptText := text
	if strings.TrimSpace(req.PromptContext) != "" {
		promptText = strings.TrimSpace(req.PromptContext) + "\n" + promptText
	} else if quote != nil {
		promptText = fmt.Sprintf("[In reply to: %q]\n%s", quote.Text, promptText)
	}

	// Fast-path quote routing: dedicated direct-chat markers ("↳ 直聊" / "↳ 直聊中" / "↳ 单次直问")
	// direct follow-up straight to an active worker.
	directTarget := ""
	if tag := ExtractQuoteWorkerTag(req.QuoteText); tag != "" {
		if w, err := e.store.GetWorker(ctx, tag); err == nil && !w.Ended {
			directTarget = w.Name
		}
	}

	source := "chat"
	if channel == "wecom" {
		source = "wecom"
	}
	pol := e.policyFor(source)
	decision := pol.Action
	if len(pol.Allow) > 0 && !slices.Contains(pol.Allow, req.From) {
		decision = "ignore"
	}
	trig := &model.Trigger{
		ID:       tid,
		Seq:      tseq,
		Source:   source,
		Thread:   thread,
		From:     req.From,
		Text:     promptText,
		At:       now,
		Policy:   decision,
		Status:   "queued",
		IdemKey:  idemKey,
		Ref:      entry.ID,
		Steer:    steer,
		Media:    append([]model.MediaRef(nil), req.Media...),
		Dispatch: e.buildDispatchOverride(req),
	}
	if decision == "ignore" {
		trig.Status = "ignored"
	} else if directTarget != "" {
		trig.Targets = []string{directTarget}
	}

	savedEntry, savedTrig, dup, err := e.store.CommitSayEntryAndTrigger(ctx, entry, trig)
	if err != nil {
		return nil, nil, err
	}
	if dup {
		return savedEntry, savedTrig, nil
	}

	if strings.TrimSpace(req.ActionID) == "" {
		e.autoResolvePendingOnUserReply(ctx, thread, req.ReEntryID, req.QuoteText, channel, text)
	}
	e.touchThread(ctx, thread, channel, "")
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnUserMessage(ctx, savedEntry)
	sink.OnStateChanged()
	return savedEntry, savedTrig, nil
}

// autoResolvePendingOnUserReply resolves a pending non-dialog action ONLY when the human
// explicitly quotes that action's entry (or another entry in its reply chain).
// Plain unquoted chat messages never auto-resolve unanswered pending decision cards.
func (e *Engine) autoResolvePendingOnUserReply(ctx context.Context, thread, reEntryID, _ string, channel, text string) {
	reID := strings.TrimSpace(reEntryID)
	if reID == "" {
		return
	}
	pending, err := e.store.ListPendingActions(ctx)
	if err != nil || len(pending) == 0 {
		return
	}
	var candidates []*model.Action
	for _, a := range pending {
		if a.Thread != thread || a.Kind == model.ActionKindWorkerDialog || a.Kind == model.ActionKindConfigConfirm || strings.TrimSpace(a.EntryID) == "" {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return
	}

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	label := oneLineClip(text, 120)

	for _, a := range candidates {
		if !e.entryInReplyChain(ctx, reID, a.EntryID) {
			continue
		}
		if resolved, err := e.store.ResolveAction(ctx, a.ID, channel, "reply", label); err == nil {
			sink.OnActionResolved(ctx, resolved)
		}
	}
}

// entryInReplyChain reports whether fromID equals targetID or reaches targetID by following
// Entry.Re.Entry parent pointers (bounded to 16 hops to prevent cycles).
func (e *Engine) entryInReplyChain(ctx context.Context, fromID, targetID string) bool {
	cur := strings.TrimSpace(fromID)
	target := strings.TrimSpace(targetID)
	if cur == "" || target == "" {
		return false
	}
	seen := make(map[string]bool, 4)
	for hops := 0; cur != "" && hops < 16; hops++ {
		if cur == target {
			return true
		}
		if seen[cur] {
			return false
		}
		seen[cur] = true
		en, err := e.store.GetEntry(ctx, cur)
		if err != nil || en == nil || en.Re == nil {
			return false
		}
		cur = strings.TrimSpace(en.Re.Entry)
	}
	return false
}

func (e *Engine) buildDispatchOverride(req SayRequest) *model.DispatchOverride {
	st := e.Settings()
	var ov model.DispatchOverride
	if m := strings.TrimSpace(req.Machine); m != "" {
		resolved := m
		if e.fleet != nil {
			resolved = e.fleet.ResolveMachine(m)
		}
		ov.Machine = orStr(resolved, "hub")
	}
	if k := agent.NormalizeKind(req.Kind); k != "" {
		ov.Kind = k
	}
	if m := strings.TrimSpace(req.Model); m != "" && agent.ModelNameRe.MatchString(m) {
		ov.Model = m
	}
	if ef := strings.TrimSpace(req.Effort); ef != "" && agent.ModelNameRe.MatchString(ef) {
		ov.Effort = ef
	}
	if pid := strings.TrimSpace(req.ProjectID); pid != "" {
		if proj := ResolveProject(st.Projects, pid); proj != nil {
			ov.ProjectID = proj.ID
		} else {
			ov.ProjectID = pid
		}
	}
	if ov == (model.DispatchOverride{}) {
		return nil
	}
	return &ov
}

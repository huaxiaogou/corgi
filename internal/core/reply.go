package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

// ReplyRequest describes a Corgi outbound reply with optional quick-choice buttons,
// review check, digest replacement, work item linkage, or explicit file attachments.
type ReplyRequest struct {
	Thread            string
	Title             string // optional short decision card title (≤20 chars plain text)
	Text              string
	ReID              string
	Options           []string
	StructuredChoices []model.ActionChoice
	Mode              string // single (default) | multi
	ConfirmLabel      string
	Files             []string
	Machine           string
	WorkID            string
	Check             bool
	Digest            bool
	Worker            string
	Category          string
}

// ReplyResult carries the persisted entry, optional action, warning string, and per-file delivery outcomes.
type ReplyResult struct {
	Entry    *model.Entry
	Action   *model.Action
	Warning  string
	Delivery ReplyDelivery
}

// Reply records Corgi's outbound message and creates a synchronized Action if options/check are set.
func (e *Engine) Reply(ctx context.Context, thread, text, reID string, options []string, check bool) (*model.Entry, *model.Action, string, error) {
	return e.ReplyWithMeta(ctx, ReplyRequest{
		Thread:  thread,
		Text:    text,
		ReID:    reID,
		Options: options,
		Check:   check,
	})
}

// ReplyWithMeta records Corgi's outbound message and creates a synchronized Action,
// replacing any prior pending patrol digest card when req.Digest is true.
func (e *Engine) ReplyWithMeta(ctx context.Context, req ReplyRequest) (*model.Entry, *model.Action, string, error) {
	res, err := e.ReplyDetailed(ctx, req)
	if err != nil {
		return nil, nil, "", err
	}
	return res.Entry, res.Action, res.Warning, nil
}

// ReplyDetailed validates and fetches any explicit attachments once on the selected machine before
// persisting the entry/action, passes the exact fetched bytes to ChannelSink, records confirmed
// delivery evidence on req.WorkID when set, and returns per-file delivery outcomes.
func (e *Engine) ReplyDetailed(ctx context.Context, req ReplyRequest) (*ReplyResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("empty reply")
	}
	structuredChoices, opts, err := normalizeStructuredChoices(req.StructuredChoices, req.Options)
	if err != nil {
		return nil, err
	}
	thread := req.Thread
	if thread == "" {
		thread = model.MainThread
	}
	workID := strings.TrimSpace(req.WorkID)
	if workID != "" {
		wi, wErr := e.store.GetWorkItem(ctx, workID)
		if wErr != nil {
			return nil, fmt.Errorf("invalid --work %q: %w", workID, wErr)
		}
		wiThread := wi.Thread
		if wiThread == "" {
			wiThread = model.MainThread
		}
		if wiThread != thread {
			return nil, fmt.Errorf("--work %q belongs to thread %q, not %q", workID, wiThread, thread)
		}
	}
	fetchedFiles, files, fileMachine, err := e.readReplyAttachments(ctx, req.Machine, req.Files)
	if err != nil {
		if strings.TrimSpace(req.Machine) == "" && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("validate reply attachments (checked Hub local disk; if the file lives on another machine pass --machine <M>): %w", err)
		}
		return nil, fmt.Errorf("validate reply attachments: %w", err)
	}
	hasOpts := len(opts) > 0
	wantCheck := !hasOpts && req.Check

	var (
		quote       *model.Quote
		quoteWorker string
		reEntry     *model.Entry
		reTrigger   *model.Trigger
	)
	if req.ReID != "" {
		if prev, err := e.store.GetEntry(ctx, req.ReID); err == nil {
			reEntry = prev
			who := "你"
			if prev.Role != "user" {
				who = prev.Role
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
			if prev.Role == "event" && prev.Ref != "" && prev.Ref != model.RouterName {
				quoteWorker = prev.Ref
			}
		} else if tr, err := e.store.GetTrigger(ctx, req.ReID); err == nil {
			reTrigger = tr
			quote = &model.Quote{Who: tr.From, Text: oneLineClip(tr.Text, 120)}
			if tr.Source == "worker" && tr.From != "" {
				quoteWorker = tr.From
			}
		}
	}
	// Resolve WorkItem provenance in strict order: explicit --work -> referenced Entry/Trigger -> related worker.
	if workID == "" {
		candidateWI := ""
		if reEntry != nil && strings.TrimSpace(reEntry.WorkItemID) != "" {
			candidateWI = strings.TrimSpace(reEntry.WorkItemID)
		} else if reTrigger != nil && strings.TrimSpace(reTrigger.WorkItemID) != "" {
			candidateWI = strings.TrimSpace(reTrigger.WorkItemID)
		}
		if candidateWI != "" {
			if wi, wErr := e.store.GetWorkItem(ctx, candidateWI); wErr == nil && wi.ClosedAt == nil && (wi.Thread == "" || wi.Thread == thread) {
				workID = wi.ID
			}
		}
	}

	// Resolve bound worker instance (name + machine + session) so delivery cards link directly
	// to the exact Worker that produced the report/artifact, and auto-attach its freshly captured
	// full report snapshot when Router did not pass explicit --file paths.
	relatedWorkerName := strings.TrimSpace(req.Worker)
	if relatedWorkerName == "" {
		relatedWorkerName = quoteWorker
	}
	var relatedWorker *model.Worker
	if relatedWorkerName != "" {
		if wk, wErr := e.store.GetWorker(ctx, relatedWorkerName); wErr == nil {
			relatedWorker = wk
		}
	} else if workID != "" {
		if wi, wErr := e.store.GetWorkItem(ctx, workID); wErr == nil && len(wi.Workers) > 0 {
			for i := len(wi.Workers) - 1; i >= 0; i-- {
				if wk, gErr := e.store.GetWorker(ctx, wi.Workers[i]); gErr == nil {
					relatedWorker = wk
					break
				}
			}
		}
	}
	var workerRef *model.EntryWorkerRef
	if relatedWorker != nil {
		if workID == "" && relatedWorker.WorkItemID != "" {
			if wi, wErr := e.store.GetWorkItem(ctx, relatedWorker.WorkItemID); wErr == nil && wi.ClosedAt == nil && (wi.Thread == "" || wi.Thread == thread) {
				workID = wi.ID
			}
		}
		workerRef = &model.EntryWorkerRef{
			Name:       relatedWorker.Name,
			Machine:    relatedWorker.Machine,
			Session:    relatedWorker.Session,
			Kind:       relatedWorker.Kind,
			Model:      relatedWorker.Model,
			Effort:     relatedWorker.Effort,
			WorkItemID: relatedWorker.WorkItemID,
		}
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	artifactRecs, artifactSummaries, cleanupBlobs, err := e.stageArtifacts(ctx, eid, thread, workID, fetchedFiles, now)
	if err != nil {
		return nil, fmt.Errorf("stage reply artifacts: %w", err)
	}

	// Chain of Recall: auto-attach a captured report snapshot ONLY when `--re <report_trigger_id>`
	// explicitly references a same-thread worker report trigger carrying an ArtifactID (and no
	// explicit `--file` was provided). Plain `--worker W` binds WorkerRef / card metadata only so
	// routine status confirmations never leak an older report snapshot into file-delivery flows.
	var autoAttachedReportID string
	var targetReportArtifactID string
	if len(fetchedFiles) == 0 && reTrigger != nil && reTrigger.Source == "worker" &&
		strings.TrimSpace(reTrigger.ArtifactID) != "" &&
		(reTrigger.Thread == "" || reTrigger.Thread == thread) &&
		(strings.TrimSpace(req.Worker) == "" || strings.EqualFold(strings.TrimSpace(req.Worker), strings.TrimSpace(reTrigger.From))) {
		targetReportArtifactID = strings.TrimSpace(reTrigger.ArtifactID)
	}
	if targetReportArtifactID != "" {
		if art, data, rErr := e.ReadArtifactForConsole(ctx, targetReportArtifactID); rErr == nil {
			autoAttachedReportID = art.ID
			artifactSummaries = append(artifactSummaries, model.ArtifactSummary{
				ID:   art.ID,
				Name: art.Name,
				Kind: art.Kind,
				Size: art.Size,
			})
			fetchedFiles = append(fetchedFiles, AttachmentFile{
				ArtifactID: art.ID,
				Path:       art.Name,
				Machine:    art.SourceMachine,
				Name:       art.Name,
				Kind:       art.Kind,
				ShareURL:   e.BuildPreviewURL(ctx, art.ID),
				Data:       data,
			})
			unlinkedEntry := art.EntryID == "" || strings.HasPrefix(art.EntryID, "standalone:")
			if unlinkedEntry || (workID != "" && art.WorkItemID == "") {
				if unlinkedEntry {
					art.EntryID = eid
				}
				if workID != "" && art.WorkItemID == "" {
					art.WorkItemID = workID
				}
				_ = e.store.InsertArtifact(ctx, art)
			}
		}
	}

	hasResolvedAttachment := len(fetchedFiles) > 0 || len(artifactSummaries) > 0 || len(files) > 0
	if !hasResolvedAttachment && claimsAttachmentIncluded(text) {
		cleanupBlobs()
		return nil, fmt.Errorf("reply claims an attachment is included, but 0 files or report snapshots were attached; pass --file /abs/path [--machine M] or --re <report_tID> --worker <W>, or rephrase without claiming an attachment")
	}

	entry := &model.Entry{
		ID:          eid,
		Seq:         eseq,
		Role:        "corgi",
		Thread:      thread,
		Channel:     "corgi",
		Text:        text,
		At:          now,
		Re:          quote,
		Options:     opts,
		Files:       files,
		FileMachine: fileMachine,
		Artifacts:   artifactSummaries,
		WorkerRef:   workerRef,
		WorkItemID:  workID,
		Check:       req.Check,
	}

	var action *model.Action
	var expireDigestKind string
	if hasOpts || wantCheck {
		aid, _, err := e.store.NextID(ctx, "a")
		if err != nil {
			cleanupBlobs()
			return nil, err
		}
		kind := model.ActionKindRouterChoice
		var choices []model.ActionChoice
		actionMode := ""
		if hasOpts {
			choices = structuredChoices
			if strings.EqualFold(strings.TrimSpace(req.Mode), model.ActionModeMulti) {
				actionMode = model.ActionModeMulti
			}
			if req.Digest {
				kind = model.ActionKindPatrolDigest
				expireDigestKind = model.ActionKindPatrolDigest
			}
		} else {
			kind = model.ActionKindRouterCheck
			if req.Digest {
				expireDigestKind = model.ActionKindPatrolDigest
			}
		}
		cat := strings.TrimSpace(req.Category)
		if cat == "" {
			switch {
			case req.Digest:
				cat = "patrol"
			case wantCheck:
				cat = "check"
			default:
				cat = "decision"
			}
		}
		confirmLabel := strings.TrimSpace(req.ConfirmLabel)
		if len([]rune(confirmLabel)) > 16 {
			confirmLabel = oneLineClip(confirmLabel, 16)
		}
		actionTitle := oneLineClip(strings.TrimSpace(req.Title), 64)
		if actionTitle == "" {
			actionTitle = oneLineClip(text, 64)
		}
		action = &model.Action{
			ID:           aid,
			Kind:         kind,
			Thread:       thread,
			EntryID:      entry.ID,
			WorkItemID:   workID,
			TargetWorker: strings.TrimSpace(req.Worker),
			Category:     cat,
			Mode:         actionMode,
			ConfirmLabel: confirmLabel,
			Title:        actionTitle,
			Summary:      text,
			Choices:      choices,
			Status:       model.ActionPending,
			CreatedAt:    now,
		}
		entry.ActionID = aid
	}

	if err := e.store.CommitReplyTx(ctx, entry, action, expireDigestKind, artifactRecs); err != nil {
		cleanupBlobs()
		return nil, err
	}
	if autoAttachedReportID != "" && relatedWorker != nil {
		relatedWorker.LastReportAttachedID = autoAttachedReportID
		_ = e.store.UpsertWorker(ctx, relatedWorker)
	}

	e.mu.Lock()
	limit := e.settings.ReplyChars
	sink := e.sink
	e.mu.Unlock()

	// Publish committed entry & Hub snapshots to Web SSE immediately before slow channel I/O.
	sink.OnStateChanged()

	var warns []string
	if !hasOpts && !wantCheck && req.Digest {
		warns = append(warns, "ignored --digest without --opt or --check (sent as plain reply; pass 2-4 --opt buttons for a decision or --check for a lightweight review reminder)")
	}
	if len(req.Files) == 0 {
		if p := mentionsDeliverablePath(text); p != "" {
			warns = append(warns, fmt.Sprintf("reply mentions %s without --file (pass --file %q [--machine M] to publish Hub preview link & WeCom attachment)", p, p))
		}
	}
	if limit > 0 {
		if n := ReadableReplyLen(text); n > limit {
			warns = append(warns, fmt.Sprintf("reply is %d units (usual budget ≤%d; ok if detail or list was requested)", n, limit))
		}
	}
	delivery := sink.OnReply(ctx, entry, action, fetchedFiles)
	if delivery.LinkStatus != "" && delivery.LinkStatus != "delivered" {
		detail := delivery.LinkStatus
		if delivery.LinkError != "" {
			detail += ": " + delivery.LinkError
		}
		warns = append(warns, "wecom preview link "+detail)
	}
	for _, fd := range delivery.Files {
		if fd.Status == "delivered" && workID != "" {
			_, _ = e.store.RecordWorkItemArtifact(ctx, workID, model.WorkArtifact{
				ArtifactID:  fd.ArtifactID,
				Name:        fd.Name,
				Kind:        fd.Kind,
				Size:        int64(len(findFetchedBytes(fetchedFiles, fd.ArtifactID))),
				Path:        fd.Path,
				Machine:     fd.Machine,
				EntryID:     entry.ID,
				DeliveredAt: time.Now(),
			})
		} else if fd.Status != "delivered" {
			detail := fd.Status
			if fd.Error != "" {
				detail += ": " + fd.Error
			}
			warns = append(warns, fmt.Sprintf("attachment %s %s", fd.Path, detail))
		}
	}
	sink.OnStateChanged()
	if len(artifactRecs) > 0 {
		go e.SweepArtifacts(context.Background())
	}
	return &ReplyResult{
		Entry:    entry,
		Action:   action,
		Warning:  strings.Join(warns, "; "),
		Delivery: delivery,
	}, nil
}

var (
	inlineCodeSpanRe   = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
	attachmentClaimRe  = regexp.MustCompile(`(?i)(已重新附上|已附上|已作为附件|作为附件(?:补发|发出|发送|交付|附上)|附件已(?:发|附上|补发|送达)|附在附件|详见附件|见附件|请查收附件|已附带(?:完整|文件|报告|方案|原型)|attached (?:below|here|the file|the report)|see attached|file attached)`)
	attachmentNegateRe = regexp.MustCompile(`(?i)(未附上|没附上|没有附上|未能附上|暂未附上|无法附上|未附带|无附件|漏发|没发出来|未发送附件|不用附|无需附|需要我|需要附|要不要|是否|如需|吗\s*$|not attached|didn'?t attach|no attachment|without attachment|\?\s*$|？\s*$)`)
	clauseSplitterRe   = regexp.MustCompile(`[。！!；;\r\n]+`)
)

// claimsAttachmentIncluded reports whether reply prose affirmatively promises that a file or
// report attachment is included in THIS reply (ignoring code spans, negations, and questions).
func claimsAttachmentIncluded(text string) bool {
	cleaned := inlineCodeSpanRe.ReplaceAllString(text, " ")
	for _, clause := range clauseSplitterRe.Split(cleaned, -1) {
		c := strings.TrimSpace(clause)
		if c == "" {
			continue
		}
		if attachmentNegateRe.MatchString(c) {
			continue
		}
		if attachmentClaimRe.MatchString(c) {
			return true
		}
	}
	return false
}

const maxOptionRunes = 240

func cleanOptions(raw []string) ([]string, error) {
	var out []string
	for i, s := range raw {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			continue
		}
		if n := len([]rune(s)); n > maxOptionRunes {
			return nil, fmt.Errorf("option %d exceeds %d characters (%d)", i+1, maxOptionRunes, n)
		}
		if !slices.Contains(out, s) {
			if len(out) >= maxActionOptions {
				return nil, fmt.Errorf("too many options (max %d supported without truncation)", maxActionOptions)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

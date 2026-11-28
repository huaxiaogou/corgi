package channel

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

func (b *Bus) sendACKMarkdown(ctx context.Context, chatID, markdown string) (status, errMsg string) {
	send := b.sendMarkdownACK
	if send == nil {
		send = b.sendFn
	}
	if send == nil || (b.wecom != nil && !b.wecom.Online()) {
		return "skipped", "wecom offline or not configured"
	}
	if err := send(ctx, chatID, markdown); err != nil {
		b.logger.Warn("send wecom preview link message failed", "chat_id", chatID, "err", err)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "unknown", err.Error()
		}
		return "failed", err.Error()
	}
	return "delivered", ""
}

func escapeWeComLinkLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `(`, `\(`, `)`, `\)`, "\r", "", "\n", " ").Replace(strings.TrimSpace(s))
}

func formatPreviewLinksBlock(attachments []core.AttachmentFile) string {
	var lines []string
	for _, att := range attachments {
		if strings.TrimSpace(att.ShareURL) == "" {
			continue
		}
		label := escapeWeComLinkLabel(att.Name)
		if label == "" {
			label = "预览文件"
		}
		lines = append(lines, "🔗 [在线预览 · "+label+"]("+att.ShareURL+")")
	}
	return strings.Join(lines, "\n")
}

func (b *Bus) OnReply(ctx context.Context, entry *model.Entry, action *model.Action, attachments []core.AttachmentFile) core.ReplyDelivery {
	chatID := threadToWeComChat(entry.Thread)
	now := time.Now()
	suppress, wecomRecent := b.wecomDeliveryState(entry.Thread, now)
	hasExplicitFiles := len(attachments) > 0 || len(entry.Files) > 0
	linksBlock := formatPreviewLinksBlock(attachments)

	// Resolve recipient ONCE across the entire reply when delivering explicit files so link + source files never split targets.
	if hasExplicitFiles {
		if resolved := b.resolveWeComChat(chatID); resolved != "" {
			chatID = resolved
		}
	}

	if suppress {
		b.clearPendingWebQuote(entry.Thread)
		b.clearPendingFocusNotice(entry.Thread)
		if !hasExplicitFiles {
			return core.ReplyDelivery{}
		}
		// Explicit --file deliveries bypass quiet-mode Web suppression: keep prose/card quiet,
		// send 1 compact links-only message (when configured) and all original source files.
		var del core.ReplyDelivery
		if len(attachments) > 0 {
			if linksBlock != "" {
				del.LinkStatus, del.LinkError = b.sendACKMarkdown(ctx, chatID, linksBlock)
			} else {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
		return del
	}

	// Real decision cards (--opt / worker_dialog) bypass Outbox batching and push immediately when not suppressed.
	// Lightweight `--check` review reminders (router_check) are Web-only reminders and flow through
	// normal plain-text WeCom delivery below (never cardified and never sealed with "请在下方卡片选择 👇").
	if action != nil && action.IsDecision() {
		b.clearPendingWebQuote(entry.Thread)
		focusNotice := b.takePendingFocusNotice(entry.Thread)
		st := b.takeRouterStreamForReply(entry.Thread, entry)
		if st != nil && st.chatID != "" && chatID == "" {
			chatID = st.chatID
		}
		cardPushed := b.pushWeComDecisionCard(ctx, entry.Thread, chatID, action, false)
		if st != nil {
			if cardPushed {
				sealText := "请在下方卡片选择 👇"
				if focusNotice != "" {
					sealText = focusNotice + "\n\n" + sealText
				}
				_, _ = st.seal(ctx, "", sealText)
			} else {
				// Fall back to readable reply prose if card push failed or WeCom went offline.
				_, _ = st.seal(ctx, "", prependReplyHeaders("", focusNotice, formatWeComReply(entry, b.effectivePublicURL())))
			}
		}
		if hasExplicitFiles {
			var del core.ReplyDelivery
			if len(attachments) > 0 {
				if linksBlock != "" {
					del.LinkStatus, del.LinkError = b.sendACKMarkdown(ctx, chatID, linksBlock)
				} else {
					del.LinkStatus = "skipped"
					del.LinkError = "preview link skipped: configure public_url or --url"
				}
			}
			del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
			return del
		}
		return core.ReplyDelivery{}
	}

	webQuote := b.takePendingWebQuote(entry.Thread)
	focusNotice := b.takePendingFocusNotice(entry.Thread)

	// Per-recipient causal FIFO Delivery Gate:
	// Take any open Router stream up front so canonical recipient chatID is unified across
	// streams, immediate replies, file deliveries, and outbox retries.
	st := b.takeRouterStreamForReply(entry.Thread, entry)
	streamChatID := ""
	if st != nil {
		streamChatID = st.chatID
	}
	chatID = b.canonicalWeComChat(entry.Thread, streamChatID)

	body := prependReplyHeaders(webQuote, focusNotice, formatWeComReply(entry, b.effectivePublicURL()))
	combined := body
	if hasExplicitFiles && linksBlock != "" {
		if combined != "" {
			combined = combined + "\n\n" + linksBlock
		} else {
			combined = linksBlock
		}
	}

	item := outboxItem{
		text:          combined,
		at:            now,
		immediate:     hasExplicitFiles || st != nil || wecomRecent || webQuote != "" || focusNotice != "",
		useACK:        hasExplicitFiles && linksBlock != "",
		hasFiles:      hasExplicitFiles,
		entry:         entry,
		attachments:   attachments,
		allowLongFile: !hasExplicitFiles,
	}

	// Non-immediate background replies (no active stream, not recent WeCom, no Web quote, no explicit files)
	// enqueue into the paced 20s batching outbox even during a brief WS reconnect so they are never dropped.
	if !item.immediate {
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxTail(chatID, item)
		}
		return core.ReplyDelivery{
			Files: b.sendReplyAttachments(ctx, chatID, entry, attachments, true),
		}
	}

	// Serialize all immediate deliveries for the same WeCom recipient so an older reply queued during
	// a transient WS disconnect ALWAYS lands before a newer reply can seal or send.
	cLock := b.chatSendLock(chatID)
	cLock.Lock()
	defer cLock.Unlock()

	// If older replies are already queued for this recipient and WS is online now, drain them FIRST
	// so this reply can still cleanly seal its own passive stream in strict causal order.
	if b.hasOutboxFor(chatID) && b.canSendMarkdown() {
		_ = b.drainChatOutboxLocked(ctx, chatID, true)
	}

	// If older unsent replies remain ahead of us OR transport is currently offline, preserve strict
	// FIFO order by queuing behind them (never sealing a newer stream out of order and never dropping).
	if b.hasOutboxFor(chatID) || !b.wecomTransportOnline() {
		if st != nil {
			st.abort()
		}
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxTail(chatID, item)
		}
		return queuedReplyDelivery(entry, attachments, linksBlock)
	}

	// Fast path: recipient FIFO is empty and transport is online.
	var del core.ReplyDelivery
	streamSealed := false
	if st != nil {
		if ok, err := st.seal(ctx, "", combined); err != nil {
			b.logger.Warn("seal wecom router stream failed", "thread", entry.Thread, "has_files", hasExplicitFiles, "err", err)
		} else if ok {
			streamSealed = true
		}
	}

	if streamSealed {
		if hasExplicitFiles && len(attachments) > 0 {
			if linksBlock != "" {
				del.LinkStatus = "delivered"
			} else {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, !hasExplicitFiles)
		return del
	}

	// Stream was absent or seal failed (e.g. WS dropped right before write).
	if !b.canSendMarkdown() {
		if b.sendFn != nil || b.wecom != nil {
			b.enqueueOutboxHead(chatID, item)
		}
		return queuedReplyDelivery(entry, attachments, linksBlock)
	}

	if hasExplicitFiles {
		if linksBlock != "" {
			status, errMsg := b.sendACKMarkdown(ctx, chatID, combined)
			del.LinkStatus, del.LinkError = status, errMsg
			if status == "failed" || status == "skipped" {
				// Pre-write / disconnect failure: hold whole item at head of FIFO for reconnect retry.
				b.enqueueOutboxHead(chatID, item)
				del.Files = queuedAttachmentResults(attachments, "queued for retry after wecom reconnect")
				return del
			}
			// status == "delivered" or "unknown": do NOT replay body; send attachments once.
		} else {
			if len(attachments) > 0 {
				del.LinkStatus = "skipped"
				del.LinkError = "preview link skipped: configure public_url or --url"
			}
			if body != "" {
				status, _ := b.sendACKMarkdown(ctx, chatID, body)
				if status == "failed" || status == "skipped" {
					b.enqueueOutboxHead(chatID, item)
					del.Files = queuedAttachmentResults(attachments, "queued for retry after wecom reconnect")
					return del
				}
			}
		}
		del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, false)
		return del
	}

	if body != "" {
		if err := b.sendFn(ctx, chatID, body); err != nil {
			b.logger.Warn("immediate wecom reply failed", "chat_id", chatID, "err", err)
			if !isUnknownDeliveryErr(err) {
				b.enqueueOutboxHead(chatID, item)
				return del
			}
		}
	}
	del.Files = b.sendReplyAttachments(ctx, chatID, entry, attachments, true)
	return del
}

func queuedReplyDelivery(entry *model.Entry, attachments []core.AttachmentFile, linksBlock string) core.ReplyDelivery {
	var del core.ReplyDelivery
	if len(attachments) > 0 {
		if linksBlock != "" {
			del.LinkStatus = "queued"
			del.LinkError = "wecom reconnecting; queued in FIFO outbox"
		} else {
			del.LinkStatus = "skipped"
			del.LinkError = "preview link skipped: configure public_url or --url"
		}
		del.Files = queuedAttachmentResults(attachments, "wecom reconnecting; queued in FIFO outbox")
	}
	return del
}

func queuedAttachmentResults(attachments []core.AttachmentFile, reason string) []core.AttachmentDelivery {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]core.AttachmentDelivery, 0, len(attachments))
	for _, att := range attachments {
		out = append(out, core.AttachmentDelivery{
			ArtifactID: att.ArtifactID,
			Path:       att.Path,
			Machine:    att.Machine,
			Name:       att.Name,
			Kind:       att.Kind,
			PreviewURL: att.ShareURL,
			Status:     "queued",
			Error:      reason,
		})
	}
	return out
}

func (b *Bus) sendReplyAttachments(ctx context.Context, chatID string, entry *model.Entry, attachments []core.AttachmentFile, allowLongFallback bool) []core.AttachmentDelivery {
	if entry == nil {
		return nil
	}
	wecomReady := b.sendFileFn != nil && (b.wecom == nil || b.wecom.Online())

	// 1. Pre-fetched attachments validated once by Engine.ReplyDetailed (supports Hub & remote nodes).
	if len(attachments) > 0 {
		out := make([]core.AttachmentDelivery, 0, len(attachments))
		for _, att := range attachments {
			res := core.AttachmentDelivery{
				ArtifactID: att.ArtifactID,
				Path:       att.Path,
				Machine:    att.Machine,
				Name:       att.Name,
				Kind:       att.Kind,
				PreviewURL: att.ShareURL,
			}
			if !wecomReady {
				res.Status = "skipped"
				res.Error = "wecom offline or not configured"
				out = append(out, res)
				continue
			}
			if err := b.sendFileFn(ctx, chatID, att.Name, att.Data); err != nil {
				b.logger.Warn("push reply attachment to wecom failed", "path", att.Path, "machine", att.Machine, "err", err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					res.Status = "unknown"
				} else {
					res.Status = "failed"
				}
				res.Error = err.Error()
			} else {
				res.Status = "delivered"
			}
			out = append(out, res)
		}
		return out
	}

	// 2. Fallback for direct Bus.OnReply callers passing entry.Files without pre-fetched bytes (local Hub disk only).
	if len(entry.Files) > 0 {
		out := make([]core.AttachmentDelivery, 0, len(entry.Files))
		for _, rawPath := range entry.Files {
			res := core.AttachmentDelivery{Path: rawPath, Machine: entry.FileMachine}
			if !wecomReady {
				res.Status = "skipped"
				res.Error = "wecom offline or not configured"
				out = append(out, res)
				continue
			}
			name, data, cleanPath, err := core.ReadAttachmentFile(rawPath)
			if err != nil {
				b.logger.Warn("skip invalid reply attachment", "path", rawPath, "err", err)
				res.Status = "failed"
				res.Error = err.Error()
				out = append(out, res)
				continue
			}
			res.Path = cleanPath
			res.Name = name
			if err := b.sendFileFn(ctx, chatID, name, data); err != nil {
				b.logger.Warn("push reply attachment to wecom failed", "path", cleanPath, "err", err)
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					res.Status = "unknown"
				} else {
					res.Status = "failed"
				}
				res.Error = err.Error()
			} else {
				res.Status = "delivered"
			}
			out = append(out, res)
		}
		return out
	}

	// 3. If plain reply body itself is very long (>1800 runes), attach as an offline-readable .md file.
	if allowLongFallback && wecomReady && len([]rune(entry.Text)) > 1800 {
		name := "corgi-report-" + time.Now().Format("150405") + ".md"
		if err := b.sendFileFn(ctx, chatID, name, []byte(entry.Text)); err != nil {
			b.logger.Warn("push long reply attachment to wecom failed", "err", err)
		}
	}
	return nil
}

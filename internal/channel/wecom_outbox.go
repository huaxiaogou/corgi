package channel

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/model"
)

type outboxItem struct {
	text          string
	at            time.Time
	attempts      int
	immediate     bool
	useACK        bool
	hasFiles      bool
	entry         *model.Entry
	attachments   []core.AttachmentFile
	allowLongFile bool
}

func (b *Bus) hasOutboxFor(chatID string) bool {
	b.mu.Lock()
	n := len(b.outbox[chatID])
	b.mu.Unlock()
	return n > 0
}

func (b *Bus) enqueueOutboxTail(chatID string, item outboxItem) {
	b.mu.Lock()
	if b.outbox == nil {
		b.outbox = map[string][]outboxItem{}
	}
	b.outbox[chatID] = append(b.outbox[chatID], item)
	b.mu.Unlock()
}

func (b *Bus) enqueueOutboxHead(chatID string, items ...outboxItem) {
	if len(items) == 0 {
		return
	}
	b.mu.Lock()
	if b.outbox == nil {
		b.outbox = map[string][]outboxItem{}
	}
	b.outbox[chatID] = append(items, b.outbox[chatID]...)
	b.mu.Unlock()
}

func isUnknownDeliveryErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// FlushDue delivers coalesced Outbox messages whose buffer window has elapsed (or all when force=true).
// Calls are serialized via flushMu so a pre-restart flush always waits for any in-flight periodic send.
func (b *Bus) FlushDue(ctx context.Context, force bool) {
	_ = b.flushInternal(ctx, force)
}

// FlushBeforeRestart waits for any in-flight outbox send to finish and flushes all pending batches.
func (b *Bus) FlushBeforeRestart(ctx context.Context) error {
	return b.flushInternal(ctx, true)
}

func (b *Bus) flushInternal(ctx context.Context, force bool) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	if !b.canSendMarkdown() {
		return nil
	}

	b.mu.Lock()
	chatIDs := make([]string, 0, len(b.outbox))
	for chatID, items := range b.outbox {
		if len(items) == 0 {
			delete(b.outbox, chatID)
			continue
		}
		chatIDs = append(chatIDs, chatID)
	}
	b.mu.Unlock()

	var firstErr error
	for _, chatID := range chatIDs {
		cLock := b.chatSendLock(chatID)
		cLock.Lock()
		err := b.drainChatOutboxLocked(ctx, chatID, force)
		cLock.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// drainChatOutboxLocked drains due FIFO outbox entries for a single recipient.
// Caller must hold b.chatSendLock(chatID) and MUST NOT hold b.mu.
func (b *Bus) drainChatOutboxLocked(ctx context.Context, chatID string, force bool) error {
	now := time.Now()
	for {
		if !b.canSendMarkdown() {
			return nil
		}
		b.mu.Lock()
		items := b.outbox[chatID]
		if len(items) == 0 {
			delete(b.outbox, chatID)
			b.mu.Unlock()
			return nil
		}
		head := items[0]
		if !force && !head.immediate && now.Sub(head.at) < b.flushDelay {
			b.mu.Unlock()
			return nil
		}

		// Coalesce consecutive plain paced items into a single markdown batch (legacy behavior);
		// send immediate / file / recovered-stream items strictly one-by-one in FIFO order.
		var batch []outboxItem
		if !head.immediate && !head.hasFiles && !head.useACK {
			cut := 1
			for cut < len(items) && !items[cut].immediate && !items[cut].hasFiles && !items[cut].useACK {
				cut++
			}
			batch = append([]outboxItem(nil), items[:cut]...)
			items = items[cut:]
		} else {
			batch = []outboxItem{head}
			items = items[1:]
		}
		if len(items) == 0 {
			delete(b.outbox, chatID)
		} else {
			b.outbox[chatID] = items
		}
		send := b.sendFn
		b.mu.Unlock()

		if len(batch) == 1 && (batch[0].immediate || batch[0].hasFiles || batch[0].useACK) {
			it := batch[0]
			var sendErr error
			if strings.TrimSpace(it.text) != "" {
				if it.useACK || it.hasFiles {
					status, errMsg := b.sendACKMarkdown(ctx, chatID, it.text)
					if status == "failed" || status == "skipped" {
						sendErr = errors.New(errMsg)
					}
					// status == "delivered" or "unknown": never replay already-written body.
				} else {
					sendErr = send(ctx, chatID, it.text)
				}
			}
			if sendErr != nil {
				b.logger.Warn("flush immediate wecom reply failed", "chat_id", chatID, "err", sendErr)
				if !isUnknownDeliveryErr(sendErr) {
					if !b.canSendMarkdown() {
						// Offline mid-flush: keep at head without burning retry budget.
						b.enqueueOutboxHead(chatID, it)
						return sendErr
					}
					if it.attempts < 3 {
						it.attempts++
						b.enqueueOutboxHead(chatID, it)
					}
				}
				return sendErr
			}
			if it.entry != nil && (it.hasFiles || it.allowLongFile) {
				_ = b.sendReplyAttachments(ctx, chatID, it.entry, it.attachments, it.allowLongFile)
			}
			continue
		}

		parts := make([]string, 0, len(batch))
		for _, it := range batch {
			if t := strings.TrimSpace(it.text); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if err := send(ctx, chatID, strings.Join(parts, "\n\n---\n\n")); err != nil {
			b.logger.Warn("flush outbox markdown failed", "chat_id", chatID, "err", err)
			if !isUnknownDeliveryErr(err) {
				if !b.canSendMarkdown() {
					b.enqueueOutboxHead(chatID, batch...)
					return err
				}
				var retryable []outboxItem
				for _, it := range batch {
					if it.attempts < 1 {
						it.attempts++
						retryable = append(retryable, it)
					}
				}
				if len(retryable) > 0 {
					b.enqueueOutboxHead(chatID, retryable...)
				}
			}
			return err
		}
	}
}

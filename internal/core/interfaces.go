package core

import (
	"context"

	"github.com/kiperjing/corgi/internal/model"
)

// AttachmentFile holds a pre-validated, pre-fetched file ready for outbound channel delivery.
type AttachmentFile struct {
	Path    string
	Machine string
	Name    string
	Data    []byte
}

// AttachmentDelivery reports the per-file delivery outcome back to `corgi reply` callers.
type AttachmentDelivery struct {
	Path    string `json:"path"`
	Machine string `json:"machine,omitempty"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status"` // delivered | failed | skipped | unknown
	Error   string `json:"error,omitempty"`
}

// ReplyDelivery summarizes outbound channel delivery outcomes for an Engine reply.
type ReplyDelivery struct {
	Files []AttachmentDelivery `json:"files,omitempty"`
}

// ChannelSink receives outbound notifications from the Super-Agent core without
// coupling internal/core to specific channel implementations (Web SSE, WeCom WSS).
type ChannelSink interface {
	OnStateChanged()
	OnUserMessage(ctx context.Context, entry *model.Entry)
	OnReply(ctx context.Context, entry *model.Entry, action *model.Action, attachments []AttachmentFile) ReplyDelivery
	OnActionCreated(ctx context.Context, action *model.Action)
	OnActionResolved(ctx context.Context, action *model.Action)
	WeComOnline() bool
}

// NopSink is a safe default ChannelSink used in tests or headless runs.
type NopSink struct{}

func (NopSink) OnStateChanged()                             {}
func (NopSink) OnUserMessage(context.Context, *model.Entry) {}
func (NopSink) OnReply(context.Context, *model.Entry, *model.Action, []AttachmentFile) ReplyDelivery {
	return ReplyDelivery{}
}
func (NopSink) OnActionCreated(context.Context, *model.Action)  {}
func (NopSink) OnActionResolved(context.Context, *model.Action) {}
func (NopSink) WeComOnline() bool                               { return false }

package core

import (
	"context"

	"github.com/kiperjing/corgi/internal/model"
)

// AttachmentFile holds a pre-validated, pre-fetched file ready for outbound channel delivery.
type AttachmentFile struct {
	ArtifactID string
	Path       string
	Machine    string
	Name       string
	Kind       string
	ShareURL   string
	Data       []byte
}

// AttachmentDelivery reports the per-file delivery outcome back to `corgi reply` callers.
type AttachmentDelivery struct {
	ArtifactID string `json:"artifact_id,omitempty"`
	Path       string `json:"path"`
	Machine    string `json:"machine,omitempty"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
	Status     string `json:"status"` // delivered | failed | skipped | unknown
	Error      string `json:"error,omitempty"`
}

// ReplyDelivery summarizes outbound channel delivery outcomes for an Engine reply.
type ReplyDelivery struct {
	LinkStatus string               `json:"link_status,omitempty"` // delivered | failed | skipped | unknown
	LinkError  string               `json:"link_error,omitempty"`
	Files      []AttachmentDelivery `json:"files,omitempty"`
}

// AutoUpdater triggers an idle overnight fleet upgrade without coupling internal/core to web.
type AutoUpdater interface {
	TryAutoUpdate(ctx context.Context)
}

// ChannelSink receives outbound notifications from the Super-Agent core without
// coupling internal/core to specific channel implementations (Web SSE, WeCom WSS).
type ChannelSink interface {
	OnStateChanged()
	OnUserMessage(ctx context.Context, entry *model.Entry)
	OnReply(ctx context.Context, entry *model.Entry, action *model.Action, attachments []AttachmentFile) ReplyDelivery
	OnActionCreated(ctx context.Context, action *model.Action)
	OnActionResolved(ctx context.Context, action *model.Action)
	OnAPIRouterProgress(ctx context.Context, thread, triggerID, think string)
	OnAPIRouterLive(ctx context.Context, live model.RouterLiveView)
	OnAPIRouterTurnEnd(ctx context.Context, thread, triggerID, reason string)
	WeComOnline() bool
}

// NopSink is a safe default ChannelSink used in tests or headless runs.
type NopSink struct{}

func (NopSink) OnStateChanged()                             {}
func (NopSink) OnUserMessage(context.Context, *model.Entry) {}
func (NopSink) OnReply(context.Context, *model.Entry, *model.Action, []AttachmentFile) ReplyDelivery {
	return ReplyDelivery{}
}
func (NopSink) OnActionCreated(context.Context, *model.Action)              {}
func (NopSink) OnActionResolved(context.Context, *model.Action)             {}
func (NopSink) OnAPIRouterProgress(context.Context, string, string, string) {}
func (NopSink) OnAPIRouterLive(context.Context, model.RouterLiveView)       {}
func (NopSink) OnAPIRouterTurnEnd(context.Context, string, string, string)  {}
func (NopSink) WeComOnline() bool                                           { return false }

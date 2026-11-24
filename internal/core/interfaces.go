package core

import (
	"context"

	"github.com/kiperjing/corgi/internal/model"
)

// ChannelSink receives outbound notifications from the Super-Agent core without
// coupling internal/core to specific channel implementations (Web SSE, WeCom WSS).
type ChannelSink interface {
	OnStateChanged()
	OnUserMessage(ctx context.Context, entry *model.Entry)
	OnReply(ctx context.Context, entry *model.Entry, action *model.Action)
	OnActionCreated(ctx context.Context, action *model.Action)
	OnActionResolved(ctx context.Context, action *model.Action)
	WeComOnline() bool
}

// NopSink is a safe default ChannelSink used in tests or headless runs.
type NopSink struct{}

func (NopSink) OnStateChanged()                                      {}
func (NopSink) OnUserMessage(context.Context, *model.Entry)          {}
func (NopSink) OnReply(context.Context, *model.Entry, *model.Action) {}
func (NopSink) OnActionCreated(context.Context, *model.Action)       {}
func (NopSink) OnActionResolved(context.Context, *model.Action)      {}
func (NopSink) WeComOnline() bool                                    { return false }

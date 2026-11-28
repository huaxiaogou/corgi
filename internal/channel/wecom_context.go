package channel

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiperjing/corgi/internal/core"
)

func parseWeComContextCommand(text string) (action string, ok bool) {
	s := strings.TrimSpace(text)
	low := strings.ToLower(s)
	switch low {
	case "/ctx", "上下文", "查看上下文":
		return "inspect", true
	case "/ctx clear", "/new", "清空上下文", "新会话":
		return "clear", true
	case "/ctx compact", "压缩上下文":
		return "compact", true
	}
	return "", false
}

func formatTokensCompact(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000.0)
	}
	return fmt.Sprintf("%d", n)
}

func (b *Bus) handleWeComContextCommand(ctx context.Context, chatID, reqID, thread, text string) bool {
	if b.engine == nil {
		return false
	}
	action, ok := parseWeComContextCommand(text)
	if !ok {
		return false
	}
	switch action {
	case "inspect":
		info, err := b.engine.InspectRouterContext(ctx, thread)
		if err != nil {
			b.sendInstantWeComText(ctx, chatID, reqID, "读取大脑上下文状态失败："+err.Error())
			return true
		}
		sum := info.Summary
		used := sum.EstTokens
		if sum.LastPromptTokens > used {
			used = sum.LastPromptTokens
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "**大脑工作上下文**（Epoch %d · %d 轮）\n", sum.Epoch, sum.ActiveTurns)
		fmt.Fprintf(&sb, "- 占用：%s / %s tokens", formatTokensCompact(used), formatTokensCompact(sum.BudgetTokens))
		if sum.LastPromptTokens > 0 && sum.LastCachedTokens > 0 {
			pct := sum.LastCachedTokens * 100 / sum.LastPromptTokens
			fmt.Fprintf(&sb, " · 缓存命中 %d%%", pct)
		}
		if sum.Compacting {
			sb.WriteString(" · 正在压缩…")
		}
		sb.WriteString("\n")
		if len(info.Groups) > 0 {
			sb.WriteString("\n**活跃事项上下文**\n")
			limit := min(len(info.Groups), 6)
			for _, g := range info.Groups[:limit] {
				tag := ""
				if g.Closed {
					tag = "（已关闭）"
				}
				fmt.Fprintf(&sb, "- %s%s：%d 轮 · 约 %s\n", g.Title, tag, g.TurnCount, formatTokensCompact(g.EstTokens))
			}
		}
		sb.WriteString("\n可用 `/ctx compact` 立即压缩、`/ctx clear` 清空重开（历史记录会保留），或直接发消息让我「抛弃某个事项的上下文」。")
		b.sendInstantWeComText(ctx, chatID, reqID, sb.String())
		return true

	case "compact":
		res, err := b.engine.PruneRouterContext(ctx, core.PruneRouterContextRequest{
			Thread: thread, Mode: "compact", Reason: "wecom_cmd",
		})
		if err != nil {
			b.sendInstantWeComText(ctx, chatID, reqID, "触发压缩失败："+err.Error())
			return true
		}
		b.sendInstantWeComText(ctx, chatID, reqID, res.Message)
		return true

	case "clear":
		res, err := b.engine.PruneRouterContext(ctx, core.PruneRouterContextRequest{
			Thread: thread, Mode: "clear", Reason: "wecom_cmd",
		})
		if err != nil {
			b.sendInstantWeComText(ctx, chatID, reqID, "清空上下文失败："+err.Error())
			return true
		}
		b.sendInstantWeComText(ctx, chatID, reqID, res.Message)
		return true
	}
	return false
}

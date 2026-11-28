package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

func (e *Engine) maybeStartContextCompaction(thread string, st store.RouterContextState, cfg LLMClientConfig, modelName, effort string, force bool) bool {
	if e.store == nil || e.llm == nil || strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(modelName) == "" {
		return false
	}
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	budget := e.Settings().EffectiveRouterContextBudgetTokens()
	effTokens := max(st.LastPromptTokens, st.TotalEstTokens)
	if !force && effTokens < budget {
		return false
	}

	e.mu.Lock()
	if e.router.compacting == nil {
		e.router.compacting = map[string]bool{}
	}
	if e.router.compacting[thread] {
		e.mu.Unlock()
		return false
	}
	e.router.compacting[thread] = true
	rootCtx := e.runCtx
	e.mu.Unlock()
	e.notifyChanged()

	if rootCtx == nil {
		rootCtx = context.Background()
	}
	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.router.compacting, thread)
			e.mu.Unlock()
			e.notifyChanged()
		}()
		cctx, cancel := context.WithTimeout(rootCtx, routerContextCompactionTimeout)
		defer cancel()
		err := e.runContextCompactionSync(cctx, thread, cfg, modelName, effort, routerContextKeepTailTurns, force)
		if err != nil {
			e.logger.Warn("background router context compaction failed", "thread", thread, "err", err)
			if !force {
				if cur, gErr := e.store.GetRouterContextState(cctx, thread); gErr == nil && cur.TotalEstTokens > budget*14/10 {
					if dropped, freed, _, dErr := e.store.DropOldestRouterContextTurns(cctx, thread, budget*8/10, routerContextKeepTailTurns, "overflow_fallback"); dErr == nil && dropped > 0 {
						e.logger.Info("applied deterministic router context overflow fallback", "thread", thread, "dropped", dropped, "freed_est_tokens", freed)
					}
				}
			}
		}
	}()
	return true
}

func (e *Engine) runContextCompactionSync(ctx context.Context, thread string, cfg LLMClientConfig, modelName, effort string, keepTail int, force bool) error {
	if e.store == nil || e.llm == nil {
		return fmt.Errorf("store or llm unavailable")
	}
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	st, err := e.store.GetRouterContextState(ctx, thread)
	if err != nil {
		return err
	}
	turns, err := e.store.ListActiveRouterContextTurns(ctx, thread, st.Epoch)
	if err != nil {
		return err
	}
	if len(turns) <= 1 {
		return nil
	}
	if keepTail < 0 {
		keepTail = 0
	}
	if len(turns) <= keepTail {
		if !force {
			return nil
		}
		keepTail = max(len(turns)/2, 1)
		if len(turns) <= keepTail {
			return nil
		}
	}

	toSummarize := turns[:len(turns)-keepTail]
	if len(toSummarize) == 0 {
		return nil
	}
	cutoffSeq := toSummarize[len(toSummarize)-1].Seq

	var b strings.Builder
	if items, iErr := e.store.ListWorkItems(ctx, true); iErr == nil {
		var openTitles, closedTitles []string
		for _, wi := range items {
			if wi == nil || orStr(wi.Thread, model.MainThread) != thread {
				continue
			}
			label := fmt.Sprintf("%s「%s」", wi.ID, oneLineClip(wi.Title, 40))
			if wi.ClosedAt != nil {
				closedTitles = append(closedTitles, label)
			} else {
				openTitles = append(openTitles, label)
			}
		}
		if len(openTitles) > 0 {
			b.WriteString("当前开放事项：" + strings.Join(openTitles, "、") + "\n")
		}
		if len(closedTitles) > 0 {
			b.WriteString("已关闭事项（仅留最终结论，丢弃过程碎话）：" + strings.Join(closedTitles, "、") + "\n")
		}
	}
	b.WriteString("\n以下是待压缩的较早对话轮次：\n")
	for _, t := range toSummarize {
		fmt.Fprintf(&b, "\n--- 轮次 %s (seq=%d", t.ID, t.Seq)
		if t.WorkItemID != "" {
			fmt.Fprintf(&b, ", work=%s", t.WorkItemID)
		}
		b.WriteString(") ---\n")
		b.WriteString("输入：\n" + clipRunes(t.UserText, 1200, "...[截断]") + "\n")
		b.WriteString("响应：\n" + clipRunes(t.AssistantText, 1800, "...[截断]") + "\n")
	}

	resp, err := e.llm.ChatCompletion(ctx, cfg, LLMChatRequest{
		Model:           modelName,
		ReasoningEffort: effort,
		Stream:          true,
		StreamOptions:   &LLMStreamOptions{IncludeUsage: true},
		Messages: []LLMMessage{
			{Role: "system", Content: contextCompactionSystemPrompt},
			{Role: "user", Content: clipRunes(b.String(), 48000, "\n...[更早部分已截断]")},
		},
	})
	if err != nil {
		return fmt.Errorf("compaction llm call: %w", err)
	}
	summaryText := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summaryText == "" {
		return fmt.Errorf("compaction returned empty summary")
	}

	sumID, _, err := e.store.NextID(ctx, "rt")
	if err != nil {
		return err
	}
	userHeader := fmt.Sprintf("[前情摘要 · epoch %d→%d，已压缩 %d 轮较早对话；更早原文可随时用 read_detail(kind=\"history\") 检索]",
		st.Epoch, st.Epoch+1, len(toSummarize))
	sumTurn := &model.RouterContextTurn{
		ID:            sumID,
		Seq:           cutoffSeq,
		Thread:        thread,
		Kind:          "summary",
		UserText:      userHeader,
		AssistantText: clipRunes(summaryText, 3600, "\n...[摘要截断]"),
		EstTokens:     estimateRouterTokens(userHeader, summaryText),
		CreatedAt:     time.Now(),
	}
	committed, _, err := e.store.CommitRouterContextCompactionTx(ctx, thread, st.Epoch, cutoffSeq, sumTurn)
	if err != nil {
		return err
	}
	if committed {
		e.logger.Info("committed router context compaction", "thread", thread, "from_epoch", st.Epoch, "to_epoch", st.Epoch+1, "summarized_turns", len(toSummarize))
	}
	return nil
}

// tryRecoverContextOverflow runs an emergency shrink/compaction when upstream rejects with context-length 400.
func (e *Engine) tryRecoverContextOverflow(ctx context.Context, thread string, cfg LLMClientConfig, modelName, effort string) bool {
	if e.store == nil {
		return false
	}
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	st, err := e.store.GetRouterContextState(ctx, thread)
	if err != nil {
		return false
	}
	turns, err := e.store.ListActiveRouterContextTurns(ctx, thread, st.Epoch)
	if err != nil || len(turns) == 0 {
		return false
	}
	if len(turns) >= 4 && e.llm != nil && strings.TrimSpace(cfg.APIKey) != "" {
		cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		cErr := e.runContextCompactionSync(cctx, thread, cfg, modelName, effort, min(3, len(turns)/2), true)
		cancel()
		if cErr == nil {
			e.notifyChanged()
			return true
		}
	}
	target := max(st.TotalEstTokens/2, 8000)
	keepTail := max(len(turns)/2, 1)
	if keepTail >= len(turns) {
		keepTail = len(turns) - 1
	}
	dropped, _, _, dErr := e.store.DropOldestRouterContextTurns(ctx, thread, target, keepTail, "overflow_400_recovery")
	if dErr == nil && dropped > 0 {
		e.notifyChanged()
		return true
	}
	return false
}

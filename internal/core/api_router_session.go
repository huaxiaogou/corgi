package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

const (
	routerContextKeepTailTurns     = 6
	routerContextCompactionTimeout = 90 * time.Second
	maxToolTraceEntryRunes         = 260
	maxToolReadExcerptRunes        = 800
	maxToolTracesBlockRunes        = 2000
	maxContextUserTextRunes        = 3000
	maxContextAssistantTextRunes   = 4500
	contextCompactionSystemPrompt  = `你是 Corgi 的上下文压缩助手。请把较早的多轮对话压缩成结构化的中文前情摘要，供主脑在后续轮次中无缝续接。

压缩规则：
1. 必须保留：所有尚未关闭事项（WorkItem）的目标、最新结论、关键技术约束、用户偏好与明确拍板、重要文件路径/分支名/快照 ID（art_xxx）/未解问题。
2. 必须主动清理：已关闭事项的来回碎话、重复寒暄、已被纠正的旧试错过程、无关的过程噪音。
3. 严禁编造对话中未出现的事实；输出简洁分条的 Markdown，控制在 1800 字以内。`
)

// PruneRouterContextRequest defines an explicit context pruning, compaction, or clear operation.
type PruneRouterContextRequest struct {
	Thread     string   `json:"thread"`
	Mode       string   `json:"mode"` // work_item | turns | before | compact | clear
	WorkItemID string   `json:"work_item_id,omitempty"`
	TurnIDs    []string `json:"turn_ids,omitempty"`
	BeforeSeq  int64    `json:"before_seq,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func estimateRouterTokens(parts ...string) int {
	total := 24
	for _, s := range parts {
		if s == "" {
			continue
		}
		var ascii, nonASCII int
		for _, r := range s {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				nonASCII++
			}
		}
		total += (ascii+3)/4 + (nonASCII*5+3)/4
	}
	return total
}

func isContextOverflowError(err error) bool {
	if err == nil || !errors.Is(err, ErrLLMBadRequest) {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, kw := range []string{
		"context_length", "maximum context", "too many tokens",
		"token limit", "context window", "exceeds the model",
		"prompt is too long", "input is too long", "context length exceeded",
	} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func (e *Engine) isRouterContextCompacting(thread string) bool {
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.router.compacting[thread]
}

func (e *Engine) buildThreadContextMessages(ctx context.Context, thread string, trig *model.Trigger, meta *turnConsentMeta) (int, []LLMMessage, int) {
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	msgs := []LLMMessage{{Role: "system", Content: apiRouterSystemPromptV1}}
	if memBlock := e.loadBoundedAPIMemory(); memBlock != "" {
		msgs = append(msgs, LLMMessage{Role: "system", Content: memBlock})
	}

	var (
		st          = store.RouterContextState{Thread: thread, Epoch: 1}
		activeTurns []*model.RouterContextTurn
	)
	if e.store != nil {
		if loaded, err := e.store.GetRouterContextState(ctx, thread); err == nil {
			st = loaded
		}
		if turns, err := e.store.ListActiveRouterContextTurns(ctx, thread, st.Epoch); err == nil {
			activeTurns = turns
		}
	}

	liveTail, peekedBgNotes := e.buildLiveTailStatePeek(ctx, thread, trig, meta)
	// Seamless cold-start / upgrade fallback when no Append-Only turns exist yet in untouched epoch 1.
	if len(activeTurns) == 0 && st.Epoch <= 1 && st.CompactionCount == 0 {
		combined := liveTail
		if hist := e.buildThreadHistoryText(ctx, thread, trig); hist != "" {
			combined = hist + "\n\n" + combined
		}
		if imgParts := e.loadTrustedTriggerImages(ctx, trig, meta); len(imgParts) > 0 {
			return st.Epoch, append(msgs, LLMMessage{Role: "user", Content: append([]LLMContentPart{{Type: "text", Text: combined}}, imgParts...)}), peekedBgNotes
		}
		return st.Epoch, append(msgs, LLMMessage{Role: "user", Content: combined}), peekedBgNotes
	}

	for _, t := range activeTurns {
		if t == nil {
			continue
		}
		uTxt, aTxt := strings.TrimSpace(t.UserText), strings.TrimSpace(t.AssistantText)
		if uTxt == "" || aTxt == "" {
			continue
		}
		msgs = append(msgs,
			LLMMessage{Role: "user", Content: uTxt},
			LLMMessage{Role: "assistant", Content: aTxt},
		)
	}

	if imgParts := e.loadTrustedTriggerImages(ctx, trig, meta); len(imgParts) > 0 {
		msgs = append(msgs, LLMMessage{Role: "user", Content: append([]LLMContentPart{{Type: "text", Text: liveTail}}, imgParts...)})
	} else {
		msgs = append(msgs, LLMMessage{Role: "user", Content: liveTail})
	}
	return st.Epoch, msgs, peekedBgNotes
}

func formatFrozenTurnUserText(trig *model.Trigger, meta *turnConsentMeta) string {
	if trig == nil {
		return ""
	}
	ts := trig.At
	if ts.IsZero() {
		ts = time.Now()
	}
	var head string
	switch trig.Source {
	case "chat", "wecom":
		head = fmt.Sprintf("[%s 用户]", ts.Format("01-02 15:04"))
	case "worker":
		head = fmt.Sprintf("[%s Worker #%s 汇报]", ts.Format("01-02 15:04"), orStr(trig.From, "worker"))
	case "timer":
		head = fmt.Sprintf("[%s 定时提醒]", ts.Format("01-02 15:04"))
	default:
		head = fmt.Sprintf("[%s %s]", ts.Format("01-02 15:04"), orStr(trig.Source, "system"))
	}
	body := strings.TrimSpace(trig.Text)
	if meta != nil && meta.approvedAction != nil {
		chosen := oneLineClip(orStr(meta.approvedAction.ChosenLabel, meta.approvedAction.ChosenKey), 120)
		if chosen != "" && !strings.Contains(body, chosen) {
			body = fmt.Sprintf("[决策卡 %s 已选「%s」]\n%s", meta.approvedAction.ID, chosen, body)
		}
	}
	refs := trig.Media
	if len(refs) == 0 && meta != nil && meta.sourceEntry != nil {
		refs = meta.sourceEntry.Media
	}
	if len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			n := orStr(strings.TrimSpace(r.Name), filepath.Base(strings.TrimSpace(r.Path)))
			if n != "" && n != "." && n != "/" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			body += "\n[附带媒体: " + strings.Join(names, ", ") + "]"
		}
	}
	return clipRunes(head+"\n"+strings.TrimSpace(body), maxContextUserTextRunes, "\n...[已截断]")
}

func formatFrozenTurnAssistantText(turn *apiRouterTurn) string {
	if turn == nil {
		return ""
	}
	traces, replyText, ops := turn.contextSnapshot()
	var parts []string
	if len(traces) > 0 {
		traceBlock := clipRunes(strings.Join(traces, "\n"), maxToolTracesBlockRunes, "\n...[更多工具轨迹已省略]")
		parts = append(parts, "[工具轨迹]\n"+traceBlock)
	}
	if replyText != "" {
		parts = append(parts, "[已回复用户]\n"+replyText)
	} else if ops != "" {
		parts = append(parts, "[已执行动作]\n"+ops)
	}
	return clipRunes(strings.Join(parts, "\n\n"), maxContextAssistantTextRunes, "\n...[已截断]")
}

func (e *Engine) recordCompletedRouterTurn(
	ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, meta *turnConsentMeta,
	startEpoch int, usage LLMUsage, cfg LLMClientConfig, modelName, effort string,
) {
	if e.store == nil || turn == nil || trig == nil {
		return
	}
	// Zero pollution: silent no_action, stopped turns, and non-effective turns never enter working context.
	if turn.hasFlag(turnFlagNoAction) || turn.hasFlag(turnFlagStopped) || (!turn.hasFlag(turnFlagReplied) && !turn.hasFlag(turnFlagMutated)) {
		return
	}
	userText := formatFrozenTurnUserText(trig, meta)
	assistantText := formatFrozenTurnAssistantText(turn)
	if userText == "" || assistantText == "" {
		return
	}

	var wiIDs []string
	primaryWI := ""
	addWI := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if primaryWI == "" {
			primaryWI = id
		}
		if !slices.Contains(wiIDs, id) {
			wiIDs = append(wiIDs, id)
		}
	}
	addWI(trig.WorkItemID)
	if meta != nil && meta.boundWorkItem != nil {
		addWI(meta.boundWorkItem.ID)
	}
	for _, id := range turn.recordedWorkItemIDs() {
		addWI(id)
	}

	id, seq, err := e.store.NextID(ctx, "rt")
	if err != nil {
		e.logger.Warn("allocate router context turn id failed", "err", err)
		return
	}
	cached := 0
	if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	rec := &model.RouterContextTurn{
		ID:            id,
		Seq:           seq,
		Thread:        turn.thread,
		Epoch:         startEpoch,
		Kind:          "turn",
		TriggerID:     trig.ID,
		Source:        trig.Source,
		WorkItemID:    primaryWI,
		WorkItemIDs:   wiIDs,
		UserText:      userText,
		AssistantText: assistantText,
		EstTokens:     estimateRouterTokens(userText, assistantText),
		CreatedAt:     time.Now(),
	}
	st, err := e.store.AppendRouterContextTurn(ctx, rec, usage.PromptTokens, cached)
	if err != nil {
		e.logger.Warn("append router context turn failed", "thread", turn.thread, "err", err)
		return
	}
	e.maybeStartContextCompaction(turn.thread, st, cfg, modelName, effort, false)
}

package core

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	fyiActivityWindow = 24 * time.Hour
	fyiActivityLimit  = 20
)

// CheckClassification records the source-side semantic bucket for a router_check reply.
type CheckClassification struct {
	Category string // model.ActionCategoryCheck ("check") | model.ActionCategoryFYI ("fyi")
	Reason   string // has_deliverable | explicit_question | explicit_review | no_progress | routine_patrol | in_progress_wait | dispatch_ack | substantive_default
}

var (
	urlTokenRe          = regexp.MustCompile(`https?://\S+`)
	quotedMatterTitleRe = regexp.MustCompile(`「([^」\r\n]{2,48})」`)

	explicitReviewAskRe = regexp.MustCompile(`(?i)(?:请(?:抽空|帮忙|先|帮我)?(?:验收|审阅|查看|看一眼|看一下|查收|核对|确认|过目|评估|反馈|告知|回复)|(?:供|请)查收|等待(?:你|您)(?:验收|审阅|反馈|确认)|待(?:你|您)(?:验收|审阅|过目|查收|核对)|please\s+(?:review|check|verify|confirm))`)
	deferredOrNegatedRe = regexp.MustCompile(`(?i)(?:无需|不用|不需要|不必|暂不|完成后|结果出来|全绿后|通过后|随后再|稍后再|等.{0,12}(?:完|后|出来|返回)|no\s+action\s+(?:needed|required))`)

	fyiNoProgressRe = regexp.MustCompile(`(?i)(?:暂无新进展|暂无进展|尚无新进展|没有新进展|仍无新进展|暂无新结果|暂无结果|尚无新结果|暂无变化|无新增变更|暂无更新|尚未输出|报告尚未|还未产出|还没出来|no\s+new\s+(?:progress|updates?))`)
	fyiRoutineOKRe  = regexp.MustCompile(`(?i)(?:无需(?:你|您|人工|手动)?(?:操作|干预|处理|介入|回复|确认|关注)|不需要(?:你|您|人工)?(?:操作|干预|处理|介入)|不用(?:你|您)?(?:操作|干预|处理|管)|巡检(?:正常|无异常|通过)|正常推进中|正常执行中|正常运行中|持续产出中|仍在正常|一切正常|no\s+action\s+(?:needed|required)|running\s+normally)`)
	fyiWaitLaterRe  = regexp.MustCompile(`(?i)(?:(?:有|等有)(?:实质)?(?:新)?(?:进展|结果|证据|结论)(?:后)?(?:我|这边)?(?:会)?(?:再|主动|第一时间)(?:同步|汇报|通知|发你|找你|告诉你)|(?:结果|测试|验证|全量|报告|审查)(?:一)?(?:出来|完成|全绿|通过|一到|返回)(?:后)?(?:我|即|将)?(?:把|会|再|将)?.{0,28}(?:发你|汇报|同步|通知)|(?:完成|搞定|跑完|结束后)(?:后)?(?:一并|再|会|即)(?:汇报|同步|发你|通知)|完成或遇阻(?:我)?再汇报)`)
	fyiInFlightRe   = regexp.MustCompile(`(?i)(?:正在跑|正在等|正等|仍在深入|仍在核查|正在逐文件|推进到后半段|仍在运行|正在运行|还在跑|正在执行)`)
	fyiDeliveredRe  = regexp.MustCompile(`(?i)(?:已交付|已就绪|已产出|已生成|请验收|供查收|已完成全部)`)
	fyiDispatchRe   = regexp.MustCompile(`^(?:已|已经)(?:把|将|向|为|给).{1,48}(?:追加|派发|转交|下发|安排)`)
	fyiDispatchTail = regexp.MustCompile(`(?:汇报|同步|推进中|执行中|不变|跟进中)`)
)

// ClassifyRouterCheck deterministically separates substantive review reminders (check) from
// routine FYI / no-action-needed status updates (fyi) at creation/ingest time.
func ClassifyRouterCheck(title, text string, hasDeliverable bool) CheckClassification {
	if hasDeliverable {
		return CheckClassification{Category: model.ActionCategoryCheck, Reason: "has_deliverable"}
	}
	body := strings.TrimSpace(text)
	if body == "" {
		body = strings.TrimSpace(title)
	}
	cleaned := stripCodeAndURLs(body)
	if cleaned == "" {
		return CheckClassification{Category: model.ActionCategoryCheck, Reason: "substantive_default"}
	}
	if strings.ContainsAny(cleaned, "?？") {
		return CheckClassification{Category: model.ActionCategoryCheck, Reason: "explicit_question"}
	}
	for _, clause := range clauseSplitterRe.Split(cleaned, -1) {
		c := strings.TrimSpace(clause)
		if c == "" {
			continue
		}
		if deferredOrNegatedRe.MatchString(c) {
			continue
		}
		if explicitReviewAskRe.MatchString(c) {
			return CheckClassification{Category: model.ActionCategoryCheck, Reason: "explicit_review"}
		}
	}
	if fyiNoProgressRe.MatchString(cleaned) {
		return CheckClassification{Category: model.ActionCategoryFYI, Reason: "no_progress"}
	}
	if fyiRoutineOKRe.MatchString(cleaned) {
		return CheckClassification{Category: model.ActionCategoryFYI, Reason: "routine_patrol"}
	}
	if fyiWaitLaterRe.MatchString(cleaned) {
		return CheckClassification{Category: model.ActionCategoryFYI, Reason: "in_progress_wait"}
	}
	if fyiInFlightRe.MatchString(cleaned) && !fyiDeliveredRe.MatchString(cleaned) {
		return CheckClassification{Category: model.ActionCategoryFYI, Reason: "in_progress_wait"}
	}
	firstLine := strings.TrimSpace(strings.SplitN(cleaned, "\n", 2)[0])
	if fyiDispatchRe.MatchString(firstLine) && !fyiDeliveredRe.MatchString(cleaned) && fyiDispatchTail.MatchString(cleaned) {
		return CheckClassification{Category: model.ActionCategoryFYI, Reason: "dispatch_ack"}
	}
	return CheckClassification{Category: model.ActionCategoryCheck, Reason: "substantive_default"}
}

func stripCodeAndURLs(s string) string {
	s = inlineCodeSpanRe.ReplaceAllString(s, " ")
	s = urlTokenRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// summarizeLatestUpdate distills a routine FYI/patrol reply into a compact single-line
// WorkItem latest_update summary (stripping redundant leading 「MatterTitle」 prefixes).
func summarizeLatestUpdate(text, workItemTitle string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	if idx := strings.Index(s, "\n\n"); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.Join(strings.Fields(s), " ")
	if wt := strings.TrimSpace(workItemTitle); wt != "" {
		for _, prefix := range []string{"「" + wt + "」", "『" + wt + "』"} {
			if rest, ok := strings.CutPrefix(s, prefix); ok {
				rest = strings.TrimLeft(strings.TrimSpace(rest), "：:—-· ")
				if rest != "" {
					s = rest
				}
				break
			}
		}
	}
	return oneLineClip(s, 120)
}

// resolveOpenWorkItemByQuotedTitle matches a leading 「Title」 mention against open WorkItems
// in the same thread, binding ONLY when an exact unique match exists.
func (e *Engine) resolveOpenWorkItemByQuotedTitle(ctx context.Context, thread, text string) *model.WorkItem {
	if e.store == nil {
		return nil
	}
	head := oneLineClip(strings.TrimSpace(text), 80)
	m := quotedMatterTitleRe.FindStringSubmatch(head)
	if len(m) < 2 {
		return nil
	}
	want := strings.TrimSpace(m[1])
	if want == "" {
		return nil
	}
	items, err := e.store.ListWorkItems(ctx, false)
	if err != nil {
		return nil
	}
	thread = orStr(strings.TrimSpace(thread), model.MainThread)
	var matched *model.WorkItem
	for _, wi := range items {
		if wi == nil || wi.ClosedAt != nil || orStr(wi.Thread, model.MainThread) != thread {
			continue
		}
		if strings.TrimSpace(wi.Title) != want {
			continue
		}
		if matched != nil {
			return nil // ambiguous
		}
		matched = wi
	}
	return matched
}

// ReclassifyPendingNoiseChecks scans legacy pending lightweight router_check actions and
// idempotently migrates routine FYI noise out of pending badges into archived FYI activity
// notices while backfilling the linked open WorkItem's latest_update.
func (e *Engine) ReclassifyPendingNoiseChecks(ctx context.Context) int {
	if e.store == nil {
		return 0
	}
	pending, err := e.store.ListPendingActions(ctx)
	if err != nil || len(pending) == 0 {
		return 0
	}
	migrated := 0
	for _, a := range pending {
		if a == nil || a.Status != model.ActionPending || a.Kind != model.ActionKindRouterCheck || len(a.Choices) > 0 {
			continue
		}
		hasDeliverable := false
		if strings.TrimSpace(a.EntryID) != "" {
			if en, eErr := e.store.GetEntry(ctx, a.EntryID); eErr == nil && en != nil {
				hasDeliverable = len(en.Files) > 0 || len(en.Artifacts) > 0
			}
		}
		body := orStr(strings.TrimSpace(a.Summary), strings.TrimSpace(a.Title))
		cls := ClassifyRouterCheck(a.Title, body, hasDeliverable)
		if cls.Category != model.ActionCategoryFYI {
			continue
		}
		var wi *model.WorkItem
		if wid := strings.TrimSpace(a.WorkItemID); wid != "" {
			if got, gErr := e.store.GetWorkItem(ctx, wid); gErr == nil && got != nil && got.ClosedAt == nil {
				wi = got
			}
		}
		if wi == nil && strings.TrimSpace(a.TargetWorker) != "" {
			if wk, wErr := e.store.GetWorker(ctx, strings.TrimSpace(a.TargetWorker)); wErr == nil && wk != nil && wk.WorkItemID != "" {
				if got, gErr := e.store.GetWorkItem(ctx, wk.WorkItemID); gErr == nil && got != nil && got.ClosedAt == nil {
					wi = got
				}
			}
		}
		if wi == nil {
			wi = e.resolveOpenWorkItemByQuotedTitle(ctx, a.Thread, body)
		}
		wiID := ""
		if wi != nil {
			wiID = wi.ID
		}
		archived, aErr := e.store.ArchiveActionAsFYI(ctx, a.ID, wiID)
		if aErr != nil {
			continue
		}
		if wi != nil {
			ts := archived.CreatedAt
			if ts.IsZero() {
				ts = time.Now()
			}
			_ = e.store.SetWorkItemLatestUpdate(ctx, wi.ID, summarizeLatestUpdate(body, wi.Title), archived.EntryID, ts)
		}
		migrated++
	}
	if migrated > 0 {
		e.logger.Info("reclassified legacy pending FYI router_checks", "count", migrated)
	}
	return migrated
}

// AckAllRouterChecks marks all pending lightweight router_check reminders in thread as seen
// without waking Router or touching any blocking decision cards.
func (e *Engine) AckAllRouterChecks(ctx context.Context, thread, channel string) (int, error) {
	acked, err := e.store.AckAllRouterChecks(ctx, thread, channel)
	if err != nil {
		return 0, err
	}
	if len(acked) > 0 {
		e.mu.Lock()
		sink := e.sink
		e.mu.Unlock()
		for _, a := range acked {
			sink.OnActionResolved(ctx, a)
		}
		sink.OnStateChanged()
	}
	return len(acked), nil
}

// ClearFYIActions clears active low-priority FYI activity notices in thread so the activity feed
// returns to zero while preserving full chat entries.
func (e *Engine) ClearFYIActions(ctx context.Context, thread string) (int, error) {
	cleared, err := e.store.ClearFYIActions(ctx, thread)
	if err != nil {
		return 0, err
	}
	if cleared > 0 {
		e.notifyChanged()
	}
	return cleared, nil
}

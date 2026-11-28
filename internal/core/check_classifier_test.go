package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestClassifyRouterCheckTable(t *testing.T) {
	cases := []struct {
		name           string
		title          string
		text           string
		hasDeliverable bool
		wantCategory   string
		wantReason     string
	}{
		// All 8 real production noise samples observed in corgi list:
		{
			name:         "prod_a4223_no_progress_and_no_action",
			text:         "「Corgi原生配置管理方案」暂无新进展——Codex 第一轮只读方案评审仍在深入核查源码，Worker 在等其完整评审报告返回。\n\n- 上一轮 Agy 自审已完成并落盘，门禁环境就绪\n- Codex 报告一到即逐项裁决、进入多轮对齐，有实质结果我再同步\n\n这期间无需你操作，有进展我会主动汇报。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "no_progress",
		},
		{
			name:         "prod_a4252_no_new_result_yet",
			text:         "「Corgi原生配置管理方案」审查阶段暂无新结果：Codex 独立只读审查正在逐文件核查 Stage 2 代码，评审报告尚未输出。\n\n- 实施侧四个提交已全部完成，无新增变更\n- 审查报告返回后即逐项裁决（砍过度防御），随后进入合入部署流程\n\n有实质结果我再找你，无需操作。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "no_progress",
		},
		{
			name:         "prod_a4338_dispatch_ack",
			text:         "已把「事项转实现接入审批确认」追加给「原生大脑评审发现修复」：prototype/readonly 升级 implement 时强制真人决策卡拍板，复用 #1 正在实现的同一套执行层核验机制，并补测试。独立 commit、门禁要求不变，完成一并汇报。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "in_progress_wait",
		},
		{
			name:         "prod_a4470_running_tests_will_send_later",
			text:         "「1204迭代合入master基线」门禁规则修复进展：master-merge.mjs 已改为\"以迭代分支为锚\"的研发分支合规检查，并补了回归测试，正在跑定向测试套件和本地裸仓库模拟验证。结果出来我把改动摘要和验证证据发你，随后再等你拍板是否走基线发布。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "in_progress_wait",
		},
		{
			name:         "prod_a4474_waiting_remaining_tests",
			text:         "「1204迭代合入master基线」门禁规则修复进展：新增的 manage-merge-master 测试用例目前已全部通过，正在等测试套件剩余部分跑完。全量测试完成后我把改动摘要和验证证据发你，是否走基线发布等你拍板。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "in_progress_wait",
		},
		{
			name:         "prod_a4493_running_suite_report_later",
			text:         "「1204迭代合入master基线」发布闭环进展：门禁规则修复已干净变基到最新 master 之上（逐字节一致、pre-commit 通过），正在跑 prototype-flow 测试套件验证。验证通过后将合入 master，全程不删远端分支、不绕门禁，有新证据我再汇报。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "in_progress_wait",
		},
		{
			name:         "prod_a4497_halfway_waiting_full_result",
			text:         "「1204迭代合入master基线」发布闭环进展：门禁规则修复已干净变基到最新 master（pre-commit 通过），测试套件已推进到后半段、目前全部通过，正等全量结果。测试全绿后将合入 master，完成后发你核验证据。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "in_progress_wait",
		},
		{
			name:         "prod_a4545_patrol_ok_no_intervention",
			text:         "「Collie高中价值功能吸收」巡检确认：Batch 1 开发正常推进中（进度 4/6），无需干预：\n\n- ✅ H2 gzip 缓存已完成\n- 🔄 H3 正在实施\n\n只是连续工作时间较长触发了自动巡检，实际在持续产出，完成或遇阻我再汇报。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "routine_patrol",
		},
		{
			name:         "english_routine_fyi_with_url_query",
			text:         "No new progress on https://ci.example.com/run?id=42 — running normally, no action required.",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "no_progress",
		},
		{
			name:         "dispatch_ack_without_wait_later_phrase",
			text:         "已将文档修订任务派发给设计助手，目前推进中。",
			wantCategory: model.ActionCategoryFYI,
			wantReason:   "dispatch_ack",
		},

		// Substantive review checks & edge cases that MUST stay check (zero false positives):
		{
			name:           "deliverable_always_stays_check",
			text:           "暂无新进展，但附上了最新诊断日志。",
			hasDeliverable: true,
			wantCategory:   model.ActionCategoryCheck,
			wantReason:     "has_deliverable",
		},
		{
			name:         "explicit_question_stays_check",
			text:         "如果同事也安装 Corgi，Hub 是共享服务还是各自部署。你更想讨论哪种？",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "explicit_question",
		},
		{
			name:         "explicit_review_ask_stays_check",
			text:         "请验收这两个分支：\n- feature/a\n- feature/b",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "explicit_review",
		},
		{
			name:         "patrol_found_real_issue_stays_check",
			text:         "巡检确认：发现线上内存泄漏告警，请查看排查报告并反馈处理意见。",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "explicit_review",
		},
		{
			name:         "ready_for_inspection_stays_check",
			text:         "阶段报告已就绪，供查收。",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "explicit_review",
		},
		{
			name:         "short_deliverable_default_stays_check",
			text:         "报告已交付",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "substantive_default",
		},
		{
			name:         "prototype_updated_stays_check",
			text:         "原型已更新",
			wantCategory: model.ActionCategoryCheck,
			wantReason:   "substantive_default",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRouterCheck(tc.title, tc.text, tc.hasDeliverable)
			if got.Category != tc.wantCategory || got.Reason != tc.wantReason {
				t.Fatalf("ClassifyRouterCheck() = %+v, want {Category:%q Reason:%q}", got, tc.wantCategory, tc.wantReason)
			}
		})
	}
}

func TestReplySourceBucketingFYIWorkItemUpdateAndBulkClear(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "1204迭代合入master基线", "待全量测试通过后确认发布", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Seed a real blocking decision and a real substantive review check.
	_, decAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread:  model.MainThread,
		WorkID:  wi.ID,
		Title:   "契约分支拦截发布",
		Text:    "是否向 master 补放行规则？",
		Options: []string{"补放行规则", "手动删分支"},
	})
	if err != nil || decAct == nil || !decAct.IsDecision() {
		t.Fatalf("seed decision failed: act=%+v err=%v", decAct, err)
	}
	_, chk1, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread,
		WorkID: wi.ID,
		Worker: "agy-1",
		Text:   "「1204迭代合入master基线」原型 v1 已交付，请验收。",
		Check:  true,
	})
	if err != nil || chk1 == nil || !chk1.IsSubstantiveCheck() {
		t.Fatalf("seed substantive check v1 failed: act=%+v err=%v", chk1, err)
	}

	// 2. Send a pure FYI --check WITHOUT explicit work_id (bound via quoted matter title).
	fyiEntry, fyiAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "「1204迭代合入master基线」暂无新进展——正在跑测试套件，结果出来我再汇报，这期间无需你操作。",
		Check:  true,
	})
	if err != nil || fyiEntry == nil || fyiAct == nil {
		t.Fatalf("FYI reply failed: entry=%+v act=%+v err=%v", fyiEntry, fyiAct, err)
	}
	if !fyiAct.IsFYI() || fyiAct.Status != model.ActionResolved || fyiAct.ResolvedBy != "auto_fyi" {
		t.Fatalf("expected auto-archived FYI action, got %+v", fyiAct)
	}
	if fyiAct.WorkItemID != wi.ID || fyiEntry.WorkItemID != wi.ID {
		t.Fatalf("expected quoted title auto-binding to %s, got act.wi=%q entry.wi=%q", wi.ID, fyiAct.WorkItemID, fyiEntry.WorkItemID)
	}
	if fyiEntry.Check || fyiEntry.ActionID != "" {
		t.Fatalf("FYI entry must not carry Check=true or ActionID, got Check=%v ActionID=%q", fyiEntry.Check, fyiEntry.ActionID)
	}

	// Verify FYI stayed out of pending actions AND did NOT supersede chk1.
	pending, err := st.ListPendingActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected exactly 2 pending actions (decision + chk1), got %d: %+v", len(pending), pending)
	}
	gotChk1, _ := st.GetAction(ctx, chk1.ID)
	if gotChk1.Status != model.ActionPending {
		t.Fatalf("FYI must NOT supersede existing substantive check, got %+v", gotChk1)
	}

	// Verify WorkItem.LatestUpdate was overwritten while NextStep remained intact.
	gotWI, err := st.GetWorkItem(ctx, wi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWI.NextStep != "待全量测试通过后确认发布" {
		t.Fatalf("NextStep must be preserved, got %q", gotWI.NextStep)
	}
	if !strings.Contains(gotWI.LatestUpdate, "暂无新进展") || strings.HasPrefix(gotWI.LatestUpdate, "「1204迭代") || gotWI.LatestUpdateEntryID != fyiEntry.ID {
		t.Fatalf("unexpected LatestUpdate: text=%q entry=%q at=%v", gotWI.LatestUpdate, gotWI.LatestUpdateEntryID, gotWI.LatestUpdateAt)
	}

	// Verify Snapshot surfaces FYI in ActivityNotices (not Actions).
	snap := eng.Snapshot(ctx, model.MainThread)
	if len(snap.Actions) != 2 || len(snap.ActivityNotices) != 1 || snap.ActivityNotices[0].ID != fyiAct.ID {
		t.Fatalf("unexpected Snapshot partition: actions=%d activity=%+v", len(snap.Actions), snap.ActivityNotices)
	}

	// 3. New substantive check on same WorkItem from a DIFFERENT worker supersedes chk1.
	_, chk2, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread,
		WorkID: wi.ID,
		Worker: "agy-2",
		Text:   "「1204迭代合入master基线」原型 v2 已交付，请验收。",
		Check:  true,
	})
	if err != nil || chk2 == nil || !chk2.IsSubstantiveCheck() {
		t.Fatalf("substantive check v2 failed: act=%+v err=%v", chk2, err)
	}
	gotChk1After, _ := st.GetAction(ctx, chk1.ID)
	if gotChk1After.Status != model.ActionExpired || gotChk1After.ExpiredReason != "superseded" {
		t.Fatalf("expected chk1 superseded by same-WorkItem chk2 from different worker, got %+v", gotChk1After)
	}

	// 4. AckAllRouterChecks clears chk2 without touching decAct.
	acked, err := eng.AckAllRouterChecks(ctx, model.MainThread, "web")
	if err != nil || acked != 1 {
		t.Fatalf("AckAllRouterChecks = (%d, %v), want (1, nil)", acked, err)
	}
	gotDec, _ := st.GetAction(ctx, decAct.ID)
	gotChk2, _ := st.GetAction(ctx, chk2.ID)
	if gotDec.Status != model.ActionPending || gotChk2.Status != model.ActionResolved || gotChk2.ChosenKey != "ack" {
		t.Fatalf("unexpected statuses after AckAllRouterChecks: dec=%+v chk2=%+v", gotDec, gotChk2)
	}

	// 5. ClearFYIActions empties ActivityNotices while preserving chat entries.
	cleared, err := eng.ClearFYIActions(ctx, model.MainThread)
	if err != nil || cleared != 1 {
		t.Fatalf("ClearFYIActions = (%d, %v), want (1, nil)", cleared, err)
	}
	snapAfter := eng.Snapshot(ctx, model.MainThread)
	if len(snapAfter.ActivityNotices) != 0 {
		t.Fatalf("expected empty ActivityNotices after clear, got %+v", snapAfter.ActivityNotices)
	}
	if _, err := st.GetEntry(ctx, fyiEntry.ID); err != nil {
		t.Fatalf("chat entry for cleared FYI must remain readable: %v", err)
	}
}

func TestReclassifyPendingNoiseChecksOnRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	wi := &model.WorkItem{ID: "wi3898", Thread: model.MainThread, Title: "1204迭代合入master基线", NextStep: "保留原下一步"}
	if err := st.CreateWorkItem(ctx, wi); err != nil {
		t.Fatal(err)
	}
	tOlder := time.Now().Add(-2 * time.Hour)
	tNewer := time.Now().Add(-30 * time.Minute)

	// Seed an attachment-bearing entry for a substantive check whose text happens to mention "暂无新进展".
	if err := st.InsertEntry(ctx, &model.Entry{
		ID: "e_with_file", Seq: 1, Role: "corgi", Thread: model.MainThread,
		Text: "暂无新进展，但附了诊断日志", Files: []string{"/tmp/diag.log"}, At: tNewer,
	}); err != nil {
		t.Fatal(err)
	}

	seedActions := []*model.Action{
		{
			ID: "a_noise_old", Kind: model.ActionKindRouterCheck, Thread: model.MainThread,
			EntryID: "e1", WorkItemID: wi.ID, Category: "check", Status: model.ActionPending,
			Title: "「1204迭代合入master基线」暂无新进展", Summary: "「1204迭代合入master基线」暂无新进展，无需你操作。", CreatedAt: tOlder,
		},
		{
			ID: "a_noise_new", Kind: model.ActionKindRouterCheck, Thread: model.MainThread,
			EntryID: "e2", Category: "check", Status: model.ActionPending,
			Title: "「1204迭代合入master基线」测试推进中", Summary: "「1204迭代合入master基线」正在跑测试套件，完成后发你核验证据。", CreatedAt: tNewer,
		},
		{
			ID: "a_real_check_file", Kind: model.ActionKindRouterCheck, Thread: model.MainThread,
			EntryID: "e_with_file", Category: "check", Status: model.ActionPending,
			Title: "暂无新进展，但附了诊断日志", Summary: "暂无新进展，但附了诊断日志", CreatedAt: tNewer,
		},
		{
			ID: "a_legacy_choice_check", Kind: model.ActionKindRouterCheck, Thread: model.MainThread,
			EntryID: "e3", Category: "check", Status: model.ActionPending,
			Title: "暂无新进展", Summary: "暂无新进展", Choices: []model.ActionChoice{{Key: "1", Label: "已确认"}}, CreatedAt: tNewer,
		},
		{
			ID: "a_real_decision", Kind: model.ActionKindRouterChoice, Thread: model.MainThread,
			EntryID: "e4", Category: "decision", Status: model.ActionPending,
			Title: "是否发版", Summary: "是否发版", Choices: []model.ActionChoice{{Key: "1", Label: "发布"}, {Key: "2", Label: "暂缓"}}, CreatedAt: tNewer,
		},
	}
	for _, a := range seedActions {
		if err := st.InsertAction(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	// Constructing Engine runs ReclassifyPendingNoiseChecks automatically on startup.
	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := st.ListPendingActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 remaining pending actions (file check, legacy choice check, real decision), got %d: %+v", len(pending), pending)
	}
	for _, id := range []string{"a_noise_old", "a_noise_new"} {
		got, err := st.GetAction(ctx, id)
		if err != nil || !got.IsFYI() || got.Status != model.ActionResolved || got.ResolvedBy != "auto_fyi" {
			t.Fatalf("expected %s archived as FYI, got %+v err=%v", id, got, err)
		}
	}

	// Newer noise update must win over older noise update for WorkItem.LatestUpdate.
	gotWI, err := st.GetWorkItem(ctx, wi.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotWI.LatestUpdate, "正在跑测试套件") {
		t.Fatalf("expected newer noise update to win LatestUpdate, got %q", gotWI.LatestUpdate)
	}
	if gotWI.NextStep != "保留原下一步" {
		t.Fatalf("expected NextStep untouched, got %q", gotWI.NextStep)
	}

	// Second call must be idempotent (0 migrated).
	if n := eng.ReclassifyPendingNoiseChecks(ctx); n != 0 {
		t.Fatalf("expected idempotent 0 migrations on second run, got %d", n)
	}

	// Watchdog triggers are allowed to invoke no_action when worker is progressing normally.
	if !canAPIRouterSilence(&model.Trigger{
		Source: "worker", Policy: "respond", From: "agy-1",
		Text: "watchdog: worker agy-1 has been running continuously for 20 minutes without a progress report.",
	}) {
		t.Fatal("expected watchdog trigger allowed to silence with no_action")
	}
}

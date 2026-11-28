package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestAPIRouterWorkItemScopeEscalationRequiresDecisionCard(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	repoDir := filepath.Join(dir, "scope-repo")
	initTestGitRepo(t, repoDir)
	cfg := eng.Settings()
	cfg.WorkerKind = "claude"
	cfg.Projects = []model.ProjectWorkspace{{ID: "demo", Name: "Demo", Paths: map[string]string{"hub": repoDir}}}
	if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		spawnSeq int
	)
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.create":
			spawnSeq++
			wsID := fmt.Sprintf("w-%d", spawnSeq)
			return map[string]any{
				"workspace": map[string]any{"workspace_id": wsID},
				"root_pane": map[string]any{"pane_id": wsID + ":p1"},
			}, nil
		case "agent.start":
			return map[string]any{
				"pane_id":      fmt.Sprintf("w-%d:p1", spawnSeq),
				"workspace_id": fmt.Sprintf("w-%d", spawnSeq),
				"session":      fmt.Sprintf("s-%d", spawnSeq),
				"pid":          4000 + spawnSeq,
			}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	protoWI, err := eng.SetWorkItem(ctx, "new", model.MainThread, "登录页改版原型", "评审原型", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWorkItemScope(ctx, protoWI.ID, "prototype"); err != nil {
		t.Fatal(err)
	}
	roWI, err := eng.SetWorkItem(ctx, "new", model.MainThread, "日志慢查询调研", "输出分析结论", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWorkItemScope(ctx, roWI.ID, "readonly"); err != nil {
		t.Fatal(err)
	}
	otherWI, err := eng.SetWorkItem(ctx, "new", model.MainThread, "其他事项", "继续推进", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWorkItemScope(ctx, otherWI.ID, "implement"); err != nil {
		t.Fatal(err)
	}

	findTriggerForAction := func(actID string) *model.Trigger {
		t.Helper()
		trs, err := st.ListQueuedTriggers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range trs {
			if en, eErr := st.GetEntry(ctx, tr.Ref); eErr == nil && en != nil && en.ActionID == actID {
				return tr
			}
		}
		t.Fatalf("no trigger found for action %s", actID)
		return nil
	}

	// 1. Plain human text mentioning "转入源码实现" MUST NOT escalate prototype or readonly work items.
	_, plainTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "owner", Text: "原型没问题了，直接开始改源码吧"})
	if err != nil {
		t.Fatal(err)
	}
	plainMeta := eng.resolveTurnConsentMeta(ctx, plainTrig)
	for _, tc := range []struct {
		wiID      string
		wantScope string
	}{
		{protoWI.ID, "prototype"},
		{roWI.ID, "readonly"},
	} {
		out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
			fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"按结论修改源码"}`, tc.wiID))
		if !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "决策卡片") {
			t.Fatalf("work_id=%s: expected plain text escalation rejected, got %s", tc.wiID, out)
		}
		if got, _ := st.GetWorkItem(ctx, tc.wiID); got == nil || got.Scope != tc.wantScope {
			t.Fatalf("work_id=%s: scope must remain %q after rejection, got %+v", tc.wiID, tc.wantScope, got)
		}
	}

	// 2. Quoted entry bound to prototype WorkItem MUST NOT escalate even when work_id is omitted.
	protoEn := &model.Entry{ID: "e-proto-delivery", Thread: model.MainThread, Role: "corgi", WorkItemID: protoWI.ID, Text: "已产出原型稿", At: time.Now()}
	if err := st.InsertEntry(ctx, protoEn); err != nil {
		t.Fatal(err)
	}
	_, quotedProtoTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "就按这个开始写代码", ReEntryID: protoEn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: quotedProtoTrig.ID}, quotedProtoTrig, eng.resolveTurnConsentMeta(ctx, quotedProtoTrig),
		`{"mode":"implement","project":"demo","goal":"按原型改代码"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, protoWI.ID) {
		t.Fatalf("expected bound prototype work item escalation rejected without work_id, got %s", out)
	}

	// 3. Resuming an existing prototype Worker (or ended readonly session) with mode="implement" (without work_id) MUST also be blocked.
	protoWk := &model.Worker{
		Name: "claude-proto-1", Kind: "claude", Thread: model.MainThread, WorkItemID: protoWI.ID,
		Mode: "prototype", Cwd: repoDir, Session: "sess-proto-1", Ended: true, Status: "ended", CreatedAt: time.Now(),
	}
	if err := st.UpsertWorker(ctx, protoWk); err != nil {
		t.Fatal(err)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		`{"mode":"implement","resume":"claude-proto-1","goal":"续接改源码"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "决策卡片") {
		t.Fatalf("expected resume escalation rejected without decision card, got %s", out)
	}
	if err := st.SaveEndedSession(ctx, &model.EndedSession{
		Name: "claude-ro-ended", Kind: "claude", Thread: model.MainThread, WorkItemID: roWI.ID,
		Mode: "readonly", Cwd: repoDir, Session: "sess-ro-ended", EndedAt: time.Now(),
	}, 20); err != nil {
		t.Fatal(err)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		`{"mode":"implement","resume":"claude-ro-ended","goal":"续接只读会话改源码"}`); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, roWI.ID) {
		t.Fatalf("expected ended-session resume escalation rejected without decision card, got %s", out)
	}
	if got, _ := st.GetWorkItem(ctx, protoWI.ID); got == nil || got.Scope != "prototype" {
		t.Fatalf("protoWI scope must remain prototype after blocked resume, got %+v", got)
	}

	// 4. Non-choice card forms and cross-work-item cards MUST NOT unlock scope escalation.
	_, customAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, WorkID: protoWI.ID, Text: "是否转入实现？", Options: []string{"转入实现", "继续调原型"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionCustom(ctx, customAct.ID, "web", "转入实现"); err != nil {
		t.Fatal(err)
	}
	customTrig := findTriggerForAction(customAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: customTrig.ID}, customTrig, eng.resolveTurnConsentMeta(ctx, customTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"按原型改代码"}`, protoWI.ID)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected custom free-text card reply rejected for scope escalation, got %s", out)
	}

	_, checkAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, WorkID: protoWI.ID, Text: "原型已更新", Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionCustom(ctx, checkAct.ID, "web", "开始改代码"); err != nil {
		t.Fatal(err)
	}
	checkTrig := findTriggerForAction(checkAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: checkTrig.ID}, checkTrig, eng.resolveTurnConsentMeta(ctx, checkTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"按原型改代码"}`, protoWI.ID)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected router_check rejected for scope escalation, got %s", out)
	}

	cardEn, quoteAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, WorkID: protoWI.ID, Text: "确认转入源码实现？", Options: []string{"转入源码实现", "继续看原型"}})
	if err != nil || quoteAct == nil {
		t.Fatalf("create quote card: %v", err)
	}
	_, quotedCardTrig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "转入源码实现", ReEntryID: cardEn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: quotedCardTrig.ID}, quotedCardTrig, eng.resolveTurnConsentMeta(ctx, quotedCardTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"按原型改代码"}`, protoWI.ID)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected quoted plain reply (chosen_key=reply) rejected for scope escalation, got %s", out)
	}

	// Decision card explicitly bound to otherWI MUST NOT authorize escalating protoWI.
	_, otherAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, WorkID: otherWI.ID, Text: "确认推进其他事项？", Options: []string{"继续推进", "暂缓"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, otherAct.ID, "web", "1"); err != nil {
		t.Fatal(err)
	}
	otherTrig := findTriggerForAction(otherAct.ID)
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: otherTrig.ID}, otherTrig, eng.resolveTurnConsentMeta(ctx, otherTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"借卡越权改代码"}`, protoWI.ID)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected cross-work-item decision card rejected, got %s", out)
	}

	// 5. Non-escalation operations remain allowed without decision cards:
	//    (a) staying in prototype on protoWI, (b) fresh implement on unrelated work, (c) existing implement work item.
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		fmt.Sprintf(`{"mode":"prototype","work_id":%q,"project":"demo","goal":"微调原型配色"}`, protoWI.ID)); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected prototype stay-in-scope dispatch allowed, got %s", out)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"继续推进已有实现事项"}`, otherWI.ID)); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected existing implement work item dispatch allowed without extra card, got %s", out)
	}
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		`{"mode":"implement","project":"demo","goal":"新需求直接修复按钮样式"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected fresh implement dispatch allowed without extra card, got %s", out)
	}

	// 6. Real human-clicked single-select decision card unlocks prototype -> implement in isolated worktree & upgrades scope.
	_, okProtoAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, WorkID: protoWI.ID, Text: "原型已定稿，是否转入源码实现？", Options: []string{"开始转入源码实现", "继续调整原型"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, okProtoAct.ID, "web", "1"); err != nil {
		t.Fatal(err)
	}
	okProtoTrig := findTriggerForAction(okProtoAct.ID)
	outProto := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: okProtoTrig.ID}, okProtoTrig, eng.resolveTurnConsentMeta(ctx, okProtoTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"worktree":"off","project":"demo","goal":"按定稿原型实现登录页"}`, protoWI.ID))
	var protoResp struct {
		OK     bool   `json:"ok"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(outProto), &protoResp); err != nil || !protoResp.OK || !strings.HasPrefix(protoResp.Branch, "corgi/") {
		t.Fatalf("expected card-approved prototype->implement to succeed in isolated worktree, got %s", outProto)
	}
	if got, _ := st.GetWorkItem(ctx, protoWI.ID); got == nil || got.Scope != "implement" {
		t.Fatalf("expected protoWI scope upgraded to implement, got %+v", got)
	}

	// Once upgraded to implement, follow-up human turns can dispatch implement without re-asking.
	if out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: plainTrig.ID}, plainTrig, plainMeta,
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"补充登录页单测"}`, protoWI.ID)); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected already-upgraded implement work item to allow follow-up implement, got %s", out)
	}

	// 7. Defense-in-depth: updateWorkItemScopeOnDispatch itself refuses unapproved readonly -> implement.
	eng.updateWorkItemScopeOnDispatch(ctx, plainMeta, roWI.ID, "implement")
	if got, _ := st.GetWorkItem(ctx, roWI.ID); got == nil || got.Scope != "readonly" {
		t.Fatalf("expected direct scope upgrade blocked without decision card, got %+v", got)
	}

	// 8. Real human-clicked multi-select decision card (same-thread, unbound work_id) unlocks readonly -> implement.
	_, okROAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread, Text: "调研已完成，确认进入源码优化？", Options: []string{"加索引并改源码", "补基准测试"}, Mode: model.ActionModeMulti,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionChoices(ctx, okROAct.ID, "wecom", []string{"1", "2"}); err != nil {
		t.Fatal(err)
	}
	okROTrig := findTriggerForAction(okROAct.ID)
	outRO := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: okROTrig.ID}, okROTrig, eng.resolveTurnConsentMeta(ctx, okROTrig),
		fmt.Sprintf(`{"mode":"implement","work_id":%q,"project":"demo","goal":"优化慢查询 SQL 并补单测"}`, roWI.ID))
	var roResp struct {
		OK     bool   `json:"ok"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(outRO), &roResp); err != nil || !roResp.OK || !strings.HasPrefix(roResp.Branch, "corgi/") {
		t.Fatalf("expected multi-select card-approved readonly->implement to succeed in worktree, got %s", outRO)
	}
	if got, _ := st.GetWorkItem(ctx, roWI.ID); got == nil || got.Scope != "implement" {
		t.Fatalf("expected roWI scope upgraded to implement, got %+v", got)
	}
}

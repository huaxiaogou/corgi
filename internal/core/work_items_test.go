package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func TestWorkItemsLedgerLifecycleInjectionAndDreamInput(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu      sync.Mutex
		prompts []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			return map[string]any{
				"workspace": map[string]any{"workspace_id": "w1"},
				"root_pane": map[string]any{"pane_id": "w1:p1"},
			}, nil
		case "agent.prompt":
			mu.Lock()
			prompts = append(prompts, params["text"].(string))
			mu.Unlock()
			return map[string]any{}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	ctx := context.Background()
	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Manual task set/list/show + spawn auto-create + --work attach + --ephemeral skip.
	wiManual, err := eng.SetWorkItem(ctx, "new", model.MainThread, "收纳箱交互原型交付", "发 HTML 到企微", "", "")
	if err != nil || !strings.HasPrefix(wiManual.ID, "wi") {
		t.Fatalf("SetWorkItem(new): wi=%+v err=%v", wiManual, err)
	}
	if shown, err := eng.Show(ctx, wiManual.ID); err != nil {
		t.Fatalf("Show(%s): %v", wiManual.ID, err)
	} else if gotWI, ok := shown.(*model.WorkItem); !ok || gotWI.Title != "收纳箱交互原型交付" {
		t.Fatalf("unexpected Show(%s) payload: %#v", wiManual.ID, shown)
	}

	wk1, err := eng.Spawn(ctx, SpawnRequest{Name: "w-one", Cwd: dir, Task: "排查登录超时问题"})
	if err != nil || wk1.WorkItemID == "" {
		t.Fatalf("Spawn auto-create work_item failed: wk=%+v err=%v", wk1, err)
	}
	wk2, err := eng.Spawn(ctx, SpawnRequest{Name: "w-two", Cwd: dir, Task: "补充回归测试", WorkID: wk1.WorkItemID})
	if err != nil || wk2.WorkItemID != wk1.WorkItemID {
		t.Fatalf("Spawn --work attach failed: wk=%+v err=%v", wk2, err)
	}
	attached, err := st.GetWorkItem(ctx, wk1.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(attached.Workers, "w-one") || !slices.Contains(attached.Workers, "w-two") {
		t.Fatalf("expected both workers linked to %s, got %+v", wk1.WorkItemID, attached.Workers)
	}

	wkEph, err := eng.Spawn(ctx, SpawnRequest{Name: "w-lookup", Cwd: dir, Task: "临时查版本号", Ephemeral: true})
	if err != nil || wkEph.WorkItemID != "" {
		t.Fatalf("ephemeral spawn should not create work_item: wk=%+v err=%v", wkEph, err)
	}

	// 2. StopWorker preserves worktree branch on work_item, and --resume carries work_item_id forward.
	wk1.Session = "sess-w-one"
	wk1.WorktreeBranch = "corgi/w-one"
	_ = st.UpsertWorker(ctx, wk1)
	if err := eng.StopWorker(ctx, wk1.Name); err != nil {
		t.Fatalf("StopWorker: %v", err)
	}
	afterStop, _ := st.GetWorkItem(ctx, wk1.WorkItemID)
	if !slices.Contains(afterStop.Branches, "corgi/w-one") {
		t.Fatalf("expected StopWorker to preserve branch on work_item, got %+v", afterStop.Branches)
	}
	resumed, err := eng.Spawn(ctx, SpawnRequest{Resume: wk1.Name, Task: "继续修单测"})
	if err != nil || resumed.WorkItemID != wk1.WorkItemID {
		t.Fatalf("resume must carry original work_item_id %s, got wk=%+v err=%v", wk1.WorkItemID, resumed, err)
	}

	// 3. SendToWorkerWithWork inherits worker's open item and rejects ambiguity across 2 open items.
	if _, gotWI, err := eng.SendToWorkerWithWork(ctx, wk2.Name, model.MainThread, "加个边界用例", ""); err != nil || gotWI != wk1.WorkItemID {
		t.Fatalf("SendToWorkerWithWork inherit failed: wi=%q err=%v", gotWI, err)
	}
	// Make an unassigned active worker linked to TWO open items -> ambiguous without --work.
	wkAmb := &model.Worker{Name: "w-amb", Kind: "claude", Cwd: dir, Thread: model.MainThread, Status: "idle", CreatedAt: time.Now()}
	_ = st.UpsertWorker(ctx, wkAmb)
	_, _ = eng.SetWorkItem(ctx, wiManual.ID, "", "", "", "w-amb", "")
	_, _ = eng.SetWorkItem(ctx, wk1.WorkItemID, "", "", "", "w-amb", "")
	if _, _, err := eng.SendToWorkerWithWork(ctx, "w-amb", model.MainThread, "继续", ""); err == nil || !strings.Contains(err.Error(), "multiple open work items") {
		t.Fatalf("expected ambiguity error for worker in 2 open items, got %v", err)
	}
	if _, resolvedWI, err := eng.SendToWorkerWithWork(ctx, "w-amb", model.MainThread, "继续", wiManual.ID); err != nil || resolvedWI != wiManual.ID {
		t.Fatalf("explicit --work disambiguation failed: wi=%q err=%v", resolvedWI, err)
	}

	// 4. Create enough open items (13 total) and verify deliverBatch injects compact 12-of-13 summary on normal human turn.
	for i := 0; i < 11; i++ {
		if _, err := eng.SetWorkItem(ctx, "new", model.MainThread, fmt.Sprintf("并发事项-%02d", i+1), "推进中", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	trig, err := eng.Ingest(ctx, "chat", model.MainThread, "you", "当前整体还有什么事情？", "", model.RouterName, "e1")
	if err != nil {
		t.Fatal(err)
	}
	eng.deliverBatch(ctx, model.RouterName, "w0:p1", []*model.Trigger{trig})
	mu.Lock()
	var delivered string
	for _, p := range prompts {
		if strings.Contains(p, "当前整体还有什么事情") {
			delivered = p
			break
		}
	}
	mu.Unlock()
	if !strings.Contains(delivered, "[open work items: showing 12 of 13 — run `corgi list` for complete list]") {
		t.Fatalf("expected 12-of-13 open work items summary injected on normal human turn, got:\n%s", delivered)
	}

	// 5. writeDreamInput includes Authoritative Open Work Items section.
	if err := eng.writeDreamInput(ctx); err != nil {
		t.Fatalf("writeDreamInput: %v", err)
	}
	dreamInput, err := os.ReadFile(filepath.Join(eng.Memory().RouterDir(), "dream", "input.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dreamInput), "## Authoritative Open Work Items (Hub SQLite)") || !strings.Contains(string(dreamInput), "收纳箱交互原型交付") {
		t.Fatalf("dream/input.md missing authoritative open work items:\n%s", string(dreamInput))
	}

	// 6. CloseWorkItem requires reason and hides closed item from open list.
	if _, err := eng.CloseWorkItem(ctx, wiManual.ID, ""); err == nil {
		t.Fatal("CloseWorkItem should require non-empty closing reason")
	}
	if closed, err := eng.CloseWorkItem(ctx, wiManual.ID, "附件已企微确认送达"); err != nil || closed.ClosedAt == nil {
		t.Fatalf("CloseWorkItem failed: wi=%+v err=%v", closed, err)
	}
	openItems, _ := eng.ListWorkItems(ctx, false)
	for _, wi := range openItems {
		if wi.ID == wiManual.ID {
			t.Fatalf("closed item %s still appeared in open ListWorkItems", wiManual.ID)
		}
	}

	// 7. Snapshot exposes open WorkItems and ClosedWorkCount; Reopen restores item and clears close_note.
	snap := eng.Snapshot(ctx, model.MainThread)
	if snap.ClosedWorkCount != 1 {
		t.Fatalf("Snapshot ClosedWorkCount want 1, got %d", snap.ClosedWorkCount)
	}
	if len(snap.WorkItems) != len(openItems) {
		t.Fatalf("Snapshot WorkItems len want %d, got %d", len(openItems), len(snap.WorkItems))
	}
	for _, wi := range snap.WorkItems {
		if wi.ID == wiManual.ID {
			t.Fatalf("closed item %s leaked into Snapshot.WorkItems", wiManual.ID)
		}
	}

	// ClearWorkItemNextStep clears next_step on an open item.
	cleared, err := eng.ClearWorkItemNextStep(ctx, wk1.WorkItemID)
	if err != nil || cleared.NextStep != "" {
		t.Fatalf("ClearWorkItemNextStep failed: wi=%+v err=%v", cleared, err)
	}

	// ReopenWorkItem restores closed item back into open list & Snapshot.
	reopened, err := eng.ReopenWorkItem(ctx, wiManual.ID)
	if err != nil || reopened.ClosedAt != nil || reopened.CloseNote != "" {
		t.Fatalf("ReopenWorkItem failed: wi=%+v err=%v", reopened, err)
	}
	snapAfter := eng.Snapshot(ctx, model.MainThread)
	if snapAfter.ClosedWorkCount != 0 {
		t.Fatalf("Snapshot ClosedWorkCount after reopen want 0, got %d", snapAfter.ClosedWorkCount)
	}
	if len(snapAfter.WorkItems) != len(openItems)+1 {
		t.Fatalf("Snapshot WorkItems after reopen want %d, got %d", len(openItems)+1, len(snapAfter.WorkItems))
	}
}

func TestCheckProtectionOptionsCapAndSayIdempotency(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	ctx := context.Background()
	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Pure --check survives unrelated unquoted chat, and ExpireWorkerActions ONLY expires worker_dialog.
	chkEntry, chkAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "请验收收纳箱原型",
		Worker: "ui-dev",
		Check:  true,
	})
	if err != nil || chkAct == nil {
		t.Fatalf("Reply(--check): %v", err)
	}
	if err := st.ExpireWorkerActions(ctx, "ui-dev"); err != nil {
		t.Fatal(err)
	}
	gotChk, _ := st.GetAction(ctx, chkAct.ID)
	if gotChk.Status != model.ActionPending {
		t.Fatalf("ExpireWorkerActions must NOT expire router_check, got %+v", gotChk)
	}
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "顺便帮我查一下部署状态"}); err != nil {
		t.Fatal(err)
	}
	gotChk, _ = st.GetAction(ctx, chkAct.ID)
	if gotChk.Status != model.ActionPending {
		t.Fatalf("unrelated unquoted follow-up must NOT clear router_check, got %+v", gotChk)
	}

	// WeCom quoted message without ReEntryID must NOT guess and auto-resolve an unrelated router_choice.
	_, optAct, _, err := eng.Reply(ctx, model.MainThread, "是否发布？", "", []string{"发布", "稍后"}, false)
	if err != nil || optAct == nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Say(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "wecom",
		From:      "owner",
		Text:      "收到",
		QuoteText: "某条历史消息内容",
	}); err != nil {
		t.Fatal(err)
	}
	gotOpt, _ := st.GetAction(ctx, optAct.ID)
	if gotOpt.Status != model.ActionPending {
		t.Fatalf("WeCom quoted reply without ReEntryID must NOT auto-resolve router_choice, got %+v", gotOpt)
	}

	// Explicit ReEntryID quoting the --check entry DOES resolve it.
	if _, _, err := eng.Say(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "原型看完了，OK",
		ReEntryID: chkEntry.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gotChk, _ = st.GetAction(ctx, chkAct.ID)
	if gotChk.Status != model.ActionResolved {
		t.Fatalf("explicit quoted reply to --check entry should resolve it, got %+v", gotChk)
	}

	// 2. cleanOptions allows >80 up to 240 runes without ellipsis, and rejects >240 runes.
	longOpt := strings.Repeat("长中文决策选项内容", 12) // 108 runes
	_, longAct, _, err := eng.Reply(ctx, model.MainThread, "请选择", "", []string{longOpt, "暂不处理"}, false)
	if err != nil || longAct == nil || longAct.Choices[0].Label != longOpt {
		t.Fatalf("expected 108-rune option preserved without ellipsis, got act=%+v err=%v", longAct, err)
	}
	tooLongOpt := strings.Repeat("超长选项", 65) // 260 runes > 240
	if _, _, _, err := eng.Reply(ctx, model.MainThread, "请选择", "", []string{tooLongOpt, "取消"}, false); err == nil {
		t.Fatal("expected error for option exceeding 240 runes")
	}

	// 3. SubmitChatSay client_msg_id deduplicates both direct delivery and queued prompts.
	beforeEntries, _, _ := st.ListEntries(ctx, model.MainThread, 200)
	_, _, e1, t1, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:      model.MainThread,
		Channel:     "web",
		From:        "you",
		Text:        "防连点重复发送测试",
		ClientMsgID: "uuid-direct-1",
	}, "auto")
	if err != nil || e1 == nil || t1 == nil {
		t.Fatalf("first direct SubmitChatSay failed: e=%v t=%v err=%v", e1, t1, err)
	}
	_, _, e2, t2, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:      model.MainThread,
		Channel:     "web",
		From:        "you",
		Text:        "防连点重复发送测试",
		ClientMsgID: "uuid-direct-1",
	}, "auto")
	if err != nil || e2 == nil || t2 == nil || e2.ID != e1.ID || t2.ID != t1.ID {
		t.Fatalf("duplicate SubmitChatSay should replay original IDs (%s/%s), got (%v/%v) err=%v", e1.ID, t1.ID, e2, t2, err)
	}
	afterEntries, _, _ := st.ListEntries(ctx, model.MainThread, 200)
	if len(afterEntries) != len(beforeEntries)+1 {
		t.Fatalf("duplicate client_msg_id created extra user entry: before=%d after=%d", len(beforeEntries), len(afterEntries))
	}

	// Queued retry with same client_msg_id returns the same queued prompt without duplicating.
	q1, item1, _, _, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:      model.MainThread,
		Channel:     "web",
		From:        "you",
		Text:        "排队重试幂等测试",
		ClientMsgID: "uuid-queue-1",
	}, "queue")
	if err != nil || !q1 || item1 == nil {
		t.Fatalf("first queued SubmitChatSay failed: q=%v item=%v err=%v", q1, item1, err)
	}
	q2, item2, _, _, err := eng.SubmitChatSay(ctx, SayRequest{
		Thread:      model.MainThread,
		Channel:     "web",
		From:        "you",
		Text:        "排队重试幂等测试",
		ClientMsgID: "uuid-queue-1",
	}, "queue")
	if err != nil || !q2 || item2 == nil || item2.ID != item1.ID {
		t.Fatalf("duplicate queued SubmitChatSay should return same item %s, got %+v err=%v", item1.ID, item2, err)
	}
}

func TestCausalWorkItemActionProvenanceAndAtomicExpiry(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	wiA, err := eng.SetWorkItem(ctx, "new", model.MainThread, "太湖接入方案", "等待评审", "wk-a", "")
	if err != nil {
		t.Fatal(err)
	}
	wiB, err := eng.SetWorkItem(ctx, "new", model.MainThread, "物言可视化方案", "等待评审", "wk-b", "")
	if err != nil {
		t.Fatal(err)
	}
	wkA := &model.Worker{Name: "wk-a", Kind: "agy", Thread: model.MainThread, WorkItemID: wiA.ID, Status: "idle"}
	wkB := &model.Worker{Name: "wk-b", Kind: "agy", Thread: model.MainThread, WorkItemID: wiB.ID, Status: "idle"}
	_ = st.UpsertWorker(ctx, wkA)
	_ = st.UpsertWorker(ctx, wkB)

	// 1. Report atomically writes BOTH WorkItemID and ArtifactID on initial trigger insert.
	longReport := strings.Repeat("太湖接入详细可行性报告段落。", 45)
	repTrig, err := eng.Report(ctx, wkA.Name, "", longReport)
	if err != nil || repTrig == nil {
		t.Fatalf("Report failed: %v", err)
	}
	persistedTrig, err := st.GetTrigger(ctx, repTrig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedTrig.WorkItemID != wiA.ID || persistedTrig.ArtifactID == "" {
		t.Fatalf("expected atomic WorkItemID=%s & non-empty ArtifactID on report trigger, got %+v", wiA.ID, persistedTrig)
	}

	// 2. Reply referencing report trigger (without explicit --work) inherits WorkItemID onto Action.
	resA, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread:  model.MainThread,
		ReID:    repTrig.ID,
		Text:    "太湖接入方案是否继续推进？",
		Options: []string{"继续推进", "暂停"},
	})
	if err != nil || resA.Action == nil || resA.Action.WorkItemID != wiA.ID {
		t.Fatalf("expected Action to inherit WorkItemID=%s from referenced trigger, got %+v err=%v", wiA.ID, resA, err)
	}

	// Also create a snoozed choice + live worker_dialog on wiA, and independent choice + 2 checks on wiB.
	resASnooze, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread:  model.MainThread,
		WorkID:  wiA.ID,
		Text:    "太湖域名申请审批",
		Options: []string{"批准", "拒绝"},
	})
	if err != nil || resASnooze.Action == nil {
		t.Fatalf("create snoozed action failed: %v", err)
	}
	if _, err := st.SnoozeAction(ctx, resASnooze.Action.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	dlgA := &model.Action{
		ID: "a_dlg_wiA", Kind: model.ActionKindWorkerDialog, Thread: model.MainThread,
		WorkItemID: wiA.ID, TargetWorker: wkA.Name, Title: "Allow bash?", Status: model.ActionPending, CreatedAt: time.Now(),
	}
	if err := st.InsertAction(ctx, dlgA); err != nil {
		t.Fatal(err)
	}

	resBChoice, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread:  model.MainThread,
		WorkID:  wiB.ID,
		Worker:  wkB.Name,
		Text:    "物言可视化是否进入 MVP？",
		Options: []string{"开始 MVP", "先等等"},
	})
	if err != nil || resBChoice.Action == nil {
		t.Fatalf("create wiB choice failed: %v", err)
	}
	resBChk1, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		WorkID: wiB.ID,
		Worker: wkB.Name,
		Text:   "物言原型 v1 已交付",
		Check:  true,
	})
	if err != nil || resBChk1.Action == nil {
		t.Fatalf("create wiB check v1 failed: %v", err)
	}
	resBChk2, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		WorkID: wiB.ID,
		Worker: wkB.Name,
		Text:   "物言原型 v2 已交付",
		Check:  true,
	})
	if err != nil || resBChk2.Action == nil {
		t.Fatalf("create wiB check v2 failed: %v", err)
	}

	// Newer router_check on wiB atomically supersedes older router_check, WITHOUT touching wiB's router_choice.
	gotChk1, _ := st.GetAction(ctx, resBChk1.Action.ID)
	gotChk2, _ := st.GetAction(ctx, resBChk2.Action.ID)
	gotBChoice, _ := st.GetAction(ctx, resBChoice.Action.ID)
	if gotChk1.Status != model.ActionExpired || gotChk1.ExpiredReason != "superseded" {
		t.Fatalf("expected older router_check superseded, got %+v", gotChk1)
	}
	if gotChk2.Status != model.ActionPending || gotBChoice.Status != model.ActionPending {
		t.Fatalf("expected latest check and router_choice still pending, got chk2=%+v choice=%+v", gotChk2, gotBChoice)
	}

	// 3. Closing wiA atomically expires wiA's pending + snoozed cards, leaves live worker_dialog & sibling wiB untouched.
	if _, err := eng.CloseWorkItem(ctx, wiA.ID, "用户已取消太湖接入"); err != nil {
		t.Fatalf("CloseWorkItem(wiA): %v", err)
	}
	gotA, _ := st.GetAction(ctx, resA.Action.ID)
	gotASnooze, _ := st.GetAction(ctx, resASnooze.Action.ID)
	gotDlgA, _ := st.GetAction(ctx, dlgA.ID)
	gotBChoiceAfter, _ := st.GetAction(ctx, resBChoice.Action.ID)

	if gotA.Status != model.ActionExpired || gotA.ResolvedBy != "expired" || gotA.ExpiredReason != "work_item_closed" {
		t.Fatalf("expected wiA pending action expired, got %+v", gotA)
	}
	if gotASnooze.Status != model.ActionExpired || gotASnooze.SnoozeUntil != nil || gotASnooze.ExpiredReason != "work_item_closed" {
		t.Fatalf("expected wiA snoozed action expired with cleared SnoozeUntil, got %+v", gotASnooze)
	}
	if woken, err := st.WakeDueSnoozedActions(ctx, time.Now().Add(time.Hour)); err != nil || len(woken) != 0 {
		t.Fatalf("closed WorkItem snoozed action must NEVER resurrect, woken=%+v err=%v", woken, err)
	}
	if _, err := eng.ResolveAction(ctx, resA.Action.ID, "web", "1"); err == nil {
		t.Fatal("expected resolving expired action of closed WorkItem to be rejected")
	}
	if gotDlgA.Status != model.ActionPending {
		t.Fatalf("live worker_dialog must NOT be implicitly expired on WorkItem close, got %+v", gotDlgA)
	}
	if gotBChoiceAfter.Status != model.ActionPending {
		t.Fatalf("sibling wiB decision card in same thread must stay pending, got %+v", gotBChoiceAfter)
	}
}

func TestReplyPreSendEmptyAttachmentGate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Claiming attachment without --file or report snapshot MUST fail BEFORE committing an entry.
	before, _, _ := st.ListEntries(ctx, model.MainThread, 50)
	if _, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "物言 Agent 数据可视化方案报告已重新附上，请查收。",
	}); err == nil || !strings.Contains(err.Error(), "0 files or report snapshots") {
		t.Fatalf("expected empty-attachment pre-send rejection, got %v", err)
	}
	after, _, _ := st.ListEntries(ctx, model.MainThread, 50)
	if len(after) != len(before) {
		t.Fatalf("rejected empty-attachment reply must not persist entry: before=%d after=%d", len(before), len(after))
	}

	// 2. Negations, questions, and code-span mentions must NOT false-trigger.
	for _, safeText := range []string{
		"刚才没有附上方案文件，我马上补发。",
		"需要我把方案报告作为附件发出来吗？",
		"排查 `已附上` 和 `请查收附件` 的门禁逻辑。",
	} {
		if _, err := eng.ReplyDetailed(ctx, ReplyRequest{Thread: model.MainThread, Text: safeText}); err != nil {
			t.Fatalf("safe reply %q unexpectedly rejected: %v", safeText, err)
		}
	}

	// 3. Claiming attachment WITH explicit --file succeeds cleanly.
	docPath := filepath.Join(dir, "proposal.md")
	if err := os.WriteFile(docPath, []byte("# Proposal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "物言方案报告已重新附上，请查收。",
		Files:  []string{docPath},
	}); err != nil || res == nil || len(res.Entry.Artifacts) != 1 {
		t.Fatalf("expected --file reply to succeed, res=%+v err=%v", res, err)
	}

	// 4. Claiming attachment WITH exact --re <report_tID> snapshot (FILES=0) ALSO succeeds cleanly.
	wk := &model.Worker{Name: "agent-flow-plan", Kind: "agy", Thread: model.MainThread, Status: "idle"}
	_ = st.UpsertWorker(ctx, wk)
	repTrig, err := eng.Report(ctx, wk.Name, "", strings.Repeat("详细数据流设计方案。", 60))
	if err != nil || repTrig == nil || repTrig.ArtifactID == "" {
		t.Fatalf("Report snapshot setup failed: trig=%+v err=%v", repTrig, err)
	}
	if res, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		ReID:   repTrig.ID,
		Worker: wk.Name,
		Text:   "完整报告已附上，请查收附件。",
	}); err != nil || res == nil || len(res.Entry.Artifacts) != 1 {
		t.Fatalf("expected --re report snapshot attachment reply to succeed, res=%+v err=%v", res, err)
	}
}

func TestWorkItemDynamicTitleGovernanceUnbindAndWeComPickerDedup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. DeriveWorkItemTitle strips real dispatch boilerplate ("新任务（用户原话）：...") and flags draft vs explicit --title.
	draftTitle, isDraft := DeriveWorkItemTitle("", `新任务（用户原话）：‘画个新的拍板卡原型 我记得以前不长这样’，附件：/Users/foo/a.png`, "")
	if !isDraft || draftTitle != "画个新的拍板卡原型 我记得以前不长这样" {
		t.Fatalf("unexpected draft title: (%q, %v)", draftTitle, isDraft)
	}
	explicitTitle, isDraft2 := DeriveWorkItemTitle("企微拍板卡交互原型", "很长的原始任务说明", "")
	if isDraft2 || explicitTitle != "企微拍板卡交互原型" {
		t.Fatalf("unexpected explicit title: (%q, %v)", explicitTitle, isDraft2)
	}

	// 2. Send without --title creates clean draft title with empty next_step; Report injects [work_item: ... (draft ...)].
	wk := &model.Worker{Name: "agy-85", Kind: "agy", PaneID: "w7:p1", Thread: model.MainThread, Status: "idle", CreatedAt: time.Now()}
	_ = st.UpsertWorker(ctx, wk)
	_, wiID, err := eng.SendToWorkerWithWorkTitle(ctx, wk.Name, model.MainThread, `新任务（用户原话）：‘画个新的拍板卡原型’`, "", "")
	if err != nil || wiID == "" {
		t.Fatalf("SendToWorkerWithWorkTitle failed: wi=%q err=%v", wiID, err)
	}
	wi, _ := st.GetWorkItem(ctx, wiID)
	if wi.Title != "画个新的拍板卡原型" || wi.NextStep != "" {
		t.Fatalf("expected clean draft title & empty initial next_step, got title=%q next=%q", wi.Title, wi.NextStep)
	}
	repTrig, err := eng.Report(ctx, wk.Name, "", "交互原型已完成")
	if err != nil || !strings.Contains(repTrig.Text, "[work_item: "+wiID) || !strings.Contains(repTrig.Text, "(draft") {
		t.Fatalf("expected [work_item: ... (draft ...)] injected in report trigger, got %q", repTrig.Text)
	}

	// 3. Router LLM freely refines draft & evolving titles, while human Web rename locks against automated overwrites.
	if _, err := eng.SetWorkItem(ctx, wiID, model.MainThread, "企微拍板卡交互原型", "评审原型", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SetWorkItem(ctx, wiID, model.MainThread, "企微拍板卡方案3实现", "待评审合入", "", ""); err != nil {
		t.Fatal(err)
	}
	wiEvolved, _ := st.GetWorkItem(ctx, wiID)
	if wiEvolved.Title != "企微拍板卡方案3实现" {
		t.Fatalf("expected Router LLM able to evolve non-locked title, got %q", wiEvolved.Title)
	}
	// Human manual edit in Web locks title.
	if _, err := eng.SetWorkItemWithOptions(ctx, wiID, model.MainThread, "拍板卡最终版（人工命名）", "", "", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SetWorkItem(ctx, wiID, model.MainThread, "Router 不应覆盖人工标题", "下一步更新仍应生效", "", ""); err != nil {
		t.Fatal(err)
	}
	wiLocked, _ := st.GetWorkItem(ctx, wiID)
	if wiLocked.Title != "拍板卡最终版（人工命名）" || wiLocked.NextStep != "下一步更新仍应生效" {
		t.Fatalf("expected user-locked title preserved while next_step updates, got title=%q next=%q", wiLocked.Title, wiLocked.NextStep)
	}

	// 4. UnbindWorkerFromWorkItem detaches mislinked worker without losing branches/artifacts or stopping pane.
	_ = st.RecordWorkItemBranch(ctx, wiID, "corgi/wecom-decision-implementation")
	wk2 := &model.Worker{Name: "wecom-impl", Kind: "agy", PaneID: "wP:p1", Thread: model.MainThread, Status: "working", WorkItemID: wiID, CreatedAt: time.Now()}
	_, _ = st.AttachWorkerAndUpsert(ctx, wk2, wiID, "", "", "")

	// 5. Before unbinding, WeCom /w ListSelectableWorkers deduplicates multi-worker WorkItem to 1 primary row (preferring working).
	picks := eng.ListSelectableWorkers(ctx, 20)
	if len(picks) < 2 || picks[0].Name != "wecom-impl" || picks[0].WorkItemID != wiID {
		t.Fatalf("expected working worker wecom-impl picked as primary for %s, got %+v", wiID, picks)
	}
	if picks[1].Name != "agy-85" || !strings.Contains(picks[1].Title, "agy-85") {
		t.Fatalf("expected secondary worker demoted with worker suffix instead of duplicate title, got %+v", picks[1])
	}

	// Now unbind agy-85 -> removed from WorkItem & worker.WorkItemID cleared, branch & pane preserved.
	unboundWI, err := eng.UnbindWorkerFromWorkItem(ctx, wiID, "agy-85")
	if err != nil {
		t.Fatalf("UnbindWorkerFromWorkItem failed: %v", err)
	}
	if len(unboundWI.Workers) != 1 || unboundWI.Workers[0] != "wecom-impl" || len(unboundWI.Branches) != 1 {
		t.Fatalf("unexpected unbound WorkItem state: %+v", unboundWI)
	}
	gotAgy, _ := st.GetWorker(ctx, "agy-85")
	if gotAgy.Ended || gotAgy.WorkItemID != "" {
		t.Fatalf("expected agy-85 pane still alive with cleared WorkItemID, got %+v", gotAgy)
	}
}

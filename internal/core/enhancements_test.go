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

func TestReadableReplyLen(t *testing.T) {
	// Markdown syntax, newlines, versions, branches, and link URLs must not inflate count.
	bulletList := strings.Join([]string{
		"当前整体进展：",
		"- **HTML 预览**：已发布 `v0.1.36`，正在构建部署",
		"- **输入框修复**：3 个问题待在独立 worktree 处理",
		"- **详单**：[查看报告](/Users/kiperjing/github/corgi/docs/report.md)",
	}, "\n")
	n := ReadableReplyLen(bulletList)
	// Raw rune length is >135, readable count should be ~50 (CJK chars + 1 per English/version token).
	if n < 35 || n > 65 {
		t.Fatalf("ReadableReplyLen(bulletList) = %d, want 35..65 (raw runes=%d)", n, len([]rune(bulletList)))
	}
	// Adding pure Markdown formatting and blank lines should keep count identical.
	plain := "当前整体进展\nHTML 预览 已发布 v0.1.36 正在构建部署\n输入框修复 3 个问题待在独立 worktree 处理\n详单 查看报告"
	if gotPlain := ReadableReplyLen(plain); gotPlain != n {
		t.Fatalf("Markdown syntax changed ReadableReplyLen: formatted=%d plain=%d", n, gotPlain)
	}
	if got := ReadableReplyLen("Done: merged feature/foo-bar and deployed v0.1.38."); got != 6 {
		t.Fatalf("English word/version count = %d, want 6", got)
	}
	if got := ReadableReplyLen("已发布v0.1.36了"); got != 5 {
		t.Fatalf("Mixed CJK+version count = %d, want 5", got)
	}
}

func TestNaturalReplyAutoResolveSilentAckAndSevenDayExpiry(t *testing.T) {
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

	// 1. Lightweight --check + silent "ack" resolves WITHOUT waking Router / creating user entry.
	_, checkAct, warn, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "请验收这两个分支：\n- feature/a\n- feature/b",
		Check:  true,
	})
	if err != nil || checkAct == nil || checkAct.Kind != model.ActionKindRouterCheck || warn != "" {
		t.Fatalf("unexpected --check result: act=%+v warn=%q err=%v", checkAct, warn, err)
	}
	beforeEntries, _, _ := st.ListEntries(ctx, model.MainThread, 50)
	acked, err := eng.ResolveAction(ctx, checkAct.ID, "web", "ack")
	if err != nil || acked.Status != model.ActionResolved || acked.ChosenKey != "ack" || acked.ChosenLabel != "已看" {
		t.Fatalf("silent ack failed: act=%+v err=%v", acked, err)
	}
	afterEntries, _, _ := st.ListEntries(ctx, model.MainThread, 50)
	if len(afterEntries) != len(beforeEntries) {
		t.Fatalf("silent ack must not add a user message entry: before=%d after=%d", len(beforeEntries), len(afterEntries))
	}

	// 2. Unquoted human reply MUST NOT auto-resolve pending router_choice OR worker_dialog.
	_, choiceAct, _, err := eng.Reply(ctx, model.MainThread, "要现在发布吗？", "", []string{"发布", "稍后"}, false)
	if err != nil || choiceAct == nil {
		t.Fatalf("Reply(--opt): %v", err)
	}
	dlgAct := &model.Action{
		ID: "a_dlg", Kind: model.ActionKindWorkerDialog, Thread: model.MainThread,
		TargetWorker: "w1", Title: "Accept edit?", Status: model.ActionPending, CreatedAt: time.Now(),
	}
	if err := st.InsertAction(ctx, dlgAct); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "顺便问下另一个项目的进展"}); err != nil {
		t.Fatal(err)
	}
	gotChoice, _ := st.GetAction(ctx, choiceAct.ID)
	if gotChoice.Status != model.ActionPending {
		t.Fatalf("expected unquoted unrelated chat to preserve pending router_choice, got %+v", gotChoice)
	}
	gotDlg, _ := st.GetAction(ctx, dlgAct.ID)
	if gotDlg.Status != model.ActionPending {
		t.Fatalf("worker_dialog must stay pending on normal chat, got %+v", gotDlg)
	}

	// 3. Explicit quoted reply (direct OR via reply-chain) resolves ONLY the quoted action, leaving newer ones intact.
	eOld, actOld, _, _ := eng.Reply(ctx, model.MainThread, "问题 A", "", []string{"A1", "A2"}, false)
	eChain, _, _, _ := eng.Reply(ctx, model.MainThread, "补充说明 A 的上下文", eOld.ID, nil, false)
	_, actNew, _, _ := eng.Reply(ctx, model.MainThread, "问题 B", "", []string{"B1", "B2"}, false)
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "按 A1 办", ReEntryID: eChain.ID}); err != nil {
		t.Fatal(err)
	}
	gotOld, _ := st.GetAction(ctx, actOld.ID)
	gotNew, _ := st.GetAction(ctx, actNew.ID)
	if gotOld.Status != model.ActionResolved || gotOld.ChosenKey != "reply" {
		t.Fatalf("reply-chain quoted action should resolve: %+v", gotOld)
	}
	if gotNew.Status != model.ActionPending {
		t.Fatalf("unquoted sibling action should stay pending: %+v", gotNew)
	}

	// 4. 7-day expiry expires old router_choice/patrol_digest actions while preserving worker_dialog & router_check.
	staleChoice := &model.Action{
		ID: "a_stale_8d_choice", Kind: model.ActionKindRouterChoice, Thread: model.MainThread,
		Title: "8天前选项", Status: model.ActionPending, CreatedAt: time.Now().Add(-8 * 24 * time.Hour),
	}
	staleCheck := &model.Action{
		ID: "a_stale_8d_check", Kind: model.ActionKindRouterCheck, Thread: model.MainThread,
		Title: "8天前待确认", Status: model.ActionPending, CreatedAt: time.Now().Add(-8 * 24 * time.Hour),
	}
	if err := st.InsertAction(ctx, staleChoice); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAction(ctx, staleCheck); err != nil {
		t.Fatal(err)
	}
	snap := eng.Snapshot(ctx, model.MainThread)
	gotStaleChoice, _ := st.GetAction(ctx, staleChoice.ID)
	if gotStaleChoice.Status != model.ActionExpired || gotStaleChoice.ResolvedBy != "expired" {
		t.Fatalf("expected 8-day-old router_choice expired, got %+v", gotStaleChoice)
	}
	gotStaleCheck, _ := st.GetAction(ctx, staleCheck.ID)
	if gotStaleCheck.Status != model.ActionPending {
		t.Fatalf("expected router_check exempted from 7-day expiry, got %+v", gotStaleCheck)
	}
	for _, a := range snap.Actions {
		if a.ID == staleChoice.ID {
			t.Fatalf("expired action leaked into Snapshot.Actions: %+v", a)
		}
	}
}

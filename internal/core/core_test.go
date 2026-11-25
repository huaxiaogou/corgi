package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestParseAndAdvanceTimer(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	base := time.Date(2026, 11, 25, 10, 0, 0, 0, loc) // Wednesday

	cases := []struct {
		spec      string
		wantNext  time.Time
		wantAfter bool // remains enabled after AdvanceTimer
	}{
		{"in 30m", base.Add(30 * time.Minute), false},
		{"every 2h", base.Add(2 * time.Hour), true},
		{"daily 09:00", time.Date(2026, 11, 26, 9, 0, 0, 0, loc), true},
		{"weekdays 11:30", time.Date(2026, 11, 25, 11, 30, 0, 0, loc), true},
		{"weekly fri 09:00", time.Date(2026, 11, 27, 9, 0, 0, 0, loc), true},
		{"at 2026-11-30 18:45", time.Date(2026, 11, 30, 18, 45, 0, 0, loc), false},
	}
	for _, tc := range cases {
		got, err := ParseTimerSpec(tc.spec, base)
		if err != nil {
			t.Fatalf("ParseTimerSpec(%q) err: %v", tc.spec, err)
		}
		if !got.Equal(tc.wantNext) {
			t.Fatalf("ParseTimerSpec(%q) = %v, want %v", tc.spec, got, tc.wantNext)
		}
		tm := &model.Timer{Spec: tc.spec, NextRun: got, Enabled: true}
		AdvanceTimer(tm, got)
		if tm.Enabled != tc.wantAfter {
			t.Fatalf("AdvanceTimer(%q) enabled=%v, want %v", tc.spec, tm.Enabled, tc.wantAfter)
		}
	}
}

func TestExtractQuoteWorkerTag(t *testing.T) {
	cases := []struct {
		quote string
		want  string
	}{
		{"[#unit-tests · claude · devbox] 单测已补完 6 个用例", "unit-tests"},
		{"[#api_profiler] 发现缺索引", "api_profiler"},
		{"普通群聊引用没有标签", ""},
	}
	for _, tc := range cases {
		if got := ExtractQuoteWorkerTag(tc.quote); got != tc.want {
			t.Fatalf("ExtractQuoteWorkerTag(%q) = %q, want %q", tc.quote, got, tc.want)
		}
	}
}

func TestMemoryManagerGuardrails(t *testing.T) {
	mm := NewMemoryManager(t.TempDir())
	if err := mm.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := mm.SaveFile("../escape.md", "bad"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if err := mm.SaveFile("messages.md", "overwrite"); err == nil {
		t.Fatal("expected messages.md read-only rejection")
	}
	if err := mm.SaveFile("notes/shop.md", "# Shop Notes"); err != nil {
		t.Fatalf("SaveFile valid note failed: %v", err)
	}
	files := mm.ListFiles()
	found := false
	for _, f := range files {
		if f.Name == "notes/shop.md" && strings.Contains(f.Text, "Shop Notes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected notes/shop.md in %+v", files)
	}
}

func TestWriteRouterDocsIncludesDefaultWorkerMachine(t *testing.T) {
	mm := NewMemoryManager(t.TempDir())
	st := model.Settings{
		ReplyChars:    200,
		WorkerMachine: "kiperjing-mb0-2",
		WorkerKind:    "agy",
		WorkerModel:   "gemini-4-neon",
		WorkerEffort:  "xhigh",
		WorkerCwd:     "~",
	}
	if err := mm.WriteRouterDocs("/usr/local/bin/corgi", st); err != nil {
		t.Fatalf("WriteRouterDocs failed: %v", err)
	}
	mm.MarkRouterDocsRead()
	b, err := os.ReadFile(filepath.Join(mm.RouterDir(), "CORGI.md"))
	if err != nil {
		t.Fatalf("read CORGI.md: %v", err)
	}
	doc := string(b)
	for _, want := range []string{
		"machine kiperjing-mb0-2",
		"kind agy",
		"model gemini-4-neon",
		"effort xhigh",
		"cwd ~",
		"default computer (`kiperjing-mb0-2`)",
		"NEVER** pass bare `--check` without `--opt`",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("CORGI.md missing %q", want)
		}
	}

	// Changing WorkerMachine should report changed=true so Router hot-reloads CORGI.md.
	st.WorkerMachine = ""
	changed, err := mm.WriteRouterDocsChanged("/usr/local/bin/corgi", st)
	if err != nil || !changed {
		t.Fatalf("expected changed=true after WorkerMachine update, got changed=%v err=%v", changed, err)
	}
	b2, _ := os.ReadFile(filepath.Join(mm.RouterDir(), "CORGI.md"))
	if !strings.Contains(string(b2), "machine hub (local)") {
		t.Fatalf("expected fallback 'machine hub (local)' when WorkerMachine is empty")
	}
}

func TestEngineReplyCreatesActionAndAtomicResolve(t *testing.T) {
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
	entry, act, _, err := eng.Reply(ctx, model.MainThread, "要建索引吗？", "", []string{"生成脚本", "暂不修改"}, false)
	if err != nil || entry == nil || act == nil {
		t.Fatalf("Reply failed: entry=%v act=%v err=%v", entry, act, err)
	}
	if act.Status != model.ActionPending || len(act.Choices) != 2 {
		t.Fatalf("unexpected action: %+v", act)
	}

	res, err := eng.ResolveAction(ctx, act.ID, "wecom", "1")
	if err != nil || res.ChosenLabel != "生成脚本" || res.ResolvedBy != "wecom" {
		t.Fatalf("ResolveAction failed: res=%+v err=%v", res, err)
	}
	if _, err := eng.ResolveAction(ctx, act.ID, "web", "2"); !errors.Is(err, store.ErrActionAlreadyResolved) {
		t.Fatalf("expected ErrActionAlreadyResolved on second click, got %v", err)
	}
}

func TestMatchActionChoiceAndCursorDialogResolution(t *testing.T) {
	act := &model.Action{
		ID:   "a9",
		Kind: model.ActionKindWorkerDialog,
		Choices: []model.ActionChoice{
			{Key: "", Label: "Yes, allow once", Value: "1"},
			{Key: "", Label: "Yes, allow for this session", Value: "2"},
			{Key: "n", Label: "No, deny command", Value: "3"},
		},
	}
	tests := []struct {
		input      string
		wantKey    string
		wantLabel  string
		wantCursor int
	}{
		{input: "1", wantKey: "", wantLabel: "Yes, allow once", wantCursor: 1},
		{input: "2", wantKey: "", wantLabel: "Yes, allow for this session", wantCursor: 2},
		{input: "n", wantKey: "n", wantLabel: "No, deny command", wantCursor: 3},
		{input: "session", wantKey: "", wantLabel: "Yes, allow for this session", wantCursor: 2},
		{input: "esc", wantKey: "esc", wantLabel: "取消 (Esc)", wantCursor: 0},
	}
	for _, tc := range tests {
		k, lbl, cur := matchActionChoice(act, tc.input)
		if k != tc.wantKey || lbl != tc.wantLabel || cur != tc.wantCursor {
			t.Fatalf("matchActionChoice(%q) = (%q, %q, %d), want (%q, %q, %d)",
				tc.input, k, lbl, cur, tc.wantKey, tc.wantLabel, tc.wantCursor)
		}
	}

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
	if err := st.InsertAction(ctx, &model.Action{
		ID:      "a9",
		Kind:    model.ActionKindWorkerDialog,
		Thread:  model.MainThread,
		Title:   "Run command",
		Choices: act.Choices,
		Status:  model.ActionPending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, "a9", "wecom", "   "); err == nil {
		t.Fatal("expected error for empty choice input")
	}
	resolved, err := eng.ResolveAction(ctx, "a9", "wecom", "2")
	if err != nil {
		t.Fatalf("ResolveAction cursor dialog failed: %v", err)
	}
	if resolved.ChosenLabel != "Yes, allow for this session" || resolved.Status != model.ActionResolved {
		t.Fatalf("unexpected resolved cursor action: %+v", resolved)
	}
	got, err := eng.GetAction(ctx, "a9")
	if err != nil || got.Status != model.ActionResolved || got.ChosenLabel != "Yes, allow for this session" {
		t.Fatalf("GetAction mismatch: got=%+v err=%v", got, err)
	}
}

func TestAdoptUnmanagedAgentsRegistersManagedWorker(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"agy"})
	eng, err := NewEngine(context.Background(), st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	kind := "agy"
	cwd := "/Users/dev/project"
	name := deriveFallbackWorkerName(kind, "w6:p1@mb0")
	eng.adoptUnmanagedAgents(ctx, []model.Agent{{
		Name:          &name,
		PaneID:        "w6:p1@mb0",
		WorkspaceID:   "w6@mb0",
		Agent:         &kind,
		AgentStatus:   "working",
		Cwd:           &cwd,
		TerminalTitle: "分析当前项目功能点及定位",
	}})

	wk, err := st.GetWorker(ctx, name)
	if err != nil {
		t.Fatalf("expected adopted worker %q in store: %v", name, err)
	}
	if wk.Status != "working" || wk.Machine != "mb0" || wk.Cwd != cwd || wk.CreatedBy != "you" {
		t.Fatalf("unexpected adopted worker fields: %+v", wk)
	}
	snap := eng.Snapshot(ctx, model.MainThread)
	if len(snap.Workers) != 1 || snap.Workers[0].Name != name {
		t.Fatalf("expected adopted worker in SuperView snapshot: %+v", snap.Workers)
	}
}

func TestReplyWithMetaBareCheckGuardrail(t *testing.T) {
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

	// Seed an existing patrol digest so we verify bare --digest does not wipe it.
	_, existingDigest, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread:  model.MainThread,
		Text:    "已有巡检简报",
		Options: []string{"处理", "稍后"},
		Digest:  true,
	})
	if err != nil || existingDigest == nil {
		t.Fatalf("seed digest failed: act=%v err=%v", existingDigest, err)
	}

	cases := []struct {
		name         string
		req          ReplyRequest
		wantAction   bool
		wantKind     string
		wantChoices  []string
		wantWarnSubs []string
		wantCheck    bool
	}{
		{
			name: "bare check on open A/B question degrades to plain reply",
			req: ReplyRequest{
				Thread: model.MainThread,
				Text:   "如果同事也安装 Corgi，Hub 是共享服务还是各自部署。你更想讨论哪种？",
				Check:  true,
			},
			wantAction:   false,
			wantWarnSubs: []string{"--check", "--opt"},
			wantCheck:    false,
		},
		{
			name: "bare check on plain status ack degrades to plain reply",
			req: ReplyRequest{
				Thread: model.MainThread,
				Text:   "收到，已开始推进实现与验证。",
				Check:  true,
			},
			wantAction:   false,
			wantWarnSubs: []string{"--check", "--opt"},
			wantCheck:    false,
		},
		{
			name: "bare digest without options degrades without fabricating 已确认",
			req: ReplyRequest{
				Thread: model.MainThread,
				Text:   "早报：今天有两项待办。",
				Digest: true,
			},
			wantAction:   false,
			wantWarnSubs: []string{"--digest", "--opt"},
			wantCheck:    false,
		},
		{
			name: "explicit options with check creates router_choice cleanly",
			req: ReplyRequest{
				Thread:  model.MainThread,
				Text:    "Hub 架构你倾向哪种？",
				Options: []string{"共享服务（推荐）", "各自部署"},
				Check:   true,
			},
			wantAction:  true,
			wantKind:    model.ActionKindRouterChoice,
			wantChoices: []string{"共享服务（推荐）", "各自部署"},
			wantCheck:   true,
		},
		{
			name: "single explicit opt creates router_choice with exact label",
			req: ReplyRequest{
				Thread:  model.MainThread,
				Text:    "请确认这份发布清单。",
				Options: []string{"已确认清单"},
			},
			wantAction:  true,
			wantKind:    model.ActionKindRouterChoice,
			wantChoices: []string{"已确认清单"},
			wantCheck:   false,
		},
		{
			name: "long reply with bare check preserves both warnings",
			req: ReplyRequest{
				Thread: model.MainThread,
				Text:   strings.Repeat("这是一段较长的回复内容，用来触发字数提示。", 15),
				Check:  true,
			},
			wantAction:   false,
			wantWarnSubs: []string{"--check", "--opt", "usual budget"},
			wantCheck:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, act, warn, err := eng.ReplyWithMeta(ctx, tc.req)
			if err != nil {
				t.Fatalf("ReplyWithMeta err: %v", err)
			}
			if entry == nil {
				t.Fatal("expected non-nil entry")
			}
			if entry.Check != tc.wantCheck {
				t.Fatalf("entry.Check = %v, want %v", entry.Check, tc.wantCheck)
			}
			if tc.wantAction {
				if act == nil || entry.ActionID != act.ID {
					t.Fatalf("expected action linked to entry, got act=%+v entry.ActionID=%q", act, entry.ActionID)
				}
				if act.Kind != tc.wantKind {
					t.Fatalf("act.Kind = %q, want %q", act.Kind, tc.wantKind)
				}
				gotLabels := make([]string, len(act.Choices))
				for i, c := range act.Choices {
					gotLabels[i] = c.Label
				}
				if strings.Join(gotLabels, "|") != strings.Join(tc.wantChoices, "|") {
					t.Fatalf("choices = %v, want %v", gotLabels, tc.wantChoices)
				}
			} else {
				if act != nil || entry.ActionID != "" {
					t.Fatalf("expected no action for bare flag, got act=%+v entry.ActionID=%q", act, entry.ActionID)
				}
			}
			if len(tc.wantWarnSubs) == 0 && warn != "" {
				t.Fatalf("expected empty warning, got %q", warn)
			}
			for _, sub := range tc.wantWarnSubs {
				if !strings.Contains(warn, sub) {
					t.Fatalf("warning %q missing %q", warn, sub)
				}
			}
		})
	}

	// Ensure seeded digest remained pending (bare --digest did not expire it).
	gotSeed, err := st.GetAction(ctx, existingDigest.ID)
	if err != nil || gotSeed.Status != model.ActionPending {
		t.Fatalf("expected seeded digest to stay pending, got %+v err=%v", gotSeed, err)
	}
}

func TestLegacyRouterCheckCompatibilityAndShowAction(t *testing.T) {
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

	legacy1 := &model.Action{
		ID:        "a366",
		Kind:      model.ActionKindRouterCheck,
		Thread:    model.MainThread,
		EntryID:   "e365",
		Title:     "历史 router_check 标题…",
		Summary:   "历史 router_check 标题完整正文",
		Choices:   []model.ActionChoice{{Key: "1", Label: "已确认"}},
		Status:    model.ActionPending,
		CreatedAt: time.Now(),
	}
	legacy2 := &model.Action{
		ID:        "a367",
		Kind:      model.ActionKindRouterCheck,
		Thread:    model.MainThread,
		EntryID:   "e366",
		Title:     "另一条历史待确认",
		Summary:   "另一条历史待确认",
		Choices:   []model.ActionChoice{{Key: "1", Label: "已确认"}},
		Status:    model.ActionPending,
		CreatedAt: time.Now(),
	}
	if err := st.InsertAction(ctx, legacy1); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAction(ctx, legacy2); err != nil {
		t.Fatal(err)
	}

	// Show supports expanding Action IDs for Chain of Recall / debugging.
	obj, err := eng.Show(ctx, "a366")
	if err != nil {
		t.Fatalf("Show(a366) err: %v", err)
	}
	shown, ok := obj.(*model.Action)
	if !ok || shown.ID != "a366" || shown.Kind != model.ActionKindRouterCheck {
		t.Fatalf("Show(a366) = %#v, want *model.Action a366", obj)
	}

	// Legacy router_check actions can still be resolved and dismissed atomically.
	res, err := eng.ResolveAction(ctx, "a366", "web", "1")
	if err != nil || res.Status != model.ActionResolved || res.ChosenLabel != "已确认" {
		t.Fatalf("ResolveAction(legacy router_check) failed: res=%+v err=%v", res, err)
	}
	if _, err := eng.ResolveAction(ctx, "a366", "wecom", "1"); !errors.Is(err, store.ErrActionAlreadyResolved) {
		t.Fatalf("expected ErrActionAlreadyResolved on double resolve, got %v", err)
	}
	dis, err := eng.DismissAction(ctx, "a367")
	if err != nil || dis.Status != model.ActionExpired || dis.ResolvedBy != "dismiss" {
		t.Fatalf("DismissAction(legacy router_check) failed: dis=%+v err=%v", dis, err)
	}
}

func TestDecisionReceiptLinkingFollowUpAndSnapshot(t *testing.T) {
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

	// 1. Before selection: pending action appears in Actions and NOT in ResolvedActions.
	qEntry, act, _, err := eng.Reply(ctx, model.MainThread, "两个需求可以并行推进，你希望怎么安排？", "", []string{
		"先分别出两份方案",
		"先只做一个，另一个排队",
		"直接写代码，不出方案",
	}, false)
	if err != nil || qEntry == nil || act == nil {
		t.Fatalf("Reply failed: entry=%v act=%v err=%v", qEntry, act, err)
	}
	snapBefore := eng.Snapshot(ctx, model.MainThread)
	if len(snapBefore.Actions) != 1 || snapBefore.Actions[0].ID != act.ID {
		t.Fatalf("expected 1 pending action before click, got %+v", snapBefore.Actions)
	}
	if len(snapBefore.ResolvedActions) != 0 {
		t.Fatalf("pending action must not appear in ResolvedActions, got %+v", snapBefore.ResolvedActions)
	}

	// 2. Interleaved unrelated message between question and click (cross-message resolution).
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "插一句：先看下日志路径"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := eng.Reply(ctx, model.MainThread, "日志在 /tmp/demo.log。", "", nil, false); err != nil {
		t.Fatal(err)
	}

	// 3. Resolve action from Web -> user entry carries ActionID + Re.Entry; Snapshot exposes ResolvedActions with all choices.
	resolved, err := eng.ResolveAction(ctx, act.ID, "web", "1")
	if err != nil || resolved.Status != model.ActionResolved || resolved.ChosenLabel != "先分别出两份方案" {
		t.Fatalf("ResolveAction failed: res=%+v err=%v", resolved, err)
	}
	if _, err := eng.ResolveAction(ctx, act.ID, "wecom", "2"); !errors.Is(err, store.ErrActionAlreadyResolved) {
		t.Fatalf("expected ErrActionAlreadyResolved on duplicate click, got %v", err)
	}

	snapAfter := eng.Snapshot(ctx, model.MainThread)
	if len(snapAfter.Actions) != 0 {
		t.Fatalf("expected 0 pending actions after resolve, got %+v", snapAfter.Actions)
	}
	ra := snapAfter.ResolvedActions[act.ID]
	if ra == nil || ra.Status != model.ActionResolved || ra.ResolvedBy != "web" || len(ra.Choices) != 3 || ra.EntryID != qEntry.ID {
		t.Fatalf("unexpected ResolvedActions[%s]: %+v", act.ID, ra)
	}
	var receiptEntries []*model.Entry
	for _, en := range snapAfter.Entries {
		if en.Role == "user" && en.ActionID == act.ID {
			receiptEntries = append(receiptEntries, en)
		}
	}
	if len(receiptEntries) != 1 {
		t.Fatalf("expected exactly 1 receipt user entry (no duplicate on double click), got %d", len(receiptEntries))
	}
	receiptEntry := receiptEntries[0]
	if receiptEntry.Text != "先分别出两份方案" || receiptEntry.Re == nil || receiptEntry.Re.Entry != qEntry.ID {
		t.Fatalf("receipt entry missing origin quote link: %+v", receiptEntry)
	}

	// 4. Follow-up supplement replying to the receipt entry carries BOTH chosen answer and original question.
	fuEntry, fuTrig, err := eng.Say(ctx, SayRequest{
		Thread:    model.MainThread,
		Channel:   "web",
		From:      "you",
		Text:      "补充：两份方案都限制在只读调研",
		ReEntryID: receiptEntry.ID,
	})
	if err != nil || fuEntry == nil || fuTrig == nil {
		t.Fatalf("follow-up Say failed: entry=%v trig=%v err=%v", fuEntry, fuTrig, err)
	}
	if fuEntry.Re == nil || fuEntry.Re.Entry != receiptEntry.ID ||
		!strings.Contains(fuEntry.Re.Text, "先分别出两份方案") ||
		!strings.Contains(fuEntry.Re.Text, "两个需求可以并行推进") {
		t.Fatalf("follow-up quote should preserve both choice and question, got %+v", fuEntry.Re)
	}
	if !strings.Contains(fuTrig.Text, "先分别出两份方案") || !strings.Contains(fuTrig.Text, "两个需求可以并行推进") {
		t.Fatalf("follow-up trigger missing decision context: %q", fuTrig.Text)
	}

	// 5. Worker dialog approval & snooze events also attach ActionID and surface in ResolvedActions.
	dialogAct := &model.Action{
		ID:           "a-dlg-1",
		Kind:         model.ActionKindWorkerDialog,
		Thread:       model.MainThread,
		TargetWorker: "agy-85",
		Title:        "推送主干标签",
		Summary:      "批准执行 git push",
		Command:      "git push origin main",
		Choices: []model.ActionChoice{
			{Key: "1", Label: "允许本次"},
			{Key: "2", Label: "始终允许"},
		},
		Status:    model.ActionPending,
		CreatedAt: time.Now(),
	}
	if err := st.InsertAction(ctx, dialogAct); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveAction(ctx, dialogAct.ID, "web", "1"); err != nil {
		t.Fatalf("resolve worker dialog failed: %v", err)
	}

	_, snoozeAct, _, err := eng.Reply(ctx, model.MainThread, "明天再看的巡检事项", "", []string{"现在处理", "留着"}, false)
	if err != nil || snoozeAct == nil {
		t.Fatalf("Reply for snooze failed: %v", err)
	}
	if _, err := eng.SnoozeAction(ctx, snoozeAct.ID, time.Now().Add(12*time.Hour)); err != nil {
		t.Fatalf("SnoozeAction failed: %v", err)
	}

	snapFinal := eng.Snapshot(ctx, model.MainThread)
	if len(snapFinal.Actions) != 0 {
		t.Fatalf("expected 0 pending actions after dialog resolve & snooze, got %+v", snapFinal.Actions)
	}
	if got := snapFinal.ResolvedActions[dialogAct.ID]; got == nil || got.Status != model.ActionResolved || got.Command != "git push origin main" {
		t.Fatalf("expected resolved worker dialog in ResolvedActions, got %+v", got)
	}
	if got := snapFinal.ResolvedActions[snoozeAct.ID]; got == nil || got.Status != model.ActionSnoozed {
		t.Fatalf("expected snoozed action in ResolvedActions, got %+v", got)
	}
	hasDialogEvent, hasSnoozeEvent := false, false
	for _, en := range snapFinal.Entries {
		if en.Role == "event" && en.ActionID == dialogAct.ID {
			hasDialogEvent = true
		}
		if en.Role == "event" && en.ActionID == snoozeAct.ID {
			hasSnoozeEvent = true
		}
	}
	if !hasDialogEvent || !hasSnoozeEvent {
		t.Fatalf("expected event entries linked to action_id: dialog=%v snooze=%v", hasDialogEvent, hasSnoozeEvent)
	}
}

func TestDefaultRotatePctAndNormalization(t *testing.T) {
	if got := model.DefaultSettings("").RotatePct; got != 90 {
		t.Fatalf("DefaultSettings().RotatePct = %d, want 90", got)
	}

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

	if snap := eng.Snapshot(ctx, model.MainThread); snap.Router.RotateAt != 90 {
		t.Fatalf("initial Snapshot Router.RotateAt = %d, want 90", snap.Router.RotateAt)
	}

	cases := []struct {
		name         string
		input        int
		wantRotateAt int
	}{
		{name: "zero falls back to 90", input: 0, wantRotateAt: 90},
		{name: "negative disables handover", input: -1, wantRotateAt: 0},
		{name: "valid explicit value kept", input: 75, wantRotateAt: 75},
		{name: "below min resets to default 90", input: 10, wantRotateAt: 90},
		{name: "above max resets to default 90", input: 99, wantRotateAt: 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := eng.Settings()
			cfg.RotatePct = tc.input
			if err := eng.SaveConfig(ctx, &cfg, nil); err != nil {
				t.Fatalf("SaveConfig(%d): %v", tc.input, err)
			}
			if got := eng.Snapshot(ctx, model.MainThread).Router.RotateAt; got != tc.wantRotateAt {
				t.Fatalf("RotateAt after SaveConfig(%d) = %d, want %d", tc.input, got, tc.wantRotateAt)
			}
		})
	}
}

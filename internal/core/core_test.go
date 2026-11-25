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

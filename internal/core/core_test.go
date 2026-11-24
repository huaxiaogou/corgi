package core

import (
	"context"
	"errors"
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

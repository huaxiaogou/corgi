package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func openTestCoreEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng, err := NewEngine(context.Background(), st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestResolveWorkerModelPrecedence(t *testing.T) {
	st := model.Settings{
		WorkerKind:   "agy",
		WorkerModel:  "gemini-4-neon",
		WorkerEffort: "xhigh",
		KindDefaults: map[string]model.ModelEffortSpec{
			"agy":       {Model: "gemini-4-neon", Effort: "xhigh"},
			"codebuddy": {Model: "claude-sonnet-5.5", Effort: "high"},
			"codex":     {Model: "gpt-6.2-astra", Effort: "xhigh"},
		},
		MachineKindOverrides: map[string]map[string]model.ModelEffortSpec{
			"hub": {
				"codebuddy": {Model: "claude-opus-5.5", Effort: "high"},
			},
		},
	}

	// 1. Explicit override wins over everything.
	m, ef, src := ResolveWorkerModel(st, "", "codebuddy", "gpt-6-luna", "low")
	if m != "gpt-6-luna" || ef != "low" || src != ModelSourceExplicit {
		t.Fatalf("explicit mismatch: got (%q, %q, %q)", m, ef, src)
	}

	// 2. Machine x kind override beats kind default on Hub.
	m, ef, src = ResolveWorkerModel(st, "", "codebuddy", "", "")
	if m != "claude-opus-5.5" || ef != "high" || src != ModelSourceMachineOverride {
		t.Fatalf("machine_override mismatch: got (%q, %q, %q)", m, ef, src)
	}

	// 3. Kind default applies on another machine where no machine override exists.
	m, ef, src = ResolveWorkerModel(st, "mac-1", "codebuddy", "", "")
	if m != "claude-sonnet-5.5" || ef != "high" || src != ModelSourceKindDefault {
		t.Fatalf("kind_default mismatch: got (%q, %q, %q)", m, ef, src)
	}

	// 4. Unconfigured kind explicitly reports cli_default rather than pretending to inherit WorkerKind.
	m, ef, src = ResolveWorkerModel(st, "mac-1", "claude", "", "")
	if m != "" || ef != "" || src != ModelSourceCLIDefault {
		t.Fatalf("cli_default mismatch: got (%q, %q, %q)", m, ef, src)
	}

	// 5. Partial effort-only explicit override preserves the resolved model.
	m, ef, src = ResolveWorkerModel(st, "mac-1", "codex", "", "medium")
	if m != "gpt-6.2-astra" || ef != "medium" || src != ModelSourceExplicit {
		t.Fatalf("effort-only explicit mismatch: got (%q, %q, %q)", m, ef, src)
	}
}

func TestNormalizeDispatchConfigAndProjects(t *testing.T) {
	st := model.Settings{
		WorkerKind:   "agy",
		WorkerModel:  "gemini-4-neon",
		WorkerEffort: "xhigh",
		Projects: []model.ProjectWorkspace{
			{
				Name:    " 物言 Agent ",
				Aliases: []string{"物言", "wuyan", "物言"},
				Note:    "只读调研默认不改 Nacos",
				Paths: map[string]string{
					"":      "/root/wuyan",
					"mac-1": "/Users/me/wuyan-group",
				},
			},
		},
	}
	NormalizeDispatchConfig(&st)

	if st.KindDefaults["agy"].Model != "gemini-4-neon" || st.KindDefaults["agy"].Effort != "xhigh" {
		t.Fatalf("expected legacy WorkerModel/Effort backfilled into KindDefaults, got %+v", st.KindDefaults)
	}
	if len(st.Projects) != 1 || st.Projects[0].ID == "" {
		t.Fatalf("unexpected normalized projects: %+v", st.Projects)
	}
	proj := ResolveProject(st.Projects, "", "帮我看下物言的生图链路")
	if proj == nil || proj.Name != "物言 Agent" {
		t.Fatalf("expected alias match for 物言, got %+v", proj)
	}
	if p := ProjectPathForMachine(proj, ""); p != "/root/wuyan" {
		t.Fatalf("expected hub path /root/wuyan, got %q", p)
	}
	if p := ProjectPathForMachine(proj, "mac-1"); p != "/Users/me/wuyan-group" {
		t.Fatalf("expected mac-1 path /Users/me/wuyan-group, got %q", p)
	}
}

func TestWorkerReportSnapshotAutoAttachAndDedupe(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()

	wi, err := eng.SetWorkItem(ctx, "new", model.MainThread, "数据流可视化方案", "待调研", "flow-plan", "")
	if err != nil {
		t.Fatal(err)
	}
	wk := &model.Worker{
		Name:       "flow-plan",
		Kind:       "agy",
		Machine:    "mac-1",
		Cwd:        "/Users/me/wuyan",
		Thread:     model.MainThread,
		Model:      "gemini-4-neon",
		Effort:     "xhigh",
		Session:    "sess-flow-1",
		WorkItemID: wi.ID,
		Status:     "done",
	}
	if err := eng.store.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}

	longReport := "# 物言 Agent 内部数据流可视化方案\n\n" + strings.Repeat("这是完整方案报告的详细内容与源码核验依据。", 60)
	trig, err := eng.Report(ctx, wk.Name, "", longReport)
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if !strings.Contains(trig.Text, "full_report_artifact:") {
		t.Fatalf("expected full_report_artifact marker in router trigger, got: %s", trig.Text)
	}

	updatedWk, err := eng.store.GetWorker(ctx, wk.Name)
	if err != nil || updatedWk.LastReportArtifactID == "" {
		t.Fatalf("expected LastReportArtifactID recorded on worker, got %+v err=%v", updatedWk, err)
	}
	firstArtID := updatedWk.LastReportArtifactID

	// Reporting identical text must deduplicate without creating a second artifact.
	if got := eng.captureWorkerReportSnapshot(ctx, updatedWk, longReport, 500); got != firstArtID {
		t.Fatalf("expected deduped artifact id %s, got %s", firstArtID, got)
	}

	// Verify WorkItem (小本本) immediately received the report artifact even before Router replies.
	updatedWI, err := eng.store.GetWorkItem(ctx, wi.ID)
	if err != nil || len(updatedWI.Artifacts) != 1 || updatedWI.Artifacts[0].ArtifactID != firstArtID {
		t.Fatalf("expected WorkItem to hold report artifact %s, got %+v", firstArtID, updatedWI)
	}

	// First Router reply referencing worker auto-attaches the full report & binds WorkerRef.
	res1, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "flow-plan 已完成方案调研，核心结论如下。",
		Worker: wk.Name,
		ReID:   trig.ID,
	})
	if err != nil {
		t.Fatalf("ReplyDetailed failed: %v", err)
	}
	if res1.Entry.WorkerRef == nil || res1.Entry.WorkerRef.Session != "sess-flow-1" || res1.Entry.WorkerRef.Machine != "mac-1" {
		t.Fatalf("expected WorkerRef bound to exact session+machine, got %+v", res1.Entry.WorkerRef)
	}
	if len(res1.Entry.Artifacts) != 1 || res1.Entry.Artifacts[0].ID != firstArtID {
		t.Fatalf("expected full report artifact auto-attached on first reply, got %+v", res1.Entry.Artifacts)
	}

	// Second casual reply referencing the same worker must NOT re-attach the already-attached report.
	res2, err := eng.ReplyDetailed(ctx, ReplyRequest{
		Thread: model.MainThread,
		Text:   "好的，有新问题随时说。",
		Worker: wk.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Entry.Artifacts) != 0 {
		t.Fatalf("expected no duplicate report attachment on second reply, got %+v", res2.Entry.Artifacts)
	}
}

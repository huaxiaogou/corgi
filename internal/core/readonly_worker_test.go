package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestReadonlyWorkerTieredSandboxLaunchResumeAndBypassGuard(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	var (
		mu          sync.Mutex
		spawnSeq    int
		startedByID = map[string][]string{}
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "workspace.create":
			spawnSeq++
			wsID := fmt.Sprintf("w-ro-%d", spawnSeq)
			return map[string]any{
				"workspace": map[string]any{"workspace_id": wsID},
				"root_pane": map[string]any{"pane_id": wsID + ":p1"},
			}, nil
		case "agent.start":
			name, _ := params["name"].(string)
			var argv []string
			if raw, ok := params["args"].([]any); ok {
				for _, a := range raw {
					if s, ok := a.(string); ok {
						argv = append(argv, s)
					}
				}
			}
			startedByID[name] = argv
			return map[string]any{
				"pane_id": fmt.Sprintf("w-ro-%d:p1", spawnSeq), "workspace_id": fmt.Sprintf("w-ro-%d", spawnSeq),
				"session": "sess-" + name, "pid": 5000 + spawnSeq,
			}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"codex", "claude", "agy", "codebuddy"})

	cases := []struct {
		kind        string
		wantTier    string
		wantLabel   string
		wantContain []string
		wantAbsent  []string
	}{
		{
			kind:        "codex",
			wantTier:    string(agent.ReadonlyEnforcementHardSandbox),
			wantLabel:   "硬沙箱",
			wantContain: []string{"--sandbox", "read-only", "--ask-for-approval", "never"},
			wantAbsent:  []string{"--dangerously-bypass-approvals-and-sandbox"},
		},
		{
			kind:        "claude",
			wantTier:    string(agent.ReadonlyEnforcementPermissionMode),
			wantLabel:   "只读权限模式",
			wantContain: []string{"--permission-mode", "plan", "--disallowedTools", "Edit,Write,NotebookEdit", "--allowedTools"},
			wantAbsent:  []string{"--dangerously-skip-permissions"},
		},
		{
			kind:        "agy",
			wantTier:    string(agent.ReadonlyEnforcementSoftPrompt),
			wantLabel:   "软约束",
			wantContain: []string{"--dangerously-skip-permissions"},
			wantAbsent:  []string{"--sandbox", "--mode"},
		},
		{
			kind:        "codebuddy",
			wantTier:    string(agent.ReadonlyEnforcementSoftPrompt),
			wantLabel:   "软约束",
			wantContain: []string{"--dangerously-skip-permissions"},
			wantAbsent:  []string{"--sandbox", "--permission-mode"},
		},
	}

	var codexWorkerName string
	for _, tc := range cases {
		t.Run("dispatch_readonly_"+tc.kind, func(t *testing.T) {
			trig, err := eng.Ingest(ctx, "worker", model.MainThread, "w-src", "后台只读排查", "", model.RouterName, "w-src")
			if err != nil {
				t.Fatal(err)
			}
			out := eng.toolDispatchWork(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}, trig, eng.resolveTurnConsentMeta(ctx, trig),
				fmt.Sprintf(`{"mode":"readonly","kind":%q,"goal":"只读排查服务配置"}`, tc.kind))
			var resp struct {
				OK                  bool   `json:"ok"`
				Worker              string `json:"worker"`
				ReadonlyEnforcement string `json:"readonly_enforcement"`
				ReadonlyLabel       string `json:"readonly_label"`
			}
			if err := json.Unmarshal([]byte(out), &resp); err != nil || !resp.OK {
				t.Fatalf("dispatch %s readonly failed: %s", tc.kind, out)
			}
			if resp.ReadonlyEnforcement != tc.wantTier || resp.ReadonlyLabel != tc.wantLabel {
				t.Fatalf("%s: got enforcement=%q label=%q, want %q/%q", tc.kind, resp.ReadonlyEnforcement, resp.ReadonlyLabel, tc.wantTier, tc.wantLabel)
			}
			if tc.kind == "codex" {
				codexWorkerName = resp.Worker
			}
			wk, err := st.GetWorker(ctx, resp.Worker)
			if err != nil || wk.Mode != "readonly" || wk.ReadonlyEnforcement != tc.wantTier {
				t.Fatalf("persisted worker mismatch: %+v err=%v", wk, err)
			}

			mu.Lock()
			argv := append([]string(nil), startedByID[resp.Worker]...)
			mu.Unlock()
			for _, want := range tc.wantContain {
				if !slices.Contains(argv, want) {
					t.Fatalf("%s readonly args %v missing %q", tc.kind, argv, want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if slices.Contains(argv, bad) {
					t.Fatalf("%s readonly args %v must not contain %q", tc.kind, argv, bad)
				}
			}

			// Verify startup event annotates readonly enforcement label.
			entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
			foundEv := false
			for _, en := range entries {
				if en.Role == "event" && en.Ref == resp.Worker && strings.Contains(en.Text, "只读("+tc.wantLabel+")") {
					foundEv = true
					break
				}
			}
			if !foundEv {
				t.Fatalf("%s: expected spawn event containing 只读(%s), entries=%+v", tc.kind, tc.wantLabel, entries)
			}
			// Verify live tail state exposes readonly enforcement label.
			tail := eng.buildLiveTailState(ctx, model.MainThread, trig, nil)
			if !strings.Contains(tail, "readonly="+tc.wantLabel) {
				t.Fatalf("%s: expected live tail state to contain readonly=%s, got:\n%s", tc.kind, tc.wantLabel, tail)
			}
		})
	}

	// Verify ResumeWorkerSession preserves readonly mode & Codex sandbox args after worker stops.
	if codexWorkerName == "" {
		t.Fatal("missing codex worker name")
	}
	wkCodex, err := st.GetWorker(ctx, codexWorkerName)
	if err != nil {
		t.Fatal(err)
	}
	wkCodex.Session = "sess-codex-ro"
	wkCodex.Ended = true
	wkCodex.Status = "done"
	if err := st.UpsertWorker(ctx, wkCodex); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	delete(startedByID, codexWorkerName)
	mu.Unlock()
	resumed, _, err := eng.ResumeWorkerSession(ctx, ResumeWorkerRequest{Name: codexWorkerName})
	if err != nil {
		t.Fatalf("ResumeWorkerSession(%s): %v", codexWorkerName, err)
	}
	if resumed.Mode != "readonly" || resumed.ReadonlyEnforcement != string(agent.ReadonlyEnforcementHardSandbox) {
		t.Fatalf("resumed worker lost readonly metadata: %+v", resumed)
	}
	mu.Lock()
	resumedArgs := append([]string(nil), startedByID[codexWorkerName]...)
	mu.Unlock()
	if !slices.Contains(resumedArgs, "--sandbox") || !slices.Contains(resumedArgs, "read-only") || slices.Contains(resumedArgs, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("resumed codex readonly args lost sandbox isolation: %v", resumedArgs)
	}

	// Verify answerRestoredAgyEditDialog refuses auto-accepting file edits on readonly agy worker.
	roAgy := &model.Worker{Name: "agy-ro-guard", Kind: "agy", Mode: "readonly", PaneID: "w-ro-x:p1", Status: "blocked"}
	ap := model.Approval{
		Title:   "Accept this file edit?",
		Detail:  "shift+tab to auto-approve file edits",
		Choices: []model.Choice{{Key: "1", Label: "Yes, accept this change"}, {Key: "2", Label: "No"}},
	}
	if eng.answerRestoredAgyEditDialog(ctx, roAgy, ap) {
		t.Fatal("readonly agy worker must NEVER auto-accept file edits")
	}
}

func TestBypassHealPreservesReadonlyCodexSandbox(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)
	eng.bypassCheckInterval = 5 * time.Millisecond
	eng.bypassExitPoll = 5 * time.Millisecond
	eng.bypassExitTimeout = 500 * time.Millisecond
	eng.bypassStartRetryTimeout = 500 * time.Millisecond

	var (
		mu          sync.Mutex
		phase       = "restored"
		startedArgs []string
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "agent.list":
			if phase == "shell" {
				return map[string]any{"agents": []any{}}, nil
			}
			return map[string]any{"agents": []any{map[string]any{
				"pane_id": "w9:p1", "workspace_id": "w9", "name": "codex-ro",
				"agent": "codex", "agent_status": "idle", "interactive_ready": true,
			}}}, nil
		case "pane.process_info":
			switch phase {
			case "restored":
				return map[string]any{"process_info": map[string]any{
					"pane_id": "w9:p1", "shell_pid": 100, "foreground_process_group_id": 200,
					"foreground_processes": []any{map[string]any{
						"pid": 200, "name": "codex", "argv": []string{"codex", "resume", "sess-ro-1"},
					}},
				}}, nil
			case "shell":
				return map[string]any{"process_info": map[string]any{
					"pane_id": "w9:p1", "shell_pid": 100, "foreground_process_group_id": 100,
					"foreground_processes": []any{map[string]any{"pid": 100, "name": "zsh", "argv0": "-zsh"}},
				}}, nil
			default:
				return map[string]any{"process_info": map[string]any{
					"pane_id": "w9:p1", "shell_pid": 100, "foreground_process_group_id": 300,
					"foreground_processes": []any{map[string]any{
						"pid": 300, "name": "codex", "argv": append([]string{"codex"}, startedArgs...),
					}},
				}}, nil
			}
		case "pane.send_text":
			if txt, _ := params["text"].(string); txt == "/exit" {
				phase = "shell"
			}
			return map[string]any{}, nil
		case "agent.start":
			startedArgs = nil
			if raw, ok := params["args"].([]any); ok {
				for _, a := range raw {
					if s, ok := a.(string); ok {
						startedArgs = append(startedArgs, s)
					}
				}
			}
			phase = "healed"
			return map[string]any{"type": "agent_started"}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"codex"})

	wk := &model.Worker{
		Name: "codex-ro", Kind: "codex", Mode: "readonly", ReadonlyEnforcement: string(agent.ReadonlyEnforcementHardSandbox),
		Cwd: dir, Thread: model.MainThread, PaneID: "w9:p1", Status: "idle", Session: "sess-ro-1", CreatedAt: time.Now().Add(-5 * time.Minute),
	}
	if err := st.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}
	eng.syncWorkers(ctx, map[string]liveInfo{
		"codex-ro": {pane: "w9:p1", status: "idle", ready: true, kind: "codex", session: "sess-ro-1"},
	}, map[string]bool{"": true}, time.Now())

	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return phase == "healed" && !eng.isHealingBypassPane("w9:p1")
	}, "readonly codex heal finished")

	mu.Lock()
	gotArgs := append([]string(nil), startedArgs...)
	mu.Unlock()
	if !slices.Contains(gotArgs, "--sandbox") || !slices.Contains(gotArgs, "read-only") || !slices.Contains(gotArgs, "--ask-for-approval") || !slices.Contains(gotArgs, "never") {
		t.Fatalf("expected readonly codex healed with read-only sandbox + never approval, got %v", gotArgs)
	}
	if slices.Contains(gotArgs, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("readonly codex must NEVER be escalated to bypass-approvals-and-sandbox: %v", gotArgs)
	}
}

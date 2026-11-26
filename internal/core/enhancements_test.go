package core

import (
	"context"
	"encoding/json"
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

	// 2. Unquoted human reply auto-resolves latest pending router_choice, but NOT worker_dialog.
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
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "先等我跑完回归再发"}); err != nil {
		t.Fatal(err)
	}
	gotChoice, _ := st.GetAction(ctx, choiceAct.ID)
	if gotChoice.Status != model.ActionResolved || gotChoice.ChosenKey != "reply" {
		t.Fatalf("expected unquoted reply to auto-resolve router_choice, got %+v", gotChoice)
	}
	gotDlg, _ := st.GetAction(ctx, dlgAct.ID)
	if gotDlg.Status != model.ActionPending {
		t.Fatalf("worker_dialog must stay pending on normal chat, got %+v", gotDlg)
	}

	// 3. Explicit quoted reply resolves ONLY the quoted action, leaving newer ones intact.
	eOld, actOld, _, _ := eng.Reply(ctx, model.MainThread, "问题 A", "", []string{"A1", "A2"}, false)
	_, actNew, _, _ := eng.Reply(ctx, model.MainThread, "问题 B", "", []string{"B1", "B2"}, false)
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "按 A1 办", ReEntryID: eOld.ID}); err != nil {
		t.Fatal(err)
	}
	gotOld, _ := st.GetAction(ctx, actOld.ID)
	gotNew, _ := st.GetAction(ctx, actNew.ID)
	if gotOld.Status != model.ActionResolved {
		t.Fatalf("quoted action should resolve: %+v", gotOld)
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

func TestClaudePermissionsAllowlistAndRouterRunPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Seed a custom user rule in .claude/settings.json to confirm merge preservation.
	claudeDir := filepath.Join(dir, "router", ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"theme":"dark","permissions":{"allow":["Bash(git status:*)"]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	mem := NewMemoryManager(dir)
	if err := mem.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := mem.WriteRouterDocs("/usr/local/bin/corgi", model.DefaultSettings(dir)); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("read .claude/settings.json: %v", err)
	}
	var parsed struct {
		Theme       string `json:"theme"`
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	if parsed.Theme != "dark" {
		t.Fatalf("existing key lost: theme=%q", parsed.Theme)
	}
	for _, want := range []string{"Bash(git status:*)", "Read(memory/**)", "Edit(memory/**)", "Bash(/usr/local/bin/corgi:*)", "Bash(corgi:*)"} {
		if !slices.Contains(parsed.Permissions.Allow, want) {
			t.Fatalf("missing permission rule %q in %v", want, parsed.Permissions.Allow)
		}
	}

	// Verify router_run persists in SQLite kv and reloads on fresh NewEngine.
	ctx := context.Background()
	b, _ := json.Marshal([2]string{"gpt-6-luna", "low"})
	if err := st.SetKV(ctx, "router_run", string(b)); err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(ctx, st, fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"}), agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eng.routerRun == nil || *eng.routerRun != [2]string{"gpt-6-luna", "low"} {
		t.Fatalf("expected persisted routerRun reloaded, got %+v", eng.routerRun)
	}
}

func TestRouterScopedErrorScanDedupAndNonDestructiveWatchdog(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var (
		mu       sync.Mutex
		screen   string
		prompts  []string
		keysSent []string
		closedWS int
	)
	hc := newFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "pane.read":
			return map[string]any{"read": map[string]any{"text": screen}}, nil
		case "agent.prompt":
			prompts = append(prompts, params["text"].(string))
		case "pane.send_keys":
			keysSent = append(keysSent, "esc")
		case "workspace.list":
			return map[string]any{
				"workspaces": []map[string]any{{"workspace_id": "w0", "label": model.RouterLabel}},
			}, nil
		case "workspace.close":
			closedWS++
		}
		return map[string]any{}, nil
	})
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})
	ctx := context.Background()
	eng, err := NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.routerMinSilentAge = -1
	eng.routerRetryDelays = []time.Duration{5 * time.Millisecond}
	eng.mu.Unlock()

	// 1. Stale error ABOVE our latest prompt marker must be ignored!
	mu.Lock()
	screen = "API Error: 429 rate limit exceeded\n[corgi] 1 new event\nReply with `corgi reply`.\nAll handled cleanly.\n"
	mu.Unlock()
	eng.mu.Lock()
	eng.routerAskedAt = time.Now().Add(-10 * time.Second)
	eng.mu.Unlock()
	eng.inspectRouterSilentTurn(ctx, "w0:p1", 0)
	time.Sleep(25 * time.Millisecond)
	mu.Lock()
	if len(prompts) != 0 {
		t.Fatalf("stale error above prompt marker should NOT trigger retry, got prompts=%v", prompts)
	}
	mu.Unlock()

	// 1b. Fresh error AFTER our latest prompt marker DOES schedule a retry!
	mu.Lock()
	screen = "Reply with `corgi reply`.\nAPI Error: 429 rate limit exceeded\n"
	mu.Unlock()
	eng.mu.Lock()
	eng.routerAskedAt = time.Now().Add(-10 * time.Second)
	eng.mu.Unlock()
	eng.inspectRouterSilentTurn(ctx, "w0:p1", 0)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(prompts)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "retry 1/1") {
		t.Fatalf("expected 1 retry prompt for post-prompt error, got %v", prompts)
	}
	mu.Unlock()

	// 2. Identical final error after retries exhausted is deduplicated within 10m.
	mu.Lock()
	screen = "Reply with `corgi reply`.\nAPI Error: 529 overloaded_error\n"
	mu.Unlock()
	eng.inspectRouterSilentTurn(ctx, "w0:p1", 1)
	eng.inspectRouterSilentTurn(ctx, "w0:p1", 1)
	entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
	errEvents := 0
	for _, en := range entries {
		if en.Role == "event" && en.Tone == "error" && strings.Contains(en.Text, "529") {
			errEvents++
		}
	}
	if errEvents != 1 {
		t.Fatalf("expected final error notice deduplicated to 1, got %d", errEvents)
	}

	// 3. Watchdog respects StallMinutes=12 (does NOT trigger at 5m) and on 2nd strike alerts WITHOUT RestartRouter.
	mu.Lock()
	screen = "thinking static output..."
	mu.Unlock()
	t0 := time.Now()
	live := map[string]liveInfo{model.RouterName: {pane: "w0:p1", status: "working", kind: "claude"}}
	eng.handleRouterLifecycle(ctx, live, t0) // seed screen hash
	// At +5m (< 12m): must NOT nudge!
	eng.handleRouterLifecycle(ctx, live, t0.Add(5*time.Minute))
	mu.Lock()
	if len(keysSent) != 0 {
		t.Fatalf("watchdog should respect StallMinutes=12 and not nudge at 5m, got keys=%v", keysSent)
	}
	mu.Unlock()
	// At +13m (>= 12m): 1st strike nudges.
	eng.handleRouterLifecycle(ctx, live, t0.Add(13*time.Minute))
	mu.Lock()
	if len(keysSent) != 1 {
		t.Fatalf("expected 1st-strike nudge at 13m, got keys=%v", keysSent)
	}
	mu.Unlock()
	// At +26m (2nd strike): alerts human WITHOUT closing/restarting Router workspace!
	eng.handleRouterLifecycle(ctx, live, t0.Add(26*time.Minute))
	mu.Lock()
	if closedWS != 0 {
		t.Fatalf("2nd-strike watchdog must NOT destructively restart Router, closedWS=%d", closedWS)
	}
	mu.Unlock()
	entries2, _, _ := st.ListEntries(ctx, model.MainThread, 30)
	foundAlert := false
	for _, en := range entries2 {
		if en.Role == "event" && en.Tone == "error" && strings.Contains(en.Text, "持续无响应") {
			foundAlert = true
		}
	}
	if !foundAlert {
		t.Fatalf("expected human alert event on 2nd-strike stall")
	}
}

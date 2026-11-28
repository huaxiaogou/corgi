package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestNormalizeProjectsMoreThanTenSameName(t *testing.T) {
	raw := make([]model.ProjectWorkspace, 15)
	for i := range raw {
		raw[i] = model.ProjectWorkspace{Name: "Billing App"}
	}
	out := NormalizeProjects(raw)
	if len(out) != 15 {
		t.Fatalf("expected 15 projects, got %d", len(out))
	}
	seen := map[string]bool{}
	for _, p := range out {
		if seen[p.ID] {
			t.Fatalf("duplicate project ID: %s", p.ID)
		}
		seen[p.ID] = true
	}
	if out[10].ID != "billing-app-11" {
		t.Fatalf("expected 11th project ID billing-app-11, got %s", out[10].ID)
	}
}

func TestValidateProjectDirAndSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	corgiDir := filepath.Join(home, "custom-corgi-state")
	if err := os.MkdirAll(corgiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	validRepo := filepath.Join(home, "work", "billing")
	if err := os.MkdirAll(filepath.Join(validRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	badLink := filepath.Join(home, "work", "evil-link")
	if err := os.Symlink("/etc", badLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	dbFile := filepath.Join(corgiDir, "corgi.db")
	_ = os.WriteFile(dbFile, []byte("sqlite"), 0o600)

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty", "", true},
		{"bare_tilde", "~", true},
		{"tilde_user", "~root/repo", true},
		{"relative", "work/billing", true},
		{"dotdot_abs", "/tmp/a/../etc", true},
		{"dotdot_tilde", "~/work/../../etc", true},
		{"control_char", "/tmp/a\nb", true},
		{"root", "/", true},
		{"etc", "/etc", true},
		{"private_etc", "/private/etc/ssh", true},
		{"usr_bin", "/usr/bin", true},
		{"var_exact", "/var", true},
		{"var_db", "/var/db", true},
		{"private_var_db", "/private/var/db", true},
		{"system", "/System/Library", true},
		{"ssh_seg", "~/work/.ssh/keys", true},
		{"corgi_seg", "~/.corgi/worktrees", true},
		{"custom_corgi_dir", corgiDir, true},
		{"custom_corgi_sub", filepath.Join(corgiDir, "uploads"), true},
		{"valid_tilde", "~/work/billing", false},
		{"valid_temp_dir", validRepo, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateProjectDir(tc.path, corgiDir)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.path, err)
			}
		})
	}

	res, err := InspectLocalProjectDir(home, corgiDir, dbFile, "~/work/billing")
	if err != nil {
		t.Fatalf("InspectLocalProjectDir valid repo failed: %v", err)
	}
	if !res.Exists || !res.IsDir || !res.IsGit {
		t.Fatalf("expected existing git dir, got %+v", res)
	}
	if _, err := InspectLocalProjectDir(home, corgiDir, dbFile, badLink); err == nil {
		t.Fatalf("expected symlink to /etc to be rejected")
	}
}

func TestConcurrentUpsertProjectAndSettingsRead(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)

	const workers = 20
	repos := make([]string, workers)
	for i := range workers {
		p := filepath.Join(dir, fmt.Sprintf("repo-%02d", i))
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		repos[i] = p
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Concurrently mutate settings and read cloned snapshots to stress -race.
			st := eng.Settings()
			if st.KindDefaults == nil {
				st.KindDefaults = map[string]model.ModelEffortSpec{}
			}
			st.KindDefaults["claude"] = model.ModelEffortSpec{Model: "mutated-copy"}

			_, err := eng.UpsertProject(ctx, "test", UpsertProjectRequest{
				Name:    fmt.Sprintf("proj-%02d", idx),
				Machine: "hub",
				Path:    repos[idx],
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent UpsertProject failed: %v", err)
	}
	final := eng.Settings()
	if len(final.Projects) != workers {
		t.Fatalf("expected %d projects persisted without lost updates, got %d", workers, len(final.Projects))
	}
	if final.KindDefaults["claude"].Model == "mutated-copy" {
		t.Fatalf("caller mutation leaked into Engine settings (Settings() must deep-copy)")
	}
}

func TestApproveWeComUserConcurrentAndOrderedHook(t *testing.T) {
	ctx := context.Background()
	eng, _, _, _ := newTestAPIEngine(t, nil)

	var (
		mu         sync.Mutex
		lastHooked []string
	)
	eng.SetOnSettingsApplied(func(st model.Settings) {
		mu.Lock()
		lastHooked = append([]string(nil), st.WeComAllowUsers...)
		mu.Unlock()
	})

	const total = 10
	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if _, err := eng.ApproveWeComUser(ctx, fmt.Sprintf("user-%02d", idx)); err != nil {
				t.Errorf("ApproveWeComUser(%d): %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	final := eng.Settings().WeComAllowUsers
	if len(final) != total {
		t.Fatalf("expected %d allowlisted users, got %d (%v)", total, len(final), final)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(lastHooked, final) {
		t.Fatalf("onSettingsApplied ended out-of-order: hooked=%v final=%v", lastHooked, final)
	}
}

func TestManageConfigStage1AStrategy1AAndRedaction(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	// Seed sensitive credentials that must NEVER leak via manage_config(get).
	cur := eng.Settings()
	cur.WeComEnabled = true
	cur.WeComBotID = "bot-123"
	cur.WeComSecret = "wecom-super-secret-xyz987"
	if err := eng.SaveConfig(ctx, &cur, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SetRouterAPIKey(ctx, RouterAPIProviderTokenHub, "", "sk-router-secret-abc12345"); err != nil {
		t.Fatal(err)
	}

	repoHub := filepath.Join(dir, "billing-hub")
	if err := os.MkdirAll(repoHub, 0o700); err != nil {
		t.Fatal(err)
	}
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			return map[string]any{"workspace": map[string]any{"workspace_id": "w1"}, "root_pane": map[string]any{"pane_id": "w1:p1"}}, nil
		case "agent.start":
			return map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "session": "s1", "pid": 42}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	// 1. Group thread MUST reject even read-only get & history.
	groupTurn := &apiRouterTurn{thread: "wecom:grp1", triggerID: "t-grp"}
	if out, _ := eng.toolManageConfig(ctx, groupTurn, &model.Trigger{ID: "t-grp", Thread: "wecom:grp1"}, &turnConsentMeta{isHuman: true}, `{"action":"get"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected group get rejected, got %s", out)
	}

	// 2. Main thread non-human turn allows redacted get, rejects writes.
	bgTrig := &model.Trigger{ID: "t-bg", Thread: model.MainThread, Source: "system", Policy: "respond", From: "corgi", Ref: "idle-patrol"}
	bgMeta := eng.resolveTurnConsentMeta(ctx, bgTrig)
	bgTurn := &apiRouterTurn{thread: model.MainThread, triggerID: bgTrig.ID}
	getOut, stop := eng.toolManageConfig(ctx, bgTurn, bgTrig, bgMeta, `{"action":"get"}`)
	if stop || !strings.Contains(getOut, `"ok":true`) {
		t.Fatalf("expected main thread bg get ok, got stop=%v out=%s", stop, getOut)
	}
	for _, secret := range []string{"sk-router-secret-abc12345", "abc12345", "wecom-super-secret-xyz987", "xyz987"} {
		if strings.Contains(getOut, secret) {
			t.Fatalf("secret fragment %q leaked in manage_config(get): %s", secret, getOut)
		}
	}
	if out, _ := eng.toolManageConfig(ctx, bgTurn, bgTrig, bgMeta, fmt.Sprintf(`{"action":"upsert_project","name":"billing","machine":"hub","path":%q}`, repoHub)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected non-human upsert_project rejected, got %s", out)
	}

	// 3. Forged trigger with missing Entry MUST be rejected on write.
	forgedTrig := &model.Trigger{ID: "t-forge", Thread: model.MainThread, Source: "chat", Policy: "respond", From: "you", Ref: "e-missing"}
	forgedMeta := eng.resolveTurnConsentMeta(ctx, forgedTrig)
	if out, _ := eng.toolManageConfig(ctx, &apiRouterTurn{thread: model.MainThread, triggerID: forgedTrig.ID}, forgedTrig, forgedMeta, fmt.Sprintf(`{"action":"upsert_project","name":"billing","machine":"hub","path":%q}`, repoHub)); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected missing-entry trigger write rejected, got %s", out)
	}

	// 4. Genuine human main turn: upsert_project -> set_project_path -> same-turn dispatch_work!
	en, trig, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "wecom", From: "alice", Text: "注册 billing 并排查"})
	if err != nil || en == nil || trig == nil {
		t.Fatalf("Say: %v", err)
	}
	hMeta := eng.resolveTurnConsentMeta(ctx, trig)
	hTurn := &apiRouterTurn{thread: model.MainThread, triggerID: trig.ID}
	upOut, stop := eng.toolManageConfig(ctx, hTurn, trig, hMeta, fmt.Sprintf(`{"action":"upsert_project","name":"billing","machine":"hub","path":%q}`, repoHub))
	if stop || !strings.Contains(upOut, `"ok":true`) {
		t.Fatalf("human upsert_project failed: stop=%v out=%s", stop, upOut)
	}
	// Verify api_router_inflight was NOT written by L1 upsert, allowing same-turn dispatch_work.
	if _, hasInflight, _ := st.GetKV(ctx, apiRouterInflightKVKey); hasInflight {
		t.Fatalf("L1 upsert_project should not set api_router_inflight")
	}
	// Merge-only check: upsert_project with paths omitting hub must NOT delete hub path, and empty path is rejected.
	if out, _ := eng.toolManageConfig(ctx, hTurn, trig, hMeta, `{"action":"upsert_project","project":"billing","paths":{"hub":""}}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected empty path in upsert_project rejected, got %s", out)
	}
	if out, _ := eng.toolManageConfig(ctx, hTurn, trig, hMeta, `{"action":"upsert_project","project":"billing","note":"默认跑单测"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected note update ok, got %s", out)
	}
	if got := eng.Settings().Projects[0].Paths["hub"]; got != repoHub {
		t.Fatalf("expected existing hub path preserved, got %q", got)
	}

	// Same turn immediately dispatches Worker using newly registered project.
	dispOut := eng.toolDispatchWork(ctx, hTurn, trig, hMeta, `{"mode":"readonly","project":"billing","goal":"检查代码"}`)
	if !strings.Contains(dispOut, `"ok":true`) {
		t.Fatalf("expected same-turn dispatch_work after upsert_project to succeed, got %s", dispOut)
	}

	// Tool defs count is now 9 in strict alphabetical order.
	defs := apiRouterToolDefs()
	if len(defs) != 9 {
		t.Fatalf("expected 9 native router tools, got %d", len(defs))
	}
	for i := 1; i < len(defs); i++ {
		if defs[i-1].Function.Name >= defs[i].Function.Name {
			t.Fatalf("tools not strictly sorted: %s >= %s", defs[i-1].Function.Name, defs[i].Function.Name)
		}
	}
}

func TestInspectMachinePathRemoteAndOldNodeFallback(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)
	hc := newFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil })
	fl := fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	// Attach modern node returning exists=true for /work/ok and exists=false for /work/missing.
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-new", Name: "MacNew", Online: true, Agents: []string{"claude"}},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body struct {
					Path string `json:"path"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				res := model.PathInspectResult{Exists: body.Path == "/work/ok", IsDir: body.Path == "/work/ok", IsGit: true, ResolvedPath: body.Path}
				b, _ := json.Marshal(res)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
			}),
		},
	})
	// Attach old node returning 404 for /api/path/inspect.
	fl.Attach(&fleet.RemoteMachine{
		Info: model.Machine{ID: "mac-old", Name: "MacOld", Online: true, Agents: []string{"claude"}},
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("404 page not found"))}, nil
			}),
		},
	})
	eng.SetFleet(fl)

	if _, err := eng.UpsertProject(ctx, "test", UpsertProjectRequest{Name: "app", Machine: "MacNew", Path: "/work/missing"}); err == nil {
		t.Fatalf("expected missing remote path rejected")
	}
	resOK, err := eng.UpsertProject(ctx, "test", UpsertProjectRequest{Name: "app", Machine: "MacNew", Path: "/work/ok"})
	if err != nil || len(resOK.Warnings) != 0 {
		t.Fatalf("expected valid remote path ok with no warnings: res=%+v err=%v", resOK, err)
	}
	resOld, err := eng.SetProjectPath(ctx, "test", "app", "MacOld", "~/work/old")
	if err != nil || len(resOld.Warnings) == 0 {
		t.Fatalf("expected old node 404 fallback with warning: res=%+v err=%v", resOld, err)
	}
	resOff, err := eng.SetProjectPath(ctx, "test", "app", "offline-box", "~/work/off")
	if err != nil || len(resOff.Warnings) == 0 {
		t.Fatalf("expected offline node pre-config with warning: res=%+v err=%v", resOff, err)
	}
}

func TestSpawnValidatesExternalCwdAndPreservesWorktreeResume(t *testing.T) {
	ctx := context.Background()
	eng, _, _, dir := newTestAPIEngine(t, nil)
	hc := newFakeHerdrClient(func(method string, _ map[string]any) (any, *herdr.Error) {
		switch method {
		case "workspace.create":
			return map[string]any{"workspace": map[string]any{"workspace_id": "w1"}, "root_pane": map[string]any{"pane_id": "w1:p1"}}, nil
		case "agent.start":
			return map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "session": "s1", "pid": 99}, nil
		default:
			return map[string]any{}, nil
		}
	})
	eng.fleet = fleet.NewFleet(hc, "hub", dir, "test", []string{"claude"})

	for _, bad := range []string{"/etc", "../etc", "~/.ssh/keys"} {
		if _, err := eng.Spawn(ctx, SpawnRequest{Kind: "claude", Cwd: bad, Task: "test"}); err == nil {
			t.Fatalf("expected Spawn to reject dangerous cwd %q", bad)
		}
	}
	wtPath := filepath.Join(dir, "worktrees", "corgi", "claude-1")
	if err := os.MkdirAll(wtPath, 0o700); err != nil {
		t.Fatal(err)
	}
	wk, err := eng.Spawn(ctx, SpawnRequest{Kind: "claude", Cwd: wtPath, Task: "worktree task", Worktree: "off"})
	if err != nil || wk == nil {
		t.Fatalf("expected managed worktree path allowed in Spawn, got err=%v", err)
	}
	_ = time.Now()
}

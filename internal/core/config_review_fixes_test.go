package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestReviewFixesMixedPatchSymlinkCwdOfflineDeviceAndURLRedaction(t *testing.T) {
	ctx := context.Background()
	eng, st, _, dir := newTestAPIEngine(t, nil)

	// 1. Mixed patch: modifying worker_model alongside another kind's kind_defaults must keep worker_model!
	initSt := eng.Settings()
	initSt.WorkerKind = "claude"
	initSt.WorkerModel = "claude-old"
	initSt.KindDefaults = map[string]model.ModelEffortSpec{"claude": {Model: "claude-old"}}
	if err := eng.SaveConfig(ctx, &initSt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.PatchManagedSettings(ctx, "router", map[string]json.RawMessage{
		"worker_model":  json.RawMessage(`"claude-new"`),
		"kind_defaults": json.RawMessage(`{"codex":{"model":"gpt-6.2","effort":"high"}}`),
	}); err != nil {
		t.Fatalf("mixed patch failed: %v", err)
	}
	after := eng.Settings()
	if after.WorkerModel != "claude-new" || after.KindDefaults["claude"].Model != "claude-new" || after.KindDefaults["codex"].Model != "gpt-6.2" {
		t.Fatalf("expected mixed patch preserved both claude-new and codex default, got %+v", after)
	}
	// Conflicting explicit kind_defaults[claude] + worker_model must return an error.
	if _, err := eng.PatchManagedSettings(ctx, "router", map[string]json.RawMessage{
		"worker_model":  json.RawMessage(`"claude-a"`),
		"kind_defaults": json.RawMessage(`{"claude":{"model":"claude-b"}}`),
	}); err == nil {
		t.Fatalf("expected conflicting worker_model + kind_defaults[claude] rejected")
	}

	// 2. Local existing worker_cwd symlink to sensitive directory must be rejected.
	symEtc := filepath.Join(dir, "link-to-etc")
	if err := os.Symlink("/etc", symEtc); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateLocalWorkerCwd(dir, dir, st.DBPath(), symEtc); err == nil {
		t.Fatalf("expected ValidateLocalWorkerCwd to reject symlink to /etc")
	}
	// Even when corgiDir == home, Corgi internal state subdirs (uploads/memory) must be rejected.
	uploadsDir := filepath.Join(dir, "uploads")
	_ = os.MkdirAll(uploadsDir, 0o700)
	if _, err := ValidateLocalWorkerCwd(dir, dir, st.DBPath(), uploadsDir); err == nil {
		t.Fatalf("expected ValidateLocalWorkerCwd to reject corgi uploads subdir even when corgiDir==home")
	}
	// Symlink inside managed worktrees pointing to /etc must NOT be treated as a managed worktree.
	wtSym := filepath.Join(dir, "worktrees", "evil-wt")
	_ = os.MkdirAll(filepath.Dir(wtSym), 0o700)
	if err := os.Symlink("/etc", wtSym); err != nil {
		t.Fatal(err)
	}
	if isManagedWorktreePath(wtSym, dir) {
		t.Fatalf("expected symlink escaping worktrees NOT treated as managed worktree")
	}

	// 3. Offline paired machine display name resolves to stable machine ID.
	if err := st.UpsertDevice(ctx, &model.Device{
		ID: "dev-mac-99", Name: "OfficeMac", Kind: "machine", TokenHash: "h", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mk, tid := eng.resolveMachineKeyForConfig(ctx, "OfficeMac")
	if mk != "dev-mac-99" || tid != "dev-mac-99" {
		t.Fatalf("expected offline paired machine resolved to dev-mac-99, got mk=%q tid=%q", mk, tid)
	}

	// 4. URL validation errors must never echo raw credentials.
	if _, _, err := NormalizeRouterAPIBaseURL(RouterAPIProviderOpenAI, "https://user:s3cr3tToken@bad host:99999"); err == nil || strings.Contains(err.Error(), "s3cr3tToken") {
		t.Fatalf("RouterAPIBaseURL error must reject without leaking credentials, got: %v", err)
	}
	if _, err := NormalizePublicURL("https://admin:topSecret99@bad host"); err == nil || strings.Contains(err.Error(), "topSecret99") {
		t.Fatalf("PublicURL error must reject without leaking credentials, got: %v", err)
	}

	// 5. Corrupt audit ring JSON aborts config commit instead of silently overwriting history.
	if err := st.SetKV(ctx, configAuditRingKVKey, "{not-valid-json"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.PatchManagedSettings(ctx, "router", map[string]json.RawMessage{
		"reply_chars": json.RawMessage(`300`),
	}); err == nil {
		t.Fatalf("expected corrupt audit ring JSON to abort config commit rather than clobbering history")
	}
}

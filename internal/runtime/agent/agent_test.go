package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeAndLaunchArgs(t *testing.T) {
	reg := NewRegistry()
	cases := []struct {
		raw      string
		wantKind string
		model    string
		effort   string
		bypass   bool
		wantFlag string
	}{
		{"claude", "claude", "opus", "high", true, "--dangerously-skip-permissions"},
		{"codex", "codex", "gpt-5-codex", "medium", true, "--dangerously-bypass-approvals-and-sandbox"},
		{"antigravity", "agy", "gemini-4-neon", "xhigh", true, "--dangerously-skip-permissions"},
		{"cbc", "codebuddy", "deepseek-v4-pro", "high", true, "--dangerously-skip-permissions"},
	}
	for _, tc := range cases {
		a, err := reg.Get(tc.raw)
		if err != nil {
			t.Fatalf("Get(%q) failed: %v", tc.raw, err)
		}
		if a.Kind() != tc.wantKind {
			t.Fatalf("got kind %q want %q", a.Kind(), tc.wantKind)
		}
		args := a.LaunchArgs(tc.model, tc.effort, tc.bypass)
		if !slices.Contains(args, tc.wantFlag) {
			t.Fatalf("LaunchArgs(%q) = %v missing %q", tc.raw, args, tc.wantFlag)
		}
	}
}

func TestAgyPreTrustWorkspaceIdempotent(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"theme":"dark"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	agy := &AgyAdapter{}
	if err := agy.PreTrustWorkspace(home, ws); err != nil {
		t.Fatalf("first PreTrustWorkspace: %v", err)
	}
	if err := agy.PreTrustWorkspace(home, ws); err != nil {
		t.Fatalf("second PreTrustWorkspace: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Theme   string   `json:"theme"`
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Theme != "dark" {
		t.Fatalf("lost existing setting: %+v", cfg)
	}
	if !slices.Contains(cfg.Trusted, ws) {
		t.Fatalf("expected %s in %v", ws, cfg.Trusted)
	}
}

func TestParseApprovalAndScreenDetectors(t *testing.T) {
	dialog := `
Allow running this command?
  go test ./internal/orders/...
  › 1. Allow once
    2. Allow for this session
    3. Deny and tell agent what to do
Esc to cancel
`
	if !ScreenAsks(dialog) {
		t.Fatal("expected ScreenAsks=true")
	}
	ap := ParseApproval(dialog)
	if ap.Style != "number" || len(ap.Choices) != 3 {
		t.Fatalf("unexpected approval: %+v", ap)
	}
	if !strings.Contains(ap.Command, "go test") {
		t.Fatalf("expected command preview, got %q", ap.Command)
	}

	if ScreenAsks("Ready for input.\n> ") {
		t.Fatal("normal prompt should not trigger ScreenAsks")
	}
	if !HasBackgroundTask("Done.\n1 background task running") {
		t.Fatal("expected HasBackgroundTask=true")
	}
	if got := ExtractQuotaError("Error: You have hit your usage limit for today", "all tests passed"); got == "" {
		t.Fatal("expected quota error to be detected")
	}
	if got := ExtractQuotaError("Error: usage limit", "Fixed the usage limit retry test"); got != "" {
		t.Fatalf("expected false positive suppressed, got %q", got)
	}
}

func TestTranscriptReaderIncrementalAndImageExtraction(t *testing.T) {
	dir := t.TempDir()
	imgDir := filepath.Join(dir, "images")
	logPath := filepath.Join(dir, "session.jsonl")
	pixel := base64.StdEncoding.EncodeToString([]byte("fakepng"))
	line1 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"看看截图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pixel + `"}}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line1), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := NewTranscriptReader(logPath, &ClaudeAdapter{}, imgDir)
	if !tr.Poll() {
		t.Fatal("expected first Poll=true")
	}
	msgs := tr.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "[image:") {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if tr.Poll() {
		t.Fatal("expected idle Poll=false")
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":"已看完，定位到问题了"}}` + "\n")
	_ = f.Close()

	if !tr.Poll() || len(tr.Messages()) != 2 {
		t.Fatalf("expected second incremental message, got %+v", tr.Messages())
	}
}

func TestCodeBuddyTranscriptAndModelDiscovery(t *testing.T) {
	help := `  --model <model>  Model for the current session. Currently supported: (hy4std, deepseek-v4-pro, glm-5.3, kimi-k3.1-pro)`
	models := parseCodeBuddyHelpModels(help)
	if len(models) != 4 || models[1].ID != "deepseek-v4-pro" {
		t.Fatalf("unexpected parsed codebuddy models: %+v", models)
	}

	cb := &CodeBuddyAdapter{}
	if args := cb.ResumeArgs("01a20b7f"); !slices.Equal(args, []string{"--resume", "01a20b7f"}) {
		t.Fatalf("unexpected ResumeArgs: %v", args)
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "01a20b7f.jsonl")
	sample := strings.Join([]string{
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"查一下慢查询"}]}`,
		`{"type":"function_call","name":"Bash","arguments":"{\"cmd\":\"go test\"}","providerData":{"usage":{"total_tokens":42000}}}`,
		`{"type":"function_call_result","name":"Bash","status":"success","output":"PASS"}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"已定位并修复索引问题"}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := NewTranscriptReader(logPath, cb, filepath.Join(dir, "img"))
	if !tr.Poll() {
		t.Fatal("expected Poll=true")
	}
	msgs := tr.Messages()
	if len(msgs) != 4 || msgs[0].Kind != "user" || msgs[1].Kind != "tool" || msgs[3].Kind != "assistant" {
		t.Fatalf("unexpected codebuddy transcript messages: %+v", msgs)
	}
	usage := cb.ReadContextUsage(logPath, 200000)
	if usage.Used != 42000 || usage.Pct() != 21 {
		t.Fatalf("unexpected codebuddy usage: %+v", usage)
	}
}

func TestCodexModelDiscoveryAndConfigTOML(t *testing.T) {
	tomlCfg := `
# top-level codex config
model = "gpt-6.2-sol" # inline comment
model_reasoning_effort = 'xhigh'

[profiles.fast]
model = "ignored-profile-model"
model_reasoning_effort = "low"
`
	defModel, defEffort := parseCodexConfigTOML(tomlCfg)
	if defModel != "gpt-6.2-sol" || defEffort != "xhigh" {
		t.Fatalf("unexpected toml defaults: model=%q effort=%q", defModel, defEffort)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer c2sR.Close()
	defer s2cR.Close()

	go func() {
		defer s2cW.Close()
		sc := bufio.NewScanner(c2sR)
		for sc.Scan() {
			var req struct {
				ID     *int   `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			switch req.Method {
			case "initialize":
				_, _ = io.WriteString(s2cW, `{"method":"server/banner","params":{}}`+"\n")
				_, _ = io.WriteString(s2cW, `{"id":1,"result":{"userAgent":"corgi/test"}}`+"\n")
			case "model/list":
				_, _ = io.WriteString(s2cW, `{"id":2,"result":{"data":[{"id":"gpt-6.2-sol","model":"gpt-6.2-sol","displayName":"GPT-6.2-Sol","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"xhigh"},{"reasoningEffort":"ultra"}]},{"id":"hidden-internal","model":"hidden-internal","displayName":"Hidden","hidden":true},{"id":"gpt-6-luna","model":"gpt-6-luna","displayName":"GPT-6-Luna","hidden":false,"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"max"}]}]}}`+"\n")
				return
			}
		}
	}()

	items, err := fetchCodexModelsRPC(context.Background(), s2cR, c2sW)
	_ = c2sW.Close()
	if err != nil {
		t.Fatalf("fetchCodexModelsRPC failed: %v", err)
	}
	models, efforts := buildCodexCatalog(items)
	if len(models) != 2 || models[0].ID != "gpt-6.2-sol" || models[0].Label != "GPT-6.2-Sol" {
		t.Fatalf("unexpected codex models: %+v", models)
	}
	if !slices.Equal(models[0].Efforts, []string{"low", "xhigh", "ultra"}) {
		t.Fatalf("unexpected per-model efforts: %v", models[0].Efforts)
	}
	if !slices.Equal(efforts, []string{"low", "xhigh", "ultra", "max"}) {
		t.Fatalf("unexpected union efforts: %v", efforts)
	}
}

func TestParseAgyModelsOutput(t *testing.T) {
	raw := "\r⠋ Fetching available models...\rgemini-4-neon-high\tGemini 4 Neon (High)\n" +
		"gemini-4-neon-xhigh\tGemini 4 Neon (XHigh)\n" +
		"gemini-4-neon-high\tDuplicate\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n"
	got := parseAgyModelsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 agy models, got %d: %+v", len(got), got)
	}
	if got[0].ID != "gemini-4-neon-high" || got[0].Label != "Gemini 4 Neon (High)" {
		t.Fatalf("unexpected first agy model: %+v", got[0])
	}
	if got[2].ID != "claude-opus-5-5-high" || got[2].Label != "Claude Opus 5.5 (High)" {
		t.Fatalf("unexpected third agy model: %+v", got[2])
	}
}

func TestAgyParseTranscriptLines(t *testing.T) {
	a := &AgyAdapter{}
	uMsgs, ok := a.ParseTranscriptLine([]byte(`{"type":"USER_INPUT","content":"<USER_REQUEST>\n修一下登录\n</USER_REQUEST>"}`), "")
	if !ok || len(uMsgs) != 1 || uMsgs[0].Kind != "user" || uMsgs[0].Text != "修一下登录" {
		t.Fatalf("unexpected agy user msg: ok=%v msgs=%+v", ok, uMsgs)
	}
	pMsgs, ok := a.ParseTranscriptLine([]byte(`{"type":"PLANNER_RESPONSE","content":"先看日志","tool_calls":[{"name":"run_command","args":{"CommandLine":"go test ./..."}}]}`), "")
	if !ok || len(pMsgs) != 2 || pMsgs[0].Kind != "assistant" || pMsgs[1].Kind != "tool" || pMsgs[1].Input != "go test ./..." {
		t.Fatalf("unexpected agy planner msgs: ok=%v msgs=%+v", ok, pMsgs)
	}
	gMsgs, ok := a.ParseTranscriptLine([]byte(`{"type":"GENERIC","content":"Created At: 2026-11-25\nCompleted At: 2026-11-25\nCommand exited with code 0.\nPASS"}`), "")
	if !ok || len(gMsgs) != 1 || gMsgs[0].Error || !strings.Contains(gMsgs[0].Output, "PASS") {
		t.Fatalf("unexpected agy generic msg: ok=%v msgs=%+v", ok, gMsgs)
	}
}

func TestCodeBuddyPreTrustWorkspaceIdempotent(t *testing.T) {
	home := t.TempDir()
	ws := filepath.Join(home, "corgi")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	cbDir := filepath.Join(home, ".codebuddy")
	if err := os.MkdirAll(cbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"model":"hy4std","gateway":{"auth":"password"},"trustedDirectories":["/existing"]}` + "\n"
	if err := os.WriteFile(filepath.Join(cbDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	cb := &CodeBuddyAdapter{}
	if err := cb.PreTrustWorkspace(home, ws); err != nil {
		t.Fatalf("first PreTrustWorkspace: %v", err)
	}
	if err := cb.PreTrustWorkspace(home, ws); err != nil {
		t.Fatalf("second PreTrustWorkspace: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(cbDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Model   string            `json:"model"`
		Gateway map[string]string `json:"gateway"`
		Trusted []string          `json:"trustedDirectories"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Model != "hy4std" || parsed.Gateway["auth"] != "password" {
		t.Fatalf("existing settings clobbered: %+v", parsed)
	}
	if !slices.Contains(parsed.Trusted, "/existing") || !slices.Contains(parsed.Trusted, ws) {
		t.Fatalf("unexpected trustedDirectories: %v", parsed.Trusted)
	}
}

func TestAgyContextProtoProgressHashWizardAndPatch(t *testing.T) {
	encodeVarint := func(v uint64) []byte {
		var out []byte
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				b |= 0x80
			}
			out = append(out, b)
			if v == 0 {
				break
			}
		}
		return out
	}
	encodeBytesField := func(fieldNum int, payload []byte) []byte {
		tag := encodeVarint(uint64(fieldNum<<3 | 2))
		ln := encodeVarint(uint64(len(payload)))
		return append(append(tag, ln...), payload...)
	}
	encodeVarintField := func(fieldNum int, v uint64) []byte {
		return append(encodeVarint(uint64(fieldNum<<3)), encodeVarint(v)...)
	}

	m10 := append(encodeVarintField(1, 54000), encodeVarintField(4, 200000)...)
	blob := encodeBytesField(1, encodeBytesField(9, encodeBytesField(10, m10)))
	usage, ok := parseAgyContextProto(blob)
	if !ok || usage.Used != 54000 || usage.Window != 200000 || usage.Pct() != 27 {
		t.Fatalf("unexpected agy proto usage: ok=%v usage=%+v", ok, usage)
	}

	// ProgressHash must ignore ticking spinner seconds and token counters.
	h1 := ProgressHash("● Bash(corgi list)\n✹ Printing… (140s · streaming · ↓ 7.8k tokens · Task running for a while)")
	h2 := ProgressHash("● Bash(corgi list)\n✹ Printing… (195s · streaming · ↓ 9.2k tokens · Task running for a while)")
	h3 := ProgressHash("● Bash(corgi list)\n● Done! Replied to user.")
	if h1 != h2 {
		t.Fatalf("ProgressHash should ignore spinner ticks: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Fatal("ProgressHash should change when real output lands")
	}

	// Wizard horizontal buttons "[Previous]  [Done]" detection and MoveKeys.
	wiz := "Terms of Service & Data Use\nPress Enter to confirm.\n  [Previous]  [Done]"
	if !ScreenAsks(wiz) {
		t.Fatal("expected wizard screen to be recognized by ScreenAsks")
	}
	ap := ParseApproval(wiz)
	if len(ap.Choices) != 2 || ap.Choices[1].Label != "Done" || !ap.Choices[1].Active {
		t.Fatalf("unexpected wizard approval: %+v", ap)
	}
	if keys := MoveKeys(ap, 0); !slices.Equal(keys, []string{"up", "enter"}) {
		t.Fatalf("unexpected MoveKeys to choice 0: %v", keys)
	}

	// MsgSync incremental patching + EarlierMessages.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "big.jsonl")
	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, `{"type":"assistant","message":{"role":"assistant","content":"msg-`+string(rune('A'+(i%26)))+`"}}`)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := NewTranscriptReader(logPath, &ClaudeAdapter{}, "")
	tr.OpenTail()
	var syncer MsgSync
	p1 := syncer.Patch(tr)
	if p1 == nil || len(p1.Msgs) != 60 || p1.Earlier == 0 {
		t.Fatalf("unexpected initial tail patch: %+v", p1)
	}
	if syncer.Patch(tr) != nil {
		t.Fatal("expected nil patch when transcript unchanged")
	}
	older, nextCur := EarlierMessages(logPath, &ClaudeAdapter{}, "", p1.Earlier)
	if len(older) != 20 || nextCur != 0 {
		t.Fatalf("expected 20 older messages and cursor=0, got len=%d cur=%d", len(older), nextCur)
	}
}

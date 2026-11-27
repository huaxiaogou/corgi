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
	// Non-fatal CLI warning banners with tree/indented continuations must be ignored.
	warningBanner := strings.Join([]string{
		"⚠ Warning: model gpt-6-luna hit rate limit fallback",
		"  └ using fallback model gpt-6-mini for this request",
		"  │ try again in 5s if needed",
		"Done.",
	}, "\n")
	if got := ExtractQuotaError(warningBanner, ""); got != "" {
		t.Fatalf("expected non-fatal warning banner ignored, got %q", got)
	}
	// Real fatal error after a warning banner must still be caught.
	withFatal := warningBanner + "\nAPI Error: 529 overloaded_error\n"
	if got := ExtractQuotaError(withFatal, ""); !strings.Contains(got, "529") {
		t.Fatalf("expected fatal error after warning banner caught, got %q", got)
	}
	// Real agy executor error prefixed with `⚠` MUST NOT be swallowed by warning stripping.
	agyFatal := strings.Join([]string{
		"⚠ failed to construct executor: plan model not specified; you must specify a valid model",
		"Error ID: ac007c06-4541-4f66-a808-7f73d8495761-19",
	}, "\n")
	if got := ExtractFatalExecutorError(agyFatal, ""); !strings.Contains(got, "plan model not specified") {
		t.Fatalf("expected agy fatal executor error caught, got %q", got)
	}
	if got := ExtractQuotaError(agyFatal, ""); !strings.Contains(got, "plan model not specified") {
		t.Fatalf("expected ExtractQuotaError to catch agy fatal executor error, got %q", got)
	}
	// Non-fatal warning mentioning model fallback must still be ignored by ExtractFatalExecutorError.
	if got := ExtractFatalExecutorError(warningBanner, ""); got != "" {
		t.Fatalf("expected benign model fallback warning ignored, got %q", got)
	}
}

func TestExtractPostDispatchDelta(t *testing.T) {
	chrome := strings.Join([]string{
		"────────────────────────────────────────",
		">",
		"────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")
	pre := strings.Join([]string{
		"> [corgi] 1 new event",
		"  #t3425 · chat:main · router · from corgi · 18:58",
		"⚠ failed to construct executor: plan model not specified; you must specify a valid model",
		"Error ID: ac007c06-4541-4f66-a808-7f73d8495761-17",
		chrome,
	}, "\n")

	// 1. Unchanged screen with stale scrollback error -> empty delta (zero false positives).
	if got := ExtractPostDispatchDelta(pre, pre, "#t3483", "继续 wi3420 只读排查"); got != "" {
		t.Fatalf("expected empty delta on unchanged screen, got %q", got)
	}

	// 2. New dispatch followed by real immediate agy crash -> isolates only the new error.
	prompt := "[corgi] 1 new event\n\n#t3483 · chat:main · router · from corgi · 19:44\n  继续 wi3420 只读排查，禁止改代码。\n"
	postCrash := strings.Join([]string{
		"⚠ failed to construct executor: plan model not specified; you must specify a valid model",
		"Error ID: ac007c06-4541-4f66-a808-7f73d8495761-17",
		"────────────────────────────────────────",
		"> [corgi] 1 new event",
		"  #t3483 · chat:main · router · from corgi · 19:44",
		"    继续 wi3420 只读排查，禁止改代码。",
		"⚠ failed to construct executor: plan model not specified; you must specify a valid model",
		"Error ID: ac007c06-4541-4f66-a808-7f73d8495761-19",
		chrome,
	}, "\n")
	delta := ExtractPostDispatchDelta(pre, postCrash, "#t3483", prompt)
	if got := ExtractQuotaError(delta, ""); !strings.Contains(got, "plan model not specified") {
		t.Fatalf("expected new post-dispatch crash detected, delta=%q err=%q", delta, got)
	}

	// 3. Prompt itself quoting the error string echoed on screen without CLI crash -> zero false positives.
	debugPrompt := "[corgi] 1 new event\n\n#t3501 · chat:main · router · from corgi · 20:10\n  排查为什么出现 failed to construct executor: plan model not specified 报错。\n"
	postEchoOnly := strings.Join([]string{
		"Done previous task.",
		"> [corgi] 1 new event",
		"  #t3501 · chat:main · router · from corgi · 20:10",
		"    排查为什么出现 failed to construct executor: plan model not specified 报错。",
		chrome,
	}, "\n")
	deltaEcho := ExtractPostDispatchDelta("Done previous task.\n"+chrome, postEchoOnly, "#t3501", debugPrompt)
	if got := ExtractQuotaError(deltaEcho, ""); got != "" {
		t.Fatalf("expected echoed prompt text suppressed, delta=%q err=%q", deltaEcho, got)
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

	// Real gpt-6-luna format: model-usage with top-level camelCase usage + providerData.rawUsage.
	// Trailing turn-metrics in the same turn must NOT overwrite exact explicit usage,
	// whereas a newer turn lacking explicit usage MUST update via turn-metrics.
	lunaPath := filepath.Join(dir, "luna.jsonl")
	lunaSample := strings.Join([]string{
		`{"type":"model-usage","model":"gpt-6-luna","usage":{"inputTokens":156798,"outputTokens":122,"totalTokens":156920},"providerData":{"messageId":"m1","rawUsage":{"prompt_tokens":156798,"completion_tokens":122,"total_tokens":156920}}}`,
		`{"type":"turn-metrics","tokenDelta":90000}`,
	}, "\n") + "\n"
	if err := os.WriteFile(lunaPath, []byte(lunaSample), 0o600); err != nil {
		t.Fatal(err)
	}
	u1 := cb.ReadContextUsage(lunaPath, 0)
	if u1.Used != 156920 || u1.Window != 200000 || u1.Pct() != 78 {
		t.Fatalf("expected exact model-usage 156920/200000 (78%%), got %+v", u1)
	}

	// Append a newer turn that only writes turn-metrics (no explicit model-usage): latest turn must win.
	f, err := os.OpenFile(lunaPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"turn-metrics","tokenDelta":172000}` + "\n")
	_ = f.Close()
	u2 := cb.ReadContextUsage(lunaPath, 0)
	if u2.Used != 172000 || u2.Pct() != 86 {
		t.Fatalf("expected newer turn-metrics 172000 (86%%) to supersede older explicit turn, got %+v", u2)
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
	if !slices.Equal(efforts, []string{"low", "xhigh", "max", "ultra"}) {
		t.Fatalf("unexpected union efforts: %v", efforts)
	}
}

func TestParseAgyModelsOutput(t *testing.T) {
	raw := "\r⠋ Fetching available models...\rgemini-4-neon-high\tGemini 4 Neon (High)\n" +
		"gemini-4-neon-xhigh\tGemini 4 Neon (XHigh)\n" +
		"gemini-4-neon-high\tDuplicate\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n"
	got := parseAgyModelsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 agy models after family grouping, got %d: %+v", len(got), got)
	}
	if got[0].ID != "gemini-4-neon" || got[0].Label != "Gemini 4 Neon" || !slices.Equal(got[0].Efforts, []string{"high", "xhigh"}) {
		t.Fatalf("unexpected first grouped agy model: %+v", got[0])
	}
	if got[1].ID != "claude-opus-5-5-high" || got[1].Label != "Claude Opus 5.5 (High)" || len(got[1].Efforts) != 0 {
		t.Fatalf("unexpected single-tier agy model: %+v", got[1])
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

func TestClaudeParseQueuedCommandTranscriptLine(t *testing.T) {
	c := &ClaudeAdapter{}
	msgs, ok := c.ParseTranscriptLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":"先跑一遍单测","origin":{"kind":"human"}}}`), "")
	if !ok || len(msgs) != 1 || msgs[0].Kind != "user" || msgs[0].Text != "先跑一遍单测" {
		t.Fatalf("expected human string queued_command parsed as user msg, got ok=%v msgs=%+v", ok, msgs)
	}
	msgs, ok = c.ParseTranscriptLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":[{"type":"text","text":"改用方案B"}],"origin":{"kind":"human"}}}`), "")
	if !ok || len(msgs) != 1 || msgs[0].Kind != "user" || msgs[0].Text != "改用方案B" {
		t.Fatalf("expected human block-array queued_command parsed, got ok=%v msgs=%+v", ok, msgs)
	}
	if msgs, ok := c.ParseTranscriptLine([]byte(`{"type":"attachment","attachment":{"type":"queued_command","prompt":"bg done","origin":{"kind":"task-notification"}}}`), ""); ok || len(msgs) != 0 {
		t.Fatalf("expected non-human queued_command ignored, got ok=%v msgs=%+v", ok, msgs)
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

	// Wizard horizontal buttons "[Previous]  [Done]" detection and horizontal MoveKeys.
	wiz := "Terms of Service & Data Use\nPress Enter to confirm.\n  [Previous]  [Done]"
	if !ScreenAsks(wiz) {
		t.Fatal("expected wizard screen to be recognized by ScreenAsks")
	}
	ap := ParseApproval(wiz)
	if len(ap.Choices) != 2 || ap.Choices[1].Label != "Done" || !ap.Choices[1].Active {
		t.Fatalf("unexpected wizard approval: %+v", ap)
	}
	if keys := MoveKeys(ap, 0); !slices.Equal(keys, []string{"left", "enter"}) {
		t.Fatalf("unexpected MoveKeys to choice 0: %v", keys)
	}

	// Full Antigravity ToS wizard with multi-line checkbox + [Previous] [Done] across cursor states.
	tosScreen := "Terms of Service & Data Use\nAI coding agents are known to have certain security risks.\n-------------------------------------------------\n  > [x] Yes, I agree to help improve Antigravity CLI by allowing\n      Google to collect and use my Interactions data,\n      settings.\n      Links:\n      - Terms: https://antigravity.google/terms\n    [Previous]      [Done]\n  ↑/↓ Navigate · enter Toggle\n  auto · Gemini 4 Neon · xhigh"
	tosAp := ParseApproval(tosScreen)
	if len(tosAp.Choices) != 3 || tosAp.Choices[0].Label != "Done" {
		t.Fatalf("expected Done first + checkbox + Previous, got %+v", tosAp.Choices)
	}
	if !slices.Equal(MoveKeys(tosAp, 0), []string{"down", "right", "enter"}) {
		t.Fatalf("expected Done from checkbox to send [down right enter], got %v", MoveKeys(tosAp, 0))
	}
	if !slices.Equal(MoveKeys(tosAp, 1), []string{"enter"}) {
		t.Fatalf("expected checkbox toggle to send [enter], got %v", MoveKeys(tosAp, 1))
	}
	if !slices.Equal(MoveKeys(tosAp, 2), []string{"down", "enter"}) {
		t.Fatalf("expected Previous from checkbox to send [down enter], got %v", MoveKeys(tosAp, 2))
	}

	// State when cursor already moved down to Previous (`>  Previous       [Done]`).
	tosPrevFocused := strings.Replace(strings.Replace(tosScreen, "  > [x]", "    [x]", 1), "    [Previous]      [Done]", "  >  Previous       [Done]", 1)
	tosAp2 := ParseApproval(tosPrevFocused)
	if len(tosAp2.Choices) != 3 || !slices.Equal(MoveKeys(tosAp2, 0), []string{"right", "enter"}) {
		t.Fatalf("expected Done from Previous to send [right enter], got %+v", tosAp2.Choices)
	}

	// Split-column theme picker (`Choose your color scheme:` with right-side diff preview box).
	themeScreen := "Choose your color scheme:\n  > terminal | |\n    light | Here's the change: |\n    solarized dark | 5 - func main() { |\n                         | 6   fmt.Println() |"
	themeAp := ParseApproval(themeScreen)
	if len(themeAp.Choices) != 3 || themeAp.Choices[0].Label != "terminal" || themeAp.Choices[1].Label != "light" || themeAp.Choices[2].Label != "solarized dark" {
		t.Fatalf("expected split-column preview stripped cleanly, got %+v", themeAp.Choices)
	}

	// Regression: Router scrollback containing a numbered Markdown summary above an idle prompt
	// MUST NOT trigger ScreenAsks (previously caused agent_blocked delivery deadlocks).
	routerIdleWithMarkdown := `
  1. **单点对话，多代理分治**：人主要只和常驻的大脑对话。
  2. **全托管闭环与最低打扰**：常规故障尽量自愈。
  3. **轻量、自托管**：Go 单二进制内嵌前端资源。
● 已处理 t89：当前确实没有进行中的任务。
✔ Worked for 3m 49s
──────────────────────────────────────────────────────
> Verify /usr/local/bin/corgi reply e92 within 120 characters    ↵ send
──────────────────────────────────────────────────────
hy4std low · ← for agents    ⏵⏵ bypass permissions on (⇧tab to cycle)
`
	if ScreenAsks(routerIdleWithMarkdown) {
		t.Fatal("idle router prompt with historical markdown 1/2/3 list must not trigger ScreenAsks")
	}

	// Regression: Router quoting a worker's trust dialog in scrollback above an idle prompt.
	quotedTrustAboveIdle := `
  worker w1 is BLOCKED waiting for approval. Screen:
    Do you trust the contents of this project?
    > Yes, I trust this folder
      No, exit
● 已告知用户需要确认目录信任。
──────────────────────────────────────────────────────
>
──────────────────────────────────────────────────────
? for shortcuts                                         auto · Claude Opus 5.5 · medium
`
	if ScreenAsks(quotedTrustAboveIdle) {
		t.Fatal("quoted worker dialog above idle prompt must not trigger ScreenAsks")
	}

	// Plain numbered markdown output with no interactive dialog hints.
	plainList := "Summary:\n1. Build passed\n2. Tests passed\n3. Deployed"
	if ScreenAsks(plainList) {
		t.Fatal("plain numbered list without dialog hints must not trigger ScreenAsks")
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

func TestAgyFamilyGroupingAndModelEffortMapping(t *testing.T) {
	rawAgy := strings.Join([]string{
		"gemini-4-neon-high\tGemini 4 Neon (High)",
		"gemini-4-neon-xhigh\tGemini 4 Neon (XHigh)",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)",
		"gemini-4-neon-low\tGemini 4 Neon (Low)",
		"gemini-4-carbon-medium\tGemini 4 Carbon (Medium)",
		"gemini-4-carbon-high\tGemini 4 Carbon (High)",
		"gemini-4-carbon-low\tGemini 4 Carbon (Low)",
		"gemini-pro-orchestrator-neon\tOrchestrator (Neon)",
		"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)",
	}, "\n")
	models := parseAgyModelsOutput(rawAgy)
	if len(models) != 4 {
		t.Fatalf("expected 4 grouped models, got %d: %+v", len(models), models)
	}
	// 1. Gemini 4 Neon grouped with sorted [low, medium, high, xhigh]
	if models[0].ID != "gemini-4-neon" || models[0].Label != "Gemini 4 Neon" ||
		!slices.Equal(models[0].Efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("unexpected neon group: %+v", models[0])
	}
	// 2. Gemini 4 Carbon grouped with [low, medium, high] (no xhigh/max)
	if models[1].ID != "gemini-4-carbon" || !slices.Equal(models[1].Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("unexpected carbon group: %+v", models[1])
	}
	// 3. Orchestrator has explicit empty [] efforts (serialized as "efforts":[] in JSON)
	if models[2].ID != "gemini-pro-orchestrator-neon" || len(models[2].Efforts) != 0 || models[2].Efforts == nil {
		t.Fatalf("expected explicit empty efforts on orchestrator: %+v", models[2])
	}
	jb, err := json.Marshal(models[2])
	if err != nil || !strings.Contains(string(jb), `"efforts":[]`) {
		t.Fatalf("expected JSON to preserve empty efforts array, got %s (err=%v)", string(jb), err)
	}
	// 4. Single-tier model keeps exact preset ID and empty efforts
	if models[3].ID != "gpt-oss-120b-medium" || len(models[3].Efforts) != 0 {
		t.Fatalf("unexpected single-tier model: %+v", models[3])
	}

	// Agy LaunchArgs stitches base + effort without redundant --effort, and avoids conflicts on pre-suffixed IDs.
	agy := &AgyAdapter{}
	if got := agy.LaunchArgs("gemini-4-neon", "xhigh", false); !slices.Equal(got, []string{"--model", "gemini-4-neon-xhigh"}) {
		t.Fatalf("unexpected stitched LaunchArgs: %v", got)
	}
	if got := agy.LaunchArgs("gemini-4-neon-xhigh", "low", false); !slices.Equal(got, []string{"--model", "gemini-4-neon-xhigh"}) {
		t.Fatalf("expected pre-suffixed model ID to ignore conflicting effort, got %v", got)
	}
	if got := agy.LaunchArgs("", "low", false); !slices.Equal(got, []string{"--effort", "low"}) {
		t.Fatalf("expected default-model effort flag, got %v", got)
	}

	// Claude gateway capabilities + effortLevel discovery.
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeCfg := `{"model":"gw-fast","effortLevel":"low","env":{"CLAUDE_CODE_MODEL_CAPABILITIES":"gw-pro=effort,xhigh_effort;gw-fast="}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(claudeCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	clCat, err := (&ClaudeAdapter{}).DiscoverModels(context.Background(), home)
	if err != nil || clCat.Default != "gw-fast" || clCat.DefaultEffort != "low" {
		t.Fatalf("unexpected claude catalog defaults: %+v (err=%v)", clCat, err)
	}
	var gwPro, gwFastFound bool
	for _, m := range clCat.Models {
		if m.ID == "gw-pro" && slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh"}) {
			gwPro = true
		}
		if m.ID == "gw-fast" && m.Efforts != nil && len(m.Efforts) == 0 {
			gwFastFound = true
		}
	}
	if !gwPro || !gwFastFound {
		t.Fatalf("expected gateway capability mapping in claude models: %+v", clCat.Models)
	}

	// CodeBuddy per-model effort mapping (echo -> [], gpt-6.1-sol -> minimal..xhigh).
	if eff := codeBuddyEffortsForModel("echo"); len(eff) != 0 {
		t.Fatalf("expected echo to have 0 efforts, got %v", eff)
	}
	if eff := codeBuddyEffortsForModel("gpt-6.1-sol"); !slices.Contains(eff, "xhigh") || slices.Contains(eff, "max") {
		t.Fatalf("unexpected gpt-6.1-sol efforts: %v", eff)
	}
}

func TestAllAdaptersReadContextUsageAndProgressHash(t *testing.T) {
	dir := t.TempDir()

	// 1. CodeBuddy: turn-metrics tokenDelta divided by distinct providerData.messageId steps.
	cbFile := filepath.Join(dir, "cb.jsonl")
	cbContent := strings.Join([]string{
		`{"type":"message","role":"assistant","providerData":{"messageId":"m1","requestModelId":"hy4std"}}`,
		`{"type":"function_call","providerData":{"messageId":"m1","requestModelId":"hy4std"}}`,
		`{"type":"message","role":"assistant","providerData":{"messageId":"m2","requestModelId":"hy4std"}}`,
		`{"type":"message","role":"assistant","providerData":{"messageId":"m3","requestModelId":"hy4std"}}`,
		`{"type":"message","role":"assistant","providerData":{"messageId":"m4","requestModelId":"hy4std"}}`,
		`{"type":"turn-metrics","durationMs":93690,"tokenDelta":600000}`,
	}, "\n") + "\n"
	if err := os.WriteFile(cbFile, []byte(cbContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cbU := (&CodeBuddyAdapter{}).ReadContextUsage(cbFile, 200000)
	if cbU.Used != 150000 || cbU.Window != 200000 || cbU.Pct() != 75 {
		t.Fatalf("unexpected CodeBuddy usage: %+v (pct=%d)", cbU, cbU.Pct())
	}

	// 2. Agy: transcript.jsonl PLANNER_RESPONSE fallback when conversation SQLite DB is absent.
	agyFile := filepath.Join(dir, "transcript.jsonl")
	agyContent := `{"type":"PLANNER_RESPONSE","model":"gemini-4-neon-xhigh","input_tokens":3000,"cache_read_tokens":496000,"output_tokens":1000}` + "\n"
	if err := os.WriteFile(agyFile, []byte(agyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	agyU := (&AgyAdapter{}).ReadContextUsage(agyFile, 200000)
	if agyU.Used != 500000 || agyU.Window != 1000000 || agyU.Pct() != 50 {
		t.Fatalf("unexpected Agy fallback usage: %+v (pct=%d)", agyU, agyU.Pct())
	}

	// 3. Claude: 1M-window model (claude-opus-5-5) should not show >100% against a 200k default.
	clFile := filepath.Join(dir, "claude.jsonl")
	clContent := `{"type":"assistant","isSidechain":false,"message":{"model":"claude-opus-5-5","usage":{"input_tokens":200,"cache_read_input_tokens":298800,"cache_creation_input_tokens":500,"output_tokens":500}}}` + "\n"
	if err := os.WriteFile(clFile, []byte(clContent), 0o600); err != nil {
		t.Fatal(err)
	}
	clU := (&ClaudeAdapter{}).ReadContextUsage(clFile, 200000)
	if clU.Used != 300000 || clU.Window != 1000000 || clU.Pct() != 30 {
		t.Fatalf("unexpected Claude 1M usage: %+v (pct=%d)", clU, clU.Pct())
	}

	// 4. Codex: fallback to input_tokens + output_tokens when total_tokens is absent.
	cxFile := filepath.Join(dir, "codex.jsonl")
	cxContent := `{"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":400000,"last_token_usage":{"input_tokens":239000,"output_tokens":1000}}}}` + "\n"
	if err := os.WriteFile(cxFile, []byte(cxContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cxU := (&CodexAdapter{}).ReadContextUsage(cxFile, 200000)
	if cxU.Used != 240000 || cxU.Window != 400000 || cxU.Pct() != 60 {
		t.Fatalf("unexpected Codex usage: %+v (pct=%d)", cxU, cxU.Pct())
	}

	// 5. ProgressHash strips animated Braille / asterisk spinners and status lines.
	h1 := ProgressHash("● Bash(corgi list)\n⠋ Thinking… (12s)\n> ")
	h2 := ProgressHash("● Bash(corgi list)\n⠸ Pondering… (142s)\n> ")
	if h1 != h2 {
		t.Fatalf("ProgressHash should ignore animated spinner frames and verbs: %q != %q", h1, h2)
	}
}

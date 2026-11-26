package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

var codebuddyModelsRe = regexp.MustCompile(`Currently supported:\s*\(([^)]+)\)`)

// CodeBuddyAdapter adapts Tencent CodeBuddy Code CLI (codebuddy / cbc) to Corgi's runtime.
type CodeBuddyAdapter struct{}

func (c *CodeBuddyAdapter) Kind() string        { return "codebuddy" }
func (c *CodeBuddyAdapter) DisplayName() string { return "CodeBuddy" }

func (c *CodeBuddyAdapter) LaunchArgs(modelName, effort string, bypassPerm bool) []string {
	var args []string
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if bypassPerm {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func (c *CodeBuddyAdapter) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--resume", sessionID}
}

// PreTrustWorkspace writes cwd (and its resolved symlink target) into CodeBuddy's
// ~/.codebuddy/settings.json trustedDirectories while preserving all other fields.
func (c *CodeBuddyAdapter) PreTrustWorkspace(home, cwd string) error {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return fmt.Errorf("workspace path must be absolute: %q", cwd)
	}
	want := []string{filepath.Clean(cwd)}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != want[0] {
		want = append(want, resolved)
	}

	settingsFile := filepath.Join(codebuddyHome(home), "settings.json")
	cfg := map[string]json.RawMessage{}
	if b, err := os.ReadFile(settingsFile); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", settingsFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", settingsFile, err)
	}

	var trusted []string
	if raw, ok := cfg["trustedDirectories"]; ok {
		if err := json.Unmarshal(raw, &trusted); err != nil {
			return fmt.Errorf("parse trustedDirectories in %s: %w", settingsFile, err)
		}
	}
	added := false
	for _, p := range want {
		if !slices.Contains(trusted, p) {
			trusted = append(trusted, p)
			added = true
		}
	}
	if !added {
		return nil
	}
	encoded, err := json.Marshal(trusted)
	if err != nil {
		return err
	}
	cfg["trustedDirectories"] = encoded
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsFile), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(settingsFile), err)
	}
	tmp := settingsFile + ".corgi-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, settingsFile)
}

func (c *CodeBuddyAdapter) DiscoverModels(ctx context.Context, home string) (*model.ModelCatalog, error) {
	cat := &model.ModelCatalog{
		Kind:    "codebuddy",
		Efforts: []string{"low", "medium", "high"},
		Models:  []model.ModelOpt{},
	}
	hasEffortFlag := true
	if bin := herdr.FindBin("codebuddy", home); bin != "" {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, "--help")
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
		if out, err := cmd.Output(); err == nil {
			txt := string(out)
			hasEffortFlag = strings.Contains(txt, "--effort")
			cat.Models = parseCodeBuddyHelpModels(txt)
		}
	}
	if len(cat.Models) == 0 {
		for _, id := range []string{"deepseek-v4-pro", "glm-5.3", "kimi-k3.1-pro", "hy4std"} {
			cat.Models = append(cat.Models, model.ModelOpt{
				ID:      id,
				Label:   id,
				Efforts: codeBuddyEffortsForModel(id),
			})
		}
	}
	if !hasEffortFlag {
		cat.Efforts = []string{}
		for i := range cat.Models {
			cat.Models[i].Efforts = []string{}
		}
	}
	if b, err := os.ReadFile(filepath.Join(codebuddyHome(home), "settings.json")); err == nil {
		var cfg struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.Model != "" {
			cat.Default = cfg.Model
			found := false
			for _, m := range cat.Models {
				if m.ID == cfg.Model {
					found = true
					break
				}
			}
			if !found && ModelNameRe.MatchString(cfg.Model) {
				effs := []string{}
				if hasEffortFlag {
					effs = codeBuddyEffortsForModel(cfg.Model)
				}
				cat.Models = append([]model.ModelOpt{{ID: cfg.Model, Label: cfg.Model, Note: "当前默认", Efforts: effs}}, cat.Models...)
			}
			if hasEffortFlag {
				cat.Efforts = codeBuddyEffortsForModel(cfg.Model)
			}
		}
	}
	return cat, nil
}

func codeBuddyEffortsForModel(id string) []string {
	low := strings.ToLower(strings.TrimSpace(id))
	switch {
	case low == "echo":
		return []string{}
	case strings.HasPrefix(low, "gpt-"):
		return []string{"minimal", "low", "medium", "high", "xhigh"}
	case strings.HasPrefix(low, "claude-opus"):
		return []string{"low", "medium", "high", "max"}
	case strings.HasPrefix(low, "gemini-4"):
		return []string{"low", "medium", "high", "xhigh"}
	default:
		return []string{"low", "medium", "high"}
	}
}

func parseCodeBuddyHelpModels(helpText string) []model.ModelOpt {
	m := codebuddyModelsRe.FindStringSubmatch(helpText)
	if len(m) < 2 {
		return nil
	}
	var out []model.ModelOpt
	for _, raw := range strings.Split(m[1], ",") {
		id := strings.TrimSpace(raw)
		if id != "" && ModelNameRe.MatchString(id) {
			out = append(out, model.ModelOpt{
				ID:      id,
				Label:   id,
				Efforts: codeBuddyEffortsForModel(id),
			})
		}
	}
	return out
}

func (c *CodeBuddyAdapter) LocateTranscript(home, cwd, sessionID string, since time.Time) string {
	projDir := filepath.Join(codebuddyHome(home), "projects")
	if sessionID != "" {
		if matches, _ := filepath.Glob(filepath.Join(projDir, "*", sessionID+".jsonl")); len(matches) > 0 {
			return matches[0]
		}
		direct := filepath.Join(projDir, sessionID+".jsonl")
		if fi, err := os.Stat(direct); err == nil && !fi.IsDir() {
			return direct
		}
		return ""
	}
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if since.IsZero() {
		since = time.Now().Add(-6 * time.Hour)
	}
	files, _ := filepath.Glob(filepath.Join(projDir, "*", "*.jsonl"))
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() == 0 || fi.ModTime().Before(since.Add(-10*time.Second)) {
			continue
		}
		if codebuddyFileMatchesCwd(f, cwd) {
			cands = append(cands, cand{path: f, mod: fi.ModTime()})
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
}

func codebuddyFileMatchesCwd(path, wantCwd string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16<<10), 256<<10)
	for i := 0; i < 3 && sc.Scan(); i++ {
		var head struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &head) == nil && head.Cwd != "" {
			return filepath.Clean(head.Cwd) == wantCwd
		}
	}
	return false
}

func (c *CodeBuddyAdapter) ParseTranscriptLine(line []byte, imgDir string) ([]*model.Message, bool) {
	var rec struct {
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		RawContent json.RawMessage `json:"rawContent"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Output     json.RawMessage `json:"output"`
		Status     string          `json:"status"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil, false
	}
	switch rec.Type {
	case "message":
		if rec.Role != "user" && rec.Role != "assistant" {
			return nil, false
		}
		txt := extractCodeBuddyContent(rec.Content, imgDir)
		if txt == "" {
			return nil, false
		}
		return []*model.Message{{Kind: rec.Role, Text: txt}}, true
	case "reasoning":
		txt := extractCodeBuddyReasoning(rec.Content)
		if txt == "" {
			txt = extractCodeBuddyReasoning(rec.RawContent)
		}
		if txt == "" {
			return nil, false
		}
		return []*model.Message{{Kind: "thinking", Text: txt}}, true
	case "function_call":
		if rec.Name == "" {
			return nil, false
		}
		return []*model.Message{{
			Kind:  "tool",
			Name:  rec.Name,
			Input: clipText(decodeRawString(rec.Arguments), 300),
		}}, true
	case "function_call_result":
		out := strings.TrimSpace(decodeRawString(rec.Output))
		if out == "" {
			return nil, false
		}
		name := rec.Name
		if name == "" {
			name = "result"
		}
		return []*model.Message{{
			Kind:   "tool",
			Name:   name,
			Output: clipText(out, 800),
			Error:  strings.EqualFold(rec.Status, "error") || strings.EqualFold(rec.Status, "failed"),
		}}, true
	}
	return nil, false
}

func extractCodeBuddyContent(raw json.RawMessage, imgDir string) string {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var blocks []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "output_text", "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "input_image", "image":
			if b.Data != "" {
				parts = append(parts, ExtractImage(b.MediaType, b.Data, imgDir))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractCodeBuddyReasoning(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		t := strings.TrimSpace(b.Text)
		if t == "" {
			t = strings.TrimSpace(b.Thinking)
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func decodeRawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func (c *CodeBuddyAdapter) ReadContextUsage(transcriptPath string, defaultWindow int) model.ContextUsage {
	if defaultWindow <= 0 {
		defaultWindow = 200000
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return model.ContextUsage{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return model.ContextUsage{}
	}
	const tail = 1 << 20
	if fi.Size() > tail {
		_, _ = f.Seek(fi.Size()-tail, io.SeekStart)
	}
	var (
		latest       model.ContextUsage
		turnExplicit model.ContextUsage
		lastModel    string
		stepIDs      = map[string]struct{}{}
	)
	type usageFields struct {
		TotalTokens      int `json:"totalTokens"`
		InputTokens      int `json:"inputTokens"`
		OutputTokens     int `json:"outputTokens"`
		Total            int `json:"total_tokens"`
		Input            int `json:"input_tokens"`
		Output           int `json:"output_tokens"`
		Prompt           int `json:"prompt_tokens"`
		Completion       int `json:"completion_tokens"`
	}
	extractUsed := func(u *usageFields) int {
		if u == nil {
			return 0
		}
		if u.TotalTokens > 0 {
			return u.TotalTokens
		}
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			return u.InputTokens + u.OutputTokens
		}
		if u.Total > 0 {
			return u.Total
		}
		if u.Input > 0 || u.Output > 0 {
			return u.Input + u.Output
		}
		if u.Prompt > 0 || u.Completion > 0 {
			return u.Prompt + u.Completion
		}
		return 0
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"turn-metrics"`)) &&
			!bytes.Contains(line, []byte(`"model-usage"`)) &&
			!bytes.Contains(line, []byte(`"providerData"`)) &&
			!bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var e struct {
			Type         string       `json:"type"`
			Model        string       `json:"model"`
			TokenDelta   int          `json:"tokenDelta"`
			Usage        *usageFields `json:"usage"`
			ProviderData *struct {
				MessageID      string       `json:"messageId"`
				Model          string       `json:"model"`
				RequestModelID string       `json:"requestModelId"`
				Usage          *usageFields `json:"usage"`
				RawUsage       *usageFields `json:"rawUsage"`
			} `json:"providerData"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if m := strings.TrimSpace(e.Model); m != "" {
			lastModel = m
		}
		explicitUsed := 0
		if e.ProviderData != nil {
			if m := strings.TrimSpace(e.ProviderData.RequestModelID); m != "" {
				lastModel = m
			} else if m := strings.TrimSpace(e.ProviderData.Model); m != "" {
				lastModel = m
			}
			if e.ProviderData.MessageID != "" {
				stepIDs[e.ProviderData.MessageID] = struct{}{}
			}
			if u := extractUsed(e.ProviderData.RawUsage); u > 0 {
				explicitUsed = u
			} else if u := extractUsed(e.ProviderData.Usage); u > 0 {
				explicitUsed = u
			}
		}
		if u := extractUsed(e.Usage); u > 0 {
			explicitUsed = u
		}
		if explicitUsed > 0 {
			win := codeBuddyWindowForModel(lastModel, defaultWindow, explicitUsed)
			turnExplicit = model.ContextUsage{Used: explicitUsed, Window: win}
			latest = turnExplicit
		}
		if e.Type == "turn-metrics" {
			if turnExplicit.Used > 0 {
				latest = turnExplicit
			} else if e.TokenDelta > 0 {
				steps := len(stepIDs)
				if steps <= 0 {
					steps = 1
				}
				estUsed := e.TokenDelta / steps
				if estUsed > 0 {
					win := codeBuddyWindowForModel(lastModel, defaultWindow, 0)
					latest = model.ContextUsage{Used: estUsed, Window: win}
				}
			}
			turnExplicit = model.ContextUsage{}
			clear(stepIDs)
		}
	}
	return latest
}

func codeBuddyWindowForModel(modelName string, fallback, used int) int {
	low := strings.ToLower(strings.TrimSpace(modelName))
	switch {
	case strings.Contains(low, "[1m]"),
		strings.Contains(low, "gemini-4"),
		strings.Contains(low, "opus-5"):
		return 1000000
	}
	if fallback <= 0 {
		fallback = 200000
	}
	if used > fallback*2 {
		return 1000000
	}
	return fallback
}

func (c *CodeBuddyAdapter) LookupTitle(_, _, terminalTitle string) string {
	return cleanTerminalTitle(terminalTitle)
}

func codebuddyHome(home string) string {
	if d := os.Getenv("CODEBUDDY_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".codebuddy")
}

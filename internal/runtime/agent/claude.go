package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/kiperjing/corgi/internal/model"
)

// ClaudeAdapter adapts Claude Code CLI to Corgi's runtime.
type ClaudeAdapter struct{}

func (c *ClaudeAdapter) Kind() string        { return "claude" }
func (c *ClaudeAdapter) DisplayName() string { return "Claude Code" }

func (c *ClaudeAdapter) LaunchArgs(modelName, effort string, bypassPerm bool) []string {
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

func (c *ClaudeAdapter) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--resume", sessionID}
}

func (c *ClaudeAdapter) PreTrustWorkspace(_, _ string) error { return nil }

func (c *ClaudeAdapter) DiscoverModels(_ context.Context, home string) (*model.ModelCatalog, error) {
	var cfg struct {
		Model  string            `json:"model"`
		Effort string            `json:"effortLevel"`
		Env    map[string]string `json:"env"`
	}
	cfgPath := filepath.Join(claudeConfigDir(home), "settings.json")
	if b, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	envVal := func(k string) string {
		if cfg.Env != nil && cfg.Env[k] != "" {
			return cfg.Env[k]
		}
		return os.Getenv(k)
	}

	// Parse gateway-declared model capabilities if configured:
	// e.g. "modelA=effort,xhigh_effort,max_effort;modelB=effort"
	gwMap := map[string][]string{}
	var gwModels []model.ModelOpt
	for _, part := range strings.Split(envVal("CLAUDE_CODE_MODEL_CAPABILITIES"), ";") {
		id, caps, _ := strings.Cut(strings.TrimSpace(part), "=")
		id = strings.TrimSpace(id)
		if id == "" || !ModelNameRe.MatchString(id) {
			continue
		}
		effs := []string{}
		for _, capName := range strings.Split(caps, ",") {
			switch strings.TrimSpace(capName) {
			case "effort":
				effs = append(effs, "low", "medium", "high")
			case "xhigh_effort":
				effs = append(effs, "xhigh")
			case "max_effort":
				effs = append(effs, "max")
			}
		}
		effs = sortEffortsCanonical(effs)
		gwMap[id] = effs
		gwModels = append(gwModels, model.ModelOpt{ID: id, Label: id, Efforts: effs})
	}
	sort.SliceStable(gwModels, func(i, j int) bool { return gwModels[i].ID < gwModels[j].ID })

	aliasEfforts := func(target string, fallback []string) []string {
		if target != "" {
			if effs, ok := gwMap[target]; ok {
				return effs
			}
		}
		return fallback
	}

	cat := &model.ModelCatalog{
		Kind:          "claude",
		Default:       cfg.Model,
		DefaultEffort: cfg.Effort,
		Efforts:       []string{"low", "medium", "high", "max"},
		Models:        []model.ModelOpt{},
	}
	seen := map[string]bool{}
	add := func(o model.ModelOpt) {
		if o.ID != "" && !seen[o.ID] && ModelNameRe.MatchString(o.ID) {
			seen[o.ID] = true
			cat.Models = append(cat.Models, o)
		}
	}

	sonnetTarget := envVal("ANTHROPIC_DEFAULT_SONNET_MODEL")
	opusTarget := envVal("ANTHROPIC_DEFAULT_OPUS_MODEL")
	haikuTarget := envVal("ANTHROPIC_DEFAULT_HAIKU_MODEL")
	add(model.ModelOpt{
		ID:      "sonnet",
		Label:   "Claude Sonnet",
		Note:    orNote(sonnetTarget, "均衡推荐"),
		Efforts: aliasEfforts(sonnetTarget, []string{"low", "medium", "high"}),
	})
	add(model.ModelOpt{
		ID:      "opus",
		Label:   "Claude Opus",
		Note:    orNote(opusTarget, "复杂推理"),
		Efforts: aliasEfforts(opusTarget, []string{"low", "medium", "high", "max"}),
	})
	add(model.ModelOpt{
		ID:      "haiku",
		Label:   "Claude Haiku",
		Note:    orNote(haikuTarget, "轻量快速"),
		Efforts: aliasEfforts(haikuTarget, []string{"low", "medium", "high"}),
	})
	for _, gm := range gwModels {
		add(gm)
	}
	if cfg.Model != "" {
		add(model.ModelOpt{ID: cfg.Model, Label: cfg.Model, Note: "当前默认"})
	}
	if len(gwModels) > 0 {
		union := []string{}
		for _, m := range cat.Models {
			for _, ef := range m.Efforts {
				if !slices.Contains(union, ef) {
					union = append(union, ef)
				}
			}
		}
		if len(union) > 0 {
			cat.Efforts = sortEffortsCanonical(union)
		}
	}
	return cat, nil
}

func orNote(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return strings.TrimSpace(primary)
	}
	return fallback
}

func (c *ClaudeAdapter) LocateTranscript(home, _, sessionID string, _ time.Time) string {
	if sessionID == "" {
		return ""
	}
	pattern := filepath.Join(claudeConfigDir(home), "projects", "*", sessionID+".jsonl")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func (c *ClaudeAdapter) ParseTranscriptLine(line []byte, imgDir string) ([]*model.Message, bool) {
	var rec struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment *struct {
			Type   string          `json:"type"`
			Prompt json.RawMessage `json:"prompt"`
			Origin *struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"origin"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.IsSidechain {
		return nil, false
	}
	// Mid-turn user messages sent while Claude Code is working are recorded as
	// attachment/queued_command once Claude picks them up, rather than type=="user".
	if rec.Type == "attachment" && rec.Attachment != nil && rec.Attachment.Type == "queued_command" {
		originKind := ""
		if rec.Attachment.Origin != nil {
			originKind = strings.TrimSpace(rec.Attachment.Origin.Kind)
		}
		if originKind != "" && originKind != "human" {
			return nil, false
		}
		if txt := extractClaudeQueuedPrompt(rec.Attachment.Prompt, imgDir); txt != "" {
			return []*model.Message{{Kind: "user", Text: txt}}, true
		}
		return nil, false
	}
	if rec.Type != "user" && rec.Type != "assistant" {
		return nil, false
	}
	// Plain string content
	var plain string
	if json.Unmarshal(rec.Message.Content, &plain) == nil {
		plain = strings.TrimSpace(plain)
		if plain == "" {
			return nil, false
		}
		return []*model.Message{{Kind: rec.Type, Text: plain}}, true
	}

	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"is_error"`
		Source  struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return nil, false
	}
	var out []*model.Message
	var textParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
			}
		case "image":
			if b.Source.Type == "base64" {
				textParts = append(textParts, ExtractImage(b.Source.MediaType, b.Source.Data, imgDir))
			}
		case "tool_use":
			if b.Name == "AskUserQuestion" {
				if m := parseClaudeAsk(b.Input); m != nil {
					out = append(out, m)
					continue
				}
			}
			out = append(out, &model.Message{
				Kind:  "tool",
				Name:  b.Name,
				Input: clipText(string(b.Input), 300),
			})
		case "tool_result":
			var txt string
			if json.Unmarshal(b.Content, &txt) != nil {
				txt = string(b.Content)
			}
			if txt = strings.TrimSpace(txt); txt != "" {
				out = append(out, &model.Message{
					Kind:   "tool",
					Name:   "result",
					Output: clipText(txt, 800),
					Error:  b.IsError,
				})
			}
		}
	}
	if len(textParts) > 0 {
		out = append([]*model.Message{{Kind: rec.Type, Text: strings.Join(textParts, "\n")}}, out...)
	}
	return out, len(out) > 0
}

func extractClaudeQueuedPrompt(raw json.RawMessage, imgDir string) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "image":
			if b.Source.Type == "base64" {
				if p := ExtractImage(b.Source.MediaType, b.Source.Data, imgDir); p != "" {
					parts = append(parts, p)
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func parseClaudeAsk(raw json.RawMessage) *model.Message {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Questions) == 0 {
		return nil
	}
	m := &model.Message{Kind: "ask", Name: "AskUserQuestion"}
	var qs []string
	for _, q := range in.Questions {
		qs = append(qs, q.Question)
	}
	m.Text = strings.Join(qs, "\n")
	if len(in.Questions) == 1 {
		for _, o := range in.Questions[0].Options {
			m.Options = append(m.Options, o.Label)
		}
	}
	return m
}

func (c *ClaudeAdapter) ReadContextUsage(transcriptPath string, claudeWindow int) model.ContextUsage {
	if claudeWindow <= 0 {
		claudeWindow = 200000
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
	var last model.ContextUsage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var e struct {
			IsSidechain bool `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
				Usage *struct {
					In     int `json:"input_tokens"`
					Read   int `json:"cache_read_input_tokens"`
					Create int `json:"cache_creation_input_tokens"`
					Out    int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) == nil && !e.IsSidechain && e.Message.Usage != nil {
			u := e.Message.Usage
			total := u.In + u.Read + u.Create + u.Out
			if total > 0 {
				win := claudeWindowForModel(e.Message.Model, claudeWindow, total)
				last = model.ContextUsage{Used: total, Window: win}
			}
		}
	}
	return last
}

func claudeWindowForModel(modelName string, fallback, used int) int {
	low := strings.ToLower(strings.TrimSpace(modelName))
	if strings.Contains(low, "[1m]") ||
		strings.Contains(low, "opus-5") ||
		strings.Contains(low, "opus-4-6") ||
		strings.Contains(low, "sonnet-4-6") {
		return 1000000
	}
	if fallback <= 0 {
		fallback = 200000
	}
	if used > fallback {
		return 1000000
	}
	return fallback
}

func (c *ClaudeAdapter) LookupTitle(_, _, terminalTitle string) string {
	return cleanTerminalTitle(terminalTitle)
}

func claudeConfigDir(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".claude")
}

var genericTitles = map[string]bool{
	"": true, "claude": true, "claude code": true,
	"codex": true, "agy": true, "antigravity": true,
	"codebuddy": true, "codebuddy code": true, "cbc": true,
	"bash": true, "zsh": true, "fish": true, "sh": true,
}

func cleanTerminalTitle(t string) string {
	t = strings.TrimLeftFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	t = strings.TrimSpace(t)
	if genericTitles[strings.ToLower(t)] {
		return ""
	}
	return clipText(t, 60)
}

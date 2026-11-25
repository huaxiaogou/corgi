package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// CodexAdapter adapts OpenAI Codex CLI to Corgi's runtime.
type CodexAdapter struct{}

func (c *CodexAdapter) Kind() string        { return "codex" }
func (c *CodexAdapter) DisplayName() string { return "Codex" }

func (c *CodexAdapter) LaunchArgs(modelName, effort string, bypassPerm bool) []string {
	var args []string
	if modelName != "" {
		args = append(args, "-m", modelName)
	}
	if effort != "" {
		b, _ := json.Marshal(effort)
		args = append(args, "-c", "model_reasoning_effort="+string(b))
	}
	if bypassPerm {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	return args
}

func (c *CodexAdapter) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"resume", sessionID}
}

func (c *CodexAdapter) PreTrustWorkspace(_, _ string) error { return nil }

func (c *CodexAdapter) DiscoverModels(ctx context.Context, home string) (*model.ModelCatalog, error) {
	cat := &model.ModelCatalog{
		Kind:    "codex",
		Efforts: []string{},
		Models:  []model.ModelOpt{},
	}

	if b, err := os.ReadFile(filepath.Join(codexHome(home), "config.toml")); err == nil {
		cat.Default, cat.DefaultEffort = parseCodexConfigTOML(string(b))
	}

	if bin := herdr.FindBin("codex", home); bin != "" {
		if items, err := queryCodexAppServer(ctx, bin); err == nil {
			cat.Models, cat.Efforts = buildCodexCatalog(items)
		}
	}

	if cat.Default != "" && ModelNameRe.MatchString(cat.Default) {
		found := false
		for _, m := range cat.Models {
			if m.ID == cat.Default {
				found = true
				break
			}
		}
		if !found {
			cat.Models = append([]model.ModelOpt{{ID: cat.Default, Label: cat.Default, Note: "当前默认"}}, cat.Models...)
		}
	}
	if len(cat.Efforts) == 0 {
		cat.Efforts = append([]string(nil), AllEfforts...)
	}
	if cat.DefaultEffort != "" && ModelNameRe.MatchString(cat.DefaultEffort) && !slices.Contains(cat.Efforts, cat.DefaultEffort) {
		cat.Efforts = append(cat.Efforts, cat.DefaultEffort)
	}
	return cat, nil
}

type codexModelItem struct {
	ID          string `json:"id"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	Hidden      bool   `json:"hidden"`
	Efforts     []struct {
		Effort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

func queryCodexAppServer(ctx context.Context, bin string) ([]codexModelItem, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, "app-server")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	return fetchCodexModelsRPC(cctx, stdout, stdin)
}

func fetchCodexModelsRPC(ctx context.Context, r io.Reader, w io.Writer) ([]codexModelItem, error) {
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	}
	if err := send(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{"name": "corgi", "version": "1"},
		},
	}); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	waitReply := func(wantID int) (json.RawMessage, error) {
		for sc.Scan() {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var msg struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil || msg.Method != "" || *msg.ID != wantID {
				continue
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("codex rpc error: %s", msg.Error.Message)
			}
			return msg.Result, nil
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	if _, err := waitReply(1); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"id": 2, "method": "model/list", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	raw, err := waitReply(2)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []codexModelItem `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

func buildCodexCatalog(items []codexModelItem) ([]model.ModelOpt, []string) {
	models := make([]model.ModelOpt, 0, len(items))
	seen := map[string]bool{}
	var allEfforts []string
	for _, it := range items {
		if it.Hidden {
			continue
		}
		id := strings.TrimSpace(it.Model)
		if id == "" {
			id = strings.TrimSpace(it.ID)
		}
		if id == "" || seen[id] || !ModelNameRe.MatchString(id) {
			continue
		}
		seen[id] = true
		label := strings.TrimSpace(it.DisplayName)
		if label == "" {
			label = id
		}
		// Initialize to empty non-nil slice so models with 0 supported efforts serialize as []
		// rather than null, letting the UI hide the effort selector cleanly.
		efforts := []string{}
		for _, e := range it.Efforts {
			ef := strings.TrimSpace(e.Effort)
			if ef == "" || !ModelNameRe.MatchString(ef) {
				continue
			}
			if !slices.Contains(efforts, ef) {
				efforts = append(efforts, ef)
			}
			if !slices.Contains(allEfforts, ef) {
				allEfforts = append(allEfforts, ef)
			}
		}
		models = append(models, model.ModelOpt{
			ID:      id,
			Label:   label,
			Efforts: sortEffortsCanonical(efforts),
		})
	}
	return models, sortEffortsCanonical(allEfforts)
}

// parseCodexConfigTOML extracts top-level model and model_reasoning_effort from ~/.codex/config.toml.
func parseCodexConfigTOML(raw string) (defaultModel, defaultEffort string) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = parseTOMLStringValue(v)
		switch k {
		case "model":
			defaultModel = v
		case "model_reasoning_effort":
			defaultEffort = v
		}
	}
	return defaultModel, defaultEffort
}

func parseTOMLStringValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		q := v[0]
		if end := strings.IndexByte(v[1:], q); end >= 0 {
			return v[1 : 1+end]
		}
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func (c *CodexAdapter) LocateTranscript(home, _, sessionID string, _ time.Time) string {
	if sessionID == "" {
		return ""
	}
	root := filepath.Join(codexHome(home), "sessions")
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".jsonl") && strings.Contains(d.Name(), sessionID) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func (c *CodexAdapter) ParseTranscriptLine(line []byte, _ string) ([]*model.Message, bool) {
	var rec struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil, false
	}
	switch rec.Type {
	case "response_item":
		var p struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Name    string `json:"name"`
			Args    string `json:"arguments"`
			Output  string `json:"output"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(rec.Payload, &p) != nil {
			return nil, false
		}
		switch p.Type {
		case "message":
			if p.Role != "user" && p.Role != "assistant" {
				return nil, false
			}
			var parts []string
			for _, item := range p.Content {
				if t := strings.TrimSpace(item.Text); t != "" {
					parts = append(parts, t)
				}
			}
			if len(parts) == 0 {
				return nil, false
			}
			return []*model.Message{{Kind: p.Role, Text: strings.Join(parts, "\n")}}, true
		case "function_call":
			return []*model.Message{{Kind: "tool", Name: p.Name, Input: clipText(p.Args, 300)}}, true
		case "function_call_output":
			return []*model.Message{{Kind: "tool", Name: "result", Output: clipText(p.Output, 800)}}, true
		}
	case "event_msg":
		var p struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(rec.Payload, &p) == nil && p.Type == "user_message" && strings.TrimSpace(p.Message) != "" {
			return []*model.Message{{Kind: "user", Text: strings.TrimSpace(p.Message)}}, true
		}
	}
	return nil, false
}

func (c *CodexAdapter) ReadContextUsage(transcriptPath string, _ int) model.ContextUsage {
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
		if !bytes.Contains(line, []byte(`"token_count"`)) {
			continue
		}
		var e struct {
			Payload struct {
				Info *struct {
					ModelWindow int `json:"model_context_window"`
					LastUsage   *struct {
						Total  int `json:"total_tokens"`
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) == nil && e.Payload.Info != nil && e.Payload.Info.LastUsage != nil {
			used := e.Payload.Info.LastUsage.Total
			if used <= 0 {
				used = e.Payload.Info.LastUsage.Input + e.Payload.Info.LastUsage.Output
			}
			if used > 0 && e.Payload.Info.ModelWindow > 0 {
				last = model.ContextUsage{
					Used:   used,
					Window: e.Payload.Info.ModelWindow,
				}
			}
		}
	}
	return last
}

func (c *CodexAdapter) LookupTitle(home, sessionID, terminalTitle string) string {
	if sessionID != "" {
		idxPath := filepath.Join(codexHome(home), "session_index.jsonl")
		if f, err := os.Open(idxPath); err == nil {
			defer f.Close()
			var title string
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				var row struct {
					ID   string `json:"id"`
					Name string `json:"thread_name"`
				}
				if json.Unmarshal(sc.Bytes(), &row) == nil && row.ID == sessionID && row.Name != "" {
					title = row.Name
				}
			}
			if title != "" {
				return clipText(title, 60)
			}
		}
	}
	return cleanTerminalTitle(terminalTitle)
}

func codexHome(home string) string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".codex")
}

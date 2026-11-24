package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
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

func (c *CodexAdapter) DiscoverModels(_ context.Context, _ string) (*model.ModelCatalog, error) {
	return &model.ModelCatalog{
		Kind:    "codex",
		Efforts: AllEfforts,
		Models: []model.ModelOpt{
			{ID: "gpt-5.3-codex", Label: "GPT-5.3 Codex", Note: "主力编码"},
			{ID: "gpt-5-codex", Label: "GPT-5 Codex", Note: "通用稳定"},
			{ID: "o4-mini", Label: "o4-mini", Note: "轻量快速"},
		},
	}, nil
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
						Total int `json:"total_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) == nil && e.Payload.Info != nil && e.Payload.Info.LastUsage != nil {
			if e.Payload.Info.LastUsage.Total > 0 && e.Payload.Info.ModelWindow > 0 {
				last = model.ContextUsage{
					Used:   e.Payload.Info.LastUsage.Total,
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

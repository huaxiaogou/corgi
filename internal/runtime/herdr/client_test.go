package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

func TestStartAgentAndPromptShellFallback(t *testing.T) {
	var (
		mu        sync.Mutex
		sentTexts []string
		sentKeys  []string
		paneAlive = true
	)

	client := NewClient(func(ctx context.Context) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			rd := bufio.NewReader(c2)
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			resp := map[string]any{"id": req.ID}
			switch req.Method {
			case "agent.start":
				resp["error"] = map[string]any{"code": "unsupported_kind", "message": "unknown agent kind codebuddy"}
			case "agent.prompt":
				if _, ok := req.Params["text"].(string); !ok {
					resp["error"] = map[string]any{"code": "invalid_request", "message": "missing field `text`"}
					break
				}
				resp["error"] = map[string]any{"code": "not_found", "message": "agent not found"}
			case "pane.send_text":
				mu.Lock()
				sentTexts = append(sentTexts, req.Params["text"].(string))
				mu.Unlock()
				resp["result"] = map[string]any{}
			case "pane.send_keys":
				mu.Lock()
				for _, k := range req.Params["keys"].([]any) {
					sentKeys = append(sentKeys, k.(string))
				}
				mu.Unlock()
				resp["result"] = map[string]any{}
			case "agent.list":
				resp["result"] = map[string]any{"agents": []any{}}
			case "pane.list":
				mu.Lock()
				alive := paneAlive
				mu.Unlock()
				if alive {
					resp["result"] = map[string]any{"panes": []any{
						map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "cwd": "/repo"},
					}}
				} else {
					resp["result"] = map[string]any{"panes": []any{}}
				}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})

	ctx := context.Background()
	if err := client.StartAgent(ctx, "w1:p1", "codebuddy", "cb-1", []string{"--model", "glm-5.3", "--dangerously-skip-permissions"}); err != nil {
		t.Fatalf("StartAgent fallback failed: %v", err)
	}

	ags, err := client.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(ags) != 1 || ags[0].Agent == nil || *ags[0].Agent != "codebuddy" || ags[0].WorkspaceID != "w1" || ags[0].Cwd == nil || *ags[0].Cwd != "/repo" {
		t.Fatalf("unexpected fallback agents: %+v", ags)
	}

	if err := client.Prompt(ctx, "cb-1", "run tests"); err != nil {
		t.Fatalf("Prompt fallback failed: %v", err)
	}

	mu.Lock()
	if len(sentTexts) != 2 || sentTexts[0] != "codebuddy --model glm-5.3 --dangerously-skip-permissions" || sentTexts[1] != "run tests" {
		t.Fatalf("unexpected sentTexts: %v", sentTexts)
	}
	if len(sentKeys) != 2 || sentKeys[0] != "enter" || sentKeys[1] != "enter" {
		t.Fatalf("unexpected sentKeys: %v", sentKeys)
	}
	paneAlive = false
	mu.Unlock()

	ags, err = client.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents after pane close: %v", err)
	}
	if len(ags) != 0 {
		t.Fatalf("expected stale fallback agent pruned, got %+v", ags)
	}
}

func TestCustomAgentPersistenceStatusAndDialogBlock(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "custom_agents.json")
	var (
		mu         sync.Mutex
		screenText = "✹ Printing… (14s · streaming · esc to interrupt)"
		rev        = uint64(1)
	)
	dialer := func(ctx context.Context) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			rd := bufio.NewReader(c2)
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			resp := map[string]any{"id": req.ID}
			switch req.Method {
			case "agent.start":
				resp["error"] = map[string]any{"code": "unsupported_kind", "message": "unknown agent kind codebuddy"}
			case "pane.send_text", "pane.send_keys":
				resp["result"] = map[string]any{}
			case "agent.list":
				resp["result"] = map[string]any{"agents": []any{}}
			case "pane.list":
				mu.Lock()
				curRev := rev
				mu.Unlock()
				resp["result"] = map[string]any{"panes": []any{
					map[string]any{"pane_id": "w2:p1", "workspace_id": "w2", "cwd": "/repo", "revision": curRev},
				}}
			case "pane.read":
				mu.Lock()
				txt := screenText
				mu.Unlock()
				resp["result"] = map[string]any{"read": map[string]any{"text": txt}}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	}

	ctx := context.Background()
	c1 := NewClient(dialer)
	c1.SetCustomStore(storePath)
	if err := c1.StartAgent(ctx, "w2:p1", "codebuddy", "cb-persist", nil); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// Simulate node restart with a brand new Client loading custom_agents.json.
	c2 := NewClient(dialer)
	c2.SetCustomStore(storePath)
	ags, err := c2.Agents(ctx)
	if err != nil || len(ags) != 1 || ags[0].AgentStatus != "working" {
		t.Fatalf("expected persisted codebuddy with working status, got err=%v ags=%+v", err, ags)
	}

	// Switch screen to a folder-trust dialog: status becomes blocked and Prompt refuses to paste.
	mu.Lock()
	screenText = "Do you trust the files in this folder?\n  › 1. Yes, I trust this folder\n    2. No, exit"
	rev = 2
	mu.Unlock()

	ags, err = c2.Agents(ctx)
	if err != nil || len(ags) != 1 || ags[0].AgentStatus != "blocked" {
		t.Fatalf("expected blocked status on trust dialog, got err=%v ags=%+v", err, ags)
	}
	pErr := c2.Prompt(ctx, "cb-persist", "do work")
	var he *Error
	if !errors.As(pErr, &he) || he.Code != "agent_blocked" {
		t.Fatalf("expected Prompt to return agent_blocked on dialog, got %v", pErr)
	}
}

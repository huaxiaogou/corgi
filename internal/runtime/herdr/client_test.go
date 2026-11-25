package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func u32p(v uint32) *uint32 { return &v }
func strp(s string) *string { return &s }

func newRPCTestClient(fn func(method string, params map[string]any) (any, *Error)) *Client {
	c := NewClient(func(ctx context.Context) (net.Conn, error) {
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
			res, herr := fn(req.Method, req.Params)
			resp := map[string]any{"id": req.ID}
			if herr != nil {
				resp["error"] = herr
			} else if res != nil {
				resp["result"] = res
			} else {
				resp["result"] = map[string]any{}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})
	c.startRetryInterval = 10 * time.Millisecond
	c.startRetryTimeout = 80 * time.Millisecond
	return c
}

func TestClassifyShellProcessInfo(t *testing.T) {
	cases := []struct {
		name string
		info *paneProcessInfo
		want shellPaneState
	}{
		{
			name: "nil info",
			info: nil,
			want: shellPaneUnknown,
		},
		{
			name: "idle zsh prompt",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(100),
				ForegroundProcesses:      []paneProcess{{PID: 100, Name: "zsh", Argv0: strp("-zsh")}},
			},
			want: shellPaneReady,
		},
		{
			name: "idle login fish by path",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(100),
				ForegroundProcesses:      []paneProcess{{PID: 100, Name: "/opt/homebrew/bin/fish"}},
			},
			want: shellPaneReady,
		},
		{
			name: "idle windows pwsh",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(100),
				ForegroundProcesses:      []paneProcess{{PID: 100, Name: "pwsh.exe"}},
			},
			want: shellPaneReady,
		},
		{
			name: "rc child in same pgid",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(100),
				ForegroundProcesses: []paneProcess{
					{PID: 101, Name: "bash"},
					{PID: 100, Name: "zsh"},
				},
			},
			want: shellPaneBusy,
		},
		{
			name: "rc child in own pgid",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(200),
				ForegroundProcesses:      []paneProcess{{PID: 200, Name: "bash"}},
			},
			want: shellPaneBusy,
		},
		{
			name: "transitional missing fg pgid",
			info: &paneProcessInfo{
				PaneID:   "w1:p1",
				ShellPID: u32p(100),
			},
			want: shellPaneBusy,
		},
		{
			name: "no shell pid",
			info: &paneProcessInfo{
				PaneID: "w1:p1",
			},
			want: shellPaneUnavailable,
		},
		{
			name: "non-shell root process (exec vim)",
			info: &paneProcessInfo{
				PaneID:                   "w1:p1",
				ShellPID:                 u32p(100),
				ForegroundProcessGroupID: u32p(100),
				ForegroundProcesses:      []paneProcess{{PID: 100, Name: "vim"}},
			},
			want: shellPaneUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyShellProcessInfo(tc.info); got != tc.want {
				t.Fatalf("classifyShellProcessInfo() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartAgentRetriesTransientShellInitBusy(t *testing.T) {
	var attempts atomic.Int32
	client := newRPCTestClient(func(method string, params map[string]any) (any, *Error) {
		switch method {
		case "agent.start":
			n := attempts.Add(1)
			if n <= 2 {
				return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w9:p1 is not an available shell"}
			}
			return map[string]any{"type": "agent_started"}, nil
		case "agent.list":
			return map[string]any{"agents": []any{}}, nil
		case "pane.get":
			return map[string]any{"pane": map[string]any{"pane_id": "w9:p1", "agent": nil}}, nil
		case "pane.process_info":
			return map[string]any{"process_info": map[string]any{
				"pane_id":                     "w9:p1",
				"shell_pid":                   100,
				"foreground_process_group_id": 100,
				"foreground_processes": []any{
					map[string]any{"pid": 101, "name": "bash"},
					map[string]any{"pid": 100, "name": "zsh"},
				},
			}}, nil
		}
		return map[string]any{}, nil
	})

	if err := client.StartAgent(context.Background(), "w9:p1", "claude", "w-1", nil); err != nil {
		t.Fatalf("expected StartAgent to recover from transient shell init busyness, got %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 agent.start attempts, got %d", got)
	}
}

func TestStartAgentRejectsOccupiedOrNonShellPanes(t *testing.T) {
	cases := []struct {
		name         string
		wantMaxCalls int32
		handler      func(method string) (any, *Error)
	}{
		{
			name:         "occupied by live native agent fails fast",
			wantMaxCalls: 1,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{
						map[string]any{"pane_id": "w1:p1", "name": "existing", "agent": "claude"},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:         "occupied by launch_pending agent fails fast",
			wantMaxCalls: 1,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{
						map[string]any{"pane_id": "w1:p1", "name": "booting", "agent": nil, "launch_pending": true},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:         "pane with restore_error fails fast",
			wantMaxCalls: 1,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get":
					return map[string]any{"pane": map[string]any{"pane_id": "w1:p1", "restore_error": "spawn failed"}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:         "pane with no shell pid fails fast",
			wantMaxCalls: 1,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get":
					return map[string]any{"pane": map[string]any{"pane_id": "w1:p1"}}, nil
				case "pane.process_info":
					return map[string]any{"process_info": map[string]any{"pane_id": "w1:p1", "shell_pid": nil}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:         "pane with non-shell root process fails fast",
			wantMaxCalls: 1,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get":
					return map[string]any{"pane": map[string]any{"pane_id": "w1:p1"}}, nil
				case "pane.process_info":
					return map[string]any{"process_info": map[string]any{
						"pane_id":                     "w1:p1",
						"shell_pid":                   100,
						"foreground_process_group_id": 100,
						"foreground_processes":        []any{map[string]any{"pid": 100, "name": "vim"}},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:         "persistently busy foreground command times out with agent_pane_busy",
			wantMaxCalls: 20,
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get":
					return map[string]any{"pane": map[string]any{"pane_id": "w1:p1"}}, nil
				case "pane.process_info":
					return map[string]any{"process_info": map[string]any{
						"pane_id":                     "w1:p1",
						"shell_pid":                   100,
						"foreground_process_group_id": 200,
						"foreground_processes":        []any{map[string]any{"pid": 200, "name": "sleep"}},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var startCalls atomic.Int32
			client := newRPCTestClient(func(method string, params map[string]any) (any, *Error) {
				if method == "agent.start" {
					startCalls.Add(1)
				}
				return tc.handler(method)
			})
			err := client.StartAgent(context.Background(), "w1:p1", "claude", "w-1", nil)
			var he *Error
			if !errors.As(err, &he) || he.Code != "agent_pane_busy" {
				t.Fatalf("expected agent_pane_busy error, got %v", err)
			}
			if got := startCalls.Load(); got > tc.wantMaxCalls {
				t.Fatalf("expected at most %d agent.start calls, got %d", tc.wantMaxCalls, got)
			}
		})
	}
}

func TestStartAgentCustomFallbackValidatesShellAndBlocksUnsafePanes(t *testing.T) {
	t.Run("waits for shell init before sending custom command", func(t *testing.T) {
		var (
			procCalls atomic.Int32
			sentTexts atomic.Int32
		)
		client := newRPCTestClient(func(method string, params map[string]any) (any, *Error) {
			switch method {
			case "agent.start":
				return nil, &Error{Code: "unsupported_agent_kind", Message: "unsupported interactive agent kind codebuddy"}
			case "agent.list":
				return map[string]any{"agents": []any{}}, nil
			case "pane.get":
				return map[string]any{"pane": map[string]any{"pane_id": "w4:p1", "agent": nil}}, nil
			case "pane.process_info":
				if procCalls.Add(1) <= 2 {
					return map[string]any{"process_info": map[string]any{
						"pane_id":                     "w4:p1",
						"shell_pid":                   100,
						"foreground_process_group_id": 200,
						"foreground_processes":        []any{map[string]any{"pid": 200, "name": "bash"}},
					}}, nil
				}
				return map[string]any{"process_info": map[string]any{
					"pane_id":                     "w4:p1",
					"shell_pid":                   100,
					"foreground_process_group_id": 100,
					"foreground_processes":        []any{map[string]any{"pid": 100, "name": "zsh"}},
				}}, nil
			case "pane.send_text":
				sentTexts.Add(1)
				return map[string]any{}, nil
			case "pane.send_keys", "pane.read":
				return map[string]any{"read": map[string]any{"text": ""}}, nil
			}
			return map[string]any{}, nil
		})

		if err := client.StartAgent(context.Background(), "w4:p1", "codebuddy", "cb-wait", nil); err != nil {
			t.Fatalf("StartAgent custom fallback failed: %v", err)
		}
		if procCalls.Load() != 3 || sentTexts.Load() != 1 {
			t.Fatalf("expected 3 process_info polls and 1 send_text, got proc=%d send=%d", procCalls.Load(), sentTexts.Load())
		}

		// Launching again on the already-tracked custom pane must fail immediately without send_text.
		err := client.StartAgent(context.Background(), "w4:p1", "codebuddy", "cb-dup", nil)
		var he *Error
		if !errors.As(err, &he) || he.Code != "agent_pane_busy" {
			t.Fatalf("expected agent_pane_busy on already-tracked custom pane, got %v", err)
		}
		if sentTexts.Load() != 1 {
			t.Fatalf("expected no send_text on occupied custom pane, got %d", sentTexts.Load())
		}
	})

	unsafeCases := []struct {
		name     string
		wantCode string
		handler  func(method string) (any, *Error)
	}{
		{
			name:     "busy foreground process never receives send_text",
			wantCode: "agent_pane_busy",
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "unsupported_agent_kind", Message: "unsupported interactive agent kind codebuddy"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get":
					return map[string]any{"pane": map[string]any{"pane_id": "w5:p1"}}, nil
				case "pane.process_info":
					return map[string]any{"process_info": map[string]any{
						"pane_id":                     "w5:p1",
						"shell_pid":                   100,
						"foreground_process_group_id": 250,
						"foreground_processes":        []any{map[string]any{"pid": 250, "name": "vim"}},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:     "pane hosting launch_pending agent never receives send_text",
			wantCode: "agent_pane_busy",
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "unsupported_agent_kind", Message: "unsupported interactive agent kind codebuddy"}
				case "agent.list":
					return map[string]any{"agents": []any{
						map[string]any{"pane_id": "w5:p1", "name": "other", "launch_pending": true},
					}}, nil
				}
				return map[string]any{}, nil
			},
		},
		{
			name:     "nonexistent pane returns not_found and never sends text",
			wantCode: "pane_not_found",
			handler: func(method string) (any, *Error) {
				switch method {
				case "agent.start":
					return nil, &Error{Code: "unsupported_agent_kind", Message: "unsupported interactive agent kind codebuddy"}
				case "agent.list":
					return map[string]any{"agents": []any{}}, nil
				case "pane.get", "pane.process_info":
					return nil, &Error{Code: "pane_not_found", Message: "pane w5:p1 not found"}
				}
				return map[string]any{}, nil
			},
		},
	}

	for _, tc := range unsafeCases {
		t.Run(tc.name, func(t *testing.T) {
			var sent atomic.Int32
			client := newRPCTestClient(func(method string, params map[string]any) (any, *Error) {
				if method == "pane.send_text" || method == "pane.send_keys" {
					sent.Add(1)
				}
				return tc.handler(method)
			})
			err := client.StartAgent(context.Background(), "w5:p1", "codebuddy", "cb-unsafe", nil)
			var he *Error
			if !errors.As(err, &he) || he.Code != tc.wantCode {
				t.Fatalf("expected error code %q, got %v", tc.wantCode, err)
			}
			if sent.Load() != 0 {
				t.Fatalf("expected zero send_text/send_keys on unsafe pane, got %d", sent.Load())
			}
		})
	}
}

func TestCreateWorkspaceRejectsEmptyIDs(t *testing.T) {
	client := newRPCTestClient(func(method string, params map[string]any) (any, *Error) {
		return map[string]any{"workspace": map[string]any{}, "root_pane": map[string]any{}}, nil
	})
	if _, _, err := client.CreateWorkspace(context.Background(), "/tmp", "bad"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected error on empty workspace/pane id, got %v", err)
	}
}

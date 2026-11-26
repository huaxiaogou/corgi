package agent

import (
	"testing"

	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestNeedsBypassHeal(t *testing.T) {
	u32 := func(v uint32) *uint32 { return &v }
	sp := func(s string) *string { return &s }

	cases := []struct {
		name     string
		kind     string
		info     *herdr.PaneProcessInfo
		wantNeed bool
		wantPID  uint32
	}{
		{
			name:     "nil process info is fail-safe false",
			kind:     "agy",
			info:     nil,
			wantNeed: false,
		},
		{
			name: "herdr-restored bare agy needs bypass heal ignoring mcp child",
			kind: "agy",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w7:p1",
				ShellPID:                 u32(64071),
				ForegroundProcessGroupID: u32(67818),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:   68461,
						Name:  "mcp-server-darw",
						Argv0: sp("mcp-server-darwin-arm64"),
						Argv:  []string{"/Applications/Pen.app/.../mcp-server-darwin-arm64", "--agent", "antigravity"},
					},
					{
						PID:     67818,
						Name:    "agy",
						Argv0:   sp("agy"),
						Argv:    []string{"agy", "--conversation", "032e20e3-5dfe-4496-ba33-b1519947f4a0"},
						Cmdline: sp("agy --conversation 032e20e3-5dfe-4496-ba33-b1519947f4a0"),
					},
				},
			},
			wantNeed: true,
			wantPID:  67818,
		},
		{
			name: "agy with bypass flag does not need heal",
			kind: "agy",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "wK:p1",
				ForegroundProcessGroupID: u32(72397),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:  72397,
						Name: "agy",
						Argv: []string{"agy", "--model", "gemini-4-neon-xhigh", "--dangerously-skip-permissions"},
					},
				},
			},
			wantNeed: false,
			wantPID:  72397,
		},
		{
			name: "agy with explicit --mode plan is respected",
			kind: "agy",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w9:p1",
				ForegroundProcessGroupID: u32(1234),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:  1234,
						Name: "agy",
						Argv: []string{"agy", "--conversation", "abc", "--mode", "plan"},
					},
				},
			},
			wantNeed: false,
			wantPID:  1234,
		},
		{
			name: "herdr-restored bare claude via node wrapper needs bypass heal",
			kind: "claude",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w2:p1",
				ForegroundProcessGroupID: u32(555),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:  555,
						Name: "node",
						Argv: []string{"node", "/usr/local/bin/claude", "--resume", "sess-1"},
					},
				},
			},
			wantNeed: true,
			wantPID:  555,
		},
		{
			name: "claude with bypassPermissions mode does not need heal",
			kind: "claude",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w2:p1",
				ForegroundProcessGroupID: u32(556),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:  556,
						Name: "claude",
						Argv: []string{"claude", "--resume", "sess-1", "--permission-mode", "bypassPermissions"},
					},
				},
			},
			wantNeed: false,
			wantPID:  556,
		},
		{
			name: "herdr-restored bare codex needs bypass heal",
			kind: "codex",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w3:p1",
				ForegroundProcessGroupID: u32(777),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:     777,
						Name:    "codex",
						Cmdline: sp("codex resume 0199-aaaa"),
					},
				},
			},
			wantNeed: true,
			wantPID:  777,
		},
		{
			name: "codex with yolo flag does not need heal",
			kind: "codex",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w3:p1",
				ForegroundProcessGroupID: u32(778),
				ForegroundProcesses: []herdr.PaneProcess{
					{
						PID:  778,
						Name: "codex",
						Argv: []string{"codex", "resume", "0199-aaaa", "--dangerously-bypass-approvals-and-sandbox"},
					},
				},
			},
			wantNeed: false,
			wantPID:  778,
		},
		{
			name: "missing argv and cmdline fails safe to false",
			kind: "agy",
			info: &herdr.PaneProcessInfo{
				PaneID:                   "w7:p1",
				ForegroundProcessGroupID: u32(999),
				ForegroundProcesses: []herdr.PaneProcess{
					{PID: 999, Name: "agy"},
				},
			},
			wantNeed: false,
			wantPID:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNeed, gotPID := NeedsBypassHeal(tc.kind, tc.info)
			if gotNeed != tc.wantNeed || gotPID != tc.wantPID {
				t.Fatalf("NeedsBypassHeal(%q) = (%v, %d), want (%v, %d)", tc.kind, gotNeed, gotPID, tc.wantNeed, tc.wantPID)
			}
		})
	}
}

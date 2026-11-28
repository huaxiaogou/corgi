package agent

import (
	"slices"
	"testing"

	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

func TestAdapterReadonlyLaunchArgsAndEnforcement(t *testing.T) {
	reg := NewRegistry()

	cases := []struct {
		name        string
		kind        string
		opts        LaunchOptions
		wantTier    ReadonlyEnforcement
		wantLabel   string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "codex readonly with bypassPerm uses read-only sandbox and never approval",
			kind:        "codex",
			opts:        LaunchOptions{Model: "gpt-5.4", Effort: "high", BypassPerm: true, Readonly: true},
			wantTier:    ReadonlyEnforcementHardSandbox,
			wantLabel:   "硬沙箱",
			wantContain: []string{"-m", "gpt-5.4", "--sandbox", "read-only", "--ask-for-approval", "never"},
			wantAbsent:  []string{"--dangerously-bypass-approvals-and-sandbox"},
		},
		{
			name:        "codex readonly with askPermissions omits never approval",
			kind:        "codex",
			opts:        LaunchOptions{Model: "gpt-5.4", BypassPerm: false, Readonly: true},
			wantTier:    ReadonlyEnforcementHardSandbox,
			wantLabel:   "硬沙箱",
			wantContain: []string{"--sandbox", "read-only"},
			wantAbsent:  []string{"--ask-for-approval", "--dangerously-bypass-approvals-and-sandbox"},
		},
		{
			name:        "codex non-readonly with bypassPerm keeps full bypass",
			kind:        "codex",
			opts:        LaunchOptions{Model: "gpt-5.4", BypassPerm: true, Readonly: false},
			wantTier:    ReadonlyEnforcementHardSandbox,
			wantLabel:   "硬沙箱",
			wantContain: []string{"--dangerously-bypass-approvals-and-sandbox"},
			wantAbsent:  []string{"--sandbox"},
		},
		{
			name:        "claude readonly uses plan permission-mode and disallows write tools",
			kind:        "claude",
			opts:        LaunchOptions{Model: "sonnet", Effort: "medium", BypassPerm: true, Readonly: true},
			wantTier:    ReadonlyEnforcementPermissionMode,
			wantLabel:   "只读权限模式",
			wantContain: []string{"--model", "sonnet", "--effort", "medium", "--permission-mode", "plan", "--disallowedTools", "Edit,Write,NotebookEdit"},
			wantAbsent:  []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "claude non-readonly with bypassPerm keeps skip-permissions",
			kind:        "claude",
			opts:        LaunchOptions{Model: "sonnet", BypassPerm: true, Readonly: false},
			wantTier:    ReadonlyEnforcementPermissionMode,
			wantLabel:   "只读权限模式",
			wantContain: []string{"--dangerously-skip-permissions"},
			wantAbsent:  []string{"--permission-mode", "--disallowedTools"},
		},
		{
			name:        "agy readonly stays soft constraint and preserves bypassPerm",
			kind:        "agy",
			opts:        LaunchOptions{Model: "gemini-4-neon", Effort: "xhigh", BypassPerm: true, Readonly: true},
			wantTier:    ReadonlyEnforcementSoftPrompt,
			wantLabel:   "软约束",
			wantContain: []string{"--model", "gemini-4-neon-xhigh", "--dangerously-skip-permissions"},
			wantAbsent:  []string{"--sandbox", "--mode"},
		},
		{
			name:        "codebuddy readonly stays soft constraint and preserves bypassPerm",
			kind:        "codebuddy",
			opts:        LaunchOptions{Model: "deepseek-v4-pro", Effort: "high", BypassPerm: true, Readonly: true},
			wantTier:    ReadonlyEnforcementSoftPrompt,
			wantLabel:   "软约束",
			wantContain: []string{"--model", "deepseek-v4-pro", "--effort", "high", "--dangerously-skip-permissions"},
			wantAbsent:  []string{"--sandbox", "--permission-mode"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad, err := reg.Get(tc.kind)
			if err != nil {
				t.Fatalf("Get(%q): %v", tc.kind, err)
			}
			if got := ad.ReadonlyEnforcement(); got != tc.wantTier {
				t.Fatalf("ReadonlyEnforcement() = %q, want %q", got, tc.wantTier)
			}
			if lbl := ReadonlyEnforcementLabel(ad.ReadonlyEnforcement()); lbl != tc.wantLabel {
				t.Fatalf("ReadonlyEnforcementLabel() = %q, want %q", lbl, tc.wantLabel)
			}
			args := ad.LaunchArgsWithOptions(tc.opts)
			for _, want := range tc.wantContain {
				if !slices.Contains(args, want) {
					t.Fatalf("LaunchArgsWithOptions(%+v) = %v missing %q", tc.opts, args, want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if slices.Contains(args, bad) {
					t.Fatalf("LaunchArgsWithOptions(%+v) = %v must not contain %q", tc.opts, args, bad)
				}
			}
		})
	}
}

func TestReadonlyFlagsNotOverriddenByNeedsBypassHeal(t *testing.T) {
	u32 := func(v uint32) *uint32 { return &v }
	for _, tc := range []struct {
		name string
		kind string
		argv []string
	}{
		{"codex sandbox read-only", "codex", []string{"codex", "--sandbox", "read-only", "--ask-for-approval", "never"}},
		{"claude permission-mode plan", "claude", []string{"claude", "--permission-mode", "plan", "--disallowedTools", "Edit,Write,NotebookEdit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, _ := NeedsBypassHeal(tc.kind, &herdr.PaneProcessInfo{
				PaneID:                   "w1:p1",
				ForegroundProcessGroupID: u32(900),
				ForegroundProcesses:      []herdr.PaneProcess{{PID: 900, Name: tc.kind, Argv: tc.argv}},
			})
			if need {
				t.Fatalf("expected NeedsBypassHeal=false for readonly %s argv=%v", tc.kind, tc.argv)
			}
		})
	}
}

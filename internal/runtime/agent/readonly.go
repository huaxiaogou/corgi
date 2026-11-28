package agent

import "strings"

// ReadonlyEnforcement describes how strongly an agent CLI enforces read-only isolation.
type ReadonlyEnforcement string

const (
	ReadonlyEnforcementNone           ReadonlyEnforcement = ""
	ReadonlyEnforcementHardSandbox    ReadonlyEnforcement = "hard_sandbox"    // OS-level read-only sandbox (Codex --sandbox read-only)
	ReadonlyEnforcementPermissionMode ReadonlyEnforcement = "permission_mode" // CLI read-only permission mode + denied write tools (Claude)
	ReadonlyEnforcementSoftPrompt     ReadonlyEnforcement = "soft_prompt"     // Prompt-only soft constraint (agy/codebuddy; preserves unattended mode)
)

// ReadonlyEnforcementLabel returns the concise human-facing Chinese label for an enforcement tier.
func ReadonlyEnforcementLabel(e ReadonlyEnforcement) string {
	switch e {
	case ReadonlyEnforcementHardSandbox:
		return "硬沙箱"
	case ReadonlyEnforcementPermissionMode:
		return "只读权限模式"
	case ReadonlyEnforcementSoftPrompt:
		return "软约束"
	default:
		return ""
	}
}

// LaunchOptions configures adapter CLI flags for spawning or resuming an agent session.
type LaunchOptions struct {
	Model      string
	Effort     string
	BypassPerm bool
	Readonly   bool
}

var claudeReadonlyDisallowedTools = strings.Join([]string{"Edit", "Write", "NotebookEdit"}, ",")

func (c *CodexAdapter) ReadonlyEnforcement() ReadonlyEnforcement {
	return ReadonlyEnforcementHardSandbox
}

func (c *CodexAdapter) LaunchArgsWithOptions(opts LaunchOptions) []string {
	if !opts.Readonly {
		return c.LaunchArgs(opts.Model, opts.Effort, opts.BypassPerm)
	}
	args := c.LaunchArgs(opts.Model, opts.Effort, false)
	args = append(args, "--sandbox", "read-only")
	if opts.BypassPerm {
		args = append(args, "--ask-for-approval", "never")
	}
	return args
}

func (c *ClaudeAdapter) ReadonlyEnforcement() ReadonlyEnforcement {
	return ReadonlyEnforcementPermissionMode
}

func (c *ClaudeAdapter) LaunchArgsWithOptions(opts LaunchOptions) []string {
	if !opts.Readonly {
		return c.LaunchArgs(opts.Model, opts.Effort, opts.BypassPerm)
	}
	args := c.LaunchArgs(opts.Model, opts.Effort, false)
	args = append(args, "--permission-mode", "plan", "--disallowedTools", claudeReadonlyDisallowedTools)
	return args
}

func (a *AgyAdapter) ReadonlyEnforcement() ReadonlyEnforcement {
	return ReadonlyEnforcementSoftPrompt
}

func (a *AgyAdapter) LaunchArgsWithOptions(opts LaunchOptions) []string {
	// Antigravity CLI lacks a local OS read-only sandbox that preserves unattended execution;
	// keep prompt-level soft constraint without stripping bypassPerm no-disturb mode.
	return a.LaunchArgs(opts.Model, opts.Effort, opts.BypassPerm)
}

func (c *CodeBuddyAdapter) ReadonlyEnforcement() ReadonlyEnforcement {
	return ReadonlyEnforcementSoftPrompt
}

func (c *CodeBuddyAdapter) LaunchArgsWithOptions(opts LaunchOptions) []string {
	// CodeBuddy's --sandbox targets Docker/Podman/E2B rather than a local read-only FS sandbox;
	// keep prompt-level soft constraint without stripping bypassPerm no-disturb mode.
	return c.LaunchArgs(opts.Model, opts.Effort, opts.BypassPerm)
}

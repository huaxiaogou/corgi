package agent

import (
	"path/filepath"
	"strings"

	"github.com/kiperjing/corgi/internal/runtime/herdr"
)

// BypassInspection summarizes whether a live agent process in a herdr pane is running
// with its bypass-permissions flag (or an explicit user mode override).
type BypassInspection struct {
	Found        bool
	PID          uint32
	Argv         []string
	Cmdline      string
	HasBypass    bool
	ExplicitMode bool
}

// InspectBypassProcess examines herdr's pane.process_info for the foreground process
// matching agent kind and checks whether bypass-permissions arguments are present.
// When process metadata or argv/cmdline is unavailable, Found is false (fail-safe).
func InspectBypassProcess(rawKind string, info *herdr.PaneProcessInfo) BypassInspection {
	kind := NormalizeKind(rawKind)
	if kind == "" || info == nil || len(info.ForegroundProcesses) == 0 {
		return BypassInspection{}
	}

	var matched *herdr.PaneProcess
	fgPGID := uint32(0)
	if info.ForegroundProcessGroupID != nil {
		fgPGID = *info.ForegroundProcessGroupID
	}
	for i := range info.ForegroundProcesses {
		p := &info.ForegroundProcesses[i]
		if !processMatchesKind(kind, p) {
			continue
		}
		if matched == nil || (fgPGID != 0 && p.PID == fgPGID) {
			matched = p
		}
	}
	if matched == nil {
		return BypassInspection{}
	}

	tokens, cmdline := processTokens(matched)
	if len(tokens) == 0 {
		return BypassInspection{}
	}

	return BypassInspection{
		Found:        true,
		PID:          matched.PID,
		Argv:         tokens,
		Cmdline:      cmdline,
		HasBypass:    tokensHaveBypassFlag(kind, tokens),
		ExplicitMode: tokensHaveExplicitMode(kind, tokens),
	}
}

// NeedsBypassHeal reports whether a pane's live agent process is confirmed to be running
// without bypass-permissions and without an explicit user mode override (e.g. after a
// herdr daemon restart restored a bare `agy --conversation <id>` or `claude --resume <id>`).
func NeedsBypassHeal(rawKind string, info *herdr.PaneProcessInfo) (bool, uint32) {
	insp := InspectBypassProcess(rawKind, info)
	if !insp.Found || insp.HasBypass || insp.ExplicitMode {
		return false, insp.PID
	}
	return true, insp.PID
}

func processTokens(p *herdr.PaneProcess) ([]string, string) {
	cmdline := ""
	if p.Cmdline != nil {
		cmdline = strings.TrimSpace(*p.Cmdline)
	}
	if len(p.Argv) > 0 {
		out := make([]string, 0, len(p.Argv))
		for _, a := range p.Argv {
			if t := strings.TrimSpace(a); t != "" {
				out = append(out, t)
			}
		}
		if cmdline == "" {
			cmdline = strings.Join(out, " ")
		}
		return out, cmdline
	}
	if cmdline != "" {
		return strings.Fields(cmdline), cmdline
	}
	return nil, ""
}

func processMatchesKind(kind string, p *herdr.PaneProcess) bool {
	if p == nil {
		return false
	}
	names := []string{cleanProcName(p.Name)}
	if p.Argv0 != nil {
		names = append(names, cleanProcName(*p.Argv0))
	}
	if len(p.Argv) > 0 {
		names = append(names, cleanProcName(p.Argv[0]))
	}
	for _, n := range names {
		if n == "" || strings.Contains(n, "mcp-server") {
			continue
		}
		switch kind {
		case "agy":
			if n == "agy" || n == "antigravity" || n == "antigravity-cli" {
				return true
			}
		case "claude":
			if n == "claude" {
				return true
			}
		case "codex":
			if n == "codex" {
				return true
			}
		case "codebuddy":
			if n == "codebuddy" || n == "cbc" {
				return true
			}
		}
	}

	// Handle Node/Bun script wrappers (e.g. `node /path/to/claude --resume ...`).
	isWrapper := false
	for _, n := range names {
		if n == "node" || n == "bun" || n == "npx" {
			isWrapper = true
			break
		}
	}
	if !isWrapper {
		return false
	}
	tokens, _ := processTokens(p)
	for i := 1; i < len(tokens) && i <= 3; i++ {
		argBase := cleanProcName(tokens[i])
		if strings.Contains(argBase, "mcp-server") {
			return false
		}
		switch kind {
		case "claude":
			if argBase == "claude" || strings.Contains(strings.ToLower(tokens[i]), "claude-code") {
				return true
			}
		case "codex":
			if argBase == "codex" || strings.Contains(strings.ToLower(tokens[i]), "/codex") {
				return true
			}
		case "codebuddy":
			if argBase == "codebuddy" || argBase == "cbc" || strings.Contains(strings.ToLower(tokens[i]), "codebuddy") {
				return true
			}
		}
	}
	return false
}

func cleanProcName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimLeft(s, "-")
	s = filepath.Base(s)
	s = strings.ToLower(strings.TrimSuffix(s, ".exe"))
	return s
}

func tokensHaveBypassFlag(kind string, tokens []string) bool {
	for i, tok := range tokens {
		low := strings.ToLower(tok)
		switch kind {
		case "agy":
			if low == "--dangerously-skip-permissions" {
				return true
			}
		case "claude":
			if low == "--dangerously-skip-permissions" || low == "--permission-mode=bypasspermissions" {
				return true
			}
			if low == "--permission-mode" && i+1 < len(tokens) && strings.EqualFold(tokens[i+1], "bypasspermissions") {
				return true
			}
		case "codex":
			if low == "--dangerously-bypass-approvals-and-sandbox" || low == "--yolo" {
				return true
			}
		case "codebuddy":
			if low == "--dangerously-skip-permissions" || low == "-y" || low == "--yes" || low == "--yolo" {
				return true
			}
		}
	}
	return false
}

func tokensHaveExplicitMode(kind string, tokens []string) bool {
	for i, tok := range tokens {
		low := strings.ToLower(tok)
		switch kind {
		case "agy":
			if low == "--mode" || strings.HasPrefix(low, "--mode=") {
				return true
			}
		case "claude":
			if low == "--permission-mode" && i+1 < len(tokens) && !strings.EqualFold(tokens[i+1], "bypasspermissions") {
				return true
			}
			if strings.HasPrefix(low, "--permission-mode=") && low != "--permission-mode=bypasspermissions" {
				return true
			}
		case "codex":
			if low == "-a" || low == "--ask-for-approval" || strings.HasPrefix(low, "--ask-for-approval=") ||
				low == "-s" || low == "--sandbox" || strings.HasPrefix(low, "--sandbox=") {
				return true
			}
		case "codebuddy":
			if low == "--permission-mode" || strings.HasPrefix(low, "--permission-mode=") {
				return true
			}
		}
	}
	return false
}

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

// handingOver reports whether a Router context handover is currently active.
func (e *Engine) handingOver() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.handover != nil
}

// claudeContextWindow resolves the compaction window for Claude Code sessions.
func (e *Engine) claudeContextWindow(routerModel string) int {
	cfgDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfgDir == "" {
		cfgDir = filepath.Join(e.home, ".claude")
	}
	var cfg struct {
		Model string            `json:"model"`
		Env   map[string]string `json:"env"`
	}
	if b, err := os.ReadFile(filepath.Join(cfgDir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	for _, k := range []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_CODE_MAX_CONTEXT_TOKENS"} {
		if raw := strings.TrimSpace(cfg.Env[k]); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 8000 {
				return n
			}
		}
	}
	m := strings.TrimSpace(routerModel)
	if m == "" {
		m = strings.TrimSpace(cfg.Model)
	}
	if m == "" {
		m = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	}
	low := strings.ToLower(m)
	if strings.Contains(low, "[1m]") ||
		strings.Contains(low, "opus-5") ||
		strings.Contains(low, "opus-4-6") ||
		strings.Contains(low, "sonnet-4-6") {
		return 1000000
	}
	return 200000
}

// checkHandover tracks Router context fill and rotates to a fresh session with memory flush
// once context reaches RotatePct between conversations.
func (e *Engine) checkHandover(ctx context.Context, live map[string]liveInfo, hasQueued bool) {
	li, alive := live[model.RouterName]
	e.mu.Lock()
	h := e.handover
	if !alive || e.dream != nil {
		e.routerUsage = model.ContextUsage{}
		e.mu.Unlock()
		return
	}
	rotateAt := e.rotatePctLocked()
	paused := e.settings.Paused
	routerModel := e.settings.RouterModel
	launched := e.routerLaunchAt
	askedAt := e.routerAskedAt
	e.mu.Unlock()

	kind := agent.NormalizeKind(li.kind)
	if kind == "" {
		kind = agent.NormalizeKind(e.Settings().RouterKind)
	}
	var (
		usage          model.ContextUsage
		transcriptPath string
	)
	if ad, err := e.agents.Get(kind); err == nil {
		if path := ad.LocateTranscript(e.home, e.memory.RouterDir(), li.session, launched); path != "" {
			transcriptPath = path
			defaultWindow := 0
			if kind == "claude" {
				defaultWindow = e.claudeContextWindow(routerModel)
			}
			usage = ad.ReadContextUsage(path, defaultWindow)
		}
	}

	e.mu.Lock()
	e.routerUsage = usage
	e.mu.Unlock()

	idle := li.status == "idle" || li.status == "done"
	if h != nil {
		e.mu.Lock()
		if li.status == "working" {
			h.worked = true
		}
		worked := h.worked
		e.mu.Unlock()

		wait := time.Since(h.at)
		if (worked && idle && wait > 5*time.Second) ||
			(!worked && idle && wait > 90*time.Second) ||
			wait > 5*time.Minute {
			e.finishHandover(ctx, h)
		}
		return
	}

	// Derive session ID from transcript filename when herdr does not populate agent_session (e.g. codebuddy).
	sessionID := li.session
	if sessionID == "" && transcriptPath != "" {
		sessionID = extractSessionID("path", transcriptPath)
	}

	// Only hand over cleanly between conversations. Never block rotation on empty sessionID,
	// because preventing context-window bloat is strictly more important than history archiving.
	if rotateAt <= 0 || paused || !idle || hasQueued || !askedAt.IsZero() ||
		usage.Pct() < rotateAt || time.Since(launched) < 5*time.Minute {
		return
	}

	// Claim reservation before sending handover prompt so a concurrent Hub restart cannot race.
	e.mu.Lock()
	if e.hubUpdating || e.dream != nil || e.handover != nil || e.routerDispatching || e.routerLaunching || e.settings.Paused {
		e.mu.Unlock()
		return
	}
	e.routerDispatching = true
	e.mu.Unlock()

	prompt := fmt.Sprintf(`[corgi] Handover. Your context window is %d%% full, so Corgi will restart you in a fresh session right after this turn. Nothing carries over except memory/ (layout in CORGI.md) and Hub SQLite work_items, so bring memory/ up to date now:
- instructions.md: standing instructions and preferences (merge duplicates, ≤80 chars/line).
- notes/: free-form deep project context and reusable lessons learned; keep notes/index.md and notes/agents.md in sync (do NOT create or maintain open-task checklists under memory/tasks/ — Hub SQLite work_items is the sole open-task ledger).
Keep up-front files (instructions.md, notes/index.md) under ~%d KB combined.
This turn is for memory files alone: edit them, then stop.`, usage.Pct(), UpFrontBudgetBytes>>10)

	if err := e.memory.WriteRouterDocs(e.binPath, e.Settings()); err != nil {
		e.logger.Warn("write router docs before handover", "err", err)
	}
	if err := e.fleet.Prompt(ctx, model.RouterName, prompt); err != nil {
		e.mu.Lock()
		e.routerDispatching = false
		e.mu.Unlock()
		e.logger.Warn("send handover prompt failed", "err", err)
		return
	}

	e.mu.Lock()
	e.handover = &handoverState{at: time.Now(), kind: kind, session: sessionID, pct: usage.Pct()}
	e.routerDispatching = false
	e.mu.Unlock()

	e.addEvent(ctx, model.MainThread,
		fmt.Sprintf("Corgi 上下文已用 %d%%，正在把要记住的事落盘到记忆，马上交接至新会话", usage.Pct()),
		"", "info")
	e.notifyChanged()
}

func (e *Engine) finishHandover(ctx context.Context, h *handoverState) {
	eid, _, _ := e.store.NextID(ctx, "h")
	if h.session != "" {
		_ = e.store.SaveEndedSession(ctx, &model.EndedSession{
			ID:      eid,
			Kind:    h.kind,
			Cwd:     e.memory.RouterDir(),
			Name:    model.RouterName,
			Session: h.session,
			EndedAt: time.Now(),
			Title:   "Corgi · 交接前的会话（" + h.at.Format("01-02 15:04") + "）",
		}, 40)
	}
	if err := e.RestartRouter(ctx); err != nil {
		e.logger.Warn("handover restart router failed", "err", err)
	}

	e.mu.Lock()
	e.handover = nil
	e.routerUsage = model.ContextUsage{}
	e.mu.Unlock()

	ref := ""
	if h.session != "" {
		ref = "h:" + eid
	}
	e.addEvent(ctx, model.MainThread, "Corgi 已切换至全新会话并完成记忆交接", ref, "info")
	e.notifyChanged()
}

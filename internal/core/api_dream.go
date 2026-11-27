package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	maxAPIDreamSteps            = 16
	maxAPIDreamToolCalls        = 24
	maxAPIDreamInputRunes       = 36000
	maxAPIDreamToolOutputBytes  = 8192
	maxAPIDreamMemoryWriteBytes = 32 << 10
	apiDreamSystemPromptV1      = `你是 Corgi 的夜间记忆整理助手（Dream），在 Hub 进程内于 Corgi 休眠时运行。你的职责是复盘上次做梦以来的对话与事件，整理 memory/ 持久记忆，并在最后调用 finish_dream 落盘本次做梦日志。

整理原则：
1. 唯一开放任务账本：Hub SQLite 中的 work_items 是唯一权威开放任务账本。绝不在 memory/tasks/ 创建或维护待办清单。
2. 精炼 instructions.md：每行一条主人的长期偏好或硬规则（≤80字）；把详细流程、历史背景沉到 notes/<topic>.md，仅在 instructions.md 留简短指针（如「见 notes/x.md」）；合并重复项，移除已有活跃定时器 + notes/ 支撑的重复定时描述。
3. 提炼跨会话规律：将在多轮中反复出现的纠偏、隐性偏好、避坑经验记录到 instructions.md 或 notes/<topic>.md，标注「(inferred)」。
4. 维护 notes/ 与 notes/index.md：把可复用工程经验（路径、有效命令、踩坑记录、各机各模型表现等）写入单层 notes/<topic>.md，并保持 notes/index.md 准确且简洁；instructions.md 与 notes/index.md 合计尽量控制在 ~10KB 内。
5. Skill 复盘：
   - 若 notes/ 中某可复用流程已反复使用且趋于稳定、尚未做成 skill，可在 finish_dream 的 skill_candidates 中列出简短名称；
   - 若现有 skill 在日志中反复导致卡点或绕路，可在 finish_dream 的 skill_fixes 中以「名称（修复点）」列出；
   - 无则留空，不要硬凑。
6. 严禁删除或篡改主人明确给出的长期规则（除非有更新的明确指令覆盖）。memory/messages.md 为系统只读文件，不可写入。
7. 必须通过调用 finish_dream 完成本次做梦：提供一句精炼中文 summary（面向主人展示）和简洁 journal（≤30行变更与观察记录）。`
)

func apiDreamToolDefs() []LLMToolDef {
	return []LLMToolDef{
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "finish_dream",
				Description: "原子落盘本次做梦日志 dreams/<YYYY-MM-DD-HHMM>.md 并结束做梦。整理完毕后必须调用此工具。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary":          map[string]any{"type": "string", "description": "一句话中文总结本次整理了什么（展示给主人，≤120字）"},
						"journal":          map[string]any{"type": "string", "description": "简洁的变更记录与观察要点（≤30行）"},
						"skill_candidates": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：已成熟且可沉淀为 skill 的流程名称列表"},
						"skill_fixes":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选：存在卡点可改进的已有 skill，格式为「名称（修复点）」"},
					},
					"required": []string{"summary", "journal"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "read_memory",
				Description: "按需分页读取 memory/ 或 dreams/ 下的单个 .md 记忆文件。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":   map[string]any{"type": "string", "description": "相对路径，如 'instructions.md'、'notes/agents.md' 或 'dreams/2026-11-27-0400.md'"},
						"offset": map[string]any{"type": "integer", "description": "字符分页偏移量（默认 0）"},
						"limit":  map[string]any{"type": "integer", "description": "最大返回字符数（默认 4000，上限 7000）"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: LLMToolFunctionSpec{
				Name:        "write_memory",
				Description: "原子写入 memory/instructions.md 或单层 memory/notes/<slug>.md（≤32KB；禁止写 messages.md 与 tasks/）。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "相对路径，仅允许 'instructions.md' 或 'notes/<slug>.md'"},
						"content": map[string]any{"type": "string", "description": "完整新内容（≤32KB）"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
	}
}

func (e *Engine) runAPIDream(d *dreamRun, _ model.Settings) {
	e.mu.Lock()
	rootCtx := e.runCtx
	e.mu.Unlock()
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	// API Dream is an async background task: no arbitrary wall-clock deadline.
	// Stream health is monitored by ChatCompletion's per-chunk SSE idle watchdog (60s)
	// and step/tool progress tracking; genuine faults are reported back to the Brain.
	dreamCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	e.mu.Lock()
	if e.dream == d {
		d.cancel = cancel
		d.lastProgressAt = time.Now()
	}
	e.mu.Unlock()

	ok := false
	errMsg := ""
	errIsAuth := false
	defer func() {
		e.mu.Lock()
		if e.dream == d {
			d.apiDone = true
			d.apiOK = ok
			d.apiErr = errMsg
			d.apiErrIsAuth = errIsAuth
			d.cancel = nil
		}
		e.mu.Unlock()
		e.wake()
	}()

	cfg, modelName, effort, cacheKey, err := e.resolveActiveRouterAPIConfig(dreamCtx)
	if err != nil {
		errMsg = oneLineClip(err.Error(), 140)
		errIsAuth = errors.Is(err, ErrLLMAuth)
		if e.logger != nil {
			e.logger.Warn("api dream config failed", "id", d.id, "err", err)
		}
		return
	}
	if err := e.memory.Ensure(); err != nil {
		errMsg = oneLineClip(err.Error(), 140)
		if e.logger != nil {
			e.logger.Warn("api dream ensure memory failed", "id", d.id, "err", err)
		}
		return
	}
	if err := e.writeDreamInput(dreamCtx); err != nil {
		errMsg = oneLineClip(err.Error(), 140)
		if e.logger != nil {
			e.logger.Warn("api dream write input failed", "id", d.id, "err", err)
		}
		return
	}

	userMsg := e.buildAPIDreamUserMessage(d.at)
	messages := []LLMMessage{
		{Role: "system", Content: apiDreamSystemPromptV1},
		{Role: "user", Content: userMsg},
	}
	tools := apiDreamToolDefs()
	totalCalls := 0
	nudgedFinish := false

	for step := 0; step < maxAPIDreamSteps; step++ {
		if dreamCtx.Err() != nil {
			errMsg = oneLineClip(dreamCtx.Err().Error(), 120)
			return
		}
		e.mu.Lock()
		if e.dream == d {
			d.step = step + 1
			d.lastProgressAt = time.Now()
		}
		e.mu.Unlock()

		resp, err := ChatCompletion(dreamCtx, cfg, LLMChatRequest{
			Model:           modelName,
			Messages:        messages,
			Tools:           tools,
			ReasoningEffort: effort,
			PromptCacheKey:  cacheKey + ":dream",
			Stream:          true,
			StreamOptions:   &LLMStreamOptions{IncludeUsage: true},
		})
		if err != nil {
			errMsg = oneLineClip(err.Error(), 140)
			errIsAuth = errors.Is(err, ErrLLMAuth)
			if e.logger != nil {
				e.logger.Warn("api dream llm call failed", "id", d.id, "step", step+1, "model", modelName, "effort", effort, "err", err)
			}
			return
		}
		choice := resp.Choices[0].Message
		if e.logger != nil {
			cached := 0
			if resp.Usage.PromptTokensDetails != nil {
				cached = resp.Usage.PromptTokensDetails.CachedTokens
			}
			reasoning := 0
			if resp.Usage.CompletionTokensDetails != nil {
				reasoning = resp.Usage.CompletionTokensDetails.ReasoningTokens
			}
			toolNames := make([]string, 0, len(choice.ToolCalls))
			for _, tc := range choice.ToolCalls {
				toolNames = append(toolNames, tc.Function.Name)
			}
			e.logger.Info("api dream step",
				"id", d.id, "step", step+1, "model", modelName, "effort", effort,
				"ttft_ms", resp.TTFT.Milliseconds(),
				"latency_ms", resp.Latency.Milliseconds(),
				"prompt_tokens", resp.Usage.PromptTokens,
				"cached_tokens", cached,
				"completion_tokens", resp.Usage.CompletionTokens,
				"reasoning_tokens", reasoning,
				"tools", strings.Join(toolNames, ","),
				"request_id", resp.RequestID)
		}

		if len(choice.ToolCalls) == 0 {
			if !nudgedFinish && step < maxAPIDreamSteps-1 {
				nudgedFinish = true
				messages = append(messages,
					LLMMessage{Role: "assistant", Content: strings.TrimSpace(choice.Content)},
					LLMMessage{Role: "user", Content: "请调用 finish_dream 工具，提供 summary 与 journal 落盘本次做梦日志以结束任务。"},
				)
				continue
			}
			errMsg = "模型未调用 finish_dream 完成日志落盘"
			return
		}

		messages = append(messages, LLMMessage{
			Role:      "assistant",
			Content:   choice.Content,
			ToolCalls: choice.ToolCalls,
		})

		done := false
		for _, tc := range choice.ToolCalls {
			totalCalls++
			e.mu.Lock()
			if e.dream == d {
				d.lastTool = tc.Function.Name
				d.lastProgressAt = time.Now()
			}
			e.mu.Unlock()

			var toolOut string
			if totalCalls > maxAPIDreamToolCalls {
				if tc.Function.Name == "finish_dream" {
					toolOut, done = e.execAPIDreamTool(d, tc)
				} else {
					toolOut = toolErr("工具调用次数已达上限，请立即调用 finish_dream 落盘本次做梦日志。")
				}
			} else {
				toolOut, done = e.execAPIDreamTool(d, tc)
			}
			if strings.Contains(toolOut, `"ok":false`) && e.logger != nil {
				e.logger.Warn("api dream tool rejected",
					"id", d.id, "step", step+1, "tool", tc.Function.Name, "result", oneLineClip(toolOut, 240))
			}
			if len(toolOut) > maxAPIDreamToolOutputBytes {
				toolOut = toolOut[:maxAPIDreamToolOutputBytes] + "...[truncated]"
			}
			messages = append(messages, LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    toolOut,
			})
			if done {
				ok = true
				return
			}
		}
		if step == maxAPIDreamSteps-2 {
			messages = append(messages, LLMMessage{
				Role:    "user",
				Content: "即将达到最大整理步数，请立即调用 finish_dream 落盘本次做梦日志。",
			})
		}
	}
	if !ok && errMsg == "" {
		errMsg = fmt.Sprintf("达到最大步数（%d 步）仍未完成做梦落盘", maxAPIDreamSteps)
	}
}

func (e *Engine) buildAPIDreamUserMessage(dreamAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "本次做梦时间：%s（日志文件名：dreams/%s.md，up-front 预算约 %d KB）\n",
		dreamAt.Format("2006-01-02 15:04"), journalName(dreamAt), UpFrontBudgetBytes>>10)

	appendCapped := func(title, rel string, maxRunes int) {
		raw, err := readConfinedMemoryFile(e.memory.MemDir(), rel, 256<<10)
		if err != nil {
			return
		}
		txt := strings.TrimSpace(string(raw))
		if txt == "" {
			return
		}
		r := []rune(txt)
		if len(r) > maxRunes {
			txt = string(r[:maxRunes]) + "\n...[truncated, use read_memory for more]"
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", title, txt)
	}

	appendCapped("当前 memory/instructions.md", "instructions.md", 6000)
	appendCapped("当前 memory/notes/index.md", "notes/index.md", 5000)

	// List filenames only (never load all memory file bodies upfront).
	memNames := e.listMemoryFileNames()
	if len(memNames) > 0 {
		b.WriteString("\n## memory/ 文件清单（仅文件名，需内容请调用 read_memory）\n")
		for _, name := range memNames {
			b.WriteString("- " + name + "\n")
		}
	}
	if dreamNames := e.listRecentDreamNames(5); len(dreamNames) > 0 {
		b.WriteString("\n## 近期 dreams/ 日志清单（仅文件名）\n")
		for _, name := range dreamNames {
			b.WriteString("- dreams/" + name + "\n")
		}
	}

	// Append dream/input.md within remaining rune budget (~36K total).
	inputPath := filepath.Join(e.memory.RouterDir(), "dream", "input.md")
	if raw, err := os.ReadFile(inputPath); err == nil {
		inputText := strings.TrimSpace(string(raw))
		remaining := maxAPIDreamInputRunes - len([]rune(b.String())) - 200
		if remaining < 4000 {
			remaining = 4000
		}
		inputText = clipDreamInputBudget(inputText, remaining)
		b.WriteString("\n## 近期上下文（dream/input.md）\n")
		b.WriteString(inputText)
		b.WriteByte('\n')
	}
	return b.String()
}

func (e *Engine) listMemoryFileNames() []string {
	if e.memory == nil {
		return nil
	}
	root := e.memory.MemDir()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		if rel, rErr := filepath.Rel(root, p); rErr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (e *Engine) listRecentDreamNames(limit int) []string {
	if e.memory == nil || limit <= 0 {
		return nil
	}
	entries, err := os.ReadDir(e.memory.DreamsDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".md") || strings.HasPrefix(en.Name(), ".") {
			continue
		}
		names = append(names, en.Name())
	}
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}

func clipDreamInputBudget(text string, maxRunes int) string {
	r := []rune(text)
	if len(r) <= maxRunes {
		return text
	}
	// Preserve the opening Authoritative Open Work Items header and the newest tail of chat/logs.
	headLen := maxRunes / 3
	if headLen > 6000 {
		headLen = 6000
	}
	tailLen := maxRunes - headLen - 40
	if tailLen < 1000 {
		return string(r[:maxRunes])
	}
	return string(r[:headLen]) + "\n\n...[中间较早记录已按预算省略]...\n\n" + string(r[len(r)-tailLen:])
}

func (e *Engine) execAPIDreamTool(d *dreamRun, tc LLMToolCall) (string, bool) {
	switch tc.Function.Name {
	case "read_memory":
		return e.toolDreamReadMemory(tc.Function.Arguments), false
	case "write_memory":
		return e.toolDreamWriteMemory(tc.Function.Arguments), false
	case "finish_dream":
		return e.toolDreamFinish(d, tc.Function.Arguments)
	default:
		return toolErr("未知做梦工具：" + tc.Function.Name), false
	}
}

func readConfinedMemoryFile(rootDir, rel string, maxBytes int64) ([]byte, error) {
	realRoot, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, fmt.Errorf("记忆目录不可用")
	}
	realRoot = filepath.Clean(realRoot)
	full := filepath.Join(realRoot, filepath.FromSlash(rel))
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无法读取：%s", rel)
	}
	realFull = filepath.Clean(realFull)
	if !strings.HasPrefix(realFull, realRoot+string(os.PathSeparator)) {
		return nil, fmt.Errorf("符号链接越界，已拒绝读取")
	}
	fi, err := os.Stat(realFull)
	if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("目标不是普通文件：%s", rel)
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		return nil, fmt.Errorf("文件超过 %dKB 上限", maxBytes>>10)
	}
	return os.ReadFile(realFull)
}

func (e *Engine) toolDreamReadMemory(rawArgs string) string {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("read_memory 参数 JSON 无效：" + err.Error())
	}
	if e.memory == nil {
		return toolErr("memory manager unavailable")
	}
	rawPath := strings.TrimSpace(args.Path)
	if rawPath == "" {
		return toolErr("path 不能为空")
	}
	slashed := filepath.ToSlash(rawPath)
	if strings.HasPrefix(slashed, "/") || strings.Contains(slashed, "..") {
		return toolErr("非法路径：仅允许读取 memory/ 或 dreams/ 下的相对 .md 路径")
	}
	var (
		rootDir string
		rel     string
	)
	if rest, ok := strings.CutPrefix(slashed, "dreams/"); ok {
		rootDir = e.memory.DreamsDir()
		rel = rest
	} else {
		rootDir = e.memory.MemDir()
		rel = strings.TrimPrefix(slashed, "memory/")
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") || !strings.HasSuffix(rel, ".md") {
		return toolErr("仅允许读取 memory/ 或 dreams/ 下的 .md 文件")
	}

	raw, err := readConfinedMemoryFile(rootDir, rel, 256<<10)
	if err != nil {
		return toolErr(err.Error())
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 4000
	}
	if limit > 7000 {
		limit = 7000
	}
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	runes := []rune(string(raw))
	total := len(runes)
	if offset >= total {
		return toolJSON(map[string]any{"ok": true, "total_chars": total, "offset": offset, "has_more": false, "content": ""})
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return toolJSON(map[string]any{
		"ok":          true,
		"total_chars": total,
		"offset":      offset,
		"has_more":    end < total,
		"content":     string(runes[offset:end]),
	})
}

func (e *Engine) toolDreamWriteMemory(rawArgs string) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("write_memory 参数 JSON 无效：" + err.Error())
	}
	if e.memory == nil {
		return toolErr("memory manager unavailable")
	}
	rawPath := strings.TrimSpace(args.Path)
	slashed := filepath.ToSlash(rawPath)
	if strings.HasPrefix(slashed, "/") || strings.Contains(slashed, "..") {
		return toolErr("非法路径：仅允许写入 instructions.md 或单层 notes/<slug>.md")
	}
	rel := strings.TrimPrefix(slashed, "memory/")
	rel = filepath.ToSlash(filepath.Clean(rel))

	allowed := false
	if rel == "instructions.md" {
		allowed = true
	} else if rest, ok := strings.CutPrefix(rel, "notes/"); ok {
		if rest != "" && rest != ".md" && !strings.Contains(rest, "/") && !strings.HasPrefix(rest, ".") && strings.HasSuffix(rest, ".md") {
			allowed = true
		}
	}
	if !allowed {
		return toolErr("write_memory 仅允许写入 instructions.md 或单层 notes/<slug>.md（禁止写 messages.md 或 tasks/）")
	}
	if len(args.Content) > maxAPIDreamMemoryWriteBytes {
		return toolErr("单份记忆文件不能超过 32KB")
	}

	memRoot := e.memory.MemDir()
	realMemRoot, err := filepath.EvalSymlinks(memRoot)
	if err != nil {
		return toolErr("记忆目录不可用：" + err.Error())
	}
	realMemRoot = filepath.Clean(realMemRoot)
	target := filepath.Join(realMemRoot, filepath.FromSlash(rel))
	targetDir := filepath.Dir(target)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return toolErr("创建目录失败：" + err.Error())
	}
	realTargetDir, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		return toolErr("目标目录不可用：" + err.Error())
	}
	realTargetDir = filepath.Clean(realTargetDir)
	expectedDir := realMemRoot
	if strings.HasPrefix(rel, "notes/") {
		expectedDir = filepath.Join(realMemRoot, "notes")
	}
	if realTargetDir != expectedDir {
		return toolErr("目标目录符号链接越界或被重映射，已拒绝写入")
	}
	if lfi, lerr := os.Lstat(target); lerr == nil {
		if lfi.Mode()&os.ModeSymlink != 0 || lfi.IsDir() {
			return toolErr("目标文件是符号链接或目录，已拒绝覆盖")
		}
	}

	normalized := strings.TrimRight(args.Content, "\n") + "\n"
	if err := writeFileAtomic(target, []byte(normalized), 0o600); err != nil {
		return toolErr("写入记忆失败：" + err.Error())
	}
	e.notifyChanged()
	return toolJSON(map[string]any{"ok": true, "path": rel, "bytes": len(normalized)})
}

func (e *Engine) toolDreamFinish(d *dreamRun, rawArgs string) (string, bool) {
	var args struct {
		Summary         string   `json:"summary"`
		Journal         string   `json:"journal"`
		SkillCandidates []string `json:"skill_candidates"`
		SkillFixes      []string `json:"skill_fixes"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return toolErr("finish_dream 参数 JSON 无效：" + err.Error()), false
	}
	summary := oneLineClip(strings.TrimSpace(args.Summary), 160)
	if summary == "" {
		return toolErr("finish_dream 的 summary 不能为空"), false
	}
	journalBody := strings.TrimSpace(args.Journal)
	if journalBody == "" {
		journalBody = "- 已复盘近期对话并核对持久记忆。"
	}

	cleanList := func(items []string) []string {
		var out []string
		for _, it := range items {
			if s := oneLineClip(strings.TrimSpace(it), 80); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	skills := cleanList(args.SkillCandidates)
	fixes := cleanList(args.SkillFixes)

	var b strings.Builder
	b.WriteString(summary)
	b.WriteByte('\n')
	if len(skills) > 0 {
		b.WriteString(dreamSkillsPrefix)
		b.WriteString(strings.Join(skills, "、"))
		b.WriteByte('\n')
	}
	if len(fixes) > 0 {
		b.WriteString(dreamFixPrefix)
		b.WriteString(strings.Join(fixes, "、"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(journalBody)
	b.WriteByte('\n')

	dreamsDir := e.memory.DreamsDir()
	if err := os.MkdirAll(dreamsDir, 0o700); err != nil {
		return toolErr("创建 dreams 目录失败：" + err.Error()), false
	}
	finalPath := filepath.Join(dreamsDir, journalName(d.at)+".md")
	if err := writeFileAtomic(finalPath, []byte(b.String()), 0o600); err != nil {
		return toolErr("落盘做梦日志失败：" + err.Error()), false
	}
	return toolJSON(map[string]any{
		"ok":      true,
		"journal": "dreams/" + journalName(d.at) + ".md",
		"summary": summary,
	}), true
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".corgi-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

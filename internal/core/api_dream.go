package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxAPIDreamSteps           = 16
	maxAPIDreamToolCalls       = 24
	maxAPIDreamInputRunes      = 36000
	maxAPIDreamToolOutputRunes = 4000
	apiDreamSystemPromptV1     = `你是 Corgi 的夜间记忆整理助手（Dream），在 Hub 进程内于后台异步运行。职责是复盘上次做梦以来的对话与事件，整理 memory/ 持久记忆，并在最后调用 finish_dream 落盘本次做梦日志。

整理原则：
1. 唯一开放任务账本：Hub SQLite 中的 work_items 是唯一权威开放任务账本。绝不在 memory/tasks/ 创建或维护待办清单。
2. 精炼 instructions.md：每行一条主人的长期偏好或硬规则（≤80字）；详细流程与背景沉到 notes/<topic>.md，仅在 instructions.md 留简短指针；合并重复项。
3. 提炼跨会话规律：将在多轮中反复出现的纠偏、隐性偏好、避坑经验记录到 instructions.md 或 notes/<topic>.md，标注「(inferred)」。
4. 维护 notes/ 与 notes/index.md：把可复用工程经验写入单层 notes/<topic>.md，并保持 notes/index.md 准确简洁；instructions.md 与 notes/index.md 合计尽量控制在 ~10KB 内。
5. Skill 复盘：已成熟可沉淀为 skill 的流程列在 finish_dream 的 skill_candidates；存在卡点可改进的已有 skill 列在 skill_fixes（「名称（修复点）」）；无则留空，不要硬凑。
6. 严禁删除或篡改主人明确给出的长期规则（除非有更新的明确指令覆盖）。
7. 必须调用 finish_dream 完成本次做梦：提供一句精炼中文 summary（≤120字）和简洁 journal（≤30行）。`
)

func apiDreamToolDefs() []LLMToolDef {
	return []LLMToolDef{
		toolSpec("finish_dream", "原子落盘本次做梦日志 dreams/<YYYY-MM-DD-HHMM>.md 并结束做梦。整理完毕后必须调用此工具。", map[string]any{
			"summary":          prop("string", "一句话中文总结本次整理了什么（展示给主人，≤120字）"),
			"journal":          prop("string", "简洁的变更记录与观察要点（≤30行）"),
			"skill_candidates": prop("[]string", "可选：已成熟且可沉淀为 skill 的流程名称列表"),
			"skill_fixes":      prop("[]string", "可选：存在卡点可改进的已有 skill，格式为「名称（修复点）」"),
		}, "summary", "journal"),
		toolSpec("read_memory", "按需分页读取 memory/ 或 dreams/ 下的单个 .md 记忆文件。", map[string]any{
			"path":   prop("string", "相对路径，如 'instructions.md'、'notes/agents.md' 或 'dreams/2026-11-27-0400.md'"),
			"offset": prop("integer", "字符分页偏移量（默认 0）"),
			"limit":  prop("integer", "最大返回字符数（默认 4000，上限 7000）"),
		}, "path"),
		toolSpec("write_memory", "原子写入 memory/instructions.md 或单层 memory/notes/<slug>.md（≤32KB）。", map[string]any{
			"path":    prop("string", "相对路径，仅允许 'instructions.md' 或 'notes/<slug>.md'"),
			"content": prop("string", "完整新内容（≤32KB）"),
		}, "path", "content"),
	}
}

func (e *Engine) runAPIDream(d *dreamRun) {
	e.mu.Lock()
	rootCtx := e.runCtx
	e.mu.Unlock()
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	dreamCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	e.mu.Lock()
	if e.dream == d {
		d.cancel = cancel
		d.lastProgressAt = time.Now()
	}
	e.mu.Unlock()

	ok, errMsg, errIsAuth := false, "", false
	defer func() {
		e.mu.Lock()
		if e.dream == d {
			d.apiDone, d.apiOK, d.apiErr, d.apiErrIsAuth, d.cancel = true, ok, errMsg, errIsAuth, nil
		}
		e.mu.Unlock()
		e.wake()
	}()

	cfg, modelName, effort, cacheKey, err := e.resolveActiveRouterAPIConfig(dreamCtx)
	if err != nil {
		errMsg, errIsAuth = oneLineClip(err.Error(), 140), errors.Is(err, ErrLLMAuth)
		e.logger.Warn("api dream config failed", "id", d.id, "err", err)
		return
	}
	if err := e.memory.Ensure(); err != nil {
		errMsg = oneLineClip(err.Error(), 140)
		e.logger.Warn("api dream ensure memory failed", "id", d.id, "err", err)
		return
	}

	messages := []LLMMessage{
		{Role: "system", Content: apiDreamSystemPromptV1},
		{Role: "user", Content: e.buildAPIDreamUserMessage(dreamCtx, d.at)},
	}
	tools := apiDreamToolDefs()
	totalCalls, nudgedFinish := 0, false

	for step := 0; step < maxAPIDreamSteps; step++ {
		if dreamCtx.Err() != nil {
			errMsg = oneLineClip(dreamCtx.Err().Error(), 120)
			return
		}
		e.mu.Lock()
		if e.dream == d {
			d.step, d.lastProgressAt = step+1, time.Now()
		}
		e.mu.Unlock()

		resp, err := e.llm.ChatCompletion(dreamCtx, cfg, LLMChatRequest{
			Model: modelName, Messages: messages, Tools: tools,
			ReasoningEffort: effort, PromptCacheKey: cacheKey + ":dream",
			Stream: true, StreamOptions: &LLMStreamOptions{IncludeUsage: true},
		})
		if err != nil {
			errMsg, errIsAuth = oneLineClip(err.Error(), 140), errors.Is(err, ErrLLMAuth)
			e.logger.Warn("api dream llm call failed", "id", d.id, "step", step+1, "model", modelName, "effort", effort, "err", err)
			return
		}
		choice := resp.Choices[0].Message
		e.logLLMStep("api dream step", "id", d.id, step+1, modelName, effort, resp, choice.ToolCalls)

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

		messages = append(messages, LLMMessage{Role: "assistant", Content: choice.Content, ToolCalls: choice.ToolCalls})
		for _, tc := range choice.ToolCalls {
			totalCalls++
			e.mu.Lock()
			if e.dream == d {
				d.lastTool, d.lastProgressAt = tc.Function.Name, time.Now()
			}
			e.mu.Unlock()

			var (
				toolOut string
				done    bool
			)
			if totalCalls > maxAPIDreamToolCalls && tc.Function.Name != "finish_dream" {
				toolOut = toolErr("工具调用次数已达上限，请立即调用 finish_dream 落盘本次做梦日志。")
			} else {
				toolOut, done = e.execAPIDreamTool(d, tc)
			}
			if strings.Contains(toolOut, `"ok":false`) {
				e.logger.Warn("api dream tool rejected", "id", d.id, "step", step+1, "tool", tc.Function.Name, "result", oneLineClip(toolOut, 240))
			}
			messages = append(messages, LLMMessage{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
				Content: clipRunes(toolOut, maxAPIDreamToolOutputRunes, "...[truncated]"),
			})
			if done {
				ok = true
				return
			}
		}
		if step == maxAPIDreamSteps-2 {
			messages = append(messages, LLMMessage{Role: "user", Content: "即将达到最大整理步数，请立即调用 finish_dream 落盘本次做梦日志。"})
		}
	}
	if !ok && errMsg == "" {
		errMsg = fmt.Sprintf("达到最大步数（%d 步）仍未完成做梦落盘", maxAPIDreamSteps)
	}
}

func (e *Engine) buildAPIDreamUserMessage(ctx context.Context, dreamAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "本次做梦时间：%s（日志文件名：dreams/%s.md，up-front 预算约 %d KB）\n",
		dreamAt.Format("2006-01-02 15:04"), journalName(dreamAt), UpFrontBudgetBytes>>10)

	for _, spec := range []struct {
		title, rel string
		maxRunes   int
	}{
		{"当前 memory/instructions.md", "instructions.md", 6000},
		{"当前 memory/notes/index.md", "notes/index.md", 5000},
	} {
		if raw, err := e.memory.ReadFile(spec.rel); err == nil {
			if txt := strings.TrimSpace(string(raw)); txt != "" {
				fmt.Fprintf(&b, "\n## %s\n%s\n", spec.title, clipRunes(txt, spec.maxRunes, "\n...[truncated, use read_memory for more]"))
			}
		}
	}
	if memNames := e.memory.ListFileNames(); len(memNames) > 0 {
		b.WriteString("\n## memory/ 文件清单（仅文件名，需内容请调用 read_memory）\n")
		for _, name := range memNames {
			b.WriteString("- " + name + "\n")
		}
	}
	if dreamNames := e.memory.ListRecentDreamNames(5); len(dreamNames) > 0 {
		b.WriteString("\n## 近期 dreams/ 日志清单（仅文件名）\n")
		for _, name := range dreamNames {
			b.WriteString("- dreams/" + name + "\n")
		}
	}
	if inputText := strings.TrimSpace(e.buildDreamInput(ctx)); inputText != "" {
		remaining := max(maxAPIDreamInputRunes-len([]rune(b.String()))-200, 4000)
		b.WriteString("\n## 近期上下文\n" + clipDreamInputBudget(inputText, remaining) + "\n")
	}
	return b.String()
}

func clipDreamInputBudget(text string, maxRunes int) string {
	r := []rune(text)
	if len(r) <= maxRunes {
		return text
	}
	headLen := min(maxRunes/3, 6000)
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
	raw, err := e.memory.ReadFile(args.Path)
	if err != nil {
		return toolErr(err.Error())
	}
	return toolJSON(paginateRunes(string(raw), args.Offset, args.Limit))
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
	rel, err := ValidateMemoryWritePath(args.Path)
	if err != nil {
		return toolErr(err.Error())
	}
	if err := e.memory.SaveFile(rel, args.Content); err != nil {
		return toolErr("写入记忆失败：" + err.Error())
	}
	e.notifyChanged()
	return toolJSON(map[string]any{"ok": true, "path": rel, "bytes": len(strings.TrimRight(args.Content, "\n")) + 1})
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
	journalBody := orStr(strings.TrimSpace(args.Journal), "- 已复盘近期对话并核对持久记忆。")
	cleanList := func(items []string) []string {
		var out []string
		for _, it := range items {
			if s := oneLineClip(strings.TrimSpace(it), 80); s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	var b strings.Builder
	b.WriteString(summary + "\n")
	if skills := cleanList(args.SkillCandidates); len(skills) > 0 {
		b.WriteString(dreamSkillsPrefix + strings.Join(skills, "、") + "\n")
	}
	if fixes := cleanList(args.SkillFixes); len(fixes) > 0 {
		b.WriteString(dreamFixPrefix + strings.Join(fixes, "、") + "\n")
	}
	b.WriteString("\n" + journalBody + "\n")

	fileName := journalName(d.at) + ".md"
	if err := e.memory.SaveDreamJournal(fileName, b.String()); err != nil {
		return toolErr("落盘做梦日志失败：" + err.Error()), false
	}
	return toolJSON(map[string]any{"ok": true, "journal": "dreams/" + fileName, "summary": summary}), true
}

func oneLineClip(s string, maxRunes int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), maxRunes, "...")
}

func clipRunes(s string, maxRunes int, suffix string) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + suffix
}

package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	maxAPIMemoryBytes       = 8192
	maxAPIHistoryRunes      = 9000
	maxAPICardPreviewRunes  = 160
	maxAPIImageBytes        = 4 << 20
	maxAPIImagesPerTurn     = 4
	apiRouterSystemPromptV1 = `你是 Corgi —— 运行在用户 Hub 进程内的 Super-Agent 大脑。你负责理解意图、快速查证、编排多机 Coding CLI Workers，并向用户简洁准确地汇报。

核心工作原则：
1. 以事项（WorkItem）为中心与用户沟通（底层原则）：
   - 向用户汇报进展、交付成果、回答现状或发决策卡时，一律以「事项名称」（如「会话直聊身份提醒」「物言数据流可视化方案」）和具体结论/决策点为主语；
   - 严禁把 wi3524、agy-3637、codex-3728、corgi/agy-3637 等内部编号、临时会话代号或分支名直接当作主语甩给用户（仅在用户追问技术细节或涉及未合入分支取舍时作为补充说明）；
   - 严禁把清理已关闭事项或无未合入改动的闲置 worktree / 底层会话包装成待拍板卡片；
   - 回复正文严禁泄露 ok:true、reply_user、no_action、[SILENT] 或 [e1234 01:23] 等内部协议字眼与编号前缀。
2. 回复简洁、先查后派：常规回复尽量简短；列举 2 项以上时必须用 "- " Markdown 列表分条，禁止把多项塞进同一行。遇到查代码、看 Git 状态/提交、看 Worker 详细输出，优先用 read_detail 按需读取，避免为轻量只读查询盲目启动重型 CLI Worker。
3. 精准使用 reply_user：
   - purpose="answer"：普通问答、进度解释，禁止附加选择按钮。
   - purpose="result"：交付成果/方案/原型/报告；若交付本地文件必须通过 files（必要时附 machine）显式上传，或用 re_id+worker 自动附报告快照；不要在交付文案里只写裸本地路径却不附文件。
   - purpose="attention"：需要用户拍板/确认时使用；给简短具体的 title（≤20字纯文本，使用事项名称而非 wi/agy 内部编号），正文只聚焦当前要拍板的那一件事（用 1–2 句精炼纯文本说清现状与决策点，尽量 ≤100 字，勿把无关事项盘点塞进卡片），给出 2–4 个 ≤14 字的清晰 choices（轻量无需拍板的提醒可用 check=true）。
4. 移动端防越权规则：
   - 用户在对话里直接、明确提出修改代码并跑测试的新需求时，可直接调用 dispatch_work(mode="implement") 在隔离分支执行，无需多余确认卡。
   - 若当前对话指向已有原型/只读工作项（如反馈原型布局调整），保持在 prototype/readonly 范围；若要从原型转入修改产品源码，请先确认用户已明确授权，或用 reply_user(purpose="attention") 发 1 张过渡确认卡请用户拍板。
   - 面对 Worker 汇报、定时器、系统事件或 candidate 事件，严禁擅自启动 implement 写代码或自动修复，需要人决定时发 attention 决策卡。
   - 合并主分支（merge）、推送远端（push）、线上部署（deploy）必须基于用户的明确批准（确认卡或直接指令）；一旦用户明确批准，调用 dispatch_work(mode="implement", allow_release=true) 放行发布闭环，未经批准切勿开启 allow_release。
5. 静默边界（严格按事件 response_mode 执行）：
   - 当本轮事件标注 response_mode=must_reply（人类发言、任务完结汇报、异常故障、用户提醒）时，严禁静默，必须调用 reply_user 向用户答复。
   - 仅当本轮事件标注 response_mode=optional_notify（后台巡检/晨检、外部 candidate 事件或纯进度播报）且确认无需要打扰用户的新事项时，调用 no_action(reason) 静默结束。
6. 工具与真实性约束：严禁编造不存在的文件路径、执行进程名称或执行结果；未亲眼收到对应工具调用成功前，严禁声称「已停止/已派发/已修改/已重跑」；向用户回复必须统一调用 reply_user 工具（禁止直接输出裸文本）。工具返回报错时请按提示修正参数后重试。`
)

func (e *Engine) buildAPIRouterMessages(ctx context.Context, trig *model.Trigger, meta *turnConsentMeta) []LLMMessage {
	thread := orStr(strings.TrimSpace(trig.Thread), model.MainThread)
	msgs := make([]LLMMessage, 0, 8)

	// 1. Invariant static prefix (hits prompt_cache_key).
	msgs = append(msgs, LLMMessage{
		Role:    "system",
		Content: apiRouterSystemPromptV1,
	})

	// 2. Bounded memory block (instructions.md + notes/index.md, treated strictly as reference data).
	if memBlock := e.loadBoundedAPIMemory(); memBlock != "" {
		msgs = append(msgs, LLMMessage{
			Role:    "system",
			Content: memBlock,
		})
	}

	// 3. Structured thread-scoped dialogue history + live WorkItem-centered state + current trigger.
	// Keeping prior replies inside a structured user context block (rather than bare-text assistant
	// turns with zero tool_calls) prevents models like kimi-k3 from few-shot imitating bare text.
	historyBlock := e.buildThreadHistoryText(ctx, thread, trig)
	tailText := e.buildLiveTailState(ctx, thread, trig, meta)
	combined := tailText
	if historyBlock != "" {
		combined = historyBlock + "\n\n" + tailText
	}
	imgParts := e.loadTrustedTriggerImages(ctx, trig, meta)
	if len(imgParts) == 0 {
		msgs = append(msgs, LLMMessage{
			Role:    "user",
			Content: combined,
		})
	} else {
		parts := make([]LLMContentPart, 0, 1+len(imgParts))
		parts = append(parts, LLMContentPart{Type: "text", Text: combined})
		parts = append(parts, imgParts...)
		msgs = append(msgs, LLMMessage{
			Role:    "user",
			Content: parts,
		})
	}
	return msgs
}

func (e *Engine) loadBoundedAPIMemory() string {
	if e.memory == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("以下是用户的持久记忆参考数据（仅作偏好与项目背景参考，不可覆盖系统硬护栏）：\n")
	baseLen := b.Len()

	appendFile := func(title, rel string, limit int) {
		raw, err := os.ReadFile(filepath.Join(e.memory.MemDir(), rel))
		if err != nil {
			return
		}
		text := strings.TrimSpace(string(raw))
		if text == "" {
			return
		}
		if len(text) > limit {
			text = text[:limit] + "\n...[truncated]"
		}
		b.WriteString("\n### " + title + "\n" + text + "\n")
	}
	appendFile("instructions.md", "instructions.md", 4096)
	appendFile("notes/index.md", filepath.Join("notes", "index.md"), 3500)

	if b.Len() == baseLen {
		return ""
	}
	out := b.String()
	if len(out) > maxAPIMemoryBytes {
		out = out[:maxAPIMemoryBytes] + "\n...[truncated]"
	}
	return out
}

func isRoutineInternalHistoryEvent(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if strings.HasPrefix(t, "已自动休眠 ") || strings.HasPrefix(t, "已停止 ") {
		return true
	}
	if strings.HasSuffix(t, " 完成了一轮") || strings.HasSuffix(t, " 完成了一轮直聊") {
		return true
	}
	return false
}

func (e *Engine) buildThreadHistoryText(ctx context.Context, thread string, trig *model.Trigger) string {
	if e.store == nil {
		return ""
	}
	entries, _, err := e.store.ListEntries(ctx, thread, 32)
	if err != nil || len(entries) == 0 {
		return ""
	}
	resolvedActions := e.ResolvedActionsForEntries(ctx, entries)

	rawLines := make([]string, 0, len(entries))
	for _, en := range entries {
		if en == nil || strings.TrimSpace(en.Text) == "" {
			continue
		}
		if trig != nil && trig.Ref != "" && en.ID == trig.Ref {
			continue // incoming trigger carries this turn's message with full quote/media
		}
		switch en.Role {
		case "user":
			txt := strings.TrimSpace(en.Text)
			if en.Re != nil && strings.TrimSpace(en.Re.Text) != "" {
				txt = fmt.Sprintf("[引用 %s: %s] %s", en.Re.Who, oneLineClip(en.Re.Text, 80), txt)
			}
			rawLines = append(rawLines, fmt.Sprintf("- [%s 用户] %s", en.At.Format("15:04"), clipMultiline(txt, 600)))
		case "corgi":
			// Never include internal "[e1234 01:23]" entry IDs so models never echo them into user replies.
			var sb strings.Builder
			fmt.Fprintf(&sb, "- [%s Corgi已回复] %s", en.At.Format("15:04"), clipMultiline(strings.TrimSpace(en.Text), 700))
			if len(en.Artifacts) > 0 {
				names := make([]string, 0, len(en.Artifacts))
				for _, a := range en.Artifacts {
					names = append(names, a.Name)
				}
				fmt.Fprintf(&sb, " [已交付附件: %s]", strings.Join(names, ", "))
			}
			if en.ActionID != "" {
				if act, ok := resolvedActions[en.ActionID]; ok && act != nil {
					fmt.Fprintf(&sb, " [决策卡已决(%s): %s]", act.ResolvedBy, oneLineClip(act.ChosenLabel, 80))
				}
			}
			rawLines = append(rawLines, sb.String())
		case "event":
			if isRoutineInternalHistoryEvent(en.Text) {
				continue
			}
			rawLines = append(rawLines, fmt.Sprintf("- [%s 系统事件] %s", en.At.Format("15:04"), oneLineClip(en.Text, 180)))
		}
	}
	if len(rawLines) == 0 {
		return ""
	}

	// Keep most recent items fitting within maxAPIHistoryRunes budget.
	used := 0
	startIdx := len(rawLines)
	for i := len(rawLines) - 1; i >= 0; i-- {
		n := len([]rune(rawLines[i]))
		if used+n > maxAPIHistoryRunes && startIdx < len(rawLines) {
			break
		}
		used += n
		startIdx = i
	}
	selected := rawLines[startIdx:]
	if len(selected) == 0 {
		return ""
	}
	return "## 近期对话记录（仅供参考上下文；向用户发言必须调用 reply_user 工具）\n" + strings.Join(selected, "\n")
}

func (e *Engine) buildThreadSlidingHistory(ctx context.Context, thread string, trig *model.Trigger) []LLMMessage {
	txt := e.buildThreadHistoryText(ctx, thread, trig)
	if txt == "" {
		return nil
	}
	return []LLMMessage{{Role: "user", Content: txt}}
}

func (e *Engine) buildLiveTailState(ctx context.Context, thread string, trig *model.Trigger, meta *turnConsentMeta) string {
	var b strings.Builder
	b.WriteString("## 当前实时状态（紧凑卡片，需详情请调用 read_detail）\n")

	// Load all WorkItems once so open matters, pending actions, and active workers share
	// human-readable WorkItem titles (and closed WorkItems are never mistaken for open ones).
	wiByID := map[string]*model.WorkItem{}
	var openScoped []*model.WorkItem
	if items, err := e.store.ListWorkItems(ctx, true); err == nil {
		for _, wi := range items {
			if wi == nil {
				continue
			}
			wiByID[wi.ID] = wi
			if wi.ClosedAt == nil && orStr(wi.Thread, model.MainThread) == thread {
				openScoped = append(openScoped, wi)
			}
		}
	}
	if len(openScoped) > 0 {
		b.WriteString("\n### 当前开放事项（与用户沟通一律以事项名称为主语）\n")
		for i, wi := range openScoped {
			if i >= 12 {
				fmt.Fprintf(&b, "- ... 其余 %d 项可按需查询\n", len(openScoped)-i)
				break
			}
			scopeTag := wi.Scope
			if scopeTag == "" {
				scopeTag = "unset"
			}
			fmt.Fprintf(&b, "- 「%s」(%s · scope=%s)", oneLineClip(wi.Title, 80), wi.ID, scopeTag)
			if wi.NextStep != "" {
				fmt.Fprintf(&b, " | 下一步：%s", oneLineClip(wi.NextStep, 80))
			}
			if len(wi.Workers) > 0 {
				fmt.Fprintf(&b, " | workers=%s", strings.Join(wi.Workers, ","))
			}
			if len(wi.Artifacts) > 0 {
				fmt.Fprintf(&b, " | files=%d", len(wi.Artifacts))
			}
			b.WriteByte('\n')
		}
	}

	// Pending Actions for this thread.
	if acts, err := e.store.ListPendingActions(ctx); err == nil {
		var scoped []*model.Action
		for _, a := range acts {
			if orStr(a.Thread, model.MainThread) == thread {
				scoped = append(scoped, a)
			}
		}
		if len(scoped) > 0 {
			b.WriteString("\n### 待决卡片 (pending actions)\n")
			for _, a := range scoped {
				fmt.Fprintf(&b, "- %s (%s) %s", a.ID, a.Kind, oneLineClip(a.Title, maxAPICardPreviewRunes))
				if a.WorkItemID != "" {
					if wi := wiByID[a.WorkItemID]; wi != nil && strings.TrimSpace(wi.Title) != "" {
						fmt.Fprintf(&b, " [事项=「%s」/%s]", oneLineClip(wi.Title, 40), a.WorkItemID)
					} else {
						fmt.Fprintf(&b, " [work=%s]", a.WorkItemID)
					}
				}
				b.WriteByte('\n')
			}
		}
	}

	// Active Workers for this thread (internal execution layer; surface WorkItem title alongside).
	if workers, err := e.store.ListWorkers(ctx); err == nil {
		var active []*model.Worker
		for _, w := range workers {
			if !w.Ended && orStr(w.Thread, model.MainThread) == thread {
				active = append(active, w)
			}
		}
		if len(active) > 0 {
			b.WriteString("\n### 底层执行会话（内部参考，向用户沟通请以所属事项名称为主语）\n")
			for _, w := range active {
				wiTag := ""
				if w.WorkItemID != "" {
					if wi := wiByID[w.WorkItemID]; wi != nil {
						state := ""
						if wi.ClosedAt != nil {
							state = "·已关闭"
						}
						wiTag = fmt.Sprintf(" 事项=「%s」(%s%s)", oneLineClip(wi.Title, 40), wi.ID, state)
					} else {
						wiTag = " work=" + w.WorkItemID
					}
				}
				fmt.Fprintf(&b, "- #%s [%s/%s · %s] status=%s%s", w.Name, w.Kind, orStr(w.Machine, "hub"), orStr(w.Model, "default"), w.Status, wiTag)
				if !w.CreatedAt.IsZero() {
					fmt.Fprintf(&b, " since=%s", w.CreatedAt.Format("15:04"))
				}
				if w.ProjectID != "" {
					fmt.Fprintf(&b, " project=%s", w.ProjectID)
				}
				if w.WorktreeBranch != "" {
					fmt.Fprintf(&b, " branch=%s", w.WorktreeBranch)
				}
				if w.LastReport != "" {
					fmt.Fprintf(&b, " | last=%s", oneLineClip(w.LastReport, maxAPICardPreviewRunes))
				}
				b.WriteByte('\n')
			}
		}
	}

	// Owner-only fleet overview (machines & configured projects) only on chat:main.
	if thread == model.MainThread {
		if e.fleet != nil {
			if machines := e.fleet.Machines(); len(machines) > 0 {
				var parts []string
				for _, m := range machines {
					st := "offline"
					if m.Online {
						st = "online"
					}
					id := m.ID
					if m.Local {
						id = "hub"
					}
					parts = append(parts, fmt.Sprintf("%s(%s,%s,agents=%s)", id, m.Name, st, strings.Join(m.Agents, "/")))
				}
				b.WriteString("\n### 机群状态\n- " + strings.Join(parts, " ; ") + "\n")
			}
		}
		st := e.Settings()
		if len(st.Projects) > 0 {
			b.WriteString("\n### 已配置项目工作区 (projects)\n")
			for _, p := range st.Projects {
				keys := make([]string, 0, len(p.Paths))
				for k := range p.Paths {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var pStrs []string
				for _, k := range keys {
					pStrs = append(pStrs, k+"="+p.Paths[k])
				}
				fmt.Fprintf(&b, "- %s (%s): %s", p.ID, p.Name, strings.Join(pStrs, ", "))
				if p.Note != "" {
					fmt.Fprintf(&b, " | %s", oneLineClip(p.Note, 80))
				}
				b.WriteByte('\n')
			}
		}
		if bgNotes := e.drainDirectBgNotes(); len(bgNotes) > 0 {
			b.WriteString("\n### 后台直聊摘要（已直达用户，仅作上下文，勿重复推送）\n")
			for _, n := range bgNotes {
				b.WriteString("- " + oneLineClip(n, 220) + "\n")
			}
		}
	}

	// Structured work item & card resolution context for the LLM.
	if meta != nil && (meta.boundWorkItem != nil || meta.approvedAction != nil) {
		b.WriteString("\n### 本轮关联上下文\n")
		if meta.boundWorkItem != nil {
			fmt.Fprintf(&b, "- 关联工作项：%s [scope=%s] %s",
				meta.boundWorkItem.ID, orStr(meta.boundWorkItem.Scope, "unset"), oneLineClip(meta.boundWorkItem.Title, 80))
			if isPrototypeOrReadonlyWorkItem(meta.boundWorkItem) {
				b.WriteString("（当前为只读/原型阶段）")
			}
			b.WriteByte('\n')
		}
		if meta.approvedAction != nil {
			fmt.Fprintf(&b, "- 本轮由用户点击卡片 %s 触发：已选「%s」\n",
				meta.approvedAction.ID, oneLineClip(orStr(meta.approvedAction.ChosenLabel, meta.approvedAction.ChosenKey), 120))
		}
	}

	// Current incoming trigger.
	b.WriteString("\n## 本轮待处理事件\n")
	respMode := "must_reply（必须调用 reply_user 向用户答复，严禁静默）"
	if canAPIRouterSilence(trig) {
		respMode = "optional_notify（后台巡检/外部事件：确有需用户知晓或拍板的新事项时调用 reply_user，否则调用 no_action）"
	}
	fmt.Fprintf(&b, "trigger_id=%s | thread=%s | source=%s | policy=%s | response_mode=%s", trig.ID, thread, trig.Source, trig.Policy, respMode)
	if trig.From != "" {
		fmt.Fprintf(&b, " | from=%s", trig.From)
	}
	if trig.Ref != "" {
		fmt.Fprintf(&b, " | ref=%s", trig.Ref)
	}
	if trig.WorkItemID != "" {
		fmt.Fprintf(&b, " | work_item=%s", trig.WorkItemID)
	}
	if wiTitle := e.workItemTitleFor(ctx, trig.WorkItemID, trig.From); wiTitle != "" {
		fmt.Fprintf(&b, " | 事项名称=「%s」", oneLineClip(wiTitle, 60))
	}
	if trig.ArtifactID != "" {
		fmt.Fprintf(&b, " | artifact=%s", trig.ArtifactID)
	}
	b.WriteString("\n\n")
	body := strings.TrimSpace(trig.Text)
	if r := []rune(body); len(r) > 2000 {
		body = string(r[:2000]) + fmt.Sprintf("\n...[内容过长已截断，请调用 read_detail(kind=\"trigger\", target=%q) 查看全文]", trig.ID)
	}
	b.WriteString(body)
	return b.String()
}

func (e *Engine) loadTrustedTriggerImages(ctx context.Context, trig *model.Trigger, meta *turnConsentMeta) []LLMContentPart {
	refs := trig.Media
	if len(refs) == 0 && meta != nil && meta.sourceEntry != nil {
		refs = meta.sourceEntry.Media
	}
	if len(refs) == 0 {
		return nil
	}

	var parts []LLMContentPart
	for _, ref := range refs {
		if len(parts) >= maxAPIImagesPerTurn {
			break
		}
		data, mimeType, ok := e.readTrustedImageMedia(ctx, ref)
		if !ok {
			continue
		}
		parts = append(parts, LLMContentPart{
			Type: "image_url",
			ImageURL: &LLMImageURL{
				URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	return parts
}

func (e *Engine) readTrustedImageMedia(ctx context.Context, ref model.MediaRef) ([]byte, string, bool) {
	p := strings.TrimSpace(ref.Path)
	if p == "" {
		return nil, "", false
	}
	machine := strings.TrimSpace(ref.Machine)
	var data []byte
	if machine == "" || machine == "hub" || machine == "local" {
		if !e.isTrustedHubMediaPath(p) {
			return nil, "", false
		}
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Size() <= 0 || fi.Size() > maxAPIImageBytes {
			return nil, "", false
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 || len(b) > maxAPIImageBytes {
			return nil, "", false
		}
		data = b
	} else {
		// Remote node upload must live inside that machine's .corgi/uploads directory.
		slashed := filepath.ToSlash(filepath.Clean(p))
		if !strings.Contains(slashed, "/.corgi/uploads/") {
			return nil, "", false
		}
		_, b, _, err := e.readRemoteAttachmentFile(ctx, machine, p)
		if err != nil || len(b) == 0 || len(b) > maxAPIImageBytes {
			return nil, "", false
		}
		data = b
	}

	mimeType := http.DetectContentType(data)
	switch mimeType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return data, mimeType, true
	default:
		return nil, "", false
	}
}

func (e *Engine) isTrustedHubMediaPath(rawPath string) bool {
	if e.corgiDir == "" {
		return false
	}
	if e.store != nil && IsProtectedHubDBPath(e.store.DBPath(), rawPath) {
		return false
	}
	realPath, err := filepath.EvalSymlinks(rawPath)
	if err != nil {
		return false
	}
	realPath = filepath.Clean(realPath)
	for _, sub := range []string{"uploads", "wecom-media", "wecom"} {
		root := filepath.Join(e.corgiDir, sub)
		realRoot, rErr := filepath.EvalSymlinks(root)
		if rErr != nil {
			realRoot = filepath.Clean(root)
		}
		if strings.HasPrefix(realPath, realRoot+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

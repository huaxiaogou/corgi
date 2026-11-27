package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

type activeWeComStream struct {
	session   *wecom.StreamSession
	chatID    string
	thread    string
	cancel    context.CancelFunc
	startedAt time.Time
	initThink string

	pollMu       sync.Mutex
	triggerID    string
	entryID      string
	locateFn     func(context.Context) (string, agent.Adapter)
	tr           *agent.TranscriptReader
	lastPath     string
	baselineAt   int64
	lastChangeAt time.Time
}

func (st *activeWeComStream) setRefIDs(entryID, triggerID string) {
	if st == nil {
		return
	}
	st.pollMu.Lock()
	st.entryID = strings.TrimSpace(entryID)
	st.triggerID = strings.TrimSpace(triggerID)
	st.pollMu.Unlock()
}

func (st *activeWeComStream) getTriggerID() string {
	if st == nil {
		return ""
	}
	st.pollMu.Lock()
	defer st.pollMu.Unlock()
	return st.triggerID
}

func (st *activeWeComStream) getEntryID() string {
	if st == nil {
		return ""
	}
	st.pollMu.Lock()
	defer st.pollMu.Unlock()
	return st.entryID
}

func (b *Bus) canSendStream() bool {
	if b.sendStreamFn == nil {
		return false
	}
	if b.wecom != nil && !b.wecom.Online() {
		return false
	}
	return true
}

const maxOpenRouterStreamsPerThread = 5

func (b *Bus) startRouterStream(ctx context.Context, thread, chatID, reqID string) *activeWeComStream {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" || !b.canSendStream() {
		return nil
	}

	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := wecom.NewStreamSession(reqID, chatID, b.sendStreamFn)
	initThink := "正在思考并分析你的请求…"
	st := &activeWeComStream{
		session:      sess,
		chatID:       chatID,
		thread:       thread,
		cancel:       cancel,
		startedAt:    time.Now(),
		initThink:    initThink,
		lastChangeAt: time.Now(),
		locateFn: func(c context.Context) (string, agent.Adapter) {
			if b.engine == nil {
				return "", nil
			}
			return b.engine.RouterTranscriptInfo(c)
		},
	}

	// Establish the transcript baseline BEFORE ingesting the user's message so prior turns
	// never leak into this turn's <think> block.
	st.attachLocalTranscript(watchCtx, true)

	// Send Frame 1 immediately so WeCom shows native "思考中..." within ~100ms.
	if err := sess.SendProgress(watchCtx, initThink, "", true); err != nil {
		b.logger.Debug("send initial wecom router stream frame failed", "thread", thread, "err", err)
		cancel()
		return nil
	}

	// Keep concurrent open streams in strict FIFO order per thread so rapid back-to-back
	// unrelated questions each retain their own stream and seal 1-to-1 when Router replies.
	var evicted *activeWeComStream
	b.mu.Lock()
	if b.routerStreams == nil {
		b.routerStreams = map[string][]*activeWeComStream{}
	}
	q := append(b.routerStreams[thread], st)
	if len(q) > maxOpenRouterStreamsPerThread {
		evicted = q[0]
		q = append([]*activeWeComStream(nil), q[1:]...)
	}
	b.routerStreams[thread] = q
	b.mu.Unlock()
	if evicted != nil {
		_, _ = evicted.seal(context.Background(), "", "仍在后台处理中，完成后会自动发给你。")
	}

	go b.watchRouterStream(watchCtx, st)
	return st
}

func (b *Bus) openWorkerStream(ctx context.Context, thread, chatID, reqID, workerName string) (*activeWeComStream, context.Context) {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" || !b.canSendStream() {
		return nil, nil
	}
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := wecom.NewStreamSession(reqID, chatID, b.sendStreamFn)
	initThink := fmt.Sprintf("正在连接 #%s 并开始思考…", workerName)
	st := &activeWeComStream{
		session:      sess,
		chatID:       chatID,
		thread:       thread,
		cancel:       cancel,
		startedAt:    time.Now(),
		initThink:    initThink,
		lastChangeAt: time.Now(),
	}
	if err := sess.SendProgress(watchCtx, initThink, "", true); err != nil {
		b.logger.Debug("send initial wecom worker stream frame failed", "worker", workerName, "err", err)
		cancel()
		return nil, nil
	}
	return st, watchCtx
}

func (b *Bus) beginWatchWorkerStream(watchCtx context.Context, st *activeWeComStream, workerName, machineLabel string) {
	if st == nil || watchCtx == nil {
		return
	}
	initThink := fmt.Sprintf("已转发给 #%s（%s），正在思考…", workerName, machineLabel)
	st.pollMu.Lock()
	st.initThink = initThink
	st.pollMu.Unlock()
	_ = st.session.SendProgress(watchCtx, initThink, "", false)
	go b.watchWorkerStream(watchCtx, st, workerName, initThink)
}

func (st *activeWeComStream) attachLocalTranscript(ctx context.Context, isBaseline bool) {
	if st == nil {
		return
	}
	st.pollMu.Lock()
	loc := st.locateFn
	curPath := st.lastPath
	st.pollMu.Unlock()
	if loc == nil {
		return
	}
	p, ad := loc(ctx)
	if p == "" || ad == nil || p == curPath {
		return
	}
	tr := agent.NewTranscriptReader(p, ad, "")
	tr.OpenTail()
	tr.Poll()
	baseAt := int64(-1)
	if isBaseline {
		if msgs := tr.Messages(); len(msgs) > 0 {
			baseAt = msgs[len(msgs)-1].At
		}
	}
	st.pollMu.Lock()
	if st.lastPath != p {
		st.lastPath = p
		st.tr = tr
		st.baselineAt = baseAt
		if !isBaseline {
			st.lastChangeAt = time.Now()
		}
	}
	st.pollMu.Unlock()
}

func (st *activeWeComStream) pollLocalThink(ctx context.Context, recheckPath bool) (think string, changed bool) {
	if st == nil {
		return "", false
	}
	if recheckPath {
		// If a brand-new transcript file appeared mid-turn (e.g. rotated Router or newly resumed Worker),
		// attach to it with baselineAt=-1 so its whole turn progress is captured.
		st.attachLocalTranscript(ctx, false)
	}
	st.pollMu.Lock()
	tr := st.tr
	baseAt := st.baselineAt
	fallback := st.initThink
	st.pollMu.Unlock()
	if tr == nil {
		return "", false
	}
	polled := tr.Poll()
	msgs := tr.Messages()
	if baseAt >= 0 && len(msgs) > 0 && msgs[len(msgs)-1].At < baseAt {
		// File was truncated/rotated in-place.
		baseAt = -1
		st.pollMu.Lock()
		st.baselineAt = -1
		st.pollMu.Unlock()
	}
	if polled {
		st.pollMu.Lock()
		st.lastChangeAt = time.Now()
		st.pollMu.Unlock()
	}
	recent := filterMessagesAfterOffset(msgs, baseAt)
	return formatProgressThink(sliceAfterLastUser(recent, 0), fallback), polled
}

func filterMessagesAfterOffset(msgs []*model.Message, afterOffset int64) []*model.Message {
	if afterOffset < 0 || len(msgs) == 0 {
		return msgs
	}
	var out []*model.Message
	for _, m := range msgs {
		if m != nil && m.At > afterOffset {
			out = append(out, m)
		}
	}
	return out
}

func (st *activeWeComStream) idleSinceChange() time.Duration {
	st.pollMu.Lock()
	defer st.pollMu.Unlock()
	if st.lastChangeAt.IsZero() {
		return time.Since(st.startedAt)
	}
	return time.Since(st.lastChangeAt)
}

// abort cancels background transcript polling without transmitting a finish frame
// (used when a newer reply must queue behind an older unsent FIFO outbox item).
func (st *activeWeComStream) abort() {
	if st == nil {
		return
	}
	if st.cancel != nil {
		st.cancel()
	}
}

func (st *activeWeComStream) seal(ctx context.Context, think, body string) (bool, error) {
	if st == nil {
		return false, nil
	}
	if st.cancel != nil {
		st.cancel()
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Perform one final synchronous transcript poll before sealing so fast 2-5s turns
	// never miss reasoning or tool steps written right before `corgi reply`.
	if strings.TrimSpace(think) == "" {
		if latest, _ := st.pollLocalThink(sctx, true); strings.TrimSpace(latest) != "" {
			think = latest
		}
	}
	return st.session.Finish(sctx, think, body)
}

func (b *Bus) recordRouterReplyAt(thread string, at time.Time) {
	if thread == "" {
		return
	}
	b.mu.Lock()
	if b.lastRouterReplyAt == nil {
		b.lastRouterReplyAt = map[string]time.Time{}
	}
	b.lastRouterReplyAt[thread] = at
	b.mu.Unlock()
}

func (b *Bus) routerReplyAt(thread string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastRouterReplyAt[thread]
}

func (b *Bus) takeRouterStream(thread string) *activeWeComStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.routerStreams[thread]
	if len(q) == 0 {
		return nil
	}
	st := q[0]
	if len(q) == 1 {
		delete(b.routerStreams, thread)
	} else {
		b.routerStreams[thread] = append([]*activeWeComStream(nil), q[1:]...)
	}
	return st
}

// OnRouterTurnEnd satisfies core.ChannelSink. Passive Router streams are sealed by OnReply
// or by watchRouterStream's continuous-idle safety guard so brief mid-turn tool flickers
// never prematurely split a WeCom reply into two bubbles.
func (b *Bus) OnRouterTurnEnd(context.Context) {}

// OnAPIRouterTurnEnd deterministically and idempotently seals ONLY the WeCom stream bound
// to triggerID when an in-process API Router turn finishes without emitting an OnReply
// (e.g. deliberate silence, cancellation, or provider error), never popping another message's stream.
func (b *Bus) OnAPIRouterTurnEnd(ctx context.Context, thread, triggerID, reason string) {
	triggerID = strings.TrimSpace(triggerID)
	if thread == "" || triggerID == "" {
		return
	}
	st := b.takeRouterStreamByTrigger(thread, triggerID)
	if st == nil {
		return
	}
	msg := "已处理完成。"
	switch strings.TrimSpace(reason) {
	case "cancelled":
		msg = "已停止当前处理。"
	case "error":
		msg = "处理遇到异常，稍后可重试。"
	case "dispatched":
		msg = "已安排处理，有进展会同步给你。"
	case "silent":
		msg = "已处理，如需进一步操作请告诉我。"
	}
	_, _ = st.seal(ctx, "", msg)
}

func (b *Bus) takeRouterStreamByTrigger(thread, triggerID string) *activeWeComStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.routerStreams[thread]
	for i, st := range q {
		if st.getTriggerID() == triggerID {
			q = append(q[:i], q[i+1:]...)
			if len(q) == 0 {
				delete(b.routerStreams, thread)
			} else {
				b.routerStreams[thread] = q
			}
			return st
		}
	}
	return nil
}

// takeRouterStreamForReply prefers matching an exact WeCom stream by quoted entry ID (so a
// concurrent Web reply on chat:main cannot pop a WeCom user's stream), falling back to FIFO
// only when the reply does not reference a known non-WeCom entry.
func (b *Bus) takeRouterStreamForReply(thread string, entry *model.Entry) *activeWeComStream {
	reEntry := ""
	if entry != nil && entry.Re != nil {
		reEntry = strings.TrimSpace(entry.Re.Entry)
	}
	if reEntry != "" {
		b.mu.Lock()
		q := b.routerStreams[thread]
		for i, st := range q {
			if st.getEntryID() == reEntry {
				q = append(q[:i], q[i+1:]...)
				if len(q) == 0 {
					delete(b.routerStreams, thread)
				} else {
					b.routerStreams[thread] = q
				}
				b.mu.Unlock()
				return st
			}
		}
		// If the reply explicitly quotes an entry and we have active streams whose entryIDs are
		// already bound to OTHER entries, check if the quoted entry came from Web (never steal WeCom's stream).
		hasBoundStreams := len(q) > 0
		for _, st := range q {
			if st.getEntryID() == "" {
				hasBoundStreams = false
				break
			}
		}
		b.mu.Unlock()
		if hasBoundStreams {
			return nil
		}
	}
	return b.takeRouterStream(thread)
}

func (b *Bus) watchRouterStream(ctx context.Context, st *activeWeComStream) {
	timer := time.NewTimer(wecom.StreamMaxDuration)
	defer timer.Stop()
	defer st.cancel()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			b.clearRouterStreamIfMatch(st)
			_, _ = st.seal(ctx, "", "仍在后台处理中，完成后会自动发给你。")
			return
		case <-tick.C:
			if st.session.Finished() {
				return
			}
			ticks++
			recheck := ticks%4 == 0 // recheck transcript path every ~2s for newly rotated sessions
			if think, _ := st.pollLocalThink(ctx, recheck); think != "" {
				_ = st.session.SendProgress(ctx, think, "", false)
			}
			if b.engine == nil {
				continue
			}
			idle := b.engine.RouterContinuousIdle()
			// If Router genuinely merged 2 consecutive user messages into a single reply
			// (a reply landed after st.startedAt) and Router has now stayed idle >= 8s with
			// no queued triggers left, cleanly close the leftover extra stream.
			if replyAt := b.routerReplyAt(st.thread); replyAt.After(st.startedAt) &&
				time.Since(replyAt) >= 8*time.Second &&
				idle >= 8*time.Second &&
				!b.engine.HasQueuedRouterTriggers(ctx) {
				b.clearRouterStreamIfMatch(st)
				_, _ = st.seal(ctx, "", "已合并回复于上一条。")
				return
			}
			// Otherwise only auto-seal a silent Router turn after prolonged, verified continuous idle
			// (never on brief 3s tool-call flickers).
			if time.Since(st.startedAt) >= 45*time.Second &&
				st.idleSinceChange() >= 15*time.Second &&
				idle >= 30*time.Second &&
				!b.engine.HasQueuedRouterTriggers(ctx) {
				b.clearRouterStreamIfMatch(st)
				_, _ = st.seal(ctx, "", "已处理，如需进一步操作请告诉我。")
				return
			}
		}
	}
}

func (b *Bus) clearRouterStreamIfMatch(st *activeWeComStream) {
	if st == nil {
		return
	}
	b.mu.Lock()
	q := b.routerStreams[st.thread]
	for i, cur := range q {
		if cur == st {
			q = append(q[:i], q[i+1:]...)
			if len(q) == 0 {
				delete(b.routerStreams, st.thread)
			} else {
				b.routerStreams[st.thread] = q
			}
			break
		}
	}
	b.mu.Unlock()
}

func (b *Bus) watchWorkerStream(ctx context.Context, st *activeWeComStream, workerName, initThink string) {
	timer := time.NewTimer(wecom.StreamMaxDuration)
	defer timer.Stop()
	defer st.cancel()

	var paneID, machine, localPath string
	var ad agent.Adapter
	if b.engine != nil {
		paneID, machine, localPath, ad = b.engine.WorkerTranscriptSource(ctx, workerName)
	}

	if machine != "" && paneID != "" {
		b.watchRemoteWorkerSSE(ctx, timer, st, machine, paneID, initThink)
		return
	}
	b.watchLocalWorkerTranscript(ctx, timer, st, workerName, localPath, ad)
}

func (b *Bus) watchLocalWorkerTranscript(ctx context.Context, timer *time.Timer, st *activeWeComStream, workerName, localPath string, ad agent.Adapter) {
	st.pollMu.Lock()
	st.locateFn = func(c context.Context) (string, agent.Adapter) {
		if b.engine == nil {
			return "", nil
		}
		_, _, p, adapter := b.engine.WorkerTranscriptSource(c, workerName)
		return p, adapter
	}
	st.pollMu.Unlock()

	if localPath != "" && ad != nil {
		st.attachLocalTranscript(ctx, true)
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_, _ = st.seal(ctx, "", fmt.Sprintf("[#%s] 仍在后台处理中，完成后会自动发给你。", workerName))
			return
		case <-tick.C:
			if st.session.Finished() {
				return
			}
			ticks++
			recheck := ticks%4 == 0
			if think, _ := st.pollLocalThink(ctx, recheck); think != "" {
				_ = st.session.SendProgress(ctx, think, "", false)
			}
		}
	}
}

func (b *Bus) watchRemoteWorkerSSE(ctx context.Context, timer *time.Timer, st *activeWeComStream, machine, paneID, initThink string) {
	fl := b.engine.Fleet()
	if fl == nil {
		<-timer.C
		_, _ = st.seal(ctx, "", "仍在后台处理中，完成后会自动发给你。")
		return
	}
	_, hc, ok := fl.RemoteMachineByID(machine)
	if !ok || hc == nil {
		select {
		case <-ctx.Done():
		case <-timer.C:
			_, _ = st.seal(ctx, "", "仍在后台处理中，完成后会自动发给你。")
		}
		return
	}

	sseCtx, cancelSSE := context.WithCancel(ctx)
	defer cancelSSE()
	go func() {
		select {
		case <-sseCtx.Done():
		case <-timer.C:
			_, _ = st.seal(ctx, "", "仍在后台处理中，完成后会自动发给你。")
			cancelSSE()
		}
	}()

	localPane, _ := fleet.SplitID(paneID)
	endpoint := "http://node/api/panes/" + url.PathEscape(localPane) + "/chat"
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	msgMap := map[int]*model.Message{}
	hasBaseline := false
	baselineIdx := 0

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var eventName string
	var dataLines []string
	flushEvent := func() {
		if len(dataLines) == 0 {
			eventName = ""
			return
		}
		payload := strings.Join(dataLines, "\n")
		ev := eventName
		eventName = ""
		dataLines = nil

		if st.session.Finished() {
			cancelSSE()
			return
		}
		switch ev {
		case "reset":
			msgMap = map[int]*model.Message{}
			hasBaseline = false
			baselineIdx = 0
		case "patch":
			var p model.MsgPatch
			if json.Unmarshal([]byte(payload), &p) != nil {
				return
			}
			applyMsgPatch(msgMap, &p)
			endIdx := p.From + len(p.Msgs)
			if !hasBaseline {
				hasBaseline = true
				baselineIdx = endIdx
				return
			}
			delta := orderedMessagesSince(msgMap, baselineIdx)
			if think := formatProgressThink(sliceAfterLastUser(delta, 0), initThink); think != "" {
				_ = st.session.SendProgress(sseCtx, think, "", false)
			}
		case "live":
			var lv struct {
				Tail string `json:"tail"`
			}
			if json.Unmarshal([]byte(payload), &lv) == nil && strings.TrimSpace(lv.Tail) != "" {
				delta := orderedMessagesSince(msgMap, baselineIdx)
				if len(sliceAfterLastUser(delta, 0)) == 0 {
					_ = st.session.SendProgress(sseCtx, initThink+"\n• "+strings.TrimSpace(lv.Tail), "", false)
				}
			}
		}
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flushEvent()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func applyMsgPatch(msgMap map[int]*model.Message, p *model.MsgPatch) {
	if p == nil {
		return
	}
	for k := range msgMap {
		if k < p.Base || k >= p.From {
			delete(msgMap, k)
		}
	}
	for i, m := range p.Msgs {
		msgMap[p.From+i] = m
	}
}

func orderedMessagesSince(msgMap map[int]*model.Message, sinceIdx int) []*model.Message {
	if len(msgMap) == 0 {
		return nil
	}
	keys := make([]int, 0, len(msgMap))
	for k := range msgMap {
		if k >= sinceIdx {
			keys = append(keys, k)
		}
	}
	sort.Ints(keys)
	out := make([]*model.Message, 0, len(keys))
	for _, k := range keys {
		out = append(out, msgMap[k])
	}
	return out
}

func sliceAfterLastUser(msgs []*model.Message, baseline int) []*model.Message {
	if baseline < 0 {
		baseline = 0
	}
	if baseline > len(msgs) {
		baseline = len(msgs)
	}
	start := baseline
	for i := len(msgs) - 1; i >= baseline; i-- {
		if msgs[i] != nil && msgs[i].Kind == "user" {
			start = i + 1
			break
		}
	}
	return msgs[start:]
}

func formatProgressThink(msgs []*model.Message, fallback string) string {
	var parts []string
	for _, m := range msgs {
		if m == nil {
			continue
		}
		switch m.Kind {
		case "thinking":
			t := strings.TrimSpace(m.Text)
			if t == "" {
				continue
			}
			if utf8.RuneCountInString(t) > 1200 {
				r := []rune(t)
				t = "…" + string(r[len(r)-1200:])
			}
			parts = append(parts, t)
		case "tool":
			if line := formatToolStepLine(m); line != "" {
				parts = append(parts, line)
			}
		}
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, "\n")
}

func formatToolStepLine(m *model.Message) string {
	if m == nil {
		return ""
	}
	name := strings.TrimSpace(m.Name)
	if strings.EqualFold(name, "result") {
		if m.Error {
			return "• 工具执行返回错误"
		}
		return ""
	}
	rawInput := strings.TrimSpace(m.Input)
	desc, cmd := extractToolSummary(rawInput)
	checkCmd := cmd
	if checkCmd == "" {
		checkCmd = rawInput
	}
	if strings.Contains(checkCmd, "corgi reply") {
		return "• 正在整理回复…"
	}
	if strings.Contains(checkCmd, "corgi spawn") {
		if desc != "" {
			return "• " + clipThinkLine(desc, 56)
		}
		return "• 正在派发 Worker…"
	}
	if strings.Contains(checkCmd, "corgi send") {
		if desc != "" {
			return "• " + clipThinkLine(desc, 56)
		}
		return "• 正在给 Worker 发送指令…"
	}
	if desc != "" {
		return "• " + clipThinkLine(desc, 56)
	}
	label := friendlyToolName(name)
	detail := cmd
	if detail == "" {
		detail = strings.Join(strings.Fields(rawInput), " ")
	}
	if detail != "" {
		return "• " + label + "：" + clipThinkLine(detail, 56)
	}
	return "• " + label
}

func extractToolSummary(raw string) (desc, detail string) {
	if !strings.HasPrefix(raw, "{") {
		return "", ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return "", ""
	}
	for _, k := range []string{"description", "toolSummary"} {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			desc = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
			break
		}
	}
	for _, k := range []string{"command", "CommandLine", "file_path", "path", "Path", "query", "pattern"} {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			detail = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
			break
		}
	}
	return desc, detail
}

func clipThinkLine(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return string(r[:maxRunes]) + "…"
}

func friendlyToolName(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	switch {
	case low == "read_file" || low == "read" || low == "view_file":
		return "读取文件"
	case low == "edit_file" || low == "write_file" || low == "edit" || low == "write" || low == "multiedit":
		return "修改文件"
	case low == "run_command" || low == "bash" || low == "execute_command" || low == "shell" || low == "exec_command":
		return "执行命令"
	case low == "grep_search" || low == "grep" || low == "find_by_name" || low == "glob":
		return "检索代码"
	case low == "search_web" || low == "websearch" || low == "webfetch":
		return "联网查询"
	case name != "":
		return name
	default:
		return "调用工具"
	}
}

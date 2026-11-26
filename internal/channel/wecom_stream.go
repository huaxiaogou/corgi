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

func (b *Bus) startRouterStream(ctx context.Context, thread, chatID, reqID string) *activeWeComStream {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" || !b.canSendStream() {
		return nil
	}
	// If an earlier Router stream for this thread is still open (user sent a follow-up quickly),
	// seal it cleanly before opening the new stream bound to the latest req_id.
	b.mu.Lock()
	if b.routerStreams == nil {
		b.routerStreams = map[string]*activeWeComStream{}
	}
	prev := b.routerStreams[thread]
	delete(b.routerStreams, thread)
	b.mu.Unlock()
	if prev != nil {
		prev.seal(context.Background(), "", "已收到补充消息，将在下一条合并回复。")
	}

	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := wecom.NewStreamSession(reqID, chatID, b.sendStreamFn)
	st := &activeWeComStream{
		session:   sess,
		chatID:    chatID,
		thread:    thread,
		cancel:    cancel,
		startedAt: time.Now(),
	}

	// Send Frame 1 immediately so WeCom shows native "思考中..." within ~100ms.
	initThink := "正在思考并分析你的请求…"
	if err := sess.SendProgress(watchCtx, initThink, "", true); err != nil {
		b.logger.Debug("send initial wecom router stream frame failed", "thread", thread, "err", err)
		cancel()
		return nil
	}

	b.mu.Lock()
	b.routerStreams[thread] = st
	b.mu.Unlock()

	go b.watchRouterStream(watchCtx, st, initThink)
	return st
}

func (b *Bus) openWorkerStream(ctx context.Context, thread, chatID, reqID, workerName string) (*activeWeComStream, context.Context) {
	reqID = strings.TrimSpace(reqID)
	if reqID == "" || !b.canSendStream() {
		return nil, nil
	}
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := wecom.NewStreamSession(reqID, chatID, b.sendStreamFn)
	st := &activeWeComStream{
		session:   sess,
		chatID:    chatID,
		thread:    thread,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	initThink := fmt.Sprintf("正在连接 #%s 并开始思考…", workerName)
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
	_ = st.session.SendProgress(watchCtx, initThink, "", false)
	go b.watchWorkerStream(watchCtx, st, workerName, initThink)
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
	return st.session.Finish(sctx, think, body)
}

func (b *Bus) takeRouterStream(thread string) *activeWeComStream {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.routerStreams == nil {
		return nil
	}
	st := b.routerStreams[thread]
	if st != nil {
		delete(b.routerStreams, thread)
	}
	return st
}

// OnRouterTurnEnd closes any dangling Router passive stream when Router finishes a turn silently
// without calling `corgi reply` and no further queued triggers await Router.
func (b *Bus) OnRouterTurnEnd(ctx context.Context) {
	if b.engine != nil && b.engine.HasQueuedRouterTriggers(ctx) {
		return
	}
	b.mu.Lock()
	var stale []*activeWeComStream
	for thread, st := range b.routerStreams {
		if time.Since(st.startedAt) >= 3*time.Second {
			stale = append(stale, st)
			delete(b.routerStreams, thread)
		}
	}
	b.mu.Unlock()
	for _, st := range stale {
		_, _ = st.seal(ctx, "", "本轮已处理完成。")
	}
}

func (b *Bus) watchRouterStream(ctx context.Context, st *activeWeComStream, initThink string) {
	timer := time.NewTimer(wecom.StreamMaxDuration)
	defer timer.Stop()
	defer st.cancel()

	var tr *agent.TranscriptReader
	var lastPath string
	baseline := 0
	initLocal := func() {
		if b.engine == nil {
			return
		}
		p, ad := b.engine.RouterTranscriptInfo(ctx)
		if p == "" || ad == nil || p == lastPath {
			return
		}
		lastPath = p
		tr = agent.NewTranscriptReader(p, ad, "")
		tr.OpenTail()
		tr.Poll()
		baseline = len(tr.Messages())
	}
	initLocal()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
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
			if tr == nil {
				initLocal()
				continue
			}
			if !tr.Poll() {
				continue
			}
			msgs := tr.Messages()
			if len(msgs) < baseline {
				baseline = 0
			}
			if think := formatProgressThink(msgs[baseline:], initThink); think != "" {
				_ = st.session.SendProgress(ctx, think, "", false)
			}
		}
	}
}

func (b *Bus) clearRouterStreamIfMatch(st *activeWeComStream) {
	if st == nil {
		return
	}
	b.mu.Lock()
	if b.routerStreams[st.thread] == st {
		delete(b.routerStreams, st.thread)
	}
	b.mu.Unlock()
}

func (b *Bus) watchWorkerStream(ctx context.Context, st *activeWeComStream, workerName, initThink string) {
	timer := time.NewTimer(wecom.StreamMaxDuration)
	defer timer.Stop()
	defer st.cancel()

	// Locate local transcript or remote Node pane.
	var paneID, machine, localPath string
	var ad agent.Adapter
	if b.engine != nil {
		paneID, machine, localPath, ad = b.engine.WorkerTranscriptSource(ctx, workerName)
	}

	if machine != "" && paneID != "" {
		b.watchRemoteWorkerSSE(ctx, timer, st, machine, paneID, initThink)
		return
	}
	b.watchLocalWorkerTranscript(ctx, timer, st, workerName, localPath, ad, initThink)
}

func (b *Bus) watchLocalWorkerTranscript(ctx context.Context, timer *time.Timer, st *activeWeComStream, workerName, localPath string, ad agent.Adapter, initThink string) {
	var tr *agent.TranscriptReader
	lastPath := ""
	baseline := 0
	attach := func(p string, adapter agent.Adapter, initial bool) {
		if p == "" || adapter == nil || p == lastPath {
			return
		}
		lastPath = p
		tr = agent.NewTranscriptReader(p, adapter, "")
		tr.OpenTail()
		tr.Poll()
		if initial {
			baseline = len(tr.Messages())
		} else {
			baseline = 0
		}
	}
	attach(localPath, ad, true)

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
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
			if tr == nil && b.engine != nil {
				_, _, p, adapter := b.engine.WorkerTranscriptSource(ctx, workerName)
				attach(p, adapter, false)
				continue
			}
			if tr == nil || !tr.Poll() {
				continue
			}
			msgs := tr.Messages()
			if len(msgs) < baseline {
				baseline = 0
			}
			if think := formatProgressThink(sliceAfterLastUser(msgs, baseline), initThink); think != "" {
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
	input := strings.Join(strings.Fields(strings.TrimSpace(m.Input)), " ")
	if strings.Contains(input, "corgi spawn") {
		return "• 正在派发 Worker…"
	}
	if strings.Contains(input, "corgi send") {
		return "• 正在给 Worker 发送指令…"
	}
	if strings.Contains(input, "corgi reply") {
		return "• 正在整理回复…"
	}
	label := friendlyToolName(name)
	if input != "" {
		r := []rune(input)
		if len(r) > 56 {
			input = string(r[:56]) + "…"
		}
		return "• " + label + "：" + input
	}
	return "• " + label
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

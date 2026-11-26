package wecom

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatStreamContentTagSafetyAndByteCeiling(t *testing.T) {
	// 1. Literal </think> in both think and body must be escaped so WeCom tags stay balanced.
	got := FormatStreamContent("步骤1 </think> 步骤2", "结论含 <think> 示例 </think>")
	if strings.Count(got, "<think>") != 1 || strings.Count(got, "</think>") != 1 {
		t.Fatalf("expected exactly one balanced <think>...</think> pair, got: %q", got)
	}
	if !strings.HasPrefix(got, "<think>") || !strings.Contains(got, "</think>\n结论含 &lt;think&gt;") {
		t.Fatalf("unexpected sanitized output: %q", got)
	}

	// 2. Oversized thinking + Chinese UTF-8 body must stay <= StreamMaxBytes and keep valid UTF-8.
	longThink := strings.Repeat("正在分析上游依赖与调用链路，定位关键瓶颈。\n", 600)
	longBody := "核心结论：" + strings.Repeat("修复方案已验证通过。", 500)
	bounded := FormatStreamContent(longThink, longBody)
	if len(bounded) > StreamMaxBytes {
		t.Fatalf("exceeded StreamMaxBytes: got %d > %d", len(bounded), StreamMaxBytes)
	}
	if !utf8.ValidString(bounded) {
		t.Fatal("formatted stream content is not valid UTF-8")
	}
	if strings.Count(bounded, "<think>") != 1 || strings.Count(bounded, "</think>") != 1 {
		t.Fatalf("tags unbalanced after trimming: count open=%d close=%d", strings.Count(bounded, "<think>"), strings.Count(bounded, "</think>"))
	}
	if !strings.Contains(bounded, "核心结论：") {
		t.Fatal("expected body to be preserved when trimming think")
	}
}

func TestStreamSessionSingleFinishPreservesLastThink(t *testing.T) {
	type frame struct {
		reqID    string
		streamID string
		content  string
		finish   bool
	}
	var frames []frame
	sess := NewStreamSession("req-1", "u1", func(_ context.Context, reqID, streamID, content string, finish bool) error {
		frames = append(frames, frame{reqID: reqID, streamID: streamID, content: content, finish: finish})
		return nil
	})
	sess.SetMinInterval(0)

	ctx := context.Background()
	if err := sess.SendProgress(ctx, "1. 读取日志", "", true); err != nil {
		t.Fatal(err)
	}
	// Duplicate content should be deduped.
	if err := sess.SendProgress(ctx, "1. 读取日志", "", true); err != nil {
		t.Fatal(err)
	}
	if err := sess.SendProgress(ctx, "1. 读取日志\n2. 定位超时", "", true); err != nil {
		t.Fatal(err)
	}
	ok, err := sess.Finish(ctx, "", "**结论**：连接池已调大")
	if err != nil || !ok {
		t.Fatalf("Finish failed: ok=%v err=%v", ok, err)
	}
	// Second Finish or subsequent SendProgress must be no-op.
	ok2, err := sess.Finish(ctx, "", "duplicate")
	if err != nil || ok2 {
		t.Fatalf("expected second Finish to be ignored, got ok=%v err=%v", ok2, err)
	}
	_ = sess.SendProgress(ctx, "late step", "", true)

	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d: %+v", len(frames), frames)
	}
	sid := frames[0].streamID
	for i, f := range frames {
		if f.reqID != "req-1" || f.streamID != sid {
			t.Fatalf("frame %d mismatched req/stream ID: %+v", i, f)
		}
	}
	last := frames[2]
	if !last.finish || !strings.Contains(last.content, "<think>1. 读取日志\n2. 定位超时</think>\n**结论**：连接池已调大") {
		t.Fatalf("expected final frame to keep lastThink + body, got: %+v", last)
	}
}

func TestWorkerPickerCardAndNavEventParsing(t *testing.T) {
	opts := []WorkerPickOption{
		{Name: "agent-flow-plan", Title: "物言 Agent 内部数据流可视化方案", Machine: "MacBook", Kind: "agy", Status: "idle", HasWorkItem: true},
		{Name: "media-eval-plan", Title: "物言 Agent 生图/生视频效果评测方案", Machine: "MacBook", Kind: "agy", Status: "idle", HasWorkItem: true},
		{Name: "session-launch-fix", Title: "修复新建会话/任务启动失败", Machine: "MacBook", Kind: "agy", Status: "idle", HasWorkItem: true},
		{Name: "taihu-plan", Title: "Corgi 移动端接入太湖方案分析", Machine: "Hub", Kind: "codebuddy", Status: "idle", HasWorkItem: true},
		{Name: "chat-inline-preview-plan", Title: "聊天链接右侧内嵌预览设计", Machine: "MacBook", Kind: "agy", Status: "idle"},
		{Name: "repo-state-check", Title: "只读仓库核验", Machine: "MacBook", Kind: "agy", Status: "idle"},
		{Name: "codebuddy-2517", Title: "拉取浅色收纳箱原型", Machine: "Hub", Kind: "codebuddy", Ended: true},
	}

	// Page 0 without focus: 5 items + "下一页 →" (6 buttons total), crisp numeric rows + "选 1..5".
	p0 := BuildWorkerPickerCard("nav:pick:1", "", "", opts, 0)
	btns0, _ := p0["button_list"].([]map[string]any)
	horiz0, _ := p0["horizontal_content_list"].([]map[string]any)
	if len(btns0) != 6 || len(horiz0) != 5 {
		t.Fatalf("expected 6 buttons & 5 rows on page 0, got btns=%d horiz=%d", len(btns0), len(horiz0))
	}
	if horiz0[0]["keyname"] != "1." || horiz0[0]["value"] != "物言 Agent 内部数据流可视化方案" {
		t.Fatalf("unexpected row 0: %+v", horiz0[0])
	}
	if btns0[0]["text"] != "选 1" || btns0[0]["key"] != "nav:focus:agent-flow-plan" {
		t.Fatalf("unexpected button 0: %+v", btns0[0])
	}
	if btns0[5]["text"] != "下一页 →" || btns0[5]["key"] != "nav:page:1" {
		t.Fatalf("expected next-page button at index 5, got: %+v", btns0[5])
	}

	// Page 1 without focus: remaining 2 items (6., 7.) + "← 上一页".
	p1 := BuildWorkerPickerCard("nav:pick:1", "", "", opts, 1)
	btns1, _ := p1["button_list"].([]map[string]any)
	horiz1, _ := p1["horizontal_content_list"].([]map[string]any)
	if len(horiz1) != 2 || horiz1[0]["keyname"] != "6." || horiz1[1]["keyname"] != "7." {
		t.Fatalf("unexpected page 1 rows: %+v", horiz1)
	}
	if len(btns1) != 3 || btns1[0]["text"] != "选 6" || btns1[2]["key"] != "nav:page:0" {
		t.Fatalf("unexpected page 1 buttons: %+v", btns1)
	}

	// Focused mode includes "回主对话" first.
	pf := BuildWorkerPickerCard("nav:pick:1", "agent-flow-plan", "物言 Agent 内部数据流可视化方案", opts[:2], 0)
	btnsF, _ := pf["button_list"].([]map[string]any)
	if len(btnsF) != 3 || btnsF[0]["key"] != "nav:main" || btnsF[1]["key"] != "nav:focus:agent-flow-plan" {
		t.Fatalf("unexpected focused picker buttons: %+v", btnsF)
	}

	ev, ok := ParseCardEventCallback("req-nav-1", []byte(`{
		"chattype":"single",
		"from":{"userid":"T123"},
		"event":{"eventtype":"template_card_event","task_id":"nav:pick:1","event_key":"nav:page:1"}
	}`))
	if !ok || ev.ActionID != "nav" || ev.ChoiceKey != "page:1" {
		t.Fatalf("unexpected nav page parse: ok=%v ev=%+v", ok, ev)
	}
}

func TestSendStreamReplyRejectsMissingIDs(t *testing.T) {
	c := NewClient(Config{}, Handlers{}, nil)
	if err := c.SendStreamReply(context.Background(), "", "s1", "hi", true); err == nil {
		t.Fatal("expected error when reqID is empty")
	}
	if err := c.SendStreamReply(context.Background(), "r1", "", "hi", true); err == nil {
		t.Fatal("expected error when streamID is empty")
	}
}

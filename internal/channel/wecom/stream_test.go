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
	card := BuildWorkerPickerCard("nav:pick:1", "alpha", []WorkerPickOption{
		{Name: "alpha", Machine: "MacBook", Kind: "agy", Status: "idle"},
		{Name: "beta", Machine: "Hub", Kind: "codebuddy", Ended: true},
	})
	btns, _ := card["button_list"].([]map[string]any)
	if len(btns) != 3 {
		t.Fatalf("expected 3 buttons (回主对话 + 2 workers), got %d", len(btns))
	}
	if btns[0]["key"] != "nav:main" || btns[1]["key"] != "nav:focus:alpha" || btns[2]["key"] != "nav:focus:beta" {
		t.Fatalf("unexpected picker button keys: %+v", btns)
	}

	ev, ok := ParseCardEventCallback("req-nav-1", []byte(`{
		"chattype":"single",
		"from":{"userid":"T123"},
		"event":{"eventtype":"template_card_event","task_id":"nav:pick:1","event_key":"nav:focus:beta"}
	}`))
	if !ok || ev.ActionID != "nav" || ev.ChoiceKey != "focus:beta" {
		t.Fatalf("unexpected nav card parse: ok=%v ev=%+v", ok, ev)
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

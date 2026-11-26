package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseThinkingAcrossAllAdapters(t *testing.T) {
	t.Run("agy", func(t *testing.T) {
		ad := &AgyAdapter{}
		line := []byte(`{"type":"PLANNER_RESPONSE","thinking":"先检查连接池配置","content":"已定位超时原因","tool_calls":[{"name":"read_file","args":{"Path":"/tmp/a.go"}}]}`)
		msgs, ok := ad.ParseTranscriptLine(line, "")
		if !ok || len(msgs) != 3 {
			t.Fatalf("expected 3 messages (thinking+assistant+tool), got ok=%v len=%d msgs=%+v", ok, len(msgs), msgs)
		}
		if msgs[0].Kind != "thinking" || msgs[0].Text != "先检查连接池配置" {
			t.Fatalf("unexpected thinking message: %+v", msgs[0])
		}
		if msgs[1].Kind != "assistant" || msgs[1].Text != "已定位超时原因" {
			t.Fatalf("unexpected assistant message: %+v", msgs[1])
		}
		if msgs[2].Kind != "tool" || msgs[2].Name != "read_file" {
			t.Fatalf("unexpected tool message: %+v", msgs[2])
		}
	})

	t.Run("codebuddy_rawContent", func(t *testing.T) {
		ad := &CodeBuddyAdapter{}
		line := []byte(`{"type":"reasoning","content":[],"rawContent":[{"type":"reasoning_text","text":"先读取 router 目录"}]}`)
		msgs, ok := ad.ParseTranscriptLine(line, "")
		if !ok || len(msgs) != 1 || msgs[0].Kind != "thinking" || msgs[0].Text != "先读取 router 目录" {
			t.Fatalf("unexpected codebuddy reasoning parse: ok=%v msgs=%+v", ok, msgs)
		}
	})

	t.Run("claude", func(t *testing.T) {
		ad := &ClaudeAdapter{}
		line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"分析边界条件"},{"type":"text","text":"结论如下"}]}}`)
		msgs, ok := ad.ParseTranscriptLine(line, "")
		if !ok || len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got ok=%v msgs=%+v", ok, msgs)
		}
		if msgs[0].Kind != "thinking" || msgs[0].Text != "分析边界条件" {
			t.Fatalf("unexpected claude thinking: %+v", msgs[0])
		}
		if msgs[1].Kind != "assistant" || msgs[1].Text != "结论如下" {
			t.Fatalf("unexpected claude assistant: %+v", msgs[1])
		}

		// Empty redacted thinking block should be skipped cleanly.
		emptyLine := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"abc"}]}}`)
		if msgs, ok := ad.ParseTranscriptLine(emptyLine, ""); ok || len(msgs) != 0 {
			t.Fatalf("expected empty claude thinking skipped, got ok=%v msgs=%+v", ok, msgs)
		}
	})

	t.Run("codex", func(t *testing.T) {
		ad := &CodexAdapter{}
		line := []byte(`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"对比两个模块的差异"}]}}`)
		msgs, ok := ad.ParseTranscriptLine(line, "")
		if !ok || len(msgs) != 1 || msgs[0].Kind != "thinking" || msgs[0].Text != "对比两个模块的差异" {
			t.Fatalf("unexpected codex reasoning parse: ok=%v msgs=%+v", ok, msgs)
		}

		// Encrypted-only reasoning with empty summary should return false cleanly.
		encLine := []byte(`{"type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"gAAAA"}}`)
		if msgs, ok := ad.ParseTranscriptLine(encLine, ""); ok || len(msgs) != 0 {
			t.Fatalf("expected encrypted-only codex reasoning ignored, got ok=%v msgs=%+v", ok, msgs)
		}
	})
}

func TestLastAssistantAfterLastUserDoesNotLeakStaleTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.jsonl")
	before := `{"type":"message","role":"user","content":[{"type":"input_text","text":"old q"}]}` + "\n" +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"stale answer from hours ago"}]}` + "\n" +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"new direct question"}]}` + "\n"
	if err := os.WriteFile(p, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := NewTranscriptReader(p, &CodeBuddyAdapter{}, "")
	tr.OpenTail()
	if txt, ok := tr.LastAssistantAfterLastUser(128000); ok || txt != "" {
		t.Fatalf("expected no reply after latest user turn, got ok=%v txt=%q", ok, txt)
	}

	after := before + `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fresh answer"}]}` + "\n"
	if err := os.WriteFile(p, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.Poll()
	if txt, ok := tr.LastAssistantAfterLastUser(128000); !ok || txt != "fresh answer" {
		t.Fatalf("expected fresh answer after latest user turn, got ok=%v txt=%q", ok, txt)
	}
}

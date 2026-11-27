package agent

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCodexToolErrorParsingAndBoundedTurnToolStats(t *testing.T) {
	ad := &CodexAdapter{}
	// Array-wrapped JSON output with non-zero exit_code -> Error: true.
	failLine := []byte(`{"type":"response_item","payload":{"type":"function_call_output","output":[{"type":"input_text","text":"Script completed\nOutput:\n"},{"type":"input_text","text":"{\"chunk_id\":\"1\",\"exit_code\":1,\"output\":\"FAIL\"}"}]}}`)
	msgs, ok := ad.ParseTranscriptLine(failLine, "")
	if !ok || len(msgs) != 1 || !msgs[0].Error {
		t.Fatalf("expected codex exit_code=1 parsed as Error=true, got ok=%v msgs=%+v", ok, msgs)
	}
	// exit_code=0 -> Error: false.
	okLine := []byte(`{"type":"response_item","payload":{"type":"function_call_output","output":[{"type":"input_text","text":"{\"chunk_id\":\"2\",\"exit_code\":0,\"output\":\"PASS\"}"}]}}`)
	msgs2, ok := ad.ParseTranscriptLine(okLine, "")
	if !ok || len(msgs2) != 1 || msgs2[0].Error {
		t.Fatalf("expected codex exit_code=0 parsed as Error=false, got ok=%v msgs=%+v", ok, msgs2)
	}

	// Build a CodeBuddy transcript with an old turn failure followed by a new turn alternating
	// edit(ok) -> test(fail) x4 (8 tool results + 8 tool calls = 16 tool messages).
	p := filepath.Join(t.TempDir(), "thrash.jsonl")
	var lines []byte
	appendLine := func(s string) { lines = append(lines, []byte(s+"\n")...) }
	appendLine(`{"type":"message","role":"user","content":[{"type":"input_text","text":"old turn"}]}`)
	appendLine(`{"type":"function_call_result","name":"Bash","status":"error","output":"old err"}`)
	appendLine(`{"type":"message","role":"user","content":[{"type":"input_text","text":"fix tests"}]}`)
	for i := 0; i < 4; i++ {
		appendLine(`{"type":"function_call","name":"Edit","arguments":{"file_path":"a.go"}}`)
		appendLine(`{"type":"function_call_result","name":"Edit","status":"success","output":"edited"}`)
		appendLine(`{"type":"function_call","name":"Bash","arguments":{"command":"go test"}}`)
		appendLine(`{"type":"function_call_result","name":"Bash","status":"error","output":"FAIL"}`)
	}
	if err := os.WriteFile(p, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	stats := ComputeTurnToolStats(p, &CodeBuddyAdapter{}, 1<<20)
	if stats == nil {
		t.Fatal("expected non-nil ToolStats")
	}
	// Calls must NOT be counted; LastUserAt must still be recorded when hitting windowSize=8.
	if stats.N != 8 || stats.Failed != 4 || stats.TrailingFailed != 1 || stats.LastFailureAt <= 0 || stats.LastUserAt <= 0 {
		t.Fatalf("unexpected tool stats: %+v", stats)
	}

	// Verify a fresh turn with fewer than 8 results stops strictly at the latest user turn
	// and never leaks failures from prior turns.
	appendLine(`{"type":"message","role":"user","content":[{"type":"input_text","text":"new clean turn"}]}`)
	appendLine(`{"type":"function_call_result","name":"Edit","status":"success","output":"ok"}`)
	if err := os.WriteFile(p, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	cleanStats := ComputeTurnToolStats(p, &CodeBuddyAdapter{}, 1<<20)
	if cleanStats == nil || cleanStats.N != 1 || cleanStats.Failed != 0 || cleanStats.LastUserAt <= stats.LastUserAt {
		t.Fatalf("expected clean turn isolated from prior failures, got %+v", cleanStats)
	}
}

func TestInspectLastTurnAndCorgiHeaderTriggerExtraction(t *testing.T) {
	// Only real Corgi deliverBatch event header lines (`#tID · ...`) should match — NOT prose mentions like #t9999.
	userPrompt := "[corgi] 2 new events (batched)\n\n#t3323 · chat:main · router · from corgi · 16:59\n  不要重复报告 #t9999 或旧版\n\n#t3327 · chat:main · router · from corgi · 17:00\n  去掉折叠把手"
	tid, seq := ExtractCorgiHeaderMaxTrigger(userPrompt)
	if tid != "t3327" || seq != 3327 {
		t.Fatalf("expected max header trigger (t3327, 3327), got (%q, %d)", tid, seq)
	}

	// Replay the 16:47 -> 16:59 #t3323 -> 17:03 tool-only fake idle sequence.
	p := filepath.Join(t.TempDir(), "turn.jsonl")
	raw := `{"type":"message","role":"user","content":[{"type":"input_text","text":"#t3313 · chat:main · router · 16:39\n  做多方案原型"}]}` + "\n" +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"16:47 旧版 4 套折叠原型"}]}` + "\n" +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"[corgi] 1 new event\n\n#t3323 · chat:main · router · 16:59\n  加入无限上下滑滚牌"}]}` + "\n" +
		`{"type":"reasoning","content":[],"rawContent":[{"type":"reasoning_text","text":"正在检查原型代码"}]}` + "\n" +
		`{"type":"function_call","name":"Read","arguments":{"file_path":"a.html"}}` + "\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := NewTranscriptReader(p, &CodeBuddyAdapter{}, "")
	tr.OpenTail()
	st := tr.InspectLastTurn(128000)
	if st.AfterLastUser {
		t.Fatalf("expected AfterLastUser=false on tool-only turn, got %+v", st)
	}
	if st.LastUserTID != "t3323" || st.LastUserSeq != 3323 || st.LastUserAt <= 0 {
		t.Fatalf("expected LastUserTID=t3323 and LastUserAt>0, got %+v", st)
	}
	if !strings.Contains(st.Text, "16:47 旧版") {
		t.Fatalf("expected fallback display text preserved when AfterLastUser=false, got %+v", st)
	}

	// Once the worker actually writes a post-user assistant message, InspectLastTurn reports AfterLastUser=true.
	raw += `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"已完成无限上下滑滚牌原型"}]}` + "\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.Poll()
	st2 := tr.InspectLastTurn(128000)
	if !st2.AfterLastUser || st2.Text != "已完成无限上下滑滚牌原型" || st2.AssistantAt <= st2.LastUserAt || st2.LastUserTID != "t3323" {
		t.Fatalf("unexpected fresh TurnState: %+v", st2)
	}
}

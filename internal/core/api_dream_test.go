package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestAPIDreamNoFleetFullLoopAndSkillCards(t *testing.T) {
	var (
		mu       sync.Mutex
		reqCount int
		seenMsgs [][]LLMMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model           string       `json:"model"`
			ReasoningEffort string       `json:"reasoning_effort"`
			Messages        []LLMMessage `json:"messages"`
			Tools           []LLMToolDef `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode llm req: %v", err)
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if body.ReasoningEffort != "no_think" {
			t.Errorf("expected user effort no_think preserved, got %q", body.ReasoningEffort)
		}
		if len(body.Tools) != 3 {
			t.Errorf("expected 3 bounded dream tools, got %d", len(body.Tools))
		}
		mu.Lock()
		reqCount++
		step := reqCount
		seenMsgs = append(seenMsgs, body.Messages)
		mu.Unlock()

		w.Header().Set("X-Request-Id", "req-dream-test")
		switch step {
		case 1:
			// Step 1: read note, attempt forbidden writes (guardrail checks), and do valid note write.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-d1", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "c1", "type": "function", "function": map[string]any{"name": "read_memory", "arguments": `{"path":"notes/deploy.md"}`}},
							{"id": "c2", "type": "function", "function": map[string]any{"name": "write_memory", "arguments": `{"path":"messages.md","content":"hack"}`}},
							{"id": "c3", "type": "function", "function": map[string]any{"name": "write_memory", "arguments": `{"path":"tasks/private/todo.md","content":"bad"}`}},
							{"id": "c4", "type": "function", "function": map[string]any{"name": "write_memory", "arguments": `{"path":"notes/sub/nested.md","content":"bad"}`}},
							{"id": "c5", "type": "function", "function": map[string]any{"name": "write_memory", "arguments": `{"path":"notes/deploy.md","content":"# 部署经验\n- 先跑 race 单测再发版\n"}`}},
						},
					},
				}},
			})
		default:
			// Step 2: finish_dream atomically writes journal with skill candidate & fix.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-d2", "model": "hy4",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "c6", "type": "function", "function": map[string]any{
								"name":      "finish_dream",
								"arguments": `{"summary":"已精简部署笔记并汇总复用规律","journal":"- 更新 notes/deploy.md\n- 校验开放工作项无遗漏","skill_candidates":["一键发版部署"],"skill_fixes":["wecom（修复流超时）"]}`,
							}},
						},
					},
				}},
			})
		}
	}))
	defer srv.Close()

	eng, st, sink, _ := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()

	// Seed a note whose body must NOT be dumped upfront (only filename in prompt).
	secretMarker := "UNIQUE_BODY_ONLY_FOR_ON_DEMAND_READ_9911"
	if err := eng.memory.SaveFile("notes/deploy.md", "# 部署\n"+secretMarker); err != nil {
		t.Fatal(err)
	}
	// Seed a symlink escape target to verify read_memory rejects symlinks outside memory/.
	outside := filepath.Join(t.TempDir(), "secret.md")
	_ = os.WriteFile(outside, []byte("top-secret"), 0o600)
	_ = os.Symlink(outside, filepath.Join(eng.memory.MemDir(), "notes", "escape.md"))
	if out := eng.toolDreamReadMemory(`{"path":"notes/escape.md"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected symlink read_memory to fail, got %s", out)
	}
	if out := eng.toolDreamWriteMemory(`{"path":"notes/escape.md","content":"overwrite"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected symlink write_memory to fail, got %s", out)
	}

	if err := eng.StartDream(ctx, "manual"); err != nil {
		t.Fatalf("StartDream with nil fleet failed: %v", err)
	}
	if !eng.Dreaming() {
		t.Fatal("expected Dreaming()=true right after StartDream")
	}

	// Wait for runAPIDream goroutine to mark apiDone, then drive finalization via tick (no fleet).
	waitUntil(t, 5*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.dream != nil && eng.dream.apiDone
	}, "api dream apiDone")

	eng.tick(ctx)

	if eng.Dreaming() {
		t.Fatal("expected Dreaming()=false after tick finalized API dream")
	}
	if eng.LastDream(ctx).IsZero() {
		t.Fatal("expected last_dream to be updated")
	}
	if _, ok, _ := st.GetKV(ctx, activeDreamKVKey); ok {
		t.Fatal("expected active_dream marker deleted after finish")
	}

	// Verify initial prompt contained filename only, NOT secret body.
	mu.Lock()
	firstPrompt, _ := seenMsgs[0][1].Content.(string)
	secondMsgs := seenMsgs[1]
	mu.Unlock()
	if !strings.Contains(firstPrompt, "notes/deploy.md") {
		t.Fatalf("expected notes/deploy.md filename listed in prompt, got: %s", firstPrompt)
	}
	if strings.Contains(firstPrompt, secretMarker) {
		t.Fatal("expected note body NOT loaded upfront into dream prompt")
	}

	// Verify guardrails rejected messages.md, tasks/private/todo.md, notes/sub/nested.md, while allowing read & write of notes/deploy.md.
	toolResults := map[string]string{}
	for _, m := range secondMsgs {
		if m.Role == "tool" {
			s, _ := m.Content.(string)
			toolResults[m.ToolCallID] = s
		}
	}
	if !strings.Contains(toolResults["c1"], secretMarker) {
		t.Fatalf("expected read_memory to return deploy.md content, got %s", toolResults["c1"])
	}
	for _, cid := range []string{"c2", "c3", "c4"} {
		if !strings.Contains(toolResults[cid], `"ok":false`) {
			t.Fatalf("expected guardrail failure for %s, got %s", cid, toolResults[cid])
		}
	}
	if !strings.Contains(toolResults["c5"], `"ok":true`) {
		t.Fatalf("expected valid write_memory to succeed, got %s", toolResults["c5"])
	}

	// Verify updated memory file, atomic journal, no leftover tmp files, and skill cards.
	gotNote, err := os.ReadFile(filepath.Join(eng.memory.MemDir(), "notes", "deploy.md"))
	if err != nil || !strings.Contains(string(gotNote), "先跑 race 单测再发版") {
		t.Fatalf("expected deploy.md updated, got %q err=%v", string(gotNote), err)
	}
	dreamEntries, _ := os.ReadDir(eng.memory.DreamsDir())
	var mdCount int
	for _, de := range dreamEntries {
		if strings.HasPrefix(de.Name(), ".corgi-tmp-") {
			t.Fatalf("leftover temp file in dreams dir: %s", de.Name())
		}
		if strings.HasSuffix(de.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 1 {
		t.Fatalf("expected 1 journal md file, got %d", mdCount)
	}

	entries, _, _ := st.ListEntries(ctx, model.MainThread, 20)
	foundWake := false
	for _, en := range entries {
		if en.Role == "event" && strings.Contains(en.Text, "已精简部署笔记并汇总复用规律") {
			foundWake = true
			break
		}
	}
	if !foundWake {
		t.Fatalf("expected wake event with dream summary, entries=%+v", entries)
	}

	sink.mu.Lock()
	replyCount := len(sink.replies)
	sink.mu.Unlock()
	if replyCount != 2 {
		t.Fatalf("expected 2 skill suggestion reply cards (candidate + fix), got %d", replyCount)
	}
}

func TestAPIDreamFailureConvergesReportsToBrainAndNightlyDoesNotSpin(t *testing.T) {
	var (
		mu           sync.Mutex
		brainReports []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			if last, ok := req.Messages[len(req.Messages)-1].Content.(string); ok && strings.Contains(last, "做梦异常报告") {
				mu.Lock()
				brainReports = append(brainReports, last)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-brain", "model": "kimi-k3",
					"choices": []map[string]any{{
						"index": 0, "finish_reason": "stop",
						"message": map[string]any{"role": "assistant", "content": "[SILENT]"},
					}},
				})
				return
			}
		}
		http.Error(w, "upstream overloaded", http.StatusBadRequest)
	}))
	defer srv.Close()

	eng, st, _, _ := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()

	if err := eng.StartDream(ctx, "nightly"); err != nil {
		t.Fatalf("StartDream nightly: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.dream != nil && eng.dream.apiDone
	}, "api dream failure done")

	eng.tick(ctx)

	if eng.Dreaming() {
		t.Fatal("expected Dreaming()=false after failed API dream finalized")
	}
	if eng.LastDream(ctx).IsZero() {
		t.Fatal("expected nightly failure to advance last_dream to prevent spin")
	}
	slot, ok := eng.readNightlyDreamSlot(ctx)
	if !ok || slot.Outcome != "failed" {
		t.Fatalf("expected nightly_dream_slot failed, got ok=%v slot=%+v", ok, slot)
	}
	if _, hasMarker, _ := st.GetKV(ctx, activeDreamKVKey); hasMarker {
		t.Fatal("expected active_dream marker removed after failure")
	}

	// Verify Brain received the Dream anomaly report to decide auto vs human intervention.
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		n := len(brainReports)
		mu.Unlock()
		return n > 0 && !eng.isRouterWorking(ctx)
	}, "brain handled dream anomaly report")
	mu.Lock()
	rep := brainReports[0]
	mu.Unlock()
	if !strings.Contains(rep, "upstream overloaded") {
		t.Fatalf("expected dream failure cause in Brain report, got: %s", rep)
	}
}

func TestAPIDreamRestartRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withJournal bool
		wantText    string
	}{
		{"recovered_with_journal", true, "重启前已落盘摘要"},
		{"recovered_without_journal", false, "本次整理记忆未能完成"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "corgi.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()

			// Save settings and seed active_dream marker as if Hub crashed mid-API-dream.
			cfg, _ := st.LoadSettings(ctx, dir)
			cfg.RouterAPIProvider = RouterAPIProviderTokenHub
			cfg.RouterAPIModel = "hy4"
			if err := st.SaveSettings(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			dreamAt := time.Now().Add(-30 * time.Second).Truncate(time.Second)
			markerRaw, _ := json.Marshal(activeDreamMarker{
				ID:     "20261128-040000.000000001",
				At:     dreamAt,
				Reason: "nightly",
			})
			if err := st.SetKV(ctx, activeDreamKVKey, string(markerRaw)); err != nil {
				t.Fatal(err)
			}

			mem := NewMemoryManager(dir)
			_ = mem.Ensure()
			if tc.withJournal {
				jPath := filepath.Join(mem.DreamsDir(), journalName(dreamAt)+".md")
				if err := os.WriteFile(jPath, []byte("重启前已落盘摘要\n\n- 明细\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// NewEngine with nil fleet loads the recovered API dream marker.
			eng, err := NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !eng.Dreaming() {
				t.Fatal("expected recovered API dream to be active on startup")
			}

			// Single tick must reconcile immediately without waiting 90s or touching herdr.
			eng.tick(ctx)

			if eng.Dreaming() {
				t.Fatal("expected recovered API dream to finalize immediately on first tick")
			}
			if _, ok, _ := st.GetKV(ctx, activeDreamKVKey); ok {
				t.Fatal("expected active_dream marker cleared")
			}
			entries, _, _ := st.ListEntries(ctx, model.MainThread, 10)
			found := false
			for _, en := range entries {
				if en.Role == "event" && strings.Contains(en.Text, tc.wantText) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected recovery event containing %q, got %+v", tc.wantText, entries)
			}
		})
	}
}

func TestAPIDreamAsyncNonBlockingAndModeSwitchMutex(t *testing.T) {
	dreamStarted := make(chan struct{})
	dreamRelease := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LLMChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		isDream := strings.HasSuffix(req.PromptCacheKey, ":dream")
		if isDream {
			select {
			case <-dreamStarted:
			default:
				close(dreamStarted)
			}
			<-dreamRelease
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-dream", "model": "kimi-k3",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "c1", "type": "function", "function": map[string]any{
								"name":      "finish_dream",
								"arguments": `{"summary":"异步做梦完成","journal":"- ok"}`,
							}},
						},
					},
				}},
			})
			return
		}
		// User chat request while API Dream is still running asynchronously.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-chat", "model": "kimi-k3",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{
						{"id": "r1", "type": "function", "function": map[string]any{
							"name":      "reply_user",
							"arguments": `{"purpose":"answer","text":"做梦中也能秒回"}`,
						}},
					},
				},
			}},
		})
	}))
	defer srv.Close()

	eng, _, sink, _ := newTestAPIEngine(t, srv.URL+"/v1")
	ctx := context.Background()

	// 1. Start async API dream.
	if err := eng.StartDream(ctx, "manual"); err != nil {
		t.Fatalf("StartDream: %v", err)
	}
	<-dreamStarted

	// Duplicate StartDream must still be rejected while dreaming.
	if err := eng.StartDream(ctx, "manual"); err == nil {
		t.Fatal("expected duplicate StartDream rejected while API dream is running")
	}

	// 2. Concurrent user chat MUST NOT be blocked by async API Dream!
	if _, _, err := eng.Say(ctx, SayRequest{Thread: model.MainThread, Channel: "web", From: "you", Text: "hello during dream"}); err != nil {
		t.Fatal(err)
	}
	eng.tick(ctx)
	waitUntil(t, 5*time.Second, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.replies) >= 1
	}, "user chat replied while API dream still running")
	waitUntil(t, 3*time.Second, func() bool {
		return !eng.isRouterWorking(ctx)
	}, "chat turn settled")
	if !eng.Dreaming() {
		t.Fatal("expected API dream still running in background while chat finished")
	}

	// 3. Release dream, finalize on tick, verify cleanly woken.
	close(dreamRelease)
	waitUntil(t, 5*time.Second, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.dream != nil && eng.dream.apiDone
	}, "api dream done after release")
	eng.tick(ctx)
	if eng.Dreaming() {
		t.Fatal("expected dream finalized after release")
	}
}

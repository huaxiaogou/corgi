package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

func TestEvalThrashSampleTwoStepConfirmationAndAlternatingLoop(t *testing.T) {
	now := time.Now()
	mk := &watchdogMark{pane: "w1:p1", workingSince: now.Add(-7 * time.Minute)}

	// Alternating edit(ok) -> test(fail) x4 in window of 8 -> 1st sample arms prevHit without alerting.
	s1 := &model.ToolStats{N: 8, Failed: 4, TrailingFailed: 1, LastFailureAt: 1000, LastUserAt: 100}
	if dec := evalThrashSample("build-a", mk, s1, now); dec.alert {
		t.Fatalf("first sample should not alert immediately: %+v", dec)
	}

	// Re-reading the exact same tail (LastFailureAt unchanged) must NOT alert.
	if dec := evalThrashSample("build-a", mk, s1, now.Add(time.Minute)); dec.alert {
		t.Fatalf("repeated identical failure offset must not alert: %+v", dec)
	}

	// Second sample with advancing LastFailureAt confirms active thrashing and alerts once.
	s2 := &model.ToolStats{N: 8, Failed: 4, TrailingFailed: 1, LastFailureAt: 1450, LastUserAt: 100}
	dec := evalThrashSample("build-a", mk, s2, now.Add(2*time.Minute))
	if !dec.alert || dec.reason != "thrash" || !strings.Contains(dec.message, "build-a") {
		t.Fatalf("expected thrash alert on advancing second sample, got: %+v", dec)
	}

	// Marking alerted suppresses repeat alerts within the same user turn.
	mk.markAlerted(s2.LastUserAt)
	s3 := &model.ToolStats{N: 8, Failed: 5, TrailingFailed: 4, LastFailureAt: 1900, LastUserAt: 100}
	if dec := evalThrashSample("build-a", mk, s3, now.Add(3*time.Minute)); dec.alert {
		t.Fatalf("expected alerted turn suppressed, got: %+v", dec)
	}
}

func TestNormalizeConstraintAndDirectPromptRemindInterval(t *testing.T) {
	c, err := NormalizeConstraint("  只读调研\n禁止改线上配置  ")
	if err != nil || c != "只读调研 禁止改线上配置" {
		t.Fatalf("unexpected normalized constraint: %q err=%v", c, err)
	}
	tooLong := strings.Repeat("禁", MaxWorkerConstraintRunes+1)
	if _, err := NormalizeConstraint(tooLong); err == nil {
		t.Fatal("expected over-limit constraint to return an explicit error without truncating")
	}

	eng := openTestCoreEngine(t)
	ctx := context.Background()
	wk := &model.Worker{
		Name:       "audit-1",
		Kind:       "codebuddy",
		Cwd:        "/tmp",
		Task:       "audit",
		Thread:     model.MainThread,
		PaneID:     "w9:p1",
		Status:     "idle",
		Constraint: "只读调研；禁止改线上配置",
	}
	if err := eng.store.UpsertWorker(ctx, wk); err != nil {
		t.Fatal(err)
	}

	// 1st direct prompt carries the 1-line red-line header.
	p1, inc1 := eng.decorateDirectPrompt(ctx, "w9:p1", "看下 nginx 配置")
	if !inc1 || !strings.HasPrefix(p1, "【本会话红线】只读调研；禁止改线上配置\n看下 nginx 配置") {
		t.Fatalf("expected first prompt decorated, got inc=%v text=%q", inc1, p1)
	}
	eng.markConstraintDelivered("w9:p1")

	// Immediate follow-up stays clean (no repeated spam).
	p2, inc2 := eng.decorateDirectPrompt(ctx, "w9:p1", "再看下证书路径")
	if inc2 || p2 != "再看下证书路径" {
		t.Fatalf("expected rapid follow-up undecorated, got inc=%v text=%q", inc2, p2)
	}

	// After 30+ minutes, red line is re-injected cleanly.
	eng.mu.Lock()
	eng.constraintRemindAt["w9:p1"] = time.Now().Add(-31 * time.Minute)
	eng.mu.Unlock()
	p3, inc3 := eng.decorateDirectPrompt(ctx, "w9:p1", "继续核对端口")
	if !inc3 || !strings.HasPrefix(p3, "【本会话红线】只读调研；禁止改线上配置\n继续核对端口") {
		t.Fatalf("expected prompt after 30m re-decorated, got inc=%v text=%q", inc3, p3)
	}
}

func TestMatchesMainChatCorgiReplyMatchesPlainAndWorkerRefEntries(t *testing.T) {
	eng := openTestCoreEngine(t)
	ctx := context.Background()
	now := time.Now()
	_ = eng.store.InsertEntry(ctx, &model.Entry{
		ID: "e1", Seq: 1, Role: "corgi", Thread: model.MainThread, Channel: "wecom",
		Text: "当前有 4 项待处理事项，优先级最高的是太湖接入方案。", At: now,
	})
	_ = eng.store.InsertEntry(ctx, &model.Entry{
		ID: "e2", Seq: 2, Role: "corgi", Thread: model.MainThread, Channel: "wecom",
		Text:      "已把规则写入长期记忆。",
		WorkerRef: &model.EntryWorkerRef{Name: "skill-worker"},
		At:        now.Add(time.Second),
	})

	if !eng.MatchesMainChatCorgiReply(ctx, model.MainThread, "当前有 4 项待处理事项") {
		t.Fatal("expected plain main-chat Corgi reply to match")
	}
	if !eng.MatchesMainChatCorgiReply(ctx, model.MainThread, "已把规则写入长期记忆") {
		t.Fatal("expected worker-referenced Corgi report to also match main-chat Corgi replies")
	}
	if eng.MatchesMainChatCorgiReply(ctx, model.MainThread, "完全无关的一句话") {
		t.Fatal("unrelated quote must not match")
	}
}

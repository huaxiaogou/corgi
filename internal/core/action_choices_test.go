package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/store"
)

func TestResolveActionChoicesStrictValidationAndConcurrency(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := NewEngine(ctx, st, nil, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Single-select action rejects >1 choice and rejects unknown keys.
	_, singleAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread:  model.MainThread,
		Text:    "请选择部署策略",
		Options: []string{"蓝绿发布", "灰度发布", "暂缓"},
	})
	if err != nil || singleAct == nil {
		t.Fatalf("ReplyWithMeta single failed: act=%+v err=%v", singleAct, err)
	}
	if _, err := eng.ResolveActionChoices(ctx, singleAct.ID, "web", []string{"1", "2"}); err == nil {
		t.Fatal("expected single-select action to reject multiple choices")
	}
	if _, err := eng.ResolveActionChoices(ctx, singleAct.ID, "web", []string{"99"}); err == nil {
		t.Fatal("expected single-select action to reject unknown key")
	}
	if _, err := eng.ResolveAction(ctx, singleAct.ID, "web", "__bogus__"); err == nil {
		t.Fatal("expected ResolveAction to reject unknown key on decision action")
	}
	resSingle, err := eng.ResolveActionChoices(ctx, singleAct.ID, "wecom", []string{"2"})
	if err != nil || resSingle.ChosenKey != "2" || resSingle.ChosenLabel != "灰度发布" || resSingle.ResolvedChannel != "wecom" {
		t.Fatalf("unexpected single resolution: %+v err=%v", resSingle, err)
	}

	// 2. Multi-select action validates keys, deduplicates, stores ChosenKeys, and enforces single winner.
	_, multiAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{
		Thread:       model.MainThread,
		Text:         "请选择发布前检查项",
		Mode:         model.ActionModeMulti,
		ConfirmLabel: "确认检查项",
		Options:      []string{"灰度验证", "数据备份", "性能压测", "回滚预案", "值班同步"},
	})
	if err != nil || multiAct == nil || !multiAct.IsMulti() || multiAct.ConfirmLabel != "确认检查项" {
		t.Fatalf("ReplyWithMeta multi failed: act=%+v err=%v", multiAct, err)
	}
	if _, err := eng.ResolveActionChoices(ctx, multiAct.ID, "web", []string{"1", "invalid"}); err == nil {
		t.Fatal("expected multi-select to reject unrecognized choice key")
	}

	// Concurrent Web + WeCom submit -> exactly one winner, other gets ErrActionAlreadyResolved.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]*model.Action, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0], errs[0] = eng.ResolveActionChoices(ctx, multiAct.ID, "wecom", []string{"1", "5", "1"})
	}()
	go func() {
		defer wg.Done()
		results[1], errs[1] = eng.ResolveActionChoices(ctx, multiAct.ID, "web", []string{"2", "3"})
	}()
	wg.Wait()

	winners := 0
	conflicts := 0
	var winner *model.Action
	for i := range 2 {
		if errs[i] == nil {
			winners++
			winner = results[i]
		} else if errors.Is(errs[i], store.ErrActionAlreadyResolved) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent error[%d]: %v", i, errs[i])
		}
	}
	if winners != 1 || conflicts != 1 || winner == nil {
		t.Fatalf("expected 1 winner and 1 conflict, got winners=%d conflicts=%d", winners, conflicts)
	}
	if len(winner.ChosenKeys) != 2 {
		t.Fatalf("expected 2 deduplicated ChosenKeys, got %+v", winner.ChosenKeys)
	}

	// 3. worker_dialog rejects multi-select and unknown keys, accepts "esc".
	dlg := &model.Action{
		ID:           "a_dlg_strict",
		Kind:         model.ActionKindWorkerDialog,
		Thread:       model.MainThread,
		TargetWorker: "w1",
		Title:        "是否允许执行命令",
		Choices:      []model.ActionChoice{{Key: "1", Label: "允许"}, {Key: "2", Label: "拒绝"}},
		Status:       model.ActionPending,
		CreatedAt:    time.Now(),
	}
	if err := st.InsertAction(ctx, dlg); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ResolveActionChoices(ctx, dlg.ID, "wecom", []string{"1", "2"}); err == nil {
		t.Fatal("expected worker_dialog to reject multiple choices")
	}
	if _, err := eng.ResolveAction(ctx, dlg.ID, "wecom", "bogus"); err == nil {
		t.Fatal("expected worker_dialog to reject bogus key")
	}
	if res, err := eng.ResolveAction(ctx, dlg.ID, "wecom", "esc"); err != nil || res.ChosenKey != "esc" {
		t.Fatalf("expected worker_dialog to accept esc, got res=%+v err=%v", res, err)
	}

	// 4. Option ceiling (16) allows 16 without truncation and rejects 17 explicitly.
	sixteen := make([]string, 16)
	for i := range sixteen {
		sixteen[i] = fmt.Sprintf("选项%d", i+1)
	}
	if _, act16, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, Text: "16项测试", Mode: model.ActionModeMulti, Options: sixteen}); err != nil || len(act16.Choices) != 16 {
		t.Fatalf("expected 16 choices accepted without truncation, got act=%+v err=%v", act16, err)
	}
	seventeen := append(append([]string(nil), sixteen...), "选项17")
	if _, _, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, Text: "17项超限", Options: seventeen}); err == nil {
		t.Fatal("expected >16 options to return an explicit error instead of silently truncating")
	}

	// 5. ListPendingDecisionsForThread excludes router_check and orders worker_dialog first.
	_, checkAct, _, err := eng.ReplyWithMeta(ctx, ReplyRequest{Thread: model.MainThread, Text: "报告已交付", Check: true})
	if err != nil || checkAct == nil {
		t.Fatalf("ReplyWithMeta check failed: %v", err)
	}
	dlg2 := &model.Action{
		ID:        "a_dlg_prio",
		Kind:      model.ActionKindWorkerDialog,
		Thread:    model.MainThread,
		Title:     "高优终端确认",
		Choices:   []model.ActionChoice{{Key: "1", Label: "同意"}},
		Status:    model.ActionPending,
		CreatedAt: time.Now(),
	}
	_ = st.InsertAction(ctx, dlg2)
	pending := eng.ListPendingDecisionsForThread(ctx, model.MainThread)
	if len(pending) == 0 || pending[0].ID != dlg2.ID {
		t.Fatalf("expected worker_dialog ranked first, got %+v", pending)
	}
	for _, a := range pending {
		if a.Kind == model.ActionKindRouterCheck {
			t.Fatalf("router_check %s must never appear in ListPendingDecisionsForThread", a.ID)
		}
	}
}

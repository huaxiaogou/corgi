package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/store"
)

const (
	configOpDeleteProject         = "delete_project"
	configOpRemoveProjectPath     = "remove_project_path"
	configOpDeletePolicy          = "delete_policy"
	configOpDisableAskPermissions = "disable_ask_permissions"
	configOpTriggerUpdate         = "trigger_update"
)

type deleteProjectPayload struct {
	Project string `json:"project"`
}

type removeProjectPathPayload struct {
	Project string `json:"project"`
	Machine string `json:"machine"`
}

type deletePolicyPayload struct {
	Source string `json:"source"`
}

// sendConfigConfirmCard persists an L2 configuration confirmation card + proposal in a single
// SQLite transaction under Strategy 2B inflight protection, then publishes to Web/WeCom and ends the turn.
func (e *Engine) sendConfigConfirmCard(ctx context.Context, turn *apiRouterTurn, trig *model.Trigger, beforeHash, op, title, summary string, payload any) (string, bool) {
	if e.store == nil {
		return toolErr("store unavailable"), false
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return toolErr("序列化确认提案失败：" + err.Error()), false
	}
	if strings.TrimSpace(beforeHash) == "" {
		_, _, beforeHash = e.configSnapshot()
	}

	// Strategy 2B: fail-closed inflight guard before creating/pushing an irreversible confirmation card.
	if err := e.recordAPIRouterInflight(context.Background(), trig, "reply_user", ""); err != nil {
		return toolErr("记录防重标记失败，已中止发送确认卡：" + err.Error()), false
	}

	eid, eseq, err := e.store.NextID(ctx, "e")
	if err != nil {
		return toolErr("分配消息编号失败：" + err.Error()), false
	}
	aid, _, err := e.store.NextID(ctx, "a")
	if err != nil {
		return toolErr("分配确认卡编号失败：" + err.Error()), false
	}

	now := time.Now()
	var quote *model.Quote
	if trig != nil && trig.Ref != "" {
		if prev, qErr := e.store.GetEntry(ctx, trig.Ref); qErr == nil && prev != nil {
			who := "你"
			if prev.Role != "user" {
				who = prev.Role
			}
			quote = &model.Quote{Entry: prev.ID, Who: who, Text: oneLineClip(prev.Text, 120)}
		}
	}
	choices := []model.ActionChoice{
		{Key: "1", Label: "确认执行"},
		{Key: "2", Label: "取消", Recommended: true},
	}
	bodyText := fmt.Sprintf("%s\n\n点击下方按钮确认后立即生效（15 分钟内有效，如期间配置被修改将自动作废）。", summary)
	entry := &model.Entry{
		ID: eid, Seq: eseq, Role: "corgi", Thread: model.MainThread, Channel: "corgi",
		Text: bodyText, At: now, Re: quote, ActionID: aid, Options: []string{"确认执行", "取消"},
	}
	action := &model.Action{
		ID: aid, Kind: model.ActionKindConfigConfirm, Thread: model.MainThread, EntryID: eid,
		Category: "config", Mode: model.ActionModeSingle, Title: oneLineClip(title, 64),
		Summary: bodyText, Choices: choices, Status: model.ActionPending, CreatedAt: now,
	}
	proposal := model.ConfigProposal{
		ActionID: aid, Op: op, Summary: summary, BeforeHash: beforeHash,
		Payload: payloadBytes, CreatedAt: now, ExpiresAt: now.Add(configProposalTTL),
	}
	propJSON, err := json.Marshal(proposal)
	if err != nil {
		return toolErr("编码确认提案失败：" + err.Error()), false
	}
	if err := e.store.CommitConfigProposalTx(ctx, entry, action, configProposalKVPrefix+aid, string(propJSON)); err != nil {
		return toolErr("写入确认卡事务失败：" + err.Error()), false
	}

	_ = e.recordAPIRouterInflight(context.Background(), trig, "reply_user", entry.ID)
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnStateChanged()
	_ = sink.OnReply(ctx, entry, action, nil)
	sink.OnStateChanged()

	turn.markMutated("已下发配置确认卡 " + aid)
	turn.flags.Or(turnFlagReplied)
	e.ackAPITriggerOnce(context.Background(), turn, trig)
	return toolJSON(map[string]any{
		"ok":              true,
		"pending_confirm": true,
		"action":          aid,
		"entry":           eid,
		"op":              op,
		"summary":         summary,
		"expires_in":      "15m",
	}), true
}

// resolveConfigConfirmAction atomically settles an L2 config_confirm card (confirm or cancel)
// under the single config writer, never waking a second LLM turn.
func (e *Engine) resolveConfigConfirmAction(ctx context.Context, actionID, resolvedBy, choiceInput string, custom bool) (*model.Action, error) {
	if custom {
		return nil, fmt.Errorf("配置确认卡不支持自由文本输入，请点击确认或取消按钮")
	}
	if !isHumanChannel(resolvedBy) {
		return nil, fmt.Errorf("配置确认卡仅限人类渠道（Web / 企微 / CLI）确认，已拒绝来自 %q 的操作", resolvedBy)
	}
	trimmed := strings.TrimSpace(choiceInput)
	if trimmed == "" {
		return nil, fmt.Errorf("empty action choice")
	}

	var (
		resolved            *model.Action
		receiptText         string
		receiptErr          bool
		appliedSt           *model.Settings
		triggerUpdateTarget string
	)
	err := e.withConfigWriteLock(ctx, func() error {
		act, err := e.store.GetAction(ctx, actionID)
		if err != nil {
			return err
		}
		if act.Status != model.ActionPending {
			return store.ErrActionAlreadyResolved
		}
		chosenKey, chosenLabel, _, ok := matchStrictActionChoice(act, trimmed)
		if !ok || (chosenKey != "1" && chosenKey != "2") {
			return fmt.Errorf("invalid choice %q for config_confirm action %s", trimmed, act.ID)
		}
		propKey := configProposalKVPrefix + actionID

		if chosenKey == "2" {
			settled, err := e.store.CommitConfigConfirmTx(ctx, store.ConfigConfirmTxRequest{
				ActionID:    actionID,
				ProposalKey: propKey,
				NextStatus:  model.ActionResolved,
				ResolvedBy:  resolvedBy,
				ChosenKey:   "2",
				ChosenLabel: chosenLabel,
			})
			if err != nil {
				return err
			}
			resolved = settled
			receiptText = fmt.Sprintf("已取消配置变更：%s（via %s）", oneLineClip(act.Title, 80), resolvedBy)
			return nil
		}

		// chosenKey == "1": verify proposal presence, TTL, and BeforeHash optimistic lock.
		propRaw, propFound, err := e.store.GetKV(ctx, propKey)
		if err != nil {
			return fmt.Errorf("读取配置确认提案失败：%w", err)
		}
		var prop model.ConfigProposal
		invalidReason := ""
		if !propFound || strings.TrimSpace(propRaw) == "" || json.Unmarshal([]byte(propRaw), &prop) != nil || prop.ActionID != actionID {
			invalidReason = "配置确认提案已失效或不存在，未执行修改"
		} else if time.Now().After(prop.ExpiresAt) {
			invalidReason = "配置确认提案已超过 15 分钟有效期，未执行修改（请重新发起）"
		} else if ConfigSnapshotHash(e.Settings(), e.Policies()) != prop.BeforeHash {
			invalidReason = "配置在确认期间已发生变化，为防覆盖已中止执行（请重新发起）"
		}
		if invalidReason != "" {
			settled, txErr := e.store.CommitConfigConfirmTx(ctx, store.ConfigConfirmTxRequest{
				ActionID:      actionID,
				ProposalKey:   propKey,
				NextStatus:    model.ActionExpired,
				ResolvedBy:    resolvedBy,
				ChosenKey:     "1",
				ChosenLabel:   chosenLabel,
				ExpiredReason: invalidReason,
			})
			if txErr != nil {
				return txErr
			}
			resolved = settled
			receiptText = invalidReason
			receiptErr = true
			return nil
		}

		// Compute resulting Settings/Policies and atomic KV updates.
		prevSt, prevPols := e.Settings(), e.Policies()
		nextSt, nextPols := CloneSettings(prevSt), ClonePolicies(prevPols)
		stChanged, polsChanged, changeSummary, updateTarget, applyErr := applyProposalMutation(&nextSt, &nextPols, prop)
		if applyErr != nil {
			settled, txErr := e.store.CommitConfigConfirmTx(ctx, store.ConfigConfirmTxRequest{
				ActionID: actionID, ProposalKey: propKey, NextStatus: model.ActionExpired,
				ResolvedBy: resolvedBy, ChosenKey: "1", ChosenLabel: chosenLabel,
				ExpiredReason: "无法执行配置变更：" + applyErr.Error(),
			})
			if txErr != nil {
				return txErr
			}
			resolved, receiptText, receiptErr = settled, "未执行配置变更："+applyErr.Error(), true
			return nil
		}

		kvUpdates := map[string]string{}
		if stChanged {
			normSt, _, nErr := e.normalizeSettingsForSave(prevSt, nextSt, true)
			if nErr != nil {
				return nErr
			}
			nextSt = normSt
			b, mErr := json.Marshal(nextSt)
			if mErr != nil {
				return fmt.Errorf("encode settings: %w", mErr)
			}
			kvUpdates["settings"] = string(b)
		}
		if polsChanged {
			nextPols = sanitizePoliciesForSave(nextPols)
			b, mErr := json.Marshal(nextPols)
			if mErr != nil {
				return fmt.Errorf("encode policies: %w", mErr)
			}
			kvUpdates["policies"] = string(b)
		}
		auditVal, aErr := e.buildAuditRingValue(ctx, resolvedBy, prop.Op, changeSummary)
		if aErr != nil {
			return aErr
		}
		kvUpdates[configAuditRingKVKey] = auditVal

		settled, txErr := e.store.CommitConfigConfirmTx(ctx, store.ConfigConfirmTxRequest{
			ActionID: actionID, ProposalKey: propKey, NextStatus: model.ActionResolved,
			ResolvedBy: resolvedBy, ChosenKey: "1", ChosenLabel: chosenLabel, KVUpdates: kvUpdates,
		})
		if txErr != nil {
			return txErr
		}

		e.mu.Lock()
		if stChanged {
			if e.settings.AskPermissions && !nextSt.AskPermissions {
				clear(e.bypassCheckedAt)
				clear(e.bypassHealedPID)
				clear(e.bypassHealFailAt)
			}
			e.settings = CloneSettings(nextSt)
			cp := CloneSettings(nextSt)
			appliedSt = &cp
		}
		if polsChanged {
			e.policies = ClonePolicies(nextPols)
		}
		onApplied := e.onSettingsApplied
		e.mu.Unlock()

		if appliedSt != nil && onApplied != nil {
			onApplied(CloneSettings(*appliedSt))
		}
		if e.logger != nil {
			e.logger.Info("config confirm applied", "action", actionID, "op", prop.Op, "by", resolvedBy, "summary", changeSummary)
		}
		resolved = settled
		triggerUpdateTarget = updateTarget
		receiptText = fmt.Sprintf("已确认并执行配置变更：%s（via %s）", changeSummary, resolvedBy)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if triggerUpdateTarget != "" {
		receiptText, receiptErr = e.executeConfirmedFleetUpdate(ctx, triggerUpdateTarget, resolvedBy)
	}
	e.postConfigConfirmReceipt(ctx, resolved, receiptText, receiptErr)
	return resolved, nil
}

func applyProposalMutation(st *model.Settings, pols *[]model.Policy, prop model.ConfigProposal) (stChanged, polsChanged bool, summary, updateTarget string, err error) {
	switch prop.Op {
	case configOpDeleteProject:
		var p deleteProjectPayload
		if err := json.Unmarshal(prop.Payload, &p); err != nil {
			return false, false, "", "", fmt.Errorf("无效的删除项目提案：%w", err)
		}
		s, err := applyDeleteProject(st, p.Project)
		return err == nil, false, s, "", err
	case configOpRemoveProjectPath:
		var p removeProjectPathPayload
		if err := json.Unmarshal(prop.Payload, &p); err != nil {
			return false, false, "", "", fmt.Errorf("无效的移除路径提案：%w", err)
		}
		s, err := applyRemoveProjectPath(st, p.Project, p.Machine)
		return err == nil, false, s, "", err
	case configOpDeletePolicy:
		var p deletePolicyPayload
		if err := json.Unmarshal(prop.Payload, &p); err != nil {
			return false, false, "", "", fmt.Errorf("无效的删除策略提案：%w", err)
		}
		s, err := applyDeletePolicy(pols, p.Source)
		return false, err == nil, s, "", err
	case configOpDisableAskPermissions:
		st.AskPermissions = false
		return true, false, "关闭全站 Agent 自动执行权限确认（ask_permissions=false）", "", nil
	case configOpTriggerUpdate:
		var p triggerUpdatePayload
		if err := json.Unmarshal(prop.Payload, &p); err != nil || strings.TrimSpace(p.TargetVersion) == "" {
			return false, false, "", "", fmt.Errorf("无效的升级提案：缺少目标版本号")
		}
		t := strings.TrimSpace(p.TargetVersion)
		return false, false, "确认触发一键升级至 " + t, t, nil
	default:
		return false, false, "", "", fmt.Errorf("不支持的配置提案操作：%s", prop.Op)
	}
}

func (e *Engine) dismissConfigConfirmAction(ctx context.Context, actionID, channel string) (*model.Action, error) {
	if !isHumanChannel(channel) {
		return nil, fmt.Errorf("配置确认卡仅限人类渠道忽略，已拒绝来自 %q 的操作", channel)
	}
	var settled *model.Action
	err := e.withConfigWriteLock(ctx, func() error {
		act, txErr := e.store.CommitConfigConfirmTx(ctx, store.ConfigConfirmTxRequest{
			ActionID:        actionID,
			ProposalKey:     configProposalKVPrefix + actionID,
			NextStatus:      model.ActionExpired,
			ResolvedBy:      "dismiss",
			ResolvedChannel: channel,
			ExpiredReason:   "已忽略",
		})
		if txErr != nil {
			return txErr
		}
		settled = act
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, settled)
	sink.OnStateChanged()
	return settled, nil
}

func (e *Engine) postConfigConfirmReceipt(ctx context.Context, act *model.Action, msg string, isErr bool) {
	if act == nil {
		return
	}
	tone := condStr(isErr, "error", "info")
	e.addEventWithAction(ctx, act.Thread, msg, "", tone, act.ID)
	_, _ = e.ReplyDetailed(ctx, ReplyRequest{
		Thread: act.Thread,
		Text:   msg,
		ReID:   act.EntryID,
	})
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	sink.OnActionResolved(ctx, act)
	sink.OnStateChanged()
}

func (e *Engine) expireStaleConfigConfirms(ctx context.Context) int {
	if e.store == nil {
		return 0
	}
	n, err := e.store.ExpirePendingConfigConfirms(ctx, time.Now().Add(-configProposalTTL), configProposalKVPrefix, "配置确认提案已过期")
	if err != nil {
		return 0
	}
	return n
}

package core

import (
	"context"
	"errors"
	"strings"
	"time"
)

type llmToolLoopConfig struct {
	ClientCfg          LLMClientConfig
	Model              string
	Effort             string
	CacheKey           string
	Tools              []LLMToolDef
	Messages           []LLMMessage
	MaxSteps           int
	MaxToolCalls       int
	MaxToolOutputRunes int
	StepDeadline       time.Duration
	StepRetryBaseDelay time.Duration
	MaxStepRetries     func(step int) int
	LogLabel           string
	LogIDKey           string
	LogIDVal           string

	MaxBareTextRetries    int
	BareTextForceTools    bool
	BareTextKeepAssistant bool
	BareTextNudge         func(step int) string
	FinalStepNudge        func() (text string, forceTools bool)

	IsClosingTool         func(name string) bool
	BlockClosingOnStepErr bool
	ToolBudgetErrMsg      string
	StepErrClosingMsg     string

	OnStepStart func(step int)
	OnToolStart func(step int, tc LLMToolCall)
	ExecTool    func(step int, tc LLMToolCall) (out string, earlyExit bool)
}

type llmToolLoopResult struct {
	StepsRun   int
	TotalCalls int
	EarlyExit  bool
	Exhausted  bool
	NoToolExit bool
	FailedStep int
	LastUsage  LLMUsage
	Err        error
}

func isRetryableLLMStepError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrLLMAuth) || errors.Is(err, ErrLLMBadRequest) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

func (e *Engine) callLLMStepWithRetry(ctx context.Context, cfg llmToolLoopConfig, step int, req LLMChatRequest, toolChoiceOK *bool) (*LLMChatResponse, error) {
	baseDelay := cfg.StepRetryBaseDelay
	if baseDelay <= 0 {
		baseDelay = 1200 * time.Millisecond
	}
	maxStepRetries := 0
	if cfg.MaxStepRetries != nil {
		maxStepRetries = max(cfg.MaxStepRetries(step), 0)
	}
	var lastErr error
	for attempt := 0; attempt <= maxStepRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		stepCtx := ctx
		var stepCancel context.CancelFunc
		if cfg.StepDeadline > 0 {
			stepCtx, stepCancel = context.WithTimeout(ctx, cfg.StepDeadline)
		}
		resp, callErr := e.llm.ChatCompletion(stepCtx, cfg.ClientCfg, req)
		if callErr != nil && req.ToolChoice != nil && errors.Is(callErr, ErrLLMBadRequest) {
			*toolChoiceOK, req.ToolChoice = false, nil
			if e.logger != nil {
				e.logger.Warn(cfg.LogLabel+" provider rejected tool_choice=required, retrying without tool_choice",
					cfg.LogIDKey, cfg.LogIDVal, "model", cfg.Model, "err", callErr)
			}
			resp, callErr = e.llm.ChatCompletion(stepCtx, cfg.ClientCfg, req)
		}
		if stepCancel != nil {
			stepCancel()
		}
		if callErr == nil {
			return resp, nil
		}
		lastErr = callErr
		if attempt >= maxStepRetries || !isRetryableLLMStepError(ctx, callErr) {
			return nil, callErr
		}
		delay := baseDelay * time.Duration(1<<attempt)
		if e.logger != nil {
			e.logger.Warn(cfg.LogLabel+" transient llm error; retrying step",
				cfg.LogIDKey, cfg.LogIDVal, "attempt", attempt+1, "delay", delay, "err", callErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// runLLMToolLoop drives the shared OpenAI-compatible multi-step tool calling loop
// for both the in-process native API Router and async API Dream, preserving SSE streaming,
// DeepSeek reasoning_content passthrough, tool_choice fallback, tool call budgets, and nudges.
func (e *Engine) runLLMToolLoop(ctx context.Context, cfg llmToolLoopConfig) llmToolLoopResult {
	messages := append([]LLMMessage(nil), cfg.Messages...)
	totalCalls, bareRetries, nudgeIdx := 0, 0, -1
	forceTools, toolChoiceOK, exhausted := false, true, false
	var lastUsage LLMUsage
	outLimit := cfg.MaxToolOutputRunes
	if outLimit <= 0 {
		outLimit = maxAPIToolOutputRunes
	}

	for step := 0; step < cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			return llmToolLoopResult{StepsRun: step, TotalCalls: totalCalls, FailedStep: step, LastUsage: lastUsage, Err: ctx.Err()}
		}
		if cfg.OnStepStart != nil {
			cfg.OnStepStart(step)
		}

		req := LLMChatRequest{
			Model:           cfg.Model,
			Messages:        messages,
			Tools:           cfg.Tools,
			ReasoningEffort: cfg.Effort,
			PromptCacheKey:  cfg.CacheKey,
			Stream:          true,
			StreamOptions:   &LLMStreamOptions{IncludeUsage: true},
		}
		if forceTools && toolChoiceOK {
			req.ToolChoice = "required"
		}

		resp, callErr := e.callLLMStepWithRetry(ctx, cfg, step, req, &toolChoiceOK)
		if callErr != nil {
			return llmToolLoopResult{StepsRun: step, TotalCalls: totalCalls, FailedStep: step + 1, LastUsage: lastUsage, Err: callErr}
		}
		if resp.Usage.PromptTokens > 0 || resp.Usage.TotalTokens > 0 {
			lastUsage = resp.Usage
		}

		choice := resp.Choices[0].Message
		e.logLLMStep(cfg.LogLabel, cfg.LogIDKey, cfg.LogIDVal, step+1, cfg.Model, cfg.Effort, resp, choice.ToolCalls)

		if len(choice.ToolCalls) == 0 {
			if bareRetries < cfg.MaxBareTextRetries && step < cfg.MaxSteps-1 && cfg.BareTextNudge != nil {
				if nudge := cfg.BareTextNudge(step); nudge != "" {
					bareRetries++
					if cfg.BareTextForceTools {
						forceTools = true
					}
					if cfg.BareTextKeepAssistant {
						messages = append(messages,
							LLMMessage{Role: "assistant", Content: strings.TrimSpace(choice.Content), ReasoningContent: choice.ReasoningContent},
							LLMMessage{Role: "user", Content: nudge},
						)
					} else {
						if nudgeIdx < 0 || nudgeIdx != len(messages)-1 {
							messages, nudgeIdx = append(messages, LLMMessage{Role: "user"}), len(messages)
						}
						messages[nudgeIdx].Content = nudge
					}
					continue
				}
			}
			return llmToolLoopResult{StepsRun: step + 1, TotalCalls: totalCalls, NoToolExit: true, LastUsage: lastUsage}
		}

		forceTools, nudgeIdx = false, -1
		messages = append(messages, LLMMessage{
			Role:             "assistant",
			Content:          choice.Content,
			ReasoningContent: choice.ReasoningContent,
			ToolCalls:        choice.ToolCalls,
		})

		earlyExit, stepHadErr := false, false
		for _, tc := range choice.ToolCalls {
			totalCalls++
			if cfg.OnToolStart != nil {
				cfg.OnToolStart(step, tc)
			}
			isClosing := cfg.IsClosingTool != nil && cfg.IsClosingTool(tc.Function.Name)
			var toolOut string
			switch {
			case cfg.MaxToolCalls > 0 && totalCalls > cfg.MaxToolCalls && !isClosing:
				toolOut, stepHadErr = toolErr(cfg.ToolBudgetErrMsg), true
			case cfg.BlockClosingOnStepErr && isClosing && stepHadErr:
				toolOut = toolErr(cfg.StepErrClosingMsg)
			default:
				if cfg.ExecTool != nil {
					toolOut, earlyExit = cfg.ExecTool(step, tc)
				}
				if strings.Contains(toolOut, `"ok":false`) {
					stepHadErr = true
					if e.logger != nil {
						e.logger.Warn(cfg.LogLabel+" tool rejected",
							cfg.LogIDKey, cfg.LogIDVal, "step", step+1, "tool", tc.Function.Name, "result", oneLineClip(toolOut, 240))
					}
				}
			}
			messages = append(messages, LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    clipRunes(toolOut, outLimit, "...[truncated]"),
			})
			if earlyExit || ctx.Err() != nil {
				break
			}
		}
		if earlyExit {
			return llmToolLoopResult{StepsRun: step + 1, TotalCalls: totalCalls, EarlyExit: true, LastUsage: lastUsage}
		}
		if step == cfg.MaxSteps-2 && cfg.FinalStepNudge != nil && ctx.Err() == nil {
			if msg, force := cfg.FinalStepNudge(); msg != "" {
				messages = append(messages, LLMMessage{Role: "user", Content: msg})
				forceTools = force
			}
		}
		exhausted = step == cfg.MaxSteps-1
	}

	return llmToolLoopResult{StepsRun: cfg.MaxSteps, TotalCalls: totalCalls, Exhausted: exhausted, LastUsage: lastUsage}
}

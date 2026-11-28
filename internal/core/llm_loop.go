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
	Err        error
}

// runLLMToolLoop drives the shared OpenAI-compatible multi-step tool calling loop
// for both the in-process native API Router and async API Dream, preserving SSE streaming,
// DeepSeek reasoning_content passthrough, tool_choice fallback, tool call budgets, and nudges.
func (e *Engine) runLLMToolLoop(ctx context.Context, cfg llmToolLoopConfig) llmToolLoopResult {
	messages := append([]LLMMessage(nil), cfg.Messages...)
	totalCalls, bareRetries, nudgeIdx := 0, 0, -1
	forceTools, toolChoiceOK, exhausted := false, true, false
	outLimit := cfg.MaxToolOutputRunes
	if outLimit <= 0 {
		outLimit = maxAPIToolOutputRunes
	}

	for step := 0; step < cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			return llmToolLoopResult{StepsRun: step, TotalCalls: totalCalls, FailedStep: step, Err: ctx.Err()}
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

		stepCtx := ctx
		var stepCancel context.CancelFunc
		if cfg.StepDeadline > 0 {
			stepCtx, stepCancel = context.WithTimeout(ctx, cfg.StepDeadline)
		}
		resp, callErr := e.llm.ChatCompletion(stepCtx, cfg.ClientCfg, req)
		if callErr != nil && req.ToolChoice != nil && errors.Is(callErr, ErrLLMBadRequest) {
			toolChoiceOK, req.ToolChoice = false, nil
			if e.logger != nil {
				e.logger.Warn(cfg.LogLabel+" provider rejected tool_choice=required, retrying without tool_choice",
					cfg.LogIDKey, cfg.LogIDVal, "model", cfg.Model, "err", callErr)
			}
			resp, callErr = e.llm.ChatCompletion(stepCtx, cfg.ClientCfg, req)
		}
		if stepCancel != nil {
			stepCancel()
		}
		if callErr != nil {
			return llmToolLoopResult{StepsRun: step, TotalCalls: totalCalls, FailedStep: step + 1, Err: callErr}
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
			return llmToolLoopResult{StepsRun: step + 1, TotalCalls: totalCalls, NoToolExit: true}
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
			return llmToolLoopResult{StepsRun: step + 1, TotalCalls: totalCalls, EarlyExit: true}
		}
		if step == cfg.MaxSteps-2 && cfg.FinalStepNudge != nil && ctx.Err() == nil {
			if msg, force := cfg.FinalStepNudge(); msg != "" {
				messages = append(messages, LLMMessage{Role: "user", Content: msg})
				forceTools = force
			}
		}
		exhausted = step == cfg.MaxSteps-1
	}

	return llmToolLoopResult{StepsRun: cfg.MaxSteps, TotalCalls: totalCalls, Exhausted: exhausted}
}

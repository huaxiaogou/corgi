package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAgyToolMessageDoubleEncodedAndStructured(t *testing.T) {
	t.Parallel()

	// 1. Double-encoded JSON string for invoke_subagent (model=="inherit" must be omitted).
	innerSub, _ := json.Marshal(map[string]any{
		"TypeName": "research",
		"Role":     "Codebase Researcher",
		"Prompt":   "Inspect all LocateTranscript call sites",
		"Model":    "inherit",
	})
	doubleEncodedSub, _ := json.Marshal(string(innerSub))
	subMsg := buildAgyToolMessage("invoke_subagent", doubleEncodedSub)
	if subMsg.Subagent == nil {
		t.Fatalf("expected Subagent metadata, got %+v", subMsg)
	}
	if subMsg.Subagent.Type != "research" || subMsg.Subagent.Role != "Codebase Researcher" || subMsg.Subagent.Model != "" {
		t.Fatalf("unexpected Subagent fields: %+v", subMsg.Subagent)
	}
	if !strings.Contains(subMsg.Subagent.Prompt, "LocateTranscript") || subMsg.Input != "Codebase Researcher" {
		t.Fatalf("unexpected subagent prompt/input: %+v", subMsg)
	}

	// 2. swarm action=="add" populates SubagentCall with explicit model.
	swarmRaw := json.RawMessage(`{"action":"add","name":"worker-h2","brief":"Implement static gzip cache","model":"pro"}`)
	swarmMsg := buildAgyToolMessage("swarm", swarmRaw)
	if swarmMsg.Subagent == nil || swarmMsg.Subagent.Type != "swarm" || swarmMsg.Subagent.Role != "worker-h2" || swarmMsg.Subagent.Model != "pro" {
		t.Fatalf("unexpected swarm add subagent: %+v", swarmMsg)
	}

	// 3. update_steps builds progress summary and checklist detail.
	stepsRaw := json.RawMessage(`{"steps":[{"title":"Write tests","status":"done"},{"title":"Run race","status":"in_progress"},{"title":"Commit","status":"pending"}]}`)
	stepsMsg := buildAgyToolMessage("update_steps", stepsRaw)
	if !strings.Contains(stepsMsg.Input, "1/3") || !strings.Contains(stepsMsg.Input, "Run race") {
		t.Fatalf("unexpected update_steps input: %q", stepsMsg.Input)
	}
	if !strings.Contains(stepsMsg.Detail, "✓ Write tests") || !strings.Contains(stepsMsg.Detail, "▶ Run race") {
		t.Fatalf("unexpected update_steps detail: %q", stepsMsg.Detail)
	}

	// 4. Generic double-encoded tool call produces readable key-value Detail.
	innerCmd, _ := json.Marshal(map[string]any{
		"CommandLine": "go test -race ./...",
		"WaitMs":      5000,
	})
	doubleCmd, _ := json.Marshal(string(innerCmd))
	cmdMsg := buildAgyToolMessage("run_command", doubleCmd)
	if cmdMsg.Input != "go test -race ./..." || !strings.Contains(cmdMsg.Detail, "CommandLine: go test -race ./...") || !strings.Contains(cmdMsg.Detail, "WaitMs: 5000") {
		t.Fatalf("unexpected generic tool msg: %+v", cmdMsg)
	}

	// 5. AgyAdapter.ParseTranscriptLine preserves GENERIC Output & Error semantics.
	ad := &AgyAdapter{}
	plannerLine := `{"type":"PLANNER_RESPONSE","content":"Delegating","tool_calls":[{"name":"invoke_subagent","args":` + string(doubleEncodedSub) + `}]}`
	msgs, ok := ad.ParseTranscriptLine([]byte(plannerLine), "")
	if !ok || len(msgs) != 2 || msgs[1].Subagent == nil || msgs[1].Subagent.Role != "Codebase Researcher" {
		t.Fatalf("PLANNER_RESPONSE parse unexpected: ok=%v msgs=%+v", ok, msgs)
	}
	errLine := `{"type":"GENERIC","content":"Created At: 2026-11-28\nCommand exited with code 2\nboom"}`
	errMsgs, ok := ad.ParseTranscriptLine([]byte(errLine), "")
	if !ok || len(errMsgs) != 1 || !errMsgs[0].Error || !strings.Contains(errMsgs[0].Output, "boom") {
		t.Fatalf("GENERIC error parse broken: ok=%v msgs=%+v", ok, errMsgs)
	}
}

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/gitwt"
)

// VerifyWorkerRequest runs stateless build/test checks (and optional read-only review)
// in a worker's workspace on its host machine.
type VerifyWorkerRequest struct {
	Worker      string `json:"worker"`
	Cmd         string `json:"cmd,omitempty"`
	Review      bool   `json:"review,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	Send        bool   `json:"send,omitempty"`
	BaseCommit  string `json:"base_commit,omitempty"`
	SkipIfClean bool   `json:"skip_if_clean,omitempty"`
}

// VerifyWorkerResponse augments gitwt.VerifyResult with worker routing and optional auto-bounce info.
type VerifyWorkerResponse struct {
	gitwt.VerifyResult
	Worker       string `json:"worker"`
	Machine      string `json:"machine,omitempty"`
	SentToWorker bool   `json:"sent_to_worker,omitempty"`
	TriggerID    string `json:"trigger_id,omitempty"`
}

// VerifyWorker executes repo checks + optional read-only review in the worker's workspace.
// If req.Send is true and verification fails (check failure or blocking review), it sends
// the concise failure/review findings directly back to the worker for automatic fixing.
func (e *Engine) VerifyWorker(ctx context.Context, req VerifyWorkerRequest) (*VerifyWorkerResponse, error) {
	name := strings.TrimSpace(req.Worker)
	if name == "" {
		return nil, fmt.Errorf("worker name is required")
	}

	var machine, dir, brief, thread, workItemID string
	if w, err := e.store.GetWorker(ctx, name); err == nil && w != nil {
		machine = w.Machine
		dir = strings.TrimSpace(w.WorktreePath)
		if dir == "" {
			dir = strings.TrimSpace(w.Cwd)
		}
		brief = strings.TrimSpace(w.Task)
		thread = w.Thread
		workItemID = w.WorkItemID
	} else if e.fleet != nil {
		// Fallback: allow verifying a manually-opened live agent by name.
		if ags, aErr := e.fleet.Agents(ctx); aErr == nil {
			for _, a := range ags {
				if a.Name != nil && *a.Name == name {
					_, machine = fleet.SplitID(a.PaneID)
					if a.Cwd != nil {
						dir = strings.TrimSpace(*a.Cwd)
					}
					break
				}
			}
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("找不到 worker %q 或其工作目录为空", name)
	}

	vReq := gitwt.VerifyRequest{
		Dir:         dir,
		Cmd:         strings.TrimSpace(req.Cmd),
		Review:      req.Review,
		Reviewer:    strings.TrimSpace(req.Reviewer),
		Brief:       brief,
		BaseCommit:  strings.TrimSpace(req.BaseCommit),
		SkipIfClean: req.SkipIfClean,
	}

	var (
		res gitwt.VerifyResult
		err error
	)
	if machine == "" {
		res, err = gitwt.Verify(ctx, e.home, vReq)
	} else {
		res, err = e.verifyOnRemoteMachine(ctx, machine, vReq)
	}
	if err != nil {
		return nil, err
	}

	out := &VerifyWorkerResponse{
		VerifyResult: res,
		Worker:       name,
		Machine:      machine,
	}

	if req.Send && !res.OK {
		bounce := formatVerifyBouncePrompt(res)
		trig, _, sErr := e.SendToWorkerWithWorkTitle(ctx, name, thread, bounce, workItemID, "")
		if sErr != nil {
			return out, fmt.Errorf("验证未通过且打回 worker %s 失败: %w", name, sErr)
		}
		out.SentToWorker = true
		if trig != nil {
			out.TriggerID = trig.ID
		}
	}
	return out, nil
}

func (e *Engine) verifyOnRemoteMachine(ctx context.Context, machine string, req gitwt.VerifyRequest) (gitwt.VerifyResult, error) {
	if e.fleet == nil {
		return gitwt.VerifyResult{}, fmt.Errorf("fleet unavailable")
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return gitwt.VerifyResult{}, err
	}
	vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return gitwt.VerifyResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(vctx, http.MethodPost, "http://node/api/git/verify", bytes.NewReader(body))
	if err != nil {
		return gitwt.VerifyResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(httpReq)
	if err != nil {
		return gitwt.VerifyResult{}, fmt.Errorf("remote verify on %s failed: %w", machine, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var er struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &er) == nil && er.Error != "" {
			return gitwt.VerifyResult{}, fmt.Errorf("%s", er.Error)
		}
		return gitwt.VerifyResult{}, fmt.Errorf("remote verify HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out gitwt.VerifyResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return gitwt.VerifyResult{}, fmt.Errorf("decode remote verify response: %w", err)
	}
	return out, nil
}

func formatVerifyBouncePrompt(res gitwt.VerifyResult) string {
	var b strings.Builder
	b.WriteString("自动验证（corgi verify）未通过，请根据以下结果修复问题、重新跑通验证并提交更新：\n\n")
	if !res.ChecksPassed {
		b.WriteString("### 构建/测试失败\n")
		for _, c := range res.Checks {
			if c.Passed {
				continue
			}
			fmt.Fprintf(&b, "- `%s` 失败：\n```\n%s\n```\n", c.Command, strings.TrimSpace(c.Output))
		}
	}
	if res.ReviewRun && !res.ReviewPassed {
		fmt.Fprintf(&b, "\n### 只读审查意见（%s）\n%s\n", res.Reviewer, strings.TrimSpace(res.ReviewSummary))
	}
	return strings.TrimSpace(b.String())
}

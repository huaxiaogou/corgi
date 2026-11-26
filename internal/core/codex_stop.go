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

	"github.com/kiperjing/corgi/internal/runtime/codex"
)

// stopCodexTurn interrupts an in-progress Codex turn in the shared Codex daemon on the
// worker's computer before its terminal workspace is closed, preventing background file mutations.
func (e *Engine) stopCodexTurn(ctx context.Context, machine, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if strings.TrimSpace(machine) == "" {
		return codex.InterruptTurn(tctx, e.home, sessionID)
	}
	if e.fleet == nil {
		return nil
	}
	hc, err := e.fleet.HTTPClient(machine)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"thread": sessionID})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/codex/interrupt", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("codex interrupt: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestInterruptTurnAtSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "codex.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		mu          sync.Mutex
		interrupted []string
	)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close(websocket.StatusNormalClosure, "")
			for {
				_, data, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				var req struct {
					ID     *int64         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(data, &req) != nil || req.ID == nil {
					continue
				}
				var res any
				switch req.Method {
				case "initialize":
					res = map[string]any{"ok": true}
				case "thread/loaded/list":
					res = map[string]any{"data": []string{"th-active", "th-idle"}}
				case "thread/read":
					tid, _ := req.Params["threadId"].(string)
					if tid == "th-active" {
						res = map[string]any{"thread": map[string]any{"turns": []map[string]any{
							{"id": "turn-1", "status": "completed"},
							{"id": "turn-2", "status": "inProgress"},
						}}}
					} else {
						res = map[string]any{"thread": map[string]any{"turns": []map[string]any{
							{"id": "turn-9", "status": "completed"},
						}}}
					}
				case "turn/interrupt":
					mu.Lock()
					interrupted = append(interrupted, req.Params["threadId"].(string)+":"+req.Params["turnId"].(string))
					mu.Unlock()
					res = map[string]any{"ok": true}
				}
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": res})
				_ = ws.Write(r.Context(), websocket.MessageText, out)
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Active thread with inProgress turn -> sends turn/interrupt.
	if err := InterruptTurnAtSocket(ctx, sock, "th-active"); err != nil {
		t.Fatalf("InterruptTurnAtSocket(th-active): %v", err)
	}
	// 2. Loaded thread with only completed turns -> no interrupt sent.
	if err := InterruptTurnAtSocket(ctx, sock, "th-idle"); err != nil {
		t.Fatalf("InterruptTurnAtSocket(th-idle): %v", err)
	}
	// 3. Unloaded thread -> no error, no interrupt.
	if err := InterruptTurnAtSocket(ctx, sock, "th-unknown"); err != nil {
		t.Fatalf("InterruptTurnAtSocket(th-unknown): %v", err)
	}
	// 4. Empty socket path -> nil no-op.
	if err := InterruptTurnAtSocket(ctx, "", "th-active"); err != nil {
		t.Fatalf("InterruptTurnAtSocket(empty): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(interrupted) != 1 || interrupted[0] != "th-active:turn-2" {
		t.Fatalf("unexpected interrupted turns: %v", interrupted)
	}
}

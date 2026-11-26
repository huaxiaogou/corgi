package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// DaemonSocket returns the Codex shared app-server Unix socket path if present.
func DaemonSocket(home string) string {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	p := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// InterruptTurn cancels an in-progress turn for threadID inside the local Codex shared daemon.
// Returns nil when no shared daemon socket exists or the thread has no in-progress turn.
func InterruptTurn(ctx context.Context, home, threadID string) error {
	return InterruptTurnAtSocket(ctx, DaemonSocket(home), threadID)
}

// InterruptTurnAtSocket dials a specific Codex control socket and interrupts any inProgress turn for threadID.
func InterruptTurnAtSocket(ctx context.Context, sock, threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if sock == "" || threadID == "" {
		return nil
	}
	c, err := dialRPC(ctx, sock)
	if err != nil {
		return err
	}
	defer c.close()

	loaded, err := c.loadedThreads()
	if err != nil || !loaded[threadID] {
		return err
	}
	var read struct {
		Thread struct {
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := c.call("thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read); err != nil {
		return err
	}
	for _, t := range read.Thread.Turns {
		if t.Status == "inProgress" && t.ID != "" {
			if err := c.call("turn/interrupt", map[string]any{"threadId": threadID, "turnId": t.ID}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

type rpcClient struct {
	ws      *websocket.Conn
	ctx     context.Context
	mu      sync.Mutex
	next    int64
	pending map[int64]chan rpcReply
}

type rpcReply struct {
	Result json.RawMessage
	Err    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
}

func dialRPC(ctx context.Context, sock string) (*rpcClient, error) {
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(dctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(dctx, "unix", sock)
			},
		},
	}
	ws, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, fmt.Errorf("dial codex daemon: %w", err)
	}
	ws.SetReadLimit(64 << 20)
	c := &rpcClient{ws: ws, ctx: ctx, pending: map[int64]chan rpcReply{}}
	go c.readLoop()

	var initRes json.RawMessage
	err = c.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "corgi", "title": "Corgi", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initRes)
	if err != nil {
		c.close()
		return nil, err
	}
	_ = c.notify("initialized", map[string]any{})
	return c, nil
}

func (c *rpcClient) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *rpcClient) readLoop() {
	for {
		_, b, err := c.ws.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &msg) != nil || msg.ID == nil || msg.Method != "" {
			continue
		}
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{Result: msg.Result, Err: msg.Error}
		}
	}
}

func encodeJSONNoEscape(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (c *rpcClient) notify(method string, params any) error {
	return c.ws.Write(c.ctx, websocket.MessageText, encodeJSONNoEscape(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}))
}

func (c *rpcClient) call(method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan rpcReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.ws.Write(c.ctx, websocket.MessageText, encodeJSONNoEscape(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("codex daemon disconnected")
		}
		if r.Err != nil {
			return fmt.Errorf("codex rpc error: %s (%d)", r.Err.Message, r.Err.Code)
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-c.ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return c.ctx.Err()
	}
}

func (c *rpcClient) loadedThreads() (map[string]bool, error) {
	ids := map[string]bool{}
	cursor := ""
	for i := 0; i < 50; i++ {
		var page struct {
			Data       []string `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		p := map[string]any{"limit": 100}
		if cursor != "" {
			p["cursor"] = cursor
		}
		if err := c.call("thread/loaded/list", p, &page); err != nil {
			return nil, err
		}
		for _, id := range page.Data {
			ids[id] = true
		}
		if page.NextCursor == nil || *page.NextCursor == "" || *page.NextCursor == cursor {
			break
		}
		cursor = *page.NextCursor
	}
	return ids, nil
}

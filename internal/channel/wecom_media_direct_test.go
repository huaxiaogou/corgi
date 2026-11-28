package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kiperjing/corgi/internal/channel/wecom"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

type channelRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn channelRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func newChannelFakeHerdrClient(handler func(method string, params map[string]any) (any, *herdr.Error)) *herdr.Client {
	return herdr.NewClient(func(ctx context.Context) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			rd := bufio.NewReader(c2)
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			res, herr := handler(req.Method, req.Params)
			resp := map[string]any{"id": req.ID}
			if herr != nil {
				resp["error"] = herr
			} else if res != nil {
				resp["result"] = res
			} else {
				resp["result"] = map[string]any{}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})
}

func TestWeComFocusDirectPassesMediaRefsToCrossMachineStage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Local HTTP server simulating a WeCom attachment download URL.
	attSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("wecom-direct-attachment-bytes"))
	}))
	t.Cleanup(attSrv.Close)

	var (
		mu             sync.Mutex
		capturedPrompt string
		uploadCount    int
	)
	remoteHerdr := newChannelFakeHerdrClient(func(method string, params map[string]any) (any, *herdr.Error) {
		switch method {
		case "agent.list":
			return map[string]any{"agents": []map[string]any{{
				"name": "w-focus", "pane_id": "wf:p1", "workspace_id": "wf",
				"agent": "claude", "agent_status": "idle", "interactive_ready": true,
			}}}, nil
		case "agent.prompt":
			mu.Lock()
			if p, ok := params["text"].(string); ok {
				capturedPrompt = p
			}
			mu.Unlock()
			return map[string]any{"ok": true}, nil
		default:
			return map[string]any{}, nil
		}
	})
	fl := fleet.NewFleet(newChannelFakeHerdrClient(func(string, map[string]any) (any, *herdr.Error) { return map[string]any{}, nil }), "hub", dir, "test", []string{"claude"})
	fl.Attach(&fleet.RemoteMachine{
		Info:  model.Machine{ID: "mac-w", Name: "MacW", Online: true, Agents: []string{"claude"}},
		Herdr: remoteHerdr,
		HTTPClient: &http.Client{
			Transport: channelRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/uploads" && r.Method == http.MethodHead {
					return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if r.URL.Path == "/api/uploads" && r.Method == http.MethodPost {
					mu.Lock()
					uploadCount++
					mu.Unlock()
					if r.Body != nil {
						_, _ = io.Copy(io.Discard, r.Body)
					}
					dest := "/Users/w/.corgi/uploads/staged/20261128/" + r.URL.Query().Get("sha256") + "-" + r.URL.Query().Get("name")
					out, _ := json.Marshal(map[string]any{"path": dest})
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(out))}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}),
		},
	})

	eng, err := core.NewEngine(ctx, st, fl, agent.NewRegistry(), dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorker(ctx, &model.Worker{
		Name: "w-focus", Kind: "claude", Machine: "mac-w", Thread: model.MainThread, PaneID: "wf:p1@mac-w", Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SaveWeComFocus(ctx, "wecom_focus:U1", &core.WeComFocus{
		Worker: "w-focus", Title: "远程排障", Machine: "mac-w", Kind: "claude",
	}); err != nil {
		t.Fatal(err)
	}

	bus := NewBus(eng, nil, nil)
	bus.SetPaths(filepath.Join(dir, "uploads", "wecom"), dir)
	bus.sendStreamFn = func(context.Context, string, string, string, bool) error { return nil }
	bus.sendFn = func(context.Context, string, string) error { return nil }
	bus.sendMarkdownACK = bus.sendFn

	bus.HandleWeComMessage(ctx, wecom.IncomingMessage{
		ReqID:    "req-f1",
		MsgID:    "msg-f1",
		ChatType: "single",
		ChatID:   "U1",
		FromUser: "U1",
		Text:     "请分析这个日志",
		Attachments: []wecom.MediaRef{{
			Type:     "file",
			URL:      attSrv.URL + "/crash.log",
			Filename: "crash.log",
		}},
	})

	mu.Lock()
	defer mu.Unlock()
	if uploadCount != 1 {
		t.Fatalf("expected 1 cross-machine staged upload from WeCom focus direct chat, got %d", uploadCount)
	}
	if !strings.Contains(capturedPrompt, "/Users/w/.corgi/uploads/staged/20261128/") || strings.Contains(capturedPrompt, filepath.Join(dir, "uploads", "wecom")) {
		t.Fatalf("expected focus direct prompt rewritten to remote staged path, got: %s", capturedPrompt)
	}
}

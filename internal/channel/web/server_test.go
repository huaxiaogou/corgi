package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiperjing/corgi/internal/channel"
	"github.com/kiperjing/corgi/internal/core"
	"github.com/kiperjing/corgi/internal/fleet"
	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	"github.com/kiperjing/corgi/internal/store"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "corgi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := agent.NewRegistry()
	fl := fleet.NewFleet(nil, "hub", dir, "test", []string{"claude"})
	eng, err := core.NewEngine(context.Background(), st, fl, reg, dir, dir, "corgi", nil)
	if err != nil {
		t.Fatal(err)
	}
	bus := channel.NewBus(eng, nil, nil)
	master := "test-master-token"
	srv := &Server{
		Store:    st,
		Engine:   eng,
		Bus:      bus,
		Fleet:    fl,
		Auth:     fleet.NewAuthManager(st, master),
		Agents:   reg,
		CorgiDir: dir,
		Home:     dir,
		Version:  "test",
		Port:     7788,
	}
	return srv, master
}

func TestMachineTokenIsolationAndActionHTTPConflict(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()

	// Pair a machine node token.
	code, _ := srv.Auth.NewCode("machine")
	_, nodeTok, err := srv.Auth.Redeem(context.Background(), code, "machine", "macbook", "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}

	// Machine token MUST NOT access Web-only endpoints like /api/devices.
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer "+nodeTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("machine token should get 401 on /api/devices, got %d", rec.Code)
	}

	// Machine token MAY call /api/super/* (for corgi report / reply CLI).
	req2 := httptest.NewRequest(http.MethodGet, "/api/super/list", nil)
	req2.Header.Set("Authorization", "Bearer "+nodeTok)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("machine token should get 200 on /api/super/list, got %d", rec2.Code)
	}

	// Create a reply with options → creates pending Action → second resolve returns 409 Conflict.
	_, act, _, err := srv.Engine.Reply(context.Background(), model.MainThread, "确认建索引？", "", []string{"同意", "拒绝"}, false)
	if err != nil || act == nil {
		t.Fatalf("Reply failed: %v", err)
	}
	resolve := func() int {
		body, _ := json.Marshal(map[string]string{"choice": "1", "by": "web"})
		r := httptest.NewRequest(http.MethodPost, "/api/super/actions/"+act.ID+"/resolve", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+master)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code1 := resolve(); code1 != http.StatusOK {
		t.Fatalf("first resolve want 200, got %d", code1)
	}
	if code2 := resolve(); code2 != http.StatusConflict {
		t.Fatalf("second resolve want 409 Conflict, got %d", code2)
	}
}

func TestWebhookAuthGitHubGenericAndDismissHTTP(t *testing.T) {
	srv, master := newTestServer(t)
	h := srv.Routes()
	hookKey := srv.Auth.HookKey()

	// 1. Bad hook key rejected with 401.
	badReq := httptest.NewRequest(http.MethodPost, "/hooks/github?key=wrong", bytes.NewBufferString(`{}`))
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad hook key want 401, got %d", badRec.Code)
	}

	// 2. Valid GitHub webhook returns 202 and deduplicates on X-GitHub-Delivery.
	ghPayload := `{"action":"created","repository":{"full_name":"org/corgi"},"issue":{"number":42,"title":"Slow query"},"comment":{"body":"Please inspect index","html_url":"https://github.com/org/corgi/issues/42#c1"},"sender":{"login":"alice"}}`
	postGH := func() (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/hooks/github?key="+hookKey, bytes.NewBufferString(ghPayload))
		r.Header.Set("X-GitHub-Event", "issue_comment")
		r.Header.Set("X-GitHub-Delivery", "deliv-42")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code1, out1 := postGH()
	if code1 != http.StatusAccepted || out1["trigger"] == nil || out1["policy"] != "candidate" {
		t.Fatalf("first github webhook failed: code=%d out=%+v", code1, out1)
	}
	code2, out2 := postGH()
	if code2 != http.StatusAccepted || out2["duplicate"] != true {
		t.Fatalf("duplicate github webhook want duplicate=true, got code=%d out=%+v", code2, out2)
	}

	// 3. Generic webhook with X-Corgi-Thread appears in /api/super/triggers.
	genReq := httptest.NewRequest(http.MethodPost, "/hooks/alerts?key="+hookKey, bytes.NewBufferString(`{"from":"prometheus","text":"CPU > 95%"}`))
	genReq.Header.Set("X-Corgi-Thread", "ops:cpu")
	genRec := httptest.NewRecorder()
	h.ServeHTTP(genRec, genReq)
	if genRec.Code != http.StatusAccepted {
		t.Fatalf("generic webhook want 202, got %d", genRec.Code)
	}

	trReq := httptest.NewRequest(http.MethodGet, "/api/super/triggers", nil)
	trReq.Header.Set("Authorization", "Bearer "+master)
	trRec := httptest.NewRecorder()
	h.ServeHTTP(trRec, trReq)
	if trRec.Code != http.StatusOK || !bytes.Contains(trRec.Body.Bytes(), []byte("github:org/corgi#42")) || !bytes.Contains(trRec.Body.Bytes(), []byte("ops:cpu")) {
		t.Fatalf("unexpected triggers response: code=%d body=%s", trRec.Code, trRec.Body.String())
	}

	// 4. Dismiss action endpoint is atomic (200 then 409).
	_, act, _, err := srv.Engine.Reply(context.Background(), model.MainThread, "请验收文档", "", nil, true)
	if err != nil || act == nil {
		t.Fatal(err)
	}
	dismiss := func() int {
		r := httptest.NewRequest(http.MethodPost, "/api/super/actions/"+act.ID+"/dismiss", bytes.NewBufferString(`{}`))
		r.Header.Set("Authorization", "Bearer "+master)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := dismiss(); c != http.StatusOK {
		t.Fatalf("first dismiss want 200, got %d", c)
	}
	if c := dismiss(); c != http.StatusConflict {
		t.Fatalf("second dismiss want 409, got %d", c)
	}

	// 5. Settings returns derived hook_key and never leaks raw master token.
	setReq := httptest.NewRequest(http.MethodGet, "/api/super/settings", nil)
	setReq.Header.Set("Authorization", "Bearer "+master)
	setRec := httptest.NewRecorder()
	h.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK || !bytes.Contains(setRec.Body.Bytes(), []byte(hookKey)) || bytes.Contains(setRec.Body.Bytes(), []byte(master)) {
		t.Fatalf("settings leak/hook_key check failed: code=%d body=%s", setRec.Code, setRec.Body.String())
	}
}

func TestHistoryResumeEndpoint(t *testing.T) {
	srv, master := newTestServer(t)
	var gotArgs []string
	fakeH := herdr.NewClient(func(ctx context.Context) (net.Conn, error) {
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
			resp := map[string]any{"id": req.ID, "result": map[string]any{}}
			switch req.Method {
			case "workspace.create":
				resp["result"] = map[string]any{
					"workspace": map[string]any{"workspace_id": "w9"},
					"root_pane": map[string]any{"pane_id": "w9:p1"},
				}
			case "agent.start":
				if rawArgs, ok := req.Params["args"].([]any); ok {
					for _, a := range rawArgs {
						gotArgs = append(gotArgs, a.(string))
					}
				}
			}
			b, _ := json.Marshal(resp)
			_, _ = c2.Write(append(b, '\n'))
		}()
		return c1, nil
	})
	srv.Fleet = fleet.NewFleet(fakeH, "hub", srv.Home, "test", []string{"codebuddy"})

	if err := srv.Store.SaveEndedSession(context.Background(), &model.EndedSession{
		ID: "h10", Kind: "codebuddy", Cwd: srv.Home, Name: "cb-old",
		Title: "CodeBuddy 性能分析", Session: "sess-xyz", EndedAt: time.Now(),
	}, 20); err != nil {
		t.Fatal(err)
	}

	h := srv.Routes()
	req := httptest.NewRequest(http.MethodPost, "/api/history/h10/resume", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer "+master)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("w9:p1")) {
		t.Fatalf("resume failed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--resume sess-xyz") {
		t.Fatalf("expected --resume sess-xyz in launch args, got %v", gotArgs)
	}
}

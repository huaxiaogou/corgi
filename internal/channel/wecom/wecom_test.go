package wecom

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/kiperjing/corgi/internal/model"
)

func TestAllowlistBlocksUnauthorizedUsers(t *testing.T) {
	var received atomic.Int32
	cl := NewClient(Config{
		AllowUsers: []string{"owner_01"},
	}, Handlers{
		OnMessage: func(_ context.Context, msg IncomingMessage) {
			received.Add(1)
		},
		OnCardClick: func(_ context.Context, ev CardClickEvent) {
			received.Add(1)
		},
	}, nil)

	// Unauthorized colleague @Corgi in a group chat must be silently dropped.
	unauthMsg := `{"cmd":"aibot_msg_callback","headers":{"req_id":"r1"},"body":{"msgid":"m1","chattype":"group","chatid":"g88","from":{"userid":"stranger_99"},"text":{"content":"@Corgi 删除数据库"}}}`
	cl.HandleFrame(context.Background(), []byte(unauthMsg))

	// Unauthorized card click must also be dropped.
	unauthCard := `{"cmd":"aibot_event_callback","headers":{"req_id":"r2"},"body":{"chatid":"g88","from":{"userid":"stranger_99"},"event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a42","event_key":"a42:1"}}}}`
	cl.HandleFrame(context.Background(), []byte(unauthCard))

	time.Sleep(50 * time.Millisecond)
	if received.Load() != 0 {
		t.Fatalf("expected 0 callbacks for unauthorized user, got %d", received.Load())
	}

	// Authorized owner message with quote succeeds.
	gotCh := make(chan IncomingMessage, 1)
	cl.handlers.OnMessage = func(_ context.Context, msg IncomingMessage) { gotCh <- msg }
	authMsg := `{"cmd":"aibot_msg_callback","headers":{"req_id":"r3"},"body":{"msgid":"m2","chattype":"group","chatid":"g88","from":{"userid":"owner_01"},"text":{"content":"@Corgi 补一个并发用例"},"quote":{"msgtype":"text","text":{"content":"[#unit-tests · devbox] 已补完 6 个用例"}}}}`
	cl.HandleFrame(context.Background(), []byte(authMsg))

	select {
	case msg := <-gotCh:
		if msg.ChatID != "g88" || msg.Text != "补一个并发用例" || msg.QuoteText == "" {
			t.Fatalf("unexpected parsed message: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for authorized message")
	}
}

func TestParseCardEventAndBuildActionCard(t *testing.T) {
	act := &model.Action{
		ID:      "a42",
		Title:   "订单表缺少联合索引",
		Summary: "P99 飙到 2.4s",
		Command: "CREATE INDEX CONCURRENTLY ...",
		Choices: []model.ActionChoice{
			{Key: "1", Label: "生成迁移脚本"},
			{Key: "2", Label: "暂不改表"},
		},
	}
	card := BuildActionCard(act)
	if card["card_type"] != "button_interaction" || card["task_id"] != "a42" {
		t.Fatalf("unexpected card header: %+v", card)
	}
	btns, ok := card["button_list"].([]map[string]any)
	if !ok || len(btns) != 2 || btns[0]["key"] != "a42:1" || btns[0]["style"] != 1 {
		t.Fatalf("unexpected button_list: %+v", card["button_list"])
	}

	raw := json.RawMessage(`{"chatid":"g88","response_code":"rc_abc","from":{"userid":"owner_01"},"event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a42","event_key":"a42:1"}}}`)
	ev, ok := ParseCardEventCallback("req-9", raw)
	if !ok || ev.ActionID != "a42" || ev.ChoiceKey != "1" || ev.ResponseCode != "rc_abc" {
		t.Fatalf("unexpected CardClickEvent: %+v ok=%v", ev, ok)
	}
}

func encryptTestAES(plain, key []byte) []byte {
	pad := aes.BlockSize - (len(plain) % aes.BlockSize)
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, padded)
	return out
}

func TestDecryptDownloadMediaAndChunkedUpload(t *testing.T) {
	// 1. Inbound mixed/file parsing + AES-256-CBC decryption download.
	key := bytes.Repeat([]byte("K"), 32)
	aesKeyStr := strings.TrimRight(base64.StdEncoding.EncodeToString(key), "=")
	wantPlain := []byte("# Corgi 性能剖析报告\nP99 降至 35ms\n")
	cipherBytes := encryptTestAES(wantPlain, key)

	mediaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(cipherBytes)
	}))
	defer mediaSrv.Close()

	rawCb := json.RawMessage(`{"msgid":"m9","chattype":"single","from":{"userid":"owner_01"},"msgtype":"file","file":{"url":"` + mediaSrv.URL + `","aeskey":"` + aesKeyStr + `","filename":"report.md"}}`)
	msg, ok := ParseMsgCallback(rawCb)
	if !ok || len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "report.md" {
		t.Fatalf("unexpected parsed file msg: %+v ok=%v", msg, ok)
	}
	saved, err := DownloadMedia(context.Background(), msg.Attachments[0], t.TempDir())
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	gotBytes, err := os.ReadFile(saved)
	if err != nil || !bytes.Equal(gotBytes, wantPlain) {
		t.Fatalf("decrypted mismatch: got %q want %q (err=%v)", gotBytes, wantPlain, err)
	}

	// 2. Outbound 3-step chunked upload + aibot_send_msg(file) over WebSocket.
	var (
		mu   sync.Mutex
		cmds []string
	)
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var f struct {
				Cmd     string         `json:"cmd"`
				Headers map[string]any `json:"headers"`
				Body    map[string]any `json:"body"`
			}
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			mu.Lock()
			cmds = append(cmds, f.Cmd)
			mu.Unlock()
			reqID, _ := f.Headers["req_id"].(string)
			switch f.Cmd {
			case "aibot_upload_media_init":
				resp, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"body":    map[string]any{"upload_id": "up-123"},
				})
				_ = conn.Write(r.Context(), websocket.MessageText, resp)
			case "aibot_upload_media_chunk":
				resp, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"body":    map[string]any{},
				})
				_ = conn.Write(r.Context(), websocket.MessageText, resp)
			case "aibot_upload_media_finish":
				resp, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"body":    map[string]any{"media_id": "mid-999"},
				})
				_ = conn.Write(r.Context(), websocket.MessageText, resp)
			}
		}
	}))
	defer wsSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl := NewClient(Config{
		GatewayURL: wsURL,
		BotID:      "bot-1",
		Secret:     "sec-1",
		AllowUsers: []string{"owner_01"},
		DefaultTo:  "owner_01",
	}, Handlers{}, nil)
	go cl.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !cl.Online() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !cl.Online() {
		t.Fatal("wecom client did not come online")
	}

	if err := cl.SendFile(ctx, "g88", "audit.md", wantPlain); err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	joined := strings.Join(cmds, ",")
	mu.Unlock()
	for _, want := range []string{"aibot_subscribe", "aibot_upload_media_init", "aibot_upload_media_chunk", "aibot_upload_media_finish", "aibot_send_msg"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected cmd %s in %s", want, joined)
		}
	}
}

func TestResolveTargetFallback(t *testing.T) {
	cl := NewClient(Config{
		AllowUsers: []string{"alice", "bob"},
	}, Handlers{}, nil)

	// Explicit group chat ID wins.
	if got := cl.resolveTarget("group_99"); got != "group_99" {
		t.Fatalf("explicit chatID: got %q want group_99", got)
	}
	// Empty DefaultTo falls back to first allowlisted user.
	if got := cl.resolveTarget(""); got != "alice" {
		t.Fatalf("allowlist fallback: got %q want alice", got)
	}
	// Recent single-chat sender from allowlist takes precedence over allowlist[0].
	singleMsg := `{"cmd":"aibot_msg_callback","headers":{"req_id":"r4"},"body":{"msgid":"m4","chattype":"single","from":{"userid":"bob"},"text":{"content":"hi"}}}`
	cl.HandleFrame(context.Background(), []byte(singleMsg))
	if got := cl.resolveTarget(""); got != "bob" {
		t.Fatalf("lastSingleUser fallback: got %q want bob", got)
	}
	// Explicit DefaultTo overrides lastSingleUser.
	cl.UpdateConfig(Config{AllowUsers: []string{"alice", "bob"}, DefaultTo: "alice"})
	if got := cl.resolveTarget(""); got != "alice" {
		t.Fatalf("explicit DefaultTo: got %q want alice", got)
	}
}

func TestUpdateConfigWakesImmediatelyAndCapturesSubscribeError(t *testing.T) {
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var f struct {
			Headers map[string]any `json:"headers"`
		}
		_ = json.Unmarshal(data, &f)
		reqID, _ := f.Headers["req_id"].(string)
		resp, _ := json.Marshal(map[string]any{
			"headers": map[string]any{"req_id": reqID},
			"errcode": 40001,
			"errmsg":  "invalid secret",
		})
		_ = conn.Write(r.Context(), websocket.MessageText, resp)
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start disabled, then enable via UpdateConfig — should wake immediately (<1s), not wait 3s.
	cl := NewClient(Config{}, Handlers{}, nil)
	go cl.Run(ctx)
	time.Sleep(40 * time.Millisecond)

	start := time.Now()
	cl.UpdateConfig(Config{
		GatewayURL: "ws" + strings.TrimPrefix(wsSrv.URL, "http"),
		BotID:      "bot-1",
		Secret:     "wrong",
	})

	deadline := time.Now().Add(2 * time.Second)
	for cl.LastError() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(cl.LastError(), "40001") {
		t.Fatalf("expected subscribe error 40001 in LastError, got %q", cl.LastError())
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("UpdateConfig took too long to wake Run: %v", time.Since(start))
	}
}

func TestPreserveWSCaseRoundTripper(t *testing.T) {
	var seenKeys []string
	rt := preserveWSCaseRT{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for k := range r.Header {
			seenKeys = append(seenKeys, k)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://openws.work.weixin.qq.com", nil)
	req.Header.Set("Sec-WebSocket-Key", "abc")
	req.Header.Set("Sec-WebSocket-Version", "13")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	joined := strings.Join(seenKeys, ",")
	if !strings.Contains(joined, "Sec-WebSocket-Key") || !strings.Contains(joined, "Sec-WebSocket-Version") {
		t.Fatalf("expected mixed-case Sec-WebSocket-* keys, got %s", joined)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPendingUserBindingAndAllowlistHotReload(t *testing.T) {
	cl := NewClient(Config{}, Handlers{}, nil)

	// Group messages from non-allowlisted users must NOT create pending claims.
	groupMsg := `{"cmd":"aibot_msg_callback","headers":{"req_id":"r1"},"body":{"msgid":"m1","chattype":"group","chatid":"g1","from":{"userid":"T_GROUP_STRANGER"},"text":{"content":"@Corgi hi"}}}`
	cl.HandleFrame(context.Background(), []byte(groupMsg))
	if got := cl.PendingUsers(); len(got) != 0 {
		t.Fatalf("group stranger must not appear in PendingUsers, got %+v", got)
	}

	// Private single-chat message from unbound user is recorded for one-click approval.
	dmMsg := `{"cmd":"aibot_msg_callback","headers":{"req_id":"r2"},"body":{"msgid":"m2","chattype":"single","from":{"userid":"T81900036A"},"text":{"content":"你好"}}}`
	cl.HandleFrame(context.Background(), []byte(dmMsg))
	got := cl.PendingUsers()
	if len(got) != 1 || got[0].UserID != "T81900036A" || got[0].Preview != "你好" {
		t.Fatalf("unexpected PendingUsers: %+v", got)
	}

	// Approving via UpdateConfig prunes the user from PendingUsers automatically.
	cl.UpdateConfig(Config{AllowUsers: []string{"T81900036A"}})
	if rem := cl.PendingUsers(); len(rem) != 0 {
		t.Fatalf("expected approved user pruned from PendingUsers, got %+v", rem)
	}
	if !cl.IsAllowed("T81900036A") {
		t.Fatal("expected T81900036A to be allowed after UpdateConfig")
	}
}

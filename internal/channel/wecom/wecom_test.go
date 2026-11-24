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

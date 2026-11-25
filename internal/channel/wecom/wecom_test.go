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

func TestBuildActionCardLayoutAndLimits(t *testing.T) {
	longBody := "第一阶段检查已完成。线上登录接口在并发刷新 token 时偶发旧会话覆盖问题，建议先加分布式锁并补齐并发回归测试，确认按哪种方案推进？"
	veryLongBody := strings.Repeat("这是一个包含关键发布上下文和风险说明的长决策正文片段，", 6) + "请确认最终执行方案。"
	veryLongOpt := "立即加分布式锁并补充高并发竞态回归测试，同时在灰度环境观察三十分钟后再全量上线"

	tests := []struct {
		name           string
		action         *model.Action
		wantTitle      string
		wantSubSubstr  string
		wantQuote      bool
		wantQuoteSub   string
		wantHorizCount int
		wantBtnCount   int
		wantFirstBtn   string
	}{
		{
			name: "short distinct title summary command and short choices",
			action: &model.Action{
				ID:           "a42",
				Kind:         model.ActionKindWorkerDialog,
				TargetWorker: "db-migrator",
				Title:        "订单表缺少联合索引",
				Summary:      "P99 飙到 2.4s",
				Command:      "CREATE INDEX CONCURRENTLY idx_orders_user ON orders(user_id);",
				Choices: []model.ActionChoice{
					{Key: "1", Label: "生成迁移脚本"},
					{Key: "2", Label: "暂不改表"},
				},
			},
			wantTitle:      "订单表缺少联合索引",
			wantSubSubstr:  "P99 飙到 2.4s",
			wantQuote:      true,
			wantQuoteSub:   "CREATE INDEX CONCURRENTLY",
			wantHorizCount: 0,
			wantBtnCount:   2,
			wantFirstBtn:   "生成迁移脚本",
		},
		{
			name: "long router_choice splits heading and expands long options",
			action: &model.Action{
				ID:      "a99",
				Kind:    model.ActionKindRouterChoice,
				Title:   string([]rune(longBody)[:40]) + "…",
				Summary: longBody,
				Choices: []model.ActionChoice{
					{Key: "1", Label: "先加分布式锁并补齐并发回归测试（推荐）"},
					{Key: "2", Label: "仅记录告警日志，暂不改动登录核心链路"},
					{Key: "3", Label: "暂缓处理"},
				},
			},
			wantTitle:      "第一阶段检查已完成",
			wantSubSubstr:  "建议先加分布式锁并补齐并发回归测试",
			wantQuote:      false,
			wantHorizCount: 3,
			wantBtnCount:   3,
			wantFirstBtn:   "选1",
		},
		{
			name: "patrol digest with 3 medium choices expands full options and uses compact buttons",
			action: &model.Action{
				ID:      "a555",
				Kind:    model.ActionKindPatrolDigest,
				Title:   "待定事项：太湖接入方案曾指出，若同事也使用 Corgi，需先定身份隔离与权限边界。你想按个人使用还是共享服务继续细化？",
				Summary: "待定事项：太湖接入方案曾指出，若同事也使用 Corgi，需先定身份隔离与权限边界。你想按个人使用还是共享服务继续细化？",
				Choices: []model.ActionChoice{
					{Key: "1", Label: "个人手机先用"},
					{Key: "2", Label: "按共享服务设计"},
					{Key: "3", Label: "暂不推进"},
				},
			},
			wantTitle:      "巡检摘要确认",
			wantSubSubstr:  "你想按个人使用还是共享服务继续细化？",
			wantQuote:      false,
			wantHorizCount: 3,
			wantBtnCount:   3,
			wantFirstBtn:   "选1",
		},
		{
			name: "very long summary and very long option spill cleanly into quote_area",
			action: &model.Action{
				ID:      "a100",
				Kind:    model.ActionKindRouterChoice,
				Title:   string([]rune(veryLongBody)[:60]) + "…",
				Summary: veryLongBody,
				Choices: []model.ActionChoice{
					{Key: "1", Label: veryLongOpt},
					{Key: "2", Label: "暂不发布"},
				},
			},
			wantTitle:      "待拍板决策",
			wantSubSubstr:  "这是一个包含关键发布上下文",
			wantQuote:      true,
			wantQuoteSub:   veryLongOpt,
			wantHorizCount: 2,
			wantBtnCount:   2,
			wantFirstBtn:   "1. 立即加分布式…",
		},
		{
			name: "long title with empty summary keeps full text in sub_title_text",
			action: &model.Action{
				ID:      "a101",
				Kind:    model.ActionKindRouterCheck,
				Title:   "请确认生产环境数据库只读账号切换以及告警阈值调整已经全部核对完成",
				Choices: []model.ActionChoice{{Key: "1", Label: "已确认"}},
			},
			wantTitle:      "待确认事项",
			wantSubSubstr:  "告警阈值调整已经全部核对完成",
			wantHorizCount: 0,
			wantBtnCount:   1,
			wantFirstBtn:   "已确认",
		},
		{
			name: "more than 6 choices capped to 6 buttons",
			action: &model.Action{
				ID:      "a102",
				Kind:    model.ActionKindRouterChoice,
				Title:   "选择目标环境",
				Summary: "选择目标环境",
				Choices: []model.ActionChoice{
					{Key: "1", Label: "环境A"},
					{Key: "2", Label: "环境B"},
					{Key: "3", Label: "环境C"},
					{Key: "4", Label: "环境D"},
					{Key: "5", Label: "环境E"},
					{Key: "6", Label: "环境F"},
					{Key: "7", Label: "环境G"},
				},
			},
			wantTitle:      "选择目标环境",
			wantHorizCount: 6,
			wantBtnCount:   6,
			wantFirstBtn:   "选1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			card := BuildActionCard(tc.action)
			mt, _ := card["main_title"].(map[string]any)
			gotTitle, _ := mt["title"].(string)
			gotDesc, _ := mt["desc"].(string)
			if gotTitle != tc.wantTitle {
				t.Fatalf("main_title.title = %q, want %q", gotTitle, tc.wantTitle)
			}
			if len([]rune(gotTitle)) > cardMainTitleMaxRunes {
				t.Fatalf("main_title.title exceeds %d runes: %q", cardMainTitleMaxRunes, gotTitle)
			}
			if len([]rune(gotDesc)) > cardMainDescMaxRunes {
				t.Fatalf("main_title.desc exceeds %d runes: %q", cardMainDescMaxRunes, gotDesc)
			}
			if gotDesc == gotTitle {
				t.Fatalf("main_title.desc must not duplicate main_title.title: %q", gotDesc)
			}

			sub, _ := card["sub_title_text"].(string)
			if len([]rune(sub)) > cardSubTitleMaxRunes {
				t.Fatalf("sub_title_text exceeds %d runes (%d): %q", cardSubTitleMaxRunes, len([]rune(sub)), sub)
			}
			if tc.wantSubSubstr != "" && !strings.Contains(sub, tc.wantSubSubstr) {
				t.Fatalf("sub_title_text %q missing %q", sub, tc.wantSubSubstr)
			}

			horiz, _ := card["horizontal_content_list"].([]map[string]any)
			if len(horiz) != tc.wantHorizCount {
				t.Fatalf("horizontal_content_list len = %d, want %d (%+v)", len(horiz), tc.wantHorizCount, horiz)
			}
			for _, row := range horiz {
				k, _ := row["keyname"].(string)
				v, _ := row["value"].(string)
				if len([]rune(k)) > 5 || len([]rune(v)) > cardHorizValueMaxRunes {
					t.Fatalf("horizontal item exceeds limits: key=%q value=%q", k, v)
				}
			}

			qa, hasQA := card["quote_area"].(map[string]any)
			if hasQA != tc.wantQuote {
				t.Fatalf("has quote_area = %v, want %v (%+v)", hasQA, tc.wantQuote, qa)
			}
			if tc.wantQuoteSub != "" {
				qt, _ := qa["quote_text"].(string)
				if !strings.Contains(qt, tc.wantQuoteSub) {
					t.Fatalf("quote_area.quote_text %q missing %q", qt, tc.wantQuoteSub)
				}
			}

			btns, _ := card["button_list"].([]map[string]any)
			if len(btns) != tc.wantBtnCount {
				t.Fatalf("button_list len = %d, want %d", len(btns), tc.wantBtnCount)
			}
			if got := btns[0]["text"].(string); got != tc.wantFirstBtn {
				t.Fatalf("button[0].text = %q, want %q", got, tc.wantFirstBtn)
			}
			for i, btn := range btns {
				txt, _ := btn["text"].(string)
				if len([]rune(txt)) > cardButtonTextMaxRunes {
					t.Fatalf("button[%d].text exceeds %d runes: %q", i, cardButtonTextMaxRunes, txt)
				}
			}
		})
	}
}

func TestCompactButtonsPreserveChoiceKeysAndClickMapping(t *testing.T) {
	act := &model.Action{
		ID:      "a555",
		Kind:    model.ActionKindPatrolDigest,
		Title:   "待定事项：太湖接入多用户策略确认",
		Summary: "待定事项：太湖接入多用户策略确认，你想按哪种方案推进？",
		Choices: []model.ActionChoice{
			{Key: "1", Label: "个人手机先用"},
			{Key: "2", Label: "按共享服务设计"},
			{Key: "3", Label: "暂不推进"},
		},
	}

	card := BuildActionCard(act)
	horiz, _ := card["horizontal_content_list"].([]map[string]any)
	wantHoriz := []struct{ k, v string }{
		{"选项 1", "个人手机先用"},
		{"选项 2", "按共享服务设计"},
		{"选项 3", "暂不推进"},
	}
	if len(horiz) != len(wantHoriz) {
		t.Fatalf("horizontal_content_list len = %d, want %d", len(horiz), len(wantHoriz))
	}
	for i, w := range wantHoriz {
		if horiz[i]["keyname"] != w.k || horiz[i]["value"] != w.v {
			t.Fatalf("horiz[%d] = %+v, want %+v", i, horiz[i], w)
		}
	}

	btns, _ := card["button_list"].([]map[string]any)
	wantBtns := []struct {
		text  string
		key   string
		style int
	}{
		{"选1", "a555:1", 1},
		{"选2", "a555:2", 2},
		{"选3", "a555:3", 2},
	}
	if len(btns) != len(wantBtns) {
		t.Fatalf("button_list len = %d, want %d", len(btns), len(wantBtns))
	}
	for i, w := range wantBtns {
		if btns[i]["text"] != w.text || btns[i]["key"] != w.key || btns[i]["style"] != w.style {
			t.Fatalf("button[%d] = %+v, want %+v", i, btns[i], w)
		}
	}

	// Clicking button 2 ("选2" -> key "a555:2") still parses to ActionID="a555", ChoiceKey="2".
	cbRaw := json.RawMessage(`{"chattype":"single","from":{"userid":"owner_01"},"event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a555","event_key":"a555:2"}}}`)
	ev, ok := ParseCardEventCallback("req-555", cbRaw)
	if !ok || ev.ActionID != "a555" || ev.ChoiceKey != "2" {
		t.Fatalf("ParseCardEventCallback mismatch: ev=%+v ok=%v", ev, ok)
	}

	// Resolved card renders the original full label ("按共享服务设计"), never the compact button label ("选2").
	act.Status = model.ActionResolved
	act.ResolvedBy = "wecom"
	act.ChosenKey = "2"
	act.ChosenLabel = "按共享服务设计"
	resolvedCard := BuildResolvedActionCard(act, "a555")
	if rt, _ := resolvedCard["replace_text"].(string); rt != "已选：按共享服务设计" {
		t.Fatalf("resolved replace_text = %q, want 已选：按共享服务设计", rt)
	}
}

func TestParseCardEventCallbackVariants(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantOK    bool
		wantActID string
		wantKey   string
		wantTask  string
		wantResp  string
		wantChat  string
	}{
		{
			name:      "nested snake_case group chat",
			raw:       `{"chatid":"g88","chattype":"group","response_code":"rc_1","from":{"userid":"u1"},"event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a42","event_key":"a42:1"}}}`,
			wantOK:    true,
			wantActID: "a42",
			wantKey:   "1",
			wantTask:  "a42",
			wantResp:  "rc_1",
			wantChat:  "g88",
		},
		{
			name:      "nested PascalCase with nested ResponseCode",
			raw:       `{"chattype":"single","from":{"userid":"u2"},"event":{"eventtype":"template_card_event","template_card_event":{"TaskId":"a55","EventKey":"a55:2","ResponseCode":"rc_nested"}}}`,
			wantOK:    true,
			wantActID: "a55",
			wantKey:   "2",
			wantTask:  "a55",
			wantResp:  "rc_nested",
			wantChat:  "u2",
		},
		{
			name:      "flattened inside event object",
			raw:       `{"chattype":"single","from":{"userid":"u3"},"event":{"eventtype":"template_card_event","task_id":"a77","event_key":"approve","response_code":"rc_flat"}}`,
			wantOK:    true,
			wantActID: "a77",
			wantKey:   "approve",
			wantTask:  "a77",
			wantResp:  "rc_flat",
			wantChat:  "u3",
		},
		{
			name:   "non card event enter_chat ignored",
			raw:    `{"chattype":"single","from":{"userid":"u1"},"event":{"eventtype":"enter_chat"}}`,
			wantOK: false,
		},
		{
			name:   "missing choice key rejected",
			raw:    `{"chattype":"single","from":{"userid":"u1"},"event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a42","event_key":""}}}`,
			wantOK: false,
		},
		{
			name:   "missing userid rejected",
			raw:    `{"chattype":"single","event":{"eventtype":"template_card_event","template_card_event":{"task_id":"a42","event_key":"a42:1"}}}`,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := ParseCardEventCallback("req-1", json.RawMessage(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ev=%+v)", ok, tc.wantOK, ev)
			}
			if !tc.wantOK {
				return
			}
			if ev.ActionID != tc.wantActID || ev.ChoiceKey != tc.wantKey || ev.TaskID != tc.wantTask || ev.ResponseCode != tc.wantResp || ev.ChatID != tc.wantChat {
				t.Fatalf("unexpected CardClickEvent: %+v", ev)
			}
		})
	}
}

func TestBuildResolvedActionCard(t *testing.T) {
	act := &model.Action{
		ID:          "a42",
		Kind:        model.ActionKindRouterChoice,
		Title:       "订单表缺少联合索引",
		Summary:     "P99 飙到 2.4s，需要决定是否立刻建索引",
		Status:      model.ActionResolved,
		ResolvedBy:  "wecom",
		ChosenKey:   "1",
		ChosenLabel: "先加分布式锁并补充高并发回归测试再灰度上线（超过三十个字符的长选项验证）",
	}
	card := BuildResolvedActionCard(act, "a42")
	if card["card_type"] != "button_interaction" || card["task_id"] != "a42" {
		t.Fatalf("unexpected resolved card envelope: %+v", card)
	}
	src, _ := card["source"].(map[string]any)
	if src["desc"] != "Corgi · 已拍板" {
		t.Fatalf("unexpected resolved source: %+v", src)
	}
	if rt, _ := card["replace_text"].(string); rt == "" || len([]rune(rt)) > cardReplaceTextMaxRunes {
		t.Fatalf("unexpected replace_text: %q", rt)
	}
	qa, _ := card["quote_area"].(map[string]any)
	if qt, _ := qa["quote_text"].(string); !strings.Contains(qt, "超过三十个字符") {
		t.Fatalf("expected long chosen label in quote_area, got %+v", qa)
	}
	btns, _ := card["button_list"].([]map[string]any)
	if len(btns) != 1 || btns[0]["text"] != "已拍板" {
		t.Fatalf("unexpected resolved button_list: %+v", btns)
	}

	expired := &model.Action{ID: "a43", Status: model.ActionExpired, ResolvedBy: "dismiss", Title: "旧提醒"}
	expCard := BuildResolvedActionCard(expired, "a43")
	if expSrc, _ := expCard["source"].(map[string]any); expSrc["desc"] != "Corgi · 已关闭" {
		t.Fatalf("unexpected dismissed source: %+v", expSrc)
	}
	if expCard["replace_text"] != "已关闭" {
		t.Fatalf("unexpected dismissed replace_text: %+v", expCard["replace_text"])
	}
}

func TestCardClickUpdateAndNonFatalCommandError(t *testing.T) {
	var (
		mu       sync.Mutex
		updates  []map[string]any
		sentCard map[string]any
		failNext atomic.Bool
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
			reqID, _ := f.Headers["req_id"].(string)
			switch f.Cmd {
			case "aibot_send_msg":
				mu.Lock()
				sentCard, _ = f.Body["template_card"].(map[string]any)
				mu.Unlock()
				ack, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": 0,
					"errmsg":  "ok",
				})
				_ = conn.Write(r.Context(), websocket.MessageText, ack)
			case "aibot_respond_update_msg":
				mu.Lock()
				updates = append(updates, map[string]any{"req_id": reqID, "body": f.Body})
				mu.Unlock()
				errCode := 0
				errMsg := "ok"
				if failNext.Load() {
					errCode = 40058
					errMsg = "invalid template_card"
				}
				ack, _ := json.Marshal(map[string]any{
					"headers": map[string]any{"req_id": reqID},
					"errcode": errCode,
					"errmsg":  errMsg,
				})
				_ = conn.Write(r.Context(), websocket.MessageText, ack)
			}
		}
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl := NewClient(Config{
		GatewayURL: "ws" + strings.TrimPrefix(wsSrv.URL, "http"),
		BotID:      "bot-1",
		Secret:     "sec-1",
		AllowUsers: []string{"owner_01"},
		DefaultTo:  "owner_01",
	}, Handlers{}, nil)
	go cl.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for !cl.Online() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if !cl.Online() {
		t.Fatal("wecom client did not come online")
	}

	act := &model.Action{
		ID:      "a42",
		Kind:    model.ActionKindRouterChoice,
		Title:   "确认是否立即发布",
		Summary: "确认是否立即发布",
		Choices: []model.ActionChoice{{Key: "1", Label: "立即发布"}, {Key: "2", Label: "稍后再说"}},
	}
	if err := cl.SendActionCard(ctx, "", act); err != nil {
		t.Fatalf("SendActionCard failed: %v", err)
	}
	mu.Lock()
	if sentCard == nil || sentCard["task_id"] != "a42" {
		mu.Unlock()
		t.Fatalf("unexpected sentCard: %+v", sentCard)
	}
	mu.Unlock()

	act.Status = model.ActionResolved
	act.ResolvedBy = "wecom"
	act.ChosenKey = "1"
	act.ChosenLabel = "立即发布"
	if err := cl.UpdateCardResolvedWithTask(ctx, "cb-req-77", "a42", act); err != nil {
		t.Fatalf("UpdateCardResolvedWithTask failed: %v", err)
	}

	mu.Lock()
	if len(updates) != 1 || updates[0]["req_id"] != "cb-req-77" {
		mu.Unlock()
		t.Fatalf("unexpected updates: %+v", updates)
	}
	body, _ := updates[0]["body"].(map[string]any)
	tc, _ := body["template_card"].(map[string]any)
	mu.Unlock()
	if body["response_type"] != "update_template_card" || tc["card_type"] != "button_interaction" || tc["task_id"] != "a42" || tc["replace_text"] == "" {
		t.Fatalf("unexpected update_template_card body: %+v", body)
	}

	// Per-command error ACK returns error cleanly without tearing down the WebSocket.
	failNext.Store(true)
	err := cl.UpdateCardResolvedWithTask(ctx, "cb-req-78", "a42", act)
	if err == nil || !strings.Contains(err.Error(), "40058") {
		t.Fatalf("expected errcode 40058 from UpdateCardResolvedWithTask, got %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if !cl.Online() {
		t.Fatal("per-command errcode should not disconnect live websocket")
	}
}

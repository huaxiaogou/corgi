package wecom

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	uploadChunkBytes = 512 << 10
	maxInboundMedia  = 25 << 20
	maxOutboundMedia = 15 << 20
	wsRPCTimeout     = 20 * time.Second
)

// MediaRef identifies an inbound image or file attached to a WeCom callback message.
type MediaRef struct {
	Type     string // image | file
	URL      string
	AESKey   string
	Filename string
}

type wsResp struct {
	ErrCode int             `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
	Body    json.RawMessage `json:"body"`
}

// DecryptMedia decrypts WeCom Smart Bot media bytes using AES-256-CBC + PKCS#7 unpadding.
func DecryptMedia(ciphertext []byte, aesKey string) ([]byte, error) {
	key, err := decodeAESKey(aesKey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("invalid ciphertext length %d", len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init aes cipher: %w", err)
	}
	iv := key[:aes.BlockSize]
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
	return unpadPKCS7(plain)
}

func decodeAESKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("empty aeskey")
	}
	// Standard WeCom 43-char base64 EncodingAESKey (without '=' padding) -> 32 bytes.
	if rem := len(s) % 4; rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("invalid aeskey length")
}

func unpadPKCS7(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(data[len(data)-1])
	// WeCom uses 32-byte block PKCS#7 padding (1..32).
	if pad < 1 || pad > 32 || pad > len(data) {
		return nil, fmt.Errorf("invalid pkcs7 padding %d", pad)
	}
	for i := len(data) - pad; i < len(data); i++ {
		if int(data[i]) != pad {
			return nil, errors.New("corrupted pkcs7 padding")
		}
	}
	return data[:len(data)-pad], nil
}

// DownloadMedia fetches ref.URL, decrypts with ref.AESKey when set, and stores it in saveDir.
func DownloadMedia(ctx context.Context, ref MediaRef, saveDir string) (string, error) {
	if strings.TrimSpace(ref.URL) == "" {
		return "", errors.New("empty media url")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build media request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("download media status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMedia+1))
	if err != nil {
		return "", fmt.Errorf("read media body: %w", err)
	}
	if len(raw) > maxInboundMedia {
		return "", fmt.Errorf("media exceeds %d MB limit", maxInboundMedia>>20)
	}
	data := raw
	if strings.TrimSpace(ref.AESKey) != "" {
		dec, err := DecryptMedia(raw, ref.AESKey)
		if err != nil {
			return "", fmt.Errorf("decrypt media: %w", err)
		}
		data = dec
	}

	name := strings.TrimSpace(filepath.Base(ref.Filename))
	if name == "" || name == "." || name == "/" {
		ext := extFromBytes(data, ref.Type)
		name = "media" + ext
	}
	dayDir := filepath.Join(saveDir, time.Now().Format("20060102"))
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir media dir: %w", err)
	}
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	dest := filepath.Join(dayDir, hex.EncodeToString(buf)+"-"+name)
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return "", fmt.Errorf("write media file: %w", err)
	}
	return dest, nil
}

func extFromBytes(b []byte, mediaType string) string {
	ct := http.DetectContentType(b)
	switch {
	case strings.Contains(ct, "png"):
		return ".png"
	case strings.Contains(ct, "jpeg"):
		return ".jpg"
	case strings.Contains(ct, "gif"):
		return ".gif"
	case strings.Contains(ct, "webp"):
		return ".webp"
	case strings.Contains(ct, "pdf"):
		return ".pdf"
	case mediaType == "image":
		return ".png"
	default:
		return ".bin"
	}
}

// callRPC sends a WS command with a generated req_id and awaits its matching response frame.
func (c *Client) callRPC(ctx context.Context, cmd string, body map[string]any) (wsResp, error) {
	return c.callRPCWithReqID(ctx, c.nextReqID(), cmd, body)
}

// callRPCWithReqID sends a WS command with an explicit req_id and awaits its matching ACK frame.
func (c *Client) callRPCWithReqID(ctx context.Context, reqID, cmd string, body map[string]any) (wsResp, error) {
	if strings.TrimSpace(reqID) == "" {
		reqID = c.nextReqID()
	}
	ch := make(chan wsResp, 1)

	c.pendingMu.Lock()
	if c.pending == nil {
		c.pending = map[string]chan wsResp{}
	}
	c.pending[reqID] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	frame := map[string]any{
		"cmd":     cmd,
		"headers": map[string]any{"req_id": reqID},
		"body":    body,
	}
	if err := c.send(ctx, frame); err != nil {
		return wsResp{}, err
	}

	rctx, cancel := context.WithTimeout(ctx, wsRPCTimeout)
	defer cancel()
	select {
	case <-rctx.Done():
		return wsResp{}, fmt.Errorf("%s timeout: %w", cmd, rctx.Err())
	case resp := <-ch:
		if resp.ErrCode != 0 {
			return resp, fmt.Errorf("%s errcode=%d: %s", cmd, resp.ErrCode, resp.ErrMsg)
		}
		return resp, nil
	}
}

// UploadMedia uploads bytes over the WeCom Smart Bot 3-step chunked WebSocket protocol
// (aibot_upload_media_init -> aibot_upload_media_chunk x N -> aibot_upload_media_finish).
func (c *Client) UploadMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty media payload")
	}
	if len(data) > maxOutboundMedia {
		return "", fmt.Errorf("file exceeds %d MB limit", maxOutboundMedia>>20)
	}
	if mediaType == "" {
		mediaType = "file"
	}
	totalChunks := (len(data) + uploadChunkBytes - 1) / uploadChunkBytes
	sum := md5.Sum(data)

	initResp, err := c.callRPC(ctx, "aibot_upload_media_init", map[string]any{
		"type":         mediaType,
		"filename":     filepath.Base(filename),
		"total_size":   len(data),
		"total_chunks": totalChunks,
		"md5":          hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", err
	}
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	if json.Unmarshal(initResp.Body, &initBody) != nil || initBody.UploadID == "" {
		return "", errors.New("missing upload_id in aibot_upload_media_init response")
	}

	for idx := 0; idx < totalChunks; idx++ {
		start := idx * uploadChunkBytes
		end := start + uploadChunkBytes
		if end > len(data) {
			end = len(data)
		}
		if _, err := c.callRPC(ctx, "aibot_upload_media_chunk", map[string]any{
			"upload_id":   initBody.UploadID,
			"chunk_index": idx,
			"base64_data": base64.StdEncoding.EncodeToString(data[start:end]),
		}); err != nil {
			return "", fmt.Errorf("chunk %d/%d: %w", idx+1, totalChunks, err)
		}
	}

	finResp, err := c.callRPC(ctx, "aibot_upload_media_finish", map[string]any{
		"upload_id": initBody.UploadID,
	})
	if err != nil {
		return "", err
	}
	var finBody struct {
		MediaID string `json:"media_id"`
	}
	if json.Unmarshal(finResp.Body, &finBody) != nil || finBody.MediaID == "" {
		return "", errors.New("missing media_id in aibot_upload_media_finish response")
	}
	return finBody.MediaID, nil
}

// SendFile uploads data as a file media and sends it to chatID via aibot_send_msg.
func (c *Client) SendFile(ctx context.Context, chatID, filename string, data []byte) error {
	chatID = c.resolveTarget(chatID)
	if chatID == "" {
		return nil
	}
	mediaID, err := c.UploadMedia(ctx, "file", filename, data)
	if err != nil {
		return fmt.Errorf("upload wecom file %s: %w", filename, err)
	}
	frame := map[string]any{
		"cmd":     "aibot_send_msg",
		"headers": map[string]any{"req_id": c.nextReqID()},
		"body": map[string]any{
			"chatid":  chatID,
			"msgtype": "file",
			"file":    map[string]any{"media_id": mediaID},
		},
	}
	return c.send(ctx, frame)
}

// SendStreamReply sends a streaming reply chunk using aibot_respond_msg.
func (c *Client) SendStreamReply(ctx context.Context, reqID, streamID, content string, finish bool) error {
	if reqID == "" || streamID == "" {
		return nil
	}
	frame := map[string]any{
		"cmd":     "aibot_respond_msg",
		"headers": map[string]any{"req_id": reqID},
		"body": map[string]any{
			"msgtype": "stream",
			"stream": map[string]any{
				"id":      streamID,
				"finish":  finish,
				"content": content,
			},
		},
	}
	return c.send(ctx, frame)
}

// preserveWSCaseRT restores RFC 6455 mixed-case Sec-WebSocket-* header keys before
// net/http writes the upgrade request. WeCom's Wwebsvr gateway rejects Go's canonicalized
// "Sec-Websocket-Key" / "Sec-Websocket-Version" with HTTP 404.
type preserveWSCaseRT struct{ base http.RoundTripper }

func (p preserveWSCaseRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	for _, k := range []string{
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Version",
		"Sec-WebSocket-Protocol",
		"Sec-WebSocket-Extensions",
	} {
		canon := http.CanonicalHeaderKey(k)
		if vals, ok := r2.Header[canon]; ok {
			delete(r2.Header, canon)
			r2.Header[k] = vals
		}
	}
	return p.base.RoundTrip(r2)
}

func newWSDialOptions() *websocket.DialOptions {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: preserveWSCaseRT{base: tr}},
		CompressionMode: websocket.CompressionDisabled,
	}
}

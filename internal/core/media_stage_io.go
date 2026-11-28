package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

var (
	errRemoteUploadReadUnsupported = errors.New("源节点版本较旧，不支持跨机读取上传文件")
	unsafeUploadNameRe             = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)
	sha256Hex64Re                  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsValidSHA256Hex reports whether s is a canonical 64-char lowercase hex SHA-256 digest.
func IsValidSHA256Hex(s string) bool {
	return sha256Hex64Re.MatchString(strings.TrimSpace(s))
}

// SanitizeUploadName normalizes an uploaded filename into a safe single-segment basename.
func SanitizeUploadName(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	ext := unsafeUploadNameRe.ReplaceAllString(filepath.Ext(name), "")
	stem := strings.Trim(unsafeUploadNameRe.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), "-"), "-.")
	if stem == "" {
		stem = "file"
	}
	return stem + ext
}

func clipStagedFilename(shaHex, safeName string) string {
	extRunes := []rune(filepath.Ext(safeName))
	if len(extRunes) > 16 {
		extRunes = extRunes[:16]
	}
	ext := string(extRunes)
	stem := strings.TrimSuffix(safeName, filepath.Ext(safeName))
	if stem == "" {
		stem = "file"
	}
	stemRunes := []rune(stem)
	if len(stemRunes) > 64 {
		stem = strings.TrimRight(string(stemRunes[:64]), "-.")
		if stem == "" {
			stem = "file"
		}
	}
	return shaHex + "-" + stem + ext
}

// ValidateUploadDestPath validates a destination path returned by a remote Node upload endpoint.
func ValidateUploadDestPath(p string) error {
	if hasControlRune(p) {
		return fmt.Errorf("目标上传路径包含非法控制字符")
	}
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("目标上传路径无效：%q", p)
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return fmt.Errorf("目标上传路径包含穿越段：%q", p)
		}
	}
	return nil
}

func (e *Engine) validateSameMachineRef(machine, rawPath string) error {
	if machine == "" {
		f, _, _, err := OpenUploadsFile(e.corgiDir, e.dbPath(), rawPath, MaxUploadStageBytes)
		if err != nil {
			return err
		}
		_ = f.Close()
		return nil
	}
	return ValidateUploadDestPath(rawPath)
}

func (e *Engine) stageOneCrossMachineFile(ctx context.Context, src, dst string, ref model.MediaRef, remainingBytes int64) (string, int64, error) {
	safeName := SanitizeUploadName(orStr(ref.Name, filepath.Base(ref.Path)))
	today := time.Now().Format("20060102")
	singleCap := min(int64(MaxUploadStageBytes), remainingBytes)

	// Direction 1: Hub -> Node
	if src == "" && dst != "" {
		return e.pushHubUploadToNode(ctx, dst, ref.Path, safeName, remainingBytes)
	}
	// Direction 2: Node A -> Hub
	if src != "" && dst == "" {
		return e.pullNodeUploadToHub(ctx, src, ref.Path, safeName, today, singleCap)
	}
	// Direction 3: Node A -> Hub (staged) -> Node B (sharing same single-file ctx)
	hubPath, pulled, err := e.pullNodeUploadToHub(ctx, src, ref.Path, safeName, today, singleCap)
	if err != nil {
		return "", 0, err
	}
	destPath, _, err := e.pushHubUploadToNode(ctx, dst, hubPath, safeName, remainingBytes)
	if err != nil {
		return "", pulled, err
	}
	return destPath, pulled, nil
}

func isPermanentStageError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUploadTooLarge) || errors.Is(err, errRemoteUploadReadUnsupported) {
		return true
	}
	msg := err.Error()
	for _, needle := range []string{"符号链接", "受保护", "不在受信任", "文件为空", "非普通文件", "非法", "总限额", "超过单次派发"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (e *Engine) pushHubUploadToNode(ctx context.Context, dst, hubPath, safeName string, remainingBytes int64) (string, int64, error) {
	if e.fleet == nil {
		return "", 0, fmt.Errorf("目标机 %s 离线", dst)
	}
	hc, err := e.fleet.HTTPClient(dst)
	if err != nil {
		return "", 0, err
	}
	f, _, size, err := OpenUploadsFile(e.corgiDir, e.dbPath(), hubPath, MaxUploadStageBytes)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, cpErr := io.Copy(h, io.LimitReader(f, MaxUploadStageBytes+1))
	_ = f.Close()
	if cpErr != nil {
		return "", 0, fmt.Errorf("计算附件哈希失败：%w", cpErr)
	}
	if n == 0 || n > MaxUploadStageBytes {
		return "", 0, fmt.Errorf("附件大小超出 %d MB 限制", MaxUploadStageBytes>>20)
	}
	size = n
	shaHex := hex.EncodeToString(h.Sum(nil))

	// Zero-byte dedup probe (HEAD returns 200 on hit, 404 on miss, 405 on old Nodes -> fallback to POST).
	if hitPath, hit, hErr := headNodeStagedUpload(ctx, hc, shaHex, safeName); hErr != nil {
		return "", 0, hErr
	} else if hit {
		return hitPath, 0, nil
	}
	if size > remainingBytes {
		return "", 0, fmt.Errorf("附件超过单次派发 %d MB 总限额", MaxStageTotalBytes>>20)
	}

	// Re-open via os.Root for streaming POST.
	f2, _, _, err := OpenUploadsFile(e.corgiDir, e.dbPath(), hubPath, MaxUploadStageBytes)
	if err != nil {
		return "", 0, err
	}
	defer f2.Close()

	qs := url.Values{"name": {safeName}, "scope": {"staged"}, "sha256": {shaHex}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/uploads?"+qs.Encode(), io.LimitReader(f2, size))
	if err != nil {
		return "", 0, fmt.Errorf("build upload push request: %w", err)
	}
	req.ContentLength = size
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("上传到节点 %s 失败：%w", dst, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&errBody)
		if errBody.Error != "" {
			return "", 0, fmt.Errorf("节点 %s 拒绝上传：%s", dst, errBody.Error)
		}
		return "", 0, fmt.Errorf("节点 %s 上传失败（HTTP %d）", dst, resp.StatusCode)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("解析节点 %s 上传响应失败：%w", dst, err)
	}
	if err := ValidateUploadDestPath(out.Path); err != nil {
		return "", 0, fmt.Errorf("节点 %s 返回非法路径：%w", dst, err)
	}
	return strings.TrimSpace(out.Path), size, nil
}

func headNodeStagedUpload(ctx context.Context, hc *http.Client, shaHex, safeName string) (string, bool, error) {
	qs := url.Values{"name": {safeName}, "scope": {"staged"}, "sha256": {shaHex}}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://node/api/uploads?"+qs.Encode(), nil)
	if err != nil {
		return "", false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("连接节点检查去重失败：%w", err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		if p := strings.TrimSpace(resp.Header.Get("X-Corgi-Staged-Path")); p != "" && ValidateUploadDestPath(p) == nil {
			return p, true, nil
		}
		return "", false, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("节点去重探测返回异常状态（HTTP %d）", resp.StatusCode)
	}
}

func (e *Engine) pullNodeUploadToHub(ctx context.Context, src, remotePath, safeName, day string, maxBytes int64) (string, int64, error) {
	if e.fleet == nil {
		return "", 0, fmt.Errorf("源机 %s 离线", src)
	}
	hc, err := e.fleet.HTTPClient(src)
	if err != nil {
		return "", 0, err
	}
	body, _ := json.Marshal(map[string]string{"path": remotePath})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/uploads/read", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build remote upload read request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("从节点 %s 读取上传文件失败：%w", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&errBody)
		if errBody.Error == "" {
			return "", 0, errRemoteUploadReadUnsupported
		}
		return "", 0, fmt.Errorf("源节点 %s：%s", src, errBody.Error)
	}
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&errBody)
		if errBody.Error != "" {
			return "", 0, fmt.Errorf("源节点 %s 拒绝读取：%s", src, errBody.Error)
		}
		return "", 0, fmt.Errorf("从节点 %s 读取上传文件失败（HTTP %d）", src, resp.StatusCode)
	}
	destPath, _, n, _, err := StageLocalFileFromReader(e.corgiDir, day, "", safeName, resp.Body, maxBytes)
	if err != nil {
		return "", 0, err
	}
	return destPath, n, nil
}

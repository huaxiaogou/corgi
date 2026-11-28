package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func writeVerifiedTempBinary(body io.Reader, expectedLen int64, dir, wantSHA string, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, ".corgi.update.*")
	if err != nil {
		return "", &UpdateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, err), Err: err}
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	limit := maxUpdateBinarySize
	if limit <= 0 {
		limit = 128 << 20
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, limit+1))
	chmodErr := f.Chmod(mode)
	closeErr := f.Close()
	if copyErr != nil {
		return "", &UpdateError{Code: "upstream_net", Relayable: true, Msg: "传输中断：" + formatUpdateError(copyErr), Err: copyErr}
	}
	if n == 0 {
		return "", &UpdateError{Code: "upstream_net", Relayable: true, Msg: "下载到空文件"}
	}
	if n > limit {
		return "", &UpdateError{Code: "checksum", Relayable: false, Msg: fmt.Sprintf("升级包体积超出上限（%d MiB）", limit>>20)}
	}
	if expectedLen > 0 && n != expectedLen {
		return "", &UpdateError{Code: "upstream_net", Relayable: true, Msg: fmt.Sprintf("下载不完整（已读 %d/%d 字节）", n, expectedLen)}
	}
	if chmodErr != nil {
		return "", &UpdateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, chmodErr), Err: chmodErr}
	}
	if closeErr != nil {
		return "", &UpdateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, closeErr), Err: closeErr}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return "", &UpdateError{
			Code:      "checksum",
			Relayable: true,
			Msg:       fmt.Sprintf("SHA-256 校验不一致（期望 %s，实际 %s）", wantSHA[:12], got[:12]),
		}
	}
	cleanup = false
	return tmpPath, nil
}

// stageReleaseBinary probes write permissions in destExe's directory, downloads the release
// binary with retry/SHA-256 verification, and smoke-tests `<tmp> version` == `corgi <tag>`.
func (m *Manager) stageReleaseBinary(ctx context.Context, tag, destExe string, preferIPv4 bool) (string, error) {
	dir := filepath.Dir(destExe)
	if err := probeWritableDir(dir); err != nil {
		return "", err
	}
	asset := "corgi-" + runtime.GOOS + "-" + runtime.GOARCH
	wantSHA, usedIPv4, err := m.downloadChecksums(ctx, tag, asset, preferIPv4)
	if err != nil {
		return "", err
	}
	tmpPath, err := m.downloadAssetToDir(ctx, tag, asset, dir, wantSHA, usedIPv4, 0o755)
	if err != nil {
		return "", err
	}
	if err := verifyBinaryVersion(ctx, tmpPath, tag); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// stageStreamedBinary writes a Hub-relayed binary stream into destExe's directory,
// verifies SHA-256, and smoke-tests `<tmp> version` == `corgi <tag>`.
func stageStreamedBinary(ctx context.Context, body io.Reader, expectedLen int64, tag, wantSHA, destExe string) (string, error) {
	dir := filepath.Dir(destExe)
	if err := probeWritableDir(dir); err != nil {
		return "", err
	}
	tmpPath, err := writeVerifiedTempBinary(body, expectedLen, dir, wantSHA, 0o755)
	if err != nil {
		return "", err
	}
	if err := verifyBinaryVersion(ctx, tmpPath, tag); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func probeWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".corgi.probe.*")
	if err != nil {
		return &UpdateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, err), Err: err}
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func verifyBinaryVersion(parentCtx context.Context, binPath, expectedTag string) error {
	vctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, binPath, "version").CombinedOutput()
	got := strings.TrimSpace(string(out))
	want := "corgi " + expectedTag
	if err != nil {
		return &UpdateError{
			Code:      "verify",
			Relayable: false,
			Msg:       fmt.Sprintf("新版本在这台电脑上无法运行（%v %s）", err, got),
			Err:       err,
		}
	}
	if got != want {
		return &UpdateError{
			Code:      "verify",
			Relayable: false,
			Msg:       fmt.Sprintf("新版本自检输出不匹配（期望 %q，实际 %q）", want, got),
		}
	}
	return nil
}

func formatLocalIOErr(dir string, err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Sprintf("目录无写权限，无法写入新版本（%s）", dir)
	case errors.Is(err, syscall.ENOSPC):
		return fmt.Sprintf("磁盘空间不足，无法写入新版本（%s）", dir)
	default:
		return fmt.Sprintf("写入新版本文件失败（%s）：%v", dir, err)
	}
}

func formatUpdateError(err error) string {
	if err == nil {
		return ""
	}
	var ue *UpdateError
	if errors.As(err, &ue) {
		return ue.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "连接 GitHub 超时（网络较慢或需要代理）"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "连接 GitHub 超时（网络较慢或需要代理）"
	}
	return strings.TrimSpace(err.Error())
}

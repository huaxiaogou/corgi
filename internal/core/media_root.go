package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// MaxUploadStageBytes caps each uploaded or cross-machine staged file at 25 MiB.
	MaxUploadStageBytes = 25 << 20
	// MaxStageFilesPerDispatch caps the number of files staged for a single dispatch/steer/direct turn.
	MaxStageFilesPerDispatch = 10
	// MaxStageTotalBytes caps total cross-machine bytes transferred in one dispatch/steer/direct turn.
	MaxStageTotalBytes = 50 << 20

	stageSingleFileTimeout = 30 * time.Second
	stageBatchTimeout      = 90 * time.Second
)

// ErrUploadTooLarge is wrapped whenever an upload exceeds its byte cap so HTTP handlers reliably return 413.
var ErrUploadTooLarge = errors.New("上传文件超过大小限制")

// openUploadSubRoot descends one directory segment inside parent without following symlinks and
// verifies inode identity via os.SameFile after OpenRoot to close directory-swap TOCTOU windows.
func openUploadSubRoot(parent *os.Root, seg string, create bool) (*os.Root, error) {
	lfi, err := parent.Lstat(seg)
	if err != nil {
		if !os.IsNotExist(err) || !create {
			return nil, err
		}
		if mkErr := parent.Mkdir(seg, 0o700); mkErr != nil && !os.IsExist(mkErr) {
			return nil, mkErr
		}
		lfi, err = parent.Lstat(seg)
		if err != nil {
			return nil, err
		}
	}
	if lfi.Mode()&os.ModeSymlink != 0 || !lfi.IsDir() {
		return nil, fmt.Errorf("目录 %q 为符号链接或非目录，已拒绝", seg)
	}
	child, err := parent.OpenRoot(seg)
	if err != nil {
		return nil, err
	}
	childFi, err := child.Stat(".")
	if err != nil || !childFi.IsDir() || !os.SameFile(lfi, childFi) {
		_ = child.Close()
		return nil, fmt.Errorf("目录 %q 在打开期间发生变化，已拒绝", seg)
	}
	return child, nil
}

func isSameProtectedDBFile(dbPath string, fi os.FileInfo) bool {
	if strings.TrimSpace(dbPath) == "" || fi == nil {
		return false
	}
	for _, cand := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if dbi, err := os.Stat(cand); err == nil && os.SameFile(dbi, fi) {
			return true
		}
	}
	return false
}

// OpenUploadsSubRoot opens <corgiDir>/uploads/<parts...> via Go 1.25 os.Root without following symlinks.
func OpenUploadsSubRoot(corgiDir string, create bool, parts ...string) (*os.Root, error) {
	if strings.TrimSpace(corgiDir) == "" {
		return nil, fmt.Errorf("corgi dir unavailable")
	}
	if create {
		if err := os.MkdirAll(corgiDir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir corgi dir: %w", err)
		}
	}
	cur, err := os.OpenRoot(corgiDir)
	if err != nil {
		return nil, fmt.Errorf("open corgi root: %w", err)
	}
	allParts := append([]string{"uploads"}, parts...)
	for _, p := range allParts {
		next, subErr := openUploadSubRoot(cur, p, create)
		_ = cur.Close()
		if subErr != nil {
			return nil, subErr
		}
		cur = next
	}
	return cur, nil
}

// OpenUploadsRoot opens <corgiDir>/uploads anchored via Go 1.25 os.Root without following symlinks.
func OpenUploadsRoot(corgiDir string, create bool) (*os.Root, error) {
	return OpenUploadsSubRoot(corgiDir, create)
}

func relativeWithinSubdir(corgiDir, sub, absPath string) (string, error) {
	if hasControlRune(absPath) {
		return "", fmt.Errorf("路径包含非法控制字符")
	}
	raw := strings.TrimSpace(absPath)
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("仅允许绝对路径：%q", absPath)
	}
	for _, seg := range strings.Split(filepath.ToSlash(raw), "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("路径禁止包含 . 或 .. 段：%q", absPath)
		}
	}
	cleanAbs := filepath.Clean(raw)
	baseDir := filepath.Clean(filepath.Join(corgiDir, sub))
	if rel, ok := strictRelPath(baseDir, cleanAbs); ok {
		return rel, nil
	}
	if realBase, err := filepath.EvalSymlinks(baseDir); err == nil {
		if rel, ok := strictRelPath(filepath.Clean(realBase), cleanAbs); ok {
			return rel, nil
		}
	}
	return "", fmt.Errorf("路径不在受信任的 %s 目录内", sub)
}

func strictRelPath(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", false
	}
	sl := filepath.ToSlash(rel)
	for _, seg := range strings.Split(sl, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return sl, true
}

func openFileInSubdirRoot(corgiDir, dbPath, sub, absPath string, maxBytes int64) (*os.File, string, int64, error) {
	if strings.TrimSpace(corgiDir) == "" {
		return nil, "", 0, fmt.Errorf("corgi dir unavailable")
	}
	if dbPath != "" && IsProtectedHubDBPath(dbPath, absPath) {
		return nil, "", 0, fmt.Errorf("禁止读取受保护数据库文件")
	}
	rel, err := relativeWithinSubdir(corgiDir, sub, absPath)
	if err != nil {
		return nil, "", 0, err
	}
	segs := strings.Split(rel, "/")
	if len(segs) == 0 || strings.HasPrefix(segs[len(segs)-1], ".") {
		return nil, "", 0, fmt.Errorf("非法文件名")
	}
	cur, err := os.OpenRoot(corgiDir)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open corgi root: %w", err)
	}
	for _, dirSeg := range append([]string{sub}, segs[:len(segs)-1]...) {
		next, sErr := openUploadSubRoot(cur, dirSeg, false)
		_ = cur.Close()
		if sErr != nil {
			return nil, "", 0, sErr
		}
		cur = next
	}
	defer cur.Close()

	base := segs[len(segs)-1]
	lfi, err := cur.Lstat(base)
	if err != nil {
		return nil, "", 0, err
	}
	if lfi.Mode()&os.ModeSymlink != 0 || !lfi.Mode().IsRegular() {
		return nil, "", 0, fmt.Errorf("拒绝读取符号链接或非普通文件")
	}
	if lfi.Size() <= 0 {
		return nil, "", 0, fmt.Errorf("文件为空")
	}
	if maxBytes > 0 && lfi.Size() > maxBytes {
		return nil, "", 0, fmt.Errorf("%w（上限 %d MB）", ErrUploadTooLarge, maxBytes>>20)
	}
	f, err := cur.Open(base)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open upload file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) || fi.Size() <= 0 || (maxBytes > 0 && fi.Size() > maxBytes) || isSameProtectedDBFile(dbPath, fi) {
		_ = f.Close()
		return nil, "", 0, fmt.Errorf("文件句柄校验失败或命中受保护文件")
	}
	return f, base, fi.Size(), nil
}

// OpenUploadsFile safely opens one regular non-symlink file inside <corgiDir>/uploads/ via os.Root.
func OpenUploadsFile(corgiDir, dbPath, absPath string, maxBytes int64) (*os.File, string, int64, error) {
	if maxBytes <= 0 {
		maxBytes = MaxUploadStageBytes
	}
	return openFileInSubdirRoot(corgiDir, dbPath, "uploads", absPath, maxBytes)
}

// readLocalTrustedMedia reads a bounded Hub-local media file via os.Root (uploads/wecom-media/wecom).
func readLocalTrustedMedia(corgiDir, dbPath, rawPath string, maxBytes int64) ([]byte, bool) {
	if strings.TrimSpace(corgiDir) == "" {
		return nil, false
	}
	for _, sub := range []string{"uploads", "wecom-media", "wecom"} {
		f, _, _, err := openFileInSubdirRoot(corgiDir, dbPath, sub, rawPath, maxBytes)
		if err != nil {
			continue
		}
		data, rErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
		_ = f.Close()
		if rErr == nil && len(data) > 0 && int64(len(data)) <= maxBytes {
			return data, true
		}
		return nil, false
	}
	return nil, false
}

func randomHexBytes(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WriteRegularUploadAtomic writes a normal upload into <corgiDir>/uploads/<day>/<prefix>-<safeName>
// via hidden temp file + non-overwriting atomic Link inside os.Root so existing uploads are never clobbered.
func WriteRegularUploadAtomic(corgiDir, day, prefix, safeName string, r io.Reader, maxBytes int64) (string, int64, error) {
	if maxBytes <= 0 {
		maxBytes = MaxUploadStageBytes
	}
	root, err := OpenUploadsSubRoot(corgiDir, true, day)
	if err != nil {
		return "", 0, err
	}
	defer root.Close()

	tmpName := ".tmp-" + randomHexBytes(6)
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create upload temp file: %w", err)
	}
	n, cpErr := io.Copy(f, io.LimitReader(r, maxBytes+1))
	cErr := f.Close()
	if cpErr != nil || cErr != nil || n > maxBytes {
		_ = root.Remove(tmpName)
		if n > maxBytes {
			return "", 0, fmt.Errorf("%w（上限 %d MB）", ErrUploadTooLarge, maxBytes>>20)
		}
		if cpErr != nil {
			return "", 0, cpErr
		}
		return "", 0, cErr
	}
	cleanName := SanitizeUploadName(safeName)
	candPrefix := prefix
	for attempt := 0; attempt < 6; attempt++ {
		if candPrefix == "" {
			candPrefix = randomHexBytes(3)
		}
		finalName := candPrefix + "-" + cleanName
		if linkErr := root.Link(tmpName, finalName); linkErr == nil {
			_ = root.Remove(tmpName)
			return filepath.Join(corgiDir, "uploads", day, finalName), n, nil
		} else if os.IsExist(linkErr) {
			candPrefix = randomHexBytes(3)
			continue
		} else {
			_ = root.Remove(tmpName)
			return "", 0, fmt.Errorf("commit upload file: %w", linkErr)
		}
	}
	_ = root.Remove(tmpName)
	return "", 0, fmt.Errorf("commit upload file: filename collision")
}

// LookupStagedFile checks whether <corgiDir>/uploads/staged/<day>/<sha64>-<safeName> exists and
// refreshes its mtime via os.Root; only returns hit when mtime renewal succeeds.
func LookupStagedFile(corgiDir, day, shaHex, safeName string) (string, int64, bool) {
	shaHex = strings.ToLower(strings.TrimSpace(shaHex))
	if !IsValidSHA256Hex(shaHex) {
		return "", 0, false
	}
	root, err := OpenUploadsSubRoot(corgiDir, false, "staged", day)
	if err != nil {
		return "", 0, false
	}
	defer root.Close()

	finalName := clipStagedFilename(shaHex, SanitizeUploadName(safeName))
	fi, err := root.Lstat(finalName)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() || fi.Size() <= 0 {
		return "", 0, false
	}
	now := time.Now()
	if err := root.Chtimes(finalName, now, now); err != nil {
		return "", 0, false
	}
	return filepath.Join(corgiDir, "uploads", "staged", day, finalName), fi.Size(), true
}

// StageLocalFileFromReader streams r into <corgiDir>/uploads/staged/<day>/<sha64>-<safeName>
// via temp file + SHA-256 verification + atomic rename inside os.Root.
func StageLocalFileFromReader(corgiDir, day, expectedSHA, safeName string, r io.Reader, maxBytes int64) (destPath, actualSHA string, size int64, deduped bool, err error) {
	if maxBytes <= 0 {
		maxBytes = MaxUploadStageBytes
	}
	expectedSHA = strings.ToLower(strings.TrimSpace(expectedSHA))
	if expectedSHA != "" && !IsValidSHA256Hex(expectedSHA) {
		return "", "", 0, false, fmt.Errorf("无效的 sha256 参数")
	}
	root, err := OpenUploadsSubRoot(corgiDir, true, "staged", day)
	if err != nil {
		return "", "", 0, false, err
	}
	defer root.Close()

	tmpName := ".tmp-" + randomHexBytes(6)
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", 0, false, fmt.Errorf("create staged temp file: %w", err)
	}
	h := sha256.New()
	n, cpErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes+1))
	cErr := f.Close()
	if cpErr != nil || cErr != nil || n == 0 || n > maxBytes {
		_ = root.Remove(tmpName)
		if n > maxBytes {
			return "", "", 0, false, fmt.Errorf("%w（上限 %d MB）", ErrUploadTooLarge, maxBytes>>20)
		}
		if n == 0 {
			return "", "", 0, false, fmt.Errorf("上传文件内容为空")
		}
		if cpErr != nil {
			return "", "", 0, false, cpErr
		}
		return "", "", 0, false, cErr
	}
	actualSHA = hex.EncodeToString(h.Sum(nil))
	if expectedSHA != "" && actualSHA != expectedSHA {
		_ = root.Remove(tmpName)
		return "", "", 0, false, fmt.Errorf("文件 SHA-256 校验不一致")
	}

	finalName := clipStagedFilename(actualSHA, SanitizeUploadName(safeName))
	destPath = filepath.Join(corgiDir, "uploads", "staged", day, finalName)
	if fi, sErr := root.Lstat(finalName); sErr == nil && fi.Mode()&os.ModeSymlink == 0 && fi.Mode().IsRegular() && fi.Size() == n {
		_ = root.Remove(tmpName)
		now := time.Now()
		_ = root.Chtimes(finalName, now, now)
		return destPath, actualSHA, fi.Size(), true, nil
	} else if sErr == nil && (fi.Mode()&os.ModeSymlink != 0 || fi.IsDir()) {
		_ = root.Remove(tmpName)
		return "", "", 0, false, errors.New("目标暂存路径为符号链接或目录，已拒绝覆写")
	}
	if err := root.Rename(tmpName, finalName); err != nil {
		_ = root.Remove(tmpName)
		return "", "", 0, false, fmt.Errorf("commit staged file: %w", err)
	}
	return destPath, actualSHA, n, false, nil
}

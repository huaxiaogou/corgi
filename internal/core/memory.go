package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// UpFrontBudgetBytes caps the files loaded upfront on every turn (~10 KB).
	UpFrontBudgetBytes  = 10 << 10
	maxMemoryWriteBytes = 32 << 10
	maxMemoryReadBytes  = 256 << 10
)

// MemoryFile represents one markdown file under memory/ or dreams/.
type MemoryFile struct {
	Name string    `json:"name"`
	Text string    `json:"text"`
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
}

// MemorySnapshot is returned to the Web UI memory editor.
type MemorySnapshot struct {
	Dir         string       `json:"dir"`
	Files       []MemoryFile `json:"files"`
	Dreams      []MemoryFile `json:"dreams"`
	UpFrontSize int64        `json:"up_front"`
	Budget      int64        `json:"budget"`
	Dreaming    bool         `json:"dreaming"`
	LastDream   *time.Time   `json:"last_dream,omitempty"`
	NextDream   *time.Time   `json:"next_dream,omitempty"`
}

// MemoryManager owns ~/.corgi/router/ (memory/, dreams/) behind Go 1.25 os.Root sandboxes.
type MemoryManager struct{ routerDir string }

func NewMemoryManager(corgiDir string) *MemoryManager {
	return &MemoryManager{routerDir: filepath.Join(corgiDir, "router")}
}

func (m *MemoryManager) RouterDir() string { return m.routerDir }
func (m *MemoryManager) MemDir() string    { return filepath.Join(m.routerDir, "memory") }
func (m *MemoryManager) DreamsDir() string { return filepath.Join(m.routerDir, "dreams") }

// Ensure creates the layered memory directory skeleton without overwriting existing files.
func (m *MemoryManager) Ensure() error {
	for _, d := range []string{filepath.Join(m.MemDir(), "notes"), m.DreamsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	root, err := os.OpenRoot(m.MemDir())
	if err != nil {
		return fmt.Errorf("open memory root: %w", err)
	}
	defer root.Close()
	for rel, content := range map[string]string{
		"instructions.md": "# 主人的长期指令与偏好\n",
		"notes/index.md":  "# 笔记索引\n\n每个主题文件一行说明。\n",
	} {
		if _, err := root.Lstat(rel); errors.Is(err, os.ErrNotExist) {
			if err := writeRootAtomic(root, rel, []byte(content), 0o600); err != nil {
				return fmt.Errorf("seed %s: %w", rel, err)
			}
		}
	}
	return nil
}

// UpFrontSize computes total bytes of up-front files (instructions.md and notes/index.md).
func (m *MemoryManager) UpFrontSize() int64 {
	root, err := os.OpenRoot(m.MemDir())
	if err != nil {
		return 0
	}
	defer root.Close()
	var n int64
	for _, rel := range []string{"instructions.md", "notes/index.md"} {
		if fi, err := root.Lstat(rel); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}

func isSingleLevelMD(name string) bool {
	return name != "" && name != "." && name != ".md" &&
		!strings.Contains(name, "/") && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".md")
}

// ValidateMemoryWritePath enforces the strict write whitelist: instructions.md or single-level notes/<slug>.md.
func ValidateMemoryWritePath(raw string) (string, error) {
	sl := filepath.ToSlash(strings.TrimSpace(raw))
	if sl == "" || strings.HasPrefix(sl, "/") || strings.Contains(sl, "..") || strings.ContainsRune(sl, '\x00') {
		return "", fmt.Errorf("非法路径：仅允许写入 instructions.md 或单层 notes/<slug>.md")
	}
	rel := path.Clean(strings.TrimPrefix(sl, "memory/"))
	if rel == "instructions.md" {
		return rel, nil
	}
	if rest, ok := strings.CutPrefix(rel, "notes/"); ok && isSingleLevelMD(rest) {
		return rel, nil
	}
	return "", fmt.Errorf("仅允许写入 instructions.md 或单层 notes/<slug>.md（禁止写 messages.md 或其他子目录）")
}

func validateMemoryReadPath(raw string) (dirArea, rel string, err error) {
	sl := filepath.ToSlash(strings.TrimSpace(raw))
	if sl == "" || strings.HasPrefix(sl, "/") || strings.Contains(sl, "..") || strings.ContainsRune(sl, '\x00') {
		return "", "", fmt.Errorf("非法路径：仅允许读取 memory/ 或 dreams/ 下的相对 .md 文件")
	}
	if rest, ok := strings.CutPrefix(sl, "dreams/"); ok {
		clean := path.Clean(rest)
		if !isSingleLevelMD(clean) {
			return "", "", fmt.Errorf("非法 dreams 日志路径：%s", raw)
		}
		return "dreams", clean, nil
	}
	clean := path.Clean(strings.TrimPrefix(sl, "memory/"))
	if clean == "" || clean == "." || clean == "messages.md" || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") || !strings.HasSuffix(clean, ".md") {
		return "", "", fmt.Errorf("仅允许读取 memory/ 或 dreams/ 下的 .md 文件")
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return "", "", fmt.Errorf("非法记忆文件路径：%s", raw)
		}
	}
	return "memory", clean, nil
}

// ReadFile reads one .md file inside memory/ or dreams/ via os.Root (≤256KB, no symlinks).
func (m *MemoryManager) ReadFile(rawPath string) ([]byte, error) {
	area, rel, err := validateMemoryReadPath(rawPath)
	if err != nil {
		return nil, err
	}
	dir := m.MemDir()
	if area == "dreams" {
		dir = m.DreamsDir()
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("记忆目录不可用：%w", err)
	}
	defer root.Close()
	return readRootRegularFile(root, rel, maxMemoryReadBytes)
}

// ReadNote resolves a short note reference ("deploy", "notes/deploy.md", "instructions.md") safely.
func (m *MemoryManager) ReadNote(target string) ([]byte, error) {
	sl := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(target)), "/")
	if sl == "" || strings.Contains(sl, "..") {
		return nil, fmt.Errorf("非法笔记路径")
	}
	rel := path.Clean(strings.TrimPrefix(sl, "memory/"))
	if rel != "instructions.md" && !strings.HasPrefix(rel, "notes/") {
		rel = "notes/" + rel
	}
	if !strings.HasSuffix(rel, ".md") {
		rel += ".md"
	}
	return m.ReadFile(rel)
}

// SaveFile atomically writes instructions.md or single-level notes/<slug>.md inside memory/ (≤32KB).
func (m *MemoryManager) SaveFile(relPath, content string) error {
	rel, err := ValidateMemoryWritePath(relPath)
	if err != nil {
		return err
	}
	if len(content) > maxMemoryWriteBytes {
		return fmt.Errorf("单份记忆文件不能超过 %dKB", maxMemoryWriteBytes>>10)
	}
	if err := os.MkdirAll(filepath.Join(m.MemDir(), "notes"), 0o700); err != nil {
		return fmt.Errorf("mkdir memory: %w", err)
	}
	root, err := os.OpenRoot(m.MemDir())
	if err != nil {
		return fmt.Errorf("open memory root: %w", err)
	}
	defer root.Close()
	if strings.HasPrefix(rel, "notes/") {
		if fi, lerr := root.Lstat("notes"); lerr == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
			return fmt.Errorf("notes 目录符号链接越界，已拒绝写入")
		}
	}
	return writeRootAtomic(root, rel, []byte(strings.TrimRight(content, "\n")+"\n"), 0o600)
}

// SaveDreamJournal atomically writes dreams/<name> inside dreams/ via os.Root (≤32KB).
func (m *MemoryManager) SaveDreamJournal(name, content string) error {
	clean := path.Clean(filepath.ToSlash(strings.TrimSpace(name)))
	if !isSingleLevelMD(clean) {
		return fmt.Errorf("非法做梦日志文件名：%s", name)
	}
	if len(content) > maxMemoryWriteBytes {
		return fmt.Errorf("做梦日志不能超过 %dKB", maxMemoryWriteBytes>>10)
	}
	if err := os.MkdirAll(m.DreamsDir(), 0o700); err != nil {
		return fmt.Errorf("mkdir dreams: %w", err)
	}
	root, err := os.OpenRoot(m.DreamsDir())
	if err != nil {
		return fmt.Errorf("open dreams root: %w", err)
	}
	defer root.Close()
	return writeRootAtomic(root, clean, []byte(content), 0o600)
}

// StatDreamJournal stats one regular journal file inside dreams/ via os.Root.
func (m *MemoryManager) StatDreamJournal(name string) (os.FileInfo, error) {
	root, err := os.OpenRoot(m.DreamsDir())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fi, err := root.Lstat(name)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular dream journal")
	}
	return fi, nil
}

func walkMemoryMD(root *os.Root, fn func(rel string)) {
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") && !strings.HasPrefix(path.Base(p), ".") && p != "messages.md" {
			fn(p)
		}
		return nil
	})
}

// ListFiles walks memory/ via os.Root and returns regular .md files sorted by path (skipping messages.md).
func (m *MemoryManager) ListFiles() []MemoryFile {
	root, err := os.OpenRoot(m.MemDir())
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []MemoryFile
	walkMemoryMD(root, func(p string) {
		if b, rErr := readRootRegularFile(root, p, maxMemoryReadBytes); rErr == nil {
			if fi, sErr := root.Lstat(p); sErr == nil {
				out = append(out, MemoryFile{Name: p, Text: string(b), Size: fi.Size(), Mod: fi.ModTime()})
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListFileNames returns sorted relative .md paths under memory/ (skipping symlinks and messages.md).
func (m *MemoryManager) ListFileNames() []string {
	root, err := os.OpenRoot(m.MemDir())
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []string
	walkMemoryMD(root, func(p string) { out = append(out, p) })
	sort.Strings(out)
	return out
}

// ListRecentDreamNames returns up to limit newest dream journal filenames under dreams/.
func (m *MemoryManager) ListRecentDreamNames(limit int) []string {
	root, err := os.OpenRoot(m.DreamsDir())
	if err != nil || limit <= 0 {
		return nil
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil
	}
	var names []string
	for _, en := range entries {
		if !en.IsDir() && en.Type()&os.ModeSymlink == 0 && isSingleLevelMD(en.Name()) {
			names = append(names, en.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	return names[:min(len(names), limit)]
}

// ListDreams returns up to 30 most recent dream journals (newest first) via os.Root.
func (m *MemoryManager) ListDreams() []MemoryFile {
	names := m.ListRecentDreamNames(30)
	root, err := os.OpenRoot(m.DreamsDir())
	if err != nil || len(names) == 0 {
		return nil
	}
	defer root.Close()
	out := make([]MemoryFile, 0, len(names))
	for _, name := range names {
		if b, rErr := readRootRegularFile(root, name, maxMemoryReadBytes); rErr == nil {
			if fi, sErr := root.Lstat(name); sErr == nil {
				out = append(out, MemoryFile{Name: name, Text: string(b), Size: fi.Size(), Mod: fi.ModTime()})
			}
		}
	}
	return out
}

func readRootRegularFile(root *os.Root, rel string, maxBytes int64) ([]byte, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无法读取：%s", rel)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("目标文件是符号链接，已拒绝读取")
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("目标不是普通文件：%s", rel)
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		return nil, fmt.Errorf("文件超过 %dKB 上限", maxBytes>>10)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败：%w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%w", err)
	}
	if maxBytes > 0 && int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("文件超过 %dKB 上限", maxBytes>>10)
	}
	return b, nil
}

func writeRootAtomic(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	if fi, err := root.Lstat(rel); err == nil && (fi.Mode()&os.ModeSymlink != 0 || fi.IsDir()) {
		return fmt.Errorf("目标文件是符号链接或目录，已拒绝覆盖")
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".corgi-tmp-" + hex.EncodeToString(rnd[:])
	if dir := path.Dir(rel); dir != "." && dir != "" {
		tmp = path.Join(dir, tmp)
	}
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

func oneLineClip(s string, maxRunes int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), maxRunes, "…")
}

func clipRunes(s string, maxRunes int, suffix string) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + suffix
}

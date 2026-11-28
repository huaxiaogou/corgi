package core

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UpFrontBudgetBytes caps the files loaded upfront on every turn (~10 KB).
const UpFrontBudgetBytes = 10 << 10

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

// MemoryManager owns ~/.corgi/router/ (memory/, dreams/).
type MemoryManager struct {
	routerDir string
}

// NewMemoryManager creates a MemoryManager rooted at corgiDir/router.
func NewMemoryManager(corgiDir string) *MemoryManager {
	return &MemoryManager{routerDir: filepath.Join(corgiDir, "router")}
}

// RouterDir returns the router workspace path.
func (m *MemoryManager) RouterDir() string { return m.routerDir }

// MemDir returns the memory directory path.
func (m *MemoryManager) MemDir() string { return filepath.Join(m.routerDir, "memory") }

// DreamsDir returns the nightly dream journal directory.
func (m *MemoryManager) DreamsDir() string { return filepath.Join(m.routerDir, "dreams") }

// Ensure creates the layered memory directory skeleton without overwriting existing files.
func (m *MemoryManager) Ensure() error {
	dirs := []string{
		filepath.Join(m.MemDir(), "notes"),
		m.DreamsDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	seeds := map[string]string{
		"instructions.md": "# 主人的长期指令与偏好\n",
		"notes/index.md":  "# 笔记索引\n\n每个主题文件一行说明。\n",
	}
	for rel, content := range seeds {
		p := filepath.Join(m.MemDir(), rel)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				return fmt.Errorf("seed %s: %w", p, err)
			}
		}
	}
	return nil
}

// UpFrontSize computes total bytes of up-front files (instructions and notes/index).
func (m *MemoryManager) UpFrontSize() int64 {
	var n int64
	add := func(rel string) {
		if fi, err := os.Stat(filepath.Join(m.MemDir(), rel)); err == nil {
			n += fi.Size()
		}
	}
	add("instructions.md")
	add("notes/index.md")
	return n
}

// ListFiles walks memory/ and returns markdown files sorted by path (skipping legacy root messages.md).
func (m *MemoryManager) ListFiles() []MemoryFile {
	var out []MemoryFile
	root := m.MemDir()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		sl := filepath.ToSlash(rel)
		if sl == "messages.md" {
			return nil
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil
		}
		b, _ := os.ReadFile(p)
		out = append(out, MemoryFile{
			Name: sl,
			Text: string(b),
			Size: fi.Size(),
			Mod:  fi.ModTime(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListDreams returns up to 30 most recent dream journals (newest first).
func (m *MemoryManager) ListDreams() []MemoryFile {
	matches, _ := filepath.Glob(filepath.Join(m.DreamsDir(), "*.md"))
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	if len(matches) > 30 {
		matches = matches[:30]
	}
	out := make([]MemoryFile, 0, len(matches))
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		b, _ := os.ReadFile(p)
		out = append(out, MemoryFile{Name: filepath.Base(p), Text: string(b), Size: fi.Size(), Mod: fi.ModTime()})
	}
	return out
}

// SaveFile writes a markdown file safely inside memory/ (disallowing traversal and messages.md).
func (m *MemoryManager) SaveFile(relPath, content string) error {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relPath)))
	if !strings.HasSuffix(clean, ".md") || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || clean == "messages.md" {
		return fmt.Errorf("只允许修改 memory/ 下的 .md 文件")
	}
	if len(content) > 256<<10 {
		return fmt.Errorf("文件内容不能超过 256 KB")
	}
	target := filepath.Join(m.MemDir(), clean)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	normalized := strings.TrimRight(content, "\n") + "\n"
	return writeFileAtomic(target, []byte(normalized), 0o600)
}

func oneLineClip(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

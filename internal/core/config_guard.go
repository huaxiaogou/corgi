package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	configProposalKVPrefix = "cfg_proposal:"
	configProposalTTL      = 15 * time.Minute
)

func effectiveProtectedCorgiDir(corgiDir, home string) string {
	cd := strings.TrimSpace(corgiDir)
	if cd == "" {
		return ""
	}
	cleanCD := filepath.Clean(cd)
	if h := strings.TrimSpace(home); h != "" && cleanCD == filepath.Clean(h) {
		return ""
	}
	return cleanCD
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func pathEqualOrUnder(candidate, root string) bool {
	c := strings.ToLower(filepath.ToSlash(filepath.Clean(candidate)))
	r := strings.ToLower(filepath.ToSlash(filepath.Clean(root)))
	if r == "" || r == "." || r == "/" {
		return false
	}
	return c == r || strings.HasPrefix(c, r+"/")
}

func isManagedWorktreePath(p, corgiDir string) bool {
	if strings.TrimSpace(p) == "" || strings.TrimSpace(corgiDir) == "" {
		return false
	}
	sl := filepath.ToSlash(strings.TrimSpace(p))
	for _, seg := range strings.Split(sl, "/") {
		if seg == ".." {
			return false
		}
	}
	wtRoot := filepath.Join(filepath.Clean(corgiDir), "worktrees")
	cleanP := filepath.Clean(p)
	if strings.HasPrefix(strings.ToLower(filepath.ToSlash(cleanP)), strings.ToLower(filepath.ToSlash(wtRoot))+"/") {
		return true
	}
	if realWT, err := filepath.EvalSymlinks(wtRoot); err == nil {
		realWT = filepath.Clean(realWT)
		if strings.HasPrefix(strings.ToLower(filepath.ToSlash(cleanP)), strings.ToLower(filepath.ToSlash(realWT))+"/") {
			return true
		}
		if realP, pErr := filepath.EvalSymlinks(cleanP); pErr == nil {
			realP = filepath.Clean(realP)
			if strings.HasPrefix(strings.ToLower(filepath.ToSlash(realP)), strings.ToLower(filepath.ToSlash(realWT))+"/") {
				return true
			}
		}
	}
	return false
}

// ValidateProjectDir validates that raw is a clean absolute or ~/... project directory outside
// sensitive OS/credential directories and outside the active corgiDir.
func ValidateProjectDir(raw, corgiDir string) (string, error) {
	if hasControlRune(raw) {
		return "", fmt.Errorf("项目路径包含非法控制字符")
	}
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("项目路径不能为空")
	}
	if p == "~" {
		return "", fmt.Errorf("项目路径不能是裸 HOME 目录 ~，请指定具体子目录")
	}
	if strings.HasPrefix(p, "~") {
		if !strings.HasPrefix(p, "~/") {
			return "", fmt.Errorf("不支持 ~user 形式路径，请使用 ~/ 或绝对路径")
		}
		relRaw := strings.TrimPrefix(filepath.ToSlash(p), "~/")
		for _, seg := range strings.Split(relRaw, "/") {
			if seg == ".." {
				return "", fmt.Errorf("项目路径禁止包含 .. 穿越段")
			}
		}
		cleanRel := path.Clean(relRaw)
		if cleanRel == "" || cleanRel == "." || strings.HasPrefix(cleanRel, "..") || strings.HasPrefix(cleanRel, "/") {
			return "", fmt.Errorf("项目路径无效：%s", raw)
		}
		if err := checkSensitiveSegments(cleanRel); err != nil {
			return "", err
		}
		return "~/" + cleanRel, nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("项目路径必须为绝对路径或 ~/ 开头：%s", p)
	}
	sl := filepath.ToSlash(p)
	for _, seg := range strings.Split(sl, "/") {
		if seg == ".." {
			return "", fmt.Errorf("项目路径禁止包含 .. 穿越段")
		}
	}
	clean := path.Clean(sl)
	if clean == "/" || clean == "." {
		return "", fmt.Errorf("项目路径不能为系统根目录 /")
	}
	if err := checkSensitiveAbsolutePath(clean, corgiDir); err != nil {
		return "", err
	}
	return clean, nil
}

// ValidateWorkerCwd validates a worker_cwd setting (allowing "" and "~", otherwise enforcing ValidateProjectDir).
func ValidateWorkerCwd(raw, corgiDir string) (string, error) {
	if hasControlRune(raw) {
		return "", fmt.Errorf("工作目录包含非法控制字符")
	}
	p := strings.TrimSpace(raw)
	if p == "" || p == "~" {
		return p, nil
	}
	return ValidateProjectDir(p, corgiDir)
}

func checkSensitiveSegments(sl string) error {
	for _, seg := range strings.Split(strings.ToLower(sl), "/") {
		switch seg {
		case ".ssh", ".aws", ".gnupg", ".kube", ".docker", ".corgi":
			return fmt.Errorf("项目路径不允许指向敏感目录（%s）", seg)
		}
	}
	return nil
}

func checkSensitiveAbsolutePath(clean, corgiDir string) error {
	low := strings.ToLower(clean)
	for _, exact := range []string{"/", "/var", "/private", "/private/var"} {
		if low == exact {
			return fmt.Errorf("项目路径不允许指向系统目录（%s）", clean)
		}
	}
	blockedPrefixes := []string{
		"/etc", "/private/etc",
		"/usr", "/bin", "/sbin", "/boot", "/proc", "/sys", "/dev", "/root",
		"/system", "/library",
		"/var/db", "/private/var/db",
		"/var/log", "/private/var/log",
		"/var/run", "/private/var/run",
		"/var/root", "/private/var/root",
		"/var/lib", "/private/var/lib",
		"/var/spool", "/private/var/spool",
	}
	for _, pref := range blockedPrefixes {
		if low == pref || strings.HasPrefix(low, pref+"/") {
			return fmt.Errorf("项目路径不允许指向系统敏感目录（%s）", pref)
		}
	}
	if err := checkSensitiveSegments(low); err != nil {
		return err
	}
	if cd := strings.TrimSpace(corgiDir); cd != "" {
		if pathEqualOrUnder(clean, cd) {
			return fmt.Errorf("项目路径不允许指向 Corgi 数据目录")
		}
		if realCD, err := filepath.EvalSymlinks(cd); err == nil && pathEqualOrUnder(clean, realCD) {
			return fmt.Errorf("项目路径不允许指向 Corgi 数据目录")
		}
	}
	return nil
}

// InspectLocalProjectDir validates and inspects a candidate project directory on the local machine
// (used identically by Hub local checks and Node POST /api/path/inspect).
func InspectLocalProjectDir(home, corgiDir, dbPath, rawPath string) (model.PathInspectResult, error) {
	protCorgi := effectiveProtectedCorgiDir(corgiDir, home)
	cleanInput, err := ValidateProjectDir(rawPath, protCorgi)
	if err != nil {
		return model.PathInspectResult{}, err
	}
	resolved := cleanInput
	if strings.HasPrefix(cleanInput, "~/") {
		if strings.TrimSpace(home) == "" {
			return model.PathInspectResult{}, fmt.Errorf("目标机 HOME 目录不可用，无法展开 %s", cleanInput)
		}
		resolved = filepath.Clean(filepath.Join(home, strings.TrimPrefix(cleanInput, "~/")))
		if err := checkSensitiveAbsolutePath(filepath.ToSlash(resolved), protCorgi); err != nil {
			return model.PathInspectResult{}, err
		}
	}
	if dbPath != "" && IsProtectedHubDBPath(dbPath, resolved) {
		return model.PathInspectResult{}, fmt.Errorf("项目路径不允许指向受保护数据库路径")
	}

	res := model.PathInspectResult{
		ResolvedPath: resolved,
		DisplayPath:  tildeDisplayPath(home, resolved),
	}
	fi, statErr := os.Stat(resolved)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return res, nil
		}
		return res, fmt.Errorf("检测路径失败：%w", statErr)
	}
	realPath, evalErr := filepath.EvalSymlinks(resolved)
	if evalErr != nil {
		return model.PathInspectResult{}, fmt.Errorf("解析符号链接失败：%w", evalErr)
	}
	realPath = filepath.Clean(realPath)
	if err := checkSensitiveAbsolutePath(filepath.ToSlash(realPath), protCorgi); err != nil {
		return model.PathInspectResult{}, fmt.Errorf("符号链接目标越权：%w", err)
	}
	if dbPath != "" && IsProtectedHubDBPath(dbPath, realPath) {
		return model.PathInspectResult{}, fmt.Errorf("符号链接目标不允许指向受保护数据库路径")
	}
	res.Exists = true
	res.IsDir = fi.IsDir()
	res.ResolvedPath = realPath
	res.DisplayPath = tildeDisplayPath(home, realPath)
	if fi.IsDir() {
		if _, gErr := os.Stat(filepath.Join(realPath, ".git")); gErr == nil {
			res.IsGit = true
		}
	}
	return res, nil
}

func tildeDisplayPath(home, absPath string) string {
	h := filepath.Clean(strings.TrimSpace(home))
	p := filepath.Clean(strings.TrimSpace(absPath))
	if h != "" && h != "/" {
		if p == h {
			return "~"
		}
		if strings.HasPrefix(p, h+"/") {
			return "~" + p[len(h):]
		}
	}
	return p
}

// ConfigSnapshotHash computes a canonical SHA-256 hash over Settings + Policies for L2 optimistic locking.
func ConfigSnapshotHash(st model.Settings, pols []model.Policy) string {
	b, _ := json.Marshal(struct {
		Settings model.Settings `json:"settings"`
		Policies []model.Policy `json:"policies"`
	}{Settings: CloneSettings(st), Policies: ClonePolicies(pols)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IsProtectedPolicySource reports whether source is a built-in internal or WeCom source
// that manage_config must never overwrite or delete.
func IsProtectedPolicySource(src string) bool {
	switch strings.ToLower(strings.TrimSpace(src)) {
	case "chat", "wecom", "worker", "timer", "system", "router":
		return true
	default:
		return false
	}
}

// IsL3SettingKey reports whether a settings JSON key is permanently locked to Web Owner only.
func IsL3SettingKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "router_api_provider", "router_api_base_url", "router_api_model", "router_api_effort", "router_api_key",
		"wecom_enabled", "wecom_bot_id", "wecom_secret", "wecom_allow_users", "wecom_default_to",
		"public_url", "auto_update", "paused", "owner_token", "hook_key", "hub_url":
		return true
	default:
		return false
	}
}

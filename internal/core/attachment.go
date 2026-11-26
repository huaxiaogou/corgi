package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxReplyAttachmentBytes caps each outbound reply file attachment at 5 MiB.
	MaxReplyAttachmentBytes = 5 << 20
	// MaxReplyAttachments caps the number of explicit file attachments per reply.
	MaxReplyAttachments = 5
)

// ValidateAttachmentPaths validates, normalizes, and deduplicates explicit absolute file paths
// attached to an outbound reply without relying on filename or extension regular expressions.
func ValidateAttachmentPaths(rawPaths []string) ([]string, error) {
	if len(rawPaths) == 0 {
		return nil, nil
	}
	if len(rawPaths) > MaxReplyAttachments {
		return nil, fmt.Errorf("too many attachments: %d exceeds limit %d", len(rawPaths), MaxReplyAttachments)
	}
	seen := make(map[string]struct{}, len(rawPaths))
	out := make([]string, 0, len(rawPaths))
	for _, raw := range rawPaths {
		clean, err := ValidateAttachmentPath(raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out, nil
}

// ValidateAttachmentPath verifies that raw is an explicit, safe, existing, non-empty regular
// file path within MaxReplyAttachmentBytes. Any filename/extension is permitted.
func ValidateAttachmentPath(raw string) (string, error) {
	clean, f, _, err := openValidatedAttachment(raw)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	return clean, nil
}

// ReadAttachmentFile validates raw and reads its bounded contents for outbound delivery.
func ReadAttachmentFile(raw string) (filename string, data []byte, cleanPath string, err error) {
	clean, f, _, err := openValidatedAttachment(raw)
	if err != nil {
		return "", nil, "", err
	}
	defer f.Close()

	data, err = io.ReadAll(io.LimitReader(f, MaxReplyAttachmentBytes+1))
	if err != nil {
		return "", nil, "", fmt.Errorf("read attachment %q: %w", clean, err)
	}
	if len(data) == 0 {
		return "", nil, "", fmt.Errorf("attachment %q is empty", clean)
	}
	if int64(len(data)) > MaxReplyAttachmentBytes {
		return "", nil, "", fmt.Errorf("attachment %q exceeds %d MB limit", clean, MaxReplyAttachmentBytes>>20)
	}
	return filepath.Base(clean), data, clean, nil
}

func openValidatedAttachment(raw string) (clean string, file *os.File, info os.FileInfo, err error) {
	clean, err = normalizeExplicitAttachmentPath(raw)
	if err != nil {
		return "", nil, nil, err
	}
	realPath, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve attachment %q: %w", clean, err)
	}
	if isBlockedAttachmentPath(realPath) {
		return "", nil, nil, fmt.Errorf("attachment path %q is not allowed", clean)
	}

	f, err := os.Open(realPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open attachment %q: %w", clean, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", nil, nil, fmt.Errorf("stat attachment %q: %w", clean, err)
	}
	if fi.IsDir() {
		_ = f.Close()
		return "", nil, nil, fmt.Errorf("attachment path %q is a directory", clean)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return "", nil, nil, fmt.Errorf("attachment path %q is not a regular file", clean)
	}
	if fi.Size() <= 0 {
		_ = f.Close()
		return "", nil, nil, fmt.Errorf("attachment %q is empty", clean)
	}
	if fi.Size() > MaxReplyAttachmentBytes {
		_ = f.Close()
		return "", nil, nil, fmt.Errorf("attachment %q exceeds %d MB limit", clean, MaxReplyAttachmentBytes>>20)
	}
	return clean, f, fi, nil
}

func normalizeExplicitAttachmentPath(raw string) (string, error) {
	if strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("attachment path contains control characters")
	}
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("attachment path is empty")
	}
	if !strings.HasPrefix(p, "/") || !filepath.IsAbs(p) {
		return "", fmt.Errorf("attachment path %q must be an absolute path", p)
	}

	// Reject explicit "." / ".." traversal segments and empty segments (e.g. "//" or trailing "/").
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if i == 0 {
			continue
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("attachment path %q must not contain traversal segments", p)
		}
		if seg == "" {
			return "", fmt.Errorf("attachment path %q is not a clean file path", p)
		}
	}

	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) || clean == "/" {
		return "", fmt.Errorf("attachment path %q must be a file path", p)
	}
	if isBlockedAttachmentPath(clean) {
		return "", fmt.Errorf("attachment path %q is not allowed", clean)
	}
	return clean, nil
}

func isBlockedAttachmentPath(p string) bool {
	slashed := filepath.ToSlash(filepath.Clean(p))
	lower := strings.ToLower(slashed)

	base := filepath.Base(slashed)
	if base == "" || base == "." || base == ".." || strings.HasPrefix(base, ".") {
		return true
	}

	blockedPrefixes := []string{
		"/etc/",
		"/private/etc/",
		"/proc/",
		"/sys/",
		"/dev/",
		"/boot/",
		"/bin/",
		"/sbin/",
		"/usr/bin/",
		"/usr/sbin/",
		"/system/",
		"/library/",
		"/private/var/db/",
		"/var/db/",
	}
	for _, prefix := range blockedPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}

	segs := strings.Split(lower, "/")
	for i, seg := range segs {
		switch seg {
		case ".ssh", ".gnupg", ".aws", ".kube", ".docker", ".git":
			return true
		case ".corgi":
			// Block direct runtime secrets/DB under ~/.corgi/ (token, config.json, node.json, corgi.db),
			// while still allowing nested deliverables under ~/.corgi/router/, uploads/, worktrees/, etc.
			if i == len(segs)-2 {
				return true
			}
		}
	}
	return false
}

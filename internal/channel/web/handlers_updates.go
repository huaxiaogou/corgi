package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultReleaseRepoURL = "https://github.com/huaxiaogou/corgi"

type outdatedMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) repoBaseURL() string {
	if s.ReleaseRepoURL != "" {
		return strings.TrimRight(s.ReleaseRepoURL, "/")
	}
	return defaultReleaseRepoURL
}

func (s *Server) latestReleaseTag(soon bool) string {
	s.updateOnce.Do(func() {
		go func() {
			for {
				s.refreshLatestRelease()
				time.Sleep(time.Hour)
			}
		}()
	})
	s.updateMu.Lock()
	tag := s.latestTag
	checked := s.latestChecked
	busy := s.latestBusy
	s.updateMu.Unlock()

	if checked.IsZero() {
		s.refreshLatestRelease()
		s.updateMu.Lock()
		tag = s.latestTag
		s.updateMu.Unlock()
	} else if soon && !busy && time.Since(checked) > 10*time.Minute {
		go s.refreshLatestRelease()
	}
	return tag
}

func (s *Server) refreshLatestRelease() {
	s.updateMu.Lock()
	if s.latestBusy {
		s.updateMu.Unlock()
		return
	}
	s.latestBusy = true
	s.updateMu.Unlock()

	tag := s.fetchLatestTagHTTP()

	s.updateMu.Lock()
	s.latestBusy = false
	s.latestChecked = time.Now()
	if tag != "" {
		s.latestTag = tag
	}
	s.updateMu.Unlock()
}

func (s *Server) fetchLatestTagHTTP() string {
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Head(s.repoBaseURL() + "/releases/latest")
	if err != nil {
		return ""
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	idx := strings.LastIndex(loc, "/tag/")
	if idx < 0 {
		return ""
	}
	tag := strings.TrimSpace(loc[idx+5:])
	if parseSemver(tag) != nil {
		return tag
	}
	return ""
}

func parseSemver(v string) []int {
	v, ok := strings.CutPrefix(strings.TrimSpace(v), "v")
	if !ok {
		return nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func isOutdatedVersion(have, latest string) bool {
	a, b := parseSemver(have), parseSemver(latest)
	if a == nil || b == nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	soon := r.URL.Query().Get("soon") != ""
	latest := s.latestReleaseTag(soon)
	outdated := []outdatedMachine{}
	if s.Fleet != nil {
		for _, m := range s.Fleet.Machines() {
			v := m.Version
			if m.Local {
				v = s.Version
			}
			if (m.Local || m.Online) && isOutdatedVersion(v, latest) {
				outdated = append(outdated, outdatedMachine{ID: m.ID, Name: m.Name, Version: v})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  s.Version,
		"latest":   latest,
		"outdated": outdated,
	})
}

// handleSelfUpdate downloads the latest release binary verified against SHA256SUMS.txt
// and replaces the running process in place via syscall.Exec so systemd/launchd and herdr stay intact.
func (s *Server) handleSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.selfUpdateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "升级正在进行中，请稍候"})
		return
	}
	restarting := false
	defer func() {
		if !restarting {
			s.selfUpdateMu.Unlock()
		}
	}()

	if parseSemver(s.Version) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "当前进程为开发编译版（" + s.Version + "），不支持在线覆盖升级"})
		return
	}
	latest := s.fetchLatestTagHTTP()
	if latest == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "无法获取最新版本号（请检查这台电脑能否访问 GitHub）"})
		return
	}
	if !isOutdatedVersion(s.Version, latest) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version, "up_to_date": true})
		return
	}

	exe, err := s.resolveExecutable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "定位当前可执行文件失败：" + err.Error()})
		return
	}
	if err := s.downloadAndVerifyRelease(r.Context(), latest, exe); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "升级失败：" + err.Error()})
		return
	}

	if s.Logger != nil {
		s.Logger.Info("self-updated binary, restarting in place", "from", s.Version, "to", latest, "exe", exe)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": s.Version, "version": latest})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	restarting = true
	execFn := s.ExecFn
	if execFn == nil {
		execFn = func(targetExe string) {
			time.Sleep(800 * time.Millisecond)
			_ = syscall.Exec(targetExe, os.Args, os.Environ())
			os.Exit(0)
		}
	}
	go execFn(exe)
}

func (s *Server) resolveExecutable() (string, error) {
	if s.ExecutablePath != "" {
		return s.ExecutablePath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
		return resolved, nil
	}
	return exe, nil
}

func (s *Server) downloadAndVerifyRelease(ctx context.Context, tag, destExe string) error {
	asset := "corgi-" + runtime.GOOS + "-" + runtime.GOARCH
	base := s.repoBaseURL() + "/releases/download/" + tag + "/"
	client := &http.Client{Timeout: 5 * time.Minute}
	fetch := func(name string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+name, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP %s (%s)", resp.Status, name)
		}
		return resp, err
	}

	sumResp, err := fetch("SHA256SUMS.txt")
	if err != nil {
		return err
	}
	wantSum := ""
	sc := bufio.NewScanner(sumResp.Body)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == asset {
			wantSum = strings.ToLower(fields[0])
			break
		}
	}
	_ = sumResp.Body.Close()
	if wantSum == "" {
		return fmt.Errorf("SHA256SUMS.txt 中未找到 %s 的校验值", asset)
	}

	binResp, err := fetch(asset)
	if err != nil {
		return err
	}
	defer binResp.Body.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(destExe), ".corgi.update.*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher), binResp.Body)
	chmodErr := tmpFile.Chmod(0o755)
	closeErr := tmpFile.Close()
	if copyErr != nil {
		return copyErr
	}
	if chmodErr != nil {
		return chmodErr
	}
	if closeErr != nil {
		return closeErr
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantSum {
		return fmt.Errorf("SHA-256 校验不一致（期望 %s，实际 %s）", wantSum[:12], got[:12])
	}

	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, runErr := exec.CommandContext(vctx, tmpPath, "version").Output()
	if runErr != nil || !strings.Contains(string(out), tag) {
		return fmt.Errorf("新二进制自检未通过：%v %s", runErr, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmpPath, destExe); err != nil {
		return err
	}
	tmpPath = ""
	return nil
}

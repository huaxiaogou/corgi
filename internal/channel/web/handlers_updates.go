package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultReleaseRepoURL      = "https://github.com/huaxiaogou/corgi"
	relayUpdateTagHeader       = "X-Corgi-Update-Tag"
	relayUpdateSHAHeader       = "X-Corgi-Update-SHA256"
	selfUpdateTimeout          = 2*time.Minute + 30*time.Second
	remoteUpdateForwardTimeout = 2*time.Minute + 50*time.Second
	updateLatestAttemptTimeout = 20 * time.Second
	updateFileAttemptTimeout   = 30 * time.Second
	updateBinAttemptTimeout    = 2 * time.Minute
	updateIdleReadTimeout      = 25 * time.Second
	updateMaxAttempts          = 3
	defaultUpdateRetryBase     = time.Second
	maxUpdateBinarySize        = int64(128 << 20)
)

var supportedReleasePlatforms = map[string]bool{
	"linux/amd64":  true,
	"linux/arm64":  true,
	"darwin/amd64": true,
	"darwin/arm64": true,
}

type outdatedMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type versionMachine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Local   bool   `json:"local"`
	Online  bool   `json:"online"`
}

type updateError struct {
	Code      string
	Relayable bool
	Msg       string
	Err       error
}

func (e *updateError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "升级失败"
}

func (e *updateError) Unwrap() error { return e.Err }

func (s *Server) serverCtx() context.Context {
	if s.ServerCtx != nil {
		return s.ServerCtx
	}
	return context.Background()
}

func (s *Server) repoBaseURL() string {
	if s.ReleaseRepoURL != "" {
		return strings.TrimRight(s.ReleaseRepoURL, "/")
	}
	return defaultReleaseRepoURL
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
		if p == "" {
			return nil
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
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

func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func parsePlatformOSArch(raw string) (goos, goarch string, ok bool) {
	parts := strings.Split(strings.TrimSpace(strings.ToLower(raw)), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	key := parts[0] + "/" + parts[1]
	if !supportedReleasePlatforms[key] {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) latestReleaseTag(soon bool) string {
	s.updateOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			s.refreshLatestRelease()
			for {
				select {
				case <-s.serverCtx().Done():
					return
				case <-ticker.C:
					s.refreshLatestRelease()
				}
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
	} else if soon && !busy {
		// When the previous lookup failed (tag == ""), allow quick retries instead of 10-minute throttling.
		minAge := 10 * time.Minute
		if tag == "" {
			minAge = 15 * time.Second
		}
		if time.Since(checked) > minAge {
			go s.refreshLatestRelease()
		}
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

	ctx, cancel := context.WithTimeout(s.serverCtx(), updateLatestAttemptTimeout*2)
	tag, _, _ := s.fetchLatestTagErr(ctx)
	cancel()

	s.updateMu.Lock()
	s.latestBusy = false
	s.latestChecked = time.Now()
	if tag != "" {
		s.latestTag = tag
	}
	s.updateMu.Unlock()
}

func (s *Server) recordLatestTag(tag string) {
	if parseSemver(tag) == nil {
		return
	}
	s.updateMu.Lock()
	s.latestTag = tag
	s.latestChecked = time.Now()
	s.updateMu.Unlock()
}

func (s *Server) collectVersionMachines(ctx context.Context) []versionMachine {
	byID := map[string]versionMachine{}
	if s.Fleet != nil {
		for _, m := range s.Fleet.Machines() {
			v := m.Version
			online := m.Online
			if m.Local {
				v = s.Version
				online = true
			}
			byID[m.ID] = versionMachine{
				ID:      m.ID,
				Name:    m.Name,
				Version: v,
				OS:      m.OS,
				Local:   m.Local,
				Online:  online,
			}
		}
	}
	if s.Auth != nil {
		if devs, err := s.Auth.List(ctx); err == nil {
			for _, d := range devs {
				if d.Kind != "machine" {
					continue
				}
				if _, exists := byID[d.ID]; !exists {
					byID[d.ID] = versionMachine{
						ID:     d.ID,
						Name:   d.Name,
						OS:     d.OS,
						Local:  false,
						Online: false,
					}
				}
			}
		}
	}
	out := make([]versionMachine, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Local != out[j].Local {
			return out[i].Local
		}
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	soon := r.URL.Query().Get("soon") != ""
	latest := s.latestReleaseTag(soon)
	machines := s.collectVersionMachines(r.Context())
	outdated := []outdatedMachine{}
	for _, m := range machines {
		if (m.Local || m.Online) && isOutdatedVersion(m.Version, latest) {
			outdated = append(outdated, outdatedMachine{ID: m.ID, Name: m.Name, Version: m.Version})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  s.Version,
		"latest":   latest,
		"outdated": outdated,
		"machines": machines,
		"update":   s.fleetUpdateSnapshot(),
	})
}

// handleHubUpdate handles authenticated POST /api/update on the Hub:
//   - ?all=1 launches the Hub-owned sequential fleet upgrade (delegating to handleFleetUpdate).
//   - ?machine=<exact-id> upgrades one remote Node (direct first, Hub-relayed fallback on network error).
//   - empty machine upgrades the Hub itself synchronously.
func (s *Server) handleHubUpdate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		s.handleFleetUpdate(w, r)
		return
	}
	machineID := strings.TrimSpace(r.URL.Query().Get("machine"))
	if machineID == "" {
		s.handleHubSelfUpdate(w, r)
		return
	}
	if s.isFleetUpdateBusy() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "全端升级正在进行中，请稍候"})
		return
	}
	if s.Fleet == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "这台电脑现在不在线（" + machineID + "）"})
		return
	}
	// Exact device ID lookup: never fall back to fuzzy ResolveMachine("hub"/"local"/hostname).
	m, hc, ok := s.Fleet.RemoteMachineByID(machineID)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "这台电脑现在不在线（" + machineID + "）"})
		return
	}
	ctx, cancel := context.WithTimeout(s.serverCtx(), remoteUpdateForwardTimeout)
	defer cancel()
	cache := newRelayRunCache()
	defer cache.Cleanup()

	res, status, err := s.updateRemoteNode(ctx, m, hc, "", cache)
	if err != nil {
		if status <= 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleHubSelfUpdate handles synchronous Hub self-upgrade on POST /api/update (no machine param).
func (s *Server) handleHubSelfUpdate(w http.ResponseWriter, r *http.Request) {
	if s.isFleetUpdateBusy() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "全端升级正在进行中，请稍候"})
		return
	}
	s.runLocalSelfUpdateHTTP(w, false)
}

// handleNodeSelfUpdate serves POST /api/update strictly inside a Node's yamux tunnel.
func (s *Server) handleNodeSelfUpdate(w http.ResponseWriter, _ *http.Request) {
	s.runLocalSelfUpdateHTTP(w, true)
}

func (s *Server) runLocalSelfUpdateHTTP(w http.ResponseWriter, isNode bool) {
	if !s.selfUpdateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "升级正在进行中，请稍候", "code": "busy", "relayable": false})
		return
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			s.selfUpdateMu.Unlock()
		}
	}()

	if parseSemver(s.Version) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":     "当前进程为开发编译版（" + s.Version + "），不支持在线覆盖升级",
			"code":      "dev_build",
			"relayable": false,
		})
		return
	}

	ctx, cancel := context.WithTimeout(s.serverCtx(), selfUpdateTimeout)
	defer cancel()

	latest, usedIPv4, err := s.fetchLatestTagErr(ctx)
	if err != nil || latest == "" {
		msg := formatUpdateError(err)
		if msg == "" {
			msg = "无法获取最新版本号（请检查这台电脑能否访问 GitHub）"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":     "查不到最新版本：" + msg,
			"code":      "upstream_net",
			"relayable": true,
		})
		return
	}
	s.recordLatestTag(latest)

	if !isOutdatedVersion(s.Version, latest) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version, "up_to_date": true})
		return
	}

	exe, err := s.resolveExecutable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":     "定位当前可执行文件失败：" + err.Error(),
			"code":      "local_io",
			"relayable": false,
		})
		return
	}

	tmpPath, err := s.stageReleaseBinary(ctx, latest, exe, usedIPv4)
	if err != nil {
		writeUpdateErrorJSON(w, err)
		return
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Remove(tmpPath)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":     "写入新版本文件失败：" + err.Error(),
			"code":      "local_io",
			"relayable": false,
		})
		return
	}

	if s.Logger != nil {
		s.Logger.Info("self-updated binary, restarting in place", "from", s.Version, "to", latest, "exe", exe, "node", isNode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": s.Version, "version": latest})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	keepLocked = true
	s.triggerExecAsync(exe, isNode, func() {
		s.selfUpdateMu.Unlock()
	})
}

// handleNodeRelayUpdate serves POST /api/update/relay strictly inside a Node's yamux tunnel.
// It streams a Hub-relayed binary into the destination directory, re-verifies SHA-256 and
// `<tmp> version`, atomically replaces the executable, and triggers in-place exec.
func (s *Server) handleNodeRelayUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.selfUpdateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "升级正在进行中，请稍候", "code": "busy"})
		return
	}
	keepLocked := false
	defer func() {
		if !keepLocked {
			s.selfUpdateMu.Unlock()
		}
	}()

	if parseSemver(s.Version) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "当前进程为开发编译版（" + s.Version + "），不支持中转升级"})
		return
	}
	tag := strings.TrimSpace(r.Header.Get(relayUpdateTagHeader))
	wantSHA := strings.ToLower(strings.TrimSpace(r.Header.Get(relayUpdateSHAHeader)))
	if parseSemver(tag) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "中转升级版本号无效"})
		return
	}
	if !isValidSHA256Hex(wantSHA) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "中转升级校验值格式无效"})
		return
	}
	if !isOutdatedVersion(s.Version, tag) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version, "up_to_date": true, "relayed": true})
		return
	}

	exe, err := s.resolveExecutable()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "定位当前可执行文件失败：" + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(s.serverCtx(), selfUpdateTimeout)
	defer cancel()

	tmpPath, err := stageStreamedBinary(ctx, r.Body, r.ContentLength, tag, wantSHA, exe)
	if err != nil {
		writeUpdateErrorJSON(w, err)
		return
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Remove(tmpPath)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "写入新版本文件失败：" + err.Error(), "code": "local_io"})
		return
	}

	if s.Logger != nil {
		s.Logger.Info("applied relayed binary, restarting in place", "from", s.Version, "to", tag, "exe", exe)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": s.Version, "version": tag, "relayed": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	keepLocked = true
	s.triggerExecAsync(exe, true, func() {
		s.selfUpdateMu.Unlock()
	})
}

func writeUpdateErrorJSON(w http.ResponseWriter, err error) {
	var ue *updateError
	if errors.As(err, &ue) {
		status := http.StatusBadGateway
		if ue.Code == "dev_build" {
			status = http.StatusBadRequest
		} else if ue.Code == "busy" {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{
			"error":     ue.Error(),
			"code":      ue.Code,
			"relayable": ue.Relayable,
		})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"error":     "升级失败：" + formatUpdateError(err),
		"code":      "unknown",
		"relayable": false,
	})
}

func (s *Server) triggerExecAsync(targetExe string, isNode bool, onExecFail func()) {
	execFn := s.ExecFn
	go func() {
		if !isNode && s.Bus != nil {
			fctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			_ = s.Bus.FlushBeforeRestart(fctx)
			cancel()
		}
		if execFn != nil {
			execFn(targetExe)
			return
		}
		time.Sleep(800 * time.Millisecond)
		args := append([]string(nil), os.Args...)
		if isNode {
			args = sanitizeNodeExecArgs(args)
		}
		if err := syscall.Exec(targetExe, args, os.Environ()); err != nil {
			if s.Logger != nil {
				s.Logger.Error("syscall.Exec failed after upgrade, keeping current process alive", "exe", targetExe, "err", err)
			}
			if onExecFail != nil {
				onExecFail()
			}
		}
	}()
}

// sanitizeNodeExecArgs strips consumed one-time pairing flags (--code, --pair-only) so
// foreground-started nodes cleanly re-exec using their persisted ~/.corgi/node.json.
func sanitizeNodeExecArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--code" || a == "-code":
			if i+1 < len(args) {
				i++ // skip value
			}
			continue
		case strings.HasPrefix(a, "--code=") || strings.HasPrefix(a, "-code="):
			continue
		case a == "--pair-only" || a == "-pair-only" || strings.HasPrefix(a, "--pair-only=") || strings.HasPrefix(a, "-pair-only="):
			continue
		}
		out = append(out, a)
	}
	return out
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

// ---------------------------------------------------------------- Download transport, retry, verification

func newUpdateTransport(forceIPv4 bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
	}
	if forceIPv4 {
		tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		}
	} else {
		tr.DialContext = dialer.DialContext
	}
	return tr
}

func requestUsesProxy(req *http.Request) bool {
	u, err := http.ProxyFromEnvironment(req)
	return err == nil && u != nil
}

type idleTimeoutReader struct {
	rc     io.ReadCloser
	d      time.Duration
	cancel context.CancelFunc

	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func newIdleTimeoutReader(rc io.ReadCloser, d time.Duration, cancel context.CancelFunc) *idleTimeoutReader {
	r := &idleTimeoutReader{rc: rc, d: d, cancel: cancel}
	if d > 0 && cancel != nil {
		r.timer = time.AfterFunc(d, cancel)
	}
	return r
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.timer != nil && !r.closed {
		r.timer.Reset(r.d)
	}
	r.mu.Unlock()
	n, err := r.rc.Read(p)
	if n > 0 {
		r.mu.Lock()
		if r.timer != nil && !r.closed {
			r.timer.Reset(r.d)
		}
		r.mu.Unlock()
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.rc.Close()
}

func (s *Server) sleepBackoff(ctx context.Context, attempt int) error {
	base := s.UpdateRetryBaseDelay
	if base <= 0 {
		base = defaultUpdateRetryBase
	}
	delay := base << attempt
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isRetryableNetErr(parentCtx context.Context, err error) bool {
	if err == nil || parentCtx.Err() != nil {
		return false
	}
	var ue *updateError
	if errors.As(err, &ue) {
		return ue.Relayable
	}
	return true
}

func (s *Server) fetchLatestTagErr(parentCtx context.Context) (string, bool, error) {
	maxAttempts := max(1, updateMaxAttempts)
	forceIPv4 := false
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tag, usedProxy, err := s.fetchLatestTagOnce(parentCtx, forceIPv4)
		if err == nil && tag != "" {
			return tag, forceIPv4, nil
		}
		lastErr = err
		if attempt+1 >= maxAttempts || !isRetryableNetErr(parentCtx, err) {
			break
		}
		if !forceIPv4 && !usedProxy {
			forceIPv4 = true
		}
		if berr := s.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	return "", forceIPv4, &updateError{
		Code:      "upstream_net",
		Relayable: true,
		Msg:       formatUpdateError(lastErr),
		Err:       lastErr,
	}
}

func (s *Server) fetchLatestTagOnce(parentCtx context.Context, forceIPv4 bool) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, updateLatestAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.repoBaseURL()+"/releases/latest", nil)
	if err != nil {
		return "", false, err
	}
	usedProxy := requestUsesProxy(req)
	tr := newUpdateTransport(forceIPv4)
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", usedProxy, err
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	idx := strings.LastIndex(loc, "/tag/")
	if idx < 0 {
		return "", usedProxy, &updateError{
			Code:      "upstream_http",
			Relayable: true,
			Msg:       fmt.Sprintf("GitHub 返回异常状态（HTTP %d）", resp.StatusCode),
		}
	}
	tag := strings.TrimSpace(loc[idx+5:])
	if parseSemver(tag) == nil {
		return "", usedProxy, &updateError{
			Code:      "upstream_http",
			Relayable: false,
			Msg:       fmt.Sprintf("最新版本标签格式异常（%s）", tag),
		}
	}
	return tag, usedProxy, nil
}

func (s *Server) downloadChecksums(parentCtx context.Context, tag, asset string, preferIPv4 bool) (string, bool, error) {
	targetURL := s.repoBaseURL() + "/releases/download/" + tag + "/SHA256SUMS.txt"
	maxAttempts := max(1, updateMaxAttempts)
	forceIPv4 := preferIPv4
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		data, usedProxy, err := fetchSmallFileOnce(parentCtx, targetURL, "SHA256SUMS.txt", forceIPv4)
		if err == nil {
			want := ""
			sc := bufio.NewScanner(bytes.NewReader(data))
			for sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == asset {
					want = strings.ToLower(strings.TrimSpace(fields[0]))
					break
				}
			}
			if want == "" {
				return "", forceIPv4, &updateError{
					Code:      "checksum",
					Relayable: false,
					Msg:       fmt.Sprintf("%s 的 SHA256SUMS.txt 中未找到 %s 的校验值", tag, asset),
				}
			}
			if !isValidSHA256Hex(want) {
				return "", forceIPv4, &updateError{
					Code:      "checksum",
					Relayable: false,
					Msg:       fmt.Sprintf("%s 的 SHA256SUMS.txt 中 %s 校验值格式无效", tag, asset),
				}
			}
			return want, forceIPv4, nil
		}
		lastErr = err
		if attempt+1 >= maxAttempts || !isRetryableNetErr(parentCtx, err) {
			break
		}
		if !forceIPv4 && !usedProxy {
			forceIPv4 = true
		}
		if berr := s.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	return "", forceIPv4, &updateError{
		Code:      "upstream_net",
		Relayable: true,
		Msg:       "下载校验文件失败：" + formatUpdateError(lastErr),
		Err:       lastErr,
	}
}

func fetchSmallFileOnce(parentCtx context.Context, targetURL, name string, forceIPv4 bool) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, updateFileAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, false, err
	}
	usedProxy := requestUsesProxy(req)
	tr := newUpdateTransport(forceIPv4)
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			usedProxy = requestUsesProxy(next)
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, usedProxy, err
	}
	body := newIdleTimeoutReader(resp.Body, updateIdleReadTimeout, cancel)
	defer body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, usedProxy, &updateError{
			Code:      "upstream_http",
			Relayable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden,
			Msg:       fmt.Sprintf("下载 %s 失败（HTTP %s）", name, resp.Status),
		}
	}
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, usedProxy, err
	}
	if resp.ContentLength > 0 && int64(len(data)) != resp.ContentLength {
		return nil, usedProxy, fmt.Errorf("下载 %s 中断（已读 %d/%d 字节）", name, len(data), resp.ContentLength)
	}
	return data, usedProxy, nil
}

func (s *Server) downloadAssetToDir(parentCtx context.Context, tag, asset, dir, wantSHA string, preferIPv4 bool, mode os.FileMode) (string, error) {
	targetURL := s.repoBaseURL() + "/releases/download/" + tag + "/" + asset
	maxAttempts := max(1, updateMaxAttempts)
	forceIPv4 := preferIPv4
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		tmp, usedProxy, err := downloadAssetOnce(parentCtx, targetURL, asset, dir, wantSHA, forceIPv4, mode)
		if err == nil {
			return tmp, nil
		}
		lastErr = err
		if attempt+1 >= maxAttempts || !isRetryableNetErr(parentCtx, err) {
			break
		}
		if !forceIPv4 && !usedProxy {
			forceIPv4 = true
		}
		if berr := s.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	var ue *updateError
	if errors.As(lastErr, &ue) {
		return "", ue
	}
	return "", &updateError{
		Code:      "upstream_net",
		Relayable: true,
		Msg:       "下载升级包失败：" + formatUpdateError(lastErr),
		Err:       lastErr,
	}
}

func downloadAssetOnce(parentCtx context.Context, targetURL, asset, dir, wantSHA string, forceIPv4 bool, mode os.FileMode) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, updateBinAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", false, err
	}
	usedProxy := requestUsesProxy(req)
	tr := newUpdateTransport(forceIPv4)
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			usedProxy = requestUsesProxy(next)
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", usedProxy, err
	}
	body := newIdleTimeoutReader(resp.Body, updateIdleReadTimeout, cancel)
	defer body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", usedProxy, &updateError{
			Code:      "upstream_http",
			Relayable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden,
			Msg:       fmt.Sprintf("下载 %s 失败（HTTP %s）", asset, resp.Status),
		}
	}

	tmpPath, err := writeVerifiedTempBinary(body, resp.ContentLength, dir, wantSHA, mode)
	if err != nil {
		return "", usedProxy, err
	}
	return tmpPath, usedProxy, nil
}

func writeVerifiedTempBinary(body io.Reader, expectedLen int64, dir, wantSHA string, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, ".corgi.update.*")
	if err != nil {
		return "", &updateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, err), Err: err}
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
		return "", &updateError{Code: "upstream_net", Relayable: true, Msg: "传输中断：" + formatUpdateError(copyErr), Err: copyErr}
	}
	if n == 0 {
		return "", &updateError{Code: "upstream_net", Relayable: true, Msg: "下载到空文件"}
	}
	if n > limit {
		return "", &updateError{Code: "checksum", Relayable: false, Msg: fmt.Sprintf("升级包体积超出上限（%d MiB）", limit>>20)}
	}
	if expectedLen > 0 && n != expectedLen {
		return "", &updateError{Code: "upstream_net", Relayable: true, Msg: fmt.Sprintf("下载不完整（已读 %d/%d 字节）", n, expectedLen)}
	}
	if chmodErr != nil {
		return "", &updateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, chmodErr), Err: chmodErr}
	}
	if closeErr != nil {
		return "", &updateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, closeErr), Err: closeErr}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return "", &updateError{
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
func (s *Server) stageReleaseBinary(ctx context.Context, tag, destExe string, preferIPv4 bool) (string, error) {
	dir := filepath.Dir(destExe)
	if err := probeWritableDir(dir); err != nil {
		return "", err
	}
	asset := "corgi-" + runtime.GOOS + "-" + runtime.GOARCH
	wantSHA, usedIPv4, err := s.downloadChecksums(ctx, tag, asset, preferIPv4)
	if err != nil {
		return "", err
	}
	tmpPath, err := s.downloadAssetToDir(ctx, tag, asset, dir, wantSHA, usedIPv4, 0o755)
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
		return &updateError{Code: "local_io", Relayable: false, Msg: formatLocalIOErr(dir, err), Err: err}
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
		return &updateError{
			Code:      "verify",
			Relayable: false,
			Msg:       fmt.Sprintf("新版本在这台电脑上无法运行（%v %s）", err, got),
			Err:       err,
		}
	}
	if got != want {
		return &updateError{
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
	var ue *updateError
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

package updater

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// CachedLatestTag returns the currently cached latest release tag without triggering network IO.
func (m *Manager) CachedLatestTag() string {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	return m.latestTag
}

// LatestReleaseTag returns the cached latest release tag, kicking off periodic or immediate refresh as needed.
func (m *Manager) LatestReleaseTag(soon bool) string {
	m.updateOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			m.refreshLatestRelease()
			for {
				select {
				case <-m.serverCtx().Done():
					return
				case <-ticker.C:
					m.refreshLatestRelease()
				}
			}
		}()
	})
	m.updateMu.Lock()
	tag := m.latestTag
	checked := m.latestChecked
	busy := m.latestBusy
	m.updateMu.Unlock()

	if checked.IsZero() {
		m.refreshLatestRelease()
		m.updateMu.Lock()
		tag = m.latestTag
		m.updateMu.Unlock()
	} else if soon && !busy {
		// When the previous lookup failed (tag == ""), allow quick retries instead of 10-minute throttling.
		minAge := 10 * time.Minute
		if tag == "" {
			minAge = 15 * time.Second
		}
		if time.Since(checked) > minAge {
			go m.refreshLatestRelease()
		}
	}
	return tag
}

func (m *Manager) refreshLatestRelease() {
	m.updateMu.Lock()
	if m.latestBusy {
		m.updateMu.Unlock()
		return
	}
	m.latestBusy = true
	m.updateMu.Unlock()

	ctx, cancel := context.WithTimeout(m.serverCtx(), updateLatestAttemptTimeout*2)
	tag, _, _ := m.fetchLatestTagErr(ctx)
	cancel()

	m.updateMu.Lock()
	m.latestBusy = false
	m.latestChecked = time.Now()
	if tag != "" {
		m.latestTag = tag
	}
	m.updateMu.Unlock()
}

func (m *Manager) recordLatestTag(tag string) {
	if parseSemver(tag) == nil {
		return
	}
	m.updateMu.Lock()
	m.latestTag = tag
	m.latestChecked = time.Now()
	m.updateMu.Unlock()
}

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

func (m *Manager) sleepBackoff(ctx context.Context, attempt int) error {
	delay := m.effectiveRetryBaseDelay() << attempt
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
	var ue *UpdateError
	if errors.As(err, &ue) {
		return ue.Relayable
	}
	return true
}

func (m *Manager) fetchLatestTagErr(parentCtx context.Context) (string, bool, error) {
	maxAttempts := max(1, updateMaxAttempts)
	forceIPv4 := false
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tag, usedProxy, err := m.fetchLatestTagOnce(parentCtx, forceIPv4)
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
		if berr := m.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	return "", forceIPv4, &UpdateError{
		Code:      "upstream_net",
		Relayable: true,
		Msg:       formatUpdateError(lastErr),
		Err:       lastErr,
	}
}

func (m *Manager) fetchLatestTagOnce(parentCtx context.Context, forceIPv4 bool) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, updateLatestAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.repoBaseURL()+"/releases/latest", nil)
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
		return "", usedProxy, &UpdateError{
			Code:      "upstream_http",
			Relayable: true,
			Msg:       fmt.Sprintf("GitHub 返回异常状态（HTTP %d）", resp.StatusCode),
		}
	}
	tag := strings.TrimSpace(loc[idx+5:])
	if parseSemver(tag) == nil {
		return "", usedProxy, &UpdateError{
			Code:      "upstream_http",
			Relayable: false,
			Msg:       fmt.Sprintf("最新版本标签格式异常（%s）", tag),
		}
	}
	return tag, usedProxy, nil
}

func (m *Manager) downloadChecksums(parentCtx context.Context, tag, asset string, preferIPv4 bool) (string, bool, error) {
	targetURL := m.repoBaseURL() + "/releases/download/" + tag + "/SHA256SUMS.txt"
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
				return "", forceIPv4, &UpdateError{
					Code:      "checksum",
					Relayable: false,
					Msg:       fmt.Sprintf("%s 的 SHA256SUMS.txt 中未找到 %s 的校验值", tag, asset),
				}
			}
			if !isValidSHA256Hex(want) {
				return "", forceIPv4, &UpdateError{
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
		if berr := m.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	return "", forceIPv4, &UpdateError{
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
		return nil, usedProxy, &UpdateError{
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

func (m *Manager) downloadAssetToDir(parentCtx context.Context, tag, asset, dir, wantSHA string, preferIPv4 bool, mode os.FileMode) (string, error) {
	targetURL := m.repoBaseURL() + "/releases/download/" + tag + "/" + asset
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
		if berr := m.sleepBackoff(parentCtx, attempt); berr != nil {
			lastErr = berr
			break
		}
	}
	var ue *UpdateError
	if errors.As(lastErr, &ue) {
		return "", ue
	}
	return "", &UpdateError{
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
		return "", usedProxy, &UpdateError{
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

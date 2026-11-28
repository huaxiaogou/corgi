package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

type relayAsset struct {
	asset  string
	sha256 string
	size   int64
	path   string
}

type relayRunCache struct {
	dir     string
	entries map[string]relayAsset
}

func newRelayRunCache() *relayRunCache {
	return &relayRunCache{entries: map[string]relayAsset{}}
}

func (c *relayRunCache) Cleanup() {
	if c == nil {
		return
	}
	for k, ent := range c.entries {
		_ = os.Remove(ent.path)
		delete(c.entries, k)
	}
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
		c.dir = ""
	}
}

func (c *relayRunCache) getOrFetch(ctx context.Context, m *Manager, tag, goos, goarch string) (relayAsset, *os.File, error) {
	key := tag + ":" + goos + "/" + goarch
	if ent, ok := c.entries[key]; ok {
		f, err := os.Open(ent.path)
		if err == nil {
			if fi, serr := f.Stat(); serr == nil && fi.Size() == ent.size {
				return ent, f, nil
			}
			_ = f.Close()
		}
		_ = os.Remove(ent.path)
		delete(c.entries, key)
	}
	if c.dir == "" {
		dir, err := os.MkdirTemp("", "corgi-relay-*")
		if err != nil {
			return relayAsset{}, nil, err
		}
		c.dir = dir
	}
	asset := "corgi-" + goos + "-" + goarch
	wantSHA, usedIPv4, err := m.downloadChecksums(ctx, tag, asset, false)
	if err != nil {
		return relayAsset{}, nil, err
	}
	tmpPath, err := m.downloadAssetToDir(ctx, tag, asset, c.dir, wantSHA, usedIPv4, 0o600)
	if err != nil {
		return relayAsset{}, nil, err
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return relayAsset{}, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return relayAsset{}, nil, err
	}
	ent := relayAsset{asset: asset, sha256: wantSHA, size: fi.Size(), path: tmpPath}
	c.entries[key] = ent
	return ent, f, nil
}

func (m *Manager) machineOS(ctx context.Context, mc model.Machine) string {
	if strings.TrimSpace(mc.OS) != "" {
		return mc.OS
	}
	if m.cfg.Devices != nil {
		if devs, err := m.cfg.Devices.List(ctx); err == nil {
			for _, d := range devs {
				if d != nil && d.ID == mc.ID && d.Kind == "machine" && strings.TrimSpace(d.OS) != "" {
					return d.OS
				}
			}
		}
	}
	return ""
}

func isLegacyRelayableMessage(msg string) bool {
	for _, nonLocal := range []string{"开发编译版", "升级正在进行中", "定位当前可执行文件", "无写权限", "磁盘空间不足", "写入新版本文件失败", "无法运行", "自检输出不匹配"} {
		if strings.Contains(msg, nonLocal) {
			return false
		}
	}
	return true
}

// UpdateSingleRemote upgrades one remote Node by exact device ID (direct first, Hub-relayed fallback).
func (m *Manager) UpdateSingleRemote(machineID string) (map[string]any, int, error) {
	if m.IsFleetBusy() {
		return nil, http.StatusConflict, errors.New("全端升级正在进行中，请稍候")
	}
	if m.cfg.Fleet == nil {
		return nil, http.StatusServiceUnavailable, errors.New("这台电脑现在不在线（" + machineID + "）")
	}
	// Exact device ID lookup: never fall back to fuzzy ResolveMachine("hub"/"local"/hostname).
	mc, hc, ok := m.cfg.Fleet.RemoteMachineByID(machineID)
	if !ok {
		return nil, http.StatusServiceUnavailable, errors.New("这台电脑现在不在线（" + machineID + "）")
	}
	ctx, cancel := context.WithTimeout(m.serverCtx(), remoteUpdateForwardTimeout)
	defer cancel()
	cache := newRelayRunCache()
	defer cache.Cleanup()

	return m.updateRemoteNode(ctx, mc, hc, "", cache)
}

func (m *Manager) updateRemoteNode(ctx context.Context, mc model.Machine, hc *http.Client, targetTag string, cache *relayRunCache) (map[string]any, int, error) {
	// Use a request-scoped client sharing the tunnel Transport so we never mutate shared hc.Timeout.
	reqClient := &http.Client{Transport: hc.Transport}
	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/update", nil)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("构造节点 %s 升级请求失败：%w", mc.Name, err)
	}
	resp, err := reqClient.Do(outReq)
	if err != nil {
		return nil, 0, fmt.Errorf("转发到 %s 失败：%s", mc.Name, formatUpdateError(err))
	}
	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("节点 %s 升级连接中断（可能正在重启），请稍候刷新查看版本", mc.Name)
	}

	if resp.StatusCode == http.StatusOK {
		var out map[string]any
		if json.Unmarshal(rawBody, &out) != nil || out == nil {
			out = map[string]any{"ok": true}
		}
		return out, http.StatusOK, nil
	}

	var errRes struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Relayable *bool  `json:"relayable"`
	}
	errMsg := strings.TrimSpace(string(rawBody))
	if json.Unmarshal(rawBody, &errRes) == nil && strings.TrimSpace(errRes.Error) != "" {
		errMsg = strings.TrimSpace(errRes.Error)
	}
	if errMsg == "" {
		errMsg = resp.Status
	}

	shouldRelay := resp.StatusCode == http.StatusBadGateway &&
		((errRes.Relayable != nil && *errRes.Relayable) || (errRes.Relayable == nil && isLegacyRelayableMessage(errMsg)))
	goos, goarch, hasPlatform := parsePlatformOSArch(m.machineOS(ctx, mc))
	if shouldRelay && hasPlatform {
		return m.relayRemoteNodeUpdate(ctx, mc, reqClient, goos, goarch, targetTag, errMsg, cache)
	}
	return nil, resp.StatusCode, errors.New(errMsg)
}

func (m *Manager) relayRemoteNodeUpdate(ctx context.Context, mc model.Machine, reqClient *http.Client, goos, goarch, targetTag, directErr string, cache *relayRunCache) (map[string]any, int, error) {
	tag := targetTag
	if tag == "" {
		var err error
		tag, _, err = m.fetchLatestTagErr(ctx)
		if err != nil || tag == "" {
			return nil, http.StatusBadGateway, fmt.Errorf("%s；Hub 中转查不到最新版本：%s", directErr, formatUpdateError(err))
		}
		m.recordLatestTag(tag)
	}
	if parseSemver(mc.Version) != nil && !IsOutdatedVersion(mc.Version, tag) {
		return map[string]any{"ok": true, "version": mc.Version, "up_to_date": true, "relayed": true}, http.StatusOK, nil
	}
	if cache == nil {
		cache = newRelayRunCache()
		defer cache.Cleanup()
	}
	ent, f, err := cache.getOrFetch(ctx, m, tag, goos, goarch)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("%s；Hub 中转下载 %s (%s/%s) 失败：%s", directErr, tag, goos, goarch, formatUpdateError(err))
	}
	defer f.Close()

	pushReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/api/update/relay", f)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("构造中转升级请求失败：%w", err)
	}
	pushReq.ContentLength = ent.size
	pushReq.Header.Set("Content-Type", "application/octet-stream")
	pushReq.Header.Set(RelayUpdateTagHeader, tag)
	pushReq.Header.Set(RelayUpdateSHAHeader, ent.sha256)

	resp, err := reqClient.Do(pushReq)
	if err != nil {
		return nil, 0, fmt.Errorf("Hub 中转推送 %s 到 %s 失败：%s", tag, mc.Name, formatUpdateError(err))
	}
	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("Hub 中转推送 %s 到 %s 时连接中断，请稍候刷新查看版本", tag, mc.Name)
	}

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		verHint := "版本较旧"
		if strings.TrimSpace(mc.Version) != "" {
			verHint = fmt.Sprintf("当前版本（%s）较旧", strings.TrimSpace(mc.Version))
		}
		return nil, http.StatusBadGateway, fmt.Errorf("%s；已尝试由 Hub 中转分发 %s，但节点 %s %s，暂不支持接收 Hub 中转升级，请先在该节点手动运行一次 install.sh 后再使用一键更新", directErr, tag, mc.Name, verHint)
	}
	if resp.StatusCode == http.StatusOK {
		var out map[string]any
		if json.Unmarshal(rawBody, &out) != nil || out == nil {
			out = map[string]any{"ok": true, "version": tag}
		}
		out["relayed"] = true
		return out, http.StatusOK, nil
	}

	var nodeRes struct {
		Error string `json:"error"`
	}
	nodeErr := strings.TrimSpace(string(rawBody))
	if json.Unmarshal(rawBody, &nodeRes) == nil && strings.TrimSpace(nodeRes.Error) != "" {
		nodeErr = strings.TrimSpace(nodeRes.Error)
	}
	if nodeErr == "" {
		nodeErr = resp.Status
	}
	return nil, resp.StatusCode, fmt.Errorf("Hub 中转分发失败：%s", nodeErr)
}

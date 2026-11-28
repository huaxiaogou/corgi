package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	maxProjectNoteRunes = 500
	maxProjectAliases   = 20
	pathInspectTimeout  = 3 * time.Second
)

// PathInspectOutcome captures local/remote path probing together with compatibility warnings.
type PathInspectOutcome struct {
	MachineKey  string                  `json:"machine"`
	CleanPath   string                  `json:"path"`
	Result      model.PathInspectResult `json:"result"`
	Offline     bool                    `json:"offline,omitempty"`
	Unsupported bool                    `json:"unsupported,omitempty"`
	Warning     string                  `json:"warning,omitempty"`
}

// UpsertProjectRequest describes a safe create/update request for a ProjectWorkspace.
// Paths is strictly merge-only (existing machine paths are never implicitly removed).
type UpsertProjectRequest struct {
	Project string            `json:"project"`
	Name    string            `json:"name"`
	Aliases []string          `json:"aliases,omitempty"`
	HasNote bool              `json:"-"`
	Note    string            `json:"note,omitempty"`
	Machine string            `json:"machine,omitempty"`
	Path    string            `json:"path,omitempty"`
	Paths   map[string]string `json:"paths,omitempty"`
}

// UpsertProjectResult returns the saved project workspace and any non-fatal inspect warnings.
type UpsertProjectResult struct {
	Project  model.ProjectWorkspace `json:"project"`
	Created  bool                   `json:"created"`
	Warnings []string               `json:"warnings,omitempty"`
}

func (e *Engine) resolveMachineKeyForConfig(ctx context.Context, raw string) (machineKey, targetID string) {
	m := strings.TrimSpace(raw)
	if m == "" || strings.EqualFold(m, "hub") || strings.EqualFold(m, "local") {
		return hubMachineKey, ""
	}
	if e.fleet != nil {
		resolved := e.fleet.ResolveMachine(m)
		if resolved == "" {
			return hubMachineKey, ""
		}
		if _, ok := e.fleet.Machine(resolved); ok {
			return resolved, resolved
		}
	}
	// Resolve offline paired machines by display name or ID from persisted device records.
	if e.store != nil {
		if devs, err := e.store.ListDevices(ctx); err == nil {
			for _, d := range devs {
				if d != nil && d.Kind == "machine" && (d.ID == m || strings.EqualFold(strings.TrimSpace(d.Name), m)) {
					return d.ID, d.ID
				}
			}
		}
	}
	return m, m
}

// InspectMachinePath statically validates rawPath and probes its existence on Hub or a connected Node.
// Offline nodes and older Nodes without POST /api/path/inspect (HTTP 404) degrade gracefully with warnings.
func (e *Engine) InspectMachinePath(ctx context.Context, machine, rawPath string) (PathInspectOutcome, error) {
	mk, targetID := e.resolveMachineKeyForConfig(ctx, machine)
	protCorgi := ""
	if targetID == "" {
		protCorgi = effectiveProtectedCorgiDir(e.corgiDir, e.home)
	}
	cleanPath, err := ValidateProjectDir(rawPath, protCorgi)
	if err != nil {
		return PathInspectOutcome{}, err
	}
	out := PathInspectOutcome{MachineKey: mk, CleanPath: cleanPath}

	if targetID == "" {
		dbPath := ""
		if e.store != nil {
			dbPath = e.store.DBPath()
		}
		res, iErr := InspectLocalProjectDir(e.home, e.corgiDir, dbPath, cleanPath)
		if iErr != nil {
			return PathInspectOutcome{}, iErr
		}
		out.Result = res
		return out, nil
	}

	if e.fleet == nil {
		out.Offline = true
		out.Warning = fmt.Sprintf("机器 %s 暂未连接，已跳过目录存在性探测", mk)
		return out, nil
	}
	mc, ok := e.fleet.Machine(targetID)
	if !ok {
		out.Offline = true
		out.Warning = fmt.Sprintf("机器 %s 当前离线，已预配置路径但未探测目录存在性", mk)
		return out, nil
	}
	hc, err := e.fleet.HTTPClient(mc.ID)
	if err != nil {
		out.Offline = true
		out.Warning = fmt.Sprintf("机器 %s 当前离线，已预配置路径但未探测目录存在性", mk)
		return out, nil
	}

	tctx, cancel := context.WithTimeout(ctx, pathInspectTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"path": cleanPath})
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "http://node/api/path/inspect", bytes.NewReader(body))
	if err != nil {
		return PathInspectOutcome{}, fmt.Errorf("build path inspect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return PathInspectOutcome{}, fmt.Errorf("探测节点 %s 路径失败：%w", mc.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		out.Unsupported = true
		out.Warning = fmt.Sprintf("节点 %s 版本较旧，未探测目录存在性", mc.Name)
		return out, nil
	}
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&errBody)
		if errBody.Error != "" {
			return PathInspectOutcome{}, fmt.Errorf("节点 %s 拒绝路径：%s", mc.Name, errBody.Error)
		}
		return PathInspectOutcome{}, fmt.Errorf("节点 %s 路径探测失败（HTTP %d）", mc.Name, resp.StatusCode)
	}
	var res model.PathInspectResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&res); err != nil {
		return PathInspectOutcome{}, fmt.Errorf("解析节点 %s 路径探测结果失败：%w", mc.Name, err)
	}
	out.Result = res
	return out, nil
}

func validateInspectOutcome(out PathInspectOutcome) error {
	if out.Offline || out.Unsupported {
		return nil
	}
	if !out.Result.Exists {
		return fmt.Errorf("机器 %s 上不存在目录：%s", out.MachineKey, out.CleanPath)
	}
	if !out.Result.IsDir {
		return fmt.Errorf("机器 %s 上的路径不是目录：%s", out.MachineKey, out.CleanPath)
	}
	return nil
}

// UpsertProject creates or updates a ProjectWorkspace with path inspection and merge-only paths.
func (e *Engine) UpsertProject(ctx context.Context, actor string, req UpsertProjectRequest) (UpsertProjectResult, error) {
	projRef := strings.TrimSpace(req.Project)
	name := strings.TrimSpace(req.Name)
	if req.HasNote && utf8.RuneCountInString(strings.TrimSpace(req.Note)) > maxProjectNoteRunes {
		return UpsertProjectResult{}, fmt.Errorf("项目备注不能超过 %d 字", maxProjectNoteRunes)
	}
	if len(req.Aliases) > maxProjectAliases {
		return UpsertProjectResult{}, fmt.Errorf("项目别名数量不能超过 %d 个", maxProjectAliases)
	}

	rawPaths := map[string]string{}
	for m, p := range req.Paths {
		if strings.TrimSpace(p) == "" {
			return UpsertProjectResult{}, fmt.Errorf("paths 不允许空路径；移除机器路径请使用 remove_project_path")
		}
		rawPaths[m] = p
	}
	if strings.TrimSpace(req.Path) != "" || strings.TrimSpace(req.Machine) != "" {
		if strings.TrimSpace(req.Path) == "" {
			return UpsertProjectResult{}, fmt.Errorf("提供 machine 时 path 不能为空")
		}
		rawPaths[orStr(strings.TrimSpace(req.Machine), hubMachineKey)] = req.Path
	}

	// Probe all candidate paths outside config lock (strict zero-IO lock discipline).
	inspected := map[string]string{}
	var warnings []string
	for _, rawM := range slices.Sorted(maps.Keys(rawPaths)) {
		out, err := e.InspectMachinePath(ctx, rawM, rawPaths[rawM])
		if err != nil {
			return UpsertProjectResult{}, err
		}
		if err := validateInspectOutcome(out); err != nil {
			return UpsertProjectResult{}, err
		}
		inspected[out.MachineKey] = out.CleanPath
		if out.Warning != "" {
			warnings = append(warnings, out.Warning)
		}
	}

	var result UpsertProjectResult
	_, _, err := e.updateManagedConfig(ctx, actor, "upsert_project", true, func(st *model.Settings, _ *[]model.Policy) (bool, bool, string, error) {
		idx := -1
		lookupKey := orStr(projRef, name)
		if lookupKey != "" {
			for i := range st.Projects {
				if strings.EqualFold(st.Projects[i].ID, lookupKey) || strings.EqualFold(st.Projects[i].Name, lookupKey) {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			if name == "" {
				return false, false, "", fmt.Errorf("新建项目必须提供 name")
			}
			p := model.ProjectWorkspace{
				ID:      projRef,
				Name:    name,
				Aliases: append([]string(nil), req.Aliases...),
				Note:    strings.TrimSpace(req.Note),
				Paths:   inspected,
			}
			st.Projects = append(st.Projects, p)
			NormalizeDispatchConfig(st)
			saved := st.Projects[len(st.Projects)-1]
			result = UpsertProjectResult{Project: saved, Created: true, Warnings: warnings}
			return true, false, fmt.Sprintf("新增项目 %s (%s)", saved.Name, saved.ID), nil
		}

		p := st.Projects[idx]
		if name != "" {
			p.Name = name
		}
		if req.Aliases != nil {
			p.Aliases = append([]string(nil), req.Aliases...)
		}
		if req.HasNote {
			p.Note = strings.TrimSpace(req.Note)
		}
		if len(inspected) > 0 {
			if p.Paths == nil {
				p.Paths = map[string]string{}
			}
			for mk, mp := range inspected {
				p.Paths[mk] = mp
			}
		}
		st.Projects[idx] = p
		NormalizeDispatchConfig(st)
		saved := st.Projects[idx]
		result = UpsertProjectResult{Project: saved, Created: false, Warnings: warnings}
		return true, false, fmt.Sprintf("更新项目 %s (%s)", saved.Name, saved.ID), nil
	})
	if err != nil {
		return UpsertProjectResult{}, err
	}
	return result, nil
}

// SetProjectPath updates or adds one machine path mapping on an existing ProjectWorkspace.
func (e *Engine) SetProjectPath(ctx context.Context, actor, projectRef, machine, rawPath string) (UpsertProjectResult, error) {
	projectRef = strings.TrimSpace(projectRef)
	if projectRef == "" {
		return UpsertProjectResult{}, fmt.Errorf("project 不能为空")
	}
	out, err := e.InspectMachinePath(ctx, machine, rawPath)
	if err != nil {
		return UpsertProjectResult{}, err
	}
	if err := validateInspectOutcome(out); err != nil {
		return UpsertProjectResult{}, err
	}
	var warnings []string
	if out.Warning != "" {
		warnings = append(warnings, out.Warning)
	}
	var result UpsertProjectResult
	_, _, err = e.updateManagedConfig(ctx, actor, "set_project_path", true, func(st *model.Settings, _ *[]model.Policy) (bool, bool, string, error) {
		idx := findProjectIndex(st.Projects, projectRef)
		if idx < 0 {
			return false, false, "", fmt.Errorf("未找到已配置的项目：%s", projectRef)
		}
		p := st.Projects[idx]
		if p.Paths == nil {
			p.Paths = map[string]string{}
		}
		p.Paths[out.MachineKey] = out.CleanPath
		st.Projects[idx] = p
		NormalizeDispatchConfig(st)
		saved := st.Projects[idx]
		result = UpsertProjectResult{Project: saved, Warnings: warnings}
		return true, false, fmt.Sprintf("设置项目 %s 在 %s 的路径为 %s", saved.Name, out.MachineKey, out.CleanPath), nil
	})
	if err != nil {
		return UpsertProjectResult{}, err
	}
	return result, nil
}

func findProjectIndex(projects []model.ProjectWorkspace, ref string) int {
	q := strings.TrimSpace(ref)
	if q == "" {
		return -1
	}
	for i := range projects {
		if strings.EqualFold(projects[i].ID, q) || strings.EqualFold(projects[i].Name, q) {
			return i
		}
	}
	return -1
}

func applyDeleteProject(st *model.Settings, projectRef string) (string, error) {
	idx := findProjectIndex(st.Projects, projectRef)
	if idx < 0 {
		return "", fmt.Errorf("未找到已配置的项目：%s", projectRef)
	}
	removed := st.Projects[idx]
	st.Projects = append(st.Projects[:idx], st.Projects[idx+1:]...)
	NormalizeDispatchConfig(st)
	return fmt.Sprintf("删除项目 %s (%s)", removed.Name, removed.ID), nil
}

func applyRemoveProjectPath(st *model.Settings, projectRef, machineKey string) (string, error) {
	idx := findProjectIndex(st.Projects, projectRef)
	if idx < 0 {
		return "", fmt.Errorf("未找到已配置的项目：%s", projectRef)
	}
	mk := NormalizeMachineKey(machineKey)
	p := st.Projects[idx]
	if _, ok := p.Paths[mk]; !ok {
		return "", fmt.Errorf("项目 %s 未配置机器 %s 的路径", p.Name, mk)
	}
	delete(p.Paths, mk)
	st.Projects[idx] = p
	NormalizeDispatchConfig(st)
	return fmt.Sprintf("移除项目 %s 在机器 %s 的路径映射", p.Name, mk), nil
}

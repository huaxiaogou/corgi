package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	_ "modernc.org/sqlite"
)

// AgyAdapter adapts Antigravity CLI (agy) to Corgi's runtime.
type AgyAdapter struct{}

func (a *AgyAdapter) Kind() string        { return "agy" }
func (a *AgyAdapter) DisplayName() string { return "Antigravity" }

func (a *AgyAdapter) LaunchArgs(modelName, effort string, bypassPerm bool) []string {
	var args []string
	if modelName != "" {
		args = append(args, "--model", modelName)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if bypassPerm {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func (a *AgyAdapter) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--conversation", sessionID}
}

// PreTrustWorkspace writes cwd (and resolved symlink target) into agy's trustedWorkspaces
// so unattended workers never block on the first-run folder trust prompt.
func (a *AgyAdapter) PreTrustWorkspace(home, cwd string) error {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return fmt.Errorf("workspace path must be absolute: %q", cwd)
	}
	want := []string{filepath.Clean(cwd)}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != want[0] {
		want = append(want, resolved)
	}

	settingsFile := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	cfg := map[string]json.RawMessage{}
	if b, err := os.ReadFile(settingsFile); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", settingsFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", settingsFile, err)
	}

	var trusted []string
	if raw, ok := cfg["trustedWorkspaces"]; ok {
		if err := json.Unmarshal(raw, &trusted); err != nil {
			return fmt.Errorf("parse trustedWorkspaces in %s: %w", settingsFile, err)
		}
	}
	added := false
	for _, p := range want {
		if !slices.Contains(trusted, p) {
			trusted = append(trusted, p)
			added = true
		}
	}
	if !added {
		return nil
	}
	encoded, err := json.Marshal(trusted)
	if err != nil {
		return err
	}
	cfg["trustedWorkspaces"] = encoded
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsFile), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(settingsFile), err)
	}
	tmp := settingsFile + ".corgi-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, settingsFile)
}

func (a *AgyAdapter) DiscoverModels(ctx context.Context, home string) (*model.ModelCatalog, error) {
	cat := &model.ModelCatalog{
		Kind:    "agy",
		Efforts: AllEfforts,
		Models:  []model.ModelOpt{},
	}
	bin := herdr.FindBin("agy", home)
	if bin != "" {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(cctx, bin, "models").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || strings.EqualFold(fields[0], "MODEL") {
					continue
				}
				id := strings.TrimPrefix(fields[0], "*")
				if ModelNameRe.MatchString(id) && len(id) > 1 {
					label := id
					if len(fields) > 1 {
						label = strings.Join(fields[1:], " ")
					}
					cat.Models = append(cat.Models, model.ModelOpt{ID: id, Label: label})
				}
			}
		}
	}
	if len(cat.Models) == 0 {
		cat.Models = []model.ModelOpt{
			{ID: "default", Label: "Antigravity 默认模型"},
		}
	}
	return cat, nil
}

func (a *AgyAdapter) LocateTranscript(home, cwd, sessionID string, since time.Time) string {
	root := agyHome(home)
	brain := filepath.Join(root, "brain")
	if sessionID != "" {
		return preferFullTranscript(filepath.Join(brain, sessionID, ".system_generated", "logs", "transcript.jsonl"))
	}
	if cwd == "" {
		return ""
	}
	cwd = strings.TrimSuffix(filepath.Clean(cwd), "/")
	if since.IsZero() {
		since = time.Now().Add(-6 * time.Hour)
	}
	workspaces := agyWorkspaces(root)
	files, _ := filepath.Glob(filepath.Join(brain, "*", ".system_generated", "logs", "transcript.jsonl"))
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, f := range files {
		convDir := filepath.Dir(filepath.Dir(filepath.Dir(f)))
		convID := filepath.Base(convDir)
		if !slices.Contains(workspaces[convID], cwd) {
			continue
		}
		fi, err := os.Stat(f)
		if err != nil || fi.ModTime().Before(since.Add(-10*time.Second)) {
			continue
		}
		cands = append(cands, cand{path: preferFullTranscript(f), mod: fi.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
}

func preferFullTranscript(path string) string {
	if full := strings.TrimSuffix(path, "transcript.jsonl") + "transcript_full.jsonl"; full != path {
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
			return full
		}
	}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return path
	}
	return ""
}

func (a *AgyAdapter) ParseTranscriptLine(line []byte, _ string) ([]*model.Message, bool) {
	var rec struct {
		Role    string `json:"role"`
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content string `json:"content"`
		Tool    string `json:"tool"`
		Name    string `json:"name"`
		Args    string `json:"args"`
		Output  string `json:"output"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil, false
	}
	text := strings.TrimSpace(rec.Text)
	if text == "" {
		text = strings.TrimSpace(rec.Content)
	}
	switch {
	case rec.Role == "user" || rec.Type == "user":
		if text != "" {
			return []*model.Message{{Kind: "user", Text: text}}, true
		}
	case rec.Role == "assistant" || rec.Role == "model" || rec.Type == "assistant":
		if text != "" {
			return []*model.Message{{Kind: "assistant", Text: text}}, true
		}
	case rec.Tool != "" || rec.Name != "":
		name := rec.Tool
		if name == "" {
			name = rec.Name
		}
		return []*model.Message{{
			Kind:   "tool",
			Name:   name,
			Input:  clipText(rec.Args, 300),
			Output: clipText(rec.Output, 800),
		}}, true
	}
	return nil, false
}

func (a *AgyAdapter) ReadContextUsage(_ string, _ int) model.ContextUsage {
	return model.ContextUsage{}
}

func (a *AgyAdapter) LookupTitle(home, sessionID, terminalTitle string) string {
	if sessionID != "" {
		dbPath := filepath.Join(agyHome(home), "conversation_summaries.db")
		if db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)"); err == nil {
			defer db.Close()
			var title string
			if db.QueryRow(`SELECT title FROM conversation_summaries WHERE conversation_id = ?`, sessionID).Scan(&title) == nil && title != "" {
				return clipText(title, 60)
			}
		}
	}
	return cleanTerminalTitle(terminalTitle)
}

func agyHome(home string) string {
	p := filepath.Join(home, ".gemini", "antigravity-cli")
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return filepath.Join(home, ".gemini", "antigravity")
}

func agyWorkspaces(root string) map[string][]string {
	out := map[string][]string{}
	dbPath := filepath.Join(root, "conversation_summaries.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return out
	}
	defer db.Close()
	cutoff := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	rows, err := db.Query(`SELECT conversation_id, workspace_uris FROM conversation_summaries WHERE last_modified_time > ?`, cutoff)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if rows.Scan(&id, &raw) != nil {
			continue
		}
		var uris []string
		if json.Unmarshal([]byte(raw), &uris) == nil {
			for _, u := range uris {
				out[id] = append(out[id], strings.TrimSuffix(strings.TrimPrefix(u, "file://"), "/"))
			}
		}
	}
	return out
}

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
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/herdr"
	_ "modernc.org/sqlite"
)

var agyUserRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

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
	if bin := herdr.FindBin("agy", home); bin != "" {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, "models")
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
		if out, err := cmd.Output(); err == nil {
			cat.Models = parseAgyModelsOutput(string(out))
		}
	}
	if b, err := os.ReadFile(filepath.Join(agyHome(home), "settings.json")); err == nil {
		var cfg struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			if cfg.Model != "" {
				cat.Default = cfg.Model
			}
			if cfg.Effort != "" {
				cat.DefaultEffort = cfg.Effort
			}
		}
	}
	return cat, nil
}

func parseAgyModelsOutput(raw string) []model.ModelOpt {
	var models []model.ModelOpt
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		if idx := strings.LastIndexByte(line, '\r'); idx >= 0 {
			line = line[idx+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var id, label string
		if left, right, ok := strings.Cut(line, "\t"); ok {
			id = strings.TrimSpace(left)
			label = strings.TrimSpace(right)
		} else {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			id = fields[0]
			if len(fields) > 1 {
				label = strings.Join(fields[1:], " ")
			}
		}
		id = strings.TrimPrefix(id, "*")
		if id == "" || len(id) <= 1 || strings.EqualFold(id, "MODEL") || seen[id] || !ModelNameRe.MatchString(id) {
			continue
		}
		seen[id] = true
		if label == "" {
			label = id
		}
		models = append(models, model.ModelOpt{ID: id, Label: label})
	}
	return models
}

func (a *AgyAdapter) LocateTranscript(home, cwd, sessionID string, since time.Time) string {
	root := agyHome(home)
	brain := filepath.Join(root, "brain")
	if sessionID != "" {
		for _, b := range []string{brain, filepath.Join(home, ".gemini", "antigravity", "brain")} {
			if p := preferFullTranscript(filepath.Join(b, sessionID, ".system_generated", "logs", "transcript.jsonl")); p != "" {
				return p
			}
		}
		return ""
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
		Role      string `json:"role"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		Content   string `json:"content"`
		Tool      string `json:"tool"`
		Name      string `json:"name"`
		Args      string `json:"args"`
		Output    string `json:"output"`
		ToolCalls []struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return nil, false
	}

	switch rec.Type {
	case "USER_INPUT":
		text := rec.Content
		if m := agyUserRequestRe.FindStringSubmatch(text); m != nil {
			text = m[1]
		} else if strings.Contains(text, "<SYSTEM_MESSAGE>") {
			return nil, false
		}
		if text = strings.TrimSpace(text); text != "" {
			return []*model.Message{{Kind: "user", Text: text}}, true
		}
		return nil, false
	case "PLANNER_RESPONSE":
		var out []*model.Message
		if t := strings.TrimSpace(rec.Content); t != "" {
			out = append(out, &model.Message{Kind: "assistant", Text: t})
		}
		for _, tc := range rec.ToolCalls {
			out = append(out, &model.Message{
				Kind:  "tool",
				Name:  tc.Name,
				Input: summarizeAgyToolArgs(tc.Args),
			})
		}
		return out, len(out) > 0
	case "GENERIC":
		outText := cleanAgyToolOutput(rec.Content)
		if outText == "" {
			return nil, false
		}
		isErr := strings.Contains(rec.Content, "exited with code") && !strings.Contains(rec.Content, "exited with code 0")
		return []*model.Message{{
			Kind:   "tool",
			Name:   "result",
			Output: clipText(outText, 800),
			Error:  isErr,
		}}, true
	case "ERROR_MESSAGE":
		if t := strings.TrimSpace(rec.Content); t != "" {
			return []*model.Message{{Kind: "assistant", Text: "⚠ " + t}}, true
		}
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

func summarizeAgyToolArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"CommandLine", "Path", "query", "toolSummary"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return clipText(v, 300)
			}
		}
	}
	return clipText(string(raw), 300)
}

func cleanAgyToolOutput(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "Created At:") || strings.HasPrefix(l, "Completed At:") {
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (a *AgyAdapter) ReadContextUsage(transcriptPath string, _ int) model.ContextUsage {
	if transcriptPath == "" {
		return model.ContextUsage{}
	}
	// Path shape: <root>/brain/<convID>/.system_generated/logs/transcript[_full].jsonl
	convDir := filepath.Dir(filepath.Dir(filepath.Dir(transcriptPath)))
	convID := filepath.Base(convDir)
	if convID == "" || convID == "." || convID == "/" {
		return model.ContextUsage{}
	}
	root := filepath.Dir(filepath.Dir(convDir))
	dbPath := filepath.Join(root, "conversations", convID+".db")
	if _, err := os.Stat(dbPath); err != nil {
		return model.ContextUsage{}
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return model.ContextUsage{}
	}
	defer db.Close()
	rows, err := db.Query(`SELECT data FROM gen_metadata ORDER BY rowid DESC LIMIT 8`)
	if err != nil {
		return model.ContextUsage{}
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) == nil {
			if u, ok := parseAgyContextProto(raw); ok {
				return u
			}
		}
	}
	return model.ContextUsage{}
}

func parseAgyContextProto(b []byte) (model.ContextUsage, bool) {
	for _, f1 := range pbSubmessages(b, 1) {
		for _, f9 := range pbSubmessages(f1, 9) {
			for _, m := range pbSubmessages(f9, 10) {
				used, window := pbVarintField(m, 1), pbVarintField(m, 4)
				if used > 0 && window > 0 {
					return model.ContextUsage{Used: used, Window: window}, true
				}
			}
		}
	}
	return model.ContextUsage{}, false
}

func walkProtoFields(b []byte, fn func(num, wire int, val uint64, sub []byte)) {
	readVarint := func() (uint64, bool) {
		var v uint64
		for shift := 0; shift < 64; shift += 7 {
			if len(b) == 0 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			v |= uint64(c&0x7f) << shift
			if c < 0x80 {
				return v, true
			}
		}
		return 0, false
	}
	for len(b) > 0 {
		tag, ok := readVarint()
		if !ok {
			return
		}
		num, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			v, ok := readVarint()
			if !ok {
				return
			}
			fn(num, wire, v, nil)
		case 1:
			if len(b) < 8 {
				return
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return
			}
			b = b[4:]
		case 2:
			n, ok := readVarint()
			if !ok || uint64(len(b)) < n {
				return
			}
			fn(num, wire, 0, b[:n])
			b = b[n:]
		default:
			return
		}
	}
}

func pbSubmessages(b []byte, wantNum int) [][]byte {
	var out [][]byte
	walkProtoFields(b, func(num, wire int, _ uint64, sub []byte) {
		if num == wantNum && wire == 2 {
			out = append(out, sub)
		}
	})
	return out
}

func pbVarintField(b []byte, wantNum int) int {
	var out int
	walkProtoFields(b, func(num, wire int, val uint64, _ []byte) {
		if num == wantNum && wire == 0 {
			out = int(val)
		}
	})
	return out
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

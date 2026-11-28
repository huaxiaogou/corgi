package agent

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const agyFirstLineMaxBytes = 64 << 10

var (
	corgiTaskWorkerRe    = regexp.MustCompile(`\[corgi task · worker=([a-z][a-z0-9_-]{0,31})\b`)
	agyPrefixCreatedAtRe = regexp.MustCompile(`"created_at"\s*:\s*"([^"]+)"`)
	agyPrefixTypeRe      = regexp.MustCompile(`"type"\s*:\s*"([^"]+)"`)
	agyPrefixSourceRe    = regexp.MustCompile(`"source"\s*:\s*"([^"]+)"`)
)

type agyFirstLineMeta struct {
	born       time.Time
	typ        string
	source     string
	workerName string
	isSubagent bool
}

func agyWorkspaces(root string) map[string][]string {
	out := map[string][]string{}
	dbPath := filepath.Join(root, "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err != nil {
		return out
	}
	for _, dsn := range []string{
		"file:" + dbPath + "?mode=ro&_pragma=busy_timeout(2000)",
		"file:" + dbPath + "?immutable=1",
	} {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			continue
		}
		cols := agySummaryColumns(db)
		if !cols["conversation_id"] || !cols["workspace_uris"] {
			_ = db.Close()
			continue
		}
		where, args := agyTopLevelWhere(cols, time.Now().Add(-48*time.Hour))
		query := "SELECT conversation_id, workspace_uris FROM conversation_summaries"
		if where != "" {
			query += " WHERE " + where
		}
		rows, qErr := db.Query(query, args...)
		if qErr != nil {
			_ = db.Close()
			continue
		}
		for rows.Next() {
			var id, raw string
			if rows.Scan(&id, &raw) != nil {
				continue
			}
			var uris []string
			if json.Unmarshal([]byte(raw), &uris) == nil {
				for _, u := range uris {
					if p := normPath(fileURIToPath(u)); p != "" {
						out[id] = append(out[id], p)
					}
				}
			}
		}
		_ = rows.Close()
		_ = db.Close()
		return out
	}
	return out
}

func agySummaryColumns(db *sql.DB) map[string]bool {
	cols := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(conversation_summaries)`)
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk) == nil && name != "" {
			cols[strings.ToLower(name)] = true
		}
	}
	return cols
}

func agyTopLevelWhere(cols map[string]bool, cutoff time.Time) (string, []any) {
	var conds []string
	var args []any
	if cols["last_modified_time"] {
		conds = append(conds, "last_modified_time > ?")
		args = append(args, cutoff.UTC().Format("2006-01-02 15:04:05"))
	}
	if cols["nesting_depth"] {
		conds = append(conds, "COALESCE(nesting_depth, 0) = 0")
	}
	if cols["killed"] {
		conds = append(conds, "COALESCE(killed, 0) = 0")
	}
	if cols["parent_conversation_id"] {
		conds = append(conds, "COALESCE(parent_conversation_id, '') = ''")
	}
	if cols["agent_name"] {
		conds = append(conds, "COALESCE(agent_name, '') = ''")
	}
	return strings.Join(conds, " AND "), args
}

func agyTranscriptMeta(path string) (agyFirstLineMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return agyFirstLineMeta{}, false
	}
	defer f.Close()

	rd := bufio.NewReaderSize(io.LimitReader(f, agyFirstLineMaxBytes), agyFirstLineMaxBytes)
	line, err := rd.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return agyFirstLineMeta{}, false
	}
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return agyFirstLineMeta{}, false
	}

	var rec struct {
		CreatedAt string `json:"created_at"`
		Type      string `json:"type"`
		Source    string `json:"source"`
		Content   string `json:"content"`
		Text      string `json:"text"`
	}
	var meta agyFirstLineMeta
	if json.Unmarshal(line, &rec) == nil {
		meta.typ = strings.TrimSpace(rec.Type)
		meta.source = strings.TrimSpace(rec.Source)
		meta.born = parseAgyTimestamp(rec.CreatedAt)
		body := rec.Content
		if body == "" {
			body = rec.Text
		}
		if m := corgiTaskWorkerRe.FindStringSubmatch(body); len(m) == 2 {
			meta.workerName = m[1]
		}
		if meta.typ == "SYSTEM_MESSAGE" || (meta.source == "SYSTEM" && meta.typ != "" && meta.typ != "USER_INPUT") {
			meta.isSubagent = true
		} else if strings.Contains(body, "<SYSTEM_MESSAGE>") && !strings.Contains(body, "<USER_REQUEST>") {
			meta.isSubagent = true
		}
		return meta, true
	}

	// Fallback for oversized (>64 KiB) first lines: inspect bounded JSON prefix.
	raw := string(line)
	if !strings.HasPrefix(raw, "{") {
		return agyFirstLineMeta{}, false
	}
	if m := agyPrefixTypeRe.FindStringSubmatch(raw); len(m) == 2 {
		meta.typ = m[1]
	}
	if m := agyPrefixSourceRe.FindStringSubmatch(raw); len(m) == 2 {
		meta.source = m[1]
	}
	if m := agyPrefixCreatedAtRe.FindStringSubmatch(raw); len(m) == 2 {
		meta.born = parseAgyTimestamp(m[1])
	}
	if m := corgiTaskWorkerRe.FindStringSubmatch(raw); len(m) == 2 {
		meta.workerName = m[1]
	}
	if meta.typ == "SYSTEM_MESSAGE" || (meta.source == "SYSTEM" && meta.typ != "" && meta.typ != "USER_INPUT") {
		meta.isSubagent = true
	}
	return meta, meta.typ != "" || !meta.born.IsZero() || meta.workerName != ""
}

func parseAgyTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func fileURIToPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "file://") {
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			if unesc, uErr := url.PathUnescape(u.Path); uErr == nil {
				return unesc
			}
			return u.Path
		}
		trimmed := strings.TrimPrefix(raw, "file://")
		if unesc, err := url.PathUnescape(trimmed); err == nil {
			return unesc
		}
		return trimmed
	}
	return raw
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != "" {
		clean = filepath.Clean(resolved)
	}
	if clean != "/" {
		clean = strings.TrimSuffix(clean, "/")
	}
	return clean
}

func canonicalAgyTranscriptPath(path string) string {
	base := strings.TrimSuffix(path, "transcript_full.jsonl")
	if base != path {
		path = base + "transcript.jsonl"
	}
	return normPath(path)
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

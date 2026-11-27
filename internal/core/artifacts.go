package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/model"
)

const (
	ArtifactShareTTL      = 7 * 24 * time.Hour
	ArtifactSnapshotTTL   = 30 * 24 * time.Hour
	ArtifactQuotaBytes    = 200 << 20 // 200 MiB total Hub blob quota
	artifactHMACDomain    = "corgi-preview-v1:"
	artifactTokenMACBytes = 16 // 128-bit truncated HMAC-SHA256
)

// ErrArtifactUnavailable is returned uniformly for unknown, invalid, expired, revoked, or evicted artifacts.
var ErrArtifactUnavailable = errors.New("artifact unavailable")

var deliverableProsePathRe = regexp.MustCompile(`(?:^|[\s` + "`" + `"'(\[])((?:/[\p{L}\p{N}._-]+)+/[\p{L}\p{N}._-]+\.(?:html|htm|pdf|png|jpe?g|webp|gif|svg|md|csv|xlsx|docx|zip))(?:$|[\s` + "`" + `"')\].,;:!?。，；：！？])`)

// NormalizePublicURL validates and normalizes a Hub public base URL for outbound preview links.
// Empty input returns ("", nil). Rejects non-HTTP(S) schemes, credentials, queries, fragments, and traversal segments.
func NormalizePublicURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid public_url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("public_url must use http:// or https://")
	}
	if u.Host == "" || strings.ContainsAny(u.Host, " \t\r\n") {
		return "", fmt.Errorf("public_url must include a valid host")
	}
	if u.User != nil {
		return "", fmt.Errorf("public_url must not include credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("public_url must not include query parameters or fragments")
	}
	if u.Path != "" {
		for _, seg := range strings.Split(u.Path, "/") {
			if seg == "." || seg == ".." {
				return "", fmt.Errorf("public_url path must not contain traversal segments")
			}
		}
		cleanPath := path.Clean(u.Path)
		if cleanPath == "/" || cleanPath == "." {
			cleanPath = ""
		}
		u.Path = strings.TrimRight(cleanPath, "/")
	}
	u.Scheme = scheme
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// ClassifyArtifact determines safe preview kind and MIME type from basename + content bytes.
// SVG is intentionally treated as plain text so it is never executed as active content.
func ClassifyArtifact(filename string, data []byte) (kind, mimeType string) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))
	switch ext {
	case ".html", ".htm":
		return "html", "text/html; charset=utf-8"
	case ".md", ".markdown":
		return "markdown", "text/markdown; charset=utf-8"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		detected := http.DetectContentType(data)
		switch detected {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return "image", detected
		default:
			return "binary", "application/octet-stream"
		}
	case ".svg":
		return "text", "text/plain; charset=utf-8"
	}
	sample := data
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return "binary", "application/octet-stream"
	}
	return "text", "text/plain; charset=utf-8"
}

func safeDisplayFilename(name string) string {
	base := filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	base = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return "file"
	}
	return base
}

func newArtifactID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "art_" + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func (e *Engine) artifactsDir() string {
	return filepath.Join(e.corgiDir, "artifacts")
}

func (e *Engine) artifactBlobPath(id string) string {
	return filepath.Join(e.artifactsDir(), id+".blob")
}

// SetPublicURLs configures startup --url / CORGI_URL fallback candidates for preview link generation.
func (e *Engine) SetPublicURLs(raw []string) {
	var cleaned []string
	for _, item := range raw {
		if norm, err := NormalizePublicURL(item); err == nil && norm != "" {
			cleaned = append(cleaned, norm)
		}
	}
	e.mu.Lock()
	e.flagPublicURLs = cleaned
	e.mu.Unlock()
}

// FlagPublicURL returns the first valid normalized startup --url / CORGI_URL value, if any.
func (e *Engine) FlagPublicURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.flagPublicURLs) == 0 {
		return ""
	}
	return e.flagPublicURLs[0]
}

// EffectivePublicURL returns the canonical Hub base URL for outbound preview links:
// Settings.PublicURL -> first validated startup --url -> "" (disabled).
func (e *Engine) EffectivePublicURL() string {
	e.mu.Lock()
	stURL := e.settings.PublicURL
	var flagURL string
	if len(e.flagPublicURLs) > 0 {
		flagURL = e.flagPublicURLs[0]
	}
	e.mu.Unlock()

	if norm, err := NormalizePublicURL(stURL); err == nil && norm != "" {
		return norm
	}
	return flagURL
}

func (e *Engine) artifactSecret(ctx context.Context) ([]byte, error) {
	e.mu.Lock()
	sec := e.artifactHMACSecret
	e.mu.Unlock()
	if len(sec) == 32 {
		return sec, nil
	}
	loaded, err := e.store.GetOrCreateArtifactSecret(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if len(e.artifactHMACSecret) != 32 {
		e.artifactHMACSecret = append([]byte(nil), loaded...)
	}
	sec = e.artifactHMACSecret
	e.mu.Unlock()
	return sec, nil
}

// ShareTokenForArtifact returns the deterministic domain-separated HMAC capability token for an artifact ID.
func (e *Engine) ShareTokenForArtifact(ctx context.Context, artifactID string) string {
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return ""
	}
	sec, err := e.artifactSecret(ctx)
	if err != nil || len(sec) != 32 {
		return ""
	}
	mac := hmac.New(sha256.New, sec)
	mac.Write([]byte(artifactHMACDomain))
	mac.Write([]byte(artifactID))
	tag := mac.Sum(nil)[:artifactTokenMACBytes]
	return artifactID + "." + base64.RawURLEncoding.EncodeToString(tag)
}

// VerifyShareToken validates a public capability token in constant time and returns its internal artifact ID.
func (e *Engine) VerifyShareToken(ctx context.Context, token string) (string, bool) {
	token = strings.TrimSpace(token)
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot >= len(token)-1 {
		return "", false
	}
	id := token[:dot]
	rawTag := token[dot+1:]
	if !strings.HasPrefix(id, "art_") || len(id) > 64 {
		return "", false
	}
	gotTag, err := base64.RawURLEncoding.DecodeString(rawTag)
	if err != nil || len(gotTag) != artifactTokenMACBytes {
		return "", false
	}
	sec, err := e.artifactSecret(ctx)
	if err != nil || len(sec) != 32 {
		return "", false
	}
	mac := hmac.New(sha256.New, sec)
	mac.Write([]byte(artifactHMACDomain))
	mac.Write([]byte(id))
	wantTag := mac.Sum(nil)[:artifactTokenMACBytes]
	if !hmac.Equal(gotTag, wantTag) {
		return "", false
	}
	return id, true
}

// BuildPreviewURL returns the canonical trailing-slash preview URL (<base>/p/<token>/) or "" when no base URL is configured.
func (e *Engine) BuildPreviewURL(ctx context.Context, artifactID string) string {
	base := e.EffectivePublicURL()
	if base == "" {
		return ""
	}
	tok := e.ShareTokenForArtifact(ctx, artifactID)
	if tok == "" {
		return ""
	}
	return base + "/p/" + tok + "/"
}

// stageArtifacts writes immutable blobs privately to ~/.corgi/artifacts/ using sync+rename before SQLite commit.
func (e *Engine) stageArtifacts(
	ctx context.Context,
	entryID, thread, workID string,
	fetched []AttachmentFile,
	now time.Time,
) ([]*model.Artifact, []model.ArtifactSummary, func(), error) {
	if len(fetched) == 0 {
		return nil, nil, func() {}, nil
	}
	dir := e.artifactsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, func() {}, fmt.Errorf("create artifacts dir: %w", err)
	}

	var (
		stagedPaths []string
		records     = make([]*model.Artifact, 0, len(fetched))
		summaries   = make([]model.ArtifactSummary, 0, len(fetched))
	)
	cleanup := func() {
		for _, p := range stagedPaths {
			_ = os.Remove(p)
		}
	}

	for i := range fetched {
		id, err := newArtifactID()
		if err != nil {
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("allocate artifact id: %w", err)
		}
		name := safeDisplayFilename(fetched[i].Name)
		kind, mimeType := ClassifyArtifact(name, fetched[i].Data)
		finalPath := e.artifactBlobPath(id)
		tmpPath := finalPath + ".tmp"

		f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("stage artifact blob: %w", err)
		}
		if _, err := f.Write(fetched[i].Data); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("write artifact blob: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("sync artifact blob: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(tmpPath)
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("close artifact blob: %w", err)
		}
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("commit artifact blob: %w", err)
		}
		stagedPaths = append(stagedPaths, finalPath)

		recEntryID := entryID
		if recEntryID == "" {
			// Assign a unique synthetic key for standalone snapshots so SQLite's
			// UNIQUE(entry_id, ordinal) never collides across multiple worker reports.
			recEntryID = "standalone:" + id
		}
		rec := &model.Artifact{
			ID:             id,
			EntryID:        recEntryID,
			Ordinal:        i,
			Thread:         thread,
			WorkItemID:     workID,
			Name:           name,
			Kind:           kind,
			MIME:           mimeType,
			Size:           int64(len(fetched[i].Data)),
			SourcePath:     fetched[i].Path,
			SourceMachine:  fetched[i].Machine,
			CreatedAt:      now,
			ShareExpiresAt: now.Add(ArtifactShareTTL),
			ExpiresAt:      now.Add(ArtifactSnapshotTTL),
		}
		records = append(records, rec)
		summaries = append(summaries, model.ArtifactSummary{
			ID:   id,
			Name: name,
			Kind: kind,
			Size: rec.Size,
		})
		fetched[i].ArtifactID = id
		fetched[i].Kind = kind
		fetched[i].Name = name
		fetched[i].ShareURL = e.BuildPreviewURL(ctx, id)
	}
	return records, summaries, cleanup, nil
}

// SaveStandaloneTextArtifact persists a full text/Markdown report directly into ~/.corgi/artifacts/
// and SQLite before any Router truncation, linking it to workID (小本本) when provided.
func (e *Engine) SaveStandaloneTextArtifact(
	ctx context.Context,
	filename, content, thread, workID, sourceMachine string,
) (*model.Artifact, model.ArtifactSummary, error) {
	data := []byte(strings.TrimSpace(content))
	if len(data) == 0 {
		return nil, model.ArtifactSummary{}, fmt.Errorf("empty artifact content")
	}
	if thread == "" {
		thread = model.MainThread
	}
	now := time.Now()
	fetched := []AttachmentFile{{
		Name:    filename,
		Machine: sourceMachine,
		Path:    filename,
		Data:    data,
	}}
	recs, sums, cleanup, err := e.stageArtifacts(ctx, "", thread, strings.TrimSpace(workID), fetched, now)
	if err != nil {
		return nil, model.ArtifactSummary{}, err
	}
	if len(recs) == 0 {
		cleanup()
		return nil, model.ArtifactSummary{}, fmt.Errorf("failed to stage text artifact")
	}
	art := recs[0]
	sum := sums[0]
	if err := e.store.InsertArtifact(ctx, art); err != nil {
		cleanup()
		return nil, model.ArtifactSummary{}, err
	}
	if strings.TrimSpace(workID) != "" {
		_, _ = e.store.RecordWorkItemArtifact(ctx, strings.TrimSpace(workID), model.WorkArtifact{
			ArtifactID:  art.ID,
			Name:        art.Name,
			Kind:        art.Kind,
			Size:        art.Size,
			Path:        art.Name,
			Machine:     sourceMachine,
			DeliveredAt: now,
		})
	}
	go e.SweepArtifacts(context.Background())
	return art, sum, nil
}

// captureWorkerReportSnapshot stores a full untruncated worker report as an immutable Hub Markdown
// snapshot when it is long enough to be a substantive deliverable, deduplicated by content hash.
func (e *Engine) captureWorkerReportSnapshot(ctx context.Context, wk *model.Worker, fullText string, minRunes int) string {
	if wk == nil {
		return ""
	}
	trimmed := strings.TrimSpace(fullText)
	if trimmed == "" || trimmed == "(no output captured)" {
		return ""
	}
	if strings.HasPrefix(strings.ToUpper(trimmed), "BLOCKED:") {
		return ""
	}
	if utf8.RuneCountInString(trimmed) < minRunes {
		return ""
	}
	sumBytes := sha256.Sum256([]byte(trimmed))
	hash := hex.EncodeToString(sumBytes[:16])
	if wk.LastReportHash == hash && wk.LastReportArtifactID != "" {
		return wk.LastReportArtifactID
	}
	name := safeDisplayFilename(wk.Name + "-完整报告.md")
	art, _, err := e.SaveStandaloneTextArtifact(ctx, name, trimmed, wk.Thread, wk.WorkItemID, wk.Machine)
	if err != nil {
		e.logger.Warn("capture worker report snapshot failed", "worker", wk.Name, "err", err)
		return ""
	}
	wk.LastReportArtifactID = art.ID
	wk.LastReportHash = hash
	_ = e.store.UpsertWorker(ctx, wk)
	e.notifyChanged()
	return art.ID
}

// ReadArtifactForConsole loads an artifact and its immutable bytes for an authenticated Web console session.
// Remains readable until snapshot retention expires even after the shorter public share link expires.
func (e *Engine) ReadArtifactForConsole(ctx context.Context, id string) (*model.Artifact, []byte, error) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "art_") {
		return nil, nil, ErrArtifactUnavailable
	}
	art, err := e.store.GetArtifact(ctx, id)
	if err != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	if art.RevokedAt != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	if !e.isArtifactRetained(ctx, art, time.Now()) {
		return nil, nil, ErrArtifactUnavailable
	}
	data, err := os.ReadFile(e.artifactBlobPath(art.ID))
	if err != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	return art, data, nil
}

// ResolvePublicShareToken validates a public capability token, share expiry, revocation, and blob presence.
func (e *Engine) ResolvePublicShareToken(ctx context.Context, token string) (*model.Artifact, []byte, error) {
	id, ok := e.VerifyShareToken(ctx, token)
	if !ok {
		return nil, nil, ErrArtifactUnavailable
	}
	art, err := e.store.GetArtifact(ctx, id)
	if err != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	now := time.Now()
	if art.RevokedAt != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	if !art.ShareExpiresAt.IsZero() && now.After(art.ShareExpiresAt) {
		return nil, nil, ErrArtifactUnavailable
	}
	if !e.isArtifactRetained(ctx, art, now) {
		return nil, nil, ErrArtifactUnavailable
	}
	data, err := os.ReadFile(e.artifactBlobPath(art.ID))
	if err != nil {
		return nil, nil, ErrArtifactUnavailable
	}
	return art, data, nil
}

// RevokeArtifact revokes public share access to an artifact immediately.
func (e *Engine) RevokeArtifact(ctx context.Context, id string) error {
	if err := e.store.RevokeArtifact(ctx, strings.TrimSpace(id)); err != nil {
		return err
	}
	e.notifyChanged()
	return nil
}

func (e *Engine) isArtifactRetained(ctx context.Context, art *model.Artifact, now time.Time) bool {
	if art == nil {
		return false
	}
	if art.WorkItemID != "" {
		if wi, err := e.store.GetWorkItem(ctx, art.WorkItemID); err == nil && wi != nil {
			if wi.ClosedAt == nil {
				return true // Open work item keeps its snapshots retained.
			}
			if now.Before(wi.ClosedAt.Add(ArtifactSnapshotTTL)) {
				return true
			}
		}
	}
	return art.ExpiresAt.IsZero() || now.Before(art.ExpiresAt)
}

func (e *Engine) blobExists(id string) bool {
	fi, err := os.Stat(e.artifactBlobPath(id))
	return err == nil && fi.Mode().IsRegular()
}

// HydrateEntriesArtifacts enriches entry.Artifacts dynamically with ShareToken and Expired status
// without ever persisting bearer credentials inside SQLite.
func (e *Engine) HydrateEntriesArtifacts(ctx context.Context, entries []*model.Entry) {
	var ids []string
	for _, en := range entries {
		if en == nil {
			continue
		}
		for _, a := range en.Artifacts {
			if a.ID != "" {
				ids = append(ids, a.ID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	metaByID, err := e.store.ListArtifactsByIDs(ctx, ids)
	if err != nil {
		return
	}
	now := time.Now()
	for _, en := range entries {
		if en == nil || len(en.Artifacts) == 0 {
			continue
		}
		for i := range en.Artifacts {
			sum := &en.Artifacts[i]
			art, ok := metaByID[sum.ID]
			if !ok || art.RevokedAt != nil || !e.isArtifactRetained(ctx, art, now) || !e.blobExists(art.ID) {
				sum.Expired = true
				sum.ShareToken = ""
				continue
			}
			sum.Expired = false
			if art.ShareExpiresAt.IsZero() || now.Before(art.ShareExpiresAt) {
				sum.ShareToken = e.ShareTokenForArtifact(ctx, art.ID)
			}
		}
	}
}

// HydrateWorkItemsSnapshots projects retained Hub snapshots onto open WorkItem views for the UI notebook
// without modifying persisted WorkArtifact.DeliveredAt semantics.
func (e *Engine) HydrateWorkItemsSnapshots(ctx context.Context, items []*model.WorkItem) {
	if len(items) == 0 {
		return
	}
	workIDs := make([]string, 0, len(items))
	for _, wi := range items {
		if wi != nil && wi.ID != "" {
			workIDs = append(workIDs, wi.ID)
		}
	}
	byWork, err := e.store.ListArtifactsByWorkIDs(ctx, workIDs)
	if err != nil || len(byWork) == 0 {
		return
	}
	now := time.Now()
	for _, wi := range items {
		if wi == nil {
			continue
		}
		arts := byWork[wi.ID]
		if len(arts) == 0 {
			continue
		}
		snaps := make([]model.ArtifactSummary, 0, len(arts))
		for _, art := range arts {
			expired := art.RevokedAt != nil || !e.isArtifactRetained(ctx, art, now) || !e.blobExists(art.ID)
			sum := model.ArtifactSummary{
				ID:      art.ID,
				Name:    art.Name,
				Kind:    art.Kind,
				Size:    art.Size,
				Expired: expired,
			}
			if !expired && (art.ShareExpiresAt.IsZero() || now.Before(art.ShareExpiresAt)) {
				sum.ShareToken = e.ShareTokenForArtifact(ctx, art.ID)
			}
			snaps = append(snaps, sum)
		}
		wi.Snapshots = snaps
	}
}

// SweepArtifacts cleans orphaned temp/blob files on disk, deletes expired snapshots (preserving open work items),
// and enforces the disk quota oldest-first while keeping lightweight DB tombstones for chat display.
func (e *Engine) SweepArtifacts(ctx context.Context) {
	dir := e.artifactsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	allMeta, err := e.store.ListAllArtifacts(ctx)
	if err != nil {
		return
	}
	known := make(map[string]*model.Artifact, len(allMeta))
	for _, art := range allMeta {
		known[art.ID] = art
	}

	now := time.Now()
	// 1. Remove orphaned blobs & stale staging temp files.
	for _, de := range entries {
		name := de.Name()
		full := filepath.Join(dir, name)
		if strings.HasSuffix(name, ".tmp") {
			if fi, sErr := de.Info(); sErr == nil && now.Sub(fi.ModTime()) > 5*time.Minute {
				_ = os.Remove(full)
			}
			continue
		}
		if !strings.HasSuffix(name, ".blob") {
			continue
		}
		id := strings.TrimSuffix(name, ".blob")
		if _, ok := known[id]; !ok {
			_ = os.Remove(full)
		}
	}

	// 2. Delete expired/revoked blobs and compute remaining live byte total (oldest-first).
	type liveBlob struct {
		art  *model.Artifact
		size int64
	}
	var live []liveBlob
	var totalBytes int64
	for _, art := range allMeta {
		p := e.artifactBlobPath(art.ID)
		fi, sErr := os.Stat(p)
		if sErr != nil {
			continue
		}
		if art.RevokedAt != nil || !e.isArtifactRetained(ctx, art, now) {
			_ = os.Remove(p)
			continue
		}
		live = append(live, liveBlob{art: art, size: fi.Size()})
		totalBytes += fi.Size()
	}

	// 3. Enforce disk quota oldest-first (keep at least the newest single delivery).
	for i := 0; totalBytes > ArtifactQuotaBytes && i < len(live)-1; i++ {
		_ = os.Remove(e.artifactBlobPath(live[i].art.ID))
		totalBytes -= live[i].size
	}
}

func mentionsDeliverablePath(text string) string {
	m := deliverableProsePathRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func findFetchedBytes(list []AttachmentFile, artifactID string) []byte {
	for _, f := range list {
		if f.ArtifactID == artifactID {
			return f.Data
		}
	}
	return nil
}

package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kiperjing/corgi/internal/model"
)

type stageBudget struct {
	ctx            context.Context
	cancel         context.CancelFunc
	remainingFiles int
	remainingBytes int64
}

func newStageBudget(parent context.Context) *stageBudget {
	bctx, cancel := context.WithTimeout(parent, stageBatchTimeout)
	return &stageBudget{
		ctx:            bctx,
		cancel:         cancel,
		remainingFiles: MaxStageFilesPerDispatch,
		remainingBytes: MaxStageTotalBytes,
	}
}

func (b *stageBudget) close() {
	if b != nil && b.cancel != nil {
		b.cancel()
	}
}

func (e *Engine) normStageMachine(m string) string {
	s := strings.TrimSpace(m)
	if s == "" || strings.EqualFold(s, "hub") || strings.EqualFold(s, "local") {
		return ""
	}
	if e.fleet != nil {
		return e.fleet.ResolveMachine(s)
	}
	return s
}

func (e *Engine) dbPath() string {
	if e.store == nil {
		return ""
	}
	return e.store.DBPath()
}

// collectTurnMedia gathers deduplicated trusted media candidates from a verified human turn only.
func (e *Engine) collectTurnMedia(ctx context.Context, thread string, trig *model.Trigger, meta *turnConsentMeta, texts ...string) []model.MediaRef {
	if meta == nil || !meta.isHuman {
		return nil
	}
	var raw []model.MediaRef
	structuredPaths := map[string]bool{}
	addStructured := func(refs []model.MediaRef) {
		for _, r := range refs {
			if p := strings.TrimSpace(r.Path); p != "" {
				structuredPaths[filepath.Clean(p)] = true
				raw = append(raw, r)
			}
		}
	}
	if trig != nil {
		addStructured(trig.Media)
	}
	if meta.sourceEntry != nil && isHumanChannel(meta.sourceEntry.Channel) {
		addStructured(meta.sourceEntry.Media)
	}
	combined := strings.Join(texts, "\n")
	if strings.TrimSpace(combined) != "" {
		if e.store != nil {
			if entries, _, err := e.store.ListEntries(ctx, orStr(thread, model.MainThread), 20); err == nil {
				for _, en := range entries {
					if en == nil || en.Role != "user" || !isHumanChannel(en.Channel) {
						continue
					}
					for _, ref := range en.Media {
						if p := strings.TrimSpace(ref.Path); p != "" && strings.Contains(combined, p) {
							structuredPaths[filepath.Clean(p)] = true
							raw = append(raw, ref)
						}
					}
				}
			}
		}
		for _, p := range extractHubUploadPaths(e.corgiDir, combined) {
			if !structuredPaths[filepath.Clean(p)] {
				raw = append(raw, model.MediaRef{Path: p, Name: filepath.Base(p)})
			}
		}
	}
	return e.dedupMediaRefs(raw)
}

func extractHubUploadPaths(corgiDir, text string) []string {
	if strings.TrimSpace(corgiDir) == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	prefixes := []string{filepath.ToSlash(filepath.Clean(filepath.Join(corgiDir, "uploads"))) + "/"}
	if realDir, err := filepath.EvalSymlinks(filepath.Join(corgiDir, "uploads")); err == nil {
		rp := filepath.ToSlash(filepath.Clean(realDir)) + "/"
		if rp != prefixes[0] {
			prefixes = append(prefixes, rp)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, pref := range prefixes {
		re := regexp.MustCompile(regexp.QuoteMeta(pref) + `[^\s"'<>|]+`)
		for _, match := range re.FindAllString(text, 20) {
			p := strings.TrimRight(match, ".,;:!?)]}，。；：！？】）")
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func (e *Engine) dedupMediaRefs(raw []model.MediaRef) []model.MediaRef {
	seen := map[string]bool{}
	out := make([]model.MediaRef, 0, len(raw))
	for _, r := range raw {
		p := strings.TrimSpace(r.Path)
		if p == "" || hasControlRune(p) {
			continue
		}
		m := e.normStageMachine(r.Machine)
		key := m + "\x00" + filepath.Clean(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		cp := r
		cp.Path = p
		cp.Machine = m
		if cp.Name == "" {
			cp.Name = filepath.Base(p)
		}
		out = append(out, cp)
	}
	return out
}

// stageTurnMediaForGoal stages eligible media refs onto targetMachine for verified human turns only.
func (e *Engine) stageTurnMediaForGoal(ctx context.Context, targetMachine, goal, origReq string, trig *model.Trigger, meta *turnConsentMeta) (string, string, []string, error) {
	if meta == nil || !meta.isHuman {
		return goal, origReq, nil, nil
	}
	thread := model.MainThread
	if trig != nil && strings.TrimSpace(trig.Thread) != "" {
		thread = trig.Thread
	}
	refs := e.collectTurnMedia(ctx, thread, trig, meta, goal, origReq)
	if len(refs) == 0 {
		return goal, origReq, nil, nil
	}
	b := newStageBudget(ctx)
	defer b.close()
	texts, warns, err := e.stageMediaForTextsWithBudget(b, targetMachine, []string{goal, origReq}, refs)
	if err != nil {
		return goal, origReq, warns, err
	}
	return texts[0], texts[1], warns, nil
}

// stageMediaForDirectText stages explicit media refs (and unreferenced Hub upload paths) for direct delivery.
func (e *Engine) stageMediaForDirectText(ctx context.Context, targetMachine, text string, refs []model.MediaRef) (string, []string, error) {
	b := newStageBudget(ctx)
	defer b.close()
	return e.stageMediaForDirectTextWithBudget(b, targetMachine, text, refs)
}

func (e *Engine) stageMediaForDirectTextWithBudget(b *stageBudget, targetMachine, text string, refs []model.MediaRef) (string, []string, error) {
	structuredPaths := map[string]bool{}
	merged := make([]model.MediaRef, 0, len(refs))
	for _, r := range refs {
		if p := strings.TrimSpace(r.Path); p != "" {
			structuredPaths[filepath.Clean(p)] = true
			merged = append(merged, r)
		}
	}
	for _, p := range extractHubUploadPaths(e.corgiDir, text) {
		if !structuredPaths[filepath.Clean(p)] {
			merged = append(merged, model.MediaRef{Path: p, Name: filepath.Base(p)})
		}
	}
	dedup := e.dedupMediaRefs(merged)
	if len(dedup) == 0 {
		return text, nil, nil
	}
	texts, warns, err := e.stageMediaForTextsWithBudget(b, targetMachine, []string{text}, dedup)
	if err != nil {
		return text, warns, err
	}
	return texts[0], warns, nil
}

func (e *Engine) stageMediaForTextsWithBudget(b *stageBudget, targetMachine string, texts []string, refs []model.MediaRef) ([]string, []string, error) {
	if len(refs) == 0 || len(texts) == 0 {
		return texts, nil, nil
	}
	dst := e.normStageMachine(targetMachine)
	var (
		warnings       []string
		replPairs      []string
		missingCross   []string
		missingLocal   []string
		failedNotes    []string
		crossAttempted int
		crossSucceeded int
		sameUsable     int
		firstCrossErr  error
	)
	type replacement struct{ oldPath, newPath string }
	var repls []replacement

	for _, ref := range refs {
		if b.remainingFiles <= 0 {
			w := fmt.Sprintf("附件 %s 超过单次派发最多 %d 个文件上限，已跳过", ref.Name, MaxStageFilesPerDispatch)
			warnings = append(warnings, w)
			failedNotes = append(failedNotes, fmt.Sprintf("- %s：%s", ref.Name, w))
			repls = append(repls, replacement{oldPath: ref.Path, newPath: "[未同步: " + ref.Name + "]"})
			continue
		}
		b.remainingFiles--

		src := e.normStageMachine(ref.Machine)
		if src == dst {
			if err := e.validateSameMachineRef(src, ref.Path); err != nil {
				w := fmt.Sprintf("附件 %s 校验失败：%v", ref.Name, err)
				warnings = append(warnings, w)
				failedNotes = append(failedNotes, fmt.Sprintf("- %s：%s", ref.Name, w))
				repls = append(repls, replacement{oldPath: ref.Path, newPath: "[未同步: " + ref.Name + "]"})
				continue
			}
			sameUsable++
			if !strings.Contains(texts[0], ref.Path) {
				missingLocal = append(missingLocal, ref.Path)
			}
			continue
		}

		crossAttempted++
		if b.remainingBytes <= 0 {
			w := fmt.Sprintf("附件 %s 超过单次派发 %d MB 总限额，已跳过", ref.Name, MaxStageTotalBytes>>20)
			warnings = append(warnings, w)
			failedNotes = append(failedNotes, fmt.Sprintf("- %s：%s", ref.Name, w))
			repls = append(repls, replacement{oldPath: ref.Path, newPath: "[未同步: " + ref.Name + "]"})
			if firstCrossErr == nil {
				firstCrossErr = errors.New(w)
			}
			continue
		}

		fctx, fcancel := context.WithTimeout(b.ctx, stageSingleFileTimeout)
		destPath, transferred, sErr := e.stageOneCrossMachineFile(fctx, src, dst, ref, b.remainingBytes)
		fcancel()
		if transferred > 0 {
			b.remainingBytes -= transferred
			if b.remainingBytes < 0 {
				b.remainingBytes = 0
			}
		}
		if sErr != nil {
			if firstCrossErr == nil {
				firstCrossErr = sErr
			}
			w := fmt.Sprintf("附件 %s 同步到 %s 失败：%v", ref.Name, orStr(dst, "hub"), sErr)
			warnings = append(warnings, w)
			failedNotes = append(failedNotes, fmt.Sprintf("- %s：%v", ref.Name, sErr))
			repls = append(repls, replacement{oldPath: ref.Path, newPath: "[未同步: " + ref.Name + "]"})
			continue
		}
		crossSucceeded++
		repls = append(repls, replacement{oldPath: ref.Path, newPath: destPath})
		if !strings.Contains(texts[0], ref.Path) {
			missingCross = append(missingCross, destPath)
		}
	}

	if crossAttempted > 0 && crossSucceeded == 0 && sameUsable == 0 {
		return texts, warnings, fmt.Errorf("跨机同步附件全部失败（目标机 %s）：%w", orStr(dst, "hub"), firstCrossErr)
	}

	sort.Slice(repls, func(i, j int) bool { return len(repls[i].oldPath) > len(repls[j].oldPath) })
	for _, rp := range repls {
		replPairs = append(replPairs, rp.oldPath, rp.newPath)
	}
	out := append([]string(nil), texts...)
	if len(replPairs) > 0 {
		replacer := strings.NewReplacer(replPairs...)
		for i := range out {
			if out[i] != "" {
				out[i] = replacer.Replace(out[i])
			}
		}
	}
	if len(missingCross) > 0 {
		out[0] = strings.TrimRight(out[0], "\n") + "\n\n[已同步到本机的附件]\n- " + strings.Join(missingCross, "\n- ")
	}
	if len(missingLocal) > 0 {
		out[0] = strings.TrimRight(out[0], "\n") + "\n\n[随消息附带的文件]\n- " + strings.Join(missingLocal, "\n- ")
	}
	if len(failedNotes) > 0 {
		out[0] = strings.TrimRight(out[0], "\n") + "\n\n[未同步的附件]\n" + strings.Join(failedNotes, "\n")
	}
	return out, warnings, nil
}

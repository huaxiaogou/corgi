package agent

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	agyWorkspaceIndexTTL = 3 * time.Second
	agyStartWindowGrace  = 10 * time.Second
)

type agyPaneClaim struct {
	paneID     string
	sessionID  string
	since      time.Time
	sinceKnown bool
}

// LocateTranscript preserves the 4-argument Adapter contract by delegating to the
// deterministic context-aware locator.
func (a *AgyAdapter) LocateTranscript(home, cwd, sessionID string, since time.Time) string {
	return a.LocateTranscriptWithHint(home, cwd, sessionID, since, LocateHint{
		SinceKnown: !since.IsZero(),
	}).Path
}

// LocateTranscriptWithHint resolves an Antigravity transcript using strict 3-step attribution:
// Step 1: explicit sessionID (never falls back to guessing when missing on disk).
// Step 2: two-layer filter (SQLite dynamic top-level schema + first-line immutable metadata + claim exclusion).
// Step 3: return Verified result only on unique exact worker match or unique launch-window match;
// otherwise return empty (no structured transcript) rather than guessing by mtime.
func (a *AgyAdapter) LocateTranscriptWithHint(home, cwd, sessionID string, since time.Time, hint LocateHint) LocateResult {
	root := agyHome(home)
	brain := filepath.Join(root, "brain")
	paneID := strings.TrimSpace(hint.PaneID)
	workerName := strings.TrimSpace(hint.WorkerName)
	sessionID = strings.TrimSpace(sessionID)

	// Step 1: explicit session ID takes absolute priority and never downgrades to guessing.
	if sessionID != "" {
		for _, b := range []string{brain, filepath.Join(home, ".gemini", "antigravity", "brain")} {
			base := filepath.Join(b, sessionID, ".system_generated", "logs", "transcript.jsonl")
			canon := canonicalAgyTranscriptPath(base)
			if p := preferFullTranscript(canon); p != "" {
				if paneID != "" {
					a.claimExplicit(paneID, canon, sessionID, since, hint.SinceKnown)
				}
				return LocateResult{Path: p, SessionID: sessionID, Verified: true}
			}
		}
		return LocateResult{}
	}

	// Step 2a: reuse live claim for this pane when generation (startedAt) still matches.
	if paneID != "" {
		if canon, sess, ok := a.existingClaim(paneID, since, hint.SinceKnown); ok {
			if p := preferFullTranscript(canon); p != "" {
				return LocateResult{Path: p, SessionID: sess, Verified: true}
			}
			a.ReleasePane(paneID)
		}
	}

	if strings.TrimSpace(cwd) == "" {
		return LocateResult{}
	}
	wantCwd := normPath(cwd)

	// Layer 1: dynamic SQLite top-level conversation index (IO strictly outside lock).
	workspaces := a.cachedWorkspaces(root)
	if len(workspaces) == 0 {
		return LocateResult{}
	}

	otherClaimed := a.snapshotOtherClaims(paneID)

	// Layer 2: first-line immutable metadata & worker identity filtering (IO outside lock).
	files, _ := filepath.Glob(filepath.Join(brain, "*", ".system_generated", "logs", "transcript.jsonl"))
	type cand struct {
		canonical string
		convID    string
		born      time.Time
		exact     bool
	}
	var cands []cand
	for _, f := range files {
		convID := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f))))
		if !slices.Contains(workspaces[convID], wantCwd) {
			continue
		}
		canon := canonicalAgyTranscriptPath(f)
		if otherClaimed[canon] {
			continue
		}
		meta, ok := agyTranscriptMeta(f)
		if !ok || meta.isSubagent {
			continue
		}
		exact := false
		if workerName != "" && meta.workerName != "" {
			if meta.workerName != workerName {
				continue
			}
			exact = true
		}
		cands = append(cands, cand{
			canonical: canon,
			convID:    convID,
			born:      meta.born,
			exact:     exact,
		})
	}

	// Step 3: strict convergence — never guess cands[0] by mtime.
	var exactCands []cand
	for _, c := range cands {
		if c.exact {
			exactCands = append(exactCands, c)
		}
	}
	if len(exactCands) > 1 && hint.SinceKnown {
		var narrowed []cand
		cutoff := since.Add(-agyStartWindowGrace)
		for _, c := range exactCands {
			if !c.born.IsZero() && !c.born.Before(cutoff) {
				narrowed = append(narrowed, c)
			}
		}
		exactCands = narrowed
	}

	var chosen *cand
	switch {
	case len(exactCands) == 1:
		c := exactCands[0]
		chosen = &c
	case len(exactCands) == 0 && hint.SinceKnown:
		cutoff := since.Add(-agyStartWindowGrace)
		var winCands []cand
		for _, c := range cands {
			if !c.born.IsZero() && !c.born.Before(cutoff) {
				winCands = append(winCands, c)
			}
		}
		if len(winCands) == 1 {
			c := winCands[0]
			chosen = &c
		}
	}

	if chosen == nil {
		return LocateResult{}
	}

	if paneID != "" {
		if !a.tryClaim(paneID, chosen.canonical, chosen.convID, since, hint.SinceKnown) {
			return LocateResult{}
		}
	}
	fullPath := preferFullTranscript(chosen.canonical)
	if fullPath == "" {
		if paneID != "" {
			a.ReleasePane(paneID)
		}
		return LocateResult{}
	}
	return LocateResult{Path: fullPath, SessionID: chosen.convID, Verified: true}
}

// ReleasePane removes any transcript claim owned by paneID.
func (a *AgyAdapter) ReleasePane(paneID string) {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return
	}
	a.claimMu.Lock()
	a.releasePaneLocked(paneID)
	a.claimMu.Unlock()
}

// ReconcileLivePanes drops claims belonging to panes absent from livePaneIDs.
func (a *AgyAdapter) ReconcileLivePanes(livePaneIDs map[string]bool) {
	a.claimMu.Lock()
	for paneID, canon := range a.paneOwner {
		if !livePaneIDs[paneID] {
			delete(a.paneOwner, paneID)
			if cl, ok := a.claimed[canon]; ok && cl.paneID == paneID {
				delete(a.claimed, canon)
			}
		}
	}
	for canon, cl := range a.claimed {
		if !livePaneIDs[cl.paneID] {
			delete(a.claimed, canon)
		}
	}
	a.claimMu.Unlock()
}

func (a *AgyAdapter) releasePaneLocked(paneID string) {
	if canon, ok := a.paneOwner[paneID]; ok {
		delete(a.paneOwner, paneID)
		if cl, exists := a.claimed[canon]; exists && cl.paneID == paneID {
			delete(a.claimed, canon)
		}
	}
}

func (a *AgyAdapter) existingClaim(paneID string, since time.Time, sinceKnown bool) (string, string, bool) {
	a.claimMu.Lock()
	defer a.claimMu.Unlock()
	canon, ok := a.paneOwner[paneID]
	if !ok {
		return "", "", false
	}
	cl, exists := a.claimed[canon]
	if !exists || cl.paneID != paneID {
		delete(a.paneOwner, paneID)
		return "", "", false
	}
	if cl.sinceKnown != sinceKnown || (sinceKnown && !cl.since.Equal(since)) {
		a.releasePaneLocked(paneID)
		return "", "", false
	}
	return canon, cl.sessionID, true
}

func (a *AgyAdapter) snapshotOtherClaims(selfPaneID string) map[string]bool {
	a.claimMu.Lock()
	defer a.claimMu.Unlock()
	if len(a.claimed) == 0 {
		return nil
	}
	out := make(map[string]bool, len(a.claimed))
	for canon, cl := range a.claimed {
		if cl.paneID != "" && cl.paneID != selfPaneID {
			out[canon] = true
		}
	}
	return out
}

func (a *AgyAdapter) claimExplicit(paneID, canon, sessionID string, since time.Time, sinceKnown bool) {
	a.claimMu.Lock()
	defer a.claimMu.Unlock()
	if a.claimed == nil {
		a.claimed = map[string]agyPaneClaim{}
	}
	if a.paneOwner == nil {
		a.paneOwner = map[string]string{}
	}
	a.releasePaneLocked(paneID)
	if prev, ok := a.claimed[canon]; ok && prev.paneID != paneID {
		if a.paneOwner[prev.paneID] == canon {
			delete(a.paneOwner, prev.paneID)
		}
	}
	a.claimed[canon] = agyPaneClaim{paneID: paneID, sessionID: sessionID, since: since, sinceKnown: sinceKnown}
	a.paneOwner[paneID] = canon
}

func (a *AgyAdapter) tryClaim(paneID, canon, sessionID string, since time.Time, sinceKnown bool) bool {
	a.claimMu.Lock()
	defer a.claimMu.Unlock()
	if a.claimed == nil {
		a.claimed = map[string]agyPaneClaim{}
	}
	if a.paneOwner == nil {
		a.paneOwner = map[string]string{}
	}
	if existing, taken := a.claimed[canon]; taken && existing.paneID != paneID {
		return false
	}
	a.releasePaneLocked(paneID)
	a.claimed[canon] = agyPaneClaim{paneID: paneID, sessionID: sessionID, since: since, sinceKnown: sinceKnown}
	a.paneOwner[paneID] = canon
	return true
}

func (a *AgyAdapter) cachedWorkspaces(root string) map[string][]string {
	a.indexMu.Lock()
	if a.indexWS != nil && a.indexRoot == root && time.Since(a.indexAt) < agyWorkspaceIndexTTL {
		ws := a.indexWS
		a.indexMu.Unlock()
		return ws
	}
	a.indexMu.Unlock()

	ws := agyWorkspaces(root)

	a.indexMu.Lock()
	a.indexRoot = root
	a.indexAt = time.Now()
	a.indexWS = ws
	a.indexMu.Unlock()
	return ws
}

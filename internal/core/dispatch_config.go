package core

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kiperjing/corgi/internal/model"
	"github.com/kiperjing/corgi/internal/runtime/agent"
)

const (
	ModelSourceExplicit        = "explicit"
	ModelSourceMachineOverride = "machine_override"
	ModelSourceKindDefault     = "kind_default"
	ModelSourceCLIDefault      = "cli_default"

	hubMachineKey = "hub"
)

var projectSlugRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// NormalizeMachineKey canonicalizes a machine identifier for map storage/lookup:
// empty or "local" or "hub" -> "hub"; otherwise trimmed machine ID.
func NormalizeMachineKey(machineID string) string {
	m := strings.TrimSpace(machineID)
	if m == "" || strings.EqualFold(m, "local") || strings.EqualFold(m, hubMachineKey) {
		return hubMachineKey
	}
	return m
}

// NormalizeDispatchConfig cleans KindDefaults, MachineKindOverrides, and Projects on Settings
// while keeping legacy WorkerModel/WorkerEffort synchronized with KindDefaults[WorkerKind].
func NormalizeDispatchConfig(st *model.Settings) {
	if st == nil {
		return
	}
	wKind := agent.NormalizeKind(st.WorkerKind)
	if wKind != "" {
		st.WorkerKind = wKind
	}

	cleanedKindDefaults := map[string]model.ModelEffortSpec{}
	for rawK, spec := range st.KindDefaults {
		k := agent.NormalizeKind(rawK)
		if k == "" {
			continue
		}
		m := strings.TrimSpace(spec.Model)
		ef := strings.TrimSpace(spec.Effort)
		if m == "" && ef == "" {
			continue
		}
		cleanedKindDefaults[k] = model.ModelEffortSpec{Model: m, Effort: ef}
	}
	// Backfill legacy WorkerModel/WorkerEffort into KindDefaults[WorkerKind] when not explicitly present,
	// or sync KindDefaults[WorkerKind] back to legacy fields so older views stay consistent.
	if wKind != "" {
		if spec, ok := cleanedKindDefaults[wKind]; ok {
			st.WorkerModel = spec.Model
			st.WorkerEffort = spec.Effort
		} else if strings.TrimSpace(st.WorkerModel) != "" || strings.TrimSpace(st.WorkerEffort) != "" {
			cleanedKindDefaults[wKind] = model.ModelEffortSpec{
				Model:  strings.TrimSpace(st.WorkerModel),
				Effort: strings.TrimSpace(st.WorkerEffort),
			}
		} else {
			st.WorkerModel = ""
			st.WorkerEffort = ""
		}
	}
	if len(cleanedKindDefaults) > 0 {
		st.KindDefaults = cleanedKindDefaults
	} else {
		st.KindDefaults = nil
	}

	cleanedOverrides := map[string]map[string]model.ModelEffortSpec{}
	for rawMachine, byKind := range st.MachineKindOverrides {
		mk := NormalizeMachineKey(rawMachine)
		for rawK, spec := range byKind {
			k := agent.NormalizeKind(rawK)
			if k == "" {
				continue
			}
			m := strings.TrimSpace(spec.Model)
			ef := strings.TrimSpace(spec.Effort)
			if m == "" && ef == "" {
				continue
			}
			if cleanedOverrides[mk] == nil {
				cleanedOverrides[mk] = map[string]model.ModelEffortSpec{}
			}
			cleanedOverrides[mk][k] = model.ModelEffortSpec{Model: m, Effort: ef}
		}
	}
	if len(cleanedOverrides) > 0 {
		st.MachineKindOverrides = cleanedOverrides
	} else {
		st.MachineKindOverrides = nil
	}

	st.Projects = NormalizeProjects(st.Projects)
}

// NormalizeProjects sanitizes project workspace entries, ensuring stable unique IDs and canonical machine keys.
func NormalizeProjects(raw []model.ProjectWorkspace) []model.ProjectWorkspace {
	if len(raw) == 0 {
		return nil
	}
	seenID := map[string]bool{}
	out := make([]model.ProjectWorkspace, 0, len(raw))
	for _, p := range raw {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		id := strings.TrimSpace(p.ID)
		if id == "" {
			id = deriveProjectID(name, len(out)+1)
		}
		baseID := id
		for suffix := 2; seenID[id]; suffix++ {
			id = baseID + "-" + string(rune('0'+suffix%10))
		}
		seenID[id] = true

		var aliases []string
		for _, a := range p.Aliases {
			a = strings.TrimSpace(a)
			if a != "" && !slices.ContainsFunc(aliases, func(x string) bool { return strings.EqualFold(x, a) }) {
				aliases = append(aliases, a)
			}
		}
		paths := map[string]string{}
		for rawM, rawP := range p.Paths {
			cleanP := strings.TrimSpace(rawP)
			if cleanP == "" {
				continue
			}
			paths[NormalizeMachineKey(rawM)] = cleanP
		}
		if len(paths) == 0 {
			paths = nil
		}
		out = append(out, model.ProjectWorkspace{
			ID:      id,
			Name:    name,
			Aliases: aliases,
			Note:    strings.TrimSpace(p.Note),
			Paths:   paths,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func deriveProjectID(name string, idx int) string {
	slug := strings.Trim(projectSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 24 {
		slug = strings.TrimRight(slug[:24], "-")
	}
	if slug != "" {
		return slug
	}
	return "proj-" + string(rune('a'+(idx%26)))
}

// ResolveWorkerModel deterministically resolves a worker's launch (model, effort, source) using 4-tier precedence:
//  1. explicit (reqModel/reqEffort)
//  2. machine_override (Settings.MachineKindOverrides[machine][kind])
//  3. kind_default (Settings.KindDefaults[kind], or legacy Settings.WorkerModel/WorkerEffort for WorkerKind)
//  4. cli_default ("", "") — explicitly falling back to the target CLI's own local config.
func ResolveWorkerModel(st model.Settings, machineID, kind, reqModel, reqEffort string) (modelName, effort, source string) {
	normKind := agent.NormalizeKind(kind)
	reqModel = strings.TrimSpace(reqModel)
	reqEffort = strings.TrimSpace(reqEffort)

	// Compute base default from tiers 2 -> 3 -> 4 first so partial explicit overrides (e.g. effort-only)
	// can cleanly inherit the configured model without dropping to cli_default.
	baseModel, baseEffort, baseSource := "", "", ModelSourceCLIDefault

	mk := NormalizeMachineKey(machineID)
	if byKind, ok := st.MachineKindOverrides[mk]; ok {
		if spec, ok := lookupKindSpec(byKind, normKind); ok && (spec.Model != "" || spec.Effort != "") {
			baseModel, baseEffort, baseSource = spec.Model, spec.Effort, ModelSourceMachineOverride
		}
	}
	if baseSource == ModelSourceCLIDefault && mk == hubMachineKey {
		if byKind, ok := st.MachineKindOverrides[""]; ok {
			if spec, ok := lookupKindSpec(byKind, normKind); ok && (spec.Model != "" || spec.Effort != "") {
				baseModel, baseEffort, baseSource = spec.Model, spec.Effort, ModelSourceMachineOverride
			}
		}
	}
	if baseSource == ModelSourceCLIDefault {
		if spec, ok := lookupKindSpec(st.KindDefaults, normKind); ok && (spec.Model != "" || spec.Effort != "") {
			baseModel, baseEffort, baseSource = spec.Model, spec.Effort, ModelSourceKindDefault
		} else if normKind != "" && normKind == agent.NormalizeKind(st.WorkerKind) &&
			(strings.TrimSpace(st.WorkerModel) != "" || strings.TrimSpace(st.WorkerEffort) != "") {
			baseModel = strings.TrimSpace(st.WorkerModel)
			baseEffort = strings.TrimSpace(st.WorkerEffort)
			baseSource = ModelSourceKindDefault
		}
	}

	if reqModel != "" || reqEffort != "" {
		if reqModel != "" {
			return reqModel, reqEffort, ModelSourceExplicit
		}
		// Effort-only override keeps the resolved tier's model.
		return baseModel, reqEffort, ModelSourceExplicit
	}
	return baseModel, baseEffort, baseSource
}

func lookupKindSpec(m map[string]model.ModelEffortSpec, normKind string) (model.ModelEffortSpec, bool) {
	if len(m) == 0 || normKind == "" {
		return model.ModelEffortSpec{}, false
	}
	if spec, ok := m[normKind]; ok {
		return model.ModelEffortSpec{Model: strings.TrimSpace(spec.Model), Effort: strings.TrimSpace(spec.Effort)}, true
	}
	for k, spec := range m {
		if agent.NormalizeKind(k) == normKind {
			return model.ModelEffortSpec{Model: strings.TrimSpace(spec.Model), Effort: strings.TrimSpace(spec.Effort)}, true
		}
	}
	return model.ModelEffortSpec{}, false
}

// ResolveProject finds a configured ProjectWorkspace by explicit ID/name first, or by matching
// project names and aliases (longest match wins) against task/user hint strings.
func ResolveProject(projects []model.ProjectWorkspace, explicitID string, hints ...string) *model.ProjectWorkspace {
	if len(projects) == 0 {
		return nil
	}
	if q := strings.TrimSpace(explicitID); q != "" {
		for i := range projects {
			if strings.EqualFold(projects[i].ID, q) || strings.EqualFold(projects[i].Name, q) {
				cp := projects[i]
				return &cp
			}
		}
	}
	combined := strings.ToLower(strings.Join(hints, "\n"))
	if strings.TrimSpace(combined) == "" {
		return nil
	}
	bestIdx := -1
	bestLen := 0
	for i := range projects {
		terms := append([]string{projects[i].Name}, projects[i].Aliases...)
		for _, term := range terms {
			t := strings.ToLower(strings.TrimSpace(term))
			rl := utf8.RuneCountInString(t)
			if rl < 2 {
				continue
			}
			if strings.Contains(combined, t) && rl > bestLen {
				bestLen = rl
				bestIdx = i
			}
		}
	}
	if bestIdx >= 0 {
		cp := projects[bestIdx]
		return &cp
	}
	return nil
}

// ProjectPathForMachine returns the configured repository root path for proj on machineID, or "".
func ProjectPathForMachine(proj *model.ProjectWorkspace, machineID string) string {
	if proj == nil || len(proj.Paths) == 0 {
		return ""
	}
	mk := NormalizeMachineKey(machineID)
	if p := strings.TrimSpace(proj.Paths[mk]); p != "" {
		return p
	}
	if mk == hubMachineKey {
		if p := strings.TrimSpace(proj.Paths[""]); p != "" {
			return p
		}
	}
	return ""
}

func buildDispatchDefaultsView(st model.Settings) *model.WorkerDispatchDefaults {
	cp := st
	NormalizeDispatchConfig(&cp)
	return &model.WorkerDispatchDefaults{
		WorkerMachine:        cp.WorkerMachine,
		WorkerKind:           cp.WorkerKind,
		WorkerModel:          cp.WorkerModel,
		WorkerEffort:         cp.WorkerEffort,
		WorkerCwd:            cp.WorkerCwd,
		KindDefaults:         cp.KindDefaults,
		MachineKindOverrides: cp.MachineKindOverrides,
		Projects:             cp.Projects,
	}
}

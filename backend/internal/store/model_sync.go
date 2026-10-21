package store

import "strings"

// ModelSyncResult is what one upstream-registry refresh changed.
//
// The lifecycle has three states, and each is reported separately so the
// console can narrate it:
//
//	Added     — a model the registry lists that the catalogue did not have. It
//	            is shown at the top with a "new" badge for one round.
//	Marked    — a model whose upstream id a probe found no longer served. It
//	            drops to the bottom, struck through, but stays callable-by-name
//	            in the state file for one round.
//	Removed   — a model that was already marked and is still not served, on a
//	            refresh that also adds at least one model. That is the rotation:
//	            a new model arrives, the struck-through ones make way. A refresh
//	            with nothing new removes nothing, no matter how often it runs.
//	            The entry is tombstoned: invisible to the console and the API,
//	            and the tombstone keeps MergeBuiltinModels from resurrecting it
//	            at boot.
//	Recovered — a marked model the probe found serving again; its mark is
//	            cleared and it returns to its ordinary place.
//
// "No longer served" is a probe result, not a registry absence. The registry is
// a picker catalogue: on 2026-10-20 `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`
// and `MiniMax-M3.1-Flash-Preview` were all absent from it yet still answered
// under their own names. An unknown id, by contrast, is silently substituted
// with the account's default model, which is what the probe detects.
type ModelSyncResult struct {
	Added     []*ModelConfig `json:"added"`
	Marked    []*ModelConfig `json:"marked"`
	Removed   []*ModelConfig `json:"removed"`
	Recovered []*ModelConfig `json:"recovered"`
}

// CatalogueIDForUpstream turns an upstream model id into the catalogue id used
// by /v1/models: `MiniMax-M3.1-Flash` -> `minimax-m3.1-flash`.
//
// The rule is a lowercase copy on purpose. It is the convention every built-in
// entry already follows, and it is reversible by eye — an operator reading the
// console can tell which upstream id an entry points at without opening the
// state file.
func CatalogueIDForUpstream(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// displayNameForUpstream strips the vendor prefix the registry repeats on every
// entry, so the catalogue shows `M3.1-Flash` rather than `MiniMax-M3.1-Flash`.
func displayNameForUpstream(id string) string {
	if name := strings.TrimSpace(strings.TrimPrefix(id, "MiniMax-")); name != "" {
		return name
	}
	return id
}

// SyncUpstreamModels applies one refresh.
//
// `upstreamIDs` is the registry's list (picker plus whitelist). `available` and
// `unavailable` are probe verdicts keyed by lowercased upstream id, and only
// cover entries the registry does not list — those are the only ones whose
// status is in question. An entry with no verdict is left exactly as it was,
// which is what a probe that could not run (no account, timeout) should mean.
//
// Only chat entries participate. The video/image entries are not reachable
// through the model endpoint at all.
//
// An upstream id already carried by an entry is never duplicated, and a
// catalogue id already taken is left alone rather than overwritten: the stored
// entry is where the operator's enable/disable choice and usage counters live.
func (s *Store) SyncUpstreamModels(upstreamIDs []string, available, unavailable map[string]bool) ModelSyncResult {
	s.mu.Lock()
	defer s.unlockAndNotify()

	result := ModelSyncResult{
		Added: []*ModelConfig{}, Marked: []*ModelConfig{},
		Removed: []*ModelConfig{}, Recovered: []*ModelConfig{},
	}
	changed := false

	known := make(map[string]string, len(upstreamIDs)) // lowercase -> upstream id
	for _, id := range upstreamIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := known[strings.ToLower(id)]; !ok {
			known[strings.ToLower(id)] = id
		}
	}

	// The upstream's own order, 1-based. The console sorts by it so the model
	// the upstream currently leads with is at the top.
	rankByUpstream := make(map[string]int, len(known))
	next := 0
	for _, id := range upstreamIDs {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := rankByUpstream[key]; ok {
			continue
		}
		next++
		rankByUpstream[key] = next
	}

	// A struck-through model is removed by a *rotation* — a refresh that brings
	// at least one new model — not merely by another refresh happening. So work
	// out what this refresh would add before deciding anything: a quiet refresh
	// leaves the marks alone, however many times it is run.
	represented := make(map[string]bool, len(s.state.Models))
	takenIDs := make(map[string]bool, len(s.state.Models))
	for _, model := range s.state.Models {
		if model == nil {
			continue
		}
		if model.ID != "" {
			takenIDs[model.ID] = true
		}
		if upstream := strings.ToLower(strings.TrimSpace(model.UpstreamModel)); upstream != "" {
			represented[upstream] = true
		}
	}
	rotation := false
	seenNew := make(map[string]bool, len(known))
	for _, id := range upstreamIDs {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" || seenNew[key] {
			continue
		}
		seenNew[key] = true
		if represented[key] || takenIDs[CatalogueIDForUpstream(id)] {
			continue
		}
		rotation = true
		break
	}

	// The "new" badge lasts exactly one refresh. An entry that has never been
	// ranked keeps it through the refresh that ranks it: that is the adopted
	// built-in case, and clearing it in the same call would mean the badge is
	// never seen.
	for _, model := range s.state.Models {
		if model != nil && model.New && model.Rank > 0 {
			model.New = false
			changed = true
		}
	}

	for _, model := range s.state.Models {
		if model == nil || model.Type != ModelTypeChat {
			continue
		}
		upstream := strings.ToLower(strings.TrimSpace(model.UpstreamModel))
		if upstream == "" {
			continue
		}

		if rank, listed := rankByUpstream[upstream]; listed {
			if model.Rank != rank {
				model.Rank = rank
				changed = true
			}
			if model.Removed {
				// Relisted after being tombstoned: bring it back rather than
				// leaving a permanent hole.
				model.Removed = false
				model.Unavailable = false
				model.New = true
				result.Added = append(result.Added, model)
				changed = true
			} else if model.Unavailable {
				model.Unavailable = false
				result.Recovered = append(result.Recovered, model)
				changed = true
			}
			continue
		}
		if model.Rank != 0 {
			model.Rank = 0
			changed = true
		}
		if model.Removed {
			continue
		}

		switch {
		case unavailable[upstream]:
			if model.Unavailable {
				if rotation {
					model.Removed = true
					result.Removed = append(result.Removed, model)
					changed = true
				}
			} else {
				model.Unavailable = true
				result.Marked = append(result.Marked, model)
				changed = true
			}
		case available[upstream]:
			if model.Unavailable {
				model.Unavailable = false
				result.Recovered = append(result.Recovered, model)
				changed = true
			}
		}
	}

	taken := make(map[string]bool, len(s.state.Models))
	for _, model := range s.state.Models {
		if model != nil && model.ID != "" {
			taken[model.ID] = true
		}
	}

	seen := make(map[string]bool, len(known))
	for _, id := range upstreamIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true

		represented := false
		for _, model := range s.state.Models {
			if model != nil && strings.EqualFold(strings.TrimSpace(model.UpstreamModel), id) {
				represented = true
				break
			}
		}
		if represented {
			continue
		}

		catalogueID := CatalogueIDForUpstream(id)
		if catalogueID == "" {
			continue
		}
		if taken[catalogueID] {
			// The id is already in the catalogue. Adopt the upstream spelling
			// when the entry has none — the same repair MergeBuiltinModels does
			// for built-ins, and safe here for the same reason: nothing in the
			// console can set UpstreamModel, so an empty value is a freeze of an
			// older default, never an operator's choice.
			for _, model := range s.state.Models {
				if model != nil && model.Type == ModelTypeChat && model.ID == catalogueID && strings.TrimSpace(model.UpstreamModel) == "" {
					model.UpstreamModel = id
					changed = true
				}
			}
			continue
		}
		model := &ModelConfig{
			ID:            catalogueID,
			Name:          displayNameForUpstream(id),
			Upstream:      "chat",
			UpstreamModel: id,
			Type:          ModelTypeChat,
			Enabled:       true,
			New:           true,
			Rank:          rankByUpstream[strings.ToLower(id)],
			Description:   "从上游模型清单同步",
		}
		s.state.Models = append(s.state.Models, model)
		taken[catalogueID] = true
		result.Added = append(result.Added, model)
		changed = true
	}

	if changed {
		s.markDirtyLocked()
	}
	return result
}

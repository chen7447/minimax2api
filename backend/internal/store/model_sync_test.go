package store

import "testing"

// syncTestStore builds a store over an explicit catalogue. The sync logic only
// touches state.Models, so the persistence machinery Open installs is not
// needed here — markDirtyLocked is a non-blocking send with a default.
func syncTestStore(models ...*ModelConfig) *Store {
	return &Store{state: State{Models: models}}
}

func modelByIDIn(models []*ModelConfig, id string) *ModelConfig {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func hasModelID(models []*ModelConfig, id string) bool {
	return modelByIDIn(models, id) != nil
}

// A registry refresh adds what the catalogue lacks and marks the addition as
// new; the badge lasts exactly one refresh.
func TestSyncUpstreamModelsAddsAndClearsNew(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m3", Upstream: "chat", UpstreamModel: "MiniMax-M3", Type: ModelTypeChat, Enabled: true, Builtin: true},
	)

	first := st.SyncUpstreamModels([]string{"MiniMax-M3", "MiniMax-M3.1-Flash"}, nil, nil)
	if len(first.Added) != 1 {
		t.Fatalf("added = %#v, want one entry", first.Added)
	}
	added := first.Added[0]
	if added.ID != "minimax-m3.1-flash" || added.UpstreamModel != "MiniMax-M3.1-Flash" {
		t.Errorf("added = %+v, want minimax-m3.1-flash -> MiniMax-M3.1-Flash", added)
	}
	if !added.New || !added.Enabled || added.Type != ModelTypeChat {
		t.Errorf("added entry should be a new, enabled chat model: %+v", added)
	}

	second := st.SyncUpstreamModels([]string{"MiniMax-M3", "MiniMax-M3.1-Flash"}, nil, nil)
	if len(second.Added) != 0 {
		t.Errorf("second refresh added %#v, want nothing", second.Added)
	}
	if modelByIDIn(st.state.Models, "minimax-m3.1-flash").New {
		t.Error("the new badge survived a second refresh")
	}
}

// An unlisted entry is only marked when the probe says the upstream substituted
// it. An unlisted entry that still answers under its own name — the preview
// alias on 2026-10-20 — is left alone.
func TestSyncUpstreamModelsMarksOnlyProbeFailures(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m3.1-flash-preview", Upstream: "chat", UpstreamModel: "MiniMax-M3.1-Flash-Preview", Type: ModelTypeChat, Enabled: true, Builtin: true},
		&ModelConfig{ID: "minimax-m2.7", Upstream: "chat", UpstreamModel: "MiniMax-M2.7", Type: ModelTypeChat, Enabled: true, Builtin: true},
	)

	result := st.SyncUpstreamModels(
		[]string{"MiniMax-M3.1-Flash"},
		map[string]bool{"minimax-m3.1-flash-preview": true},
		map[string]bool{"minimax-m2.7": true},
	)

	if len(result.Marked) != 1 || result.Marked[0].ID != "minimax-m2.7" {
		t.Fatalf("marked = %#v, want minimax-m2.7", result.Marked)
	}
	if !modelByIDIn(st.state.Models, "minimax-m2.7").Unavailable {
		t.Error("the marked entry did not carry the unavailable flag")
	}
	if modelByIDIn(st.state.Models, "minimax-m3.1-flash-preview").Unavailable {
		t.Error("an entry whose probe succeeded was marked unavailable")
	}
	if hasModelID(result.Removed, "minimax-m2.7") {
		t.Error("an entry was removed on the first failure; removal is the next refresh's job")
	}
}

// The lifecycle the console narrates: marked when the probe fails, kept
// struck through across quiet refreshes, and removed only when a refresh brings
// a new model — the rotation. A refresh with nothing new removes nothing.
func TestSyncUpstreamModelsRemovesOnlyOnRotation(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m2.7", Upstream: "chat", UpstreamModel: "MiniMax-M2.7", Type: ModelTypeChat, Enabled: true, Builtin: true},
	)
	dead := map[string]bool{"minimax-m2.7": true}

	// Round 1 adds M3 and marks M2.7. The mark is not a removal even though
	// this refresh happens to be a rotation: only previously marked entries
	// make way.
	first := st.SyncUpstreamModels([]string{"MiniMax-M3"}, nil, dead)
	if len(first.Marked) != 1 || len(first.Removed) != 0 {
		t.Fatalf("first refresh marked=%#v removed=%#v, want one mark and no removal", first.Marked, first.Removed)
	}

	// Round 2: nothing new in the registry, so the mark stays — however often
	// the operator presses refresh.
	second := st.SyncUpstreamModels([]string{"MiniMax-M3"}, nil, dead)
	if len(second.Removed) != 0 {
		t.Fatalf("a quiet refresh removed %#v; removal is tied to a rotation", second.Removed)
	}
	if !hasModelID(st.ListModels(), "minimax-m2.7") {
		t.Fatal("a marked entry must stay visible until a new model arrives")
	}

	// Round 3: a new model appears, so the struck-through entry makes way.
	third := st.SyncUpstreamModels([]string{"MiniMax-M3.1-Flash"}, nil, dead)
	if len(third.Removed) != 1 || third.Removed[0].ID != "minimax-m2.7" {
		t.Fatalf("rotation removed = %#v, want minimax-m2.7", third.Removed)
	}
	if hasModelID(st.ListModels(), "minimax-m2.7") {
		t.Error("a removed entry is still listed")
	}
	if _, ok := st.ModelByID("minimax-m2.7"); ok {
		t.Error("a removed entry is still resolvable")
	}

	// The tombstone keeps MergeBuiltinModels from resurrecting the built-in.
	merged, _ := MergeBuiltinModels(st.state.Models)
	count := 0
	for _, model := range merged {
		if model.ID == "minimax-m2.7" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the removed built-in appears %d times after a merge, want the tombstone only", count)
	}
}

// A probe that starts succeeding again clears the mark.
func TestSyncUpstreamModelsRecovers(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m2.7", Upstream: "chat", UpstreamModel: "MiniMax-M2.7", Type: ModelTypeChat, Enabled: true, Builtin: true, Unavailable: true},
	)
	result := st.SyncUpstreamModels([]string{"MiniMax-M3"}, map[string]bool{"minimax-m2.7": true}, nil)
	if len(result.Recovered) != 1 {
		t.Fatalf("recovered = %#v, want one", result.Recovered)
	}
	if modelByIDIn(st.state.Models, "minimax-m2.7").Unavailable {
		t.Error("the unavailable flag survived a successful probe")
	}
}

// An entry tombstoned earlier comes back if the registry lists it again.
func TestSyncUpstreamModelsResurrectsRelisted(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m2.7", Upstream: "chat", UpstreamModel: "MiniMax-M2.7", Type: ModelTypeChat, Enabled: true, Builtin: true, Removed: true},
	)
	result := st.SyncUpstreamModels([]string{"MiniMax-M2.7"}, nil, nil)
	if len(result.Added) != 1 || result.Added[0].ID != "minimax-m2.7" {
		t.Fatalf("added = %#v, want the relisted entry", result.Added)
	}
	got := modelByIDIn(st.state.Models, "minimax-m2.7")
	if got.Removed || got.Unavailable || !got.New {
		t.Errorf("relisted entry = %+v, want live and new", got)
	}
}

// The console order: new at the top, then the upstream's own model order, then
// unranked entries, then unavailable. Removed entries never appear.
func TestListModelsOrderAndVisibility(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "legacy", Type: ModelTypeChat, Enabled: true},
		&ModelConfig{ID: "ranked2", Type: ModelTypeChat, Enabled: true, Rank: 2},
		&ModelConfig{ID: "ranked1", Type: ModelTypeChat, Enabled: true, Rank: 1},
		&ModelConfig{ID: "gone", Type: ModelTypeChat, Enabled: true, Unavailable: true},
		&ModelConfig{ID: "fresh", Type: ModelTypeChat, Enabled: true, New: true, Rank: 3},
		&ModelConfig{ID: "tombstone", Type: ModelTypeChat, Enabled: true, Removed: true},
	)
	got := make([]string, 0, 3)
	for _, model := range st.ListModels() {
		got = append(got, model.ID)
	}
	want := []string{"fresh", "ranked1", "ranked2", "legacy", "gone"}
	if len(got) != len(want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}

// The registry's order becomes the catalogue's rank, so the model the upstream
// currently leads with is at the top without a release.
func TestSyncUpstreamModelsAssignsRegistryRank(t *testing.T) {
	st := syncTestStore(
		&ModelConfig{ID: "minimax-m3", Upstream: "chat", UpstreamModel: "MiniMax-M3", Type: ModelTypeChat, Enabled: true, Builtin: true},
		&ModelConfig{ID: "minimax-m3.1-flash", Upstream: "chat", UpstreamModel: "MiniMax-M3.1-Flash", Type: ModelTypeChat, Enabled: true, Builtin: true},
	)
	st.SyncUpstreamModels([]string{"MiniMax-M3.1-Flash", "MiniMax-M3"}, nil, nil)
	if got := modelByIDIn(st.state.Models, "minimax-m3.1-flash").Rank; got != 1 {
		t.Errorf("m3.1-flash rank = %d, want 1", got)
	}
	if got := modelByIDIn(st.state.Models, "minimax-m3").Rank; got != 2 {
		t.Errorf("m3 rank = %d, want 2", got)
	}

	// Dropping out of the registry clears the rank.
	st.SyncUpstreamModels([]string{"MiniMax-M3"}, nil, nil)
	if got := modelByIDIn(st.state.Models, "minimax-m3.1-flash").Rank; got != 0 {
		t.Errorf("unlisted rank = %d, want 0", got)
	}
}

func TestCatalogueIDForUpstream(t *testing.T) {
	cases := map[string]string{
		"MiniMax-M3.1-Flash": "minimax-m3.1-flash",
		"  MiniMax-M3  ":     "minimax-m3",
		"":                   "",
	}
	for in, want := range cases {
		if got := CatalogueIDForUpstream(in); got != want {
			t.Errorf("CatalogueIDForUpstream(%q) = %q, want %q", in, got, want)
		}
	}
}

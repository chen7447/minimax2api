package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minimax2api/internal/config"
	"minimax2api/internal/minimax"
	"minimax2api/internal/store"
)

type modelSyncBody struct {
	Upstream  []string            `json:"upstream"`
	Added     []store.ModelConfig `json:"added"`
	Marked    []store.ModelConfig `json:"marked"`
	Removed   []store.ModelConfig `json:"removed"`
	Recovered []store.ModelConfig `json:"recovered"`
	Total     int                 `json:"total"`
}

// The whole lifecycle through the handler: a registry addition appears and is
// marked new; when the registry drops it and the probe shows the upstream
// substituting another model, it is marked and shown struck through. Quiet
// refreshes leave it alone — removal happens on the next *rotation*, the
// refresh that brings a new model. An unlisted model the probe still serves
// (the preview alias) is never touched.
func TestRefreshModelsLifecycle(t *testing.T) {
	registry := `{"providers":[{"config":{"models":{"MiniMax-M3":{},"MiniMax-M3.1-Flash":{},"MiniMax-M9":{}},"whitelist":["MiniMax-M2.7","MiniMax-M2.7-highspeed","MiniMax-M3","MiniMax-M3.1-Flash","MiniMax-M9"]}}]}`
	dead := map[string]bool{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case minimax.DefaultModelsPath:
			if got := r.Header.Get("authorization"); got != "" {
				t.Errorf("registry authorization = %q, want none", got)
			}
			_, _ = io.WriteString(w, registry)
		case minimax.DefaultModelPath:
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			echo := body.Model
			if dead[body.Model] {
				// What the upstream really does with an id it no longer knows:
				// runs the default and names that in the response.
				echo = "MiniMax-M3.1-Flash"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": echo, "content": []any{}})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	settings := config.DefaultSettings(t.TempDir())
	settings.Upstream.BaseURL = server.URL
	client := minimax.New(func() config.Settings { return settings })

	st, err := store.Open(t.TempDir(), "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.AddAccount(&store.Account{Name: "probe", Token: "jwt-token", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	api := New(st, nil, client, nil, func() config.Settings { return settings })
	refresh := func() modelSyncBody {
		t.Helper()
		rec := httptest.NewRecorder()
		api.refreshModels(rec, httptest.NewRequest(http.MethodPost, "/admin/api/models/refresh", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body modelSyncBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}

	// Round 1: M9 is listed, everything unlisted still answers.
	first := refresh()
	if len(first.Added) != 1 || first.Added[0].ID != "minimax-m9" || !first.Added[0].New {
		t.Errorf("added = %#v, want minimax-m9 marked new", first.Added)
	}
	if len(first.Marked) != 0 || len(first.Removed) != 0 {
		t.Errorf("first round marked=%#v removed=%#v, want none", first.Marked, first.Removed)
	}

	// Round 2: the registry drops M9 and the probe sees it substituted.
	registry = `{"providers":[{"config":{"models":{"MiniMax-M3":{},"MiniMax-M3.1-Flash":{}},"whitelist":["MiniMax-M2.7","MiniMax-M2.7-highspeed","MiniMax-M3","MiniMax-M3.1-Flash"]}}]}`
	dead["MiniMax-M9"] = true
	second := refresh()
	if len(second.Marked) != 1 || second.Marked[0].ID != "minimax-m9" {
		t.Errorf("second round marked = %#v, want minimax-m9", second.Marked)
	}
	if len(second.Removed) != 0 {
		t.Errorf("second round removed = %#v, want none", second.Removed)
	}
	if _, ok := st.ModelByID("minimax-m9"); !ok {
		t.Error("a marked model must stay visible")
	}
	if _, ok := st.ModelByID("minimax-m3.1-flash-preview"); !ok {
		t.Error("the preview alias was touched; the probe still serves it")
	}

	// Round 3: a quiet refresh with nothing new must not remove anything.
	third := refresh()
	if len(third.Removed) != 0 {
		t.Errorf("a quiet refresh removed %#v; removal is tied to a rotation", third.Removed)
	}
	if _, ok := st.ModelByID("minimax-m9"); !ok {
		t.Error("a marked model was removed by a refresh that added nothing")
	}

	// Round 4: a new model arrives, so the struck-through one makes way.
	registry = `{"providers":[{"config":{"models":{"MiniMax-M3":{},"MiniMax-M3.1-Flash":{},"MiniMax-M10":{}},"whitelist":["MiniMax-M2.7","MiniMax-M2.7-highspeed","MiniMax-M3","MiniMax-M3.1-Flash","MiniMax-M10"]}}]}`
	fourth := refresh()
	if len(fourth.Added) != 1 || fourth.Added[0].ID != "minimax-m10" {
		t.Errorf("rotation added = %#v, want minimax-m10", fourth.Added)
	}
	if len(fourth.Removed) != 1 || fourth.Removed[0].ID != "minimax-m9" {
		t.Errorf("rotation removed = %#v, want minimax-m9", fourth.Removed)
	}
	if _, ok := st.ModelByID("minimax-m9"); ok {
		t.Error("a removed model is still resolvable")
	}
	if strings.Contains(strings.Join(modelIDs(st.ListModels()), ","), "minimax-m9") {
		t.Error("a removed model is still listed")
	}
}

func modelIDs(models []*store.ModelConfig) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

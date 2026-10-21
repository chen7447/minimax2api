package config

import "testing"

// TestNormalizeRepairsTheKnownBrokenLegacyDefaults pins the one thing that makes
// a default fix reach an install that already exists.
//
// Normalize otherwise only fills in *empty* settings, and a fresh install writes
// a copy of every default into the settings file. So changing a default reaches
// new installs and nothing else: an existing install keeps the old value on disk
// and there is no symptom, because the value looks like a normal configuration.
//
// Both of these were shipped as defaults by an earlier build. Neither can work —
// one is answered with the SPA's HTML, the other with a 200 that opens no
// session — so a settings file holding either is broken in a way that no status
// code reveals.
func TestNormalizeRepairsTheKnownBrokenLegacyDefaults(t *testing.T) {
	settings := Settings{}
	settings.Upstream.SessionPath = legacyDefaultSessionPath
	settings.Upstream.AgentID = legacyDefaultAgentID

	settings.Normalize(t.TempDir())

	def := DefaultSettings(t.TempDir())
	if settings.Upstream.SessionPath != def.Upstream.SessionPath {
		t.Errorf("session path = %q, want it repaired to %q",
			settings.Upstream.SessionPath, def.Upstream.SessionPath)
	}
	if settings.Upstream.AgentID != def.Upstream.AgentID {
		t.Errorf("agent id = %q, want it repaired to %q",
			settings.Upstream.AgentID, def.Upstream.AgentID)
	}
}

// TestNormalizeLeavesDeliberateOverridesAlone is the other half of that rule: a
// repair must not become a reset.
//
// An operator pointing the gateway at a self-hosted proxy, or pinning a specific
// agent, has to keep that value. Only the exact legacy strings are rewritten —
// a path that merely resembles one, or an id that happens to contain a role
// name, is left untouched.
func TestNormalizeLeavesDeliberateOverridesAlone(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		agentID   string
		wantSess  string
		wantAgent string
	}{
		{
			name:    "a prefixed path that is not the legacy one",
			session: "/minimax-cloud/api/v1/agent/{agent_id}/session",
		},
		{
			name:    "a custom proxy path",
			session: "/internal/proxy/agent/{agent_id}/session",
		},
		{
			name:    "a path that merely contains the legacy one",
			session: "/x/agent/{agent_id}/session",
		},
		{
			name:    "a real numeric agent id",
			agentID: "443154487857417",
		},
		{
			name:    "an id that contains a role name",
			agentID: "general-purpose-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{}
			settings.Upstream.SessionPath = tc.session
			settings.Upstream.AgentID = tc.agentID

			settings.Normalize(t.TempDir())

			if tc.session != "" && settings.Upstream.SessionPath != tc.session {
				t.Errorf("session path = %q, want the override %q kept",
					settings.Upstream.SessionPath, tc.session)
			}
			if tc.agentID != "" && settings.Upstream.AgentID != tc.agentID {
				t.Errorf("agent id = %q, want the override %q kept",
					settings.Upstream.AgentID, tc.agentID)
			}
		})
	}
}

// The upstream transport is a switch, and the two ways of getting it wrong are
// both silent: reading "direct" as the agent transport quietly removes tool
// calling, and reading a blank value as "direct" moves an install that was
// serving chat onto a protocol it was never tested against.
func TestUpstreamModeNormalisation(t *testing.T) {
	cases := []struct {
		name  string
		store string
		want  string
	}{
		{"blank is the transport this install already used", "", ModeAgent},
		{"the agent transport is kept", ModeAgent, ModeAgent},
		{"the model transport is kept", ModeDirect, ModeDirect},
		{"case and padding do not decide the transport", "  DiReCt ", ModeDirect},
		{"an unrecognised value falls back to the agent transport", "nonsense", ModeAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{}
			settings.Upstream.Mode = tc.store
			settings.Normalize(t.TempDir())
			if settings.Upstream.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", settings.Upstream.Mode, tc.want)
			}
		})
	}
}

// UsesDirectModelAPI is read on the request path, where a settings snapshot may
// not have been through Normalize yet — so it has to decide for itself rather
// than trusting the stored value to be canonical.
func TestUsesDirectModelAPIToleratesUnnormalisedValues(t *testing.T) {
	cases := map[string]bool{
		"":           false,
		ModeAgent:    false,
		ModeDirect:   true,
		"DIRECT":     true,
		"  direct  ": true,
		"direct-ish": false,
	}
	for value, want := range cases {
		settings := Settings{}
		settings.Upstream.Mode = value
		if got := settings.UsesDirectModelAPI(); got != want {
			t.Errorf("UsesDirectModelAPI(%q) = %v, want %v", value, got, want)
		}
	}
}

// The direct transport's paths have to survive a blank settings file, or an
// install that never opened the console would post every turn to an empty URL.
func TestDirectPathsHaveDefaults(t *testing.T) {
	settings := Settings{}
	settings.Normalize(t.TempDir())
	if settings.Upstream.ModelPath != DefaultSettings("").Upstream.ModelPath {
		t.Fatalf("model path = %q", settings.Upstream.ModelPath)
	}
	if settings.Upstream.ModelsPath != DefaultSettings("").Upstream.ModelsPath {
		t.Fatalf("models path = %q", settings.Upstream.ModelsPath)
	}
}

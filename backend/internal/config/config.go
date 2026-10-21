package config

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds process-level bootstrap options. Runtime-tunable gateway
// parameters live in the persistent store instead.
type Config struct {
	Addr      string
	DataDir   string
	StaticDir string
	// AdminUser is a display label only. The console and the admin API are both
	// gated by AdminKey alone — there is no username to type or to remember.
	AdminUser string
	AdminKey  string
}

func Load() Config {
	var cfg Config
	flag.StringVar(&cfg.Addr, "addr", env("MINIMAX2API_ADDR", "127.0.0.1:8080"), "HTTP listen address")
	flag.StringVar(&cfg.DataDir, "data", env("MINIMAX2API_DATA", "data"), "persistent data directory")
	flag.StringVar(&cfg.StaticDir, "static", env("MINIMAX2API_STATIC", "frontend/dist"), "built frontend directory")
	flag.StringVar(&cfg.AdminUser, "admin-user", env("MINIMAX2API_ADMIN_USER", "admin"), "admin label (display only)")
	flag.StringVar(&cfg.AdminKey, "admin-key", env("MINIMAX2API_ADMIN_KEY", ""), "initial admin key (random when empty)")
	flag.Parse()

	if cfg.AdminKey == "" {
		cfg.AdminKey = env("MINIMAX2API_ADMIN_KEY", "")
	}
	return cfg
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Settings mirrors the runtime-tunable section exposed by the admin console.
type Settings struct {
	Server   ServerSettings   `json:"server"`
	Upstream UpstreamSettings `json:"upstream"`
	Routing  RoutingSettings  `json:"routing"`
	Audit    AuditSettings    `json:"audit"`
	Media    MediaSettings    `json:"media"`
	Signin   SigninSettings   `json:"signin"`
	Video    VideoSettings    `json:"video"`
}

// VideoSettings drives the video-generation route.
//
// Video is not a chat mode. MiniMax-H3 has no model id in the conversation
// API: the agent reaches it through the `video-creater` plugin, and the plugin
// is selected by *mentioning* it in the message text — the composer's reference
// chip serialises to `@video-creater` and that string is the whole mechanism.
// The generation parameters then travel in a trailing
// `<video-generation-options>` block appended to the same text, not in a
// request field.
//
// Both of those belong to the upstream bundle rather than to this gateway, so
// both are settings: a renamed plugin or a renamed tag should be a console edit,
// not a rebuild.
type VideoSettings struct {
	// PluginName is the plugin reference the agent routes on. It is written
	// into the message text as `@<pluginName>`.
	PluginName string `json:"pluginName"`
	// OptionsTag is the tag the generation parameters travel in.
	OptionsTag string `json:"optionsTag"`
	// Default* fill in whatever the caller left out.
	//
	// They matter more than an ordinary default would. The plugin's own skill
	// asks the user to confirm every unspecified choice before generating, and
	// a headless API call has nobody to answer — so an omitted parameter does
	// not quietly fall back to an upstream default, it stalls the turn. Always
	// sending a complete set is what keeps a request unattended.
	DefaultRatio      string `json:"defaultRatio"`
	DefaultResolution string `json:"defaultResolution"`
	DefaultDuration   int    `json:"defaultDuration"`
	// TimeoutSec bounds one video turn, separately from the chat timeout: the
	// fast variant alone spends around twenty seconds generating, and the slow
	// one is documented at 15–30 minutes.
	TimeoutSec int `json:"timeoutSec"`
}

// SigninSettings drives the daily check-in sweep and the credit guard.
//
// Only the international deployment is implemented. MiniMax runs the check-in
// under a different parameter set on the mainland host (it keys off
// timezone_id rather than timezone_offset), and signing a mainland account with
// the international protocol just earns a rejection, so those accounts are
// skipped rather than guessed at.
type SigninSettings struct {
	Enabled bool `json:"enabled"`
	// Hour and Minute are the pre-randomization fixed slot, kept only as the
	// fallback for a store whose SlotSeed has not been issued yet. The console
	// no longer edits them.
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
	// SlotSeed personalizes the randomized daily check-in slot. Issued once per
	// install (the admin salt at first boot, an explicit random value when
	// missing), it feeds SigninRandomSlot together with the Beijing date. It is
	// not a secret in play — it only needs to be stable per deployment so
	// restarts do not move today's slot.
	SlotSeed string `json:"slotSeed"`
	// GapSeconds spaces out the per-account requests. Check-in is a
	// risk-control sensitive endpoint; a burst of parallel calls from one IP is
	// exactly the pattern that gets accounts flagged.
	GapSeconds int `json:"gapSeconds"`
	// TimeoutSec bounds a single check-in call.
	TimeoutSec int `json:"timeoutSec"`
	// SkipZeroCredit removes accounts whose last known balance is zero from
	// scheduling. See CreditFreshMin for how stale a reading may be.
	SkipZeroCredit bool `json:"skipZeroCredit"`
	// CreditFreshMin is how long a balance reading stays actionable. Past it
	// the account is scheduled again: refusing to use an account because of a
	// day-old zero would strand capacity that a single request would refill.
	CreditFreshMin int `json:"creditFreshMin"`
	// CreditRefreshMin is how often balances are polled in the background. 0
	// disables the poller, leaving manual refresh only.
	CreditRefreshMin int `json:"creditRefreshMin"`

	// Browser fingerprint reported to the check-in endpoints. The captured
	// values are reproduced by default; they only have to stay self-consistent,
	// since the signature covers whatever is actually sent.
	Lang              string `json:"lang"`
	OSName            string `json:"osName"`
	BrowserName       string `json:"browserName"`
	BrowserLanguage   string `json:"browserLanguage"`
	BrowserPlatform   string `json:"browserPlatform"`
	DeviceMemory      int    `json:"deviceMemory"`
	CPUCoreNum        int    `json:"cpuCoreNum"`
	TimezoneOffsetMin int    `json:"timezoneOffsetMin"`

	// Paths are settings rather than constants for the same reason the agent
	// paths are: a rename upstream should not need a rebuild.
	StatusPath string `json:"statusPath"`
	ClaimPath  string `json:"claimPath"`
	CreditPath string `json:"creditPath"`
	// CreditDetailsPath lists each credit grant separately. It is what makes a
	// check-in auditable: the claim endpoint reports success whether or not the
	// points were issued, so the grant is the only evidence they arrived.
	CreditDetailsPath string `json:"creditDetailsPath"`
}

type ServerSettings struct {
	Addr                  string `json:"addr"`
	MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
	AdminUsername         string `json:"adminUsername"`
}

// UpstreamSettings describes how to reach MiniMax Agent.
//
// Two hosts are kept side by side because MiniMax runs separate mainland and
// international deployments with separate account databases: an account
// registered with a +86 phone number authenticates only against the mainland
// host, and one registered with an email or an overseas number only against
// the international host. Routing each account to the right host is what makes
// both kinds of accounts work in a single pool.
type UpstreamSettings struct {
	BaseURL     string `json:"baseURL"`
	BaseURLCN   string `json:"baseURLCN"`
	AgentID     string `json:"agentID"`
	SessionPath string `json:"sessionPath"`
	MessagePath string `json:"messagePath"`
	// Mode selects which upstream transport chat turns use.
	//
	//   - `agent`  — the agent conversation endpoint (session + message). This is
	//     what the gateway has always used, and the only transport that can reach
	//     the video and image plugin routes.
	//   - `direct` — the model endpoint the official client uses for model turns
	//     (`/mavis/api/v1/llm/v1/messages`). Anthropic Messages, native tool use,
	//     no session, no echo.
	//
	// The two are not interchangeable, and the difference is not one of quality:
	// the agent endpoint answers as MiniMax's own cloud agent — its own persona,
	// its own sandbox, its own memory — and has no field that could carry a tool
	// declaration. A client that wants to drive a tool loop (Claude Code, ZCode
	// and the like) needs `direct`. See internal/minimax/direct.go.
	Mode string `json:"mode"`
	// ModelPath is where one turn is posted on the direct transport.
	ModelPath string `json:"modelPath"`
	// ModelsPath lists the model ids the direct transport accepts.
	ModelsPath string `json:"modelsPath"`
	// UserInfoPath is where an account's realUserID can be read. Every signed
	// endpoint demands that value in its query and answers a bare 401 without
	// it, and it appears nowhere in the token — so this call is the only way an
	// account added as a bare JWT can ever become usable.
	UserInfoPath string `json:"userInfoPath"`
	// AgentListPath is where an account's agents are listed. It is what turns
	// the `general` *role* into the numeric agent *id* every URL needs; without
	// it a session cannot be opened at all.
	AgentListPath string `json:"agentListPath"`
	// ConfigPath is the agent-side initialisation call. A freshly registered
	// account refuses to answer messages until it has run once, and says so
	// with a message about environment variables that mentions none of this.
	//
	// It also has to run before a check-in, not after: a claim made before the
	// account's agent-side record exists is registered and never paid out, and
	// running the sequence afterwards does not recover it.
	ConfigPath string `json:"configPath"`
	// ConnectionsPath completes the opening sequence the web client runs when
	// it opens the agent page.
	ConnectionsPath string `json:"connectionsPath"`
	// SummariesPath lists a session's turns together with the files each one
	// produced. Generated media is not announced in the conversation stream, so
	// this is the only place a finished video is visible from.
	SummariesPath string `json:"summariesPath"`
	// DriveFilePath addresses one drive node. Its `download-url` sub-resource is
	// what turns a node into a playable link.
	DriveFilePath string `json:"driveFilePath"`
	// UploadsPath mints a private upload policy for an attachment.
	//
	// The message endpoint only accepts an image the account owns, so an image
	// has to be uploaded before it can be attached, and this is where that
	// starts. It is a setting for the same reason as the other paths: it belongs
	// to the upstream bundle, and a rename should be an edit rather than a
	// rebuild.
	UploadsPath          string `json:"uploadsPath"`
	ModelPayload         string `json:"modelPayload"`
	Language             string `json:"language"`
	ScreenWidth          int    `json:"screenWidth"`
	ScreenHeight         int    `json:"screenHeight"`
	RequestTimeoutSec    int    `json:"requestTimeoutSec"`
	StreamIdleTimeoutSec int    `json:"streamIdleTimeoutSec"`
	Proxy                string `json:"proxy"`
	// StreamBaseURL overrides the host the conversation endpoint answers on.
	//
	// It is separate from BaseURL because the captured web client posts
	// messages to `agent-stream.<domain>` while every other call stays on the
	// API host — and the two hosts are not interchangeable: the same path on
	// the API host reaches a different entry point, which is exactly what the
	// gateway did before this field existed.
	//
	// Empty means "derive it from BaseURL", which is the better default: it
	// follows an install or an account that has been pointed somewhere else
	// (a self-hosted relay, a test server) instead of overriding it with a
	// hostname those deployments do not have.
	StreamBaseURL string `json:"streamBaseURL"`
	UserAgent     string `json:"userAgent"`
}

type RoutingSettings struct {
	Strategy        string `json:"strategy"`
	CooldownBaseSec int    `json:"cooldownBaseSec"`
	CooldownMaxSec  int    `json:"cooldownMaxSec"`
	MaxAttempts     int    `json:"maxAttempts"`
	CapacityWaitSec int    `json:"capacityWaitSec"`
	StickyTTLSec    int    `json:"stickyTTLSec"`
	PreferIdle      bool   `json:"preferIdle"`
	// ActivePoolSize is the number of accounts kept in the working set the
	// scheduler draws from ("使用池"). The working set prefers the session's
	// current account, evicts an account the moment upstream reports its
	// balance spent, and backfills from the reserve with the highest known
	// credit (ties broken randomly). Keeping it small concentrates traffic on a
	// few accounts so the rest of the pool keeps both its balances and its
	// risk-control footprint untouched. 0 schedules across the whole pool.
	ActivePoolSize int `json:"activePoolSize"`
	// ExhaustedRetryHours is how long an exhausted account waits before the
	// background sweep re-runs its check-in on a per-account clock. Any credit
	// reading above zero revives the account immediately; this is the fallback
	// for accounts whose daily check-in failed. 0 disables the retry.
	ExhaustedRetryHours int `json:"exhaustedRetryHours"`
}

type AuditSettings struct {
	RetentionDays  int  `json:"retentionDays"`
	MaxRecords     int  `json:"maxRecords"`
	RecordBody     bool `json:"recordBody"`
	BodyLimitBytes int  `json:"bodyLimitBytes"`
}

type MediaSettings struct {
	GeneratedDir   string `json:"generatedDir"`
	PublicBaseURL  string `json:"publicBaseURL"`
	MaxTotalSizeMB int    `json:"maxTotalSizeMB"`
	AutoDownload   bool   `json:"autoDownload"`
}

// Settings an earlier build shipped as its own defaults and that turned out to
// be wrong. They are listed so Normalize can recognise and repair them.
//
// Repairing is necessary because Normalize otherwise only fills in *empty*
// settings, and a fresh install freezes a copy of every default into the
// settings file. A default that is later found to be wrong therefore stays wrong
// on every existing install, with no symptom — which is how both of these
// survived a fix to the defaults themselves.
//
// Neither value can work, so overwriting one is never a loss of intent:
//
//   - the session path resolves to the site's SPA fallback, which answers 200
//     with a full page of HTML rather than a session;
//   - `general` is an agent *role*, which the upstream answers with a 200 that
//     opens no session at all.
const (
	legacyDefaultSessionPath = "/agent/{agent_id}/session"
	legacyDefaultAgentID     = "general"
	// legacyDefaultMessagePath is a different kind of entry: it was not broken,
	// it was merely the wrong *door*. It answers, and the agent replies, but the
	// entry point it names hands the agent no rendering tool — so every video
	// request came back as a friendly description of a video that was never
	// made. The captured web client uses the `/minimax-cloud` path below, on the
	// streaming host; the two go together, which is why both are migrated.
	legacyDefaultMessagePath = "/archon/api/v1/session/{session_id}/message"
)

// Upstream transport modes. See UpstreamSettings.Mode for what each one means.
const (
	// ModeAgent is the agent conversation endpoint.
	ModeAgent = "agent"
	// ModeDirect is the model endpoint the official client uses for model turns.
	ModeDirect = "direct"
)

// DefaultSettings returns the built-in runtime configuration.
func DefaultSettings(dataDir string) Settings {
	return Settings{
		Server: ServerSettings{
			Addr:                  "127.0.0.1:8080",
			MaxConcurrentRequests: 64,
			AdminUsername:         "admin",
		},
		Upstream: UpstreamSettings{
			BaseURL:   "https://agent.minimax.io",
			BaseURLCN: "https://agent.minimaxi.com",
			// Empty on purpose: `general` is an agent *role*, not an id, and the
			// upstream accepts it with a 200 that opens no session. The real id
			// is a per-account number, discovered when the account is prepared.
			AgentID: "",
			// Must agree with minimax.DefaultSessionPath — this package cannot
			// import it (the dependency runs the other way), so the agreement is
			// pinned by a test in the minimax package instead.
			SessionPath: "/minimax-cloud/api/v1/agent/{agent_id}/session",
			MessagePath: "/minimax-cloud/api/v1/session/{session_id}/message",
			// The agent transport stays the default. Switching it is a deliberate
			// act — the direct transport is the better one for tool-calling
			// clients, but it is a different protocol and an install that is
			// happily serving chat should not be moved onto it by an upgrade.
			Mode:            ModeAgent,
			ModelPath:       "/mavis/api/v1/llm/v1/messages",
			ModelsPath:      "/mavis/api/v1/models",
			UserInfoPath:    "/v1/api/user/info",
			AgentListPath:   "/minimax-cloud/api/v1/agent",
			ConfigPath:      "/minimax-cloud/api/v1/config",
			ConnectionsPath: "/minimax-cloud/api/v1/channel/connections",
			SummariesPath:   "/minimax-cloud/api/v1/session/{session_id}/input-summaries",
			DriveFilePath:   "/minimax-cloud/api/v1/drive/file/{node_id}",
			// Must agree with minimax.DefaultUploadsPath — this package cannot
			// import it (the dependency runs the other way), so the agreement is
			// pinned by a test in the minimax package instead.
			UploadsPath:          "/minimax-cloud/api/v1/uploads/prepare",
			ModelPayload:         "",
			Language:             "zh-CN,zh;q=0.9,en;q=0.8",
			ScreenWidth:          1920,
			ScreenHeight:         1080,
			RequestTimeoutSec:    300,
			StreamIdleTimeoutSec: 120,
			Proxy:                "",
			UserAgent:            defaultUserAgent,
		},
		Routing: RoutingSettings{
			Strategy:            "least_inflight",
			CooldownBaseSec:     60,
			CooldownMaxSec:      900,
			MaxAttempts:         3,
			CapacityWaitSec:     20,
			StickyTTLSec:        300,
			PreferIdle:          true,
			ActivePoolSize:      3,
			ExhaustedRetryHours: 24,
		},
		Audit: AuditSettings{
			RetentionDays:  7,
			MaxRecords:     5000,
			RecordBody:     true,
			BodyLimitBytes: 8192,
		},
		Media: MediaSettings{
			GeneratedDir:   filepath.Join(dataDir, "generated"),
			PublicBaseURL:  "",
			MaxTotalSizeMB: 2048,
			AutoDownload:   true,
		},
		Signin: SigninSettings{
			// Off by default: it spends a request against every account in the
			// pool and is only useful once the operator has accounts they are
			// willing to automate.
			Enabled:           false,
			Hour:              9,
			Minute:            5,
			GapSeconds:        2,
			TimeoutSec:        30,
			SkipZeroCredit:    false,
			CreditFreshMin:    360,
			CreditRefreshMin:  30,
			Lang:              "en",
			OSName:            "Windows",
			BrowserName:       "Chrome",
			BrowserLanguage:   "en-US",
			BrowserPlatform:   "Win32",
			DeviceMemory:      16,
			CPUCoreNum:        8,
			TimezoneOffsetMin: 480,
			StatusPath:        "/minimax-cloud/api/v1/signin/status",
			ClaimPath:         "/minimax-cloud/api/v1/signin/claim",
			CreditPath:        "/matrix/api/v1/commerce/get_membership_info",
			CreditDetailsPath: "/minimax-cloud/api/v1/credit/details",
		},
		Video: VideoSettings{
			PluginName:        "video-creater",
			OptionsTag:        "video-generation-options",
			DefaultRatio:      "16:9",
			DefaultResolution: "768P",
			DefaultDuration:   5,
			// Comfortably above the fast variant's ~20s and long enough that a
			// slow H3 turn returns whatever progress the agent has reached
			// rather than a bare timeout. It cannot cover a full H3 render:
			// 15–30 minutes is beyond any synchronous HTTP surface, which is
			// why the slow model is documented as submit-and-follow-up.
			TimeoutSec: 600,
		},
	}
}

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// Normalize repairs out-of-range values coming from a stored config file.
// Normalize fills in absent settings and repairs the known-broken legacy
// defaults, and reports whether it changed anything.
//
// The report matters because a repair that is not written back leaves the
// settings file describing a configuration the process is not using — and a file
// that disagrees with the running process is how a broken value survives a fix
// in the first place.
func (s *Settings) Normalize(dataDir string) bool {
	before := *s
	def := DefaultSettings(dataDir)
	if s.Server.MaxConcurrentRequests <= 0 {
		s.Server.MaxConcurrentRequests = def.Server.MaxConcurrentRequests
	}
	if s.Server.AdminUsername == "" {
		s.Server.AdminUsername = def.Server.AdminUsername
	}
	if s.Upstream.BaseURL == "" {
		s.Upstream.BaseURL = def.Upstream.BaseURL
	}
	if s.Upstream.BaseURLCN == "" {
		s.Upstream.BaseURLCN = def.Upstream.BaseURLCN
	}
	if s.Upstream.AgentID == "" {
		s.Upstream.AgentID = def.Upstream.AgentID
	}
	if s.Upstream.SessionPath == "" {
		s.Upstream.SessionPath = def.Upstream.SessionPath
	}
	// Repair the two known-broken legacy defaults. Kept next to the empty-value
	// repairs above rather than in a version-keyed migration table, because there
	// are two of them and a reader should be able to see at a glance what is
	// being changed and why. See the constants for what each one costs.
	if s.Upstream.SessionPath == legacyDefaultSessionPath {
		s.Upstream.SessionPath = def.Upstream.SessionPath
	}
	if s.Upstream.AgentID == legacyDefaultAgentID {
		s.Upstream.AgentID = def.Upstream.AgentID
	}
	if s.Upstream.MessagePath == "" {
		s.Upstream.MessagePath = def.Upstream.MessagePath
	}
	// The message path and the streaming host go together: the path was captured
	// on that host, and pointing one at the other is a combination that was never
	// observed. Migrating both keeps an upgraded instance on the pair the web
	// client actually uses.
	if s.Upstream.MessagePath == legacyDefaultMessagePath {
		s.Upstream.MessagePath = def.Upstream.MessagePath
	}
	// Mode is repaired rather than honoured when unrecognised. An empty value is
	// what a settings file written before this field existed carries, and it is
	// also what a console save that does not know the field would send — so the
	// safe reading of "no value" is "the transport this install already used",
	// not "guess". See UpstreamSettings.Mode.
	switch strings.ToLower(strings.TrimSpace(s.Upstream.Mode)) {
	case ModeDirect:
		s.Upstream.Mode = ModeDirect
	default:
		s.Upstream.Mode = ModeAgent
	}
	if s.Upstream.ModelPath == "" {
		s.Upstream.ModelPath = def.Upstream.ModelPath
	}
	if s.Upstream.ModelsPath == "" {
		s.Upstream.ModelsPath = def.Upstream.ModelsPath
	}
	if s.Upstream.UserInfoPath == "" {
		s.Upstream.UserInfoPath = def.Upstream.UserInfoPath
	}
	if s.Upstream.AgentListPath == "" {
		s.Upstream.AgentListPath = def.Upstream.AgentListPath
	}
	if s.Upstream.ConfigPath == "" {
		s.Upstream.ConfigPath = def.Upstream.ConfigPath
	}
	if s.Upstream.ConnectionsPath == "" {
		s.Upstream.ConnectionsPath = def.Upstream.ConnectionsPath
	}
	if s.Upstream.SummariesPath == "" {
		s.Upstream.SummariesPath = def.Upstream.SummariesPath
	}
	if s.Upstream.DriveFilePath == "" {
		s.Upstream.DriveFilePath = def.Upstream.DriveFilePath
	}
	if s.Upstream.UploadsPath == "" {
		s.Upstream.UploadsPath = def.Upstream.UploadsPath
	}
	if s.Upstream.ScreenWidth <= 0 {
		s.Upstream.ScreenWidth = def.Upstream.ScreenWidth
	}
	if s.Upstream.ScreenHeight <= 0 {
		s.Upstream.ScreenHeight = def.Upstream.ScreenHeight
	}
	if s.Upstream.Language == "" {
		s.Upstream.Language = def.Upstream.Language
	}
	if s.Upstream.RequestTimeoutSec < 5 {
		s.Upstream.RequestTimeoutSec = def.Upstream.RequestTimeoutSec
	}
	if s.Upstream.StreamIdleTimeoutSec < 5 {
		s.Upstream.StreamIdleTimeoutSec = def.Upstream.StreamIdleTimeoutSec
	}
	if s.Upstream.UserAgent == "" {
		s.Upstream.UserAgent = def.Upstream.UserAgent
	}
	switch s.Routing.Strategy {
	case "least_inflight", "round_robin", "priority", "random":
	default:
		s.Routing.Strategy = def.Routing.Strategy
	}
	if s.Routing.CooldownBaseSec <= 0 {
		s.Routing.CooldownBaseSec = def.Routing.CooldownBaseSec
	}
	if s.Routing.CooldownMaxSec < s.Routing.CooldownBaseSec {
		s.Routing.CooldownMaxSec = def.Routing.CooldownMaxSec
	}
	if s.Routing.MaxAttempts < 1 || s.Routing.MaxAttempts > 20 {
		s.Routing.MaxAttempts = def.Routing.MaxAttempts
	}
	if s.Routing.CapacityWaitSec < 0 {
		s.Routing.CapacityWaitSec = def.Routing.CapacityWaitSec
	}
	if s.Routing.StickyTTLSec < 0 {
		s.Routing.StickyTTLSec = def.Routing.StickyTTLSec
	}
	if s.Routing.ActivePoolSize < 0 || s.Routing.ActivePoolSize > 64 {
		s.Routing.ActivePoolSize = def.Routing.ActivePoolSize
	}
	if s.Routing.ExhaustedRetryHours < 0 || s.Routing.ExhaustedRetryHours > 24*30 {
		s.Routing.ExhaustedRetryHours = def.Routing.ExhaustedRetryHours
	}
	if s.Audit.MaxRecords < 100 {
		s.Audit.MaxRecords = def.Audit.MaxRecords
	}
	if s.Audit.BodyLimitBytes < 256 {
		s.Audit.BodyLimitBytes = def.Audit.BodyLimitBytes
	}
	if s.Media.GeneratedDir == "" {
		s.Media.GeneratedDir = def.Media.GeneratedDir
	}
	if s.Media.MaxTotalSizeMB < 64 {
		s.Media.MaxTotalSizeMB = def.Media.MaxTotalSizeMB
	}

	if s.Signin.Hour < 0 || s.Signin.Hour > 23 {
		s.Signin.Hour = def.Signin.Hour
	}
	if s.Signin.Minute < 0 || s.Signin.Minute > 59 {
		s.Signin.Minute = def.Signin.Minute
	}
	if s.Signin.GapSeconds < 0 {
		s.Signin.GapSeconds = def.Signin.GapSeconds
	}
	if s.Signin.TimeoutSec < 5 {
		s.Signin.TimeoutSec = def.Signin.TimeoutSec
	}
	if s.Signin.CreditFreshMin <= 0 {
		// Zero is repaired rather than honoured: an unbounded freshness window
		// would let one stale zero hold an account out of rotation forever,
		// which is the failure this guard exists to avoid, not to create.
		s.Signin.CreditFreshMin = def.Signin.CreditFreshMin
	}
	if s.Signin.CreditRefreshMin < 0 {
		s.Signin.CreditRefreshMin = def.Signin.CreditRefreshMin
	}
	if s.Signin.DeviceMemory <= 0 {
		s.Signin.DeviceMemory = def.Signin.DeviceMemory
	}
	if s.Signin.CPUCoreNum <= 0 {
		s.Signin.CPUCoreNum = def.Signin.CPUCoreNum
	}
	if s.Signin.Lang == "" {
		s.Signin.Lang = def.Signin.Lang
	}
	if s.Signin.OSName == "" {
		s.Signin.OSName = def.Signin.OSName
	}
	if s.Signin.BrowserName == "" {
		s.Signin.BrowserName = def.Signin.BrowserName
	}
	if s.Signin.BrowserLanguage == "" {
		s.Signin.BrowserLanguage = def.Signin.BrowserLanguage
	}
	if s.Signin.BrowserPlatform == "" {
		s.Signin.BrowserPlatform = def.Signin.BrowserPlatform
	}
	if s.Signin.StatusPath == "" {
		s.Signin.StatusPath = def.Signin.StatusPath
	}
	if s.Signin.ClaimPath == "" {
		s.Signin.ClaimPath = def.Signin.ClaimPath
	}
	if s.Signin.CreditPath == "" {
		s.Signin.CreditPath = def.Signin.CreditPath
	}
	if s.Signin.CreditDetailsPath == "" {
		s.Signin.CreditDetailsPath = def.Signin.CreditDetailsPath
	}
	if s.Video.PluginName == "" {
		s.Video.PluginName = def.Video.PluginName
	}
	if s.Video.OptionsTag == "" {
		s.Video.OptionsTag = def.Video.OptionsTag
	}
	if s.Video.DefaultRatio == "" {
		s.Video.DefaultRatio = def.Video.DefaultRatio
	}
	if s.Video.DefaultResolution == "" {
		s.Video.DefaultResolution = def.Video.DefaultResolution
	}
	if s.Video.DefaultDuration <= 0 {
		s.Video.DefaultDuration = def.Video.DefaultDuration
	}
	if s.Video.TimeoutSec <= 0 {
		s.Video.TimeoutSec = def.Video.TimeoutSec
	}
	// TimezoneOffsetMin is deliberately not repaired here. A stored 0 cannot be
	// told apart from an absent field, and signinParams already maps 0 onto the
	// +8 default the capture used, so repairing it here would only add a second
	// place to get the same answer. The cost is that exactly-UTC is not
	// expressible; every other offset is, and the upstream appears to use this
	// value for reporting rather than for deciding when the day rolls over.
	return *s != before
}

func (s Settings) Clone() Settings {
	raw, err := json.Marshal(s)
	if err != nil {
		return s
	}
	var clone Settings
	if err := json.Unmarshal(raw, &clone); err != nil {
		return s
	}
	return clone
}

func (s Settings) RequestTimeout() time.Duration {
	return time.Duration(s.Upstream.RequestTimeoutSec) * time.Second
}

func (s Settings) StreamIdleTimeout() time.Duration {
	return time.Duration(s.Upstream.StreamIdleTimeoutSec) * time.Second
}

// UsesDirectModelAPI reports whether chat turns go to the model endpoint rather
// than the agent conversation endpoint.
//
// It is deliberately tolerant of the stored value rather than trusting
// Normalize to have run: a settings snapshot can be handed around before it is
// repaired, and reading "Direct" or " direct " as the agent transport would be
// a silent downgrade of exactly the thing this setting exists to select.
func (s Settings) UsesDirectModelAPI() bool {
	return strings.EqualFold(strings.TrimSpace(s.Upstream.Mode), ModeDirect)
}

// VideoTimeout bounds one video-generation turn.
//
// It is a separate budget from RequestTimeout because the two have nothing in
// common: a chat turn that takes a minute is broken, and a video turn that
// takes a minute has not started yet.
func (s Settings) VideoTimeout() time.Duration {
	return time.Duration(s.Video.TimeoutSec) * time.Second
}

func (s Settings) CooldownBase() time.Duration {
	return time.Duration(s.Routing.CooldownBaseSec) * time.Second
}

func (s Settings) CooldownMax() time.Duration {
	return time.Duration(s.Routing.CooldownMaxSec) * time.Second
}

func (s Settings) CapacityWait() time.Duration {
	return time.Duration(s.Routing.CapacityWaitSec) * time.Second
}

func (s Settings) StickyTTL() time.Duration {
	return time.Duration(s.Routing.StickyTTLSec) * time.Second
}

// ActivePoolSize is the working-set size the scheduler draws from; 0 keeps the
// legacy whole-pool scheduling.
func (s Settings) ActivePoolSize() int {
	return s.Routing.ActivePoolSize
}

// ExhaustedRetry is how long an exhausted account waits before its check-in is
// retried in the background; 0 disables the retry.
func (s Settings) ExhaustedRetry() time.Duration {
	return time.Duration(s.Routing.ExhaustedRetryHours) * time.Hour
}

func (s Settings) Retention() time.Duration {
	return time.Duration(s.Audit.RetentionDays) * 24 * time.Hour
}

func (s Settings) MediaLimitBytes() int64 {
	return int64(s.Media.MaxTotalSizeMB) * 1024 * 1024
}

// SigninClock is the configured local wall-clock time for the daily sweep,
// expressed as an offset from midnight.
//
// Historical field: since the randomized slot (SigninRandomSlot) it is only a
// fallback for installs that predate the feature, and is no longer surfaced in
// the console.
func (s Settings) SigninClock() time.Duration {
	return time.Duration(s.Signin.Hour)*time.Hour + time.Duration(s.Signin.Minute)*time.Minute
}

// signinSlotWindowMin is the earliest minute of the Beijing day a randomized
// slot may land on, and signinSlotSpanMin is how many minutes the window spans.
//
// The window is 08:00–24:00 Beijing rather than the full 24 hours, for two
// reasons that both trace back to how the upstream accounts days. The streak
// and the seven-day board are day-shaped, and the upstream's day boundary is
// almost certainly Beijing midnight (check-in requests carry
// timezone_offset=480) or possibly UTC midnight. A slot uniform over 00:00–
// 24:00 local can put two of this schedule's slots inside one upstream day
// whenever the two midnights disagree, which signs into the wrong day: the
// claim burns the day's slot, reports success, and pays nothing — invisible,
// permanent, and hitting roughly every fifth day at worst. Anchoring slots to
// the Beijing calendar day with an 8-hour margin before local midnight makes
// that impossible for both candidate boundaries. It also reads human: nobody
// checks in at 03:47, and a check-in timestamp at that hour is the kind of
// fingerprint that uniform randomness invites.
const (
	signinSlotWindowMin = 8 * 60
	signinSlotSpanMin   = 16 * 60
)

// SigninRandomSlot derives the check-in instant for the Beijing calendar day
// containing t, as an offset from that day's midnight.
//
// Deterministic on purpose: hash(stable-seed, Beijing-date) → one minute in the
// day's window. Every restart, every NextRun call and the guard timer all
// derive the same instant for a given day, so the schedule survives reboots —
// but the instant differs day to day and is not guessable in advance, which is
// the point: a fixed 09:05 sweep is a pattern, and this is not.
//
// The seed is the admin salt, which already exists on every install, is
// regenerated only when the key is reset, and never leaves the store — a
// fingerprint of this deployment, not a secret in play. When the salt is empty
// (unreadable store, tests) the empty string still hashes; only the
// determinism-per-install is lost, never correctness.
func (s Settings) SigninRandomSlot(t time.Time) time.Duration {
	beijing := t.In(BeijingZone())
	seed := fmt.Sprintf("signin-slot|%s|%04d-%02d-%02d",
		s.Signin.SlotSeed, beijing.Year(), beijing.Month(), beijing.Day())
	sum := sha256.Sum256([]byte(seed))
	offset := int(binary.BigEndian.Uint64(sum[:8]) % uint64(signinSlotSpanMin))
	return time.Duration(signinSlotWindowMin+offset) * time.Minute
}

// BeijingZone is Asia/Shanghai, looked up once. It is the reference frame for
// the randomized check-in slot: the pool is international-only, but the
// operator and the upstream's likely day boundary both live in this zone, and
// a slot computed in whatever zone the VPS happens to run would change meaning
// on a server move.
func BeijingZone() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// An embedded tz database should always resolve; fall back to a fixed
		// UTC+8 rather than guessing from the host zone, so the slot semantics
		// survive even a stripped-down container.
		return time.FixedZone("CST+8", 8*60*60)
	}
	return loc
}

// SigninGap is the pause between two accounts during a sweep.
func (s Settings) SigninGap() time.Duration {
	return time.Duration(s.Signin.GapSeconds) * time.Second
}

// SigninTimeout bounds a single check-in call.
func (s Settings) SigninTimeout() time.Duration {
	seconds := s.Signin.TimeoutSec
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

// CreditFresh is how long a balance reading stays actionable for routing.
func (s Settings) CreditFresh() time.Duration {
	return time.Duration(s.Signin.CreditFreshMin) * time.Minute
}

// CreditRefresh is the background balance polling interval.
func (s Settings) CreditRefresh() time.Duration {
	return time.Duration(s.Signin.CreditRefreshMin) * time.Minute
}

// ParseIntOrDefault is a small helper for query parameters.
func ParseIntOrDefault(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

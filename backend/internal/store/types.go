package store

import (
	"time"

	"minimax2api/internal/config"
)

// Account status values.
const (
	StatusActive   = "active"
	StatusCooldown = "cooldown"
	StatusDisabled = "disabled"
	StatusInvalid  = "invalid"
	// StatusExhausted means the upstream refused the account for lack of
	// balance (HTTP 402 or the 50110-in-content form). Unlike a transient
	// cooldown it carries no recovery clock of its own: the account comes back
	// when credits land (check-in payout or top-up), or when the exhausted
	// sweep re-runs its check-in after ExhaustedRetryHours.
	StatusExhausted = "exhausted"
)

// Account kinds.
const (
	KindToken = "token"
	KindGuest = "guest"
)

// Model types.
//
// The distinction is not cosmetic: the gateway dispatches on it. A chat model
// goes to the conversation API unchanged; a video model is rewritten into a
// plugin reference with generation parameters attached, because MiniMax-H3 is
// not selectable as a model at all.
const (
	ModelTypeChat  = "chat"
	ModelTypeImage = "image"
	ModelTypeVideo = "video"
)

// Account regions. MiniMax operates two deployments with separate account
// databases, and a token issued by one is rejected by the other, so the region
// has to travel with the account.
const (
	RegionCN     = "cn"
	RegionGlobal = "global"
)

// Quota describes the last probe of an account.
type Quota struct {
	SyncedAt  time.Time `json:"syncedAt"`
	Available bool      `json:"available"`
	LatencyMs int64     `json:"latencyMs"`
	Plan      string    `json:"plan"`
	Note      string    `json:"note"`
}

// Check-in outcome values stored on an account.
const (
	SigninOK      = "ok"      // claimed (or already claimed) today
	SigninAlready = "already" // upstream said today's credits were taken
	SigninFailed  = "failed"
	SigninSkipped = "skipped" // mainland account, disabled, or no fingerprint
	// SigninUnpaid means the claim was accepted and the points never arrived.
	//
	// It is a distinct state because the claim endpoint cannot express it: it
	// answers success either way. Only the credit grants can tell the two apart,
	// and only well after the fact — the balance takes a minute to catch up, so
	// this is decided by a later reading, never at claim time.
	SigninUnpaid = "unpaid"
)

// SigninDay is one slot of the seven-day check-in cycle.
type SigninDay struct {
	DayNo   int  `json:"dayNo"`
	Points  int  `json:"points"`
	Status  int  `json:"status"`
	IsToday bool `json:"isToday"`
}

// SigninPanel is the persisted view of the upstream check-in board. It is kept
// so the console can render the seven-day strip without calling upstream on
// every page load.
type SigninPanel struct {
	Scene int         `json:"scene"`
	Days  []SigninDay `json:"days"`
}

// Credit is the last observed balance.
//
// Total is the number that matters for routing. The upstream reports it inside
// op_credit_summary.total_remaining_amount; the flat fields beside it belong to
// the pre-migration credit system and read zero on every migrated account.
type Credit struct {
	Total     int       `json:"total"`
	Free      int       `json:"free"`
	Purchased int       `json:"purchased"`
	PlanName  string    `json:"planName"`
	PlanType  int       `json:"planType"`
	SyncedAt  time.Time `json:"syncedAt"`
}

// CreditGrant is one batch of credits, as the upstream's credit/details
// endpoint reports it. Check-in payouts, promotional drops and purchased
// packs all land here as separate grants, each with its own expiry — which
// is what the console's credit breakdown panel is built from.
type CreditGrant struct {
	GrantedAt time.Time `json:"grantedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Granted is the amount issued and Remaining what is left of it. A grant
	// spent to zero is kept: it still shows where the credits came from.
	Granted   float64 `json:"granted"`
	Remaining float64 `json:"remaining"`
}

// Exhausted reports whether the balance is known to be spent.
func (c *Credit) Exhausted() bool {
	return c != nil && c.Total <= 0
}

type Account struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Region string `json:"region"`
	// Token is the JWT lifted from the agent site's localStorage. The renewal
	// loop overwrites it in place with the fresh JWT from /user/renewal; the
	// fingerprint fields below stay as they were — the renewed token works
	// against the original fingerprint.
	Token string `json:"token"`
	// TokenRenewedAt is when the renewal loop last swapped this account's
	// token, and TokenRenewExp the new token's expiry. Zero means never
	// renewed (the token still carries its original exp).
	TokenRenewedAt time.Time `json:"tokenRenewedAt,omitempty"`
	TokenRenewExp  time.Time `json:"tokenRenewExp,omitempty"`
	// UserID is the account's realUserID. The mainland deployment expects it
	// alongside the token, and it doubles as a stable display identifier.
	UserID string `json:"userId"`
	// Identifier is a best-effort email or phone number decoded from the token,
	// used to tell accounts apart in the console.
	Identifier string `json:"identifier"`
	// AgentID pins the account to a specific agent; empty means use the global
	// default.
	AgentID string `json:"agentID"`
	// The three fields below form the browser fingerprint. They are not
	// cosmetic: the `yy` signature is computed over the query string built from
	// them, so a token must be replayed with the fingerprint it was issued to.
	DeviceID     string `json:"deviceID"`
	UUID         string `json:"uuid"`
	ScreenWidth  int    `json:"screenWidth"`
	ScreenHeight int    `json:"screenHeight"`
	// BaseURL optionally overrides the host for this account.
	BaseURL string `json:"baseURL"`
	Group   string `json:"group"`
	// Gongyi marks an account dedicated to the public-welfare pool. Public keys
	// only see marked accounts; personal keys never see them.
	Gongyi        bool      `json:"gongyi,omitempty"`
	Remark        string    `json:"remark"`
	Enabled       bool      `json:"enabled"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"maxConcurrent"`
	Status        string    `json:"status"`
	CooldownUntil time.Time `json:"cooldownUntil"`
	// ExhaustedAt anchors the exhausted sweep's per-account retry clock; zero
	// while the account is not exhausted.
	ExhaustedAt  time.Time `json:"exhaustedAt"`
	FailCount    int       `json:"failCount"`
	SuccessCount int       `json:"successCount"`
	LastUsedAt   time.Time `json:"lastUsedAt"`
	LastError    string    `json:"lastError"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Quota        *Quota    `json:"quota,omitempty"`
	// SigninAt is the last check-in attempt, successful or not. Written even on
	// failure: the console shows the outcome, and reconcile judges the payout by
	// its timestamp. It is NOT the scheduler's "swept today" marker — the
	// enqueue worker stamps it for fresh accounts at any hour, so it cannot
	// distinguish "the pool was swept" from "one fresh account signed in". That
	// judgement is State.SigninSweptAt's job.
	SigninAt     time.Time    `json:"signinAt"`
	SigninStatus string       `json:"signinStatus"`
	SigninStreak int          `json:"signinStreak"`
	SigninPoints int          `json:"signinPoints"`
	SigninTotal  int64        `json:"signinTotal"`
	SigninError  string       `json:"signinError"`
	SigninPanel  *SigninPanel `json:"signinPanel,omitempty"`
	// Credit is the last observed balance, refreshed by the background poller
	// and by the sweep. It is advisory: routing only acts on it while it is
	// fresh (see Settings.CreditFresh).
	Credit *Credit `json:"credit,omitempty"`
	// Grants is the per-batch credit breakdown read alongside the balance.
	// Populated by the same poller pass — one extra request per account per
	// cycle — and kept on the account so the console's credit panel reads
	// cached data instead of walking the whole pool on open.
	Grants         []CreditGrant `json:"grants,omitempty"`
	GrantsSyncedAt time.Time     `json:"grantsSyncedAt,omitempty"`
}

// AccountView is the API representation. It is an explicit projection rather
// than an embedded struct so the raw token can never be serialised by accident.
type AccountView struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Region        string    `json:"region"`
	UserID        string    `json:"userId"`
	Identifier    string    `json:"identifier"`
	AgentID       string    `json:"agentID"`
	DeviceID      string    `json:"deviceID"`
	UUID          string    `json:"uuid"`
	ScreenWidth   int       `json:"screenWidth"`
	ScreenHeight  int       `json:"screenHeight"`
	BaseURL       string    `json:"baseURL"`
	Group         string    `json:"group"`
	Gongyi        bool      `json:"gongyi"`
	Remark        string    `json:"remark"`
	Enabled       bool      `json:"enabled"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"maxConcurrent"`
	Status        string    `json:"status"`
	CooldownUntil time.Time `json:"cooldownUntil"`
	ExhaustedAt   time.Time `json:"exhaustedAt"`
	FailCount     int       `json:"failCount"`
	SuccessCount  int       `json:"successCount"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	LastError     string    `json:"lastError"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// No omitempty: the console declares quota as a required field that may be
	// null, so the key must always be present. Omitting it would make the
	// property undefined instead of null and silently break strict checks.
	Quota          *Quota        `json:"quota"`
	SigninAt       time.Time     `json:"signinAt"`
	SigninStatus   string        `json:"signinStatus"`
	SigninStreak   int           `json:"signinStreak"`
	SigninPoints   int           `json:"signinPoints"`
	SigninTotal    int64         `json:"signinTotal"`
	SigninError    string        `json:"signinError"`
	SigninPanel    *SigninPanel  `json:"signinPanel"`
	Credit         *Credit       `json:"credit"`
	Grants         []CreditGrant `json:"grants"`
	GrantsSyncedAt time.Time     `json:"grantsSyncedAt"`
	// Token lifecycle: when the renewal loop last swapped the token and what
	// the new expiry is. Zero on both means "never renewed" — the token still
	// carries its original exp.
	TokenRenewedAt time.Time `json:"tokenRenewedAt"`
	TokenRenewExp  time.Time `json:"tokenRenewExp"`
	TokenMasked    string    `json:"tokenMasked"`
	Inflight       int       `json:"inflight"`
}

// NewAccountView projects an account for API responses.
func NewAccountView(account *Account, inflight int) AccountView {
	return AccountView{
		ID: account.ID, Name: account.Name, Kind: account.Kind, Region: account.Region,
		UserID: account.UserID, Identifier: account.Identifier, AgentID: account.AgentID,
		DeviceID: account.DeviceID, UUID: account.UUID,
		ScreenWidth: account.ScreenWidth, ScreenHeight: account.ScreenHeight,
		BaseURL: account.BaseURL, Group: account.Group, Gongyi: account.Gongyi, Remark: account.Remark,
		Enabled: account.Enabled, Priority: account.Priority,
		MaxConcurrent: account.MaxConcurrent, Status: account.Status,
		CooldownUntil: account.CooldownUntil, ExhaustedAt: account.ExhaustedAt,
		FailCount:    account.FailCount,
		SuccessCount: account.SuccessCount, LastUsedAt: account.LastUsedAt,
		LastError: account.LastError, CreatedAt: account.CreatedAt, UpdatedAt: account.UpdatedAt,
		Quota: account.Quota, TokenMasked: MaskToken(account.Token), Inflight: inflight,
		SigninAt: account.SigninAt, SigninStatus: account.SigninStatus,
		SigninStreak: account.SigninStreak, SigninPoints: account.SigninPoints,
		SigninTotal: account.SigninTotal, SigninError: account.SigninError,
		SigninPanel: account.SigninPanel, Credit: account.Credit,
		Grants: account.Grants, GrantsSyncedAt: account.GrantsSyncedAt,
		TokenRenewedAt: account.TokenRenewedAt, TokenRenewExp: account.TokenRenewExp,
	}
}

type ClientKey struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Key           string `json:"key"`
	Enabled       bool   `json:"enabled"`
	RPMLimit      int    `json:"rpmLimit"`
	MaxConcurrent int    `json:"maxConcurrent"`
	// Pool binds the key to an account pool: "" = personal, "gongyi" = public.
	Pool          string    `json:"pool,omitempty"`
	TotalRequests int64     `json:"totalRequests"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
}

type Audit struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"createdAt"`
	KeyName          string    `json:"keyName"`
	Model            string    `json:"model"`
	AccountName      string    `json:"accountName"`
	Status           int       `json:"status"`
	LatencyMs        int64     `json:"latencyMs"`
	FirstTokenMs     int64     `json:"firstTokenMs"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	Stream           bool      `json:"stream"`
	Retries          int       `json:"retries"`
	IP               string    `json:"ip"`
	UserAgent        string    `json:"userAgent"`
	Error            string    `json:"error"`
	RequestBody      string    `json:"requestBody"`
	ResponseBody     string    `json:"responseBody"`
}

type MediaItem struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	SourceURL   string    `json:"sourceUrl"`
	Prompt      string    `json:"prompt"`
	Model       string    `json:"model"`
	AccountName string    `json:"accountName"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Admin struct {
	Username string `json:"username"`
	// KeyHash is the PBKDF2 hash of the admin key. The JSON tag keeps its
	// historical name so a state file written before the username/password pair
	// became a single key still loads — losing it would silently re-seed a fresh
	// admin on every restart.
	KeyHash string `json:"passwordHash"`
	Salt    string `json:"salt"`
}

type ModelConfig struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	// UpstreamModel is the model identifier the upstream itself uses, when one
	// exists.
	//
	// It is not the same thing as Upstream. That field is a *local* dispatch
	// hint (`agent`, `chat`, `think`, `image`) and never leaves this process;
	// this one is a value the upstream recognises — `MiniMax-H3`, for instance,
	// which the video plugin expects to receive as a generation parameter.
	// Empty for the chat entries, which have no model selector to fill in.
	UpstreamModel string `json:"upstreamModel"`
	Type          string `json:"type"`
	Enabled       bool   `json:"enabled"`
	Builtin       bool   `json:"builtin"`
	Description   string `json:"description"`
	Requests      int64  `json:"requests"`
	Tokens        int64  `json:"tokens"`

	// New marks an entry the most recent registry refresh added. It is cleared
	// at the start of the next refresh, so the console can say "this is new"
	// for exactly one round.
	New bool `json:"new,omitempty"`

	// Rank is the entry's 1-based position in the upstream's own model order
	// (its picker's `model_order`), refreshed on every sync. 0 means the
	// registry does not list the entry. The console sorts by it so the model the
	// upstream currently leads with is at the top, without a release.
	Rank int `json:"rank,omitempty"`

	// Unavailable marks an entry the upstream no longer serves: a probe with its
	// upstream id came back substituted with another model, or the endpoint
	// rejected the id. Such an entry is kept for one round — the console shows
	// it struck through at the bottom — and removed by the next refresh that
	// still finds it unavailable. Requests naming it are refused rather than
	// silently answered by the substitute.
	Unavailable bool `json:"unavailable,omitempty"`

	// Removed is the tombstone left behind by that second refresh. The entry
	// stays in the state file (so MergeBuiltinModels cannot resurrect a built-in
	// on the next boot) but is invisible to the console and the API.
	Removed bool `json:"removed,omitempty"`
}

type State struct {
	Version    int             `json:"version"`
	Admin      Admin           `json:"admin"`
	Settings   config.Settings `json:"settings"`
	Accounts   []*Account      `json:"accounts"`
	ClientKeys []*ClientKey    `json:"clientKeys"`
	Audits     []*Audit        `json:"audits"`
	Media      []*MediaItem    `json:"media"`
	Models     []*ModelConfig  `json:"models"`
	// SigninSweptAt is when the daily full-pool sweep last *completed*, and is
	// the scheduler's authoritative "that day is served" marker.
	//
	// It is written once, after the walk finishes — not per account, and not at
	// sweep start. Per-account stamps would let a mid-sweep crash claim the day
	// was served; a start stamp would let it claim that while the walk is still
	// running. Neither may happen: the day is only consumed by a sweep that
	// actually walked the pool, so an interrupted sweep leaves the day unserved
	// and the next tick (or the next boot) re-runs it.
	SigninSweptAt time.Time `json:"signinSweptAt,omitempty"`
}

// BuiltinModels is the model catalogue exposed through /v1/models.
//
// The chat entries all reach the same upstream agent; a conversation cannot be
// routed to a different service by naming a different model. What an entry can
// do since the upstream grew a model selector (the desktop client picks between
// M3.1-Flash, M3, M2.7 and M2.7 HighSpeed) is *name* the model, and that is
// what UpstreamModel carries: the id placed into the request's `model` object.
// An entry without one — `minimax-agent`, `minimax-m3-thinking` — sends no
// model field at all, exactly as every build before the selector did.
//
// The ids below are the ones the upstream's own registry lists
// (`GET /mavis/api/v1/models`): `MiniMax-M3` and `MiniMax-M3.1-Flash`, with
// `MiniMax-M2.7` / `MiniMax-M2.7-highspeed` still in its whitelist. The list
// changes without a release here — the registry is the authority, and the
// console's "refresh from upstream" button is how a new id is adopted without
// one. `minimax-m3.1-flash-preview` is the *old* name for M3.1-Flash and is
// kept as an alias: the upstream still answers it, but it is no longer
// advertised. The global model-field template can still override or extend any
// of this; see buildMessageBody.
//
// The video entries are different in kind. MiniMax-H3 is not reachable as a
// chat model at all — it is a plugin the agent calls server-side, selected by
// mentioning the plugin in the message text. What the model entry carries is
// therefore not a routing decision but the *generation parameter*: the value
// that goes into the `<video-generation-options>` block.
func BuiltinModels() []*ModelConfig {
	return []*ModelConfig{
		{
			ID: "minimax-agent", Name: "MiniMax Agent", Upstream: "agent", Type: ModelTypeChat,
			Enabled: true, Builtin: true, Description: "通用 Agent，自动规划并调用工具",
		},
		{
			ID: "minimax-m3.1-flash", Name: "MiniMax M3.1 Flash", Upstream: "chat",
			UpstreamModel: "MiniMax-M3.1-Flash", Type: ModelTypeChat,
			Enabled: true, Builtin: true, New: true,
			Description: "当前一代对话模型；上游 2026-10 由 Preview 转正",
		},
		{
			ID: "minimax-m3.1-flash-preview", Name: "MiniMax M3.1 Flash Preview", Upstream: "chat",
			UpstreamModel: "MiniMax-M3.1-Flash-Preview", Type: ModelTypeChat,
			Enabled: true, Builtin: true,
			Description: "旧名称，上游已改名为 M3.1 Flash；保留作兼容别名",
		},
		{
			ID: "minimax-m3", Name: "MiniMax M3", Upstream: "chat", UpstreamModel: "MiniMax-M3", Type: ModelTypeChat,
			Enabled: true, Builtin: true, Description: "对话模式，响应更快",
		},
		{
			ID: "minimax-m3-thinking", Name: "MiniMax M3 Thinking", Upstream: "think", Type: ModelTypeChat,
			Enabled: true, Builtin: true, Description: "深度思考模式，附带推理内容",
		},
		{
			ID: "minimax-m2.7", Name: "MiniMax M2.7", Upstream: "chat", UpstreamModel: "MiniMax-M2.7", Type: ModelTypeChat,
			Enabled: true, Builtin: true, Description: "上一代对话模型",
		},
		{
			ID: "minimax-m2.7-highspeed", Name: "MiniMax M2.7 HighSpeed", Upstream: "chat",
			UpstreamModel: "MiniMax-M2.7-highspeed", Type: ModelTypeChat,
			Enabled: true, Builtin: true, Description: "上一代对话模型的高速版",
		},
		{
			ID: "minimax-image", Name: "MiniMax Image", Upstream: "image", Type: ModelTypeImage,
			Enabled: true, Builtin: true, Description: "图像生成",
		},
		{
			ID: "minimax-h3", Name: "MiniMax H3.0", Upstream: "video", UpstreamModel: "MiniMax-H3",
			Type: ModelTypeVideo, Enabled: true, Builtin: true,
			Description: "视频生成，质量优先；消耗账号积分，不占用 Token Plan；支持多模态参考",
		},
		{
			ID: "minimax-h3-max", Name: "MiniMax H3 Max", Upstream: "video", UpstreamModel: "MiniMax-H3-Max",
			Type: ModelTypeVideo, Enabled: true, Builtin: true,
			Description: "视频生成，约 20 秒完成；仅支持文生视频与首/尾帧，480P/768P，5-15 秒",
		},
		{
			ID: "minimax-hailuo-2-3", Name: "MiniMax Hailuo 2.3", Upstream: "video",
			UpstreamModel: "MiniMax-Hailuo-2.3",
			Type:          ModelTypeVideo, Enabled: true, Builtin: true,
			Description: "视频生成，成本更低；可用 Token Plan；输出无声视频",
		},
	}
}

// MergeBuiltinModels adds any built-in entry the stored catalogue is missing.
//
// Without this a new built-in never reaches an existing install. `Open` only
// seeds the catalogue when it is *empty*, so on every install that already has
// models the list is whatever that install first froze — and a release that adds
// a model would ship it to new users only, with no symptom on anyone else's
// machine. That is the same trap the settings defaults fall into; the difference
// is that here the fix has to be a merge, because the stored list is also where
// the operator's own enable/disable choices and usage counters live and must not
// be replaced.
//
// Entries already present are left exactly as stored. Only the *existence* of a
// built-in is enforced, never its state — with two deliberate exceptions: an
// upstream model id the stored copy is missing is adopted from the built-in, and
// a built-in that declares itself `New` is adopted as new by an install that
// already stored it. There is no console and no API that sets UpstreamModel, so
// an empty value on a built-in entry can never be the operator's intent; it is
// always the freeze of a default from before that entry carried an id. The same
// argument applies to the badge: an install that stored the entry before the
// badge existed would otherwise never show it. The adoption is self-limiting —
// once a refresh ranks the entry (Rank > 0), the badge is no longer re-applied.
func MergeBuiltinModels(models []*ModelConfig) ([]*ModelConfig, bool) {
	builtinByID := make(map[string]*ModelConfig)
	for _, builtin := range BuiltinModels() {
		builtinByID[builtin.ID] = builtin
	}
	known := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			known[model.ID] = struct{}{}
		}
	}
	changed := false
	for _, model := range models {
		if model == nil || !model.Builtin {
			continue
		}
		builtin, ok := builtinByID[model.ID]
		if !ok {
			continue
		}
		if builtin.UpstreamModel != "" && model.UpstreamModel == "" {
			model.UpstreamModel = builtin.UpstreamModel
			changed = true
		}
		if builtin.New && !model.New && model.Rank == 0 && !model.Removed {
			model.New = true
			changed = true
		}
	}
	for _, builtin := range BuiltinModels() {
		if _, ok := known[builtin.ID]; ok {
			continue
		}
		models = append(models, builtin)
		known[builtin.ID] = struct{}{}
		changed = true
	}
	return models, changed
}

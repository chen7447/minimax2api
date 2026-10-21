package admin

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"minimax2api/internal/store"
)

// Credential capture is a handshake between three parties: this console, an
// operator who is about to log in to MiniMax in a browser, and a helper running
// on the operator's own machine that drives that browser.
//
// The console cannot reach into a browser itself — the credential lives in
// another origin's localStorage, and a page cannot read it. So the flow is
// inverted: the console publishes a *request*, a helper on the operator's
// machine picks it up, opens the site, waits for the login, and posts the
// credential back. The session below is the shared state that makes the two
// halves agree on what is being attempted and when to stop.
//
// Everything here is deliberately in-memory. A capture is a live handshake with
// a browser that is open *right now*: persisting it across a restart would
// resurrect a request nobody is listening to, which is the exact "stuck in
// capturing" state the cancel button exists to prevent.
const (
	// captureTTL bounds a capture from the server side, so a helper that dies
	// (or never starts) cannot leave the console waiting forever. The operator
	// can also cancel by hand; this is the backstop for when they cannot.
	captureTTL = 3 * time.Minute
	// helperOnlineWindow is how recently the helper must have polled to count
	// as running. The helper polls every 2s, so this tolerates a few misses
	// before the console says it is gone.
	helperOnlineWindow = 15 * time.Second
)

// Capture states. `waiting` is the only non-terminal one.
const (
	captureWaiting   = "waiting"
	captureDelivered = "delivered"
	captureCancelled = "cancelled"
	captureExpired   = "expired"
)

// captureSession is one in-flight capture request.
type captureSession struct {
	ID        string
	Region    string
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Result is filled in on delivery, so the console can say what happened
	// rather than just "done".
	AccountID string
	Account   string
	Updated   bool
	Error     string
}

// captureHub owns the single active capture session.
//
// One at a time is a deliberate limit rather than a simplification: there is
// one operator and one browser profile, and two concurrent captures would race
// for the same Chrome instance with no way to tell whose login is whose.
type captureHub struct {
	mu       sync.Mutex
	current  *captureSession
	helperAt time.Time
}

func newCaptureHub() *captureHub {
	return &captureHub{}
}

// start opens a new session, superseding whatever was there.
//
// Superseding instead of refusing matters for the stuck case: if a previous
// request was left waiting by a helper that died, pressing the button again
// must work without the operator first hunting for a cancel button.
func (h *captureHub) start(region string, now time.Time) *captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	session := &captureSession{
		ID:        newCaptureID(),
		Region:    region,
		State:     captureWaiting,
		CreatedAt: now,
		ExpiresAt: now.Add(captureTTL),
	}
	h.current = session
	return session
}

// currentSession returns the live session with lazy expiry applied.
func (h *captureHub) session(now time.Time) *captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
	return h.current
}

// pending is what the helper polls: the session it should be working on, or nil
// when there is nothing to do. The poll doubles as the helper's heartbeat,
// which is why it lives here rather than in a separate ping endpoint — a helper
// that is polling is by definition alive.
func (h *captureHub) pending(now time.Time) *captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.helperAt = now
	h.expireLocked(now)
	if h.current == nil || h.current.State != captureWaiting {
		return nil
	}
	return h.current
}

// cancel stops a session. Idempotent, and it works even with no helper online:
// clearing the request is the whole point, since the operator may be cancelling
// precisely because nothing is listening.
func (h *captureHub) cancel(id string, now time.Time) *captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expireLocked(now)
	if h.current == nil || h.current.ID != id {
		return nil
	}
	if h.current.State == captureWaiting {
		h.current.State = captureCancelled
	}
	return h.current
}

// deliver records the outcome of a capture. It returns the session only when
// this call is the one that moved it out of `waiting`, so a duplicate delivery
// cannot create the account twice.
func (h *captureHub) deliver(id, accountID, accountName string, updated bool, err error) *captureSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil || h.current.ID != id || h.current.State != captureWaiting {
		return nil
	}
	h.current.AccountID = accountID
	h.current.Account = accountName
	h.current.Updated = updated
	if err != nil {
		h.current.State = captureCancelled
		h.current.Error = err.Error()
		return h.current
	}
	h.current.State = captureDelivered
	return h.current
}

// helperOnline reports whether a helper polled recently enough to be trusted.
func (h *captureHub) helperOnline(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.helperAt.IsZero() && now.Sub(h.helperAt) < helperOnlineWindow
}

func (h *captureHub) expireLocked(now time.Time) {
	if h.current == nil || h.current.State != captureWaiting {
		return
	}
	if now.After(h.current.ExpiresAt) {
		h.current.State = captureExpired
	}
}

// captureView is the wire shape of a session. `idle` is the state of a console
// that has no session at all — a distinct thing from a cancelled one, because
// the dialog renders differently (nothing attempted vs. attempt ended).
//
// The timestamps are pointers so an idle view carries nulls rather than Go's
// zero time, which would render as a capture from the year 1.
type captureView struct {
	ID           string     `json:"id"`
	Region       string     `json:"region"`
	State        string     `json:"state"`
	CreatedAt    *time.Time `json:"createdAt"`
	ExpiresAt    *time.Time `json:"expiresAt"`
	HelperOnline bool       `json:"helperOnline"`
	AccountID    string     `json:"accountID"`
	Account      string     `json:"account"`
	Updated      bool       `json:"updated"`
	Error        string     `json:"error"`
}

func (h *captureHub) view(now time.Time) captureView {
	session := h.session(now)
	view := captureView{State: "idle", HelperOnline: h.helperOnline(now)}
	if session == nil {
		return view
	}
	createdAt, expiresAt := session.CreatedAt, session.ExpiresAt
	view.ID = session.ID
	view.Region = session.Region
	view.State = session.State
	view.CreatedAt = &createdAt
	view.ExpiresAt = &expiresAt
	view.AccountID = session.AccountID
	view.Account = session.Account
	view.Updated = session.Updated
	view.Error = session.Error
	return view
}

// ------------------------------------------------------------------ handlers

func (a *API) startCapture(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Region string `json:"region"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	region := strings.TrimSpace(payload.Region)
	if region != store.RegionCN && region != store.RegionGlobal {
		writeError(w, http.StatusBadRequest, "区域只能是 cn（国内站）或 global（国际站）")
		return
	}
	a.capture.start(region, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"capture": a.capture.view(time.Now())})
}

func (a *API) getCapture(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"capture": a.capture.view(time.Now())})
}

func (a *API) cancelCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.capture.cancel(id, time.Now()) == nil {
		writeError(w, http.StatusNotFound, "抓取会话不存在或已结束")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capture": a.capture.view(time.Now())})
}

// capturePending is what the local helper polls. An empty body (rather than a
// 404) is the normal idle answer: the helper is expected to keep asking, and
// distinguishing "nothing to do" from "wrong URL" only produces noise.
func (a *API) capturePending(w http.ResponseWriter, r *http.Request) {
	session := a.capture.pending(time.Now())
	if session == nil {
		writeJSON(w, http.StatusOK, map[string]any{"capture": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capture": map[string]any{
		"id": session.ID, "region": session.Region, "expiresAt": session.ExpiresAt,
	}})
}

// captureCredential takes the credential the helper read out of the browser and
// turns it into an account.
//
// It reuses the import path's upsert rather than createAccount's strict insert:
// capturing the same account again is the normal way to refresh an expired
// token, and answering "already exists" would make the operator delete the
// account first — which throws away its quota history and check-in state.
func (a *API) captureCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload struct {
		Token    string `json:"token"`
		UUID     string `json:"uuid"`
		DeviceID string `json:"deviceID"`
	}
	if err := decode(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	// The session must still be the live one before anything is written: a
	// credential that arrives after a cancel is exactly what the cancel was
	// meant to prevent, and accepting it would resurrect the account the
	// operator just aborted.
	session := a.capture.session(time.Now())
	if session == nil || session.ID != id {
		writeError(w, http.StatusNotFound, "抓取会话不存在或已结束")
		return
	}
	if session.State != captureWaiting {
		writeError(w, http.StatusConflict, "抓取会话已结束")
		return
	}

	region := session.Region
	account, created, err := a.applyCapturedCredential(payload.Token, payload.UUID, payload.DeviceID, region)
	if err != nil {
		a.capture.deliver(id, "", "", false, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.capture.deliver(id, account.ID, account.Name, !created, nil)
	go a.syncQuotaDetached(account.ID)
	// A freshly captured account signs up today, not tomorrow: the daily sweep
	// has usually already run by the time the operator is capturing, and
	// nothing else would come back for this account until the next morning.
	a.signin.Enqueue(account.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"capture": a.capture.view(time.Now()),
		"account": a.view(account.ID),
		"created": created,
	})
}

// applyCapturedCredential turns a captured credential into a pool entry.
//
// Kept separate from the handler so the write itself can be tested without the
// background probe the handler kicks off: the probe talks to the upstream and
// would make a test either hit the network or race with its own assertions.
func (a *API) applyCapturedCredential(token, uuid, deviceID, region string) (*store.Account, bool, error) {
	account, err := buildAccount(accountPayload{
		Token: token, Region: region, UUID: uuid, DeviceID: deviceID,
	})
	if err != nil {
		return nil, false, err
	}

	existing := a.findByToken(account.Token)
	if existing == nil {
		if err := a.store.AddAccount(account); err != nil {
			return nil, false, err
		}
		return account, true, nil
	}

	// Re-capturing the same account is the documented way to refresh an expired
	// token, so an existing entry is updated rather than rejected — the same
	// rule the bulk import follows. Rejecting it would force the operator to
	// delete the account first, throwing away its quota history and check-in
	// state.
	updated, err := a.store.UpdateAccount(existing.ID, func(target *store.Account) {
		target.Token = account.Token
		target.Region = account.Region
		// A captured fingerprint is the real one: the browser that just logged
		// in is the client the upstream will see. Overwrite rather than only
		// fill blanks.
		if account.UUID != "" {
			target.UUID = account.UUID
		}
		if account.DeviceID != "" {
			target.DeviceID = account.DeviceID
		}
		if account.UserID != "" {
			target.UserID = account.UserID
		}
		if account.Identifier != "" {
			target.Identifier = account.Identifier
		}
		target.Status = store.StatusActive
		target.FailCount = 0
		target.LastError = ""
	})
	if err != nil {
		return nil, false, err
	}
	return updated, false, nil
}

// newCaptureID returns an opaque session id. It only has to be unique among the
// handful of sessions a single console ever creates, and unguessable enough
// that a stray request cannot land on someone else's capture.
func newCaptureID() string {
	return "cap_" + newUUID()
}

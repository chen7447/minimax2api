package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"minimax2api/internal/store"
)

// captureTestAPI builds the smallest API the capture handlers need. The capture
// hub is constructed here rather than through New so a test does not have to
// stand up a pool, a client and a check-in service to exercise a state machine.
func captureTestAPI(t *testing.T) *API {
	t.Helper()
	st, err := store.Open(t.TempDir(), "admin", "admin12345")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &API{store: st, capture: newCaptureHub(), started: time.Now()}
}

func TestCaptureSessionLifecycle(t *testing.T) {
	hub := newCaptureHub()
	now := time.Now()

	session := hub.start(store.RegionGlobal, now)
	if session.State != captureWaiting {
		t.Fatalf("new session state = %q, want %q", session.State, captureWaiting)
	}
	if got := hub.session(now); got == nil || got.ID != session.ID {
		t.Fatalf("session() did not return the live session")
	}

	// The helper's poll is the only thing that marks a helper as alive, so a
	// session created but never polled must report the helper as offline.
	if hub.helperOnline(now) {
		t.Fatal("helper reported online before it ever polled")
	}
	pending := hub.pending(now)
	if pending == nil || pending.ID != session.ID {
		t.Fatal("pending() did not hand the waiting session to the helper")
	}
	if !hub.helperOnline(now) {
		t.Fatal("helper not reported online right after polling")
	}
	if hub.helperOnline(now.Add(helperOnlineWindow + time.Second)) {
		t.Fatal("helper still online after the online window elapsed")
	}

	// Once delivered, the helper has nothing left to do — polling again must
	// not hand it the finished session, or it would re-capture in a loop.
	if hub.deliver(session.ID, "acc_1", "global-01", true, nil) == nil {
		t.Fatal("deliver() refused the first delivery")
	}
	if hub.pending(now) != nil {
		t.Fatal("pending() handed out a delivered session")
	}
	if again := hub.deliver(session.ID, "acc_2", "global-02", true, nil); again != nil {
		t.Fatal("deliver() accepted a duplicate delivery")
	}
}

func TestCaptureSessionExpiresWithoutAHelper(t *testing.T) {
	hub := newCaptureHub()
	now := time.Now()
	session := hub.start(store.RegionCN, now)

	// This is the guarantee behind the cancel button: a helper that dies mid
	// capture cannot leave the console waiting forever, because the session
	// expires on its own.
	view := hub.view(now.Add(captureTTL + time.Second))
	if view.State != captureExpired {
		t.Fatalf("state after TTL = %q, want %q", view.State, captureExpired)
	}
	if hub.pending(now.Add(captureTTL + time.Second)) != nil {
		t.Fatal("pending() handed out an expired session")
	}
	_ = session
}

func TestCaptureCancelIsIdempotentAndSurvivesAnOfflineHelper(t *testing.T) {
	hub := newCaptureHub()
	now := time.Now()
	session := hub.start(store.RegionGlobal, now)

	if hub.cancel("cap_someone_else", now) != nil {
		t.Fatal("cancel() accepted an id that is not the live session")
	}
	cancelled := hub.cancel(session.ID, now)
	if cancelled == nil || cancelled.State != captureCancelled {
		t.Fatalf("cancel() state = %v, want %q", cancelled, captureCancelled)
	}
	// A second press must not turn "cancelled" into an error or resurrect the
	// session: the button is expected to be safe to hit twice.
	again := hub.cancel(session.ID, now)
	if again == nil || again.State != captureCancelled {
		t.Fatalf("second cancel() state = %v, want %q", again, captureCancelled)
	}
	if hub.pending(now) != nil {
		t.Fatal("pending() handed out a cancelled session")
	}
	// Delivery after a cancel is the case the cancel exists to prevent.
	if hub.deliver(session.ID, "acc_1", "x", false, nil) != nil {
		t.Fatal("deliver() accepted a credential for a cancelled session")
	}
}

func TestCaptureStartSupersedesAStuckSession(t *testing.T) {
	hub := newCaptureHub()
	now := time.Now()
	stuck := hub.start(store.RegionGlobal, now)

	// Pressing the button again must work without first hunting for a cancel:
	// the previous request is replaced, not queued behind.
	fresh := hub.start(store.RegionCN, now.Add(time.Second))
	if fresh.ID == stuck.ID {
		t.Fatal("start() reused the previous session id")
	}
	if view := hub.view(now.Add(time.Second)); view.ID != fresh.ID || view.Region != store.RegionCN {
		t.Fatalf("view() = %+v, want the new session", view)
	}
	if hub.cancel(stuck.ID, now) != nil {
		t.Fatal("the superseded session was still cancellable")
	}
}

func TestCaptureViewIsIdleWithoutASession(t *testing.T) {
	hub := newCaptureHub()
	view := hub.view(time.Now())
	if view.State != "idle" {
		t.Fatalf("state = %q, want idle", view.State)
	}
	// The console serialises this straight to the browser, so an idle view must
	// not carry a half-filled session.
	if view.ID != "" || view.Region != "" {
		t.Fatalf("idle view leaked session fields: %+v", view)
	}
}

func TestCaptureCredentialCreatesThenUpdates(t *testing.T) {
	api := captureTestAPI(t)
	token := unsignedToken(t, map[string]any{"user_id": "u-1", "email": "capture@example.com"})

	account, created, err := api.applyCapturedCredential(token, "11111111-2222-4333-8444-555555555555", "12345678", store.RegionGlobal)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if !created {
		t.Fatal("first capture did not report a creation")
	}
	if account.UUID != "11111111-2222-4333-8444-555555555555" || account.DeviceID != "12345678" {
		t.Fatalf("captured fingerprint was not stored: uuid=%q deviceID=%q", account.UUID, account.DeviceID)
	}
	if account.Region != store.RegionGlobal {
		t.Fatalf("region = %q, want %q", account.Region, store.RegionGlobal)
	}

	// Same token, new fingerprint: this is the documented way to refresh a
	// token, and the captured fingerprint must win over the synthetic one the
	// first call generated for the fields that were supplied.
	refreshed, created, err := api.applyCapturedCredential(token, "99999999-8888-4777-8666-555555555555", "87654321", store.RegionGlobal)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if created {
		t.Fatal("re-capturing the same token created a second account")
	}
	if refreshed.ID != account.ID {
		t.Fatalf("re-capture produced a different account: %s vs %s", refreshed.ID, account.ID)
	}
	if refreshed.UUID != "99999999-8888-4777-8666-555555555555" || refreshed.DeviceID != "87654321" {
		t.Fatalf("fingerprint was not refreshed: uuid=%q deviceID=%q", refreshed.UUID, refreshed.DeviceID)
	}
	if total := len(api.store.ListAccounts()); total != 1 {
		t.Fatalf("account count = %d, want 1", total)
	}
}

func TestCaptureCredentialRejectsAMalformedToken(t *testing.T) {
	api := captureTestAPI(t)
	if _, _, err := api.applyCapturedCredential("not-a-token", "", "", store.RegionGlobal); err == nil {
		t.Fatal("a malformed token was accepted")
	}
	if total := len(api.store.ListAccounts()); total != 0 {
		t.Fatalf("account count = %d, want 0", total)
	}
}

func TestCaptureCredentialHandlerRefusesAFinishedSession(t *testing.T) {
	api := captureTestAPI(t)
	token := unsignedToken(t, map[string]any{"user_id": "u-2"})

	body := `{"token":` + jsonString(token) + `}`
	call := func(id string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/admin/api/capture/"+id+"/credential", strings.NewReader(body))
		request.SetPathValue("id", id)
		api.captureCredential(recorder, request)
		return recorder
	}

	if code := call("cap_unknown").Code; code != 404 {
		t.Fatalf("unknown session status = %d, want 404", code)
	}

	session := api.capture.start(store.RegionGlobal, time.Now())
	api.capture.cancel(session.ID, time.Now())
	// The credential arrives after the operator pressed cancel: accepting it
	// would create the very account they just aborted.
	if code := call(session.ID).Code; code != 409 {
		t.Fatalf("cancelled session status = %d, want 409", code)
	}
	if total := len(api.store.ListAccounts()); total != 0 {
		t.Fatalf("account count = %d, want 0", total)
	}
}

// jsonString renders a Go string as a JSON string literal, so the test body is
// built from the same value the helper would send.
func jsonString(value string) string {
	blob, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(blob)
}

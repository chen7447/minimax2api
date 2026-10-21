// Package signin drives the daily MiniMax check-in and the credit readings
// that keep spent accounts out of rotation.
//
// Only the international deployment (agent.minimax.io) speaks the protocol
// implemented in internal/minimax. Mainland accounts share the same product but
// a different check-in parameter set, so they are reported as skipped rather
// than signed with the wrong shape — a rejection there would be indistinguishable
// from a genuinely broken token and would retire a healthy account.
package signin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"minimax2api/internal/config"
	"minimax2api/internal/minimax"
	"minimax2api/internal/store"
)

// Client is the slice of the upstream client this package depends on. Declaring
// it here rather than taking *minimax.Client keeps the scheduler testable with a
// stub and free of any dependency on the transport layer.
type Client interface {
	SigninStatus(ctx context.Context, cred minimax.Credential) (*minimax.SigninPanel, error)
	SigninClaim(ctx context.Context, cred minimax.Credential) (*minimax.SigninClaim, error)
	Credit(ctx context.Context, cred minimax.Credential) (*minimax.CreditInfo, error)
	// Prepare runs the agent-side opening sequence. It has to happen before a
	// claim rather than after — see checkAccount for what goes wrong otherwise.
	Prepare(ctx context.Context, cred minimax.Credential) (*minimax.PrepareResult, error)
	// CreditGrants reads the per-grant breakdown, which is the only evidence
	// that a claimed payout was actually issued.
	CreditGrants(ctx context.Context, cred minimax.Credential) ([]minimax.CreditGrant, error)
	// RenewToken exchanges the account's current token for a fresh 30-day JWT.
	// The old token stays valid, so a failed renewal costs nothing but a retry.
	RenewToken(ctx context.Context, cred minimax.Credential) (string, error)
}

// AccountResult is one account's outcome within a sweep.
type AccountResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Points int    `json:"points"`
	Streak int    `json:"streak"`
	Credit int    `json:"credit"`
	Error  string `json:"error"`
}

// Report summarises a sweep.
type Report struct {
	StartedAt time.Time       `json:"startedAt"`
	EndedAt   time.Time       `json:"endedAt"`
	Total     int             `json:"total"`
	Claimed   int             `json:"claimed"`
	Already   int             `json:"already"`
	Failed    int             `json:"failed"`
	Skipped   int             `json:"skipped"`
	Points    int             `json:"points"`
	Results   []AccountResult `json:"results"`
}

// Service owns the daily timer and the credit poller.
type Service struct {
	store    *store.Store
	client   Client
	settings func() config.Settings
	credOf   func(*store.Account) minimax.Credential
	// slotSeed personalizes the randomized daily slot; empty falls back to the
	// legacy fixed clock. Set once at construction, see New.
	slotSeed string

	// queue carries ids of freshly added accounts waiting for their first
	// check-in. Buffered, and dropped on overflow: the daily sweep is the
	// backstop, so losing a queued id only delays that account to tomorrow.
	queue chan string

	mu      sync.Mutex
	running bool
	last    *Report
	lastAt  time.Time

	// Renewal pass state: claimed by both the timed loop and the console's
	// pool-wide button so they cannot stampede the renewal endpoint together.
	renewMu      sync.Mutex
	renewRunning bool
}

// New builds a Service. credOf converts a stored account into the credential
// the upstream client expects; it is injected so this package does not have to
// import the gateway. slotSeed is the per-install value that scatters the daily
// slot; empty keeps the legacy fixed schedule.
func New(st *store.Store, client Client, settings func() config.Settings, credOf func(*store.Account) minimax.Credential) *Service {
	return &Service{
		store: st, client: client, settings: settings, credOf: credOf,
		slotSeed: st.SigninSlotSeed(),
		queue:    make(chan string, 512),
	}
}

// Start launches the background loops. They stop when ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
	go s.creditLoop(ctx)
	go s.queueLoop(ctx)
	go s.exhaustedLoop(ctx)
	go s.renewLoop(ctx)
}

// Enqueue asks for one account's check-in as soon as the worker reaches it.
//
// The daily sweep covers only the accounts that exist when it runs, and once
// anything was swept today the scheduler moves straight to tomorrow's slot —
// so an account added afterwards would otherwise sit unsigned until the next
// morning. Every path that puts an account into the pool (manual add, bulk
// import, credential capture) calls this right after the write succeeds.
//
// It is safe to enqueue the same account twice: the second pass reads as
// "already claimed" upstream and costs one status read. A full queue drops the
// request rather than blocking the console request that captured the account.
func (s *Service) Enqueue(id string) {
	select {
	case s.queue <- id:
	default:
		log.Printf("自动签到队列已满，账号 %s 留待下次全量签到", id)
	}
}

// queueLoop serves the enqueue requests one at a time, with the same spacing
// as a sweep: the check-in endpoints sit behind the same risk control whether
// the trigger was the clock or a fresh capture.
func (s *Service) queueLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.queue:
			// Re-read inside the loop so flipping the switch in the console
			// takes effect without a restart, matching creditLoop.
			if !s.settings().Signin.Enabled {
				continue
			}
			if !s.waitForIdentity(ctx, id) {
				return
			}
			result, err := s.CheckOne(ctx, id)
			if err != nil {
				// Deleted between enqueue and here; nothing left to sign.
				continue
			}
			log.Printf("新号自动签到 %s：%s", result.Name, signinResultSummary(result))
			if !sleep(ctx, s.settings().SigninGap()) {
				return
			}
		}
	}
}

// waitForIdentity gives the account's asynchronous preparation a short window
// before the check-in is attempted.
//
// Every entry path kicks off prepareAccount in the background, and that call
// is what discovers user_id — a value the token does not carry and a check-in
// cannot run without. Signing immediately after the add therefore records a
// skip ("缺少 user_id") that the daily sweep would have to redo the next
// morning, when the identity had been sitting there all along. Discovery
// usually lands within a couple of seconds, so waiting is cheap; the wait
// ends early for accounts that will be skipped regardless (disabled, non-
// global) and after the deadline for accounts whose preparation failed, which
// the attempt then reports honestly.
func (s *Service) waitForIdentity(ctx context.Context, id string) bool {
	const identityWait = 15 * time.Second
	deadline := time.Now().Add(identityWait)
	for {
		account, ok := s.store.AccountByID(id)
		ready := !ok || // deleted; let CheckOne answer "账号不存在"
			!account.Enabled ||
			account.Region != store.RegionGlobal ||
			(account.UserID != "" && account.UserID != "0") ||
			!time.Now().Before(deadline)
		if ready {
			return true
		}
		if !sleep(ctx, time.Second) {
			return false
		}
	}
}

// signinResultSummary renders one account outcome for the log line.
func signinResultSummary(result AccountResult) string {
	switch result.Status {
	case store.SigninOK:
		return fmt.Sprintf("新领 %d 积分", result.Points)
	case store.SigninAlready:
		return "今日已领"
	case store.SigninSkipped:
		return "跳过：" + result.Error
	default:
		return "失败：" + result.Error
	}
}

// LastReport returns the most recent sweep, if any.
func (s *Service) LastReport() *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// LastRunAt reports when the last sweep finished.
func (s *Service) LastRunAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt
}

// Running reports whether a sweep is in flight.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ------------------------------------------------------------------ schedule

// slotOn is the check-in instant on the calendar day containing t.
//
// The slot is randomized per day (see SigninRandomSlot) once the deployment has
// a slot seed, and falls back to the legacy fixed hour:minute for stores that
// predate the seed. In both shapes the result is anchored to the Beijing day:
// the randomized window intentionally stops short of Beijing midnight so two
// slots can never land inside one upstream day (the upstream's day boundary is
// what the streak is booked against).
func (s *Service) slotOn(t time.Time) time.Time {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, config.BeijingZone())
	if s.slotSeed == "" {
		return midnight.Add(s.settings().SigninClock())
	}
	return midnight.Add(s.settings().SigninRandomSlot(t))
}

// NextRun is the next instant the sweep should fire.
//
// A slot that has already passed but was never served is due immediately rather
// than skipped. Without that, a machine that was asleep at the scheduled minute
// would silently miss the day, and a check-in tool that misses days is not worth
// scheduling. This is what makes the randomized slot safe: whenever the process
// starts after today's slot — a reboot, a swap-stall, anything — the sweep runs
// the moment the scheduler ticks.
func (s *Service) NextRun(now time.Time) time.Time {
	slot := s.slotOn(now)
	// The served day is checked before the "slot still ahead" branch: a day
	// swept early — a catch-up on startup, a manual run from the console — must
	// not be swept a second time at the drawn slot. Consulting the slot first
	// would do exactly that for any restart that lands before it.
	if s.sweptOn(now) {
		return slot.Add(24 * time.Hour)
	}
	if now.Before(slot) {
		return slot
	}
	return now
}

// sweptOn reports whether the daily sweep served the calendar day containing t.
//
// The store's SigninSweptAt — stamped only after a completed walk of the pool —
// is the sole authority. Per-account markers cannot do this job: the enqueue
// worker stamps SigninAt for freshly added accounts at any hour, so "someone
// signed in today" is not "the pool was swept today". Reading the wrong signal
// here is exactly how a pool of 382 goes unsigned: the day reads served off one
// fresh account's queue check-in and the real sweep skips to tomorrow.
func (s *Service) sweptOn(t time.Time) bool {
	swept := s.store.SigninSweptAt()
	if swept.IsZero() {
		return false
	}
	year, month, day := t.In(config.BeijingZone()).Date()
	local := swept.In(config.BeijingZone())
	sy, sm, sd := local.Date()
	return sy == year && sm == month && sd == day
}

func (s *Service) loop(ctx context.Context) {
	for {
		next := s.NextRun(time.Now())
		if !sleep(ctx, time.Until(next)) {
			return
		}
		// The tick that woke can predate a switch flip: the operator may have
		// disabled sign-ins between scheduling this wake-up and now. Re-checking
		// here keeps the off switch honest instead of honouring it only for the
		// queue, credit and exhausted loops.
		if !s.settings().Signin.Enabled {
			continue
		}
		s.Sweep(ctx)
		// Move straight to tomorrow's slot. Recomputing from NextRun would spin
		// here whenever the sweep legitimately marks nothing — an empty pool, or
		// a pool of accounts that were all skipped.
		if !sleep(ctx, time.Until(s.slotOn(time.Now().Add(24*time.Hour)))) {
			return
		}
	}
}

// sleep waits for d, reporting false if ctx was cancelled first. A zero or
// negative duration returns immediately.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// -------------------------------------------------------------------- sweep

// Sweep runs one check-in pass over the pool.
//
// Accounts are visited one at a time with a gap between them. Check-in sits
// behind the same risk control as everything else on the site, and a burst of
// simultaneous requests from a single address is the pattern that attracts it.
func (s *Service) Sweep(ctx context.Context) *Report {
	s.mu.Lock()
	if s.running {
		// Report "nothing happened" rather than the previous run: the console
		// needs to tell the operator that their click was absorbed by an
		// in-flight sweep, not that it completed instantly.
		s.mu.Unlock()
		return nil
	}
	s.running = true
	s.mu.Unlock()

	report := &Report{StartedAt: time.Now()}
	defer func() {
		report.EndedAt = time.Now()
		s.mu.Lock()
		s.running = false
		s.last = report
		s.lastAt = report.EndedAt
		s.mu.Unlock()
	}()

	accounts := s.store.ListAccounts()
	gap := s.settings().SigninGap()

	completed := true
	for index, account := range accounts {
		if ctx.Err() != nil {
			// A cancelled walk leaves the day unserved on purpose: the stamp is
			// only written for a sweep that reached the end of the pool, so an
			// interrupted run is retried by the next tick rather than recorded
			// as done with half the pool behind it.
			completed = false
			break
		}
		result := s.checkAccount(ctx, account)
		report.Total++
		switch result.Status {
		case store.SigninOK:
			report.Claimed++
			report.Points += result.Points
		case store.SigninAlready:
			report.Already++
		case store.SigninSkipped:
			report.Skipped++
		default:
			report.Failed++
		}
		report.Results = append(report.Results, result)
		if index < len(accounts)-1 && gap > 0 {
			if !sleep(ctx, gap) {
				completed = false
				break
			}
		}
	}
	log.Printf("签到完成：共 %d，新领 %d，已领 %d，失败 %d，跳过 %d，本次积分 %d",
		report.Total, report.Claimed, report.Already, report.Failed, report.Skipped, report.Points)
	// A completed walk consumes the day, whichever trigger set it going — the
	// scheduled slot, the console button, the guard timer: they all walk the
	// same pool, so letting the scheduler re-run the slot after a manual sweep
	// would only replay 382 "already claimed" reads against the risk control.
	// Failed accounts are still consumed: their retry paths (exhausted loop,
	// next day's slot) are the designed remedy, not an immediate re-sweep.
	// Stamp the wall clock here, not report.EndedAt — that field is written by
	// the deferred finalizer, i.e. after this line, and stamping it now would
	// persist the zero time and make the day read unserved forever.
	if completed {
		s.store.MarkSigninSwept(time.Now())
	}
	return report
}

// CheckOne runs the check-in for a single account, for the console's manual
// trigger.
func (s *Service) CheckOne(ctx context.Context, id string) (AccountResult, error) {
	account, ok := s.store.AccountByID(id)
	if !ok {
		return AccountResult{}, errors.New("账号不存在")
	}
	return s.checkAccount(ctx, account), nil
}

// checkAccount performs the status-then-claim sequence for one account.
func (s *Service) checkAccount(ctx context.Context, account *store.Account) AccountResult {
	result := AccountResult{ID: account.ID, Name: account.Name}
	if account.Credit != nil {
		result.Credit = account.Credit.Total
	}

	// Refuse to sign an account the protocol does not cover, and say so plainly
	// instead of recording a failure that looks like a broken token.
	if reason := skipReason(account); reason != "" {
		result.Status = store.SigninSkipped
		result.Error = reason
		s.record(account.ID, func(target *store.Account) {
			target.SigninAt = time.Now()
			target.SigninStatus = store.SigninSkipped
			target.SigninError = reason
		})
		return result
	}

	cred := s.credOf(account)
	timeout := s.settings().SigninTimeout()

	// The agent-side opening sequence runs first — before the status read as well
	// as before the claim, matching the order the web client uses.
	//
	// `/config` is what creates the account's record on the agent side. A claim
	// made before that record exists is registered and never paid out: the
	// endpoint still answers `claim_result=1`, so the failure is completely
	// silent, and running the sequence afterwards does not bring the points
	// back. Measured on fresh accounts, the correct order paid out 4/4 times
	// (within seconds) while the reverse order left two accounts at zero for
	// good.
	//
	// So a failure here stops the check-in. Losing a day to a visible failure is
	// recoverable — the operator can re-run the sweep — whereas a claim in the
	// wrong order loses the day and reports success.
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, timeout)
	_, prepareErr := s.client.Prepare(prepareCtx, cred)
	cancelPrepare()
	if prepareErr != nil {
		return s.fail(account, result, fmt.Errorf("agent 侧初始化失败，已跳过领取以免积分静默丢失: %w", prepareErr))
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	panel, err := s.client.SigninStatus(callCtx, cred)
	if err != nil {
		return s.fail(account, result, err)
	}
	result.Streak = panel.TodayDayNo

	if panel.ClaimedToday {
		// Already served today: still refresh the balance, because the sweep is
		// the only time we know the account is reachable.
		result.Status = store.SigninAlready
		result.Points = panel.TodayPoints
		credit := s.refreshCredit(ctx, account)
		if credit >= 0 {
			result.Credit = credit
		}
		s.record(account.ID, func(target *store.Account) {
			target.SigninAt = time.Now()
			target.SigninStatus = store.SigninAlready
			target.SigninStreak = panel.TodayDayNo
			target.SigninPoints = panel.TodayPoints
			target.SigninError = ""
			target.SigninPanel = panelOf(panel)
		})
		return result
	}

	// The opening sequence already ran, above the status read.
	claimCtx, cancelClaim := context.WithTimeout(ctx, timeout)
	defer cancelClaim()
	claim, err := s.client.SigninClaim(claimCtx, cred)
	if err != nil {
		return s.fail(account, result, err)
	}

	result.Points = claim.Points
	result.Streak = claim.DayNo
	if claim.IsDuplicate() {
		result.Status = store.SigninAlready
	} else {
		result.Status = store.SigninOK
	}
	credit := s.refreshCredit(ctx, account)
	if credit >= 0 {
		result.Credit = credit
	}

	// Prefer the board the upstream echoed back from the claim: it is the state
	// *after* the claim. The panel fetched a moment earlier still shows today as
	// unclaimed, and storing that would leave the console displaying "claimed"
	// beside an empty dot for today.
	stored := claim.Panel
	if stored == nil {
		stored = claimPanel(panel, claim)
	}

	s.record(account.ID, func(target *store.Account) {
		target.SigninAt = time.Now()
		target.SigninStatus = result.Status
		target.SigninStreak = claim.DayNo
		target.SigninPoints = claim.Points
		target.SigninError = ""
		target.SigninPanel = panelOf(stored)
		if !claim.IsDuplicate() && claim.Points > 0 {
			target.SigninTotal += int64(claim.Points)
		}
	})
	return result
}

// claimPanel folds a claim onto the board fetched just before it.
//
// Fallback for when the claim response carries no panel of its own. The only
// thing a claim changes is today's slot, and the claim response names that slot
// explicitly, so there is nothing to guess at.
//
// A duplicate answer ("already claimed today") still means today is served. We
// only reach the claim at all when the earlier board said it was not, so the
// board was the stale one — trust the claim about the claim.
func claimPanel(before *minimax.SigninPanel, claim *minimax.SigninClaim) *minimax.SigninPanel {
	if before == nil {
		return nil
	}
	after := &minimax.SigninPanel{
		Scene:        before.Scene,
		TodayDayNo:   before.TodayDayNo,
		TodayPoints:  before.TodayPoints,
		ClaimedToday: before.ClaimedToday,
		Days:         append([]minimax.SigninDay(nil), before.Days...),
	}
	for i, day := range after.Days {
		if day.IsToday || (claim.DayNo > 0 && day.DayNo == claim.DayNo) {
			after.Days[i].Status = minimax.SigninDayClaimed
			// A duplicate answer may not restate the points; keep what the board
			// already said rather than blanking a day that was worth something.
			if claim.Points > 0 {
				after.Days[i].Points = claim.Points
			}
			after.ClaimedToday = true
			after.TodayPoints = after.Days[i].Points
			after.TodayDayNo = after.Days[i].DayNo
		}
	}
	return after
}

// fail records an unsuccessful attempt. The attempt timestamp is written even
// on failure so the day counts as served: otherwise a sweep in which every
// account errored would be retried on the next tick, and a persistent upstream
// outage would turn into a request loop.
func (s *Service) fail(account *store.Account, result AccountResult, err error) AccountResult {
	result.Status = store.SigninFailed
	result.Error = truncate(err.Error(), 200)

	invalid := errors.Is(err, minimax.ErrInvalidCredential)
	s.record(account.ID, func(target *store.Account) {
		target.SigninAt = time.Now()
		target.SigninStatus = store.SigninFailed
		target.SigninError = result.Error
		if invalid {
			target.Status = store.StatusInvalid
			target.CooldownUntil = time.Time{}
			target.LastError = "签到：令牌已被上游拒绝"
		}
	})
	return result
}

// skipReason explains why an account cannot be checked in, or "" when it can.
func skipReason(account *store.Account) string {
	if !account.Enabled {
		return "账号已禁用"
	}
	if account.Region != store.RegionGlobal {
		return "仅支持国际站账号（国内站签到参数不同）"
	}
	if strings.TrimSpace(account.Token) == "" || account.UUID == "" || account.DeviceID == "" {
		return "缺少令牌或设备指纹"
	}
	// Every signed endpoint needs user_id, and the upstream answers a bare 401
	// without it — the same 401 a dead token produces. Attempting the call would
	// therefore retire a healthy account as invalid, so refuse before spending
	// the request. The value is usually filled in automatically when the account
	// is added; reaching here means that discovery failed.
	if strings.TrimSpace(account.UserID) == "" || account.UserID == "0" {
		return "缺少 user_id（realUserID），未从上游取得，无法签到"
	}
	return ""
}

// record applies a mutation to a stored account.
func (s *Service) record(id string, mutate func(*store.Account)) {
	s.store.SaveAccountState(id, mutate)
}

func panelOf(panel *minimax.SigninPanel) *store.SigninPanel {
	if panel == nil {
		return nil
	}
	out := &store.SigninPanel{Scene: panel.Scene, Days: make([]store.SigninDay, 0, len(panel.Days))}
	for _, day := range panel.Days {
		out.Days = append(out.Days, store.SigninDay{
			DayNo: day.DayNo, Points: day.Points, Status: day.Status, IsToday: day.IsToday,
		})
	}
	return out
}

// ---------------------------------------------------------------- reconciling

// reconcileGrace is how long a payout is given to show up after a claim.
//
// The grant list lags the claim. A payout has been observed as fast as zero
// seconds, but a reading taken five seconds later still showed nothing and one
// case took over a minute to appear. Judging any sooner than this would report a
// perfectly healthy check-in as unpaid.
const reconcileGrace = 5 * time.Minute

// reconcile decides whether today's claimed points were actually issued.
//
// The claim endpoint cannot answer this. It reports `claim_result=1` whether or
// not the points were ever created — when the account's agent-side record did
// not exist at claim time they are dropped, silently and permanently — so the
// only evidence is a credit grant dated today.
//
// The decision reads the grant's *timestamp*, never its balance. An account that
// legitimately spent everything still has the grant, and must not be reported as
// unpaid for having used what it was given.
//
// Costs one request per account per balance refresh, and only while a claim is
// outstanding for the current day; the balance poller is off by default.
func (s *Service) reconcile(ctx context.Context, account *store.Account) {
	if account.Region != store.RegionGlobal || account.SigninAt.IsZero() {
		return
	}
	switch account.SigninStatus {
	case store.SigninOK, store.SigninAlready, store.SigninUnpaid:
	default:
		return
	}
	// Only today's claim is judgeable: a grant list cannot say which day a
	// missing payout belonged to, and yesterday's is already beyond recovery.
	if !sameLocalDay(account.SigninAt, time.Now()) {
		return
	}
	if time.Since(account.SigninAt) < reconcileGrace {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, s.settings().SigninTimeout())
	defer cancel()
	grants, err := s.client.CreditGrants(callCtx, s.credOf(account))
	if err != nil {
		// A read failure says nothing about the payout. Leave the state alone
		// rather than flagging an account on a hiccup.
		return
	}

	for _, grant := range grants {
		if sameLocalDay(grant.GrantedAt, account.SigninAt) {
			// Paid. Clear a flag set by an earlier pass, which is possible
			// because the grant can take a minute to appear. The distinction
			// between ok and already is not worth preserving here; the claim is
			// confirmed either way.
			if account.SigninStatus == store.SigninUnpaid {
				s.record(account.ID, func(target *store.Account) {
					target.SigninStatus = store.SigninOK
					target.SigninError = ""
				})
			}
			return
		}
	}

	s.record(account.ID, func(target *store.Account) {
		target.SigninStatus = store.SigninUnpaid
		target.SigninError = "签到返回成功，但上游没有发放积分：credit/details 里没有今天的发放记录（顺序或风控问题，次日重试）"
	})
}

// sameLocalDay reports whether two instants fall on the same calendar day.
func sameLocalDay(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

// ------------------------------------------------------------------- credit

// refreshCredit reads the balance and stores it, returning the new total or -1
// when the read failed.
//
// The failure path is deliberately silent about status: a credit read failing
// says nothing about whether the token works, and marking the account invalid
// here would retire healthy accounts whenever the balance endpoint hiccups.
func (s *Service) refreshCredit(ctx context.Context, account *store.Account) int {
	if account.Region != store.RegionGlobal {
		return -1
	}
	// Independent of the balance read: a claim that paid out is worth confirming
	// even when the balance endpoint is unhappy.
	s.reconcile(ctx, account)

	callCtx, cancel := context.WithTimeout(ctx, s.settings().SigninTimeout())
	defer cancel()

	cred := s.credOf(account)
	info, err := s.client.Credit(callCtx, cred)
	if err != nil {
		return -1
	}
	// The per-grant breakdown rides the same poller pass. One more request
	// inside the existing risk-control spacing beats a second walk of the
	// pool; a failure here must not discard the fresh balance above.
	grants, grantErr := s.client.CreditGrants(callCtx, cred)
	s.record(account.ID, func(target *store.Account) {
		applyCredit(target, info)
		if grantErr == nil {
			target.Grants = grantsOf(grants)
			target.GrantsSyncedAt = time.Now()
		}
	})
	return info.Total
}

// applyCredit stores a fresh balance and revives an exhausted account whose
// balance came back. This is the credit-activation recovery: an exhausted
// account carries no recovery clock of its own, so any reading above zero —
// check-in payout, top-up, manual refresh — puts it back into the reserve,
// from which the working set pulls it when it ranks highest.
func applyCredit(target *store.Account, info *minimax.CreditInfo) {
	target.Credit = &store.Credit{
		Total: info.Total, Free: info.Free, Purchased: info.Purchased,
		PlanName: info.PlanName, PlanType: info.PlanType, SyncedAt: time.Now(),
	}
	if info.Total > 0 && target.Status == store.StatusExhausted {
		target.Status = store.StatusActive
		target.ExhaustedAt = time.Time{}
		target.CooldownUntil = time.Time{}
		target.LastError = ""
		log.Printf("账号 %s 额度到账（%d），自动恢复使用", target.Name, info.Total)
	}
}

// RefreshCredit is the console's manual balance refresh.
func (s *Service) RefreshCredit(ctx context.Context, id string) (*store.Credit, error) {
	account, ok := s.store.AccountByID(id)
	if !ok {
		return nil, errors.New("账号不存在")
	}
	if account.Region != store.RegionGlobal {
		return nil, errors.New("余额接口仅支持国际站账号")
	}
	s.reconcile(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, s.settings().SigninTimeout())
	defer cancel()

	cred := s.credOf(account)
	info, err := s.client.Credit(callCtx, cred)
	if err != nil {
		return nil, err
	}
	// Same deal as the background poller: the breakdown rides along, but its
	// failure does not sink the balance update.
	grants, grantErr := s.client.CreditGrants(callCtx, cred)
	s.record(id, func(target *store.Account) {
		applyCredit(target, info)
		if grantErr == nil {
			target.Grants = grantsOf(grants)
			target.GrantsSyncedAt = time.Now()
		}
	})
	return &store.Credit{
		Total: info.Total, Free: info.Free, Purchased: info.Purchased,
		PlanName: info.PlanName, PlanType: info.PlanType, SyncedAt: time.Now(),
	}, nil
}

// grantsOf converts upstream grants into their stored shape. Upstream amounts
// are floats; they travel unchanged. A nil result is stored as an empty slice
// so a legitimately empty breakdown does not read as "never fetched" — that
// distinction is GrantsSyncedAt's job.
func grantsOf(grants []minimax.CreditGrant) []store.CreditGrant {
	out := make([]store.CreditGrant, 0, len(grants))
	for _, grant := range grants {
		out = append(out, store.CreditGrant{
			GrantedAt: grant.GrantedAt, ExpiresAt: grant.ExpiresAt,
			Granted: grant.Granted, Remaining: grant.Remaining,
		})
	}
	return out
}

// RefreshGrants re-reads only the per-grant breakdown for one account — the
// console's per-account sync on the credit panel. It skips the balance call:
// a breakdown without a fresh balance is fine there, and it keeps a manual
// click to two upstream requests instead of three.
func (s *Service) RefreshGrants(ctx context.Context, id string) ([]store.CreditGrant, error) {
	account, ok := s.store.AccountByID(id)
	if !ok {
		return nil, errors.New("账号不存在")
	}
	if account.Region != store.RegionGlobal {
		return nil, errors.New("积分明细接口仅支持国际站账号")
	}
	callCtx, cancel := context.WithTimeout(ctx, s.settings().SigninTimeout())
	defer cancel()

	grants, err := s.client.CreditGrants(callCtx, s.credOf(account))
	if err != nil {
		return nil, err
	}
	stored := grantsOf(grants)
	s.record(id, func(target *store.Account) {
		target.Grants = stored
		target.GrantsSyncedAt = time.Now()
	})
	return stored, nil
}

// creditLoop keeps the stored balances warm so the routing guard has something
// current to act on without waiting for the next sweep.
func (s *Service) creditLoop(ctx context.Context) {
	for {
		interval := s.settings().CreditRefresh()
		if interval <= 0 {
			// Disabled. Re-check periodically rather than returning, so turning
			// the poller back on in the console takes effect without a restart.
			if !sleep(ctx, time.Minute) {
				return
			}
			continue
		}
		// Cold start: a pool where no account has a grant breakdown yet (first
		// run of the credit panel, or a freshly imported pool) walks right away
		// instead of idling a full interval — the panel would otherwise sit at
		// "未同步" for up to 30 minutes after every deploy that reset the data.
		// Steady-state restarts keep the sleep-first rhythm: grants survive the
		// restart on disk, so nothing justifies re-hitting upstream immediately.
		if !grantsColdStart(s.store.ListAccounts()) {
			if !sleep(ctx, interval) {
				return
			}
		}
		if !s.settings().Signin.Enabled {
			continue
		}
		for _, account := range s.store.ListAccounts() {
			if ctx.Err() != nil {
				return
			}
			if !account.Enabled || account.Region != store.RegionGlobal {
				continue
			}
			s.refreshCredit(ctx, account)
			if !sleep(ctx, s.settings().SigninGap()) {
				return
			}
		}
	}
}

// grantsColdStart reports whether no account carries a grant breakdown yet.
func grantsColdStart(accounts []*store.Account) bool {
	for _, account := range accounts {
		if !account.GrantsSyncedAt.IsZero() {
			return false
		}
	}
	return len(accounts) > 0
}

// exhaustedLoop is the failed-check-in safety net for exhausted accounts.
//
// The daily sweep already re-runs every account's check-in, but it marks the
// day as served even when a claim errors, so an exhausted account whose sweep
// failed would sit at zero until the next sweep — and succeed or fail again on
// the same wall clock forever. Each exhausted account therefore carries its
// own retry clock (ExhaustedAt): once it has been exhausted for
// ExhaustedRetryHours its check-in runs again here. A payout revives it via
// applyCredit; a flat balance keeps it exhausted and resets the clock for the
// next attempt.
func (s *Service) exhaustedLoop(ctx context.Context) {
	for {
		if !sleep(ctx, 30*time.Minute) {
			return
		}
		if !s.settings().Signin.Enabled {
			continue
		}
		retry := s.settings().ExhaustedRetry()
		if retry <= 0 {
			continue
		}
		now := time.Now()
		for _, account := range s.store.ListAccounts() {
			if ctx.Err() != nil {
				return
			}
			if account.Status != store.StatusExhausted || account.ExhaustedAt.IsZero() {
				continue
			}
			if now.Sub(account.ExhaustedAt) < retry {
				continue
			}
			if s.Running() {
				// A full sweep is in flight and will reach this account on
				// its own; running both would double-hit the claim endpoint.
				break
			}
			log.Printf("账号 %s 已耗尽超过 %s，自动重试签到", account.Name, retry)
			if _, err := s.CheckOne(ctx, account.ID); err != nil {
				log.Printf("账号 %s 耗尽重试签到失败：%v", account.Name, err)
			}
			s.record(account.ID, func(target *store.Account) {
				if target.Status == store.StatusExhausted {
					target.ExhaustedAt = time.Now()
				}
			})
			if !sleep(ctx, s.settings().SigninGap()) {
				return
			}
		}
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// ---------------------------------------------------------------- renewing

// renewSkew is how long before a token's expiry the renewal loop moves on it.
//
// Three days absorbs a long upstream outage plus a weekend of failed retries,
// with margin: renewal is a single lightweight POST whose only prerequisite is
// that the old token still validates, so even a window measured in hours would
// do — this just keeps the loop out of the panic business entirely.
const renewSkew = 3 * 24 * time.Hour

// renewDedup is how long a successful renewal holds the loop off an account.
// The swap is idempotent and the old token survives it, so a redundant pass is
// merely a wasted request; the dedup window exists to keep a burst of sweeps
// from stampeding the renewal endpoint, not to guard correctness.
const renewDedup = 24 * time.Hour

// tokenExpiry decodes the JWT's exp claim without verifying the signature —
// the upstream validates it, and locally the claim is only read to schedule
// the renewal. A token without a parsable exp reports zero: the loop then
// skips the account rather than renewing on a guess. That is the safe side:
// renewing an unparseable token would be fine (the endpoint would answer or
// not), but a stampede of pointless renewals every half hour would not.
func tokenExpiry(token string) time.Time {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// RenewOne renews a single account's token through the official endpoint and
// persists the result. Exported for the console's per-account button.
//
// Failure touches nothing: the old token is still valid after a rejected
// renewal, and the loop simply tries again later. A success updates the token,
// stamps the renewal, and records the new expiry for the console.
func (s *Service) RenewOne(ctx context.Context, id string) (time.Time, error) {
	account, ok := s.store.AccountByID(id)
	if !ok {
		return time.Time{}, errors.New("账号不存在")
	}
	if strings.TrimSpace(account.Token) == "" {
		return time.Time{}, errors.New("账号没有 token")
	}

	callCtx, cancel := context.WithTimeout(ctx, s.settings().SigninTimeout())
	defer cancel()
	fresh, err := s.client.RenewToken(callCtx, s.credOf(account))
	if err != nil {
		return time.Time{}, err
	}

	exp := tokenExpiry(fresh)
	if exp.IsZero() {
		// The endpoint answered but the payload does not parse as our JWT
		// shape. Do not store a token we cannot schedule; leave the old one in
		// force and surface the oddity.
		return time.Time{}, errors.New("renewal 返回的 token 无法解析 exp，未落库")
	}
	s.store.SaveAccountState(id, func(target *store.Account) {
		target.Token = fresh
		target.TokenRenewedAt = time.Now()
		target.TokenRenewExp = exp
	})
	return exp, nil
}

// renewLoop keeps every token ahead of its expiry.
//
// Each account's JWT carries an exp (about 30 days out); the official
// /user/renewal endpoint swaps it for a fresh one, no login flow involved. The
// loop walks the pool every half hour and renews whatever falls inside the
// skew window — typically a handful of accounts a day, given the pool's
// original issue dates cluster within days of each other.
//
// There is no lock coordination with the sweep: renewal touches a different
// endpoint, needs no prepare sequence, and replaces state the sweep does not
// read. The only shared resource is the risk control, paid for with the same
// gap spacing.
func (s *Service) renewLoop(ctx context.Context) {
	for {
		if !sleep(ctx, 30*time.Minute) {
			return
		}
		if !s.StartRenewPass() {
			continue // a manual pass is in flight; it covers the same ground
		}
		if !s.renewPass(ctx) {
			s.EndRenewPass()
			return
		}
		s.EndRenewPass()
	}
}

// StartRenewPass claims the renewal slot for a pool-wide pass, reporting false
// when one is already running — the console's button and the timed loop both
// come through here, and a double pass would only stampede the endpoint.
func (s *Service) StartRenewPass() bool {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	if s.renewRunning {
		return false
	}
	s.renewRunning = true
	return true
}

// EndRenewPass releases the renewal slot.
func (s *Service) EndRenewPass() {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	s.renewRunning = false
}

// RenewPassDetached runs one pool-wide pass under the claimed slot. It is the
// console entry's body; the timed loop calls renewPass directly. Reports false
// only when the context died mid-pass.
func (s *Service) RenewPassDetached(ctx context.Context) bool {
	return s.renewPass(ctx)
}

// Renewing reports whether a pool-wide renewal pass is in flight, for the
// console's button state.
func (s *Service) Renewing() bool {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	return s.renewRunning
}

// renewPass walks the pool once, renewing every account inside the window. It
// reports false only when the context died mid-pass.
//
// An account is due when its token's exp falls inside the skew window. The exp
// is the stored TokenRenewExp when a renewal already landed, otherwise decoded
// straight from the current token. A successful renewal also stamps
// TokenRenewedAt, and the dedup window keeps a same-day second pass from
// re-hitting the endpoint for an account it just served.
func (s *Service) renewPass(ctx context.Context) bool {
	now := time.Now()
	due := 0
	for _, account := range s.store.ListAccounts() {
		if ctx.Err() != nil {
			return false
		}
		if account.Region != store.RegionGlobal || strings.TrimSpace(account.Token) == "" {
			continue
		}
		exp := account.TokenRenewExp
		if exp.IsZero() {
			exp = tokenExpiry(account.Token)
		}
		if exp.IsZero() || now.Add(renewSkew).Before(exp) {
			// Unparsable token (never schedule on a guess), or expiry safely
			// beyond the window.
			continue
		}
		if !account.TokenRenewedAt.IsZero() && now.Sub(account.TokenRenewedAt) < renewDedup {
			continue
		}
		due++
		newExp, err := s.RenewOne(ctx, account.ID)
		if err != nil {
			// The old token remains valid — a rejected renewal is a retry,
			// not a retirement. Say so and move on.
			log.Printf("token 续期 %s 失败（旧 token 未受影响）：%v", account.Name, err)
		} else {
			log.Printf("token 续期 %s：新到期 %s", account.Name, newExp.Local().Format("01-02 15:04"))
		}
		if !sleep(ctx, s.settings().SigninGap()) {
			return false
		}
	}
	if due > 0 {
		log.Printf("token 续期轮完成：处理 %d 个账号", due)
	}
	return true
}

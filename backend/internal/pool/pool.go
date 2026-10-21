package pool

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"minimax2api/internal/config"
	"minimax2api/internal/minimax"
	"minimax2api/internal/store"
)

var (
	// ErrNoAccount means the pool is empty.
	ErrNoAccount = errors.New("no account available")
	// ErrNoCapacity means every account is saturated or cooling down.
	ErrNoCapacity = errors.New("all accounts are busy")
	// ErrQuotaDrained marks an upstream refusal for lack of balance. It arrives
	// either as a real HTTP 402 from the model endpoints or as HTTP 200 with
	// the failure embedded in the assistant content
	// ("50110:insufficient balance (1008)"); the transports fold both into this
	// verdict. Only release() can act on it: the account is marked exhausted
	// and leaves the working set until credits land again.
	ErrQuotaDrained = errors.New("upstream quota drained (HTTP 402 / 50110 in content)")
)

type stickyEntry struct {
	accountID string
	expires   time.Time
}

// Pool owns account selection, in-flight accounting and cooldown state.
type Pool struct {
	mu       sync.Mutex
	store    *store.Store
	settings func() config.Settings
	inflight map[string]int
	sticky   map[string]stickyEntry
	// activePool is the working set the scheduler draws from ("使用池"). It is
	// advisory state, rebuilt lazily after a restart: pick() tops it up from
	// the reserve by highest known credit whenever it runs short. Members are
	// evicted when they turn exhausted, disabled or invalid; a member in a
	// transient cooldown stays a member and merely waits it out.
	activePool map[string]time.Time
	cursor     int
}

func New(st *store.Store, settings func() config.Settings) *Pool {
	return &Pool{
		store:      st,
		settings:   settings,
		inflight:   make(map[string]int),
		sticky:     make(map[string]stickyEntry),
		activePool: make(map[string]time.Time),
	}
}

// Lease is one borrowed account. Release must be called exactly once.
type Lease struct {
	Account *store.Account
	pool    *Pool
	once    sync.Once
}

// Release returns the account to the pool and applies health bookkeeping.
func (l *Lease) Release(err error) {
	l.once.Do(func() {
		l.pool.release(l.Account.ID, err)
	})
}

// Inflight reports the current in-flight count for an account.
func (p *Pool) Inflight(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[id]
}

// Snapshot returns a copy of the in-flight counters.
func (p *Pool) Snapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.inflight))
	for key, value := range p.inflight {
		out[key] = value
	}
	return out
}

// Acquire borrows an account from the requested pool ("" = personal,
// "gongyi" = public welfare), waiting up to the configured capacity timeout.
func (p *Pool) Acquire(ctx context.Context, sessionKey, group string) (*Lease, error) {
	settings := p.settings()
	deadline := time.Now().Add(settings.CapacityWait())

	for {
		account, wait := p.pick(sessionKey, group)
		if account != nil {
			return &Lease{Account: account, pool: p}, nil
		}
		if time.Now().After(deadline) {
			return nil, wait
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// matchPool enforces the hard boundary: public keys only see gongyi accounts,
// personal keys never see them. No cross-pool fallback.
func matchPool(account *store.Account, group string) bool {
	if group == "gongyi" {
		return account.Gongyi
	}
	return !account.Gongyi
}

// pick selects a routable account and reserves an in-flight slot.
func (p *Pool) pick(sessionKey, group string) (*store.Account, error) {
	// Read the store and settings before taking p.mu. Keeping every cross
	// component call outside the critical section means p.mu can never take
	// part in a lock-order cycle with store.mu.
	accounts := p.store.ListAccounts()
	if len(accounts) == 0 {
		return nil, ErrNoAccount
	}
	settings := p.settings()

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	stickyKey := group + "|" + sessionKey

	// Sticky sessions reuse their previous account while it stays routable.
	if sessionKey != "" && settings.StickyTTL() > 0 {
		if entry, ok := p.sticky[stickyKey]; ok {
			if now.After(entry.expires) {
				delete(p.sticky, stickyKey)
			} else {
				for _, account := range accounts {
					if account.ID == entry.accountID && matchPool(account, group) &&
						routable(account, p.inflight[account.ID], now, settings) {
						p.inflight[account.ID]++
						entry.expires = now.Add(settings.StickyTTL())
						p.sticky[stickyKey] = entry
						return account, nil
					}
				}
			}
		}
	}

	candidates := make([]*store.Account, 0, len(accounts))
	for _, account := range accounts {
		if !matchPool(account, group) {
			continue
		}
		if routable(account, p.inflight[account.ID], now, settings) {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoCapacity
	}

	// The public pool is small and dedicated; bypass the shared working set so
	// a public key can never consume a personal account and vice versa.
	if group != "" {
		chosen := p.selectLocked(candidates, settings)
		p.inflight[chosen.ID]++
		if sessionKey != "" && settings.StickyTTL() > 0 {
			p.sticky[stickyKey] = stickyEntry{accountID: chosen.ID, expires: now.Add(settings.StickyTTL())}
		}
		return chosen, nil
	}

	poolSize := settings.ActivePoolSize()
	if poolSize > 0 {
		p.pruneWorkingSetLocked(accounts)
	}

	var chosen *store.Account
	switch {
	case poolSize <= 0:
		// Legacy mode: schedule across the whole pool.
		chosen = p.selectLocked(candidates, settings)
	default:
		working := make([]*store.Account, 0, len(candidates))
		for _, account := range candidates {
			if _, ok := p.activePool[account.ID]; ok {
				working = append(working, account)
			}
		}
		if len(working) > 0 {
			chosen = p.selectLocked(working, settings)
		} else {
			// The working set has nothing usable: backfill with the reserve
			// account of highest credit and use it for this request.
			chosen = p.admitBestLocked(candidates, now)
			if chosen == nil {
				chosen = p.selectLocked(candidates, settings)
			}
		}
		// Keep the working set at its configured size; after a restart this is
		// what rebuilds it.
		p.topUpWorkingSetLocked(candidates, now, poolSize)
	}

	p.inflight[chosen.ID]++
	if sessionKey != "" && settings.StickyTTL() > 0 {
		p.sticky[stickyKey] = stickyEntry{accountID: chosen.ID, expires: now.Add(settings.StickyTTL())}
	}
	return chosen, nil
}

// selectLocked applies the configured strategy within the given candidate
// list. The list is expected to be small (a working set) or already ordered;
// the stable sort keeps legacy whole-pool behaviour intact.
func (p *Pool) selectLocked(candidates []*store.Account, settings config.Settings) *store.Account {
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if settings.Routing.PreferIdle && p.inflight[left.ID] != p.inflight[right.ID] {
			return p.inflight[left.ID] < p.inflight[right.ID]
		}
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})

	switch settings.Routing.Strategy {
	case "round_robin":
		p.cursor = (p.cursor + 1) % len(candidates)
		return candidates[p.cursor]
	case "random":
		return candidates[rand.Intn(len(candidates))]
	case "priority":
		return candidates[0]
	default: // least_inflight
		chosen := candidates[0]
		for _, candidate := range candidates[1:] {
			if p.inflight[candidate.ID] < p.inflight[chosen.ID] {
				chosen = candidate
			}
		}
		return chosen
	}
}

// pruneWorkingSetLocked drops working-set members that can no longer serve at
// all — exhausted, disabled, invalid or removed. A member in a transient
// cooldown stays a member: it comes back on its own when the window elapses.
func (p *Pool) pruneWorkingSetLocked(accounts []*store.Account) {
	byID := make(map[string]*store.Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	for id := range p.activePool {
		account, ok := byID[id]
		if !ok || !account.Enabled || account.Gongyi ||
			account.Status == store.StatusExhausted ||
			account.Status == store.StatusDisabled ||
			account.Status == store.StatusInvalid {
			delete(p.activePool, id)
		}
	}
}

// admitBestLocked admits the reserve candidate with the highest known credit
// into the working set and returns it. The operator's priority breaks credit
// ties — it is the explicit lever for exactly this choice; accounts equal on
// both resolve randomly, so a cohort of fresh equal-credit accounts wears
// evenly instead of always leaning on one. Returns nil when the reserve is
// empty.
func (p *Pool) admitBestLocked(candidates []*store.Account, now time.Time) *store.Account {
	var ties []*store.Account
	bestCredit, bestPriority := 0, 0
	seen := false
	for _, account := range candidates {
		if _, ok := p.activePool[account.ID]; ok {
			continue
		}
		credit := creditOf(account)
		better := !seen || credit > bestCredit ||
			(credit == bestCredit && account.Priority < bestPriority)
		equal := seen && credit == bestCredit && account.Priority == bestPriority
		switch {
		case better:
			seen = true
			bestCredit, bestPriority = credit, account.Priority
			ties = ties[:0]
			ties = append(ties, account)
		case equal:
			ties = append(ties, account)
		}
	}
	if len(ties) == 0 {
		return nil
	}
	chosen := ties[rand.Intn(len(ties))]
	p.activePool[chosen.ID] = now
	return chosen
}

// topUpWorkingSetLocked grows the working set to its configured size with the
// highest-credit reserve candidates.
func (p *Pool) topUpWorkingSetLocked(candidates []*store.Account, now time.Time, poolSize int) {
	for len(p.activePool) < poolSize {
		if p.admitBestLocked(candidates, now) == nil {
			return
		}
	}
}

// creditOf ranks an account by its last known balance; an account that has
// never been probed ranks below any probed one. The reading is advisory — the
// upstream stays the final authority, and a wrong guess self-corrects on the
// next 402.
func creditOf(account *store.Account) int {
	if account.Credit == nil {
		return -1
	}
	return account.Credit.Total
}

func routable(account *store.Account, inflight int, now time.Time, settings config.Settings) bool {
	if !account.Enabled {
		return false
	}
	switch account.Status {
	case store.StatusDisabled, store.StatusInvalid, store.StatusExhausted:
		return false
	}
	if account.Status == store.StatusCooldown && now.Before(account.CooldownUntil) {
		return false
	}
	// A token without a fingerprint cannot be replayed: the yy signature is
	// computed over the fingerprint query string, so an account missing either
	// half is unusable rather than merely degraded.
	if account.Token == "" || account.UUID == "" || account.DeviceID == "" {
		return false
	}
	// An account that has not been prepared is worse than unusable, because of
	// how it fails.
	//
	// Without the realUserID every signed call answers a bare 401 — the same 401
	// a dead token produces — and this pool retires an account that reports one.
	// So routing an unprepared account would delete a healthy account from the
	// pool. Without the agent id the session handshake answers 200 and opens
	// nothing, which burns a request to learn nothing.
	//
	// Both are filled in when the account is added or probed; an account that
	// still lacks them is held out of rotation rather than handed a request that
	// can only produce a false verdict.
	if account.UserID == "" || account.UserID == "0" || account.AgentID == "" {
		return false
	}
	// A spent account is held out of rotation, but only while the reading is
	// recent. Past CreditFresh the balance counts as unknown and the account is
	// scheduled again: a day-old zero would otherwise strand capacity that a
	// single request refills, and the upstream remains the final authority on
	// whether a request is affordable.
	if settings.Signin.SkipZeroCredit && account.Credit.Exhausted() &&
		now.Sub(account.Credit.SyncedAt) < settings.CreditFresh() {
		return false
	}
	limit := account.MaxConcurrent
	if limit <= 0 {
		limit = 1
	}
	return inflight < limit
}

// release applies health bookkeeping and clears the in-flight slot.
func (p *Pool) release(id string, err error) {
	p.mu.Lock()
	if p.inflight[id] > 0 {
		p.inflight[id]--
	}
	p.mu.Unlock()

	settings := p.settings()
	p.store.SaveAccountState(id, func(account *store.Account) {
		account.LastUsedAt = time.Now()
		if err == nil {
			account.SuccessCount++
			account.FailCount = 0
			if account.Status == store.StatusCooldown {
				account.Status = store.StatusActive
				account.CooldownUntil = time.Time{}
			}
			if account.Status == store.StatusActive {
				account.LastError = ""
			}
			return
		}

		account.FailCount++
		account.LastError = truncate(err.Error(), 240)

		if errors.Is(err, ErrQuotaDrained) {
			// The account is not transiently broken, its balance is spent. It
			// leaves the working set and the rotation outright — there is no
			// timed recovery to wait out. It comes back when a credit reading
			// above zero lands (check-in payout, top-up, manual refresh), or
			// when the exhausted sweep re-runs its check-in after the
			// configured interval. A later probe or credit refresh rewrites
			// Credit either way.
			account.Status = store.StatusExhausted
			account.ExhaustedAt = time.Now()
			account.CooldownUntil = time.Time{}
			delete(p.activePool, id)
			return
		}

		if errors.Is(err, minimax.ErrInvalidCredential) {
			account.Status = store.StatusInvalid
			account.CooldownUntil = time.Time{}
			return
		}

		base := settings.CooldownBase()
		maxCooldown := settings.CooldownMax()
		backoff := time.Duration(float64(base) * math.Pow(2, float64(min(account.FailCount-1, 6))))
		if backoff > maxCooldown {
			backoff = maxCooldown
		}
		account.Status = store.StatusCooldown
		account.CooldownUntil = time.Now().Add(backoff)
	})
}

// ClearCooldown resets a cooling account so it can be scheduled again. It
// doubles as the manual revive for an exhausted account: the operator (or the
// console button) is asserting the balance question is settled.
func (p *Pool) ClearCooldown(id string) {
	p.mu.Lock()
	delete(p.activePool, id)
	p.mu.Unlock()
	p.store.SaveAccountState(id, func(account *store.Account) {
		account.Status = store.StatusActive
		account.CooldownUntil = time.Time{}
		account.ExhaustedAt = time.Time{}
		account.FailCount = 0
		account.LastError = ""
	})
}

// DropSticky forgets the sticky binding for a session (both pools).
func (p *Pool) DropSticky(sessionKey string) {
	if sessionKey == "" {
		return
	}
	p.mu.Lock()
	delete(p.sticky, "|"+sessionKey)
	delete(p.sticky, "gongyi|"+sessionKey)
	for key := range p.sticky {
		if strings.HasSuffix(key, "|"+sessionKey) {
			delete(p.sticky, key)
		}
	}
	p.mu.Unlock()
}

// Summary reports pool health for dashboards and /health.
func (p *Pool) Summary() (total, active, cooldown, disabled, invalid, exhausted, routableCount int) {
	accounts := p.store.ListAccounts()
	settings := p.settings()
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range accounts {
		total++
		// Enabled is checked first because the two mutation paths disagree:
		// UpdateAccount mirrors Enabled into Status, while SaveAccountState does
		// not. An account switched off through the latter would otherwise still
		// be reported as active on the dashboard.
		switch {
		case !account.Enabled, account.Status == store.StatusDisabled:
			disabled++
		case account.Status == store.StatusInvalid:
			invalid++
		case account.Status == store.StatusExhausted:
			exhausted++
		case account.Status == store.StatusCooldown && now.Before(account.CooldownUntil):
			// Only a cooldown that is still running counts as cooling. Status is
			// a snapshot written when the failure happened, so it keeps saying
			// cooldown long after the window elapsed; counting it here would
			// report a pool of 0 active accounts while every one of them is in
			// fact schedulable again.
			cooldown++
		default:
			active++
		}
		if routable(account, p.inflight[account.ID], now, settings) {
			routableCount++
		}
	}
	return
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	// Cut on a rune boundary. Slicing at a fixed byte offset can split a
	// multi-byte character, which would push invalid UTF-8 into lastError and
	// from there into the audit log and the console.
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

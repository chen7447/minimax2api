package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minimax2api/internal/config"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrAlreadyExists = errors.New("already exists")
)

const (
	stateFile     = "app.json"
	pbkdfRounds   = 120000
	sessionTTL    = 12 * time.Hour
	schemaVersion = 1
)

// Store keeps the whole application state in memory and persists it through a
// single background writer. Mutations never touch the disk while holding the
// lock, so a slow or blocked filesystem cannot stall request handling.
//
// Settings is additionally mirrored into an atomic snapshot so callers can read
// it even while they hold mu. This matters because callbacks passed into
// mutation helpers (SaveAccountState, UpdateAccount, ...) run under the write
// lock and legitimately need settings; a plain RWMutex would deadlock there
// since Go's sync.RWMutex is not reentrant.
type Store struct {
	mu       sync.RWMutex
	path     string
	dataDir  string
	state    State
	sessions map[string]time.Time
	onChange []func()

	settings atomic.Value // config.Settings

	// slotSeed memoizes SigninSlotSeed; see there.
	slotSeedOnce sync.Once
	slotSeed     string

	// closeOnce makes Close idempotent; see there.
	closeOnce sync.Once

	dirty  chan struct{}
	flush  chan chan error
	closed chan struct{}
	wg     sync.WaitGroup
}

// Open loads (or initialises) the persistent store inside dataDir.
func Open(dataDir, adminUser, adminKey string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	store := &Store{
		path:     filepath.Join(dataDir, stateFile),
		dataDir:  dataDir,
		sessions: make(map[string]time.Time),
		dirty:    make(chan struct{}, 1),
		flush:    make(chan chan error),
		closed:   make(chan struct{}),
	}

	fresh := false
	raw, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &store.state); err != nil {
			return nil, fmt.Errorf("parse %s: %w", store.path, err)
		}
	case os.IsNotExist(err):
		fresh = true
	default:
		return nil, fmt.Errorf("read %s: %w", store.path, err)
	}

	if fresh {
		store.state.Settings = config.DefaultSettings(dataDir)
	}
	store.state.Version = schemaVersion
	// A repair has to be written back, not just applied in memory: leaving the
	// file describing a value the process is not using is exactly how a broken
	// default survived a fix to the default itself. The write is queued here and
	// picked up by the loop started below.
	if store.state.Settings.Normalize(dataDir) {
		store.markDirtyLocked()
	}
	store.settings.Store(store.state.Settings)
	if len(store.state.Models) == 0 {
		store.state.Models = BuiltinModels()
	} else if merged, changed := MergeBuiltinModels(store.state.Models); changed {
		// A catalogue that predates a built-in entry has to be written back for
		// the same reason a repaired setting does: leaving the file describing a
		// shorter list than the process is serving means the next release has to
		// merge it again, and the file never becomes an accurate description of
		// what is running.
		store.state.Models = merged
		store.markDirtyLocked()
	}
	if store.state.Admin.Username == "" {
		if adminUser == "" {
			adminUser = "admin"
		}
		key := adminKey
		generated := false
		if key == "" {
			key = "mm2api-" + randomToken(16)
			generated = true
		}
		salt := randomToken(16)
		store.state.Admin = Admin{Username: adminUser, Salt: salt, KeyHash: hashPassword(key, salt)}
		store.state.Settings.Server.AdminUsername = adminUser
		if err := store.writeSnapshotLocked(); err != nil {
			return nil, err
		}
		if generated {
			fmt.Printf("\n  ⚠ 已生成初始管理员密钥：%s\n  管理台登录与 /admin/api/* 都用这一把 key，请立即在「设置」里改掉并留底。\n\n", key)
		}
	}
	if err := os.MkdirAll(store.state.Settings.Media.GeneratedDir, 0o755); err != nil {
		return nil, fmt.Errorf("create media dir: %w", err)
	}

	store.wg.Add(1)
	go store.loop()
	return store, nil
}

// loop coalesces mutation signals into disk writes.
func (s *Store) loop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.dirty:
			// Debounce so a burst of mutations results in a single write.
			time.Sleep(40 * time.Millisecond)
			select {
			case <-s.dirty:
			default:
			}
			if err := s.writeSnapshot(); err != nil {
				log.Printf("持久化失败: %v", err)
			}
		case reply := <-s.flush:
			reply <- s.writeSnapshot()
		case <-s.closed:
			return
		}
	}
}

// writeSnapshot marshals under a read lock and performs file I/O outside of it.
func (s *Store) writeSnapshot() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, raw)
}

// writeSnapshotLocked is only used during bootstrap, before the writer starts.
func (s *Store) writeSnapshotLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, raw)
}

func writeFileAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) DataDir() string { return s.dataDir }

// OnChange registers a callback fired after every successful mutation.
func (s *Store) OnChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = append(s.onChange, fn)
}

// unlockAndNotify releases mu and then runs the registered change callbacks.
// Callbacks are invoked only after the lock is dropped so they may safely
// re-enter the store: Go's sync.RWMutex is not reentrant, so a callback that
// reads settings while the write lock is still held would deadlock forever.
func (s *Store) unlockAndNotify() {
	callbacks := make([]func(), len(s.onChange))
	copy(callbacks, s.onChange)
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

// markDirtyLocked schedules a background write. It never blocks.
func (s *Store) markDirtyLocked() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Save flushes the current state to disk synchronously.
func (s *Store) Save() error {
	s.mu.Lock()
	callbacks := make([]func(), len(s.onChange))
	copy(callbacks, s.onChange)
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}

	reply := make(chan error, 1)
	select {
	case s.flush <- reply:
		return <-reply
	case <-s.closed:
		return errors.New("store closed")
	}
}

// Close performs a final flush and stops the background writer. Safe to call
// twice: a second call returns after the first one's flush, rather than
// panicking on the already-closed channel — a restart path that closes an old
// handle and a shutdown hook may both get there.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		// Blocking send on purpose. The writer is either selecting — it takes
		// this immediately — or mid-write, in which case it takes it as soon as
		// that write returns. The old non-blocking send dropped the final flush
		// whenever the writer happened to be busy, which is exactly how a
		// completed sweep's stamp (and a fresh gallery entry) could vanish on
		// restart, and why two tests failed only under load.
		reply := make(chan error, 1)
		s.flush <- reply
		<-reply
		close(s.closed)
	})
	s.wg.Wait()
	return nil
}

// ---------------------------------------------------------------- admin auth

func hashPassword(password, salt string) string {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(password))
	sum := mac.Sum(nil)
	for i := 1; i < pbkdfRounds; i++ {
		mac = hmac.New(sha256.New, sum)
		mac.Write([]byte(password + salt))
		sum = mac.Sum(nil)
	}
	return hex.EncodeToString(sum)
}

func randomToken(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// VerifyKey reports whether key is the admin key. There is deliberately no
// username argument: one secret gates both the console and the admin API.
func (s *Store) VerifyKey(key string) bool {
	if key == "" {
		return false
	}
	s.mu.RLock()
	admin := s.state.Admin
	s.mu.RUnlock()
	if admin.KeyHash == "" || admin.Salt == "" {
		return false
	}
	expected := hashPassword(key, admin.Salt)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(admin.KeyHash)) == 1
}

// SetKey replaces the admin key. Sessions are not touched here; a caller that
// wants them dropped (the change-key handler) revokes them explicitly.
func (s *Store) SetKey(key string) error {
	if len(key) < 8 {
		return errors.New("密钥至少 8 位")
	}
	s.mu.Lock()
	defer s.unlockAndNotify()
	salt := randomToken(16)
	s.state.Admin.Salt = salt
	s.state.Admin.KeyHash = hashPassword(key, salt)
	s.markDirtyLocked()
	return nil
}

func (s *Store) AdminProfile() Admin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Admin
}

// SigninSlotSeed returns the seed the randomized check-in slot is derived from,
// issuing and persisting it on first use.
//
// Installs that predate the feature have no seed in their settings; baking
// issuance into boot would touch the disk on a read path, so the first call
// settles it: the admin salt where one exists (already a stable per-install
// random value), a fresh random token otherwise. The chosen value is written
// back into the settings so it survives the store file, not just the process —
// a restart must not move today's slot, and the scheduler calls this from
// whatever process happens to be running.
func (s *Store) SigninSlotSeed() string {
	s.slotSeedOnce.Do(func() {
		seed := s.Settings().Signin.SlotSeed
		if seed == "" {
			if salt := s.AdminProfile().Salt; salt != "" {
				seed = salt
			} else {
				seed = randomToken(16)
			}
			_ = s.UpdateSettings(func(c *config.Settings) {
				c.Signin.SlotSeed = seed
			})
		}
		s.slotSeed = seed
	})
	return s.slotSeed
}

// SigninSweptAt reports when the daily sweep last completed, if ever.
func (s *Store) SigninSweptAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SigninSweptAt
}

// MarkSigninSwept records the completion of a full-pool sweep. The stamp drives
// the scheduler's catch-up decision, so it must land on disk: a stamp that only
// lived in memory would let a restart re-sweep an already served day, which is
// harmless once a day but a full second walk of the pool every boot.
func (s *Store) MarkSigninSwept(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SigninSweptAt = at
	s.markDirtyLocked()
}

// IssueSession creates an admin session token.
func (s *Store) IssueSession() (string, time.Time) {
	token := randomToken(24)
	expires := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.sessions[token] = expires
	s.mu.Unlock()
	return token, expires
}

func (s *Store) ValidateSession(token string) bool {
	if token == "" {
		return false
	}
	s.mu.RLock()
	expires, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok || time.Now().After(expires) {
		return false
	}
	return true
}

func (s *Store) RevokeSession(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *Store) RevokeAllSessions() {
	s.mu.Lock()
	s.sessions = make(map[string]time.Time)
	s.mu.Unlock()
}

// ------------------------------------------------------------------ settings

// Settings returns the current runtime configuration. It reads an immutable
// atomic snapshot rather than taking mu, so it is safe to call from inside
// callbacks that already run under the write lock.
func (s *Store) Settings() config.Settings {
	if value := s.settings.Load(); value != nil {
		return value.(config.Settings)
	}
	// Only reachable before Open finishes priming the snapshot.
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.Settings
}

func (s *Store) UpdateSettings(apply func(*config.Settings)) error {
	s.mu.Lock()
	apply(&s.state.Settings)
	s.state.Settings.Normalize(s.dataDir)
	snapshot := s.state.Settings
	if err := os.MkdirAll(snapshot.Media.GeneratedDir, 0o755); err != nil {
		s.mu.Unlock()
		return err
	}
	s.markDirtyLocked()
	// Publish inside the lock so a callback reading settings sees the new value.
	s.settings.Store(snapshot)
	defer s.unlockAndNotify()
	return nil
}

// ------------------------------------------------------------------ accounts

// MaskToken renders a token safe for display.
//
// A JWT's first bytes are its base64 header, which is identical for every token
// issued by the same service, so the prefix carries no information. The tail is
// the part that actually differs between accounts, which is why most of the
// visible characters are taken from the end.
func MaskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 20 {
		return "••••••"
	}
	return token[:6] + "…" + token[len(token)-10:]
}

// clone returns a deep copy of an account.
//
// The store owns the live account objects and mutates them in place under s.mu.
// Handing those pointers to callers would let them read fields without holding
// the lock, which races with SaveAccountState and friends. Copying is cheap
// here because Account holds only value types apart from Quota.
func (a *Account) clone() *Account {
	if a == nil {
		return nil
	}
	out := *a
	if a.Quota != nil {
		quota := *a.Quota
		out.Quota = &quota
	}
	// Credit and SigninPanel are pointers, so the shallow copy above would let a
	// caller mutate the store's own struct without holding the lock — and the
	// panel's slice would share a backing array, so an append on either side
	// could scribble over the other's view.
	if a.Credit != nil {
		credit := *a.Credit
		out.Credit = &credit
	}
	if a.SigninPanel != nil {
		panel := *a.SigninPanel
		panel.Days = append([]SigninDay(nil), a.SigninPanel.Days...)
		out.SigninPanel = &panel
	}
	return &out
}

// ListAccounts returns a detached snapshot of every account. Callers may read
// and even mutate the result without holding any lock and without affecting the
// store; all writes must go through the mutation helpers.
func (s *Store) ListAccounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.state.Accounts))
	for _, account := range s.state.Accounts {
		out = append(out, account.clone())
	}
	return out
}

func (s *Store) AccountByID(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, account := range s.state.Accounts {
		if account.ID == id {
			return account.clone(), true
		}
	}
	return nil, false
}

func (s *Store) AddAccount(account *Account) error {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for _, existing := range s.state.Accounts {
		if existing.Token != "" && existing.Token == account.Token {
			return ErrAlreadyExists
		}
	}
	if account.ID == "" {
		account.ID = "acc_" + randomToken(8)
	}
	if account.Kind == "" {
		account.Kind = KindToken
	}
	if account.Region == "" {
		account.Region = RegionGlobal
	}
	if account.Priority <= 0 {
		account.Priority = 50
	}
	if account.MaxConcurrent <= 0 {
		account.MaxConcurrent = 2
	}
	account.Status = StatusActive
	now := time.Now()
	account.CreatedAt = now
	account.UpdatedAt = now
	s.state.Accounts = append(s.state.Accounts, account)
	s.markDirtyLocked()
	return nil
}

func (s *Store) UpdateAccount(id string, apply func(*Account)) (*Account, error) {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for _, account := range s.state.Accounts {
		if account.ID != id {
			continue
		}
		apply(account)
		account.UpdatedAt = time.Now()
		if !account.Enabled {
			account.Status = StatusDisabled
		} else if account.Status == StatusDisabled || account.Status == StatusExhausted {
			// Re-enabling (or explicitly re-saving) an exhausted account is a
			// manual revive: the operator is saying the balance question is
			// settled, so the marker goes with the status.
			account.Status = StatusActive
			account.ExhaustedAt = time.Time{}
		}
		s.markDirtyLocked()
		return account, nil
	}
	return nil, ErrNotFound
}

func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for index, account := range s.state.Accounts {
		if account.ID != id {
			continue
		}
		s.state.Accounts = append(s.state.Accounts[:index], s.state.Accounts[index+1:]...)
		s.markDirtyLocked()
		return nil
	}
	return ErrNotFound
}

// DeleteAccounts removes every account whose id is listed and returns the count.
func (s *Store) DeleteAccounts(ids []string) (int, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.unlockAndNotify()
	kept := make([]*Account, 0, len(s.state.Accounts))
	deleted := 0
	for _, account := range s.state.Accounts {
		if _, ok := wanted[account.ID]; ok {
			deleted++
			continue
		}
		kept = append(kept, account)
	}
	if deleted == 0 {
		return 0, nil
	}
	s.state.Accounts = kept
	s.markDirtyLocked()
	return deleted, nil
}

// DeleteAccountsByStatus removes accounts in any of the given states.
func (s *Store) DeleteAccountsByStatus(statuses []string) (int, error) {
	wanted := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		wanted[status] = struct{}{}
	}
	s.mu.Lock()
	defer s.unlockAndNotify()
	kept := make([]*Account, 0, len(s.state.Accounts))
	deleted := 0
	for _, account := range s.state.Accounts {
		if _, ok := wanted[account.Status]; ok {
			deleted++
			continue
		}
		kept = append(kept, account)
	}
	if deleted == 0 {
		return 0, nil
	}
	s.state.Accounts = kept
	s.markDirtyLocked()
	return deleted, nil
}

// UpdateAccounts applies a mutation to every id and persists once.
func (s *Store) UpdateAccounts(ids []string, apply func(*Account)) (int, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.unlockAndNotify()
	updated := 0
	now := time.Now()
	for _, account := range s.state.Accounts {
		if _, ok := wanted[account.ID]; !ok {
			continue
		}
		apply(account)
		account.UpdatedAt = now
		if !account.Enabled {
			account.Status = StatusDisabled
		} else if account.Status == StatusDisabled || account.Status == StatusExhausted {
			// Re-enabling (or explicitly re-saving) an exhausted account is a
			// manual revive: the operator is saying the balance question is
			// settled, so the marker goes with the status.
			account.Status = StatusActive
			account.ExhaustedAt = time.Time{}
		}
		updated++
	}
	if updated == 0 {
		return 0, nil
	}
	s.markDirtyLocked()
	return updated, nil
}

// SaveAccountState persists mutable runtime fields of one account.
func (s *Store) SaveAccountState(id string, apply func(*Account)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.state.Accounts {
		if account.ID == id {
			apply(account)
			break
		}
	}
	s.markDirtyLocked()
}

func (s *Store) AccountGroups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	for _, account := range s.state.Accounts {
		if account.Group == "" {
			continue
		}
		if _, ok := seen[account.Group]; ok {
			continue
		}
		seen[account.Group] = struct{}{}
		out = append(out, account.Group)
	}
	sort.Strings(out)
	return out
}

// --------------------------------------------------------------- client keys

func (s *Store) ListClientKeys() []*ClientKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ClientKey, len(s.state.ClientKeys))
	copy(out, s.state.ClientKeys)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) ClientKeyByValue(value string) (*ClientKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range s.state.ClientKeys {
		if key.Key == value {
			return key, true
		}
	}
	return nil, false
}

func (s *Store) CreateClientKey(name string, rpm, maxConcurrent int) (*ClientKey, error) {
	return s.CreateClientKeyWithPool(name, rpm, maxConcurrent, "")
}

// CreateClientKeyWithPool creates a key bound to an account pool
// ("" = personal, "gongyi" = public welfare).
func (s *Store) CreateClientKeyWithPool(name string, rpm, maxConcurrent int, pool string) (*ClientKey, error) {
	s.mu.Lock()
	defer s.unlockAndNotify()
	key := &ClientKey{
		ID:            "key_" + randomToken(6),
		Name:          name,
		Key:           "sk-mm-" + randomToken(20),
		Enabled:       true,
		RPMLimit:      rpm,
		MaxConcurrent: maxConcurrent,
		Pool:          normalizePool(pool),
		CreatedAt:     time.Now(),
	}
	s.state.ClientKeys = append(s.state.ClientKeys, key)
	s.markDirtyLocked()
	return key, nil
}

// normalizePool collapses unknown pool names to "".
func normalizePool(pool string) string {
	if strings.EqualFold(strings.TrimSpace(pool), "gongyi") {
		return "gongyi"
	}
	return ""
}

// SetGongyiBatch marks accounts as public-pool members in one write. replace
// clears the flag on every account not in ids.
func (s *Store) SetGongyiBatch(ids map[string]bool, replace bool) int {
	s.mu.Lock()
	defer s.unlockAndNotify()
	n := 0
	for _, account := range s.state.Accounts {
		if replace {
			account.Gongyi = ids[account.ID]
		} else if ids[account.ID] {
			account.Gongyi = true
		}
		if account.Gongyi {
			n++
		}
	}
	s.markDirtyLocked()
	return n
}

// GongyiAccounts returns a detached snapshot of public-pool accounts.
func (s *Store) GongyiAccounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, 64)
	for _, account := range s.state.Accounts {
		if account.Gongyi {
			out = append(out, account.clone())
		}
	}
	return out
}

// SetClientKeyPool binds a client key to a pool.
func (s *Store) SetClientKeyPool(id, pool string) bool {
	_, err := s.UpdateClientKey(id, func(key *ClientKey) {
		key.Pool = normalizePool(pool)
	})
	return err == nil
}

func (s *Store) UpdateClientKey(id string, apply func(*ClientKey)) (*ClientKey, error) {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for _, key := range s.state.ClientKeys {
		if key.ID != id {
			continue
		}
		apply(key)
		s.markDirtyLocked()
		return key, nil
	}
	return nil, ErrNotFound
}

func (s *Store) DeleteClientKey(id string) error {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for index, key := range s.state.ClientKeys {
		if key.ID != id {
			continue
		}
		s.state.ClientKeys = append(s.state.ClientKeys[:index], s.state.ClientKeys[index+1:]...)
		s.markDirtyLocked()
		return nil
	}
	return ErrNotFound
}

// BumpClientKeyUsage records usage without forcing a synchronous disk write on
// the request path.
func (s *Store) BumpClientKeyUsage(id string) {
	s.mu.Lock()
	for _, key := range s.state.ClientKeys {
		if key.ID == id {
			key.TotalRequests++
			key.LastUsedAt = time.Now()
			break
		}
	}
	s.mu.Unlock()
}

// -------------------------------------------------------------------- models

// ListModels returns the visible catalogue in console order: entries the last
// refresh added first, ordinary entries next, unavailable entries last.
//
// The order is the console's lifecycle made visible: a new model appears at the
// top with a badge, and a model the upstream stopped serving drops to the
// bottom, struck through, until the next refresh removes it. Removed entries are
// tombstones and never appear.
func (s *Store) ListModels() []*ModelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ModelConfig, 0, len(s.state.Models))
	for _, model := range s.state.Models {
		if model == nil || model.Removed {
			continue
		}
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool {
		group := func(model *ModelConfig) int {
			switch {
			case model.New:
				return 0
			case model.Unavailable:
				return 2
			default:
				return 1
			}
		}
		gi, gj := group(out[i]), group(out[j])
		if gi != gj {
			return gi < gj
		}
		if gi == 2 {
			// Unavailable entries keep their previous relative order.
			return false
		}
		// Within a group, follow the upstream's own model order. Unranked
		// entries (the registry does not list them) come after ranked ones.
		const unranked = int(^uint(0) >> 1)
		ri, rj := out[i].Rank, out[j].Rank
		if ri == 0 {
			ri = unranked
		}
		if rj == 0 {
			rj = unranked
		}
		return ri < rj
	})
	return out
}

// ModelByID finds a live catalogue entry. Removed entries are invisible; an
// unavailable one is still returned so the caller can refuse it with a message
// that explains why, instead of "unknown model".
func (s *Store) ModelByID(id string) (*ModelConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, model := range s.state.Models {
		if model.ID == id && !model.Removed {
			return model, true
		}
	}
	return nil, false
}

func (s *Store) UpdateModel(id string, apply func(*ModelConfig)) (*ModelConfig, error) {
	s.mu.Lock()
	defer s.unlockAndNotify()
	for _, model := range s.state.Models {
		if model.ID != id {
			continue
		}
		apply(model)
		s.markDirtyLocked()
		return model, nil
	}
	return nil, ErrNotFound
}

// RecordModelUsage accumulates per-model counters.
func (s *Store) RecordModelUsage(id string, requests, tokens int64) {
	s.mu.Lock()
	for _, model := range s.state.Models {
		if model.ID == id {
			model.Requests += requests
			model.Tokens += tokens
			break
		}
	}
	s.mu.Unlock()
}

// -------------------------------------------------------------------- audits

func (s *Store) AppendAudit(audit *Audit) {
	settings := s.Settings()
	s.mu.Lock()
	s.state.Audits = append([]*Audit{audit}, s.state.Audits...)
	if len(s.state.Audits) > settings.Audit.MaxRecords {
		s.state.Audits = s.state.Audits[:settings.Audit.MaxRecords]
	}
	s.mu.Unlock()
}

func (s *Store) ListAudits() []*Audit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Audit, len(s.state.Audits))
	copy(out, s.state.Audits)
	return out
}

func (s *Store) AuditByID(id string) (*Audit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, audit := range s.state.Audits {
		if audit.ID == id {
			return audit, true
		}
	}
	return nil, false
}

func (s *Store) ClearAudits() error {
	s.mu.Lock()
	defer s.unlockAndNotify()
	s.state.Audits = nil
	s.markDirtyLocked()
	return nil
}

// PurgeAudits drops records older than the retention window.
func (s *Store) PurgeAudits() {
	settings := s.Settings()
	if settings.Audit.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-settings.Retention())
	s.mu.Lock()
	kept := make([]*Audit, 0, len(s.state.Audits))
	for _, audit := range s.state.Audits {
		if audit.CreatedAt.After(cutoff) {
			kept = append(kept, audit)
		}
	}
	s.state.Audits = kept
	s.mu.Unlock()
}

// --------------------------------------------------------------------- media

func (s *Store) AddMedia(item *MediaItem) {
	s.mu.Lock()
	defer s.unlockAndNotify()
	s.state.Media = append([]*MediaItem{item}, s.state.Media...)
	if len(s.state.Media) > 500 {
		s.state.Media = s.state.Media[:500]
	}
	// Without this the entry only reaches disk when some other mutation happens
	// to mark the state dirty — a gallery entry can outlive its request but not
	// a restart, and the file it names becomes unreachable from the console.
	s.markDirtyLocked()
}

// DeleteMedia removes the listed entries and returns what was actually removed,
// so the caller can delete the files those entries name. Unknown ids are
// ignored: deleting twice, or deleting from two tabs at once, must not fail.
func (s *Store) DeleteMedia(ids []string) []*MediaItem {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	s.mu.Lock()
	defer s.unlockAndNotify()
	kept := make([]*MediaItem, 0, len(s.state.Media))
	removed := make([]*MediaItem, 0, len(ids))
	for _, item := range s.state.Media {
		if _, ok := wanted[item.ID]; ok {
			removed = append(removed, item)
			continue
		}
		kept = append(kept, item)
	}
	if len(removed) == 0 {
		return nil
	}
	s.state.Media = kept
	s.markDirtyLocked()
	return removed
}

func (s *Store) ListMedia() []*MediaItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MediaItem, len(s.state.Media))
	copy(out, s.state.Media)
	return out
}

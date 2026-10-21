// gongyi.go public-welfare pool: selection, credit meter, usage, and one-way
// water-level push to translate.7447.site (pool B, the daily pool).
//
// Hard boundary: the public client key only selects accounts marked Gongyi;
// personal keys never select them. The pool has no cross-pool fallback.
package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimax2api/internal/store"
)

// ---------------------------------------------------------------------------
// config + persistence (data/gongyi.json)
// ---------------------------------------------------------------------------

type gongyiConfig struct {
	Enabled   bool   `json:"enabled"`
	WorkerURL string `json:"worker_url"`
	AdminKey  string `json:"admin_key"`
	PoolID    string `json:"pool_id"`
	Capacity  int64  `json:"capacity"`
	// FullPerAccount is kept for compatibility with older gongyi.json files;
	// the water level now uses DailyGrant × ReserveDays instead.
	FullPerAccount float64 `json:"full_per_account,omitempty"`
	DailyGrant     float64 `json:"daily_grant"`
	ReserveDays    int     `json:"reserve_days"`
	Target         int     `json:"target"`
	PushMinutes    int     `json:"push_minutes"`
	AutoPush       bool    `json:"auto_push"`
	KeyID          string  `json:"key_id"`
	RefreshPerTick int     `json:"refresh_per_tick"`
	RefreshMinutes int     `json:"refresh_minutes"`
}

func gongyiDefaults() gongyiConfig {
	return gongyiConfig{
		Enabled:        false,
		WorkerURL:      "https://translate.7447.site",
		PoolID:         "B",
		Capacity:       2000,
		DailyGrant:     500,
		ReserveDays:    7,
		Target:         42,
		PushMinutes:    5,
		AutoPush:       true,
		RefreshPerTick: 2,
		RefreshMinutes: 5,
	}
}

func (c gongyiConfig) normalize() gongyiConfig {
	d := gongyiDefaults()
	c.WorkerURL = strings.TrimRight(strings.TrimSpace(c.WorkerURL), "/")
	if c.WorkerURL == "" {
		c.WorkerURL = d.WorkerURL
	}
	if c.PoolID != "A" {
		c.PoolID = "B"
	}
	if c.Capacity <= 0 {
		c.Capacity = d.Capacity
	}
	if c.FullPerAccount <= 0 {
		c.FullPerAccount = 0 // legacy field: unused; do not invent a value
	}
	if c.DailyGrant <= 0 {
		c.DailyGrant = d.DailyGrant
	}
	if c.ReserveDays <= 0 {
		c.ReserveDays = d.ReserveDays
	}
	if c.ReserveDays > 90 {
		c.ReserveDays = 90
	}
	if c.Target <= 0 {
		c.Target = d.Target
	}
	if c.Target > 1000 {
		c.Target = 1000
	}
	if c.PushMinutes <= 0 {
		c.PushMinutes = d.PushMinutes
	}
	if c.PushMinutes > 1440 {
		c.PushMinutes = 1440
	}
	if c.RefreshPerTick <= 0 {
		c.RefreshPerTick = d.RefreshPerTick
	}
	if c.RefreshPerTick > 50 {
		c.RefreshPerTick = 50
	}
	if c.RefreshMinutes <= 0 {
		c.RefreshMinutes = d.RefreshMinutes
	}
	if c.RefreshMinutes > 1440 {
		c.RefreshMinutes = 1440
	}
	return c
}

type gongyiDay struct {
	CreditUsed float64 `json:"credit_used"`
}

type gongyiPushRecord struct {
	At        string  `json:"at"`
	Pct       float64 `json:"pct"`
	Remaining int64   `json:"remaining"`
	HTTP      int     `json:"http"`
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
}

type gongyiStateFile struct {
	Config   gongyiConfig          `json:"config"`
	Baseline map[string]float64    `json:"credit_baseline"`
	Days     map[string]*gongyiDay `json:"days"`
	Pushes   []gongyiPushRecord    `json:"pushes"`
}

type gongyiState struct {
	mu   sync.Mutex
	path string
	cur  gongyiStateFile
}

func loadGongyiState(path string) *gongyiState {
	s := &gongyiState{path: path, cur: gongyiStateFile{
		Config:   gongyiDefaults(),
		Baseline: map[string]float64{},
		Days:     map[string]*gongyiDay{},
	}}
	if raw, err := os.ReadFile(path); err == nil {
		var stored gongyiStateFile
		if json.Unmarshal(raw, &stored) == nil {
			if stored.Config != (gongyiConfig{}) {
				s.cur.Config = stored.Config.normalize()
			}
			if stored.Baseline != nil {
				s.cur.Baseline = stored.Baseline
			}
			if stored.Days != nil {
				s.cur.Days = stored.Days
			}
			s.cur.Pushes = stored.Pushes
		}
	}
	return s
}

func (s *gongyiState) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.cur, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func (s *gongyiState) config() gongyiConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.Config
}

func (s *gongyiState) saveConfig(c gongyiConfig) gongyiConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Config = c.normalize()
	s.saveLocked()
	return s.cur.Config
}

func gongyiDayKey(t time.Time) string { return t.Format("2006-01-02") }

func (s *gongyiState) observeLocked(id string, credits float64, now time.Time) {
	base, ok := s.cur.Baseline[id]
	if !ok {
		s.cur.Baseline[id] = credits
		return
	}
	if credits == base {
		return
	}
	if credits < base {
		key := gongyiDayKey(now)
		day := s.cur.Days[key]
		if day == nil {
			day = &gongyiDay{}
			s.cur.Days[key] = day
		}
		day.CreditUsed += base - credits
	}
	s.cur.Baseline[id] = credits
}

func (s *gongyiState) ObserveMany(values map[string]float64) {
	if s == nil || len(values) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, credits := range values {
		s.observeLocked(id, credits, now)
	}
	cut := now.AddDate(0, 0, -90).Format("2006-01-02")
	for k := range s.cur.Days {
		if k < cut {
			delete(s.cur.Days, k)
		}
	}
	s.saveLocked()
}

func (s *gongyiState) SetBaselines(values map[string]float64) {
	if s == nil || len(values) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, credits := range values {
		s.cur.Baseline[id] = credits
	}
	s.saveLocked()
}

func (s *gongyiState) TodayCreditUsed() float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.cur.Days[gongyiDayKey(time.Now())]; d != nil {
		return d.CreditUsed
	}
	return 0
}

func (s *gongyiState) CreditSeries(days int) []map[string]any {
	if s == nil || days <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i)
		k := gongyiDayKey(d)
		var v float64
		if day := s.cur.Days[k]; day != nil {
			v = day.CreditUsed
		}
		out = append(out, map[string]any{"date": k, "credit_used": v})
	}
	return out
}

func (s *gongyiState) AddPush(rec gongyiPushRecord) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Pushes = append(s.cur.Pushes, rec)
	if len(s.cur.Pushes) > 50 {
		s.cur.Pushes = s.cur.Pushes[len(s.cur.Pushes)-50:]
	}
	s.saveLocked()
}

func (s *gongyiState) Pushes() []gongyiPushRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gongyiPushRecord, len(s.cur.Pushes))
	copy(out, s.cur.Pushes)
	return out
}

// ---------------------------------------------------------------------------
// snapshot / usage
// ---------------------------------------------------------------------------

type gongyiSnapshot struct {
	Target         int     `json:"target"`
	Total          int     `json:"total"`
	Enabled        int     `json:"enabled"`
	Cooling        int     `json:"cooling"`
	Exhausted      int     `json:"exhausted"`
	CreditsTotal   float64 `json:"credits_total"`
	DailyGrant     float64 `json:"daily_grant"`
	ReserveDays    int     `json:"reserve_days"`
	RunwayDays     float64 `json:"runway_days"`
	FullPerAccount float64 `json:"full_per_account"` // = DailyGrant × ReserveDays（兼容字段）
	Pct            float64 `json:"pct"`
	Capacity       int64   `json:"capacity"`
	Remaining      int64   `json:"remaining"`
	Gap            int     `json:"gap"`
}

// gongyiGrantsTotal returns the unexpired remaining credits for one account.
// Grants carry the real 30-day expiry; Credit.Total is an integer aggregate.
func gongyiGrantsTotal(account *store.Account) float64 {
	if account == nil {
		return 0
	}
	now := time.Now()
	if len(account.Grants) > 0 {
		var sum float64
		for _, grant := range account.Grants {
			if grant.Remaining <= 0 {
				continue
			}
			if !grant.ExpiresAt.IsZero() && grant.ExpiresAt.Before(now) {
				continue
			}
			sum += grant.Remaining
		}
		return sum
	}
	if account.Credit != nil && account.Credit.Total > 0 {
		return float64(account.Credit.Total)
	}
	return 0
}

func (a *API) gongyiSnapshot() gongyiSnapshot {
	cfg := a.gongyi.config()
	snap := gongyiSnapshot{
		Target:      cfg.Target,
		DailyGrant:  cfg.DailyGrant,
		ReserveDays: cfg.ReserveDays,
		Capacity:    cfg.Capacity,
	}
	now := time.Now()
	for _, account := range a.store.GongyiAccounts() {
		snap.Total++
		if account.Enabled && account.Status != store.StatusDisabled && account.Status != store.StatusInvalid {
			snap.Enabled++
			snap.CreditsTotal += gongyiGrantsTotal(account)
		}
		if account.Status == store.StatusCooldown && now.Before(account.CooldownUntil) {
			snap.Cooling++
		}
		if account.Status == store.StatusExhausted {
			snap.Exhausted++
		}
	}
	if snap.Target <= 0 {
		snap.Target = 42
	}
	if snap.DailyGrant <= 0 {
		snap.DailyGrant = 500
	}
	if snap.ReserveDays <= 0 {
		snap.ReserveDays = 7
	}
	if snap.Capacity <= 0 {
		snap.Capacity = 2000
	}
	snap.FullPerAccount = snap.DailyGrant * float64(snap.ReserveDays)
	perDay := float64(snap.Target) * snap.DailyGrant
	if perDay > 0 {
		snap.RunwayDays = snap.CreditsTotal / perDay
	}
	full := perDay * float64(snap.ReserveDays)
	if full > 0 {
		snap.Pct = math.Min(1, snap.CreditsTotal/full)
	}
	snap.Remaining = int64(math.Round(snap.Pct * float64(snap.Capacity)))
	snap.Gap = snap.Target - snap.Total
	if snap.Gap < 0 {
		snap.Gap = 0
	}
	return snap
}

func gongyiDisplayName(account *store.Account) string {
	if account.Name != "" {
		return account.Name
	}
	if account.Identifier != "" {
		return account.Identifier
	}
	return store.MaskToken(account.Token)
}

// gongyiKeyName returns the configured public key's display name.
func (a *API) gongyiKeyName() string {
	cfg := a.gongyi.config()
	if cfg.KeyID == "" {
		return ""
	}
	for _, key := range a.store.ListClientKeys() {
		if key.ID == cfg.KeyID {
			return key.Name
		}
	}
	return ""
}

// gongyiAuditStats aggregates today's audits for the public key (or, when no
// key is bound, for the public accounts by display name).
func (a *API) gongyiAuditStats() (requests, errors int64, promptTok, completionTok int64, avgLatency float64) {
	keyName := a.gongyiKeyName()
	names := map[string]bool{}
	for _, account := range a.store.GongyiAccounts() {
		names[gongyiDisplayName(account)] = true
	}
	today := time.Now().Format("2006-01-02")
	var latencySum int64
	var latencyN int64
	for _, audit := range a.store.ListAudits() {
		if audit.CreatedAt.Format("2006-01-02") != today {
			continue
		}
		if keyName != "" {
			if audit.KeyName != keyName {
				continue
			}
		} else if !names[audit.AccountName] {
			continue
		}
		requests++
		if audit.Status >= 400 {
			errors++
		}
		promptTok += int64(audit.PromptTokens)
		completionTok += int64(audit.CompletionTokens)
		latencySum += audit.LatencyMs
		latencyN++
	}
	if latencyN > 0 {
		avgLatency = float64(latencySum) / float64(latencyN)
	}
	return
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type gongyiSelectReq struct {
	Limit      int     `json:"limit"`
	MinCredits float64 `json:"min_credits"`
	Mode       string  `json:"mode"` // replace | add
	DryRun     bool    `json:"dry_run"`
	Refresh    *bool   `json:"refresh"`
	KeyID      string  `json:"key_id"`
	ScanLimit  int     `json:"scan_limit"`
}

type gongyiCandidate struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Region     string  `json:"region"`
	Credits    float64 `json:"credits"`
	Status     string  `json:"status"`
	LastUsedAt string  `json:"last_used_at,omitempty"`
	failed     bool
}

func (a *API) adminGongyiSelect(w http.ResponseWriter, r *http.Request) {
	var req gongyiSelectReq
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Limit <= 0 {
		req.Limit = a.gongyi.config().Target
	}
	if req.Limit > 500 {
		req.Limit = 500
	}
	if req.MinCredits <= 0 {
		req.MinCredits = 1
	}
	if req.Mode != "add" {
		req.Mode = "replace"
	}
	doRefresh := req.Refresh == nil || *req.Refresh

	type cand struct {
		account *store.Account
		credits float64
	}
	var refs []cand
	excludedOff := 0
	for _, account := range a.store.ListAccounts() {
		if !account.Enabled || account.Status == store.StatusDisabled || account.Status == store.StatusInvalid {
			excludedOff++
			continue
		}
		credits := gongyiGrantsTotal(account)
		refs = append(refs, cand{account: account, credits: credits})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].credits != refs[j].credits {
			return refs[i].credits > refs[j].credits
		}
		return refs[i].account.LastUsedAt.After(refs[j].account.LastUsedAt)
	})
	// Only refresh a bounded scan window (top cached credits). With 382
	// accounts, refreshing every one would spend minutes of upstream calls for
	// a 42-account selection.
	if req.ScanLimit <= 0 {
		req.ScanLimit = req.Limit * 3
	}
	if req.ScanLimit < req.Limit {
		req.ScanLimit = req.Limit
	}
	if req.ScanLimit > 300 {
		req.ScanLimit = 300
	}
	if len(refs) > req.ScanLimit {
		refs = refs[:req.ScanLimit]
	}

	refreshFailed := 0
	if doRefresh && a.signin != nil && len(refs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for i := range refs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				credit, err := a.signin.RefreshCredit(ctx, refs[i].account.ID)
				mu.Lock()
				defer mu.Unlock()
				if err != nil || credit == nil {
					refs[i].credits = -1
					refreshFailed++
					return
				}
				if updated, ok := a.store.AccountByID(refs[i].account.ID); ok {
					refs[i].credits = gongyiGrantsTotal(updated)
				} else {
					refs[i].credits = float64(credit.Total)
				}
			}(i)
		}
		wg.Wait()
	}

	cands := make([]gongyiCandidate, 0, len(refs))
	values := map[string]float64{}
	for _, ref := range refs {
		if ref.credits < req.MinCredits {
			continue
		}
		c := gongyiCandidate{
			ID:      ref.account.ID,
			Name:    gongyiDisplayName(ref.account),
			Region:  ref.account.Region,
			Credits: ref.credits,
			Status:  ref.account.Status,
		}
		if !ref.account.LastUsedAt.IsZero() {
			c.LastUsedAt = ref.account.LastUsedAt.Format(time.RFC3339)
		}
		cands = append(cands, c)
		values[ref.account.ID] = ref.credits
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Credits != cands[j].Credits {
			return cands[i].Credits > cands[j].Credits
		}
		return cands[i].LastUsedAt > cands[j].LastUsedAt
	})
	if len(cands) > req.Limit {
		cands = cands[:req.Limit]
	}
	cutoff := 0.0
	if len(cands) > 0 {
		cutoff = cands[len(cands)-1].Credits
	}
	resp := map[string]any{
		"ok":             true,
		"dry_run":        req.DryRun,
		"candidates":     len(refs),
		"refreshed":      doRefresh,
		"refresh_failed": refreshFailed,
		"qualifying":     len(cands),
		"selected":       len(cands),
		"cutoff_credits": cutoff,
		"target":         req.Limit,
		"mode":           req.Mode,
		"excluded_off":   excludedOff,
		"gap":            maxInt(0, req.Limit-len(cands)),
		"accounts":       cands,
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if len(cands) == 0 {
		writeError(w, http.StatusConflict, "没有达标账号（实查失败或额度不足），公益池保持不变")
		return
	}
	ids := make(map[string]bool, len(cands))
	for _, c := range cands {
		ids[c.ID] = true
	}
	poolCount := a.store.SetGongyiBatch(ids, req.Mode == "replace")
	baseline := make(map[string]float64, len(cands))
	for _, c := range cands {
		baseline[c.ID] = c.Credits
	}
	a.gongyi.SetBaselines(baseline)
	if req.KeyID != "" {
		a.store.SetClientKeyPool(req.KeyID, "gongyi")
	}
	resp["pool_total"] = poolCount
	log.Printf("gongyi-select: mode=%s selected=%d pool_total=%d cutoff=%.0f refresh_failed=%d",
		req.Mode, len(cands), poolCount, cutoff, refreshFailed)
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) adminGongyiAccounts(w http.ResponseWriter, r *http.Request) {
	// Per-account today usage from audits.
	usageByName := map[string]map[string]any{}
	today := time.Now().Format("2006-01-02")
	names := map[string]bool{}
	accounts := a.store.GongyiAccounts()
	for _, account := range accounts {
		names[gongyiDisplayName(account)] = true
	}
	for _, audit := range a.store.ListAudits() {
		if audit.CreatedAt.Format("2006-01-02") != today || !names[audit.AccountName] {
			continue
		}
		row := usageByName[audit.AccountName]
		if row == nil {
			row = map[string]any{"requests": int64(0), "errors": int64(0), "tokens": int64(0), "latency_ms": int64(0)}
			usageByName[audit.AccountName] = row
		}
		row["requests"] = row["requests"].(int64) + 1
		if audit.Status >= 400 {
			row["errors"] = row["errors"].(int64) + 1
		}
		row["tokens"] = row["tokens"].(int64) + int64(audit.PromptTokens+audit.CompletionTokens)
		row["latency_ms"] = row["latency_ms"].(int64) + audit.LatencyMs
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		credits := gongyiGrantsTotal(account)
		name := gongyiDisplayName(account)
		row := map[string]any{
			"id":           account.ID,
			"name":         name,
			"region":       account.Region,
			"credits":      credits,
			"enabled":      account.Enabled,
			"status":       account.Status,
			"cooldown":     account.CooldownUntil,
			"exhausted_at": account.ExhaustedAt,
			"last_used_at": account.LastUsedAt,
			"last_error":   account.LastError,
			"success":      account.SuccessCount,
			"fail":         account.FailCount,
			"today":        usageByName[name],
		}
		if account.Credit != nil {
			row["credit_synced_at"] = account.Credit.SyncedAt
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "total": len(out)})
}

func (a *API) adminGongyiToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		InPool *bool `json:"in_pool"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.InPool == nil {
		writeError(w, http.StatusBadRequest, "缺少 in_pool")
		return
	}
	account, err := a.store.UpdateAccount(id, func(account *store.Account) {
		account.Gongyi = *req.InPool
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if *req.InPool {
		a.gongyi.SetBaselines(map[string]float64{id: gongyiGrantsTotal(account)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "in_pool": *req.InPool})
}

func (a *API) adminGongyiRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
		N   int      `json:"n"`
	}
	_ = decode(r, &req)
	okCnt, failCnt := a.gongyiRefresh(req.IDs, req.N)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refreshed": okCnt, "failed": failCnt})
}

func (a *API) gongyiRefresh(ids []string, n int) (okCnt, failCnt int) {
	if a.signin == nil {
		return 0, 0
	}
	if len(ids) == 0 {
		if n <= 0 {
			n = 2
		}
		accounts := a.store.GongyiAccounts()
		sort.Slice(accounts, func(i, j int) bool {
			left, right := accounts[i].Credit, accounts[j].Credit
			if left == nil {
				return true
			}
			if right == nil {
				return false
			}
			return left.SyncedAt.Before(right.SyncedAt)
		})
		for i := 0; i < len(accounts) && i < n; i++ {
			ids = append(ids, accounts[i].ID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	values := map[string]float64{}
	for _, id := range ids {
		credit, err := a.signin.RefreshCredit(ctx, id)
		if err != nil || credit == nil {
			failCnt++
			continue
		}
		if updated, ok := a.store.AccountByID(id); ok {
			values[id] = gongyiGrantsTotal(updated)
		} else {
			values[id] = float64(credit.Total)
		}
		okCnt++
	}
	a.gongyi.ObserveMany(values)
	return okCnt, failCnt
}

func (a *API) adminGongyiOverview(w http.ResponseWriter, r *http.Request) {
	cfg := a.gongyi.config()
	snap := a.gongyiSnapshot()
	requests, errors, promptTok, completionTok, avgLatency := a.gongyiAuditStats()
	pushes := a.gongyi.Pushes()
	var lastPush *gongyiPushRecord
	if len(pushes) > 0 {
		rec := pushes[len(pushes)-1]
		lastPush = &rec
	}
	a.gongyiPushMu.Lock()
	nextAt := "启动后首轮"
	if !a.gongyiLastPushAt.IsZero() {
		nextAt = a.gongyiLastPushAt.Add(time.Duration(cfg.PushMinutes) * time.Minute).Format("2006-01-02 15:04:05")
	}
	a.gongyiPushMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snap,
		"today": map[string]any{
			"requests":          requests,
			"errors":            errors,
			"prompt_tokens":     promptTok,
			"completion_tokens": completionTok,
			"total_tokens":      promptTok + completionTok,
			"avg_latency_ms":    avgLatency,
		},
		"credit_used":   a.gongyi.TodayCreditUsed(),
		"credit_series": a.gongyi.CreditSeries(7),
		"push": map[string]any{
			"enabled":      cfg.Enabled,
			"auto":         cfg.AutoPush,
			"worker_url":   cfg.WorkerURL,
			"pool_id":      cfg.PoolID,
			"capacity":     cfg.Capacity,
			"daily_grant":  cfg.DailyGrant,
			"reserve_days": cfg.ReserveDays,
			"target":       cfg.Target,
			"minutes":      cfg.PushMinutes,
			"next_at":      nextAt,
			"last":         lastPush,
		},
		"online":   a.gongyiOnline(false),
		"requests": a.gongyiRecentAudits(100),
	})
}

func (a *API) adminGongyiUsage(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	// Daily aggregates from audits.
	type dayAgg struct {
		Requests int64 `json:"requests"`
		Errors   int64 `json:"errors"`
		Tokens   int64 `json:"tokens"`
	}
	byDay := map[string]*dayAgg{}
	keyName := a.gongyiKeyName()
	names := map[string]bool{}
	for _, account := range a.store.GongyiAccounts() {
		names[gongyiDisplayName(account)] = true
	}
	cutoff := time.Now().AddDate(0, 0, -days+1).Format("2006-01-02")
	for _, audit := range a.store.ListAudits() {
		key := audit.CreatedAt.Format("2006-01-02")
		if key < cutoff {
			continue
		}
		if keyName != "" {
			if audit.KeyName != keyName {
				continue
			}
		} else if !names[audit.AccountName] {
			continue
		}
		row := byDay[key]
		if row == nil {
			row = &dayAgg{}
			byDay[key] = row
		}
		row.Requests++
		if audit.Status >= 400 {
			row.Errors++
		}
		row.Tokens += int64(audit.PromptTokens + audit.CompletionTokens)
	}
	series := make([]map[string]any, 0, days)
	for i := days - 1; i >= 0; i-- {
		key := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		row := byDay[key]
		if row == nil {
			row = &dayAgg{}
		}
		series = append(series, map[string]any{
			"date": key, "requests": row.Requests, "errors": row.Errors, "tokens": row.Tokens,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":          days,
		"series":        series,
		"credit_series": a.gongyi.CreditSeries(days),
	})
}

func (a *API) gongyiRecentAudits(limit int) []map[string]any {
	keyName := a.gongyiKeyName()
	names := map[string]bool{}
	for _, account := range a.store.GongyiAccounts() {
		names[gongyiDisplayName(account)] = true
	}
	out := make([]map[string]any, 0, limit)
	for _, audit := range a.store.ListAudits() {
		if keyName != "" {
			if audit.KeyName != keyName {
				continue
			}
		} else if !names[audit.AccountName] {
			continue
		}
		out = append(out, map[string]any{
			"id": audit.ID, "created_at": audit.CreatedAt, "account": audit.AccountName,
			"model": audit.Model, "status": audit.Status, "latency_ms": audit.LatencyMs,
			"prompt_tokens": audit.PromptTokens, "completion_tokens": audit.CompletionTokens,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// config endpoints
// ---------------------------------------------------------------------------

func maskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "…" + value[len(value)-4:]
}

func secretUnchanged(in, cur string) bool {
	in = strings.TrimSpace(in)
	return in == "" || strings.Contains(in, "…") || in == maskSecret(cur)
}

func (a *API) adminGongyiConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.gongyi.config()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":          cfg.Enabled,
		"worker_url":       cfg.WorkerURL,
		"admin_key":        maskSecret(cfg.AdminKey),
		"pool_id":          cfg.PoolID,
		"capacity":         cfg.Capacity,
		"full_per_account": cfg.FullPerAccount,
		"daily_grant":      cfg.DailyGrant,
		"reserve_days":     cfg.ReserveDays,
		"target":           cfg.Target,
		"push_minutes":     cfg.PushMinutes,
		"auto_push":        cfg.AutoPush,
		"key_id":           cfg.KeyID,
		"refresh_per_tick": cfg.RefreshPerTick,
		"refresh_minutes":  cfg.RefreshMinutes,
	})
}

func (a *API) adminGongyiConfigSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled        *bool   `json:"enabled"`
		WorkerURL      string  `json:"worker_url"`
		AdminKey       string  `json:"admin_key"`
		PoolID         string  `json:"pool_id"`
		Capacity       int64   `json:"capacity"`
		FullPerAccount float64 `json:"full_per_account"`
		DailyGrant     float64 `json:"daily_grant"`
		ReserveDays    int     `json:"reserve_days"`
		Target         int     `json:"target"`
		PushMinutes    int     `json:"push_minutes"`
		AutoPush       *bool   `json:"auto_push"`
		KeyID          string  `json:"key_id"`
		RefreshPerTick int     `json:"refresh_per_tick"`
		RefreshMinutes int     `json:"refresh_minutes"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	cfg := a.gongyi.config()
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if strings.TrimSpace(req.WorkerURL) != "" {
		cfg.WorkerURL = strings.TrimSpace(req.WorkerURL)
	}
	if !secretUnchanged(req.AdminKey, cfg.AdminKey) {
		cfg.AdminKey = strings.TrimSpace(req.AdminKey)
	}
	if req.PoolID != "" {
		cfg.PoolID = req.PoolID
	}
	if req.Capacity > 0 {
		cfg.Capacity = req.Capacity
	}
	if req.FullPerAccount > 0 {
		cfg.FullPerAccount = req.FullPerAccount
	}
	if req.DailyGrant > 0 {
		cfg.DailyGrant = req.DailyGrant
	}
	if req.ReserveDays > 0 {
		cfg.ReserveDays = req.ReserveDays
	}
	if req.Target > 0 {
		cfg.Target = req.Target
	}
	if req.PushMinutes > 0 {
		cfg.PushMinutes = req.PushMinutes
	}
	if req.AutoPush != nil {
		cfg.AutoPush = *req.AutoPush
	}
	if req.KeyID != "" {
		cfg.KeyID = req.KeyID
		a.store.SetClientKeyPool(req.KeyID, "gongyi")
	}
	if req.RefreshPerTick > 0 {
		cfg.RefreshPerTick = req.RefreshPerTick
	}
	if req.RefreshMinutes > 0 {
		cfg.RefreshMinutes = req.RefreshMinutes
	}
	saved := a.gongyi.saveConfig(cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "admin_key": maskSecret(saved.AdminKey), "worker_url": saved.WorkerURL,
	})
}

// ---------------------------------------------------------------------------
// push + background loop
// ---------------------------------------------------------------------------

func gongyiSign(secret, method, path, ts, nonce, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// gongyiNonce returns a 128-bit URL-safe nonce for the Worker admin HMAC.
func gongyiNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (a *API) pushGongyi() gongyiPushRecord {
	cfg := a.gongyi.config()
	now := time.Now()
	res := gongyiPushRecord{At: now.Format("2006-01-02 15:04:05")}
	fail := func(msg string) gongyiPushRecord {
		res.Error = msg
		a.gongyiPushMu.Lock()
		a.gongyiLastPushAt = now
		a.gongyiPushMu.Unlock()
		a.gongyi.AddPush(res)
		return res
	}
	if !cfg.Enabled {
		return fail("公益联动未启用")
	}
	if cfg.WorkerURL == "" || cfg.AdminKey == "" {
		return fail("未配置 Worker 地址或管理密钥")
	}
	snap := a.gongyiSnapshot()
	res.Pct = snap.Pct
	res.Remaining = snap.Remaining
	if snap.Total == 0 {
		return fail("公益池为空，跳过推送")
	}
	body, _ := json.Marshal(map[string]any{
		"id": cfg.PoolID, "capacity": snap.Capacity, "remaining": snap.Remaining,
	})
	ts := strconv.FormatInt(now.Unix(), 10)
	path := "/admin/pool"
	nonce := gongyiNonce()
	req, err := http.NewRequest(http.MethodPost, cfg.WorkerURL+path, strings.NewReader(string(body)))
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("X-Admin-Key", cfg.AdminKey)
	req.Header.Set("X-Admin-Ts", ts)
	req.Header.Set("X-Admin-Nonce", nonce)
	req.Header.Set("X-Admin-Sig", gongyiSign(cfg.AdminKey, http.MethodPost, path, ts, nonce, string(body)))
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	res.HTTP = resp.StatusCode
	res.OK = resp.StatusCode == http.StatusOK
	if !res.OK {
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	a.gongyiPushMu.Lock()
	a.gongyiLastPushAt = now
	a.gongyiPushMu.Unlock()
	a.gongyi.AddPush(res)
	log.Printf("gongyi-push: pct=%.1f%% remaining=%d http=%d ok=%v", res.Pct*100, res.Remaining, res.HTTP, res.OK)
	return res
}

func (a *API) adminGongyiPush(w http.ResponseWriter, r *http.Request) {
	res := a.pushGongyi()
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res})
}

func (a *API) adminGongyiSelfCheck(w http.ResponseWriter, r *http.Request) {
	res := a.pushGongyi()
	online := a.gongyiOnline(true)
	diff := any(nil)
	if daily, ok := online["daily"].(float64); ok {
		diff = math.Round((res.Pct*100-daily)*10) / 10
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res, "online": online, "diff": diff})
}

func (a *API) gongyiOnline(force bool) map[string]any {
	cfg := a.gongyi.config()
	a.gongyiOnlineMu.Lock()
	if !force && a.gongyiOnlineAt.After(time.Now().Add(-30*time.Second)) {
		v := a.gongyiOnlineV
		a.gongyiOnlineMu.Unlock()
		return v
	}
	a.gongyiOnlineMu.Unlock()
	if cfg.WorkerURL == "" {
		return map[string]any{"error": "未配置"}
	}
	url := cfg.WorkerURL + "/status.json"
	if force {
		url += "?t=" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{"error": "bad json"}
	}
	v["fetched_at"] = time.Now().Format("2006-01-02 15:04:05")
	a.gongyiOnlineMu.Lock()
	a.gongyiOnlineV = v
	a.gongyiOnlineAt = time.Now()
	a.gongyiOnlineMu.Unlock()
	return v
}

// StartGongyi starts the public-pool background loop (push + credit rotation).
func (a *API) StartGongyi(ctx context.Context) {
	go func() {
		t := time.NewTimer(15 * time.Second)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.gongyiTick()
		}
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.gongyiTick()
			}
		}
	}()
}

func (a *API) gongyiTick() {
	cfg := a.gongyi.config()
	now := time.Now()
	if cfg.Enabled && cfg.AutoPush {
		a.gongyiPushMu.Lock()
		last := a.gongyiLastPushAt
		a.gongyiPushMu.Unlock()
		if last.IsZero() || now.Sub(last) >= time.Duration(cfg.PushMinutes)*time.Minute {
			a.pushGongyi()
		}
	}
	if cfg.Enabled {
		a.gongyiRefreshMu.Lock()
		due := a.gongyiLastRefreshAt.IsZero() ||
			now.Sub(a.gongyiLastRefreshAt) >= time.Duration(cfg.RefreshMinutes)*time.Minute
		if due && !a.gongyiRefreshing {
			a.gongyiRefreshing = true
			a.gongyiLastRefreshAt = now
			n := cfg.RefreshPerTick
			a.gongyiRefreshMu.Unlock()
			go func() {
				defer func() {
					a.gongyiRefreshMu.Lock()
					a.gongyiRefreshing = false
					a.gongyiRefreshMu.Unlock()
				}()
				a.gongyiRefresh(nil, n)
			}()
		} else {
			a.gongyiRefreshMu.Unlock()
		}
	}
}

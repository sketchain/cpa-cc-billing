package ccbilling

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// KeyInfo is what the billing store knows about a downstream key.
type KeyInfo struct {
	Preview string
	Label   string
	PlanID  string
	// Weekly describes the key's 7-day plan window; HasWeekly is false when
	// its plan has none. Amounts are in USD.
	HasWeekly   bool
	WeeklyUSD   float64
	UsedWeekUSD float64
	CycleStart  time.Time
	CycleEnd    time.Time
	// Anchored reports whether the plan's cycles start at a fixed anchor.
	Anchored bool
}

// Alignment is the outcome of keeping a plan's weekly window on an account's
// 7-day reset.
type Alignment struct {
	Status    string    `json:"status"`
	WaitUntil time.Time `json:"wait_until,omitzero"`
}

// Alignment statuses.
const (
	AlignAligned     = "aligned"
	AlignChanged     = "changed"
	AlignWaiting     = "waiting"
	AlignUnsupported = "unsupported"
	AlignNoWindow    = "no_window"
	AlignUnknown     = "unknown"
)

// WeeklyPeriod is an account's current 7-day period as its reports tell it.
type WeeklyPeriod struct {
	Start, ResetAt time.Time
	// EarlyResetAt is when the latest early reset was detected, if any.
	EarlyResetAt time.Time
}

// Host gives the manager the billing store's data. Every method must be safe
// to call while the manager holds its own lock, and none may call back into it.
type Host interface {
	// WithDB runs fn with the billing database, holding it open.
	WithDB(fn func(*sql.DB) error) error
	Now() time.Time
	ToUSD(amount float64, currency string) float64
	LatestObservations(authIndexes []string, since time.Time) (map[string]Observation, error)
	WeeklyPeriod(authIndex string, now time.Time) (WeeklyPeriod, bool, error)
	KeyInfo(scope string) (KeyInfo, bool)
	KeyUsageUSD(scope string, from, to time.Time) (float64, error)
	AlignWeekly(planID string, resetAt time.Time) (Alignment, error)
	Logf(format string, args ...any)
}

type accountCache struct {
	stream   StreamState
	segments []Segment
	estimate Estimate
	weekly   WeeklyPeriod
	hasWeek  bool
}

// Manager keeps the estimates of every configured account and answers
// admission and balance questions. All work happens synchronously within host
// calls; refreshes are throttled by RefreshInterval.
type Manager struct {
	host Host

	mu          sync.Mutex
	db          *sql.DB
	loaded      bool
	settings    Settings
	accounts    map[string]*accountCache
	latest      map[string]Observation
	lastRefresh time.Time
	lastPrune   time.Time
	lastError   string
	alignments  map[string]Alignment

	// RefreshInterval throttles reprocessing of new request history.
	RefreshInterval time.Duration
	// ProcessBudget bounds the history one refresh processes, so a large
	// backlog is spread over several host calls.
	ProcessBudget time.Duration
}

func NewManager(host Host) *Manager {
	return &Manager{host: host, accounts: map[string]*accountCache{}, latest: map[string]Observation{},
		alignments: map[string]Alignment{}, RefreshInterval: time.Minute, ProcessBudget: 3 * time.Second}
}

// Settings returns the current settings, loading them on first use.
func (m *Manager) Settings() (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(false); err != nil && !m.loaded {
		return Settings{}, err
	}
	return m.settings, nil
}

// SetSettings validates, stores and applies new settings.
func (m *Manager) SetSettings(next Settings) (Settings, error) {
	normalized, err := next.Normalized()
	if err != nil {
		return Settings{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.withDB(func(db *sql.DB) error { return saveSettings(db, normalized) }); err != nil {
		return Settings{}, err
	}
	m.settings = normalized
	m.alignments = map[string]Alignment{}
	return normalized, m.refreshLocked(true)
}

// Rebuild discards the stored segments and rebuilds them from the retained
// request history with the current parameters.
func (m *Manager) Rebuild() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.withDB(resetStreams); err != nil {
		return err
	}
	m.accounts = map[string]*accountCache{}
	return m.refreshLocked(true)
}

// Refresh processes new history now.
func (m *Manager) Refresh() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked(true)
}

// Observe records an account's latest report as soon as its usage arrives.
func (m *Manager) Observe(authIndex string, obs Observation) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || (!obs.Has5h && !obs.Has7d) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latest[authIndex] = mergeLatest(m.latest[authIndex], obs)
}

// mergeLatest keeps the newest value of each window. Reports of the same
// period never lower its utilization: a lower one arrived out of order.
func mergeLatest(current, next Observation) Observation {
	merge := func(has *bool, used *float64, reset *time.Time, nextHas bool, nextUsed float64, nextReset time.Time, newer bool) {
		if !nextHas {
			return
		}
		samePeriod := *has && absDuration(nextReset.Sub(*reset)) <= boundaryJitter
		switch {
		case !*has || (newer && !samePeriod && !nextReset.Before(reset.Add(-boundaryJitter))):
			*has, *used, *reset = true, nextUsed, nextReset
		case samePeriod && nextUsed > *used:
			*used = nextUsed
		}
	}
	newer := next.At.After(current.At)
	merge(&current.Has5h, &current.Used5h, &current.Reset5h, next.Has5h, next.Used5h, next.Reset5h, newer)
	merge(&current.Has7d, &current.Used7d, &current.Reset7d, next.Has7d, next.Used7d, next.Reset7d, newer)
	if newer {
		current.At = next.At
	}
	return current
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func (m *Manager) withDB(fn func(*sql.DB) error) error {
	return m.host.WithDB(func(db *sql.DB) error {
		if db == nil {
			return fmt.Errorf("The billing database is not open")
		}
		if db != m.db {
			if err := EnsureSchema(db); err != nil {
				return err
			}
			settings, err := loadSettings(db)
			if err != nil {
				return err
			}
			m.db, m.loaded, m.settings = db, true, settings
			m.accounts, m.latest, m.alignments = map[string]*accountCache{}, map[string]Observation{}, map[string]Alignment{}
			m.lastRefresh, m.lastPrune = time.Time{}, time.Time{}
		}
		return fn(db)
	})
}

func (m *Manager) refreshLocked(force bool) error {
	now := m.host.Now()
	if !force && m.loaded && !m.lastRefresh.IsZero() && now.Sub(m.lastRefresh) < m.RefreshInterval {
		return nil
	}
	err := m.withDB(func(db *sql.DB) error {
		params := m.settings.Params
		cutoff := now.Add(-params.retention())
		if m.lastPrune.IsZero() || now.Sub(m.lastPrune) > time.Hour {
			if err := pruneSegments(db, cutoff); err != nil {
				return err
			}
			m.lastPrune = now
		}
		deadline := time.Now().Add(m.ProcessBudget)
		for _, authIndex := range m.settings.authIndexes() {
			cache := m.accounts[authIndex]
			if cache == nil {
				stream, err := loadStream(db, authIndex)
				if err != nil {
					return err
				}
				segments, err := loadSegments(db, authIndex, cutoff)
				if err != nil {
					return err
				}
				cache = &accountCache{stream: stream, segments: segments}
				m.accounts[authIndex] = cache
			}
			if err := m.advance(db, authIndex, cache, now.Add(-ReorderHorizon), deadline); err != nil {
				return err
			}
			first := 0
			for first < len(cache.segments) && cache.segments[first].At.Before(cutoff) {
				first++
			}
			cache.segments = cache.segments[first:]
			cache.estimate = EstimateSegments(cache.segments, params, now)
		}
		return nil
	})
	if err != nil {
		m.recordError(err)
		return err
	}
	indexes := m.settings.authIndexes()
	if latest, err := m.host.LatestObservations(indexes, now.Add(-Week-time.Hour)); err == nil {
		for index, obs := range latest {
			m.latest[index] = mergeLatest(m.latest[index], obs)
		}
	} else {
		m.recordError(err)
	}
	for _, index := range indexes {
		if cache := m.accounts[index]; cache != nil {
			if period, ok, err := m.host.WeeklyPeriod(index, now); err == nil {
				cache.weekly, cache.hasWeek = period, ok
			}
		}
	}
	m.lastRefresh = now
	m.lastError = ""
	return nil
}

func (m *Manager) recordError(err error) {
	if err.Error() != m.lastError {
		m.host.Logf("CC estimation failed: %v", err)
	}
	m.lastError = err.Error()
}

// advance processes an account's history in chunks of at most a day until
// until, or until the deadline passes.
func (m *Manager) advance(db *sql.DB, authIndex string, cache *accountCache, until, deadline time.Time) error {
	params := m.settings.Params
	if cache.stream.Cursor.IsZero() {
		var first sql.NullInt64
		if err := db.QueryRow(`SELECT min(at) FROM quota_report_windows WHERE auth_index = ? AND provider = ?`,
			authIndex, claudeProvider).Scan(&first); err != nil {
			return fmt.Errorf("Read Claude quota reports: %w", err)
		}
		if !first.Valid {
			return nil
		}
		cache.stream.Cursor = timeAt(first.Int64).Add(-time.Nanosecond)
	}
	for cache.stream.Cursor.Before(until) {
		end := cache.stream.Cursor.Add(24 * time.Hour)
		if end.After(until) {
			end = until
		}
		observations, err := readObservations(db, authIndex, cache.stream.Cursor, end)
		if err != nil {
			return err
		}
		costs, err := readCosts(db, authIndex, cache.stream.Cursor, end, m.host.ToUSD)
		if err != nil {
			return err
		}
		next := cache.stream
		segments := next.Advance(observations, costs, end, params)
		if err := commitAdvance(db, authIndex, next, segments); err != nil {
			return err
		}
		cache.stream = next
		cache.segments = append(cache.segments, segments...)
		if time.Now().After(deadline) {
			break
		}
	}
	return nil
}

func (m *Manager) accountStatus(authIndex string, now time.Time) AccountStatus {
	cache := m.accounts[authIndex]
	if cache == nil {
		cache = &accountCache{}
	}
	return ResolveAccount(authIndex, m.settings.Accounts[authIndex], cache.estimate, cache.stream, m.latest[authIndex], m.settings.Params, now)
}

// poolStatus needs the lock; it reads key usage through the host.
func (m *Manager) poolStatus(pool Pool, now time.Time) PoolStatus {
	accounts := make([]AccountStatus, 0, len(pool.AuthIndexes))
	for _, index := range pool.AuthIndexes {
		accounts = append(accounts, m.accountStatus(index, now))
	}
	var fiveHourStart time.Time
	if len(accounts) > 0 && accounts[0].Reset5h.After(now) {
		fiveHourStart = accounts[0].Reset5h.Add(-FiveHours)
	}
	var fixed []FixedKeyUsage
	for _, scope := range sortedKeys(m.settings.Keys) {
		settings := m.settings.Keys[scope]
		if settings.Kind != KeyFixed || settings.PoolID != pool.ID {
			continue
		}
		usage := FixedKeyUsage{Scope: scope, FiveHourShare: settings.FiveHourShare}
		if info, ok := m.host.KeyInfo(scope); ok && info.HasWeekly {
			usage.Valid, usage.WeeklyUSD, usage.CycleStart, usage.CycleEnd = true, info.WeeklyUSD, info.CycleStart, info.CycleEnd
			if !info.CycleStart.After(now) && now.Before(info.CycleEnd) {
				usage.UsedWeekUSD = info.UsedWeekUSD
			}
			if !fiveHourStart.IsZero() {
				if used, err := m.host.KeyUsageUSD(scope, fiveHourStart, now); err == nil {
					usage.UsedFiveHour = used
				}
			}
		}
		fixed = append(fixed, usage)
	}
	return BuildPool(pool, accounts, fixed)
}

// remainderCapUsage is a remainder key's usage in the anchor account's
// current 7-day period, or the last 7 days when it is unknown.
func (m *Manager) remainderCapUsage(scope string, pool PoolStatus, now time.Time) float64 {
	from := now.Add(-Week)
	if pool.Reset7d.After(now) {
		from = pool.Reset7d.Add(-Week)
	}
	used, err := m.host.KeyUsageUSD(scope, from, now)
	if err != nil {
		return 0
	}
	return used
}

// alignFixed keeps a fixed key's weekly plan window on the anchor account's
// 7-day reset before quota admission can open a cycle.
func (m *Manager) alignFixed(scope string, pool Pool) Alignment {
	if len(pool.AuthIndexes) == 0 {
		return Alignment{Status: AlignUnknown}
	}
	cache := m.accounts[pool.AuthIndexes[0]]
	info, ok := m.host.KeyInfo(scope)
	if !ok || info.PlanID == "" || !info.HasWeekly {
		return Alignment{Status: AlignNoWindow}
	}
	if cache == nil || !cache.hasWeek || cache.weekly.ResetAt.IsZero() {
		return Alignment{Status: AlignUnknown}
	}
	cacheKey := info.PlanID + "\x00" + cache.weekly.ResetAt.Format(time.RFC3339)
	if previous, ok := m.alignments[cacheKey]; ok && previous.Status == AlignAligned {
		return previous
	}
	result, err := m.host.AlignWeekly(info.PlanID, cache.weekly.ResetAt)
	if err != nil {
		m.recordError(err)
		return Alignment{Status: AlignUnknown}
	}
	if result.Status == AlignChanged {
		result.Status = AlignAligned
	}
	m.alignments[cacheKey] = result
	return result
}

// Admit decides whether the key may send a request now. Keys outside every
// pool and fixed keys are always admitted here; fixed keys are limited by
// their subscription plan.
func (m *Manager) Admit(scope string) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(false); err != nil && !m.loaded {
		return Decision{Allowed: true}
	}
	settings, ok := m.settings.Keys[scope]
	if !ok {
		return Decision{Allowed: true}
	}
	pool, found := m.settings.pool(settings.PoolID)
	if !found {
		return Decision{Allowed: true}
	}
	now := m.host.Now()
	if settings.Kind == KeyFixed {
		m.alignFixed(scope, pool)
		return Decision{Allowed: true}
	}
	status := m.poolStatus(pool, now)
	used := 0.0
	if settings.WeeklyCapUSD != nil {
		used = m.remainderCapUsage(scope, status, now)
	}
	return status.AdmitRemainder(settings.WeeklyCapUSD, used)
}

// Balance is a key's weekly balance in USD; ok is false for keys outside
// every pool.
func (m *Manager) Balance(scope string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(false); err != nil && !m.loaded {
		return 0, false
	}
	settings, ok := m.settings.Keys[scope]
	if !ok {
		return 0, false
	}
	pool, found := m.settings.pool(settings.PoolID)
	if !found {
		return 0, false
	}
	now := m.host.Now()
	if settings.Kind == KeyFixed {
		m.alignFixed(scope, pool)
		info, ok := m.host.KeyInfo(scope)
		if !ok || !info.HasWeekly {
			return 0, true
		}
		used := 0.0
		if !info.CycleStart.After(now) && now.Before(info.CycleEnd) {
			used = info.UsedWeekUSD
		}
		return info.WeeklyUSD - used, true
	}
	status := m.poolStatus(pool, now)
	used := 0.0
	if settings.WeeklyCapUSD != nil {
		used = m.remainderCapUsage(scope, status, now)
	}
	return status.RemainderKeyBalance(settings.WeeklyCapUSD, used), true
}

// KeyStatus is one assigned key on the CC page.
type KeyStatus struct {
	Scope    string      `json:"scope"`
	Preview  string      `json:"preview,omitempty"`
	Label    string      `json:"label,omitempty"`
	Settings KeySettings `json:"settings"`
	PlanID   string      `json:"plan_id,omitempty"`
	// Fixed keys.
	WeeklyUSD       float64    `json:"weekly_usd"`
	UsedWeekUSD     float64    `json:"used_week_usd"`
	UsedFiveHourUSD float64    `json:"used_five_hour_usd"`
	ReserveFiveHour float64    `json:"reserve_five_hour_usd"`
	ReserveWeek     float64    `json:"reserve_week_usd"`
	CycleStart      time.Time  `json:"cycle_start,omitzero"`
	CycleEnd        time.Time  `json:"cycle_end,omitzero"`
	Alignment       *Alignment `json:"alignment,omitempty"`
	Valid           bool       `json:"valid"`
	// Remainder keys.
	UsedCapUSD float64 `json:"used_cap_usd"`
	// Balance is the weekly balance; Reported is what new-api receives.
	Balance  float64   `json:"balance_usd"`
	Reported float64   `json:"reported_balance_usd"`
	Admitted *bool     `json:"admitted,omitempty"`
	Refusal  string    `json:"refusal,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
}

// AccountView adds an account's segment history to its status.
type AccountView struct {
	AccountStatus
	Cursor       time.Time      `json:"cursor,omitzero"`
	WeeklyStart  time.Time      `json:"weekly_start,omitzero"`
	EarlyResetAt time.Time      `json:"early_reset_at,omitzero"`
	RecentBlocks []BlockSummary `json:"recent_blocks"`
}

// View is the CC page.
type View struct {
	Settings Settings      `json:"settings"`
	Defaults Params        `json:"defaults"`
	Accounts []AccountView `json:"accounts"`
	Pools    []PoolStatus  `json:"pools"`
	Keys     []KeyStatus   `json:"keys"`
	Segments int           `json:"stored_segments"`
	Error    string        `json:"error,omitempty"`
	At       time.Time     `json:"at"`
}

// View refreshes and describes every account, pool and assigned key.
func (m *Manager) View() (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(true); err != nil && !m.loaded {
		return View{}, err
	}
	now := m.host.Now()
	view := View{Settings: m.settings, Defaults: DefaultParams(), Accounts: []AccountView{}, Pools: []PoolStatus{}, Keys: []KeyStatus{},
		Error: m.lastError, At: now}
	_ = m.withDB(func(db *sql.DB) error {
		view.Segments, _ = segmentStats(db)
		return nil
	})
	for _, index := range m.settings.authIndexes() {
		account := AccountView{AccountStatus: m.accountStatus(index, now), RecentBlocks: []BlockSummary{}}
		if cache := m.accounts[index]; cache != nil {
			account.Cursor = cache.stream.Cursor
			if cache.hasWeek {
				account.WeeklyStart, account.EarlyResetAt = cache.weekly.Start, cache.weekly.EarlyResetAt
			}
			blocks := cache.estimate.Blocks
			account.RecentBlocks = slices.Clone(blocks[max(0, len(blocks)-20):])
			slices.Reverse(account.RecentBlocks)
		}
		view.Accounts = append(view.Accounts, account)
	}
	pools := map[string]PoolStatus{}
	for _, pool := range m.settings.Pools {
		status := m.poolStatus(pool, now)
		pools[pool.ID] = status
		view.Pools = append(view.Pools, status)
	}
	for _, scope := range sortedKeys(m.settings.Keys) {
		settings := m.settings.Keys[scope]
		key := KeyStatus{Scope: scope, Settings: settings}
		info, ok := m.host.KeyInfo(scope)
		if ok {
			key.Preview, key.Label, key.PlanID = info.Preview, info.Label, info.PlanID
		}
		pool := pools[settings.PoolID]
		poolSettings, _ := m.settings.pool(settings.PoolID)
		if settings.Kind == KeyFixed {
			alignment := m.alignFixed(scope, poolSettings)
			key.Alignment = &alignment
			for _, fixed := range pool.Fixed {
				if fixed.Scope == scope {
					key.Valid, key.WeeklyUSD, key.UsedWeekUSD = fixed.Valid, fixed.WeeklyUSD, fixed.UsedWeekUSD
					key.UsedFiveHourUSD, key.ReserveFiveHour, key.ReserveWeek = fixed.UsedFiveHour, fixed.ReserveFiveHour, fixed.RemainingWeek
					key.CycleStart, key.CycleEnd, key.Balance = fixed.CycleStart, fixed.CycleEnd, fixed.BalanceAvailable
				}
			}
		} else {
			key.Valid = true
			if settings.WeeklyCapUSD != nil {
				key.UsedCapUSD = m.remainderCapUsage(scope, pool, now)
			}
			key.Balance = pool.RemainderKeyBalance(settings.WeeklyCapUSD, key.UsedCapUSD)
			decision := pool.AdmitRemainder(settings.WeeklyCapUSD, key.UsedCapUSD)
			key.Admitted, key.Refusal, key.RetryAt = &decision.Allowed, decision.Reason, decision.RetryAt
		}
		key.Reported = ReportedBalance(key.Balance)
		view.Keys = append(view.Keys, key)
	}
	return view, nil
}

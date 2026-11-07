package ccbilling

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-super-billing/internal/messages"
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
	// SharedQuota reports that the plan pools one quota across its keys, so
	// the weekly amounts above are the whole pool's. CC fixed keys may not
	// use such a plan.
	SharedQuota bool
	// PlanRevision changes whenever the key's plan does.
	PlanRevision string
	// Seq is the usage sequence number UsedWeekUSD includes.
	Seq uint64
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
	// LatestObservations reads each account's latest main window reports
	// from its source.
	LatestObservations(sources map[string]QuotaSource, since time.Time) (map[string]Observation, error)
	WeeklyPeriod(authIndex string, source QuotaSource, now time.Time) (WeeklyPeriod, bool, error)
	KeyInfo(scope string) (KeyInfo, bool)
	// KeyUsageUSD returns the key's usage and the usage sequence number it includes.
	KeyUsageUSD(scope string, from, to time.Time) (float64, uint64, error)
	AlignWeekly(planID string, resetAt time.Time) (Alignment, error)
	Logf(format string, args ...any)
}

// accountCache is an account's history as the refresh keeps it.
type accountCache struct {
	stream   StreamState
	segments []Segment
	buckets  []KBucket
}

// accountSnapshot is what a refresh computed about an account.
type accountSnapshot struct {
	// estimate is on the basis in effect; raw and weighted are both bases.
	estimate Estimate
	raw      Estimate
	weighted Estimate
	k        KEstimate
	weekly   WeeklyPeriod
	hasWeek  bool
	cursor   time.Time
	kCursor  time.Time
	// window, pause and excluded come from the account's stream.
	window      string
	paused      bool
	pauseReason string
	pausedSince time.Time
	excluded    ExcludedCost
}

// keySnapshot is what a refresh read about an assigned key. Usage observed
// after the refresh is added from the live deltas.
type keySnapshot struct {
	info          KeyInfo
	known         bool
	usedFiveHour  float64
	fiveHourStart time.Time
	fiveHourSeq   uint64
	usedCap       float64
	capFrom       time.Time
	capSeq        uint64
}

// snapshot is everything admission reads. A refresh replaces it as a whole.
type snapshot struct {
	at       time.Time
	settings Settings
	accounts map[string]accountSnapshot
	keys     map[string]keySnapshot
	// weights is each provider's quota weights.
	weights map[string]weightsSnapshot
}

type costDelta struct {
	seq uint64
	at  time.Time
	usd float64
}

// Manager keeps the estimates of every configured account and answers
// admission and balance questions.
//
// Admission and balances only read the latest snapshot and the live
// utilization; they never wait for a refresh or touch the database. Refreshes
// run synchronously within host calls that are off the request path
// (usage.handle and management calls): a usage-triggered refresh is skipped
// while another runs, and none starts a goroutine.
type Manager struct {
	host Host

	// refreshMu serializes refreshes and owns the fields below it.
	refreshMu   sync.Mutex
	db          *sql.DB
	settings    Settings
	accounts    map[string]*accountCache
	lastRefresh time.Time
	lastPrune   time.Time
	// sharedLogged maps each fixed key last logged on a shared quota plan
	// to that plan.
	sharedLogged map[string]string

	snap atomic.Pointer[snapshot]

	liveMu  sync.RWMutex
	latest  map[string]Observation
	deltas  map[string][]costDelta
	dropped bool

	// loaded holds the settings once read, so admission can tell missing
	// estimates from settings that never loaded.
	loaded atomic.Pointer[Settings]

	alignMu    sync.Mutex
	alignments map[string]alignmentEntry

	// weightCache holds each provider's last weight estimate and the inputs
	// it used; it belongs to refreshMu.
	weightCache map[string]weightCache

	panel atomic.Pointer[PanelSettings]

	// blockMu guards the refusal counts not yet saved.
	blockMu       sync.Mutex
	blocks        map[blockKey]int64
	blocksDropped int64

	lastError      atomic.Value
	loggedUnloaded atomic.Bool

	// RefreshInterval throttles usage-triggered refreshes.
	RefreshInterval time.Duration
	// ProcessBudget bounds the history one refresh processes, so a large
	// backlog is spread over several host calls.
	ProcessBudget time.Duration
}

func NewManager(host Host) *Manager {
	m := &Manager{host: host, accounts: map[string]*accountCache{}, latest: map[string]Observation{},
		deltas: map[string][]costDelta{}, alignments: map[string]alignmentEntry{}, RefreshInterval: time.Minute, ProcessBudget: 3 * time.Second}
	m.lastError.Store("")
	return m
}

// Loaded reports whether a refresh has produced a snapshot.
func (m *Manager) Loaded() bool { return m.snap.Load() != nil }

// Settings returns the current settings, loading them on first use.
func (m *Manager) Settings() (Settings, error) {
	if snap := m.snap.Load(); snap != nil {
		return snap.settings, nil
	}
	if err := m.Refresh(); err != nil && m.snap.Load() == nil {
		return Settings{}, err
	}
	return m.snap.Load().settings, nil
}

// SetSettings validates, stores and applies new settings.
func (m *Manager) SetSettings(next Settings) (Settings, error) {
	normalized, err := next.Normalized()
	if err != nil {
		return Settings{}, err
	}
	if err := m.checkSharedPlans(normalized); err != nil {
		return Settings{}, err
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(func(db *sql.DB) error { return saveSettings(db, normalized) }); err != nil {
		return Settings{}, err
	}
	m.settings = normalized
	m.loaded.Store(&normalized)
	m.alignMu.Lock()
	m.alignments = map[string]alignmentEntry{}
	m.alignMu.Unlock()
	return normalized, m.refreshLocked()
}

// Rebuild discards the stored segments and k evidence and rebuilds them from
// the retained request history with the current parameters.
func (m *Manager) Rebuild() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(resetStreams); err != nil {
		return err
	}
	m.accounts = map[string]*accountCache{}
	m.weightCache = nil
	return m.refreshLocked()
}

// Refresh processes new history now, waiting for a running refresh. Only
// management calls use it.
func (m *Manager) Refresh() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.refreshLocked()
}

// MaybeRefresh refreshes when the interval has passed and no other refresh
// is running; it never waits.
func (m *Manager) MaybeRefresh() {
	if !m.refreshMu.TryLock() {
		return
	}
	defer m.refreshMu.Unlock()
	if m.snap.Load() != nil && m.host.Now().Sub(m.lastRefresh) < m.RefreshInterval {
		if m.pendingBlocks() {
			if err := m.withDB(m.flushBlocksLocked); err != nil {
				m.recordError(err)
			}
		}
		return
	}
	_ = m.refreshLocked()
}

// Observe records an account's latest report as soon as its usage arrives.
func (m *Manager) Observe(authIndex string, obs Observation) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || (!obs.Has5h && !obs.Has7d) {
		return
	}
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	m.latest[authIndex] = mergeLatest(m.latest[authIndex], obs)
}

// maxDeltasPerKey bounds the live usage kept for one key between refreshes;
// the oldest is dropped first.
const maxDeltasPerKey = 50000

// ObserveCost adds the cost of an assigned key's request, made at at, to the
// usage of the latest snapshot. seq is the store's usage sequence number of
// the request; snapshot reads report the number they include, so a request
// is never counted twice. It is called after the request is stored.
func (m *Manager) ObserveCost(scope string, at time.Time, usd float64, seq uint64) {
	settings := m.loaded.Load()
	if settings == nil || usd <= 0 || !finite(usd) {
		return
	}
	if _, assigned := settings.Keys[scope]; !assigned {
		return
	}
	m.liveMu.Lock()
	defer m.liveMu.Unlock()
	deltas := append(m.deltas[scope], costDelta{seq: seq, at: at, usd: usd})
	if len(deltas) > maxDeltasPerKey {
		deltas = slices.Delete(deltas, 0, len(deltas)-maxDeltasPerKey)
		m.dropped = true
	}
	m.deltas[scope] = deltas
}

// pruneDeltasLocked forgets live usage that every snapshot read of its key
// includes, and usage too old for any window. It needs liveMu.
func (m *Manager) pruneDeltasLocked(included map[string]uint64, now time.Time) {
	oldest := now.Add(-Week - time.Hour)
	for scope, deltas := range m.deltas {
		cutoff, known := included[scope]
		kept := deltas[:0]
		for _, delta := range deltas {
			if (!known || delta.seq > cutoff) && !delta.at.Before(oldest) {
				kept = append(kept, delta)
			}
		}
		if len(kept) == 0 {
			delete(m.deltas, scope)
		} else {
			m.deltas[scope] = kept
		}
	}
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
	if newer || current.At.IsZero() {
		current.LimitReached = next.LimitReached
		if next.HasCredits {
			current.HasCredits, current.Credits = true, next.Credits
		}
	}
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

// withDB needs refreshMu.
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
			panel, err := loadPanelSettings(db)
			if err != nil {
				// Page preferences must not stop the estimates.
				m.host.Logf("%v; using the default page settings", err)
				panel = DefaultPanelSettings()
			}
			m.db, m.settings = db, settings
			m.loaded.Store(&settings)
			m.panel.Store(&panel)
			m.accounts = map[string]*accountCache{}
			m.weightCache = nil
			m.lastRefresh, m.lastPrune = time.Time{}, time.Time{}
			m.snap.Store(nil)
			m.liveMu.Lock()
			m.latest, m.deltas = map[string]Observation{}, map[string][]costDelta{}
			m.liveMu.Unlock()
			m.alignMu.Lock()
			m.alignments = map[string]alignmentEntry{}
			m.alignMu.Unlock()
		}
		return fn(db)
	})
}

// refreshLocked needs refreshMu. It processes new history, recomputes the
// estimates and publishes a new snapshot.
func (m *Manager) refreshLocked() error {
	now := m.host.Now()
	accounts := map[string]accountSnapshot{}
	var weights map[string]weightsSnapshot
	err := m.withDB(func(db *sql.DB) error {
		if err := m.flushBlocksLocked(db); err != nil {
			// Counts stay pending; the estimates must not wait for them.
			m.recordError(err)
		}
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
				buckets, err := loadKBuckets(db, authIndex, cutoff)
				if err != nil {
					return err
				}
				cache = &accountCache{stream: stream, segments: segments, buckets: buckets}
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
			first = 0
			for first < len(cache.buckets) && cache.buckets[first].At.Before(cutoff) {
				first++
			}
			cache.buckets = cache.buckets[first:]
		}
		var err error
		if weights, err = m.refreshWeights(db, now); err != nil {
			return err
		}
		for _, authIndex := range m.settings.authIndexes() {
			cache := m.accounts[authIndex]
			params := m.settings.accountParams(authIndex)
			provider := weights[m.settings.AccountProvider(authIndex)]
			stream := cache.stream
			account := accountSnapshot{k: EstimateK(cache.buckets, params, now), cursor: stream.Cursor, kCursor: stream.K.Cursor,
				window: stream.window(), paused: stream.Paused, pauseReason: stream.PauseReason, pausedSince: stream.PausedSince,
				excluded: stream.Excluded}
			account.raw = EstimateSegments(cache.segments, params, now)
			account.weighted = account.raw
			if len(provider.factors) > 0 {
				account.weighted = EstimateSegments(ConvertSegments(cache.segments, provider.factors), params, now)
			}
			if account.window == WindowWeek {
				// Segments of the 7-day window give its total; "" means 5 hours.
				account.raw.Window, account.weighted.Window = WindowWeek, WindowWeek
			}
			account.estimate = account.raw
			if provider.basis == BasisWeighted {
				account.estimate = account.weighted
			}
			accounts[authIndex] = account
		}
		return nil
	})
	if err != nil {
		m.recordError(err)
		m.liveMu.Lock()
		m.pruneDeltasLocked(nil, now)
		m.liveMu.Unlock()
		return err
	}
	// Read errors below keep the last known values; they stay reported.
	failed := false
	fail := func(err error) {
		failed = true
		m.recordError(err)
	}
	previous := m.snap.Load()
	indexes := m.settings.authIndexes()
	if latest, err := m.host.LatestObservations(m.settings.AccountSources(), now.Add(-Week-time.Hour)); err == nil {
		m.liveMu.Lock()
		for index, obs := range latest {
			m.latest[index] = mergeLatest(m.latest[index], obs)
		}
		m.liveMu.Unlock()
	} else {
		fail(err)
	}
	for _, index := range indexes {
		account := accounts[index]
		if period, ok, err := m.host.WeeklyPeriod(index, m.settings.accountSource(index), now); err == nil {
			account.weekly, account.hasWeek = period, ok
		} else {
			// Keep the last known period rather than forgetting it.
			fail(err)
			if previous != nil {
				old := previous.accounts[index]
				account.weekly, account.hasWeek = old.weekly, old.hasWeek
			}
		}
		accounts[index] = account
	}
	snap := &snapshot{at: now, settings: m.settings, accounts: accounts, keys: map[string]keySnapshot{}, weights: weights}
	// included is, per key, the usage sequence number every read of the key
	// includes; live usage at or below it is already counted.
	included := map[string]uint64{}
	for scope, settings := range m.settings.Keys {
		pool, ok := m.settings.pool(settings.PoolID)
		if !ok {
			continue
		}
		var old keySnapshot
		if previous != nil {
			old = previous.keys[scope]
		}
		key := keySnapshot{}
		key.info, key.known = m.host.KeyInfo(scope)
		seq := key.info.Seq
		latest := m.latestFor(pool)
		switch settings.Kind {
		case KeyFixed:
			if latest.Has5h && latest.Reset5h.After(now) {
				key.fiveHourStart = latest.Reset5h.Add(-FiveHours)
				used, usedSeq, err := m.host.KeyUsageUSD(scope, key.fiveHourStart, now)
				switch {
				case err == nil:
					key.usedFiveHour, key.fiveHourSeq = used, usedSeq
				case old.fiveHourStart.Equal(key.fiveHourStart):
					// Keep the last successful read of the same window.
					fail(err)
					key.usedFiveHour, key.fiveHourSeq = old.usedFiveHour, old.fiveHourSeq
				default:
					fail(err)
				}
				seq = min(seq, key.fiveHourSeq)
			}
		case KeyRemainder:
			seq = 0
			if settings.WeeklyCapUSD != nil {
				key.capFrom = now.Add(-Week)
				if latest.Has7d && latest.Reset7d.After(now) {
					key.capFrom = latest.Reset7d.Add(-Week)
				}
				used, usedSeq, err := m.host.KeyUsageUSD(scope, key.capFrom, now)
				switch {
				case err == nil:
					key.usedCap, key.capSeq = used, usedSeq
				case !old.capFrom.IsZero() && absDuration(old.capFrom.Sub(key.capFrom)) <= boundaryJitter:
					fail(err)
					key.usedCap, key.capSeq = old.usedCap, old.capSeq
				default:
					fail(err)
				}
				seq = key.capSeq
			}
		}
		included[scope] = seq
		snap.keys[scope] = key
	}
	m.liveMu.Lock()
	m.pruneDeltasLocked(included, now)
	dropped := m.dropped
	m.dropped = false
	m.liveMu.Unlock()
	if dropped {
		m.host.Logf("CC live usage exceeded %d requests for a key between refreshes; the oldest were dropped", maxDeltasPerKey)
	}
	m.snap.Store(snap)
	m.lastRefresh = now
	m.logSharedPlans(snap)
	if !failed {
		m.lastError.Store("")
	}
	return nil
}

// latestFor is the live report of a pool's anchor account.
func (m *Manager) latestFor(pool Pool) Observation {
	if len(pool.AuthIndexes) == 0 {
		return Observation{}
	}
	m.liveMu.RLock()
	defer m.liveMu.RUnlock()
	return m.latest[pool.AuthIndexes[0]]
}

func (m *Manager) recordError(err error) {
	if err.Error() != m.lastError.Load().(string) {
		m.host.Logf("CC estimation failed: %v", err)
	}
	m.lastError.Store(err.Error())
}

// advance processes an account's history in chunks of at most a day until
// until, or until the deadline passes. Segments and k evidence keep separate
// cursors, so k can be rebuilt without touching the segments.
func (m *Manager) advance(db *sql.DB, authIndex string, cache *accountCache, until, deadline time.Time) error {
	params := m.settings.accountParams(authIndex)
	source := m.settings.accountSource(authIndex)
	if cache.stream.Cursor.IsZero() || cache.stream.K.Cursor.IsZero() {
		var first sql.NullInt64
		if err := db.QueryRow(`SELECT min(at) FROM quota_report_windows WHERE auth_index = ? AND provider = ?`,
			authIndex, source.Provider).Scan(&first); err != nil {
			return fmt.Errorf("Read quota reports: %w", err)
		}
		if !first.Valid {
			return nil
		}
		start := timeAt(first.Int64).Add(-time.Nanosecond)
		if cache.stream.Cursor.IsZero() {
			cache.stream.Cursor = start
		}
		if cache.stream.K.Cursor.IsZero() {
			cache.stream.K.Cursor = start
		}
	}
	for {
		from := cache.stream.Cursor
		if cache.stream.K.Cursor.Before(from) {
			from = cache.stream.K.Cursor
		}
		if !from.Before(until) {
			return nil
		}
		end := from.Add(24 * time.Hour)
		if end.After(until) {
			end = until
		}
		observations, err := readObservations(db, source, authIndex, from, end)
		if err != nil {
			return err
		}
		next := cache.stream
		var segments []Segment
		if next.Cursor.Before(end) {
			costs, err := readCosts(db, source, params.rules, authIndex, next.Cursor, end, m.host.ToUSD)
			if err != nil {
				return err
			}
			segments = next.Advance(observations, costs, end, params)
		}
		var buckets []KBucket
		if next.K.Cursor.Before(end) {
			buckets = BucketEvidence(next.K.Advance(observations, end))
		}
		if err := commitAdvance(db, authIndex, next, segments, buckets); err != nil {
			return err
		}
		if next.restarted {
			cache.segments = nil
			next.restarted = false
		}
		cache.stream = next
		cache.segments = append(cache.segments, segments...)
		cache.buckets = mergeBuckets(cache.buckets, buckets)
		if time.Now().After(deadline) {
			return nil
		}
	}
}

// live returns copies of the live reports and deltas.
func (m *Manager) live(_ *snapshot) (map[string]Observation, map[string][]costDelta) {
	m.liveMu.RLock()
	defer m.liveMu.RUnlock()
	latest := make(map[string]Observation, len(m.latest))
	for index, obs := range m.latest {
		latest[index] = obs
	}
	deltas := make(map[string][]costDelta, len(m.deltas))
	for scope, items := range m.deltas {
		deltas[scope] = slices.Clone(items)
	}
	return latest, deltas
}

// deltaSum adds the live usage in [from, to) that a read including usage up
// to seq has not counted. A zero to has no end.
func deltaSum(deltas []costDelta, seq uint64, from, to time.Time) float64 {
	sum := 0.0
	for _, delta := range deltas {
		if delta.seq > seq && !delta.at.Before(from) && (to.IsZero() || delta.at.Before(to)) {
			sum += delta.usd
		}
	}
	return sum
}

func accountStatus(snap *snapshot, latest map[string]Observation, authIndex string, now time.Time) AccountStatus {
	account := snap.accounts[authIndex]
	provider := snap.settings.AccountProvider(authIndex)
	params := snap.settings.ParamsFor(provider)
	status := ResolveAccount(authIndex, snap.settings.Accounts[authIndex], account.estimate, account.k, latest[authIndex], params, now)
	status.Provider = provider
	if params.rules.PauseWhenExhausted {
		live := latest[authIndex]
		current := live
		// A window whose reset passed is no longer used up.
		current.Has5h = live.Has5h && (live.Reset5h.IsZero() || live.Reset5h.After(now))
		current.Has7d = live.Has7d && (live.Reset7d.IsZero() || live.Reset7d.After(now))
		switch {
		case current.exhausted():
			status.Paused, status.PauseReason = true, PauseLimitReached
			if account.paused {
				status.PausedSince = account.pausedSince
			}
		case account.paused && (account.pauseReason == PauseCredits || !live.At.After(account.cursor)):
			// The history is processed up to the cursor; a newer live
			// report below the limit means the account recovered since.
			status.Paused, status.PauseReason, status.PausedSince = true, account.pauseReason, account.pausedSince
		}
	}
	return status
}

// poolStatus reads only the snapshot and the live state.
func poolStatus(snap *snapshot, latest map[string]Observation, deltas map[string][]costDelta, pool Pool, now time.Time) PoolStatus {
	accounts := make([]AccountStatus, 0, len(pool.AuthIndexes))
	for _, index := range pool.AuthIndexes {
		accounts = append(accounts, accountStatus(snap, latest, index, now))
	}
	var fiveHourStart time.Time
	if len(accounts) > 0 && accounts[0].Reset5h.After(now) {
		fiveHourStart = accounts[0].Reset5h.Add(-FiveHours)
	}
	var fixed []FixedKeyUsage
	for _, scope := range sortedKeys(snap.settings.Keys) {
		settings := snap.settings.Keys[scope]
		if settings.Kind != KeyFixed || settings.PoolID != pool.ID {
			continue
		}
		usage := FixedKeyUsage{Scope: scope, FiveHourShare: settings.FiveHourShare}
		key := snap.keys[scope]
		if info := key.info; key.known && info.HasWeekly {
			usage.Valid, usage.WeeklyUSD, usage.CycleStart, usage.CycleEnd = true, info.WeeklyUSD, info.CycleStart, info.CycleEnd
			if !info.CycleStart.After(now) && now.Before(info.CycleEnd) {
				usage.UsedWeekUSD = info.UsedWeekUSD + deltaSum(deltas[scope], info.Seq, info.CycleStart, info.CycleEnd)
			}
			if !fiveHourStart.IsZero() {
				seq := uint64(0)
				if fiveHourStart.Equal(key.fiveHourStart) {
					usage.UsedFiveHour, seq = key.usedFiveHour, key.fiveHourSeq
				}
				usage.UsedFiveHour += deltaSum(deltas[scope], seq, fiveHourStart, time.Time{})
			}
		}
		fixed = append(fixed, usage)
	}
	return BuildPool(pool, accounts, fixed)
}

// remainderCapUsage is a remainder key's usage in the anchor account's
// current 7-day period, or the last 7 days when it is unknown.
func remainderCapUsage(snap *snapshot, deltas map[string][]costDelta, scope string, pool PoolStatus, now time.Time) float64 {
	from := now.Add(-Week)
	if pool.Reset7d.After(now) {
		from = pool.Reset7d.Add(-Week)
	}
	key := snap.keys[scope]
	used, seq := 0.0, uint64(0)
	if !key.capFrom.IsZero() && absDuration(key.capFrom.Sub(from)) <= boundaryJitter {
		used, seq = key.usedCap, key.capSeq
	}
	return used + deltaSum(deltas[scope], seq, from, time.Time{})
}

// alignFixed keeps a fixed key's weekly plan window on the anchor account's
// 7-day reset before quota admission can open a cycle. It only reads memory
// unless the anchor has to move.
func (m *Manager) alignFixed(snap *snapshot, scope string, pool Pool) Alignment {
	if len(pool.AuthIndexes) == 0 {
		return Alignment{Status: AlignUnknown}
	}
	key := snap.keys[scope]
	if !key.known || key.info.PlanID == "" || !key.info.HasWeekly {
		return Alignment{Status: AlignNoWindow}
	}
	account := snap.accounts[pool.AuthIndexes[0]]
	if !account.hasWeek || account.weekly.ResetAt.IsZero() {
		return Alignment{Status: AlignUnknown}
	}
	cacheKey := key.info.PlanID + "\x00" + account.weekly.ResetAt.Format(time.RFC3339)
	now := m.host.Now()
	m.alignMu.Lock()
	defer m.alignMu.Unlock()
	if previous, ok := m.alignments[cacheKey]; ok && previous.current(now, key.info.PlanRevision) {
		return previous.result
	}
	result, err := m.host.AlignWeekly(key.info.PlanID, account.weekly.ResetAt)
	if err != nil {
		m.recordError(err)
		return Alignment{Status: AlignUnknown}
	}
	if result.Status == AlignChanged {
		result.Status = AlignAligned
	}
	m.alignments[cacheKey] = alignmentEntry{result: result, revision: key.info.PlanRevision}
	return result
}

// alignmentEntry caches an alignment so fixed keys' requests do not ask the
// store every time: an aligned plan stays aligned for that reset until the
// plan changes, a waiting one until its running cycles end, and an
// unsupported one until its plan or the settings change.
type alignmentEntry struct {
	result   Alignment
	revision string
}

func (e alignmentEntry) current(now time.Time, revision string) bool {
	switch e.result.Status {
	case AlignAligned:
		// An edited plan may have lost its anchor, so it is checked again.
		return e.revision == revision
	case AlignWaiting:
		return now.Before(e.result.WaitUntil)
	case AlignUnsupported:
		return e.revision == revision
	}
	return false
}

// Admit decides whether the key may send a request now. Keys outside every
// pool return at once; fixed keys are limited by their subscription plan.
// Before the first snapshot every key is admitted.
func (m *Manager) Admit(scope string) Decision {
	snap := m.snap.Load()
	if snap == nil {
		return m.admitUnloaded(scope)
	}
	settings, ok := snap.settings.Keys[scope]
	if !ok {
		return Decision{Allowed: true}
	}
	pool, found := snap.settings.pool(settings.PoolID)
	if !found {
		return Decision{Allowed: true}
	}
	now := m.host.Now()
	if settings.Kind == KeyFixed {
		m.alignFixed(snap, scope, pool)
		return Decision{Allowed: true}
	}
	latest, deltas := m.live(snap)
	status := poolStatus(snap, latest, deltas, pool, now)
	used := 0.0
	if settings.WeeklyCapUSD != nil {
		used = remainderCapUsage(snap, deltas, scope, status, now)
	}
	return status.AdmitRemainder(settings.WeeklyCapUSD, used)
}

// Balance outcomes.
const (
	BalanceOK = iota
	BalanceUnassigned
	BalanceNotReady
)

// Balance is a key's weekly balance in USD.
func (m *Manager) Balance(scope string) (float64, int) {
	snap := m.snap.Load()
	if snap == nil {
		return 0, BalanceNotReady
	}
	settings, ok := snap.settings.Keys[scope]
	if !ok {
		return 0, BalanceUnassigned
	}
	pool, found := snap.settings.pool(settings.PoolID)
	if !found {
		return 0, BalanceUnassigned
	}
	now := m.host.Now()
	latest, deltas := m.live(snap)
	if settings.Kind == KeyFixed {
		m.alignFixed(snap, scope, pool)
		for _, fixed := range poolStatus(snap, latest, deltas, pool, now).Fixed {
			if fixed.Scope == scope {
				return fixed.WeeklyUSD - fixed.UsedWeekUSD, BalanceOK
			}
		}
		return 0, BalanceOK
	}
	status := poolStatus(snap, latest, deltas, pool, now)
	used := 0.0
	if settings.WeeklyCapUSD != nil {
		used = remainderCapUsage(snap, deltas, scope, status, now)
	}
	return status.RemainderKeyBalance(settings.WeeklyCapUSD, used), BalanceOK
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
	Balance        float64          `json:"balance_usd"`
	Reported       float64          `json:"reported_balance_usd"`
	Admitted       *bool            `json:"admitted,omitempty"`
	Refusal        string           `json:"refusal,omitempty"`
	RefusalMessage messages.Message `json:"refusal_message,omitzero"`
	RetryAt        time.Time        `json:"retry_at,omitzero"`
}

// AccountView adds an account's history to its status.
type AccountView struct {
	AccountStatus
	Cursor       time.Time      `json:"cursor,omitzero"`
	KCursor      time.Time      `json:"k_cursor,omitzero"`
	WeeklyStart  time.Time      `json:"weekly_start,omitzero"`
	EarlyResetAt time.Time      `json:"early_reset_at,omitzero"`
	RecentBlocks []BlockSummary `json:"recent_blocks"`
	// Excluded is the cost the provider's segment rules left out.
	Excluded ExcludedCost `json:"excluded"`
}

// View is the CC page.
type View struct {
	Settings Settings `json:"settings"`
	Defaults Params   `json:"defaults"`
	// DefaultProviders are the built-in provider profiles.
	DefaultProviders map[string]ProviderProfile `json:"default_providers"`
	Accounts         []AccountView              `json:"accounts"`
	Pools            []PoolStatus               `json:"pools"`
	Keys             []KeyStatus                `json:"keys"`
	Segments         int                        `json:"stored_segments"`
	// Weights is each provider's model quota weights.
	Weights map[string]WeightsView `json:"weights"`
	Error   string                 `json:"error,omitempty"`
	// Warnings are problems the settings check cannot prevent, such as a
	// fixed key's plan switched to a shared quota after the settings saved.
	Warnings []messages.Message `json:"warnings"`
	At       time.Time          `json:"at"`
	// Loaded is false while no estimate snapshot exists.
	Loaded bool `json:"loaded"`
}

// View refreshes and describes every account, pool and assigned key. It is a
// management call, so it may wait for a running refresh.
func (m *Manager) View() (View, error) {
	refreshErr := m.Refresh()
	snap := m.snap.Load()
	now := m.host.Now()
	if snap == nil {
		// Loaded settings without estimates show as an explicit "not loaded" state.
		settings := m.loaded.Load()
		if settings == nil {
			if refreshErr == nil {
				refreshErr = fmt.Errorf("CC estimates are not loaded")
			}
			return View{}, refreshErr
		}
		return View{Settings: *settings, Defaults: DefaultGlobalParams(), DefaultProviders: DefaultProviders(), Accounts: []AccountView{}, Pools: []PoolStatus{}, Keys: []KeyStatus{},
			Weights: emptyWeightsViews(), Warnings: []messages.Message{}, Error: m.lastError.Load().(string), At: now}, nil
	}
	view := View{Settings: snap.settings, Defaults: DefaultGlobalParams(), DefaultProviders: DefaultProviders(), Accounts: []AccountView{}, Pools: []PoolStatus{}, Keys: []KeyStatus{},
		Error: m.lastError.Load().(string), At: now, Loaded: true}
	m.refreshMu.Lock()
	_ = m.withDB(func(db *sql.DB) error {
		view.Segments, _ = segmentStats(db)
		return nil
	})
	m.refreshMu.Unlock()
	latest, deltas := m.live(snap)
	view.Weights = weightsViews(snap, latest, now)
	for _, index := range snap.settings.authIndexes() {
		account := AccountView{AccountStatus: accountStatus(snap, latest, index, now), RecentBlocks: []BlockSummary{}}
		cached := snap.accounts[index]
		account.Cursor, account.KCursor, account.Excluded = cached.cursor, cached.kCursor, cached.excluded
		if cached.hasWeek {
			account.WeeklyStart, account.EarlyResetAt = cached.weekly.Start, cached.weekly.EarlyResetAt
		}
		blocks := cached.estimate.Blocks
		account.RecentBlocks = slices.Clone(blocks[max(0, len(blocks)-20):])
		slices.Reverse(account.RecentBlocks)
		view.Accounts = append(view.Accounts, account)
	}
	pools := map[string]PoolStatus{}
	for _, pool := range snap.settings.Pools {
		status := poolStatus(snap, latest, deltas, pool, now)
		pools[pool.ID] = status
		view.Pools = append(view.Pools, status)
	}
	for _, scope := range sortedKeys(snap.settings.Keys) {
		settings := snap.settings.Keys[scope]
		key := KeyStatus{Scope: scope, Settings: settings}
		if cached := snap.keys[scope]; cached.known {
			key.Preview, key.Label, key.PlanID = cached.info.Preview, cached.info.Label, cached.info.PlanID
		}
		pool := pools[settings.PoolID]
		poolSettings, _ := snap.settings.pool(settings.PoolID)
		if settings.Kind == KeyFixed {
			alignment := m.alignFixed(snap, scope, poolSettings)
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
				key.UsedCapUSD = remainderCapUsage(snap, deltas, scope, pool, now)
			}
			key.Balance = pool.RemainderKeyBalance(settings.WeeklyCapUSD, key.UsedCapUSD)
			decision := pool.AdmitRemainder(settings.WeeklyCapUSD, key.UsedCapUSD)
			key.Admitted, key.Refusal, key.RetryAt = &decision.Allowed, decision.Reason, decision.RetryAt
			key.RefusalMessage = decision.Detail
		}
		key.Reported = ReportedBalance(key.Balance)
		view.Keys = append(view.Keys, key)
	}
	view.Warnings = sharedPlanWarnings(snap)
	return view, nil
}

// AccountSummary is a pooled account's estimate as the auth files page shows it.
type AccountSummary struct {
	Pools             []string `json:"pools"`
	Provider          string   `json:"provider"`
	SegmentWindow     string   `json:"segment_window"`
	NeedsK            bool     `json:"needs_k"`
	Paused            bool     `json:"paused"`
	Tier              string   `json:"tier"`
	FiveHourTotal     float64  `json:"five_hour_total_usd"`
	WeeklyTotal       float64  `json:"weekly_total_usd"`
	TotalsFrom        string   `json:"totals_from"`
	K                 *float64 `json:"k,omitempty"`
	KSource           string   `json:"k_source"`
	RemainingWeek     float64  `json:"remaining_week_usd"`
	RemainingFiveHour float64  `json:"remaining_five_hour_usd"`
	ColdStart         bool     `json:"cold_start"`
	CleanBlocks       int      `json:"clean_blocks"`
	ColdStartBlocks   int      `json:"cold_start_blocks"`
}

// AccountSummaries describes the accounts of every pool from the latest
// snapshot and live reports. It never refreshes or touches the database, and
// returns nil before the first snapshot.
func (m *Manager) AccountSummaries() map[string]AccountSummary {
	snap := m.snap.Load()
	if snap == nil {
		return nil
	}
	now := m.host.Now()
	latest, _ := m.live(snap)
	result := map[string]AccountSummary{}
	for _, pool := range snap.settings.Pools {
		name := pool.Name
		if name == "" {
			name = pool.ID
		}
		for _, index := range pool.AuthIndexes {
			summary, ok := result[index]
			if !ok {
				status := accountStatus(snap, latest, index, now)
				summary = AccountSummary{Provider: status.Provider, SegmentWindow: segmentWindow(status.Window), NeedsK: status.NeedsK, Paused: status.Paused, Tier: status.Tier, FiveHourTotal: status.FiveHourTotal, WeeklyTotal: status.WeeklyTotal,
					TotalsFrom: status.TotalsFrom, K: status.K, KSource: status.KSource, RemainingWeek: status.RemainingWeek,
					RemainingFiveHour: status.RemainingFiveHour, ColdStart: status.ColdStart, CleanBlocks: status.CleanBlocks,
					ColdStartBlocks: snap.settings.ParamsFor(status.Provider).ColdStartBlocks}
			}
			summary.Pools = append(summary.Pools, name)
			result[index] = summary
		}
	}
	return result
}

// segmentWindow names the window an estimate's segments measure.
func segmentWindow(window string) string {
	if window == WindowWeek {
		return WindowWeek
	}
	return WindowFiveHour
}

// admitUnloaded decides before the first snapshot. Once the settings are
// loaded, remainder keys are refused (unless the operator turned that off)
// because their reservations cannot be computed; fixed keys and keys outside
// every pool are admitted. Without settings every key is admitted.
func (m *Manager) admitUnloaded(scope string) Decision {
	settings := m.loaded.Load()
	if settings == nil {
		if !m.loggedUnloaded.Swap(true) {
			m.host.Logf("CC settings are not loaded yet; admitting requests without CC reservations")
		}
		return Decision{Allowed: true}
	}
	key, ok := settings.Keys[scope]
	if !ok || key.Kind != KeyRemainder || !settings.Params.RefuseUnloaded {
		return Decision{Allowed: true}
	}
	if _, found := settings.pool(key.PoolID); !found {
		return Decision{Allowed: true}
	}
	return refusal(RefusedNotLoaded, m.host.Now().Add(time.Minute),
		"API key blocked because the CC estimates have not loaded yet; remainder keys wait until the pool's reservations can be computed.")
}

// checkSharedPlans refuses fixed keys that share a subscription plan across
// pools: aligning the plan to two anchor accounts would move its cycles back
// and forth. It also refuses fixed keys on a shared quota plan: each fixed
// key reserves its own weekly balance, while every member of a shared plan
// reports the whole pool's.
func (m *Manager) checkSharedPlans(settings Settings) error {
	type owner struct{ scope, pool string }
	plans := map[string]owner{}
	for _, scope := range sortedKeys(settings.Keys) {
		key := settings.Keys[scope]
		if key.Kind != KeyFixed {
			continue
		}
		info, known := m.host.KeyInfo(scope)
		if !known || info.PlanID == "" {
			continue
		}
		if info.SharedQuota {
			return messages.Errorf("CC fixed key %s is bound to shared quota plan %q; give it a plan without shared quota",
				keyLabel(scope, m.host), info.PlanID)
		}
		previous, seen := plans[info.PlanID]
		if !seen {
			plans[info.PlanID] = owner{scope, key.PoolID}
			continue
		}
		if previous.pool != key.PoolID {
			return messages.Errorf("Fixed keys %s and %s share subscription plan %q but belong to different CC pools; give each pool its own plan",
				keyLabel(previous.scope, m.host), keyLabel(scope, m.host), info.PlanID)
		}
	}
	return nil
}

func keyLabel(scope string, host Host) string {
	if info, ok := host.KeyInfo(scope); ok && info.Preview != "" {
		return info.Preview
	}
	return scope
}

// AccountSource is where a configured account's main window reports come
// from, as the loaded settings say; false when the account is not configured.
func (m *Manager) AccountSource(authIndex string) (QuotaSource, bool) {
	settings := m.loaded.Load()
	if settings == nil || !slices.Contains(settings.authIndexes(), authIndex) {
		return QuotaSource{}, false
	}
	return settings.accountSource(authIndex), true
}

// KeyKind is the key's type in the loaded settings, or "" when unassigned.
func (m *Manager) KeyKind(scope string) string {
	if settings := m.loaded.Load(); settings != nil {
		return settings.Keys[scope].Kind
	}
	return ""
}

// sharedFixedKeys lists the fixed keys whose plan shares one quota across its
// keys, by scope. Settings with such a key cannot be saved, but a plan can
// still be switched afterwards or come from settings saved before the check.
// Each of them then reports the whole pool's weekly amounts, so the pool's
// reservations count that pool once per such key: overstated, never short.
func sharedFixedKeys(snap *snapshot) map[string]KeyInfo {
	result := map[string]KeyInfo{}
	for scope, settings := range snap.settings.Keys {
		if key := snap.keys[scope]; settings.Kind == KeyFixed && key.known && key.info.SharedQuota {
			result[scope] = key.info
		}
	}
	return result
}

func sharedPlanWarnings(snap *snapshot) []messages.Message {
	shared := sharedFixedKeys(snap)
	warnings := []messages.Message{}
	for _, scope := range sortedKeys(shared) {
		info := shared[scope]
		warnings = append(warnings, messages.New("CC fixed key %s is bound to shared quota plan %q; its weekly reservation counts the whole shared pool and overstates the pool's reservations. Bind it to a plan without shared quota.", fixedKeyLabel(scope, info), info.PlanID))
	}
	return warnings
}

// logSharedPlans logs each fixed key once when its plan becomes shared.
func (m *Manager) logSharedPlans(snap *snapshot) {
	shared := sharedFixedKeys(snap)
	for _, scope := range sortedKeys(shared) {
		if info := shared[scope]; m.sharedLogged[scope] != info.PlanID {
			m.host.Logf("CC fixed key %s is bound to shared quota plan %q; its weekly reservation counts the whole shared pool", fixedKeyLabel(scope, info), info.PlanID)
		}
	}
	m.sharedLogged = map[string]string{}
	for scope, info := range shared {
		m.sharedLogged[scope] = info.PlanID
	}
}

func fixedKeyLabel(scope string, info KeyInfo) string {
	if info.Preview != "" {
		return info.Preview
	}
	return scope
}

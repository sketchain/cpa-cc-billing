package ccbilling

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-super-billing/internal/messages"

	_ "github.com/mattn/go-sqlite3"
)

// fakeHost serves a real SQLite database with the billing tables the manager
// reads, counts every call, and can hold WithDB to imitate a long backfill.
type fakeHost struct {
	db    *sql.DB
	now   time.Time
	calls atomic.Int64
	// gate, when set, blocks WithDB until it is closed.
	gate    atomic.Pointer[chan struct{}]
	entered chan struct{}
	keys    map[string]KeyInfo
	// weekly and align configure WeeklyPeriod and AlignWeekly; aligns counts
	// AlignWeekly calls.
	weekly *WeeklyPeriod
	align  Alignment
	aligns atomic.Int64
	// usage, seq and usageErr configure KeyUsageUSD; weeklyErr fails WeeklyPeriod.
	usage     map[string]float64
	seq       uint64
	usageErr  error
	weeklyErr error
	dbErr     error
	logs      []string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE request_events (id INTEGER PRIMARY KEY, at INTEGER, scope TEXT NOT NULL DEFAULT '', auth_index TEXT, latency_ms INTEGER, total_cost REAL, currency TEXT,
			billing_model TEXT NOT NULL DEFAULT '', upstream_model TEXT NOT NULL DEFAULT '', price_source TEXT NOT NULL DEFAULT '');
		CREATE TABLE quota_reports (request_event_id INTEGER PRIMARY KEY, auth_index TEXT, provider TEXT, at INTEGER,
			plan TEXT NOT NULL DEFAULT '', credit_balance TEXT NOT NULL DEFAULT '', credits_unlimited INTEGER);
		CREATE TABLE quota_report_windows (request_event_id INTEGER, limit_key TEXT, seconds INTEGER, auth_index TEXT, provider TEXT,
			at INTEGER, used_percent REAL, reset_at INTEGER);`); err != nil {
		t.Fatal(err)
	}
	return &fakeHost{db: db, now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), entered: make(chan struct{}, 16), keys: map[string]KeyInfo{},
		usage: map[string]float64{}}
}

func (h *fakeHost) WithDB(fn func(*sql.DB) error) error {
	h.calls.Add(1)
	if h.dbErr != nil {
		return h.dbErr
	}
	if gate := h.gate.Load(); gate != nil {
		h.entered <- struct{}{}
		<-*gate
	}
	return fn(h.db)
}
func (h *fakeHost) Now() time.Time                         { h.calls.Add(1); return h.now }
func (h *fakeHost) ToUSD(amount float64, _ string) float64 { return amount }
func (h *fakeHost) LatestObservations(map[string]QuotaSource, time.Time) (map[string]Observation, error) {
	h.calls.Add(1)
	return map[string]Observation{}, nil
}
func (h *fakeHost) WeeklyPeriod(string, QuotaSource, time.Time) (WeeklyPeriod, bool, error) {
	h.calls.Add(1)
	if h.weeklyErr != nil {
		return WeeklyPeriod{}, false, h.weeklyErr
	}
	if h.weekly != nil {
		return *h.weekly, true, nil
	}
	return WeeklyPeriod{}, false, nil
}
func (h *fakeHost) KeyInfo(scope string) (KeyInfo, bool) {
	h.calls.Add(1)
	info, ok := h.keys[scope]
	if info.Seq == 0 {
		info.Seq = h.seq
	}
	return info, ok
}
func (h *fakeHost) KeyUsageUSD(scope string, _, _ time.Time) (float64, uint64, error) {
	h.calls.Add(1)
	if h.usageErr != nil {
		return 0, 0, h.usageErr
	}
	return h.usage[scope], h.seq, nil
}
func (h *fakeHost) AlignWeekly(string, time.Time) (Alignment, error) {
	h.calls.Add(1)
	h.aligns.Add(1)
	if h.align.Status == "" {
		return Alignment{Status: AlignAligned}, nil
	}
	return h.align, nil
}
func (h *fakeHost) Logf(format string, args ...any) {
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
}

func configuredManager(t *testing.T) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, CycleStart: host.now.Add(-time.Hour), CycleEnd: host.now.Add(48 * time.Hour)}
	host.keys["remainder"] = KeyInfo{}
	manager := NewManager(host)
	manager.RefreshInterval = 0
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "p", AuthIndexes: []string{"a1"}}}
	settings.Keys["fixed"] = KeySettings{Kind: KeyFixed, PoolID: "p"}
	settings.Keys["remainder"] = KeySettings{Kind: KeyRemainder, PoolID: "p"}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	manager.Observe("a1", Observation{At: host.now, Has7d: true, Used7d: 95, Reset7d: host.now.Add(48 * time.Hour)})
	return manager, host
}

func TestAdmissionIsNotBlockedByARunningRefresh(t *testing.T) {
	manager, host := configuredManager(t)
	gate := make(chan struct{})
	host.gate.Store(&gate)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		_ = manager.Refresh() // holds the refresh lock while "backfilling"
	}()
	<-host.entered

	var wg sync.WaitGroup
	start := time.Now()
	decisions := make([]Decision, 64)
	for i := range decisions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				decisions[i] = manager.Admit("unassigned")
			case 1:
				decisions[i] = manager.Admit("fixed")
			case 2:
				decisions[i] = manager.Admit("remainder")
			default:
				manager.MaybeRefresh() // a usage callback during the backfill skips it
				manager.ObserveCost("fixed", host.now, 1, 1)
				decisions[i] = manager.Admit("remainder")
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("admission waited %v for the refresh", elapsed)
	}
	select {
	case <-refreshed:
		t.Fatal("the refresh finished before the gate opened")
	default:
	}
	for i, decision := range decisions {
		wantAllowed := i%4 == 0 || i%4 == 1
		if decision.Allowed != wantAllowed {
			t.Fatalf("decision %d = %+v", i, decision)
		}
		if !wantAllowed && decision.Reason != RefusedWeekly {
			t.Fatalf("decision %d reason = %q", i, decision.Reason)
		}
	}
	close(gate)
	<-refreshed
	host.gate.Store(nil)
}

func TestUnassignedKeysNeverTouchTheEstimator(t *testing.T) {
	manager, host := configuredManager(t)
	before := host.calls.Load()
	for range 100 {
		if decision := manager.Admit("unassigned"); !decision.Allowed {
			t.Fatalf("unassigned key refused: %+v", decision)
		}
		if _, status := manager.Balance("unassigned"); status != BalanceUnassigned {
			t.Fatalf("unassigned balance status = %d", status)
		}
		manager.ObserveCost("unassigned", host.now, 1, 1)
	}
	if after := host.calls.Load(); after != before {
		t.Fatalf("unassigned keys made %d host calls", after-before)
	}
}

func TestAdmissionBeforeTheFirstSnapshotAdmits(t *testing.T) {
	host := newFakeHost(t)
	manager := NewManager(host)
	if decision := manager.Admit("anything"); !decision.Allowed {
		t.Fatalf("decision = %+v", decision)
	}
	if _, status := manager.Balance("anything"); status != BalanceNotReady {
		t.Fatalf("balance status = %d", status)
	}
}

func TestLiveCostsReachFixedKeyUsageWithoutARefresh(t *testing.T) {
	manager, host := configuredManager(t)
	manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: 10, Reset5h: host.now.Add(time.Hour)})
	manager.RefreshInterval = time.Hour
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	manager.ObserveCost("fixed", host.now, 30, 1)
	manager.MaybeRefresh() // throttled: the snapshot stays
	balance, status := manager.Balance("fixed")
	if status != BalanceOK || balance != 70 {
		t.Fatalf("fixed balance = %v (%d)", balance, status)
	}
	// A refresh that reads the stored usage drops the delta it covers.
	host.seq = 1
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, UsedWeekUSD: 30, CycleStart: host.now.Add(-time.Hour), CycleEnd: host.now.Add(48 * time.Hour)}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	if balance, _ := manager.Balance("fixed"); balance != 70 {
		t.Fatalf("fixed balance after refresh = %v", balance)
	}
}

// Version 1 streams summed k from the start; upgrading rebuilds bucketed k
// evidence from the retained reports and keeps the segments as they are.
func TestUpgradeRebuildsKEvidenceAndKeepsSegments(t *testing.T) {
	host := newFakeHost(t)
	start := host.now.Add(-48 * time.Hour)
	reset5, reset7 := start.Add(5*time.Hour), start.Add(6*24*time.Hour)
	for i := 0; i < 240; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		used5, used7 := float64(i/2), float64(i/12)
		for _, row := range []struct {
			seconds int64
			used    float64
			reset   time.Time
		}{{18000, used5, reset5}, {604800, used7, reset7}} {
			if _, err := host.db.Exec(`INSERT INTO quota_report_windows VALUES (?, 'unified', ?, 'a1', 'claude', ?, ?, ?)`,
				i, row.seconds, nanos(at), row.used, nanos(row.reset)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := host.db.Exec(ccMigrations[0]); err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "p", AuthIndexes: []string{"a1"}}}
	raw := `{"params":{"segment_retention_days":365,"block_gap_minutes":5,"window_percent":8,"reference_mode":"global","suspicious_threshold":0.2,` +
		`"exclude_vote_share":0.5,"weight_power":2,"recent_days":14,"recent_share":0.9,"k_min_weekly_points":10,"cold_start_blocks":20,` +
		`"tier_weekly_usd":{"pro":250,"max5x":1250,"max20x":2500},"safety_factor":0.9},"pools":[{"id":"p","name":"","auth_indexes":["a1"]}],"accounts":{},"keys":{}}`
	cursor := host.now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := host.db.Exec(`CREATE TABLE cc_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO cc_meta VALUES ('schema_version', '1'), ('settings', ?);
		INSERT INTO cc_streams VALUES ('a1', ?);
		INSERT INTO cc_segments (auth_index, block_at, at, cost_usd, gain) VALUES ('a1', ?, ?, 0.5, 1), ('a1', ?, ?, 0.4, 1);`,
		raw, `{"cursor":"`+cursor+`","k_sum_5h":999,"k_sum_7d":1}`, nanos(start), nanos(start.Add(time.Minute)), nanos(start), nanos(start.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(host)
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	var segments, buckets int
	if err := host.db.QueryRow(`SELECT (SELECT count(*) FROM cc_segments), (SELECT count(*) FROM cc_k_buckets)`).Scan(&segments, &buckets); err != nil {
		t.Fatal(err)
	}
	if segments != 2 || buckets == 0 {
		t.Fatalf("segments = %d, k buckets = %d", segments, buckets)
	}
	k := manager.snap.Load().accounts["a1"].k
	if k.K == nil || math.Abs(*k.K-6) > 0.6 {
		t.Fatalf("rebuilt k = %+v", k)
	}
}

func TestAlignmentResultsAreCached(t *testing.T) {
	manager, host := configuredManager(t)
	host.weekly = &WeeklyPeriod{Start: host.now.Add(-24 * time.Hour), ResetAt: host.now.Add(6 * 24 * time.Hour)}
	waitUntil := host.now.Add(2 * time.Hour)
	host.align = Alignment{Status: AlignWaiting, WaitUntil: waitUntil}
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, PlanRevision: "r1",
		CycleStart: host.now.Add(-time.Hour), CycleEnd: waitUntil}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	admitFixed := func(times int) {
		for range times {
			if decision := manager.Admit("fixed"); !decision.Allowed {
				t.Fatalf("fixed key refused: %+v", decision)
			}
		}
	}

	// Waiting: asked once, then cached until the running cycle ends.
	admitFixed(5)
	if calls := host.aligns.Load(); calls != 1 {
		t.Fatalf("waiting: AlignWeekly calls = %d, want 1", calls)
	}
	host.now = waitUntil
	host.align = Alignment{Status: AlignAligned}
	admitFixed(5)
	if calls := host.aligns.Load(); calls != 2 {
		t.Fatalf("after the wait: AlignWeekly calls = %d, want 2", calls)
	}

	// Unsupported: cached until the plan changes or the settings are saved.
	manager2, host2 := configuredManager(t)
	host2.weekly = &WeeklyPeriod{Start: host2.now.Add(-24 * time.Hour), ResetAt: host2.now.Add(6 * 24 * time.Hour)}
	host2.align = Alignment{Status: AlignUnsupported}
	host2.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, PlanRevision: "r1",
		CycleStart: host2.now.Add(-time.Hour), CycleEnd: host2.now.Add(48 * time.Hour)}
	if err := manager2.Refresh(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		manager2.Admit("fixed")
	}
	host2.now = host2.now.Add(24 * time.Hour)
	if err := manager2.Refresh(); err != nil { // same plan revision
		t.Fatal(err)
	}
	manager2.Admit("fixed")
	if calls := host2.aligns.Load(); calls != 1 {
		t.Fatalf("unsupported: AlignWeekly calls = %d, want 1", calls)
	}
	info := host2.keys["fixed"]
	info.PlanRevision = "r2"
	host2.keys["fixed"] = info
	if err := manager2.Refresh(); err != nil {
		t.Fatal(err)
	}
	manager2.Admit("fixed")
	if calls := host2.aligns.Load(); calls != 2 {
		t.Fatalf("after a plan change: AlignWeekly calls = %d, want 2", calls)
	}
	settings, _ := manager2.Settings()
	if _, err := manager2.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	manager2.Admit("fixed")
	if calls := host2.aligns.Load(); calls != 3 {
		t.Fatalf("after saving settings: AlignWeekly calls = %d, want 3", calls)
	}
}

func TestSettingsWithoutASnapshotRefuseRemainderKeys(t *testing.T) {
	manager, _ := configuredManager(t)
	manager.snap.Store(nil)
	decision := manager.Admit("remainder")
	if decision.Allowed || decision.Reason != RefusedNotLoaded || decision.Detail.Key == "" || decision.RetryAt.IsZero() {
		t.Fatalf("remainder before the snapshot = %+v", decision)
	}
	for _, scope := range []string{"fixed", "unassigned"} {
		if decision := manager.Admit(scope); !decision.Allowed {
			t.Fatalf("%s before the snapshot = %+v", scope, decision)
		}
	}
	view, err := manager.View()
	if err != nil || !view.Loaded {
		t.Fatalf("view after a refresh = %+v, %v", view.Loaded, err)
	}

	// The operator can keep admitting remainder keys instead.
	settings, _ := manager.Settings()
	settings.Params.RefuseUnloaded = false
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	manager.snap.Store(nil)
	if decision := manager.Admit("remainder"); !decision.Allowed {
		t.Fatalf("remainder with the switch off = %+v", decision)
	}
}

func TestViewReportsMissingEstimates(t *testing.T) {
	manager, host := configuredManager(t)
	manager.snap.Store(nil)
	host.dbErr = errors.New("database is locked")
	view, err := manager.View()
	if err != nil || view.Loaded || len(view.Settings.Keys) != 2 || view.Error == "" {
		t.Fatalf("view without estimates = %+v, %v", view, err)
	}
	if decision := manager.Admit("remainder"); decision.Reason != RefusedNotLoaded {
		t.Fatalf("remainder = %+v", decision)
	}
}

func TestAlignedPlansAreCheckedAgainAfterAPlanChange(t *testing.T) {
	manager, host := configuredManager(t)
	host.weekly = &WeeklyPeriod{Start: host.now.Add(-24 * time.Hour), ResetAt: host.now.Add(6 * 24 * time.Hour)}
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, PlanRevision: "r1",
		CycleStart: host.now.Add(-time.Hour), CycleEnd: host.now.Add(48 * time.Hour)}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		manager.Admit("fixed")
	}
	if calls := host.aligns.Load(); calls != 1 {
		t.Fatalf("aligned: AlignWeekly calls = %d, want 1", calls)
	}
	info := host.keys["fixed"]
	info.PlanRevision = "r2"
	host.keys["fixed"] = info
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	manager.Admit("fixed")
	if calls := host.aligns.Load(); calls != 2 {
		t.Fatalf("after a plan change: AlignWeekly calls = %d, want 2", calls)
	}
}

// Usage the store numbered at or below a read's sequence is in that read, so
// its live delta is not added again, whichever arrives first.
func TestLiveUsageIsCountedOnceAcrossARefresh(t *testing.T) {
	manager, host := configuredManager(t)
	manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: 10, Reset5h: host.now.Add(time.Hour)})
	host.seq = 5
	host.usage["fixed"] = 12
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, UsedWeekUSD: 30,
		CycleStart: host.now.Add(-time.Hour), CycleEnd: host.now.Add(48 * time.Hour)}
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	// Request 5 was stored before the read but reported after it.
	manager.ObserveCost("fixed", host.now, 30, 5)
	if balance, _ := manager.Balance("fixed"); balance != 70 {
		t.Fatalf("balance with an already counted request = %v, want 70", balance)
	}
	manager.ObserveCost("fixed", host.now, 10, 6)
	if balance, _ := manager.Balance("fixed"); balance != 60 {
		t.Fatalf("balance with a new request = %v, want 60", balance)
	}
	view, _ := manager.View()
	for _, key := range view.Keys {
		if key.Scope == "fixed" && key.UsedFiveHourUSD != 22 {
			t.Fatalf("5-hour usage = %v, want 12 read + 10 live", key.UsedFiveHourUSD)
		}
	}
}

func TestLiveUsageStaysBoundedWhenRefreshesFail(t *testing.T) {
	manager, host := configuredManager(t)
	manager.ObserveCost("fixed", host.now.Add(-8*24*time.Hour), 1, 1)
	for i := range maxDeltasPerKey + 10 {
		manager.ObserveCost("remainder", host.now, 0.01, uint64(i+2))
	}
	host.dbErr = errors.New("disk I/O error")
	if err := manager.Refresh(); err == nil {
		t.Fatal("refresh succeeded")
	}
	manager.liveMu.RLock()
	fixed, remainder := len(manager.deltas["fixed"]), len(manager.deltas["remainder"])
	manager.liveMu.RUnlock()
	if fixed != 0 || remainder != maxDeltasPerKey {
		t.Fatalf("deltas after a failed refresh: fixed %d, remainder %d", fixed, remainder)
	}
	host.dbErr = nil
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(host.logs, func(line string) bool { return strings.Contains(line, "oldest were dropped") }) {
		t.Fatalf("logs = %q", host.logs)
	}
}

func TestReadErrorsKeepTheLastSuccessfulValues(t *testing.T) {
	manager, host := configuredManager(t)
	host.weekly = &WeeklyPeriod{Start: host.now.Add(-24 * time.Hour), ResetAt: host.now.Add(6 * 24 * time.Hour)}
	manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: 10, Reset5h: host.now.Add(time.Hour)})
	host.usage["fixed"] = 12
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	host.usageErr = errors.New("database is locked")
	host.weeklyErr = errors.New("database is locked")
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	snap := manager.snap.Load()
	if key := snap.keys["fixed"]; key.usedFiveHour != 12 {
		t.Fatalf("5-hour usage after a failed read = %v, want 12", key.usedFiveHour)
	}
	if account := snap.accounts["a1"]; !account.hasWeek || !account.weekly.ResetAt.Equal(host.weekly.ResetAt) {
		t.Fatalf("weekly period after a failed read = %+v", account)
	}
	if manager.lastError.Load().(string) == "" {
		t.Fatal("the read errors were not recorded")
	}
}

func TestAnAccountBelongsToOnePool(t *testing.T) {
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "a", Name: "A", AuthIndexes: []string{"x", "y"}}, {ID: "b", Name: "B", AuthIndexes: []string{"y"}}}
	_, err := settings.Normalized()
	if err == nil || !strings.Contains(err.Error(), `"y" is already in CC pool "A"`) || messages.FromError(err).Key == "" {
		t.Fatalf("duplicate account error = %v (%+v)", err, messages.FromError(err))
	}
}

func TestPoolAnchors(t *testing.T) {
	settings := DefaultSettings()
	settings.Pools = []Pool{
		{ID: "legacy", AuthIndexes: []string{"a", "b"}},
		{ID: "chosen", AuthIndexes: []string{"c", "d", "e"}, Anchor: "d"},
		{ID: "stale", AuthIndexes: []string{"f"}, Anchor: "gone"},
	}
	normalized, err := settings.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		anchor  string
		indexes []string
	}{{"a", []string{"a", "b"}}, {"d", []string{"d", "c", "e"}}, {"f", []string{"f"}}} {
		pool := normalized.Pools[i]
		if pool.Anchor != want.anchor || !slices.Equal(pool.AuthIndexes, want.indexes) {
			t.Fatalf("pool %s = %+v, want anchor %s and %v", pool.ID, pool, want.anchor, want.indexes)
		}
	}
}

func TestFixedKeysOfOnePlanStayInOnePool(t *testing.T) {
	manager, host := configuredManager(t)
	host.keys["fixed2"] = KeyInfo{Preview: "sk-f2", PlanID: "plan", HasWeekly: true, WeeklyUSD: 50}
	settings, _ := manager.Settings()
	settings.Pools = append(settings.Pools, Pool{ID: "q", AuthIndexes: []string{"a2"}})
	settings.Keys["fixed2"] = KeySettings{Kind: KeyFixed, PoolID: "p"}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatalf("same plan in one pool: %v", err)
	}
	settings.Keys["fixed2"] = KeySettings{Kind: KeyFixed, PoolID: "q"}
	_, err := manager.SetSettings(settings)
	if err == nil || !strings.Contains(err.Error(), `share subscription plan "plan"`) || messages.FromError(err).Key == "" {
		t.Fatalf("shared plan across pools = %v", err)
	}
}

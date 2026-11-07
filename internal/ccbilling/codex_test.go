package ccbilling

import (
	"database/sql"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testWindow struct {
	limit   string
	seconds int64
	used    float64
	reset   time.Time
}

type testRequest struct {
	at      time.Time
	latency time.Duration
	usd     float64
	model   string
	source  string // price source; "" means priced
	credits string
	windows []testWindow
}

var requestIDs int64

// insertRequests stores requests of a Codex account the way the plugin
// records them: a request event, and a quota report with its windows when
// the response reported any.
func insertRequests(t *testing.T, db *sql.DB, authIndex, provider string, requests []testRequest) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, request := range requests {
		requestIDs++
		id := requestIDs
		source := request.source
		if source == "" {
			source = "reference"
		}
		if _, err := tx.Exec(`INSERT INTO request_events (id, at, scope, auth_index, latency_ms, total_cost, currency, billing_model, upstream_model, price_source)
			VALUES (?, ?, 'fixed', ?, ?, ?, 'USD', ?, ?, ?)`, id, nanos(request.at), authIndex, request.latency.Milliseconds(), request.usd,
			request.model, request.model, source); err != nil {
			t.Fatal(err)
		}
		if len(request.windows) == 0 && request.credits == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO quota_reports (request_event_id, auth_index, provider, at, credit_balance) VALUES (?, ?, ?, ?, ?)`,
			id, authIndex, provider, nanos(request.at), request.credits); err != nil {
			t.Fatal(err)
		}
		for _, window := range request.windows {
			if _, err := tx.Exec(`INSERT INTO quota_report_windows VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				id, window.limit, window.seconds, authIndex, provider, nanos(request.at), window.used, nanos(window.reset)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func codexMain(used5, used7 float64, reset5, reset7 time.Time) []testWindow {
	return []testWindow{{"codex", 18000, used5, reset5}, {"codex", 604800, used7, reset7}}
}

func codexSettings() Settings {
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "gpt", Name: "GPT", Provider: ProviderCodex, AuthIndexes: []string{"c1"}}}
	return settings
}

func TestLegacySettingsMoveToClaudesProfile(t *testing.T) {
	legacy := `{"params":{"segment_retention_days":365,"block_gap_minutes":5,"window_percent":8,"reference_mode":"global",` +
		`"suspicious_threshold":0.2,"exclude_vote_share":0.5,"weight_power":2,"recent_days":14,"recent_share":0.9,"k_min_weekly_points":10,` +
		`"cold_start_blocks":20,"tier_weekly_usd":{"pro":300,"max5x":1250,"max20x":2600},"tier_k":{"pro":7,"max5x":8,"max20x":9},` +
		`"refuse_remainder_when_unloaded":true,"safety_factor":0.9},"pools":[{"id":"p","name":"P","auth_indexes":["a1"],"anchor_auth_index":"a1"}],` +
		`"accounts":{"a1":{"tier":"max20x"}},"keys":{"k1":{"kind":"remainder","pool_id":"p"}},` +
		`"weights":{"enabled":true,"items":[{"id":"fable","name":"Fable","models":["claude-fable"]}],"min_segments":30,"min_cost_usd":20,"min_share_spread":0.05}}`
	host := newFakeHost(t)
	if err := EnsureSchema(host.db); err != nil {
		t.Fatal(err)
	}
	if _, err := host.db.Exec(`INSERT INTO cc_meta (key, value) VALUES ('settings', ?)`, legacy); err != nil {
		t.Fatal(err)
	}
	settings, err := loadSettings(host.db)
	if err != nil {
		t.Fatal(err)
	}
	claude := settings.Providers[ProviderClaude]
	tiers := map[string][2]float64{}
	for _, tier := range claude.Tiers {
		tiers[tier.ID] = [2]float64{tier.WeeklyUSD, *tier.K}
	}
	if !reflect.DeepEqual(tiers, map[string][2]float64{TierPro: {300, 7}, TierMax5x: {1250, 8}, TierMax20x: {2600, 9}}) {
		t.Fatalf("claude tiers = %+v", claude.Tiers)
	}
	if !claude.Weights.Enabled || len(claude.Weights.Items) != 1 || settings.Weights != nil {
		t.Fatalf("weights = %+v / %+v", claude.Weights, settings.Weights)
	}
	if settings.Pools[0].Provider != ProviderClaude || settings.Params.TierWeeklyUSD != nil || settings.Params.TierK != nil {
		t.Fatalf("settings = %+v", settings)
	}
	codex := settings.Providers[ProviderCodex]
	if len(codex.Tiers) != 1 || codex.Tiers[0].ID != TierTeam || codex.Tiers[0].WeeklyUSD != 60 || codex.Tiers[0].K != nil ||
		codex.LimitKey != "codex" || !codex.Rules.MainWindowOnly || !codex.Rules.ExcludeUnpriced || !codex.Rules.PauseWhenExhausted {
		t.Fatalf("codex profile = %+v", codex)
	}
	if params := settings.ParamsFor(ProviderClaude); params.TierWeekly(TierMax20x) != 2600 || *params.TierKFor("") != 7 {
		t.Fatalf("claude params = %+v", params)
	}
	// Saving and loading again changes nothing, and stores no legacy field.
	if err := saveSettings(host.db, settings); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := host.db.QueryRow(`SELECT value FROM cc_meta WHERE key = 'settings'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "tier_weekly_usd") || strings.Contains(raw, `"weights":{"enabled":true,"items":[{"id":"fable"`+`","name"`) {
		t.Fatalf("stored settings keep legacy fields: %s", raw)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored["weights"] != nil {
		t.Fatalf("stored settings = %s", raw)
	}
	again, err := loadSettings(host.db)
	if err != nil || !reflect.DeepEqual(again, settings) {
		t.Fatalf("reloaded settings differ:\n%+v\n%+v (%v)", again, settings, err)
	}
	normalized, err := again.Normalized()
	if err != nil || !reflect.DeepEqual(normalized, settings) {
		t.Fatalf("normalizing normalized settings changed them (%v)", err)
	}
}

func TestProviderSettingsValidation(t *testing.T) {
	settings := codexSettings()
	settings.Accounts["c1"] = AccountSettings{Tier: TierPro}
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `"pro" is not a tier of ChatGPT accounts`) {
		t.Fatalf("claude tier on a codex account: %v", err)
	}
	settings.Accounts["c1"] = AccountSettings{Tier: TierTeam, Provider: ProviderClaude}
	normalized, err := settings.Normalized()
	if err != nil || normalized.Accounts["c1"].Provider != ProviderCodex || normalized.AccountProvider("c1") != ProviderCodex {
		t.Fatalf("pooled account provider = %+v, %v", normalized.Accounts["c1"], err)
	}
	settings.Pools[0].Provider = "gemini"
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Unknown provider "gemini"`) {
		t.Fatalf("unknown pool provider: %v", err)
	}
	settings = codexSettings()
	profile := settings.Providers[ProviderCodex]
	profile.Tiers = []Tier{{ID: TierTeam, WeeklyUSD: 80, K: floatPointer(0.5)}}
	settings.Providers = cloneProviders(settings.Providers)
	settings.Providers[ProviderCodex] = profile
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), "default k of tier Team") {
		t.Fatalf("k out of range: %v", err)
	}
	profile.Tiers = []Tier{{ID: "plus", WeeklyUSD: 80}}
	settings.Providers[ProviderCodex] = profile
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), `Unknown tier "plus"`) {
		t.Fatalf("unknown tier: %v", err)
	}
	profile.Tiers = []Tier{{ID: TierTeam, WeeklyUSD: 80, K: floatPointer(5)}}
	cold := 3
	profile.Params = ParamOverrides{ColdStartBlocks: &cold}
	profile.Rules = SegmentRules{}
	profile.LimitKey = "other"
	settings.Providers[ProviderCodex] = profile
	normalized, err = settings.Normalized()
	if err != nil {
		t.Fatal(err)
	}
	codex := normalized.Providers[ProviderCodex]
	if codex.Tiers[0].WeeklyUSD != 80 || *codex.Tiers[0].K != 5 || codex.LimitKey != "codex" || !codex.Rules.MainWindowOnly {
		t.Fatalf("codex profile = %+v", codex)
	}
	if params := normalized.ParamsFor(ProviderCodex); params.ColdStartBlocks != 3 || params.TierWeekly("") != 80 {
		t.Fatalf("codex params = %+v", params)
	}
	if params := normalized.ParamsFor(ProviderClaude); params.ColdStartBlocks != 20 {
		t.Fatalf("claude params took codex's override: %+v", params)
	}
}

// Reports with decimals cut segments where a whole point is crossed, and a
// rise across several points makes one segment of that gain.
func TestSegmentsCutAtWholePoints(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	reset5, reset7 := start.Add(4*time.Hour), start.Add(5*24*time.Hour)
	used := []float64{10.2, 10.6, 11.3, 11.9, 12.4, 15.2, 15.9, 16.0}
	var observations []Observation
	var costs []CostEvent
	for i, value := range used {
		at := start.Add(time.Duration(i) * time.Minute)
		observations = append(observations, Observation{At: at, Has5h: true, Used5h: value, Reset5h: reset5, Has7d: true, Used7d: 3, Reset7d: reset7})
		costs = append(costs, CostEvent{At: at.Add(30 * time.Second), USD: float64(i + 1), Model: "gpt-test"})
	}
	var state StreamState
	state.Cursor = start.Add(-time.Second)
	params := codexSettings().ParamsFor(ProviderCodex)
	segments := state.Advance(observations, costs, start.Add(time.Hour), params)
	// Armed at 11.3; the costs completed after it until 12.4 crosses 12
	// (3+4); until 15.2 crosses 13-15 (5); until 16.0 crosses 16 (6+7).
	want := []struct{ gain, cost float64 }{{12.4 - 11.3, 3 + 4}, {15.2 - 12.4, 5}, {16 - 15.2, 6 + 7}}
	if len(segments) != len(want) {
		t.Fatalf("segments = %+v", segments)
	}
	for i, segment := range segments {
		if math.Abs(segment.Gain-want[i].gain) > 1e-6 || math.Abs(segment.CostUSD-want[i].cost) > 1e-9 {
			t.Fatalf("segment %d = %+v, want %+v", i, segment, want[i])
		}
	}
	// Whole-point reports behave as before: each rise is a segment.
	var whole StreamState
	whole.Cursor = start.Add(-time.Second)
	var wholeObs []Observation
	for i, value := range []float64{10, 11, 14, 15} {
		wholeObs = append(wholeObs, Observation{At: start.Add(time.Duration(i) * time.Minute), Has5h: true, Used5h: value, Reset5h: reset5})
	}
	if segments := whole.Advance(wholeObs, costs[:3], start.Add(time.Hour), DefaultParams()); len(segments) != 2 || segments[0].Gain != 3 || segments[1].Gain != 1 {
		t.Fatalf("whole segments = %+v", segments)
	}
	// A fraction times 100 that lands just below a whole point counts as it.
	if !crossesPoint(28.000000000000004, 0.29*100) || crossesPoint(0.29*100, 29.4) {
		t.Fatal("rounding of whole points")
	}
}

func codexManager(t *testing.T, settings Settings) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	manager := NewManager(host)
	manager.RefreshInterval = 0
	manager.ProcessBudget = time.Hour
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	return manager, host
}

func refreshedAccount(t *testing.T, manager *Manager, authIndex string) (AccountView, PoolStatus) {
	t.Helper()
	view, err := manager.View()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range view.Accounts {
		if account.AuthIndex == authIndex {
			return account, view.Pools[0]
		}
	}
	t.Fatalf("account %s missing from %+v", authIndex, view.Accounts)
	return AccountView{}, PoolStatus{}
}

// steadyRequests are requests of an account whose 5-hour window rises by
// one point for every $0.10 (5-hour total $10) and whose 7-day window by one
// point for every $0.60 (weekly total $60), in blocks of busy minutes.
func steadyRequests(start time.Time, blocks int, with5h bool, extra func(i int, request *testRequest)) []testRequest {
	var requests []testRequest
	spent := 0.0
	reset7 := start.Add(6 * 24 * time.Hour)
	for block := 0; block < blocks; block++ {
		blockStart := start.Add(time.Duration(block) * 2 * time.Hour)
		reset5 := blockStart.Add(5 * time.Hour)
		spent5 := 0.0
		for i := 0; i < 30; i++ {
			at := blockStart.Add(time.Duration(i) * time.Minute)
			request := testRequest{at: at, latency: 10 * time.Second, usd: 0.05, model: "gpt-test"}
			windows := []testWindow{{"codex", 604800, math.Floor(spent / 0.6), reset7}}
			if with5h {
				windows = append(windows, testWindow{"codex", 18000, math.Floor(spent5 / 0.1), reset5})
			}
			request.windows = windows
			if extra != nil {
				extra(block*30+i, &request)
			}
			requests = append(requests, request)
			if request.usd > 0 && len(request.windows) > 0 && request.windows[0].limit == "codex" {
				spent += request.usd
				spent5 += request.usd
			}
		}
		// The next block starts a new 5-hour window.
		if with5h {
			spent5 = 0
		}
	}
	return requests
}

func TestCodexAccountWithFiveHourWindow(t *testing.T) {
	settings := codexSettings()
	cold, points := 3, 100.0
	profile := settings.Providers[ProviderCodex]
	profile.Params.ColdStartBlocks = &cold
	// Too little 7-day gain to estimate k.
	profile.Params.KMinWeeklyPoints = &points
	settings.Providers[ProviderCodex] = profile
	manager, host := codexManager(t, settings)
	start := host.now.Add(-2 * 24 * time.Hour)
	insertRequests(t, host.db, "c1", ProviderCodex, steadyRequests(start, 8, true, nil))
	account, pool := refreshedAccount(t, manager, "c1")
	if account.Provider != ProviderCodex || account.Window == WindowWeek || account.Tier != TierTeam {
		t.Fatalf("account = %+v", account.AccountStatus)
	}
	if account.ColdStart || math.Abs(account.Estimate.FiveHourUSD-10) > 1 {
		t.Fatalf("5-hour estimate = %+v", account.Estimate)
	}
	// No manual, estimated or tier k: the weekly total is the tier's and
	// the 5-hour reservation is off.
	if account.K != nil || !account.NeedsK || account.WeeklyTotal != 60 || account.TotalsFrom != SourceTier || pool.FiveHourEnabled {
		t.Fatalf("account without k = %+v, pool five = %v", account.AccountStatus, pool.FiveHourEnabled)
	}
	manual := 6.0
	settings.Accounts["c1"] = AccountSettings{ManualK: &manual}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	account, pool = refreshedAccount(t, manager, "c1")
	if account.NeedsK || account.KSource != SourceManual || math.Abs(account.WeeklyTotal-6*account.FiveHourTotal) > 1e-9 || !pool.FiveHourEnabled {
		t.Fatalf("account with manual k = %+v", account.AccountStatus)
	}
}

func TestCodexAccountWithOnlyTheWeeklyWindow(t *testing.T) {
	settings := codexSettings()
	cold := 3
	profile := settings.Providers[ProviderCodex]
	profile.Params.ColdStartBlocks = &cold
	settings.Providers[ProviderCodex] = profile
	manager, host := codexManager(t, settings)
	start := host.now.Add(-4 * 24 * time.Hour)
	// Larger requests so the weekly window rises in each block.
	insertRequests(t, host.db, "c1", ProviderCodex, steadyRequests(start, 12, false, func(_ int, request *testRequest) { request.usd = 0.3 }))
	account, pool := refreshedAccount(t, manager, "c1")
	if account.Window != WindowWeek || account.NeedsK || account.FiveHourTotal != 0 || pool.FiveHourEnabled {
		t.Fatalf("weekly-only account = %+v", account.AccountStatus)
	}
	if account.ColdStart || account.TotalsFrom != SourceEstimated || math.Abs(account.WeeklyTotal-60) > 6 {
		t.Fatalf("weekly total = %+v", account.AccountStatus)
	}
	segments := account.Estimate.Segments
	// A 5-hour window appearing later restarts the segments on it.
	later := host.now.Add(-5 * time.Minute)
	insertRequests(t, host.db, "c1", ProviderCodex, []testRequest{
		{at: later, usd: 0.05, model: "gpt-test", windows: codexMain(1, 80, later.Add(4*time.Hour), later.Add(48*time.Hour))},
	})
	host.now = host.now.Add(20 * time.Minute)
	account, _ = refreshedAccount(t, manager, "c1")
	var stored int
	if err := host.db.QueryRow(`SELECT count(*) FROM cc_segments WHERE auth_index = 'c1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if account.Window == WindowWeek || stored != 0 || segments == 0 {
		t.Fatalf("after a 5-hour window: window %q, %d stored segments (had %d)", account.Window, stored, segments)
	}
}

// Requests metered against another limit, without a report, or of an
// unpriced model stay out of the segments, and the page shows their cost.
func TestCodexSegmentsExcludeOtherLimitsAndUnpricedModels(t *testing.T) {
	settings := codexSettings()
	manager, host := codexManager(t, settings)
	start := host.now.Add(-2 * 24 * time.Hour)
	requests := steadyRequests(start, 6, true, nil)
	clean := steadyRequests(start, 6, true, nil)
	reset := start.Add(5 * time.Hour)
	var extra []testRequest
	for i, request := range requests {
		at := request.at.Add(20 * time.Second)
		switch i % 10 {
		case 1:
			// A Spark request reports only its own pool.
			extra = append(extra, testRequest{at: at, usd: 1, model: "gpt-spark", windows: []testWindow{{"bengalfox", 18000, 50, reset}}})
		case 3:
			extra = append(extra, testRequest{at: at, usd: 2, model: "gpt-review", windows: []testWindow{{"code-review", 604800, 10, reset}}})
		case 5:
			extra = append(extra, testRequest{at: at, usd: 0, model: "gpt-unpriced", source: "none",
				windows: codexMain(request.windows[1].used, request.windows[0].used, request.windows[1].reset, request.windows[0].reset)})
		case 7:
			// No report at all, such as a failed request.
			extra = append(extra, testRequest{at: at, usd: 3, model: "gpt-test"})
		}
	}
	insertRequests(t, host.db, "c1", ProviderCodex, append(requests, extra...))
	account, _ := refreshedAccount(t, manager, "c1")

	other, otherHost := codexManager(t, settings)
	insertRequests(t, otherHost.db, "c1", ProviderCodex, clean)
	reference, _ := refreshedAccount(t, other, "c1")
	if account.Estimate.Ratio != reference.Estimate.Ratio || account.Estimate.Segments != reference.Estimate.Segments {
		t.Fatalf("excluded requests changed the estimate: %+v vs %+v", account.Estimate, reference.Estimate)
	}
	excluded := account.Excluded
	if math.Abs(excluded.USD[ExcludedOtherLimit]-float64(18*1+18*2+18*3)) > 1e-9 || excluded.Requests[ExcludedUnpriced] != 18 ||
		excluded.UnpricedModels["gpt-unpriced"] != 18 {
		t.Fatalf("excluded = %+v", excluded)
	}
}

// A used-up window or credits being spent pause the segments; they resume
// once usage moves the utilization again.
func TestCodexSegmentsPauseWhileExhausted(t *testing.T) {
	settings := codexSettings()
	manager, host := codexManager(t, settings)
	start := host.now.Add(-2 * 24 * time.Hour)
	reset7 := start.Add(6 * 24 * time.Hour)
	var requests []testRequest
	add := func(at time.Time, used5, used7 float64, credits string, usd float64) {
		requests = append(requests, testRequest{at: at, latency: time.Second, usd: usd, model: "gpt-test", credits: credits,
			windows: codexMain(used5, used7, at.Truncate(5*time.Hour).Add(5*time.Hour), reset7)})
	}
	// Normal rises, then the 5-hour window is used up while requests go on.
	for i := 0; i < 10; i++ {
		add(start.Add(time.Duration(i)*time.Minute), float64(90+i), 40, "", 0.1)
	}
	for i := 0; i < 5; i++ {
		add(start.Add(time.Duration(10+i)*time.Minute), 100, 40, "", 5)
	}
	insertRequests(t, host.db, "c1", ProviderCodex, requests)
	host.now = start.Add(30 * time.Minute)
	account, _ := refreshedAccount(t, manager, "c1")
	if account.Estimate.Segments != 8 {
		t.Fatalf("segments before the limit = %d", account.Estimate.Segments)
	}
	manager.Observe("c1", Observation{At: host.now, Has5h: true, Used5h: 100, Reset5h: host.now.Add(time.Hour), LimitReached: true})
	account, _ = refreshedAccount(t, manager, "c1")
	if !account.Paused || account.PauseReason != PauseLimitReached {
		t.Fatalf("account at the limit = %+v", account.AccountStatus)
	}
	// Credits are spent while the windows stay below the limit.
	requests = nil
	next := start.Add(6 * time.Hour)
	for i, credits := range []string{"10", "9.5", "9", "8.5"} {
		add(next.Add(time.Duration(i)*time.Minute), float64(i+1), 41, credits, 5)
	}
	// Then usage moves the utilization again.
	for i := 0; i < 6; i++ {
		add(next.Add(time.Duration(10+i)*time.Minute), float64(10+i), 41, "8.5", 0.1)
	}
	insertRequests(t, host.db, "c1", ProviderCodex, requests)
	host.now = next.Add(5 * time.Minute)
	manager.Observe("c1", Observation{At: host.now.Add(-time.Hour), Has5h: true, Used5h: 4, Reset5h: next.Add(5 * time.Hour)})
	host.now = next.Add(30 * time.Minute)
	account, _ = refreshedAccount(t, manager, "c1")
	// The credit requests cost $20 but moved the utilization 3 points: no
	// segment includes them. After them, 5 rises make 4 segments.
	if account.Estimate.Segments != 8+4 {
		t.Fatalf("segments after resuming = %d", account.Estimate.Segments)
	}
	for _, segment := range manager.accounts["c1"].segments {
		if segment.CostUSD > 1 {
			t.Fatalf("a paused cost reached segment %+v", segment)
		}
	}
	// Replaying the credit period alone pauses with the credits reason.
	var state StreamState
	state.Cursor = next.Add(-time.Second)
	var observations []Observation
	for i, credits := range []float64{10, 9.5} {
		observations = append(observations, Observation{At: next.Add(time.Duration(i) * time.Minute), Has5h: true, Used5h: float64(i + 1),
			Reset5h: next.Add(time.Hour), HasCredits: true, Credits: credits})
	}
	state.Advance(observations, nil, next.Add(time.Hour), settings.ParamsFor(ProviderCodex))
	if !state.Paused || state.PauseReason != PauseCredits {
		t.Fatalf("credit stream = %+v", state)
	}
	// Claude has no such rule.
	var claude StreamState
	claude.Cursor = next.Add(-time.Second)
	claude.Advance([]Observation{{At: next, Has5h: true, Used5h: 100, Reset5h: next.Add(time.Hour)}}, nil, next.Add(time.Hour), DefaultParams())
	if claude.Paused {
		t.Fatal("a Claude stream paused")
	}
}

// Fixed and remainder keys share a Codex pool like a Claude one.
func TestCodexPoolReservationsAndBalances(t *testing.T) {
	settings := codexSettings()
	settings.Keys["fixed"] = KeySettings{Kind: KeyFixed, PoolID: "gpt"}
	settings.Keys["remainder"] = KeySettings{Kind: KeyRemainder, PoolID: "gpt"}
	manager, host := codexManager(t, settings)
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 20, UsedWeekUSD: 5,
		CycleStart: host.now.Add(-time.Hour), CycleEnd: host.now.Add(6 * 24 * time.Hour)}
	host.keys["remainder"] = KeyInfo{}
	host.usage["fixed"] = 1
	if err := manager.Refresh(); err != nil {
		t.Fatal(err)
	}
	observe := func(used5, used7 float64) {
		manager.Observe("c1", Observation{At: host.now, Has5h: true, Used5h: used5, Reset5h: host.now.Add(3 * time.Hour),
			Has7d: true, Used7d: used7, Reset7d: host.now.Add(5 * 24 * time.Hour)})
	}
	observe(10, 20)
	// Cold start: the Team default of $60 a week, no k, no 5-hour reservation.
	_, pool := refreshedAccount(t, manager, "c1")
	if math.Abs(pool.RemainingWeek-0.9*60*0.8) > 1e-9 || pool.ReservedWeek != 15 || pool.FiveHourEnabled {
		t.Fatalf("pool = %+v", pool)
	}
	if decision := manager.Admit("remainder"); !decision.Allowed {
		t.Fatalf("remainder refused: %+v", decision)
	}
	if balance, status := manager.Balance("remainder"); status != BalanceOK || math.Abs(balance-(0.9*60*0.8-15)) > 1e-9 {
		t.Fatalf("remainder balance = %v, %d", balance, status)
	}
	if balance, _ := manager.Balance("fixed"); balance != 15 {
		t.Fatalf("fixed balance = %v", balance)
	}
	// With a tier default k the 5-hour reservation applies: T5 = 60/6 = 10,
	// R5 = 0.9 × 10 × 0.1 = 0.9 < 20/6 − 1.
	profile := settings.Providers[ProviderCodex]
	profile.Tiers = []Tier{{ID: TierTeam, WeeklyUSD: 60, K: floatPointer(6)}}
	settings.Providers[ProviderCodex] = profile
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	observe(90, 20)
	_, pool = refreshedAccount(t, manager, "c1")
	if !pool.FiveHourEnabled || math.Abs(pool.ReservedFiveHour-(20.0/6-1)) > 1e-9 {
		t.Fatalf("pool with k = %+v", pool)
	}
	if decision := manager.Admit("remainder"); decision.Allowed || decision.Reason != RefusedFiveHour {
		t.Fatalf("remainder at the 5-hour reserve = %+v", decision)
	}
	observe(10, 75)
	if decision := manager.Admit("remainder"); decision.Allowed || decision.Reason != RefusedWeekly {
		t.Fatalf("remainder at the weekly reserve = %+v", decision)
	}
	if balance, _ := manager.Balance("remainder"); balance != 0.9*60*0.25-15 {
		t.Fatalf("remainder balance below the reserve = %v", balance)
	}
}

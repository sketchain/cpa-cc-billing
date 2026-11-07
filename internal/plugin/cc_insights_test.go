package plugin

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/ccbilling"
)

func ccKeyUsage(t *testing.T, app *App) ccbilling.KeyUsage {
	t.Helper()
	var response struct {
		Usage ccbilling.KeyUsage `json:"usage"`
	}
	callOK(t, app, http.MethodGet, routeCCUsage, nil, nil, http.StatusOK, &response)
	return response.Usage
}

func ccKeySeries(t *testing.T, usage ccbilling.KeyUsage, key string) ccbilling.KeySeries {
	t.Helper()
	for _, pool := range usage.Pools {
		for _, series := range pool.Keys {
			if series.Scope == billing.CallerScope(key) {
				return series
			}
		}
	}
	t.Fatalf("key %s missing from %+v", key, usage)
	return ccbilling.KeySeries{}
}

// Every refusal is counted while the log keeps one line per key, reason and
// window.
func TestCCRefusalCountsKeepTheLogDeduplicated(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.92, reset5h, reset7d)
	before := ccTables(t, app)
	for range 3 {
		if response := ccAdmit(t, app, ccRemainderKey); !response.Terminate {
			t.Fatal("remainder key admitted")
		}
	}
	// Admission only counts in memory.
	if after := ccTables(t, app); after != before {
		t.Fatalf("admission wrote to the CC tables:\nbefore %s\nafter %s", before, after)
	}
	blocks := ccKeySeries(t, ccKeyUsage(t, app), ccRemainderKey).Blocks
	if len(blocks) != 1 || blocks[0].Count != 3 || blocks[0].Reason != ccbilling.RefusedWeekly || !blocks[0].At.Equal(clock.now.Truncate(time.Hour)) {
		t.Fatalf("blocks = %+v", blocks)
	}
	clock.now = clock.now.Add(10 * time.Minute)
	for range 2 {
		ccAdmit(t, app, ccRemainderKey)
	}
	// Reading again saves only the new refusals.
	ccKeyUsage(t, app)
	if blocks := ccKeySeries(t, ccKeyUsage(t, app), ccRemainderKey).Blocks; len(blocks) != 1 || blocks[0].Count != 5 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if lines := ccBlockLogs(t, app); len(lines) != 1 {
		t.Fatalf("block logs = %q", lines)
	}
	// A fixed key is never refused by the split.
	if blocks := ccKeySeries(t, ccKeyUsage(t, app), ccFixedKey).Blocks; len(blocks) != 0 {
		t.Fatalf("fixed key blocks = %+v", blocks)
	}
}

func callAccountMethod(t *testing.T, app *App, method, path, apiKey string, body any) ManagementResponse {
	t.Helper()
	headers := http.Header{}
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, ManagementRequest{
		Method: method, Path: resourceBase + path, Headers: headers, Body: mustMarshal(t, body),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	return response
}

func TestCCInsightEndpoints(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(4*time.Hour), clock.now.Add(5*24*time.Hour)
	// One hour of steady use: 5-hour utilization up 1 point and the week 0.1
	// point every 10 minutes.
	for i := range 7 {
		ccUsage(t, app, ccFixedKey, 0.5, float64(10+i)/100, float64(200+i)/1000, reset5h, reset7d)
		ccUsage(t, app, ccRemainderKey, 0.25, float64(10+i)/100, float64(200+i)/1000, reset5h, reset7d)
		if i < 6 {
			clock.now = clock.now.Add(10 * time.Minute)
		}
	}
	callOK(t, app, http.MethodPost, routeCCRefresh, nil, nil, http.StatusOK, nil)

	var forecast struct {
		Forecast ccbilling.Forecast `json:"forecast"`
	}
	callOK(t, app, http.MethodGet, routeCCFore, nil, nil, http.StatusOK, &forecast)
	if len(forecast.Forecast.Pools) != 1 || len(forecast.Forecast.Pools[0].Accounts) != 1 || forecast.Forecast.Window5h != 3600 ||
		forecast.Forecast.Window7d != 86400 {
		t.Fatalf("forecast = %+v", forecast.Forecast)
	}
	pool := forecast.Forecast.Pools[0]
	account := pool.Accounts[0]
	// 6 points an hour on the 5-hour window: 84 left is 14 hours, after the
	// reset in 4.
	if account.FiveHour.Source != ccbilling.RateReading || math.Abs(account.FiveHour.Reading.PerHour-6) > 1e-6 ||
		account.FiveHour.Outcome.Status != ccbilling.OutcomeAfterReset {
		t.Fatalf("5-hour forecast = %+v", account.FiveHour)
	}
	// The key spend: $4.50 in the last hour on the cold-start $156.25
	// 5-hour total, $1.5625 a point: 2.88 points an hour.
	if !account.FiveHour.Spend.Available || math.Abs(account.FiveHour.Spend.RecordedUSD-5.25) > 1e-6 ||
		math.Abs(account.FiveHour.Spend.PerHour-5.25/1.5625) > 1e-6 {
		t.Fatalf("5-hour spend = %+v", account.FiveHour.Spend)
	}
	if pool.Week.Source != ccbilling.RateReading || pool.Week.Reserved <= 0 || pool.Week.Block.Status == ccbilling.OutcomeUnknown {
		t.Fatalf("pool week = %+v", pool.Week)
	}
	callOK(t, app, http.MethodGet, routeCCFore, url.Values{"window_5h_minutes": {"30"}, "window_7d_hours": {"6"}}, nil, http.StatusOK, &forecast)
	if forecast.Forecast.Window5h != 1800 || forecast.Forecast.Window7d != 6*3600 {
		t.Fatalf("windows = %d %d", forecast.Forecast.Window5h, forecast.Forecast.Window7d)
	}
	callOK(t, app, http.MethodGet, routeCCFore, url.Values{"window_5h_minutes": {"1"}}, nil, http.StatusBadRequest, nil)
	callOK(t, app, http.MethodGet, routeCCFore, url.Values{"window_7d_hours": {"200"}}, nil, http.StatusBadRequest, nil)

	// The simulation writes nothing and changes no setting.
	var settingsBefore struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &settingsBefore)
	plansBefore, _ := json.Marshal(app.store.PlanViews())
	keysBefore, _ := json.Marshal(app.store.KeyViews())
	tablesBefore := ccTables(t, app)
	usage := 50.0
	weekly := 40.0
	var simulated struct {
		Simulation ccbilling.SimResult `json:"simulation"`
	}
	callOK(t, app, http.MethodPost, routeCCSim, nil, ccbilling.SimRequest{PoolID: "main",
		Add:     []ccbilling.SimNewKey{{WeeklyUSD: 200, WeeklyUsageUSD: &usage}},
		Changes: map[string]ccbilling.SimChange{billing.CallerScope(ccFixedKey): {WeeklyUSD: &weekly}}}, http.StatusOK, &simulated)
	result := simulated.Simulation
	if result.PoolID != "main" || len(result.Weeks) == 0 || result.Remainder != 1 || len(result.Keys) != 2 || !result.Keys[1].New ||
		result.Keys[0].WeeklyUSD != 40 || result.Keys[0].BaselineWeeklyUSD != 100 || len(result.Accounts) != 1 || len(result.Approximations) == 0 {
		t.Fatalf("simulation = %+v", result)
	}
	if result.Scenario.Hours == 0 || result.Scenario.Hours != result.Baseline.Hours {
		t.Fatalf("simulation hours = %+v / %+v", result.Baseline, result.Scenario)
	}
	if after := ccTables(t, app); after != tablesBefore {
		t.Fatalf("the simulation changed the CC tables:\nbefore %s\nafter %s", tablesBefore, after)
	}
	plansAfter, _ := json.Marshal(app.store.PlanViews())
	keysAfter, _ := json.Marshal(app.store.KeyViews())
	var settingsAfter struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &settingsAfter)
	settingsA, _ := json.Marshal(settingsBefore.CC.Settings)
	settingsB, _ := json.Marshal(settingsAfter.CC.Settings)
	if !bytes.Equal(plansBefore, plansAfter) || !bytes.Equal(keysBefore, keysAfter) || !bytes.Equal(settingsA, settingsB) {
		t.Fatal("the simulation changed plans, keys or settings")
	}
	resp := callManagement(t, app, http.MethodPost, routeCCSim, nil, ccbilling.SimRequest{PoolID: "missing"})
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(resp.Body, []byte("backend.cc_pool_value_does_not_exist")) {
		t.Fatalf("missing pool = %d %s", resp.StatusCode, resp.Body)
	}
	resp = callManagement(t, app, http.MethodPost, routeCCSim, nil, ccbilling.SimRequest{PoolID: "main",
		Changes: map[string]ccbilling.SimChange{billing.CallerScope(ccRemainderKey): {Remove: true}}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("changing a remainder key = %d %s", resp.StatusCode, resp.Body)
	}
	callOK(t, app, http.MethodPost, routeCCSim, nil, ccbilling.SimRequest{PoolID: "main", Add: []ccbilling.SimNewKey{{WeeklyUSD: -1}}},
		http.StatusBadRequest, nil)

	// The key usage: hourly cost as billed, and the fixed key's cycle.
	keyUsage := ccKeyUsage(t, app)
	fixed := ccKeySeries(t, keyUsage, ccFixedKey)
	var spent float64
	for _, hour := range fixed.Hours {
		spent += hour[1]
	}
	if fixed.Kind != ccbilling.KeyFixed || math.Abs(spent-3.5) > 1e-6 || len(fixed.Cycles) == 0 || fixed.WeeklyUSD != 100 {
		t.Fatalf("fixed key usage = %+v", fixed)
	}
	if current := fixed.Cycles[len(fixed.Cycles)-1]; current.Settled || math.Abs(current.UsedUSD-3.5) > 1e-6 || math.Abs(current.RemainingUSD-96.5) > 1e-6 {
		t.Fatalf("current cycle = %+v", current)
	}
	remainder := ccKeySeries(t, keyUsage, ccRemainderKey)
	if remainder.Kind != ccbilling.KeyRemainder || len(remainder.Cycles) != 0 {
		t.Fatalf("remainder key usage = %+v", remainder)
	}

	// API key users have no route to any of them.
	for _, key := range []string{ccRemainderKey, ""} {
		for _, path := range []string{routeCCFore, routeCCUsage} {
			if resp := callAccount(t, app, path, key, nil); resp.StatusCode != http.StatusNotFound || bytes.Contains(resp.Body, []byte("pools")) {
				t.Fatalf("API key %s = %d %s", path, resp.StatusCode, resp.Body)
			}
		}
		if resp := callAccountMethod(t, app, http.MethodPost, routeCCSim, key, ccbilling.SimRequest{PoolID: "main"}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("API key simulation = %d %s", resp.StatusCode, resp.Body)
		}
	}
}

func TestCCInsightsWaitForTheFirstSnapshot(t *testing.T) {
	app := newConfiguredApp(t)
	app.cc = ccbilling.NewManager(&ccHost{app: app})
	for _, call := range []struct{ method, path string }{{http.MethodGet, routeCCFore}, {http.MethodGet, routeCCUsage}} {
		if resp := callManagement(t, app, call.method, call.path, nil, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d %s", call.path, resp.StatusCode, resp.Body)
		}
	}
	if resp := callManagement(t, app, http.MethodPost, routeCCSim, nil, ccbilling.SimRequest{PoolID: "main"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("simulation = %d %s", resp.StatusCode, resp.Body)
	}
}

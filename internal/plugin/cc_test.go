package plugin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-cc-billing/internal/billing"
	"cpa-cc-billing/internal/ccbilling"
)

const (
	ccFixedKey     = "sk-cc-fixed-0001"
	ccRemainderKey = "sk-cc-remainder-0001"
	ccModel        = "claude-cc-test"
	ccAuth         = "auth-claude-1"
)

type ccTestClock struct{ now time.Time }

func (c *ccTestClock) Now() time.Time { return c.now }

func newCCTestApp(t *testing.T) (*App, *ccTestClock) {
	t.Helper()
	app := newConfiguredApp(t)
	clock := &ccTestClock{now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	app.store.SetClock(clock.Now)
	app.cc.RefreshInterval = 0
	if _, err := app.store.UpsertPrice(billing.CustomPrice{ModelID: ccModel, PriceRates: billing.PriceRates{InputPer1M: 1, OutputPer1M: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.SyncKeys([]string{ccFixedKey, ccRemainderKey}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreatePlanWithBindings(billing.Plan{ID: "fixed-a", Name: "Fixed A", Windows: []billing.QuotaWindow{
		{Name: "Week", PeriodSeconds: weekSeconds, Amount: 100},
	}}, []string{billing.CallerScope(ccFixedKey)}); err != nil {
		t.Fatal(err)
	}
	settings := ccbilling.DefaultSettings()
	settings.Pools = []ccbilling.Pool{{ID: "main", Name: "Main", AuthIndexes: []string{ccAuth}}}
	settings.Accounts[ccAuth] = ccbilling.AccountSettings{Tier: ccbilling.TierMax5x}
	settings.Keys[billing.CallerScope(ccFixedKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyFixed, PoolID: "main"}
	settings.Keys[billing.CallerScope(ccRemainderKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyRemainder, PoolID: "main"}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)
	return app, clock
}

// ccUsage publishes a Claude OAuth request of key costing usd whose response
// reported the given utilizations and reset times.
func ccUsage(t *testing.T, app *App, key string, usd, used5h, used7d float64, reset5h, reset7d time.Time) {
	t.Helper()
	tokens := int64(math.Round(usd * 1e6))
	publishUsageRecord(t, app, UsageRecord{
		Provider: "claude", ExecutorType: "ClaudeExecutor", Model: ccModel, Alias: ccModel, APIKey: key,
		AuthIndex: ccAuth, AuthType: "oauth", Generate: true, RequestedAt: app.store.Now(),
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {strconv.FormatFloat(used5h, 'f', -1, 64)},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(reset5h.Unix(), 10)},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {strconv.FormatFloat(used7d, 'f', -1, 64)},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(reset7d.Unix(), 10)},
		},
		Detail: UsageDetail{InputTokens: tokens, TotalTokens: tokens},
	})
}

func ccAdmit(t *testing.T, app *App, key string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: fmt.Sprint(time.Now().UnixNano()), SourceFormat: "claude", Model: ccModel, RequestedModel: ccModel,
		Metadata: map[string]any{MetadataCallerScope: billing.CallerScope(key), MetadataRequestPath: "/v1/messages"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

func ccBalance(t *testing.T, app *App, key string) (int, float64) {
	t.Helper()
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + routeBalance, Headers: http.Header{"Authorization": {"Bearer " + key}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	var body struct {
		Object         string   `json:"object"`
		TotalAvailable *float64 `json:"total_available"`
	}
	_ = json.Unmarshal(response.Body, &body)
	if response.StatusCode == http.StatusOK && (body.Object != "credit_summary" || body.TotalAvailable == nil) {
		t.Fatalf("balance body = %s", response.Body)
	}
	if body.TotalAvailable == nil {
		return response.StatusCode, 0
	}
	return response.StatusCode, *body.TotalAvailable
}

func refusalMessage(t *testing.T, response RequestInterceptResponse) string {
	t.Helper()
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.ResponseBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "rate_limit_error" {
		t.Fatalf("refusal type = %q", body.Error.Type)
	}
	return body.Error.Message
}

func TestCCRemainderKeyWeeklyAndFiveHourReservations(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour)

	// Cold start on a 5x account: T_week = $1250. R_week = 0.9 × 1250 × 0.6 =
	// $675 exceeds the fixed key's $100 reservation.
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.40, reset5h, reset7d)
	if response := ccAdmit(t, app, ccRemainderKey); response.Terminate {
		t.Fatalf("remainder key refused: %s", response.ResponseBody)
	}
	if status, balance := ccBalance(t, app, ccRemainderKey); status != http.StatusOK || math.Abs(balance-575) > 1e-6 {
		t.Fatalf("remainder balance = %d %.4f, want 575", status, balance)
	}
	if status, balance := ccBalance(t, app, ccFixedKey); status != http.StatusOK || balance != 100 {
		t.Fatalf("fixed balance = %d %.4f, want 100", status, balance)
	}

	// Without a manual or estimated k the tier's default k of 8 applies:
	// T_5h = $156.25, R_5h = 0.9 × 156.25 × 0.05 = $7.03 against $12.50.
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.95, 0.40, reset5h, reset7d)
	if response := ccAdmit(t, app, ccRemainderKey); !response.Terminate || !strings.Contains(refusalMessage(t, response), "$12.50 reserved") {
		t.Fatalf("5-hour refusal with the tier k = %+v", response)
	}
	// A manual k of 5: T_5h = $250, R_5h = $11.25 against a $20 reservation.
	var view struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	settings := view.CC.Settings
	manualK := 5.0
	settings.Accounts[ccAuth] = ccbilling.AccountSettings{Tier: ccbilling.TierMax5x, ManualK: &manualK}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)
	response := ccAdmit(t, app, ccRemainderKey)
	if !response.Terminate || response.ResponseHeaders.Get("Retry-After") != "3600" {
		t.Fatalf("5-hour refusal = %+v", response)
	}
	if message := refusalMessage(t, response); !strings.Contains(message, "5-hour reservation") {
		t.Fatalf("5-hour refusal message = %q", message)
	}
	// The balance only counts the week: 0.9 × 1250 × 0.6 − 100.
	if _, balance := ccBalance(t, app, ccRemainderKey); math.Abs(balance-575) > 1e-6 {
		t.Fatalf("remainder balance during 5-hour refusal = %.4f, want 575", balance)
	}

	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	if len(view.CC.Pools) != 1 || !view.CC.Pools[0].FiveHourEnabled || math.Abs(view.CC.Pools[0].ReservedFiveHour-20) > 1e-9 {
		t.Fatalf("pool view = %+v", view.CC.Pools)
	}
	if len(view.CC.Accounts) != 1 || !view.CC.Accounts[0].ColdStart || view.CC.Accounts[0].KSource != ccbilling.SourceManual {
		t.Fatalf("account view = %+v", view.CC.Accounts)
	}

	// A remainder key's own weekly cap, once the 5-hour window has room again.
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.40, reset5h.Add(5*time.Hour), reset7d)
	capUSD := 0.75
	settings.Accounts[ccAuth] = ccbilling.AccountSettings{Tier: ccbilling.TierMax5x}
	settings.Keys[billing.CallerScope(ccRemainderKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyRemainder, PoolID: "main", WeeklyCapUSD: &capUSD}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)
	if message := refusalMessage(t, ccAdmit(t, app, ccRemainderKey)); !strings.Contains(message, "weekly cap") {
		t.Fatalf("cap refusal message = %q", message)
	}
	if _, balance := ccBalance(t, app, ccRemainderKey); balance != 1 {
		t.Fatalf("capped remainder balance = %.4f, want 1", balance)
	}

	settings.Keys[billing.CallerScope(ccRemainderKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyRemainder, PoolID: "main"}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)

	// At 92% weekly utilization R_week = $90 no longer exceeds $100.
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.92, reset5h, reset7d)
	response = ccAdmit(t, app, ccRemainderKey)
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests || response.ResponseHeaders.Get("Retry-After") != "3600" {
		t.Fatalf("weekly refusal = %+v", response)
	}
	if message := refusalMessage(t, response); !strings.Contains(message, "weekly reservation") {
		t.Fatalf("weekly refusal message = %q", message)
	}
	// new-api disables a channel at a zero balance, so it reads 1.
	if _, balance := ccBalance(t, app, ccRemainderKey); balance != 1 {
		t.Fatalf("exhausted remainder balance = %.4f, want 1", balance)
	}
	// Fixed keys are never held back by the pool.
	if response := ccAdmit(t, app, ccFixedKey); response.Terminate {
		t.Fatalf("fixed key refused: %s", response.ResponseBody)
	}

	// Keys outside every pool have no balance.
	settings.Keys = map[string]ccbilling.KeySettings{}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)
	if status, _ := ccBalance(t, app, ccRemainderKey); status != http.StatusNotFound {
		t.Fatalf("unassigned balance status = %d", status)
	}
	if status, _ := ccBalance(t, app, "sk-unknown-key"); status != http.StatusUnauthorized {
		t.Fatalf("unknown key balance status = %d", status)
	}
}

func weeklyWindow(t *testing.T, app *App, scope string) billing.QuotaWindowView {
	t.Helper()
	view, ok := app.store.KeyViewForScope(scope)
	if !ok || len(view.Windows) != 1 {
		t.Fatalf("key view = %+v", view)
	}
	return view.Windows[0]
}

func TestCCFixedKeyCycleFollowsAccountAndSurvivesEarlyReset(t *testing.T) {
	app, clock := newCCTestApp(t)
	scope := billing.CallerScope(ccFixedKey)
	reset5h, reset7d := clock.now.Add(3*time.Hour), clock.now.Add(3*24*time.Hour)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.40, reset5h, reset7d)

	// The fixed key's first admission anchors its weekly window on the
	// account's 7-day reset before the cycle opens.
	if response := ccAdmit(t, app, ccFixedKey); response.Terminate {
		t.Fatalf("fixed key refused: %s", response.ResponseBody)
	}
	plans := app.store.Plans()
	if anchor := plans[0].Windows[0].CycleAnchorAt; !anchor.Equal(reset7d) {
		t.Fatalf("anchor = %v, want %v", anchor, reset7d)
	}
	window := weeklyWindow(t, app, scope)
	if !window.EndAt.Equal(reset7d) {
		t.Fatalf("cycle ends %v, want %v", window.EndAt, reset7d)
	}
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccFixedKey, 30, 0.20, 0.45, reset5h, reset7d)
	if used, _ := weeklyWindow(t, app, scope).Dimensions[0].Used.Float64(); math.Abs(used-30) > 1e-6 {
		t.Fatalf("used = %v, want 30", used)
	}

	// Anthropic resets the week early: utilization drops and the reset time
	// moves a week from now. The fixed key keeps its cycle and its usage.
	clock.now = clock.now.Add(24 * time.Hour)
	earlyReset := clock.now.Add(7 * 24 * time.Hour).Truncate(time.Second)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.01, 0.0, clock.now.Add(5*time.Hour), earlyReset)
	if response := ccAdmit(t, app, ccFixedKey); response.Terminate {
		t.Fatalf("fixed key refused: %s", response.ResponseBody)
	}
	window = weeklyWindow(t, app, scope)
	if used, _ := window.Dimensions[0].Used.Float64(); !window.EndAt.Equal(reset7d) || math.Abs(used-30) > 1e-6 {
		t.Fatalf("after early reset: cycle ends %v used %v, want %v and 30", window.EndAt, used, reset7d)
	}
	var view struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	if view.CC.Accounts[0].EarlyResetAt.IsZero() || view.CC.Keys[0].Alignment == nil || view.CC.Keys[0].Alignment.Status != ccbilling.AlignWaiting {
		t.Fatalf("view after early reset = %+v / %+v", view.CC.Accounts[0], view.CC.Keys)
	}
	if _, balance := ccBalance(t, app, ccFixedKey); math.Abs(balance-70) > 1e-6 {
		t.Fatalf("fixed balance = %.4f, want 70", balance)
	}

	// At its scheduled end the cycle expires unspent; the next one ends at
	// the account's new reset.
	clock.now = reset7d.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.05, 0.10, clock.now.Add(5*time.Hour), earlyReset)
	if response := ccAdmit(t, app, ccFixedKey); response.Terminate {
		t.Fatalf("fixed key refused: %s", response.ResponseBody)
	}
	window = weeklyWindow(t, app, scope)
	if used, _ := window.Dimensions[0].Used.Float64(); !window.EndAt.Equal(earlyReset) || used != 0 {
		t.Fatalf("next cycle ends %v used %v, want %v and 0", window.EndAt, used, earlyReset)
	}
	if _, balance := ccBalance(t, app, ccFixedKey); balance != 100 {
		t.Fatalf("fixed balance in the new cycle = %.4f, want 100", balance)
	}
}

func TestCCImportPriceList(t *testing.T) {
	app := newConfiguredApp(t)
	body := `[{"name":"claude-opus-test","aliases":["opus-alias","claude-opus-test"],"input":5,"output":25,"cache_read":0.5,"cache_write":6.25,"tiers":"ignored"},
		{"name":"claude-haiku-test","input":1,"output":5}]`
	var result struct {
		Models   int `json:"models"`
		ModelIDs int `json:"model_ids"`
	}
	callOK(t, app, http.MethodPost, routePriceJSON, nil, body, http.StatusOK, &result)
	if result.Models != 2 || result.ModelIDs != 3 {
		t.Fatalf("import result = %+v", result)
	}
	price, model, err := app.store.ResolveModelPrice("opus-alias", "", false)
	if err != nil || price.Source != billing.PriceSourceCustom || price.InputPer1M != 5 || price.CacheWritePer1M != 6.25 {
		t.Fatalf("alias price = %+v (%s, %v)", price, model, err)
	}
	callOK(t, app, http.MethodPost, routePriceJSON, nil, `{"models":[{"name":"x","input":1,"output":2}]}`, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routePriceJSON, nil, `{"y":{"input":1,"output":2}}`, http.StatusOK, nil)
	if resp := callManagement(t, app, http.MethodPost, routePriceJSON, nil, `[{"name":"z","input":1}]`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete entry status = %d", resp.StatusCode)
	}
}

// A database copied from cpa-key-billing opens with its data intact and gains
// the CC tables, without changing the billing schema version.
func TestCCOpensCopiedKeyBillingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpa-key-billing-state-v1.db")
	legacy := newTestApp(t)
	if _, err := legacy.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.store.SyncKeys([]string{ccFixedKey}, false); err != nil {
		t.Fatal(err)
	}
	legacy.Shutdown()
	before := sqliteUserVersion(t, path)

	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if _, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	if _, ok := app.store.KeyViewForScope(billing.CallerScope(ccFixedKey)); !ok {
		t.Fatal("the copied key is missing")
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, nil)
	app.Shutdown()
	if after := sqliteUserVersion(t, path); after != before {
		t.Fatalf("billing schema version changed from %d to %d", before, after)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('cc_meta', 'cc_segments', 'cc_streams')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("CC tables = %d (%v)", tables, err)
	}
}

func sqliteUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// Configuration builds the first snapshot, so after a restart remainder keys
// are held to the reservations before any request has completed.
func TestCCRestartAppliesReservationsBeforeTheFirstUsage(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.92, reset5h, reset7d)
	if response := ccAdmit(t, app, ccRemainderKey); !response.Terminate {
		t.Fatal("remainder key admitted before the restart")
	}
	path := app.store.Config().StateFile
	app.Shutdown()

	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	restarted.store.SetClock(clock.Now)
	if _, err := restarted.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	if !restarted.cc.Loaded() {
		t.Fatal("configuration did not build a snapshot")
	}
	response := ccAdmit(t, restarted, ccRemainderKey)
	if !response.Terminate || !strings.Contains(refusalMessage(t, response), "weekly reservation") {
		t.Fatalf("remainder key after restart = %+v", response)
	}
}

func TestAuthFilesCarryCCEstimates(t *testing.T) {
	app, clock := newCCTestApp(t)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.20, clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour))
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(`{"files":[
			{"id":"claude-1.json","auth_index":"` + ccAuth + `","name":"claude-1.json","type":"claude","provider":"claude"},
			{"id":"claude-2.json","auth_index":"auth-claude-2","name":"claude-2.json","type":"claude","provider":"claude"}
		]}`), nil
	})
	var admin authFileListResponse
	callOK(t, app, http.MethodGet, routeAuthFiles, nil, nil, http.StatusOK, &admin)
	byIndex := map[string]authFileView{}
	for _, file := range admin.Files {
		byIndex[file.AuthIndex] = file
	}
	pooled := byIndex[ccAuth].CC
	if pooled == nil || !slices.Equal(pooled.Pools, []string{"Main"}) || pooled.Tier != ccbilling.TierMax5x || !pooled.ColdStart ||
		pooled.WeeklyTotal != 1250 || pooled.RemainingWeek <= 0 {
		t.Fatalf("pooled account CC = %+v", pooled)
	}
	if byIndex["auth-claude-2"].CC != nil {
		t.Fatalf("an account outside every pool has CC estimates: %+v", byIndex["auth-claude-2"].CC)
	}
	response := callAccount(t, app, routeAuthFiles, ccRemainderKey, nil)
	if response.StatusCode != http.StatusOK || bytes.Contains(response.Body, []byte(`"cc"`)) {
		t.Fatalf("API key auth files = %d %s, want no CC estimates", response.StatusCode, response.Body)
	}
}

func ccBlockLogs(t *testing.T, app *App) []string {
	t.Helper()
	var page struct {
		Entries []struct {
			Message string `json:"message"`
		} `json:"entries"`
	}
	callOK(t, app, http.MethodGet, routePluginLogs, url.Values{"level": {"all"}}, nil, http.StatusOK, &page)
	var lines []string
	for _, entry := range page.Entries {
		if strings.HasPrefix(entry.Message, "CC blocked:") {
			lines = append(lines, entry.Message)
		}
	}
	return lines
}

func TestCCRefusalsAreLoggedOncePerWindow(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.92, reset5h, reset7d)
	for range 3 {
		if response := ccAdmit(t, app, ccRemainderKey); !response.Terminate {
			t.Fatal("remainder key admitted")
		}
	}
	lines := ccBlockLogs(t, app)
	if len(lines) != 1 || !strings.Contains(lines[0], "reason=weekly_reserve") || !strings.Contains(lines[0], "$90.00") ||
		!strings.Contains(lines[0], "$100.00") {
		t.Fatalf("block logs = %q", lines)
	}
	// A new weekly window logs again.
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.92, reset5h, reset7d.Add(ccbilling.Week))
	ccAdmit(t, app, ccRemainderKey)
	if lines := ccBlockLogs(t, app); len(lines) != 2 {
		t.Fatalf("block logs after a new window = %q", lines)
	}
}

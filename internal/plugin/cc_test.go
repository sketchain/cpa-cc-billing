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

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/ccbilling"
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
	plan, _ := app.store.PlanByID("fixed-a")
	if anchor := plan.Windows[0].CycleAnchorAt; !anchor.Equal(reset7d) {
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

func TestCCSettingsRejectSharedAccountsAndPlans(t *testing.T) {
	app, _ := newCCTestApp(t)
	var view struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	if pool := view.CC.Settings.Pools[0]; pool.Anchor != ccAuth {
		t.Fatalf("anchor of a pool saved without one = %q", pool.Anchor)
	}
	var failure struct {
		Error struct {
			Key string `json:"message_key"`
		} `json:"error"`
	}

	duplicate := view.CC.Settings
	duplicate.Pools = append(duplicate.Pools, ccbilling.Pool{ID: "spare", AuthIndexes: []string{ccAuth}})
	callOK(t, app, http.MethodPut, routeCCConfig, nil, duplicate, http.StatusBadRequest, &failure)
	if failure.Error.Key != "backend.cc_account_value_already_in_pool_value" {
		t.Fatalf("duplicate account error key = %q", failure.Error.Key)
	}

	const otherFixed = "sk-cc-fixed-0002"
	if _, err := app.store.SyncKeys([]string{ccFixedKey, ccRemainderKey, otherFixed}, false); err != nil {
		t.Fatal(err)
	}
	callOK(t, app, http.MethodPost, routeKeysBind, nil, map[string]string{"scope": billing.CallerScope(otherFixed), "plan_id": "fixed-a"}, http.StatusOK, nil)
	shared := view.CC.Settings
	shared.Pools = append(shared.Pools, ccbilling.Pool{ID: "second", AuthIndexes: []string{"auth-claude-2"}})
	shared.Keys[billing.CallerScope(otherFixed)] = ccbilling.KeySettings{Kind: ccbilling.KeyFixed, PoolID: "second"}
	failure.Error.Key = ""
	callOK(t, app, http.MethodPut, routeCCConfig, nil, shared, http.StatusBadRequest, &failure)
	if failure.Error.Key != "backend.cc_fixed_keys_share_plan_across_pools" {
		t.Fatalf("shared plan error key = %q", failure.Error.Key)
	}
	// Key rows tell the API key page which keys are CC fixed keys.
	for _, row := range readKeyRows(t, app) {
		if row.Scope == billing.CallerScope(ccFixedKey) && row.CCKind != ccbilling.KeyFixed {
			t.Fatalf("fixed key row cc_kind = %q", row.CCKind)
		}
	}
}

func readKeyRows(t *testing.T, app *App) []keyRow {
	t.Helper()
	var response struct {
		Keys []keyRow `json:"keys"`
	}
	callOK(t, app, http.MethodGet, routeKeys, nil, nil, http.StatusOK, &response)
	return response.Keys
}

// ccTables dumps every CC table, so a test can tell whether a call wrote.
func ccTables(t *testing.T, app *App) string {
	t.Helper()
	var dump strings.Builder
	err := (&ccHost{app: app}).WithDB(func(db *sql.DB) error {
		for _, query := range []string{
			`SELECT auth_index, block_at, at, cost_usd, gain FROM cc_segments ORDER BY id`,
			`SELECT auth_index, bucket_at, gain_5h, gain_7d, '' FROM cc_k_buckets ORDER BY auth_index, bucket_at`,
			`SELECT auth_index, state_json, '', '', '' FROM cc_streams ORDER BY auth_index`,
			`SELECT key, value, '', '', '' FROM cc_meta ORDER BY key`,
			`SELECT segment_id, model, cost_usd, '', '' FROM cc_segment_costs ORDER BY segment_id, model`,
			`SELECT id, item_json, auth_index, started_at, ended_at || result_json FROM cc_calibrations ORDER BY id`,
			`SELECT scope, hour_at, reason, count, '' FROM cc_block_counts ORDER BY scope, hour_at, reason`,
		} {
			rows, err := db.Query(query)
			if err != nil {
				return err
			}
			for rows.Next() {
				values := make([]any, 5)
				pointers := make([]any, 5)
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					rows.Close()
					return err
				}
				fmt.Fprintln(&dump, values...)
			}
			rows.Close()
			fmt.Fprintln(&dump, "--")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dump.String()
}

func TestCCTrendReadsStoredDataWithoutRefreshing(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(4*time.Hour), clock.now.Add(5*24*time.Hour)
	for i := range 6 {
		clock.now = clock.now.Add(5 * time.Minute)
		ccUsage(t, app, ccRemainderKey, 0.5, float64(i+1)*0.04, float64(i+1)*0.005, reset5h, reset7d)
	}
	clock.now = clock.now.Add(ccbilling.ReorderHorizon + time.Minute)
	callOK(t, app, http.MethodPost, routeCCRefresh, nil, nil, http.StatusOK, nil)

	// Usage after the last refresh stays unprocessed while usage refreshes wait.
	app.cc.RefreshInterval = 24 * time.Hour
	clock.now = clock.now.Add(time.Minute)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.40, 0.04, reset5h, reset7d)
	clock.now = clock.now.Add(ccbilling.ReorderHorizon + time.Minute)
	before := ccTables(t, app)

	var response struct {
		Trend ccbilling.Trend `json:"trend"`
	}
	callOK(t, app, http.MethodGet, routeCCTrend, url.Values{"auth_index": {ccAuth}}, nil, http.StatusOK, &response)
	if after := ccTables(t, app); after != before {
		t.Fatalf("the trend request changed the CC tables:\nbefore %s\nafter %s", before, after)
	}
	trend := response.Trend
	if trend.AuthIndex != ccAuth || !trend.Loaded || trend.Exclusion != ccbilling.ExclusionEstimate || len(trend.Segments) == 0 ||
		len(trend.Buckets) == 0 || trend.K == nil || trend.KSource == "" {
		t.Fatalf("trend = %+v", trend)
	}
	var gain7d float64
	for _, bucket := range trend.Buckets {
		gain7d += bucket.Gain7d
	}
	// Only the refreshed history, not the pending report.
	if gain7d <= 0 || gain7d >= 4 {
		t.Fatalf("trend 7-day gain = %v, want only the history processed before the request", gain7d)
	}

	callOK(t, app, http.MethodGet, routeCCTrend, nil, nil, http.StatusBadRequest, nil)
	callOK(t, app, http.MethodGet, routeCCTrend, url.Values{"auth_index": {"auth-unknown"}}, nil, http.StatusNotFound, nil)
	// API key users have no route to it.
	for _, key := range []string{ccRemainderKey, ""} {
		resp := callAccount(t, app, routeCCTrend, key, url.Values{"auth_index": {ccAuth}})
		if resp.StatusCode != http.StatusNotFound || bytes.Contains(resp.Body, []byte("gain_7d")) {
			t.Fatalf("API key trend = %d %s", resp.StatusCode, resp.Body)
		}
	}
}

func TestCCQuotaWeightEndpoints(t *testing.T) {
	app, clock := newCCTestApp(t)
	reset5h, reset7d := clock.now.Add(4*time.Hour), clock.now.Add(5*24*time.Hour)
	for i := range 12 {
		ccUsage(t, app, ccRemainderKey, 0.4, float64(i), float64(i)/8, reset5h, reset7d)
		clock.now = clock.now.Add(time.Minute)
	}
	clock.now = clock.now.Add(time.Hour)
	var current struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &current)
	settings := current.CC.Settings
	claude := settings.Providers[ccbilling.ProviderClaude]
	claude.Weights.Items = []ccbilling.WeightItem{{ID: "test", Name: "Test", Models: []string{strings.ToUpper(ccModel)}}}
	settings.Providers[ccbilling.ProviderClaude] = claude
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)

	var view struct {
		CC     ccbilling.View                     `json:"cc"`
		Prices map[string]billing.PriceMultiplier `json:"price_multipliers"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	weights := view.CC.Weights[ccbilling.ProviderClaude]
	if len(weights.Models) != 1 || weights.Models[0].Model != ccModel || weights.Models[0].ItemID != "test" || weights.UnsplitSegments != 0 {
		t.Fatalf("weights = %+v", weights)
	}
	if len(weights.Effective) != 1 || weights.Effective[0].Source != ccbilling.WeightSourceDefault || weights.Estimate.Items[0].Status != ccbilling.WeightInsufficient {
		t.Fatalf("weights = %+v", weights)
	}
	// The test model has a custom price but no official one to compare with.
	if price := view.Prices[ccModel]; !price.Custom || price.Input != 0 {
		t.Fatalf("prices = %+v", view.Prices)
	}

	resp := callManagement(t, app, http.MethodPost, routeCCCalib, nil, map[string]string{"item_id": "missing", "auth_index": ccAuth})
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(resp.Body, []byte("backend.cc_test_item_value_does_not_exist")) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	var started struct {
		Calibration ccbilling.Calibration `json:"calibration"`
	}
	callOK(t, app, http.MethodPost, routeCCCalib, nil, map[string]string{"item_id": "test", "auth_index": ccAuth}, http.StatusOK, &started)
	if started.Calibration.ID == 0 || started.Calibration.Item.Name != "Test" {
		t.Fatalf("calibration = %+v", started.Calibration)
	}
	callOK(t, app, http.MethodPost, routeCCStop, nil, map[string]int64{"id": started.Calibration.ID}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeCCStop, nil, map[string]int64{"id": started.Calibration.ID}, http.StatusNotFound, nil)
	callOK(t, app, http.MethodDelete, routeCCCalib, url.Values{"id": {strconv.FormatInt(started.Calibration.ID, 10)}}, nil, http.StatusOK, nil)
	callOK(t, app, http.MethodDelete, routeCCCalib, url.Values{"id": {"x"}}, nil, http.StatusBadRequest, nil)

	var estimates struct {
		Estimates ccbilling.Estimates `json:"estimates"`
	}
	callOK(t, app, http.MethodGet, routeCCEstim, nil, nil, http.StatusOK, &estimates)
	if claudeWeights := estimates.Estimates.For(ccbilling.ProviderClaude); claudeWeights.Basis != ccbilling.BasisRaw ||
		len(estimates.Estimates.Accounts) != 1 || len(claudeWeights.Weights) != 1 {
		t.Fatalf("estimates = %+v", estimates.Estimates)
	}
}

// The plan revision covers the plan's configuration only: requests change its
// pool cycles and quota view, not the revision the alignment cache keys on.
func TestCCKeyInfoPlanRevisionIgnoresUsage(t *testing.T) {
	app, clock := newCCTestApp(t)
	host := &ccHost{app: app}
	scope := billing.CallerScope(ccRemainderKey)
	if _, err := app.store.CreatePlanWithBindings(billing.Plan{ID: "pool", Name: "Pool", Windows: []billing.QuotaWindow{
		{Name: "Week", PeriodSeconds: weekSeconds, Amount: 100, CycleAnchorAt: clock.now.Add(3 * 24 * time.Hour)},
	}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	revision := func() string {
		t.Helper()
		info, ok := host.KeyInfo(scope)
		if !ok || info.PlanID != "pool" || info.PlanRevision == "" {
			t.Fatalf("key info = %+v, %v", info, ok)
		}
		return info.PlanRevision
	}
	before := revision()
	shared := true
	if _, err := app.store.UpdatePlanWithBindings(billing.PlanPatch{ID: "pool", SharedQuota: &shared}, nil); err != nil {
		t.Fatal(err)
	}
	sharedRevision := revision()
	if sharedRevision == before {
		t.Fatal("switching the plan to a shared quota kept its revision")
	}
	if decision := app.store.Authorize(scope, clock.now); !decision.Allowed {
		t.Fatalf("admission = %+v", decision)
	}
	ccUsage(t, app, ccRemainderKey, 2, 0.1, 0.2, clock.now.Add(time.Hour), clock.now.Add(3*24*time.Hour))
	clock.now = clock.now.Add(time.Minute)
	if after := revision(); after != sharedRevision {
		t.Fatalf("usage changed the plan revision:\n%s\n%s", sharedRevision, after)
	}
	plan, _ := app.store.PlanByID("pool")
	spent := 0.0
	for _, cycle := range plan.Cycles {
		spent += cycle.Spending.USD
	}
	if len(plan.Cycles) != 1 || spent <= 0 {
		t.Fatalf("the shared pool did not record the usage: %+v", plan.Cycles)
	}
}

// A shared pool's cycle can run while no member has a key cycle. Aligning
// the plan then waits for the pool cycle instead of failing to save a pool
// cycle off the new anchor's grid, and moving the anchor afterwards clears
// both the pool's and the keys' cycles.
func TestCCAlignWaitsForSharedPoolCycle(t *testing.T) {
	app, clock := newCCTestApp(t)
	first, second := billing.CallerScope(ccRemainderKey), billing.CallerScope("sk-cc-shared-member")
	if _, err := app.store.SyncKeys([]string{ccFixedKey, ccRemainderKey, "sk-cc-shared-member"}, false); err != nil {
		t.Fatal(err)
	}
	anchor := clock.now.Add(5 * 24 * time.Hour)
	if _, err := app.store.CreatePlanWithBindings(billing.Plan{ID: "shared", Name: "Shared", SharedQuota: true, Windows: []billing.QuotaWindow{
		{Name: "Week", PeriodSeconds: weekSeconds, Amount: 100, CycleAnchorAt: anchor},
	}}, []string{first}); err != nil {
		t.Fatal(err)
	}
	if decision := app.store.Authorize(first, clock.now); !decision.Allowed {
		t.Fatalf("admission = %+v", decision)
	}
	// The member leaves; the pool cycle stays, and the new member has none.
	if err := app.store.UnbindKey(first); err != nil {
		t.Fatal(err)
	}
	if err := app.store.BindKey(second, "shared"); err != nil {
		t.Fatal(err)
	}
	// The anchor is within a week ahead, so the running pool cycle ends there.
	poolEnd := anchor
	resetAt := clock.now.Add(36*time.Hour + 17*time.Minute)
	result, err := app.store.AlignWindowAnchor("shared", weekSeconds, resetAt)
	if err != nil || result.Status != billing.AnchorWaiting || !result.WaitUntil.Equal(poolEnd) {
		t.Fatalf("alignment = %+v, %v; want waiting until %v", result, err, poolEnd)
	}
	if plan, _ := app.store.PlanByID("shared"); !plan.Windows[0].CycleAnchorAt.Equal(anchor) || len(plan.Cycles) != 1 {
		t.Fatalf("waiting changed the plan: %+v", plan)
	}

	// Once the pool cycle has ended, without any admission settling it, the
	// anchor moves and no stale cycle remains on the plan or its keys.
	clock.now = poolEnd.Add(time.Minute)
	resetAt = clock.now.Add(36*time.Hour + 17*time.Minute)
	result, err = app.store.AlignWindowAnchor("shared", weekSeconds, resetAt)
	if err != nil || result.Status != billing.AnchorChanged {
		t.Fatalf("alignment = %+v, %v; want changed", result, err)
	}
	plan, _ := app.store.PlanByID("shared")
	if !plan.Windows[0].CycleAnchorAt.Equal(resetAt) || len(plan.Cycles) != 0 {
		t.Fatalf("aligned plan = %+v", plan)
	}
	if view, ok := app.store.KeyViewForScope(second); !ok || view.PlanID != "shared" || !view.Windows[0].StartAt.Add(ccbilling.Week).Equal(resetAt) {
		t.Fatalf("member view after alignment = %+v", view)
	}
	// The new schedule persists and reloads.
	if decision := app.store.Authorize(second, clock.now); !decision.Allowed {
		t.Fatalf("admission after alignment = %+v", decision)
	}
	if plan, _ := app.store.PlanByID("shared"); len(plan.Cycles) != 1 || !plan.Cycles[plan.Windows[0].ID].EndAt.Equal(resetAt) {
		t.Fatalf("pool cycle after alignment = %+v", plan.Cycles)
	}
}

type messageFailure struct {
	Error struct {
		Key    string            `json:"message_key"`
		Params map[string]string `json:"message_params"`
	} `json:"error"`
}

// CC fixed keys may not use a shared quota plan: the CC settings refuse such
// a key, and plan edits refuse to put a fixed key on a shared plan.
func TestCCFixedKeysRejectSharedQuotaPlans(t *testing.T) {
	app, clock := newCCTestApp(t)
	fixed := billing.CallerScope(ccFixedKey)
	const spare = "sk-cc-spare-0003"
	if _, err := app.store.SyncKeys([]string{ccFixedKey, ccRemainderKey, spare}, false); err != nil {
		t.Fatal(err)
	}
	anchor := clock.now.Add(3 * 24 * time.Hour)
	week := []billing.QuotaWindow{{Name: "Week", PeriodSeconds: weekSeconds, Amount: 50, CycleAnchorAt: anchor}}
	fixedPlan, _ := app.store.PlanByID("fixed-a")
	fixedWeek := slices.Clone(fixedPlan.Windows)
	fixedWeek[0].CycleAnchorAt = anchor
	if _, err := app.store.CreatePlanWithBindings(billing.Plan{ID: "pool", Name: "Pool", SharedQuota: true, Windows: week},
		[]string{billing.CallerScope(spare)}); err != nil {
		t.Fatal(err)
	}
	expect := func(failure messageFailure, key string) {
		t.Helper()
		if failure.Error.Key != key || failure.Error.Params["v0"] == "" || failure.Error.Params["v1"] == "" {
			t.Fatalf("error = %+v, want %s", failure.Error, key)
		}
	}

	// Plan edits that would put the fixed key on a shared plan.
	var failure messageFailure
	callOK(t, app, http.MethodPost, routeKeysBind, nil, map[string]string{"scope": fixed, "plan_id": "pool"}, http.StatusConflict, &failure)
	expect(failure, "backend.cc_shared_plan_has_fixed_key")
	failure = messageFailure{}
	callOK(t, app, http.MethodPatch, routePlans, nil, map[string]any{"id": "pool", "scopes": []string{billing.CallerScope(spare), fixed}},
		http.StatusConflict, &failure)
	expect(failure, "backend.cc_shared_plan_has_fixed_key")
	failure = messageFailure{}
	callOK(t, app, http.MethodPatch, routePlans, nil, map[string]any{"id": "fixed-a", "shared_quota": true, "windows": fixedWeek},
		http.StatusConflict, &failure)
	expect(failure, "backend.cc_shared_plan_has_fixed_key")
	failure = messageFailure{}
	callOK(t, app, http.MethodPost, routePlans, nil, map[string]any{"name": "New pool", "shared_quota": true, "windows": week, "scopes": []string{fixed}},
		http.StatusConflict, &failure)
	expect(failure, "backend.cc_shared_plan_has_fixed_key")
	if key, _ := app.store.KeyViewForScope(fixed); key.PlanID != "fixed-a" {
		t.Fatalf("a refused edit rebound the fixed key to %q", key.PlanID)
	}
	if plan, _ := app.store.PlanByID("fixed-a"); plan.SharedQuota {
		t.Fatal("a refused edit made the fixed key's plan shared")
	}
	// Edits that keep fixed keys off shared plans still work.
	callOK(t, app, http.MethodPatch, routePlans, nil, map[string]any{"id": "pool", "name": "Pool 2"}, http.StatusOK, nil)
	callOK(t, app, http.MethodPost, routeKeysBind, nil, map[string]string{"scope": billing.CallerScope(ccRemainderKey), "plan_id": "pool"}, http.StatusOK, nil)
	callOK(t, app, http.MethodPatch, routePlans, nil, map[string]any{"id": "fixed-a", "name": "Fixed A2"}, http.StatusOK, nil)

	// A fixed key that ends up on a shared plan anyway, here through the
	// store, makes the CC settings unsavable and the CC page warn.
	shared := true
	if _, err := app.store.UpdatePlanWithBindings(billing.PlanPatch{ID: "fixed-a", SharedQuota: &shared, Windows: &fixedWeek}, nil); err != nil {
		t.Fatal(err)
	}
	var view struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	if len(view.CC.Warnings) != 1 || view.CC.Warnings[0].Key != "backend.cc_fixed_key_shared_plan_warning" {
		t.Fatalf("CC warnings = %+v", view.CC.Warnings)
	}
	failure = messageFailure{}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, view.CC.Settings, http.StatusBadRequest, &failure)
	expect(failure, "backend.cc_fixed_key_on_shared_plan")
	callOK(t, app, http.MethodPost, routeCCRefresh, nil, nil, http.StatusOK, nil)
	var page struct {
		Entries []struct {
			Message string `json:"message"`
		} `json:"entries"`
	}
	callOK(t, app, http.MethodGet, routePluginLogs, url.Values{"level": {"all"}}, nil, http.StatusOK, &page)
	logged := 0
	for _, entry := range page.Entries {
		if strings.Contains(entry.Message, "is bound to shared quota plan") {
			logged++
		}
	}
	if logged != 1 {
		t.Fatalf("shared plan logged %d times, want once", logged)
	}

	// Switching the plan back clears the warning and the settings save again.
	shared = false
	if _, err := app.store.UpdatePlanWithBindings(billing.PlanPatch{ID: "fixed-a", SharedQuota: &shared}, nil); err != nil {
		t.Fatal(err)
	}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, view.CC.Settings, http.StatusOK, nil)
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	if len(view.CC.Warnings) != 0 {
		t.Fatalf("CC warnings after unsharing = %+v", view.CC.Warnings)
	}
}

func openStateFile(t *testing.T, path string, clock *ccTestClock) *App {
	t.Helper()
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if clock != nil {
		app.store.SetClock(clock.Now)
	}
	if _, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	return app
}

func ccMetaValue(t *testing.T, path, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM cc_meta WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatalf("cc_meta %s: %v", key, err)
	}
	return value
}

// The billing schema upgrade to version 25 (shared quota plans) and the CC
// tables, versioned separately in cc_meta, leave each other alone: a
// database of upstream schema 25 with a shared pool opens with its pool, a
// database of this fork's previous release (schema 24 with CC tables)
// upgrades with its CC settings intact, and a new file gets both.
func TestCCDatabasesAcrossTheSharedQuotaSchema(t *testing.T) {
	clock := &ccTestClock{now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	anchor := clock.now.Add(2 * 24 * time.Hour)
	member := billing.CallerScope(ccRemainderKey)

	t.Run("upstream schema 25 with a shared pool", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cpa-key-billing-state-v1.db")
		legacy := openStateFile(t, path, clock)
		if _, err := legacy.store.SyncKeys([]string{ccRemainderKey}, false); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.store.CreatePlanWithBindings(billing.Plan{ID: "pool", Name: "Pool", SharedQuota: true, Windows: []billing.QuotaWindow{
			{Name: "Week", PeriodSeconds: weekSeconds, Amount: 50, CycleAnchorAt: anchor},
		}}, []string{member}); err != nil {
			t.Fatal(err)
		}
		if decision := legacy.store.Authorize(member, clock.now); !decision.Allowed {
			t.Fatalf("admission = %+v", decision)
		}
		legacy.Shutdown()
		if version := sqliteUserVersion(t, path); version != 25 {
			t.Fatalf("billing schema = %d, want 25", version)
		}

		app := openStateFile(t, path, clock)
		callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, nil)
		plan, found := app.store.PlanByID("pool")
		if !found || !plan.SharedQuota || len(plan.Cycles) != 1 {
			t.Fatalf("shared pool after reopening = %+v", plan)
		}
		app.Shutdown()
		if version := sqliteUserVersion(t, path); version != 25 {
			t.Fatalf("billing schema after the CC tables = %d", version)
		}
		if ccMetaValue(t, path, "schema_version") == "" {
			t.Fatal("CC tables missing")
		}
	})

	t.Run("previous release at schema 24 with CC tables", func(t *testing.T) {
		app, _ := newCCTestApp(t)
		path := app.store.Config().StateFile
		settingsBefore := ccMetaValue(t, path, "settings")
		app.Shutdown()
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`ALTER TABLE plans DROP COLUMN shared_quota; ALTER TABLE plans DROP COLUMN cycles_json; PRAGMA user_version = 24;`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		ccVersion := ccMetaValue(t, path, "schema_version")

		upgraded := openStateFile(t, path, nil)
		var view struct {
			CC ccbilling.View `json:"cc"`
		}
		callOK(t, upgraded, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
		if key := view.CC.Settings.Keys[billing.CallerScope(ccFixedKey)]; key.Kind != ccbilling.KeyFixed || key.PoolID != "main" {
			t.Fatalf("CC settings after the upgrade = %+v", view.CC.Settings)
		}
		plan, found := upgraded.store.PlanByID("fixed-a")
		if !found || plan.SharedQuota || plan.Windows[0].Amount != 100 {
			t.Fatalf("plan after the upgrade = %+v", plan)
		}
		upgraded.Shutdown()
		if version := sqliteUserVersion(t, path); version != 25 {
			t.Fatalf("billing schema after the upgrade = %d, want 25", version)
		}
		if after := ccMetaValue(t, path, "schema_version"); after != ccVersion {
			t.Fatalf("CC schema changed from %s to %s", ccVersion, after)
		}
		if after := ccMetaValue(t, path, "settings"); after != settingsBefore {
			t.Fatalf("CC settings changed:\n%s\n%s", settingsBefore, after)
		}
	})

	t.Run("new database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cpa-super-billing-state-v1.db")
		app := openStateFile(t, path, clock)
		callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, nil)
		app.Shutdown()
		if version := sqliteUserVersion(t, path); version != 25 {
			t.Fatalf("billing schema = %d, want 25", version)
		}
		if ccMetaValue(t, path, "schema_version") == "" {
			t.Fatal("CC tables missing")
		}
	})
}

package plugin

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/ccbilling"
)

const (
	codexFixedKey     = "sk-codex-fixed-0001"
	codexRemainderKey = "sk-codex-remainder-0001"
	codexModel        = "gpt-codex-test"
	codexAuth         = "auth-codex-1"
)

func codexHostFiles(app *App) {
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(`{"files":[
			{"id":"claude-1.json","auth_index":"` + ccAuth + `","name":"claude-1.json","type":"claude","provider":"claude"},
			{"id":"codex-1.json","auth_index":"` + codexAuth + `","name":"codex-1.json","type":"codex","provider":"codex"}
		]}`), nil
	})
}

// newCodexTestApp has a ChatGPT pool with a fixed key on a $20 weekly plan
// and a remainder key, next to the Claude pool of newCCTestApp.
func newCodexTestApp(t *testing.T) (*App, *ccTestClock) {
	t.Helper()
	app, clock := newCCTestApp(t)
	codexHostFiles(app)
	if _, err := app.store.UpsertPrice(billing.CustomPrice{ModelID: codexModel, PriceRates: billing.PriceRates{InputPer1M: 1, OutputPer1M: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.SyncKeys([]string{ccFixedKey, ccRemainderKey, codexFixedKey, codexRemainderKey}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreatePlanWithBindings(billing.Plan{ID: "codex-fixed", Name: "Codex fixed", Windows: []billing.QuotaWindow{
		{Name: "Week", PeriodSeconds: weekSeconds, Amount: 20},
	}}, []string{billing.CallerScope(codexFixedKey)}); err != nil {
		t.Fatal(err)
	}
	var view struct {
		CC ccbilling.View `json:"cc"`
	}
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	settings := view.CC.Settings
	settings.Pools = append(settings.Pools, ccbilling.Pool{ID: "gpt", Name: "GPT", Provider: ccbilling.ProviderCodex, AuthIndexes: []string{codexAuth}})
	settings.Accounts[codexAuth] = ccbilling.AccountSettings{Tier: ccbilling.TierTeam}
	settings.Keys[billing.CallerScope(codexFixedKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyFixed, PoolID: "gpt"}
	settings.Keys[billing.CallerScope(codexRemainderKey)] = ccbilling.KeySettings{Kind: ccbilling.KeyRemainder, PoolID: "gpt"}
	callOK(t, app, http.MethodPut, routeCCConfig, nil, settings, http.StatusOK, nil)
	return app, clock
}

// codexUsage publishes a Codex OAuth request of key costing usd whose
// response reported the given headers.
func codexUsage(t *testing.T, app *App, key string, usd float64, headers http.Header) {
	t.Helper()
	tokens := int64(math.Round(usd * 1e6))
	publishUsageRecord(t, app, UsageRecord{
		Provider: "codex", ExecutorType: "CodexExecutor", Model: codexModel, Alias: codexModel, APIKey: key,
		AuthIndex: codexAuth, AuthType: "oauth", Generate: true, RequestedAt: app.store.Now(), ResponseHeaders: headers,
		Detail: UsageDetail{InputTokens: tokens, TotalTokens: tokens},
	})
}

func codexHeaders(used5h, used7d float64, reset5h, reset7d time.Time) http.Header {
	return http.Header{
		"X-Codex-Plan-Type":                {"team"},
		"X-Codex-Primary-Used-Percent":     {strconv.FormatFloat(used5h, 'f', -1, 64)},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Primary-Reset-At":         {strconv.FormatInt(reset5h.Unix(), 10)},
		"X-Codex-Secondary-Used-Percent":   {strconv.FormatFloat(used7d, 'f', -1, 64)},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {strconv.FormatInt(reset7d.Unix(), 10)},
		"X-Codex-Credits-Balance":          {"0"},
	}
}

func codexAdmit(t *testing.T, app *App, key string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: strconv.FormatInt(time.Now().UnixNano(), 10), SourceFormat: "openai-response", Model: codexModel, RequestedModel: codexModel,
		Metadata: map[string]any{MetadataCallerScope: billing.CallerScope(key), MetadataRequestPath: "/v1/responses"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

type codexView struct {
	CC          ccbilling.View `json:"cc"`
	Credentials []ccCredential `json:"credentials"`
	KeyRouting  []ccKeyRouting `json:"key_routing"`
}

func readCodexView(t *testing.T, app *App) codexView {
	t.Helper()
	var view codexView
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, &view)
	return view
}

func TestCodexPoolThroughThePlugin(t *testing.T) {
	app, clock := newCodexTestApp(t)
	reset5, reset7 := clock.now.Add(3*time.Hour), clock.now.Add(5*24*time.Hour)
	codexUsage(t, app, codexRemainderKey, 0.2, codexHeaders(10, 20, reset5, reset7))

	view := readCodexView(t, app)
	var codexCredential ccCredential
	for _, credential := range view.Credentials {
		if credential.AuthIndex == codexAuth {
			codexCredential = credential
		}
	}
	if codexCredential.Provider != ccbilling.ProviderCodex || codexCredential.Plan != "team" || codexCredential.SuggestedTier != ccbilling.TierTeam {
		t.Fatalf("codex credential = %+v (all %+v)", codexCredential, view.Credentials)
	}
	var gpt ccbilling.PoolStatus
	for _, pool := range view.CC.Pools {
		if pool.ID == "gpt" {
			gpt = pool
		}
	}
	// Cold start on the Team default: $60 a week, no k, so no 5-hour reservation.
	if gpt.Provider != ccbilling.ProviderCodex || math.Abs(gpt.RemainingWeek-0.9*60*0.8) > 1e-9 || gpt.ReservedWeek != 20 || gpt.FiveHourEnabled {
		t.Fatalf("gpt pool = %+v", gpt)
	}
	for _, account := range view.CC.Accounts {
		if account.AuthIndex == codexAuth && (!account.NeedsK || account.Provider != ccbilling.ProviderCodex || account.Used7d != 20) {
			t.Fatalf("codex account = %+v", account.AccountStatus)
		}
	}
	if response := codexAdmit(t, app, codexRemainderKey); response.Terminate {
		t.Fatalf("remainder refused: %s", response.ResponseBody)
	}
	if status, balance := ccBalance(t, app, codexRemainderKey); status != http.StatusOK || math.Abs(balance-(0.9*60*0.8-20)) > 1e-6 {
		t.Fatalf("remainder balance = %d %v", status, balance)
	}
	if status, balance := ccBalance(t, app, codexFixedKey); status != http.StatusOK || balance != 20 {
		t.Fatalf("fixed balance = %d %v", status, balance)
	}
	// The weekly reservation refuses the remainder key once the pool runs low.
	clock.now = clock.now.Add(time.Minute)
	codexUsage(t, app, codexRemainderKey, 0.2, codexHeaders(20, 65, reset5, reset7))
	if response := codexAdmit(t, app, codexRemainderKey); !response.Terminate || response.StatusCode != http.StatusTooManyRequests ||
		!strings.Contains(refusalMessage(t, response), "weekly reservation") {
		t.Fatalf("remainder at the reserve = %+v %s", response, response.ResponseBody)
	}
	// The Claude pool's keys are unaffected.
	if response := ccAdmit(t, app, ccFixedKey); response.Terminate {
		t.Fatalf("claude fixed key refused: %s", response.ResponseBody)
	}
	// A response saying the limit is reached pauses the account.
	clock.now = clock.now.Add(time.Minute)
	headers := codexHeaders(100, 65, reset5, reset7)
	headers.Set("X-Codex-Limit-Reached", "true")
	codexUsage(t, app, codexRemainderKey, 0.2, headers)
	view = readCodexView(t, app)
	for _, account := range view.CC.Accounts {
		if account.AuthIndex == codexAuth && (!account.Paused || account.PauseReason != ccbilling.PauseLimitReached) {
			t.Fatalf("account at the limit = %+v", account.AccountStatus)
		}
	}
}

func TestCodexPoolsRejectOtherProvidersAccounts(t *testing.T) {
	app, _ := newCodexTestApp(t)
	view := readCodexView(t, app)
	settings := view.CC.Settings
	settings.Pools[1].AuthIndexes = append(settings.Pools[1].AuthIndexes, ccAuth)
	settings.Pools[0].AuthIndexes = []string{}
	response := callManagement(t, app, http.MethodPut, routeCCConfig, nil, settings)
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(response.Body, []byte("backend.cc_account_value_is_value_pool_value_holds_value")) ||
		!bytes.Contains(response.Body, []byte(`"v1":"Claude"`)) || !bytes.Contains(response.Body, []byte(`"v3":"ChatGPT"`)) {
		t.Fatalf("mixed pool = %d %s", response.StatusCode, response.Body)
	}
	settings = readCodexView(t, app).CC.Settings
	settings.Accounts[codexAuth] = ccbilling.AccountSettings{Tier: ccbilling.TierMax5x}
	response = callManagement(t, app, http.MethodPut, routeCCConfig, nil, settings)
	if response.StatusCode != http.StatusBadRequest || !bytes.Contains(response.Body, []byte("backend.cc_tier_value_is_not_a_tier_of_value_accounts")) {
		t.Fatalf("claude tier on a codex account = %d %s", response.StatusCode, response.Body)
	}
}

func TestCodexKeysWithoutProviderRoutingAreFlagged(t *testing.T) {
	app, _ := newCodexTestApp(t)
	view := readCodexView(t, app)
	flagged := map[string]ccKeyRouting{}
	for _, hint := range view.KeyRouting {
		flagged[hint.Scope] = hint
	}
	hint, ok := flagged[billing.CallerScope(codexRemainderKey)]
	if !ok || !hint.Unrestricted || hint.Provider != ccbilling.ProviderCodex {
		t.Fatalf("routing hints = %+v", view.KeyRouting)
	}
	// A route limited to Codex auth files clears the hint.
	route, err := app.store.CreateRoute(billing.Route{Name: "Codex only", Rule: billing.RouteRule{
		CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}}}},
		[]string{billing.CallerScope(codexRemainderKey)})
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range readCodexView(t, app).KeyRouting {
		if hint.Scope == billing.CallerScope(codexRemainderKey) {
			t.Fatalf("a key limited to Codex is flagged: %+v", hint)
		}
	}
	// One that also allows Claude credentials is flagged.
	if _, err := app.store.UpdateRoute(billing.RoutePatch{ID: route.ID, Rule: &billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{
		{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}, {Source: billing.CredentialSourceAuthFiles, Provider: "claude"}}}}, nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hint := range readCodexView(t, app).KeyRouting {
		found = found || hint.Scope == billing.CallerScope(codexRemainderKey) && !hint.Unrestricted
	}
	if !found {
		t.Fatal("a key reaching Claude credentials is not flagged")
	}
}

func TestCodexLimitReachedIsReadForTheMainLimitOnly(t *testing.T) {
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	headers := codexHeaders(100, 50, at.Add(time.Hour), at.Add(48*time.Hour))
	headers.Set("X-Codex-Limit-Reached", "true")
	report := quotaHeaderReport("codex", headers, at)
	if report == nil || !report.LimitReached || report.CreditBalance != "0" {
		t.Fatalf("main limit report = %+v", report)
	}
	headers.Set("X-Codex-Active-Limit", "bengalfox")
	if report := quotaHeaderReport("codex", headers, at); report != nil && report.LimitReached {
		t.Fatalf("another limit's response reached the main limit: %+v", report)
	}
	source := ccbilling.QuotaSource{Provider: "codex", LimitKey: "codex"}
	obs, ok := ccObservation(*quotaHeaderReport("codex", codexHeaders(12.5, 40, at.Add(time.Hour), at.Add(48*time.Hour)), at), source, at)
	if !ok || !obs.Has5h || obs.Used5h != 12.5 || obs.Used7d != 40 || !obs.HasCredits || obs.Credits != 0 {
		t.Fatalf("codex observation = %+v, %v", obs, ok)
	}
	if _, ok := ccObservation(*quotaHeaderReport("codex", codexHeaders(1, 1, at, at), at), ccbilling.QuotaSource{Provider: "claude", LimitKey: "unified"}, at); ok {
		t.Fatal("a codex report was read for a Claude account")
	}
}

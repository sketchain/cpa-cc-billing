package plugin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/ccbilling"
)

// This file wires the CC split (internal/ccbilling) into the plugin. Upstream
// files only call ccAdmission and ccObserve; the routes register themselves.

const (
	routeCC        = "/cc"
	routeCCConfig  = "/cc/settings"
	routeCCRefresh = "/cc/refresh"
	routeCCRebuild = "/cc/rebuild"
	routeBalance   = "/balance"
	routePriceJSON = "/prices/import"
	routePanel     = "/panel-settings"

	weekSeconds = int64(7 * 24 * 60 * 60)
)

func init() {
	managementEndpoints = append(managementEndpoints,
		managementEndpoint{http.MethodGet, routeCC, "View the CC split of Claude accounts between API keys", (*App).ccView},
		managementEndpoint{http.MethodPut, routeCCConfig, "Update the CC split settings", (*App).ccPutSettings},
		managementEndpoint{http.MethodPost, routeCCRefresh, "Process new usage into CC estimates", (*App).ccRefresh},
		managementEndpoint{http.MethodPost, routeCCRebuild, "Rebuild CC segments from the retained request history", (*App).ccRebuild},
		managementEndpoint{http.MethodPost, routePriceJSON, "Import custom model prices from a JSON price list", (*App).importPriceList},
		managementEndpoint{http.MethodGet, routePanel, "View the page display and API key self-service settings", (*App).panelSettings},
		managementEndpoint{http.MethodPut, routePanel, "Update the page display and API key self-service settings", (*App).putPanelSettings},
	)
	resourceEndpoints = append(resourceEndpoints, resourceEndpoint{routeBalance, (*App).ccBalance})
}

// ccHost reads the billing store for the CC manager.
type ccHost struct {
	app  *App
	rate billing.ExchangeRate
}

func (h *ccHost) WithDB(fn func(*sql.DB) error) error {
	// Read the rate before taking the repository lock: ToUSD runs inside it.
	h.rate = h.app.store.Config().CNYPerUSD
	return h.app.store.UseRepository(func(repo billing.Repository) error {
		database, ok := repo.(interface{ SQL() *sql.DB })
		if !ok {
			return fmt.Errorf("The billing repository does not support CC tables")
		}
		return fn(database.SQL())
	})
}

func (h *ccHost) Now() time.Time { return h.app.store.Now() }

func (h *ccHost) ToUSD(amount float64, currency string) float64 {
	rate := h.rate
	if rate <= 0 {
		rate = billing.DefaultExchangeRate
	}
	return rate.Convert(amount, billing.Currency(currency), billing.USD)
}

func (h *ccHost) LatestObservations(authIndexes []string, since time.Time) (map[string]ccbilling.Observation, error) {
	if len(authIndexes) == 0 {
		return map[string]ccbilling.Observation{}, nil
	}
	reports, err := h.app.store.LatestQuotaReports(authIndexes, since)
	if err != nil {
		return nil, err
	}
	result := make(map[string]ccbilling.Observation, len(reports))
	for index, observations := range reports {
		var merged ccbilling.Observation
		for _, observation := range observations {
			if obs, ok := ccObservation(observation.QuotaReport, observation.At); ok {
				if obs.Has5h {
					merged.Has5h, merged.Used5h, merged.Reset5h = true, obs.Used5h, obs.Reset5h
				}
				if obs.Has7d {
					merged.Has7d, merged.Used7d, merged.Reset7d = true, obs.Used7d, obs.Reset7d
				}
				if obs.At.After(merged.At) {
					merged.At = obs.At
				}
			}
		}
		if merged.Has5h || merged.Has7d {
			result[index] = merged
		}
	}
	return result, nil
}

func (h *ccHost) WeeklyPeriod(authIndex string, now time.Time) (ccbilling.WeeklyPeriod, bool, error) {
	histories, err := h.app.store.QuotaWindowHistories(authIndex, "claude", now.Add(-ccbilling.Week-24*time.Hour), now.Add(time.Second))
	if err != nil {
		return ccbilling.WeeklyPeriod{}, false, err
	}
	for _, history := range histories {
		if history.LimitKey != claudeUnifiedLimit || history.Seconds != weekSeconds {
			continue
		}
		periods := billing.QuotaWindowPeriods(ccbilling.Week, history.Reports, now)
		var result ccbilling.WeeklyPeriod
		for _, period := range periods {
			if period.EndReason == billing.QuotaPeriodEarlyReset {
				result.EarlyResetAt = period.End
			}
		}
		if len(periods) == 0 {
			return result, false, nil
		}
		last := periods[len(periods)-1]
		if !last.End.IsZero() || !last.ResetAt.After(now) {
			return result, false, nil
		}
		result.Start, result.ResetAt = last.Start, last.ResetAt
		return result, true, nil
	}
	return ccbilling.WeeklyPeriod{}, false, nil
}

func (h *ccHost) KeyInfo(scope string) (ccbilling.KeyInfo, bool) {
	view, ok := h.app.store.KeyViewForScope(scope)
	if !ok {
		return ccbilling.KeyInfo{}, false
	}
	info := ccbilling.KeyInfo{Preview: view.Preview, Label: view.Label, PlanID: view.PlanID}
	for _, plan := range h.app.store.Plans() {
		if plan.ID == view.PlanID {
			revision, _ := json.Marshal(plan)
			info.PlanRevision = string(revision)
		}
	}
	rate := h.app.store.Config().CNYPerUSD
	for _, window := range view.Windows {
		if window.PeriodSeconds != weekSeconds {
			continue
		}
		info.Anchored = !window.CycleAnchorAt.IsZero()
		for _, balance := range window.Dimensions {
			if balance.Metric != billing.QuotaAmount {
				continue
			}
			limit, _ := balance.Limit.Float64()
			used, _ := balance.Used.Float64()
			info.HasWeekly = true
			info.WeeklyUSD = rate.Convert(limit, balance.Currency, billing.USD)
			info.UsedWeekUSD = rate.Convert(used, balance.Currency, billing.USD)
			info.CycleStart, info.CycleEnd = window.StartAt, window.EndAt
		}
	}
	return info, true
}

func (h *ccHost) KeyUsageUSD(scope string, from, to time.Time) (float64, error) {
	if !from.Before(to) {
		return 0, nil
	}
	// Include requests made at the current instant.
	totals, err := h.app.store.UsageTotals(scope, []billing.UsageQuery{{From: from, To: to.Add(time.Nanosecond)}})
	if err != nil || len(totals) == 0 {
		return 0, err
	}
	return totals[0].CostUSD, nil
}

func (h *ccHost) AlignWeekly(planID string, resetAt time.Time) (ccbilling.Alignment, error) {
	result, err := h.app.store.AlignWindowAnchor(planID, weekSeconds, resetAt)
	if err != nil {
		return ccbilling.Alignment{}, err
	}
	return ccbilling.Alignment{Status: result.Status, WaitUntil: result.WaitUntil}, nil
}

func (h *ccHost) Logf(format string, args ...any) {
	h.app.store.AddPluginLog(billing.PluginLogError, format, args...)
}

func newCCManager(app *App) *ccbilling.Manager {
	manager := ccbilling.NewManager(&ccHost{app: app})
	app.store.SetUsageObserver(manager.ObserveCost)
	return manager
}

// ccConfigured loads the CC estimates when the plugin is configured, so
// admission has a snapshot before traffic arrives.
func (a *App) ccConfigured() {
	if a.cc != nil {
		_ = a.cc.Refresh()
	}
}

// ccObservation reads the unified Claude windows of a quota report.
func ccObservation(report billing.QuotaReport, at time.Time) (ccbilling.Observation, bool) {
	if !strings.EqualFold(report.Provider, "claude") {
		return ccbilling.Observation{}, false
	}
	obs := ccbilling.Observation{At: at}
	for _, window := range report.Windows {
		if window.LimitKey != claudeUnifiedLimit {
			continue
		}
		switch window.Seconds {
		case 5 * 60 * 60:
			obs.Has5h, obs.Used5h, obs.Reset5h = true, window.UsedPercent, window.ResetAt
		case weekSeconds:
			obs.Has7d, obs.Used7d, obs.Reset7d = true, window.UsedPercent, window.ResetAt
		}
	}
	return obs, obs.Has5h || obs.Has7d
}

// ccObserve feeds a response's quota report to the live account state.
func (a *App) ccObserve(authIndex string, report *billing.QuotaReport, at time.Time) {
	if a.cc == nil || report == nil {
		return
	}
	if at.IsZero() {
		at = a.store.Now()
	}
	if obs, ok := ccObservation(*report, at); ok {
		a.cc.Observe(authIndex, obs)
	}
}

// ccAfterUsage refreshes the estimates from usage.handle, which runs after the
// client's request has completed. It never waits for a running refresh.
func (a *App) ccAfterUsage() {
	if a.cc != nil {
		a.cc.MaybeRefresh()
	}
}

// ccAdmission runs before subscription quota admission. It aligns fixed keys'
// weekly cycles and refuses a remainder key's generating request when the
// pool's remaining capacity is reserved for fixed keys.
func (a *App) ccAdmission(sourceFormat, scope string, generate bool) (RequestInterceptResponse, bool) {
	if a.cc == nil || scope == "" {
		return RequestInterceptResponse{}, false
	}
	decision := a.cc.Admit(scope)
	if decision.Allowed || !generate {
		return RequestInterceptResponse{}, false
	}
	headers := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
	if retryAfter := retryAfterSeconds(decision.RetryAt, a.store.Now()); retryAfter > 0 {
		headers.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	message := decision.Message
	if !decision.RetryAt.IsZero() {
		message += " The window resets at " + decision.RetryAt.UTC().Format(time.RFC3339) + "."
	}
	return RequestInterceptResponse{
		Terminate: true, StatusCode: http.StatusTooManyRequests, ResponseHeaders: headers,
		ResponseBody: refusalBody(sourceFormat, quotaExhaustedError, message),
	}, true
}

type ccCredential struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Disabled  bool   `json:"disabled"`
}

type ccKeyOption struct {
	Scope     string `json:"scope"`
	Preview   string `json:"preview,omitempty"`
	Label     string `json:"label,omitempty"`
	PlanName  string `json:"plan_name,omitempty"`
	HasWeekly bool   `json:"has_weekly_window"`
}

func (a *App) ccView(_ ManagementRequest) ManagementResponse {
	view, err := a.cc.View()
	if err != nil {
		return errorResponse(err)
	}
	credentials := []ccCredential{}
	var warning string
	if files, errFiles := a.listHostAuthFiles(); errFiles == nil {
		for _, file := range files {
			provider := strings.ToLower(strings.TrimSpace(file.Provider))
			if provider == "" {
				provider = strings.ToLower(strings.TrimSpace(file.Type))
			}
			if provider != "claude" || strings.TrimSpace(file.AuthIndex) == "" {
				continue
			}
			credentials = append(credentials, ccCredential{AuthIndex: file.AuthIndex, Name: authFileTitle(file), Disabled: file.Disabled})
		}
	} else {
		warning = errFiles.Error()
	}
	keys := []ccKeyOption{}
	for _, key := range a.store.KeyViews() {
		if !key.DeletedAt.IsZero() {
			continue
		}
		option := ccKeyOption{Scope: key.Scope, Preview: key.Preview, Label: key.Label, PlanName: key.PlanName}
		for _, window := range key.Windows {
			for _, balance := range window.Dimensions {
				option.HasWeekly = option.HasWeekly || window.PeriodSeconds == weekSeconds && balance.Metric == billing.QuotaAmount
			}
		}
		keys = append(keys, option)
	}
	return JSONResponse(http.StatusOK, map[string]any{"cc": view, "credentials": credentials, "key_options": keys, "credential_warning": warning})
}

func (a *App) ccPutSettings(req ManagementRequest) ManagementResponse {
	var settings ccbilling.Settings
	if err := decodeStrict(req.Body, &settings); err != nil {
		return errorResponse(err)
	}
	stored, err := a.cc.SetSettings(settings)
	if err != nil {
		return JSONError(http.StatusBadRequest, "invalid", err.Error())
	}
	return JSONResponse(http.StatusOK, map[string]any{"settings": stored})
}

func (a *App) ccRefresh(_ ManagementRequest) ManagementResponse {
	if err := a.cc.Refresh(); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"refreshed": true})
}

func (a *App) ccRebuild(_ ManagementRequest) ManagementResponse {
	if err := a.cc.Rebuild(); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"rebuilt": true})
}

// ccBalance answers new-api's Advanced Custom balance query for the presenting
// key: its weekly balance in USD, reported as 1 when it is zero or less.
func (a *App) ccBalance(_ ManagementRequest, access viewAccess) ManagementResponse {
	if !access.Tracked {
		return apiKeyUnauthorized()
	}
	balance, status := a.cc.Balance(access.Scope)
	switch status {
	case ccbilling.BalanceUnassigned:
		return apiKeyJSONError(http.StatusNotFound, "not_found", "This API key is not assigned to a CC pool")
	case ccbilling.BalanceNotReady:
		response := apiKeyJSONError(http.StatusServiceUnavailable, "not_ready", "CC estimates are still loading; retry shortly")
		response.Headers.Set("Retry-After", "60")
		return response
	}
	return apiKeyJSON(http.StatusOK, map[string]any{"object": "credit_summary", "total_available": ccbilling.ReportedBalance(balance)})
}

// priceListEntry is one model of an imported price list, in USD per million
// tokens. Fields the plugin does not use, such as tier notes, are ignored.
type priceListEntry struct {
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// parsePriceList accepts a list of entries, an object holding that list under
// models, prices or data, or an object of entries keyed by model name.
func parsePriceList(body []byte) ([]priceListEntry, error) {
	var entries []priceListEntry
	if err := json.Unmarshal(body, &entries); err == nil {
		return entries, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, &billing.Error{Kind: billing.KindInvalid, Msg: "The price list must be a JSON array or object"}
	}
	for _, field := range []string{"models", "prices", "data"} {
		if raw, ok := object[field]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, &billing.Error{Kind: billing.KindInvalid, Msg: "Invalid price list: " + err.Error()}
			}
			return entries, nil
		}
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		var entry priceListEntry
		if err := json.Unmarshal(object[name], &entry); err != nil {
			return nil, &billing.Error{Kind: billing.KindInvalid, Msg: "Invalid price list entry " + strconv.Quote(name)}
		}
		if entry.Name == "" {
			entry.Name = name
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (a *App) importPriceList(req ManagementRequest) ManagementResponse {
	entries, err := parsePriceList(req.Body)
	if err != nil {
		return errorResponse(err)
	}
	var prices []billing.CustomPrice
	seen := map[string]bool{}
	models := 0
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		if entry.Input == nil || entry.Output == nil {
			return JSONError(http.StatusBadRequest, "invalid", fmt.Sprintf("Model %q needs input and output prices", name))
		}
		models++
		rates := billing.PriceRates{InputPer1M: *entry.Input, OutputPer1M: *entry.Output, CacheReadPer1M: entry.CacheRead, CacheWritePer1M: entry.CacheWrite}
		for _, id := range append([]string{name}, entry.Aliases...) {
			id = strings.TrimSpace(id)
			normalized := billing.NormalizeModelID(id)
			if id == "" || seen[normalized] {
				continue
			}
			seen[normalized] = true
			price := billing.CustomPrice{ModelID: id, Currency: billing.USD, PriceRates: rates}
			if err := price.Validate(); err != nil {
				return errorResponse(err)
			}
			prices = append(prices, price)
		}
	}
	if len(prices) == 0 {
		return JSONError(http.StatusBadRequest, "invalid", "The price list contains no models")
	}
	for _, price := range prices {
		if _, err := a.store.UpsertPrice(price); err != nil {
			return errorResponse(err)
		}
	}
	return JSONResponse(http.StatusOK, map[string]any{"models": models, "model_ids": len(prices)})
}

func (a *App) panelSettings(_ ManagementRequest) ManagementResponse {
	return JSONResponse(http.StatusOK, map[string]any{"settings": a.cc.PanelSettings(), "tabs": ccbilling.PanelTabs})
}

func (a *App) putPanelSettings(req ManagementRequest) ManagementResponse {
	var settings ccbilling.PanelSettings
	if err := decodeStrict(req.Body, &settings); err != nil {
		return errorResponse(err)
	}
	stored, err := a.cc.SetPanelSettings(settings)
	if err != nil {
		return JSONError(http.StatusBadRequest, "invalid", err.Error())
	}
	return JSONResponse(http.StatusOK, map[string]any{"settings": stored, "tabs": ccbilling.PanelTabs})
}

// ccResourceAllowed reports whether an API key resource route is served. When
// self-service is off, only the balance query new-api uses remains.
func (a *App) ccResourceAllowed(suffix string) bool {
	return suffix == routeBalance || a.cc == nil || a.cc.PanelSettings().AccountSelfService
}

// uiAccountAccessOn marks the page template's API key sign-in entry, which the
// page hides when self-service is off. The page cannot ask before sign-in.
var (
	uiAccountAccessOn  = []byte(`data-account-access="on"`)
	uiAccountAccessOff = []byte(`data-account-access="off"`)
)

func (a *App) ccUIHTML() []byte {
	if a.cc == nil || a.cc.PanelSettings().AccountSelfService {
		return uiHTML
	}
	return bytes.Replace(uiHTML, uiAccountAccessOn, uiAccountAccessOff, 1)
}

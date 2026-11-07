package plugin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/ccbilling"
	"cpa-super-billing/internal/messages"
)

// This file wires the CC split (internal/ccbilling) into the plugin. Upstream
// files only call ccAdmission and ccObserve; the routes register themselves.

const (
	routeCC        = "/cc"
	routeCCConfig  = "/cc/settings"
	routeCCRefresh = "/cc/refresh"
	routeCCRebuild = "/cc/rebuild"
	routeCCTrend   = "/cc/trend"
	routeCCCalib   = "/cc/calibrations"
	routeCCStop    = "/cc/calibrations/stop"
	routeCCEstim   = "/cc/estimates"
	routeCCFore    = "/cc/forecast"
	routeCCSim     = "/cc/simulate"
	routeCCUsage   = "/cc/key-usage"
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
		managementEndpoint{http.MethodGet, routeCCTrend, "View a CC account's retained hourly k evidence and 1% segments", (*App).ccTrend},
		managementEndpoint{http.MethodPost, routeCCRebuild, "Rebuild CC segments from the retained request history", (*App).ccRebuild},
		managementEndpoint{http.MethodPost, routeCCCalib, "Start calibrating a model's CC quota weight on an account", (*App).ccStartCalibration},
		managementEndpoint{http.MethodPost, routeCCStop, "End a running CC quota weight calibration", (*App).ccStopCalibration},
		managementEndpoint{http.MethodDelete, routeCCCalib, "Delete a CC quota weight calibration", (*App).ccDeleteCalibration},
		managementEndpoint{http.MethodGet, routeCCEstim, "View the CC estimates on the basis in effect", (*App).ccEstimates},
		managementEndpoint{http.MethodGet, routeCCFore, "Forecast when CC accounts and pools run out at the recent rate", (*App).ccForecast},
		managementEndpoint{http.MethodPost, routeCCSim, "Simulate a CC pool's history with fixed keys added, changed or removed (read only)", (*App).ccSimulate},
		managementEndpoint{http.MethodGet, routeCCUsage, "View each pooled key's daily usage, fixed key cycles and CC refusals", (*App).ccKeyUsage},
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

func (h *ccHost) LatestObservations(sources map[string]ccbilling.QuotaSource, since time.Time) (map[string]ccbilling.Observation, error) {
	if len(sources) == 0 {
		return map[string]ccbilling.Observation{}, nil
	}
	reports, err := h.app.store.LatestQuotaReports(slices.Sorted(maps.Keys(sources)), since)
	if err != nil {
		return nil, err
	}
	result := make(map[string]ccbilling.Observation, len(reports))
	for index, observations := range reports {
		var merged ccbilling.Observation
		var creditsAt time.Time
		for _, observation := range observations {
			obs, ok := ccObservation(observation.QuotaReport, sources[index], observation.At)
			if obs.HasCredits && !observation.At.Before(creditsAt) {
				merged.HasCredits, merged.Credits, creditsAt = true, obs.Credits, observation.At
			}
			if !ok {
				continue
			}
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
		if merged.Has5h || merged.Has7d {
			result[index] = merged
		}
	}
	return result, nil
}

func (h *ccHost) WeeklyPeriod(authIndex string, source ccbilling.QuotaSource, now time.Time) (ccbilling.WeeklyPeriod, bool, error) {
	histories, err := h.app.store.QuotaWindowHistories(authIndex, source.Provider, now.Add(-ccbilling.Week-24*time.Hour), now.Add(time.Second))
	if err != nil {
		return ccbilling.WeeklyPeriod{}, false, err
	}
	for _, history := range histories {
		if history.LimitKey != source.LimitKey || history.Seconds != weekSeconds {
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
	view, ok, seq := h.app.store.KeyViewForScopeAt(scope)
	if !ok {
		return ccbilling.KeyInfo{Seq: seq}, false
	}
	info := ccbilling.KeyInfo{Preview: view.Preview, Label: view.Label, PlanID: view.PlanID, Seq: seq}
	if plan, found := h.app.store.PlanByID(view.PlanID); found {
		// Only the plan's configuration: its pool cycles and quota view change
		// with every request and would invalidate the alignment cache.
		revision, _ := json.Marshal(plan)
		info.PlanRevision = string(revision)
		info.SharedQuota = plan.SharedQuota
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

func (h *ccHost) KeyUsageUSD(scope string, from, to time.Time) (float64, uint64, error) {
	if !from.Before(to) {
		// An empty range reads nothing, so it includes no usage either.
		return 0, 0, nil
	}
	// Include requests made at the current instant.
	totals, seq, err := h.app.store.UsageTotalsAt(scope, []billing.UsageQuery{{From: from, To: to.Add(time.Nanosecond)}})
	if err != nil {
		return 0, 0, err
	}
	return totals[0].CostUSD, seq, nil
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

// ccObservation reads the main windows of a quota report from the source of
// the account it belongs to: the provider's main limit (Claude's unified
// limit, ChatGPT's Codex limit), the credit balance and whether the limit is
// used up. The second result reports whether it has a main window.
func ccObservation(report billing.QuotaReport, source ccbilling.QuotaSource, at time.Time) (ccbilling.Observation, bool) {
	if source.Provider == "" || !strings.EqualFold(report.Provider, source.Provider) {
		return ccbilling.Observation{}, false
	}
	obs := ccbilling.Observation{At: at}
	if balance, err := strconv.ParseFloat(strings.TrimSpace(report.CreditBalance), 64); err == nil && !math.IsNaN(balance) && !math.IsInf(balance, 0) {
		obs.HasCredits, obs.Credits = true, balance
	}
	for _, window := range report.Windows {
		if window.LimitKey != source.LimitKey {
			continue
		}
		switch window.Seconds {
		case 5 * 60 * 60:
			obs.Has5h, obs.Used5h, obs.Reset5h = true, window.UsedPercent, window.ResetAt
		case weekSeconds:
			obs.Has7d, obs.Used7d, obs.Reset7d = true, window.UsedPercent, window.ResetAt
		}
	}
	obs.LimitReached = report.LimitReached && (obs.Has5h || obs.Has7d)
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
	source, configured := a.cc.AccountSource(authIndex)
	if !configured {
		return
	}
	if obs, ok := ccObservation(*report, source, at); ok {
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
	a.ccReportBlock(scope, decision)
	headers := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
	if retryAfter := retryAfterSeconds(decision.RetryAt, a.store.Now()); retryAfter > 0 {
		headers.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	message := decision.Message
	if !decision.RetryAt.IsZero() && decision.Reason != ccbilling.RefusedNotLoaded {
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
	Provider  string `json:"provider"`
	// Plan is the subscription plan the account's latest quota report named;
	// SuggestedTier is the tier it corresponds to.
	Plan          string `json:"plan,omitempty"`
	SuggestedTier string `json:"suggested_tier,omitempty"`
}

// ccKeyRouting flags an assigned key whose routing rules let it reach
// credentials of another provider than its pool's. It is a hint only: the
// split does not refuse such requests.
type ccKeyRouting struct {
	Scope    string `json:"scope"`
	Provider string `json:"provider"`
	// Unrestricted means the rules do not restrict credentials at all.
	Unrestricted bool `json:"unrestricted"`
}

// ccProviderOf is the split provider of a host credential, "" for other
// providers.
func ccProviderOf(file hostAuthFile) string {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(file.Type))
	}
	if slices.Contains(ccbilling.Providers, provider) {
		return provider
	}
	return ""
}

// ccRoutingHints lists the assigned keys whose routing rules do not keep
// them on their pool's provider.
func (a *App) ccRoutingHints(settings ccbilling.Settings, files []hostAuthFile) []ccKeyRouting {
	hints := []ccKeyRouting{}
	for _, scope := range slices.Sorted(maps.Keys(settings.Keys)) {
		key := settings.Keys[scope]
		var provider string
		for _, pool := range settings.Pools {
			if pool.ID == key.PoolID {
				provider = pool.Provider
			}
		}
		if provider == "" {
			continue
		}
		decision := a.store.ResolveRouting(scope, "", "")
		if !decision.RestrictsCredentials() {
			hints = append(hints, ccKeyRouting{Scope: scope, Provider: provider, Unrestricted: true})
			continue
		}
		if a.ccRoutingReachesOthers(decision, provider, files) {
			hints = append(hints, ccKeyRouting{Scope: scope, Provider: provider})
		}
	}
	return hints
}

func (a *App) ccRoutingReachesOthers(decision billing.RoutingDecision, provider string, files []hostAuthFile) bool {
	for _, selector := range decision.CredentialProviders {
		if !strings.EqualFold(selector.Provider, provider) {
			return true
		}
	}
	if len(decision.CredentialIDs) == 0 && len(decision.CredentialProviders) == 0 {
		// Only denials: every credential they leave is allowed, including
		// configured API keys of other providers.
		return true
	}
	for _, file := range files {
		other := strings.ToLower(strings.TrimSpace(file.Provider))
		if other == "" {
			other = strings.ToLower(strings.TrimSpace(file.Type))
		}
		if other != provider && routingAllowsAuthFile(file, decision) {
			return true
		}
	}
	a.routingMu.Lock()
	defer a.routingMu.Unlock()
	for _, ref := range decision.CredentialIDs {
		if view, known := a.credentials[ref]; known && view.Provider != "" && !strings.EqualFold(view.Provider, provider) {
			return true
		}
	}
	return false
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
	files, errFiles := a.listHostAuthFiles()
	if errFiles == nil {
		var indexes []string
		for _, file := range files {
			provider := ccProviderOf(file)
			if provider == "" || strings.TrimSpace(file.AuthIndex) == "" {
				continue
			}
			credentials = append(credentials, ccCredential{AuthIndex: file.AuthIndex, Name: authFileTitle(file), Disabled: file.Disabled, Provider: provider})
			indexes = append(indexes, file.AuthIndex)
		}
		if reports, err := a.store.LatestQuotaReports(indexes, a.store.Now().Add(-30*24*time.Hour)); err == nil {
			for i := range credentials {
				var planAt time.Time
				for _, observation := range reports[credentials[i].AuthIndex] {
					if observation.Plan != "" && strings.EqualFold(observation.Provider, credentials[i].Provider) && !observation.At.Before(planAt) {
						credentials[i].Plan, planAt = observation.Plan, observation.At
					}
				}
				credentials[i].SuggestedTier = ccbilling.SuggestedTier(credentials[i].Provider, credentials[i].Plan)
			}
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
	var models []string
	for _, provider := range ccbilling.Providers {
		for _, item := range view.Settings.Providers[provider].Weights.Items {
			models = append(models, item.Models...)
		}
	}
	prices := map[string]billing.PriceMultiplier{}
	var priceWarning string
	if len(models) > 0 {
		if prices, err = a.store.PriceMultipliers(models); err != nil {
			prices, priceWarning = map[string]billing.PriceMultiplier{}, err.Error()
		}
	}
	routing := []ccKeyRouting{}
	if errFiles == nil {
		routing = a.ccRoutingHints(view.Settings, files)
	}
	body := map[string]any{"cc": view, "credentials": credentials, "key_options": keys, "credential_warning": warning,
		"price_multipliers": prices, "price_warning": priceWarning, "key_routing": routing}
	if legacy, ok := a.store.LegacyDatabaseNotice(); ok {
		body["legacy_database"] = legacy
	}
	return JSONResponse(http.StatusOK, body)
}

// ccCalibrationError maps a calibration error to its response.
func ccCalibrationError(err error) ManagementResponse {
	switch {
	case ccbilling.IsInvalid(err):
		return jsonMessageError(http.StatusBadRequest, "invalid", messages.FromError(err))
	case ccbilling.IsCalibrationNotFound(err):
		return JSONError(http.StatusNotFound, "not_found", "The calibration does not exist or has already ended")
	}
	return errorResponse(err)
}

func (a *App) ccStartCalibration(req ManagementRequest) ManagementResponse {
	var body struct {
		ItemID    string `json:"item_id"`
		AuthIndex string `json:"auth_index"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	calibration, err := a.cc.StartCalibration(strings.TrimSpace(body.ItemID), strings.TrimSpace(body.AuthIndex))
	if err != nil {
		return ccCalibrationError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"calibration": calibration})
}

func (a *App) ccStopCalibration(req ManagementRequest) ManagementResponse {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	if err := a.cc.StopCalibration(body.ID); err != nil {
		return ccCalibrationError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"stopped": true})
}

func (a *App) ccDeleteCalibration(req ManagementRequest) ManagementResponse {
	id, err := strconv.ParseInt(strings.TrimSpace(req.Query.Get("id")), 10, 64)
	if err != nil || id <= 0 {
		return JSONError(http.StatusBadRequest, "invalid", "id must be a calibration ID")
	}
	if err := a.cc.DeleteCalibration(id); err != nil {
		return ccCalibrationError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"deleted": true})
}

// ccNotReady answers a read that needs the first estimate snapshot.
func ccNotReady() ManagementResponse {
	response := JSONError(http.StatusServiceUnavailable, "not_ready", "CC estimates are still loading; retry shortly")
	response.Headers.Set("Retry-After", "60")
	return response
}

// ccInsightError maps an error of the forecast, simulation or key usage.
func ccInsightError(err error) ManagementResponse {
	switch {
	case ccbilling.IsNotLoaded(err):
		return ccNotReady()
	case ccbilling.IsInvalid(err):
		return jsonMessageError(http.StatusBadRequest, "invalid", messages.FromError(err))
	}
	return errorResponse(err)
}

// ccPriceMultipliers returns, for the models under test, the price table
// multiplier of those with a custom price whose input and output multipliers
// agree: the conversion of key cost checks whether it already carries the
// quota multiplier. Models without one are left out.
func (a *App) ccPriceMultipliers() map[string]float64 {
	estimates, ok := a.cc.Estimates()
	result := map[string]float64{}
	if !ok {
		return result
	}
	var models []string
	for _, provider := range ccbilling.Providers {
		if weights := estimates.For(provider); weights.Basis == ccbilling.BasisWeighted {
			for _, weight := range weights.Weights {
				models = append(models, weight.Models...)
			}
		}
	}
	if len(models) == 0 {
		return result
	}
	prices, err := a.store.PriceMultipliers(models)
	if err != nil {
		a.store.AddPluginLog(billing.PluginLogError, "CC could not read price multipliers; key costs are converted with every quota multiplier: %v", err)
		return result
	}
	for model, price := range prices {
		if price.Custom && price.Input > 0 && math.Abs(price.Input-price.Output) <= 1e-9*math.Max(price.Input, price.Output) {
			result[model] = price.Input
		}
	}
	return result
}

// ccRetainedFrom is where the retained request history begins.
func (a *App) ccRetainedFrom() time.Time {
	days := a.store.Config().RequestEventRetentionDays
	return a.store.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

// ccDuration reads a whole-number query parameter in unit, within limits.
func ccDuration(req ManagementRequest, name string, unit, fallback, low, high time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(req.Query.Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	duration := time.Duration(value) * unit
	if err != nil || duration < low || duration > high {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", name, low/unit, high/unit)
	}
	return duration, nil
}

// ccForecast projects each pool and account at the recent consumption rate.
func (a *App) ccForecast(req ManagementRequest) ManagementResponse {
	window5h, err := ccDuration(req, "window_5h_minutes", time.Minute, ccbilling.DefaultForecastWindow5h,
		ccbilling.MinForecastWindow5h, ccbilling.MaxForecastWindow5h)
	if err != nil {
		return JSONError(http.StatusBadRequest, "invalid", err.Error())
	}
	window7d, err := ccDuration(req, "window_7d_hours", time.Hour, ccbilling.DefaultForecastWindow7d,
		ccbilling.MinForecastWindow7d, ccbilling.MaxForecastWindow7d)
	if err != nil {
		return JSONError(http.StatusBadRequest, "invalid", err.Error())
	}
	forecast, err := a.cc.Forecast(ccbilling.ForecastOptions{Window5h: window5h, Window7d: window7d, Prices: a.ccPriceMultipliers()})
	if err != nil {
		return ccInsightError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"forecast": forecast})
}

// ccSimulate replays a pool's history with hypothetical fixed keys. It
// changes neither the settings nor the database.
func (a *App) ccSimulate(req ManagementRequest) ManagementResponse {
	var request ccbilling.SimRequest
	if err := decodeStrict(req.Body, &request); err != nil {
		return errorResponse(err)
	}
	result, err := a.cc.Simulate(request, ccbilling.SimOptions{RetainedFrom: a.ccRetainedFrom(), Prices: a.ccPriceMultipliers()})
	if err != nil {
		return ccInsightError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"simulation": result})
}

// ccKeyUsage returns each pooled key's hourly cost, refusals and cycles.
func (a *App) ccKeyUsage(_ ManagementRequest) ManagementResponse {
	usage, err := a.cc.KeyUsage(a.ccRetainedFrom())
	if err != nil {
		return ccInsightError(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"usage": usage})
}

// ccEstimates returns the latest estimates on the basis in effect, the same
// values later features read through the manager. It never refreshes.
func (a *App) ccEstimates(_ ManagementRequest) ManagementResponse {
	estimates, ok := a.cc.Estimates()
	if !ok {
		return ccNotReady()
	}
	return JSONResponse(http.StatusOK, map[string]any{"estimates": estimates})
}

func (a *App) ccPutSettings(req ManagementRequest) ManagementResponse {
	var settings ccbilling.Settings
	if err := decodeStrict(req.Body, &settings); err != nil {
		return errorResponse(err)
	}
	if err := a.ccCheckPoolProviders(settings); err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid", messages.FromError(err))
	}
	stored, err := a.cc.SetSettings(settings)
	if err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid", messages.FromError(err))
	}
	return JSONResponse(http.StatusOK, map[string]any{"settings": stored})
}

// ccCheckPoolProviders refuses a pool holding an account of another provider
// than the pool's, as the host's credential list tells it. Accounts the host
// does not list, such as deleted ones, cannot be checked and are kept.
func (a *App) ccCheckPoolProviders(settings ccbilling.Settings) error {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return nil
	}
	known := map[string]hostAuthFile{}
	for _, file := range files {
		if index := strings.TrimSpace(file.AuthIndex); index != "" {
			known[index] = file
		}
	}
	for _, pool := range settings.Pools {
		provider := strings.TrimSpace(pool.Provider)
		if provider == "" {
			provider = ccbilling.ProviderClaude
		}
		label := strings.TrimSpace(pool.Name)
		if label == "" {
			label = strings.TrimSpace(pool.ID)
		}
		for _, index := range pool.AuthIndexes {
			file, listed := known[strings.TrimSpace(index)]
			if !listed {
				continue
			}
			if actual := ccProviderOf(file); actual != provider {
				kind := ccbilling.ProviderLabel(actual)
				if actual == "" {
					kind = strings.TrimSpace(file.Provider)
				}
				return messages.Errorf("Account %s is a %s account; pool %q only holds %s accounts",
					authFileTitle(file), kind, label, ccbilling.ProviderLabel(provider))
			}
		}
	}
	return nil
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

// ccTrend returns an account's retained k evidence and segments for the
// quota trend charts. It reads stored data only: no refresh, no writes.
func (a *App) ccTrend(req ManagementRequest) ManagementResponse {
	authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
	if authIndex == "" {
		return JSONError(http.StatusBadRequest, "invalid", "auth_index is required")
	}
	trend, found, err := a.cc.Trend(authIndex)
	if err != nil {
		return errorResponse(err)
	}
	if !found {
		return JSONError(http.StatusNotFound, "not_found", "The account is not configured in the CC split")
	}
	return JSONResponse(http.StatusOK, map[string]any{"trend": trend})
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
	// One transaction: a failure leaves every existing price as it was.
	if err := a.store.UpsertPrices(prices); err != nil {
		return errorResponse(err)
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

// ccAttachAuthFiles adds the CC estimate of pooled Claude accounts to the auth
// files page. It reads the latest snapshot only.
func (a *App) ccAttachAuthFiles(views []authFileView) {
	if a.cc == nil {
		return
	}
	summaries := a.cc.AccountSummaries()
	for i := range views {
		if summary, ok := summaries[views[i].AuthIndex]; ok && strings.EqualFold(views[i].Category, summary.Provider) {
			views[i].CC = &summary
		}
	}
}

// ccBlockLog remembers which CC refusals were logged: one line per key,
// reason and window reset, like the subscription quota's block log.
type ccBlockLog struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// ccReportBlock logs a remainder key's refusal once per key, reason and
// window, with the estimate that decided it.
func (a *App) ccReportBlock(scope string, decision ccbilling.Decision) {
	now := a.store.Now()
	// Every refusal is counted, in memory; only the log line is deduplicated.
	a.cc.RecordBlock(scope, decision.Reason, now)
	signature := scope + "\x00" + decision.Reason + "\x00" + decision.RetryAt.UTC().Format(time.RFC3339)
	if decision.Reason == ccbilling.RefusedNotLoaded {
		// Its retry time moves with every request; one line per load is enough.
		signature = scope + "\x00" + decision.Reason
	}
	expires := decision.RetryAt
	if !expires.After(now) {
		expires = now.Add(time.Hour)
	}
	a.ccBlocks.mu.Lock()
	if a.ccBlocks.seen == nil {
		a.ccBlocks.seen = map[string]time.Time{}
	}
	for key, until := range a.ccBlocks.seen {
		if !until.After(now) {
			delete(a.ccBlocks.seen, key)
		}
	}
	_, logged := a.ccBlocks.seen[signature]
	a.ccBlocks.seen[signature] = expires
	a.ccBlocks.mu.Unlock()
	if logged {
		return
	}
	name := scope
	if view, ok := a.store.KeyViewForScope(scope); ok {
		name = view.Preview
		if view.Label != "" {
			name = view.Label + " (" + view.Preview + ")"
		}
	}
	a.store.AddPluginLog(billing.PluginLogInfo, "CC blocked: %s reason=%s: %s", name, decision.Reason, decision.Message)
}

// ccKeyKind is a key's CC split type for the API key page.
// ccRefuseSharedPlan refuses a plan change that would leave a CC fixed key on
// a shared quota plan: creating or editing a shared plan with fixed keys among
// its keys, switching such a plan to a shared quota, or binding a fixed key to
// a shared plan. The CC settings check refuses the other direction.
func (a *App) ccRefuseSharedPlan(plan string, shared bool, scopes []string) (ManagementResponse, bool) {
	if a.cc == nil || !shared {
		return ManagementResponse{}, false
	}
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if a.cc.KeyKind(scope) != ccbilling.KeyFixed {
			continue
		}
		label := scope
		if view, ok := a.store.KeyViewForScope(scope); ok && view.Preview != "" {
			label = view.Preview
		}
		err := messages.Errorf("Subscription plan %q cannot share quota while CC fixed key %s is bound to it; give the fixed key a plan without shared quota", plan, label)
		return jsonMessageError(http.StatusConflict, string(billing.KindConflict), messages.FromError(err)), true
	}
	return ManagementResponse{}, false
}

// ccRefuseSharedPlanUpdate applies ccRefuseSharedPlan to a plan edit: the
// edit's shared setting and key selection, or the plan's current ones.
func (a *App) ccRefuseSharedPlanUpdate(patch billing.PlanPatch, scopes *[]string) (ManagementResponse, bool) {
	plan, found := a.store.PlanByID(patch.ID)
	if !found {
		// The store reports the missing plan.
		return ManagementResponse{}, false
	}
	shared := plan.SharedQuota
	if patch.SharedQuota != nil {
		shared = *patch.SharedQuota
	}
	if !shared {
		return ManagementResponse{}, false
	}
	var members []string
	if scopes != nil {
		members = *scopes
	} else {
		for _, key := range a.store.KeyViews() {
			if key.PlanID == plan.ID && key.DeletedAt.IsZero() {
				members = append(members, key.Scope)
			}
		}
	}
	return a.ccRefuseSharedPlan(plan.ID, shared, members)
}

func (a *App) ccKeyKind(scope string) string {
	if a.cc == nil {
		return ""
	}
	return a.cc.KeyKind(scope)
}

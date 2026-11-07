package plugin

import (
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"cpa-super-billing/internal/billing"
)

var codexQuotaGroup = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,120}$`)

// usageQuotaReport is what an OAuth credential's response reported about its quota.
func usageQuotaReport(record UsageRecord, fallback time.Time) *billing.QuotaReport {
	if !strings.EqualFold(strings.TrimSpace(record.AuthType), "oauth") || strings.TrimSpace(record.AuthIndex) == "" {
		return nil
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = fallback
	}
	return quotaHeaderReport(record.Provider, record.ResponseHeaders, at)
}

// quotaHeaderReport reads what response headers received at at say about a
// credential's quota; nil means they say nothing. A window reported twice
// keeps its later value.
func quotaHeaderReport(provider string, input http.Header, at time.Time) *billing.QuotaReport {
	headers := make(http.Header)
	for key, values := range input {
		if len(values) > 0 && len(values[0]) <= 256 {
			headers.Set(strings.TrimSpace(key), strings.TrimSpace(values[0]))
		}
	}
	report := billing.QuotaReport{Provider: strings.ToLower(strings.TrimSpace(provider))}
	switch report.Provider {
	case "codex":
		groups := []string{}
		for key := range headers {
			if strings.HasPrefix(key, "X-Codex-") && strings.HasSuffix(key, "-Limit-Name") {
				group := strings.TrimSuffix(strings.TrimPrefix(key, "X-Codex-"), "-Limit-Name")
				if codexQuotaGroup.MatchString(group) && headers.Get(key) != "" {
					groups = append(groups, group)
				}
			}
		}
		sort.Strings(groups)
		// The ungrouped windows belong to the limit the response names: the main
		// limit ("premium", or no name on older responses), a listed additional
		// limit they copy, or another pool. Image requests report their pool's
		// 24-hour window there, so the main limit keeps only its 5-hour and
		// weekly windows.
		if active := headers.Get("X-Codex-Active-Limit"); active == "" || quotaLimitAlias(active) == "premium" {
			appendCodexWindows(&report, headers, "X-Codex-", codexMainLimit, at)
			report.LimitReached = strings.EqualFold(headers.Get("X-Codex-Limit-Reached"), "true") && len(report.Windows) > 0
		}
		for _, group := range groups {
			prefix := "X-Codex-" + group + "-"
			appendCodexWindows(&report, headers, prefix, codexAdditionalLimit(headers.Get(prefix+"Limit-Name")), at)
		}
		if plan := headers.Get("X-Codex-Plan-Type"); len(plan) <= 40 && codexQuotaGroup.MatchString(plan) {
			report.Plan = plan
		}
		appendCodexWindows(&report, headers, "X-Codex-Code-Review-", codexCodeReviewLimit, at)
		report.CreditBalance = codexCreditBalance(headers.Get("X-Codex-Credits-Balance"))
		if value := headers.Get("X-Codex-Credits-Unlimited"); value == "true" || value == "false" {
			unlimited := value == "true"
			report.CreditsUnlimited = &unlimited
		}
	case "claude":
		for _, window := range []struct {
			role    string
			seconds int64
		}{{"5h", 5 * 60 * 60}, {"7d", 7 * 24 * 60 * 60}} {
			prefix := "Anthropic-Ratelimit-Unified-" + window.role + "-"
			used, ok := finiteQuotaHeader(headers, prefix+"Utilization")
			if !ok || used < 0 || math.IsInf(used*100, 0) {
				continue
			}
			addQuotaWindow(&report, billing.QuotaReportWindow{LimitKey: claudeUnifiedLimit, Seconds: window.seconds,
				UsedPercent: used * 100, ResetAt: quotaHeaderReset(headers, prefix, "Reset", at)})
		}
	}
	if report.Plan == "" && report.CreditBalance == "" && report.CreditsUnlimited == nil && len(report.Windows) == 0 {
		return nil
	}
	return &report
}

func appendCodexWindows(report *billing.QuotaReport, headers http.Header, prefix string, limit codexLimit, at time.Time) {
	for _, role := range []string{"Primary", "Secondary"} {
		key := prefix + role + "-"
		used, ok := finiteQuotaHeader(headers, key+"Used-Percent")
		minutes, err := strconv.ParseInt(headers.Get(key+"Window-Minutes"), 10, 64)
		if !ok || used < 0 || err != nil || minutes <= 0 || minutes > 366*24*60 ||
			limit == codexMainLimit && minutes != 5*60 && minutes != 7*24*60 {
			continue
		}
		addQuotaWindow(report, billing.QuotaReportWindow{LimitKey: limit.key, Seconds: minutes * 60, UsedPercent: used,
			ResetAt: quotaHeaderReset(headers, key, "Reset-At", at)})
	}
}

func addQuotaWindow(report *billing.QuotaReport, window billing.QuotaReportWindow) {
	for i := range report.Windows {
		if report.Windows[i].LimitKey == window.LimitKey && report.Windows[i].Seconds == window.Seconds {
			report.Windows[i] = window
			return
		}
	}
	report.Windows = append(report.Windows, window)
}

// reportQuota presents a report made at at.
func reportQuota(report billing.QuotaReport, at time.Time) authQuotaResponse {
	result := authQuotaResponse{FetchedAt: at, Plan: codexPlanLabel(report.Plan), Quota: []quotaRow{}}
	if report.CreditBalance != "" {
		result.CreditBalance, result.CreditBalanceObservedAt = report.CreditBalance, at
	}
	if report.CreditsUnlimited != nil {
		result.CreditsUnlimited, result.CreditsUnlimitedObservedAt = *report.CreditsUnlimited, at
	}
	for _, window := range report.Windows {
		labelPrefix, meter := reportedLimit(report.Provider, window.LimitKey)
		value := quotaWindow{Seconds: window.Seconds, RemainingPercent: remainingPercent(100 - window.UsedPercent), meter: meter}
		if !window.ResetAt.IsZero() {
			value.ResetAt = window.ResetAt.UTC().Format(time.RFC3339)
		}
		row := value.row(window.LimitKey, labelPrefix)
		row.ObservedAt = at
		result.Quota = append(result.Quota, row)
	}
	return result
}

func quotaHeaderReset(headers http.Header, prefix, resetKey string, at time.Time) time.Time {
	if reset, err := strconv.ParseInt(headers.Get(prefix+resetKey), 10, 64); err == nil && reset > 0 && reset <= 253402300799 {
		return time.Unix(reset, 0).UTC()
	}
	if resetKey == "Reset-At" {
		if after, err := strconv.ParseInt(headers.Get(prefix+"Reset-After-Seconds"), 10, 64); err == nil && after >= 0 && after <= 366*24*60*60 {
			return at.Add(time.Duration(after) * time.Second).UTC()
		}
	}
	return time.Time{}
}

func finiteQuotaHeader(headers http.Header, key string) (float64, bool) {
	value, err := strconv.ParseFloat(headers.Get(key), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func quotaLimitAlias(value string) string {
	value = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(value))
	return strings.TrimPrefix(value, "codex")
}

// observedAuthQuota rebuilds a credential's quota from its recorded reports,
// oldest first. A response reports only the windows of the limit it used, so
// each report updates just the windows it lists. A window drops out once its
// limit is reported after the window's reset time without it, as the 5-hour
// window is after an upgrade to a plan that has none.
func observedAuthQuota(category, revision string, observations []billing.QuotaObservation) *authQuotaResponse {
	var result *authQuotaResponse
	reportedAt := make(map[string]time.Time)
	for _, observation := range observations {
		if !strings.EqualFold(observation.Provider, category) {
			continue
		}
		if result == nil {
			result = &authQuotaResponse{AuthRevision: revision, Quota: []quotaRow{}}
		}
		decoded := reportQuota(observation.QuotaReport, observation.At)
		result.FetchedAt = observation.At
		if decoded.Plan != "" {
			result.Plan, result.PlanObservedAt = decoded.Plan, observation.At
		}
		if !decoded.CreditBalanceObservedAt.IsZero() {
			result.CreditBalance, result.CreditBalanceObservedAt = decoded.CreditBalance, observation.At
		}
		if !decoded.CreditsUnlimitedObservedAt.IsZero() {
			result.CreditsUnlimited, result.CreditsUnlimitedObservedAt = decoded.CreditsUnlimited, observation.At
		}
		for _, row := range decoded.Quota {
			reportedAt[row.LimitKey] = observation.At
			if index := slices.IndexFunc(result.Quota, row.sameWindow); index >= 0 {
				result.Quota[index] = row
			} else {
				result.Quota = append(result.Quota, row)
			}
		}
	}
	if result == nil {
		return nil
	}
	result.Quota = slices.DeleteFunc(result.Quota, func(row quotaRow) bool {
		resetAt, err := time.Parse(time.RFC3339, row.ResetAt)
		return err == nil && reportedAt[row.LimitKey].After(row.ObservedAt) && !reportedAt[row.LimitKey].Before(resetAt)
	})
	slices.SortStableFunc(result.Quota, compareQuotaWindows)
	return result
}

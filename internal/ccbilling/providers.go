package ccbilling

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"cpa-super-billing/internal/messages"
)

// Subscription providers the split supports, by the provider name CLIProxyAPI
// gives their OAuth credentials.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// Providers lists the supported providers in display order.
var Providers = []string{ProviderClaude, ProviderCodex}

// Tier ids. Claude's are the subscription tiers; ChatGPT (Codex) has Team.
const (
	TierTeam = "team"
)

// Tier is one subscription tier of a provider: its default weekly total, used
// while an account is in cold start, and its default k. A nil K means no
// default: an account then needs a manual or estimated k for 5-hour
// reservations.
type Tier struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	WeeklyUSD float64  `json:"weekly_usd"`
	K         *float64 `json:"k"`
}

// ParamOverrides replace global parameters for one provider's accounts; nil
// keeps the global value.
type ParamOverrides struct {
	BlockGapMinutes  *float64 `json:"block_gap_minutes,omitempty"`
	WindowPercent    *float64 `json:"window_percent,omitempty"`
	KMinWeeklyPoints *float64 `json:"k_min_weekly_points,omitempty"`
	ColdStartBlocks  *int     `json:"cold_start_blocks,omitempty"`
	SafetyFactor     *float64 `json:"safety_factor,omitempty"`
}

func (o ParamOverrides) apply(params Params) Params {
	if o.BlockGapMinutes != nil {
		params.BlockGapMinutes = *o.BlockGapMinutes
	}
	if o.WindowPercent != nil {
		params.WindowPercent = *o.WindowPercent
	}
	if o.KMinWeeklyPoints != nil {
		params.KMinWeeklyPoints = *o.KMinWeeklyPoints
	}
	if o.ColdStartBlocks != nil {
		params.ColdStartBlocks = *o.ColdStartBlocks
	}
	if o.SafetyFactor != nil {
		params.SafetyFactor = *o.SafetyFactor
	}
	return params
}

// SegmentRules decide which of an account's requests and reports feed its
// segments. They are fixed per provider.
type SegmentRules struct {
	// MainWindowOnly counts a request's cost only when its own quota report
	// carries a main window of the provider's limit: a request metered
	// against another pool (an additional model limit, code review, image
	// generation) reports only that pool, and its cost does not move the
	// main utilization.
	MainWindowOnly bool `json:"main_window_only"`
	// ExcludeUnpriced leaves out requests of models without a price, whose
	// recorded cost of zero would make the account look cheaper per point.
	ExcludeUnpriced bool `json:"exclude_unpriced"`
	// PauseWhenExhausted stops segments while a main window is used up or
	// credits are being consumed: that usage does not move the utilization.
	PauseWhenExhausted bool `json:"pause_when_exhausted"`
}

// ProviderProfile is what the split knows about one provider.
type ProviderProfile struct {
	// LimitKey is the quota report limit whose 5-hour and 7-day windows are
	// the subscription's main windows.
	LimitKey string `json:"limit_key"`
	// Tiers in display order; the first is the default of accounts without
	// a tier.
	Tiers   []Tier         `json:"tiers"`
	Params  ParamOverrides `json:"params"`
	Rules   SegmentRules   `json:"segment_rules"`
	Weights WeightSettings `json:"weights"`
}

// ProviderLabel is a provider's display name in messages.
func ProviderLabel(provider string) string { return providerLabel(provider) }

// SuggestedTier is the tier a provider's subscription plan, as its quota
// reports name it, corresponds to; "" when none does.
func SuggestedTier(provider, plan string) string {
	profile, ok := DefaultProviderProfile(provider)
	if plan = strings.ToLower(strings.TrimSpace(plan)); !ok || plan == "" || !profile.hasTier(plan) {
		return ""
	}
	return plan
}

// providerLabel is a provider's display name in messages.
func providerLabel(provider string) string {
	switch provider {
	case ProviderClaude:
		return "Claude"
	case ProviderCodex:
		return "ChatGPT"
	}
	return provider
}

func floatPointer(value float64) *float64 { return &value }

// DefaultProviderProfile is the built-in profile of a provider.
func DefaultProviderProfile(provider string) (ProviderProfile, bool) {
	switch provider {
	case ProviderClaude:
		return ProviderProfile{LimitKey: "unified", Tiers: []Tier{
			{ID: TierPro, Name: "Pro", WeeklyUSD: 250, K: floatPointer(8)},
			{ID: TierMax5x, Name: "Max 5x", WeeklyUSD: 1250, K: floatPointer(8)},
			{ID: TierMax20x, Name: "Max 20x", WeeklyUSD: 2500, K: floatPointer(8)},
		}, Weights: DefaultWeightSettings()}, true
	case ProviderCodex:
		return ProviderProfile{LimitKey: "codex", Tiers: []Tier{{ID: TierTeam, Name: "Team", WeeklyUSD: 60}},
			Rules: SegmentRules{MainWindowOnly: true, ExcludeUnpriced: true, PauseWhenExhausted: true}, Weights: DefaultWeightSettings()}, true
	}
	return ProviderProfile{}, false
}

// DefaultProviders returns every built-in profile.
func DefaultProviders() map[string]ProviderProfile {
	result := make(map[string]ProviderProfile, len(Providers))
	for _, provider := range Providers {
		result[provider], _ = DefaultProviderProfile(provider)
	}
	return result
}

// tier returns the profile's tier, or its first tier when id is unknown.
func (p ProviderProfile) tier(id string) Tier {
	for _, tier := range p.Tiers {
		if tier.ID == id {
			return tier
		}
	}
	if len(p.Tiers) > 0 {
		return p.Tiers[0]
	}
	return Tier{}
}

func (p ProviderProfile) hasTier(id string) bool {
	return slices.ContainsFunc(p.Tiers, func(tier Tier) bool { return tier.ID == id })
}

// normalizedProvider validates the operator's version of a provider profile.
// Only the tiers' amounts and k, the parameter overrides and the weights are
// the operator's; the rest is built in.
func normalizedProvider(provider string, input ProviderProfile, global Params) (ProviderProfile, error) {
	profile, _ := DefaultProviderProfile(provider)
	for _, tier := range input.Tiers {
		index := slices.IndexFunc(profile.Tiers, func(known Tier) bool { return known.ID == tier.ID })
		if index < 0 {
			return ProviderProfile{}, messages.Errorf("Unknown tier %q for provider %s", tier.ID, provider)
		}
		if !finite(tier.WeeklyUSD) || tier.WeeklyUSD <= 0 || tier.WeeklyUSD > 1e9 {
			return ProviderProfile{}, messages.Errorf("The weekly total of tier %s must be a positive amount", profile.Tiers[index].Name)
		}
		if tier.K != nil && (!finite(*tier.K) || *tier.K < 1 || *tier.K > 100) {
			return ProviderProfile{}, messages.Errorf("The default k of tier %s must be from 1 to 100", profile.Tiers[index].Name)
		}
		profile.Tiers[index].WeeklyUSD = tier.WeeklyUSD
		profile.Tiers[index].K = nil
		if tier.K != nil {
			profile.Tiers[index].K = floatPointer(*tier.K)
		}
	}
	profile.Params = input.Params
	if err := profile.Params.apply(global).validate(); err != nil {
		return ProviderProfile{}, fmt.Errorf("%s: %w", provider, err)
	}
	weights, err := input.Weights.normalized()
	if err != nil {
		return ProviderProfile{}, err
	}
	profile.Weights = weights
	return profile, nil
}

// legacyTiers applies the tier amounts and k of settings saved before
// providers existed to Claude's profile.
func legacyTiers(profile ProviderProfile, weekly, k map[string]float64) ProviderProfile {
	profile.Tiers = slices.Clone(profile.Tiers)
	for i, tier := range profile.Tiers {
		if value, ok := weekly[tier.ID]; ok {
			profile.Tiers[i].WeeklyUSD = value
		}
		if value, ok := k[tier.ID]; ok {
			profile.Tiers[i].K = floatPointer(value)
		}
	}
	return profile
}

// tierTables are a profile's tier amounts and k as the maps Params carries
// for the accounts of one provider.
func (p ProviderProfile) tierTables() (map[string]float64, map[string]float64) {
	weekly, k := map[string]float64{}, map[string]float64{}
	for _, tier := range p.Tiers {
		weekly[tier.ID] = tier.WeeklyUSD
		if tier.K != nil {
			k[tier.ID] = *tier.K
		}
	}
	return weekly, k
}

// provider returns a provider's profile, the built-in one when missing.
func (s Settings) provider(name string) ProviderProfile {
	if profile, ok := s.Providers[name]; ok {
		return profile
	}
	profile, _ := DefaultProviderProfile(name)
	return profile
}

// ParamsFor is the parameters of one provider's accounts: the global ones
// with the provider's overrides, and its tiers.
func (s Settings) ParamsFor(provider string) Params {
	profile := s.provider(provider)
	params := profile.Params.apply(s.Params)
	params.TierWeeklyUSD, params.TierK = profile.tierTables()
	params.defaultTier = profile.tier("").ID
	params.rules = profile.Rules
	return params
}

// AccountProvider is the provider of a configured account: its pool's, else
// the one its settings name, else Claude, the only provider of settings saved
// before providers existed.
func (s Settings) AccountProvider(authIndex string) string {
	for _, pool := range s.Pools {
		if slices.Contains(pool.AuthIndexes, authIndex) {
			return pool.Provider
		}
	}
	if provider := s.Accounts[authIndex].Provider; provider != "" {
		return provider
	}
	return ProviderClaude
}

// accountParams is the parameters of a configured account.
func (s Settings) accountParams(authIndex string) Params {
	return s.ParamsFor(s.AccountProvider(authIndex))
}

// QuotaSource names the quota reports of an account's main windows.
type QuotaSource struct {
	Provider string
	LimitKey string
}

func (s Settings) accountSource(authIndex string) QuotaSource {
	provider := s.AccountProvider(authIndex)
	return QuotaSource{Provider: provider, LimitKey: s.provider(provider).LimitKey}
}

// AccountSources is the quota source of every configured account.
func (s Settings) AccountSources() map[string]QuotaSource {
	result := map[string]QuotaSource{}
	for _, index := range s.authIndexes() {
		result[index] = s.accountSource(index)
	}
	return result
}

// providerAccounts lists the configured accounts of one provider.
func (s Settings) providerAccounts(provider string) []string {
	var result []string
	for _, index := range s.authIndexes() {
		if s.AccountProvider(index) == provider {
			result = append(result, index)
		}
	}
	return result
}

func cloneProviders(providers map[string]ProviderProfile) map[string]ProviderProfile {
	result := maps.Clone(providers)
	for name, profile := range result {
		profile.Tiers = slices.Clone(profile.Tiers)
		result[name] = profile
	}
	return result
}

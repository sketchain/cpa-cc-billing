// Package ccbilling splits subscription accounts (Claude, and ChatGPT through
// CLIProxyAPI's Codex channel) between downstream API keys.
//
// It estimates each account's quota in USD from what the plugin already
// records passively: the cost of every request and the 5-hour and 7-day
// utilization its response reports. Fixed keys reserve their weekly
// subscription plan amount; remainder keys may only use what is left.
package ccbilling

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"cpa-super-billing/internal/messages"
)

// Key kinds.
const (
	KeyFixed     = "fixed"
	KeyRemainder = "remainder"
)

// Account tiers and the reference modes of contamination filtering.
const (
	TierPro    = "pro"
	TierMax5x  = "max5x"
	TierMax20x = "max20x"

	ReferenceGlobal = "global"
	ReferenceBlock  = "block"
)

// Params are the tunable estimation and admission parameters. Percentages are
// expressed as fractions where the name says Share or Threshold.
type Params struct {
	// SegmentRetentionDays keeps 1% segments independently of request events.
	SegmentRetentionDays int `json:"segment_retention_days"`
	// BlockGapMinutes breaks a block when two adjacent data points are further apart.
	BlockGapMinutes float64 `json:"block_gap_minutes"`
	// WindowPercent is the width, in utilization points, of the sliding window.
	WindowPercent float64 `json:"window_percent"`
	// ReferenceMode is "global" (historical weighted average, falling back to the
	// block's own iterative average while there is no history) or "block".
	ReferenceMode string `json:"reference_mode"`
	// SuspiciousThreshold marks a window whose ratio exceeds the reference by this share.
	SuspiciousThreshold float64 `json:"suspicious_threshold"`
	// ExcludeVoteShare excludes a segment when at least this share of the
	// windows covering it are suspicious.
	ExcludeVoteShare float64 `json:"exclude_vote_share"`
	// WeightPower weights a block's clean ratio by (clean gain)^WeightPower.
	WeightPower float64 `json:"weight_power"`
	// RecentDays and RecentShare split the global average by block age.
	RecentDays  float64 `json:"recent_days"`
	RecentShare float64 `json:"recent_share"`
	// KMinWeeklyPoints is the 7-day utilization gain needed before k is estimated.
	KMinWeeklyPoints float64 `json:"k_min_weekly_points"`
	// ColdStartBlocks is the number of clean blocks needed to trust the estimate.
	ColdStartBlocks int `json:"cold_start_blocks"`
	// TierWeeklyUSD and TierK are the default weekly totals, in USD, used
	// while an account is in cold start, and the default weekly/5-hour
	// ratios, used while an account has neither a manual nor an estimated k,
	// of one provider's tiers. Settings keep them in the provider profiles;
	// ParamsFor fills them in. In stored settings they only appear in those
	// saved before providers existed, which held Claude's.
	TierWeeklyUSD map[string]float64 `json:"tier_weekly_usd,omitempty"`
	TierK         map[string]float64 `json:"tier_k,omitempty"`
	// RefuseUnloaded refuses remainder keys while the settings are loaded
	// but no estimate snapshot exists yet.
	RefuseUnloaded bool `json:"refuse_remainder_when_unloaded"`
	// SafetyFactor discounts an account's estimated remaining capacity.
	SafetyFactor float64 `json:"safety_factor"`

	// defaultTier is the tier of accounts without one; rules decide what
	// feeds the segments. ParamsFor sets both for a provider.
	defaultTier string
	rules       SegmentRules
}

// Pool groups the accounts of one provider that fixed and remainder keys
// share. The anchor account's resets set the cycles and 5-hour window of the
// pool's keys; normalized settings list it first.
type Pool struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Provider is the provider of every account in the pool; pools saved
	// before providers existed are Claude's.
	Provider    string   `json:"provider"`
	AuthIndexes []string `json:"auth_indexes"`
	// Anchor is the anchor account. Settings saved before it existed, or
	// naming an account outside the pool, use the first account.
	Anchor string `json:"anchor_auth_index"`
}

// AccountSettings are the operator's per-account choices.
type AccountSettings struct {
	// Provider is the account's provider. Accounts in a pool take their
	// pool's; it keeps the provider of an account outside every pool.
	Provider string `json:"provider,omitempty"`
	Tier     string `json:"tier,omitempty"`
	// ManualK overrides the estimated weekly/5-hour ratio when set.
	ManualK *float64 `json:"manual_k,omitempty"`
}

// KeySettings assigns a downstream key, by caller scope, to a pool.
type KeySettings struct {
	Kind   string `json:"kind"`
	PoolID string `json:"pool_id"`
	// FiveHourShare overrides a fixed key's 5-hour reservation share of its
	// weekly amount (default 1/k).
	FiveHourShare *float64 `json:"five_hour_share,omitempty"`
	// WeeklyCapUSD optionally caps a remainder key's own weekly usage.
	WeeklyCapUSD *float64 `json:"weekly_cap_usd,omitempty"`
}

// Settings is everything the operator configures on the subscription split
// page.
type Settings struct {
	Params Params `json:"params"`
	// Providers holds each supported provider's profile.
	Providers map[string]ProviderProfile `json:"providers"`
	Pools     []Pool                     `json:"pools"`
	Accounts  map[string]AccountSettings `json:"accounts"`
	Keys      map[string]KeySettings     `json:"keys"`
	// Weights only appears in settings saved before providers existed; it
	// held Claude's model quota weights and moves to Claude's profile.
	Weights *WeightSettings `json:"weights,omitempty"`
}

// DefaultParams returns the documented defaults.
func DefaultParams() Params {
	return Params{
		SegmentRetentionDays: 365,
		BlockGapMinutes:      5,
		WindowPercent:        8,
		ReferenceMode:        ReferenceGlobal,
		SuspiciousThreshold:  0.20,
		ExcludeVoteShare:     0.50,
		WeightPower:          2,
		RecentDays:           14,
		RecentShare:          0.90,
		KMinWeeklyPoints:     10,
		ColdStartBlocks:      20,
		TierWeeklyUSD:        map[string]float64{TierPro: 250, TierMax5x: 1250, TierMax20x: 2500},
		TierK:                map[string]float64{TierPro: 8, TierMax5x: 8, TierMax20x: 8},
		RefuseUnloaded:       true,
		SafetyFactor:         0.9,
		defaultTier:          TierPro,
	}
}

// DefaultGlobalParams are the defaults of the parameters every provider
// shares; the tier tables belong to the provider profiles.
func DefaultGlobalParams() Params {
	params := DefaultParams()
	params.TierWeeklyUSD, params.TierK, params.defaultTier = nil, nil, ""
	return params
}

func DefaultSettings() Settings {
	return Settings{Params: DefaultGlobalParams(), Providers: DefaultProviders(), Pools: []Pool{}, Accounts: map[string]AccountSettings{},
		Keys: map[string]KeySettings{}}
}

func (p Params) gap() time.Duration { return time.Duration(p.BlockGapMinutes * float64(time.Minute)) }

func (p Params) recent() time.Duration { return time.Duration(p.RecentDays * 24 * float64(time.Hour)) }

func (p Params) retention() time.Duration {
	return time.Duration(p.SegmentRetentionDays) * 24 * time.Hour
}

// fallbackTier is the tier an account without a known tier is treated as: the
// provider's first, which for Claude is the smallest, so an unconfigured
// account errs on the conservative side.
func (p Params) fallbackTier() string {
	if p.defaultTier != "" {
		return p.defaultTier
	}
	return TierPro
}

// resolveTier is tier when the provider has it, else the fallback tier.
func (p Params) resolveTier(tier string) string {
	if _, ok := p.TierWeeklyUSD[tier]; ok {
		return tier
	}
	return p.fallbackTier()
}

// TierWeekly is the default weekly total of a tier; an unknown tier uses the
// fallback tier's.
func (p Params) TierWeekly(tier string) float64 {
	return p.TierWeeklyUSD[p.resolveTier(tier)]
}

// TierKFor is the default k of a tier, nil when the tier has none; an unknown
// tier uses the fallback tier's.
func (p Params) TierKFor(tier string) *float64 {
	if value, ok := p.TierK[p.resolveTier(tier)]; ok {
		return &value
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (p Params) validate() error {
	switch {
	case p.SegmentRetentionDays < 1 || p.SegmentRetentionDays > 3650:
		return fmt.Errorf("segment_retention_days must be an integer from 1 to 3650")
	case !finite(p.BlockGapMinutes) || p.BlockGapMinutes <= 0 || p.BlockGapMinutes > 24*60:
		return fmt.Errorf("block_gap_minutes must be greater than 0 and at most 1440")
	case !finite(p.WindowPercent) || p.WindowPercent < 1 || p.WindowPercent > 100:
		return fmt.Errorf("window_percent must be from 1 to 100")
	case p.ReferenceMode != ReferenceGlobal && p.ReferenceMode != ReferenceBlock:
		return fmt.Errorf("reference_mode must be global or block")
	case !finite(p.SuspiciousThreshold) || p.SuspiciousThreshold <= 0 || p.SuspiciousThreshold > 10:
		return fmt.Errorf("suspicious_threshold must be greater than 0 and at most 10")
	case !finite(p.ExcludeVoteShare) || p.ExcludeVoteShare <= 0 || p.ExcludeVoteShare > 1:
		return fmt.Errorf("exclude_vote_share must be greater than 0 and at most 1")
	case !finite(p.WeightPower) || p.WeightPower < 0 || p.WeightPower > 10:
		return fmt.Errorf("weight_power must be from 0 to 10")
	case !finite(p.RecentDays) || p.RecentDays <= 0 || p.RecentDays > 3650:
		return fmt.Errorf("recent_days must be greater than 0 and at most 3650")
	case !finite(p.RecentShare) || p.RecentShare < 0 || p.RecentShare > 1:
		return fmt.Errorf("recent_share must be from 0 to 1")
	case !finite(p.KMinWeeklyPoints) || p.KMinWeeklyPoints < 1 || p.KMinWeeklyPoints > 100:
		return fmt.Errorf("k_min_weekly_points must be from 1 to 100")
	case p.ColdStartBlocks < 0 || p.ColdStartBlocks > 100000:
		return fmt.Errorf("cold_start_blocks must be an integer from 0 to 100000")
	case !finite(p.SafetyFactor) || p.SafetyFactor <= 0 || p.SafetyFactor > 1:
		return fmt.Errorf("safety_factor must be greater than 0 and at most 1")
	}
	return nil
}

// Normalized validates the settings and returns a canonical copy.
//
// Settings saved before providers existed are Claude's: their tier tables and
// weights move to Claude's profile, and their pools are Claude pools.
// Normalizing normalized settings changes nothing.
func (s Settings) Normalized() (Settings, error) {
	global := s.Params
	legacyWeekly, legacyK := global.TierWeeklyUSD, global.TierK
	global.TierWeeklyUSD, global.TierK, global.defaultTier, global.rules = nil, nil, "", SegmentRules{}
	if err := global.validate(); err != nil {
		return Settings{}, err
	}
	for name := range s.Providers {
		if _, known := DefaultProviderProfile(name); !known {
			return Settings{}, messages.Errorf("Unknown provider %q", name)
		}
	}
	providers := make(map[string]ProviderProfile, len(Providers))
	for _, name := range Providers {
		input, ok := s.Providers[name]
		if !ok {
			input, _ = DefaultProviderProfile(name)
			if name == ProviderClaude {
				input = legacyTiers(input, legacyWeekly, legacyK)
				if s.Weights != nil {
					input.Weights = *s.Weights
				}
			}
		}
		profile, err := normalizedProvider(name, input, global)
		if err != nil {
			return Settings{}, err
		}
		providers[name] = profile
	}
	out := Settings{Params: global, Providers: providers, Pools: make([]Pool, 0, len(s.Pools)),
		Accounts: make(map[string]AccountSettings, len(s.Accounts)), Keys: make(map[string]KeySettings, len(s.Keys))}
	ids := map[string]bool{}
	owners := map[string]string{}
	for _, pool := range s.Pools {
		pool.ID, pool.Name = strings.TrimSpace(pool.ID), strings.TrimSpace(pool.Name)
		if pool.ID == "" || len(pool.ID) > 64 || ids[pool.ID] {
			return Settings{}, fmt.Errorf("Pool IDs must be unique, non-empty and at most 64 bytes")
		}
		if len(pool.Name) > 128 {
			return Settings{}, fmt.Errorf("Pool names must not exceed 128 bytes")
		}
		ids[pool.ID] = true
		pool.Provider = strings.TrimSpace(pool.Provider)
		if pool.Provider == "" {
			pool.Provider = ProviderClaude
		}
		if _, known := providers[pool.Provider]; !known {
			return Settings{}, messages.Errorf("Unknown provider %q", pool.Provider)
		}
		var indexes []string
		label := pool.Name
		if label == "" {
			label = pool.ID
		}
		for _, index := range pool.AuthIndexes {
			if index = strings.TrimSpace(index); index != "" && !slices.Contains(indexes, index) {
				// An account's capacity can only be shared out once.
				if owner, taken := owners[index]; taken {
					return Settings{}, messages.Errorf("Account %q is already in CC pool %q; an account can belong to only one pool", index, owner)
				}
				owners[index] = label
				indexes = append(indexes, index)
			}
		}
		if indexes == nil {
			indexes = []string{}
		}
		pool.Anchor = strings.TrimSpace(pool.Anchor)
		if anchor := slices.Index(indexes, pool.Anchor); anchor > 0 {
			indexes = append(append([]string{pool.Anchor}, indexes[:anchor]...), indexes[anchor+1:]...)
		} else if anchor < 0 {
			pool.Anchor = ""
			if len(indexes) > 0 {
				pool.Anchor = indexes[0]
			}
		}
		pool.AuthIndexes = indexes
		out.Pools = append(out.Pools, pool)
	}
	poolProvider := map[string]string{}
	for _, pool := range out.Pools {
		for _, index := range pool.AuthIndexes {
			poolProvider[index] = pool.Provider
		}
	}
	for index, account := range s.Accounts {
		index = strings.TrimSpace(index)
		if index == "" {
			continue
		}
		account.Provider = strings.TrimSpace(account.Provider)
		if provider, pooled := poolProvider[index]; pooled {
			account.Provider = provider
		}
		provider := account.Provider
		if provider == "" {
			provider = ProviderClaude
		} else if provider == ProviderClaude {
			// Claude is the default; saved settings leave it out.
			account.Provider = ""
		}
		profile, known := providers[provider]
		if !known {
			return Settings{}, messages.Errorf("Unknown provider %q", provider)
		}
		if account.Tier != "" && !profile.hasTier(account.Tier) {
			return Settings{}, messages.Errorf("Tier %q is not a tier of %s accounts", account.Tier, providerLabel(provider))
		}
		if account.ManualK != nil && (!finite(*account.ManualK) || *account.ManualK < 1 || *account.ManualK > 100) {
			return Settings{}, fmt.Errorf("Manual k must be from 1 to 100")
		}
		out.Accounts[index] = account
	}
	for scope, key := range s.Keys {
		scope = strings.TrimSpace(scope)
		if scope == "" || key.Kind == "" {
			continue
		}
		if key.Kind != KeyFixed && key.Kind != KeyRemainder {
			return Settings{}, fmt.Errorf("Key kind must be fixed or remainder")
		}
		if !ids[key.PoolID] {
			return Settings{}, fmt.Errorf("Key is assigned to a pool that does not exist")
		}
		if key.FiveHourShare != nil && (key.Kind != KeyFixed || !finite(*key.FiveHourShare) || *key.FiveHourShare < 0 || *key.FiveHourShare > 1) {
			return Settings{}, fmt.Errorf("The 5-hour reservation share applies to fixed keys and must be from 0 to 1")
		}
		if key.WeeklyCapUSD != nil && (key.Kind != KeyRemainder || !finite(*key.WeeklyCapUSD) || *key.WeeklyCapUSD < 0 || *key.WeeklyCapUSD > 1e9) {
			return Settings{}, fmt.Errorf("The weekly cap applies to remainder keys and must be a non-negative amount")
		}
		out.Keys[scope] = key
	}
	return out, nil
}

func (s Settings) pool(id string) (Pool, bool) {
	for _, pool := range s.Pools {
		if pool.ID == id {
			return pool, true
		}
	}
	return Pool{}, false
}

// authIndexes lists every account the settings mention, pools first.
func (s Settings) authIndexes() []string {
	var result []string
	for _, pool := range s.Pools {
		for _, index := range pool.AuthIndexes {
			if !slices.Contains(result, index) {
				result = append(result, index)
			}
		}
	}
	for _, index := range sortedKeys(s.Accounts) {
		if !slices.Contains(result, index) {
			result = append(result, index)
		}
	}
	return result
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

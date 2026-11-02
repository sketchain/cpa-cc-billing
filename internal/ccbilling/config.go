// Package ccbilling splits one Claude subscription between downstream API keys.
//
// It estimates each Claude account's quota in USD from what the plugin already
// records passively: the cost of every request and the unified 5-hour and
// 7-day utilization headers of its response. Fixed keys reserve their weekly
// subscription plan amount; remainder keys may only use what is left.
package ccbilling

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
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
	// Default weekly totals, in USD, while an account is in cold start.
	TierWeeklyUSD map[string]float64 `json:"tier_weekly_usd"`
	// SafetyFactor discounts an account's estimated remaining capacity.
	SafetyFactor float64 `json:"safety_factor"`
}

// Pool groups the Claude accounts that fixed and remainder keys share. The
// first credential anchors the cycles of the pool's keys.
type Pool struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AuthIndexes []string `json:"auth_indexes"`
}

// AccountSettings are the operator's per-account choices.
type AccountSettings struct {
	Tier string `json:"tier,omitempty"`
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

// Settings is everything the operator configures on the CC page.
type Settings struct {
	Params   Params                     `json:"params"`
	Pools    []Pool                     `json:"pools"`
	Accounts map[string]AccountSettings `json:"accounts"`
	Keys     map[string]KeySettings     `json:"keys"`
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
		SafetyFactor:         0.9,
	}
}

func DefaultSettings() Settings {
	return Settings{Params: DefaultParams(), Pools: []Pool{}, Accounts: map[string]AccountSettings{}, Keys: map[string]KeySettings{}}
}

func (p Params) gap() time.Duration { return time.Duration(p.BlockGapMinutes * float64(time.Minute)) }

func (p Params) recent() time.Duration { return time.Duration(p.RecentDays * 24 * float64(time.Hour)) }

func (p Params) retention() time.Duration {
	return time.Duration(p.SegmentRetentionDays) * 24 * time.Hour
}

// TierWeekly is the default weekly total of a tier; an unknown tier is treated
// as the smallest, so an unconfigured account errs on the conservative side.
func (p Params) TierWeekly(tier string) float64 {
	if value, ok := p.TierWeeklyUSD[tier]; ok {
		return value
	}
	return p.TierWeeklyUSD[TierPro]
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
	for _, tier := range []string{TierPro, TierMax5x, TierMax20x} {
		if value, ok := p.TierWeeklyUSD[tier]; !ok || !finite(value) || value <= 0 || value > 1e9 {
			return fmt.Errorf("tier_weekly_usd.%s must be a positive amount", tier)
		}
	}
	for tier := range p.TierWeeklyUSD {
		if tier != TierPro && tier != TierMax5x && tier != TierMax20x {
			return fmt.Errorf("Unknown tier %q", tier)
		}
	}
	return nil
}

// Normalized validates the settings and returns a canonical copy.
func (s Settings) Normalized() (Settings, error) {
	if err := s.Params.validate(); err != nil {
		return Settings{}, err
	}
	out := Settings{Params: s.Params, Pools: make([]Pool, 0, len(s.Pools)),
		Accounts: make(map[string]AccountSettings, len(s.Accounts)), Keys: make(map[string]KeySettings, len(s.Keys))}
	out.Params.TierWeeklyUSD = make(map[string]float64, len(s.Params.TierWeeklyUSD))
	for tier, value := range s.Params.TierWeeklyUSD {
		out.Params.TierWeeklyUSD[tier] = value
	}
	ids := map[string]bool{}
	for _, pool := range s.Pools {
		pool.ID, pool.Name = strings.TrimSpace(pool.ID), strings.TrimSpace(pool.Name)
		if pool.ID == "" || len(pool.ID) > 64 || ids[pool.ID] {
			return Settings{}, fmt.Errorf("Pool IDs must be unique, non-empty and at most 64 bytes")
		}
		if len(pool.Name) > 128 {
			return Settings{}, fmt.Errorf("Pool names must not exceed 128 bytes")
		}
		ids[pool.ID] = true
		var indexes []string
		for _, index := range pool.AuthIndexes {
			if index = strings.TrimSpace(index); index != "" && !slices.Contains(indexes, index) {
				indexes = append(indexes, index)
			}
		}
		if indexes == nil {
			indexes = []string{}
		}
		pool.AuthIndexes = indexes
		out.Pools = append(out.Pools, pool)
	}
	for index, account := range s.Accounts {
		index = strings.TrimSpace(index)
		if index == "" {
			continue
		}
		switch account.Tier {
		case "", TierPro, TierMax5x, TierMax20x:
		default:
			return Settings{}, fmt.Errorf("Account tier must be pro, max5x or max20x")
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

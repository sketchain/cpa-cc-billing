package ccbilling

import (
	"math"
	"time"

	"cpa-super-billing/internal/messages"
)

// Sources of an account's totals and k.
const (
	SourceEstimated = "estimated"
	SourceTier      = "tier"
	SourceManual    = "manual"
	SourceAuto      = "auto"
	SourceNone      = "none"
)

// AccountStatus is one account's estimate and current capacity.
type AccountStatus struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Tier      string `json:"tier"`
	Estimate
	KAuto      *float64  `json:"k_auto,omitempty"`
	KManual    *float64  `json:"k_manual,omitempty"`
	K          *float64  `json:"k,omitempty"`
	KSource    string    `json:"k_source"`
	KEstimate  KEstimate `json:"k_estimate"`
	ColdStart  bool      `json:"cold_start"`
	TotalsFrom string    `json:"totals_from"`
	// NeedsK reports an account whose 5-hour reservation needs a k it does
	// not have: no manual, estimated or tier default k.
	NeedsK bool `json:"needs_k"`
	// FiveHourTotal and WeeklyTotal are the effective totals in USD. A zero
	// FiveHourTotal means it is unknown, which disables 5-hour reservations.
	FiveHourTotal float64 `json:"five_hour_total_usd"`
	WeeklyTotal   float64 `json:"weekly_total_usd"`
	// Utilization in percent; a window whose reset time passed reads as 0.
	Used5h  float64   `json:"used_5h"`
	Used7d  float64   `json:"used_7d"`
	Reset5h time.Time `json:"reset_5h,omitzero"`
	Reset7d time.Time `json:"reset_7d,omitzero"`
	// ObservedAt is when the latest report was made; zero means never.
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// RemainingWeek and RemainingFiveHour are R_week and R_5h after the safety factor.
	RemainingWeek     float64 `json:"remaining_week_usd"`
	RemainingFiveHour float64 `json:"remaining_five_hour_usd"`
	// Paused reports that the account's estimate stopped updating, and why.
	Paused      bool      `json:"paused"`
	PauseReason string    `json:"pause_reason,omitempty"`
	PausedSince time.Time `json:"paused_since,omitzero"`
}

// Segment windows.
const (
	WindowFiveHour = "5h"
	WindowWeek     = "7d"
)

// ResolveAccount combines an account's estimate, k evidence and latest report.
// params are the parameters of the account's provider.
//
// Segments of the 5-hour window give its total; k turns it into the weekly
// total. Without any k the weekly total is the tier's default and 5-hour
// reservations are off. Segments of the 7-day window, for accounts whose
// reports have no 5-hour window, give the weekly total directly, without k,
// and such accounts have no 5-hour reservation.
func ResolveAccount(authIndex string, settings AccountSettings, estimate Estimate, k KEstimate, latest Observation, params Params, now time.Time) AccountStatus {
	status := AccountStatus{AuthIndex: authIndex, Tier: settings.Tier, Estimate: estimate, KManual: settings.ManualK,
		KEstimate: k, KSource: SourceNone, ObservedAt: latest.At}
	if status.Tier == "" {
		status.Tier = params.fallbackTier()
	}
	weekly := estimate.Window == WindowWeek
	status.KAuto = k.K
	switch {
	case settings.ManualK != nil:
		status.K, status.KSource = settings.ManualK, SourceManual
	case status.KAuto != nil:
		status.K, status.KSource = status.KAuto, SourceAuto
	default:
		// Without a manual or estimated k the tier's default, if it has one,
		// keeps the 5-hour reservation working.
		if tierK := params.TierKFor(status.Tier); tierK != nil {
			status.K, status.KSource = tierK, SourceTier
		}
	}
	status.ColdStart = estimate.CleanBlocks < params.ColdStartBlocks || estimate.FiveHourUSD <= 0
	switch {
	case status.ColdStart:
		status.TotalsFrom = SourceTier
		status.WeeklyTotal = params.TierWeekly(status.Tier)
		if status.K != nil && !weekly {
			status.FiveHourTotal = status.WeeklyTotal / *status.K
		}
	case weekly:
		status.TotalsFrom = SourceEstimated
		status.WeeklyTotal = estimate.FiveHourUSD
	case status.K != nil:
		status.TotalsFrom = SourceEstimated
		status.FiveHourTotal = estimate.FiveHourUSD
		status.WeeklyTotal = *status.K * estimate.FiveHourUSD
	default:
		// The 5-hour total is measured, but the weekly total needs k.
		status.TotalsFrom = SourceTier
		status.FiveHourTotal = estimate.FiveHourUSD
		status.WeeklyTotal = params.TierWeekly(status.Tier)
	}
	status.NeedsK = status.K == nil && !weekly
	if latest.Has5h && latest.Reset5h.After(now) {
		status.Used5h, status.Reset5h = latest.Used5h, latest.Reset5h
	} else if latest.Has5h && latest.Reset5h.IsZero() {
		status.Used5h = latest.Used5h
	}
	if latest.Has7d && latest.Reset7d.After(now) {
		status.Used7d, status.Reset7d = latest.Used7d, latest.Reset7d
	} else if latest.Has7d && latest.Reset7d.IsZero() {
		status.Used7d = latest.Used7d
	}
	status.RemainingWeek = params.SafetyFactor * status.WeeklyTotal * math.Max(0, 1-status.Used7d/100)
	status.RemainingFiveHour = params.SafetyFactor * status.FiveHourTotal * math.Max(0, 1-status.Used5h/100)
	return status
}

// FixedKeyUsage is what a fixed key has used of its weekly plan amount.
type FixedKeyUsage struct {
	Scope string `json:"scope"`
	// WeeklyUSD is the amount of the key's 7-day plan window; ok is false when
	// the key has no such plan window.
	WeeklyUSD     float64   `json:"weekly_usd"`
	UsedWeekUSD   float64   `json:"used_week_usd"`
	UsedFiveHour  float64   `json:"used_five_hour_usd"`
	FiveHourShare *float64  `json:"five_hour_share,omitempty"`
	CycleStart    time.Time `json:"cycle_start,omitzero"`
	CycleEnd      time.Time `json:"cycle_end,omitzero"`
	Valid         bool      `json:"valid"`
}

// FixedKeyStatus adds the key's reservations.
type FixedKeyStatus struct {
	FixedKeyUsage
	RemainingWeek    float64 `json:"remaining_week_usd"`
	ReserveFiveHour  float64 `json:"reserve_five_hour_usd"`
	EffectiveShare   float64 `json:"effective_five_hour_share"`
	ShareOverridden  bool    `json:"share_overridden"`
	BalanceAvailable float64 `json:"balance_usd"`
}

// PoolStatus is the admission state of one pool.
type PoolStatus struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	Provider          string           `json:"provider"`
	Accounts          []string         `json:"auth_indexes"`
	RemainingWeek     float64          `json:"remaining_week_usd"`
	RemainingFiveHour float64          `json:"remaining_five_hour_usd"`
	ReservedWeek      float64          `json:"reserved_week_usd"`
	ReservedFiveHour  float64          `json:"reserved_five_hour_usd"`
	K                 float64          `json:"k"`
	FiveHourEnabled   bool             `json:"five_hour_enabled"`
	Fixed             []FixedKeyStatus `json:"fixed"`
	// RemainderBalance is Σ R_week − P_week.
	RemainderBalance float64   `json:"remainder_balance_usd"`
	Reset5h          time.Time `json:"reset_5h,omitzero"`
	Reset7d          time.Time `json:"reset_7d,omitzero"`
}

// BuildPool sums the pool's accounts and its fixed keys' reservations. Each
// fixed key reserves its own plan balance; fixed keys on a shared quota plan
// are refused (see Manager.checkSharedPlans), and supporting them would mean
// reserving each shared plan's balance once here instead of once per key.
func BuildPool(pool Pool, accounts []AccountStatus, fixed []FixedKeyUsage) PoolStatus {
	status := PoolStatus{ID: pool.ID, Name: pool.Name, Provider: pool.Provider, Accounts: pool.AuthIndexes, Fixed: []FixedKeyStatus{}}
	var weekly, fiveHour float64
	status.FiveHourEnabled = len(accounts) > 0
	for i, account := range accounts {
		status.RemainingWeek += account.RemainingWeek
		status.RemainingFiveHour += account.RemainingFiveHour
		weekly += account.WeeklyTotal
		fiveHour += account.FiveHourTotal
		if account.K == nil || account.FiveHourTotal <= 0 {
			status.FiveHourEnabled = false
		}
		if i == 0 {
			status.Reset5h, status.Reset7d = account.Reset5h, account.Reset7d
		}
	}
	if status.FiveHourEnabled {
		status.K = weekly / fiveHour
	}
	for _, usage := range fixed {
		key := FixedKeyStatus{FixedKeyUsage: usage}
		if usage.Valid {
			key.RemainingWeek = math.Max(0, usage.WeeklyUSD-usage.UsedWeekUSD)
			key.BalanceAvailable = usage.WeeklyUSD - usage.UsedWeekUSD
			status.ReservedWeek += key.RemainingWeek
			if usage.FiveHourShare != nil {
				key.EffectiveShare, key.ShareOverridden = *usage.FiveHourShare, true
			} else if status.FiveHourEnabled {
				key.EffectiveShare = 1 / status.K
			}
			if status.FiveHourEnabled {
				key.ReserveFiveHour = math.Max(0, math.Min(key.EffectiveShare*usage.WeeklyUSD-usage.UsedFiveHour, key.RemainingWeek))
				status.ReservedFiveHour += key.ReserveFiveHour
			}
		}
		status.Fixed = append(status.Fixed, key)
	}
	status.RemainderBalance = status.RemainingWeek - status.ReservedWeek
	return status
}

// Refusal reasons of a remainder key.
const (
	RefusedWeekly    = "weekly_reserve"
	RefusedFiveHour  = "five_hour_reserve"
	RefusedCap       = "weekly_cap"
	RefusedNoPool    = "pool_unavailable"
	RefusedNotLoaded = "not_loaded"
)

// Decision is the admission verdict for a remainder key. Message is the
// English text clients receive; Detail carries it with translation metadata.
type Decision struct {
	Allowed bool
	Reason  string
	Message string
	Detail  messages.Message
	RetryAt time.Time
}

func refusal(reason string, retryAt time.Time, format string, args ...any) Decision {
	detail := messages.New(format, args...)
	return Decision{Reason: reason, Message: detail.Text, Detail: detail, RetryAt: retryAt}
}

// AdmitRemainder decides whether a remainder key may send a request now.
// capUSD and usedCap describe its optional own weekly cap.
func (p PoolStatus) AdmitRemainder(capUSD *float64, usedCap float64) Decision {
	if len(p.Accounts) == 0 {
		return refusal(RefusedNoPool, time.Time{}, "CC pool %q has no account.", p.label())
	}
	if p.RemainingWeek <= p.ReservedWeek {
		return refusal(RefusedWeekly, p.Reset7d,
			"API key blocked by the CC pool %q weekly reservation: the pool's estimated remaining weekly capacity $%.2f does not exceed the $%.2f reserved for fixed keys.",
			p.label(), p.RemainingWeek, p.ReservedWeek)
	}
	if p.FiveHourEnabled && p.RemainingFiveHour <= p.ReservedFiveHour {
		return refusal(RefusedFiveHour, p.Reset5h,
			"API key blocked by the CC pool %q 5-hour reservation: the pool's estimated remaining 5-hour capacity $%.2f does not exceed the $%.2f reserved for fixed keys.",
			p.label(), p.RemainingFiveHour, p.ReservedFiveHour)
	}
	if capUSD != nil && usedCap >= *capUSD {
		return refusal(RefusedCap, p.Reset7d, "API key reached its own weekly cap in CC pool %q: $%.2f of $%.2f used.", p.label(), usedCap, *capUSD)
	}
	return Decision{Allowed: true}
}

// RemainderBalance is the weekly balance a remainder key may still use.
func (p PoolStatus) RemainderKeyBalance(capUSD *float64, usedCap float64) float64 {
	balance := p.RemainderBalance
	if capUSD != nil {
		balance = math.Min(balance, *capUSD-usedCap)
	}
	return balance
}

func (p PoolStatus) label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// ReportedBalance is the amount reported to new-api, which disables a channel
// whose balance is zero or less; such balances are reported as 1.
func ReportedBalance(balance float64) float64 {
	if !finite(balance) || balance <= 0 {
		return 1
	}
	return math.Round(balance*1e6) / 1e6
}

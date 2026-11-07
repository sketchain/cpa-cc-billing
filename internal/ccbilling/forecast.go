package ccbilling

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Forecast outcomes: when a quantity running down at a steady rate reaches 0.
const (
	// OutcomeETA: it reaches 0 at At, before the window resets.
	OutcomeETA = "eta"
	// OutcomeAfterReset: at this rate it would reach 0 at At, after the
	// window resets, so it does not run out in this window.
	OutcomeAfterReset = "after_reset"
	// OutcomeIdle: nothing is being used, so it never reaches 0.
	OutcomeIdle = "idle"
	// OutcomeReached: it is already 0, or the remainder keys are refused now.
	OutcomeReached = "reached"
	// OutcomeUnknown: no rate is known.
	OutcomeUnknown = "unknown"
)

// Rate sources of a forecast.
const (
	RateReading = "reading"
	RateSpend   = "spend"
	RateNone    = "none"
)

// Default and allowed look-back windows of the consumption rate.
const (
	DefaultForecastWindow5h = time.Hour
	DefaultForecastWindow7d = 24 * time.Hour
	MinForecastWindow5h     = 5 * time.Minute
	MaxForecastWindow5h     = FiveHours
	MinForecastWindow7d     = time.Hour
	MaxForecastWindow7d     = Week
)

// Outcome is when a forecast quantity reaches 0.
type Outcome struct {
	Status string    `json:"status"`
	At     time.Time `json:"at,omitzero"`
}

// Project linearly forecasts when remaining, used up at perHour, reaches 0.
// A zero reset means the window's end is unknown.
func Project(now time.Time, remaining, perHour float64, reset time.Time) Outcome {
	switch {
	case !finite(remaining) || !finite(perHour):
		return Outcome{Status: OutcomeUnknown}
	case remaining <= 0:
		return Outcome{Status: OutcomeReached}
	case perHour <= 0:
		return Outcome{Status: OutcomeIdle}
	}
	hours := remaining / perHour
	if hours > 100*365*24 {
		return Outcome{Status: OutcomeIdle}
	}
	at := now.Add(time.Duration(hours * float64(time.Hour)))
	if !reset.IsZero() && at.After(reset) {
		return Outcome{Status: OutcomeAfterReset, At: at}
	}
	return Outcome{Status: OutcomeETA, At: at}
}

// Velocity is a consumption rate measured over [From, To]. For an account
// PerHour is utilization points per hour; for a pool it is USD of the pool's
// remaining capacity per hour, after the safety factor.
type Velocity struct {
	Available bool      `json:"available"`
	PerHour   float64   `json:"per_hour"`
	From      time.Time `json:"from,omitzero"`
	To        time.Time `json:"to,omitzero"`
	// StartUsed and EndUsed are the readings the rate was measured between.
	StartUsed float64 `json:"start_used"`
	EndUsed   float64 `json:"end_used"`
	// RecordedUSD is the keys' cost at the price table; BasisUSD the same
	// cost on the estimate's basis. USDPerPoint converts it to points.
	RecordedUSD float64 `json:"recorded_usd"`
	BasisUSD    float64 `json:"basis_usd"`
	USDPerPoint float64 `json:"usd_per_point,omitempty"`
	// Outcome is when the window reaches 100% (accounts) or the pool's
	// remaining capacity runs out (pools); Block when remainder keys are
	// refused by the pool's reservation.
	Outcome Outcome `json:"outcome"`
	Block   Outcome `json:"block,omitzero"`
}

// ReadingVelocity measures an account window's utilization rate over the
// window before now from its reports. points are the reports after
// now−window, before the last report at or before it (when found), and
// current the latest utilization and reset. Only reports of the current
// period count: a period that began within the window starts from 0%.
func ReadingVelocity(points []WindowPoint, before WindowPoint, hasBefore bool, current WindowPoint, length, window time.Duration, now time.Time) Velocity {
	if current.Reset.IsZero() || !current.Reset.After(now) {
		return Velocity{}
	}
	same := func(point WindowPoint) bool { return absDuration(point.Reset.Sub(current.Reset)) <= boundaryJitter }
	from, periodStart := now.Add(-window), current.Reset.Add(-length)
	var baseAt time.Time
	var baseUsed float64
	switch {
	case !periodStart.Before(from):
		baseAt = periodStart
	case hasBefore && same(before):
		baseAt, baseUsed = from, before.Used
	default:
		for _, point := range points {
			if same(point) {
				baseAt, baseUsed = point.At, point.Used
				break
			}
		}
	}
	if baseAt.IsZero() || now.Sub(baseAt) < time.Minute {
		return Velocity{}
	}
	return Velocity{Available: true, PerHour: math.Max(0, current.Used-baseUsed) / now.Sub(baseAt).Hours(), From: baseAt, To: now,
		StartUsed: baseUsed, EndUsed: current.Used}
}

// ForecastOptions configure a forecast.
type ForecastOptions struct {
	Window5h, Window7d time.Duration
	// Prices are the price table multipliers BasisConversion needs.
	Prices map[string]float64
}

// AccountWindowForecast forecasts one window of an account.
type AccountWindowForecast struct {
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	Reading Velocity  `json:"reading"`
	Spend   Velocity  `json:"spend"`
	// Source is the rate Outcome uses: readings when known, else spend.
	Source  string  `json:"source"`
	Outcome Outcome `json:"outcome"`
}

// AccountForecast forecasts both windows of a pooled account.
type AccountForecast struct {
	AuthIndex string                `json:"auth_index"`
	FiveHour  AccountWindowForecast `json:"five_hour"`
	Week      AccountWindowForecast `json:"week"`
}

// PoolWindowForecast forecasts one window of a pool: when its remaining
// capacity R runs out and when R − P, the headroom above the fixed keys'
// reservations, does, which refuses the remainder keys.
type PoolWindowForecast struct {
	Enabled   bool      `json:"enabled"`
	Remaining float64   `json:"remaining_usd"`
	Reserved  float64   `json:"reserved_usd"`
	Headroom  float64   `json:"headroom_usd"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Reading   Velocity  `json:"reading"`
	Spend     Velocity  `json:"spend"`
	// ReadingPartial: some accounts had no reading rate, so their spend
	// rate stands in.
	ReadingPartial bool `json:"reading_partial"`
	// FixedUSD is what fixed keys that still hold a reservation spent in
	// the window; FixedPerHour, the rate their reservation shrinks at.
	FixedUSD     float64 `json:"fixed_usd"`
	FixedPerHour float64 `json:"fixed_per_hour"`
	Source       string  `json:"source"`
	Exhaust      Outcome `json:"exhaust"`
	Block        Outcome `json:"block"`
}

// PoolForecast forecasts a pool and its accounts. Its spend is converted to
// its provider's basis and discounted by its provider's safety factor.
type PoolForecast struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Provider     string             `json:"provider"`
	SafetyFactor float64            `json:"safety_factor"`
	Conversion   BasisConversion    `json:"conversion"`
	Accounts     []AccountForecast  `json:"accounts"`
	FiveHour     PoolWindowForecast `json:"five_hour"`
	Week         PoolWindowForecast `json:"week"`
}

// Forecast is every pool's consumption forecast.
type Forecast struct {
	At       time.Time      `json:"at"`
	Window5h int64          `json:"window_5h_seconds"`
	Window7d int64          `json:"window_7d_seconds"`
	Pools    []PoolForecast `json:"pools"`
}

type accountPoints struct {
	points    []WindowPoint
	before    WindowPoint
	hasBefore bool
}

// Forecast projects each pool and account from the latest snapshot, the live
// utilization and the recent history. It reads only: no refresh, no writes.
func (m *Manager) Forecast(options ForecastOptions) (Forecast, error) {
	snap := m.snap.Load()
	estimates, ok := m.Estimates()
	if snap == nil || !ok {
		return Forecast{}, errNotLoaded
	}
	now := m.host.Now()
	w5, w7 := options.Window5h, options.Window7d
	result := Forecast{At: now, Window5h: int64(w5 / time.Second), Window7d: int64(w7 / time.Second), Pools: []PoolForecast{}}
	type windowKey struct {
		authIndex string
		length    time.Duration
	}
	history := map[windowKey]accountPoints{}
	var spend5h, spend7d []SpendRow
	err := m.host.WithDB(func(db *sql.DB) error {
		if db == nil {
			return fmt.Errorf("The billing database is not open")
		}
		var scopes []string
		for _, scope := range sortedKeys(snap.settings.Keys) {
			if _, ok := snap.settings.pool(snap.settings.Keys[scope].PoolID); ok {
				scopes = append(scopes, scope)
			}
		}
		var err error
		if spend5h, err = readSpend(db, scopes, now.Add(-w5), now.Add(time.Nanosecond), false, m.host.ToUSD); err != nil {
			return err
		}
		if spend7d, err = readSpend(db, scopes, now.Add(-w7), now.Add(time.Nanosecond), false, m.host.ToUSD); err != nil {
			return err
		}
		for _, pool := range snap.settings.Pools {
			for _, index := range pool.AuthIndexes {
				for _, window := range []struct{ length, span time.Duration }{{FiveHours, w5}, {Week, w7}} {
					var entry accountPoints
					from := now.Add(-window.span)
					source := snap.settings.accountSource(index)
					if entry.before, entry.hasBefore, err = lastWindowPoint(db, source, index, window.length, from); err != nil {
						return err
					}
					if entry.points, err = readWindowPoints(db, source, index, window.length, from, now); err != nil {
						return err
					}
					history[windowKey{index, window.length}] = entry
				}
			}
		}
		return nil
	})
	if err != nil {
		return Forecast{}, err
	}
	latest, deltas := m.live(snap)
	for _, pool := range snap.settings.Pools {
		status := poolStatus(snap, latest, deltas, pool, now)
		safety := snap.settings.ParamsFor(pool.Provider).SafetyFactor
		conversion := NewBasisConversion(estimates.For(pool.Provider), options.Prices)
		forecast := PoolForecast{ID: pool.ID, Name: pool.Name, Provider: pool.Provider, SafetyFactor: safety, Conversion: conversion,
			Accounts: []AccountForecast{}}
		inPool := map[string]bool{}
		for _, index := range pool.AuthIndexes {
			inPool[index] = true
		}
		// Spend of the pool's keys on its accounts, by account, and of
		// fixed keys that still hold a reservation.
		reserving := map[string][2]bool{}
		for _, fixed := range status.Fixed {
			reserving[fixed.Scope] = [2]bool{fixed.ReserveFiveHour > 0, fixed.RemainingWeek > 0}
		}
		sum := func(rows []SpendRow, fiveHour bool) (map[string][2]float64, [2]float64, float64) {
			byAccount := map[string][2]float64{}
			var total [2]float64
			fixed := 0.0
			for _, row := range rows {
				key, assigned := snap.settings.Keys[row.Scope]
				if !assigned || key.PoolID != pool.ID || !inPool[row.AuthIndex] {
					continue
				}
				basis := conversion.Convert(row.Model, row.USD)
				value := byAccount[row.AuthIndex]
				value[0], value[1] = value[0]+row.USD, value[1]+basis
				byAccount[row.AuthIndex] = value
				total[0], total[1] = total[0]+row.USD, total[1]+basis
				if flags := reserving[row.Scope]; key.Kind == KeyFixed && (fiveHour && flags[0] || !fiveHour && flags[1]) {
					fixed += row.USD
				}
			}
			return byAccount, total, fixed
		}
		account5h, total5h, fixed5h := sum(spend5h, true)
		account7d, total7d, fixed7d := sum(spend7d, false)
		var burn [2]float64
		var readable [2]bool
		var partial [2]bool
		for _, index := range pool.AuthIndexes {
			account := estimates.Accounts[index]
			item := AccountForecast{AuthIndex: index}
			for i, window := range []struct {
				target *AccountWindowForecast
				length time.Duration
				span   time.Duration
				used   float64
				reset  time.Time
				total  float64
				spend  [2]float64
			}{
				{&item.FiveHour, FiveHours, w5, account.Used5h, account.Reset5h, account.FiveHourUSD, account5h[index]},
				{&item.Week, Week, w7, account.Used7d, account.Reset7d, account.WeeklyUSD, account7d[index]},
			} {
				target := window.target
				target.Used, target.ResetAt = window.used, window.reset
				entry := history[windowKey{index, window.length}]
				target.Reading = ReadingVelocity(entry.points, entry.before, entry.hasBefore,
					WindowPoint{Used: window.used, Reset: window.reset}, window.length, window.span, now)
				reset := window.reset
				if reset.IsZero() || !reset.After(now) {
					// The next window opens with the next request.
					reset = now.Add(window.length)
				}
				if target.Reading.Available {
					target.Reading.Outcome = Project(now, 100-window.used, target.Reading.PerHour, reset)
				} else {
					target.Reading.Outcome = Outcome{Status: OutcomeUnknown}
				}
				target.Spend = Velocity{From: now.Add(-window.span), To: now, RecordedUSD: window.spend[0], BasisUSD: window.spend[1],
					Outcome: Outcome{Status: OutcomeUnknown}}
				if window.total > 0 {
					target.Spend.Available, target.Spend.USDPerPoint = true, window.total/100
					target.Spend.PerHour = window.spend[1] / target.Spend.USDPerPoint / window.span.Hours()
					target.Spend.Outcome = Project(now, 100-window.used, target.Spend.PerHour, reset)
				}
				target.Source, target.Outcome = RateNone, Outcome{Status: OutcomeUnknown}
				switch {
				case target.Reading.Available:
					target.Source, target.Outcome = RateReading, target.Reading.Outcome
					readable[i] = true
					burn[i] += safety * window.total * target.Reading.PerHour / 100
				case target.Spend.Available:
					target.Source, target.Outcome = RateSpend, target.Spend.Outcome
					partial[i] = true
					burn[i] += safety * window.total * target.Spend.PerHour / 100
				default:
					partial[i] = true
				}
			}
			forecast.Accounts = append(forecast.Accounts, item)
		}
		for i, window := range []struct {
			target    *PoolWindowForecast
			enabled   bool
			remaining float64
			reserved  float64
			reset     time.Time
			length    time.Duration
			span      time.Duration
			total     [2]float64
			fixed     float64
		}{
			{&forecast.FiveHour, status.FiveHourEnabled, status.RemainingFiveHour, status.ReservedFiveHour, status.Reset5h, FiveHours, w5, total5h, fixed5h},
			{&forecast.Week, len(pool.AuthIndexes) > 0, status.RemainingWeek, status.ReservedWeek, status.Reset7d, Week, w7, total7d, fixed7d},
		} {
			target := window.target
			target.Enabled, target.Remaining, target.Reserved = window.enabled, window.remaining, window.reserved
			target.Headroom, target.ResetAt = window.remaining-window.reserved, window.reset
			target.FixedUSD, target.FixedPerHour = window.fixed, window.fixed/window.span.Hours()
			reset := window.reset
			if reset.IsZero() || !reset.After(now) {
				reset = now.Add(window.length)
			}
			project := func(velocity *Velocity) {
				velocity.Outcome = Project(now, target.Remaining, velocity.PerHour, reset)
				velocity.Block = Project(now, target.Headroom, velocity.PerHour-target.FixedPerHour, reset)
				if !target.Enabled {
					velocity.Block = Outcome{Status: OutcomeUnknown}
				}
			}
			target.Reading = Velocity{From: now.Add(-window.span), To: now, Outcome: Outcome{Status: OutcomeUnknown}, Block: Outcome{Status: OutcomeUnknown}}
			if readable[i] {
				target.Reading.Available, target.Reading.PerHour, target.ReadingPartial = true, burn[i], partial[i]
				project(&target.Reading)
			}
			target.Spend = Velocity{Available: len(pool.AuthIndexes) > 0, From: now.Add(-window.span), To: now, RecordedUSD: window.total[0],
				BasisUSD: window.total[1], PerHour: safety * window.total[1] / window.span.Hours(),
				Outcome: Outcome{Status: OutcomeUnknown}, Block: Outcome{Status: OutcomeUnknown}}
			if target.Spend.Available {
				project(&target.Spend)
			}
			target.Source, target.Exhaust, target.Block = RateNone, Outcome{Status: OutcomeUnknown}, Outcome{Status: OutcomeUnknown}
			switch {
			case target.Reading.Available:
				target.Source, target.Exhaust, target.Block = RateReading, target.Reading.Outcome, target.Reading.Block
			case target.Spend.Available:
				target.Source, target.Exhaust, target.Block = RateSpend, target.Spend.Outcome, target.Spend.Block
			}
		}
		result.Pools = append(result.Pools, forecast)
	}
	return result, nil
}

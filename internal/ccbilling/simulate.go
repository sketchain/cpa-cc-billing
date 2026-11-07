package ccbilling

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// The simulation replays a pool's retained history hour by hour with fixed
// keys added, changed or removed, and counts the hours its remainder keys
// would have been refused. It reads only; nothing is stored or changed.

// Simulation limits.
const (
	MaxSimulationDays    = 365
	maxSimulationNewKeys = 10
	maxSimulationSpans   = 200
)

// Approximations the simulation makes; the page explains each.
const (
	ApproxCurrentEstimates = "current_estimates"
	ApproxReadings         = "readings_external"
	ApproxMissingReadings  = "missing_readings"
	ApproxHourly           = "hourly"
	ApproxNewKeyProfile    = "new_key_profile"
	ApproxQuotaChanges     = "quota_changes"
	ApproxRemainderDemand  = "remainder_demand"
	ApproxDerivedCycles    = "derived_cycles"
	ApproxPartialCycles    = "partial_cycles"
	ApproxCurrentQuota     = "current_quota"
	ApproxWeights          = "weights"
)

// SimNewKey is a hypothetical fixed key.
type SimNewKey struct {
	WeeklyUSD float64 `json:"weekly_usd"`
	// FiveHourShare is its 5-hour reservation share; the pool's 1/k when unset.
	FiveHourShare *float64 `json:"five_hour_share,omitempty"`
	// WeeklyUsageUSD is what it uses each week; its whole amount when unset.
	WeeklyUsageUSD *float64 `json:"weekly_usage_usd,omitempty"`
}

// SimChange changes or removes an existing fixed key of the pool.
type SimChange struct {
	Remove        bool     `json:"remove,omitempty"`
	WeeklyUSD     *float64 `json:"weekly_usd,omitempty"`
	FiveHourShare *float64 `json:"five_hour_share,omitempty"`
}

// SimRequest is one simulation.
type SimRequest struct {
	PoolID  string               `json:"pool_id"`
	Add     []SimNewKey          `json:"add"`
	Changes map[string]SimChange `json:"changes"`
}

// SimOptions configure a simulation run.
type SimOptions struct {
	// RetainedFrom is where the retained request history begins.
	RetainedFrom time.Time
	Prices       map[string]float64
}

// SimOutcome counts one replay's hours within a cycle.
type SimOutcome struct {
	Hours                int     `json:"hours"`
	BlockedHours         int     `json:"blocked_hours"`
	BlockedWeekHours     int     `json:"blocked_week_hours"`
	BlockedFiveHourHours int     `json:"blocked_five_hour_hours"`
	LostUSD              float64 `json:"lost_usd"`
	OversoldWeekHours    int     `json:"oversold_week_hours"`
	OversoldFiveHours    int     `json:"oversold_five_hour_hours"`
	MaxShortfallUSD      float64 `json:"max_shortfall_usd"`
	// FirstOversold is the first hour the weekly reservations exceeded the
	// pool's remaining capacity.
	FirstOversold time.Time `json:"first_oversold,omitzero"`
}

func (o *SimOutcome) add(other SimOutcome) {
	o.Hours += other.Hours
	o.BlockedHours += other.BlockedHours
	o.BlockedWeekHours += other.BlockedWeekHours
	o.BlockedFiveHourHours += other.BlockedFiveHourHours
	o.LostUSD += other.LostUSD
	o.OversoldWeekHours += other.OversoldWeekHours
	o.OversoldFiveHours += other.OversoldFiveHours
	o.MaxShortfallUSD = math.Max(o.MaxShortfallUSD, other.MaxShortfallUSD)
	if o.FirstOversold.IsZero() || !other.FirstOversold.IsZero() && other.FirstOversold.Before(o.FirstOversold) {
		o.FirstOversold = other.FirstOversold
	}
}

// SimWeek is one anchor cycle of the replay.
type SimWeek struct {
	Cycle
	// Partial: the retained history begins within the cycle, or it is still
	// running.
	Partial  bool       `json:"partial"`
	Running  bool       `json:"running"`
	Baseline SimOutcome `json:"baseline"`
	Scenario SimOutcome `json:"scenario"`
}

// SimSpan is a run of consecutive hours in which fixed keys' reservations
// exceeded the pool's remaining capacity in the scenario.
type SimSpan struct {
	Window string    `json:"window"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// MaxShortfallUSD is the largest P − R of the run.
	MaxShortfallUSD float64 `json:"max_shortfall_usd"`
}

// SimAccount is the estimate an account is replayed with.
type SimAccount struct {
	AuthIndex   string  `json:"auth_index"`
	WeeklyUSD   float64 `json:"weekly_usd"`
	FiveHourUSD float64 `json:"five_hour_usd"`
}

// SimKey is a fixed key as the scenario replays it.
type SimKey struct {
	Scope     string   `json:"scope,omitempty"`
	New       bool     `json:"new,omitempty"`
	Removed   bool     `json:"removed,omitempty"`
	WeeklyUSD float64  `json:"weekly_usd"`
	Share     *float64 `json:"five_hour_share,omitempty"`
	UsageUSD  float64  `json:"weekly_usage_usd,omitempty"`
	// BaselineWeeklyUSD is the key's current amount.
	BaselineWeeklyUSD float64 `json:"baseline_weekly_usd,omitempty"`
}

// SimResult is a simulation's outcome.
type SimResult struct {
	PoolID         string          `json:"pool_id"`
	Provider       string          `json:"provider"`
	From           time.Time       `json:"from"`
	To             time.Time       `json:"to"`
	Conversion     BasisConversion `json:"conversion"`
	SafetyFactor   float64         `json:"safety_factor"`
	K              float64         `json:"k"`
	FiveHour       bool            `json:"five_hour_enabled"`
	Accounts       []SimAccount    `json:"accounts"`
	Keys           []SimKey        `json:"keys"`
	Remainder      int             `json:"remainder_keys"`
	Weeks          []SimWeek       `json:"weeks"`
	Baseline       SimOutcome      `json:"baseline"`
	Scenario       SimOutcome      `json:"scenario"`
	Spans          []SimSpan       `json:"oversold_spans"`
	SpansTruncated bool            `json:"oversold_spans_truncated"`
	Approximations []string        `json:"approximations"`
}

// ReplayAccount is an account's estimate and hourly reports.
type ReplayAccount struct {
	AuthIndex           string
	WeeklyUSD           float64
	FiveHourUSD         float64
	Week, FiveHourTrack []WindowPoint
}

// ReplayFixed is a fixed key of one replay. New keys have no scope.
type ReplayFixed struct {
	Scope   string
	Weekly  float64
	Share   *float64
	Removed bool
	// Capped limits the key's recorded use per cycle to Weekly, for a
	// lowered amount.
	Capped bool
	New    bool
	Usage  float64
}

// ReplayInput is everything one replay reads. Accounts[0] is the anchor.
type ReplayInput struct {
	From, Now  time.Time
	Safety     float64
	FiveHour   bool
	Cycles     []Cycle
	Accounts   []ReplayAccount
	Remainder  []string
	Spend      []SpendRow
	Conversion BasisConversion
}

type replayResult struct {
	weeks   []SimOutcome
	spans   []SimSpan
	missing bool
}

// hourSeries holds one value per hour of the replay.
type hourSeries map[string][]float64

func (s hourSeries) at(key string, hour int) float64 {
	if values := s[key]; hour >= 0 && hour < len(values) {
		return values[hour]
	}
	return 0
}

// pointCursor walks hourly reports, returning the latest made before t.
type pointCursor struct {
	points []WindowPoint
	next   int
	last   *WindowPoint
}

func (c *pointCursor) before(t time.Time) *WindowPoint {
	for c.next < len(c.points) && c.points[c.next].At.Before(t) {
		c.last = &c.points[c.next]
		c.next++
	}
	return c.last
}

// used is the utilization a report says at t: 0 once its window reset.
func usedAt(point *WindowPoint, t time.Time) (float64, bool) {
	if point == nil {
		return 0, false
	}
	if !point.Reset.IsZero() && !point.Reset.After(t) {
		return 0, true
	}
	return point.Used, true
}

// Replay runs one replay of the fixed keys over the input's hours. Each hour
// is judged at its start from the reports and use before it: the pool's
// remaining capacity R comes from each account's latest utilization report,
// moved by the use the scenario adds or removes within the anchor's cycle (or
// 5-hour window); P from the fixed keys' recorded use. A remainder key refused
// in an hour does not use its recorded cost then.
func Replay(input ReplayInput, fixed []ReplayFixed) replayResult {
	start := input.From.UTC().Truncate(time.Hour)
	hours := int(math.Ceil(input.Now.Sub(start).Hours()))
	result := replayResult{weeks: make([]SimOutcome, len(input.Cycles))}
	if hours <= 0 || len(input.Accounts) == 0 {
		return result
	}
	index := func(at time.Time) int { return int(math.Floor(float64(at.Sub(start)) / float64(time.Hour))) }
	ceilIndex := func(at time.Time) int { return int(math.Ceil(float64(at.Sub(start)) / float64(time.Hour))) }
	inPool := map[string]bool{}
	for _, account := range input.Accounts {
		inPool[account.AuthIndex] = true
	}
	// Recorded use counts against a key's plan wherever it ran; capacity
	// only moves with use on the pool's accounts.
	recorded, basis := hourSeries{}, hourSeries{}
	poolRecorded, poolBasis := make([]float64, hours), make([]float64, hours)
	for _, row := range input.Spend {
		hour := index(row.Hour)
		if hour < 0 || hour >= hours {
			continue
		}
		if recorded[row.Scope] == nil {
			recorded[row.Scope], basis[row.Scope] = make([]float64, hours), make([]float64, hours)
		}
		recorded[row.Scope][hour] += row.USD
		if inPool[row.AuthIndex] {
			converted := input.Conversion.Convert(row.Model, row.USD)
			basis[row.Scope][hour] += converted
			poolRecorded[hour] += row.USD
			poolBasis[hour] += converted
		}
	}
	var weeklyTotal, fiveHourTotal float64
	for _, account := range input.Accounts {
		weeklyTotal += account.WeeklyUSD
		fiveHourTotal += account.FiveHourUSD
	}
	k := 0.0
	if fiveHourTotal > 0 {
		k = weeklyTotal / fiveHourTotal
	}
	weekCursors := make([]pointCursor, len(input.Accounts))
	fiveCursors := make([]pointCursor, len(input.Accounts))
	for i, account := range input.Accounts {
		weekCursors[i].points, fiveCursors[i].points = account.Week, account.FiveHourTrack
	}
	// adjust is, per hour, the basis USD the scenario adds to the pool's
	// use (negative when it removes use); used5 each fixed key's recorded
	// use per hour after caps.
	adjust := make([]float64, hours)
	used5 := make([][]float64, len(fixed))
	for i := range used5 {
		used5[i] = make([]float64, hours)
	}
	spans := map[string]*SimSpan{}
	closeSpan := func(window string) {
		if span := spans[window]; span != nil {
			result.spans = append(result.spans, *span)
			delete(spans, window)
		}
	}
	for c, cycle := range input.Cycles {
		// An hour belongs to the cycle its start falls in.
		first, last := max(ceilIndex(cycle.Start), 0), min(ceilIndex(cycle.End), hours)
		if first >= last {
			continue
		}
		// New keys use their weekly amount spread over the cycle like the
		// pool's own use, prorated when only part of the cycle is replayed,
		// on the pool's basis ratio.
		var cycleRecorded, cycleBasis float64
		for hour := first; hour < last; hour++ {
			cycleRecorded += poolRecorded[hour]
			cycleBasis += poolBasis[hour]
		}
		ratio := 1.0
		if cycleRecorded > 0 {
			ratio = cycleBasis / cycleRecorded
		}
		share := math.Min(1, float64(last-first)/math.Max(1, cycle.End.Sub(cycle.Start).Hours()))
		profile := func(hour int) float64 {
			if cycleRecorded > 0 {
				return poolRecorded[hour] / cycleRecorded
			}
			return 1 / float64(last-first)
		}
		cumulative := make([]float64, len(fixed))
		var weekAdjust float64
		outcome := &result.weeks[c]
		for hour := first; hour < last; hour++ {
			t := start.Add(time.Duration(hour) * time.Hour)
			// The pool's remaining capacity from the reports before t.
			var remainingWeek, remainingFive float64
			for i, account := range input.Accounts {
				used7, ok7 := usedAt(weekCursors[i].before(t), t)
				used5h, ok5 := usedAt(fiveCursors[i].before(t), t)
				if !ok7 || !ok5 {
					result.missing = true
				}
				remainingWeek += input.Safety * account.WeeklyUSD * math.Max(0, 1-used7/100)
				remainingFive += input.Safety * account.FiveHourUSD * math.Max(0, 1-used5h/100)
			}
			remainingWeek = math.Max(0, remainingWeek-input.Safety*weekAdjust)
			// The anchor's 5-hour window; a fresh one when its last report's
			// window already reset.
			windowStart := t
			if anchor := fiveCursors[0].last; anchor != nil && anchor.Reset.After(t) {
				windowStart = anchor.Reset.Add(-FiveHours)
			}
			// The window counts the hour it opened in: its first request.
			windowFirst := max(index(windowStart), 0)
			var fiveAdjust float64
			for h := windowFirst; h < hour; h++ {
				fiveAdjust += adjust[h]
			}
			remainingFive = math.Max(0, remainingFive-input.Safety*fiveAdjust)
			var reservedWeek, reservedFive float64
			for i, key := range fixed {
				if key.Removed {
					continue
				}
				left := math.Max(0, key.Weekly-cumulative[i])
				reservedWeek += left
				share := 0.0
				switch {
				case key.Share != nil:
					share = *key.Share
				case k > 0:
					share = 1 / k
				}
				var window float64
				for h := windowFirst; h < hour; h++ {
					window += used5[i][h]
				}
				reservedFive += math.Max(0, math.Min(share*key.Weekly-window, left))
			}
			blockedWeek := remainingWeek <= reservedWeek
			blockedFive := input.FiveHour && remainingFive <= reservedFive
			outcome.Hours++
			var remainderRecorded, remainderBasis float64
			for _, scope := range input.Remainder {
				remainderRecorded += recorded.at(scope, hour)
				remainderBasis += basis.at(scope, hour)
			}
			if blockedWeek || blockedFive {
				outcome.BlockedHours++
				outcome.LostUSD += remainderRecorded
				adjust[hour] -= remainderBasis
			}
			if blockedWeek {
				outcome.BlockedWeekHours++
			}
			if blockedFive {
				outcome.BlockedFiveHourHours++
			}
			for _, window := range []struct {
				name                string
				oversold            bool
				reserved, remaining float64
				count               *int
			}{
				{"week", reservedWeek > remainingWeek, reservedWeek, remainingWeek, &outcome.OversoldWeekHours},
				{"five_hour", input.FiveHour && reservedFive > remainingFive, reservedFive, remainingFive, &outcome.OversoldFiveHours},
			} {
				if !window.oversold {
					closeSpan(window.name)
					continue
				}
				*window.count++
				shortfall := window.reserved - window.remaining
				outcome.MaxShortfallUSD = math.Max(outcome.MaxShortfallUSD, shortfall)
				if window.name == "week" && outcome.FirstOversold.IsZero() {
					outcome.FirstOversold = t
				}
				current := spans[window.name]
				if current == nil {
					current = &SimSpan{Window: window.name, From: t}
					spans[window.name] = current
				}
				current.To = t.Add(time.Hour)
				current.MaxShortfallUSD = math.Max(current.MaxShortfallUSD, shortfall)
			}
			// The hour's use.
			for i, key := range fixed {
				if key.New {
					use := math.Min(key.Usage*share*profile(hour), math.Max(0, key.Weekly-cumulative[i]))
					cumulative[i] += use
					used5[i][hour] = use
					adjust[hour] += use * ratio
					continue
				}
				use, converted := recorded.at(key.Scope, hour), basis.at(key.Scope, hour)
				switch {
				case key.Removed:
					adjust[hour] -= converted
					continue
				case key.Capped:
					allowed := math.Max(0, math.Min(use, key.Weekly-cumulative[i]))
					if use > 0 {
						adjust[hour] -= converted * (use - allowed) / use
					}
					use = allowed
				}
				cumulative[i] += use
				used5[i][hour] = use
			}
			weekAdjust += adjust[hour]
		}
		// Spans do not cross cycles.
		closeSpan("week")
		closeSpan("five_hour")
	}
	slices.SortFunc(result.spans, func(a, b SimSpan) int { return a.From.Compare(b.From) })
	return result
}

func positive(value float64) bool { return finite(value) && value > 0 }

// validateSimulation checks a request against the pool's fixed keys.
func validateSimulation(request SimRequest, settings Settings) (Pool, error) {
	pool, ok := settings.pool(strings.TrimSpace(request.PoolID))
	if !ok {
		return Pool{}, invalidf("CC pool %q does not exist", request.PoolID)
	}
	if len(pool.AuthIndexes) == 0 {
		return Pool{}, invalidf("CC pool %q has no Claude account.", pool.ID)
	}
	if len(request.Add) > maxSimulationNewKeys {
		return Pool{}, invalidf("A simulation can add at most %d fixed keys", maxSimulationNewKeys)
	}
	share := func(value *float64) bool { return value == nil || finite(*value) && *value >= 0 && *value <= 1 }
	for _, key := range request.Add {
		if !positive(key.WeeklyUSD) || key.WeeklyUSD > 1e9 || !share(key.FiveHourShare) ||
			key.WeeklyUsageUSD != nil && (!finite(*key.WeeklyUsageUSD) || *key.WeeklyUsageUSD < 0 || *key.WeeklyUsageUSD > 1e9) {
			return Pool{}, invalidf("A simulated key needs a positive weekly amount, a 5-hour share from 0 to 1 and a non-negative weekly usage")
		}
	}
	for scope, change := range request.Changes {
		key, assigned := settings.Keys[scope]
		if !assigned || key.Kind != KeyFixed || key.PoolID != pool.ID {
			return Pool{}, invalidf("Only fixed keys of the simulated pool can be changed")
		}
		if change.WeeklyUSD != nil && (!finite(*change.WeeklyUSD) || *change.WeeklyUSD < 0 || *change.WeeklyUSD > 1e9) || !share(change.FiveHourShare) {
			return Pool{}, invalidf("A simulated key needs a positive weekly amount, a 5-hour share from 0 to 1 and a non-negative weekly usage")
		}
	}
	return pool, nil
}

// Simulate replays a pool's retained history with the request's fixed keys.
// It reads the latest snapshot and the database only: it never refreshes,
// migrates, writes or changes settings.
func (m *Manager) Simulate(request SimRequest, options SimOptions) (SimResult, error) {
	snap := m.snap.Load()
	estimates, ok := m.Estimates()
	if snap == nil || !ok {
		return SimResult{}, errNotLoaded
	}
	settings := snap.settings
	pool, err := validateSimulation(request, settings)
	if err != nil {
		return SimResult{}, err
	}
	now := m.host.Now()
	from := now.Add(-MaxSimulationDays * 24 * time.Hour)
	if options.RetainedFrom.After(from) {
		from = options.RetainedFrom
	}
	conversion := NewBasisConversion(estimates.For(pool.Provider), options.Prices)
	safety := settings.ParamsFor(pool.Provider).SafetyFactor
	latest, deltas := m.live(snap)
	status := poolStatus(snap, latest, deltas, pool, now)
	result := SimResult{PoolID: pool.ID, Provider: pool.Provider, From: from, To: now, Conversion: conversion, SafetyFactor: safety,
		K: status.K, FiveHour: status.FiveHourEnabled, Accounts: []SimAccount{}, Keys: []SimKey{}, Weeks: []SimWeek{}, Spans: []SimSpan{},
		Approximations: []string{ApproxCurrentEstimates, ApproxReadings, ApproxHourly, ApproxRemainderDemand, ApproxCurrentQuota}}
	input := ReplayInput{From: from, Now: now, Safety: safety, FiveHour: status.FiveHourEnabled, Conversion: conversion}
	var scopes []string
	var baseline, scenario []ReplayFixed
	for _, fixed := range status.Fixed {
		key := settings.Keys[fixed.Scope]
		scopes = append(scopes, fixed.Scope)
		if !fixed.Valid {
			continue
		}
		base := ReplayFixed{Scope: fixed.Scope, Weekly: fixed.WeeklyUSD, Share: key.FiveHourShare}
		baseline = append(baseline, base)
		next := base
		simKey := SimKey{Scope: fixed.Scope, WeeklyUSD: base.Weekly, Share: base.Share, BaselineWeeklyUSD: base.Weekly}
		if change, changed := request.Changes[fixed.Scope]; changed {
			next.Removed, simKey.Removed = change.Remove, change.Remove
			if change.WeeklyUSD != nil {
				next.Capped = *change.WeeklyUSD < base.Weekly
				next.Weekly, simKey.WeeklyUSD = *change.WeeklyUSD, *change.WeeklyUSD
			}
			if change.FiveHourShare != nil {
				next.Share, simKey.Share = change.FiveHourShare, change.FiveHourShare
			}
		}
		scenario = append(scenario, next)
		result.Keys = append(result.Keys, simKey)
	}
	for _, scope := range sortedKeys(settings.Keys) {
		if key := settings.Keys[scope]; key.Kind == KeyRemainder && key.PoolID == pool.ID {
			input.Remainder = append(input.Remainder, scope)
			scopes = append(scopes, scope)
		}
	}
	result.Remainder = len(input.Remainder)
	for _, key := range request.Add {
		usage := key.WeeklyUSD
		if key.WeeklyUsageUSD != nil {
			usage = *key.WeeklyUsageUSD
		}
		scenario = append(scenario, ReplayFixed{New: true, Weekly: key.WeeklyUSD, Share: key.FiveHourShare, Usage: usage})
		result.Keys = append(result.Keys, SimKey{New: true, WeeklyUSD: key.WeeklyUSD, Share: key.FiveHourShare, UsageUSD: usage})
	}
	if len(request.Add) > 0 {
		result.Approximations = append(result.Approximations, ApproxNewKeyProfile)
	}
	if len(request.Changes) > 0 {
		result.Approximations = append(result.Approximations, ApproxQuotaChanges)
	}
	if conversion.Basis == BasisWeighted {
		result.Approximations = append(result.Approximations, ApproxWeights)
	}
	for _, index := range pool.AuthIndexes {
		account := estimates.Accounts[index]
		input.Accounts = append(input.Accounts, ReplayAccount{AuthIndex: index, WeeklyUSD: account.WeeklyUSD, FiveHourUSD: account.FiveHourUSD})
		result.Accounts = append(result.Accounts, SimAccount{AuthIndex: index, WeeklyUSD: account.WeeklyUSD, FiveHourUSD: account.FiveHourUSD})
	}
	var resets []ResetSeen
	for i := range input.Accounts {
		account := &input.Accounts[i]
		// One account's reports a week at a time, each in its own call.
		for start := from.UTC().Truncate(time.Hour); start.Before(now); start = start.Add(Week) {
			end := start.Add(Week)
			if end.After(now) {
				end = now
			}
			err := m.host.WithDB(func(db *sql.DB) error {
				if db == nil {
					return fmt.Errorf("The billing database is not open")
				}
				source := settings.accountSource(account.AuthIndex)
				week, err := readHourlyPoints(db, source, account.AuthIndex, Week, start, end)
				if err != nil {
					return err
				}
				five, err := readHourlyPoints(db, source, account.AuthIndex, FiveHours, start, end)
				account.Week, account.FiveHourTrack = append(account.Week, week...), append(account.FiveHourTrack, five...)
				return err
			})
			if err != nil {
				return SimResult{}, err
			}
		}
	}
	err = m.host.WithDB(func(db *sql.DB) error {
		if db == nil {
			return fmt.Errorf("The billing database is not open")
		}
		var err error
		resets, err = readWeeklyResets(db, settings.accountSource(pool.AuthIndexes[0]), pool.AuthIndexes[0], from.Add(-Week))
		return err
	})
	if err != nil {
		return SimResult{}, err
	}
	input.Cycles = WeeklyCycles(resets, estimates.Accounts[pool.AuthIndexes[0]].Reset7d, from, now)
	if input.Spend, err = m.readSpendChunked(scopes, from, now.Add(time.Nanosecond)); err != nil {
		return SimResult{}, err
	}
	base := Replay(input, baseline)
	next := Replay(input, scenario)
	derived, partial := false, false
	for i, cycle := range input.Cycles {
		week := SimWeek{Cycle: cycle, Running: cycle.End.After(now), Baseline: base.weeks[i], Scenario: next.weeks[i]}
		week.Partial = week.Running || cycle.Start.Before(from)
		derived = derived || !cycle.Observed
		partial = partial || cycle.Start.Before(from)
		result.Weeks = append(result.Weeks, week)
		result.Baseline.add(week.Baseline)
		result.Scenario.add(week.Scenario)
	}
	if derived {
		result.Approximations = append(result.Approximations, ApproxDerivedCycles)
	}
	if partial {
		result.Approximations = append(result.Approximations, ApproxPartialCycles)
	}
	if base.missing || next.missing {
		result.Approximations = append(result.Approximations, ApproxMissingReadings)
	}
	result.Spans = next.spans
	if len(result.Spans) > maxSimulationSpans {
		result.Spans, result.SpansTruncated = result.Spans[:maxSimulationSpans], true
	}
	if result.Spans == nil {
		result.Spans = []SimSpan{}
	}
	return result, nil
}

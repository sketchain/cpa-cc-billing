package ccbilling

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"time"
)

// SpendHour is a key's recorded cost within one UTC hour.
type SpendHour struct {
	At  time.Time
	USD float64
}

// CycleUsage is what a fixed key used of its weekly amount in one cycle. A
// cycle settles when it ends: what was left is forfeited, not carried over.
type CycleUsage struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	WeeklyUSD float64   `json:"weekly_usd"`
	UsedUSD   float64   `json:"used_usd"`
	// ForfeitedUSD is weekly − used once the cycle has ended; RemainingUSD
	// is the same while it still runs.
	ForfeitedUSD float64 `json:"forfeited_usd"`
	RemainingUSD float64 `json:"remaining_usd"`
	Settled      bool    `json:"settled"`
	// Partial: the retained history begins within the cycle, so its use is
	// incomplete.
	Partial bool `json:"partial"`
}

// SettleCycles sums a fixed key's hourly use into its cycles and settles the
// ended ones. An hour counts in the cycle its start falls in.
func SettleCycles(cycles []Cycle, weekly float64, hours []SpendHour, from, now time.Time) []CycleUsage {
	result := make([]CycleUsage, 0, len(cycles))
	for _, cycle := range cycles {
		usage := CycleUsage{Start: cycle.Start, End: cycle.End, WeeklyUSD: weekly, Settled: !cycle.End.After(now), Partial: cycle.Start.Before(from)}
		for _, hour := range hours {
			if !hour.At.Before(cycle.Start) && hour.At.Before(cycle.End) {
				usage.UsedUSD += hour.USD
			}
		}
		left := math.Max(0, weekly-usage.UsedUSD)
		if usage.Settled {
			usage.ForfeitedUSD = left
		} else {
			usage.RemainingUSD = left
		}
		result = append(result, usage)
	}
	return result
}

// KeyBlockHour is how often a key was refused within one hour for one reason.
type KeyBlockHour struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Count  int64     `json:"count"`
}

// KeySeries is one pooled key's hourly use, its refusals and, for fixed keys,
// its cycles.
type KeySeries struct {
	Scope     string  `json:"scope"`
	Preview   string  `json:"preview,omitempty"`
	Label     string  `json:"label,omitempty"`
	Kind      string  `json:"kind"`
	WeeklyUSD float64 `json:"weekly_usd,omitempty"`
	// Hours are [Unix seconds of the UTC hour, recorded USD] pairs.
	Hours  [][2]float64   `json:"hours"`
	Blocks []KeyBlockHour `json:"blocks"`
	Cycles []CycleUsage   `json:"cycles"`
}

// PoolKeyUsage is a pool's keys.
type PoolKeyUsage struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Provider string      `json:"provider"`
	Keys     []KeySeries `json:"keys"`
}

// KeyUsage is every pool's key use over the retained history.
type KeyUsage struct {
	At    time.Time      `json:"at"`
	From  time.Time      `json:"from"`
	Pools []PoolKeyUsage `json:"pools"`
}

// KeyUsage reads each pooled key's hourly cost, as billed, and refusals over
// the retained history. It saves pending refusal counts first; otherwise it
// reads only.
func (m *Manager) KeyUsage(retainedFrom time.Time) (KeyUsage, error) {
	if m.snap.Load() == nil {
		return KeyUsage{}, errNotLoaded
	}
	if err := m.FlushBlocks(); err != nil {
		return KeyUsage{}, err
	}
	snap := m.snap.Load()
	settings := snap.settings
	now := m.host.Now()
	from := now.Add(-MaxSimulationDays * 24 * time.Hour)
	if retainedFrom.After(from) {
		from = retainedFrom
	}
	result := KeyUsage{At: now, From: from, Pools: []PoolKeyUsage{}}
	var scopes []string
	for _, scope := range sortedKeys(settings.Keys) {
		if _, ok := settings.pool(settings.Keys[scope].PoolID); ok {
			scopes = append(scopes, scope)
		}
	}
	rows, err := m.readSpendChunked(scopes, from, now.Add(time.Nanosecond))
	if err != nil {
		return KeyUsage{}, err
	}
	var blocks []BlockCount
	resets := map[string][]ResetSeen{}
	err = m.host.WithDB(func(db *sql.DB) error {
		if db == nil {
			return fmt.Errorf("The billing database is not open")
		}
		var err error
		if blocks, err = readBlockCounts(db, scopes, from); err != nil {
			return err
		}
		for _, pool := range settings.Pools {
			if len(pool.AuthIndexes) > 0 {
				if resets[pool.ID], err = readWeeklyResets(db, settings.accountSource(pool.AuthIndexes[0]), pool.AuthIndexes[0], from.Add(-Week)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return KeyUsage{}, err
	}
	hourly := map[string]map[time.Time]float64{}
	for _, row := range rows {
		if hourly[row.Scope] == nil {
			hourly[row.Scope] = map[time.Time]float64{}
		}
		hourly[row.Scope][row.Hour] += row.USD
	}
	refusals := map[string][]KeyBlockHour{}
	for _, block := range blocks {
		refusals[block.Scope] = append(refusals[block.Scope], KeyBlockHour{At: block.At, Reason: block.Reason, Count: block.Count})
	}
	latest, _ := m.live(snap)
	for _, pool := range settings.Pools {
		usage := PoolKeyUsage{ID: pool.ID, Name: pool.Name, Provider: pool.Provider, Keys: []KeySeries{}}
		var cycles []Cycle
		if len(pool.AuthIndexes) > 0 {
			current := accountStatus(snap, latest, pool.AuthIndexes[0], now).Reset7d
			cycles = WeeklyCycles(resets[pool.ID], current, from, now)
		}
		for _, scope := range scopes {
			key := settings.Keys[scope]
			if key.PoolID != pool.ID {
				continue
			}
			info := snap.keys[scope].info
			series := KeySeries{Scope: scope, Preview: info.Preview, Label: info.Label, Kind: key.Kind, Hours: [][2]float64{},
				Blocks: []KeyBlockHour{}, Cycles: []CycleUsage{}}
			var spend []SpendHour
			for _, hour := range sortedTimes(hourly[scope]) {
				value := hourly[scope][hour]
				series.Hours = append(series.Hours, [2]float64{float64(hour.Unix()), value})
				spend = append(spend, SpendHour{At: hour, USD: value})
			}
			if blocks := refusals[scope]; blocks != nil {
				series.Blocks = blocks
			}
			if key.Kind == KeyFixed && snap.keys[scope].known && info.HasWeekly {
				series.WeeklyUSD = info.WeeklyUSD
				series.Cycles = SettleCycles(planCycles(cycles, info, now), info.WeeklyUSD, spend, from, now)
			}
			usage.Keys = append(usage.Keys, series)
		}
		result.Pools = append(result.Pools, usage)
	}
	return result, nil
}

// planCycles uses the key's own current plan cycle, which the alignment
// keeps on the anchor's reset, in place of the anchor's current period.
func planCycles(cycles []Cycle, info KeyInfo, now time.Time) []Cycle {
	if info.CycleStart.IsZero() || !info.CycleEnd.After(now) || info.CycleStart.After(now) {
		return cycles
	}
	var result []Cycle
	for _, cycle := range cycles {
		if !cycle.Start.Before(info.CycleStart) {
			break
		}
		if cycle.End.After(info.CycleStart) {
			cycle.End = info.CycleStart
		}
		result = append(result, cycle)
	}
	return append(result, Cycle{Start: info.CycleStart, End: info.CycleEnd, Observed: true})
}

func sortedTimes[V any](values map[time.Time]V) []time.Time {
	keys := make([]time.Time, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b time.Time) int { return a.Compare(b) })
	return keys
}

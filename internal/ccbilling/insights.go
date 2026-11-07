package ccbilling

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// This file holds what the forecast, the simulation and the key usage charts
// share: the conversion of recorded key cost to the estimate's basis and the
// reads of key spend and utilization history. Every read is read only.

// priceMatchTolerance is how close, relatively, a price table multiplier must
// be to a model's quota multiplier to count as already carrying it.
const priceMatchTolerance = 0.01

// BasisConversion converts recorded key cost, billed at the price table, to
// the basis the estimate's capacity is in. On the raw basis it changes
// nothing. On the weighted basis a tested model's cost counts its effective
// multiplier times, unless the price table already bills the model at that
// multiplier: its recorded cost then already is baseline USD.
type BasisConversion struct {
	Basis string `json:"basis"`
	// Factors are the multipliers applied, by normalized model.
	Factors map[string]float64 `json:"factors"`
	// Priced lists the models whose price table multiplier equals their
	// quota multiplier, so their cost is not converted again.
	Priced []string `json:"priced_models"`
}

// NewBasisConversion builds the conversion of one provider's estimates'
// basis. prices holds, by model, the price table multiplier of models with a
// custom price whose input and output multipliers agree.
func NewBasisConversion(estimates ProviderWeights, prices map[string]float64) BasisConversion {
	conversion := BasisConversion{Basis: estimates.Basis, Factors: map[string]float64{}, Priced: []string{}}
	if conversion.Basis == "" {
		conversion.Basis = BasisRaw
	}
	if conversion.Basis != BasisWeighted {
		return conversion
	}
	normalized := map[string]float64{}
	for model, price := range prices {
		normalized[NormalizeModel(model)] = price
	}
	for _, weight := range estimates.Weights {
		if !finite(weight.Multiplier) || weight.Multiplier <= 0 || weight.Multiplier == 1 {
			continue
		}
		for _, model := range weight.Models {
			model = NormalizeModel(model)
			if price, ok := normalized[model]; ok && math.Abs(price-weight.Multiplier) <= priceMatchTolerance*weight.Multiplier {
				conversion.Priced = append(conversion.Priced, model)
				continue
			}
			conversion.Factors[model] = weight.Multiplier
		}
	}
	slices.Sort(conversion.Priced)
	return conversion
}

// Convert returns a model's recorded cost on the basis.
func (c BasisConversion) Convert(model string, usd float64) float64 {
	if factor, ok := c.Factors[NormalizeModel(model)]; ok {
		return usd * factor
	}
	return usd
}

// SpendRow is the recorded cost of one key's requests on one account and
// model, within one hour or another range.
type SpendRow struct {
	Scope     string
	AuthIndex string
	Hour      time.Time
	Model     string
	USD       float64
}

func placeholders(count int) string { return strings.TrimSuffix(strings.Repeat("?,", count), ",") }

// readSpend sums the cost of the keys' requests made in [from, to), by key,
// account, model and, when hourly, UTC hour. Requests count when they start.
func readSpend(db *sql.DB, scopes []string, from, to time.Time, hourly bool, toUSD func(float64, string) float64) ([]SpendRow, error) {
	if len(scopes) == 0 || !from.Before(to) {
		return nil, nil
	}
	hour := "0"
	if hourly {
		hour = fmt.Sprintf("at / %d", int64(time.Hour))
	}
	args := []any{nanos(from), nanos(to)}
	for _, scope := range scopes {
		args = append(args, scope)
	}
	rows, err := db.Query(`SELECT scope, auth_index, `+hour+` AS hour,
		CASE WHEN billing_model <> '' THEN billing_model ELSE upstream_model END AS model, currency, sum(total_cost)
		FROM request_events WHERE at >= ? AND at < ? AND scope IN (`+placeholders(len(scopes))+`)
		GROUP BY scope, auth_index, hour, model, currency`, args...)
	if err != nil {
		return nil, fmt.Errorf("Read key usage: %w", err)
	}
	defer rows.Close()
	var result []SpendRow
	for rows.Next() {
		var row SpendRow
		var bucket int64
		var currency string
		var cost float64
		if err := rows.Scan(&row.Scope, &row.AuthIndex, &bucket, &row.Model, &currency, &cost); err != nil {
			return nil, fmt.Errorf("Read key usage: %w", err)
		}
		if hourly {
			row.Hour = time.Unix(0, bucket*int64(time.Hour)).UTC()
		}
		row.Model = NormalizeModel(row.Model)
		row.USD = toUSD(cost, currency)
		result = append(result, row)
	}
	return result, rows.Err()
}

// readSpendChunked reads hourly spend a week at a time, each in its own
// database call, so a long history never holds the database for long.
func (m *Manager) readSpendChunked(scopes []string, from, to time.Time) ([]SpendRow, error) {
	var result []SpendRow
	for start := from; start.Before(to); start = start.Add(Week) {
		end := start.Add(Week)
		if end.After(to) {
			end = to
		}
		err := m.host.WithDB(func(db *sql.DB) error {
			if db == nil {
				return fmt.Errorf("The billing database is not open")
			}
			rows, err := readSpend(db, scopes, start, end, true, m.host.ToUSD)
			result = append(result, rows...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// WindowPoint is one utilization report of a window.
type WindowPoint struct {
	At    time.Time `json:"at"`
	Used  float64   `json:"used"`
	Reset time.Time `json:"reset_at,omitzero"`
}

// readWindowPoints reads an account's reports of one main window made in
// (from, to], oldest first.
func readWindowPoints(db *sql.DB, source QuotaSource, authIndex string, length time.Duration, from, to time.Time) ([]WindowPoint, error) {
	rows, err := db.Query(`SELECT at, used_percent, reset_at FROM quota_report_windows
		WHERE auth_index = ? AND provider = ? AND limit_key = ? AND seconds = ? AND at > ? AND at <= ? ORDER BY at, request_event_id`,
		authIndex, source.Provider, source.LimitKey, int64(length/time.Second), nanos(from), nanos(to))
	if err != nil {
		return nil, fmt.Errorf("Read quota reports: %w", err)
	}
	defer rows.Close()
	var points []WindowPoint
	for rows.Next() {
		var at, reset int64
		var used float64
		if err := rows.Scan(&at, &used, &reset); err != nil {
			return nil, fmt.Errorf("Read quota reports: %w", err)
		}
		points = append(points, WindowPoint{At: timeAt(at), Used: used, Reset: timeAt(reset)})
	}
	return points, rows.Err()
}

// lastWindowPoint is an account's latest report of a window made at or before at.
func lastWindowPoint(db *sql.DB, source QuotaSource, authIndex string, length time.Duration, at time.Time) (WindowPoint, bool, error) {
	var point WindowPoint
	var when, reset int64
	err := db.QueryRow(`SELECT at, used_percent, reset_at FROM quota_report_windows
		WHERE auth_index = ? AND provider = ? AND limit_key = ? AND seconds = ? AND at <= ? ORDER BY at DESC, request_event_id DESC LIMIT 1`,
		authIndex, source.Provider, source.LimitKey, int64(length/time.Second), nanos(at)).Scan(&when, &point.Used, &reset)
	switch {
	case err == sql.ErrNoRows:
		return WindowPoint{}, false, nil
	case err != nil:
		return WindowPoint{}, false, fmt.Errorf("Read quota reports: %w", err)
	}
	point.At, point.Reset = timeAt(when), timeAt(reset)
	return point, true, nil
}

// readHourlyPoints reads an account's last report of a window in each UTC
// hour of [from, to), oldest first. SQLite takes the bare columns from the
// row holding max(at).
func readHourlyPoints(db *sql.DB, source QuotaSource, authIndex string, length time.Duration, from, to time.Time) ([]WindowPoint, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT at / %d AS hour, max(at), used_percent, reset_at FROM quota_report_windows
		WHERE auth_index = ? AND provider = ? AND limit_key = ? AND seconds = ? AND at >= ? AND at < ? GROUP BY hour ORDER BY hour`, int64(time.Hour)),
		authIndex, source.Provider, source.LimitKey, int64(length/time.Second), nanos(from), nanos(to))
	if err != nil {
		return nil, fmt.Errorf("Read quota reports: %w", err)
	}
	defer rows.Close()
	var points []WindowPoint
	for rows.Next() {
		var hour, at, reset int64
		var used float64
		if err := rows.Scan(&hour, &at, &used, &reset); err != nil {
			return nil, fmt.Errorf("Read quota reports: %w", err)
		}
		points = append(points, WindowPoint{At: timeAt(at), Used: used, Reset: timeAt(reset)})
	}
	return points, rows.Err()
}

// ResetSeen is a scheduled 7-day reset of an account and when a report first
// named it.
type ResetSeen struct {
	Reset     time.Time
	FirstSeen time.Time
}

func readWeeklyResets(db *sql.DB, source QuotaSource, authIndex string, from time.Time) ([]ResetSeen, error) {
	rows, err := db.Query(`SELECT reset_at, min(at) FROM quota_report_windows
		WHERE auth_index = ? AND provider = ? AND limit_key = ? AND seconds = ? AND at >= ? AND reset_at > 0 GROUP BY reset_at ORDER BY reset_at`,
		authIndex, source.Provider, source.LimitKey, int64(Week/time.Second), nanos(from))
	if err != nil {
		return nil, fmt.Errorf("Read quota reports: %w", err)
	}
	defer rows.Close()
	var result []ResetSeen
	for rows.Next() {
		var reset, first int64
		if err := rows.Scan(&reset, &first); err != nil {
			return nil, fmt.Errorf("Read quota reports: %w", err)
		}
		result = append(result, ResetSeen{Reset: timeAt(reset), FirstSeen: timeAt(first)})
	}
	return result, rows.Err()
}

// Cycle is one 7-day period of a pool's anchor account.
type Cycle struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Observed is true when a report named the cycle's reset; otherwise it
	// is derived by stepping back 7 days.
	Observed bool `json:"observed"`
}

// WeeklyCycles derives the anchor account's 7-day periods that overlap
// [from, now] from the resets its reports named, oldest first. Resets within a
// minute are one. A period ends at its reset, or earlier when a report already
// named the next reset (an early reset). Before the first known reset, and
// across gaps in the reports, periods step back 7 days from the next one.
// current is the anchor's current reset, which may be zero.
func WeeklyCycles(resets []ResetSeen, current, from, now time.Time) []Cycle {
	var merged []ResetSeen
	sorted := slices.Clone(resets)
	if !current.IsZero() {
		sorted = append(sorted, ResetSeen{Reset: current, FirstSeen: now})
	}
	slices.SortFunc(sorted, func(a, b ResetSeen) int { return a.Reset.Compare(b.Reset) })
	for _, reset := range sorted {
		if n := len(merged); n > 0 && reset.Reset.Sub(merged[n-1].Reset) <= boundaryJitter {
			if reset.FirstSeen.Before(merged[n-1].FirstSeen) {
				merged[n-1].FirstSeen = reset.FirstSeen
			}
			continue
		}
		merged = append(merged, reset)
	}
	if len(merged) == 0 {
		// Nothing known: weeks ending now.
		merged = []ResetSeen{{Reset: now, FirstSeen: now}}
	}
	var observed []Cycle
	for i, reset := range merged {
		end := reset.Reset
		if i+1 < len(merged) && merged[i+1].FirstSeen.Before(end) {
			end = merged[i+1].FirstSeen
		}
		start := reset.Reset.Add(-Week)
		if i > 0 && start.Before(observed[i-1].End) {
			start = observed[i-1].End
		}
		if !end.After(start) {
			end = start
		}
		observed = append(observed, Cycle{Start: start, End: end, Observed: true})
	}
	var cycles []Cycle
	// Derived weeks before the first observed one.
	for end := observed[0].Start; end.After(from); end = end.Add(-Week) {
		cycles = append(cycles, Cycle{Start: end.Add(-Week), End: end})
	}
	slices.Reverse(cycles)
	for i, cycle := range observed {
		if i > 0 {
			// Fill a gap after the previous period with derived weeks.
			var gap []Cycle
			for end := cycle.Start; end.Sub(observed[i-1].End) > boundaryJitter; end = end.Add(-Week) {
				start := end.Add(-Week)
				if start.Before(observed[i-1].End) {
					start = observed[i-1].End
				}
				gap = append(gap, Cycle{Start: start, End: end})
			}
			slices.Reverse(gap)
			cycles = append(cycles, gap...)
		}
		if cycle.End.After(cycle.Start) {
			cycles = append(cycles, cycle)
		}
	}
	// Derived weeks after the last one when it already ended.
	for last := cycles[len(cycles)-1]; !last.End.After(now); last = cycles[len(cycles)-1] {
		cycles = append(cycles, Cycle{Start: last.End, End: last.End.Add(Week)})
	}
	result := cycles[:0]
	for _, cycle := range cycles {
		if cycle.End.After(from) && cycle.Start.Before(now) {
			result = append(result, cycle)
		}
	}
	return result
}

// errNotLoaded reports a read that needs an estimate snapshot before the
// first one exists.
var errNotLoaded = errors.New("CC estimates are still loading; retry shortly")

// IsNotLoaded reports whether err means no estimate snapshot exists yet.
func IsNotLoaded(err error) bool { return errors.Is(err, errNotLoaded) }

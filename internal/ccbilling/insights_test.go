package ccbilling

import (
	"math"
	"slices"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestProjectFindsTheLinearArrivalAndTheResetCutoff(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	// 60 points left at 6 points an hour: 10 hours.
	if got := Project(now, 60, 6, now.Add(20*time.Hour)); got.Status != OutcomeETA || !got.At.Equal(now.Add(10*time.Hour)) {
		t.Fatalf("eta = %+v", got)
	}
	// The window resets after 4 hours: it does not run out before then.
	if got := Project(now, 60, 6, now.Add(4*time.Hour)); got.Status != OutcomeAfterReset || !got.At.Equal(now.Add(10*time.Hour)) {
		t.Fatalf("after reset = %+v", got)
	}
	if got := Project(now, 60, 0, time.Time{}); got.Status != OutcomeIdle {
		t.Fatalf("idle = %+v", got)
	}
	if got := Project(now, 0, 6, time.Time{}); got.Status != OutcomeReached {
		t.Fatalf("reached = %+v", got)
	}
	if got := Project(now, -5, 6, time.Time{}); got.Status != OutcomeReached {
		t.Fatalf("negative headroom = %+v", got)
	}
	if got := Project(now, 10, math.NaN(), time.Time{}); got.Status != OutcomeUnknown {
		t.Fatalf("unknown = %+v", got)
	}
}

func TestReadingVelocityUnderLinearUse(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	// One point every 10 minutes: 6 points an hour, 40% now.
	var points []WindowPoint
	for i := 5; i >= 0; i-- {
		points = append(points, WindowPoint{At: now.Add(-time.Duration(i) * 10 * time.Minute), Used: 40 - float64(i), Reset: reset})
	}
	before := WindowPoint{At: now.Add(-61 * time.Minute), Used: 34, Reset: reset}
	velocity := ReadingVelocity(points, before, true, WindowPoint{Used: 40, Reset: reset}, FiveHours, time.Hour, now)
	if !velocity.Available || !velocity.From.Equal(now.Add(-time.Hour)) || velocity.StartUsed != 34 {
		t.Fatalf("velocity = %+v", velocity)
	}
	near(t, "points per hour", velocity.PerHour, 6)
	// 60 points left at 6 an hour is 10 hours, after the reset in 3.
	if got := Project(now, 100-40, velocity.PerHour, reset); got.Status != OutcomeAfterReset {
		t.Fatalf("outcome = %+v", got)
	}
	// A later reset: it reaches 100% in 10 hours.
	late := now.Add(48 * time.Hour)
	for i := range points {
		points[i].Reset = late
	}
	before.Reset = late
	velocity = ReadingVelocity(points, before, true, WindowPoint{Used: 40, Reset: late}, Week, time.Hour, now)
	if got := Project(now, 60, velocity.PerHour, late); got.Status != OutcomeETA || !got.At.Equal(now.Add(10*time.Hour)) {
		t.Fatalf("outcome = %+v", got)
	}

	// A period that began 30 minutes ago starts from 0%: 6 points in 30 minutes.
	fresh := now.Add(FiveHours - 30*time.Minute)
	velocity = ReadingVelocity([]WindowPoint{{At: now.Add(-20 * time.Minute), Used: 2, Reset: fresh}},
		WindowPoint{At: now.Add(-2 * time.Hour), Used: 90, Reset: now.Add(-30 * time.Minute)}, true,
		WindowPoint{Used: 6, Reset: fresh}, FiveHours, time.Hour, now)
	near(t, "fresh period rate", velocity.PerHour, 12)
	// Reports of another period are ignored; without one of this period
	// there is no rate.
	velocity = ReadingVelocity([]WindowPoint{{At: now.Add(-20 * time.Minute), Used: 80, Reset: now.Add(time.Hour)}}, WindowPoint{}, false,
		WindowPoint{Used: 6, Reset: now.Add(48 * time.Hour)}, Week, time.Hour, now)
	if velocity.Available {
		t.Fatalf("velocity across periods = %+v", velocity)
	}
	// A window whose reset passed has no rate.
	if velocity := ReadingVelocity(points, before, true, WindowPoint{Used: 40, Reset: now.Add(-time.Minute)}, Week, time.Hour, now); velocity.Available {
		t.Fatalf("expired velocity = %+v", velocity)
	}
}

func TestBasisConversionAppliesMultipliersOnceOnTheWeightedBasis(t *testing.T) {
	estimates := ProviderWeights{Basis: BasisWeighted, Weights: []EffectiveWeight{
		{ID: "a", Models: []string{"model-a"}, Multiplier: 2},
		{ID: "b", Models: []string{"model-b"}, Multiplier: 1.5},
		{ID: "c", Models: []string{"model-c"}, Multiplier: 1},
	}}
	// model-b is already billed at ×1.5 in the price table.
	conversion := NewBasisConversion(estimates, map[string]float64{"Model-B": 1.505, "model-a": 1.2})
	near(t, "model-a", conversion.Convert("MODEL-A", 10), 20)
	near(t, "model-b", conversion.Convert("model-b", 10), 10)
	near(t, "model-c", conversion.Convert("model-c", 10), 10)
	near(t, "baseline", conversion.Convert("other", 10), 10)
	if !slices.Equal(conversion.Priced, []string{"model-b"}) || len(conversion.Factors) != 1 {
		t.Fatalf("conversion = %+v", conversion)
	}
	estimates.Basis = BasisRaw
	raw := NewBasisConversion(estimates, nil)
	near(t, "raw", raw.Convert("model-a", 10), 10)
	if raw.Basis != BasisRaw || len(raw.Factors) != 0 {
		t.Fatalf("raw conversion = %+v", raw)
	}
}

func TestWeeklyCyclesFollowObservedResetsAndStepBackWithoutThem(t *testing.T) {
	day := 24 * time.Hour
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(20 * day)
	// Resets seen at day 7 and, after an early reset at day 12, day 19; the
	// current one at day 26.
	resets := []ResetSeen{
		{Reset: base.Add(7 * day), FirstSeen: base.Add(day)},
		{Reset: base.Add(7*day + 30*time.Second), FirstSeen: base.Add(2 * day)},
		{Reset: base.Add(14 * day), FirstSeen: base.Add(8 * day)},
		{Reset: base.Add(19 * day), FirstSeen: base.Add(12 * day)},
	}
	cycles := WeeklyCycles(resets, base.Add(26*day), base.Add(-10*day), now)
	want := []Cycle{
		{Start: base.Add(-14 * day), End: base.Add(-7 * day)},
		{Start: base.Add(-7 * day), End: base},
		{Start: base, End: base.Add(7 * day), Observed: true},
		{Start: base.Add(7 * day), End: base.Add(12 * day), Observed: true},
		{Start: base.Add(12 * day), End: base.Add(19 * day), Observed: true},
		{Start: base.Add(19 * day), End: base.Add(26 * day), Observed: true},
	}
	if !slices.Equal(cycles, want) {
		t.Fatalf("cycles = %+v\nwant %+v", cycles, want)
	}
	// Nothing known: the current reset, stepping back.
	cycles = WeeklyCycles(nil, now.Add(2*day), now.Add(-8*day), now)
	if len(cycles) != 2 || !cycles[1].End.Equal(now.Add(2*day)) || !cycles[0].End.Equal(now.Add(-5*day)) || cycles[0].Observed != false {
		t.Fatalf("derived cycles = %+v", cycles)
	}
	// A gap between observed resets is filled with derived weeks.
	cycles = WeeklyCycles([]ResetSeen{{Reset: base.Add(7 * day), FirstSeen: base}, {Reset: base.Add(28 * day), FirstSeen: base.Add(22 * day)}},
		time.Time{}, base, base.Add(25*day))
	if len(cycles) != 4 || cycles[1].Observed || !cycles[1].Start.Equal(base.Add(7*day)) || !cycles[2].Start.Equal(base.Add(14*day)) || !cycles[3].Observed {
		t.Fatalf("gap cycles = %+v", cycles)
	}
}

func TestSettleCyclesForfeitsWhatEndedCyclesLeft(t *testing.T) {
	day := 24 * time.Hour
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cycles := []Cycle{{Start: base, End: base.Add(7 * day)}, {Start: base.Add(7 * day), End: base.Add(14 * day)}, {Start: base.Add(14 * day), End: base.Add(21 * day)}}
	hours := []SpendHour{
		{At: base.Add(time.Hour), USD: 30}, {At: base.Add(2 * day), USD: 40},
		// The first hour of the second cycle counts there.
		{At: base.Add(7 * day), USD: 120},
		{At: base.Add(15 * day), USD: 25},
	}
	got := SettleCycles(cycles, 100, hours, base.Add(time.Hour), base.Add(16*day))
	if len(got) != 3 {
		t.Fatalf("cycles = %+v", got)
	}
	// Partial: the history begins after the first cycle started.
	if !got[0].Settled || !got[0].Partial || got[0].UsedUSD != 70 || got[0].ForfeitedUSD != 30 || got[0].RemainingUSD != 0 {
		t.Fatalf("first = %+v", got[0])
	}
	// Overuse forfeits nothing.
	if !got[1].Settled || got[1].Partial || got[1].UsedUSD != 120 || got[1].ForfeitedUSD != 0 {
		t.Fatalf("second = %+v", got[1])
	}
	// The running cycle is not settled yet.
	if got[2].Settled || got[2].UsedUSD != 25 || got[2].ForfeitedUSD != 0 || got[2].RemainingUSD != 75 {
		t.Fatalf("running = %+v", got[2])
	}
}

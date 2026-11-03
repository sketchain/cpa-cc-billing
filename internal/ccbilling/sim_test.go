package ccbilling

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// simulation models one Claude account: requests through the plugin raise the
// hidden 5-hour and 7-day utilization by cost / total; outside usage raises it
// without any cost the plugin sees. Responses report utilization rounded down
// to whole percent, evaluated when each request starts.
type simulation struct {
	rng            *rand.Rand
	fiveHour       float64 // true 5-hour total in USD
	k              float64 // true weekly / 5-hour ratio
	start          time.Time
	used5, used7   float64
	reset5, reset7 time.Time
	pending        []pendingCost // completions not yet applied
	obs            []Observation
	costs          []CostEvent
	outside        float64 // outside points added in total
}

type pendingCost struct {
	at   time.Time
	usd  float64
	pure bool // outside usage: raises utilization, no plugin cost
}

func newSimulation(seed int64, fiveHour, k float64) *simulation {
	return &simulation{rng: rand.New(rand.NewSource(seed)), fiveHour: fiveHour, k: k,
		start: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)}
}

// applyUntil applies every completion up to at, resetting windows whose time came.
func (s *simulation) applyUntil(at time.Time) {
	sort.Slice(s.pending, func(i, j int) bool { return s.pending[i].at.Before(s.pending[j].at) })
	for len(s.pending) > 0 && !s.pending[0].at.After(at) {
		p := s.pending[0]
		s.pending = s.pending[1:]
		s.rollWindows(p.at)
		s.used5 += p.usd / s.fiveHour * 100
		s.used7 += p.usd / (s.fiveHour * s.k) * 100
		if p.pure {
			s.outside += p.usd / s.fiveHour * 100
		}
	}
	s.rollWindows(at)
}

func (s *simulation) rollWindows(at time.Time) {
	if !s.reset5.IsZero() && !at.Before(s.reset5) {
		s.used5, s.reset5 = 0, time.Time{}
	}
	if !s.reset7.IsZero() && !at.Before(s.reset7) {
		s.used7, s.reset7 = 0, time.Time{}
	}
}

// request starts a plugin request at at that costs usd and lasts latency.
func (s *simulation) request(at time.Time, usd float64, latency time.Duration) {
	s.applyUntil(at)
	if s.reset5.IsZero() {
		s.reset5 = at.Add(FiveHours).Truncate(time.Second)
	}
	if s.reset7.IsZero() {
		s.reset7 = at.Add(Week).Truncate(time.Second)
	}
	s.obs = append(s.obs, Observation{At: at, Has5h: true, Used5h: math.Floor(s.used5), Reset5h: s.reset5,
		Has7d: true, Used7d: math.Floor(s.used7), Reset7d: s.reset7})
	s.pending = append(s.pending, pendingCost{at: at.Add(latency), usd: usd})
	s.costs = append(s.costs, CostEvent{At: at.Add(latency), USD: usd})
}

// outsideUsage spends usd outside the plugin at at.
func (s *simulation) outsideUsage(at time.Time, usd float64) {
	s.applyUntil(at)
	if s.reset5.IsZero() {
		s.reset5 = at.Add(FiveHours).Truncate(time.Second)
	}
	if s.reset7.IsZero() {
		s.reset7 = at.Add(Week).Truncate(time.Second)
	}
	s.pending = append(s.pending, pendingCost{at: at, usd: usd, pure: true})
}

type contamination int

const (
	cleanUsage contamination = iota
	sporadicOutside
	sustainedOutside
)

// run simulates days of sessions: each day a few busy sessions of
// concurrent requests separated by idle gaps.
func (s *simulation) run(days int, kind contamination) { s.runDays(0, days, kind) }

// runDays simulates days first to first+days-1.
func (s *simulation) runDays(first, days int, kind contamination) {
	for day := first; day < first+days; day++ {
		dayStart := s.start.Add(time.Duration(day) * 24 * time.Hour)
		for session := 0; session < 3; session++ {
			at := dayStart.Add(time.Duration(session)*5*time.Hour + time.Duration(s.rng.Intn(30))*time.Minute)
			end := at.Add(time.Duration(90+s.rng.Intn(90)) * time.Minute)
			sustainedFrom := at.Add(30 * time.Minute)
			sustainedTo := sustainedFrom.Add(40 * time.Minute)
			for at.Before(end) {
				// Up to three requests overlap.
				for range 1 + s.rng.Intn(3) {
					usd := 0.05 + s.rng.ExpFloat64()*0.25
					latency := time.Duration(2+s.rng.Intn(90)) * time.Second
					s.request(at.Add(time.Duration(s.rng.Intn(5000))*time.Millisecond), usd, latency)
				}
				switch kind {
				case sporadicOutside:
					if s.rng.Float64() < 0.01 {
						s.outsideUsage(at, s.fiveHour*0.03) // a 3% jump
					}
				case sustainedOutside:
					if !at.Before(sustainedFrom) && at.Before(sustainedTo) {
						s.outsideUsage(at, 0.12) // about half the plugin's rate
					}
				}
				at = at.Add(time.Duration(20+s.rng.Intn(40)) * time.Second)
			}
		}
	}
	s.applyUntil(s.start.Add(time.Duration(first+days) * 24 * time.Hour))
}

func (s *simulation) end(days int) time.Time {
	return s.start.Add(time.Duration(days) * 24 * time.Hour)
}

// process feeds the history to a stream in chunks, as refreshes would.
func process(t *testing.T, sim *simulation, until time.Time, chunk time.Duration, params Params) (StreamState, []Segment) {
	t.Helper()
	var state StreamState
	state.Cursor = sim.start.Add(-time.Second)
	var segments []Segment
	for state.Cursor.Before(until) {
		end := state.Cursor.Add(chunk)
		if end.After(until) {
			end = until
		}
		segments = append(segments, state.Advance(sim.obs, sim.costs, end, params)...)
	}
	return state, segments
}

func relativeError(estimate, truth float64) float64 { return (estimate - truth) / truth }

func TestEstimateCleanUsageWithWholePercentReports(t *testing.T) {
	params := DefaultParams()
	const truth = 40.0
	sim := newSimulation(1, truth, 6)
	sim.run(20, cleanUsage)
	_, segments := process(t, sim, sim.end(21), 24*time.Hour, params)
	estimate := EstimateSegments(segments, params, sim.end(21))
	if estimate.CleanBlocks < params.ColdStartBlocks {
		t.Fatalf("clean blocks = %d, want at least %d", estimate.CleanBlocks, params.ColdStartBlocks)
	}
	if err := relativeError(estimate.FiveHourUSD, truth); math.Abs(err) > 0.05 {
		t.Fatalf("T_5h = %.2f, truth %.2f (error %.1f%%)", estimate.FiveHourUSD, truth, err*100)
	}
	for _, segment := range segments {
		if segment.Gain < 1-1e-9 || segment.Gain != math.Round(segment.Gain) {
			t.Fatalf("segment gain %v is not a whole number of points", segment.Gain)
		}
	}
}

func TestOutsideUsageOnlyMakesEstimateConservative(t *testing.T) {
	params := DefaultParams()
	const truth = 40.0
	clean := newSimulation(7, truth, 6)
	clean.run(20, cleanUsage)
	_, cleanSegments := process(t, clean, clean.end(21), 24*time.Hour, params)
	cleanEstimate := EstimateSegments(cleanSegments, params, clean.end(21))
	for _, test := range []struct {
		name      string
		kind      contamination
		tolerance float64
	}{
		{"sporadic", sporadicOutside, 0.08},
		{"sustained", sustainedOutside, 0.10},
	} {
		t.Run(test.name, func(t *testing.T) {
			sim := newSimulation(7, truth, 6)
			sim.run(20, test.kind)
			if sim.outside < 50 {
				t.Fatalf("simulation added only %.1f outside points", sim.outside)
			}
			_, segments := process(t, sim, sim.end(21), 24*time.Hour, params)
			estimate := EstimateSegments(segments, params, sim.end(21))
			if estimate.ExcludedSegments == 0 {
				t.Fatal("no contaminated segment was excluded")
			}
			if err := relativeError(estimate.FiveHourUSD, truth); math.Abs(err) > test.tolerance {
				t.Fatalf("T_5h = %.2f, truth %.2f (error %.1f%%)", estimate.FiveHourUSD, truth, err*100)
			}
			// Outside usage adds points without cost, which raises ratios and
			// lowers the total: it may only err on the conservative side.
			if estimate.FiveHourUSD > cleanEstimate.FiveHourUSD*1.01 {
				t.Fatalf("contaminated T_5h %.2f exceeds clean estimate %.2f", estimate.FiveHourUSD, cleanEstimate.FiveHourUSD)
			}
			unfiltered := 0.0
			cost := 0.0
			for _, segment := range segments {
				unfiltered += segment.Gain
				cost += segment.CostUSD
			}
			if raw := 100 / (unfiltered / cost); estimate.FiveHourUSD < raw {
				t.Fatalf("filtering made the estimate worse: %.2f < unfiltered %.2f", estimate.FiveHourUSD, raw)
			}
		})
	}
}

func TestBlockReferenceModeAlsoFiltersOutsideUsage(t *testing.T) {
	params := DefaultParams()
	params.ReferenceMode = ReferenceBlock
	const truth = 40.0
	sim := newSimulation(11, truth, 6)
	sim.run(20, sustainedOutside)
	_, segments := process(t, sim, sim.end(21), 24*time.Hour, params)
	estimate := EstimateSegments(segments, params, sim.end(21))
	if err := relativeError(estimate.FiveHourUSD, truth); err > 0.03 || err < -0.15 {
		t.Fatalf("T_5h = %.2f, truth %.2f (error %.1f%%)", estimate.FiveHourUSD, truth, err*100)
	}
}

func TestOutOfOrderDeliveryAndChunking(t *testing.T) {
	params := DefaultParams()
	sim := newSimulation(3, 40, 6)
	sim.run(10, cleanUsage)
	_, whole := process(t, sim, sim.end(11), 30*24*time.Hour, params)
	// Shuffle the input: storage order must not matter, and refreshes of any
	// size must produce the same segments.
	rng := rand.New(rand.NewSource(5))
	rng.Shuffle(len(sim.obs), func(i, j int) { sim.obs[i], sim.obs[j] = sim.obs[j], sim.obs[i] })
	rng.Shuffle(len(sim.costs), func(i, j int) { sim.costs[i], sim.costs[j] = sim.costs[j], sim.costs[i] })
	_, chunked := process(t, sim, sim.end(11), 7*time.Minute, params)
	if len(whole) != len(chunked) || len(whole) == 0 {
		t.Fatalf("segments = %d whole, %d chunked", len(whole), len(chunked))
	}
	for i := range whole {
		if !whole[i].At.Equal(chunked[i].At) || math.Abs(whole[i].CostUSD-chunked[i].CostUSD) > 1e-9 || whole[i].Gain != chunked[i].Gain {
			t.Fatalf("segment %d differs: %+v vs %+v", i, whole[i], chunked[i])
		}
	}
}

func TestStaleReportsDoNotRaiseUtilization(t *testing.T) {
	params := DefaultParams()
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	reset := base.Add(4 * time.Hour)
	obs := []Observation{
		{At: base, Has5h: true, Used5h: 10, Reset5h: reset},
		{At: base.Add(time.Minute), Has5h: true, Used5h: 11, Reset5h: reset},
		{At: base.Add(2 * time.Minute), Has5h: true, Used5h: 10, Reset5h: reset}, // began earlier, arrived later
		{At: base.Add(3 * time.Minute), Has5h: true, Used5h: 12, Reset5h: reset},
	}
	costs := []CostEvent{{At: base.Add(90 * time.Second), USD: 0.4}, {At: base.Add(150 * time.Second), USD: 0.3}}
	var state StreamState
	state.Cursor = base.Add(-time.Second)
	segments := state.Advance(obs, costs, base.Add(time.Hour), params)
	if len(segments) != 1 || segments[0].Gain != 1 || math.Abs(segments[0].CostUSD-0.7) > 1e-9 {
		t.Fatalf("segments = %+v", segments)
	}
}

func TestFiveHourResetAndGapsBreakBlocks(t *testing.T) {
	params := DefaultParams()
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	reset := base.Add(10 * time.Minute)
	var obs []Observation
	var costs []CostEvent
	used := 50.0
	for i := 0; i < 40; i++ {
		at := base.Add(time.Duration(i) * 30 * time.Second)
		if !at.Before(reset) && used > 20 {
			used = 0
		}
		next := reset
		if !at.Before(reset) {
			next = reset.Add(FiveHours)
		}
		obs = append(obs, Observation{At: at, Has5h: true, Used5h: used, Reset5h: next})
		costs = append(costs, CostEvent{At: at.Add(time.Second), USD: 0.5})
		used++
	}
	// A request after a long gap starts another block.
	late := base.Add(time.Hour)
	obs = append(obs, Observation{At: late, Has5h: true, Used5h: used + 5, Reset5h: reset.Add(FiveHours)},
		Observation{At: late.Add(30 * time.Second), Has5h: true, Used5h: used + 6, Reset5h: reset.Add(FiveHours)},
		Observation{At: late.Add(60 * time.Second), Has5h: true, Used5h: used + 7, Reset5h: reset.Add(FiveHours)})
	costs = append(costs, CostEvent{At: late.Add(31 * time.Second), USD: 0.5}, CostEvent{At: late.Add(61 * time.Second), USD: 0.5})
	var state StreamState
	state.Cursor = base.Add(-time.Second)
	segments := state.Advance(obs, costs, base.Add(2*time.Hour), params)
	blocks := map[time.Time]bool{}
	for _, segment := range segments {
		blocks[segment.Block] = true
		if segment.Block.Before(reset) && !segment.At.Before(reset) {
			t.Fatalf("segment %+v spans the 5-hour reset", segment)
		}
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (before reset, after reset, after gap): %+v", len(blocks), segments)
	}
	// The jump over the gap was never counted as a segment.
	for _, segment := range segments {
		if segment.Gain > 1 {
			t.Fatalf("segment %+v includes the jump across the gap", segment)
		}
	}
}

// kFrom runs a k stream over the simulation in daily chunks.
func kFrom(sim *simulation, until time.Time, params Params) []KBucket {
	stream := KStream{Cursor: sim.start.Add(-time.Second)}
	var evidence []KEvidence
	for stream.Cursor.Before(until) {
		end := stream.Cursor.Add(24 * time.Hour)
		if end.After(until) {
			end = until
		}
		evidence = append(evidence, stream.Advance(sim.obs, end)...)
	}
	return BucketEvidence(evidence)
}

func TestKEstimate(t *testing.T) {
	params := DefaultParams()
	const k = 6.0
	sim := newSimulation(9, 40, k)
	sim.run(12, sporadicOutside)
	estimate := EstimateK(kFrom(sim, sim.end(13), params), params, sim.end(13))
	if estimate.K == nil || estimate.Basis != KBasisRecent {
		t.Fatalf("k not estimated: %+v", estimate)
	}
	if err := relativeError(*estimate.K, k); math.Abs(err) > 0.03 {
		t.Fatalf("k = %.3f, truth %.2f (%+v)", *estimate.K, k, estimate)
	}
	early := EstimateK(kFrom(sim, sim.start.Add(30*time.Minute), params), params, sim.start.Add(30*time.Minute))
	if early.K != nil || early.Basis != KBasisNone {
		t.Fatalf("k estimated before enough weekly gain: %+v", early)
	}
}

func TestKFollowsAChangeThroughTheRecentGroup(t *testing.T) {
	params := DefaultParams()
	sim := newSimulation(21, 40, 6)
	sim.runDays(0, 30, cleanUsage)
	sim.k = 4
	sim.runDays(30, 14, cleanUsage)
	now := sim.end(44)
	estimate := EstimateK(kFrom(sim, now, params), params, now)
	if !estimate.Recent.Valid || !estimate.Old.Valid || estimate.Basis != KBasisBlend {
		t.Fatalf("groups = %+v", estimate)
	}
	if relativeError(estimate.Recent.K, 4) > 0.04 || relativeError(estimate.Recent.K, 4) < -0.04 ||
		relativeError(estimate.Old.K, 6) > 0.04 || relativeError(estimate.Old.K, 6) < -0.04 {
		t.Fatalf("recent k %.3f (truth 4), old k %.3f (truth 6)", estimate.Recent.K, estimate.Old.K)
	}
	want := 0.9*estimate.Recent.K + 0.1*estimate.Old.K
	if math.Abs(*estimate.K-want) > 1e-9 || *estimate.K > 4.4 {
		t.Fatalf("k = %.3f, want %.3f", *estimate.K, want)
	}
	// An all-time sum would still be dominated by the old k.
	if estimate.Total.K < 4.6 {
		t.Fatalf("total k = %.3f", estimate.Total.K)
	}
}

func TestKGroupFallbacks(t *testing.T) {
	params := DefaultParams()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	recent := func(g5, g7 float64) KBucket { return KBucket{At: now.Add(-48 * time.Hour), Gain5h: g5, Gain7d: g7} }
	old := func(g5, g7 float64) KBucket {
		return KBucket{At: now.Add(-30 * 24 * time.Hour), Gain5h: g5, Gain7d: g7}
	}
	for _, test := range []struct {
		name    string
		buckets []KBucket
		basis   string
		k       float64
	}{
		{"both valid", []KBucket{recent(50, 10), old(80, 10)}, KBasisBlend, 0.9*5 + 0.1*8},
		{"recent only", []KBucket{recent(50, 10), old(16, 2)}, KBasisRecent, 5},
		{"old only", []KBucket{recent(10, 2), old(80, 10)}, KBasisOld, 8},
		{"combined", []KBucket{recent(30, 6), old(28, 4)}, KBasisTotal, 5.8},
		{"not enough", []KBucket{recent(20, 4), old(20, 4)}, KBasisNone, 0},
		{"no evidence", nil, KBasisNone, 0},
	} {
		estimate := EstimateK(test.buckets, params, now)
		if estimate.Basis != test.basis || (test.k == 0) != (estimate.K == nil) || estimate.K != nil && math.Abs(*estimate.K-test.k) > 1e-9 {
			t.Errorf("%s: %+v", test.name, estimate)
		}
	}
	// Without a valid k, cold start uses the tier's default k.
	status := ResolveAccount("a", AccountSettings{Tier: TierPro}, Estimate{}, EstimateK(nil, params, now), Observation{}, params, now)
	if status.K == nil || *status.K != 8 || status.KSource != SourceTier || status.FiveHourTotal != 250.0/8 || status.WeeklyTotal != 250 || !status.ColdStart {
		t.Fatalf("status = %+v", status)
	}
}

func TestColdStartSwitchesToEstimate(t *testing.T) {
	params := DefaultParams()
	const truth = 40.0
	sim := newSimulation(13, truth, 6)
	sim.run(20, cleanUsage)
	now := sim.end(21)
	_, segments := process(t, sim, now, 24*time.Hour, params)
	latest := Observation{At: now.Add(-time.Minute), Has5h: true, Used5h: 20, Reset5h: now.Add(time.Hour), Has7d: true, Used7d: 50, Reset7d: now.Add(48 * time.Hour)}

	// Few blocks: cold start uses the tier default; without a manual k and
	// before k is estimated, the tier's default k gives the 5-hour total.
	early := EstimateSegments(segments[:40], params, now)
	kEstimate := EstimateK(kFrom(sim, now, params), params, now)
	status := ResolveAccount("a", AccountSettings{Tier: TierMax5x}, early, KEstimate{}, latest, params, now)
	if !status.ColdStart || status.WeeklyTotal != 1250 || status.FiveHourTotal != 1250.0/8 || status.KSource != SourceTier {
		t.Fatalf("cold status = %+v", status)
	}
	params.TierK[TierMax5x] = 5
	if tuned := ResolveAccount("a", AccountSettings{Tier: TierMax5x}, early, KEstimate{}, latest, params, now); tuned.FiveHourTotal != 250 {
		t.Fatalf("tier k override = %+v", tuned)
	}
	params.TierK[TierMax5x] = 8
	manual := 5.0
	status = ResolveAccount("a", AccountSettings{Tier: TierMax5x, ManualK: &manual}, early, KEstimate{}, latest, params, now)
	if status.FiveHourTotal != 250 || status.KSource != SourceManual {
		t.Fatalf("cold status with manual k = %+v", status)
	}
	status = ResolveAccount("a", AccountSettings{Tier: TierMax5x}, early, kEstimate, latest, params, now)
	if status.KSource != SourceAuto || status.FiveHourTotal <= 0 {
		t.Fatalf("cold status with estimated k = %+v", status)
	}

	full := EstimateSegments(segments, params, now)
	status = ResolveAccount("a", AccountSettings{Tier: TierMax5x}, full, kEstimate, latest, params, now)
	if status.ColdStart || status.TotalsFrom != SourceEstimated {
		t.Fatalf("warm status = %+v", status)
	}
	if err := relativeError(status.FiveHourTotal, truth); math.Abs(err) > 0.05 {
		t.Fatalf("T_5h = %.2f", status.FiveHourTotal)
	}
	if math.Abs(status.WeeklyTotal-*status.K*status.FiveHourTotal) > 1e-9 {
		t.Fatalf("T_week = %.2f, k %.2f", status.WeeklyTotal, *status.K)
	}
	wantWeek := params.SafetyFactor * status.WeeklyTotal * 0.5
	if math.Abs(status.RemainingWeek-wantWeek) > 1e-9 || math.Abs(status.RemainingFiveHour-params.SafetyFactor*status.FiveHourTotal*0.8) > 1e-9 {
		t.Fatalf("remaining = %.2f / %.2f", status.RemainingWeek, status.RemainingFiveHour)
	}
	// A window whose reset passed reads as unused.
	status = ResolveAccount("a", AccountSettings{}, full, kEstimate, latest, params, now.Add(2*time.Hour))
	if status.Used5h != 0 || status.Used7d != 50 || status.Tier != TierPro {
		t.Fatalf("expired status = %+v", status)
	}
}

func TestEstimateAcrossSeeds(t *testing.T) {
	params := DefaultParams()
	const truth = 40.0
	for seed := int64(100); seed < 105; seed++ {
		var clean float64
		for _, kind := range []contamination{cleanUsage, sporadicOutside, sustainedOutside} {
			sim := newSimulation(seed, truth, 6)
			sim.run(20, kind)
			_, segments := process(t, sim, sim.end(21), 24*time.Hour, params)
			estimate := EstimateSegments(segments, params, sim.end(21))
			if kind == cleanUsage {
				clean = estimate.FiveHourUSD
			}
			if err := relativeError(estimate.FiveHourUSD, truth); err > 0.02 || err < -0.06 {
				t.Errorf("seed %d kind %d: T_5h = %.2f", seed, kind, estimate.FiveHourUSD)
			}
			if estimate.FiveHourUSD > clean*1.01 {
				t.Errorf("seed %d kind %d: contaminated T_5h %.2f exceeds clean %.2f", seed, kind, estimate.FiveHourUSD, clean)
			}
			estimateK := EstimateK(kFrom(sim, sim.end(21), params), params, sim.end(21))
			if k := estimateK.K; k == nil || relativeError(*k, 6) > 0.03 || relativeError(*k, 6) < -0.03 {
				t.Errorf("seed %d kind %d: k = %+v", seed, kind, estimateK)
			}
		}
	}
}

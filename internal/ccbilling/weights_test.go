package ccbilling

import (
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func reflectEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

// mixedModel is one model of a mixed simulation: its share of a session's
// requests and the quota its cost uses per USD.
type mixedModel struct {
	name   string
	weight float64
}

// runMixed simulates days of sessions whose model mix varies: each session
// draws its own probability of sending each tested model, from mix.
func (s *simulation) runMixed(days int, base []string, tested []mixedModel, mix func(session int) []float64, kind contamination) {
	session := 0
	for day := range days {
		dayStart := s.start.Add(time.Duration(day) * 24 * time.Hour)
		for slot := 0; slot < 3; slot++ {
			probabilities := mix(session)
			session++
			at := dayStart.Add(time.Duration(slot)*5*time.Hour + time.Duration(s.rng.Intn(30))*time.Minute)
			end := at.Add(time.Duration(90+s.rng.Intn(90)) * time.Minute)
			for at.Before(end) {
				for range 1 + s.rng.Intn(3) {
					usd := 0.05 + s.rng.ExpFloat64()*0.25
					latency := time.Duration(2+s.rng.Intn(90)) * time.Second
					model, weight := base[s.rng.Intn(len(base))], 1.0
					draw := s.rng.Float64()
					for i, item := range tested {
						if draw < probabilities[i] {
							model, weight = item.name, item.weight
							break
						}
						draw -= probabilities[i]
					}
					s.requestModel(at.Add(time.Duration(s.rng.Intn(5000))*time.Millisecond), usd, latency, model, weight)
				}
				if kind == sporadicOutside && s.rng.Float64() < 0.01 {
					s.outsideUsage(at, s.fiveHour*0.03)
				}
				at = at.Add(time.Duration(20+s.rng.Intn(40)) * time.Second)
			}
		}
	}
	s.applyUntil(s.end(days))
}

func weightSettings(items ...WeightItem) WeightSettings {
	settings := DefaultWeightSettings()
	settings.Items = items
	normalized, err := settings.normalized()
	if err != nil {
		panic(err)
	}
	return normalized
}

func mixedSegments(t *testing.T, seed int64, fiveHour float64, days int, tested []mixedModel, mix func(int) []float64, kind contamination) []Segment {
	t.Helper()
	sim := newSimulation(seed, fiveHour, 8)
	sim.runMixed(days, []string{"claude-sonnet", "claude-opus"}, tested, mix, kind)
	_, segments := process(t, sim, sim.end(days), 6*time.Hour, DefaultParams())
	return segments
}

func TestEstimateWeightsRecoversAMultiplierFromMixedUsage(t *testing.T) {
	params := DefaultParams()
	tested := []mixedModel{{name: "claude-fable", weight: 4}}
	// Sessions range from no Fable to mostly Fable.
	mix := func(session int) []float64 { return []float64{float64(session%7) / 10} }
	accounts := []AccountSegments{
		{AuthIndex: "a1", Segments: mixedSegments(t, 1, 40, 30, tested, mix, cleanUsage)},
		// A second account of another size shares the multiplier.
		{AuthIndex: "a2", Segments: mixedSegments(t, 2, 160, 30, tested, mix, sporadicOutside)},
	}
	settings := weightSettings(WeightItem{ID: "fable", Models: []string{"claude-fable"}})
	now := newSimulation(0, 1, 1).end(30)
	estimate := EstimateWeights(accounts, settings, params, now)
	item := estimate.Items[0]
	if item.Status != WeightOK || item.Multiplier == nil {
		t.Fatalf("estimate = %+v", estimate)
	}
	if math.Abs(*item.Multiplier-4)/4 > 0.08 {
		t.Fatalf("multiplier = %.3f ± %.3f, want 4: %+v", *item.Multiplier, item.StdErr, item)
	}
	// The standard error is calibrated across seeds (rms z ≈ 1); one run
	// stays within three of them.
	if math.Abs(*item.Multiplier-4) > 3*item.StdErr || item.Low >= *item.Multiplier || item.High <= *item.Multiplier {
		t.Fatalf("multiplier %.3f ± %.3f is not consistent with 4", *item.Multiplier, item.StdErr)
	}
	if !estimate.Converged || item.ShareMax <= item.ShareMin || item.Segments < settings.MinSegments {
		t.Fatalf("estimate = %+v", estimate)
	}
	t.Logf("m = %.3f ± %.3f over %d chunks, share %.0f%%–%.0f%%", *item.Multiplier, item.StdErr, estimate.Chunks, item.ShareMin*100, item.ShareMax*100)
}

func TestEstimateWeightsSeparatesTwoItems(t *testing.T) {
	params := DefaultParams()
	tested := []mixedModel{{name: "claude-fable-20261001", weight: 4}, {name: "claude-haiku", weight: 0.5}}
	// The two items' shares vary independently of each other.
	mix := func(session int) []float64 { return []float64{float64(session%5) / 10, float64(session%3) / 6} }
	accounts := []AccountSegments{{AuthIndex: "a1", Segments: mixedSegments(t, 3, 60, 40, tested, mix, cleanUsage)}}
	settings := weightSettings(
		// Two dated names of one model form one item.
		WeightItem{ID: "fable", Name: "Fable", Models: []string{"claude-fable-20261001", "Claude-Fable-20260801"}},
		WeightItem{ID: "haiku", Models: []string{"claude-haiku"}},
	)
	estimate := EstimateWeights(accounts, settings, params, newSimulation(0, 1, 1).end(40))
	for i, want := range []float64{4, 0.5} {
		item := estimate.Items[i]
		if item.Status != WeightOK || math.Abs(*item.Multiplier-want) > math.Max(3*item.StdErr, 0.02*want) {
			t.Fatalf("item %d = %+v, want %v", i, item, want)
		}
	}
}

// directSegments builds segments from requests whose costs split between
// models by split(request): true utilization rises by points per baseline
// USD times the weighted cost, and a segment closes whenever its whole
// percent rises.
func directSegments(rng *rand.Rand, requests int, pointsPerUSD float64, weights map[string]float64, split func(int) map[string]float64) []Segment {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var segments []Segment
	var used float64
	block := start
	models := map[string]float64{}
	for i := range requests {
		at := start.Add(time.Duration(i) * 20 * time.Second)
		if i%300 == 0 {
			block, used, models = at, rng.Float64(), map[string]float64{}
		}
		usd := 0.05 + rng.ExpFloat64()*0.25
		before := math.Floor(used)
		for model, share := range split(i) {
			models[model] += usd * share
			used += usd * share * weights[model] * pointsPerUSD
		}
		if gain := math.Floor(used) - before; gain > 0 {
			cost := 0.0
			for _, value := range models {
				cost += value
			}
			segments = append(segments, Segment{At: at, Block: block, CostUSD: cost, Gain: gain, Models: models})
			models = map[string]float64{}
		}
	}
	return segments
}

func TestEstimateWeightsDetectsAConstantShare(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	weights := map[string]float64{"claude-sonnet": 1, "claude-fable": 4, "claude-haiku": 0.5}
	// Every request splits its cost 70/30, so the multiplier and the
	// baseline rate cannot be told apart.
	constant := directSegments(rng, 20000, 2.5, weights, func(int) map[string]float64 {
		return map[string]float64{"claude-sonnet": 0.7, "claude-fable": 0.3}
	})
	now := constant[len(constant)-1].At
	settings := weightSettings(WeightItem{ID: "fable", Models: []string{"claude-fable"}})
	estimate := EstimateWeights([]AccountSegments{{AuthIndex: "a1", Segments: constant}}, settings, DefaultParams(), now)
	if item := estimate.Items[0]; item.Status != WeightCollinear || item.Multiplier != nil || item.Segments < settings.MinSegments ||
		math.Abs(item.ShareMin-0.3) > 1e-9 || math.Abs(item.ShareMax-0.3) > 1e-9 {
		t.Fatalf("constant share: %+v", item)
	}
	// Two items whose shares vary but always together are identified only
	// jointly; the variance inflation detects it.
	together := directSegments(rng, 20000, 2.5, weights, func(i int) map[string]float64 {
		share := float64((i/150)%5) / 10
		return map[string]float64{"claude-sonnet": 1 - 1.5*share, "claude-fable": share, "claude-haiku": share / 2}
	})
	settings = weightSettings(WeightItem{ID: "fable", Models: []string{"claude-fable"}}, WeightItem{ID: "haiku", Models: []string{"claude-haiku"}})
	estimate = EstimateWeights([]AccountSegments{{AuthIndex: "a1", Segments: together}}, settings, DefaultParams(), together[len(together)-1].At)
	collinear := 0
	for _, item := range estimate.Items {
		if item.Status == WeightCollinear {
			collinear++
			if item.Multiplier != nil {
				t.Fatalf("item = %+v", item)
			}
		}
	}
	if collinear != 2 {
		t.Fatalf("estimate = %+v", estimate.Items)
	}
}

func TestEstimateWeightsNeedsEnoughSamples(t *testing.T) {
	tested := []mixedModel{{name: "claude-fable", weight: 4}}
	mix := func(session int) []float64 { return []float64{float64(session%7) / 10} }
	accounts := []AccountSegments{{AuthIndex: "a1", Segments: mixedSegments(t, 5, 40, 2, tested, mix, cleanUsage)}}
	now := newSimulation(0, 1, 1).end(2)
	settings := weightSettings(WeightItem{ID: "fable", Models: []string{"claude-fable"}})
	settings.MinSegments = 100000
	if item := EstimateWeights(accounts, settings, DefaultParams(), now).Items[0]; item.Status != WeightInsufficient || item.Multiplier != nil {
		t.Fatalf("too few segments: %+v", item)
	}
	settings.MinSegments, settings.MinCostUSD = 1, 1e6
	if item := EstimateWeights(accounts, settings, DefaultParams(), now).Items[0]; item.Status != WeightInsufficient || item.Multiplier != nil {
		t.Fatalf("too little cost: %+v", item)
	}
	// A model that never ran has nothing at all.
	settings = weightSettings(WeightItem{ID: "unused", Models: []string{"claude-unused"}})
	if item := EstimateWeights(accounts, settings, DefaultParams(), now).Items[0]; item.Status != WeightInsufficient || item.Segments != 0 {
		t.Fatalf("unused model: %+v", item)
	}
}

// Filtering on the raw basis would treat sessions heavy in a model that uses
// more quota per USD as outside usage; filtering on the converted basis
// keeps them and recovers the multiplier.
func TestEstimateWeightsFiltersOnTheConvertedBasis(t *testing.T) {
	params := DefaultParams()
	tested := []mixedModel{{name: "claude-fable", weight: 4}}
	// Most sessions are baseline only; every fourth is almost all Fable.
	mix := func(session int) []float64 {
		if session%4 == 3 {
			return []float64{0.9}
		}
		return []float64{0}
	}
	segments := mixedSegments(t, 6, 40, 30, tested, mix, cleanUsage)
	accounts := []AccountSegments{{AuthIndex: "a1", Segments: segments}}
	settings := weightSettings(WeightItem{ID: "fable", Models: []string{"claude-fable"}})
	now := newSimulation(0, 1, 1).end(30)
	fableShare := func(excluded []bool, want bool) float64 {
		var fable, total float64
		for i, segment := range segments {
			if excluded[i] == want {
				fable += segment.Models["claude-fable"]
				total += segment.CostUSD
			}
		}
		return fable / total
	}
	raw := SegmentExclusions(segments, params, now)
	if share := fableShare(raw, true); share < 0.5 {
		t.Fatalf("the raw filter should mostly exclude Fable: excluded share %.2f", share)
	}
	estimate := EstimateWeights(accounts, settings, params, now)
	item := estimate.Items[0]
	if item.Status != WeightOK || math.Abs(*item.Multiplier-4) > math.Max(3*item.StdErr, 0.1) {
		t.Fatalf("estimate = %+v", item)
	}
	converted := SegmentExclusions(ConvertSegments(segments, map[string]float64{"claude-fable": *item.Multiplier}), params, now)
	excludedRaw, excludedConverted := 0, 0
	for i := range segments {
		if raw[i] {
			excludedRaw++
		}
		if converted[i] {
			excludedConverted++
		}
	}
	if excludedConverted*3 > excludedRaw {
		t.Fatalf("converted basis excludes %d segments, raw %d", excludedConverted, excludedRaw)
	}
}

func TestConvertSegments(t *testing.T) {
	segments := []Segment{
		{CostUSD: 3, Gain: 1, Models: map[string]float64{"base": 1, "fable": 2}},
		{CostUSD: 2, Gain: 1}, // recorded before the split: unchanged
		{CostUSD: 2, Gain: 1, Models: map[string]float64{"fable": 1, unknownModel: 1}},
	}
	converted := ConvertSegments(segments, map[string]float64{"fable": 4})
	for i, want := range []float64{9, 2, 5} {
		if converted[i].CostUSD != want {
			t.Fatalf("segment %d = %v, want %v", i, converted[i].CostUSD, want)
		}
	}
	if segments[0].CostUSD != 3 {
		t.Fatal("conversion changed its input")
	}
	if same := ConvertSegments(segments, nil); &same[0] != &segments[0] {
		t.Fatal("no factors must return the segments unchanged")
	}
}

// Recording the model split leaves every segment's time, cost and gain as
// they were.
func TestModelSplitLeavesSegmentsUnchanged(t *testing.T) {
	sim := newSimulation(7, 40, 8)
	tested := []mixedModel{{name: "claude-fable", weight: 1}}
	sim.runMixed(5, []string{"claude-sonnet"}, tested, func(session int) []float64 { return []float64{0.3} }, sporadicOutside)
	params := DefaultParams()
	_, split := process(t, sim, sim.end(5), 6*time.Hour, params)
	for i := range sim.costs {
		sim.costs[i].Model = ""
	}
	_, plain := process(t, sim, sim.end(5), 6*time.Hour, params)
	if len(split) != len(plain) || len(split) == 0 {
		t.Fatalf("segments = %d and %d", len(split), len(plain))
	}
	for i := range split {
		a, b := split[i], plain[i]
		if !a.At.Equal(b.At) || !a.Block.Equal(b.Block) || a.CostUSD != b.CostUSD || a.Gain != b.Gain {
			t.Fatalf("segment %d differs: %+v vs %+v", i, a, b)
		}
		if a.CostUSD > 0 && !a.Split() || b.CostUSD > 0 && b.Split() {
			t.Fatalf("segment %d split = %v, %v", i, a.Split(), b.Split())
		}
		sum := 0.0
		for _, cost := range a.Models {
			sum += cost
		}
		if math.Abs(sum-a.CostUSD) > 1e-9 {
			t.Fatalf("segment %d models sum to %v of %v", i, sum, a.CostUSD)
		}
	}
	if !reflectEqual(EstimateSegments(split, params, sim.end(5)), EstimateSegments(plain, params, sim.end(5))) {
		t.Fatal("estimates differ")
	}
}

func TestCalibrationUsesPureSegmentsOnly(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tested := map[string]bool{"claude-fable": true, "claude-haiku": true}
	item := WeightItem{ID: "fable", Models: []string{"claude-fable"}}
	var segments []Segment
	at := start.Add(-24 * time.Hour)
	add := func(models map[string]float64, gain float64) {
		at = at.Add(time.Minute)
		cost := 0.0
		for _, value := range models {
			cost += value
		}
		segments = append(segments, Segment{At: at, Block: start, Gain: gain, CostUSD: cost, Models: models})
	}
	// Before the period: baseline only, $0.40 per point.
	for range 20 {
		add(map[string]float64{"claude-sonnet": 0.4}, 1)
	}
	at = start
	for i := range 40 {
		// Pure Fable at $0.10 per point: four times the baseline's quota.
		add(map[string]float64{"claude-fable": 0.09 + 0.02*float64(i%2)}, 1)
	}
	for range 7 {
		add(map[string]float64{"claude-fable": 0.1, "claude-sonnet": 0.2}, 1) // mixed
		add(map[string]float64{"claude-haiku": 0.5}, 1)                       // another tested model: not baseline
	}
	segments = append(segments, Segment{At: at.Add(time.Minute), Block: start, Gain: 1, CostUSD: 0.4}) // unsplit
	end := at.Add(time.Hour)
	result := calibrate(segments, item, tested, start, end, DefaultParams())
	if result.Status != CalibrationOK || result.Multiplier == nil || math.Abs(*result.Multiplier-4) > 0.05 {
		t.Fatalf("result = %+v", result)
	}
	if result.ItemSegments != 40 || result.Excluded != 14 || result.Unsplit != 1 || result.BaseFrom != CalibrationBaseRecent || result.BaseSegments != 20 {
		t.Fatalf("result = %+v", result)
	}
	if result.Low > 4 || result.High < 4 || math.Abs(result.ItemUSDPerPoint-0.1) > 1e-9 || math.Abs(result.BaseUSDPerPoint-0.4) > 1e-9 {
		t.Fatalf("result = %+v", result)
	}
	// Without enough pure segments there is no number.
	short := calibrate(segments, item, tested, start, start.Add(5*time.Minute), DefaultParams())
	if short.Status != CalibrationInsufficient || short.Multiplier != nil {
		t.Fatalf("short = %+v", short)
	}
}

func TestResolveWeightsPrecedence(t *testing.T) {
	manual := 3.0
	settings := weightSettings(
		WeightItem{ID: "manual", Models: []string{"m1"}, Manual: &manual},
		WeightItem{ID: "calibrated", Models: []string{"m2"}},
		WeightItem{ID: "estimated", Models: []string{"m3"}},
		WeightItem{ID: "collinear", Models: []string{"m4"}},
	)
	two, five, six := 2.0, 5.0, 6.0
	estimate := WeightEstimate{Items: []ItemEstimate{
		{ID: "manual", Status: WeightOK, Multiplier: &six},
		{ID: "calibrated", Status: WeightOK, Multiplier: &six},
		{ID: "estimated", Status: WeightOK, Multiplier: &five},
		{ID: "collinear", Status: WeightCollinear},
	}}
	ended := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	calibrations := []Calibration{
		{ID: 1, Item: WeightItem{ID: "calibrated"}, EndedAt: ended, Final: true, Result: CalibrationResult{Status: CalibrationOK, Multiplier: &six}},
		{ID: 2, Item: WeightItem{ID: "calibrated"}, EndedAt: ended.Add(time.Hour), Final: true, Result: CalibrationResult{Status: CalibrationOK, Multiplier: &two}},
		// Running or failed calibrations do not count.
		{ID: 3, Item: WeightItem{ID: "estimated"}, Result: CalibrationResult{Status: CalibrationRunning, Multiplier: &six}},
		{ID: 4, Item: WeightItem{ID: "collinear"}, EndedAt: ended, Final: true, Result: CalibrationResult{Status: CalibrationInsufficient}},
	}
	got := resolveWeights(settings, estimate, calibrations)
	want := []struct {
		multiplier float64
		source     string
	}{{3, WeightSourceManual}, {2, WeightSourceCalibration}, {5, WeightSourceEstimated}, {1, WeightSourceDefault}}
	for i, item := range want {
		if got[i].Multiplier != item.multiplier || got[i].Source != item.source {
			t.Fatalf("item %d = %+v, want %+v", i, got[i], item)
		}
	}
	if got[1].CalibrationID != 2 || got[3].Status != WeightCollinear {
		t.Fatalf("weights = %+v", got)
	}
}

func TestWeightSettingsValidation(t *testing.T) {
	settings := DefaultSettings()
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) {
		w.Items = []WeightItem{{ID: "a", Models: []string{"X"}}, {ID: "b", Models: []string{"x"}}}
	})
	if _, err := settings.Normalized(); err == nil || !strings.Contains(err.Error(), "already in test item") {
		t.Fatalf("err = %v", err)
	}
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items = []WeightItem{{ID: "a", Models: []string{" "}}} })
	if _, err := settings.Normalized(); err == nil {
		t.Fatal("an item needs a model")
	}
	zero := 0.0
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items = []WeightItem{{ID: "a", Models: []string{"x"}, Manual: &zero}} })
	if _, err := settings.Normalized(); err == nil {
		t.Fatal("a zero manual multiplier must be refused")
	}
	// Settings written before the weights existed keep their defaults.
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { *w = WeightSettings{} })
	normalized, err := settings.Normalized()
	if err != nil || !reflectEqual(normalized.Providers[ProviderClaude].Weights, DefaultWeightSettings()) {
		t.Fatalf("weights = %+v, %v", normalized.Providers[ProviderClaude].Weights, err)
	}
}

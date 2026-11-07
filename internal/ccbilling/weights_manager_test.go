package ccbilling

import (
	"database/sql"
	"math"
	"reflect"
	"testing"
	"time"
)

// fillHost stores a simulation's reports and costs as the billing tables do.
func fillHost(t *testing.T, host *fakeHost, sim *simulation, authIndex string) {
	t.Helper()
	tx, err := host.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, obs := range sim.obs {
		id++
		for _, row := range []struct {
			seconds int64
			used    float64
			reset   time.Time
		}{{18000, obs.Used5h, obs.Reset5h}, {604800, obs.Used7d, obs.Reset7d}} {
			if _, err := tx.Exec(`INSERT INTO quota_report_windows VALUES (?, 'unified', ?, ?, 'claude', ?, ?, ?)`,
				id, row.seconds, authIndex, nanos(obs.At), row.used, nanos(row.reset)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, cost := range sim.costs {
		if _, err := tx.Exec(`INSERT INTO request_events (at, auth_index, latency_ms, total_cost, currency, billing_model) VALUES (?, ?, 0, ?, 'USD', ?)`,
			nanos(cost.At), authIndex, cost.USD, cost.Model); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func weightedHost(t *testing.T) (*Manager, *fakeHost) {
	return weightedHostWith(t, func(session int) []float64 { return []float64{float64(session%7) / 10} })
}

func weightedHostWith(t *testing.T, mix func(int) []float64) (*Manager, *fakeHost) {
	t.Helper()
	host := newFakeHost(t)
	sim := newSimulation(11, 40, 8)
	sim.runMixed(30, []string{"claude-sonnet", "claude-opus"}, []mixedModel{{name: "claude-fable", weight: 4}}, mix, sporadicOutside)
	fillHost(t, host, sim, "a1")
	manager := NewManager(host)
	manager.RefreshInterval = 0
	manager.ProcessBudget = time.Minute
	return manager, host
}

func storedSegments(t *testing.T, host *fakeHost, authIndex string) []Segment {
	t.Helper()
	segments, err := loadSegments(host.db, authIndex, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return segments
}

func TestWeightsSwitchConvertsOnlyWhenOn(t *testing.T) {
	manager, host := weightedHost(t)
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "p", AuthIndexes: []string{"a1"}}}
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) {
		w.Items = []WeightItem{{ID: "fable", Name: "Fable", Models: []string{"claude-fable"}}}
	})
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	segments := storedSegments(t, host, "a1")
	if len(segments) < 1000 {
		t.Fatalf("segments = %d", len(segments))
	}
	for _, segment := range segments {
		if !segment.Split() {
			t.Fatalf("segment without a split: %+v", segment)
		}
	}
	params := settings.Params
	snap := manager.snap.Load()
	account := snap.accounts["a1"]
	// Off: exactly the estimate without weights, value for value.
	if snap.weights[ProviderClaude].basis != BasisRaw || !reflect.DeepEqual(account.estimate, EstimateSegments(segments, params, snap.at)) ||
		!reflect.DeepEqual(account.estimate, account.raw) {
		t.Fatal("the switched-off estimate differs from the raw estimate")
	}
	effective := snap.weights[ProviderClaude].effective[0]
	if effective.Source != WeightSourceEstimated || math.Abs(effective.Multiplier-4) > 0.2 {
		t.Fatalf("effective = %+v, estimate = %+v", effective, snap.weights[ProviderClaude].estimate)
	}
	off, _ := manager.Estimates()
	if off.For(ProviderClaude).Basis != BasisRaw || off.Accounts["a1"].Ratio != account.raw.Ratio {
		t.Fatalf("estimates = %+v", off)
	}
	// The raw basis excludes Fable-heavy segments as if they were outside usage.
	if account.raw.ExcludedSegments <= account.weighted.ExcludedSegments {
		t.Fatalf("excluded raw %d, weighted %d", account.raw.ExcludedSegments, account.weighted.ExcludedSegments)
	}

	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Enabled = true })
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	snap = manager.snap.Load()
	account = snap.accounts["a1"]
	factors := map[string]float64{"claude-fable": snap.weights[ProviderClaude].effective[0].Multiplier}
	if snap.weights[ProviderClaude].basis != BasisWeighted || !reflect.DeepEqual(snap.weights[ProviderClaude].factors, factors) ||
		!reflect.DeepEqual(account.estimate, EstimateSegments(ConvertSegments(segments, factors), params, snap.at)) {
		t.Fatal("the switched-on estimate is not the estimate of the converted costs")
	}
	// The weighted ratio is points per baseline USD: 100 / $40.
	if math.Abs(account.estimate.FiveHourUSD-40)/40 > 0.05 {
		t.Fatalf("weighted 5-hour total = %.2f", account.estimate.FiveHourUSD)
	}
	on, ok := manager.Estimates()
	if !ok || on.For(ProviderClaude).Basis != BasisWeighted || on.Accounts["a1"].Ratio != account.weighted.Ratio || len(on.For(ProviderClaude).Weights) != 1 {
		t.Fatalf("estimates = %+v", on)
	}
	view, err := manager.View()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Weights[ProviderClaude].Comparison) != 1 || view.Weights[ProviderClaude].Comparison[0].Raw.Ratio != account.raw.Ratio ||
		view.Weights[ProviderClaude].Comparison[0].Weighted.Ratio != account.weighted.Ratio || len(view.Weights[ProviderClaude].Models) != 3 {
		t.Fatalf("weights view = %+v", view.Weights)
	}

	// A manual multiplier takes precedence.
	manual := 2.0
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items[0].Manual = &manual })
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if weight := manager.snap.Load().weights[ProviderClaude].effective[0]; weight.Source != WeightSourceManual || weight.Multiplier != 2 {
		t.Fatalf("effective = %+v", weight)
	}
	// Trend marks the exclusions of the converted costs.
	trend, _, err := manager.Trend("a1")
	if err != nil || trend.Basis != BasisWeighted {
		t.Fatalf("trend = %+v, %v", trend.Basis, err)
	}
}

func TestCalibrationLifecycle(t *testing.T) {
	// Three sessions a day: days 10 to 14 send only Fable, the days before
	// only baseline models, the days after a mix.
	manager, host := weightedHostWith(t, func(session int) []float64 {
		switch day := session / 3; {
		case day < 10:
			return []float64{0}
		case day < 15:
			return []float64{1}
		}
		return []float64{0.3}
	})
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "p", AuthIndexes: []string{"a1"}}}
	editWeights(&settings, ProviderClaude, func(w *WeightSettings) { w.Items = []WeightItem{{ID: "fable", Models: []string{"claude-fable"}}} })
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartCalibration("missing", "a1"); !IsInvalid(err) {
		t.Fatalf("err = %v", err)
	}
	if _, err := manager.StartCalibration("fable", "a2"); !IsInvalid(err) {
		t.Fatalf("err = %v", err)
	}
	// Calibrate the Fable-only days, which the history already covers.
	host.now = time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	calibration, err := manager.StartCalibration("fable", "a1")
	if err != nil || calibration.Result.Status != CalibrationRunning {
		t.Fatalf("calibration = %+v, %v", calibration, err)
	}
	if _, err := manager.StartCalibration("fable", "a1"); !IsInvalid(err) {
		t.Fatalf("a second calibration of the account: %v", err)
	}
	host.now = time.Date(2026, 9, 15, 23, 0, 0, 0, time.UTC)
	if calibrations := manager.snap.Load().weights[ProviderClaude].calibrations; len(calibrations) != 1 || calibrations[0].Final ||
		calibrations[0].Result.Status != CalibrationRunning {
		t.Fatalf("calibrations = %+v", calibrations)
	}
	if err := manager.StopCalibration(calibration.ID); err != nil {
		t.Fatal(err)
	}
	// The history was processed past the period, so the result is final.
	calibrations := manager.snap.Load().weights[ProviderClaude].calibrations
	if len(calibrations) != 1 || !calibrations[0].Final {
		t.Fatalf("calibrations = %+v", calibrations)
	}
	result := calibrations[0].Result
	// The period has no baseline segments, so it compares with the days before.
	if result.Status != CalibrationOK || result.BaseFrom != CalibrationBaseRecent || math.Abs(*result.Multiplier-4) > math.Max(3*result.StdErr, 0.1) {
		t.Fatalf("result = %+v", result)
	}
	if weight := manager.snap.Load().weights[ProviderClaude].effective[0]; weight.Source != WeightSourceCalibration || weight.CalibrationID != calibration.ID {
		t.Fatalf("effective = %+v", weight)
	}
	var stored string
	if err := host.db.QueryRow(`SELECT result_json FROM cc_calibrations WHERE id = ?`, calibration.ID).Scan(&stored); err != nil || stored == "" {
		t.Fatalf("stored result = %q, %v", stored, err)
	}
	if err := manager.StopCalibration(calibration.ID); !IsCalibrationNotFound(err) {
		t.Fatalf("stopping an ended calibration: %v", err)
	}
	if err := manager.DeleteCalibration(calibration.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteCalibration(calibration.ID); !IsCalibrationNotFound(err) {
		t.Fatalf("deleting twice: %v", err)
	}
	if calibrations := manager.snap.Load().weights[ProviderClaude].calibrations; len(calibrations) != 0 {
		t.Fatalf("calibrations = %+v", calibrations)
	}
}

func TestSchemaMigrationIsRepeatable(t *testing.T) {
	host := newFakeHost(t)
	for range 2 {
		if err := EnsureSchema(host.db); err != nil {
			t.Fatal(err)
		}
	}
	// Each migration step can run again on tables it already created.
	for _, migration := range ccMigrations {
		if _, err := host.db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	var version string
	if err := host.db.QueryRow(`SELECT value FROM cc_meta WHERE key = 'schema_version'`).Scan(&version); err != nil || version != "4" {
		t.Fatalf("version = %q, %v", version, err)
	}
}

// A version 2 database keeps its segments, which have no model split until a
// rebuild recreates them from the retained request history.
func TestUpgradeKeepsSegmentsAndRebuildAddsTheirSplit(t *testing.T) {
	manager, host := weightedHost(t)
	settings := DefaultSettings()
	settings.Pools = []Pool{{ID: "p", AuthIndexes: []string{"a1"}}}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	before := storedSegments(t, host, "a1")
	// Turn the database back into version 2: no split, no version 3 tables.
	if _, err := host.db.Exec(`DROP TABLE cc_segment_costs; DROP TABLE cc_calibrations; UPDATE cc_meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	upgraded := NewManager(host)
	upgraded.RefreshInterval, upgraded.ProcessBudget = 0, time.Minute
	view, err := upgraded.View()
	if err != nil {
		t.Fatal(err)
	}
	after := storedSegments(t, host, "a1")
	if len(after) != len(before) || view.Weights[ProviderClaude].UnsplitSegments == 0 || view.Weights[ProviderClaude].Segments != len(before) {
		t.Fatalf("segments %d → %d, unsplit %d", len(before), len(after), view.Weights[ProviderClaude].UnsplitSegments)
	}
	for i := range after {
		if after[i].Split() && after[i].CostUSD > 0 || after[i].CostUSD != before[i].CostUSD || after[i].Gain != before[i].Gain {
			t.Fatalf("segment %d = %+v, was %+v", i, after[i], before[i])
		}
	}
	// The estimate does not depend on the split.
	if !reflect.DeepEqual(upgraded.snap.Load().accounts["a1"].estimate, EstimateSegments(before, settings.Params, upgraded.snap.Load().at)) {
		t.Fatal("the upgraded estimate changed")
	}
	if err := upgraded.Rebuild(); err != nil {
		t.Fatal(err)
	}
	rebuilt := storedSegments(t, host, "a1")
	if len(rebuilt) != len(before) {
		t.Fatalf("rebuilt %d segments of %d", len(rebuilt), len(before))
	}
	for i := range rebuilt {
		if !rebuilt[i].Split() || rebuilt[i].CostUSD != before[i].CostUSD {
			t.Fatalf("rebuilt segment %d = %+v", i, rebuilt[i])
		}
	}
}

func TestPruneRemovesSegmentSplits(t *testing.T) {
	host := newFakeHost(t)
	if err := EnsureSchema(host.db); err != nil {
		t.Fatal(err)
	}
	old, recent := host.now.Add(-48*time.Hour), host.now.Add(-time.Hour)
	if err := commitAdvance(host.db, "a1", StreamState{}, []Segment{
		{At: old, Block: old, CostUSD: 1, Gain: 1, Models: map[string]float64{"x": 1}},
		{At: recent, Block: recent, CostUSD: 2, Gain: 1, Models: map[string]float64{"x": 1.5, "y": 0.5}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := pruneSegments(host.db, host.now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var costs int
	if err := host.db.QueryRow(`SELECT count(*) FROM cc_segment_costs`).Scan(&costs); err != nil || costs != 2 {
		t.Fatalf("split rows = %d, %v", costs, err)
	}
	segments := storedSegments(t, host, "a1")
	if len(segments) != 1 || !reflect.DeepEqual(segments[0].Models, map[string]float64{"x": 1.5, "y": 0.5}) {
		t.Fatalf("segments = %+v", segments)
	}
	_ = sql.ErrNoRows
}

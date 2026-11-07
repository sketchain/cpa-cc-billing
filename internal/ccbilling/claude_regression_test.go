package ccbilling

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// claudeRegressionDigest runs a simulated Claude account with sporadic
// outside usage through the manager, with a fixed and a remainder key, and
// describes every estimate, reservation, admission and balance it produces.
// The golden digest below was produced by the plugin before providers
// existed, so Claude's results stay value for value what they were.
func claudeRegressionDigest(t *testing.T) string {
	t.Helper()
	sim := newSimulation(29, 40, 6)
	sim.run(24, sporadicOutside)
	host := newFakeHost(t)
	host.now = sim.end(24).Add(-30 * time.Minute)
	tx, err := host.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i, obs := range sim.obs {
		if _, err := tx.Exec(`INSERT INTO request_events (id, at, scope, auth_index, latency_ms, total_cost, currency, billing_model, upstream_model)
			VALUES (?, ?, 'fixed', 'a1', ?, ?, 'USD', 'claude-test', 'claude-test')`,
			i+1, nanos(obs.At), sim.costs[i].At.Sub(obs.At).Milliseconds(), sim.costs[i].USD); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			seconds int64
			used    float64
			reset   time.Time
		}{{18000, obs.Used5h, obs.Reset5h}, {604800, obs.Used7d, obs.Reset7d}} {
			if _, err := tx.Exec(`INSERT INTO quota_report_windows VALUES (?, 'unified', ?, 'a1', 'claude', ?, ?, ?)`,
				i+1, row.seconds, nanos(obs.At), row.used, nanos(row.reset)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	host.keys["fixed"] = KeyInfo{PlanID: "plan", HasWeekly: true, WeeklyUSD: 100, UsedWeekUSD: 20,
		CycleStart: host.now.Add(-48 * time.Hour), CycleEnd: host.now.Add(5 * 24 * time.Hour)}
	host.keys["remainder"] = KeyInfo{}
	host.usage["fixed"] = 9
	host.usage["remainder"] = 30
	manager := NewManager(host)
	manager.RefreshInterval = 0
	manager.ProcessBudget = time.Hour
	settings := DefaultSettings()
	manualK := 6.5
	cap := 70.0
	settings.Pools = []Pool{{ID: "p", Name: "Pool", AuthIndexes: []string{"a1"}}}
	settings.Accounts["a1"] = AccountSettings{Tier: TierMax5x}
	settings.Keys["fixed"] = KeySettings{Kind: KeyFixed, PoolID: "p"}
	settings.Keys["remainder"] = KeySettings{Kind: KeyRemainder, PoolID: "p", WeeklyCapUSD: &cap}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	last := sim.obs[len(sim.obs)-1]
	last.Reset7d = host.now.Add(3 * 24 * time.Hour)
	var b strings.Builder
	describe := func(label string) {
		if err := manager.Refresh(); err != nil {
			t.Fatal(err)
		}
		view, err := manager.View()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s segments=%d\n", label, view.Segments)
		for _, account := range view.Accounts {
			k := "nil"
			if account.K != nil {
				k = fmt.Sprintf("%.10g", *account.K)
			}
			fmt.Fprintf(&b, "account %s tier=%s ratio=%.10g t5=%.10g blocks=%d/%d segs=%d/%d k=%s/%s cold=%v from=%s T5=%.10g T7=%.10g u=%.4g/%.4g R=%.10g/%.10g\n",
				account.AuthIndex, account.Tier, account.Ratio, account.Estimate.FiveHourUSD, account.CleanBlocks, account.TotalBlocks,
				account.Estimate.Segments, account.ExcludedSegments, k, account.KSource, account.ColdStart, account.TotalsFrom,
				account.FiveHourTotal, account.WeeklyTotal, account.Used5h, account.Used7d, account.RemainingFiveHour, account.RemainingWeek)
		}
		for _, pool := range view.Pools {
			fmt.Fprintf(&b, "pool %s R=%.10g/%.10g P=%.10g/%.10g k=%.10g five=%v balance=%.10g\n", pool.ID, pool.RemainingFiveHour, pool.RemainingWeek,
				pool.ReservedFiveHour, pool.ReservedWeek, pool.K, pool.FiveHourEnabled, pool.RemainderBalance)
			for _, fixed := range pool.Fixed {
				fmt.Fprintf(&b, "fixed %s used=%.10g/%.10g reserve=%.10g/%.10g share=%.10g\n", fixed.Scope, fixed.UsedFiveHour, fixed.UsedWeekUSD,
					fixed.ReserveFiveHour, fixed.RemainingWeek, fixed.EffectiveShare)
			}
		}
		for _, scope := range []string{"fixed", "remainder"} {
			decision := manager.Admit(scope)
			balance, status := manager.Balance(scope)
			fmt.Fprintf(&b, "key %s allowed=%v reason=%s balance=%.10g/%d reported=%.10g\n", scope, decision.Allowed, decision.Reason, balance, status,
				ReportedBalance(balance))
		}
	}
	for _, used := range []float64{20, 60, 97} {
		manager.Observe("a1", Observation{At: host.now, Has5h: true, Used5h: used, Reset5h: host.now.Add(2 * time.Hour),
			Has7d: true, Used7d: 40, Reset7d: last.Reset7d})
		describe(fmt.Sprintf("5h=%g", used))
	}
	settings.Accounts["a1"] = AccountSettings{Tier: TierMax5x, ManualK: &manualK}
	if _, err := manager.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	describe("manual k")
	return b.String()
}

func TestClaudeResultsMatchThePluginBeforeProviders(t *testing.T) {
	got := claudeRegressionDigest(t)
	if got != claudeRegressionGolden {
		t.Fatalf("Claude digest changed:\n--- got\n%s\n--- want\n%s", got, claudeRegressionGolden)
	}
}

const claudeRegressionGolden = `5h=20 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=20/40 R=28.83470803/129.6600453
pool p R=28.83470803/129.6600453 P=7.679024737/80 k=5.995554391 five=true balance=49.66004525
fixed fixed used=9/20 reserve=7.679024737/80 share=0.1667902474
key fixed allowed=true reason= balance=80/0 reported=80
key remainder allowed=true reason= balance=40/0 reported=40
5h=60 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=60/40 R=14.41735401/129.6600453
pool p R=14.41735401/129.6600453 P=7.679024737/80 k=5.995554391 five=true balance=49.66004525
fixed fixed used=9/20 reserve=7.679024737/80 share=0.1667902474
key fixed allowed=true reason= balance=80/0 reported=80
key remainder allowed=true reason= balance=40/0 reported=40
5h=97 segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=5.995554391/auto cold=false from=estimated T5=40.0482056 T7=240.1111949 u=97/40 R=1.081301551/129.6600453
pool p R=1.081301551/129.6600453 P=7.679024737/80 k=5.995554391 five=true balance=49.66004525
fixed fixed used=9/20 reserve=7.679024737/80 share=0.1667902474
key fixed allowed=true reason= balance=80/0 reported=80
key remainder allowed=false reason=five_hour_reserve balance=40/0 reported=40
manual k segments=10959
account a1 tier=max5x ratio=2.496990777 t5=40.0482056 blocks=97/98 segs=10959/683 k=6.5/manual cold=false from=estimated T5=40.0482056 T7=260.3133364 u=97/40 R=1.081301551/140.5692016
pool p R=1.081301551/140.5692016 P=6.384615385/80 k=6.5 five=true balance=60.56920164
fixed fixed used=9/20 reserve=6.384615385/80 share=0.1538461538
key fixed allowed=true reason= balance=80/0 reported=80
key remainder allowed=false reason=five_hour_reserve balance=40/0 reported=40
`

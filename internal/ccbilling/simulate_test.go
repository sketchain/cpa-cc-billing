package ccbilling

import (
	"testing"
	"time"
)

// replayFixture is four replayed hours of one account with a $1000 week:
// utilization reports of 50%, 60% and 70% at 00:30, 01:30 and 02:30, so
// R = 0.9 × 1000 × (1 − u) is $900, $450, $360 and $270 at the hours' starts.
// The fixed key spends $10, $20 and $30 on model-x; the remainder key $5 an
// hour on the baseline model.
func replayFixture(conversion BasisConversion) ReplayInput {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reset := start.Add(3 * 24 * time.Hour)
	input := ReplayInput{From: start, Now: start.Add(4 * time.Hour), Safety: 0.9, Conversion: conversion,
		Cycles:    []Cycle{{Start: reset.Add(-Week), End: reset, Observed: true}},
		Remainder: []string{"rem"},
		Accounts: []ReplayAccount{{AuthIndex: "a1", WeeklyUSD: 1000, FiveHourUSD: 100, Week: []WindowPoint{
			{At: start.Add(30 * time.Minute), Used: 50, Reset: reset},
			{At: start.Add(90 * time.Minute), Used: 60, Reset: reset},
			{At: start.Add(150 * time.Minute), Used: 70, Reset: reset},
		}}}}
	for hour, usd := range []float64{10, 20, 30} {
		input.Spend = append(input.Spend, SpendRow{Scope: "fix", AuthIndex: "a1", Hour: start.Add(time.Duration(hour) * time.Hour), Model: "model-x", USD: usd})
	}
	for hour := range 4 {
		input.Spend = append(input.Spend, SpendRow{Scope: "rem", AuthIndex: "a1", Hour: start.Add(time.Duration(hour) * time.Hour), Model: "base", USD: 5})
	}
	return input
}

func TestReplayCountsBlockedHoursAndOversoldMomentsByHand(t *testing.T) {
	input := replayFixture(BasisConversion{Basis: BasisRaw})
	start := input.From
	// Baseline: the $400 key. P = 400, 390, 370, 340.
	// 00:00 900 > 400; 01:00 450 > 390; 02:00 360 ≤ 370: refused, $10 short.
	// The refused remainder's $5 is not used, so R at 03:00 is
	// 270 + 0.9 × 5 = 274.5 ≤ 340: refused, $65.5 short.
	base := Replay(input, []ReplayFixed{{Scope: "fix", Weekly: 400}})
	got := base.weeks[0]
	if got.Hours != 4 || got.BlockedHours != 2 || got.BlockedWeekHours != 2 || got.BlockedFiveHourHours != 0 || got.OversoldWeekHours != 2 {
		t.Fatalf("baseline = %+v", got)
	}
	near(t, "lost", got.LostUSD, 10)
	near(t, "shortfall", got.MaxShortfallUSD, 65.5)
	if !got.FirstOversold.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("first oversold = %v", got.FirstOversold)
	}
	if len(base.spans) != 1 || !base.spans[0].From.Equal(start.Add(2*time.Hour)) || !base.spans[0].To.Equal(start.Add(4*time.Hour)) {
		t.Fatalf("spans = %+v", base.spans)
	}
	// The first hour has no report yet.
	if !base.missing {
		t.Fatal("missing reports not reported")
	}

	// Scenario: the key lowered to $300 plus a new $50 key that uses
	// nothing. P = 350, 340, 320, 290; only 03:00 (270 ≤ 290) is refused.
	next := Replay(input, []ReplayFixed{{Scope: "fix", Weekly: 300, Capped: true}, {New: true, Weekly: 50}})
	got = next.weeks[0]
	if got.BlockedHours != 1 || got.OversoldWeekHours != 1 || !got.FirstOversold.Equal(start.Add(3*time.Hour)) {
		t.Fatalf("scenario = %+v", got)
	}
	near(t, "scenario lost", got.LostUSD, 5)
	near(t, "scenario shortfall", got.MaxShortfallUSD, 20)
}

// Lowering the key to $15 caps its use: the cut use no longer counts against
// the pool, on the estimate's basis. A new $380 key reserves without use.
func TestReplayConvertsCutUseWithTheQuotaMultipliers(t *testing.T) {
	keys := []ReplayFixed{{Scope: "fix", Weekly: 15, Capped: true}, {New: true, Weekly: 380}}

	// Without multipliers. 01:00: it used $10, P = 5 + 380 = 385 < 450.
	// At 01:00 it may use only $5 of $20: $15 is cut, R at 02:00 is
	// 360 + 0.9 × 15 = 373.5 ≤ 380, refused ($6.5 short). All $30 of 02:00
	// and the refused remainder's $5 are cut: R at 03:00 is
	// 270 + 0.9 × (15 + 30 + 5) = 315 ≤ 380, refused ($65 short).
	got := Replay(replayFixture(BasisConversion{Basis: BasisRaw}), keys).weeks[0]
	if got.BlockedHours != 2 || got.OversoldWeekHours != 2 {
		t.Fatalf("raw = %+v", got)
	}
	near(t, "raw lost", got.LostUSD, 10)
	near(t, "raw shortfall", got.MaxShortfallUSD, 65)

	// model-x counts twice on the weighted basis: R at 02:00 is
	// 360 + 0.9 × 30 = 387 > 380, admitted; at 03:00 270 + 0.9 × (30 + 60)
	// = 351 ≤ 380, refused ($29 short).
	got = Replay(replayFixture(BasisConversion{Basis: BasisWeighted, Factors: map[string]float64{"model-x": 2}}), keys).weeks[0]
	if got.BlockedHours != 1 || got.OversoldWeekHours != 1 {
		t.Fatalf("weighted = %+v", got)
	}
	near(t, "weighted lost", got.LostUSD, 5)
	near(t, "weighted shortfall", got.MaxShortfallUSD, 29)
}

// The 5-hour reservation: the key's share 0.2 of $400 is $80, less its use in
// the anchor's 5-hour window, against R_5h = 0.9 × 100 × (1 − u).
func TestReplayFiveHourReservation(t *testing.T) {
	input := replayFixture(BasisConversion{Basis: BasisRaw})
	start := input.From
	input.FiveHour = true
	account := &input.Accounts[0]
	// A week too large to refuse; the 5-hour window opened at 00:00.
	account.WeeklyUSD = 100000
	account.FiveHourTrack = []WindowPoint{}
	for i, point := range account.Week {
		account.FiveHourTrack = append(account.FiveHourTrack, WindowPoint{At: point.At, Used: point.Used, Reset: start.Add(FiveHours)})
		account.Week[i].Used = 0
	}
	share := 0.2
	// 00:00 R 90 > 80. 01:00 R 45 ≤ 80 − 10: refused. 02:00 R 36 ≤ 80 − 30:
	// refused. 03:00 R = 27 + 0.9 × 10 = 36 > 80 − 60.
	got := Replay(input, []ReplayFixed{{Scope: "fix", Weekly: 400, Share: &share}}).weeks[0]
	if got.BlockedHours != 2 || got.BlockedFiveHourHours != 2 || got.BlockedWeekHours != 0 || got.OversoldFiveHours != 2 || got.OversoldWeekHours != 0 {
		t.Fatalf("five hour = %+v", got)
	}
	near(t, "five hour shortfall", got.MaxShortfallUSD, 25)
}

import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

// cc-trend.js only declares functions and constants at load time; the stubs
// stand in for the page helpers its tooltip text uses.
const source = fs.readFileSync(new URL("../internal/plugin/web/cc-trend.js", import.meta.url), "utf8");
const context = vm.createContext({
  m: (key, params) => key + (params ? JSON.stringify(params) : ""),
  ccNumber: (value) => String(Math.round(value * 100) / 100),
  ccUSD: (value) => "$" + value.toFixed(2),
  locale: () => "en-US",
  Intl,
  Date
});
vm.runInContext(source, context);
const { ccTrendDays, ccTrendRange, ccTrendEnough, ccDayKey, ccTrendTooltipLines } = context;

const hours = (offset) => () => offset * 60;
const bucket = (at, gain5h, gain7d) => ({ at, gain_5h: gain5h, gain_7d: gain7d });
// One server-side hour of segments: kept ones' count, cost and gain, plus the excluded count.
const hour = (at, cost, gain, segments = 1, excluded = 0) => ({ at, cost_usd: cost, gain, segments, excluded });
const trend = (fields) => ({ k_min_weekly_points: 10, exclusion: "estimate", buckets: [], segment_hours: [], ...fields });
const byKey = (days) => Object.fromEntries(days.map((day) => [day.key, day]));
const close = (actual, expected) => assert.ok(Math.abs(actual - expected) < 1e-9, `${actual} != ${expected}`);

test("hourly buckets split at local midnight for each time zone offset", () => {
  const data = trend({ buckets: [bucket("2026-10-01T15:00:00Z", 8, 1), bucket("2026-10-01T16:00:00Z", 16, 2), bucket("2026-10-02T04:00:00Z", 4, 1)] });
  const now = Date.parse("2026-10-02T12:00:00Z");
  // UTC+8: 23:00 on the 1st, then 00:00 and 12:00 on the 2nd.
  let days = byKey(ccTrendDays(data, { now, offset: hours(8) }));
  assert.deepEqual(Object.keys(days), ["2026-10-01", "2026-10-02"]);
  assert.equal(days["2026-10-01"].gain5h, 8);
  assert.equal(days["2026-10-02"].gain5h, 20);
  // UTC: the first two on the 1st.
  days = byKey(ccTrendDays(data, { now, offset: hours(0) }));
  assert.equal(days["2026-10-01"].gain5h, 24);
  assert.equal(days["2026-10-02"].gain5h, 4);
  // UTC−5: 04:00Z on the 2nd is 23:00 on the 1st.
  days = byKey(ccTrendDays(data, { now, offset: hours(-5) }));
  assert.equal(days["2026-10-01"].gain5h, 28);
  assert.equal(days["2026-10-02"].gain5h, 0);
  assert.equal(days["2026-10-02"].hasData, false);
  // UTC+5:30: 15:00Z is 20:30, 16:00Z is 21:30, both on the 1st.
  days = byKey(ccTrendDays(data, { now, offset: hours(5.5) }));
  assert.equal(days["2026-10-01"].gain5h, 24);
  // Segments follow the same days.
  days = byKey(ccTrendDays(trend({ segment_hours: [hour("2026-10-01T15:00:00Z", 1, 1), hour("2026-10-01T16:00:00Z", 2, 1)] }),
    { now, offset: hours(8) }));
  assert.equal(days["2026-10-01"].cost, 1);
  assert.equal(days["2026-10-02"].cost, 2);
});

test("the offset is taken at each instant, so daylight saving changes move midnight", () => {
  // New York: EDT (−4) until 2026-11-01 06:00Z, EST (−5) after.
  const offset = (ms) => (ms < Date.parse("2026-11-01T06:00:00Z") ? -240 : -300);
  const data = trend({ buckets: [bucket("2026-11-01T03:00:00Z", 8, 1), bucket("2026-11-02T04:00:00Z", 8, 1), bucket("2026-11-02T05:00:00Z", 8, 1)] });
  const days = byKey(ccTrendDays(data, { now: Date.parse("2026-11-02T12:00:00Z"), offset }));
  assert.equal(days["2026-10-31"].gain5h, 8);
  assert.equal(days["2026-11-01"].gain5h, 8);
  assert.equal(days["2026-11-02"].gain5h, 8);
  assert.equal(ccDayKey(Date.parse("2026-11-01T04:30:00Z"), offset), "2026-11-01");
});

test("the browser's own time zone is the default", () => {
  const saved = process.env.TZ;
  try {
    process.env.TZ = "Asia/Shanghai";
    const days = ccTrendDays(trend({ buckets: [bucket("2026-10-01T15:00:00Z", 8, 1), bucket("2026-10-01T16:00:00Z", 8, 1)] }),
      { now: Date.parse("2026-10-01T17:00:00Z") });
    assert.deepEqual([...days.map((day) => day.key)], ["2026-10-01", "2026-10-02"]);
  } finally {
    if (saved === undefined) delete process.env.TZ; else process.env.TZ = saved;
  }
});

test("a day without 7-day gain has no k, and empty days run up to today", () => {
  const data = trend({ buckets: [bucket("2026-10-01T10:00:00Z", 80, 10), bucket("2026-10-02T10:00:00Z", 6, 0), bucket("2026-10-04T10:00:00Z", 90, 10)] });
  const days = ccTrendDays(data, { now: Date.parse("2026-10-06T10:00:00Z"), offset: hours(0) });
  assert.deepEqual([...days.map((day) => day.key)], ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06"]);
  assert.equal(days[0].k, 8);
  assert.equal(days[1].k, null);
  assert.equal(days[1].lowPrecision, false);
  assert.equal(days[2].k, null);
  assert.equal(days[2].hasData, false);
  // The rolling window still sums the zero day's 5-hour gain.
  close(days[3].rollingK, (80 + 6 + 90) / 20);
  assert.equal(ccTrendDays(trend({}), { now: Date.now() }).length, 0);
});

test("the rolling k uses fewer days at the start and seven after", () => {
  const buckets = [];
  for (let day = 1; day <= 9; day++) buckets.push(bucket(`2026-10-0${day}T10:00:00Z`, day * 10, 10));
  const days = ccTrendDays(trend({ buckets }), { now: Date.parse("2026-10-09T12:00:00Z"), offset: hours(0) });
  assert.deepEqual([...days.map((day) => day.rollingDays)], [1, 2, 3, 4, 5, 6, 7, 7, 7]);
  close(days[0].rollingK, 1);
  close(days[2].rollingK, (10 + 20 + 30) / 30);
  // Day 8 drops day 1: (20 + … + 80) / 70.
  close(days[7].rollingK, 350 / 70);
});

test("days below the k minimum are imprecise and project with the rolling k", () => {
  const data = trend({
    buckets: [bucket("2026-10-01T10:00:00Z", 100, 10), bucket("2026-10-02T10:00:00Z", 20, 4)],
    segment_hours: [hour("2026-10-01T11:00:00Z", 0.5, 1), hour("2026-10-02T11:00:00Z", 0.25, 1), hour("2026-10-02T12:00:00Z", 0.25, 1)]
  });
  const [first, second] = ccTrendDays(data, { now: Date.parse("2026-10-02T13:00:00Z"), offset: hours(0) });
  assert.equal(first.lowPrecision, false);
  assert.equal(first.weeklyBasis, "daily");
  close(first.weekly, 10 * 0.5 * 100);
  assert.equal(second.lowPrecision, true);
  assert.equal(second.k, 5);
  assert.equal(second.weeklyBasis, "rolling");
  close(second.rollingK, 120 / 14);
  close(second.usdPerPoint, 0.25);
  close(second.weekly, (120 / 14) * 0.25 * 100);
  const lines = ccTrendTooltipLines(second, data);
  assert.ok(lines.some((line) => line.startsWith("cc.trend_tip_low_precision")) || lines.some((line) => line.includes("cc.trend_tip_low_precision")));
  assert.ok(lines.at(-1).startsWith("cc.trend_tip_weekly_rolling"));
  // A minimum of exactly the day's gain is precise.
  const exact = ccTrendDays({ ...data, k_min_weekly_points: 4 }, { now: Date.parse("2026-10-02T13:00:00Z"), offset: hours(0) })[1];
  assert.equal(exact.lowPrecision, false);
  assert.equal(exact.weeklyBasis, "daily");
});

test("$/1% uses the kept segments the server summed and counts the excluded ones", () => {
  const options = { now: Date.parse("2026-10-01T12:00:00Z"), offset: hours(0) };
  // Estimate mode: 0.01 USD of excluded cost is already left out of the hour.
  const estimate = trend({ segment_hours: [hour("2026-10-01T10:00:00Z", 0.8, 2, 2, 1), hour("2026-10-01T11:00:00Z", 0.4, 1, 1, 0)],
    buckets: [bucket("2026-10-01T10:00:00Z", 16, 2)] });
  const [filtered] = ccTrendDays(estimate, options);
  assert.equal(filtered.segments, 3);
  assert.equal(filtered.excluded, 1);
  close(filtered.usdPerPoint, 0.4);
  assert.ok(ccTrendTooltipLines(filtered, estimate).some((line) => line.startsWith("cc.trend_tip_segments_filtered")));
  // Raw mode: every segment is kept.
  const raw = trend({ exclusion: "raw", segment_hours: [hour("2026-10-01T10:00:00Z", 0.81, 3, 3, 0)] });
  const [unfiltered] = ccTrendDays(raw, options);
  assert.equal(unfiltered.segments, 3);
  close(unfiltered.usdPerPoint, 0.27);
  assert.ok(ccTrendTooltipLines(unfiltered, raw).some((line) => line.startsWith("cc.trend_tip_segments{")));
  // An hour whose segments were all excluded has no $/1%.
  const [excludedOnly] = ccTrendDays(trend({ segment_hours: [hour("2026-10-01T10:00:00Z", 0, 0, 0, 4)], buckets: [bucket("2026-10-01T10:00:00Z", 80, 10)] }), options);
  assert.equal(excludedOnly.usdPerPoint, null);
  assert.equal(excludedOnly.excluded, 4);
  // Without segments a day has no $/1% and no projection.
  const [bare] = ccTrendDays(trend({ buckets: [bucket("2026-10-01T10:00:00Z", 80, 10)] }), options);
  assert.equal(bare.usdPerPoint, null);
  assert.equal(bare.weekly, null);
  assert.equal(bare.weeklyBasis, null);
});

test("ranges end today and too little data is reported", () => {
  const buckets = [bucket("2026-09-01T10:00:00Z", 80, 10), bucket("2026-10-20T10:00:00Z", 80, 10)];
  const days = ccTrendDays(trend({ buckets }), { now: Date.parse("2026-10-25T10:00:00Z"), offset: hours(0) });
  assert.equal(days.length, 55);
  const week = ccTrendRange(days, "7");
  assert.deepEqual([week[0].key, week.at(-1).key, week.length], ["2026-10-19", "2026-10-25", 7]);
  assert.equal(ccTrendRange(days, "30").length, 30);
  assert.equal(ccTrendRange(days, "all").length, 55);
  assert.equal(ccTrendEnough(days), true);
  assert.equal(ccTrendEnough(week), false);
  assert.equal(ccTrendEnough([]), false);
});

test("7-day window accounts take the weekly total from their segments without k", () => {
  const data = trend({ segment_window: "7d", segment_hours: [hour("2026-10-01T10:00:00Z", 1.2, 2), hour("2026-10-02T10:00:00Z", 0.6, 1)] });
  const days = byKey(ccTrendDays(data, { now: Date.parse("2026-10-02T12:00:00Z"), offset: hours(0) }));
  close(days["2026-10-01"].weekly, 60);
  assert.equal(days["2026-10-01"].weeklyBasis, "window");
  assert.ok(ccTrendTooltipLines(days["2026-10-02"], data).some((line) => line.startsWith("cc.trend_tip_weekly_window")));
});

test("accounts that need a k draw no weekly total", () => {
  const data = trend({ needs_k: true, buckets: [bucket("2026-10-01T10:00:00Z", 60, 10)], segment_hours: [hour("2026-10-01T10:00:00Z", 1, 2)] });
  const [day] = ccTrendDays(data, { now: Date.parse("2026-10-01T12:00:00Z"), offset: hours(0) });
  assert.equal(day.weekly, null);
  assert.equal(day.weeklyBasis, null);
});

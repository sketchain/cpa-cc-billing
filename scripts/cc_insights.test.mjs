import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

// cc-insights.js reuses cc-trend.js's day helpers; both only declare
// functions and constants at load time apart from reading saved windows.
const read = (name) => fs.readFileSync(new URL("../internal/plugin/web/" + name, import.meta.url), "utf8");
const context = vm.createContext({
  m: (key, params) => key + (params ? JSON.stringify(params) : ""),
  ccNumber: (value) => String(Math.round(value * 100) / 100),
  ccUSD: (value) => "$" + value.toFixed(2),
  locale: () => "en-US",
  localStorage: { getItem: () => null, setItem: () => {} },
  Intl,
  Date
});
vm.runInContext(read("cc-trend.js") + "\n" + read("cc-insights.js"), context);
const { ccUsageDays, ccUsageCycles, ccTrendRange } = context;

const hours = (offset) => () => offset * 60;
const at = (iso) => Date.parse(iso) / 1000;
const byKey = (days) => Object.fromEntries(days.map((day) => [day.key, day]));

test("key spend and refusals split at local midnight and fill empty days to today", () => {
  const keys = [
    { scope: "fixed", kind: "fixed", hours: [[at("2026-10-01T15:00:00Z"), 2], [at("2026-10-01T16:00:00Z"), 3]], blocks: [] },
    {
      scope: "rem",
      kind: "remainder",
      hours: [[at("2026-10-03T01:00:00Z"), 1.5]],
      blocks: [
        { at: "2026-10-01T16:00:00Z", reason: "weekly_reserve", count: 4 },
        { at: "2026-10-01T16:00:00Z", reason: "five_hour_reserve", count: 2 },
        { at: "2026-10-03T01:00:00Z", reason: "weekly_reserve", count: 1 }
      ]
    }
  ];
  const now = Date.parse("2026-10-04T12:00:00Z");
  // UTC+8: 23:00 on the 1st, then 00:00 on the 2nd.
  let days = byKey(ccUsageDays(keys, { now, offset: hours(8) }));
  assert.deepEqual(Object.keys(days), ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"]);
  assert.equal(days["2026-10-01"].spend.fixed, 2);
  assert.equal(days["2026-10-02"].spend.fixed, 3);
  assert.equal(days["2026-10-02"].blocks.rem, 6);
  assert.deepEqual({ ...days["2026-10-02"].reasons.rem }, { weekly_reserve: 4, five_hour_reserve: 2 });
  assert.equal(days["2026-10-03"].total, 1.5);
  assert.equal(days["2026-10-04"].total, 0);
  // UTC−5: 01:00Z on the 3rd is the evening of the 2nd.
  days = byKey(ccUsageDays(keys, { now, offset: hours(-5) }));
  assert.equal(days["2026-10-01"].spend.fixed, 5);
  assert.equal(days["2026-10-02"].spend.rem, 1.5);
  assert.equal(days["2026-10-02"].blocks.rem, 1);
  assert.equal(ccUsageDays([], { now }).length, 0);
});

test("ranges keep the last days and the cycles ending within them", () => {
  const keys = [{ scope: "fixed", kind: "fixed", hours: [[at("2026-09-01T12:00:00Z"), 1]], blocks: [] }];
  const days = ccTrendRange(ccUsageDays(keys, { now: Date.parse("2026-10-10T12:00:00Z"), offset: hours(0) }), "7");
  assert.deepEqual([days[0].key, days.at(-1).key], ["2026-10-04", "2026-10-10"]);
  const cycles = [
    { start: "2026-09-20T00:00:00Z", end: "2026-09-27T00:00:00Z" },
    { start: "2026-09-27T00:00:00Z", end: "2026-10-04T00:00:00Z" },
    { start: "2026-10-04T00:00:00Z", end: "2026-10-11T00:00:00Z" }
  ];
  // A cycle ending at midnight ends on the day before.
  assert.deepEqual(ccUsageCycles(cycles, days, "7", hours(0)).map((cycle) => cycle.start), ["2026-10-04T00:00:00Z"]);
  assert.equal(ccUsageCycles(cycles, days, "all", hours(0)).length, 3);
});

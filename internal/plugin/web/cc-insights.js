// CC insights: each pool's and account's consumption forecast, and each pooled
// key's daily usage, fixed key cycles and refusals. The day functions are pure
// and read no page state, so scripts/cc_insights.test.mjs runs them alone.
const CC_FORECAST_STORAGE = "cc-forecast-windows";
const CC_FORECAST_LIMITS = { window5h: [5, 300, 60], window7d: [1, 168, 24] };
const ccForecastState = { window5h: 60, window7d: 24 };
const ccUsageState = { range: "30", charts: [] };

try {
  const saved = JSON.parse(localStorage.getItem(CC_FORECAST_STORAGE) || "{}");
  for (const [name, [low, high]] of Object.entries(CC_FORECAST_LIMITS))
    if (Number.isInteger(saved[name]) && saved[name] >= low && saved[name] <= high) ccForecastState[name] = saved[name];
} catch (_) {}

function ccForecastKey() { return JSON.stringify([ccForecastState.window5h, ccForecastState.window7d]); }

function readCCForecast(reload) {
  const data = ccQuery.value;
  if (!data?.cc?.loaded || !data.cc.settings.pools.length) return Promise.resolve(null);
  const query = new URLSearchParams({ window_5h_minutes: ccForecastState.window5h, window_7d_hours: ccForecastState.window7d });
  return ccForecastQuery.read(ccForecastKey(), () => plugin("GET", "/cc/forecast?" + query).then((response) => response.forecast), reload);
}

function readCCUsage(reload) {
  const data = ccQuery.value;
  if (!data?.cc?.loaded || !data.cc.settings.pools.length) return Promise.resolve(null);
  return ccUsageQuery.read("usage", () => plugin("GET", "/cc/key-usage").then((response) => response.usage), reload);
}

// The pools of the provider on display.
function ccShownPools(data) { return data.cc.settings.pools.filter((pool) => ccPoolProvider(pool) === ccState.provider); }

function ccRate(value, digits = 2) { return Number.isFinite(value) ? ccNumber(value, digits) : "—"; }

function ccHoursText(seconds) { return ccNumber(seconds / 3600, 2); }

// The moment a forecast reaches 0, as "in 3 hours" with the clock time.
function ccOutcomeLines(outcome) {
  switch (outcome.status) {
    case "eta": {
      const at = new Date(outcome.at);
      return [compactTimeUntil(Math.max(0, (at - Date.now()) / 1000)), compactResetDate(at)];
    }
    case "after_reset":
      return [m("cc.forecast_after_reset"), null];
    case "idle":
      return [m("cc.forecast_idle"), null];
    case "reached":
      return [m("cc.forecast_reached"), null];
  }
  return ["—", null];
}

function ccOutcomeText(outcome) {
  const [text, at] = ccOutcomeLines(outcome);
  return at ? text + DISPLAY_SEPARATOR + at : text;
}

// The conversion of key cost to the estimate's basis, for tooltips.
function ccConversionLine(conversion) {
  if (conversion.basis !== "weighted") return m("cc.forecast_tip_basis_raw");
  const factors = Object.entries(conversion.factors || {}).map(([model, factor]) => model + " ×" + ccNumber(factor));
  let line = m("cc.forecast_tip_basis_weighted", { v0: factors.length ? factors.join(", ") : "—" });
  if (conversion.priced_models?.length) line += "\n" + m("cc.forecast_tip_basis_priced", { v0: conversion.priced_models.join(", ") });
  return line;
}

function ccForecastCell(main, detail, tooltip, status) {
  return el(
    "td",
    {},
    el(
      "div",
      { class: "cc-forecast-cell", "data-tip": tooltip || null, tabindex: tooltip ? "0" : null },
      el("div", { class: "cc-forecast-outcome cc-outcome-" + (status || "unknown"), text: main }),
      detail ? el("div", { class: "muted small", text: detail }) : null
    )
  );
}

function ccAccountWindowCell(forecast, item, label, span) {
  const [main, at] = ccOutcomeLines(item.outcome);
  const rate = item.source === "reading" ? item.reading.per_hour : item.source === "spend" ? item.spend.per_hour : null;
  const detail = [ccPercent(item.used), rate === null ? null : m("cc.forecast_points_per_hour", { v0: ccRate(rate) }), at].filter(Boolean);
  const lines = [
    m("cc.forecast_tip_used", { v0: label, v1: ccPercent(item.used), v2: ccWhen(item.reset_at) }),
    item.reading.available
      ? m("cc.forecast_tip_reading", {
          v0: ccPercent(item.reading.start_used),
          v1: ccPercent(item.reading.end_used),
          v2: ccWhen(item.reading.from),
          v3: ccWhen(item.reading.to),
          v4: ccRate(item.reading.per_hour)
        }) +
        DISPLAY_SEPARATOR +
        ccOutcomeText(item.reading.outcome)
      : m("cc.forecast_tip_reading_none", { v0: span }),
    item.spend.available
      ? m("cc.forecast_tip_spend", {
          v0: span,
          v1: ccUSD(item.spend.recorded_usd),
          v2: ccUSD(item.spend.basis_usd),
          v3: ccUSD(item.spend.usd_per_point),
          v4: ccRate(item.spend.per_hour)
        }) +
        DISPLAY_SEPARATOR +
        ccOutcomeText(item.spend.outcome)
      : m("cc.forecast_tip_spend_none"),
    ccConversionLine(forecast.conversion),
    m("cc.forecast_tip_source_" + item.source),
    m("cc.forecast_tip_algo_account")
  ];
  return ccForecastCell(main, detail.join(DISPLAY_SEPARATOR), lines.join("\n"), item.outcome.status);
}

function ccPoolWindowLines(forecast, item, label, span) {
  const lines = [
    m("cc.forecast_tip_pool", { v0: label, v1: ccUSD(item.remaining_usd), v2: ccUSD(item.reserved_usd), v3: ccWhen(item.reset_at) }),
    item.reading.available
      ? m("cc.forecast_tip_pool_reading", { v0: span, v1: ccUSD(item.reading.per_hour) }) +
        (item.reading_partial ? " " + m("cc.forecast_tip_pool_partial") : "")
      : m("cc.forecast_tip_reading_none", { v0: span }),
    m("cc.forecast_tip_pool_spend", {
      v0: span,
      v1: ccUSD(item.spend.recorded_usd),
      v2: ccUSD(item.spend.basis_usd),
      v3: ccNumber(forecast.safety_factor * 100) + "%",
      v4: ccUSD(item.spend.per_hour)
    }),
    ccConversionLine(forecast.conversion)
  ];
  return lines;
}

function ccPoolExhaustCell(forecast, item, label, span) {
  const [main, at] = ccOutcomeLines(item.exhaust);
  const rate = item.source === "reading" ? item.reading.per_hour : item.spend.per_hour;
  const lines = ccPoolWindowLines(forecast, item, label, span);
  lines.push(
    m("cc.forecast_tip_outcomes", { v0: ccOutcomeText(item.reading.outcome), v1: ccOutcomeText(item.spend.outcome) }),
    m("cc.forecast_tip_source_" + item.source),
    m("cc.forecast_tip_algo_pool")
  );
  const detail = [ccUSD(item.remaining_usd), m("cc.forecast_usd_per_hour", { v0: ccUSD(rate) }), at].filter(Boolean);
  return ccForecastCell(main, detail.join(DISPLAY_SEPARATOR), lines.join("\n"), item.exhaust.status);
}

function ccPoolBlockCell(forecast, item, label, span) {
  if (!item.enabled) return ccForecastCell("—", null, m("cc.forecast_tip_five_hour_disabled"), "unknown");
  const [main, at] = ccOutcomeLines(item.block);
  const lines = ccPoolWindowLines(forecast, item, label, span);
  lines.push(
    m("cc.forecast_tip_fixed", { v0: span, v1: ccUSD(item.fixed_usd), v2: ccUSD(item.fixed_per_hour) }),
    m("cc.forecast_tip_outcomes", { v0: ccOutcomeText(item.reading.block), v1: ccOutcomeText(item.spend.block) }),
    m("cc.forecast_tip_source_" + item.source),
    m("cc.forecast_tip_algo_block")
  );
  const detail = [m("cc.forecast_headroom", { v0: ccUSD(item.headroom_usd) }), at].filter(Boolean);
  return ccForecastCell(
    main,
    detail.join(DISPLAY_SEPARATOR),
    lines.join("\n"),
    item.block.status === "reached" ? "blocked" : item.block.status
  );
}

function ccForecastWindowInput(name, label) {
  const [low, high] = CC_FORECAST_LIMITS[name];
  return el(
    "label",
    { class: "cc-forecast-window" },
    el("span", { text: label }),
    el("input", {
      type: "number",
      class: "cc-number cc-forecast-input",
      min: String(low),
      max: String(high),
      step: "1",
      value: String(ccForecastState[name]),
      onchange: (event) => {
        const value = Number(event.target.value);
        if (!Number.isInteger(value) || value < low || value > high) {
          event.target.value = String(ccForecastState[name]);
          notify(m("cc.forecast_window_invalid", { v0: low, v1: high }), "err");
          return;
        }
        ccForecastState[name] = value;
        try { localStorage.setItem(CC_FORECAST_STORAGE, JSON.stringify(ccForecastState)); } catch (_) {}
        guard(() => readCCForecast(true));
      }
    })
  );
}

function renderCCForecast() {
  const target = $("cc-forecast");
  if (!target) return;
  const data = ccQuery.value;
  if (!data) {
    target.replaceChildren();
    return;
  }
  const head = el(
    "div",
    { class: "cc-trend-toolbar" },
    el("h3", { text: m("cc.forecast_title") }),
    el(
      "div",
      { class: "cc-forecast-windows" },
      ccForecastWindowInput("window5h", m("cc.forecast_window_5h")),
      ccForecastWindowInput("window7d", m("cc.forecast_window_7d"))
    )
  );
  const hint = el("p", { class: "muted small", text: m("cc.forecast_hint") });
  const status = (text) => target.replaceChildren(head, hint, el("div", { class: "empty", text }));
  if (!ccShownPools(data).length) return status(m("cc.forecast_no_pools"));
  if (!data.cc.loaded) return status(m("cc.not_loaded"));
  const forecast = ccForecastQuery.key === ccForecastKey() ? ccForecastQuery.value : null;
  if (!forecast) return status(ccForecastQuery.error ? m("cc.forecast_load_failed", { v0: ccForecastQuery.error }) : m("ui.loading"));
  const span5h = m("cc.forecast_span_minutes", { v0: ccNumber(forecast.window_5h_seconds / 60, 0) });
  const span7d = m("cc.forecast_span_hours", { v0: ccNumber(forecast.window_7d_seconds / 3600, 0) });
  const rows = [];
  for (const pool of forecast.pools.filter((item) => item.accounts.length && (item.provider || "claude") === ccState.provider)) {
    // Each pool's spend is on its provider's basis and safety factor.
    const context = { ...forecast, conversion: pool.conversion, safety_factor: pool.safety_factor };
    rows.push(
      el(
        "tr",
        { class: "cc-forecast-pool" },
        el("td", {}, el("strong", { text: pool.name || pool.id }), el("div", { class: "muted small", text: m("cc.forecast_pool_row") })),
        ccPoolExhaustCell(context, pool.five_hour, m("cc.forecast_label_5h"), span5h),
        ccPoolExhaustCell(context, pool.week, m("cc.forecast_label_7d"), span7d),
        ccPoolBlockCell(context, pool.week, m("cc.forecast_label_7d"), span7d),
        ccPoolBlockCell(context, pool.five_hour, m("cc.forecast_label_5h"), span5h)
      )
    );
    for (const account of pool.accounts)
      rows.push(
        el(
          "tr",
          {},
          el("td", { class: "cc-forecast-account" }, el("div", { text: ccCredentialName(data, account.auth_index) })),
          ccAccountWindowCell(context, account.five_hour, m("cc.forecast_label_5h"), span5h),
          ccAccountWindowCell(context, account.week, m("cc.forecast_label_7d"), span7d),
          el("td", { class: "muted", text: "—" }),
          el("td", { class: "muted", text: "—" })
        )
      );
  }
  target.replaceChildren(
    head,
    hint,
    ccTable(
      [m("cc.forecast_target"), m("cc.forecast_5h_full"), m("cc.forecast_7d_full"), m("cc.forecast_block_week"), m("cc.forecast_block_5h")],
      rows,
      m("cc.forecast_no_pools")
    )
  );
}

// Daily sums of pooled keys' hourly cost and refusals from the first day with
// data to today, split at the viewer's midnight. Each day carries spend and
// blocks by scope, refusal reasons by scope and the day's total cost.
function ccUsageDays(keys, { now = Date.now(), offset = ccLocalOffset } = {}) {
  const sums = new Map();
  const day = (ms) => {
    const key = ccDayKey(ms, offset);
    if (!sums.has(key)) sums.set(key, { spend: {}, blocks: {}, reasons: {}, total: 0 });
    return sums.get(key);
  };
  for (const key of keys) {
    for (const [seconds, usd] of key.hours || []) {
      const sum = day(seconds * 1000);
      sum.spend[key.scope] = (sum.spend[key.scope] || 0) + usd;
      sum.total += usd;
    }
    for (const block of key.blocks || []) {
      const sum = day(Date.parse(block.at));
      sum.blocks[key.scope] = (sum.blocks[key.scope] || 0) + block.count;
      sum.reasons[key.scope] ||= {};
      sum.reasons[key.scope][block.reason] = (sum.reasons[key.scope][block.reason] || 0) + block.count;
    }
  }
  if (!sums.size) return [];
  const keysSorted = [...sums.keys()].sort();
  const today = ccDayKey(now, offset);
  const last = today > keysSorted.at(-1) ? today : keysSorted.at(-1);
  const days = [];
  for (let key = keysSorted[0]; key <= last; key = ccShiftDay(key, 1))
    days.push({ key, ...(sums.get(key) || { spend: {}, blocks: {}, reasons: {}, total: 0 }) });
  return days;
}

// The cycles that end within the shown days; all of them for "all".
function ccUsageCycles(cycles, days, range, offset = ccLocalOffset) {
  if (range === "all" || !days.length) return cycles;
  return cycles.filter((cycle) => ccDayKey(Date.parse(cycle.end) - 1, offset) >= days[0].key);
}

function ccKeyName(key) {
  if (key.label) return key.label + (key.preview ? " (" + key.preview + ")" : "");
  return key.preview || key.scope.slice(0, 8) + "…";
}

function ccDestroyUsageCharts() {
  for (const chart of ccUsageState.charts) chart.destroy();
  ccUsageState.charts = [];
}

function ccDrawUsageChart(panel, config) {
  const canvas = panel.querySelector("canvas");
  const status = panel.querySelector(".empty");
  const message = typeof Chart !== "function" ? chartError || m("ui.loading_charts") : "";
  canvas.classList.toggle("hidden", !!message);
  status.classList.toggle("hidden", !message);
  setText(status, message);
  if (!message) ccUsageState.charts.push(new Chart(canvas, window.billingI18n.chartValue(config)));
}

function ccFaded(color, share = 35) {
  return "color-mix(in srgb, " + color + " " + share + "%, " + themeColor("--analysis-segment-border", "#f0eee8") + ")";
}

// Refusal lines stand apart from the spend bars, starting from the palette's red.
function ccRefusalColor(colors, index) { return colors[(4 + index) % colors.length]; }

function ccRefusedKeys(days, keys) { return keys.filter((key) => key.kind === "remainder" && days.some((day) => day.blocks[key.scope])); }

function ccUsageDailyConfig(days, keys, colors, width) {
  const axis = chartAxisColors();
  const remainder = ccRefusedKeys(days, keys);
  const datasets = keys.map((key, index) => ({
    type: "bar",
    label: ccKeyName(key),
    data: days.map((day) => day.spend[key.scope] || 0),
    yAxisID: "usd",
    stack: "spend",
    order: 1,
    backgroundColor: colors[index % colors.length],
    borderWidth: 0,
    maxBarThickness: 22
  }));
  for (const key of remainder) {
    const color = ccRefusalColor(colors, remainder.indexOf(key));
    datasets.push({
      type: "line",
      label: m("cc.usage_blocks_of", { v0: ccKeyName(key) }),
      data: days.map((day) => day.blocks[key.scope] || 0),
      yAxisID: "blocks",
      borderColor: color,
      backgroundColor: color,
      borderDash: [5, 4],
      borderWidth: 2,
      // Drawn over the bars; days without refusals have no point.
      order: 0,
      pointRadius: days.map((day) => (day.blocks[key.scope] ? 3 : 0)),
      pointHoverRadius: 4,
      scope: key.scope
    });
  }
  const scales = {
    x: { ...timeAxis(axis, width), stacked: true },
    usd: {
      stacked: true,
      beginAtZero: true,
      grid: { color: axis.grid },
      border: { color: axis.grid },
      ticks: { color: axis.text, maxTicksLimit: 5, callback: (value) => "$" + ccNumber(value, 2) }
    }
  };
  if (remainder.length)
    scales.blocks = {
      beginAtZero: true,
      position: "right",
      grid: { drawOnChartArea: false },
      ticks: { color: axis.text, maxTicksLimit: 5, precision: 0 }
    };
  return {
    type: "bar",
    data: { labels: days.map((day) => ccTrendDayLabel(day.key)), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      onResize: (chart, size) => { chart.options.scales.x.ticks.maxTicksLimit = timeTickLimit(size.width); },
      interaction: { mode: "index", intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: chartTooltip({
          callbacks: {
            title: (items) => (items.length ? ccTrendDayLabel(days[items[0].dataIndex].key, { dateStyle: "medium" }) : ""),
            label: (item) => {
              const day = days[item.dataIndex];
              if (item.dataset.yAxisID !== "blocks") return item.dataset.label + ": " + ccUSD(item.raw);
              const reasons = Object.entries(day.reasons[item.dataset.scope] || {}).map(
                ([reason, count]) => m("cc.refusal_reason_" + reason) + " " + count
              );
              return (
                item.dataset.label + ": " + m("cc.usage_block_count", { v0: item.raw }) +
                (reasons.length ? " (" + reasons.join(", ") + ")" : "")
              );
            },
            footer: (items) => (items.length ? m("cc.usage_day_total", { v0: ccUSD(days[items[0].dataIndex].total) }) : "")
          }
        })
      },
      scales
    }
  };
}

function ccCycleLabel(cycle) {
  const format = (ms) => ccTrendDayLabel(ccDayKey(ms));
  return format(Date.parse(cycle.start)) + "–" + format(Date.parse(cycle.end) - 1);
}

function ccUsageCycleConfig(keys, colors, width) {
  // Cycles share their start across keys of one pool: one category each.
  const starts = [...new Set(keys.flatMap((key) => key.shown.map((cycle) => cycle.start)))].sort();
  const sample = new Map(keys.flatMap((key) => key.shown.map((cycle) => [cycle.start, cycle])));
  const axis = chartAxisColors();
  const datasets = [];
  keys.forEach((key) => {
    const color = colors[key.color % colors.length];
    const byStart = new Map(key.shown.map((cycle) => [cycle.start, cycle]));
    const series = (label, value, background, extra = {}) => ({
      type: "bar",
      label,
      data: starts.map((start) => (byStart.has(start) ? value(byStart.get(start)) : null)),
      stack: key.scope,
      backgroundColor: background,
      borderColor: color,
      borderWidth: 0,
      maxBarThickness: 26,
      cycleKey: key,
      ...extra
    });
    datasets.push(
      series(m("cc.usage_cycle_used_of", { v0: ccKeyName(key) }), (cycle) => cycle.used_usd, color),
      series(m("cc.usage_cycle_forfeited_of", { v0: ccKeyName(key) }), (cycle) => cycle.forfeited_usd, ccFaded(color, 30)),
      series(m("cc.usage_cycle_remaining_of", { v0: ccKeyName(key) }), (cycle) => cycle.remaining_usd, "transparent", {
        borderWidth: 1.5,
        borderDash: [4, 3]
      })
    );
  });
  return {
    type: "bar",
    data: { labels: starts.map((start) => ccCycleLabel(sample.get(start))), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      interaction: { mode: "index", intersect: false },
      plugins: {
        legend: { display: false },
        tooltip: chartTooltip({
          filter: (item) => item.raw !== null && item.raw !== undefined && (item.raw > 0 || item.datasetIndex % 3 === 0),
          callbacks: {
            title: (items) => {
              if (!items.length) return "";
              const cycle = sample.get(starts[items[0].dataIndex]);
              const notes = [cycle.settled ? m("cc.usage_cycle_settled") : m("cc.usage_cycle_running")];
              if (cycle.partial) notes.push(m("cc.usage_cycle_partial"));
              return ccWhen(cycle.start) + " – " + ccWhen(cycle.end) + "\n" + notes.join(DISPLAY_SEPARATOR);
            },
            label: (item) => {
              const cycle = item.dataset.cycleKey.shown.find((entry) => entry.start === starts[item.dataIndex]);
              const line = item.dataset.label + ": " + ccUSD(item.raw);
              return item.datasetIndex % 3 === 0
                ? line + DISPLAY_SEPARATOR + m("cc.usage_cycle_weekly", { v0: ccUSD(cycle.weekly_usd) })
                : line;
            }
          }
        })
      },
      scales: {
        x: { ...timeAxis(axis, width), stacked: true },
        y: {
          stacked: true,
          beginAtZero: true,
          grid: { color: axis.grid },
          border: { color: axis.grid },
          ticks: { color: axis.text, maxTicksLimit: 5, callback: (value) => "$" + ccNumber(value, 0) }
        }
      }
    }
  };
}

function ccUsageRangeControl() {
  return el(
    "div",
    { class: "segmented-control cc-trend-range", role: "group", "aria-label": m("cc.trend_range") },
    CC_TREND_RANGES.map((range) =>
      el("button", {
        type: "button",
        class: range === ccUsageState.range ? "active" : "",
        "aria-pressed": String(range === ccUsageState.range),
        text: m("cc.trend_range_" + range),
        onclick: () => {
          ccUsageState.range = range;
          renderCCUsage();
        }
      })
    )
  );
}

function ccUsagePoolCard(pool, colors, draws) {
  const card = el(
    "div",
    { class: "inset-card cc-trend-card" },
    el("div", { class: "cc-trend-head" }, el("strong", { text: pool.name || pool.id }))
  );
  if (!pool.keys.length) {
    card.append(el("div", { class: "empty", text: m("cc.usage_no_keys") }));
    return card;
  }
  const days = ccTrendRange(ccUsageDays(pool.keys), ccUsageState.range);
  const legend = el(
    "div",
    { class: "analysis-legend" },
    pool.keys.map((key, index) =>
      analysisLegendItem(colors[index % colors.length], ccKeyName(key) + DISPLAY_SEPARATOR + m("cc.kind_" + key.kind))
    ),
    ccRefusedKeys(days, pool.keys).map((key, index) =>
      el(
        "span",
        { class: "analysis-legend-item cc-usage-refusal", style: "--legend-color:" + ccRefusalColor(colors, index) },
        m("cc.usage_blocks_of", { v0: ccKeyName(key) })
      )
    )
  );
  const figures = el("div", { class: "cc-trend-charts" });
  if (days.some((day) => day.total > 0 || Object.keys(day.blocks).length)) {
    const panel = ccTrendPanel(m("cc.usage_chart_daily"));
    figures.append(
      el("div", { class: "cc-trend-figure" }, el("div", { class: "cc-trend-figure-title", text: m("cc.usage_chart_daily") }), legend, panel)
    );
    const keys = pool.keys;
    draws.push(() => ccDrawUsageChart(panel, ccUsageDailyConfig(days, keys, colors, panel.clientWidth)));
  } else { figures.append(el("div", { class: "cc-trend-figure" }, el("div", { class: "empty", text: m("cc.usage_empty") }))); }
  const fixed = pool.keys.map((key, index) => ({ ...key, color: index, shown: ccUsageCycles(key.cycles || [], days, ccUsageState.range) }))
    .filter((key) => key.kind === "fixed" && key.shown.length);
  if (fixed.length) {
    const panel = ccTrendPanel(m("cc.usage_chart_cycles"));
    figures.append(
      el(
        "div",
        { class: "cc-trend-figure" },
        el("div", { class: "cc-trend-figure-title", text: m("cc.usage_chart_cycles") }),
        el(
          "div",
          { class: "analysis-legend" },
          analysisLegendItem(colors[fixed[0].color % colors.length], m("cc.usage_cycle_used")),
          analysisLegendItem(ccFaded(colors[fixed[0].color % colors.length], 30), m("cc.usage_cycle_forfeited")),
          el("span", { class: "analysis-legend-item cc-usage-dashed", text: m("cc.usage_cycle_remaining") })
        ),
        panel
      )
    );
    draws.push(() => ccDrawUsageChart(panel, ccUsageCycleConfig(fixed, colors, panel.clientWidth)));
  }
  card.append(figures);
  return card;
}

function renderCCUsage() {
  const target = $("cc-usage");
  if (!target) return;
  ccDestroyUsageCharts();
  const data = ccQuery.value;
  if (!data) {
    target.replaceChildren();
    return;
  }
  const head = el("div", { class: "cc-trend-toolbar" }, el("h3", { text: m("cc.usage_title") }), ccUsageRangeControl());
  const hint = el("p", { class: "muted small", text: m("cc.usage_hint") });
  const status = (text) => target.replaceChildren(head, hint, el("div", { class: "empty", text }));
  if (!ccShownPools(data).length) return status(m("cc.forecast_no_pools"));
  if (!data.cc.loaded) return status(m("cc.not_loaded"));
  const usage = ccUsageQuery.value;
  if (!usage) return status(ccUsageQuery.error ? m("cc.usage_load_failed", { v0: ccUsageQuery.error }) : m("ui.loading"));
  const colors = analysisColors();
  const draws = [];
  target.replaceChildren(
    head,
    hint,
    ...usage.pools.filter((pool) => (pool.provider || "claude") === ccState.provider).map((pool) => ccUsagePoolCard(pool, colors, draws))
  );
  for (const draw of draws) draw();
}

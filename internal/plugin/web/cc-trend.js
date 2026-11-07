// CC quota trend: each pooled account's hourly k evidence and 1% segments,
// split into days of the viewer's time zone. The aggregation functions are
// pure and read no page state, so scripts/cc_trend.test.mjs runs them alone.
const CC_TREND_RANGES = ["7", "14", "30", "all"];
const CC_TREND_ROLLING_DAYS = 7;
const CC_TREND_EPSILON = 1e-9;
const ccTrendState = { range: "all", charts: [] };

// The viewer's UTC offset in minutes at an instant, daylight saving included.
function ccLocalOffset(ms) { return -new Date(ms).getTimezoneOffset(); }

// The calendar day (YYYY-MM-DD) an instant falls on at the given offset.
function ccDayKey(ms, offset = ccLocalOffset) { return new Date(ms + offset(ms) * 60000).toISOString().slice(0, 10); }

function ccShiftDay(key, days) {
  const date = new Date(key + "T00:00:00Z");
  date.setUTCDate(date.getUTCDate() + days);
  return date.toISOString().slice(0, 10);
}

// Daily sums from the first day with data to today. An hour of k evidence or
// segments counts on the day the hour starts. Each day carries:
// - gain5h, gain7d: summed utilization gains in percentage points;
// - k: gain5h / gain7d, null without 7-day gain; lowPrecision when the 7-day
//   gain is below k_min_weekly_points;
// - rollingK over the last 7 days (rollingDays fewer at the start);
// - usdPerPoint: cost / gain of the day's segments, without the segments the
//   estimate excluded (a raw trend has none marked);
// - weekly: k × usdPerPoint × 100, using the rolling k (weeklyBasis
//   "rolling") when the day's own k is missing, zero or imprecise; none when
//   the account needs a k it does not have. An account whose segments
//   measure the 7-day window has weekly = usdPerPoint × 100 ("window").
function ccTrendDays(trend, { now = Date.now(), offset = ccLocalOffset } = {}) {
  const sums = new Map();
  const day = (at) => {
    const key = ccDayKey(Date.parse(at), offset);
    if (!sums.has(key)) sums.set(key, { gain5h: 0, gain7d: 0, segments: 0, excluded: 0, cost: 0, gain: 0 });
    return sums.get(key);
  };
  for (const bucket of trend.buckets || []) {
    const sum = day(bucket.at);
    sum.gain5h += bucket.gain_5h;
    sum.gain7d += bucket.gain_7d;
  }
  // The server sums each hour's segments; cost and gain cover kept ones only.
  for (const hour of trend.segment_hours || []) {
    const sum = day(hour.at);
    sum.segments += hour.segments;
    sum.excluded += hour.excluded;
    sum.cost += hour.cost_usd;
    sum.gain += hour.gain;
  }
  if (!sums.size) return [];
  const keys = [...sums.keys()].sort();
  const today = ccDayKey(now, offset);
  const last = today > keys.at(-1) ? today : keys.at(-1);
  const minimum = Number(trend.k_min_weekly_points) || 0;
  const days = [];
  for (let key = keys[0]; key <= last; key = ccShiftDay(key, 1)) {
    const sum = sums.get(key) || { gain5h: 0, gain7d: 0, segments: 0, excluded: 0, cost: 0, gain: 0 };
    const k = sum.gain7d > 0 ? sum.gain5h / sum.gain7d : null;
    days.push({
      key,
      ...sum,
      hasData: sums.has(key),
      k,
      lowPrecision: k !== null && sum.gain7d < minimum - CC_TREND_EPSILON,
      usdPerPoint: sum.gain > 0 ? sum.cost / sum.gain : null
    });
  }
  days.forEach((entry, index) => {
    const window = days.slice(Math.max(0, index - CC_TREND_ROLLING_DAYS + 1), index + 1);
    const gain5h = window.reduce((total, item) => total + item.gain5h, 0);
    const gain7d = window.reduce((total, item) => total + item.gain7d, 0);
    entry.rollingK = gain7d > 0 ? gain5h / gain7d : null;
    entry.rollingDays = window.length;
    if (trend.segment_window === "7d") {
      // Segments of the 7-day window: a point is 1% of the weekly total.
      entry.weeklyBasis = entry.usdPerPoint === null ? null : "window";
      entry.weekly = entry.weeklyBasis ? entry.usdPerPoint * 100 : null;
      return;
    }
    const trusted = entry.k !== null && entry.k > 0 && !entry.lowPrecision;
    const k = trend.needs_k ? null : trusted ? entry.k : entry.rollingK > 0 ? entry.rollingK : null;
    entry.weeklyBasis = k === null || entry.usdPerPoint === null ? null : trusted ? "daily" : "rolling";
    entry.weekly = entry.weeklyBasis ? k * entry.usdPerPoint * 100 : null;
  });
  return days;
}

// The days of the selected range ending today; "all" keeps every day.
function ccTrendRange(days, range) {
  if (range === "all" || !days.length) return days;
  const first = ccShiftDay(days.at(-1).key, 1 - Number(range));
  return days.filter((day) => day.key >= first);
}

// A trend needs at least two days with usage to show anything.
function ccTrendEnough(days) { return days.filter((day) => day.gain5h > 0 || day.gain7d > 0 || day.segments > 0).length >= 2; }

function ccTrendDayLabel(key, options = { month: "numeric", day: "numeric" }) {
  return new Intl.DateTimeFormat(locale(), { ...options, timeZone: "UTC" }).format(new Date(key + "T00:00:00Z"));
}

// Every raw value of a day, for both charts' tooltips.
function ccTrendTooltipLines(day, trend) {
  const lines = [
    m("cc.trend_tip_gains", { v0: ccNumber(day.gain5h), v1: ccNumber(day.gain7d) }),
    trend.exclusion === "estimate"
      ? m("cc.trend_tip_segments_filtered", { v0: day.segments, v1: day.excluded })
      : m("cc.trend_tip_segments", { v0: day.segments }),
    m("cc.trend_tip_cost", { v0: ccUSD(day.cost) }),
    m("cc.trend_tip_daily_k", { v0: day.k === null ? "—" : ccNumber(day.k) }) +
      (day.lowPrecision ? " " + m("cc.trend_tip_low_precision", { v0: ccNumber(trend.k_min_weekly_points) }) : ""),
    m("cc.trend_tip_rolling_k", { v0: day.rollingK === null ? "—" : ccNumber(day.rollingK), v1: day.rollingDays }),
    m("cc.trend_tip_usd_per_point", { v0: day.usdPerPoint === null ? "—" : ccUSD(day.usdPerPoint) })
  ];
  if (day.weeklyBasis === "daily") lines.push(m("cc.trend_tip_weekly_daily", { v0: ccUSD(day.weekly) }));
  else if (day.weeklyBasis === "window") lines.push(m("cc.trend_tip_weekly_window", { v0: ccUSD(day.weekly) }));
  else if (day.weeklyBasis === "rolling") lines.push(m("cc.trend_tip_weekly_rolling", { v0: ccUSD(day.weekly) }));
  else lines.push(m("cc.trend_tip_weekly_none"));
  return lines;
}

function ccDestroyTrendCharts() {
  for (const chart of ccTrendState.charts) chart.destroy();
  ccTrendState.charts = [];
}

function ccTrendPanel(label) {
  return el("div", { class: "cc-trend-chart" }, el("canvas", { role: "img", "aria-label": label }), el("div", { class: "empty hidden" }));
}

// Charts draw once their canvases are in the page; until the library loads,
// each panel shows its status instead.
function ccDrawTrendChart(panel, config) {
  const canvas = panel.querySelector("canvas");
  const status = panel.querySelector(".empty");
  const message = typeof Chart !== "function" ? chartError || m("ui.loading_charts") : "";
  canvas.classList.toggle("hidden", !!message);
  status.classList.toggle("hidden", !message);
  setText(status, message);
  if (!message) ccTrendState.charts.push(new Chart(canvas, window.billingI18n.chartValue(config)));
}

function ccTrendTooltip(days, trend) {
  return chartTooltip({
    usePointStyle: true,
    boxWidth: 8,
    boxHeight: 8,
    boxPadding: 6,
    // One block of lines per day, whichever series is under the pointer.
    filter: (_item, index) => index === 0,
    callbacks: {
      title: (items) => (items.length ? ccTrendDayLabel(days[items[0].dataIndex].key, { dateStyle: "medium" }) : ""),
      labelPointStyle: () => ({ pointStyle: "circle", rotation: 0 }),
      label: (item) => ccTrendTooltipLines(days[item.dataIndex], trend)
    }
  });
}

function ccTrendKConfig(days, trend, colors, width) {
  const axis = chartAxisColors();
  const surface = themeColor("--analysis-segment-border", "#f0eee8");
  const faded = "color-mix(in srgb, " + colors[0] + " 45%, " + surface + ")";
  const datasets = [
    {
      type: "line",
      label: m("cc.trend_daily_k"),
      data: days.map((day) => day.k),
      borderColor: colors[0],
      backgroundColor: colors[0],
      borderWidth: 1.5,
      spanGaps: true,
      // Days without k are skipped; the line across them is dashed.
      segment: {
        borderDash: (context) => (context.p0.skip || context.p1.skip || context.p1DataIndex - context.p0DataIndex > 1 ? [4, 4] : undefined)
      },
      pointRadius: 4,
      pointHoverRadius: 6,
      pointBorderWidth: 2,
      pointBorderColor: days.map((day) => (day.lowPrecision ? faded : colors[0])),
      pointBackgroundColor: days.map((day) => (day.lowPrecision ? surface : colors[0]))
    },
    {
      type: "line",
      label: m("cc.trend_rolling_k"),
      data: days.map((day) => day.rollingK),
      borderColor: colors[1],
      backgroundColor: colors[1],
      borderWidth: 4,
      spanGaps: true,
      pointRadius: 0,
      pointHoverRadius: 4,
      tension: 0.3
    }
  ];
  if (trend.k)
    datasets.push({
      type: "line",
      label: m("cc.trend_current_k", { v0: ccNumber(trend.k), v1: m("cc.k_origin_" + trend.k_source) }),
      data: days.map(() => trend.k),
      borderColor: axis.text,
      backgroundColor: axis.text,
      borderWidth: 1.5,
      borderDash: [6, 4],
      pointRadius: 0,
      pointHoverRadius: 0
    });
  return {
    type: "line",
    data: { labels: days.map((day) => ccTrendDayLabel(day.key)), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      onResize: (chart, size) => { chart.options.scales.x.ticks.maxTicksLimit = timeTickLimit(size.width); },
      interaction: { mode: "index", intersect: false },
      plugins: { legend: { display: false }, tooltip: ccTrendTooltip(days, trend) },
      scales: {
        x: timeAxis(axis, width),
        y: {
          beginAtZero: false,
          grace: "10%",
          grid: { color: axis.grid },
          border: { color: axis.grid },
          ticks: { color: axis.text, maxTicksLimit: 6, callback: (value) => ccNumber(value) }
        }
      }
    }
  };
}

function ccTrendGainConfig(days, trend, colors, width) {
  const axis = chartAxisColors();
  const bar = (label, data, color) => ({
    type: "bar",
    label,
    data,
    yAxisID: "points",
    backgroundColor: color,
    borderColor: color,
    borderWidth: 0,
    maxBarThickness: 18,
    categoryPercentage: 0.8,
    barPercentage: 0.9
  });
  const datasets = [
    trend.segment_window !== "7d" && bar(m("cc.trend_gain_5h"), days.map((day) => (day.hasData ? day.gain5h : null)), colors[0]),
    // A 7-day window account's segments are its 7-day gain; it has no k evidence.
    bar(
      m("cc.trend_gain_7d"),
      days.map((day) => (day.hasData ? (trend.segment_window === "7d" ? day.gain : day.gain7d) : null)),
      colors[2]
    ),
    {
      type: "line",
      label: m("cc.trend_weekly"),
      data: days.map((day) => day.weekly),
      yAxisID: "usd",
      borderColor: colors[4],
      backgroundColor: colors[4],
      borderWidth: 2,
      spanGaps: true,
      segment: { borderDash: (context) => (context.p1DataIndex - context.p0DataIndex > 1 ? [4, 4] : undefined) },
      // Days projected with the rolling k show as diamonds.
      pointStyle: days.map((day) => (day.weeklyBasis === "rolling" ? "rectRot" : "circle")),
      pointRadius: 3.5,
      pointHoverRadius: 5
    }
  ].filter(Boolean);
  return {
    type: "bar",
    data: { labels: days.map((day) => ccTrendDayLabel(day.key)), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      onResize: (chart, size) => { chart.options.scales.x.ticks.maxTicksLimit = timeTickLimit(size.width); },
      interaction: { mode: "index", intersect: false },
      plugins: { legend: { display: false }, tooltip: ccTrendTooltip(days, trend) },
      scales: {
        x: timeAxis(axis, width),
        points: {
          beginAtZero: true,
          grid: { color: axis.grid },
          border: { color: axis.grid },
          ticks: { color: axis.text, maxTicksLimit: 5, callback: (value) => ccNumber(value, 1) }
        },
        usd: {
          beginAtZero: true,
          position: "right",
          grid: { drawOnChartArea: false },
          ticks: { color: axis.text, maxTicksLimit: 5, callback: (value) => "$" + ccNumber(value, 0) }
        }
      }
    }
  };
}

function ccTrendRangeControl() {
  return el(
    "div",
    { class: "segmented-control cc-trend-range", role: "group", "aria-label": m("cc.trend_range") },
    CC_TREND_RANGES.map((range) =>
      el("button", {
        type: "button",
        class: range === ccTrendState.range ? "active" : "",
        "aria-pressed": String(range === ccTrendState.range),
        text: m("cc.trend_range_" + range),
        onclick: () => {
          ccTrendState.range = range;
          renderCCTrend();
        }
      })
    )
  );
}

// The saved pools' accounts, in pool order.
function ccTrendAccounts(data) { return [...new Set(data.cc.settings.pools.flatMap((pool) => pool.auth_indexes))]; }

// The pooled accounts of the provider on display.
function ccTrendShownAccounts(data) {
  return [
    ...new Set(data.cc.settings.pools.filter((pool) => ccPoolProvider(pool) === ccState.provider).flatMap((pool) => pool.auth_indexes))
  ];
}

function readCCTrend(reload) {
  const data = ccQuery.value;
  if (!data) return Promise.resolve(null);
  const indexes = ccTrendAccounts(data);
  return ccTrendQuery.read(
    JSON.stringify(indexes),
    async () => {
      const entries = await Promise.all(
        indexes.map(async (index) => {
          try {
            const response = await plugin("GET", "/cc/trend?" + new URLSearchParams({ auth_index: index }));
            return [index, { trend: response.trend }];
          } catch (error) {
            if (error instanceof AuthError) throw error;
            return [index, { error: error.message || String(error) }];
          }
        })
      );
      return Object.fromEntries(entries);
    },
    reload
  );
}

function ccTrendCard(data, index, entry, colors, draws) {
  const head = el("div", { class: "cc-trend-head" }, el("strong", { text: ccCredentialName(data, index) }));
  const card = el("div", { class: "inset-card cc-trend-card" }, head);
  if (!entry) {
    card.append(el("div", { class: "empty", text: m("ui.loading") }));
    return card;
  }
  if (entry.error) {
    card.append(el("div", { class: "empty", text: m("cc.trend_load_failed", { v0: entry.error }) }));
    return card;
  }
  const trend = entry.trend;
  const weeklyWindow = trend.segment_window === "7d";
  if (trend.k && !weeklyWindow)
    head.append(ccTag(m("cc.trend_current_k", { v0: ccNumber(trend.k), v1: m("cc.k_origin_" + trend.k_source) }), "info"));
  if (weeklyWindow) head.append(ccTag(m("cc.window_7d_only"), "info", m("cc.window_7d_only_tip")));
  else if (trend.needs_k) head.append(ccTag(m("cc.needs_k"), "warn", m("cc.needs_k_tip")));
  const days = ccTrendRange(ccTrendDays(trend), ccTrendState.range);
  if (!ccTrendEnough(days)) {
    card.append(el("div", { class: "empty", text: m("cc.trend_empty") }));
    return card;
  }
  if (weeklyWindow) {
    // No 5-hour window: no k evidence, and the segments measure the 7-day window.
    const gainPanel = ccTrendPanel(m("cc.trend_chart_gain"));
    card.append(
      el(
        "div",
        { class: "cc-trend-charts" },
        el(
          "div",
          { class: "cc-trend-figure" },
          el("div", { class: "cc-trend-figure-title", text: m("cc.trend_chart_gain") }),
          el(
            "div",
            { class: "analysis-legend" },
            analysisLegendItem(colors[2], m("cc.trend_gain_7d")),
            analysisLegendItem(colors[4], m("cc.trend_weekly"))
          ),
          gainPanel
        )
      ),
      el("p", { class: "muted small", text: m("cc.trend_window_7d_note") })
    );
    draws.push(() => ccDrawTrendChart(gainPanel, ccTrendGainConfig(days, trend, colors, gainPanel.clientWidth)));
    return card;
  }
  const kPanel = ccTrendPanel(m("cc.trend_chart_k"));
  const gainPanel = ccTrendPanel(m("cc.trend_chart_gain"));
  const legend = (...items) => el("div", { class: "analysis-legend" }, ...items);
  card.append(
    el(
      "div",
      { class: "cc-trend-charts" },
      el(
        "div",
        { class: "cc-trend-figure" },
        el("div", { class: "cc-trend-figure-title", text: m("cc.trend_chart_k") }),
        legend(
          analysisLegendItem(colors[0], m("cc.trend_daily_k")),
          analysisLegendItem(colors[1], m("cc.trend_rolling_k")),
          trend.k
            ? analysisLegendItem(
                chartAxisColors().text,
                m("cc.trend_current_k", { v0: ccNumber(trend.k), v1: m("cc.k_origin_" + trend.k_source) })
              )
            : null,
          el("span", {
            class: "analysis-legend-item cc-trend-hollow",
            text: m("cc.trend_low_precision_legend", { v0: ccNumber(trend.k_min_weekly_points) })
          })
        ),
        kPanel
      ),
      el(
        "div",
        { class: "cc-trend-figure" },
        el("div", { class: "cc-trend-figure-title", text: m("cc.trend_chart_gain") }),
        legend(
          analysisLegendItem(colors[0], m("cc.trend_gain_5h")),
          analysisLegendItem(colors[2], m("cc.trend_gain_7d")),
          analysisLegendItem(colors[4], m("cc.trend_weekly"))
        ),
        gainPanel
      )
    ),
    trend.needs_k ? el("p", { class: "muted small", text: m("cc.trend_needs_k_note") }) : "",
    el("p", { class: "muted small", text: m(trend.exclusion === "estimate" ? "cc.trend_basis_estimate" : "cc.trend_basis_raw") })
  );
  draws.push(() => ccDrawTrendChart(kPanel, ccTrendKConfig(days, trend, colors, kPanel.clientWidth)));
  draws.push(() => ccDrawTrendChart(gainPanel, ccTrendGainConfig(days, trend, colors, gainPanel.clientWidth)));
  return card;
}

function renderCCTrend() {
  const target = $("cc-trend");
  if (!target) return;
  ccDestroyTrendCharts();
  const data = ccQuery.value;
  if (!data) {
    target.replaceChildren();
    return;
  }
  const indexes = ccTrendShownAccounts(data);
  const head = el(
    "div",
    { class: "cc-trend-toolbar" },
    el("h3", { text: m("cc.trend_title") }),
    indexes.length ? ccTrendRangeControl() : null
  );
  const hint = el("p", { class: "muted small", text: m("cc.trend_hint") });
  if (!indexes.length) {
    target.replaceChildren(head, hint, el("div", { class: "empty", text: m("cc.trend_no_accounts") }));
    return;
  }
  const values = ccTrendQuery.key === JSON.stringify(ccTrendAccounts(data)) ? ccTrendQuery.value : null;
  if (!values && ccTrendQuery.error) {
    target.replaceChildren(head, hint, el("div", { class: "empty", text: m("cc.trend_load_failed", { v0: ccTrendQuery.error }) }));
    return;
  }
  const colors = analysisColors();
  const draws = [];
  target.replaceChildren(head, hint, ...indexes.map((index) => ccTrendCard(data, index, values?.[index], colors, draws)));
  for (const draw of draws) draw();
}

// CC simulation: replay a pool's retained history with fixed keys added,
// changed or removed. It only reads; the settings and the database stay as
// they are.
const ccSimState = { poolID: "", add: [], changes: {}, result: null, error: "", busy: false };

function resetCCSim() { Object.assign(ccSimState, { poolID: "", add: [], changes: {}, result: null, error: "", busy: false }); }

// A number input of the simulation; empty means "use the default".
function ccSimInput(value, onChange, attrs = {}) {
  return el("input", {
    type: "number",
    class: "cc-number",
    step: "any",
    value: value === null || value === undefined ? "" : String(value),
    ...attrs,
    oninput: (event) => {
      const raw = event.target.value.trim();
      onChange(raw === "" ? null : Number(raw));
    }
  });
}

function ccSimPools(data) { return ccShownPools(data).filter((pool) => pool.auth_indexes.length); }

function ccSimFixedKeys(data, poolID) {
  return data.cc.keys.filter((key) => key.settings.kind === "fixed" && key.settings.pool_id === poolID && key.valid);
}

function ccSimRequest() {
  const changes = {};
  for (const [scope, change] of Object.entries(ccSimState.changes)) {
    const next = {};
    if (change.remove) next.remove = true;
    if (change.weekly_usd !== null && change.weekly_usd !== undefined) next.weekly_usd = change.weekly_usd;
    if (change.share !== null && change.share !== undefined) next.five_hour_share = change.share / 100;
    if (Object.keys(next).length) changes[scope] = next;
  }
  const add = ccSimState.add.map((key) => {
    const next = { weekly_usd: key.weekly_usd ?? 0 };
    if (key.share !== null && key.share !== undefined) next.five_hour_share = key.share / 100;
    if (key.usage !== null && key.usage !== undefined) next.weekly_usage_usd = key.usage;
    return next;
  });
  return { pool_id: ccSimState.poolID, add, changes };
}

async function ccRunSimulation() {
  if (ccSimState.busy) return;
  ccSimState.busy = true;
  ccSimState.error = "";
  renderCCSim();
  try {
    const response = await plugin("POST", "/cc/simulate", ccSimRequest());
    ccSimState.result = response.simulation;
  } catch (error) {
    if (error instanceof AuthError) throw error;
    ccSimState.result = null;
    ccSimState.error = error.message || String(error);
  } finally {
    ccSimState.busy = false;
    renderCCSim();
  }
}

function ccSimKeysEditor(data) {
  const fixed = ccSimFixedKeys(data, ccSimState.poolID);
  const rows = fixed.map((key) => {
    const change = (ccSimState.changes[key.scope] ||= {});
    const option = data.key_options.find((item) => item.scope === key.scope) || key;
    const share = key.settings.five_hour_share === undefined ? null : Math.round(key.settings.five_hour_share * 10000) / 100;
    return el(
      "tr",
      { class: change.remove ? "cc-sim-removed" : "" },
      el("td", {}, el("div", { text: ccKeyName(option) }), el("div", { class: "muted small", text: ccUSD(key.weekly_usd) })),
      el(
        "td",
        {},
        ccSimInput(
          change.weekly_usd,
          (value) => { change.weekly_usd = value; },
          { min: "0", placeholder: ccNumber(key.weekly_usd), "aria-label": m("cc.sim_weekly"), disabled: !!change.remove }
        )
      ),
      el(
        "td",
        {},
        ccSimInput(
          change.share,
          (value) => { change.share = value; },
          {
            min: "0",
            max: "100",
            placeholder: share === null ? m("cc.sim_share_default") : ccNumber(share),
            "aria-label": m("cc.five_hour_share"),
            disabled: !!change.remove
          }
        )
      ),
      el(
        "td",
        {},
        el(
          "label",
          { class: "cc-check" },
          checkboxControl({
            input: el("input", {
              type: "checkbox",
              checked: !!change.remove,
              "aria-label": m("cc.sim_remove"),
              onchange: (event) => {
                change.remove = event.target.checked;
                renderCCSim();
              }
            })
          }),
          el("span", { text: m("cc.sim_remove") })
        )
      )
    );
  });
  const added = ccSimState.add.map((key, index) =>
    el(
      "tr",
      {},
      el("td", {}, ccTag(m("cc.sim_new_key", { v0: index + 1 }), "info")),
      el("td", {}, ccSimInput(key.weekly_usd, (value) => { key.weekly_usd = value; }, { min: "0", "aria-label": m("cc.sim_weekly") })),
      el(
        "td",
        {},
        ccSimInput(
          key.share,
          (value) => { key.share = value; },
          { min: "0", max: "100", placeholder: m("cc.sim_share_default"), "aria-label": m("cc.five_hour_share") }
        )
      ),
      el(
        "td",
        { class: "cc-sim-new-usage" },
        ccSimInput(
          key.usage,
          (value) => { key.usage = value; },
          { min: "0", placeholder: m("cc.sim_usage_default"), "aria-label": m("cc.sim_usage") }
        ),
        el("button", {
          type: "button",
          class: "link",
          text: m("cc.sim_delete"),
          onclick: () => {
            ccSimState.add.splice(index, 1);
            renderCCSim();
          }
        })
      )
    )
  );
  return el(
    "div",
    {},
    ccTable(
      [m("cc.sim_key"), m("cc.sim_weekly"), m("cc.sim_share"), m("cc.sim_remove_or_usage")],
      [...rows, ...added],
      m("cc.sim_no_fixed")
    ),
    el(
      "div",
      { class: "cc-sim-actions" },
      el("button", {
        type: "button",
        text: m("cc.sim_add_key"),
        disabled: ccSimState.add.length >= 10,
        onclick: () => {
          ccSimState.add.push({ weekly_usd: 100, share: null, usage: null });
          renderCCSim();
        }
      }),
      el("button", {
        type: "button",
        class: "primary",
        text: ccSimState.busy ? m("cc.sim_running") : m("cc.sim_run"),
        disabled: ccSimState.busy,
        onclick: () => guard(ccRunSimulation)
      })
    )
  );
}

function ccSimCompare(base, next, format = (value) => String(value)) {
  const changed = format(base) !== format(next);
  return el(
    "span",
    { class: "cc-sim-compare" },
    el("span", { class: "muted", text: format(base) }),
    el("span", { class: "muted", text: " → " }),
    el("strong", { class: changed ? (next > base ? "cc-sim-worse" : "cc-sim-better") : "", text: format(next) })
  );
}

function ccSimBlockedTip(outcome) {
  return m("cc.sim_blocked_tip", { v0: outcome.blocked_week_hours, v1: outcome.blocked_five_hour_hours, v2: outcome.hours });
}

function ccSimResult(data, result) {
  const summary = el(
    "div",
    { class: "cc-sim-summary" },
    el(
      "div",
      { class: "cc-sim-metric", "data-tip": ccSimBlockedTip(result.scenario) },
      el("span", { class: "muted small", text: m("cc.sim_blocked_hours") }),
      ccSimCompare(result.baseline.blocked_hours, result.scenario.blocked_hours)
    ),
    el(
      "div",
      { class: "cc-sim-metric", "data-tip": m("cc.sim_lost_tip") },
      el("span", { class: "muted small", text: m("cc.sim_lost") }),
      ccSimCompare(result.baseline.lost_usd, result.scenario.lost_usd, ccUSD)
    ),
    el(
      "div",
      { class: "cc-sim-metric", "data-tip": m("cc.sim_oversold_tip") },
      el("span", { class: "muted small", text: m("cc.sim_oversold_hours") }),
      ccSimCompare(result.baseline.oversold_week_hours, result.scenario.oversold_week_hours)
    ),
    el(
      "div",
      { class: "cc-sim-metric" },
      el("span", { class: "muted small", text: m("cc.sim_oversold_5h_hours") }),
      ccSimCompare(result.baseline.oversold_five_hour_hours, result.scenario.oversold_five_hour_hours)
    )
  );
  const weeks = [...result.weeks].reverse().map((week) =>
    el(
      "tr",
      {},
      el(
        "td",
        {},
        el("div", { text: ccCycleLabel(week) }),
        week.partial ? ccTag(m(week.running ? "cc.sim_week_running" : "cc.sim_week_partial"), "warn") : null,
        week.observed ? null : ccTag(m("cc.sim_week_derived"), "plain", m("cc.sim_week_derived_tip"))
      ),
      el("td", { "data-tip": ccSimBlockedTip(week.scenario) }, ccSimCompare(week.baseline.blocked_hours, week.scenario.blocked_hours)),
      el("td", {}, ccSimCompare(week.baseline.lost_usd, week.scenario.lost_usd, ccUSD)),
      el(
        "td",
        { "data-tip": week.scenario.first_oversold ? m("cc.sim_first_oversold", { v0: ccWhen(week.scenario.first_oversold) }) : null },
        ccSimCompare(week.baseline.oversold_week_hours, week.scenario.oversold_week_hours)
      ),
      el("td", {}, ccSimCompare(week.baseline.oversold_five_hour_hours, week.scenario.oversold_five_hour_hours)),
      el("td", { text: week.scenario.max_shortfall_usd > 0 ? ccUSD(week.scenario.max_shortfall_usd) : "—" })
    )
  );
  const spans = result.oversold_spans.map((span) =>
    el(
      "li",
      {},
      m("cc.sim_span", {
        v0: m(span.window === "week" ? "cc.forecast_label_7d" : "cc.forecast_label_5h"),
        v1: ccWhen(span.from),
        v2: ccWhen(span.to),
        v3: ccUSD(span.max_shortfall_usd)
      })
    )
  );
  const accounts = result.accounts.map((account) =>
    m("cc.sim_account_basis", {
      v0: ccCredentialName(data, account.auth_index),
      v1: ccUSD(account.weekly_usd),
      v2: ccUSD(account.five_hour_usd)
    })
  )
    .join("\n");
  return el(
    "div",
    { class: "cc-sim-result" },
    el(
      "p",
      { class: "notice show cc-sim-approx-note" },
      m("cc.sim_approximate", { v0: ccWhen(result.from), v1: ccWhen(result.to), v2: result.remainder_keys })
    ),
    summary,
    ccTable(
      [
        m("cc.sim_week"),
        m("cc.sim_blocked_hours"),
        m("cc.sim_lost"),
        m("cc.sim_oversold_hours"),
        m("cc.sim_oversold_5h_hours"),
        m("cc.sim_shortfall")
      ],
      weeks,
      m("cc.sim_no_weeks")
    ),
    spans.length
      ? el(
          "details",
          { class: "cc-sim-spans" },
          el("summary", {
            text: m("cc.sim_spans", { v0: spans.length }) + (result.oversold_spans_truncated ? " " + m("cc.sim_spans_truncated") : "")
          }),
          el("ul", {}, spans)
        )
      : null,
    el(
      "details",
      { class: "cc-sim-approx" },
      el("summary", { text: m("cc.sim_approximations") }),
      el(
        "ul",
        {},
        el("li", { class: "cc-sim-accounts", text: accounts }),
        el("li", { text: ccConversionLine(result.conversion) }),
        result.approximations.map((code) => el("li", { text: m("cc.sim_approx_" + code) }))
      )
    )
  );
}

function renderCCSim() {
  const target = $("cc-sim");
  if (!target) return;
  const data = ccQuery.value;
  if (!data) {
    target.replaceChildren();
    return;
  }
  const head = [el("h3", { text: m("cc.sim_title") }), el("p", { class: "muted small", text: m("cc.sim_hint") })];
  const pools = ccSimPools(data);
  if (!data.cc.loaded) return target.replaceChildren(...head, el("div", { class: "empty", text: m("cc.not_loaded") }));
  if (!pools.length) return target.replaceChildren(...head, el("div", { class: "empty", text: m("cc.sim_no_pools") }));
  if (!pools.some((pool) => pool.id === ccSimState.poolID)) {
    ccSimState.poolID = pools[0].id;
    ccSimState.changes = {};
    ccSimState.result = null;
  }
  const select = el(
    "label",
    { class: "cc-sim-pool" },
    el("span", { text: m("cc.sim_pool") }),
    el(
      "span",
      { class: "native-select" },
      el(
        "select",
        {
          "aria-label": m("cc.sim_pool"),
          onchange: (event) => {
            Object.assign(ccSimState, { poolID: event.target.value, changes: {}, result: null, error: "" });
            renderCCSim();
          }
        },
        pools.map((pool) => el("option", { value: pool.id, text: pool.name || pool.id, selected: pool.id === ccSimState.poolID }))
      )
    )
  );
  const result =
    ccSimState.result && ccSimState.result.pool_id === ccSimState.poolID
      ? ccSimResult(data, ccSimState.result)
      : ccSimState.error ? el("div", { class: "notice err show", text: m("cc.sim_failed", { v0: ccSimState.error }) }) : null;
  target.replaceChildren(
    ...[
      ...head,
      ccState.dirty && el("p", { class: "notice show", text: m("cc.sim_saved_settings") }),
      select,
      ccSimKeysEditor(data),
      result
    ].filter(Boolean)
  );
}

// CC split: Claude account estimates, pools, key assignments and parameters.
// The page edits a draft of the settings and saves it as a whole.
const ccQuery = createQuery(m("cc.title"));
const ccTrendQuery = createQuery(m("cc.trend_title"));
const ccForecastQuery = createQuery(m("cc.forecast_title"));
const ccUsageQuery = createQuery(m("cc.usage_title"));
const ccState = { draft: null, dirty: false, busy: false, openBlocks: new Set() };

const CC_PARAM_FIELDS = [
  { name: "segment_retention_days", label: "cc.param_segment_retention_days", hint: "cc.hint_segment_retention_days", step: "1" },
  { name: "block_gap_minutes", label: "cc.param_block_gap_minutes", hint: "cc.hint_block_gap_minutes" },
  { name: "window_percent", label: "cc.param_window_percent", hint: "cc.hint_window_percent" },
  { name: "reference_mode", label: "cc.param_reference_mode", hint: "cc.hint_reference_mode", options: ["global", "block"] },
  { name: "suspicious_threshold", label: "cc.param_suspicious_threshold", hint: "cc.hint_suspicious_threshold", percent: true },
  { name: "exclude_vote_share", label: "cc.param_exclude_vote_share", hint: "cc.hint_exclude_vote_share", percent: true },
  { name: "weight_power", label: "cc.param_weight_power", hint: "cc.hint_weight_power" },
  { name: "recent_days", label: "cc.param_recent_days", hint: "cc.hint_recent_days" },
  { name: "recent_share", label: "cc.param_recent_share", hint: "cc.hint_recent_share", percent: true },
  { name: "k_min_weekly_points", label: "cc.param_k_min_weekly_points", hint: "cc.hint_k_min_weekly_points" },
  { name: "cold_start_blocks", label: "cc.param_cold_start_blocks", hint: "cc.hint_cold_start_blocks", step: "1" },
  { name: "safety_factor", label: "cc.param_safety_factor", hint: "cc.hint_safety_factor", percent: true }
];
const CC_TIERS = ["pro", "max5x", "max20x"];

pages.admin.cc = {
  autoRefresh: true,
  // Auto refresh would rebuild the inputs being edited; it waits for a save,
  // and while a simulation or forecast input has focus.
  refreshPaused: () => ccState.dirty || !!document.activeElement?.closest?.("#cc-sim, #cc-forecast"),
  queries: [ccQuery, ccTrendQuery, ccForecastQuery, ccUsageQuery],
  load: (reload) => {
    const tasks = [ccQuery.read("cc", () => plugin("GET", "/cc"), reload).then(() => ccReadInsights(reload))];
    if (typeof Chart !== "function" && (!chartError || reload)) tasks.push(loadCharts());
    return settleLoads(tasks);
  },
  sections: [
    { sources: [ccQuery], render: renderCC, displayed: null },
    { sources: [ccQuery, ccTrendQuery], render: renderCCTrend, displayed: null },
    { sources: [ccQuery, ccForecastQuery], render: renderCCForecast, displayed: null },
    { sources: [ccQuery, ccUsageQuery], render: renderCCUsage, displayed: null }
  ],
  sources: [ccQuery, ccTrendQuery, ccForecastQuery, ccUsageQuery],
  groups: [],
  task: null
};

// The trend, forecast and key usage read after the CC view, independently.
function ccReadInsights(reload) { return settleLoads([readCCTrend(reload), readCCForecast(reload), readCCUsage(reload)]); }

function ccUSD(value) {
  return Number.isFinite(value)
    ? "$" + Number(value).toLocaleString(locale(), { minimumFractionDigits: 2, maximumFractionDigits: 2 })
    : "—";
}
function ccPercent(value) {
  return Number.isFinite(value) ? Number(value).toLocaleString(locale(), { maximumFractionDigits: 1 }) + "%" : "—";
}
function ccNumber(value, digits = 2) {
  return Number.isFinite(value) ? Number(value).toLocaleString(locale(), { maximumFractionDigits: digits }) : "—";
}
function ccWhen(value) { return value && !String(value).startsWith("0001-") ? when(value) : "—"; }
function ccTag(text, kind = "plain", tooltip) { return el("span", { class: "tag " + kind, text, "data-tip": tooltip || null }); }

function ccMarkDirty() {
  ccState.dirty = true;
  updateCCButtons();
  updateRefreshControls();
}

// A new session must not see the previous one's estimates or unsaved draft.
function resetCCState() {
  ccQuery.clear();
  ccTrendQuery.clear();
  ccForecastQuery.clear();
  ccUsageQuery.clear();
  ccDestroyTrendCharts();
  ccDestroyUsageCharts();
  resetCCSim();
  Object.assign(ccState, { draft: null, dirty: false, busy: false });
  ccState.openBlocks.clear();
}

// Settings entries whose key or credential no longer exists. Credentials only
// count as gone when their list loaded.
function ccStaleEntries(data, draft) {
  const keys = new Set(data.key_options.map((option) => option.scope));
  const credentials = data.credential_warning ? null : new Set(data.credentials.map((item) => item.auth_index));
  const accounts = credentials
    ? [...new Set([...Object.keys(draft.accounts), ...draft.pools.flatMap((pool) => pool.auth_indexes)])].filter(
        (index) => !credentials.has(index)
      )
    : [];
  return { keys: Object.keys(draft.keys).filter((scope) => !keys.has(scope)), accounts };
}

function ccRemoveStale(data, draft) {
  const stale = ccStaleEntries(data, draft);
  for (const scope of stale.keys) delete draft.keys[scope];
  for (const index of stale.accounts) {
    delete draft.accounts[index];
    for (const pool of draft.pools) {
      pool.auth_indexes = pool.auth_indexes.filter((item) => item !== index);
      if (pool.anchor_auth_index === index) pool.anchor_auth_index = pool.auth_indexes[0] || "";
    }
  }
  ccMarkDirty();
  renderCC();
}

function ccDeletedTag() { return ccTag(m("cc.deleted"), "bad", m("cc.deleted_tip")); }

function updateCCButtons() {
  for (const id of ["cc-save", "cc-refresh", "cc-rebuild"]) if ($(id)) $(id).disabled = ccState.busy;
  if ($("cc-save")) $("cc-save").classList.toggle("primary", ccState.dirty);
  if ($("cc-dirty")) $("cc-dirty").classList.toggle("hidden", !ccState.dirty);
}

function ccCredentialName(data, authIndex) {
  const credential = data.credentials.find((item) => item.auth_index === authIndex);
  return credential ? credential.name || authIndex : authIndex;
}

function ccNumberInput(value, onChange, attrs = {}) {
  return el("input", {
    type: "number",
    class: "cc-number",
    value: value === null || value === undefined ? "" : String(value),
    step: "any",
    ...attrs,
    oninput: (event) => {
      const raw = event.target.value.trim();
      onChange(raw === "" ? null : Number(raw));
      ccMarkDirty();
    }
  });
}

function ccSelect(value, options, onChange, attrs = {}) {
  return el(
    "span",
    { class: "native-select" },
    el(
      "select",
      {
        ...attrs,
        onchange: (event) => {
          onChange(event.target.value);
          ccMarkDirty();
          renderCC();
        }
      },
      options.map(([optionValue, label]) => el("option", { value: optionValue, text: label, selected: optionValue === value }))
    )
  );
}

function ccTable(headers, rows, empty) {
  if (!rows.length) return el("div", { class: "empty", text: empty || m("ui.no_data") });
  return el(
    "div",
    { class: "table-scroll" },
    el(
      "table",
      { class: "cc-table" },
      el("thead", {}, el("tr", {}, headers.map((header) => el("th", { text: header })))),
      el("tbody", {}, rows)
    )
  );
}

function ccSection(title, hint, ...children) {
  return el("div", { class: "cc-section" }, el("h3", { text: title }), hint && el("p", { class: "muted small", text: hint }), ...children);
}

function renderCC() {
  const target = $("cc-body");
  if (!target) return;
  if (renderDataPending(target, [ccQuery])) return;
  const data = ccQuery.value;
  if (!ccState.draft || !ccState.dirty) ccState.draft = structuredClone(data.cc.settings);
  const draft = ccState.draft;
  draft.accounts ||= {};
  draft.keys ||= {};
  draft.pools ||= [];
  const notices = [];
  if (data.cc.loaded === false)
    notices.push(
      el(
        "div",
        { class: "notice err show cc-not-loaded", role: "alert" },
        el("strong", { text: m("cc.not_loaded") }),
        " ",
        m(draft.params.refuse_remainder_when_unloaded ? "cc.not_loaded_refusing" : "cc.not_loaded_admitting")
      )
    );
  if (data.cc.error) notices.push(el("div", { class: "notice err show", text: m("cc.estimation_error", { v0: data.cc.error }) }));
  const stale = ccStaleEntries(data, draft);
  if (stale.keys.length || stale.accounts.length)
    notices.push(
      el(
        "div",
        { class: "notice show cc-stale" },
        el("span", { text: m("cc.stale_entries", { v0: stale.keys.length, v1: stale.accounts.length }) }),
        el("button", { type: "button", class: "link", text: m("cc.remove_stale"), onclick: () => ccRemoveStale(data, draft) })
      )
    );
  if (data.credential_warning)
    notices.push(el("div", { class: "notice err show", text: m("cc.credential_warning", { v0: data.credential_warning }) }));
  target.replaceChildren(
    ...notices,
    el("p", { id: "cc-dirty", class: "notice err show hidden", text: m("cc.unsaved_changes") }),
    ccAccountsSection(data, draft),
    el("div", { id: "cc-forecast", class: "cc-section" }),
    el("div", { id: "cc-trend", class: "cc-section" }),
    el("div", { id: "cc-usage", class: "cc-section" }),
    ccWeightsSection(data, draft),
    ccPoolsSection(data, draft),
    ccKeysSection(data, draft),
    el("div", { id: "cc-sim", class: "cc-section" }),
    ccParamsSection(data, draft),
    ccBlocksSection(data)
  );
  renderCCForecast();
  renderCCTrend();
  renderCCUsage();
  renderCCSim();
  updateCCButtons();
}

function ccAccountsSection(data, draft) {
  const views = new Map(data.cc.accounts.map((account) => [account.auth_index, account]));
  const indexes = [...new Set([...data.credentials.map((item) => item.auth_index), ...data.cc.accounts.map((item) => item.auth_index)])];
  const rows = indexes.map((index) => {
    const view = views.get(index);
    const settings = draft.accounts[index] || {};
    const update = (patch) => {
      const next = { ...settings, ...patch };
      if (!next.tier) delete next.tier;
      if (next.manual_k === null || next.manual_k === undefined) delete next.manual_k;
      if (Object.keys(next).length) draft.accounts[index] = next; else delete draft.accounts[index];
    };
    const deleted = !data.credential_warning && !data.credentials.some((item) => item.auth_index === index);
    const status = deleted
      ? ccDeletedTag()
      : !view
        ? ccTag(m("cc.not_estimated"))
        : view.cold_start
          ? ccTag(m("cc.cold_start"), "warn", m("cc.cold_start_tip", { v0: view.clean_blocks, v1: draft.params.cold_start_blocks }))
          : ccTag(m("cc.estimated"), "ok");
    const kGroup = (label, group) =>
      el("div", {
        class: group.valid ? "" : "muted",
        text: m(label, { v0: group.k > 0 ? ccNumber(group.k) : "—", v1: ccNumber(group.gain_5h, 0), v2: ccNumber(group.gain_7d, 0) }),
        "data-tip": group.valid ? null : m("cc.k_group_insufficient", { v0: draft.params.k_min_weekly_points })
      });
    const kCell = view
      ? [
          kGroup("cc.k_recent_value", view.k_estimate.recent),
          kGroup("cc.k_old_value", view.k_estimate.old),
          el("div", {
            class: "cc-k-final",
            text: m("cc.k_used_value", { v0: view.k ? ccNumber(view.k) : "—" }) + DISPLAY_SEPARATOR + m("cc.k_source_" + view.k_source),
            "data-tip": m("cc.k_basis_" + view.k_estimate.basis)
          })
        ]
      : "—";
    return el(
      "tr",
      {},
      el("td", {}, el("div", { text: ccCredentialName(data, index) }), el("div", { class: "muted small", text: index })),
      el(
        "td",
        {},
        ccSelect(
          settings.tier || "",
          [["", m("cc.tier_unset")], ...CC_TIERS.map((tier) => [tier, m("cc.tier_" + tier)])],
          (value) => update({ tier: value }),
          { "aria-label": m("cc.tier") }
        )
      ),
      el(
        "td",
        {},
        ccNumberInput(settings.manual_k, (value) => update({ manual_k: value }), {
          min: "1",
          max: "100",
          placeholder: m("cc.auto"),
          "aria-label": m("cc.manual_k")
        })
      ),
      el("td", {}, status),
      el("td", {
        text: view ? ccPercent(view.used_5h) + DISPLAY_SLASH + ccPercent(view.used_7d) : "—",
        "data-tip": view ? m("cc.resets_tip", { v0: ccWhen(view.reset_5h), v1: ccWhen(view.reset_7d) }) : null
      }),
      el("td", { text: view ? (view.five_hour_total_usd > 0 ? ccUSD(view.five_hour_total_usd) : m("cc.unknown")) : "—" }),
      el("td", { text: view ? ccUSD(view.weekly_total_usd) : "—", "data-tip": view ? m("cc.totals_from_" + view.totals_from) : null }),
      el("td", { class: "cc-k" }, kCell),
      el("td", { text: view ? ccUSD(view.remaining_week_usd) + DISPLAY_SLASH + ccUSD(view.remaining_five_hour_usd) : "—" }),
      el("td", { text: view ? m("cc.blocks_value", { v0: view.clean_blocks, v1: view.total_blocks }) : "—" }),
      el("td", { text: view ? m("cc.segments_value", { v0: view.segments - view.excluded_segments, v1: view.excluded_segments }) : "—" }),
      el("td", { text: view ? ccWhen(view.cursor) : "—" })
    );
  });
  return ccSection(
    m("cc.accounts"),
    m("cc.accounts_hint"),
    ccTable(
      [
        m("cc.account"),
        m("cc.tier"),
        m("cc.manual_k"),
        m("cc.status"),
        m("cc.used_5h_7d"),
        m("cc.total_5h"),
        m("cc.total_week"),
        m("cc.k"),
        m("cc.remaining_week_5h"),
        m("cc.clean_blocks"),
        m("cc.segments_kept_excluded"),
        m("cc.processed_until")
      ],
      rows,
      m("cc.no_claude_accounts")
    )
  );
}

function ccNewPoolID(draft) {
  for (let index = draft.pools.length + 1; ; index++) {
    const id = "pool-" + index;
    if (!draft.pools.some((pool) => pool.id === id)) return id;
  }
}

function ccPoolsSection(data, draft) {
  const statuses = new Map(data.cc.pools.map((pool) => [pool.id, pool]));
  const cards = draft.pools.map((pool) => {
    const status = statuses.get(pool.id);
    const indexes = [...new Set([...data.credentials.map((item) => item.auth_index), ...pool.auth_indexes])];
    const anchor = pool.auth_indexes.includes(pool.anchor_auth_index) ? pool.anchor_auth_index : pool.auth_indexes[0] || "";
    const accountChoices = indexes.map((index) => {
      const member = pool.auth_indexes.includes(index);
      // An account's capacity can be shared out by one pool only.
      const owner = draft.pools.find((other) => other !== pool && other.auth_indexes.includes(index));
      const input = el("input", {
        type: "checkbox",
        checked: member,
        disabled: !!owner,
        onchange: (event) => {
          pool.auth_indexes = event.target.checked ? [...pool.auth_indexes, index] : pool.auth_indexes.filter((item) => item !== index);
          if (!pool.auth_indexes.includes(pool.anchor_auth_index)) pool.anchor_auth_index = pool.auth_indexes[0] || "";
          ccMarkDirty();
          renderCC();
        }
      });
      const deleted = !data.credential_warning && !data.credentials.some((item) => item.auth_index === index);
      return el(
        "span",
        { class: "cc-check cc-pool-account" + (owner ? " muted" : "") },
        el(
          "label",
          { class: "cc-check", "data-tip": owner ? m("cc.account_in_other_pool", { v0: owner.name || owner.id }) : null },
          checkboxControl({ input }),
          el("span", { text: ccCredentialName(data, index) }),
          owner && el("span", { class: "muted small", text: m("cc.in_pool_value", { v0: owner.name || owner.id }) })
        ),
        deleted && ccDeletedTag(),
        member &&
          (index === anchor
            ? ccTag(m("cc.anchor_account"), "info", m("cc.anchor_account_tip"))
            : el("button", {
                type: "button",
                class: "link small",
                text: m("cc.set_anchor"),
                "data-tip": m("cc.anchor_account_tip"),
                onclick: () => {
                  pool.anchor_auth_index = index;
                  ccMarkDirty();
                  renderCC();
                }
              }))
      );
    });
    const metrics = status
      ? metricTag(
          [
            [m("cc.pool_remaining_week"), ccUSD(status.remaining_week_usd)],
            [m("cc.pool_reserved_week"), ccUSD(status.reserved_week_usd)],
            [m("cc.pool_remaining_5h"), ccUSD(status.remaining_five_hour_usd)],
            [m("cc.pool_reserved_5h"), status.five_hour_enabled ? ccUSD(status.reserved_five_hour_usd) : m("cc.disabled")],
            [m("cc.k"), status.five_hour_enabled ? ccNumber(status.k) : "—"],
            [m("cc.remainder_balance"), ccUSD(status.remainder_balance_usd)]
          ],
          m("cc.pool")
        )
      : el("span", { class: "muted small", text: m("cc.save_to_see_status") });
    return el(
      "div",
      { class: "inset-card cc-pool" },
      el(
        "div",
        { class: "row spread" },
        el("input", {
          class: "cc-pool-name",
          value: pool.name || "",
          placeholder: m("cc.pool_name"),
          "aria-label": m("cc.pool_name"),
          oninput: (event) => {
            pool.name = event.target.value;
            ccMarkDirty();
          }
        }),
        el("span", { class: "muted small", text: pool.id }),
        el("button", {
          type: "button",
          class: "danger",
          text: m("cc.delete_pool"),
          onclick: () => {
            draft.pools = draft.pools.filter((item) => item !== pool);
            for (const [scope, key] of Object.entries(draft.keys)) if (key.pool_id === pool.id) delete draft.keys[scope];
            ccMarkDirty();
            renderCC();
          }
        })
      ),
      el(
        "div",
        { class: "cc-checks" },
        accountChoices.length ? accountChoices : el("span", { class: "muted small", text: m("cc.no_claude_accounts") })
      ),
      metrics
    );
  });
  return ccSection(
    m("cc.pools"),
    m("cc.pools_hint"),
    ...cards,
    el("button", {
      type: "button",
      text: m("cc.add_pool"),
      onclick: () => {
        draft.pools.push({ id: ccNewPoolID(draft), name: "", auth_indexes: [], anchor_auth_index: "" });
        ccMarkDirty();
        renderCC();
      }
    })
  );
}

function ccKeyStatus(key, option, settings) {
  if (!settings.kind) return "";
  if (!key) return ccTag(m("cc.save_to_see_status"));
  if (settings.kind === "fixed") {
    if (!option?.has_weekly_window) return ccTag(m("cc.needs_weekly_plan"), "bad", m("cc.needs_weekly_plan_tip"));
    const alignment = key.alignment?.status;
    if (alignment === "waiting")
      return ccTag(m("cc.alignment_waiting"), "warn", m("cc.alignment_waiting_tip", { v0: ccWhen(key.alignment.wait_until) }));
    if (alignment === "unsupported") return ccTag(m("cc.alignment_unsupported"), "bad", m("cc.alignment_unsupported_tip"));
    if (alignment === "aligned") return ccTag(m("cc.alignment_aligned"), "ok");
    return ccTag(m("cc.alignment_unknown"), "plain", m("cc.alignment_unknown_tip"));
  }
  if (key.admitted) return ccTag(m("cc.admitted"), "ok");
  const detail = window.billingI18n.serverMessage(key.refusal_message, "");
  const retry = key.retry_at ? m("cc.retry_at", { v0: ccWhen(key.retry_at) }) : "";
  return ccTag(m("cc.refused_" + key.refusal), "bad", [detail, retry].filter((part) => String(part)).join(DISPLAY_SEPARATOR) || null);
}

function ccKeysSection(data, draft) {
  const statuses = new Map(data.cc.keys.map((key) => [key.scope, key]));
  const options = [...data.key_options];
  for (const key of data.cc.keys)
    if (!options.some((option) => option.scope === key.scope))
      options.push({ scope: key.scope, preview: key.preview, label: key.label, deleted: true });
  for (const scope of Object.keys(draft.keys))
    if (!options.some((option) => option.scope === scope)) options.push({ scope, deleted: true });
  const pools = draft.pools.map((pool) => [pool.id, pool.name || pool.id]);
  const rows = options.map((option) => {
    const settings = draft.keys[option.scope] || {};
    const status = statuses.get(option.scope);
    const update = (patch) => {
      const next = { ...settings, ...patch };
      if (!next.kind) delete draft.keys[option.scope];
      else {
        next.pool_id ||= pools[0]?.[0] || "";
        if (next.kind !== "fixed") delete next.five_hour_share;
        if (next.kind !== "remainder") delete next.weekly_cap_usd;
        for (const field of ["five_hour_share", "weekly_cap_usd"]) if (next[field] === null) delete next[field];
        draft.keys[option.scope] = next;
      }
    };
    const fixed = settings.kind === "fixed", remainder = settings.kind === "remainder";
    const share = settings.five_hour_share === undefined ? null : Math.round(settings.five_hour_share * 10000) / 100;
    return el(
      "tr",
      {},
      el(
        "td",
        {},
        renderAPIKeyIdentity({ scope: option.scope, preview: option.preview, label: option.label }),
        option.plan_name && el("div", { class: "muted small", text: option.plan_name }),
        option.deleted && ccDeletedTag()
      ),
      el(
        "td",
        {},
        ccSelect(
          settings.kind || "",
          [["", m("cc.kind_none")], ["fixed", m("cc.kind_fixed")], ["remainder", m("cc.kind_remainder")]],
          (value) => update({ kind: value }),
          { "aria-label": m("cc.kind") }
        )
      ),
      el(
        "td",
        {},
        settings.kind ? ccSelect(settings.pool_id || "", pools, (value) => update({ pool_id: value }), { "aria-label": m("cc.pool") }) : "—"
      ),
      el(
        "td",
        {},
        fixed
          ? ccNumberInput(share, (value) => update({ five_hour_share: value === null ? null : value / 100 }), {
              min: "0",
              max: "100",
              placeholder: m("cc.auto"),
              "aria-label": m("cc.five_hour_share")
            })
          : remainder
            ? ccNumberInput(settings.weekly_cap_usd, (value) => update({ weekly_cap_usd: value }), {
                min: "0",
                placeholder: m("cc.unlimited"),
                "aria-label": m("cc.weekly_cap")
              })
            : "—"
      ),
      el("td", {
        text:
          fixed && status
            ? ccUSD(status.weekly_usd)
            : remainder && settings.weekly_cap_usd !== undefined ? ccUSD(settings.weekly_cap_usd) : "—"
      }),
      el("td", {
        text:
          fixed && status
            ? ccUSD(status.used_week_usd) + DISPLAY_SLASH + ccUSD(status.used_five_hour_usd)
            : remainder && status && settings.weekly_cap_usd !== undefined ? ccUSD(status.used_cap_usd) : "—"
      }),
      el("td", { text: fixed && status ? ccUSD(status.reserve_week_usd) + DISPLAY_SLASH + ccUSD(status.reserve_five_hour_usd) : "—" }),
      el("td", { text: fixed && status?.cycle_end ? ccWhen(status.cycle_end) : "—" }),
      el("td", {
        text: status ? ccUSD(status.balance_usd) : "—",
        "data-tip": status ? m("cc.reported_balance_tip", { v0: ccNumber(status.reported_balance_usd, 6) }) : null
      }),
      el("td", {}, ccKeyStatus(status, option, settings))
    );
  });
  return ccSection(
    m("cc.keys"),
    m("cc.keys_hint"),
    ccTable(
      [
        m("cc.key"),
        m("cc.kind"),
        m("cc.pool"),
        m("cc.key_override"),
        m("cc.weekly_amount"),
        m("cc.used_week_5h"),
        m("cc.reserved_week_5h"),
        m("cc.cycle_ends"),
        m("cc.balance"),
        m("cc.status")
      ],
      rows,
      m("cc.no_keys")
    )
  );
}

function ccParamsSection(data, draft) {
  const params = draft.params;
  const defaults = data.cc.defaults;
  const field = (definition) => {
    const value = params[definition.name];
    const fallback = defaults[definition.name];
    let control;
    if (definition.options) {
      control = ccSelect(
        value,
        definition.options.map((option) => [option, m("cc.reference_" + option)]),
        (next) => { params[definition.name] = next; },
        { id: "cc-param-" + definition.name }
      );
    } else {
      const shown = definition.percent ? Math.round(value * 10000) / 100 : value;
      control = ccNumberInput(
        shown,
        (next) => { params[definition.name] = next === null ? fallback : definition.percent ? next / 100 : next; },
        { id: "cc-param-" + definition.name, step: definition.step || "any" }
      );
    }
    const defaultText = definition.options
      ? m("cc.reference_" + fallback)
      : definition.percent ? ccNumber(fallback * 100) + "%" : ccNumber(fallback);
    return el(
      "label",
      { class: "cc-param", for: "cc-param-" + definition.name },
      el("span", { class: "cc-param-label", text: m(definition.label) }),
      control,
      el("span", { class: "muted small", text: m(definition.hint) + DISPLAY_SEPARATOR + m("cc.default_value", { v0: defaultText }) })
    );
  };
  const tiers = CC_TIERS.map((tier) =>
    el(
      "label",
      { class: "cc-param", for: "cc-param-tier-" + tier },
      el("span", { class: "cc-param-label", text: m("cc.tier_weekly_default", { v0: m("cc.tier_" + tier) }) }),
      ccNumberInput(
        params.tier_weekly_usd[tier],
        (next) => { params.tier_weekly_usd[tier] = next === null ? defaults.tier_weekly_usd[tier] : next; },
        { id: "cc-param-tier-" + tier, min: "0" }
      ),
      el("span", { class: "muted small", text: m("cc.default_value", { v0: ccUSD(defaults.tier_weekly_usd[tier]) }) })
    )
  );
  const tierK = CC_TIERS.map((tier) =>
    el(
      "label",
      { class: "cc-param", for: "cc-param-tier-k-" + tier },
      el("span", { class: "cc-param-label", text: m("cc.tier_k_default", { v0: m("cc.tier_" + tier) }) }),
      ccNumberInput(
        params.tier_k[tier],
        (next) => { params.tier_k[tier] = next === null ? defaults.tier_k[tier] : next; },
        { id: "cc-param-tier-k-" + tier, min: "1", max: "100" }
      ),
      el("span", {
        class: "muted small",
        text: m("cc.hint_tier_k") + DISPLAY_SEPARATOR + m("cc.default_value", { v0: ccNumber(defaults.tier_k[tier]) })
      })
    )
  );
  const unloaded = el(
    "label",
    { class: "toggle-filter cc-param-switch" },
    switchControl({
      input: el("input", {
        type: "checkbox",
        role: "switch",
        checked: params.refuse_remainder_when_unloaded,
        onchange: (event) => {
          params.refuse_remainder_when_unloaded = event.target.checked;
          ccMarkDirty();
        }
      })
    }),
    el("span", { text: m("cc.param_refuse_unloaded") })
  );
  return ccSection(
    m("cc.parameters"),
    m("cc.parameters_hint"),
    el("div", { class: "cc-params" }, CC_PARAM_FIELDS.map(field), tiers, tierK),
    el("div", { class: "cc-param-toggle" }, unloaded, el("p", { class: "muted small", text: m("cc.hint_refuse_unloaded") }))
  );
}

function ccBlocksSection(data) {
  const accounts = data.cc.accounts.filter((account) => account.recent_blocks.length);
  if (!accounts.length) return ccSection(m("cc.recent_blocks"), m("cc.recent_blocks_empty"));
  return ccSection(
    m("cc.recent_blocks"),
    m("cc.recent_blocks_hint"),
    accounts.map((account) => {
      const details = el(
        "details",
        {
          open: ccState.openBlocks.has(account.auth_index),
          ontoggle: (event) =>
            event.target.open ? ccState.openBlocks.add(account.auth_index) : ccState.openBlocks.delete(account.auth_index)
        },
        el("summary", { text: ccCredentialName(data, account.auth_index) }),
        ccTable(
          [
            m("cc.block_start"),
            m("cc.block_end"),
            m("cc.segments_kept_excluded"),
            m("cc.block_gain_cost"),
            m("cc.block_ratio"),
            m("cc.block_reference")
          ],
          account.recent_blocks.map((block) =>
            el(
              "tr",
              {},
              el("td", { text: ccWhen(block.start) }),
              el("td", { text: ccWhen(block.end) }),
              el("td", { text: m("cc.segments_value", { v0: block.segments - block.excluded, v1: block.excluded }) }),
              el("td", { text: ccPercent(block.clean_gain) + DISPLAY_SLASH + ccUSD(block.clean_cost_usd) }),
              el("td", {
                text: block.clean_cost_usd > 0 ? ccUSD(100 / (block.clean_gain / block.clean_cost_usd)) : "—",
                "data-tip": m("cc.block_ratio_tip")
              }),
              el("td", { text: m("cc.reference_" + block.reference_mode) })
            )
          )
        )
      );
      return details;
    })
  );
}

// The CC split's estimate of a pooled Claude account on the auth files page and drawer.
function renderCCAuthSummary(cc, heading = true) {
  const item = (label, value, tooltip) =>
    el(
      "span",
      { class: "cc-auth-item", "data-tip": tooltip || null },
      el("span", { class: "cc-auth-label", text: label }),
      el("span", { class: "cc-auth-value", text: value })
    );
  const status = cc.cold_start
    ? el("span", {
        class: "tag warn",
        text: m("cc.cold_start"),
        "data-tip": m("cc.cold_start_tip", { v0: cc.clean_blocks, v1: cc.cold_start_blocks })
      })
    : el("span", { class: "tag ok", text: m("cc.estimated") });
  const k = cc.k ? ccNumber(cc.k) + DISPLAY_SEPARATOR + m("cc.k_origin_" + cc.k_source) : "—";
  return el(
    "div",
    { class: "cc-auth-summary" },
    el(
      "div",
      { class: "cc-auth-head" },
      heading && el("span", { class: "cc-auth-title", text: m("cc.auth_estimate_title") }),
      status,
      el("span", { class: "muted", text: cc.pools.join(", ") })
    ),
    el(
      "div",
      { class: "cc-auth-grid" },
      item(
        m("cc.total_5h"),
        cc.five_hour_total_usd > 0 ? ccUSD(cc.five_hour_total_usd) : m("cc.unknown"),
        cc.five_hour_total_usd > 0 ? "" : m("cc.five_hour_unknown_tip")
      ),
      item(m("cc.total_week"), ccUSD(cc.weekly_total_usd), m("cc.totals_from_" + cc.totals_from)),
      item(m("cc.k"), k, cc.k ? "" : m("cc.k_basis_none")),
      item(
        m("cc.remaining_week_5h"),
        ccUSD(cc.remaining_week_usd) + DISPLAY_SLASH + ccUSD(cc.remaining_five_hour_usd),
        m("cc.remaining_tip")
      )
    )
  );
}

async function ccRun(action) {
  if (ccState.busy) return;
  ccState.busy = true;
  updateCCButtons();
  try {
    await action();
    await ccQuery.read("cc", () => plugin("GET", "/cc"), true);
    await ccReadInsights(true);
  } finally {
    ccState.busy = false;
    renderCC();
  }
}

$("cc-save").onclick = () =>
  guard(() =>
    ccRun(async () => {
      await plugin("PUT", "/cc/settings", ccState.draft);
      ccState.dirty = false;
      updateRefreshControls();
      notify(m("cc.saved"));
    })
  );
$("cc-refresh").onclick = () => guard(() => ccRun(() => plugin("POST", "/cc/refresh")));
$("cc-rebuild").onclick = () =>
  guard(async () => {
    const confirmed = await openActionDialog(m("cc.rebuild"), m("cc.rebuild_confirm"), undefined, null, m("cc.rebuild"));
    if (confirmed) await ccRun(() => plugin("POST", "/cc/rebuild"));
  });

// Custom price import from a JSON price list.
$("price-import").onclick = () => $("price-import-file").click();
$("price-import-file").onchange = (event) =>
  guard(async () => {
    const file = event.target.files[0];
    event.target.value = "";
    if (!file) return;
    let body;
    try { body = JSON.parse(await file.text()); } catch (_) { throw new UIError(m("cc.price_import_invalid")); }
    const result = await plugin("POST", "/prices/import", body);
    notify(m("cc.price_import_done", { v0: result.models, v1: result.model_ids }));
    await loadDataGroup(false, "pricing", true);
  });

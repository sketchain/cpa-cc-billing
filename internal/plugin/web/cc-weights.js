// CC quota weights: models under test, their estimated, calibrated and manual
// multipliers, calibration runs and the raw/weighted comparison.
const CC_WEIGHT_FIELDS = [
  { name: "min_segments", label: "cc.weights_min_segments", hint: "cc.weights_hint_min_segments", step: "1" },
  { name: "min_cost_usd", label: "cc.weights_min_cost", hint: "cc.weights_hint_min_cost" },
  { name: "min_share_spread", label: "cc.weights_min_spread", hint: "cc.weights_hint_min_spread", percent: true }
];

function ccMultiplier(value, digits = 2) { return Number.isFinite(value) ? "×" + ccNumber(value, digits) : "—"; }

function ccNewItemID(weights) {
  for (let index = weights.items.length + 1; ; index++) {
    const id = "item-" + index;
    if (!weights.items.some((item) => item.id === id)) return id;
  }
}

function ccItemLabel(item) { return item.name || item.id; }

function ccModelLabel(model) { return model === "" ? m("cc.weights_unknown_model") : model; }

function ccWeightStatusTag(status) {
  return ccTag(m("cc.weights_status_" + status), status === "ok" ? "ok" : "warn", m("cc.weights_status_" + status + "_tip"));
}

// The multiplier text of an estimate or calibration result with its interval.
function ccIntervalText(result) {
  return (
    ccMultiplier(result.multiplier) + " ± " + ccNumber(result.std_err, 2) + DISPLAY_SEPARATOR +
    m("cc.weights_interval", { v0: ccNumber(result.low, 2), v1: ccNumber(result.high, 2) })
  );
}

// The price table's multiplier of an item: one value when its models agree.
function ccItemPrice(data, item) {
  const prices = item.models.map((model) => data.price_multipliers?.[model]).filter(Boolean);
  if (!prices.length) return { known: false };
  const input = prices[0].input, output = prices[0].output;
  const uniform = prices.every((price) => price.input === input && price.output === output);
  if (!uniform || !(input > 0) || !(output > 0)) return { known: false, custom: prices.some((price) => price.custom) };
  return { known: true, input, output, custom: prices.some((price) => price.custom) };
}

// The model quota weights of the provider on display: each provider's
// accounts have their own multipliers.
function ccProviderWeights(data) { return data.cc.weights?.[ccState.provider] || data.cc.weights?.claude; }

function ccWeightsSection(data, draft) {
  const weights = ccProviderWeights(data);
  const profile = ccProfile(draft);
  profile.weights ||= structuredClone(data.cc.settings.providers[ccState.provider].weights);
  const dw = profile.weights;
  dw.items ||= [];
  const saved = data.cc.settings.providers?.[ccState.provider]?.weights?.items || [];
  const estimates = new Map((weights.estimate.items || []).map((item) => [item.id, item]));
  const effective = new Map(weights.effective.map((item) => [item.id, item]));
  const owner = (model) => dw.items.find((item) => item.models.includes(model));
  const update = () => {
    ccMarkDirty();
    renderCC();
  };

  const toggle = el(
    "label",
    { class: "toggle-filter cc-param-switch" },
    switchControl({
      input: el("input", {
        type: "checkbox",
        role: "switch",
        id: "cc-weights-enabled",
        checked: !!dw.enabled,
        onchange: (event) => {
          dw.enabled = event.target.checked;
          ccMarkDirty();
        }
      })
    }),
    el("span", { text: m("cc.weights_enabled") })
  );
  const notices = [];
  if (weights.unsplit_segments > 0)
    notices.push(
      el("p", {
        class: "notice show cc-weights-unsplit",
        text: m("cc.weights_unsplit", { v0: weights.unsplit_segments, v1: weights.segments })
      })
    );

  // Models of the retained segments, with the items they belong to.
  const listed = weights.models.map((usage) => usage.model);
  const models = [
    ...weights.models,
    ...dw.items.flatMap((item) => item.models).filter((model) => !listed.includes(model))
      .map((model) => ({ model, cost_usd: 0, share: 0, segments: 0 }))
  ];
  const modelRows = models.map((usage) => {
    const item = owner(usage.model);
    const choices = [
      ["", m("cc.weights_baseline")],
      ...dw.items.map((option) => [option.id, ccItemLabel(option)]),
      ["+", m("cc.weights_new_item")]
    ];
    const select =
      usage.model === ""
        ? el("span", { class: "muted small", text: m("cc.weights_unknown_model_tip") })
        : ccSelect(
            item ? item.id : "",
            choices,
            (value) => {
              for (const other of dw.items) other.models = other.models.filter((model) => model !== usage.model);
              if (value === "+") dw.items.push({ id: ccNewItemID(dw), name: usage.model, models: [usage.model] });
              else if (value) dw.items.find((option) => option.id === value).models.push(usage.model);
              dw.items = dw.items.filter((option) => option.models.length);
            },
            { "aria-label": m("cc.weights_item_for", { v0: usage.model }) }
          );
    return el(
      "tr",
      {},
      el("td", { class: "cc-weights-model", text: ccModelLabel(usage.model) }),
      el("td", { text: ccUSD(usage.cost_usd) }),
      el("td", { text: ccPercent(usage.share * 100) }),
      el("td", { text: ccNumber(usage.segments, 0) }),
      el("td", {}, select)
    );
  });

  const itemRows = dw.items.map((item) => {
    const estimate = estimates.get(item.id);
    const weight = effective.get(item.id);
    const isSaved = saved.some((other) => other.id === item.id);
    const estimateCell =
      !isSaved || !estimate
        ? el("span", { class: "muted small", text: m("cc.save_to_see_status") })
        : el(
            "div",
            { class: "cc-weights-estimate" },
            estimate.status === "ok"
              ? el("div", { class: "cc-k-final", text: ccIntervalText(estimate) })
              : ccWeightStatusTag(estimate.status),
            el("div", {
              class: "muted small",
              text: m("cc.weights_estimate_detail", {
                v0: ccNumber(estimate.segments, 0),
                v1: ccUSD(estimate.cost_usd),
                v2: ccPercent(estimate.share_min * 100),
                v3: ccPercent(estimate.share_max * 100),
                v4: ccPercent(estimate.share_spread * 100)
              })
            })
          );
    const price = ccItemPrice(data, item);
    const current = price.known
      ? price.input === price.output
        ? ccMultiplier(price.input)
        : m("cc.weights_price_io", { v0: ccMultiplier(price.input), v1: ccMultiplier(price.output) })
      : "—";
    const suggested =
      weight && price.known ? ccMultiplier(weight.multiplier * price.input) : weight ? ccMultiplier(weight.multiplier) : "—";
    const effectiveCell =
      weight && isSaved
        ? el(
            "div",
            {},
            el("div", { class: "cc-k-final", text: ccMultiplier(weight.multiplier) }),
            ccTag(
              m("cc.weights_source_" + weight.source),
              weight.source === "default" ? "warn" : "info",
              weight.source === "default" ? m("cc.weights_default_tip", { v0: m("cc.weights_status_" + weight.status) }) : null
            )
          )
        : "—";
    return el(
      "tr",
      {},
      el(
        "td",
        {},
        el("input", {
          class: "cc-weights-name",
          value: item.name || "",
          placeholder: item.id,
          "aria-label": m("cc.weights_item_name"),
          oninput: (event) => {
            item.name = event.target.value;
            ccMarkDirty();
          }
        })
      ),
      el("td", {}, el("div", { class: "cc-weights-models" }, item.models.map((model) => ccTag(model)))),
      el("td", {}, estimateCell),
      el(
        "td",
        {},
        ccNumberInput(
          item.manual_multiplier,
          (value) => { if (value === null) delete item.manual_multiplier; else item.manual_multiplier = value; },
          { min: "0", max: "100", placeholder: m("cc.auto"), "aria-label": m("cc.weights_manual") }
        )
      ),
      el("td", {}, effectiveCell),
      el("td", {
        text: suggested,
        "data-tip": price.known ? m("cc.weights_suggested_tip", { v0: current }) : m("cc.weights_suggested_unknown_tip")
      }),
      el("td", {
        text: current,
        "data-tip": price.known
          ? price.custom ? m("cc.weights_price_custom_tip") : m("cc.weights_price_official_tip")
          : m("cc.weights_price_unknown_tip")
      }),
      el(
        "td",
        {},
        el("button", {
          type: "button",
          class: "danger small",
          text: m("cc.weights_delete_item"),
          onclick: () => {
            dw.items = dw.items.filter((other) => other !== item);
            update();
          }
        })
      )
    );
  });

  const fields = CC_WEIGHT_FIELDS.map((definition) => {
    const value = dw[definition.name];
    const fallback = { min_segments: 30, min_cost_usd: 20, min_share_spread: 0.05 }[definition.name];
    const shown = definition.percent ? Math.round(value * 10000) / 100 : value;
    return el(
      "label",
      { class: "cc-param", for: "cc-weights-" + definition.name },
      el("span", { class: "cc-param-label", text: m(definition.label) }),
      ccNumberInput(
        shown,
        (next) => { dw[definition.name] = next === null ? fallback : definition.percent ? next / 100 : next; },
        { id: "cc-weights-" + definition.name, step: definition.step || "any", min: "0" }
      ),
      el("span", {
        class: "muted small",
        text:
          m(definition.hint) + DISPLAY_SEPARATOR +
          m("cc.default_value", { v0: definition.percent ? ccNumber(fallback * 100) + "%" : ccNumber(fallback) })
      })
    );
  });

  return ccSection(
    m("cc.weights"),
    m("cc.weights_hint"),
    ...notices,
    el(
      "div",
      { class: "cc-param-toggle cc-weights-toggle" },
      toggle,
      el("p", { class: "muted small", text: m("cc.weights_enabled_hint") })
    ),
    el("h4", { text: m("cc.weights_models") }),
    el("p", { class: "muted small", text: m("cc.weights_models_hint") }),
    ccTable(
      [m("cc.weights_model"), m("cc.weights_cost"), m("cc.weights_share"), m("cc.weights_segments"), m("cc.weights_item")],
      modelRows,
      m("cc.weights_no_models")
    ),
    el("h4", { text: m("cc.weights_items") }),
    el("p", { class: "muted small", text: m("cc.weights_items_hint") }),
    ccTable(
      [
        m("cc.weights_item"),
        m("cc.weights_item_models"),
        m("cc.weights_estimate"),
        m("cc.weights_manual"),
        m("cc.weights_effective"),
        m("cc.weights_suggested"),
        m("cc.weights_current_price"),
        ""
      ],
      itemRows,
      m("cc.weights_no_items")
    ),
    el("div", { class: "cc-params cc-weights-params" }, fields),
    ccCalibrationBlock(data, saved),
    ccComparisonBlock(data)
  );
}

const ccCalibrationChoice = { item: "", account: "" };

function ccCalibrationBlock(data, saved) {
  const calibrations = ccProviderWeights(data).calibrations;
  const accounts = data.cc.accounts.filter((account) => (account.provider || "claude") === ccState.provider)
    .map((account) => account.auth_index);
  if (!saved.some((item) => item.id === ccCalibrationChoice.item)) ccCalibrationChoice.item = saved[0]?.id || "";
  if (!accounts.includes(ccCalibrationChoice.account)) ccCalibrationChoice.account = accounts[0] || "";
  const running = new Set(calibrations.filter((item) => !item.ended_at).map((item) => item.auth_index));
  const pick = (field, value) => { ccCalibrationChoice[field] = value; };
  const plainSelect = (value, options, field, label) =>
    el(
      "span",
      { class: "native-select" },
      el(
        "select",
        {
          "aria-label": label,
          onchange: (event) => {
            pick(field, event.target.value);
            renderCC();
          }
        },
        options.map(([optionValue, text]) => el("option", { value: optionValue, text, selected: optionValue === value }))
      )
    );
  const canStart = ccCalibrationChoice.item && ccCalibrationChoice.account && !running.has(ccCalibrationChoice.account);
  const controls = el(
    "div",
    { class: "row cc-calibration-controls" },
    plainSelect(ccCalibrationChoice.item, saved.map((item) => [item.id, ccItemLabel(item)]), "item", m("cc.weights_item")),
    plainSelect(ccCalibrationChoice.account, accounts.map((index) => [index, ccCredentialName(data, index)]), "account", m("cc.account")),
    el("button", {
      type: "button",
      id: "cc-calibration-start",
      text: m("cc.calibration_start"),
      disabled: !canStart || ccState.busy,
      "data-tip": running.has(ccCalibrationChoice.account) ? m("cc.calibration_running_tip") : null,
      onclick: () =>
        guard(() =>
          ccRun(() => plugin("POST", "/cc/calibrations", { item_id: ccCalibrationChoice.item, auth_index: ccCalibrationChoice.account }))
        )
    })
  );
  const rows = calibrations.map((calibration) => {
    const result = calibration.result;
    const status = calibration.ended_at
      ? result.status === "ok"
        ? el("div", { class: "cc-k-final", text: ccIntervalText(result) })
        : ccTag(m("cc.calibration_insufficient"), "warn", m("cc.calibration_insufficient_tip"))
      : ccTag(m("cc.calibration_running"), "info");
    return el(
      "tr",
      {},
      el("td", { text: ccItemLabel(calibration.item) }),
      el("td", { text: ccCredentialName(data, calibration.auth_index) }),
      el("td", { text: ccWhen(calibration.started_at) + " → " + (calibration.ended_at ? ccWhen(calibration.ended_at) : "…") }),
      el("td", {}, status),
      el("td", {
        text: m("cc.calibration_pure_value", {
          v0: result.item_segments,
          v1: result.item_usd_per_point ? ccUSD(result.item_usd_per_point) : "—",
          v2: result.base_segments,
          v3: result.base_usd_per_point ? ccUSD(result.base_usd_per_point) : "—"
        }),
        "data-tip": m("cc.calibration_base_" + (result.base_from || "period"))
      }),
      el("td", { text: m("cc.calibration_excluded_value", { v0: result.excluded_segments, v1: result.unsplit_segments }) }),
      el(
        "td",
        { class: "cc-calibration-actions" },
        !calibration.ended_at &&
          el("button", {
            type: "button",
            class: "small",
            text: m("cc.calibration_stop"),
            onclick: () => guard(() => ccRun(() => plugin("POST", "/cc/calibrations/stop", { id: calibration.id })))
          }),
        el("button", {
          type: "button",
          class: "danger small",
          text: m("cc.calibration_delete"),
          onclick: () =>
            guard(async () => {
              const confirmed = await openActionDialog(
                m("cc.calibration_delete"),
                m("cc.calibration_delete_confirm"),
                undefined,
                null,
                m("cc.calibration_delete")
              );
              if (confirmed) await ccRun(() => plugin("DELETE", "/cc/calibrations?id=" + encodeURIComponent(calibration.id)));
            })
        })
      )
    );
  });
  return el(
    "div",
    { class: "cc-calibrations" },
    el("h4", { text: m("cc.calibrations") }),
    el("p", { class: "muted small", text: m("cc.calibrations_hint") }),
    saved.length && accounts.length ? controls : el("p", { class: "muted small", text: m("cc.calibration_needs_items") }),
    ccTable(
      [
        m("cc.weights_item"),
        m("cc.account"),
        m("cc.calibration_period"),
        m("cc.calibration_result"),
        m("cc.calibration_pure"),
        m("cc.calibration_excluded"),
        ""
      ],
      rows,
      m("cc.calibration_none")
    )
  );
}

function ccComparisonBlock(data) {
  const weights = ccProviderWeights(data);
  const active = weights.basis;
  const cells = (values, basis) => {
    const attrs = { class: basis === active ? "cc-basis-active" : "" };
    return [
      el("td", { ...attrs, text: values.usd_per_point > 0 ? ccUSD(values.usd_per_point) : "—" }),
      el("td", { ...attrs, text: ccUSD(values.weekly_usd), "data-tip": m("cc.totals_from_" + values.totals_from) }),
      el("td", { ...attrs, text: m("cc.segments_value", { v0: values.segments - values.excluded_segments, v1: values.excluded_segments }) })
    ];
  };
  const rows = weights.comparison.map((row) =>
    el("tr", {}, el("td", { text: ccCredentialName(data, row.auth_index) }), ...cells(row.raw, "raw"), ...cells(row.weighted, "weighted"))
  );
  return el(
    "div",
    { class: "cc-weights-comparison" },
    el("h4", { text: m("cc.weights_comparison") }),
    el("p", { class: "muted small", text: m("cc.weights_comparison_hint", { v0: m("cc.weights_basis_" + active) }) }),
    ccTable(
      [
        m("cc.account"),
        m("cc.weights_raw_ratio"),
        m("cc.weights_raw_week"),
        m("cc.weights_raw_segments"),
        m("cc.weights_weighted_ratio"),
        m("cc.weights_weighted_week"),
        m("cc.weights_weighted_segments")
      ],
      rows,
      m("cc.no_provider_accounts", { v0: ccProviderLabel(ccState.provider) })
    )
  );
}

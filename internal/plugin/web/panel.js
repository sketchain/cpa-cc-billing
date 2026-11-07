// Page display settings: which management tabs are shown and whether API key
// users may open their self-service view. Hiding a tab never disables its API.
const panelQuery = createQuery(m("panel.title"));
const panelState = { busy: false };
const PANEL_TAB_LABELS = {
  keys: () => "API Key",
  analysis: () => m("ui.analysis"),
  "request-events": () => m("ui.request_events"),
  errors: () => m("ui.error_events"),
  "auth-files": () => m("ui.auth_files"),
  "ai-providers": () => m("ui.ai_providers"),
  split: () => m("cc.tab")
};

function readPanelSettings(reload = false) { return panelQuery.read("panel", () => plugin("GET", "/panel-settings"), reload); }

// Until the settings load, or when they cannot, every tab stays visible.
function hiddenAdminTabs() { return new Set(panelQuery.value?.settings.hidden_tabs || []); }

function firstVisibleAdminTab() {
  const hidden = hiddenAdminTabs();
  return ADMIN_TAB_IDS.find((tab) => !hidden.has(tab));
}

function applyAdminTabVisibility() {
  const hidden = hiddenAdminTabs();
  for (const tab of TABS) tab.classList.toggle("hidden", hidden.has(tab.dataset.tab));
  const select = $("page-tab-select");
  select.replaceChildren(
    ...TABS.filter((tab) => !hidden.has(tab.dataset.tab)).map((tab) =>
      el("option", { value: tab.dataset.tab }, window.billingI18n.boundText(tab))
    )
  );
  select.value = activeTab(false);
  if (currentRole === "admin" && hidden.has(activeTab(false))) {
    setAdminTab(firstVisibleAdminTab());
    guard(() => loadPage(false));
  }
  schedulePageTabLayout();
}

async function savePanelSettings(patch) {
  if (panelState.busy || !panelQuery.value) return;
  panelState.busy = true;
  renderPanelSettings();
  try {
    const result = await plugin("PUT", "/panel-settings", { ...panelQuery.value.settings, ...patch });
    panelQuery.value = result;
    notify(m("panel.saved"));
  } finally {
    panelState.busy = false;
    renderPanelSettings();
    applyAdminTabVisibility();
  }
}

function renderPanelSettings() {
  const target = $("panel-body");
  if (renderDataPending(target, [panelQuery])) return;
  const { settings, tabs } = panelQuery.value;
  const hidden = new Set(settings.hidden_tabs);
  const tabChoice = (tab) =>
    el(
      "label",
      { class: "cc-check" },
      checkboxControl({
        input: el("input", {
          type: "checkbox",
          checked: hidden.has(tab),
          disabled: panelState.busy,
          onchange: (event) =>
            guard(() =>
              savePanelSettings({
                hidden_tabs: event.target.checked ? [...hidden, tab] : settings.hidden_tabs.filter((item) => item !== tab)
              })
            )
        })
      }),
      el("span", { text: PANEL_TAB_LABELS[tab]?.() || tab })
    );
  target.replaceChildren(
    el(
      "div",
      { class: "panel-setting" },
      el("div", { class: "panel-setting-label", text: m("panel.hidden_tabs") }),
      el("p", { class: "muted small", text: m("panel.hidden_tabs_hint") }),
      el("div", { class: "cc-checks" }, tabs.map(tabChoice))
    ),
    el(
      "div",
      { class: "panel-setting" },
      el(
        "label",
        { class: "toggle-filter" },
        switchControl({
          input: el("input", {
            type: "checkbox",
            role: "switch",
            checked: settings.account_self_service,
            disabled: panelState.busy,
            onchange: (event) => guard(() => savePanelSettings({ account_self_service: event.target.checked }))
          })
        }),
        el("span", { text: m("panel.account_self_service") })
      ),
      el("p", { class: "muted small", text: m("panel.account_self_service_hint") })
    )
  );
}

{
  const page = pages.admin.settings;
  const loadList = page.load;
  page.sections.unshift({ sources: [panelQuery], render: renderPanelSettings, displayed: null });
  page.sources.push(panelQuery);
  page.queries.push(panelQuery);
  page.load = (reload) => settleLoads([loadList(reload), readPanelSettings(reload)]);
}

// Without self-service, the page offers only the management key sign-in.
if (!ACCOUNT_ACCESS) {
  $("gate-mode-account").closest(".segmented-control").classList.add("hidden");
  $("gate-title").dataset.i18n = "panel.sign_in_title";
  setText($("gate-title"), m("panel.sign_in_title"));
}

package plugin

import (
	"bytes"
	"net/http"
	"slices"
	"testing"
	"time"

	"cpa-super-billing/internal/ccbilling"
)

type panelResponse struct {
	Settings ccbilling.PanelSettings `json:"settings"`
	Tabs     []string                `json:"tabs"`
}

func servedUI(t *testing.T, app *App) []byte {
	t.Helper()
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, ManagementRequest{Method: http.MethodGet, Path: resourceBase + resourceUIPath}))
	if err != nil {
		t.Fatal(err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("UI status = %d", response.StatusCode)
	}
	return response.Body
}

func TestPanelSettingsDefaultsAndValidation(t *testing.T) {
	app := newConfiguredApp(t)
	var got panelResponse
	callOK(t, app, http.MethodGet, routePanel, nil, nil, http.StatusOK, &got)
	if !slices.Equal(got.Settings.HiddenTabs, []string{"ai-providers"}) || !got.Settings.AccountSelfService || slices.Contains(got.Tabs, "settings") {
		t.Fatalf("defaults = %+v", got)
	}
	callOK(t, app, http.MethodPut, routePanel, nil, map[string]any{"hidden_tabs": []string{"settings"}, "account_self_service": true},
		http.StatusBadRequest, nil)
	callOK(t, app, http.MethodPut, routePanel, nil, map[string]any{"hidden_tabs": []string{"cc", "keys", "cc"}, "account_self_service": true},
		http.StatusOK, &got)
	// "cc" is the former id of the subscription split tab.
	if !slices.Equal(got.Settings.HiddenTabs, []string{"keys", "split"}) {
		t.Fatalf("hidden tabs = %v, want page order without duplicates", got.Settings.HiddenTabs)
	}
	// Hidden pages only affect the page: their management routes still answer.
	callOK(t, app, http.MethodGet, routeKeys, nil, nil, http.StatusOK, nil)
	callOK(t, app, http.MethodGet, routeCC, nil, nil, http.StatusOK, nil)
}

func TestPanelSettingsSurviveRestart(t *testing.T) {
	app, path := newAppWithPriceAndState(t, true)
	callOK(t, app, http.MethodPut, routePanel, nil, ccbilling.PanelSettings{HiddenTabs: []string{}, AccountSelfService: false}, http.StatusOK, nil)
	app.Shutdown()

	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	if _, err := restarted.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	})); err != nil {
		t.Fatal(err)
	}
	var got panelResponse
	callOK(t, restarted, http.MethodGet, routePanel, nil, nil, http.StatusOK, &got)
	if len(got.Settings.HiddenTabs) != 0 || got.Settings.AccountSelfService {
		t.Fatalf("restored settings = %+v", got.Settings)
	}
}

// Page settings saved while the subscription split was the "cc" tab keep it
// hidden under its new id.
func TestPanelSettingsMoveTheFormerSplitTab(t *testing.T) {
	app, path := newAppWithPriceAndState(t, true)
	app.Shutdown()
	execSQL(t, path, `INSERT INTO cc_meta (key, value) VALUES ('panel_settings', '{"hidden_tabs":["cc","ai-providers"],"account_self_service":true}')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	if err := registerState(t, restarted, path); err != nil {
		t.Fatal(err)
	}
	var got panelResponse
	callOK(t, restarted, http.MethodGet, routePanel, nil, nil, http.StatusOK, &got)
	if !slices.Equal(got.Settings.HiddenTabs, []string{"ai-providers", "split"}) {
		t.Fatalf("hidden tabs = %v", got.Settings.HiddenTabs)
	}
}

func TestAccountSelfServiceSwitch(t *testing.T) {
	app, clock := newCCTestApp(t)
	ccUsage(t, app, ccRemainderKey, 0.5, 0.10, 0.20, clock.now.Add(3*time.Hour), clock.now.Add(4*24*time.Hour))
	if !bytes.Contains(servedUI(t, app), uiAccountAccessOn) {
		t.Fatal("UI does not offer API key access by default")
	}
	for _, path := range []string{routeProfile, routeSubscription} {
		if response := callAccount(t, app, path, ccRemainderKey, nil); response.StatusCode != http.StatusOK {
			t.Fatalf("%s with self-service on = %d (%s)", path, response.StatusCode, response.Body)
		}
	}

	callOK(t, app, http.MethodPut, routePanel, nil, ccbilling.PanelSettings{HiddenTabs: []string{"ai-providers"}, AccountSelfService: false},
		http.StatusOK, nil)
	if ui := servedUI(t, app); !bytes.Contains(ui, uiAccountAccessOff) || bytes.Contains(ui, uiAccountAccessOn) {
		t.Fatal("UI still offers API key access")
	}
	for _, endpoint := range resourceEndpoints {
		if endpoint.path == routeBalance {
			continue
		}
		if response := callAccount(t, app, endpoint.path, ccRemainderKey, nil); response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with self-service off = %d (%s), want 404", endpoint.path, response.StatusCode, response.Body)
		}
	}
	if status, balance := ccBalance(t, app, ccRemainderKey); status != http.StatusOK || balance <= 0 {
		t.Fatalf("balance with self-service off = %d %v", status, balance)
	}
	if status, _ := ccBalance(t, app, "sk-unknown-key-0001"); status != http.StatusUnauthorized {
		t.Fatalf("balance of an unknown key = %d, want 401", status)
	}

	callOK(t, app, http.MethodPut, routePanel, nil, ccbilling.PanelSettings{AccountSelfService: true}, http.StatusOK, nil)
	if response := callAccount(t, app, routeProfile, ccRemainderKey, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("profile after turning self-service back on = %d", response.StatusCode)
	}
}

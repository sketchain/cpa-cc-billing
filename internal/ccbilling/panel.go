package ccbilling

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Management page tabs an operator may hide. Settings is always shown.
var PanelTabs = []string{"keys", "analysis", "request-events", "errors", "auth-files", "ai-providers", "split"}

// legacyPanelTabs maps former tab ids to the current ones: the subscription
// split page was the "cc" tab.
var legacyPanelTabs = map[string]string{"cc": "split"}

func currentPanelTab(tab string) string {
	if current, ok := legacyPanelTabs[tab]; ok {
		return current
	}
	return tab
}

// PanelSettings are management page preferences shared by every operator.
// They only change what the page shows and whether API key users may open
// their self-service view; the management API is unaffected.
type PanelSettings struct {
	HiddenTabs         []string `json:"hidden_tabs"`
	AccountSelfService bool     `json:"account_self_service"`
}

// storedPanelSettings leaves fields unset that a database written by an
// older version lacks, so they keep their defaults.
type storedPanelSettings struct {
	HiddenTabs         *[]string `json:"hidden_tabs,omitempty"`
	AccountSelfService *bool     `json:"account_self_service,omitempty"`
}

const metaPanelSettings = "panel_settings"

func DefaultPanelSettings() PanelSettings {
	return PanelSettings{HiddenTabs: []string{"ai-providers"}, AccountSelfService: true}
}

// Normalized validates the settings and returns a canonical copy, with the
// hidden tabs in page order.
func (s PanelSettings) Normalized() (PanelSettings, error) {
	hidden := []string{}
	tabs := make([]string, 0, len(s.HiddenTabs))
	for _, tab := range s.HiddenTabs {
		tabs = append(tabs, currentPanelTab(tab))
	}
	for _, tab := range tabs {
		if !slices.Contains(PanelTabs, tab) {
			return PanelSettings{}, fmt.Errorf("Unknown page %q; the settings page cannot be hidden", tab)
		}
	}
	for _, tab := range PanelTabs {
		if slices.Contains(tabs, tab) {
			hidden = append(hidden, tab)
		}
	}
	return PanelSettings{HiddenTabs: hidden, AccountSelfService: s.AccountSelfService}, nil
}

func loadPanelSettings(db *sql.DB) (PanelSettings, error) {
	settings := DefaultPanelSettings()
	var raw string
	switch err := db.QueryRow(`SELECT value FROM cc_meta WHERE key = ?`, metaPanelSettings).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return settings, nil
	case err != nil:
		return PanelSettings{}, fmt.Errorf("Read page settings: %w", err)
	}
	var stored storedPanelSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return PanelSettings{}, fmt.Errorf("Read page settings: %w", err)
	}
	if stored.HiddenTabs != nil {
		// A page a later version added and this one lacks is dropped.
		settings.HiddenTabs = slices.DeleteFunc(*stored.HiddenTabs, func(tab string) bool { return !slices.Contains(PanelTabs, currentPanelTab(tab)) })
	}
	if stored.AccountSelfService != nil {
		settings.AccountSelfService = *stored.AccountSelfService
	}
	return settings.Normalized()
}

func savePanelSettings(db *sql.DB, settings PanelSettings) error {
	raw, err := json.Marshal(storedPanelSettings{HiddenTabs: &settings.HiddenTabs, AccountSelfService: &settings.AccountSelfService})
	if err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO cc_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaPanelSettings, string(raw)); err != nil {
		return fmt.Errorf("Save page settings: %w", err)
	}
	return nil
}

// PanelSettings returns the page settings, reading them when the database
// was opened. Before the database can be read it returns the defaults.
func (m *Manager) PanelSettings() PanelSettings {
	if settings := m.panel.Load(); settings != nil {
		return *settings
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(func(*sql.DB) error { return nil }); err != nil {
		return DefaultPanelSettings()
	}
	if settings := m.panel.Load(); settings != nil {
		return *settings
	}
	return DefaultPanelSettings()
}

// SetPanelSettings validates and stores new page settings.
func (m *Manager) SetPanelSettings(next PanelSettings) (PanelSettings, error) {
	normalized, err := next.Normalized()
	if err != nil {
		return PanelSettings{}, err
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(func(db *sql.DB) error { return savePanelSettings(db, normalized) }); err != nil {
		return PanelSettings{}, err
	}
	m.panel.Store(&normalized)
	return normalized, nil
}

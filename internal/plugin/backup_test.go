package plugin

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-super-billing/internal/billing"
)

func registerState(t *testing.T, app *App, path string) error {
	t.Helper()
	_, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + path + "\"\n"),
	}))
	return err
}

func execSQL(t *testing.T, path, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
}

func pluginLogMessages(t *testing.T, app *App) []string {
	t.Helper()
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	messages := []string{}
	for _, entry := range page.Entries {
		messages = append(messages, string(entry.Level)+": "+entry.Message)
	}
	return messages
}

// Registering on a database of an older format backs it up beside itself and
// records where in the plugin log before the migration runs.
func TestRegisterBacksUpTheDatabaseBeforeMigrating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpa-super-billing-state-v1.db")
	first := newTestApp(t)
	if err := registerState(t, first, path); err != nil {
		t.Fatal(err)
	}
	first.Shutdown()
	execSQL(t, path, `ALTER TABLE plans DROP COLUMN shared_quota; ALTER TABLE plans DROP COLUMN cycles_json; PRAGMA user_version = 24;`)

	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if err := registerState(t, app, path); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "cpa-super-billing-state-v1.db.bak-u24-to-u25-cc4-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backups = %v (%v)", matches, err)
	}
	logged := false
	for _, message := range pluginLogMessages(t, app) {
		if strings.HasPrefix(message, "info: Backed up billing database "+path) && strings.Contains(message, matches[0]) {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("plugin logs = %v", pluginLogMessages(t, app))
	}
	if version := sqliteUserVersion(t, path); version != 25 {
		t.Fatalf("user_version = %d", version)
	}

	// Opening it again needs no migration and takes no backup.
	app.Shutdown()
	again := newTestApp(t)
	t.Cleanup(again.Shutdown)
	if err := registerState(t, again, path); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.bak-*")); len(matches) != 1 {
		t.Fatalf("backups = %v", matches)
	}
}

// A damaged database fails registration with the original error, the backups
// it can be restored from, and how to salvage it.
func TestRegisterExplainsADamagedDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	first := newTestApp(t)
	if err := registerState(t, first, path); err != nil {
		t.Fatal(err)
	}
	first.Shutdown()
	execSQL(t, path, `PRAGMA writable_schema = ON; UPDATE sqlite_master SET rootpage = 99999 WHERE name = 'cc_segment_costs'; PRAGMA writable_schema = OFF;`)
	backup := "state.db.bak-u24-to-u25-cc4-20261001T000000"
	if err := os.WriteFile(filepath.Join(dir, backup), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := registerState(t, newTestApp(t), path)
	if err == nil {
		t.Fatal("registration accepted a damaged database")
	}
	for _, part := range []string{"invalid rootpage", path, backup, `".recover"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q lacks %q", err, part)
		}
	}
}

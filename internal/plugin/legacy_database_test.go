package plugin

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/sqlite"
)

// newMigratingApp is a test app that adopts the former plugin's database, as
// the plugin does. Create it before changing the working directory: it reads
// test data by relative path.
func newMigratingApp(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t)
	app.store.SetDatabaseCopier(sqlite.CopyDatabase)
	return app
}

func registerConfig(t *testing.T, app *App, config string) error {
	t.Helper()
	_, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: []byte(config)}))
	return err
}

// writeLegacyDatabase creates a database at path, as the former plugin left
// it, holding one marker row.
func writeLegacyDatabase(t *testing.T, app *App, path, marker string) {
	t.Helper()
	if err := registerState(t, app, path); err != nil {
		t.Fatal(err)
	}
	app.Shutdown()
	execSQL(t, path, `CREATE TABLE test_marker (value TEXT NOT NULL); INSERT INTO test_marker (value) VALUES ('`+marker+`');`)
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM test_marker`).Scan(&value); err != nil {
		return ""
	}
	return value
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUnsetStateFileAdoptsACopyOfTheFormerPluginsDatabase(t *testing.T) {
	writer, app := newTestApp(t), newMigratingApp(t)
	t.Chdir(t.TempDir())
	writeLegacyDatabase(t, writer, billing.LegacyStateFile, "from cpa-cc-billing")
	before := readFile(t, billing.LegacyStateFile)

	t.Cleanup(app.Shutdown)
	if err := registerConfig(t, app, "enabled: true\n"); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(t, billing.DefaultStateFile); got != "from cpa-cc-billing" {
		t.Fatalf("new database marker = %q, want the old database's rows", got)
	}
	if !bytes.Equal(before, readFile(t, billing.LegacyStateFile)) {
		t.Fatal("the old database changed")
	}
	if _, err := os.Stat(billing.DefaultStateFile + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial copy left behind: %v", err)
	}
	info, err := os.Stat(billing.DefaultStateFile)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("copy mode = %v, %v; want private", info.Mode(), err)
	}
	logs := strings.Join(pluginLogMessages(t, app), "\n")
	if !strings.Contains(logs, "info: Copied the cpa-cc-billing database") || !strings.Contains(logs, "left untouched") {
		t.Fatalf("plugin log does not report the copy:\n%s", logs)
	}
	notice, ok := app.store.LegacyDatabaseNotice()
	legacy, _ := filepath.Abs(billing.LegacyStateFile)
	if !ok || notice.Path != legacy || notice.CopiedAt.IsZero() {
		t.Fatalf("notice = %+v, %v; want the copied old database", notice, ok)
	}
	response := app.ccView(ManagementRequest{})
	if !strings.Contains(string(response.Body), `"legacy_database":{"path":"`+legacy) {
		t.Fatalf("CC view does not show the old database: %s", response.Body)
	}
}

func TestExplicitStateFileIsUsedAsConfigured(t *testing.T) {
	writer, app := newTestApp(t), newMigratingApp(t)
	t.Chdir(t.TempDir())
	writeLegacyDatabase(t, writer, billing.LegacyStateFile, "old")

	t.Cleanup(app.Shutdown)
	if err := registerConfig(t, app, "enabled: true\nstate_file: \""+billing.DefaultStateFile+"\"\n"); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(t, billing.DefaultStateFile); got != "" {
		t.Fatalf("explicit state_file adopted the old database (%q)", got)
	}
	if notice, ok := app.store.LegacyDatabaseNotice(); ok {
		t.Fatalf("explicit state_file reported the old database: %+v", notice)
	}
}

func TestExistingDefaultDatabaseWinsOverTheFormerOne(t *testing.T) {
	oldWriter, newWriter, app := newTestApp(t), newTestApp(t), newMigratingApp(t)
	t.Chdir(t.TempDir())
	writeLegacyDatabase(t, oldWriter, billing.LegacyStateFile, "old")
	writeLegacyDatabase(t, newWriter, billing.DefaultStateFile, "new")
	before := readFile(t, billing.LegacyStateFile)

	t.Cleanup(app.Shutdown)
	if err := registerConfig(t, app, "enabled: true\n"); err != nil {
		t.Fatal(err)
	}
	if got := readMarker(t, billing.DefaultStateFile); got != "new" {
		t.Fatalf("default database marker = %q, want the existing new database", got)
	}
	if !bytes.Equal(before, readFile(t, billing.LegacyStateFile)) {
		t.Fatal("the old database changed")
	}
	notice, ok := app.store.LegacyDatabaseNotice()
	if !ok || !notice.CopiedAt.IsZero() {
		t.Fatalf("notice = %+v, %v; want the old database reported as not copied now", notice, ok)
	}
	if logs := strings.Join(pluginLogMessages(t, app), "\n"); strings.Contains(logs, "Copied the cpa-cc-billing database") {
		t.Fatalf("plugin log reports a copy that did not happen:\n%s", logs)
	}
}

func TestFailedCopyLeavesTheFormerDatabaseAndRefusesToStart(t *testing.T) {
	writer, app := newTestApp(t), newTestApp(t)
	app.store.SetDatabaseCopier(func(string, string) error { return errors.New("disk full") })
	t.Chdir(t.TempDir())
	writeLegacyDatabase(t, writer, billing.LegacyStateFile, "old")
	before := readFile(t, billing.LegacyStateFile)

	t.Cleanup(app.Shutdown)
	err := registerConfig(t, app, "enabled: true\n")
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "was not copied") {
		t.Fatalf("register error = %v, want the copy failure", err)
	}
	if _, errStat := os.Stat(billing.DefaultStateFile); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("a new database was created after the copy failed: %v", errStat)
	}
	if !bytes.Equal(before, readFile(t, billing.LegacyStateFile)) {
		t.Fatal("the old database changed")
	}
}

func TestNoFormerDatabaseStartsFresh(t *testing.T) {
	app := newMigratingApp(t)
	t.Chdir(t.TempDir())
	t.Cleanup(app.Shutdown)
	if err := registerConfig(t, app, "enabled: true\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(billing.DefaultStateFile); err != nil {
		t.Fatal(err)
	}
	if notice, ok := app.store.LegacyDatabaseNotice(); ok {
		t.Fatalf("notice without an old database: %+v", notice)
	}
}

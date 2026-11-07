package sqlite

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
	"cpa-super-billing/internal/ccbilling"
)

var backupClock = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func atTime(at time.Time) Option {
	return func(options *openOptions) { options.now = func() time.Time { return at } }
}

func withCC() Option { return WithCCSchema(ccbilling.SchemaVersion, ccbilling.EnsureSchema) }

func openWith(t *testing.T, path string, opts ...Option) *DB {
	t.Helper()
	database, err := Open(path, nil, opts...)
	if err != nil {
		t.Fatalf("Open error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// seedV24 leaves a database at billing format 24 with current CC tables,
// holding one key and three request events, one of them failed and one all
// zero.
func seedV24(t *testing.T, path string) {
	t.Helper()
	database := openWith(t, path, withCC())
	state := billing.NewState()
	state.Keys["scope-a"] = &billing.KeyState{Preview: "sk-tes…0001", Label: "Alice"}
	normal := requestEvent("scope-a", eventStart)
	zero := billing.RequestEvent{At: eventStart.Add(time.Second), Scope: "scope-a"}
	failed := billing.RequestEvent{At: eventStart.Add(2 * time.Second), Scope: "scope-a", Failed: true}
	mustSave(t, database, state, billing.Changes{AllKeys: true, NormalRequestEvents: []billing.RequestEvent{normal, zero},
		RequestErrorEvents: []billing.RequestErrorEvent{{Event: failed, Error: billing.RequestError{StatusCode: 502, Body: "kept"}}}})
	if _, err := database.db.Exec(`
		ALTER TABLE plans DROP COLUMN shared_quota;
		ALTER TABLE plans DROP COLUMN cycles_json;
		PRAGMA user_version = 24;`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func readOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	handle, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}

func intQuery(t *testing.T, handle *sql.DB, query string) int {
	t.Helper()
	var value int
	if err := handle.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func backupNames(t *testing.T, path string) []string {
	t.Helper()
	backups, err := migrationBackups(path)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, backup := range backups {
		names = append(names, backup.name)
	}
	return names
}

func TestOpenBacksUpTheWholeDatabaseBeforeMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpa-super-billing-state-v1.db")
	seedV24(t, path)

	database := openWith(t, path, withCC(), atTime(backupClock))
	want := "cpa-super-billing-state-v1.db.bak-u24-to-u25-cc4-20261107T120000"
	if names := backupNames(t, path); !slices.Equal(names, []string{want}) {
		t.Fatalf("backups = %v, want [%s]", names, want)
	}
	backup := filepath.Join(filepath.Dir(path), want)
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", mode)
	}
	if _, err := os.Stat(backup + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial copy left behind: %v", err)
	}

	// The backup is the database as it was, complete and at its old format.
	copy := readOnly(t, backup)
	var check string
	if err := copy.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("backup integrity = %q, %v", check, err)
	}
	if version := intQuery(t, copy, "PRAGMA user_version"); version != 24 {
		t.Fatalf("backup user_version = %d, want 24", version)
	}
	if columns := intQuery(t, copy, `SELECT count(*) FROM pragma_table_info('plans') WHERE name = 'shared_quota'`); columns != 0 {
		t.Fatal("backup was taken after the migration")
	}
	for _, query := range []string{
		"SELECT count(*) FROM request_events",
		"SELECT count(*) FROM request_events WHERE failed = 1",
		"SELECT count(*) FROM api_keys",
	} {
		if got, want := intQuery(t, copy, query), intQuery(t, database.db, query); got != want || got == 0 {
			t.Fatalf("%s: backup %d, migrated %d", query, got, want)
		}
	}
	if raw := intQuery(t, copy, `SELECT CAST(value AS INTEGER) FROM cc_meta WHERE key = 'schema_version'`); raw != ccbilling.SchemaVersion {
		t.Fatalf("backup CC version = %d", raw)
	}
	if version := intQuery(t, database.db, "PRAGMA user_version"); version != schemaVersion {
		t.Fatalf("migrated user_version = %d", version)
	}
	notices := database.OpenNotices()
	if len(notices) != 1 || notices[0].Level != billing.PluginLogInfo || !strings.Contains(notices[0].Message, backup) ||
		!strings.Contains(notices[0].Message, "u24-to-u25-cc4") {
		t.Fatalf("notices = %+v", notices)
	}

	// A restored backup opens and migrates again.
	restored := filepath.Join(t.TempDir(), "restored.db")
	raw, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restored, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	view := mustQueryRequestEvents(t, openWith(t, restored, withCC(), atTime(backupClock)), billing.RequestEventQuery{})
	if view.Total != 3 || view.Statuses.Failed != 1 {
		t.Fatalf("restored history = %+v", view)
	}
}

func TestOpenBacksUpBeforeMigratingOnlyTheCCTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database := openWith(t, path, withCC())
	if _, err := database.db.Exec(`DROP TABLE cc_block_counts; UPDATE cc_meta SET value = '3' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database = openWith(t, path, withCC(), atTime(backupClock))
	if names := backupNames(t, path); !slices.Equal(names, []string{"state.db.bak-u25-cc3-to-cc4-20261107T120000"}) {
		t.Fatalf("backups = %v", names)
	}
	if tables := intQuery(t, database.db, `SELECT count(*) FROM sqlite_master WHERE name = 'cc_block_counts'`); tables != 1 {
		t.Fatal("CC tables were not migrated")
	}
}

func TestOpenTakesNoBackupWithoutAMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	// A new database, a current one, and a billing database copied from
	// cpa-key-billing that only gains the CC tables are not migrations.
	database := openWith(t, path, withCC(), atTime(backupClock))
	if len(database.OpenNotices()) != 0 {
		t.Fatalf("notices = %+v", database.OpenNotices())
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	openWith(t, path, withCC(), atTime(backupClock)).Close()

	upstream := filepath.Join(dir, "upstream.db")
	openWith(t, upstream).Close()
	openWith(t, upstream, withCC(), atTime(backupClock)).Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".bak-") {
			t.Fatalf("unexpected backup %s", entry.Name())
		}
	}
}

func TestOpenKeepsTheNewestThreeMigrationBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	seedV24(t, path)
	older := []string{
		"state.db.bak-u22-to-u25-cc3-to-cc4-20260101T000000",
		"state.db.bak-u23-to-u25-cc4-20260301T000000",
		"state.db.bak-u24-to-u25-cc4-20260501T000000",
		"state.db.bak-u24-to-u25-cc4-20260501T000000-2",
	}
	unrelated := []string{"state.db.bak-manual", "other.db.bak-u24-to-u25-cc4-20250101T000000", "state.db-wal.bak"}
	for _, name := range append(slices.Clone(older), unrelated...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	database := openWith(t, path, withCC(), atTime(backupClock))
	want := []string{"state.db.bak-u24-to-u25-cc4-20261107T120000", older[3], older[2]}
	if names := backupNames(t, path); !slices.Equal(names, want) {
		t.Fatalf("backups = %v, want %v", names, want)
	}
	for _, name := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %s: %v", name, err)
		}
	}
	removed := 0
	for _, notice := range database.OpenNotices() {
		if strings.Contains(notice.Message, "Removed old migration backup") {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("notices = %+v", database.OpenNotices())
	}
}

func TestOpenDoesNotMigrateWhenTheBackupFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	seedV24(t, path)
	// Something already occupies the name the copy is written under.
	blocker := filepath.Join(dir, "state.db.bak-u24-to-u25-cc4-20261107T120000.partial")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := Open(path, nil, withCC(), atTime(backupClock))
	if err == nil {
		t.Fatal("Open succeeded without a backup")
	}
	for _, part := range []string{"Back up billing database", path, "u24-to-u25-cc4", "was not migrated"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q lacks %q", err, part)
		}
	}
	if version := intQuery(t, readOnly(t, path), "PRAGMA user_version"); version != 24 {
		t.Fatalf("user_version = %d after a failed backup, want 24", version)
	}
	if names := backupNames(t, path); len(names) != 0 {
		t.Fatalf("backups = %v", names)
	}
}

func TestOpenNamesTheBackupWhenTheMigrationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seedV24(t, path)
	failing := WithCCSchema(ccbilling.SchemaVersion+1, func(*sql.DB) error { return fmt.Errorf("CC migration failed") })
	_, err := Open(path, nil, failing, atTime(backupClock))
	if err == nil || !strings.Contains(err.Error(), "CC migration failed") ||
		!strings.Contains(err.Error(), "state.db.bak-u24-to-u25-cc4-to-cc5-20261107T120000") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenExplainsHowToRecoverADamagedDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	database := openWith(t, path, withCC())
	// The failure of the incident: a schema entry pointing at a page that is
	// not the table's root.
	if _, err := database.db.Exec(`PRAGMA writable_schema = ON;
		UPDATE sqlite_master SET rootpage = 99999 WHERE name = 'cc_segment_costs';
		PRAGMA writable_schema = OFF;`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	backups := []string{"state.db.bak-u23-to-u25-cc4-20260301T000000", "state.db.bak-u24-to-u25-cc4-20261001T000000"}
	for _, name := range backups {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Open(path, nil, withCC())
	if err == nil {
		t.Fatal("Open accepted a damaged database")
	}
	message := err.Error()
	for _, part := range []string{"malformed", "appears damaged", backups[1] + ", " + backups[0], `".recover"`, path} {
		if !strings.Contains(message, part) {
			t.Fatalf("error %q lacks %q", message, part)
		}
	}

	for _, name := range backups {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(path, nil, withCC()); err == nil || !strings.Contains(err.Error(), "No migration backups were found") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenDoesNotExplainOrdinaryFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database := openWith(t, path)
	if _, err := database.db.Exec("PRAGMA user_version = 9"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, nil, withCC())
	if err == nil || strings.Contains(err.Error(), "damaged") || strings.Contains(err.Error(), "Back up") {
		t.Fatalf("error = %v", err)
	}
}

// Rows another connection committed but never checkpointed live only in the
// write-ahead log; a copy of the database file alone would miss them.
func TestMigrationBackupIncludesTheWriteAheadLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seedV24(t, path)
	writer, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String()+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint = 0;
		INSERT INTO api_keys (scope) SELECT scope || '-wal' FROM api_keys;`); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("write-ahead log = %v, %v", info, err)
	}

	openWith(t, path, withCC(), atTime(backupClock))
	copy := readOnly(t, filepath.Join(filepath.Dir(path), "state.db.bak-u24-to-u25-cc4-20261107T120000"))
	if keys := intQuery(t, copy, "SELECT count(*) FROM api_keys"); keys != 2 {
		t.Fatalf("backup has %d keys, want the 2 including the one only in the write-ahead log", keys)
	}
}

// CopyDatabase copies a database, including what only its write-ahead log
// holds, without changing the source, and refuses to overwrite a file.
func TestCopyDatabaseCopiesTheWriteAheadLogAndLeavesTheSource(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "old.db"), filepath.Join(dir, "plugins", "new.db")
	openWith(t, source, withCC()).Close()
	writer, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: source, OmitHost: true}).String()+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint = 0; INSERT INTO api_keys (scope) VALUES ('only-in-wal');`); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	if err := CopyDatabase(source, target); err != nil {
		t.Fatalf("CopyDatabase error = %v", err)
	}
	copied := readOnly(t, target)
	if keys := intQuery(t, copied, "SELECT count(*) FROM api_keys WHERE scope = 'only-in-wal'"); keys != 1 {
		t.Fatalf("copy has %d rows from the write-ahead log, want 1", keys)
	}
	if version := intQuery(t, copied, "PRAGMA user_version"); version != schemaVersion {
		t.Fatalf("copy format = %d, want %d", version, schemaVersion)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("CopyDatabase changed the source file")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, %v; want 0600", info, err)
	}
	if err := CopyDatabase(source, target); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second CopyDatabase error = %v, want a refusal to overwrite", err)
	}
	if _, err := os.Stat(target + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
}

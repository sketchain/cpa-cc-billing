package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"cpa-super-billing/internal/billing"
)

// migrationBackupsKept is how many migration backups stay beside a database;
// each new backup removes the oldest beyond it.
const migrationBackupsKept = 3

// oldestMigratableVersion is the oldest billing format initSchema migrates.
const oldestMigratableVersion = 10

const backupTimeLayout = "20060102T150405"

// Option adjusts how Open prepares a database.
type Option func(*openOptions)

type openOptions struct {
	ccVersion int
	ccEnsure  func(*sql.DB) error
	now       func() time.Time
}

// WithCCSchema has Open also bring the CC tables, which version themselves in
// cc_meta, to version with ensure, so one backup covers both migrations.
func WithCCSchema(version int, ensure func(*sql.DB) error) Option {
	return func(options *openOptions) {
		options.ccVersion, options.ccEnsure = version, ensure
	}
}

// schemaVersions is a database's billing format (PRAGMA user_version) and the
// format of its CC tables; cc is 0 while the CC tables do not exist.
type schemaVersions struct {
	billing int
	cc      int
}

// readSchemaVersions reads both formats without changing the database.
func (d *DB) readSchemaVersions() (schemaVersions, error) {
	var versions schemaVersions
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&versions.billing); err != nil {
		return versions, fmt.Errorf("Read billing database %s: %w", d.path, err)
	}
	var tables int
	if err := d.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cc_meta'`).Scan(&tables); err != nil {
		return versions, fmt.Errorf("Read billing database %s: %w", d.path, err)
	}
	if tables == 0 {
		return versions, nil
	}
	var raw string
	switch err := d.db.QueryRow(`SELECT value FROM cc_meta WHERE key = 'schema_version'`).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return versions, fmt.Errorf("Read CC schema version of %s: %w", d.path, err)
	default:
		// An unreadable value is left for the CC migration to reject.
		_, _ = fmt.Sscanf(raw, "%d", &versions.cc)
	}
	return versions, nil
}

// migrationTarget reports the formats Open moves a database at current to,
// and whether that rewrites existing data. Creating a new database or adding
// the CC tables to one that has none is not a migration; formats Open rejects
// are not migrated either.
func migrationTarget(current schemaVersions, options openOptions) (schemaVersions, bool) {
	target := current
	migrates := false
	if current.billing >= oldestMigratableVersion && current.billing < schemaVersion {
		target.billing, migrates = schemaVersion, true
	}
	if options.ccEnsure != nil {
		if current.cc > 0 && current.cc < options.ccVersion {
			migrates = true
		}
		if current.cc <= options.ccVersion {
			target.cc = options.ccVersion
		}
	}
	return target, migrates
}

// migrationBackupLabel names a migration, for example u24-to-u25-cc4.
func migrationBackupLabel(from, to schemaVersions, withCC bool) string {
	label := fmt.Sprintf("u%d", from.billing)
	if to.billing != from.billing {
		label += fmt.Sprintf("-to-u%d", to.billing)
	}
	if withCC {
		label += fmt.Sprintf("-cc%d", from.cc)
		if to.cc != from.cc {
			label += fmt.Sprintf("-to-cc%d", to.cc)
		}
	}
	return label
}

func migrationBackupPattern(base string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(base) +
		`\.bak-u\d+(?:-to-u\d+)?(?:-cc\d+(?:-to-cc\d+)?)?-(\d{8}T\d{6})(?:-(\d+))?$`)
}

type migrationBackup struct {
	name     string
	at       string
	sequence int
}

// migrationBackups lists the migration backups beside path, newest first.
func migrationBackups(path string) ([]migrationBackup, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	pattern := migrationBackupPattern(filepath.Base(path))
	backups := []migrationBackup{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		backup := migrationBackup{name: entry.Name(), at: match[1]}
		if match[2] != "" {
			_, _ = fmt.Sscanf(match[2], "%d", &backup.sequence)
		}
		backups = append(backups, backup)
	}
	slices.SortFunc(backups, func(left, right migrationBackup) int {
		if order := strings.Compare(right.at, left.at); order != 0 {
			return order
		}
		if left.sequence != right.sequence {
			return right.sequence - left.sequence
		}
		return strings.Compare(right.name, left.name)
	})
	return backups, nil
}

// backupBeforeMigration copies the whole database, including what its
// write-ahead log still holds, into a new file beside it with SQLite's
// VACUUM INTO, checks that the copy opens intact at the old format, and only
// then gives it its final name. It returns the backup path and the older
// backups it removed.
func (d *DB) backupBeforeMigration(from, to schemaVersions, withCC bool, now time.Time) (string, []string, error) {
	dir, base := filepath.Dir(d.path), filepath.Base(d.path)
	name := base + ".bak-" + migrationBackupLabel(from, to, withCC) + "-" + now.UTC().Format(backupTimeLayout)
	final := filepath.Join(dir, name)
	for sequence := 2; ; sequence++ {
		if _, err := os.Lstat(final); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", nil, err
		}
		final = filepath.Join(dir, fmt.Sprintf("%s-%d", name, sequence))
	}
	partial := final + ".partial"
	// The copy holds what the database holds, so it is private from the start.
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(partial)
		return "", nil, err
	}
	if err := d.writeBackup(partial, from); err != nil {
		_ = os.Remove(partial)
		return "", nil, err
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return "", nil, err
	}
	syncDir(dir)
	removed, err := pruneMigrationBackups(d.path, migrationBackupsKept)
	return final, removed, err
}

func (d *DB) writeBackup(target string, from schemaVersions) error {
	return vacuumInto(d.db, target, from.billing)
}

// vacuumInto copies the database behind handle, including what its
// write-ahead log still holds, into the empty file target, syncs it and checks
// that it opens intact at billingVersion.
func vacuumInto(handle *sql.DB, target string, billingVersion int) error {
	if _, err := handle.Exec("VACUUM INTO ?", target); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	errSync := file.Sync()
	if errClose := file.Close(); errSync == nil {
		errSync = errClose
	}
	if errSync != nil {
		return errSync
	}
	return verifyBackup(target, billingVersion)
}

// verifyBackup opens the copy read-only and checks its structure and format.
func verifyBackup(path string, billingVersion int) error {
	dsn := (&url.URL{Scheme: "file", Path: path, OmitHost: true}).String() + "?mode=ro"
	handle, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer handle.Close()
	var result string
	if err := handle.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("the copy failed its integrity check: %s", result)
	}
	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if version != billingVersion {
		return fmt.Errorf("the copy has format %d, want %d", version, billingVersion)
	}
	return nil
}

// CopyDatabase copies the billing database at source into a new file at
// target the way a migration backup is taken: VACUUM INTO a private partial
// file beside target, an integrity and format check of the copy, and only then
// the final name. source is opened read-only and is left as it is; target must
// not exist yet.
func CopyDatabase(source, target string) error {
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("Copy billing database %s: %s already exists", source, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Copy billing database %s: %w", source, err)
	}
	dsn := (&url.URL{Scheme: "file", Path: source, OmitHost: true}).String() + "?mode=ro&_busy_timeout=5000"
	handle, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("Open billing database %s: %w", source, err)
	}
	defer handle.Close()
	handle.SetMaxOpenConns(1)
	var version int
	if err := handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return explainDamage(source, fmt.Errorf("Read billing database %s: %w", source, err))
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("Create billing database directory %s: %w", dir, err)
	}
	partial := target + ".partial"
	// The copy holds what the database holds, so it is private from the start.
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("Copy billing database %s: %w", source, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("Copy billing database %s: %w", source, err)
	}
	if err := vacuumInto(handle, partial, version); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("Copy billing database %s to %s: %w", source, target, err)
	}
	if err := os.Rename(partial, target); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("Copy billing database %s to %s: %w", source, target, err)
	}
	syncDir(dir)
	return nil
}

// pruneMigrationBackups keeps the newest kept migration backups of path.
func pruneMigrationBackups(path string, kept int) ([]string, error) {
	backups, err := migrationBackups(path)
	if err != nil {
		return nil, fmt.Errorf("List migration backups: %w", err)
	}
	removed := []string{}
	var errs []error
	for index := kept; index < len(backups); index++ {
		name := filepath.Join(filepath.Dir(path), backups[index].name)
		if err := os.Remove(name); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

func syncDir(dir string) {
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
}

// databaseDamaged reports whether err says the database file itself is damaged.
func databaseDamaged(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrCorrupt || sqliteErr.Code == sqlite3.ErrNotADB) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "malformed") || strings.Contains(message, "file is not a database")
}

// explainDamage adds how to recover path to an error that says it is damaged.
func explainDamage(path string, err error) error {
	if err == nil || !databaseDamaged(err) {
		return err
	}
	var hint strings.Builder
	hint.WriteString("; the billing database ")
	hint.WriteString(path)
	hint.WriteString(" appears damaged. ")
	backups, errList := migrationBackups(path)
	switch {
	case errList != nil:
		fmt.Fprintf(&hint, "Its migration backups could not be listed: %v. ", errList)
	case len(backups) == 0:
		hint.WriteString("No migration backups were found beside it. ")
	default:
		names := make([]string, len(backups))
		for index, backup := range backups {
			names[index] = backup.name
		}
		fmt.Fprintf(&hint, "Migration backups beside it, newest first: %s. To restore one, stop CLIProxyAPI, keep the damaged files aside, "+
			"copy the backup to %s and delete %s-wal and %s-shm; the plugin migrates it again on start, and anything recorded after that backup is lost. ", strings.Join(names, ", "), path, path, path)
	}
	fmt.Fprintf(&hint, "To salvage the damaged file, stop CLIProxyAPI and run: sqlite3 %q \".recover\" | sqlite3 %q, "+
		"then move the recovered file into place and rebuild the CC data from history on the CC page", path, path+".recovered")
	return fmt.Errorf("%w%s", err, hint.String())
}

// OpenNotices reports what Open did beyond opening the database, such as the
// backup it took before migrating.
func (d *DB) OpenNotices() []billing.RepositoryNotice {
	return slices.Clone(d.notices)
}

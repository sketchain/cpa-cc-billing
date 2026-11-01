package ccbilling

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The CC tables live beside the billing tables in the same database. They are
// versioned separately in cc_meta, so the billing schema version and its
// migrations stay exactly those of upstream cpa-key-billing: a database copied
// from cpa-key-billing opens unchanged and gains these tables on first use.
const ccSchemaVersion = 1

var ccMigrations = []string{
	// Version 1.
	`CREATE TABLE IF NOT EXISTS cc_segments (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		auth_index TEXT    NOT NULL,
		block_at   INTEGER NOT NULL,
		at         INTEGER NOT NULL,
		cost_usd   REAL    NOT NULL,
		gain       REAL    NOT NULL
	);
	CREATE INDEX IF NOT EXISTS cc_segments_auth_at ON cc_segments(auth_index, at);
	CREATE TABLE IF NOT EXISTS cc_streams (
		auth_index TEXT PRIMARY KEY,
		state_json TEXT NOT NULL
	);`,
}

const (
	metaSchemaVersion = "schema_version"
	metaSettings      = "settings"
	claudeProvider    = "claude"
	claudeLimitKey    = "unified"
)

func nanos(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UTC().UnixNano()
}

func timeAt(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func transact(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("Begin CC transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Commit CC transaction: %w", err)
	}
	return nil
}

// EnsureSchema creates or migrates the CC tables.
func EnsureSchema(db *sql.DB) error {
	return transact(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS cc_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
			return fmt.Errorf("Create CC tables: %w", err)
		}
		version := 0
		var raw string
		switch err := tx.QueryRow(`SELECT value FROM cc_meta WHERE key = ?`, metaSchemaVersion).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("Read CC schema version: %w", err)
		default:
			if _, err := fmt.Sscanf(raw, "%d", &version); err != nil {
				return fmt.Errorf("Invalid CC schema version %q", raw)
			}
		}
		if version > ccSchemaVersion {
			return fmt.Errorf("CC tables use a newer format (%d); upgrade the plugin", version)
		}
		for ; version < ccSchemaVersion; version++ {
			if _, err := tx.Exec(ccMigrations[version]); err != nil {
				return fmt.Errorf("Migrate CC tables to version %d: %w", version+1, err)
			}
		}
		_, err := tx.Exec(`INSERT INTO cc_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			metaSchemaVersion, fmt.Sprint(ccSchemaVersion))
		return err
	})
}

func loadSettings(db *sql.DB) (Settings, error) {
	var raw string
	switch err := db.QueryRow(`SELECT value FROM cc_meta WHERE key = ?`, metaSettings).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return DefaultSettings(), nil
	case err != nil:
		return Settings{}, fmt.Errorf("Read CC settings: %w", err)
	}
	settings := DefaultSettings()
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return Settings{}, fmt.Errorf("Read CC settings: %w", err)
	}
	// Parameters added by later versions keep their defaults.
	defaults := DefaultParams()
	for tier, value := range defaults.TierWeeklyUSD {
		if _, ok := settings.Params.TierWeeklyUSD[tier]; !ok {
			settings.Params.TierWeeklyUSD[tier] = value
		}
	}
	return settings.Normalized()
}

func saveSettings(db *sql.DB, settings Settings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO cc_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaSettings, string(raw)); err != nil {
		return fmt.Errorf("Save CC settings: %w", err)
	}
	return nil
}

func loadStream(db *sql.DB, authIndex string) (StreamState, error) {
	var raw string
	switch err := db.QueryRow(`SELECT state_json FROM cc_streams WHERE auth_index = ?`, authIndex).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return StreamState{}, nil
	case err != nil:
		return StreamState{}, fmt.Errorf("Read CC stream: %w", err)
	}
	var state StreamState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return StreamState{}, fmt.Errorf("Read CC stream: %w", err)
	}
	return state, nil
}

func loadSegments(db *sql.DB, authIndex string, since time.Time) ([]Segment, error) {
	rows, err := db.Query(`SELECT block_at, at, cost_usd, gain FROM cc_segments WHERE auth_index = ? AND at >= ? ORDER BY at, id`,
		authIndex, nanos(since))
	if err != nil {
		return nil, fmt.Errorf("Read CC segments: %w", err)
	}
	defer rows.Close()
	var segments []Segment
	for rows.Next() {
		var block, at int64
		var segment Segment
		if err := rows.Scan(&block, &at, &segment.CostUSD, &segment.Gain); err != nil {
			return nil, fmt.Errorf("Read CC segments: %w", err)
		}
		segment.Block, segment.At = timeAt(block), timeAt(at)
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

// commitAdvance stores an account's new segments with the stream position
// that produced them, so a failure retries the same data.
func commitAdvance(db *sql.DB, authIndex string, state StreamState, segments []Segment) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return transact(db, func(tx *sql.Tx) error {
		for _, segment := range segments {
			if _, err := tx.Exec(`INSERT INTO cc_segments (auth_index, block_at, at, cost_usd, gain) VALUES (?, ?, ?, ?, ?)`,
				authIndex, nanos(segment.Block), nanos(segment.At), segment.CostUSD, segment.Gain); err != nil {
				return fmt.Errorf("Save CC segments: %w", err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO cc_streams (auth_index, state_json) VALUES (?, ?)
			ON CONFLICT(auth_index) DO UPDATE SET state_json = excluded.state_json`, authIndex, string(raw)); err != nil {
			return fmt.Errorf("Save CC stream: %w", err)
		}
		return nil
	})
}

func pruneSegments(db *sql.DB, before time.Time) error {
	if _, err := db.Exec(`DELETE FROM cc_segments WHERE at < ?`, nanos(before)); err != nil {
		return fmt.Errorf("Prune CC segments: %w", err)
	}
	return nil
}

// resetStreams forgets every segment and stream position, so the next refresh
// rebuilds them from the retained request history.
func resetStreams(db *sql.DB) error {
	return transact(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM cc_segments; DELETE FROM cc_streams;`); err != nil {
			return fmt.Errorf("Reset CC segments: %w", err)
		}
		return nil
	})
}

// readObservations returns an account's unified window reports made after from
// and no later than to, one per request, oldest first.
func readObservations(db *sql.DB, authIndex string, from, to time.Time) ([]Observation, error) {
	rows, err := db.Query(`SELECT request_event_id, at, seconds, used_percent, reset_at FROM quota_report_windows
		WHERE auth_index = ? AND provider = ? AND limit_key = ? AND seconds IN (?, ?) AND at > ? AND at <= ?
		ORDER BY at, request_event_id`,
		authIndex, claudeProvider, claudeLimitKey, int64(FiveHours/time.Second), int64(Week/time.Second), nanos(from), nanos(to))
	if err != nil {
		return nil, fmt.Errorf("Read Claude quota reports: %w", err)
	}
	defer rows.Close()
	var observations []Observation
	lastID := int64(-1)
	for rows.Next() {
		var id, at, seconds, resetAt int64
		var used float64
		if err := rows.Scan(&id, &at, &seconds, &used, &resetAt); err != nil {
			return nil, fmt.Errorf("Read Claude quota reports: %w", err)
		}
		if id != lastID || len(observations) == 0 {
			observations = append(observations, Observation{At: timeAt(at)})
			lastID = id
		}
		obs := &observations[len(observations)-1]
		if seconds == int64(FiveHours/time.Second) {
			obs.Has5h, obs.Used5h, obs.Reset5h = true, used, timeAt(resetAt)
		} else {
			obs.Has7d, obs.Used7d, obs.Reset7d = true, used, timeAt(resetAt)
		}
	}
	return observations, rows.Err()
}

// maxRequestDuration bounds how long before its completion a request began.
const maxRequestDuration = 2 * time.Hour

// readCosts returns the costs of an account's requests completed after from
// and no later than to, in USD.
func readCosts(db *sql.DB, authIndex string, from, to time.Time, toUSD func(float64, string) float64) ([]CostEvent, error) {
	rows, err := db.Query(`SELECT at, latency_ms, total_cost, currency FROM request_events
		WHERE auth_index = ? AND at > ? AND at <= ? ORDER BY at, id`,
		authIndex, nanos(from.Add(-maxRequestDuration)), nanos(to))
	if err != nil {
		return nil, fmt.Errorf("Read request costs: %w", err)
	}
	defer rows.Close()
	var costs []CostEvent
	for rows.Next() {
		var at, latency int64
		var cost float64
		var currency string
		if err := rows.Scan(&at, &latency, &cost, &currency); err != nil {
			return nil, fmt.Errorf("Read request costs: %w", err)
		}
		completed := timeAt(at).Add(time.Duration(max(latency, 0)) * time.Millisecond)
		if completed.After(from) && !completed.After(to) {
			costs = append(costs, CostEvent{At: completed, USD: toUSD(cost, currency)})
		}
	}
	return costs, rows.Err()
}

func segmentStats(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow(`SELECT count(*) FROM cc_segments`).Scan(&count)
	return count, err
}

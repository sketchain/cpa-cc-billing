package ccbilling

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// blockHour is the width of a refusal count's bucket.
const blockHour = time.Hour

// maxPendingBlocks bounds the refusal counts kept in memory between flushes;
// a key, hour and reason is one entry, so the bound is only reached when
// nothing flushes for a long time. Counts beyond it are dropped and reported.
const maxPendingBlocks = 10000

type blockKey struct {
	scope  string
	hour   time.Time
	reason string
}

// BlockCount is how often the CC split refused a key's requests within one
// hour for one reason.
type BlockCount struct {
	Scope  string    `json:"scope"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Count  int64     `json:"count"`
}

// RecordBlock counts a refused request. It runs on the request path, so it
// only adds to memory; the counts reach the database with the next refresh
// or flush, which run within host calls off the request path.
func (m *Manager) RecordBlock(scope, reason string, at time.Time) {
	scope, reason = strings.TrimSpace(scope), strings.TrimSpace(reason)
	if scope == "" || reason == "" {
		return
	}
	key := blockKey{scope: scope, hour: at.UTC().Truncate(blockHour), reason: reason}
	m.blockMu.Lock()
	defer m.blockMu.Unlock()
	if m.blocks == nil {
		m.blocks = map[blockKey]int64{}
	}
	if _, ok := m.blocks[key]; !ok && len(m.blocks) >= maxPendingBlocks {
		m.blocksDropped++
		return
	}
	m.blocks[key]++
}

// takeBlocks removes and returns the pending counts.
func (m *Manager) takeBlocks() (map[blockKey]int64, int64) {
	m.blockMu.Lock()
	defer m.blockMu.Unlock()
	pending, dropped := m.blocks, m.blocksDropped
	m.blocks, m.blocksDropped = nil, 0
	return pending, dropped
}

// restoreBlocks puts counts that could not be saved back, so a failed
// flush loses nothing.
func (m *Manager) restoreBlocks(pending map[blockKey]int64) {
	m.blockMu.Lock()
	defer m.blockMu.Unlock()
	if m.blocks == nil {
		m.blocks = map[blockKey]int64{}
	}
	for key, count := range pending {
		m.blocks[key] += count
	}
}

// flushBlocksLocked needs refreshMu and runs within withDB. It adds the
// pending counts to the stored ones in one transaction.
func (m *Manager) flushBlocksLocked(db *sql.DB) error {
	pending, dropped := m.takeBlocks()
	if dropped > 0 {
		m.host.Logf("CC refusal counts exceeded %d pending entries; %d refusals were not counted", maxPendingBlocks, dropped)
	}
	if len(pending) == 0 {
		return nil
	}
	keys := make([]blockKey, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b blockKey) int {
		return cmp.Or(strings.Compare(a.scope, b.scope), a.hour.Compare(b.hour), strings.Compare(a.reason, b.reason))
	})
	err := transact(db, func(tx *sql.Tx) error {
		statement, err := tx.Prepare(`INSERT INTO cc_block_counts (scope, hour_at, reason, count) VALUES (?, ?, ?, ?)
			ON CONFLICT(scope, hour_at, reason) DO UPDATE SET count = count + excluded.count`)
		if err != nil {
			return fmt.Errorf("Save CC refusal counts: %w", err)
		}
		defer statement.Close()
		for _, key := range keys {
			if _, err := statement.Exec(key.scope, nanos(key.hour), key.reason, pending[key]); err != nil {
				return fmt.Errorf("Save CC refusal counts: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		m.restoreBlocks(pending)
	}
	return err
}

// FlushBlocks saves the pending refusal counts now. Only host calls off the
// request path use it.
func (m *Manager) FlushBlocks() error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.withDB(m.flushBlocksLocked)
}

// pendingBlocks reports whether refusal counts wait for a flush.
func (m *Manager) pendingBlocks() bool {
	m.blockMu.Lock()
	defer m.blockMu.Unlock()
	return len(m.blocks) > 0 || m.blocksDropped > 0
}

func readBlockCounts(db *sql.DB, scopes []string, from time.Time) ([]BlockCount, error) {
	result := []BlockCount{}
	if len(scopes) == 0 {
		return result, nil
	}
	args := []any{nanos(from)}
	for _, scope := range scopes {
		args = append(args, scope)
	}
	rows, err := db.Query(`SELECT scope, hour_at, reason, count FROM cc_block_counts WHERE hour_at >= ? AND scope IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(scopes)), ",")+`) ORDER BY scope, hour_at, reason`, args...)
	if err != nil {
		return nil, fmt.Errorf("Read CC refusal counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var count BlockCount
		var at int64
		if err := rows.Scan(&count.Scope, &at, &count.Reason, &count.Count); err != nil {
			return nil, fmt.Errorf("Read CC refusal counts: %w", err)
		}
		count.At = timeAt(at)
		result = append(result, count)
	}
	return result, rows.Err()
}

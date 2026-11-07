package ccbilling

import (
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Exclusion modes of a trend's segments.
const (
	// ExclusionEstimate marks the segments the estimate's filter excluded.
	ExclusionEstimate = "estimate"
	// ExclusionRaw means no estimate exists, so no segment is marked.
	ExclusionRaw = "raw"
)

// TrendHour sums an account's 1% segments that ended within one hour, like
// the hourly k buckets. CostUSD and Gain cover the kept segments only.
type TrendHour struct {
	At       time.Time `json:"at"`
	Segments int       `json:"segments"`
	Excluded int       `json:"excluded"`
	CostUSD  float64   `json:"cost_usd"`
	Gain     float64   `json:"gain"`
}

// Trend is an account's retained k evidence and segments by hour, so the page
// can split them into days in the viewer's time zone.
type Trend struct {
	AuthIndex string `json:"auth_index"`
	// Loaded is false while no estimate snapshot exists.
	Loaded bool      `json:"loaded"`
	From   time.Time `json:"from"`
	At     time.Time `json:"at"`
	// RetentionDays and KMinWeeklyPoints are the parameters the data was
	// retained and judged with.
	RetentionDays    int     `json:"retention_days"`
	KMinWeeklyPoints float64 `json:"k_min_weekly_points"`
	// Provider is the account's provider; Window the window its segments
	// measure. Accounts measuring the 7-day window have no k evidence.
	Provider string `json:"provider"`
	Window   string `json:"segment_window"`
	// K is the account's effective k and KSource where it comes from;
	// NeedsK reports that no k is set or estimated.
	K         *float64 `json:"k,omitempty"`
	KSource   string   `json:"k_source"`
	NeedsK    bool     `json:"needs_k"`
	Exclusion string   `json:"exclusion"`
	// Basis is the estimate's basis; on the weighted basis segment costs
	// are in baseline USD.
	Basis    string      `json:"basis"`
	Buckets  []KBucket   `json:"buckets"`
	Segments []TrendHour `json:"segment_hours"`
}

// Trend reads an account's retained hourly k buckets and segments. Segments
// are summed per hour, marking those the estimate excluded. It reads
// the latest snapshot and the database only: it never refreshes, migrates or
// writes, and does not wait for a running refresh. The second result is false
// when the account is not configured.
func (m *Manager) Trend(authIndex string) (Trend, bool, error) {
	snap := m.snap.Load()
	var settings Settings
	switch {
	case snap != nil:
		settings = snap.settings
	case m.loaded.Load() != nil:
		settings = *m.loaded.Load()
	default:
		return Trend{}, false, fmt.Errorf("CC estimates are not loaded")
	}
	if !slices.Contains(settings.authIndexes(), authIndex) {
		return Trend{}, false, nil
	}
	params := settings.accountParams(authIndex)
	now := m.host.Now()
	// The snapshot's cutoff selects exactly the segments its estimate used.
	asOf := now
	if snap != nil {
		asOf = snap.at
	}
	trend := Trend{AuthIndex: authIndex, Loaded: snap != nil, From: asOf.Add(-params.retention()), At: now,
		RetentionDays: params.SegmentRetentionDays, KMinWeeklyPoints: params.KMinWeeklyPoints, Exclusion: ExclusionRaw, Basis: BasisRaw,
		Buckets: []KBucket{}, Segments: []TrendHour{}}
	var segments []Segment
	// The host's database, not withDB: that would create or migrate tables
	// when the database changed. Settings were loaded, so the tables exist.
	err := m.host.WithDB(func(db *sql.DB) error {
		if db == nil {
			return fmt.Errorf("The billing database is not open")
		}
		var err error
		if segments, err = loadSegments(db, authIndex, trend.From); err != nil {
			return err
		}
		buckets, err := loadKBuckets(db, authIndex, trend.From)
		if buckets != nil {
			trend.Buckets = buckets
		}
		return err
	})
	if err != nil {
		return Trend{}, true, err
	}
	var excluded []bool
	var status AccountStatus
	if snap != nil {
		if weights := snap.weights[settings.AccountProvider(authIndex)]; weights.basis == BasisWeighted {
			// The estimate filtered the converted costs.
			segments = ConvertSegments(segments, weights.factors)
			trend.Basis = BasisWeighted
		}
		excluded = SegmentExclusions(segments, params, snap.at)
		trend.Exclusion = ExclusionEstimate
		latest, _ := m.live(snap)
		status = accountStatus(snap, latest, authIndex, now)
	} else {
		status = ResolveAccount(authIndex, settings.Accounts[authIndex], Estimate{}, KEstimate{Basis: KBasisNone}, Observation{}, params, now)
	}
	trend.K, trend.KSource, trend.NeedsK = status.K, status.KSource, status.NeedsK
	trend.Provider, trend.Window = settings.AccountProvider(authIndex), WindowFiveHour
	if snap != nil {
		trend.Window = segmentWindow(snap.accounts[authIndex].window)
	}
	for i, segment := range segments {
		hour := segment.At.UTC().Truncate(kBucketWidth)
		if n := len(trend.Segments); n == 0 || !trend.Segments[n-1].At.Equal(hour) {
			trend.Segments = append(trend.Segments, TrendHour{At: hour})
		}
		item := &trend.Segments[len(trend.Segments)-1]
		if excluded != nil && excluded[i] {
			item.Excluded++
			continue
		}
		item.Segments++
		item.CostUSD += segment.CostUSD
		item.Gain += segment.Gain
	}
	return trend, true, nil
}

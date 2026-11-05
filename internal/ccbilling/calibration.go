package ccbilling

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Calibration statuses.
const (
	CalibrationRunning      = "running"
	CalibrationOK           = "ok"
	CalibrationInsufficient = "insufficient"
)

// Calibration bases of the baseline ratio.
const (
	// CalibrationBasePeriod compares with baseline-only segments of the
	// calibration period.
	CalibrationBasePeriod = "period"
	// CalibrationBaseRecent adds the account's baseline-only segments of the
	// recent period before it, when the calibration has too few of its own.
	CalibrationBaseRecent = "recent"
)

// A calibration needs this many pure segments of the item and of the baseline.
const (
	calibrationMinItemSegments = 10
	calibrationMinBaseSegments = 10
)

// CalibrationResult compares the points per USD of segments that contain
// only the item with those that contain only baseline models.
type CalibrationResult struct {
	Status     string   `json:"status"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	StdErr     float64  `json:"std_err,omitempty"`
	Low        float64  `json:"low,omitempty"`
	High       float64  `json:"high,omitempty"`
	// Pure item segments of the calibration period.
	ItemSegments    int     `json:"item_segments"`
	ItemGain        float64 `json:"item_gain"`
	ItemCostUSD     float64 `json:"item_cost_usd"`
	ItemUSDPerPoint float64 `json:"item_usd_per_point,omitempty"`
	// Pure baseline segments the item is compared with.
	BaseSegments    int     `json:"base_segments"`
	BaseGain        float64 `json:"base_gain"`
	BaseCostUSD     float64 `json:"base_cost_usd"`
	BaseUSDPerPoint float64 `json:"base_usd_per_point,omitempty"`
	BaseFrom        string  `json:"base_from"`
	// Excluded counts the period's segments that mixed the item with other
	// models; Unsplit those without a model split.
	Excluded int `json:"excluded_segments"`
	Unsplit  int `json:"unsplit_segments"`
}

// Calibration is one calibration run of an item on an account. Item is the
// item as it was when the run started.
type Calibration struct {
	ID        int64      `json:"id"`
	Item      WeightItem `json:"item"`
	AuthIndex string     `json:"auth_index"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   time.Time  `json:"ended_at,omitzero"`
	// Final means the run ended and every segment of its period was
	// processed, so its stored result no longer changes.
	Final  bool              `json:"final"`
	Result CalibrationResult `json:"result"`
}

// Running reports whether the calibration has not ended.
func (c Calibration) Running() bool { return c.EndedAt.IsZero() }

// segmentPurity classifies a split segment as containing only models, only
// baseline models (none of tested), or a mixture.
type segmentPurity int

const (
	purityMixed segmentPurity = iota
	purityItem
	purityBase
	purityEmpty
)

func classifySegment(segment Segment, item map[string]bool, tested map[string]bool) segmentPurity {
	if segment.CostUSD <= 0 {
		return purityEmpty
	}
	onlyItem, onlyBase := true, true
	for model, cost := range segment.Models {
		if cost <= 0 {
			continue
		}
		if !item[model] {
			onlyItem = false
		}
		if tested[model] || item[model] {
			onlyBase = false
		}
	}
	switch {
	case onlyItem:
		return purityItem
	case onlyBase:
		return purityBase
	}
	return purityMixed
}

// ratioStats sums pure segments for a ratio estimate of points per USD.
type ratioStats struct {
	gains, costs []float64
	gain, cost   float64
}

func (r *ratioStats) add(segment Segment) {
	r.gains, r.costs = append(r.gains, segment.Gain), append(r.costs, segment.CostUSD)
	r.gain += segment.Gain
	r.cost += segment.CostUSD
}

// relativeError is the ratio estimator's standard error relative to the ratio.
func (r ratioStats) relativeError() float64 {
	n := float64(len(r.gains))
	if n < 2 || r.gain <= 0 || r.cost <= 0 {
		return math.Inf(1)
	}
	ratio := r.gain / r.cost
	mean := r.cost / n
	sum := 0.0
	for i := range r.gains {
		residual := r.gains[i] - ratio*r.costs[i]
		sum += residual * residual
	}
	return math.Sqrt(sum/(n*(n-1))) / mean / ratio
}

// calibrate computes a calibration's result from the account's segments,
// oldest first. tested holds every model under test, so baseline segments
// exclude them all.
func calibrate(segments []Segment, item WeightItem, tested map[string]bool, start, end time.Time, params Params) CalibrationResult {
	models := map[string]bool{}
	for _, model := range item.Models {
		models[model] = true
	}
	result := CalibrationResult{Status: CalibrationInsufficient, BaseFrom: CalibrationBasePeriod}
	var itemStats, baseStats, recentStats ratioStats
	recentFrom := start.Add(-params.recent())
	for _, segment := range segments {
		inPeriod := segment.At.After(start) && !segment.At.After(end)
		inRecent := segment.At.After(recentFrom) && !segment.At.After(start)
		if !inPeriod && !inRecent {
			continue
		}
		if !segment.Split() {
			if inPeriod {
				result.Unsplit++
			}
			continue
		}
		switch classifySegment(segment, models, tested) {
		case purityItem:
			if inPeriod {
				itemStats.add(segment)
			}
		case purityBase:
			if inPeriod {
				baseStats.add(segment)
			} else {
				recentStats.add(segment)
			}
		case purityMixed:
			if inPeriod {
				result.Excluded++
			}
		}
	}
	if len(baseStats.gains) < calibrationMinBaseSegments && len(recentStats.gains) > 0 {
		for i := range recentStats.gains {
			baseStats.add(Segment{Gain: recentStats.gains[i], CostUSD: recentStats.costs[i]})
		}
		result.BaseFrom = CalibrationBaseRecent
	}
	result.ItemSegments, result.ItemGain, result.ItemCostUSD = len(itemStats.gains), itemStats.gain, itemStats.cost
	result.BaseSegments, result.BaseGain, result.BaseCostUSD = len(baseStats.gains), baseStats.gain, baseStats.cost
	if itemStats.gain > 0 {
		result.ItemUSDPerPoint = itemStats.cost / itemStats.gain
	}
	if baseStats.gain > 0 {
		result.BaseUSDPerPoint = baseStats.cost / baseStats.gain
	}
	if result.ItemSegments < calibrationMinItemSegments || result.BaseSegments < calibrationMinBaseSegments ||
		itemStats.cost <= 0 || baseStats.cost <= 0 || itemStats.gain <= 0 || baseStats.gain <= 0 {
		return result
	}
	// Points per USD of the item over the baseline's: the item's quota use
	// per USD in baseline units.
	multiplier := (itemStats.gain / itemStats.cost) / (baseStats.gain / baseStats.cost)
	relative := math.Hypot(itemStats.relativeError(), baseStats.relativeError())
	if !finite(multiplier) || !finite(relative) {
		return result
	}
	result.Status = CalibrationOK
	result.Multiplier = &multiplier
	result.StdErr = multiplier * relative
	result.Low, result.High = math.Max(0, multiplier-z95*result.StdErr), multiplier+z95*result.StdErr
	return result
}

func insertCalibration(db *sql.DB, item WeightItem, authIndex string, at time.Time) (int64, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return 0, err
	}
	result, err := db.Exec(`INSERT INTO cc_calibrations (item_json, auth_index, started_at) VALUES (?, ?, ?)`, string(raw), authIndex, nanos(at))
	if err != nil {
		return 0, fmt.Errorf("Save CC calibration: %w", err)
	}
	return result.LastInsertId()
}

func loadCalibrations(db *sql.DB) ([]Calibration, error) {
	rows, err := db.Query(`SELECT id, item_json, auth_index, started_at, ended_at, result_json FROM cc_calibrations ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("Read CC calibrations: %w", err)
	}
	defer rows.Close()
	calibrations := []Calibration{}
	for rows.Next() {
		var calibration Calibration
		var item, result string
		var started, ended int64
		if err := rows.Scan(&calibration.ID, &item, &calibration.AuthIndex, &started, &ended, &result); err != nil {
			return nil, fmt.Errorf("Read CC calibrations: %w", err)
		}
		if err := json.Unmarshal([]byte(item), &calibration.Item); err != nil {
			return nil, fmt.Errorf("Read CC calibrations: %w", err)
		}
		calibration.StartedAt, calibration.EndedAt = timeAt(started), timeAt(ended)
		if result != "" {
			if err := json.Unmarshal([]byte(result), &calibration.Result); err != nil {
				return nil, fmt.Errorf("Read CC calibrations: %w", err)
			}
			calibration.Final = true
		}
		calibrations = append(calibrations, calibration)
	}
	return calibrations, rows.Err()
}

func finishCalibration(db *sql.DB, id int64, result CalibrationResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE cc_calibrations SET result_json = ? WHERE id = ? AND ended_at <> 0`, string(raw), id); err != nil {
		return fmt.Errorf("Save CC calibration: %w", err)
	}
	return nil
}

var errCalibrationNotFound = errors.New("calibration not found")

func endCalibration(db *sql.DB, id int64, at time.Time) error {
	result, err := db.Exec(`UPDATE cc_calibrations SET ended_at = ? WHERE id = ? AND ended_at = 0`, nanos(at), id)
	if err != nil {
		return fmt.Errorf("Save CC calibration: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return errCalibrationNotFound
	}
	return nil
}

func deleteCalibration(db *sql.DB, id int64) error {
	result, err := db.Exec(`DELETE FROM cc_calibrations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("Delete CC calibration: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return errCalibrationNotFound
	}
	return nil
}

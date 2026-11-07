package ccbilling

import (
	"math"
	"time"
)

// BlockSummary is one uninterrupted run of an account's segments after
// contamination filtering.
type BlockSummary struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Segments int       `json:"segments"`
	Excluded int       `json:"excluded"`
	Gain     float64   `json:"gain"`
	Cost     float64   `json:"cost_usd"`
	// CleanGain and CleanCost sum the segments that survived filtering.
	CleanGain float64 `json:"clean_gain"`
	CleanCost float64 `json:"clean_cost_usd"`
	// Reference is the ratio, in points per USD, windows were compared with.
	Reference     float64 `json:"reference"`
	ReferenceMode string  `json:"reference_mode"`
}

// Clean reports whether the block contributes to the global average.
func (b BlockSummary) Clean() bool { return b.CleanGain > gainEpsilon && b.CleanCost > 0 }

// Ratio is the block's clean utilization points per USD.
func (b BlockSummary) Ratio() float64 {
	if !b.Clean() {
		return 0
	}
	return b.CleanGain / b.CleanCost
}

// Estimate is what an account's segments say about its 5-hour quota.
type Estimate struct {
	Blocks           []BlockSummary `json:"-"`
	TotalBlocks      int            `json:"total_blocks"`
	CleanBlocks      int            `json:"clean_blocks"`
	Segments         int            `json:"segments"`
	ExcludedSegments int            `json:"excluded_segments"`
	// Ratio is the global weighted average in points per USD; zero when unknown.
	Ratio float64 `json:"ratio"`
	// FiveHourUSD is 100 / Ratio: the total in USD of the window the
	// segments measure, the 5-hour window unless Window says otherwise.
	FiveHourUSD float64 `json:"five_hour_usd"`
	// Window is the window the segments measure: WindowFiveHour, or
	// WindowWeek for an account whose reports have no 5-hour window.
	Window string `json:"segment_window,omitempty"`
}

// EstimateSegments filters each block of chronologically ordered segments and
// combines the clean blocks into a global ratio as of now.
func EstimateSegments(segments []Segment, params Params, now time.Time) Estimate {
	return estimateSegments(segments, params, now, nil)
}

// SegmentExclusions reports, for each segment, whether the estimate's
// contamination filter excludes it. The filter is a pure function of the
// segments and parameters, so the same inputs give the estimate's own result.
func SegmentExclusions(segments []Segment, params Params, now time.Time) []bool {
	excluded := make([]bool, len(segments))
	estimateSegments(segments, params, now, excluded)
	return excluded
}

// estimateSegments fills excluded, when it is not nil, with each segment's
// filtering outcome.
func estimateSegments(segments []Segment, params Params, now time.Time, excluded []bool) Estimate {
	var result Estimate
	for start := 0; start < len(segments); {
		end := start + 1
		for end < len(segments) && segments[end].Block.Equal(segments[start].Block) {
			end++
		}
		block := segments[start:end]
		reference, mode := 0.0, ReferenceBlock
		if params.ReferenceMode == ReferenceGlobal {
			if global := globalRatio(result.Blocks, params, block[0].Block); global > 0 {
				reference, mode = global, ReferenceGlobal
			}
		}
		var mask []bool
		if excluded != nil {
			mask = excluded[start:end]
		}
		summary := filterBlock(block, reference, params, mask)
		summary.ReferenceMode = mode
		result.Blocks = append(result.Blocks, summary)
		result.Segments += summary.Segments
		result.ExcludedSegments += summary.Excluded
		if summary.Clean() {
			result.CleanBlocks++
		}
		start = end
	}
	result.TotalBlocks = len(result.Blocks)
	result.Ratio = globalRatio(result.Blocks, params, now)
	if result.Ratio > 0 {
		result.FiveHourUSD = 100 / result.Ratio
	}
	return result
}

type window struct {
	first, last int
	gain, cost  float64
}

func (w window) ratio() float64 {
	if w.cost <= 0 {
		return math.Inf(1)
	}
	return w.gain / w.cost
}

// filterBlock removes the segments that windows of suspiciously many points
// per USD cover. Outside usage raises utilization without cost through the
// plugin, so it can only raise a window's ratio. A zero reference asks for the
// block's own iterative average. A non-nil excluded receives each segment's
// outcome.
func filterBlock(block []Segment, reference float64, params Params, excluded []bool) BlockSummary {
	summary := BlockSummary{Start: block[0].Block, End: block[len(block)-1].At, Segments: len(block)}
	for _, segment := range block {
		summary.Gain += segment.Gain
		summary.Cost += segment.CostUSD
	}
	var windows []window
	for first := range block {
		current := window{first: first}
		for last := first; last < len(block); last++ {
			current.gain += block[last].Gain
			current.cost += block[last].CostUSD
			if current.gain >= params.WindowPercent-gainEpsilon {
				current.last = last
				windows = append(windows, current)
				break
			}
		}
	}
	if len(windows) == 0 {
		// A block narrower than one window is judged as a whole against the
		// global reference; without one it is kept with its small weight.
		windows = []window{{first: 0, last: len(block) - 1, gain: summary.Gain, cost: summary.Cost}}
		if reference <= 0 {
			summary.CleanGain, summary.CleanCost = summary.Gain, summary.Cost
			if summary.Cost <= 0 {
				summary.CleanGain, summary.CleanCost, summary.Excluded = 0, 0, len(block)
				for i := range excluded {
					excluded[i] = true
				}
			}
			summary.Reference = summary.Ratio()
			return summary
		}
	}
	if reference <= 0 {
		reference = iterativeReference(windows, params)
	}
	summary.Reference = reference
	limit := reference * (1 + params.SuspiciousThreshold)
	covering := make([]int, len(block))
	suspicious := make([]int, len(block))
	for _, w := range windows {
		bad := !(w.ratio() <= limit)
		for i := w.first; i <= w.last; i++ {
			covering[i]++
			if bad {
				suspicious[i]++
			}
		}
	}
	for i, segment := range block {
		if covering[i] > 0 && float64(suspicious[i]) >= params.ExcludeVoteShare*float64(covering[i])-gainEpsilon {
			summary.Excluded++
			if excluded != nil {
				excluded[i] = true
			}
			continue
		}
		summary.CleanGain += segment.Gain
		summary.CleanCost += segment.CostUSD
	}
	return summary
}

// iterativeReference averages the windows, drops those above the threshold
// and repeats until no window is dropped. Zero means no window has a cost.
func iterativeReference(windows []window, params Params) float64 {
	kept := append([]window(nil), windows...)
	reference := 0.0
	for range len(windows) + 1 {
		var gain, cost float64
		for _, w := range kept {
			gain += w.gain
			cost += w.cost
		}
		if cost <= 0 {
			return reference
		}
		reference = gain / cost
		limit := reference * (1 + params.SuspiciousThreshold)
		next := kept[:0:0]
		for _, w := range kept {
			if w.ratio() <= limit {
				next = append(next, w)
			}
		}
		if len(next) == len(kept) || len(next) == 0 {
			return reference
		}
		kept = next
	}
	return reference
}

// globalRatio is the weighted average of the clean ratios of blocks that ended
// by at, weighting each by its clean gain to the configured power. Blocks of
// the recent period and older ones are averaged separately and then combined
// by the recent share; a group without blocks yields its share to the other.
func globalRatio(blocks []BlockSummary, params Params, at time.Time) float64 {
	cutoff := at.Add(-params.recent())
	var recentSum, recentWeight, oldSum, oldWeight float64
	for _, block := range blocks {
		if !block.Clean() || block.End.After(at) {
			continue
		}
		weight := math.Pow(block.CleanGain, params.WeightPower)
		if block.End.Before(cutoff) {
			oldSum += weight * block.Ratio()
			oldWeight += weight
		} else {
			recentSum += weight * block.Ratio()
			recentWeight += weight
		}
	}
	switch {
	case recentWeight > 0 && oldWeight > 0:
		return params.RecentShare*recentSum/recentWeight + (1-params.RecentShare)*oldSum/oldWeight
	case recentWeight > 0:
		return recentSum / recentWeight
	case oldWeight > 0:
		return oldSum / oldWeight
	}
	return 0
}

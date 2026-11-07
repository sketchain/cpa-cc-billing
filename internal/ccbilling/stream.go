package ccbilling

import (
	"math"
	"sort"
	"time"
)

// Window lengths of the subscriptions' main limits.
const (
	FiveHours = 5 * time.Hour
	Week      = 7 * 24 * time.Hour
)

// Shared with billing.QuotaWindowPeriods: reset times within a minute are the
// same boundary, and a request that began before a reset can still report the
// old counter for a while, so only data older than the reorder horizon is final.
const (
	boundaryJitter  = time.Minute
	ReorderHorizon  = 10 * time.Minute
	resetPriorFloor = 5
	resetNearZero   = 1
	resetLargeDrop  = 20
	gainEpsilon     = 1e-6
)

// Observation is what one response reported about an account's main windows.
// The provider evaluates them when it admits the request, so At is the
// request's start time.
type Observation struct {
	At      time.Time `json:"at"`
	Has5h   bool      `json:"has_5h"`
	Used5h  float64   `json:"used_5h"`
	Reset5h time.Time `json:"reset_5h,omitzero"`
	Has7d   bool      `json:"has_7d"`
	Used7d  float64   `json:"used_7d"`
	Reset7d time.Time `json:"reset_7d,omitzero"`
	// LimitReached is the provider saying the limit is used up; stored
	// reports do not keep it, and a window at 100% says the same.
	LimitReached bool `json:"limit_reached,omitempty"`
	// Credits is the credit balance the report named, when HasCredits.
	HasCredits bool    `json:"has_credits,omitempty"`
	Credits    float64 `json:"credits,omitempty"`
}

// exhausted reports a main window used up.
func (o Observation) exhausted() bool {
	return o.LimitReached || o.Has5h && o.Used5h >= 100 || o.Has7d && o.Used7d >= 100
}

// CostEvent is one request's cost, which counts against the account once the
// request completes.
type CostEvent struct {
	At  time.Time
	USD float64
	// Model is the request's normalized billing model; "" when unknown.
	Model string
	// Excluded is why the provider's segment rules leave the request out,
	// "" when it counts.
	Excluded string
}

// Reasons a request's cost stays out of the segments.
const (
	// ExcludedOtherLimit: the request was metered against another limit
	// than the main one, so its report has no main window.
	ExcludedOtherLimit = "other_limit"
	// ExcludedUnpriced: the request's model has no price.
	ExcludedUnpriced = "unpriced"
)

// Reasons an account's segments are paused.
const (
	PauseLimitReached = "limit_reached"
	PauseCredits      = "credits"
)

// maxUnpricedModels bounds the unpriced models a stream remembers.
const maxUnpricedModels = 50

// Segment is the cost of one rise of the 5-hour utilization, normally 1%, of
// one account. Block is the start of the uninterrupted run it belongs to.
type Segment struct {
	At      time.Time `json:"at"`
	Block   time.Time `json:"block"`
	CostUSD float64   `json:"cost_usd"`
	Gain    float64   `json:"gain"`
	// Models splits CostUSD by billing model. A segment recorded before the
	// split existed has none; cost whose model is unknown is under "".
	Models map[string]float64 `json:"models,omitempty"`
}

// ExcludedCost is what an account's segment rules left out of its segments.
type ExcludedCost struct {
	// USD and Requests by reason.
	USD      map[string]float64 `json:"usd,omitempty"`
	Requests map[string]int64   `json:"requests,omitempty"`
	// UnpricedModels counts the unpriced requests of each model.
	UnpricedModels map[string]int64 `json:"unpriced_models,omitempty"`
}

func (e *ExcludedCost) add(cost CostEvent) {
	if e.USD == nil {
		e.USD, e.Requests = map[string]float64{}, map[string]int64{}
	}
	if finite(cost.USD) && cost.USD > 0 {
		e.USD[cost.Excluded] += cost.USD
	}
	e.Requests[cost.Excluded]++
	if cost.Excluded == ExcludedUnpriced {
		if e.UnpricedModels == nil {
			e.UnpricedModels = map[string]int64{}
		}
		if _, known := e.UnpricedModels[cost.Model]; known || len(e.UnpricedModels) < maxUnpricedModels {
			e.UnpricedModels[cost.Model]++
		}
	}
}

// unknownModel holds the part of a segment's cost whose model is unknown.
const unknownModel = ""

// Split reports whether the segment's cost is fully attributed to models.
func (s Segment) Split() bool {
	if s.CostUSD <= 0 {
		return true
	}
	if len(s.Models) == 0 {
		return false
	}
	_, unknown := s.Models[unknownModel]
	return !unknown
}

// splitCosts attributes a segment's accumulated cost to its models; any part
// the models do not cover, such as cost accumulated before an upgrade, is
// unknown.
func splitCosts(total float64, models map[string]float64) map[string]float64 {
	if total <= 0 {
		return nil
	}
	split := make(map[string]float64, len(models)+1)
	covered := 0.0
	for model, cost := range models {
		if cost > 0 {
			split[model] += cost
			covered += cost
		}
	}
	if rest := total - covered; rest > 1e-9*max(1, total) {
		split[unknownModel] += rest
	}
	return split
}

// windowTrack follows the current period of one quota window.
type windowTrack struct {
	Known   bool      `json:"known"`
	ResetAt time.Time `json:"reset_at,omitzero"`
	Max     float64   `json:"max"`
}

type windowStep int

const (
	stepStale windowStep = iota
	stepSame
	stepNewPeriod
)

func counterReset(previous, current float64) bool {
	return previous >= resetPriorFloor && current < previous &&
		(current <= resetNearZero || previous-current >= resetLargeDrop && current <= previous/2)
}

// observe applies one report and returns how it relates to the tracked
// period and how far it raised the period's highest utilization. Lower
// values within a period are reports that arrived out of order.
func (w *windowTrack) observe(at time.Time, used float64, resetAt time.Time) (windowStep, float64) {
	scheduled := resetAt.After(at)
	if !scheduled {
		resetAt = time.Time{}
	}
	start := func() (windowStep, float64) {
		*w = windowTrack{Known: true, ResetAt: resetAt, Max: used}
		return stepNewPeriod, 0
	}
	if !w.Known {
		return start()
	}
	expired := !w.ResetAt.IsZero() && !at.Before(w.ResetAt)
	switch {
	case expired:
		if scheduled && resetAt.Before(w.ResetAt.Add(-boundaryJitter)) {
			return stepStale, 0
		}
		return start()
	case scheduled && !w.ResetAt.IsZero() && resetAt.Before(w.ResetAt.Add(-boundaryJitter)):
		// An earlier period's report delivered after its successor began.
		return stepStale, 0
	case counterReset(w.Max, used):
		return start()
	case scheduled && !w.ResetAt.IsZero() && resetAt.After(w.ResetAt.Add(boundaryJitter)) && used < w.Max-resetNearZero:
		// The reset time moved later while usage dropped: an early reset.
		return start()
	}
	if scheduled {
		w.ResetAt = resetAt
	}
	if used > w.Max+gainEpsilon {
		delta := used - w.Max
		w.Max = used
		return stepSame, delta
	}
	return stepSame, 0
}

// StreamState is an account's incremental position in its request history.
// Everything up to Cursor has been turned into segments and k evidence.
type StreamState struct {
	Cursor      time.Time   `json:"cursor,omitzero"`
	LastEventAt time.Time   `json:"last_event_at,omitzero"`
	FiveHour    windowTrack `json:"five_hour"`
	Weekly      windowTrack `json:"weekly"`
	BlockStart  time.Time   `json:"block_start,omitzero"`
	// Armed means the block has seen a first rise, so the cost since the last
	// rise belongs to the next segment. Cost before a block's first rise
	// covers an unknown fraction of a point and is discarded.
	Armed   bool    `json:"armed"`
	AccCost float64 `json:"acc_cost"`
	// AccModels splits AccCost by model.
	AccModels map[string]float64 `json:"acc_models,omitempty"`
	// K follows the same reports for k evidence with its own cursor, so k can
	// be rebuilt from history without touching the segments.
	K KStream `json:"k"`
	// Window is the window segments measure: the 5-hour one, or the 7-day
	// one while the account's reports have no 5-hour window. "" before the
	// first report, and in streams saved before it existed, means 5 hours.
	Window string `json:"window,omitempty"`
	// CutAt is the utilization at the latest segment boundary of the block.
	// A segment ends when the utilization crosses a whole point, so reports
	// with decimals add up to whole-point segments.
	CutAt float64 `json:"cut_at,omitempty"`
	// Paused, with its reason and start, stops segments while the provider's
	// rules say usage does not move the utilization.
	Paused      bool      `json:"paused,omitempty"`
	PauseReason string    `json:"pause_reason,omitempty"`
	PausedSince time.Time `json:"paused_since,omitzero"`
	// LastCredits is the credit balance of the latest report that named one.
	HasCredits  bool    `json:"has_credits,omitempty"`
	LastCredits float64 `json:"last_credits,omitempty"`
	// Excluded is the cost the segment rules left out.
	Excluded ExcludedCost `json:"excluded,omitzero"`

	// restarted reports that the segment window changed, so the segments
	// stored before measure another window and are dropped.
	restarted bool
}

func (s *StreamState) breakBlock() {
	s.BlockStart, s.Armed, s.AccCost, s.AccModels, s.CutAt = time.Time{}, false, 0, nil, 0
}

// window is the window segments measure.
func (s *StreamState) window() string {
	if s.Window == WindowWeek {
		return WindowWeek
	}
	return WindowFiveHour
}

// crossesPoint reports whether a rise from previous to current utilization
// crosses a whole point. Values within a millionth of a point of a whole one
// count as that point, absorbing the rounding of fractions times 100.
func crossesPoint(previous, current float64) bool {
	return math.Floor(current+gainEpsilon) > math.Floor(previous+gainEpsilon)
}

type streamEvent struct {
	at       time.Time
	obs      *Observation
	cost     float64
	model    string
	excluded string
}

// Advance consumes the observations and costs that happened after the cursor
// and no later than until, and returns the segments they complete.
func (s *StreamState) Advance(observations []Observation, costs []CostEvent, until time.Time, params Params) []Segment {
	events := make([]streamEvent, 0, len(observations)+len(costs))
	for i := range observations {
		if observations[i].At.After(s.Cursor) && !observations[i].At.After(until) {
			events = append(events, streamEvent{at: observations[i].At, obs: &observations[i]})
		}
	}
	for _, cost := range costs {
		if cost.At.After(s.Cursor) && !cost.At.After(until) {
			events = append(events, streamEvent{at: cost.At, cost: cost.USD, model: cost.Model, excluded: cost.Excluded})
		}
	}
	// A report precedes a cost at the same instant: it was evaluated before
	// that request's usage could count.
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		return events[i].obs != nil && events[j].obs == nil
	})
	var segments []Segment
	gap := params.gap()
	rules := params.rules
	for _, event := range events {
		if !s.LastEventAt.IsZero() && event.at.Sub(s.LastEventAt) > gap {
			s.breakBlock()
		}
		if event.at.After(s.LastEventAt) {
			s.LastEventAt = event.at
		}
		if s.BlockStart.IsZero() {
			s.BlockStart = event.at
		}
		if event.obs == nil {
			if event.excluded != "" {
				s.Excluded.add(CostEvent{USD: event.cost, Model: event.model, Excluded: event.excluded})
				continue
			}
			if s.Armed && finite(event.cost) && event.cost > 0 {
				s.AccCost += event.cost
				if s.AccModels == nil {
					s.AccModels = map[string]float64{}
				}
				s.AccModels[event.model] += event.cost
			}
			continue
		}
		obs := event.obs
		// An account whose reports have no 5-hour window measures the 7-day
		// one; once a 5-hour window appears it measures that, and the
		// segments of the 7-day window are dropped.
		switch {
		case obs.Has5h && s.Window == WindowWeek:
			s.Window, s.restarted = WindowFiveHour, true
			s.breakBlock()
			segments = nil
		case obs.Has5h && s.Window == "":
			s.Window = WindowFiveHour
		case !obs.Has5h && obs.Has7d && s.Window == "":
			s.Window = WindowWeek
		}
		step5, delta5 := stepStale, 0.0
		if obs.Has5h {
			step5, delta5 = s.FiveHour.observe(obs.At, obs.Used5h, obs.Reset5h)
		}
		step7, delta7 := stepStale, 0.0
		if obs.Has7d {
			step7, delta7 = s.Weekly.observe(obs.At, obs.Used7d, obs.Reset7d)
		}
		step, delta, track := step5, delta5, &s.FiveHour
		if s.window() == WindowWeek {
			step, delta, track = step7, delta7, &s.Weekly
		}
		if rules.PauseWhenExhausted {
			creditsUsed := obs.HasCredits && s.HasCredits && obs.Credits < s.LastCredits-gainEpsilon
			if obs.HasCredits {
				s.HasCredits, s.LastCredits = true, obs.Credits
			}
			if obs.exhausted() || creditsUsed {
				// Usage beyond the limit, or paid for with credits, does
				// not move the utilization: no segment may include it.
				if !s.Paused {
					s.Paused, s.PausedSince, s.PauseReason = true, obs.At, PauseLimitReached
					if creditsUsed && !obs.exhausted() {
						s.PauseReason = PauseCredits
					}
				}
				s.breakBlock()
				continue
			}
			if s.Paused {
				s.Paused, s.PausedSince, s.PauseReason = false, time.Time{}, ""
				s.breakBlock()
				s.BlockStart = event.at
				continue
			}
		}
		switch step {
		case stepNewPeriod:
			s.breakBlock()
			s.BlockStart = event.at
		case stepSame:
			if delta > gainEpsilon && crossesPoint(track.Max-delta, track.Max) {
				if s.Armed {
					segments = append(segments, Segment{At: event.at, Block: s.BlockStart, CostUSD: s.AccCost, Gain: roundGain(track.Max - s.CutAt),
						Models: splitCosts(s.AccCost, s.AccModels)})
				}
				s.Armed, s.AccCost, s.AccModels, s.CutAt = true, 0, nil, track.Max
			}
		}
	}
	if until.After(s.Cursor) {
		s.Cursor = until
	}
	return segments
}

func roundGain(value float64) float64 { return math.Round(value*1e6) / 1e6 }

// KStream turns an account's reports into k evidence: the 5-hour and 7-day
// gains between consecutive reports in which neither window reset, so outside
// usage raises both alike.
type KStream struct {
	Cursor   time.Time   `json:"cursor,omitzero"`
	FiveHour windowTrack `json:"five_hour"`
	Weekly   windowTrack `json:"weekly"`
}

// KEvidence is the paired gain one report added.
type KEvidence struct {
	At     time.Time
	Gain5h float64
	Gain7d float64
}

// Advance consumes the reports after the cursor and no later than until.
func (k *KStream) Advance(observations []Observation, until time.Time) []KEvidence {
	selected := make([]Observation, 0, len(observations))
	for _, obs := range observations {
		if obs.At.After(k.Cursor) && !obs.At.After(until) {
			selected = append(selected, obs)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].At.Before(selected[j].At) })
	var evidence []KEvidence
	for _, obs := range selected {
		step5, delta5, step7, delta7 := stepStale, 0.0, stepStale, 0.0
		known5, known7 := k.FiveHour.Known, k.Weekly.Known
		if obs.Has5h {
			step5, delta5 = k.FiveHour.observe(obs.At, obs.Used5h, obs.Reset5h)
		}
		if obs.Has7d {
			step7, delta7 = k.Weekly.observe(obs.At, obs.Used7d, obs.Reset7d)
		}
		if step5 == stepSame && step7 == stepSame && known5 && known7 && (delta5 > gainEpsilon || delta7 > gainEpsilon) {
			evidence = append(evidence, KEvidence{At: obs.At, Gain5h: delta5, Gain7d: delta7})
		}
	}
	if until.After(k.Cursor) {
		k.Cursor = until
	}
	return evidence
}

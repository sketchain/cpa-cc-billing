package billing

import (
	"fmt"
	"time"
)

// This file is the cpa-cc-billing bridge into the billing store. It keeps the
// CC features out of the upstream files.

// UseRepository runs fn with the open repository under the store's read lock,
// so Configure cannot close it meanwhile. fn must not call other Store methods.
func (s *Store) UseRepository(fn func(Repository) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.repo == nil {
		return fmt.Errorf("The billing database is not open")
	}
	return fn(s.repo)
}

// Weekly anchor alignment results.
const (
	AnchorAligned     = "aligned"
	AnchorChanged     = "changed"
	AnchorWaiting     = "waiting"
	AnchorUnsupported = "unsupported"
	AnchorNoWindow    = "no_window"
)

// AnchorAlignment reports how AlignWindowAnchor left a plan window.
type AnchorAlignment struct {
	Status    string
	WaitUntil time.Time
}

// AlignWindowAnchor keeps the plan window of periodSeconds anchored on the
// phase of resetAt, an upstream account's scheduled reset.
//
// A key's running cycle is never cut short: while any key on the plan has an
// active cycle in that window, the anchor stays and the result is waiting.
// Once those cycles have ended, the anchor moves; the next cycle the key's
// admission opens ends at resetAt and only counts usage from its opening. A
// rolling window can be converted only when it is the plan's only window,
// because all windows of a plan share one cycle mode.
func (s *Store) AlignWindowAnchor(planID string, periodSeconds int64, resetAt time.Time) (AnchorAlignment, error) {
	resetAt = resetAt.UTC().Truncate(time.Second)
	now := s.Now()
	if !resetAt.After(now) || periodSeconds <= 0 || resetAt.After(now.Add(time.Duration(periodSeconds)*time.Second+time.Minute)) {
		return AnchorAlignment{}, invalidf("The reset time must be in the future and within one period")
	}
	target := QuotaWindow{PeriodSeconds: periodSeconds, CycleAnchorAt: resetAt}
	inspect := func(state *State) (int, int, AnchorAlignment) {
		planIndex := -1
		for i := range state.Plans {
			if state.Plans[i].ID == planID {
				planIndex = i
			}
		}
		if planIndex < 0 {
			return -1, -1, AnchorAlignment{Status: AnchorNoWindow}
		}
		plan := state.Plans[planIndex]
		windowIndex := -1
		for i, window := range plan.Windows {
			if window.PeriodSeconds == periodSeconds {
				windowIndex = i
			}
		}
		if windowIndex < 0 {
			return planIndex, -1, AnchorAlignment{Status: AnchorNoWindow}
		}
		window := plan.Windows[windowIndex]
		if !window.CycleAnchorAt.IsZero() && window.sameSchedule(target) {
			return planIndex, windowIndex, AnchorAlignment{Status: AnchorAligned}
		}
		if window.CycleAnchorAt.IsZero() && len(plan.Windows) > 1 {
			return planIndex, windowIndex, AnchorAlignment{Status: AnchorUnsupported}
		}
		var waiting AnchorAlignment
		for _, key := range state.Keys {
			if key == nil || key.PlanID != planID {
				continue
			}
			if cycle, ok := key.Cycles[window.ID]; ok && now.Before(cycle.EndAt) {
				waiting.Status = AnchorWaiting
				if cycle.EndAt.After(waiting.WaitUntil) {
					waiting.WaitUntil = cycle.EndAt
				}
			}
		}
		if waiting.Status != "" {
			return planIndex, windowIndex, waiting
		}
		return planIndex, windowIndex, AnchorAlignment{Status: AnchorChanged}
	}
	var current AnchorAlignment
	s.read(func(state *State) { _, _, current = inspect(state) })
	if current.Status != AnchorChanged {
		return current, nil
	}
	result, err := editConfiguration(s, func(state *State) (AnchorAlignment, Changes, error) {
		planIndex, windowIndex, result := inspect(state)
		if result.Status != AnchorChanged {
			return result, Changes{}, nil
		}
		window := &state.Plans[planIndex].Windows[windowIndex]
		window.CycleAnchorAt = resetAt
		var keys []string
		for scope, key := range state.Keys {
			if key == nil || key.PlanID != planID {
				continue
			}
			if _, ok := key.Cycles[window.ID]; ok {
				delete(key.Cycles, window.ID)
			}
			keys = append(keys, scope)
		}
		return result, Changes{Plans: true, Keys: keys}, nil
	})
	if err == nil && result.Status == AnchorChanged {
		s.AddPluginLog(PluginLogInfo, "Aligned the %d-second window of subscription plan %q with the upstream account reset at %s",
			periodSeconds, planID, resetAt.Format(time.RFC3339))
	}
	return result, err
}

// SetClock replaces the store's clock; tests outside this package use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

package billing

import (
	"fmt"
	"strings"
	"time"
)

// This file is the cpa-super-billing bridge into the billing store. It keeps the
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
// A running cycle is never cut short: while any key on the plan, or the
// plan's shared pool, has an active cycle in that window, the anchor stays
// and the result is waiting. Once those cycles have ended, the anchor moves
// and the ended cycles are cleared; the next cycle the key's
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
		// A shared pool's cycle outlives its members' key cycles: unbinding,
		// rebinding or a new member's first request leaves it running alone.
		if cycle, ok := plan.Cycles[window.ID]; ok && now.Before(cycle.EndAt) {
			waiting.Status = AnchorWaiting
			if cycle.EndAt.After(waiting.WaitUntil) {
				waiting.WaitUntil = cycle.EndAt
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
		plan := &state.Plans[planIndex]
		window := &plan.Windows[windowIndex]
		window.CycleAnchorAt = resetAt
		// An ended pool cycle that admission has not settled yet would no
		// longer lie on the new anchor's grid, and saving it would fail.
		delete(plan.Cycles, window.ID)
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
		return result, Changes{Plans: true, PlanCycles: []string{planID}, Keys: keys}, nil
	})
	if err == nil && result.Status == AnchorChanged {
		s.AddPluginLog(PluginLogInfo, "Aligned the %d-second window of subscription plan %q with the upstream account reset at %s",
			periodSeconds, planID, resetAt.Format(time.RFC3339))
	}
	return result, err
}

// PlanByID returns a copy of a subscription plan. It only reads, so hot paths
// may call it; unlike PlanViews it never settles or saves shared pool cycles.
func (s *Store) PlanByID(id string) (Plan, bool) {
	var plan Plan
	found := false
	s.read(func(state *State) {
		if current := state.FindPlan(id); current != nil {
			plan, found = clonePlan(*current), true
		}
	})
	return plan, found
}

// SetClock replaces the store's clock; tests outside this package use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// UsageObserver receives each recorded request after it is stored: its key,
// request time, cost in USD and usage sequence number.
type UsageObserver func(scope string, at time.Time, costUSD float64, seq uint64)

// SetUsageObserver installs the observer recorded usage is reported to. It is
// set once, before the store receives traffic.
func (s *Store) SetUsageObserver(observer UsageObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageObserver = observer
}

func (s *Store) notifyUsage(event RequestEvent, seq uint64) {
	s.mu.RLock()
	observer, rate := s.usageObserver, s.cfg.CNYPerUSD
	s.mu.RUnlock()
	if observer == nil || event.Scope == "" {
		return
	}
	observer(event.Scope, event.At, rate.Convert(event.Cost.Total, event.Cost.Currency, USD), seq)
}

// KeyViewForScopeAt is KeyViewForScope that also returns the usage sequence
// number the view includes: every usage numbered at or below it is counted.
func (s *Store) KeyViewForScopeAt(scope string) (KeyView, bool, uint64) {
	scope = normalizeScope(scope)
	if scope == "" {
		return KeyView{}, false, 0
	}
	type result struct {
		view KeyView
		ok   bool
		seq  uint64
	}
	current := updateResult(s, func(state *State) (result, Changes) {
		key := state.Keys[scope]
		if key == nil || !key.DeletedAt.IsZero() {
			return result{seq: s.usageSeq}, Changes{}
		}
		now := s.Now()
		plan := state.FindPlan(key.PlanID)
		changed := settleQuota(scope, key, plan, now)
		return result{view: s.keyView(state, scope, key, plan, now), ok: true, seq: s.usageSeq}, changed
	})
	return current.view, current.ok, current.seq
}

// UsageTotalsAt is UsageTotals that also returns the usage sequence number
// the totals include.
func (s *Store) UsageTotalsAt(scope string, queries []UsageQuery) ([]UsageTotal, uint64, error) {
	type result struct {
		totals []UsageTotal
		seq    uint64
	}
	current, err := withRepository(s, func(repo Repository) (result, error) {
		totals := make([]UsageTotal, len(queries))
		cutoff := s.cfg.RequestEventRetentionDays.cutoff(s.Now())
		for i, query := range queries {
			if query.From.Before(cutoff) {
				query.From = cutoff
			}
			total, err := repo.UsageTotal(scope, query, s.cfg.CNYPerUSD)
			if err != nil {
				return result{}, err
			}
			totals[i] = total
		}
		return result{totals: totals, seq: s.usageSeq}, nil
	})
	if err == nil && current.totals == nil {
		// The repository is not open.
		return nil, 0, fmt.Errorf("The billing database is not open")
	}
	return current.totals, current.seq, err
}

// UpsertPrices stores several custom prices as one change: when any of them
// is invalid or cannot be saved, none is.
func (s *Store) UpsertPrices(prices []CustomPrice) error {
	prepared := make([]CustomPrice, len(prices))
	for i, price := range prices {
		price.ModelID = strings.TrimSpace(price.ModelID)
		if price.Currency == "" {
			price.Currency = USD
		}
		if err := price.Validate(); err != nil {
			return err
		}
		prepared[i] = price
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range prepared {
		if existing, found := s.state.Prices[NormalizeModelID(prepared[i].ModelID)]; found {
			// Preserve the stored spelling when updating the same model.
			prepared[i].ModelID = existing.ModelID
		}
	}
	if s.repo != nil {
		batch, ok := s.repo.(interface{ UpsertPrices([]CustomPrice) error })
		if !ok {
			return fmt.Errorf("The billing repository does not support batch price imports")
		}
		if err := batch.UpsertPrices(prepared); err != nil {
			return err
		}
	}
	for _, price := range prepared {
		s.state.Prices[NormalizeModelID(price.ModelID)] = price
	}
	return nil
}

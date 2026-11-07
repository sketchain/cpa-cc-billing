package ccbilling

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"cpa-super-billing/internal/messages"
)

// Estimate bases.
const (
	// BasisRaw estimates from costs as recorded.
	BasisRaw = "raw"
	// BasisWeighted converts costs to baseline USD with the effective
	// multipliers first, so $/1% means baseline USD per point.
	BasisWeighted = "weighted"
)

// Sources of an item's effective multiplier, in order of precedence.
const (
	WeightSourceManual      = "manual"
	WeightSourceCalibration = "calibration"
	WeightSourceEstimated   = "estimated"
	// WeightSourceDefault: nothing usable, so the item counts as baseline.
	WeightSourceDefault = "default"
)

// weightRecompute bounds how long an unchanged weight estimate is reused;
// only the recent period's boundary moves meanwhile.
const weightRecompute = 10 * time.Minute

// EffectiveWeight is the multiplier an item's costs are converted with.
type EffectiveWeight struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Models     []string `json:"models"`
	Multiplier float64  `json:"multiplier"`
	Source     string   `json:"source"`
	// Status is the historical estimate's status, which explains a default.
	Status        string `json:"status"`
	CalibrationID int64  `json:"calibration_id,omitempty"`
}

// ModelUsage is one model's cost in the retained segments of the configured
// accounts. Model "" sums cost without a known model, including segments
// recorded before the split existed.
type ModelUsage struct {
	Model    string  `json:"model"`
	CostUSD  float64 `json:"cost_usd"`
	Share    float64 `json:"share"`
	Segments int     `json:"segments"`
	ItemID   string  `json:"item_id,omitempty"`
}

// weightsSnapshot is what a refresh computed about the quota weights.
type weightsSnapshot struct {
	basis        string
	estimate     WeightEstimate
	effective    []EffectiveWeight
	factors      map[string]float64
	calibrations []Calibration
	models       []ModelUsage
	unsplit      int
	segments     int
}

type weightCache struct {
	key      string
	at       time.Time
	estimate WeightEstimate
}

// refreshWeights needs refreshMu and runs within withDB after the accounts
// advanced. Each provider's accounts have their own multipliers: it estimates
// them, settles calibrations and resolves the effective multipliers.
func (m *Manager) refreshWeights(db *sql.DB, now time.Time) (map[string]weightsSnapshot, error) {
	calibrations, err := loadCalibrations(db)
	if err != nil {
		return nil, err
	}
	result := map[string]weightsSnapshot{}
	for _, provider := range Providers {
		var own []Calibration
		for _, calibration := range calibrations {
			if m.settings.AccountProvider(calibration.AuthIndex) == provider {
				own = append(own, calibration)
			}
		}
		weights, err := m.refreshProviderWeights(db, provider, own, now)
		if err != nil {
			return nil, err
		}
		result[provider] = weights
	}
	return result, nil
}

func (m *Manager) refreshProviderWeights(db *sql.DB, provider string, calibrations []Calibration, now time.Time) (weightsSnapshot, error) {
	settings := m.settings.provider(provider).Weights
	params := m.settings.ParamsFor(provider)
	result := weightsSnapshot{basis: BasisRaw}
	if settings.Enabled {
		result.basis = BasisWeighted
	}
	indexes := m.settings.providerAccounts(provider)
	accounts := make([]AccountSegments, 0, len(indexes))
	for _, index := range indexes {
		accounts = append(accounts, AccountSegments{AuthIndex: index, Segments: m.accounts[index].segments})
	}
	result.models, result.unsplit, result.segments = modelUsage(accounts, settings)

	tested := map[string]bool{}
	for _, item := range settings.Items {
		for _, model := range item.Models {
			tested[model] = true
		}
	}
	for i := range calibrations {
		calibration := &calibrations[i]
		if calibration.Final {
			continue
		}
		cache := m.accounts[calibration.AuthIndex]
		if cache == nil {
			calibration.Result = CalibrationResult{Status: CalibrationInsufficient, BaseFrom: CalibrationBasePeriod}
			continue
		}
		end := now
		if !calibration.Running() {
			end = calibration.EndedAt
		}
		models := maps.Clone(tested)
		for _, model := range calibration.Item.Models {
			models[model] = true
		}
		calibration.Result = calibrate(cache.segments, calibration.Item, models, calibration.StartedAt, end, params)
		if calibration.Running() {
			calibration.Result.Status = CalibrationRunning
			continue
		}
		if !cache.stream.Cursor.Before(calibration.EndedAt) {
			// Every segment of the period is processed: the result is final.
			if err := finishCalibration(db, calibration.ID, calibration.Result); err != nil {
				return result, err
			}
			calibration.Final = true
		}
	}
	result.calibrations = calibrations

	key := weightCacheKey(params, settings, accounts)
	if m.weightCache == nil {
		m.weightCache = map[string]weightCache{}
	}
	cached := m.weightCache[provider]
	if key == cached.key && now.Sub(cached.at) < weightRecompute && !now.Before(cached.at) {
		result.estimate = cached.estimate
	} else {
		result.estimate = EstimateWeights(accounts, settings, params, now)
		m.weightCache[provider] = weightCache{key: key, at: now, estimate: result.estimate}
	}
	result.effective = resolveWeights(settings, result.estimate, calibrations)
	multipliers := make([]float64, len(result.effective))
	for i, weight := range result.effective {
		multipliers[i] = weight.Multiplier
	}
	result.factors = weightFactors(settings.Items, multipliers)
	return result, nil
}

// weightCacheKey changes whenever the estimate's inputs other than the clock do.
func weightCacheKey(params Params, settings WeightSettings, accounts []AccountSegments) string {
	var builder strings.Builder
	raw, _ := json.Marshal(struct {
		Params Params
		Items  []WeightItem
		Min    int
		Cost   float64
		Spread float64
	}{params, settings.Items, settings.MinSegments, settings.MinCostUSD, settings.MinShareSpread})
	builder.Write(raw)
	for _, account := range accounts {
		builder.WriteString("\x00" + account.AuthIndex)
		if n := len(account.Segments); n > 0 {
			builder.WriteString(account.Segments[0].At.Format(time.RFC3339Nano) + account.Segments[n-1].At.Format(time.RFC3339Nano))
			builder.WriteString(strconv.Itoa(n))
		}
	}
	return builder.String()
}

// resolveWeights picks each item's multiplier: a manual override, else the
// latest final calibration with a result, else the historical estimate. An
// item without any counts as baseline.
func resolveWeights(settings WeightSettings, estimate WeightEstimate, calibrations []Calibration) []EffectiveWeight {
	result := make([]EffectiveWeight, 0, len(settings.Items))
	for i, item := range settings.Items {
		weight := EffectiveWeight{ID: item.ID, Name: item.Name, Models: item.Models, Multiplier: 1, Source: WeightSourceDefault, Status: WeightInsufficient}
		var itemEstimate ItemEstimate
		if i < len(estimate.Items) && estimate.Items[i].ID == item.ID {
			itemEstimate = estimate.Items[i]
			weight.Status = itemEstimate.Status
		}
		var calibration *Calibration
		for j := range calibrations {
			candidate := &calibrations[j]
			if candidate.Item.ID != item.ID || !candidate.Final || candidate.Result.Status != CalibrationOK || candidate.Result.Multiplier == nil {
				continue
			}
			if calibration == nil || candidate.EndedAt.After(calibration.EndedAt) {
				calibration = candidate
			}
		}
		switch {
		case item.Manual != nil:
			weight.Multiplier, weight.Source = *item.Manual, WeightSourceManual
		case calibration != nil:
			weight.Multiplier, weight.Source, weight.CalibrationID = *calibration.Result.Multiplier, WeightSourceCalibration, calibration.ID
		case itemEstimate.Status == WeightOK && itemEstimate.Multiplier != nil:
			weight.Multiplier, weight.Source = *itemEstimate.Multiplier, WeightSourceEstimated
		}
		result = append(result, weight)
	}
	return result
}

// modelUsage sums the segments' cost by model.
func modelUsage(accounts []AccountSegments, settings WeightSettings) ([]ModelUsage, int, int) {
	totals := map[string]*ModelUsage{}
	itemOf := settings.itemFor()
	var total float64
	unsplit, segments := 0, 0
	for _, account := range accounts {
		for _, segment := range account.Segments {
			segments++
			if segment.CostUSD <= 0 {
				continue
			}
			models := segment.Models
			if len(models) == 0 {
				unsplit++
				models = map[string]float64{unknownModel: segment.CostUSD}
			} else if !segment.Split() {
				unsplit++
			}
			for model, cost := range models {
				usage := totals[model]
				if usage == nil {
					usage = &ModelUsage{Model: model}
					if index, ok := itemOf[model]; ok {
						usage.ItemID = settings.Items[index].ID
					}
					totals[model] = usage
				}
				usage.CostUSD += cost
				usage.Segments++
				total += cost
			}
		}
	}
	result := make([]ModelUsage, 0, len(totals))
	for _, usage := range totals {
		if total > 0 {
			usage.Share = usage.CostUSD / total
		}
		result = append(result, *usage)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CostUSD != result[j].CostUSD {
			return result[i].CostUSD > result[j].CostUSD
		}
		return result[i].Model < result[j].Model
	})
	return result, unsplit, segments
}

// BasisValues is an account's estimate on one basis.
type BasisValues struct {
	// Ratio is points per USD; USDPerPoint its inverse. On the weighted
	// basis the USD are baseline USD.
	Ratio            float64 `json:"ratio"`
	USDPerPoint      float64 `json:"usd_per_point"`
	FiveHourUSD      float64 `json:"five_hour_usd"`
	WeeklyUSD        float64 `json:"weekly_usd"`
	TotalsFrom       string  `json:"totals_from"`
	Segments         int     `json:"segments"`
	ExcludedSegments int     `json:"excluded_segments"`
	CleanBlocks      int     `json:"clean_blocks"`
}

// BasisComparison shows an account's estimate on both bases.
type BasisComparison struct {
	AuthIndex string      `json:"auth_index"`
	Raw       BasisValues `json:"raw"`
	Weighted  BasisValues `json:"weighted"`
}

// WeightsView is the quota weights on the CC page.
type WeightsView struct {
	Basis        string            `json:"basis"`
	Estimate     WeightEstimate    `json:"estimate"`
	Effective    []EffectiveWeight `json:"effective"`
	Calibrations []Calibration     `json:"calibrations"`
	Models       []ModelUsage      `json:"models"`
	Comparison   []BasisComparison `json:"comparison"`
	// UnsplitSegments of Segments retained have no model split; a rebuild
	// adds it from the retained request history.
	UnsplitSegments int `json:"unsplit_segments"`
	Segments        int `json:"segments"`
}

func emptyWeightsView() WeightsView {
	return WeightsView{Basis: BasisRaw, Estimate: WeightEstimate{Items: []ItemEstimate{}}, Effective: []EffectiveWeight{},
		Calibrations: []Calibration{}, Models: []ModelUsage{}, Comparison: []BasisComparison{}}
}

func emptyWeightsViews() map[string]WeightsView {
	result := map[string]WeightsView{}
	for _, provider := range Providers {
		result[provider] = emptyWeightsView()
	}
	return result
}

func basisValues(authIndex string, settings Settings, estimate Estimate, account accountSnapshot, latest Observation, now time.Time) BasisValues {
	status := ResolveAccount(authIndex, settings.Accounts[authIndex], estimate, account.k, latest, settings.accountParams(authIndex), now)
	values := BasisValues{Ratio: estimate.Ratio, FiveHourUSD: status.FiveHourTotal, WeeklyUSD: status.WeeklyTotal, TotalsFrom: status.TotalsFrom,
		Segments: estimate.Segments, ExcludedSegments: estimate.ExcludedSegments, CleanBlocks: estimate.CleanBlocks}
	if estimate.Ratio > 0 {
		values.USDPerPoint = 1 / estimate.Ratio
	}
	return values
}

// weightsViews is each provider's quota weights.
func weightsViews(snap *snapshot, latest map[string]Observation, now time.Time) map[string]WeightsView {
	result := map[string]WeightsView{}
	for _, provider := range Providers {
		result[provider] = weightsView(snap, provider, latest, now)
	}
	return result
}

func weightsView(snap *snapshot, provider string, latest map[string]Observation, now time.Time) WeightsView {
	view := emptyWeightsView()
	weights := snap.weights[provider]
	if weights.basis != "" {
		view.Basis = weights.basis
	}
	if weights.estimate.Items != nil {
		view.Estimate = weights.estimate
	}
	if weights.effective != nil {
		view.Effective = weights.effective
	}
	if weights.calibrations != nil {
		view.Calibrations = weights.calibrations
	}
	if weights.models != nil {
		view.Models = weights.models
	}
	view.UnsplitSegments, view.Segments = weights.unsplit, weights.segments
	for _, index := range snap.settings.providerAccounts(provider) {
		account := snap.accounts[index]
		view.Comparison = append(view.Comparison, BasisComparison{AuthIndex: index,
			Raw:      basisValues(index, snap.settings, account.raw, account, latest[index], now),
			Weighted: basisValues(index, snap.settings, account.weighted, account, latest[index], now)})
	}
	return view
}

// AccountEstimate is an account's estimate on the basis in effect.
type AccountEstimate struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	BasisValues
	K          *float64  `json:"k,omitempty"`
	KSource    string    `json:"k_source"`
	ColdStart  bool      `json:"cold_start"`
	Used5h     float64   `json:"used_5h"`
	Used7d     float64   `json:"used_7d"`
	Reset5h    time.Time `json:"reset_5h,omitzero"`
	Reset7d    time.Time `json:"reset_7d,omitzero"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// ProviderWeights is the basis one provider's estimates are on and the
// multipliers that basis uses.
type ProviderWeights struct {
	Basis   string            `json:"basis"`
	Weights []EffectiveWeight `json:"weights"`
}

// Estimates is every configured account's estimate on the basis in effect
// for its provider, with the multipliers each basis uses.
type Estimates struct {
	At        time.Time                  `json:"at"`
	Providers map[string]ProviderWeights `json:"providers"`
	Accounts  map[string]AccountEstimate `json:"accounts"`
}

// For is the basis and multipliers of one provider's estimates.
func (e Estimates) For(provider string) ProviderWeights {
	weights, ok := e.Providers[provider]
	if !ok || weights.Basis == "" {
		return ProviderWeights{Basis: BasisRaw, Weights: []EffectiveWeight{}}
	}
	return weights
}

// Estimates returns the estimates of the latest snapshot on the basis in
// effect. It is the one place later features read the estimates from; it
// never refreshes or touches the database, and reports false before the
// first snapshot.
func (m *Manager) Estimates() (Estimates, bool) {
	snap := m.snap.Load()
	if snap == nil {
		return Estimates{}, false
	}
	now := m.host.Now()
	latest, _ := m.live(snap)
	result := Estimates{At: snap.at, Providers: map[string]ProviderWeights{}, Accounts: map[string]AccountEstimate{}}
	for _, provider := range Providers {
		weights := snap.weights[provider]
		entry := ProviderWeights{Basis: weights.basis, Weights: slices.Clone(weights.effective)}
		if entry.Basis == "" {
			entry.Basis = BasisRaw
		}
		if entry.Weights == nil {
			entry.Weights = []EffectiveWeight{}
		}
		result.Providers[provider] = entry
	}
	for _, index := range snap.settings.authIndexes() {
		account := snap.accounts[index]
		status := accountStatus(snap, latest, index, now)
		result.Accounts[index] = AccountEstimate{AuthIndex: index, Provider: status.Provider,
			BasisValues: basisValues(index, snap.settings, account.estimate, account, latest[index], now),
			K:           status.K, KSource: status.KSource, ColdStart: status.ColdStart, Used5h: status.Used5h, Used7d: status.Used7d,
			Reset5h: status.Reset5h, Reset7d: status.Reset7d, ObservedAt: status.ObservedAt}
	}
	return result, true
}

// StartCalibration starts calibrating an item on an account now. An account
// runs one calibration at a time.
func (m *Manager) StartCalibration(itemID, authIndex string) (Calibration, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	var calibration Calibration
	err := m.withDB(func(db *sql.DB) error {
		if !slices.Contains(m.settings.authIndexes(), authIndex) {
			return invalidf("Account %q is not configured in the CC split", authIndex)
		}
		item, ok := m.settings.provider(m.settings.AccountProvider(authIndex)).Weights.item(itemID)
		if !ok {
			return invalidf("Model test item %q does not exist", itemID)
		}
		existing, err := loadCalibrations(db)
		if err != nil {
			return err
		}
		for _, other := range existing {
			if other.AuthIndex == authIndex && other.Running() {
				return invalidf("Account %q is already calibrating; end that calibration first", authIndex)
			}
		}
		now := m.host.Now()
		id, err := insertCalibration(db, item, authIndex, now)
		if err != nil {
			return err
		}
		calibration = Calibration{ID: id, Item: item, AuthIndex: authIndex, StartedAt: now,
			Result: CalibrationResult{Status: CalibrationRunning, BaseFrom: CalibrationBasePeriod}}
		return nil
	})
	if err != nil {
		return Calibration{}, err
	}
	return calibration, m.refreshLocked()
}

// ErrCalibrationNotFound reports a calibration that does not exist or, when
// ending one, has already ended.
var ErrCalibrationNotFound = errCalibrationNotFound

// StopCalibration ends a running calibration now.
func (m *Manager) StopCalibration(id int64) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(func(db *sql.DB) error { return endCalibration(db, id, m.host.Now()) }); err != nil {
		return err
	}
	return m.refreshLocked()
}

// DeleteCalibration deletes a calibration and its result.
func (m *Manager) DeleteCalibration(id int64) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := m.withDB(func(db *sql.DB) error { return deleteCalibration(db, id) }); err != nil {
		return err
	}
	return m.refreshLocked()
}

// IsCalibrationNotFound reports whether err means the calibration is missing.
func IsCalibrationNotFound(err error) bool { return errors.Is(err, errCalibrationNotFound) }

// invalidError marks a request the operator can correct, keeping its message.
type invalidError struct{ error }

func (e invalidError) MessageDetail() messages.Message { return messages.FromError(e.error) }
func (e invalidError) Unwrap() error                   { return e.error }

func invalidf(format string, args ...any) error {
	return invalidError{messages.Errorf(format, args...)}
}

// IsInvalid reports whether err rejects the request itself.
func IsInvalid(err error) bool {
	var invalid invalidError
	return errors.As(err, &invalid)
}

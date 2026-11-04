package ccbilling

import (
	"math"
	"slices"
	"time"
)

// Quota weights: some models may use more subscription quota per USD than
// others. Every 1% segment records its cost by model, so its gain is modelled
// as
//
//	gain ≈ s_a × (C_base + Σ_i m_i × C_i)
//
// where C_base is the cost of baseline models (every model outside the items
// under test), C_i the cost of item i, s_a the account's baseline points per
// USD and m_i item i's multiplier. Accounts have their own s_a, since their
// tiers differ; the multipliers are shared.

// Statuses of an item's estimated multiplier.
const (
	WeightOK = "ok"
	// WeightInsufficient: too few clean segments or too little cost include the item.
	WeightInsufficient = "insufficient"
	// WeightCollinear: the item's cost share varies too little between chunks
	// to tell its multiplier apart from the baseline's.
	WeightCollinear = "collinear"
	// WeightUnstable: the fit failed or its error is too large.
	WeightUnstable = "unstable"
)

const (
	// maxWeightVIF bounds the variance inflation of an item's multiplier.
	maxWeightVIF = 100
	// maxWeightRelativeError bounds an item's standard error relative to its multiplier.
	maxWeightRelativeError = 0.5
	// weightIterations bounds the rounds of filtering on the converted basis.
	weightIterations = 10
	weightTolerance  = 1e-3
	// The seed fit drops chunks whose gain exceeds the prediction by this
	// many standard deviations, for this many rounds: outside usage only
	// raises gain.
	seedTrimRounds = 3
	seedTrimSigma  = 2.5
	z95            = 1.959964
	// chunkSpan is how long a chunk runs before it ends at the next rise.
	// Ending chunks by elapsed time rather than by gain keeps the rounding
	// error out of the costs: a chunk cut at a fixed gain makes its cost the
	// noisy quantity, which biases the multipliers towards 1.
	chunkSpan = 30 * time.Minute
)

// AccountSegments is one account's retained segments, oldest first.
type AccountSegments struct {
	AuthIndex string
	Segments  []Segment
}

// ItemEstimate is the estimated multiplier of one item under test.
type ItemEstimate struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Multiplier, StdErr and the 95% interval are set when Status is ok.
	Multiplier *float64 `json:"multiplier,omitempty"`
	StdErr     float64  `json:"std_err,omitempty"`
	Low        float64  `json:"low,omitempty"`
	High       float64  `json:"high,omitempty"`
	// Segments and CostUSD count the clean split segments that include the item.
	Segments int     `json:"segments"`
	CostUSD  float64 `json:"cost_usd"`
	// Share is the item's share of the clean split cost; ShareMin, ShareMax
	// and ShareSpread describe its share across the fitted chunks.
	Share       float64 `json:"share"`
	ShareMin    float64 `json:"share_min"`
	ShareMax    float64 `json:"share_max"`
	ShareSpread float64 `json:"share_spread"`
	VIF         float64 `json:"vif,omitempty"`
}

// WeightEstimate is the outcome of estimating every item's multiplier.
type WeightEstimate struct {
	Items []ItemEstimate `json:"items"`
	// Chunks and Segments count the clean split data the fit used.
	Chunks   int `json:"chunks"`
	Segments int `json:"segments"`
	// Excluded counts segments the filter excluded on the converted basis;
	// Unsplit counts segments without a model split.
	Excluded   int  `json:"excluded_segments"`
	Unsplit    int  `json:"unsplit_segments"`
	Iterations int  `json:"iterations"`
	Converged  bool `json:"converged"`
}

// multipliers returns each item's estimated multiplier, 1 when it has none.
func (e WeightEstimate) multipliers() []float64 {
	result := make([]float64, len(e.Items))
	for i, item := range e.Items {
		result[i] = 1
		if item.Status == WeightOK && item.Multiplier != nil {
			result[i] = *item.Multiplier
		}
	}
	return result
}

// weightFactors maps each model of an item whose multiplier is not 1 to it.
func weightFactors(items []WeightItem, multipliers []float64) map[string]float64 {
	factors := map[string]float64{}
	for i, item := range items {
		if i < len(multipliers) && multipliers[i] != 1 {
			for _, model := range item.Models {
				factors[model] = multipliers[i]
			}
		}
	}
	return factors
}

// ConvertSegments returns the segments with each cost on the baseline basis:
// a model's cost counts factor times. Cost of other models, of unknown models
// and of unsplit segments is unchanged. Without factors it returns the
// segments themselves, so their costs stay bit for bit the same.
func ConvertSegments(segments []Segment, factors map[string]float64) []Segment {
	if len(factors) == 0 {
		return segments
	}
	models := sortedKeys(factors)
	out := make([]Segment, len(segments))
	for i, segment := range segments {
		out[i] = segment
		for _, model := range models {
			if cost, ok := segment.Models[model]; ok {
				out[i].CostUSD += cost * (factors[model] - 1)
			}
		}
	}
	return out
}

// weightChunk sums consecutive clean split segments of one block until they
// gain about a window. A chunk's gain is the difference of two reports, so
// whole-percent rounding adds the same error to every chunk.
type weightChunk struct {
	account int
	start   time.Time
	at      time.Time
	gain    float64
	base    float64
	items   []float64
}

type chunkSet struct {
	chunks       []weightChunk
	segments     int
	excluded     int
	unsplit      int
	itemSegments []int
	itemCost     []float64
}

// buildChunks cuts each block's split segments into chunks of about span and
// drops every chunk with a segment the masks exclude; nil masks keep every
// one. Chunks end only at rises, by elapsed time, and are kept or dropped
// whole: cutting them at excluded segments would select on the rounding
// error, which biases the fit just like cutting them at a fixed gain.
func buildChunks(accounts []AccountSegments, masks [][]bool, itemOf map[string]int, items int, span time.Duration) chunkSet {
	set := chunkSet{itemSegments: make([]int, items), itemCost: make([]float64, items)}
	for a, account := range accounts {
		var current *weightChunk
		var segments, excluded int
		var itemSegments []int
		flush := func() {
			if current != nil && current.gain > gainEpsilon {
				if excluded > 0 {
					set.excluded += excluded + segments
				} else {
					set.excluded += excluded
					set.segments += segments
					set.chunks = append(set.chunks, *current)
					for i := range items {
						set.itemSegments[i] += itemSegments[i]
						set.itemCost[i] += current.items[i]
					}
				}
			} else {
				set.excluded += excluded
			}
			current, segments, excluded = nil, 0, 0
		}
		var block, previous time.Time
		for s, segment := range account.Segments {
			if !segment.Block.Equal(block) {
				flush()
				block, previous = segment.Block, segment.Block
			}
			start := previous
			previous = segment.At
			if !segment.Split() {
				set.unsplit++
				flush()
				continue
			}
			if current == nil {
				current = &weightChunk{account: a, start: start, items: make([]float64, items)}
				itemSegments = make([]int, items)
			}
			current.at = segment.At
			current.gain += segment.Gain
			if masks != nil && masks[a] != nil && masks[a][s] {
				excluded++
			} else {
				segments++
			}
			seen := make([]bool, items)
			for _, model := range sortedKeys(segment.Models) {
				cost := segment.Models[model]
				if index, ok := itemOf[model]; ok {
					if cost > 0 && !seen[index] {
						seen[index] = true
						itemSegments[index]++
					}
					current.items[index] += cost
				} else {
					current.base += cost
				}
			}
			if segment.At.Sub(current.start) >= span {
				flush()
			}
		}
		flush()
	}
	return set
}

// chunkWeights splits the weight between recent and older chunks by the
// recent share, like the global ratio, scaled to a mean of 1.
func chunkWeights(chunks []weightChunk, params Params, now time.Time) []float64 {
	cutoff := now.Add(-params.recent())
	recent := 0
	for _, chunk := range chunks {
		if !chunk.at.Before(cutoff) {
			recent++
		}
	}
	old := len(chunks) - recent
	weights := make([]float64, len(chunks))
	for i, chunk := range chunks {
		switch {
		case recent > 0 && old > 0 && !chunk.at.Before(cutoff):
			weights[i] = params.RecentShare / float64(recent)
		case recent > 0 && old > 0:
			weights[i] = (1 - params.RecentShare) / float64(old)
		default:
			weights[i] = 1 / float64(len(chunks))
		}
		weights[i] *= float64(len(chunks))
	}
	return weights
}

// EstimateWeights estimates every item's multiplier from the accounts'
// segments.
//
// The contamination filter judges points per USD, which the multipliers
// change: on the raw basis a segment heavy in a model that uses more quota
// per USD looks like outside usage. So the filter runs on the converted
// basis of the current multipliers, whatever the operator's switch says. The
// first multipliers come from a fit to every split segment that trims chunks
// gaining far more than predicted, since outside usage only raises gain;
// then filtering and fitting alternate until the multipliers settle.
func EstimateWeights(accounts []AccountSegments, weights WeightSettings, params Params, now time.Time) WeightEstimate {
	count := len(weights.Items)
	if count == 0 {
		return WeightEstimate{Items: []ItemEstimate{}}
	}
	itemOf := weights.itemFor()
	set := buildChunks(accounts, nil, itemOf, count, chunkSpan)
	fit := fitWeights(set, weights, params, now, true)
	multipliers := fit.multipliers()
	masks := make([][]bool, len(accounts))
	for iteration := 1; iteration <= weightIterations; iteration++ {
		factors := weightFactors(weights.Items, multipliers)
		for a, account := range accounts {
			masks[a] = SegmentExclusions(ConvertSegments(account.Segments, factors), params, now)
		}
		set = buildChunks(accounts, masks, itemOf, count, chunkSpan)
		fit = fitWeights(set, weights, params, now, false)
		fit.Iterations = iteration
		next := fit.multipliers()
		change := 0.0
		for i := range next {
			change = max(change, math.Abs(next[i]-multipliers[i]))
		}
		multipliers = next
		if change < weightTolerance {
			fit.Converged = true
			break
		}
	}
	return fit
}

// fitWeights fits the multipliers of the items with enough varied data.
// Items that are left out count as baseline in the fit.
func fitWeights(set chunkSet, settings WeightSettings, params Params, now time.Time, seed bool) WeightEstimate {
	count := len(settings.Items)
	result := WeightEstimate{Items: make([]ItemEstimate, count), Chunks: len(set.chunks), Segments: set.segments,
		Excluded: set.excluded, Unsplit: set.unsplit}
	weights := chunkWeights(set.chunks, params, now)
	var totalCost float64
	for _, chunk := range set.chunks {
		totalCost += chunk.base
		for _, cost := range chunk.items {
			totalCost += cost
		}
	}
	free := []int{}
	for i, item := range settings.Items {
		estimate := ItemEstimate{ID: item.ID, Segments: set.itemSegments[i], CostUSD: set.itemCost[i], ShareMin: math.Inf(1)}
		var sum, sumSquares, weightSum float64
		for j, chunk := range set.chunks {
			total := chunk.base
			for _, cost := range chunk.items {
				total += cost
			}
			if total <= 0 {
				continue
			}
			share := chunk.items[i] / total
			estimate.ShareMin, estimate.ShareMax = math.Min(estimate.ShareMin, share), math.Max(estimate.ShareMax, share)
			sum += weights[j] * share
			sumSquares += weights[j] * share * share
			weightSum += weights[j]
		}
		if weightSum > 0 {
			mean := sum / weightSum
			estimate.ShareSpread = math.Sqrt(math.Max(0, sumSquares/weightSum-mean*mean))
		} else {
			estimate.ShareMin = 0
		}
		if totalCost > 0 {
			estimate.Share = set.itemCost[i] / totalCost
		}
		switch {
		case estimate.Segments < settings.MinSegments || estimate.CostUSD < settings.MinCostUSD || estimate.CostUSD <= 0:
			estimate.Status = WeightInsufficient
		case estimate.ShareSpread < settings.MinShareSpread:
			estimate.Status = WeightCollinear
		default:
			free = append(free, i)
		}
		result.Items[i] = estimate
	}
	chunks := set.chunks
	for len(free) > 0 {
		solution, ok := solveWeights(chunks, weights, free, seed)
		if !ok {
			for _, i := range free {
				result.Items[i].Status = WeightUnstable
			}
			break
		}
		// Items whose multipliers cannot be told apart, from the baseline or
		// from each other, are taken out and the others refitted without them.
		var collinear []int
		for position, i := range free {
			if vif := solution.vif[position]; !(vif <= maxWeightVIF) {
				result.Items[i].Status, result.Items[i].VIF = WeightCollinear, vif
				collinear = append(collinear, i)
			}
		}
		if len(collinear) > 0 {
			free = slices.DeleteFunc(free, func(i int) bool { return slices.Contains(collinear, i) })
			continue
		}
		for position, i := range free {
			item := &result.Items[i]
			multiplier, stdErr := solution.multipliers[position], solution.stdErrs[position]
			item.VIF = solution.vif[position]
			if !finite(multiplier) || multiplier <= 0 || !finite(stdErr) || stdErr > maxWeightRelativeError*multiplier {
				item.Status = WeightUnstable
				continue
			}
			item.Status = WeightOK
			item.Multiplier = &multiplier
			item.StdErr = stdErr
			item.Low, item.High = math.Max(0, multiplier-z95*stdErr), multiplier+z95*stdErr
		}
		break
	}
	return result
}

type weightSolution struct {
	multipliers []float64
	stdErrs     []float64
	vif         []float64
}

// solveWeights fits s per account and m per free item by damped Gauss-Newton
// weighted least squares. The seed fit trims chunks far above the prediction.
func solveWeights(chunks []weightChunk, weights []float64, free []int, seed bool) (weightSolution, bool) {
	kept := make([]bool, len(chunks))
	for i := range kept {
		kept[i] = true
	}
	rounds := 1
	if seed {
		rounds += seedTrimRounds
	}
	var theta []float64
	var accountParam map[int]int
	for round := 0; round < rounds; round++ {
		accountParam = map[int]int{}
		for j, chunk := range chunks {
			if _, ok := accountParam[chunk.account]; !ok && kept[j] {
				accountParam[chunk.account] = len(accountParam)
			}
		}
		theta = fitTheta(chunks, weights, kept, free, accountParam)
		if theta == nil {
			return weightSolution{}, false
		}
		if round == rounds-1 {
			break
		}
		var sse, weightSum float64
		for j, chunk := range chunks {
			if kept[j] {
				r := chunk.gain - predict(chunk, theta, free, accountParam)
				sse += weights[j] * r * r
				weightSum += weights[j]
			}
		}
		sigma := math.Sqrt(sse / weightSum)
		trimmed := false
		for j, chunk := range chunks {
			if kept[j] && chunk.gain-predict(chunk, theta, free, accountParam) > seedTrimSigma*sigma {
				kept[j], trimmed = false, true
			}
		}
		if !trimmed {
			break
		}
	}
	params := len(accountParam) + len(free)
	n, sse := 0, 0.0
	hessian := newMatrix(params)
	for j, chunk := range chunks {
		if !kept[j] {
			continue
		}
		n++
		r := chunk.gain - predict(chunk, theta, free, accountParam)
		sse += weights[j] * r * r
		gradient := jacobian(chunk, theta, free, accountParam, params)
		for a := range params {
			for b := range params {
				hessian[a][b] += weights[j] * gradient[a] * gradient[b]
			}
		}
	}
	if n-params < 1 {
		return weightSolution{}, false
	}
	inverse, ok := invert(hessian)
	if !ok {
		solution := weightSolution{multipliers: make([]float64, len(free)), stdErrs: make([]float64, len(free)), vif: make([]float64, len(free))}
		for i := range solution.vif {
			solution.vif[i] = math.Inf(1)
		}
		return solution, true
	}
	variance := sse / float64(n-params)
	solution := weightSolution{}
	offset := len(accountParam)
	for position := range free {
		index := offset + position
		solution.multipliers = append(solution.multipliers, theta[index])
		solution.stdErrs = append(solution.stdErrs, math.Sqrt(math.Max(0, variance*inverse[index][index])))
		solution.vif = append(solution.vif, inverse[index][index]*hessian[index][index])
	}
	return solution, true
}

// predict is s_a × (C_base + Σ m_i × C_i), with items outside free counted
// as baseline.
func predict(chunk weightChunk, theta []float64, free []int, accountParam map[int]int) float64 {
	return theta[accountParam[chunk.account]] * convertedCost(chunk, theta[len(accountParam):], free)
}

func convertedCost(chunk weightChunk, multipliers []float64, free []int) float64 {
	cost := chunk.base
	for _, value := range chunk.items {
		cost += value
	}
	for position, i := range free {
		cost += chunk.items[i] * (multipliers[position] - 1)
	}
	return cost
}

func jacobian(chunk weightChunk, theta []float64, free []int, accountParam map[int]int, params int) []float64 {
	gradient := make([]float64, params)
	account := accountParam[chunk.account]
	gradient[account] = convertedCost(chunk, theta[len(accountParam):], free)
	for position, i := range free {
		gradient[len(accountParam)+position] = theta[account] * chunk.items[i]
	}
	return gradient
}

// fitTheta runs Levenberg-Marquardt from every multiplier at 1. It returns
// nil when the data cannot be fitted.
func fitTheta(chunks []weightChunk, weights []float64, kept []bool, free []int, accountParam map[int]int) []float64 {
	params := len(accountParam) + len(free)
	theta := make([]float64, params)
	gains, costs := make([]float64, len(accountParam)), make([]float64, len(accountParam))
	for j, chunk := range chunks {
		if kept[j] {
			a := accountParam[chunk.account]
			gains[a] += weights[j] * chunk.gain
			costs[a] += weights[j] * convertedCost(chunk, nil, nil)
		}
	}
	for a := range gains {
		if costs[a] <= 0 {
			return nil
		}
		theta[a] = gains[a] / costs[a]
	}
	for position := range free {
		theta[len(accountParam)+position] = 1
	}
	objective := func(candidate []float64) float64 {
		sum := 0.0
		for j, chunk := range chunks {
			if kept[j] {
				r := chunk.gain - predict(chunk, candidate, free, accountParam)
				sum += weights[j] * r * r
			}
		}
		return sum
	}
	sse := objective(theta)
	lambda := 1e-3
	for range 200 {
		hessian := newMatrix(params)
		gradient := make([]float64, params)
		for j, chunk := range chunks {
			if !kept[j] {
				continue
			}
			r := chunk.gain - predict(chunk, theta, free, accountParam)
			row := jacobian(chunk, theta, free, accountParam, params)
			for a := range params {
				gradient[a] += weights[j] * row[a] * r
				for b := range params {
					hessian[a][b] += weights[j] * row[a] * row[b]
				}
			}
		}
		improved := false
		for range 20 {
			damped := newMatrix(params)
			for a := range params {
				copy(damped[a], hessian[a])
				damped[a][a] += lambda * math.Max(hessian[a][a], 1e-12)
			}
			step, ok := solve(damped, gradient)
			if !ok {
				lambda *= 10
				continue
			}
			candidate := make([]float64, params)
			for a := range params {
				candidate[a] = theta[a] + step[a]
			}
			if next := objective(candidate); next < sse {
				done := sse-next <= 1e-14*math.Max(sse, 1e-300)
				theta, sse, improved = candidate, next, true
				lambda = math.Max(lambda/10, 1e-12)
				if done {
					return theta
				}
				break
			}
			lambda *= 10
		}
		if !improved {
			break
		}
	}
	for _, value := range theta {
		if !finite(value) {
			return nil
		}
	}
	return theta
}

func newMatrix(size int) [][]float64 {
	matrix := make([][]float64, size)
	for i := range matrix {
		matrix[i] = make([]float64, size)
	}
	return matrix
}

// solve returns x with a x = b by Gaussian elimination with partial pivoting.
func solve(a [][]float64, b []float64) ([]float64, bool) {
	size := len(b)
	inverse, ok := invert(a)
	if !ok {
		return nil, false
	}
	x := make([]float64, size)
	for i := range size {
		for j := range size {
			x[i] += inverse[i][j] * b[j]
		}
	}
	return x, true
}

// invert returns a's inverse after scaling it to a unit diagonal, which keeps
// the pivots meaningful when the parameters differ in scale. It reports false
// when a is singular or nearly so.
func invert(a [][]float64) ([][]float64, bool) {
	size := len(a)
	scale := make([]float64, size)
	for i := range size {
		if !(a[i][i] > 0) || !finite(a[i][i]) {
			return nil, false
		}
		scale[i] = 1 / math.Sqrt(a[i][i])
	}
	work := newMatrix(size)
	inverse := newMatrix(size)
	for i := range size {
		for j := range size {
			work[i][j] = a[i][j] * scale[i] * scale[j]
		}
		inverse[i][i] = 1
	}
	for column := range size {
		pivot := column
		for row := column + 1; row < size; row++ {
			if math.Abs(work[row][column]) > math.Abs(work[pivot][column]) {
				pivot = row
			}
		}
		if math.Abs(work[pivot][column]) < 1e-12 {
			return nil, false
		}
		work[column], work[pivot] = work[pivot], work[column]
		inverse[column], inverse[pivot] = inverse[pivot], inverse[column]
		divisor := work[column][column]
		for j := range size {
			work[column][j] /= divisor
			inverse[column][j] /= divisor
		}
		for row := range size {
			if row == column || work[row][column] == 0 {
				continue
			}
			factor := work[row][column]
			for j := range size {
				work[row][j] -= factor * work[column][j]
				inverse[row][j] -= factor * inverse[column][j]
			}
		}
	}
	for i := range size {
		for j := range size {
			inverse[i][j] *= scale[i] * scale[j]
		}
	}
	return inverse, true
}

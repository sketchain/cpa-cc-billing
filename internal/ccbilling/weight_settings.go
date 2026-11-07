package ccbilling

import (
	"fmt"
	"slices"
	"strings"

	"cpa-super-billing/internal/messages"
)

// WeightItem is one model under test: models whose subscription quota use
// per USD may differ from the baseline. Every model outside all items is
// baseline, with a weight of 1.
type WeightItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Models are normalized billing model names, such as one model's dated
	// variants.
	Models []string `json:"models"`
	// Manual overrides the calibrated and estimated multipliers when set.
	Manual *float64 `json:"manual_multiplier,omitempty"`
}

// Label is the item's name, or its ID when unnamed.
func (i WeightItem) Label() string {
	if i.Name != "" {
		return i.Name
	}
	return i.ID
}

// WeightSettings configure the quota weights of models under test.
type WeightSettings struct {
	// Enabled converts every segment's cost to the baseline basis before
	// the estimate: C_base + Σ m_i × C_i.
	Enabled bool         `json:"enabled"`
	Items   []WeightItem `json:"items"`
	// An item's multiplier is estimated only from at least MinSegments clean
	// segments that include it, costing at least MinCostUSD in total.
	MinSegments int     `json:"min_segments"`
	MinCostUSD  float64 `json:"min_cost_usd"`
	// MinShareSpread is the least standard deviation of an item's cost share
	// across chunks that tells its multiplier apart from the baseline's.
	MinShareSpread float64 `json:"min_share_spread"`
}

// maxWeightItems bounds the models under test; each adds a parameter.
const maxWeightItems = 8

func DefaultWeightSettings() WeightSettings {
	return WeightSettings{Items: []WeightItem{}, MinSegments: 30, MinCostUSD: 20, MinShareSpread: 0.05}
}

// itemFor maps each model of an item to the item's index.
func (w WeightSettings) itemFor() map[string]int {
	result := map[string]int{}
	for i, item := range w.Items {
		for _, model := range item.Models {
			result[model] = i
		}
	}
	return result
}

func (w WeightSettings) item(id string) (WeightItem, bool) {
	for _, item := range w.Items {
		if item.ID == id {
			return item, true
		}
	}
	return WeightItem{}, false
}

func (w WeightSettings) normalized() (WeightSettings, error) {
	if !w.Enabled && len(w.Items) == 0 && w.MinSegments == 0 && w.MinCostUSD == 0 && w.MinShareSpread == 0 {
		// Settings written without the weights, such as by an API client
		// that predates them, keep the defaults.
		return DefaultWeightSettings(), nil
	}
	switch {
	case w.MinSegments < 1 || w.MinSegments > 100000:
		return WeightSettings{}, fmt.Errorf("weights.min_segments must be an integer from 1 to 100000")
	case !finite(w.MinCostUSD) || w.MinCostUSD < 0 || w.MinCostUSD > 1e9:
		return WeightSettings{}, fmt.Errorf("weights.min_cost_usd must be a non-negative amount")
	case !finite(w.MinShareSpread) || w.MinShareSpread <= 0 || w.MinShareSpread > 0.5:
		return WeightSettings{}, fmt.Errorf("weights.min_share_spread must be greater than 0 and at most 0.5")
	case len(w.Items) > maxWeightItems:
		return WeightSettings{}, fmt.Errorf("At most %d models can be under test", maxWeightItems)
	}
	out := WeightSettings{Enabled: w.Enabled, Items: make([]WeightItem, 0, len(w.Items)), MinSegments: w.MinSegments,
		MinCostUSD: w.MinCostUSD, MinShareSpread: w.MinShareSpread}
	ids := map[string]bool{}
	owners := map[string]string{}
	for _, item := range w.Items {
		item.ID, item.Name = strings.TrimSpace(item.ID), strings.TrimSpace(item.Name)
		if item.ID == "" || len(item.ID) > 64 || ids[item.ID] {
			return WeightSettings{}, fmt.Errorf("Model test item IDs must be unique, non-empty and at most 64 bytes")
		}
		if len(item.Name) > 128 {
			return WeightSettings{}, fmt.Errorf("Model test item names must not exceed 128 bytes")
		}
		ids[item.ID] = true
		var models []string
		for _, model := range item.Models {
			model = NormalizeModel(model)
			if model == "" || slices.Contains(models, model) {
				continue
			}
			if len(model) > 1024 {
				return WeightSettings{}, fmt.Errorf("Model ID is too long")
			}
			if owner, taken := owners[model]; taken {
				return WeightSettings{}, messages.Errorf("Model %q is already in test item %q; a model can belong to only one item", model, owner)
			}
			owners[model] = item.Label()
			models = append(models, model)
		}
		if len(models) == 0 {
			return WeightSettings{}, fmt.Errorf("Model test item %q needs at least one model", item.Label())
		}
		slices.Sort(models)
		item.Models = models
		if item.Manual != nil && (!finite(*item.Manual) || *item.Manual <= 0 || *item.Manual > 100) {
			return WeightSettings{}, fmt.Errorf("Manual multipliers must be greater than 0 and at most 100")
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

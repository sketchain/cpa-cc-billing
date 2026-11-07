package ccbilling

import "slices"

// editWeights changes a provider's weights in a copy of settings' profiles,
// so settings read from a manager are not changed in place.
func editWeights(settings *Settings, provider string, edit func(*WeightSettings)) {
	if settings.Providers == nil {
		settings.Providers = DefaultProviders()
	}
	settings.Providers = cloneProviders(settings.Providers)
	profile := settings.provider(provider)
	profile.Weights.Items = slices.Clone(profile.Weights.Items)
	edit(&profile.Weights)
	settings.Providers[provider] = profile
}

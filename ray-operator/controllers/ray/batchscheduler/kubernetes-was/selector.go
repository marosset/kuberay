package kuberneteswas

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/rest"
)

// selectProvider resolves which versioned provider to use.
//
// When targetVersion is set, the provider registered for exactly that version is
// used (and must be served by the cluster). Otherwise registered providers are
// ranked by Kubernetes API version ordering and the highest-ranked version the
// cluster serves is selected.
func selectProvider(config *rest.Config, targetVersion string) (Provider, error) {
	if len(registeredProviders) == 0 {
		return nil, fmt.Errorf("no %s providers registered", PluginName)
	}

	providers := slices.Clone(registeredProviders)
	slices.SortStableFunc(providers, func(left, right Provider) int {
		return version.CompareKubeAwareVersionStrings(right.GroupVersion().Version, left.GroupVersion().Version)
	})

	if targetVersion != "" {
		return selectPinnedProvider(config, providers, targetVersion)
	}

	// A nil config (e.g. in unit tests) skips discovery and uses the preferred provider.
	if config == nil {
		return providers[0], nil
	}

	var unavailable []error
	for _, provider := range providers {
		if err := provider.Available(config); err != nil {
			unavailable = append(unavailable, err)
			continue
		}
		return provider, nil
	}
	return nil, fmt.Errorf("no served scheduling.k8s.io API version available for %s: %w", PluginName, errors.Join(unavailable...))
}

// selectPinnedProvider returns the provider for exactly targetVersion, requiring it
// to be registered and (unless config is nil) served by the cluster.
func selectPinnedProvider(config *rest.Config, providers []Provider, targetVersion string) (Provider, error) {
	for _, provider := range providers {
		if provider.GroupVersion().Version != targetVersion {
			continue
		}
		if config != nil {
			if err := provider.Available(config); err != nil {
				return nil, fmt.Errorf("pinned %s target version %q is not served by the cluster: %w", PluginName, targetVersion, err)
			}
		}
		return provider, nil
	}
	registered := make([]string, 0, len(providers))
	for _, provider := range providers {
		registered = append(registered, provider.GroupVersion().Version)
	}
	return nil, fmt.Errorf("no %s provider registered for pinned target version %q (registered: %s)", PluginName, targetVersion, strings.Join(registered, ", "))
}

package shared

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

// APIVersionAvailable returns nil when the given scheduling.k8s.io group-version is served by the
// cluster reachable through config. A nil config skips discovery (e.g. in unit tests).
func APIVersionAvailable(config *rest.Config, gv schema.GroupVersion) error {
	if config == nil {
		return nil
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}
	if _, err := discoveryClient.ServerResourcesForGroupVersion(gv.String()); err != nil {
		return fmt.Errorf("%s API is not available: %w", gv.String(), err)
	}
	return nil
}

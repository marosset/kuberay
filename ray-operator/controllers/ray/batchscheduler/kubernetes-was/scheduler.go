// Package kuberneteswas selects a versioned Kubernetes workload-aware scheduling
// (WAS) provider and exposes it as a KubeRay batch scheduler. Each supported
// scheduling.k8s.io API version has its own provider in a versioned subpackage
// (e.g. v1alpha2); this package picks the one served by the cluster.
package kuberneteswas

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	schedulerinterface "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/interface"
)

// PluginName is the version-agnostic umbrella identity for the Kubernetes WAS
// batch scheduler. Each per-version provider reports its own
// "kubernetes-was-<version>" identity from BatchScheduler.Name().
const PluginName = "kubernetes-was"

// GetPluginName returns the version-agnostic umbrella name for Kubernetes WAS.
func GetPluginName() string { return PluginName }

// Provider is a versioned implementation of Kubernetes workload-aware scheduling.
// Each provider targets one scheduling.k8s.io API version and is registered via
// RegisterProvider (typically from the versioned subpackage's init function).
type Provider interface {
	// GroupVersion is the scheduling.k8s.io API version this provider targets.
	GroupVersion() schema.GroupVersion

	// Available returns nil when this provider's API version is served by the
	// cluster reachable through config, and an error otherwise.
	Available(config *rest.Config) error

	// AddToScheme registers this provider's API types with the given scheme.
	AddToScheme(scheme *runtime.Scheme)

	// ConfigureReconciler adds watches for this provider's owned resource types.
	ConfigureReconciler(b *builder.Builder) *builder.Builder

	// NewScheduler builds the batch scheduler backed by this provider.
	NewScheduler(cli client.Client) schedulerinterface.BatchScheduler
}

var registeredProviders []Provider

// RegisterProvider adds a versioned WAS provider to the selection registry. It is
// intended to be called only from a provider subpackage's init function; the
// registry is not safe for concurrent mutation.
func RegisterProvider(p Provider) {
	registeredProviders = append(registeredProviders, p)
}

// SchedulerFactory selects a registered Provider based on the served API version
// and builds the batch scheduler for it. New must be called before AddToScheme or
// ConfigureReconciler, which delegate to the provider chosen by New.
type SchedulerFactory struct {
	// TargetVersion optionally pins the scheduling.k8s.io API version to use
	// (e.g. "v1beta1"); empty selects the most mature served version.
	TargetVersion string

	provider Provider
}

func (f *SchedulerFactory) New(_ context.Context, config *rest.Config, cli client.Client) (schedulerinterface.BatchScheduler, error) {
	provider, err := selectProvider(config, f.TargetVersion)
	if err != nil {
		return nil, err
	}
	f.provider = provider
	return provider.NewScheduler(cli), nil
}

func (f *SchedulerFactory) AddToScheme(scheme *runtime.Scheme) {
	if f.provider != nil {
		f.provider.AddToScheme(scheme)
	}
}

func (f *SchedulerFactory) ConfigureReconciler(b *builder.Builder) *builder.Builder {
	if f.provider != nil {
		return f.provider.ConfigureReconciler(b)
	}
	return b
}

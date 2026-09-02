package v1alpha1

import (
	"fmt"
	"slices"

	"github.com/go-logr/logr"

	kaischeduler "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/kai-scheduler"
	schedulerplugins "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/scheduler-plugins"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/volcano"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/yunikorn"
	"github.com/ray-project/kuberay/ray-operator/pkg/features"
)

// supportedKubernetesWASTargetVersions is the set of scheduling.k8s.io API versions the WAS
// scheduler may be pinned to via --kubernetes-was-target-version. It mirrors the Helm chart's
// validateKubernetesWAS guard. A static list (not the provider registry) avoids coupling config
// validation to provider registration order.
var supportedKubernetesWASTargetVersions = []string{"v1alpha2", "v1beta1", "v1alpha3"}

func ValidateBatchSchedulerConfig(logger logr.Logger, config Configuration) error {
	// The KubernetesWAS feature gate selects the Kubernetes WAS scheduler and is mutually
	// exclusive with --batch-scheduler and --enable-batch-scheduler.
	if features.Enabled(features.KubernetesWAS) {
		if config.EnableBatchScheduler || len(config.BatchScheduler) > 0 {
			return fmt.Errorf("the KubernetesWAS feature gate cannot be combined with --batch-scheduler or --enable-batch-scheduler")
		}
		if v := config.KubernetesWASTargetVersion; len(v) > 0 && !slices.Contains(supportedKubernetesWASTargetVersions, v) {
			return fmt.Errorf("kubernetes-was-target-version %q is not supported; must be one of %v", v, supportedKubernetesWASTargetVersions)
		}
		return nil
	}

	// KubernetesWASTargetVersion only applies when the WAS scheduler (KubernetesWAS gate) is selected.
	if len(config.KubernetesWASTargetVersion) > 0 {
		return fmt.Errorf("kubernetes-was-target-version is only valid with the KubernetesWAS feature gate")
	}

	if config.EnableBatchScheduler && len(config.BatchScheduler) > 0 {
		return fmt.Errorf("both feature flags enable-batch-scheduler (deprecated) and batch-scheduler are set. Please use batch-scheduler only")
	}

	if config.EnableBatchScheduler {
		logger.Info("Feature flag enable-batch-scheduler is deprecated and will not be supported soon. " +
			"Use batch-scheduler instead. ")
		return nil
	}

	if len(config.BatchScheduler) > 0 {
		// if a customized scheduler is configured, check it is supported
		if config.BatchScheduler == volcano.GetPluginName() || config.BatchScheduler == yunikorn.GetPluginName() || config.BatchScheduler == schedulerplugins.GetPluginName() || config.BatchScheduler == kaischeduler.GetPluginName() {
			logger.Info("Feature flag batch-scheduler is enabled",
				"scheduler name", config.BatchScheduler)
		} else {
			return fmt.Errorf("scheduler is not supported, name=%s", config.BatchScheduler)
		}
	}

	return nil
}

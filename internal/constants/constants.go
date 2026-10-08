package constants

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// Global backoff configurations
var (
	// Standard backoff for most operations
	StandardBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
	}

	// Slow backoff for operations that need more time
	ReconcileBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
	}

	// Lightweight backoff for individual Prometheus queries (collector, etc.)
	PrometheusQueryBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5, // 500ms, 1s, 2s, 4s = ~7.5s total
	}

	// Prometheus validation backoff with longer intervals
	// TODO: investigate why Prometheus needs longer backoff durations
	PrometheusValidationBackoff = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    6, // 5s, 10s, 20s, 40s, 80s, 160s = ~5 minutes total
	}
)

type GpuInfo struct {
	Vendor              string
	ResourceName        string
	ProductLabel        string
	ProductLabelAliases []string
	MemoryLabel         string
}

var (
	// vendorResources lists each supported GPU resource and its discovery labels.
	VendorResources = []GpuInfo{
		{
			Vendor:       "NVIDIA",
			ResourceName: "nvidia.com/gpu",
			ProductLabel: "nvidia.com/gpu.product",
			// Aliases are provider label keys carrying the same fact. The VALUE
			// shape differs between them and that is fine: the accelerator name is
			// whatever the node label says, and an operator keys quotas by the same
			// string. GKE says "nvidia-tesla-v100"; CoreWeave says "H200" where
			// NVIDIA GPU Feature Discovery would say "NVIDIA-H200".
			//
			// Without the CoreWeave key a fleet pinned with gpu.nvidia.com/model
			// resolves to "unknown": the controller reports its GPUs unattributed
			// and emits AcceleratorNotResolved while every pod is demonstrably on
			// an H200. Replica scaling still works, but accelerator-keyed metrics
			// are wrong and enabling the gpu-inventory limiter would block scale-up
			// on capacity it cannot see.
			// Only keys a provider DOCUMENTS as naming the GPU model. Several
			// plausible ones are deliberately absent -- see the note below.
			ProductLabelAliases: []string{
				"cloud.google.com/gke-accelerator",    // GKE: nvidia-h100-80gb
				"eks.amazonaws.com/instance-gpu-name", // EKS Auto Mode: h100
				"karpenter.k8s.aws/instance-gpu-name", // Karpenter on AWS: h100
				"karpenter.azure.com/sku-gpu-name",    // AKS node auto-provisioning: A100
				"gpu.nvidia.com/model",                // CoreWeave CKS
				"gpu.nvidia.com/class",                // CoreWeave CKS: A100_NVLINK_80GB
			},
			MemoryLabel: "nvidia.com/gpu.memory",
		},
		{
			Vendor:       "AMD",
			ResourceName: "amd.com/gpu",
			ProductLabel: "amd.com/gpu.product-name",
			// The standalone ROCm device-plugin labeller writes the beta. prefix;
			// the AMD GPU Operator writes the unprefixed one.
			ProductLabelAliases: []string{"beta.amd.com/gpu.product-name"},
			MemoryLabel:         "amd.com/gpu.memory",
		},
		// habana.ai/product.name is UNVERIFIED. Intel documents a Gaudi Feature
		// Discovery component and requires habana.ai in NFD's extraLabelNs, but no
		// reachable page enumerates the label keys it produces. Kept because it
		// ships and removing it could break a working deployment; treat a Gaudi
		// fleet resolving to "unknown" as this line being wrong, not the cluster.
		//
		// NOT aliases, deliberately, and each for its own reason:
		//
		//   accelerator=nvidia (AKS)          names the VENDOR. It would resolve
		//                                     every NVIDIA GPU to one accelerator
		//                                     and merge an A100 fleet with an H100
		//                                     one -- worse than "unknown", which at
		//                                     least reads as "any GPU can serve this".
		//   k8s.amazonaws.com/accelerator     a convention from eksctl and blog
		//                                     posts, not a label AWS applies.
		//   kubernetes.azure.com/accelerator  appears only in the cluster-autoscaler
		//                                     Azure provider README, not in AKS docs.
		//   feature.node.kubernetes.io/pci-*  NFD stops at vendor and class; it has
		//                                     no product label at all, which is why
		//                                     GPU Feature Discovery exists.
		//
		// Two hazards in the VALUES, which this package deliberately does not
		// normalise. Sharing modes mutate them: time-slicing appends -SHARED
		// (Tesla-T4-SHARED) and MIG appends the profile
		// (A100-SXM4-40GB-MIG-1g.5gb). Keeping those distinct is correct -- a MIG
		// slice is not a whole A100 and must not share its quota. The driver also
		// varies the prefix (Tesla-T4 vs NVIDIA-Tesla-T4), so an operator's quota
		// keys must match whatever their own nodes report.
		//
		// Precedence when a pod pins two of these keys is alphabetical, which is an
		// accident of GetProductKeys sorting for determinism rather than a decision.
		// Left alone here on purpose: changing it would silently move existing
		// deployments from one accelerator name to another, and the names are what
		// quotas key on.
		//
		// NOTE: Node labeling rules installed for Node Feature Discovery (NFD) by Intel GPU operator,
		// provide product labels only for Data Center products. Current Intel Gaudi / GPU operators
		// do not label nodes with device memory information, that info needs to be labeled separately.
		{
			Vendor:       "Intel",
			ResourceName: "habana.ai/gaudi",
			ProductLabel: "habana.ai/product.name",
			MemoryLabel:  "habana.ai/device.memory",
		},
		{
			Vendor:       "Intel",
			ResourceName: "gpu.intel.com/i915",
			ProductLabel: "gpu.intel.com/product",
			MemoryLabel:  "gpu.intel.com/memory",
		},
		{
			Vendor:       "Intel",
			ResourceName: "gpu.intel.com/xe",
			ProductLabel: "gpu.intel.com/product",
			MemoryLabel:  "gpu.intel.com/memory",
		},
	}

	SpecReplicasFallback int32 = 1 // in case Spec.Replicas is nil
)

// Kubernetes resource kinds and API versions for supported scale targets.
const (
	DeploymentKind            = "Deployment"
	DeploymentAPIVersion      = "apps/v1"
	StatefulSetKind           = "StatefulSet"
	PodKind                   = "Pod"
	ReplicaSetKind            = "ReplicaSet"
	PodAPIVersion             = "v1"
	LeaderWorkerSetKind       = "LeaderWorkerSet"
	LeaderWorkerSetAPIVersion = "leaderworkerset.x-k8s.io/v1"

	// KEDA ScaledObject identity, used to recognize the HPA KEDA generates per
	// ScaledObject (the child HPA inherits the ScaledObject's annotations).
	ScaledObjectKind     = "ScaledObject"
	ScaledObjectAPIGroup = "keda.sh"

	// K8s Events
	K8SEventScaledUp                          = "ScaledUp"
	K8SEventScaledDown                        = "ScaledDown"
	K8SEventResourceConstrained               = "ResourceConstrained"
	K8SEventMetricsUnavailable                = "MetricsUnavailable"
	K8SEventScaledToZero                      = "ScaledToZero"
	K8SEventOptimizationFailed                = "OptimizationFailed"
	K8SEventUnattributedReadyPods             = "UnattributedReadyPods"
	K8SEventThroughputAnalyzerRestartRequired = "ThroughputAnalyzerRestartRequired"
	// Utilization-share transfer Events, on the donor's and the receiver's
	// scale targets.
	K8SEventUtilizationShareGiving            = "UtilizationShareGiving"
	K8SEventUtilizationShareReceiving         = "UtilizationShareReceiving"
	K8SEventUtilizationShareReceived          = "UtilizationShareReceived"
	K8SEventUtilizationShareReleaseAborted    = "UtilizationShareReleaseAborted"
	K8SEventUtilizationShareWrongPod          = "UtilizationShareWrongPod"
	K8SEventUtilizationShareFillTimedOut      = "UtilizationShareFillTimedOut"
	K8SEventUtilizationShareCancelled         = "UtilizationShareCancelled"
	K8SEventUtilizationShareRedirected        = "UtilizationShareRedirected"
	K8SEventUtilizationShareDonorNotSteerable = "UtilizationShareDonorNotSteerable"

	// Enforcer policy types.
	EnforcerPolicyTypeScaleToZero     = "scale_to_zero"
	EnforcerPolicyTypeMinimumReplicas = "minimum_replicas"

	// DefaultAcceleratorName is used internally by the GPU limiter when the
	// accelerator type cannot be resolved from the scale target or VA label.
	// In homogeneous clusters (single GPU type), the limiter resolves this to
	// the real type before it reaches status or metrics. This value must never
	// be persisted to VA status or used as a Prometheus label.
	DefaultAcceleratorName = "unknown"

	// UnresolvedAcceleratorType is the bounded accelerator_type label value used
	// on the replica scaling gauges (wva_current_replicas / wva_desired_replicas /
	// wva_desired_ratio) when the real accelerator type is not yet known. Unlike
	// the internal DefaultAcceleratorName sentinel, this IS a valid label value:
	// it lets the scaling signal flow to HPA/KEDA without leaking "unknown" and
	// without withholding scaling. It is never persisted to VA status.
	UnresolvedAcceleratorType = "unresolved"
)

// Component names identify WVA components for observability (metrics, logging, tracing).
const (
	ComponentCollector  = "collector"
	ComponentAnalyzer   = "analyzer"
	ComponentOptimizer  = "optimizer"
	ComponentLimiter    = "limiter"
	ComponentEnforcer   = "enforcer"
	ComponentController = "controller"
)

// IsAcceleratorResolved returns true if the accelerator name is a real GPU type
// (not empty, not the "unknown" internal sentinel, and not the "unresolved"
// label placeholder). UnresolvedAcceleratorType is a label-only output value;
// treating it as unresolved here means that if it ever flows back in as an
// accelerator name it is re-mapped rather than mistaken for a real type, and it
// is never persisted to VA status.
func IsAcceleratorResolved(name string) bool {
	return name != "" && name != DefaultAcceleratorName && name != UnresolvedAcceleratorType
}

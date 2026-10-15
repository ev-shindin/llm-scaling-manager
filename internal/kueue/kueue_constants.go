package kueue

// Identifiers copied from Kueue. The project takes no Kueue module dependency
// (reader.go), so every API group, kind, label, gate, condition type, reason
// and field name it matches against is written here, once, with the Kueue
// source it was copied from (paths relative to sigs.k8s.io/kueue). Checked
// against Kueue v0.19.x and v0.20.x (TestedKueueVersions). A Kueue controller
// outside that range withholds borrowing (ReasonKueueVersionUntested) until
// these are checked again and the range is widened.
//
// The nominal-quota read does not depend on the version check: its fields
// (nominalQuota, namespaceSelector, LocalQueue spec.clusterQueue) have not
// changed since v1beta1.

// TestedKueueVersions are the Kueue minor releases these identifiers were
// checked against, as "v<major>.<minor>". Kept in the "Supported Kueue
// versions" table of docs/well-lit-paths/kueue-bounded-quotas/README.md.
var TestedKueueVersions = []string{"v0.19", "v0.20"}

// Group is the Kueue API group. The version is resolved from the cluster's
// RESTMapper at read time, never hardcoded (apis/kueue/v1beta1/groupversion_info.go).
const Group = "kueue.x-k8s.io"

// Kinds read (apis/kueue/v1beta1/*_types.go, v1beta2 likewise).
const (
	KindClusterQueue   = "ClusterQueue"
	KindLocalQueue     = "LocalQueue"
	KindResourceFlavor = "ResourceFlavor"
	KindCohort         = "Cohort"
	// KindWorkload is read by the managed check, the preemption check and the
	// lender-waiting signal.
	KindWorkload = "Workload"
)

// Labels and scheduling gates Kueue puts on the pods it manages. A tenant can
// write any of them into its own pod template, so none is proof that Kueue
// charges a pod; the managed check (Eligibility) asks for the Workload.
const (
	// ManagedLabel marks a pod Kueue manages through the pod integration
	// (pkg/controller/constants/constants.go, ManagedByKueueLabelKey).
	ManagedLabel = "kueue.x-k8s.io/managed"
	// QueueNameLabel names the LocalQueue a workload asks for. It is a request,
	// not proof that Kueue manages the object
	// (pkg/controller/constants/constants.go, QueueLabel).
	QueueNameLabel = "kueue.x-k8s.io/queue-name"
	// PodGroupNameLabel names the pod group a pod belongs to: Kueue admits a
	// pod group -- a LeaderWorkerSet group included -- as one Workload
	// (pkg/controller/jobs/pod/constants/constants.go, GroupNameLabel).
	PodGroupNameLabel = "kueue.x-k8s.io/pod-group-name"
	// AdmissionGate holds a managed pod until Kueue admits its workload
	// (pkg/controller/jobs/pod/constants/constants.go, SchedulingGateName).
	AdmissionGate = "kueue.x-k8s.io/admission"
	// TopologyGate holds a managed pod until Topology-Aware Scheduling assigns
	// it (apis/kueue/v1beta1/topology_types.go, TopologySchedulingGate).
	TopologyGate = "kueue.x-k8s.io/topology"
)

// Reclaim policies of a ClusterQueue's spec.preemption.reclaimWithinCohort.
// Absent means ReclaimNever (apis/kueue/v1beta1/clusterqueue_types.go,
// PreemptionPolicy).
const (
	ReclaimNever         = "Never"
	ReclaimLowerPriority = "LowerPriority"
	ReclaimAny           = "Any"
)

// Workload condition types and the eviction reason of a preemption
// (apis/kueue/v1beta1/workload_types.go).
const (
	WorkloadQuotaReserved = "QuotaReserved"
	WorkloadAdmitted      = "Admitted"
	WorkloadEvicted       = "Evicted"
	WorkloadFinished      = "Finished"
	// WorkloadEvictedByPreemption is the reason of an Evicted condition Kueue
	// sets when it preempts the Workload (WorkloadEvictedByPreemption).
	WorkloadEvictedByPreemption = "Preempted"
)

// Pod condition Kueue sets on a pod it is about to delete, and its reasons
// (pkg/controller/jobs/pod/pod_controller.go: ConditionTypeTerminationTarget,
// and "WorkloadEvictedDueTo" + the Workload's eviction reason).
const (
	// TerminationTargetCondition is the pod condition type.
	TerminationTargetCondition = "TerminationTarget"
	// PodReasonEvictedByPreemption is its reason for a preempted Workload.
	PodReasonEvictedByPreemption = "WorkloadEvictedDueToPreempted"
	// PodReasonEvicted is the generic reason an older Kueue wrote for every
	// eviction, naming the cause only in the message.
	PodReasonEvicted = "WorkloadEvicted"
)

// API fields read from ClusterQueues and ResourceFlavors that only the
// borrowing conditions use.
const (
	// CQFieldAdmissionChecks is ClusterQueueSpec.AdmissionChecks
	// (apis/kueue/v1beta1/clusterqueue_types.go, v1beta2 likewise).
	CQFieldAdmissionChecks = "admissionChecks"
	// CQFieldAdmissionChecksStrategy is ClusterQueueSpec.AdmissionChecksStrategy,
	// whose admissionChecks list is how MultiKueue is configured
	// (apis/kueue/v1beta1/clusterqueue_types.go).
	CQFieldAdmissionChecksStrategy = "admissionChecksStrategy"
	// RFFieldTopologyName is ResourceFlavorSpec.TopologyName: set, the flavor
	// uses Topology-Aware Scheduling (apis/kueue/v1beta1/resourceflavor_types.go).
	RFFieldTopologyName = "topologyName"
)

// The Kueue controller, whose image tag names the running version
// (config/components/manager/manager.yaml; the release manifests install it in
// kueue-system).
const (
	ControllerDeployment       = "kueue-controller-manager"
	DefaultControllerNamespace = "kueue-system"
)

// integratedOwners are the owner kinds Kueue's integrations set on the
// Workloads they create, by API group: the pod integration (a Deployment's
// pods too) owns it by the Pod, batch/v1 and JobSet by the Job or JobSet, and
// the LeaderWorkerSet and StatefulSet integrations by the set itself
// (pkg/controller/jobs/*, checked against Kueue v0.20.0's reconcilers).
var integratedOwners = map[string]string{
	"Pod":             "",
	"Job":             "batch",
	"JobSet":          "jobset.x-k8s.io",
	"LeaderWorkerSet": "leaderworkerset.x-k8s.io",
	"StatefulSet":     "apps",
}

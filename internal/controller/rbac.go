package controller

// This file centralizes the +kubebuilder:rbac markers for cluster resources the
// WVA controller and optimization engine read or scale. They previously lived on
// the VariantAutoscaling reconciler, which has been removed; the permissions are
// still required by the collector and the saturation / scale-from-zero engines.
// `make manifests` aggregates these into config/base/rbac/manager-clusterrole.yaml.

// Scale targets and their pods.
//
// READ ONLY, and that is the whole architecture rather than an oversight: KEDA
// owns the HPA and performs every write. WVA computes a replica count and hands
// it over gRPC (internal/actuator publishes to an in-process decision.Set); it
// never calls Update, Patch or the scale subresource on anything.
//
// The update/patch verbs on deployments, deployments/scale and the LeaderWorkerSet
// equivalents were granted and never exercised — a cluster-wide licence to resize
// any workload, which is the single permission a cluster admin is most right to
// refuse. Do not re-add them without a caller: if one ever appears, the design
// question ("why is WVA writing when KEDA actuates?") comes first.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/scale,verbs=get

// Node inventory for accelerator/capacity discovery.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;watch

// Core resources read during optimization and metrics collection.
//
// Pods are READ cluster-wide and never written through this ClusterRole. The
// controller does patch pods in two opt-in places -- the warm pool (an
// InferencePool's selector labels and a deletion cost on a lent pool Pod) and
// the utilization-share optimizer (a deletion cost and its transfer mark) --
// and `patch` for those is granted per namespace by role/wva-pod-writer
// (deploy/lib/pod_writer.sh), which deploy/warmpool.sh and
// deploy/utilization-share.sh create where they are used. A cluster-wide
// `patch` on pods reached every Pod's image, kube-system included: a stolen
// controller token could run code as any ServiceAccount. The warm pool checks
// the grant itself (internal/warmpool/preflight.go) and disables a pool it
// cannot lend from. The namespace-scoped tenant Role keeps `patch`: it is
// namespaced already.
//
// `pods/status` is deliberately NOT taken: readiness is reported by the pool
// Pod's own probe, so nothing here writes Pod status.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Note: Namespace watch permission is required for label-based namespace opt-in for namespace-local ConfigMaps.

// ScaledObjects, read one at a time to resolve a registered workload's scale
// target and min/max envelope (registry.Enricher, and the external scaler's own
// lookups).
//
// get and patch only. The markers that used to grant this rule belonged to
// the ScaledObject and HPA reconcilers and went with them, leaving the checked-in
// ClusterRole correct only because nothing had regenerated it — the next
// `make manifests` silently drops the permission, and enrichment then fails for
// every workload, which is the whole of discovery.
//
// list and watch are NOT granted and must not be: a cached/listing read is served
// by a cluster-wide informer, the very LIST+WATCH this design exists to remove.
// Every read goes through mgr.GetAPIReader().
//
// patch is for one write, registry.Rewirer: when KEDA left a ScaledObject's
// HPA without the scaler's External metric (its one metric-spec call failed,
// and KEDA does not retry), the controller changes the trigger metadata key
// wvaRewire so KEDA asks again. It is a JSON patch guarded by a test of the
// trigger's scalerAddress, on ScaledObjects that call this scaler. Without
// patch the controller logs it once and make scaledobjects-rewire does it by
// hand; --scaledobject-rewire=false turns the write off.
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;patch

// HPAs, read one at a time by registry.Rewirer to see whether KEDA wired the
// scaler's External metric onto a ScaledObject's HPA. GET ONLY, uncached, for
// the same reason as ScaledObjects. KEDA owns these objects; nothing writes
// them.
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get

// Kueue quotas, read by the quota limiter when a quota entry sets
// `kueue.enabled` (internal/kueue.Reader): ClusterQueues carry the nominal
// quotas, ResourceFlavors name the accelerator behind each flavor, LocalQueues
// tie a namespace to its ClusterQueues.
//
// get and list only. The reader lists on demand, unstructured, through the
// direct client path — no informer, so no watch — and never writes: Kueue owns
// these objects. Kueue itself is optional; a rule naming an API group the
// cluster does not serve is inert, and the reader reports "not served" rather
// than an empty quota when it is missing. It also gets Namespaces, to evaluate a
// ClusterQueue namespaceSelector; the namespaces rule above already covers that.
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues;localqueues;resourceflavors,verbs=get;list

// Kueue cohort borrowing (the Kueue borrowing proposal, PR #161,
// B2). Cohorts are listed to tell a flat cohort from a nested one; a Forbidden
// list reads as "hierarchy unknown", never as flat. Workloads, the namespace's
// own ResourceQuotas and the owners of GPU pods (Jobs; the apps kinds and
// LeaderWorkerSets are granted above) back the checks that decide whether
// borrowing may be counted. Workloads are read through an informer of the
// manager's cache (kueue.InformerWorkloads), started on first use, so they
// need watch; everything else is get and list only. Nothing is written.
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=cohorts,verbs=get;list
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get
//
// Whether a Kueue leader runs is read from its leader-election Lease
// (kueue.ControllerLease) in the Kueue namespace: get only. A namespace-scoped
// install gets it, with the kueue-controller-manager Deployment, from a Role in
// the Kueue namespace the prereqs phase creates; without either, borrowing
// freezes (kueue-controller-down). Limited by name to that Lease
// (kueue.ControllerLease), as the namespace-scoped Role is: the cluster-scoped
// install reads no other Lease through this rule.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,resourceNames=c1f6bfd2.kueue.x-k8s.io,verbs=get
//
// The placement timeout of a borrowed replica waits while the cluster
// autoscaler reports TriggeredScaleUp on its pod (B5): Events are read, get and
// list only, never watched.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list

// InferencePool discovery and controller-owned ServiceMonitor observation.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.networking.x-k8s.io;inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch

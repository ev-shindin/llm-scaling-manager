package kueue

// Identifiers copied from Kueue. The project takes no Kueue module dependency
// (reader.go), so every label, gate, condition, reason, annotation and field
// name it matches against is written here, once, with the Kueue source it was
// copied from. Checked against Kueue v0.19.x and v0.20.x.

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

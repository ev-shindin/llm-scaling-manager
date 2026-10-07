package constants

// Utilization-share optimizer metrics (docs/proposals/utilization-share-optimizer.md,
// section 9). Role-keyed series carry namespace, model_name and role; group-keyed
// series carry accelerator_type and scope (a namespace for a namespace-quota
// group, "cluster" for the cluster group). Every series is replaced each cycle,
// so a model or group that leaves takes its series with it.
const (
	// WVAUtilizationShareHeadroom is a gauge: x_r, the traffic spike a role
	// absorbs before it must scale, as a fraction of its need (negative when the
	// role is short). Absent for a role with no demand.
	WVAUtilizationShareHeadroom = "wva_utilization_share_headroom"

	// WVAUtilizationShareTargetGPUs is a gauge: Ĝ_r, the continuous target the
	// tolerance band is judged against, in GPUs.
	WVAUtilizationShareTargetGPUs = "wva_utilization_share_target_gpus"

	// WVAUtilizationShareActionable is a gauge: 1 when a role is out of band and
	// off its integer target -- a move could fix it -- else 0.
	WVAUtilizationShareActionable = "wva_utilization_share_actionable"

	// WVAUtilizationShareSpareGPUs is a gauge: S, the group's budget minus every
	// role's claim. Negative when the quota is short.
	WVAUtilizationShareSpareGPUs = "wva_utilization_share_spare_gpus"

	// WVAUtilizationShareReplicasToMove is a gauge: the replicas the integer
	// target would move. In shadow mode this is what would be planned.
	WVAUtilizationShareReplicasToMove = "wva_utilization_share_replicas_to_move"

	// LabelScope is the budget scope of a utilization-share group.
	LabelScope = "scope"

	// UtilizationShareClusterScope is the scope label of the cluster group.
	UtilizationShareClusterScope = "cluster"
)

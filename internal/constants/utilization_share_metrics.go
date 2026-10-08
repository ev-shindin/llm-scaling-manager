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

	// WVAUtilizationShareTransfersTotal is a counter: transfers that left the
	// ledger, by outcome (done, fill-timeout, cancelled, aborted, redirected,
	// wrong-pod).
	WVAUtilizationShareTransfersTotal = "wva_utilization_share_transfers_total"

	// WVAUtilizationSharePromisedGPUs is a gauge: P, GPUs released for a
	// receiver and not yet held by it.
	WVAUtilizationSharePromisedGPUs = "wva_utilization_share_promised_gpus"

	// WVAUtilizationShareEffectiveSeconds is a gauge: a derived timing in force,
	// by param and the source of its inputs (section 8.4).
	WVAUtilizationShareEffectiveSeconds = "wva_utilization_share_effective_seconds"

	// WVAUtilizationShareSwinging is a gauge: 1 while a role is planned on its
	// mean need (section 6.7 rule 5).
	WVAUtilizationShareSwinging = "wva_utilization_share_swinging"

	// WVAUtilizationShareReleaseSeconds is a histogram: transfer start to the
	// donor's GPUs released.
	WVAUtilizationShareReleaseSeconds = "wva_utilization_share_release_seconds"

	// WVAUtilizationShareReserveDebtGPUs is a gauge: reserve GPUs spent and
	// not yet refilled (section 6.2).
	WVAUtilizationShareReserveDebtGPUs = "wva_utilization_share_reserve_debt_gpus"

	// WVAUtilizationShareWithheldTotal is a counter: transfers not planned, by
	// reason (reversal-hold, not-actionable).
	WVAUtilizationShareWithheldTotal = "wva_utilization_share_withheld_total"

	// WVAUtilizationShareActual is a gauge: u_r, a role's utilization at the
	// GPUs it holds, on the same scale as its scale-up threshold.
	WVAUtilizationShareActual = "wva_utilization_share_actual"

	// WVAUtilizationShareFloorExcessGPUs is a gauge: the GPUs a role's floor
	// holds above its need (section 5.5).
	WVAUtilizationShareFloorExcessGPUs = "wva_utilization_share_floor_excess_gpus"

	// WVAUtilizationShareClaimsTotal is a counter: wake claims on releasing
	// transfers, by outcome (redirected, refused-score, refused-fit,
	// refused-held, none-releasing).
	WVAUtilizationShareClaimsTotal = "wva_utilization_share_claims_total"

	// WVAUtilizationShareDonorsPerTransfer is a histogram: the donor replicas
	// that fund one receiver replica (section 6.5).
	WVAUtilizationShareDonorsPerTransfer = "wva_utilization_share_donors_per_transfer"

	// LabelParam names a derived timing; LabelSource is where its inputs came
	// from.
	LabelParam  = "param"
	LabelSource = "source"

	// LabelUrgent is "true" on a transfer whose receiver was below its need.
	LabelUrgent = "urgent"

	// LabelScope is the budget scope of a utilization-share group.
	LabelScope = "scope"

	// UtilizationShareClusterScope is the scope label of the cluster group.
	UtilizationShareClusterScope = "cluster"
)

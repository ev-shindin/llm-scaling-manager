package scaletarget

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestRollingOut(t *testing.T) {
	dep := func(replicas, updated int32) ScaleTargetAccessor {
		return NewDeploymentAccessor(&appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: replicas, UpdatedReplicas: updated}})
	}
	unobserved := NewDeploymentAccessor(&appsv1.Deployment{ObjectMeta: v1.ObjectMeta{Generation: 3},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3}})
	lws := func(status v1.ConditionStatus) ScaleTargetAccessor {
		return NewLWSAccessor(&lwsv1.LeaderWorkerSet{Status: lwsv1.LeaderWorkerSetStatus{Conditions: []v1.Condition{
			{Type: string(lwsv1.LeaderWorkerSetUpdateInProgress), Status: status}}}})
	}
	for _, c := range []struct {
		name string
		acc  ScaleTargetAccessor
		want bool
	}{
		{"deployment, every pod updated", dep(3, 3), false},
		{"deployment, old pods left", dep(3, 1), true},
		{"deployment, new ReplicaSet with no pods yet", dep(3, 0), true},
		{"deployment, a spec its controller has not observed", unobserved, true},
		{"lws updating", lws(v1.ConditionTrue), true},
		{"lws not updating", lws(v1.ConditionFalse), false},
	} {
		if got := RollingOut(c.acc); got != c.want {
			t.Errorf("%s: RollingOut = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLWSPartition(t *testing.T) {
	p := int32(3)
	held := NewLWSAccessor(&lwsv1.LeaderWorkerSet{Spec: lwsv1.LeaderWorkerSetSpec{RolloutStrategy: lwsv1.RolloutStrategy{
		RollingUpdateConfiguration: &lwsv1.RollingUpdateConfiguration{Partition: &p}}}})
	if got := LWSPartition(held); got != 3 {
		t.Errorf("partition %d, want 3", got)
	}
	if got := LWSPartition(NewLWSAccessor(&lwsv1.LeaderWorkerSet{})); got != 0 {
		t.Errorf("unset partition %d, want 0", got)
	}
	if got := LWSPartition(NewDeploymentAccessor(&appsv1.Deployment{})); got != 0 {
		t.Errorf("a Deployment's partition %d, want 0", got)
	}
}

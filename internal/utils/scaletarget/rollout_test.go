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
		{"lws updating", lws(v1.ConditionTrue), true},
		{"lws not updating", lws(v1.ConditionFalse), false},
	} {
		if got := RollingOut(c.acc); got != c.want {
			t.Errorf("%s: RollingOut = %v, want %v", c.name, got, c.want)
		}
	}
}

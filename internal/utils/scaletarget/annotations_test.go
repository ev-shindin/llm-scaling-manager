package scaletarget

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// TestAnnotations reads the scale target's OWN annotations for both kinds --
// not the pod template's, which would roll the workload to set.
func TestAnnotations(t *testing.T) {
	want := map[string]string{"llm-d.ai/forget-learned-state": "t1"}
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Annotations: want}}
	deploy.Spec.Template.Annotations = map[string]string{"llm-d.ai/forget-learned-state": "template"}
	if got := Annotations(NewDeploymentAccessor(deploy))["llm-d.ai/forget-learned-state"]; got != "t1" {
		t.Errorf("Deployment: got %q, want the object's own annotation", got)
	}
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Annotations: want}}
	if got := Annotations(NewLWSAccessor(lws))["llm-d.ai/forget-learned-state"]; got != "t1" {
		t.Errorf("LeaderWorkerSet: got %q", got)
	}
	if Annotations(nil) != nil {
		t.Error("a nil accessor has no annotations")
	}
}

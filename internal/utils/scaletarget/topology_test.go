package scaletarget

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// A LeaderWorkerSet's exclusive-topology annotation names the node label its
// groups are confined to; a Deployment, or an LWS without the annotation,
// confines nothing.
func TestExclusiveTopology(t *testing.T) {
	lws := func(ann map[string]string) ScaleTargetAccessor {
		return NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Annotations: ann}})
	}
	assert.Equal(t, "rack", ExclusiveTopology(lws(map[string]string{lwsv1.ExclusiveKeyAnnotationKey: "rack"})))
	assert.Empty(t, ExclusiveTopology(lws(nil)))
	assert.Empty(t, ExclusiveTopology(NewDeploymentAccessor(&appsv1.Deployment{})))
}

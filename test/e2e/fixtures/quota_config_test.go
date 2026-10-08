package fixtures

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

// ClearLeftoverQuota removes only what SetNamespaceQuota writes: the suite's
// limiter and the optimizer block written with it. An operator's limiters stay.
func TestClearLeftoverQuota(t *testing.T) {
	const ns, name = "wva", "policy"
	for _, tc := range []struct {
		name         string
		entry        string
		wantCleared  bool
		wantLimiters []string
		wantOptim    bool
	}{
		{"suite limiter alone", "limiters:\n- name: e2e-warm-pool-quota\n  type: quota\noptimizer:\n  type: utilizationShare\n",
			true, nil, false},
		{"beside an operator's limiter",
			"limiters:\n- name: ops\n  type: inventory\n- name: e2e-warm-pool-quota\n  type: quota\noptimizer:\n  type: utilizationShare\n",
			true, []string{"ops"}, false},
		{"operator's limiter only", "limiters:\n- name: ops\n  type: quota\noptimizer:\n  type: utilizationShare\n",
			false, []string{"ops"}, true},
		{"no limiters", "kvCacheThreshold: 0.8\n", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientset(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
				Data:       map[string]string{defaultEntryKey: tc.entry},
			})
			cleared, err := ClearLeftoverQuota(context.Background(), c, ns, name)
			if err != nil {
				t.Fatal(err)
			}
			if cleared != tc.wantCleared {
				t.Fatalf("cleared = %v, want %v", cleared, tc.wantCleared)
			}
			cm, err := c.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Limiters  []struct{ Name string } `json:"limiters"`
				Optimizer map[string]any          `json:"optimizer"`
			}
			if err := yaml.Unmarshal([]byte(cm.Data[defaultEntryKey]), &doc); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(doc.Limiters))
			for _, l := range doc.Limiters {
				got = append(got, l.Name)
			}
			if len(got) != len(tc.wantLimiters) || (len(got) > 0 && got[0] != tc.wantLimiters[0]) {
				t.Fatalf("limiters after = %v, want %v", got, tc.wantLimiters)
			}
			if (doc.Optimizer != nil) != tc.wantOptim {
				t.Fatalf("optimizer kept = %v, want %v", doc.Optimizer != nil, tc.wantOptim)
			}
		})
	}

	t.Run("no ConfigMap", func(t *testing.T) {
		cleared, err := ClearLeftoverQuota(context.Background(), fake.NewClientset(), ns, name)
		if err != nil || cleared {
			t.Fatalf("got %v %v, want nothing to clear", cleared, err)
		}
	})
}

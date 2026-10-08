package config

import "testing"

// clusterNamespaces limits the cluster-scope groups to the namespaces it names;
// empty plans every namespace, and an empty entry is an error.
func TestUtilizationShareClusterNamespaces(t *testing.T) {
	all, err := ResolveUtilizationShare(&UtilizationShareConfig{})
	if err != nil || !all.InClusterGroup("anything") {
		t.Fatalf("no list: %v %v", all.InClusterGroup("anything"), err)
	}
	some, err := ResolveUtilizationShare(&UtilizationShareConfig{ClusterNamespaces: []string{"team-a"}})
	if err != nil || !some.InClusterGroup("team-a") || some.InClusterGroup("team-b") {
		t.Fatalf("list [team-a]: a=%v b=%v %v", some.InClusterGroup("team-a"), some.InClusterGroup("team-b"), err)
	}
	if _, err := ResolveUtilizationShare(&UtilizationShareConfig{ClusterNamespaces: []string{""}}); err == nil {
		t.Fatal("an empty entry was accepted")
	}
}

// Package recorderpatch holds the learner recorder's Deployment patches to
// what docs/developer-guide/testing.md says they do, against a real API
// server (envtest): the patch an operator applies with kubectl patch, the
// patch that takes it out, and what a re-run of the installer does to it.
package recorderpatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	namespace = "wva-system"
	// installedName is the Deployment's name as installed: every overlay
	// prefixes the base's controller-manager with wva-.
	installedName = "wva-controller-manager"
	lastApplied   = "kubectl.kubernetes.io/last-applied-configuration"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test")
		dir = parent
	}
}

// startAPIServer starts envtest. Its binaries come from KUBEBUILDER_ASSETS
// (make test sets it) or bin/k8s; without them the test fails rather than
// skips, as the other envtest suites do.
func startAPIServer(t *testing.T, root string) client.Client {
	t.Helper()
	env := &envtest.Environment{}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		entries, _ := os.ReadDir(filepath.Join(root, "bin", "k8s"))
		for _, e := range entries {
			if e.IsDir() {
				env.BinaryAssetsDirectory = filepath.Join(root, "bin", "k8s", e.Name())
				break
			}
		}
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs its binaries: run make setup-envtest, or make test")
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	require.NoError(t, err)
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	return c
}

// baseDeployment is config/base/manager/deployment.yaml as the installer
// applies it: named with the overlays' prefix, in the namespace, with the
// last-applied annotation a client-side kubectl apply leaves.
func baseDeployment(t *testing.T, root string) (*appsv1.Deployment, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "config", "base", "manager", "deployment.yaml"))
	require.NoError(t, err)
	var d appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(b, &d))
	d.Name, d.Namespace = installedName, namespace
	applied, err := json.Marshal(&d)
	require.NoError(t, err)
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[lastApplied] = string(applied)
	return &d, applied
}

// patchFile is one of the patch files as kubectl patch --patch-file sends
// it: YAML turned to JSON, a strategic-merge patch.
func patchFile(t *testing.T, root string, rel ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	require.NoError(t, err)
	j, err := yaml.YAMLToJSON(b)
	require.NoError(t, err)
	return j
}

func get(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: installedName}, &d))
	return &d
}

func container(spec corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

// The documented commands, against an API server: the on patch turns the
// recorder on in the installed Deployment, the off patch gives the pod
// spec back exactly as it was, and a re-run of the installer's client-side
// apply keeps the recorder on without a new pod.
func TestRecorderPatchesAgainstAnAPIServer(t *testing.T) {
	root := repoRoot(t)
	c := startAPIServer(t, root)
	ctx := context.Background()
	base, applied := baseDeployment(t, root)
	require.NoError(t, c.Create(ctx, base))
	before := get(t, c)

	on := patchFile(t, root, "config", "components", "learner-recorder", "learner-recorder-deployment.yaml")
	require.NoError(t, c.Patch(ctx, before.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, on)),
		"kubectl patch deployment %s --type strategic --patch-file learner-recorder-deployment.yaml", installedName)
	patched := get(t, c)
	spec := patched.Spec.Template.Spec
	manager, sidecar := container(spec, "manager"), container(spec, "recordings")
	require.NotNil(t, manager)
	require.NotNil(t, sidecar, "the recordings sidecar")
	assert.Contains(t, manager.Env, corev1.EnvVar{Name: "LEARNER_RECORDING_DIR", Value: "/var/lib/wva/learner-recordings"})
	assert.Contains(t, manager.VolumeMounts, corev1.VolumeMount{Name: "learner-recordings", MountPath: "/var/lib/wva/learner-recordings"})
	assert.Equal(t, before.Spec.Template.Spec.Containers[0].Image, manager.Image, "the patch changes no image")
	assert.Greater(t, patched.Generation, before.Generation, "turning it on rolls a new pod")

	// What a re-run of the installer does: kubectl apply (client-side)
	// sends the three-way patch from its last-applied configuration, which
	// never named the recorder.
	live := patched.DeepCopy()
	live.APIVersion, live.Kind = "apps/v1", "Deployment"
	current, err := json.Marshal(live)
	require.NoError(t, err)
	// The manifest as the installer renders it again: not the object Create
	// filled in with the server's fields.
	fresh, _ := baseDeployment(t, root)
	reapplied, err := json.Marshal(fresh)
	require.NoError(t, err)
	threeWay, err := strategicpatch.CreateThreeWayMergePatch(applied, reapplied, current,
		mustLookup(t), true)
	require.NoError(t, err)
	require.NoError(t, c.Patch(ctx, patched.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, threeWay)))
	again := get(t, c)
	assert.NotNil(t, container(again.Spec.Template.Spec, "recordings"), "a re-run of the installer turned the recorder off")
	assert.Equal(t, patched.Generation, again.Generation, "a re-run of the installer that changes nothing else rolls no new pod")

	off := patchFile(t, root, "config", "components", "learner-recorder", "learner-recorder-off-deployment.yaml")
	require.NoError(t, c.Patch(ctx, again.DeepCopy(), client.RawPatch(types.StrategicMergePatchType, off)))
	after := get(t, c)
	if !reflect.DeepEqual(before.Spec.Template.Spec, after.Spec.Template.Spec) {
		bj, _ := json.MarshalIndent(before.Spec.Template.Spec, "", " ")
		aj, _ := json.MarshalIndent(after.Spec.Template.Spec, "", " ")
		t.Fatalf("the off patch did not give the pod spec back:\nbefore %s\nafter  %s", bj, aj)
	}
	assert.Greater(t, after.Generation, again.Generation, "turning it off rolls a new pod")
}

func mustLookup(t *testing.T) strategicpatch.LookupPatchMeta {
	t.Helper()
	m, err := strategicpatch.NewPatchMetaFromStruct(&appsv1.Deployment{})
	require.NoError(t, err)
	return m
}

// The OpenShift patch is the same as the Kubernetes one, less the sidecar's
// fixed runAsUser, which the restricted-v2 SCC refuses.
func TestOpenShiftPatchDiffersOnlyByRunAsUser(t *testing.T) {
	root := repoRoot(t)
	read := func(rel ...string) *appsv1.Deployment {
		var d appsv1.Deployment
		require.NoError(t, json.Unmarshal(patchFile(t, root, rel...), &d))
		return &d
	}
	k8s := read("config", "components", "learner-recorder", "learner-recorder-deployment.yaml")
	ocp := read("config", "components", "learner-recorder-openshift", "learner-recorder-openshift-deployment.yaml")
	side := container(k8s.Spec.Template.Spec, "recordings")
	require.NotNil(t, side)
	require.NotNil(t, side.SecurityContext.RunAsUser, "precondition: the Kubernetes patch fixes the user")
	assert.Nil(t, container(ocp.Spec.Template.Spec, "recordings").SecurityContext.RunAsUser)
	side.SecurityContext.RunAsUser = nil
	assert.Equal(t, k8s, ocp)
}

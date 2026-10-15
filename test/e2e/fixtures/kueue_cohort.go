package fixtures

import (
	"context"
	stderrors "errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KueueE2ENamespaceLabel marks a namespace whose pods and workloads go through
// Kueue's job webhooks. deploy/ci-pr-checks/install-kueue.sh narrows those
// webhooks to namespaces carrying it (KUEUE_WEBHOOK_NAMESPACE_LABEL in the
// Makefile), so a Kueue manager that is restarting cannot fail pod creation in
// the rest of the suite. On a cluster whose Kueue was installed some other way
// the label is inert.
const KueueE2ENamespaceLabel = "llm-d.ai/kueue-e2e"

// Kueue pod-side markers the live-Kueue specs assert on. They mirror the
// constants in internal/kueue, restated so the fixtures package does not depend
// on the controller's internal packages.
const (
	// KueueManagedLabel marks a pod Kueue manages.
	KueueManagedLabel = "kueue.x-k8s.io/managed"
	// KueueQueueNameLabel names the LocalQueue a workload asks for.
	KueueQueueNameLabel = "kueue.x-k8s.io/queue-name"
	// KueueAdmissionGate holds a managed pod until Kueue admits it.
	KueueAdmissionGate = "kueue.x-k8s.io/admission"
)

// Kueue reclaimWithinCohort policies.
const (
	KueueReclaimNever = "Never"
	KueueReclaimAny   = "Any"
)

// KueueCohortQueue is one ClusterQueue of a KueueCohort and the LocalQueue that
// grants it to one namespace. The LocalQueue takes the ClusterQueue's name.
type KueueCohortQueue struct {
	Name      string
	Namespace string
	// GPUs is the nominalQuota of the cohort's GPU resource on its flavor.
	GPUs int
	// Reclaim is spec.preemption.reclaimWithinCohort.
	Reclaim string
}

// KueueCohort is a flat Kueue cohort: one ResourceFlavor, and ClusterQueues
// that name the cohort and declare quota on that flavor. Every queue covers
// cpu and memory too -- Kueue refuses a pod that requests a resource its queue
// does not cover -- with quota generous enough that only GPUs bind.
type KueueCohort struct {
	// Cohort is spec.cohortName (spec.cohort under v1beta1).
	Cohort string
	// Flavor names the ResourceFlavor.
	Flavor string
	// ProductLabel and Product are the flavor's node label; the reader turns
	// the value into the accelerator type.
	ProductLabel, Product string
	// Resource is the GPU extended resource, e.g. nvidia.com/gpu.
	Resource string
	Queues   []KueueCohortQueue
}

// KueueQueueStatus is the part of a ClusterQueue's status the borrowing figures
// are computed from, for the cohort's GPU resource on its flavor.
type KueueQueueStatus struct {
	// HasStatus is false until Kueue has written flavorsReservation for the
	// GPU resource -- the tell of CRDs without a running controller.
	HasStatus bool
	// Reserved is flavorsReservation total.
	Reserved int
	// Borrowed is flavorsReservation borrowed.
	Borrowed int
	// Pending is status.pendingWorkloads.
	Pending int
}

// KueueControllerRunning reports whether a Kueue controller Deployment, as the
// release manifest labels it, has an available replica in any namespace. The
// string says what was found, for a Skip or Fail message.
func KueueControllerRunning(ctx context.Context, k8s kubernetes.Interface) (bool, string, error) {
	deps, err := k8s.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=kueue,control-plane=controller-manager",
	})
	if err != nil {
		return false, "", fmt.Errorf("list Kueue controller Deployments: %w", err)
	}
	if len(deps.Items) == 0 {
		return false, "no Deployment labelled app.kubernetes.io/name=kueue,control-plane=controller-manager", nil
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Status.AvailableReplicas > 0 {
			return true, fmt.Sprintf("%s/%s available", d.Namespace, d.Name), nil
		}
	}
	d := &deps.Items[0]
	return false, fmt.Sprintf("%s/%s has no available replica", d.Namespace, d.Name), nil
}

// CreateKueueCohort creates the flavor, then each ClusterQueue and its
// LocalQueue. The namespaces must exist. Leftovers of an earlier run are
// replaced, as CreateKueueQuota does.
func CreateKueueCohort(ctx context.Context, c client.Client, kc KueueCohort) error {
	objs, err := kueueCohortObjects(c, kc)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := createOrReplaceKueueObject(ctx, c, obj); err != nil {
			return err
		}
	}
	return nil
}

// DeleteKueueCohort deletes the LocalQueues, ClusterQueues and the flavor, in
// that order, and waits for each to be gone: Kueue holds a ClusterQueue with a
// finalizer while workloads still reserve in it, and a flavor while a queue
// names it. Delete the workloads first. Missing objects are not an error.
func DeleteKueueCohort(ctx context.Context, c client.Client, kc KueueCohort) error {
	objs, err := kueueCohortObjects(c, kc)
	if err != nil {
		return err
	}
	// Every object is deleted, whatever happens to the one before it: a
	// cleanup that stopped at the first held ClusterQueue leaked the rest into
	// the next run.
	var errs []error
	for i := len(objs) - 1; i >= 0; i-- {
		obj := objs[i]
		if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s %s: %w", obj.GetKind(), obj.GetName(), err))
		}
	}
	for i := len(objs) - 1; i >= 0; i-- {
		obj := objs[i]
		if err := waitGone(ctx, c, obj); err == nil {
			continue
		}
		// Still held (Kueue's in-use finalizers, typically a workload that
		// outlived its pods): the final, forced delete. The error stays, so the
		// run reports that cleanup needed it.
		errs = append(errs, forceRemoveFinalizers(ctx, c, obj))
		if err := waitGone(ctx, c, obj); err != nil {
			errs = append(errs, err)
		}
	}
	return stderrors.Join(errs...)
}

// forceRemoveFinalizers clears obj's finalizers so a delete already issued
// completes. It reports that it had to, as an error, even when it succeeded.
func forceRemoveFinalizers(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	held := obj.DeepCopy()
	err := c.Patch(ctx, held, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("%s %s held after deletion; removing its finalizers failed: %w", obj.GetKind(), obj.GetName(), err)
	}
	return fmt.Errorf("%s %s held after deletion; its finalizers were removed", obj.GetKind(), obj.GetName())
}

// SetKueueReclaim sets a ClusterQueue's spec.preemption.reclaimWithinCohort.
func SetKueueReclaim(ctx context.Context, c client.Client, clusterQueue, policy string) error {
	gvk, err := kueueVersion(c, "ClusterQueue")
	if err != nil {
		return err
	}
	cq := &unstructured.Unstructured{}
	cq.SetGroupVersionKind(gvk)
	cq.SetName(clusterQueue)
	patch := fmt.Sprintf(`{"spec":{"preemption":{"reclaimWithinCohort":%q}}}`, policy)
	if err := c.Patch(ctx, cq, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return fmt.Errorf("set reclaimWithinCohort=%s on ClusterQueue %s: %w", policy, clusterQueue, err)
	}
	return nil
}

// ReadKueueQueueStatus reads a ClusterQueue's reservation of resource on flavor
// and its pending workload count.
func ReadKueueQueueStatus(ctx context.Context, c client.Client, clusterQueue, flavor, resourceName string) (KueueQueueStatus, error) {
	gvk, err := kueueVersion(c, "ClusterQueue")
	if err != nil {
		return KueueQueueStatus{}, err
	}
	cq := &unstructured.Unstructured{}
	cq.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, client.ObjectKey{Name: clusterQueue}, cq); err != nil {
		return KueueQueueStatus{}, fmt.Errorf("get ClusterQueue %s: %w", clusterQueue, err)
	}
	var st KueueQueueStatus
	pending, _, err := unstructured.NestedInt64(cq.Object, "status", "pendingWorkloads")
	if err != nil {
		return st, fmt.Errorf("ClusterQueue %s status.pendingWorkloads: %w", clusterQueue, err)
	}
	st.Pending = int(pending)
	flavors, _, _ := unstructured.NestedSlice(cq.Object, "status", "flavorsReservation")
	for _, f := range flavors {
		fm, _ := f.(map[string]any)
		if fm["name"] != flavor {
			continue
		}
		res, _ := fm["resources"].([]any)
		for _, r := range res {
			rm, _ := r.(map[string]any)
			if rm["name"] != resourceName {
				continue
			}
			if st.Reserved, err = quantityInt(rm["total"]); err != nil {
				return st, fmt.Errorf("ClusterQueue %s reservation total: %w", clusterQueue, err)
			}
			if st.Borrowed, err = quantityInt(rm["borrowed"]); err != nil {
				return st, fmt.Errorf("ClusterQueue %s reservation borrowed: %w", clusterQueue, err)
			}
			st.HasStatus = true
		}
	}
	return st, nil
}

// CreateKueueGPUDeployment creates a Deployment of pause pods, each requesting
// gpusPerPod of resourceName, labelled for LocalQueue queue so Kueue admits
// each pod as its own workload. An empty queue leaves the label off: the pods
// are then GPU pods Kueue does not manage. Requests equal limits, as an extended resource
// requires; the small cpu and memory requests are covered by the queue.
func CreateKueueGPUDeployment(
	ctx context.Context, k8s kubernetes.Interface,
	namespace, name, queue, resourceName string, replicas, gpusPerPod int,
) error {
	gpus := *resource.NewQuantity(int64(gpusPerPod), resource.DecimalSI)
	mem := resource.MustParse("16Mi")
	labels := map[string]string{}
	if queue != "" {
		labels[KueueQueueNameLabel] = queue
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(replicas)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(1)),
					Containers: []corev1.Container{{
						Name:  "pause",
						Image: fillerImage,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:                resource.MustParse("10m"),
								corev1.ResourceMemory:             mem,
								corev1.ResourceName(resourceName): gpus,
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory:             mem,
								corev1.ResourceName(resourceName): gpus,
							},
						},
					}},
				},
			},
		},
	}
	if _, err := k8s.AppsV1().Deployments(namespace).Create(ctx, d, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create Deployment %s/%s: %w", namespace, name, err)
	}
	return nil
}

// quantityInt reads a status quantity ("4", "12") as a whole number.
func quantityInt(v any) (int, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("quantity %v is not a string", v)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	return int(q.Value()), nil
}

// kueueCohortObjects builds the flavor, ClusterQueues and LocalQueues in
// creation order, at the API version the cluster serves for each kind.
func kueueCohortObjects(c client.Client, kc KueueCohort) ([]*unstructured.Unstructured, error) {
	versions := map[string]string{}
	for _, kind := range []string{"ResourceFlavor", "ClusterQueue", "LocalQueue"} {
		gvk, err := kueueVersion(c, kind)
		if err != nil {
			return nil, err
		}
		versions[kind] = gvk.Version
	}
	withKind := func(obj *unstructured.Unstructured, kind string) *unstructured.Unstructured {
		obj.SetAPIVersion(kueueGroup + "/" + versions[kind])
		obj.SetKind(kind)
		return obj
	}

	objs := []*unstructured.Unstructured{withKind(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": kc.Flavor},
		"spec": map[string]any{
			"nodeLabels": map[string]any{kc.ProductLabel: kc.Product},
		},
	}}, "ResourceFlavor")}

	// v1beta2 renamed spec.cohort to spec.cohortName.
	cohortField := "cohortName"
	if versions["ClusterQueue"] == "v1beta1" {
		cohortField = "cohort"
	}
	for _, q := range kc.Queues {
		objs = append(objs, withKind(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": q.Name},
			"spec": map[string]any{
				cohortField:         kc.Cohort,
				"namespaceSelector": map[string]any{},
				"preemption": map[string]any{
					"reclaimWithinCohort": q.Reclaim,
					"withinClusterQueue":  "Never",
				},
				"resourceGroups": []any{map[string]any{
					"coveredResources": []any{"cpu", "memory", kc.Resource},
					"flavors": []any{map[string]any{
						"name": kc.Flavor,
						"resources": []any{
							map[string]any{"name": "cpu", "nominalQuota": "100"},
							map[string]any{"name": "memory", "nominalQuota": "200Gi"},
							map[string]any{"name": kc.Resource, "nominalQuota": strconv.Itoa(q.GPUs)},
						},
					}},
				}},
			},
		}}, "ClusterQueue"))
	}
	for _, q := range kc.Queues {
		objs = append(objs, withKind(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": q.Name, "namespace": q.Namespace},
			"spec":     map[string]any{"clusterQueue": q.Name},
		}}, "LocalQueue"))
	}
	return objs, nil
}

// createOrReplaceKueueObject creates obj, replacing a leftover of the same
// name. The leftover's finalizers are waited out before the create.
func createOrReplaceKueueObject(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	err := c.Create(ctx, obj.DeepCopy())
	if err == nil {
		return nil
	}
	if !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("replace %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	if err := waitGone(ctx, c, obj); err != nil {
		return err
	}
	if err := c.Create(ctx, obj.DeepCopy()); err != nil {
		return fmt.Errorf("recreate %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// WaitDeploymentGone deletes a Deployment, if present, and waits until neither
// it nor any pod matching podSelector is left in the namespace, for up to
// timeout. Kueue releases a workload's reservation only when its pod is gone.
func WaitDeploymentGone(ctx context.Context, k8s kubernetes.Interface, namespace, name, podSelector string, timeout time.Duration) error {
	err := k8s.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete Deployment %s/%s: %w", namespace, name, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		_, getErr := k8s.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		pods, listErr := k8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: podSelector})
		if errors.IsNotFound(getErr) && listErr == nil && len(pods.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			n := -1
			if listErr == nil {
				n = len(pods.Items)
			}
			return fmt.Errorf("deployment %s/%s or its pods still present after %s (pods: %d, get: %v)",
				namespace, name, timeout, n, getErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

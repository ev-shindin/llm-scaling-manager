package utils

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// DumpControllerLogs fetches and prints the controller manager logs for debugging.
// Call this in AfterEach or DeferCleanup to capture logs on test failure.
func DumpControllerLogs(ctx context.Context, k8sClient *kubernetes.Clientset, controllerNamespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Controller Manager Logs ===\n")

	pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list controller pods: %v\n", err)
		return
	}

	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "No controller pods found in namespace %s\n", controllerNamespace)
		return
	}

	for _, pod := range pods.Items {
		_, _ = fmt.Fprintf(w, "\n--- Logs from pod %s ---\n", pod.Name)
		logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: ptr.To(int64(200)),
		}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Failed to get logs: %v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", string(logs))
	}
}

// DumpKEDAOperatorLogs prints the KEDA operator's own log, filtered to the
// lines that say why it could not build a scaler.
//
// It exists because the one line that explains a whole class of failure lives
// only here. An HPA found carrying the Kubernetes CPU default instead of KEDA's
// external metric is a dead end from WVA's side -- WVA publishes the right
// number throughout, the ScaledObject reports Ready=True, and the HPA reports a
// CPU metric it cannot read -- while the KEDA operator says plainly:
//
//	scale_handler  error getting metric spec for the scaler  "got empty metric spec"
//	external_scaler  error  ... transport: Error while dialing ... i/o timeout
//
// Two CI failures were investigated end to end without that line, because
// nothing collected it; it had to be reproduced on a local cluster to be seen
// at all. Grepped rather than dumped whole: the operator is chatty and the
// interesting lines are the errors.
func DumpKEDAOperatorLogs(ctx context.Context, k8sClient *kubernetes.Clientset, kedaNamespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== KEDA operator log (errors) in %s ===\n", kedaNamespace)
	if kedaNamespace == "" {
		_, _ = fmt.Fprintf(w, "(KEDA namespace not configured; set KEDA_NAMESPACE)\n")
		return
	}
	pods, err := k8sClient.CoreV1().Pods(kedaNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=keda-operator",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list KEDA operator pods: %v\n", err)
		return
	}
	if len(pods.Items) == 0 {
		// Not a silent skip: "no operator pod" is itself a finding, and a
		// different one from "the operator logged nothing".
		_, _ = fmt.Fprintf(w, "No pod matching app=keda-operator in %s -- if KEDA is installed "+
			"under different labels here, this dump is blind and the label needs updating\n", kedaNamespace)
		return
	}
	for _, pod := range pods.Items {
		_, _ = fmt.Fprintf(w, "\n--- %s ---\n", pod.Name)
		logs, err := k8sClient.CoreV1().Pods(kedaNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: ptr.To(int64(500)),
		}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Failed to get logs: %v\n", err)
			continue
		}
		var kept int
		for _, line := range strings.Split(string(logs), "\n") {
			if strings.Contains(line, "ERROR") ||
				strings.Contains(line, "empty metric spec") ||
				strings.Contains(line, "external_scaler") {
				_, _ = fmt.Fprintf(w, "%s\n", line)
				kept++
			}
		}
		if kept == 0 {
			_, _ = fmt.Fprintf(w, "(no error lines in the last 500 -- the operator is healthy, "+
				"so a missing external metric was not refused here)\n")
		}
	}
}

// DumpManagedScalers fetches and prints the HorizontalPodAutoscalers in every
// namespace for debugging. WVA discovers variants from annotated HPAs (and KEDA
// ScaledObjects, which KEDA in turn manages via HPAs), so the HPA list plus its
// currentMetrics is the observable annotation-discovery surface.
func DumpManagedScalers(ctx context.Context, k8sClient *kubernetes.Clientset,
	dynClient dynamic.Interface, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Managed HorizontalPodAutoscalers ===\n")

	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list HPAs: %v\n", err)
		return
	}

	// Said out loud, because an empty section under a heading reads as "the dump
	// did not look" when what it means is "KEDA is managing nothing at all" --
	// which is a finding, and the one that explains a ScaledObject whose metric
	// WVA republishes every cycle while the Deployment never moves.
	if len(hpaList.Items) == 0 {
		_, _ = fmt.Fprintf(w, "(none in any namespace -- nothing is actuating; "+
			"see the ScaledObject conditions below)\n")
	}

	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		_, _ = fmt.Fprintf(w, "\nHPA: %s/%s\n", hpa.Namespace, hpa.Name)
		_, _ = fmt.Fprintf(w, "  ScaleTargetRef: %s/%s\n", hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.ScaleTargetRef.Name)
		_, _ = fmt.Fprintf(w, "  Annotations: %v\n", hpa.Annotations)
		_, _ = fmt.Fprintf(w, "  DesiredReplicas: %d\n", hpa.Status.DesiredReplicas)
		_, _ = fmt.Fprintf(w, "  CurrentMetrics: %v\n", hpa.Status.CurrentMetrics)

		// The CONDITIONS are where a stalled HPA says WHY, and without them the two
		// fields above are unreadable: DesiredReplicas 0 with a nil CurrentMetrics
		// entry looks identical whether the metrics API is unavailable, the
		// external scaler returned an error, or the metric name does not resolve.
		// ScalingActive=False carries the reason -- FailedGetExternalMetric -- and
		// the message underneath it, which is the only place that distinction
		// appears. A smoke failure was diagnosed as "KEDA infrastructure" for
		// hours on the strength of those two fields alone; the cause was an error
		// returned by our own external scaler.
		if len(hpa.Status.Conditions) == 0 {
			_, _ = fmt.Fprintf(w, "  Conditions: (none -- the HPA controller has not evaluated this object yet)\n")
		}
		for _, c := range hpa.Status.Conditions {
			_, _ = fmt.Fprintf(w, "  Condition: %s=%s reason=%s: %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
		dumpHPAEvents(ctx, k8sClient, hpa.Namespace, hpa.Name, w)
	}
}

// dumpHPAEvents prints the events the HPA controller recorded against one HPA.
//
// Separate from the conditions above because the two carry different things: a
// condition holds the CURRENT state, while FailedGetExternalMetric arrives as a
// repeating event that keeps the verbatim error text from the metrics API --
// including which metric could not be served. When the condition has since
// settled back, the event is the only remaining trace.
func dumpHPAEvents(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string, w io.Writer) {
	events, err := k8sClient.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name + ",involvedObject.kind=HorizontalPodAutoscaler",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "  Events: failed to list: %v\n", err)
		return
	}
	if len(events.Items) == 0 {
		_, _ = fmt.Fprintf(w, "  Events: (none)\n")
		return
	}
	for i := range events.Items {
		e := &events.Items[i]
		_, _ = fmt.Fprintf(w, "  Event: %s %s x%d: %s\n", e.Type, e.Reason, e.Count, e.Message)
	}
}

// DumpScaledObjects prints every KEDA ScaledObject with the status conditions
// that decide whether it is actuating anything.
//
// The HPA dump above shows the DERIVED object and says nothing about why it is
// absent. KEDA maintains an HPA only for a ScaledObject it considers Ready, so
// when nothing actuates the reason is a condition here -- and without it a run
// reports the symptom (replicas never moved) with no trace of the cause.
func DumpScaledObjects(ctx context.Context, dynClient dynamic.Interface, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== KEDA ScaledObjects ===\n")
	if dynClient == nil {
		_, _ = fmt.Fprintf(w, "(no dynamic client)\n")
		return
	}

	gvr := schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	list, err := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list ScaledObjects: %v\n", err)
		return
	}
	if len(list.Items) == 0 {
		_, _ = fmt.Fprintf(w, "(none in any namespace)\n")
		return
	}

	for i := range list.Items {
		so := &list.Items[i]
		_, _ = fmt.Fprintf(w, "\nScaledObject: %s/%s\n", so.GetNamespace(), so.GetName())
		spec, _, _ := unstructured.NestedMap(so.Object, "spec")
		if spec != nil {
			_, _ = fmt.Fprintf(w, "  min/max: %v/%v\n", spec["minReplicaCount"], spec["maxReplicaCount"])
			if target, ok := spec["scaleTargetRef"].(map[string]interface{}); ok {
				_, _ = fmt.Fprintf(w, "  ScaleTargetRef: %v\n", target["name"])
			}
		}
		conds, _, _ := unstructured.NestedSlice(so.Object, "status", "conditions")
		if len(conds) == 0 {
			_, _ = fmt.Fprintf(w, "  Conditions: (none yet -- KEDA has not reconciled "+
				"this object, so no HPA exists for it)\n")
			continue
		}
		for _, c := range conds {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			_, _ = fmt.Fprintf(w, "  Condition %v=%v reason=%v message=%v\n",
				cm["type"], cm["status"], cm["reason"], cm["message"])
		}
	}
}

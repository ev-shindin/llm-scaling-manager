// Package constants provides centralized constant definitions for the autoscaler.
// This file contains Kubernetes label keys used for filtering and identification.
package constants

// Kubernetes Label Keys
// Label keys used on Kubernetes resources for filtering and identification.
const (
	// ControllerInstanceLabelKey is the label key used to associate VAs with specific controller instances.
	// Used for multi-controller isolation where each controller only manages VAs with matching labels.
	ControllerInstanceLabelKey = "wva.llmd.ai/controller-instance"

	// NamespaceConfigEnabledLabelKey is the label key used to opt-in namespaces for namespace-local ConfigMap overrides.
	// When a namespace has this label set to "true", the controller will watch for namespace-local ConfigMaps
	// even if no VariantAutoscaling resources exist in that namespace yet.
	// This enables creating namespace-local ConfigMaps before VAs are created, avoiding race conditions.
	NamespaceConfigEnabledLabelKey = "wva.llmd.ai/config-enabled"

	// PolicyNamespaceLabelKey, on a NAMESPACE object, names the namespace WVA reads
	// limiters and quotas from for workloads in that namespace.
	//
	// It lives on the Namespace because that object is cluster-scoped: a namespace
	// admin holds RBAC INSIDE their namespace and cannot edit it. So this is a
	// pointer the subject of the policy can read but not rewrite.
	//
	// A namespace name is a valid label value, and a label is the form that can be
	// selected on. `kubectl get ns -l wva.llmd.ai/policy-namespace=platform-policy`
	// answers "which namespaces does this policy govern?" — the question an admin
	// actually asks when auditing, and one an annotation cannot answer.
	//
	PolicyNamespaceLabelKey = "wva.llmd.ai/policy-namespace"

	// ModelLabelKey names the model a serving pod belongs to. llm-d puts it on
	// every model-server pod template, and FMA copies it onto a launcher pod at
	// bind time from the InferenceServerConfig's label map.
	//
	// Its value is a sanitized, DNS-safe form of the model ID, NOT the model ID:
	// `Qwen/Qwen3-0.6B` appears here as `qwen-qwe-694d2b87-en3-0-6b`. It is
	// therefore comparable with another pod's copy of the same label, and not
	// with the `model_name` label on a vLLM metric series.
	ModelLabelKey = "llm-d.ai/model"

	// DualPodsPairLabelKey names the other half of a Fast Model Actuation pair.
	//
	// FMA's dual-pods controller maintains it on BOTH the server-requesting pod
	// (the requester, which a scaler moves) and the server-providing pod (the
	// launcher, which holds the GPU and runs the engine), each naming the other,
	// and only while the two are bound. It is how a launcher — owned by a
	// LauncherConfig, and deliberately not adopted by any ReplicaSet — can still
	// be resolved to the workload that governs it.
	//
	// Note the value is a pod NAME carried in a label, so it is subject to the
	// 63-character limit on label values while pod names may be longer. A pair
	// whose requester name exceeds that cannot be expressed here.
	DualPodsPairLabelKey = "dual-pods.llm-d.ai/dual"

	// ComponentLabelKey is the standard Kubernetes component label. FMA sets it
	// on launcher pods, which is the only use this project reads it for.
	ComponentLabelKey = "app.kubernetes.io/component"

	// LauncherComponent is the ComponentLabelKey value FMA gives a
	// server-providing pod. Such a pod's ownerReferences lead to a
	// LauncherConfig, never to a scale target, so it is attributable only through
	// DualPodsPairLabelKey — which makes this label the way to tell a warm spare
	// apart from a pod that genuinely should have resolved.
	LauncherComponent = "launcher"
)

// Kubernetes Annotation Keys
// Annotation keys used on Kubernetes resources for metadata and exclusion.
const (
	// NamespaceExcludeAnnotationKey is the annotation key used to exclude namespaces from WVA management.
	// When a namespace has this annotation set to "true", the controller will not watch it
	// for namespace-local ConfigMaps or reconcile VariantAutoscaling resources in it,
	// even if the namespace has VAs or opt-in labels.
	// This provides explicit control to exclude namespaces from WVA management.
	NamespaceExcludeAnnotationKey = "wva.llmd.ai/exclude"

	// PodDeletionCostAnnotation is the pod annotation the ReplicaSet controller
	// reads when choosing a scale-down victim: it removes the cheapest first.
	// The warm pool and the utilization-share optimizer both steer with it.
	PodDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"
)

// AnnotationValueTrue is the canonical string value for boolean annotations and labels.
const AnnotationValueTrue = "true"

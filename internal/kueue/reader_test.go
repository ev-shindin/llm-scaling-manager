package kueue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

func TestKueue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Kueue Reader Suite")
}

// The served API version is whatever the cluster's RESTMapper says; the fake
// mapper serves v1beta2 here and v1beta1 in one spec, and the Reader must not care.
func kueueMapper(version string) meta.RESTMapper {
	gv := schema.GroupVersion{Group: Group, Version: version}
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	m.Add(gv.WithKind(KindClusterQueue), meta.RESTScopeRoot)
	m.Add(gv.WithKind(KindResourceFlavor), meta.RESTScopeRoot)
	m.Add(gv.WithKind(KindLocalQueue), meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return m
}

// namespace is a core Namespace with the given labels, for selector evaluation.
func namespace(name string, lbls map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Namespace")
	u.SetName(name)
	u.SetLabels(lbls)
	return u
}

// withSelector sets spec.namespaceSelector on a ClusterQueue; nil removes it.
func withSelector(cq *unstructured.Unstructured, sel map[string]interface{}) *unstructured.Unstructured {
	if sel == nil {
		unstructured.RemoveNestedField(cq.Object, "spec", "namespaceSelector")
		return cq
	}
	Expect(unstructured.SetNestedMap(cq.Object, sel, "spec", "namespaceSelector")).To(Succeed())
	return cq
}

// matchLabels is a namespaceSelector of one label.
func matchLabels(k, v string) map[string]interface{} {
	return map[string]interface{}{"matchLabels": map[string]interface{}{k: v}}
}

func obj(version, kind, ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(Group + "/" + version)
	u.SetKind(kind)
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	u.Object["spec"] = spec
	return u
}

func flavor(version, name string, nodeLabels map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if nodeLabels != nil {
		spec["nodeLabels"] = nodeLabels
	}
	return obj(version, KindResourceFlavor, "", name, spec)
}

// quota is one {name, nominalQuota} resource line; nominalQuota is a string, as
// the API server stores a Quantity.
func quota(resName string, nominal interface{}) map[string]interface{} {
	return map[string]interface{}{"name": resName, "nominalQuota": nominal}
}

func flavorQuotas(name string, resources ...map[string]interface{}) map[string]interface{} {
	rs := make([]interface{}, 0, len(resources))
	for _, r := range resources {
		rs = append(rs, r)
	}
	return map[string]interface{}{"name": name, "resources": rs}
}

// clusterQueue builds a ClusterQueue that admits every namespace (an empty
// namespaceSelector), as Kueue's own quickstart does; withSelector narrows it.
func clusterQueue(version, name string, flavors ...map[string]interface{}) *unstructured.Unstructured {
	fs := make([]interface{}, 0, len(flavors))
	covered := map[string]bool{}
	for _, f := range flavors {
		fs = append(fs, f)
		for _, r := range f["resources"].([]interface{}) {
			covered[r.(map[string]interface{})["name"].(string)] = true
		}
	}
	cr := make([]interface{}, 0, len(covered))
	for r := range covered {
		cr = append(cr, r)
	}
	return obj(version, KindClusterQueue, "", name, map[string]interface{}{
		"namespaceSelector": map[string]interface{}{},
		"resourceGroups": []interface{}{
			map[string]interface{}{"coveredResources": cr, "flavors": fs},
		},
	})
}

func localQueue(version, ns, name, cq string) *unstructured.Unstructured {
	return obj(version, KindLocalQueue, ns, name, map[string]interface{}{"clusterQueue": cq})
}

// forbidden is the API server's answer to a get on a Namespace the caller may
// not read.
func forbidden(name string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, name, errors.New("no RBAC"))
}

// typedCaps is a grant of typed caps only.
// denyNamespaceList forbids listing Namespaces, as the RBAC of a
// namespace-scoped install does, so the reader falls back to one get per
// namespace.
func denyNamespaceList(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
	if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "NamespaceList" {
		return forbidden("namespaces")
	}
	return cl.List(ctx, list, opts...)
}

func typedCaps(m map[string]int) config.ExternalCaps { return config.ExternalCaps{ByType: m} }

func newFakeClient(version string, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(kueueMapper(version)).
		WithObjects(objs...).
		Build()
}

var _ = Describe("Reader", func() {
	const v = "v1beta2"
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("derives per-namespace and cluster caps from flavors, ClusterQueues and LocalQueues", func() {
		c := newFakeClient(v,
			flavor(v, "h100-sxm", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}),
			flavor(v, "h100-pcie", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-PCIE-80GB"}),
			flavor(v, "gke-l4", map[string]interface{}{"cloud.google.com/gke-accelerator": "nvidia-l4"}),
			flavor(v, "cpu-only", map[string]interface{}{"node.kubernetes.io/instance-type": "m5.large"}),
			flavor(v, "mi300x", nil),
			clusterQueue(v, "team-a-cq",
				flavorQuotas("h100-sxm", quota("nvidia.com/gpu", "8"), quota("cpu", "64")),
				flavorQuotas("h100-pcie", quota("nvidia.com/gpu", "2")),
			),
			clusterQueue(v, "team-b-cq",
				flavorQuotas("gke-l4", quota("nvidia.com/gpu", "4")),
				flavorQuotas("cpu-only", quota("cpu", "128"), quota("memory", "512Gi")),
			),
			clusterQueue(v, "batch-cq", flavorQuotas("mi300x", quota("amd.com/gpu", "16"))),
			localQueue(v, "team-a", "lq", "team-a-cq"),
			localQueue(v, "team-a", "lq-2", "team-a-cq"), // same queue twice: counted once
			localQueue(v, "team-b", "lq", "team-b-cq"),
			localQueue(v, "team-b", "batch", "batch-cq"),
			localQueue(v, "team-c", "lq", "cpu-cq"),      // dangling queue: no GPU opinion
			localQueue(v, "team-d", "lq", "cpu-only-cq"), // CPU queue: no GPU opinion
			clusterQueue(v, "cpu-only-cq", flavorQuotas("cpu-only", quota("cpu", "8"))),
			clusterQueue(v, "zero-cq", flavorQuotas("h100-sxm", quota("nvidia.com/gpu", "0"))),
			localQueue(v, "team-e", "lq", "zero-cq"), // explicit 0: governed, granted nothing
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.ObservedAt).NotTo(BeZero())
		// Product labels are kept as written: the two H100 flavors stay two
		// grants, and the static side decides whether "H100" means both.
		Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{
			"team-a": typedCaps(map[string]int{"NVIDIA-H100-80GB-HBM3": 8, "NVIDIA-H100-PCIE-80GB": 2}),
			// mi300x has a ResourceFlavor with no product label: an untyped grant.
			"team-b": {ByType: map[string]int{"nvidia-l4": 4}, Untyped: 16, HasUntyped: true},
			"team-e": typedCaps(map[string]int{"NVIDIA-H100-80GB-HBM3": 0}),
		}))
		Expect(snap.Cluster).To(Equal(config.ExternalCaps{
			ByType:  map[string]int{"NVIDIA-H100-80GB-HBM3": 8, "NVIDIA-H100-PCIE-80GB": 2, "nvidia-l4": 4},
			Untyped: 16, HasUntyped: true,
		}))
	})

	It("reads a flavor without a product label as an untyped grant, never as a type", func() {
		// Kueue's own quickstart: `default-flavor`, no nodeLabels. Naming a type
		// after it would make it compete with every static key and zero them all.
		c := newFakeClient(v,
			flavor(v, "default-flavor", nil),
			clusterQueue(v, "cq", flavorQuotas("default-flavor", quota("nvidia.com/gpu", "6"))),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(config.ExternalCaps{Untyped: 6, HasUntyped: true}))
		Expect(snap.Namespace["team-a"].ByType).To(BeEmpty())
	})

	It("treats a queue Kueue will not admit from as governing, and granting nothing", func() {
		// Two ways a ClusterQueue stops admitting: a flavor it names does not
		// exist (FlavorNotFound), or stopPolicy holds it. Either way Kueue admits
		// nothing, so the namespace must read as governed with nothing granted
		// -- NOT as ungoverned, which would leave it to the static entry and,
		// with the recommended `default: {H100: -1}`, unlimited.
		stopped := clusterQueue(v, "stopped", flavorQuotas("h100", quota("nvidia.com/gpu", "8")))
		Expect(unstructured.SetNestedField(stopped.Object, "Hold", "spec", "stopPolicy")).To(Succeed())
		c := newFakeClient(v,
			flavor(v, "h100", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}),
			clusterQueue(v, "dangling", flavorQuotas("renamed-away", quota("nvidia.com/gpu", "8"))),
			// A valid flavor beside the missing one does not rescue the queue:
			// Kueue admits nothing from an inactive queue.
			clusterQueue(v, "half", flavorQuotas("h100", quota("nvidia.com/gpu", "4")), flavorQuotas("gone", quota("nvidia.com/gpu", "4"))),
			stopped,
			localQueue(v, "team-a", "lq", "dangling"),
			localQueue(v, "team-b", "lq", "half"),
			localQueue(v, "team-c", "lq", "stopped"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		nothing := config.ExternalCaps{HasUntyped: true}
		Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{
			"team-a": nothing, "team-b": nothing, "team-c": nothing,
		}))
		Expect(snap.Cluster).To(Equal(nothing))
	})

	It("takes the largest, not the sum, of untyped grants across GPU vendors in one queue", func() {
		c := newFakeClient(v,
			flavor(v, "nv", nil),
			flavor(v, "amd", nil),
			clusterQueue(v, "cq",
				flavorQuotas("nv", quota("nvidia.com/gpu", "8")),
				flavorQuotas("amd", quota("amd.com/gpu", "16")),
			),
			clusterQueue(v, "cq2", flavorQuotas("nv", quota("nvidia.com/gpu", "2"))),
			localQueue(v, "team-a", "lq", "cq"),
			localQueue(v, "team-a", "lq2", "cq2"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		// max(8, 16) within cq, plus cq2's 2: two queues of one namespace add.
		Expect(snap.Namespace["team-a"]).To(Equal(config.ExternalCaps{Untyped: 18, HasUntyped: true}))
	})

	It("keeps two products of one family apart, and sums two grants of one product", func() {
		// PCIe and SXM are what ResourceFlavors exist to tell apart; a static
		// "H100" is bounded by both together at bound time (config.BoundBy), a
		// static long name only by its own. Two queues granting the SAME product
		// to one namespace add up.
		c := newFakeClient(v,
			flavor(v, "sxm", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}),
			flavor(v, "pcie", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-PCIE-80GB"}),
			clusterQueue(v, "cq",
				flavorQuotas("sxm", quota("nvidia.com/gpu", "8")),
				flavorQuotas("pcie", quota("nvidia.com/gpu", "4")),
			),
			clusterQueue(v, "cq2", flavorQuotas("sxm", quota("nvidia.com/gpu", "2"))),
			localQueue(v, "team-a", "lq", "cq"),
			localQueue(v, "team-a", "lq2", "cq2"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"].ByType).To(Equal(map[string]int{
			"NVIDIA-H100-80GB-HBM3": 10, "NVIDIA-H100-PCIE-80GB": 4,
		}))
	})

	It("reads the API version the cluster serves", func() {
		const old = "v1beta1"
		c := newFakeClient(old,
			flavor(old, "a100", nil),
			clusterQueue(old, "cq", flavorQuotas("a100", quota("nvidia.com/gpu", "3"))),
			localQueue(old, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(config.ExternalCaps{Untyped: 3, HasUntyped: true}))
	})

	It("counts only the configured resource names", func() {
		c := newFakeClient(v,
			flavor(v, "h100", nil),
			flavor(v, "mi300x", nil),
			clusterQueue(v, "cq",
				flavorQuotas("h100", quota("nvidia.com/gpu", "6")),
				flavorQuotas("mi300x", quota("amd.com/gpu", "8")),
			),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{Resources: []string{"amd.com/gpu"}}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(config.ExternalCaps{Untyped: 8, HasUntyped: true}))
	})

	It("lists LocalQueues in one namespace only when told to", func() {
		c := newFakeClient(v,
			flavor(v, "h100", nil),
			clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
			localQueue(v, "team-a", "lq", "cq"),
			localQueue(v, "team-b", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{Namespace: "team-b"}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace).To(HaveLen(1))
		Expect(snap.Namespace).To(HaveKey("team-b"))
		// Cluster-scoped queues are still the whole cluster's figure.
		Expect(snap.Cluster).To(Equal(config.ExternalCaps{Untyped: 6, HasUntyped: true}))
	})

	It("clamps a huge nominalQuota and accepts numeric quantities", func() {
		c := newFakeClient(v,
			flavor(v, "h100", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}),
			flavor(v, "a100", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}),
			clusterQueue(v, "cq",
				flavorQuotas("h100", quota("nvidia.com/gpu", "1G")),
				flavorQuotas("a100", quota("nvidia.com/gpu", int64(2))),
			),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(typedCaps(map[string]int{
			"NVIDIA-H100-80GB-HBM3": config.MaxQuotaValue, "NVIDIA-A100-SXM4-80GB": 2,
		})))
	})

	It("does not record a caller's own context error as the source's", func() {
		// One reader serves both engines. A caller whose context expires
		// mid-list must not leave "deadline exceeded" cached for the other
		// caller's next, healthy call; the next call reads afresh.
		now := time.Unix(1000, 0)
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(kueueMapper(v)).
			WithObjects(
				flavor(v, "h100", nil),
				clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
				localQueue(v, "team-a", "lq", "cq"),
			).
			WithInterceptorFuncs(interceptor.Funcs{
				// The fake client does not look at ctx; a real API call would.
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		r := NewReader(c, Options{RefreshInterval: 30 * time.Second, now: func() time.Time { return now }})

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := r.Quotas(cancelled)
		Expect(err).To(HaveOccurred())

		snap, err := r.Quotas(ctx) // same instant, healthy context: a real read
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(config.ExternalCaps{Untyped: 6, HasUntyped: true}))
	})

	Describe("ClusterQueue namespaceSelector", func() {
		// A LocalQueue is the tenant's to create, pointing at any ClusterQueue.
		// Its namespace gets the queue's grant only where Kueue would admit
		// through it. Semantics are Kueue's (ClusterQueueSpec.NamespaceSelector):
		// nil admits no namespace, {} admits every namespace.
		h100 := func() *unstructured.Unstructured {
			return flavor(v, "h100", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"})
		}
		eight := func(name string) *unstructured.Unstructured {
			return clusterQueue(v, name, flavorQuotas("h100", quota("nvidia.com/gpu", "8")))
		}
		granted := typedCaps(map[string]int{"NVIDIA-H100-80GB-HBM3": 8})

		It("grants the namespaces the selector matches", func() {
			c := newFakeClient(v, h100(),
				withSelector(eight("team-a-cq"), matchLabels("team", "a")),
				namespace("team-a", map[string]string{"team": "a"}),
				localQueue(v, "team-a", "lq", "team-a-cq"),
			)
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{"team-a": granted}))
		})

		It("does not grant a namespace the selector does not match", func() {
			c := newFakeClient(v, h100(),
				withSelector(eight("team-a-cq"), matchLabels("team", "a")),
				namespace("team-a", map[string]string{"team": "a"}),
				namespace("intruder", map[string]string{"team": "b"}),
				localQueue(v, "team-a", "lq", "team-a-cq"),
				localQueue(v, "intruder", "lq", "team-a-cq"), // points at someone else's queue
			)
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).NotTo(HaveKey("intruder"),
				"a LocalQueue on a ClusterQueue whose selector excludes the namespace must not carry the grant")
			Expect(snap.Namespace).To(HaveKeyWithValue("team-a", granted))
			// The queue still counts once at cluster scope.
			Expect(snap.Cluster).To(Equal(granted))
		})

		It("grants no namespace through a nil selector", func() {
			c := newFakeClient(v, h100(),
				withSelector(eight("cq"), nil),
				namespace("team-a", map[string]string{"team": "a"}),
				localQueue(v, "team-a", "lq", "cq"),
			)
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(BeEmpty())
		})

		It("grants every namespace through an empty selector, without reading any Namespace", func() {
			gets := 0
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(), eight("cq"),
					localQueue(v, "team-a", "lq", "cq"),
					localQueue(v, "team-b", "lq", "cq")).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						gets++
						return cl.Get(ctx, key, o, opts...)
					},
				}).
				Build()
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{"team-a": granted, "team-b": granted}))
			Expect(gets).To(BeZero(), "an empty selector needs no namespace labels, and so no RBAC on namespaces")
		})

		It("withholds the grant, and bounds the namespace at nothing, when its labels cannot be read", func() {
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(),
					withSelector(eight("team-a-cq"), matchLabels("team", "a")),
					eight("open-cq"),
					namespace("team-a", map[string]string{"team": "a"}),
					namespace("team-b", map[string]string{"team": "a"}),
					localQueue(v, "team-a", "lq", "team-a-cq"),
					localQueue(v, "team-b", "lq", "team-a-cq"),
					localQueue(v, "team-b", "lq2", "open-cq"),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: denyNamespaceList,
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						return forbidden(key.Name)
					},
				}).
				Build()
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred(), "one unreadable namespace must not discard the whole snapshot")
			// Not granted, and not left to the static entry either: governed, nothing.
			Expect(snap.Namespace).To(HaveKeyWithValue("team-a", config.ExternalCaps{HasUntyped: true}))
			// A namespace with another, verifiable grant keeps only that one.
			Expect(snap.Namespace).To(HaveKeyWithValue("team-b", granted))
		})

		It("withholds the grant from a namespace that no longer exists", func() {
			// The LocalQueue was listed, then its Namespace was deleted before
			// the label read. No namespace object is created here at all.
			c := newFakeClient(v, h100(),
				withSelector(eight("team-a-cq"), matchLabels("team", "a")),
				localQueue(v, "gone", "lq", "team-a-cq"),
			)
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(HaveKeyWithValue("gone", config.ExternalCaps{HasUntyped: true}))
		})

		It("keeps the previous snapshot when a Namespace read fails transiently", func() {
			now := time.Unix(1000, 0)
			fail := false
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(),
					withSelector(eight("team-a-cq"), matchLabels("team", "a")),
					namespace("team-a", map[string]string{"team": "a"}),
					localQueue(v, "team-a", "lq", "team-a-cq"),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: denyNamespaceList,
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						if fail {
							return apierrors.NewServiceUnavailable("etcd leader change")
						}
						return cl.Get(ctx, key, o, opts...)
					},
				}).
				Build()
			r := NewReader(c, Options{RefreshInterval: time.Second, now: func() time.Time { return now }})
			first, err := r.Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Namespace).To(HaveKeyWithValue("team-a", granted))

			fail = true
			now = now.Add(time.Minute)
			snap, err := r.Quotas(ctx)
			Expect(err).To(HaveOccurred(), "a transient Namespace read must fail the read, not withhold the grant")
			Expect(snap).To(Equal(first), "the previous snapshot stays in force")
		})

		It("does not cache a snapshot from a read cancelled mid-way", func() {
			now := time.Unix(1000, 0)
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(),
					withSelector(eight("team-a-cq"), matchLabels("team", "a")),
					namespace("team-a", map[string]string{"team": "a"}),
					localQueue(v, "team-a", "lq", "team-a-cq"),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: denyNamespaceList,
					// The caller gives up while the label read is in flight; the
					// API answers with a status error rather than ctx.Err().
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						cancel()
						return forbidden(key.Name)
					},
				}).
				Build()
			r := NewReader(c, Options{RefreshInterval: time.Minute, now: func() time.Time { return now }})
			snap, err := r.Quotas(cctx)
			Expect(err).To(HaveOccurred())
			Expect(snap.ObservedAt).To(BeZero(), "nothing was cached")
			Expect(r.last.ObservedAt).To(BeZero())
			Expect(r.lastTry).To(BeZero(), "the next caller reads afresh")
		})

		It("reads an unreadable Namespace once per read, however many queues need it", func() {
			gets := 0
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(),
					withSelector(eight("cq-1"), matchLabels("team", "a")),
					withSelector(eight("cq-2"), matchLabels("tier", "gpu")),
					withSelector(eight("cq-3"), matchLabels("team", "a")),
					localQueue(v, "team-a", "lq-1", "cq-1"),
					localQueue(v, "team-a", "lq-2", "cq-2"),
					localQueue(v, "team-a", "lq-3", "cq-3"),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: denyNamespaceList,
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						gets++
						return forbidden(key.Name)
					},
				}).
				Build()
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(HaveKeyWithValue("team-a", config.ExternalCaps{HasUntyped: true}))
			Expect(gets).To(Equal(1))
		})

		It("evaluates a matchExpressions selector, granting only the queue that admits the namespace", func() {
			inB := map[string]interface{}{"matchExpressions": []interface{}{
				map[string]interface{}{"key": "team", "operator": "In", "values": []interface{}{"b", "c"}},
			}}
			c := newFakeClient(v, h100(),
				withSelector(eight("team-a-cq"), matchLabels("team", "a")),
				withSelector(clusterQueue(v, "team-b-cq", flavorQuotas("h100", quota("nvidia.com/gpu", "3"))), inB),
				namespace("team-b", map[string]string{"team": "b"}),
				localQueue(v, "team-b", "lq-a", "team-a-cq"), // not admitted
				localQueue(v, "team-b", "lq-b", "team-b-cq"), // admitted
			)
			snap, err := NewReader(c, Options{}).Quotas(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Namespace).To(Equal(map[string]config.ExternalCaps{
				"team-b": typedCaps(map[string]int{"NVIDIA-H100-80GB-HBM3": 3}),
			}))
		})

		It("logs a withheld grant once, and again after a successful read re-arms it", func() {
			now := time.Unix(1000, 0)
			deny := true
			var logged []string
			lctx := logr.NewContext(ctx, funcr.New(func(prefix, args string) {
				logged = append(logged, args)
			}, funcr.Options{}))
			c := fake.NewClientBuilder().
				WithScheme(runtime.NewScheme()).
				WithRESTMapper(kueueMapper(v)).
				WithObjects(h100(),
					withSelector(eight("team-a-cq"), matchLabels("team", "a")),
					namespace("team-a", map[string]string{"team": "a"}),
					localQueue(v, "team-a", "lq", "team-a-cq"),
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: denyNamespaceList,
					Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
						if deny {
							return forbidden(key.Name)
						}
						return cl.Get(ctx, key, o, opts...)
					},
				}).
				Build()
			r := NewReader(c, Options{now: func() time.Time { return now }})
			read := func() config.ExternalQuotas {
				now = now.Add(time.Minute)
				snap, err := r.Quotas(lctx)
				Expect(err).NotTo(HaveOccurred())
				return snap
			}

			read()
			Expect(logged).To(HaveLen(1))
			Expect(logged[0]).To(ContainSubstring("grant get on namespaces"))
			read()
			Expect(logged).To(HaveLen(1), "a standing denial is logged once")

			deny = false
			Expect(read().Namespace).To(HaveKeyWithValue("team-a", granted))
			deny = true
			read()
			Expect(logged).To(HaveLen(2), "a revoke after a successful read is news and is logged again")
		})
	})

	It("reports a cluster without Kueue as an error, not as no quota", func() {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).To(MatchError(ContainSubstring("is Kueue installed?")))
		Expect(snap.ObservedAt).To(BeZero())
		Expect(snap.Namespace).To(BeEmpty())
	})

	It("serves the cached snapshot inside the refresh interval and re-reads after it", func() {
		now := time.Unix(1000, 0)
		lists := 0
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(kueueMapper(v)).
			WithObjects(
				clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
				localQueue(v, "team-a", "lq", "cq"),
			).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		r := NewReader(c, Options{RefreshInterval: 30 * time.Second, now: func() time.Time { return now }})

		_, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(3), "one list per kind")

		now = now.Add(10 * time.Second)
		snap, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(3), "inside the interval nothing is re-read")
		Expect(snap.ObservedAt).To(Equal(time.Unix(1000, 0)))

		now = now.Add(30 * time.Second)
		snap, err = r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(6))
		Expect(snap.ObservedAt).To(Equal(now))
	})

	It("keeps the last good snapshot when a read fails, and says how old it is", func() {
		now := time.Unix(1000, 0)
		fail := false
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(kueueMapper(v)).
			WithObjects(
				clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
				localQueue(v, "team-a", "lq", "cq"),
			).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if fail {
						return errors.New("forbidden: boom")
					}
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		r := NewReader(c, Options{RefreshInterval: time.Second, now: func() time.Time { return now }})

		first, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())

		fail = true
		now = now.Add(time.Minute)
		snap, err := r.Quotas(ctx)
		Expect(err).To(MatchError(ContainSubstring("forbidden")))
		Expect(snap).To(Equal(first))
		Expect(snap.ObservedAt).To(Equal(time.Unix(1000, 0)))

		// The failure is remembered for the rest of the interval, not hidden by
		// the cache.
		now = now.Add(100 * time.Millisecond)
		_, err = r.Quotas(ctx)
		Expect(err).To(HaveOccurred())
	})
})

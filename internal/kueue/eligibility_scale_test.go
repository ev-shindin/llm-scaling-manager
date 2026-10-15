package kueue

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Review finding (scale): the eligibility check held its lock across every API
// read, so one slow namespace stalled the other engine's whole check; and
// every namespace fell due in the same cycle, a burst of reads once per
// interval.
var _ = Describe("Eligibility at scale", func() {
	// podLists counts pod lists per namespace; a namespace in block waits on
	// its channel before listing.
	type counter struct {
		mu    sync.Mutex
		lists map[string]int
		block map[string]chan struct{}
	}
	newCounter := func() *counter {
		return &counter{lists: map[string]int{}, block: map[string]chan struct{}{}}
	}
	funcsOf := func(c *counter) *interceptor.Funcs {
		return &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				lo := &client.ListOptions{}
				lo.ApplyOptions(opts)
				c.mu.Lock()
				c.lists[lo.Namespace]++
				ch := c.block[lo.Namespace]
				c.mu.Unlock()
				if ch != nil {
					<-ch
				}
			}
			return cl.List(ctx, list, opts...)
		}}
	}

	It("holds no lock across an API read: a slow namespace does not stall another caller", func() {
		c := newCounter()
		release := make(chan struct{})
		c.block["slow"] = release
		e := NewEligibility(eligibilityClient(funcsOf(c), chargedPod("p")...), Options{RefreshInterval: time.Minute})

		slowDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(slowDone)
			e.Evaluate(context.Background(), []string{"slow"})
		}()
		Eventually(func() int { c.mu.Lock(); defer c.mu.Unlock(); return c.lists["slow"] }).Should(Equal(1))

		fastDone := make(chan map[string]NamespaceEligibility, 1)
		go func() { fastDone <- e.Evaluate(context.Background(), []string{eligNS}) }()
		var got map[string]NamespaceEligibility
		Eventually(fastDone, 5*time.Second).Should(Receive(&got), "the other namespace is answered while the slow one is read")
		Expect(got[eligNS].Reason).To(BeEmpty())

		close(release)
		Eventually(slowDone, 5*time.Second).Should(BeClosed())
	})

	It("spreads re-evaluation over the interval instead of a burst once per interval", func() {
		c := newCounter()
		now := time.Unix(0, 0)
		var mu sync.Mutex
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		e := NewEligibility(eligibilityClient(funcsOf(c), chargedPod("p")...), Options{RefreshInterval: time.Minute, now: clock})
		namespaces := make([]string, 40)
		for i := range namespaces {
			namespaces[i] = fmt.Sprintf("ns-%02d", i)
		}
		e.Evaluate(context.Background(), namespaces)
		reread := func() int {
			n := 0
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, ns := range namespaces {
				if c.lists[ns] > 1 {
					n++
				}
			}
			return n
		}
		Expect(reread()).To(BeZero())

		mu.Lock()
		now = now.Add(45 * time.Second)
		mu.Unlock()
		e.Evaluate(context.Background(), namespaces)
		Expect(reread()).To(And(BeNumerically(">", 0), BeNumerically("<", len(namespaces))),
			"part of the namespaces fall due before the interval ends, not all at once")

		mu.Lock()
		now = now.Add(15 * time.Second)
		mu.Unlock()
		e.Evaluate(context.Background(), namespaces)
		Expect(reread()).To(Equal(len(namespaces)), "every namespace is re-read within one interval")
	})
})

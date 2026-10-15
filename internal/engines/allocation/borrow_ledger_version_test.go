package allocation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ledgerFrom41d34cc5a is a ledger exactly as commit 41d34cc5a (before the
// version field) persisted it: a release in flight, a released figure, both
// cooldowns and a placement-timeout hold.
const ledgerFrom41d34cc5a = `{"entries":[{"cohort":"c","lender":"lender","flavor":"h100","resource":"nvidia.com/gpu",` +
	`"units":[{"namespace":"b","variant":"m-v","to":3}],"releasedAt":"2026-10-15T12:01:01Z"}],` +
	`"released":{"c/h100/nvidia.com/gpu":8},"cohortCooldown":{"c":"2026-10-15T12:12:00Z"},` +
	`"modelCooldown":{"b/m":"2026-10-15T12:10:00Z"},"timeoutHeld":{"b/x-v":1}}`

var _ = Describe("the persisted borrow ledger across versions", func() {
	It("loads a ledger written before the version field, whole", func() {
		l, err := UnmarshalBorrowLedger(ledgerFrom41d34cc5a)
		Expect(err).NotTo(HaveOccurred())
		g := NewBorrowGovernor(DefaultBorrowSettings())
		Expect(g.Restore(l)).To(Equal(0))
		got := g.Ledger()
		Expect(got.Version).To(Equal(BorrowLedgerVersion), "rewritten in the current format")
		Expect(got.Entries).To(HaveLen(1))
		Expect(got.Entries[0].Units).To(Equal([]releasedReplicas{{Namespace: "b", Variant: "m-v", To: 3}}))
		Expect(got.Released).To(Equal(map[string]int{"c/h100/nvidia.com/gpu": 8}))
		Expect(got.CohortCooldown).To(HaveKey("c"))
		Expect(got.ModelCooldown).To(HaveKey("b/m"))
		Expect(got.TimeoutHeld).To(Equal(map[string]int{"b/x-v": 1}))
	})

	It("loads a ledger of this version with fields it does not know", func() {
		withExtra := strings.Replace(ledgerFrom41d34cc5a, `{"entries"`, `{"version":1,"fromALaterPatch":{"x":[1,2]},"entries"`, 1)
		l, err := UnmarshalBorrowLedger(withExtra)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Entries).To(HaveLen(1))
		Expect(l.TimeoutHeld).To(Equal(map[string]int{"b/x-v": 1}))
	})

	It("writes a version an older build ignores (rollback): only fields it knows", func() {
		g := NewBorrowGovernor(DefaultBorrowSettings())
		l, err := UnmarshalBorrowLedger(ledgerFrom41d34cc5a)
		Expect(err).NotTo(HaveOccurred())
		g.Restore(l)
		raw, err := g.MarshalLedger()
		Expect(err).NotTo(HaveOccurred())
		Expect(raw).To(HavePrefix(`{"version":1,"entries":`))
		Expect(strings.Replace(raw, `"version":1,`, "", 1)).To(Equal(ledgerFrom41d34cc5a),
			"apart from the version field the format is unchanged, so 41d34cc5a reads it as before")
	})

	It("discards a ledger of an unknown version as such, not as corrupt", func() {
		for _, v := range []int{2, 99, -1} {
			l, err := UnmarshalBorrowLedger(fmt.Sprintf(`{"version":%d,"entries":[],"timeoutHeld":{"b/x-v":1}}`, v))
			Expect(errors.Is(err, ErrBorrowLedgerVersion)).To(BeTrue(), "version %d", v)
			Expect(l).To(Equal(BorrowLedger{}), "nothing of it is restored")
		}
	})

	It("drops absurd values and caps the entry count on load", func() {
		at := time.Unix(1000, 0)
		key := LenderKey{Cohort: "c", Lender: "l", Flavor: "h100", Resource: "nvidia.com/gpu"}
		l := BorrowLedger{
			Entries: []ReleaseLedgerEntry{
				{LenderKey: key, ReleasedAt: at, Units: []releasedReplicas{{Namespace: "b", Variant: "v", To: maxLedgerReplicas + 1}}},
			},
			Released:    map[string]int{"c/h100/nvidia.com/gpu": maxLedgerGPUs + 1, "c/a100/nvidia.com/gpu": -3},
			TimeoutHeld: map[string]int{"b/huge": maxLedgerReplicas + 1, "b/neg": -1, "b/ok": 2},
		}
		g := NewBorrowGovernor(DefaultBorrowSettings())
		Expect(g.Restore(l)).To(Equal(3), "the absurd unit, released figure and hold")
		got := g.Ledger()
		Expect(got.Entries).To(BeEmpty(), "an entry left with no unit pins nothing")
		Expect(got.Released).To(BeEmpty(), "negative and absurd figures are not restored")
		Expect(got.TimeoutHeld).To(Equal(map[string]int{"b/ok": 2}))

		many := BorrowLedger{TimeoutHeld: map[string]int{}, ModelCooldown: map[string]time.Time{}}
		for i := range maxLedgerItems + 10 {
			many.TimeoutHeld[fmt.Sprintf("b/v%05d", i)] = 1
			many.ModelCooldown[fmt.Sprintf("b/m%05d", i)] = at
		}
		g = NewBorrowGovernor(DefaultBorrowSettings())
		Expect(g.Restore(many)).To(Equal(20))
		Expect(g.Ledger().TimeoutHeld).To(HaveLen(maxLedgerItems))
		Expect(g.Ledger().ModelCooldown).To(HaveLen(maxLedgerItems))
	})

	It("does not let a restored idleSince from the future hold the release state", func() {
		now := time.Unix(50000, 0)
		future := now.Add(24 * time.Hour)
		g := NewBorrowGovernor(DefaultBorrowSettings())
		g.Restore(BorrowLedger{TimeoutHeld: map[string]int{"b/x-v": 1}, IdleSince: &future})
		s := DefaultBorrowSettings()
		g.HoldReleases(now, nil, nil)
		g.HoldReleases(now.Add(s.Cooldown+s.ReleaseActuation+time.Second), nil, nil)
		Expect(g.Ledger().TimeoutHeld).To(BeEmpty(), "the window counts from now, not from a day ahead")
	})
})

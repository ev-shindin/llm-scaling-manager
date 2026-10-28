package saturation

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// lbProcess is variant-b borrowing from variant-a, both on a compatible
// engine, with the donor's window at donorK2.
func lbProcess(t *testing.T, donorK2 float64) (a *SaturationAnalyzer, own, donor string, clock *time.Time) {
	t.Helper()
	now := time.Now()
	clock = &now
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	store.Update("test-ns", "test-model", "variant-b", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
	a = NewSaturationAnalyzer(store)
	a.now = func() time.Time { return *clock }
	own = a.historyKey("test-model", "test-ns", "variant-b", "H100", 1, domain.RoleBoth, 50, 5)
	donor = a.historyKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleBoth, 50, 5)
	lbSetDonor(a, donor, donorK2)
	return a, own, donor, clock
}

func lbSetDonor(a *SaturationAnalyzer, donor string, k2 float64) {
	ra := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, time.Now())
	ra.AddAt(k2, time.Now())
	a.mu.Lock()
	a.computeCapacityHistory[donor] = ra
	a.mu.Unlock()
}

// lbSpend serves the bound through its hold and relaxation.
func lbSpend(t *testing.T, a *SaturationAnalyzer, own string, clock *time.Time) {
	t.Helper()
	if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	for end := clock.Add(BorrowedK2Hold + BorrowedK2Relax + time.Minute); clock.Before(end); {
		*clock = clock.Add(time.Minute)
		a.noteBorrowServing(own, *clock, true)
	}
	if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); ok {
		t.Fatal("precondition: the bound is spent")
	}
}

// A spent bound re-arms only on a figure materially LOWER than the one it last
// ran on, from the same donor or a new one. A donor that measured higher would only re-arm the variant downward
// from its own, relaxed figure, for nothing.
func TestASpentBoundReArmsOnlyOnALowerFigure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		donor  float64 // the donor's new figure; 0 = the donor gone, a new one at 3000 in its place
		rearms bool
	}{
		{"the same donor measured higher", 6000, false},
		{"the same donor drifted down within the threshold", 3400, false},
		{"the same donor measured materially lower", 3000, true},
		{"a new, lower donor took the old one's place", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, own, donor, clock := lbProcess(t, 4000)
			lbSpend(t, a, own, clock)
			if tc.donor > 0 {
				lbSetDonor(a, donor, tc.donor)
			} else {
				a.mu.Lock()
				delete(a.computeCapacityHistory, donor)
				a.mu.Unlock()
				a.capacityStore.Update("test-ns", "test-model", "variant-c", capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, time.Now())
				lbSetDonor(a, a.historyKey("test-model", "test-ns", "variant-c", "H100", 1, domain.RoleBoth, 50, 5), 3000)
			}
			*clock = clock.Add(time.Minute)
			_, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true)
			if ok != tc.rearms {
				t.Errorf("re-armed = %v, want %v", ok, tc.rearms)
			}
		})
	}
}

// Every map field of the analyzer is in learnedMapsLocked, the list forget
// and flush walk: a map added and not listed is a forget that leaves it
// behind. notLearned names the maps that are not learned state, and why.
func TestEveryAnalyzerMapIsRegistered(t *testing.T) {
	notLearned := map[string]string{
		"restoreOutcomes": "counts not yet published, by restore outcome; nothing a forget or flush may take back",
	}
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.mu.Lock()
	listed := map[string]bool{}
	for _, m := range a.learnedMapsLocked() {
		listed[m.name] = true
	}
	a.mu.Unlock()
	typ := reflect.TypeOf(a).Elem()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if _, exempt := notLearned[f.Name]; f.Type.Kind() == reflect.Map && !listed[f.Name] && !exempt {
			t.Errorf("SaturationAnalyzer.%s is a map learnedMapsLocked does not list", f.Name)
		}
	}
}

// Forgetting a variant leaves none of its entries in any map whose keys name
// it, and leaves another variant's alone. Each registered map gets an entry
// for both, by reflection, so a map added to the list is covered at once.
func TestForgetEmptiesEveryRegisteredMapOfTheVariant(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	keys := func(variant string) map[keyScope]string {
		return map[keyScope]string{
			scopeK2Key:       "test-ns|" + variant + "|test-model|H100|1|both|m|q5",
			scopeITLKey:      "test-ns|test-model|" + variant + "|H100|1",
			scopeVariantName: "test-ns/" + variant,
		}
	}
	v := reflect.ValueOf(a).Elem()
	a.mu.Lock()
	scopes := map[string]keyScope{}
	for _, m := range a.learnedMapsLocked() {
		scopes[m.name] = m.scope
	}
	a.mu.Unlock()
	for name, scope := range scopes {
		if scope == scopeOwnRule || scope == scopeShared {
			continue
		}
		f := v.FieldByName(name)
		f = reflect.NewAt(f.Type(), f.Addr().UnsafePointer()).Elem()
		for _, variant := range []string{"variant-b", "variant-c"} {
			f.SetMapIndex(reflect.ValueOf(keys(variant)[scope]), reflect.New(f.Type().Elem()).Elem())
		}
	}
	a.ForgetVariant("test-ns", "test-model", "variant-b", "t1")
	for name, scope := range scopes {
		if scope == scopeOwnRule || scope == scopeShared {
			continue
		}
		f := v.FieldByName(name)
		f = reflect.NewAt(f.Type(), f.Addr().UnsafePointer()).Elem()
		for _, k := range f.MapKeys() {
			if strings.Contains(k.String(), "variant-b") {
				t.Errorf("%s kept %q after the variant was forgotten", name, k.String())
			}
		}
		if f.Len() != 1 {
			t.Errorf("%s: forgetting variant-b left %d entries, want variant-c's one", name, f.Len())
		}
	}
}

// A running sibling borrow is written in the form a controller from before
// the lower bounds reads (K2BorrowedSince), so a rollback keeps it; the
// variant's own previous figure, which that controller has no notion of, is
// not.
func TestARunningBorrowIsExportedForARollback(t *testing.T) {
	a, own, _, clock := lbProcess(t, 4000)
	if _, ok := a.borrowedK2(own, "test-model", 12800, mtParams(256, "vllm:v1"), true, true); !ok {
		t.Fatal("precondition: variant-b borrows")
	}
	prior := "test-ns|variant-b|test-model|H100|1|both|xl|q5"
	a.mu.Lock()
	a.lowerBounds[prior] = &lowerBound{armed: true, since: *clock, tick: *clock, seen: *clock, own: true, prior: 3000}
	a.mu.Unlock()
	s := a.ExportLearnedState()
	if _, ok := s.K2BorrowedSince[own]; !ok {
		t.Error("a running sibling borrow was not written for a rollback")
	}
	if _, ok := s.K2BorrowedSince[prior]; ok {
		t.Error("the variant's own previous figure was written as a sibling borrow")
	}
	if len(s.LowerBounds) != 2 {
		t.Errorf("want both bounds exported whole, got %d", len(s.LowerBounds))
	}
}

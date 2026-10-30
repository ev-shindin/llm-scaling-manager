package main

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The recorder's byte flags take their defaults from the environment, which
// is how an installed controller sets them (kubectl set env). A value that is
// not a whole number is one the options check refuses: the recorder stays
// off, with an error logged, rather than recording at a size nobody set.
func TestEnvBytes(t *testing.T) {
	t.Setenv("LEARNREC_TEST_BYTES", "")
	if got := envBytes("LEARNREC_TEST_BYTES", 7); got != 7 {
		t.Errorf("empty: %d, want the default", got)
	}
	if got := envBytes("LEARNREC_TEST_UNSET_BYTES", 7); got != 7 {
		t.Errorf("unset: %d, want the default", got)
	}
	t.Setenv("LEARNREC_TEST_BYTES", "1048576")
	if got := envBytes("LEARNREC_TEST_BYTES", 7); got != 1048576 {
		t.Errorf("set: %d", got)
	}
	t.Setenv("LEARNREC_TEST_BYTES", "1Mi")
	got := envBytes("LEARNREC_TEST_BYTES", 7)
	if err := (learnrec.Options{Dir: "/var/x", CapBytes: got}).Check(); err == nil {
		t.Errorf("a cap of %q read as %d, and the options check took it", "1Mi", got)
	}
}

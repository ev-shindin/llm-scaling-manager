package warmpool

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
)

const fallbackMsg = "no ScaledObject declares a warm pool; falling back to the unnamed one"

// The reconciler resolves its pools on every pass, and an install that
// declares none wrote the fallback line on every one of them at the shipped
// verbosity. It is said when what was seen changes, and at DEBUG otherwise.
func TestFallbackIsLoggedOncePerChangeAtTheShippedLevel(t *testing.T) {
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))

	plain := registry.Entry{Namespace: "workload", Name: "model-a", Metadata: map[string]string{"modelID": "a"}}
	entries := []registry.Entry{plain}
	r := &RegistryPools{
		Snapshot:  func() []registry.Entry { return entries },
		Namespace: "workload",
		Fallback:  policy.Config{SleepMinSize: 1},
	}
	pass := func(n int) {
		t.Helper()
		for range n {
			if _, err := r.Pools(ctx); err != nil {
				t.Fatalf("Pools: %v", err)
			}
		}
	}
	said := func() int { return logs.FilterMessage(fallbackMsg).Len() }

	pass(10)
	if got := said(); got != 1 {
		t.Fatalf("ten passes over the same registry: want the fallback said once, got %d", got)
	}

	// What was seen changed: a second entry arrived. Said again, once.
	entries = []registry.Entry{plain, {Namespace: "workload", Name: "model-b", Metadata: map[string]string{"modelID": "b"}}}
	pass(10)
	if got := said(); got != 2 {
		t.Fatalf("a new entry is a change: want the fallback said twice in all, got %d", got)
	}

	// A pool is declared, then removed: falling back again is said again.
	entries = []registry.Entry{poolTrigger("pool-so", "fast", 4, nil)}
	pass(3)
	if got := said(); got != 2 {
		t.Fatalf("a declared pool is no fallback: want still 2 lines, got %d", got)
	}
	entries = []registry.Entry{plain}
	pass(3)
	if got := said(); got != 3 {
		t.Fatalf("falling back after a pool was declared is a change: want 3 lines, got %d", got)
	}
}

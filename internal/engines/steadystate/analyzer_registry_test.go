package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An Engine built by hand, with no registry, registers, reconciles and
// freezes as one built by NewEngine does.
func TestAnalyzerRegistry_AHandBuiltEngineRegistersAndFreezes(t *testing.T) {
	e := &Engine{}
	e.RemoveExternalAnalyzer("none") // a nil registry removes nothing
	assert.Empty(t, e.analyzerRunEntries())

	require.NoError(t, e.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"}))
	e.UpsertExternalAnalyzer("ext", &spyAnalyzer{name: "ext"})
	e.ensureAnalyzers().start()
	require.ErrorContains(t, e.RegisterAnalyzer("late", &spyAnalyzer{name: "late"}), "called after StartOptimizeLoop")

	entries := e.analyzerRunEntries()
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.name)
	}
	assert.Equal(t, []string{"throughput", "ext"}, names)
	e.RemoveExternalAnalyzer("ext")
	assert.Empty(t, e.analyzers.externalNames())
}

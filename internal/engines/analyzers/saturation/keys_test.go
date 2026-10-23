package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A prefill replica keeps its own output length in the k2 key: it generates
// one token per request, so its bucket is fixed, and the decode fleet's length
// is not prefill's work.
func TestK2KeyOutput_PrefillKeepsItsOwnLength(t *testing.T) {
	assert.Equal(t, 1.0, k2KeyOutput(domain.RolePrefill, 6000, 1))
	assert.Equal(t, 6000.0, k2KeyOutput(domain.RoleDecode, 6000, 480))
	assert.Equal(t, 6000.0, k2KeyOutput(domain.RoleBoth, 6000, 480))
	assert.Equal(t, 480.0, k2KeyOutput(domain.RoleDecode, 0, 480), "no tracked length yet: the replica's own")
}

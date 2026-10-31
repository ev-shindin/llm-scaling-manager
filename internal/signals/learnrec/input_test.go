package learnrec

import (
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The recorded input is the analyzer input field for field, less the fields
// no analyzer reads (VariantMetadata) and plus the policy's type. A field
// added to domain.AnalyzerInput is either recorded or named here: an
// analyzer that reads a field the recording drops cannot be replayed. The
// values themselves are held by learnreplay's
// TestInput_EveryAnalyzerInputFieldSurvivesTheRecording.
//
// The VariantMetadata exemption is revisited at #345 step 2, which takes the
// composite's variants from it; the step that first has an analyzer read it
// records it and ends the exemption.
func TestInput_MirrorsTheAnalyzerInput(t *testing.T) {
	names := func(typ reflect.Type, drop ...string) []string {
		out := []string{}
		for i := range typ.NumField() {
			if n := typ.Field(i).Name; !slices.Contains(drop, n) {
				out = append(out, n)
			}
		}
		return out
	}
	assert.Equal(t,
		names(reflect.TypeFor[domain.AnalyzerInput](), "VariantMetadata"),
		names(reflect.TypeFor[Input](), "ConfigType"))
}

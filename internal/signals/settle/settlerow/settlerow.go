// Package settlerow reads the collector's replica rows as the settle
// tracker's (internal/signals/settle). The engine observes these, and a
// replay of a learner recording observes the same, so the two cannot read a
// row differently. It is apart from settle so that settle itself imports
// only the standard library.
package settlerow

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/settle"
)

// Of is one cycle's rows as the settle tracker reads them: one row per
// replica, with its role read from rolesByVariant (a variant it does not name
// is aggregated).
func Of(rows []domain.ReplicaMetrics, rolesByVariant map[string]string) []settle.Row {
	out := make([]settle.Row, 0, len(rows))
	for _, rm := range rows {
		out = append(out, Row(rm, domain.CanonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill))
	}
	return out
}

// Row is the settle tracker's row of rm; prefill says it serves the prefill
// role.
func Row(rm domain.ReplicaMetrics, prefill bool) settle.Row {
	itl := settle.ITLAbsent
	switch rm.ITLState() {
	case domain.ITLFinite:
		itl = settle.ITLFinite
	case domain.ITLEmpty:
		itl = settle.ITLEmpty
	}
	return settle.Row{
		Key:                 Key(rm),
		Prefill:             prefill,
		Ready:               rm.Ready,
		ReadySince:          rm.ReadySince,
		LentSince:           rm.LentSince,
		Revision:            rm.Revision,
		KV:                  rm.KvCacheUsage,
		KVKnown:             rm.KvCacheUsageKnown,
		ITL:                 itl,
		GenerationRate:      rm.GenerationTokenRate,
		GenerationRateKnown: rm.GenerationTokenRateKnown,
		RequestRate:         rm.RequestRate,
		RequestRateKnown:    rm.RequestRateKnown,
	}
}

// Key is the settle tracker's key of rm.
func Key(rm domain.ReplicaMetrics) settle.Key {
	return settle.Key{Pod: rm.PodName, Variant: rm.VariantName, Bridge: rm.FromWarmPool}
}

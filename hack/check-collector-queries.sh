#!/usr/bin/env bash
# The collector's per-pod vLLM queries fold a pod's data-parallel ranks the way
# the collector merges ranks that run their own API servers, and leave a pod
# with one rank exactly as before.
#
# A query that takes a max over a pod's ranks parses, returns one row per pod
# and looks healthy; it only prices the busiest rank as the whole pod. promtool
# evaluates the queries on synthetic per-rank series: one pod of eight ranks
# behind one API server, one ordinary pod scraped twice, a pod of two ranks
# scraped twice, and a pod of two ranks whose gauges carry one copy per API
# server process (issue #150).
#
# The fixture's exprs are the queries the registry builds; the Go test
# TestPerPodQueriesMatchPromtoolFixture fails if either side changes alone.
#
# PROMTOOL is the promtool binary (make promtool installs a pinned one).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMTOOL=${PROMTOOL:-promtool}
FIXTURE="$ROOT/internal/collector/registration/testdata/per-pod-queries.test.yaml"

if ! command -v "$PROMTOOL" >/dev/null 2>&1; then
  echo "FAIL: promtool not found (PROMTOOL=$PROMTOOL); run make promtool" >&2
  exit 1
fi
if [ ! -f "$FIXTURE" ]; then
  echo "FAIL: $FIXTURE is missing" >&2
  exit 1
fi
# An empty fixture passes promtool; count the cases it holds first.
N=$(grep -c '^      - expr:' "$FIXTURE")
if [ "$N" -ne 14 ]; then
  echo "FAIL: $N query cases in $(basename "$FIXTURE"), not 14. Update the count deliberately." >&2
  exit 1
fi
if ! (cd "$(dirname "$FIXTURE")" && "$PROMTOOL" test rules "$(basename "$FIXTURE")"); then
  echo "FAIL: a per-pod query does not fold data-parallel ranks as the collector expects" >&2
  exit 1
fi
echo "ok: the per-pod queries fold data-parallel ranks ($N queries)"

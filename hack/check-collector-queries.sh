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
# A second fixture evaluates the scheduler flow-control queue queries over two
# namespaces serving the same model name. Grouped by model name alone they
# returned one summed series, and the idle namespace read the busy one's queue
# as its own demand (TestEPPQueueQueriesMatchPromtoolFixture pins its exprs).
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
if [ "$N" -ne 20 ]; then
  echo "FAIL: $N query cases in $(basename "$FIXTURE"), not 20. Update the count deliberately." >&2
  exit 1
fi
if ! (cd "$(dirname "$FIXTURE")" && "$PROMTOOL" test rules "$(basename "$FIXTURE")"); then
  echo "FAIL: a per-pod query does not fold data-parallel ranks as the collector expects" >&2
  exit 1
fi
echo "ok: the per-pod queries fold data-parallel ranks ($N queries)"

QFIXTURE="$ROOT/internal/collector/registration/testdata/epp-queue-namespace.test.yaml"
if [ ! -f "$QFIXTURE" ]; then
  echo "FAIL: $QFIXTURE is missing" >&2
  exit 1
fi
QN=$(grep -c '^      - expr:' "$QFIXTURE")
if [ "$QN" -ne 2 ]; then
  echo "FAIL: $QN query cases in $(basename "$QFIXTURE"), not 2. Update the count deliberately." >&2
  exit 1
fi
if ! (cd "$(dirname "$QFIXTURE")" && "$PROMTOOL" test rules "$(basename "$QFIXTURE")"); then
  echo "FAIL: a scheduler queue query does not keep namespaces apart" >&2
  exit 1
fi
echo "ok: the scheduler queue queries keep each namespace's queue apart ($QN queries)"

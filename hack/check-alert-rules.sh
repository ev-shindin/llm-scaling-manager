#!/usr/bin/env bash
# The shipped PrometheusRule parses, and the SLO alerts fire when they should
# and stay quiet when they should not.
#
# test/alerts checks that every alert names a metric that exists; it cannot
# evaluate an expression. A rule that errors at evaluation (a many-to-many join
# after a controller restart) or never fires (a gap that resets its for:) loads
# cleanly and looks exactly like a healthy model. promtool evaluates them on
# synthetic series. It needs no cluster.
#
#   1. promtool check rules: every expression in the shipped rule parses;
#   2. promtool test rules on every test/alerts/testdata/*.test.yaml:
#      slo-alerts: a judged miss fires, a frozen or parked model and too few
#      miss-minutes do not, overlapping series from two controller pods still
#      join, and a model never judged raises WVAModelSLOUnjudged.
#      kueue-alerts: the Kueue borrowing alerts fire per namespace, per quota
#      entry and per controller instance as their runbooks say, the
#      thrashing alert counts four losses, not three, and not releases to a
#      waiting lender, and the lender-waiting alert adds a release's latency
#      to the cooldown only in the cohort it was measured for.
#      learned-state-alerts: a snapshot that cannot be written raises
#      WVALearnedStateSnapshotFailing, and one that recovers stops it.
#      utilization-share-alerts: a frozen group, floors past the quota and an
#      invalid optimizer block fire, passing states do not, a parked model
#      with no wake signal pages once, not twice, and a same-named model
#      refusing requests in another namespace pages neither.
#      load-imbalance-alerts: a role kept imbalanced for 10 minutes pages
#      once per model and role, from two controller pods too; one that
#      clears or flips inside the window does not; a dip does not resolve a
#      firing page, and a cleared one resolves after keep_firing_for.
#
# PROMTOOL is the promtool binary (make promtool installs a pinned one); YQ is
# mikefarah yq v4.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROMTOOL=${PROMTOOL:-promtool}
YQ=${YQ:-yq}
RULE=${RULE:-$ROOT/config/components/prometheus-alerts/prometheusrule.yaml}
TESTS="$ROOT/test/alerts/testdata"

if ! command -v "$PROMTOOL" >/dev/null 2>&1; then
  echo "FAIL: promtool not found (PROMTOOL=$PROMTOOL); run make promtool" >&2
  exit 1
fi
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# promtool reads plain rule files, not the PrometheusRule wrapper.
"$YQ" '{"groups": .spec.groups}' "$RULE" > "$WORK/full.yaml" || exit 1
N=$("$YQ" '[.groups[].rules[]] | length' "$WORK/full.yaml")
case "$N" in '' | *[!0-9]*) N=0 ;; esac
if [ "$N" -lt 1 ]; then
  echo "FAIL: no rules extracted from $RULE" >&2
  exit 1
fi
if ! "$PROMTOOL" check rules "$WORK/full.yaml"; then
  echo "FAIL: the shipped PrometheusRule does not parse" >&2
  exit 1
fi

# The tests compare labels and firing, not the annotation prose.
"$YQ" 'del(.groups[].rules[].annotations)' "$WORK/full.yaml" > "$WORK/rules.yaml" || exit 1
RAN=0
for t in "$TESTS"/*.test.yaml; do
  [ -f "$t" ] || continue
  cp "$t" "$WORK/"
  if ! (cd "$WORK" && "$PROMTOOL" test rules "$(basename "$t")"); then
    echo "FAIL: an alert fired, or stayed quiet, against $(basename "$t")" >&2
    exit 1
  fi
  RAN=$((RAN + 1))
done
# Every file, or a rename silently drops one.
if [ "$RAN" -ne 5 ]; then
  echo "FAIL: $RAN alert test files ran, not 5. Update the count deliberately." >&2
  exit 1
fi
echo "ok: alert rules parse and the alert tests pass ($RAN files)"

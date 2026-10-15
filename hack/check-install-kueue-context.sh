#!/usr/bin/env bash
#
# Executes deploy/ci-pr-checks/install-kueue.sh against a stub kubectl and
# checks its context guard: it installs Kueue -- and narrows Kueue's webhooks --
# only on a kind cluster (context kind-*), or where KUEUE_ALLOW_NON_KIND=true
# says the operator means it. A guard that does not refuse parses perfectly;
# only running it shows that a production context gets nothing applied.
#
# The stub answers `config current-context` with $STUB_CONTEXT (failing when it
# is empty, as kubectl does with no context) and reports a Kueue controller in
# another namespace to every Deployment list, so a run that gets past the guard
# stops at the next refusal ("outside kueue-system") before anything is
# downloaded or applied. Every call is logged; a refused run must have made no
# call but the context read.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/deploy/ci-pr-checks/install-kueue.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAIL=0
CASES=0

mkdir -p "$TMP/bin"
cat > "$TMP/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "$STUB_LOG"
if [ "$1 $2" = "config current-context" ]; then
    [ -n "$STUB_CONTEXT" ] || { echo "error: current-context is not set" >&2; exit 1; }
    echo "$STUB_CONTEXT"
    exit 0
fi
if [ "$1 $2" = "get deploy" ]; then
    echo "elsewhere/kueue-controller-manager"
    exit 0
fi
exit 1
STUB
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >> "$STUB_LOG"
exit 1
STUB
chmod +x "$TMP/bin/kubectl" "$TMP/bin/curl"

# run <name> <context> <allow> <want: refused|past-guard>
run() {
    local name="$1" context="$2" allow="$3" want="$4" out status
    CASES=$((CASES + 1))
    : > "$TMP/log"
    out="$(PATH="$TMP/bin:$PATH" STUB_LOG="$TMP/log" STUB_CONTEXT="$context" \
        KUEUE_ALLOW_NON_KIND="$allow" bash "$SCRIPT" 2>&1)"
    status=$?
    if [ "$status" -eq 0 ]; then
        echo "FAIL $name: exited 0 against a stub that refuses everything"
        FAIL=1
        return
    fi
    case "$want" in
        refused)
            if ! grep -q "is not a kind cluster" <<<"$out"; then
                echo "FAIL $name: not refused by the context guard: $out"
                FAIL=1
            elif [ "$(grep -cv '^kubectl config current-context$' "$TMP/log")" -ne 0 ]; then
                echo "FAIL $name: refused, but called more than the context read:"
                cat "$TMP/log"
                FAIL=1
            fi
            ;;
        past-guard)
            if grep -q "is not a kind cluster" <<<"$out" || ! grep -q "outside kueue-system" <<<"$out"; then
                echo "FAIL $name: did not get past the context guard: $out"
                FAIL=1
            fi
            ;;
    esac
}

run "production context"                       "prod-admin@prod" ""     refused
run "no context at all"                         ""                ""     refused
run "a kind-like name that is not kind-*"       "mykind"          ""     refused
run "an override that is not exactly true"      "prod-admin@prod" "yes"  refused
run "a kind cluster"                            "kind-kind"       ""     past-guard
run "a kind cluster named for the e2e"          "kind-wva-e2e"    ""     past-guard
run "a non-kind context with the override"      "prod-admin@prod" "true" past-guard

if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
echo "install-kueue.sh context guard: ${CASES} cases passed"

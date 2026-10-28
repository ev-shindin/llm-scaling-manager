#!/usr/bin/env bash
#
# Executes the installer's Prometheus auto-detection against a stubbed kubectl
# holding several Prometheus resources, and checks it picks one that scrapes
# the model namespace.
#
# It used to take the first Prometheus the API listed, which is ordered by
# namespace name. On a shared cluster a new per-user Prometheus selecting
# monitors only in its own namespace sorted first; the controller read zero
# series, every variant looked idle, and the install reported success.
#
# Needs jq. Missing it is FATAL, never a skip.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

command -v jq >/dev/null 2>&1 || { printf 'FATAL: jq is required.\n' >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh infra_wva.sh infra_monitoring.sh; do
    tr -d '\r' < "deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/infra_wva.sh"
# shellcheck source=/dev/null
source "$WORK/infra_monitoring.sh"

EXPECT_CASES=43
RAN=0
FAILED=0

check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

# has <name> <needle> <haystack>: a case that the text contains the needle.
has() {
    if printf '%s' "$3" | grep -qF -- "$2"; then check "$1" yes yes; else check "$1" yes "no: $(printf '%s' "$3" | tr '\n' '|' | cut -c1-600)"; fi
}

# lacks <name> <needle> <haystack>: a case that the text does not contain it.
lacks() {
    if printf '%s' "$3" | grep -qF -- "$2"; then check "$1" absent "present: $(printf '%s' "$3" | tr '\n' '|' | cut -c1-600)"; else check "$1" absent absent; fi
}

# ----------------------------------------------------------------------------
# The stub: one JSON world per case.
#   crs   [{ns, name, spec}]           Prometheus resources, in API order
#   nss   {<ns>: <labels>}             Namespaces
#   pms   {<ns>: [<labels>]}           PodMonitors, by their labels
#   sms   {<ns>: [<labels>]}           ServiceMonitors, by their labels
#   forbid [<token>]                   reads refused: namespace, podmonitors,
#                                      servicemonitors, prometheuses, crjson
# Every Prometheus namespace has a plain-http prometheus-operated Service.
# ----------------------------------------------------------------------------
world() {
    printf '%s' '{"crs": [], "nss": {}, "pms": {}, "sms": {}, "forbid": []}' > "$WORK/world.json"
}
# cr <namespace> <name> <spec json>
cr() {
    jq --arg ns "$1" --arg n "$2" --argjson s "$3" '.crs += [{ns: $ns, name: $n, spec: $s}]' \
        "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
}
# set_world <jq update>, e.g. set_world '.nss["team-a"] = {}'
set_world() {
    jq "$1" "$WORK/world.json" > "$WORK/world.next" && mv "$WORK/world.next" "$WORK/world.json"
}

forbidden() {
    printf 'Error from server (Forbidden): %s is forbidden: User "tenant" cannot get resource "%s"\n' "$1" "$1" >&2
    return 1
}

refused() { jq -e --arg k "$1" '.forbid | index($k)' "$WORK/world.json" >/dev/null; }

kubectl() {
    local W="$WORK/world.json" verb="$1" kind="" name="" ns="" all=0 out="" obj
    shift
    case "$verb" in
        get) ;;
        *) return 0 ;;
    esac
    kind="$1"
    shift
    while [ $# -gt 0 ]; do
        case "$1" in
            -n) ns="$2"; shift 2 ;;
            -A) all=1; shift ;;
            -o) out="$2"; shift 2 ;;
            -o*) out="${1#-o}"; shift ;;
            *) name="$1"; shift ;;
        esac
    done
    case "$kind" in
        prometheuses.monitoring.coreos.com)
            refused prometheuses && { forbidden prometheuses; return 1; }
            if [ "$all" = 1 ]; then
                case "$out" in
                    json)
                        refused crjson && { forbidden prometheuses; return 1; }
                        jq '{items: [.crs[] | {metadata: {namespace: .ns, name: .name}, spec: .spec}]}' "$W" ;;
                    *) jq -r '.crs[] | .ns + " " + .name' "$W" ;;
                esac
                return 0
            fi
            obj="$(jq -c --arg ns "$ns" --arg n "$name" '.crs[] | select(.ns == $ns and .name == $n)' "$W")"
            [ -n "$obj" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
            case "$out" in
                json) printf '%s' "$obj" | jq '{metadata: {name: .name, namespace: .ns}, spec: .spec}' ;;
                *tlsConfig*) : ;;
            esac ;;
        namespace|namespaces)
            refused namespace && { forbidden namespaces; return 1; }
            obj="$(jq -c --arg n "$name" '.nss[$n] // empty' "$W")"
            [ -n "$obj" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
            jq -n --arg n "$name" --argjson l "$obj" '{metadata: {name: $n, labels: $l}}' ;;
        podmonitors.monitoring.coreos.com)
            refused podmonitors && { forbidden podmonitors; return 1; }
            jq --arg ns "$ns" '{items: [(.pms[$ns] // [])[] | {metadata: {labels: .}}]}' "$W" ;;
        servicemonitors.monitoring.coreos.com)
            refused servicemonitors && { forbidden servicemonitors; return 1; }
            jq --arg ns "$ns" '{items: [(.sms[$ns] // [])[] | {metadata: {labels: .}}]}' "$W" ;;
        svc)
            # A prometheus-operated Service in each namespace that holds a CR.
            jq -e --arg ns "$ns" 'any(.crs[]; .ns == $ns)' "$W" >/dev/null || {
                [ -z "$name" ] && [ "$out" = json ] && echo '{"items": []}'
                [ -n "$name" ] && { echo "Error from server (NotFound): not found" >&2; return 1; }
                return 0
            }
            if [ -z "$name" ]; then
                case "$out" in
                    json) echo '{"items": [{"metadata": {"name": "prometheus-operated"}, "spec": {"selector": {"app.kubernetes.io/name": "prometheus"}, "ports": [{"name": "web", "port": 9090}]}}]}' ;;
                    *) echo "prometheus-operated 9090" ;;
                esac
                return 0
            fi
            case "$out" in
                "") echo "$name" ;;
                *) echo 9090 ;;
            esac ;;
    esac
    return 0
}

ENVIRONMENT=kubernetes
WVA_SCOPE=namespace
WVA_NS=wva-system
MONITORING_NAMESPACE=nothing-here
NAMESPACE="" NAMESPACE_EXPLICIT="" WVA_WATCH_NS="" PROMETHEUS_URL="" PROMETHEUS_URL_EXPLICIT=""

url() { printf 'http://prometheus-operated.%s.svc.cluster.local:9090' "$1"; }
detect() { ( wva_detect_prometheus_url 2>/dev/null ); }
detect_err() { ( wva_detect_prometheus_url 2>&1 >/dev/null ); }
verdicts() { ( wva_prometheus_coverage "$1" 2>/dev/null | awk '{print $1 "=" $3}' | paste -sd' ' - ); }

OWN() { printf '{"matchLabels": {"kubernetes.io/metadata.name": "%s"}}' "$1"; }
ALL='{"podMonitorNamespaceSelector": {}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {}, "serviceMonitorSelector": {}}'

# ----------------------------------------------------------------------------
# The shapes seen on a shared cluster, in the order the API lists them
# ----------------------------------------------------------------------------
shared_cluster() {
    world
    cr aaa-dev prometheus "{\"podMonitorNamespaceSelector\": $(OWN aaa-dev), \"serviceMonitorNamespaceSelector\": $(OWN aaa-dev), \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
    cr llm-d-wva-monitoring prometheus "$ALL"
    cr monitoring k8s "$ALL"
    cr zz-dev prometheus "{\"podMonitorNamespaceSelector\": $(OWN zz-dev), \"serviceMonitorSelector\": {\"matchLabels\": {\"experiment\": \"e1\"}}, \"podMonitorSelector\": {}}"
    set_world '.nss = {"team-a": {"kubernetes.io/metadata.name": "team-a", "tier": "gpu"},
                       "aaa-dev": {"kubernetes.io/metadata.name": "aaa-dev"},
                       "zz-dev": {"kubernetes.io/metadata.name": "zz-dev"}}'
}

shared_cluster
NAMESPACE_EXPLICIT=team-a
check 'shared cluster: a per-user Prometheus sorted first is passed over for one that selects every namespace' \
    "$(url llm-d-wva-monitoring)" "$(detect)"
lacks 'shared cluster: a covering Prometheus found is not a warning' 'WARNING' "$(detect_err)"
check 'shared cluster: own-namespace selectors do not select team-a; {} selects it; null selects only its own' \
    'aaa-dev=no llm-d-wva-monitoring=yes monitoring=yes zz-dev=no' "$(verdicts team-a)"
check 'own-namespace selector: selects its own namespace' "$(url aaa-dev)" "$(NAMESPACE_EXPLICIT=aaa-dev detect)"
check 'null serviceMonitorNamespaceSelector selects its own namespace, but a ServiceMonitor selector our monitors do not match does not cover it' \
    'aaa-dev=no llm-d-wva-monitoring=yes monitoring=yes zz-dev=no' "$(verdicts zz-dev)"
has 'non-matching monitor selector: the reason names it' 'its serviceMonitorSelector selects no ServiceMonitor in zz-dev' \
    "$(wva_prometheus_coverage zz-dev 2>/dev/null)"

# ----------------------------------------------------------------------------
# null against {}, per selector
# ----------------------------------------------------------------------------
world
cr mon a '{"podMonitorNamespaceSelector": null, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {}, "serviceMonitorSelector": {}}'
cr mon b "$ALL"
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}'
check 'null podMonitorNamespaceSelector: only its own namespace; {}: every namespace' 'mon=no mon=yes' "$(verdicts team-a)"
check 'null against {}: the {} one is picked' "$(url mon)" "$(detect)"
world
cr mon a '{"podMonitorNamespaceSelector": {}, "serviceMonitorNamespaceSelector": {}, "serviceMonitorSelector": {}}'
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}'
check 'null podMonitorSelector selects no PodMonitor' 'mon=no' "$(verdicts team-a)"
has 'null podMonitorSelector: the reason says so' 'its podMonitorSelector selects no PodMonitor in team-a' \
    "$(wva_prometheus_coverage team-a 2>/dev/null)"

# ----------------------------------------------------------------------------
# Both kinds must select it
# ----------------------------------------------------------------------------
world
cr mon half "{\"podMonitorNamespaceSelector\": {}, \"serviceMonitorNamespaceSelector\": $(OWN mon), \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}'
check 'PodMonitors from every namespace but ServiceMonitors from its own: not covering (the EPP is a ServiceMonitor)' \
    'mon=no' "$(verdicts team-a)"

# ----------------------------------------------------------------------------
# matchExpressions against the namespace labels
# ----------------------------------------------------------------------------
expr() { # expr <operator> <values json or null> <key>
    world
    cr mon p "{\"podMonitorNamespaceSelector\": {\"matchExpressions\": [{\"key\": \"$3\", \"operator\": \"$1\", \"values\": $2}]}, \"serviceMonitorNamespaceSelector\": {}, \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
    set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a", "tier": "gpu"}'
    verdicts team-a
}
check 'matchExpressions In, value present: selects' 'mon=yes' "$(expr In '["cpu", "gpu"]' tier)"
check 'matchExpressions In, value absent: does not select' 'mon=no' "$(expr In '["cpu"]' tier)"
check 'matchExpressions NotIn, value present: does not select' 'mon=no' "$(expr NotIn '["gpu"]' tier)"
check 'matchExpressions NotIn, key absent: selects' 'mon=yes' "$(expr NotIn '["gpu"]' zone)"
check 'matchExpressions Exists: selects' 'mon=yes' "$(expr Exists null tier)"
check 'matchExpressions DoesNotExist, key present: does not select' 'mon=no' "$(expr DoesNotExist null tier)"
check 'matchExpressions DoesNotExist, key absent: selects' 'mon=yes' "$(expr DoesNotExist null zone)"

# ----------------------------------------------------------------------------
# Monitor selectors against the monitors in the namespace
# ----------------------------------------------------------------------------
world
cr mon kps '{"podMonitorNamespaceSelector": {}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {"matchLabels": {"release": "kps"}}, "serviceMonitorSelector": {"matchLabels": {"release": "kps"}}}'
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}
           | .pms["team-a"] = [{"release": "kps", "app": "vllm"}] | .sms["team-a"] = [{"release": "kps"}]'
check 'monitor selector matching the monitors in the namespace: covering' 'mon=yes' "$(verdicts team-a)"
set_world '.sms["team-a"] = [{"app": "epp"}]'
check 'ServiceMonitor selector matching none of the ServiceMonitors there: not covering' 'mon=no' "$(verdicts team-a)"
set_world '.pms["team-a"] = [] | .sms["team-a"] = []'
check 'no monitors yet, a selector WVA own monitors do not carry: not covering' 'mon=no' "$(verdicts team-a)"
world
cr mon wva '{"podMonitorNamespaceSelector": {}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {"matchLabels": {"app.kubernetes.io/name": "workload-variant-autoscaler"}}, "serviceMonitorSelector": {"matchExpressions": [{"key": "llm-d.ai/applied-by", "operator": "Exists"}]}}'
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}'
check 'no monitors yet, a selector WVA own monitors carry: covering' 'mon=yes' "$(verdicts team-a)"

# ----------------------------------------------------------------------------
# Forbidden is not absent
# ----------------------------------------------------------------------------
world
cr aaa first "{\"podMonitorNamespaceSelector\": $(OWN aaa), \"serviceMonitorNamespaceSelector\": $(OWN aaa), \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
cr mon tiered '{"podMonitorNamespaceSelector": {"matchLabels": {"tier": "gpu"}}, "serviceMonitorNamespaceSelector": {"matchLabels": {"tier": "gpu"}}, "podMonitorSelector": {}, "serviceMonitorSelector": {}}'
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a", "tier": "gpu"} | .forbid = ["namespace"]'
check 'namespace labels Forbidden: a selector on the name is still decided; one on other labels is unknown' \
    'aaa=no mon=unknown' "$(verdicts team-a)"
check 'namespace labels Forbidden: the unchecked Prometheus is picked before one known not to scrape it' \
    "$(url mon)" "$(detect)"
err="$(detect_err)"
has 'namespace labels Forbidden: says it could not check' 'could not check (reading the labels of namespace team-a failed)' "$err"
has 'namespace labels Forbidden: tells how to choose' 'PROMETHEUS_URL=<url>' "$err"
set_world '.forbid = []'
check 'namespace labels readable: the same selector is decided' 'aaa=no mon=yes' "$(verdicts team-a)"

world
cr mon kps '{"podMonitorNamespaceSelector": {}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {"matchLabels": {"release": "kps"}}, "serviceMonitorSelector": {}}'
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"} | .forbid = ["podmonitors"]'
check 'PodMonitors Forbidden and a selector WVA own monitor does not carry: unknown, not no' 'mon=unknown' "$(verdicts team-a)"
has 'PodMonitors Forbidden: the reason says it could not list them' 'could not check (listing PodMonitors in team-a failed)' \
    "$(wva_prometheus_coverage team-a 2>/dev/null)"
set_world '.crs[0].spec.podMonitorSelector = {}'
check 'PodMonitors Forbidden and a {} selector: decided without them' 'mon=yes' "$(verdicts team-a)"

world
cr aaa first "$ALL"
cr mon second "$ALL"
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"} | .forbid = ["crjson"]'
check 'Prometheus specs unreadable: the first one listed, as before' "$(url aaa)" "$(detect)"
has 'Prometheus specs unreadable: says it could not check' 'Could not check which Prometheus scrapes team-a' "$(detect_err)"

# ----------------------------------------------------------------------------
# None known to cover: a warning, the candidates, and the fallback
# ----------------------------------------------------------------------------
world
cr aaa first "{\"podMonitorNamespaceSelector\": $(OWN aaa), \"serviceMonitorNamespaceSelector\": $(OWN aaa), \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
cr bbb second "{\"podMonitorNamespaceSelector\": $(OWN bbb), \"serviceMonitorNamespaceSelector\": $(OWN bbb), \"podMonitorSelector\": {}, \"serviceMonitorSelector\": {}}"
set_world '.nss["team-a"] = {"kubernetes.io/metadata.name": "team-a"}'
check 'none covering: still falls back to the first, rather than the shipped default' "$(url aaa)" "$(detect)"
err="$(detect_err)"
has 'none covering: warns' '[WARNING] No Prometheus is known to scrape team-a' "$err"
has 'none covering: names each candidate and why' 'bbb/second: its podMonitorNamespaceSelector does not select team-a' "$err"
has 'none covering: tells the reader to pass PROMETHEUS_URL' 'Pass PROMETHEUS_URL=<url> of the Prometheus that scrapes team-a' "$err"

# ----------------------------------------------------------------------------
# Several managed namespaces: every one must be selected
# ----------------------------------------------------------------------------
world
cr aaa only-a '{"podMonitorNamespaceSelector": {"matchExpressions": [{"key": "kubernetes.io/metadata.name", "operator": "In", "values": ["team-a"]}]}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {}, "serviceMonitorSelector": {}}'
cr mon both '{"podMonitorNamespaceSelector": {"matchExpressions": [{"key": "kubernetes.io/metadata.name", "operator": "In", "values": ["team-a", "team-b"]}]}, "serviceMonitorNamespaceSelector": {}, "podMonitorSelector": {}, "serviceMonitorSelector": {}}'
set_world '.nss = {"team-a": {"kubernetes.io/metadata.name": "team-a"}, "team-b": {"kubernetes.io/metadata.name": "team-b"}}'
check 'WVA_WATCH_NS list: the Prometheus that selects every listed namespace' "$(url mon)" \
    "$(NAMESPACE_EXPLICIT='' WVA_WATCH_NS=team-a,team-b detect)"

# ----------------------------------------------------------------------------
# No model namespace named, and the explicit override
# ----------------------------------------------------------------------------
shared_cluster
check 'no model namespace named: the first, as before' "$(url aaa-dev)" "$(NAMESPACE_EXPLICIT='' detect)"
has 'no model namespace named: a note says it was not checked' 'no model namespace named' "$(NAMESPACE_EXPLICIT='' detect_err)"
world
cr monitoring k8s "$ALL"
lacks 'no model namespace named, one Prometheus: no note' 'no model namespace named' "$(NAMESPACE_EXPLICIT='' detect_err)"

shared_cluster
check 'explicit PROMETHEUS_URL: the endpoint is the one given' 'https://prom.example.com:443' \
    "$( PROMETHEUS_URL_EXPLICIT='https://prom.example.com:443'; wva_prometheus_endpoint )"
check 'explicit PROMETHEUS_URL: resolution leaves it alone' 'https://prom.example.com:443' \
    "$( PROMETHEUS_URL_EXPLICIT=1 PROMETHEUS_URL='https://prom.example.com:443'
        wva_resolve_prometheus_url >/dev/null 2>&1; printf '%s' "$PROMETHEUS_URL" )"
check 'no explicit URL: resolution takes the covering one' "$(url llm-d-wva-monitoring)" \
    "$( PROMETHEUS_URL='https://kube-prometheus-stack-prometheus.default:9090'; WVA_PROMETHEUS_URL_RESOLVED=""
        wva_resolve_prometheus_url >/dev/null 2>&1; printf '%s' "$PROMETHEUS_URL" )"

printf '\n%d passed, %d failed (expected %d)\n' "$RAN" "$FAILED" "$EXPECT_CASES"
[ "$FAILED" -eq 0 ] || exit 1
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf 'FAIL: %d cases ran, expected %d -- a case was added or lost without updating EXPECT_CASES.\n' "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi

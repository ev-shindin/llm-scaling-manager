#!/usr/bin/env bash
#
# Executes the installer's external-scaler TLS code (deploy/lib/scaler_tls.sh
# and wva_verify_scaler_tls in deploy/lib/verify.sh) against a STUBBED kubectl
# that keeps objects in a directory, with the real openssl.
#
# What it pins, each of which parses cleanly and installs cleanly when wrong:
#
#   the default            TLS, once KEDA's side is written and no ScaledObject
#                          would be left behind; otherwise plaintext with a
#                          warning (a refusal when WVA_SCALER_TLS=true); a
#                          re-install keeps TLS on; hand-set TLS flags and an
#                          unreadable Deployment are refusals, not "off"
#   KEDA's side            the client Secret copied to KEDA's namespace, the
#                          ClusterTriggerAuthentication, the marker the controller
#                          phase reads; foreign objects refused; deferred until
#                          this install puts KEDA there
#   ScaledObjects          those naming the scaler on the plaintext address hold
#                          TLS back unless WVA_SCALER_TLS_MIGRATE=true, which
#                          moves them to the TLS address and authenticationRef,
#                          only once the new Pods alone serve the scaler, and
#                          then re-wires an HPA KEDA built without the metric
#   openssl certificates   the serving certificate names both Service hosts and
#                          chains to the CA; the client one carries the pinned
#                          identity; a re-run changes nothing that is still
#                          good, and re-issues a leaf that is expiring, names
#                          the wrong hosts, or outlived its CA
#   ownership              a Secret, Issuer or Certificate the installer did not
#                          create is never overwritten; Forbidden is a refusal
#   cert-manager           the Issuer chain and Certificates it applies, and
#                          refusals when the webhook or issuance fails
#   OpenShift              the serving certificate is the service CA's
#                          (annotation on the Service), never minted here
#   the Deployment         flags, mounts and the client-CA digest present
#                          exactly when TLS is on, in every shipped overlay
#   verify                 the reported mode, expiry, names, and ScaledObjects
#                          that would still dial plaintext
#
# Every case asserts something that a missing function also fails, so a run
# against code without this feature fails rather than passes empty.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }
ok() { if eval "$1"; then pass; else fail "$2"; fi; }

for tool in yq jq openssl; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"
        exit 1
    fi
done

# render <dir> <out>: kustomize build with whatever is available.
render() {
    if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
        "$KUSTOMIZE" build "$1" >"$2"
    elif command -v kustomize >/dev/null 2>&1; then
        command kustomize build "$1" >"$2"
    elif command -v kubectl >/dev/null 2>&1; then
        command kubectl kustomize "$1" >"$2"
    else
        echo "FAIL: neither kustomize nor kubectl is available to render overlays"
        exit 1
    fi
}

# Sourced through CR-stripped copies: a Windows checkout can write these as
# CRLF, and bash then stops at the first function definition. A file that does
# not exist yet (the code under test, on a tree without it) becomes an empty
# one, so every case fails on its missing function instead of the run aborting.
for f in constants common verify infra_wva scaler_tls scaledobject; do
    if [ -f "$ROOT/deploy/lib/$f.sh" ]; then
        tr -d '\r' <"$ROOT/deploy/lib/$f.sh" >"$WORK/$f.sh"
    else
        : >"$WORK/$f.sh"
    fi
done

NS=wva-check
STORE="$WORK/store"

# forbidden is kubectl's Forbidden text, naming the namespace STUB_FORBID_NS
# (default the controller's): a namespace called notfound-prod must not turn it
# into NotFound.
forbidden() { echo "Error from server (Forbidden): secrets is forbidden: User \"tenant\" cannot get resource \"secrets\" in API group \"\" in the namespace \"${STUB_FORBID_NS:-$NS}\"" >&2; }
notfound() { echo 'Error from server (NotFound): not found' >&2; }

# store_get <resource> <name>: the stored object, or NotFound.
store_get() {
    if [ -f "$STORE/$1__$2.json" ]; then cat "$STORE/$1__$2.json"; else notfound; return 1; fi
}

# store_put_secret <file>: a Secret JSON, as a write.
store_put_secret() {
    local name
    name="$(jq -r .metadata.name "$1")"
    cp "$1" "$STORE/secret__$name.json"
    echo "secret/$name" >>"$WORK/writes"
}

# store_put <file>: a server-side apply. Secrets in $NS are the certificate
# writes ($WORK/writes); everything of KEDA's side goes to $WORK/keda-writes.
store_put() {
    local kind name ns
    kind="$(jq -r .kind "$1")"
    name="$(jq -r .metadata.name "$1")"
    ns="$(jq -r '.metadata.namespace // ""' "$1")"
    case "$kind" in
        Secret)
            if [ -z "$ns" ] || [ "$ns" = "$NS" ]; then
                store_put_secret "$1"
            else
                cp "$1" "$STORE/secret@${ns}__$name.json"
                echo "secret@$ns/$name" >>"$WORK/keda-writes"
            fi ;;
        ConfigMap)
            cp "$1" "$STORE/configmap__$name.json"
            echo "configmap/$name" >>"$WORK/keda-writes" ;;
        ClusterTriggerAuthentication)
            cp "$1" "$STORE/clustertriggerauthentications.keda.sh__$name.json"
            echo "cta/$name" >>"$WORK/keda-writes" ;;
    esac
}

# cm_issue <manifest>: what cert-manager would do with the applied manifest --
# store every Issuer and Certificate, and write each Certificate's Secret with
# the secretTemplate labels and a stand-in CA.
cm_issue() {
    local doc kind name secret labels
    yq -o=json -I=0 '.' "$1" | while IFS= read -r doc; do
        [ -n "$doc" ] && [ "$doc" != null ] && [ "$doc" != --- ] || continue
        kind="$(printf '%s' "$doc" | jq -r .kind)"
        name="$(printf '%s' "$doc" | jq -r .metadata.name)"
        case "$kind" in
            Issuer) printf '%s' "$doc" >"$STORE/issuers.cert-manager.io__$name.json" ;;
            Certificate)
                printf '%s' "$doc" >"$STORE/certificates.cert-manager.io__$name.json"
                secret="$(printf '%s' "$doc" | jq -r .spec.secretName)"
                labels="$(printf '%s' "$doc" | jq -c '.spec.secretTemplate.labels // {}')"
                jq -n --arg n "$secret" --arg c "$name" --argjson l "$labels" \
                    --arg ca "$(printf 'FAKE-CERT-MANAGER-CA\n' | base64 | tr -d '\n')" \
                    '{apiVersion: "v1", kind: "Secret",
                      metadata: {name: $n, labels: $l, annotations: {"cert-manager.io/certificate-name": $c}},
                      data: {"tls.crt": $ca, "tls.key": $ca, "ca.crt": $ca}}' >"$STORE/secret__$secret.json"
                echo "cm-secret/$secret" >>"$WORK/writes"
                ;;
        esac
    done
}

# stub_wired succeeds when KEDA has wired the scaler's metric, per STUB_UNWIRED:
# unset (wired), always, hpa (the HPA lacks it, the status lists it), or
# until-rewire (wired once a re-wire patch was sent).
stub_wired() {
    case "${STUB_UNWIRED:-}" in
        "") return 0 ;;
        until-rewire) grep -q "wvaRewire" "$WORK/patches" ;;
        *) return 1 ;;
    esac
}
# stub_emn: the ScaledObject's status.externalMetricNames.
stub_emn() {
    if stub_wired || [ "${STUB_UNWIRED:-}" = hpa ]; then echo '["s0-wva-desired-replicas"]'; else echo '[]'; fi
}
# stub_hpa: the HPA KEDA built, with the external metric only when wired.
stub_hpa() {
    if stub_wired; then
        echo '{"spec":{"metrics":[{"type":"External","external":{"metric":{"name":"s0-wva-desired-replicas"}}}]}}'
    else
        echo '{"spec":{"metrics":[{"type":"Resource","resource":{"name":"cpu"}}]}}'
    fi
}
# stub_pods <name>...: Ready controller Pods; a name ending in "~" is terminating.
stub_pods() {
    printf '%s\n' "$@" | jq -R . | jq -s '{items: [.[] | if endswith("~")
        then {metadata: {name: rtrimstr("~"), deletionTimestamp: "2026-01-01T00:00:00Z"}, status: {conditions: [{type: "Ready", status: "False"}]}}
        else {metadata: {name: .}, status: {conditions: [{type: "Ready", status: "True"}]}} end]}'
}
# stub_eps: the scaler Service EndpointSlices. $WORK/eps.d holds one file per
# read, taken in order, the last one repeating; default: wva-new, ready.
stub_eps() {
    local f n
    if [ -d "$WORK/eps.d" ]; then
        f="$(ls "$WORK/eps.d" | sort -n | head -1)"
        n="$(ls "$WORK/eps.d" | wc -l)"
        cat "$WORK/eps.d/$f"
        [ "$n" -le 1 ] || rm -f "$WORK/eps.d/$f"
    else
        eps_json wva-new
    fi
}
# eps_json <name>...: one EndpointSlice; a name ending in "~" is terminating.
eps_json() {
    printf '%s\n' "$@" | jq -R . | jq -s '{items: [{endpoints: [.[] | if endswith("~")
        then {targetRef: {name: rtrimstr("~")}, conditions: {ready: false, serving: true, terminating: true}}
        else {targetRef: {name: .}, conditions: {ready: true}} end]}]}'
}

# stub_kubectl answers from $STORE and STUB_* variables:
#   STUB_CM            present | absent | anything else (discovery error)
#   STUB_DEPLOY        FORBIDDEN makes the controller Deployment unreadable
#   STUB_SECRETS       FORBIDDEN makes every Secret unreadable
#   STUB_DRYRUN        fail makes the server-side dry run fail (webhook down)
#   STUB_WAIT          fail makes the Certificate wait time out
#   STUB_SO            FORBIDDEN makes ScaledObjects unreadable; else $WORK/so.json
#   STUB_KEDA          present (default) | absent: KEDA's keda.sh/v1alpha1 API
#   STUB_CANI          no makes every `auth can-i` answer no
#   STUB_CANI_CTA      no makes only `auth can-i create clustertriggerauthentications` no
#   STUB_CANI_NO       an `auth can-i` containing this text answers no (e.g. "get certificates")
#   STUB_KEDA_OPERATOR FORBIDDEN makes KEDA's operator Deployment unreadable
#   STUB_SERVICE_CA    the OpenShift service CA ConfigMap content (default FAKE-SERVICE-CA)
#   STUB_CTA           FORBIDDEN makes the ClusterTriggerAuthentication unreadable
#   STUB_CONFIGMAPS    FORBIDDEN makes ConfigMaps in the controller namespace unreadable
#   STUB_FORBID_NS     the namespace a Forbidden message names
#   STUB_APISERVICE    "<namespace> <service>" behind the external-metrics APIService (else
#                      NotFound); FORBIDDEN makes it unreadable
#   STUB_NP            the namespace an existing wva-external-scaler NetworkPolicy admits (else
#                      NotFound); FORBIDDEN makes it unreadable
#   STUB_KEDA_DEPLOYS  FORBIDDEN makes the Deployments in KEDA's namespace unreadable
#   STUB_KEDA_OBJ_NS   KEDA_CLUSTER_OBJECT_NAMESPACE on KEDA's operator (else unset)
#   STUB_LOGS          FORBIDDEN makes the controller's log unreadable; else $WORK/ctrl.log
#   STUB_KEDA_PODS     FORBIDDEN makes the Pods in KEDA's namespace (keda) unreadable; else
#                      keda-operator-7f9c at 10.128.5.168 and keda-admission-5d at 10.128.5.170
# A file $STORE/deployment@<ns>__keda-operator.json is KEDA's operator in <ns>.
stub_kubectl() {
    printf '%s\n' "$*" >>"$WORK/kubectl.log"
    local a="$*" tmp name
    case "$a" in
        "get --raw /apis/cert-manager.io")
            case "${STUB_CM:-absent}" in
                present) echo '{"kind":"APIGroup","name":"cert-manager.io"}' ;;
                absent) echo 'Error from server (NotFound): the server could not find the requested resource' >&2; return 1 ;;
                *) echo 'Unable to connect to the server: i/o timeout' >&2; return 1 ;;
            esac
            ;;
        "get --raw /apis/keda.sh/v1alpha1")
            case "${STUB_KEDA:-present}" in
                present) echo '{"kind":"APIResourceList","resources":[{"name":"scaledobjects"},{"name":"clustertriggerauthentications"}]}' ;;
                *) echo 'Error from server (NotFound): the server could not find the requested resource' >&2; return 1 ;;
            esac
            ;;
        "auth can-i create clustertriggerauthentications.keda.sh"*)
            [ "${STUB_CANI_CTA:-yes}" = yes ] && [ "${STUB_CANI:-yes}" = yes ]
            ;;
        "auth can-i "*)
            if [ -n "${STUB_CANI_NO:-}" ]; then
                case "$a" in *"$STUB_CANI_NO"*) return 1 ;; esac
            fi
            [ "${STUB_CANI:-yes}" = yes ] ;;
        "get configmap openshift-service-ca.crt -n $NS -o jsonpath="*) printf '%s\n' "${STUB_SERVICE_CA:-FAKE-SERVICE-CA}" ;;
        "get clustertriggerauthentications.keda.sh "*" -o json")
            [ "${STUB_CTA:-}" != FORBIDDEN ] || { forbidden; return 1; }
            set -- $a
            store_get clustertriggerauthentications.keda.sh "$3"
            ;;
        "get secret "*" -n "*" -o json")
            set -- $a
            if [ "$5" != "$NS" ]; then
                store_get "secret@$5" "$3"
            else
                [ "${STUB_SECRETS:-}" != FORBIDDEN ] || { forbidden; return 1; }
                store_get secret "$3"
            fi
            ;;
        "get apiservice v1beta1.external.metrics.k8s.io "*)
            [ "${STUB_APISERVICE:-}" != FORBIDDEN ] || { forbidden; return 1; }
            if [ -n "${STUB_APISERVICE:-}" ]; then printf '%s' "$STUB_APISERVICE"; else notfound; return 1; fi
            ;;
        "get networkpolicy wva-external-scaler -n $NS "*)
            [ "${STUB_NP:-}" != FORBIDDEN ] || { forbidden; return 1; }
            if [ -n "${STUB_NP:-}" ]; then printf '9090 |%s \n' "$STUB_NP"; else notfound; return 1; fi
            ;;
        "get deployments -n "*" -o json")
            [ "${STUB_KEDA_DEPLOYS:-}" != FORBIDDEN ] || { forbidden; return 1; }
            jq -n --arg o "${STUB_KEDA_OBJ_NS:-}" 'if $o == "" then {items: []} else {items: [{spec: {template: {spec: {containers: [
                {name: "keda-operator", env: [{name: "KEDA_CLUSTER_OBJECT_NAMESPACE", value: $o}]}]}}}}]} end'
            ;;
        "get deployment keda-operator -n "*" -o name")
            [ "${STUB_KEDA_OPERATOR:-}" != FORBIDDEN ] || { forbidden; return 1; }
            set -- $a
            if [ -f "$STORE/deployment@${5}__keda-operator.json" ]; then echo deployment.apps/keda-operator; else notfound; return 1; fi
            ;;
        "delete clustertriggerauthentications.keda.sh "*)
            set -- $a
            rm -f "$STORE/clustertriggerauthentications.keda.sh__$3.json"
            ;;
        "delete configmap "*" -n $NS "*)
            set -- $a
            rm -f "$STORE/configmap__$3.json"
            ;;
        "delete secret "*" -n "*" --ignore-not-found")
            set -- $a
            if [ "$#" = 6 ] && [ "$4" = -n ] && [ "$5" != "$NS" ]; then rm -f "$STORE/secret@${5}__$3.json"; fi
            if [ "$#" = 6 ] && [ "$4" = -n ] && [ "$5" = "$NS" ]; then rm -f "$STORE/secret__$3.json"; fi
            ;;
        "get scaledobject "*" -n "*" -o json")
            set -- $a
            jq --arg n "$3" --arg ns "$5" --argjson w "$(stub_emn)" \
                '.items[] | select(.metadata.name == $n and .metadata.namespace == $ns) | .status.externalMetricNames //= $w' "$WORK/so.json"
            ;;
        "get hpa "*" -n "*" -o json")
            stub_hpa
            ;;
        "logs -n $NS -l "*)
            if [ "${STUB_LOGS:-}" = FORBIDDEN ]; then
                echo 'Error from server (Forbidden): pods "wva-new" is forbidden: User "tenant" cannot get resource "pods/log"' >&2
                return 1
            fi
            [ ! -f "$WORK/ctrl.log" ] || cat "$WORK/ctrl.log"
            ;;
        "get pods -n keda -o json")
            [ "${STUB_KEDA_PODS:-}" != FORBIDDEN ] || { forbidden; return 1; }
            jq -n '{items: [{metadata: {name: "keda-operator-7f9c"}, status: {podIP: "10.128.5.168", podIPs: [{ip: "10.128.5.168"}]}},
                            {metadata: {name: "keda-admission-5d"}, status: {podIP: "10.128.5.170"}}]}'
            ;;
        "get pods -n $NS -l "*" -o json")
            if [ -f "$WORK/pods.json" ]; then cat "$WORK/pods.json"; else stub_pods wva-new; fi
            ;;
        "get endpointslices -n $NS -l kubernetes.io/service-name=wva-external-scaler -o json")
            [ "${STUB_EPS:-}" != FORBIDDEN ] || { forbidden; return 1; }
            stub_eps
            ;;
        "get leases -n $NS -o json")
            [ "${STUB_LEASES:-}" != FORBIDDEN ] || { forbidden; return 1; }
            jq -n --arg h "${STUB_LEASE_HOLDER:-wva-new_0f1e}" '{items: [{metadata: {name: "72dd1cf1.llm-d.ai"}, spec: {holderIdentity: $h}}]}'
            ;;
        "patch scaledobject "*) printf '%s\n' "$a" >>"$WORK/patches" ;;
        "get deployment wva-controller-manager -n $NS -o json")
            [ "${STUB_DEPLOY:-}" != FORBIDDEN ] || { forbidden; return 1; }
            store_get deployment wva-controller-manager
            ;;
        "get scaledobjects.keda.sh "*)
            [ "${STUB_SO:-}" != FORBIDDEN ] || { forbidden; return 1; }
            if [ "${STUB_KEDA:-present}" != present ]; then
                echo 'error: the server doesn'"'"'t have a resource type "scaledobjects"' >&2
                return 1
            fi
            if [ -f "$WORK/so.json" ]; then cat "$WORK/so.json"; else echo '{"items":[]}'; fi
            ;;
        "get secret "*" -n $NS -o jsonpath="*)
            [ "${STUB_SECRETS:-}" != FORBIDDEN ] || { forbidden; return 1; }
            set -- $a
            store_get secret "$3" | jq -r '.data["tls.crt"] // ""'
            ;;
        "get "*" -n $NS -o json")
            set -- $a
            if [ "$2" = secret ] && [ "${STUB_SECRETS:-}" = FORBIDDEN ]; then forbidden; return 1; fi
            if [ "$2" = configmap ] && [ "${STUB_CONFIGMAPS:-}" = FORBIDDEN ]; then forbidden; return 1; fi
            store_get "$2" "$3"
            ;;
        "apply --server-side "*)
            tmp="$WORK/apply.json"
            cat >"$tmp"
            store_put "$tmp"
            ;;
        "apply --dry-run=server -f "*)
            [ "${STUB_DRYRUN:-ok}" = ok ] || { echo 'Error from server (InternalError): failed calling webhook "webhook.cert-manager.io": connection refused' >&2; return 1; }
            ;;
        "apply -f "*)
            cp "${a#apply -f }" "$WORK/cm-applied.yaml"
            cm_issue "$WORK/cm-applied.yaml"
            ;;
        "wait "*) [ "${STUB_WAIT:-ok}" = ok ] || return 1 ;;
        *) return 0 ;;
    esac
}

# run <function call> [VAR=value...]: runs it in a subshell with the libraries
# sourced and kubectl stubbed. Refusal text lands in $WORK/refusal, warnings in
# $WORK/warnings, info and success lines in $WORK/info; the variables the code
# sets are read back from $WORK/vars. Sets RC.
run() {
    local call="$1"
    shift
    : >"$WORK/kubectl.log"
    : >"$WORK/warnings"
    : >"$WORK/info"
    : >"$WORK/refusal"
    : >"$WORK/writes"
    : >"$WORK/keda-writes"
    : >"$WORK/patches"
    : >"$WORK/vars"
    : >"$WORK/stdout"
    : >"$WORK/stderr"
    rm -f "$WORK/cm-applied.yaml"
    (
        # shellcheck disable=SC1091
        . "$WORK/constants.sh"
        # shellcheck disable=SC1091
        . "$WORK/common.sh"
        # shellcheck disable=SC1091
        . "$WORK/verify.sh"
        # shellcheck disable=SC1091
        . "$WORK/infra_wva.sh"
        # shellcheck disable=SC1091
        . "$WORK/scaler_tls.sh"
        log_info() { echo "$1" >>"$WORK/info"; }
        log_success() { echo "$1" >>"$WORK/info"; }
        log_warning() { echo "$1" >>"$WORK/warnings"; }
        log_error() { echo "$1" >"$WORK/refusal"; exit 1; }
        kubectl() { stub_kubectl "$@"; }
        WVA_NS=$NS
        ENVIRONMENT=kubernetes
        WVA_SCOPE=namespace
        KEDA_NAMESPACE_EXPLICIT=true
        KEDA_NAMESPACE=keda
        # What the previous run read back must not leak into this one.
        unset WVA_SCALER_TLS_ON WVA_SCALER_TLS_PROVIDER_RESOLVED WVA_SCALER_CLIENT_CA_HASH WVA_SCALER_TLS_WANTED \
            WVA_SCALER_TLS_MIGRATE_ROWS WVA_SCALER_KEDA_DEFERRED WVA_SCALER_TLS_MIGRATE_MODE
        # No waiting in a check: each wait reads once, unless a case says otherwise.
        WVA_SCALER_SETTLE_TIMEOUT=0
        WVA_SCALER_REWIRE_WAIT=0
        WVA_SCALER_POLL_SECONDS=0
        WVA_VERIFY_WIRE_WAIT=0
        local kv
        for kv in "$@"; do eval "$kv"; done
        declare -F "${call%% *}" >/dev/null || { echo "${call%% *} is not defined" >"$WORK/refusal"; exit 2; }
        eval "$call" >"$WORK/stdout" 2>>"$WORK/stderr"
        local st=$?
        # Plain assignments, not `declare -p`: sourced inside run, a declare
        # would make them local to it.
        printf 'WVA_SCALER_TLS_ON=%q\nWVA_SCALER_TLS_PROVIDER_RESOLVED=%q\nWVA_SCALER_CLIENT_CA_HASH=%q\nWVA_SCALER_TLS_WANTED=%q\nWVA_SCALER_TLS_MIGRATE_ROWS=%q\nWVA_SCALER_KEDA_DEFERRED=%q\nWVA_SCALER_TLS_MIGRATE_MODE=%q\n' \
            "${WVA_SCALER_TLS_ON:-}" "${WVA_SCALER_TLS_PROVIDER_RESOLVED:-}" "${WVA_SCALER_CLIENT_CA_HASH:-}" \
            "${WVA_SCALER_TLS_WANTED:-}" "${WVA_SCALER_TLS_MIGRATE_ROWS:-}" "${WVA_SCALER_KEDA_DEFERRED:-}" \
            "${WVA_SCALER_TLS_MIGRATE_MODE:-}" >"$WORK/vars"
        exit "$st"
    )
    RC=$?
    WVA_SCALER_TLS_ON="" WVA_SCALER_TLS_PROVIDER_RESOLVED="" WVA_SCALER_CLIENT_CA_HASH=""
    WVA_SCALER_TLS_WANTED="" WVA_SCALER_TLS_MIGRATE_ROWS="" WVA_SCALER_KEDA_DEFERRED=""
    WVA_SCALER_TLS_MIGRATE_MODE=""
    # shellcheck disable=SC1091
    . "$WORK/vars" 2>/dev/null || true
}

reset_store() { rm -rf "$STORE" "$WORK/eps.d"; mkdir -p "$STORE"; rm -f "$WORK/so.json" "$WORK/pods.json" "$WORK/ctrl.log"; }

# secret_file <name> <key> <out>: a stored Secret key, decoded.
secret_file() {
    jq -r --arg k "$2" '.data[$k] // ""' "$STORE/secret__$1.json" 2>/dev/null | base64 -d >"$3" 2>/dev/null || : >"$3"
}
sans_of() { openssl x509 -in "$1" -noout -text 2>/dev/null | awk '/Subject Alternative Name/ { getline; print }' | tr ',' '\n' | sed -n 's/^ *DNS://p' | sort -u | tr '\n' ' '; }
label_of() { jq -r '.metadata.labels["wva.llmd.ai/scaler-tls"] // ""' "$STORE/secret__$1.json" 2>/dev/null; }
# yqs is yq over a multi-document file, without the --- it prints between results.
yqs() { yq "$@" | grep -vx -- ---; }
sha_of() { openssl dgst -sha256 "$1" | awk '{ print $NF }'; }

# put_deploy <provider|plaintext|manual>: a controller Deployment in the store.
put_deploy() {
    local args='["--leader-elect=true"]' ann='{}'
    case "$1" in
        plaintext) ;;
        manual) args='["--leader-elect=true","--external-scaler-cert-path=/mine"]' ;;
        *) args='["--leader-elect=true","--external-scaler-cert-path=/etc/wva/scaler-tls"]'
           ann="{\"wva.llmd.ai/scaler-tls\":\"$1\"}" ;;
    esac
    jq -n --argjson args "$args" --argjson ann "$ann" \
        '{kind: "Deployment", metadata: {name: "wva-controller-manager"},
          spec: {template: {metadata: {annotations: $ann},
                            spec: {containers: [{name: "manager", args: $args}]}}}}' \
        >"$STORE/deployment__wva-controller-manager.json"
}

# put_foreign_secret <name> [annotations-json]: a Secret this installer did not write.
put_foreign_secret() {
    local ann="${2:-}"
    [ -n "$ann" ] || ann='{}'
    jq -n --arg n "$1" --argjson a "$ann" \
        '{kind: "Secret", metadata: {name: $n, annotations: $a}, data: {"tls.crt": "Zm9yZWlnbgo="}}' \
        >"$STORE/secret__$1.json"
}

SERVING=wva-external-scaler-tls
CLIENT=wva-external-scaler-client-tls
CA=wva-external-scaler-ca
WANT_SANS="wva-external-scaler.$NS.svc wva-external-scaler.$NS.svc.cluster.local "

# ---------------------------------------------------------------- the default
echo "== resolve: a new install wants TLS (the default)"
reset_store
run wva_resolve_scaler_tls
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_WANTED" = true ] && [ "$WVA_SCALER_TLS_ON" = false ]' "new install: exit $RC, wanted=$WVA_SCALER_TLS_WANTED on=$WVA_SCALER_TLS_ON (want 0, true, false until prepare): $(cat "$WORK/refusal")"

echo "== resolve: a re-install keeps TLS on, and its provider"
reset_store
put_deploy openssl
run wva_resolve_scaler_tls STUB_CM=present
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_WANTED" = true ]' "re-install: wanted=$WVA_SCALER_TLS_WANTED, want true: $(cat "$WORK/refusal")"
ok '[ "$WVA_SCALER_TLS_PROVIDER_RESOLVED" = openssl ]' "re-install: provider $WVA_SCALER_TLS_PROVIDER_RESOLVED, want the live openssl even with cert-manager now present"

echo "== resolve: WVA_SCALER_TLS=false turns a live TLS install off"
run wva_resolve_scaler_tls WVA_SCALER_TLS=false
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_WANTED" = false ]' "explicit false: wanted=$WVA_SCALER_TLS_WANTED"

echo "== resolve: an unreadable Deployment is a refusal, not plaintext"
run wva_resolve_scaler_tls STUB_DEPLOY=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "WVA_SCALER_TLS=true or WVA_SCALER_TLS=false" "$WORK/refusal"' "Forbidden Deployment: exit $RC, $(cat "$WORK/refusal")"

echo "== resolve: TLS flags set by hand are a refusal, not dropped"
reset_store
put_deploy manual
run wva_resolve_scaler_tls
ok '[ "$RC" = 1 ] && grep -q "did not set" "$WORK/refusal"' "hand-set flags: exit $RC, $(cat "$WORK/refusal")"

echo "== resolve: a bogus value refuses"
reset_store
run wva_resolve_scaler_tls WVA_SCALER_TLS=yes
ok '[ "$RC" = 1 ]' "WVA_SCALER_TLS=yes: exit $RC"

echo "== resolve: auto picks cert-manager when installed, openssl when not, refuses when unknown"
run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=present
ok '[ "$WVA_SCALER_TLS_PROVIDER_RESOLVED" = cert-manager ]' "cert-manager present: got '$WVA_SCALER_TLS_PROVIDER_RESOLVED'"
run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$WVA_SCALER_TLS_PROVIDER_RESOLVED" = openssl ]' "cert-manager absent: got '$WVA_SCALER_TLS_PROVIDER_RESOLVED'"
run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=error
ok '[ "$RC" = 1 ] && grep -q WVA_SCALER_TLS_PROVIDER "$WORK/refusal"' "discovery error: exit $RC (want a refusal naming WVA_SCALER_TLS_PROVIDER)"

# ---------------------------------------------------------------- openssl
echo "== openssl: a fresh install writes the CA, serving and client Secrets"
reset_store
mkdir -p "$WORK/k"
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 0 ]' "fresh: exit $RC: $(cat "$WORK/refusal")"
ok '[ "$(label_of $CA)/$(label_of $SERVING)/$(label_of $CLIENT)" = ca/serving/client ]' "fresh: Secret labels $(label_of $CA)/$(label_of $SERVING)/$(label_of $CLIENT), want ca/serving/client"
secret_file "$CA" tls.crt "$WORK/ca.pem"
secret_file "$SERVING" tls.crt "$WORK/serving.pem"
secret_file "$CLIENT" tls.crt "$WORK/client.pem"
secret_file "$CLIENT" ca.crt "$WORK/client-ca.pem"
ok '[ "$(sans_of "$WORK/serving.pem")" = "$WANT_SANS" ]' "serving SANs '$(sans_of "$WORK/serving.pem")', want '$WANT_SANS'"
ok 'openssl verify -CAfile "$WORK/ca.pem" "$WORK/serving.pem" >/dev/null 2>&1' "serving does not chain to the CA"
ok 'openssl x509 -in "$WORK/serving.pem" -noout -text | grep -q "TLS Web Server Authentication"' "serving lacks the serverAuth usage"
ok '[ "$(sans_of "$WORK/client.pem")" = "wva-keda-client " ]' "client SANs '$(sans_of "$WORK/client.pem")', want 'wva-keda-client'"
ok 'openssl x509 -in "$WORK/client.pem" -noout -text | grep -q "TLS Web Client Authentication"' "client lacks the clientAuth usage"
ok 'openssl verify -CAfile "$WORK/ca.pem" "$WORK/client.pem" >/dev/null 2>&1' "client does not chain to the CA"
ok 'cmp -s "$WORK/client-ca.pem" "$WORK/ca.pem"' "the client Secret's ca.crt (the controller's client CA) is not the CA"
ok '[ -n "$WVA_SCALER_CLIENT_CA_HASH" ] && [ "$WVA_SCALER_CLIENT_CA_HASH" = "$(sha_of "$WORK/ca.pem")" ]' "client CA digest '$WVA_SCALER_CLIENT_CA_HASH' is not the CA's"
ok 'grep -q -- "--server-side" "$WORK/kubectl.log" && ! grep -q "^apply -f -" "$WORK/kubectl.log"' "Secrets not written server-side (a client-side apply copies the key into an annotation)"
cp "$WORK/k/kustomization.yaml" "$WORK/kustomization.tls"
FIRST_HASH="$WVA_SCALER_CLIENT_CA_HASH"
cp "$WORK/ca.pem" "$WORK/ca.first"

echo "== openssl: a re-run with nothing to renew writes nothing"
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
put_deploy openssl
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/writes" ]' "unchanged re-run: exit $RC, wrote: $(tr '\n' ' ' <"$WORK/writes")"
ok '[ "$WVA_SCALER_CLIENT_CA_HASH" = "$FIRST_HASH" ]' "unchanged re-run moved the client CA digest (the controller would restart for nothing)"
ok 'grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "kept-on re-run did not render the TLS flags"

# reissue_case <label> <mutation>: mutates the stored serving cert, re-runs,
# and asserts the serving leaf alone was re-issued, CA and digest kept.
reissue_case() {
    echo "== openssl: re-issues a serving certificate that $1"
    eval "$2"
    printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
    secret_file "$SERVING" tls.crt "$WORK/serving.new"
    ok '[ "$RC" = 0 ] && [ "$(tr "\n" " " <"$WORK/writes")" = "secret/$SERVING " ]' "$1: exit $RC, wrote '$(tr '\n' ' ' <"$WORK/writes")', want the serving Secret alone"
    ok '[ "$(sans_of "$WORK/serving.new")" = "$WANT_SANS" ] && openssl x509 -in "$WORK/serving.new" -noout -checkend $((300 * 86400)) >/dev/null 2>&1' "$1: the new serving certificate is not a full-length one naming both hosts"
    ok '[ "$WVA_SCALER_CLIENT_CA_HASH" = "$FIRST_HASH" ]' "$1: the client CA changed, which restarts the controller for a leaf it hot-reloads"
}

# mint_serving <days> <names...>: a serving cert signed by the stored CA.
mint_serving() {
    local days="$1" san="" n
    shift
    for n in "$@"; do san="${san:+$san,}DNS:$n"; done
    secret_file "$CA" tls.crt "$WORK/m-ca.crt"
    secret_file "$CA" tls.key "$WORK/m-ca.key"
    printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$san" >"$WORK/m.ext"
    openssl req -new -newkey rsa:2048 -nodes -subj /CN=x -keyout "$WORK/m.key" -out "$WORK/m.csr" >/dev/null 2>&1
    openssl x509 -req -in "$WORK/m.csr" -CA "$WORK/m-ca.crt" -CAkey "$WORK/m-ca.key" -set_serial 7 \
        -days "$days" -extfile "$WORK/m.ext" -out "$WORK/m.crt" >/dev/null 2>&1
    jq --arg c "$(base64 <"$WORK/m.crt" | tr -d '\n')" '.data["tls.crt"] = $c' "$STORE/secret__$SERVING.json" >"$WORK/m.json"
    mv "$WORK/m.json" "$STORE/secret__$SERVING.json"
}
reissue_case "expires within 30 days" "mint_serving 10 wva-external-scaler.$NS.svc wva-external-scaler.$NS.svc.cluster.local"
reissue_case "does not name svc.cluster.local" "mint_serving 365 wva-external-scaler.$NS.svc"

echo "== openssl: a missing CA re-issues everything, and moves the digest"
rm -f "$STORE/secret__$CA.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
secret_file "$CA" tls.crt "$WORK/ca.new"
secret_file "$SERVING" tls.crt "$WORK/serving.new"
ok '[ "$RC" = 0 ] && [ "$(sort "$WORK/writes" | tr "\n" " ")" = "secret/$CA secret/$CLIENT secret/$SERVING " ]' "new CA: wrote '$(sort "$WORK/writes" | tr '\n' ' ')', want all three"
ok 'openssl verify -CAfile "$WORK/ca.new" "$WORK/serving.new" >/dev/null 2>&1' "new CA: serving not re-signed by it"
ok '[ "$WVA_SCALER_CLIENT_CA_HASH" = "$(sha_of "$WORK/ca.new")" ] && [ "$WVA_SCALER_CLIENT_CA_HASH" != "$FIRST_HASH" ]' "new CA: the digest did not move, so the controller keeps the old client CA"

echo "== openssl: a Secret this installer did not write is never overwritten"
reset_store
put_foreign_secret "$SERVING"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "not created by this installer" "$WORK/refusal"' "foreign Secret: exit $RC, $(cat "$WORK/refusal")"
ok '[ ! -s "$WORK/writes" ] && [ "$(jq -r .data[\"tls.crt\"] "$STORE/secret__$SERVING.json")" = Zm9yZWlnbgo= ]' "foreign Secret: something was written ($(tr '\n' ' ' <"$WORK/writes"))"

echo "== openssl: an unreadable Secret is a refusal, not absent"
reset_store
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_SECRETS=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "Cannot read" "$WORK/refusal" && [ ! -s "$WORK/writes" ]' "Forbidden Secret: exit $RC, $(cat "$WORK/refusal")"

echo "== openssl: refuses clearly when openssl is missing"
reset_store
mkdir -p "$WORK/noopenssl"
for t in jq grep head sed tr awk cat mktemp rm sort; do ln -sf "$(command -v "$t")" "$WORK/noopenssl/$t"; done
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent "PATH=$WORK/noopenssl"
ok '[ "$RC" = 1 ] && grep -q "openssl" "$WORK/refusal" && [ ! -s "$WORK/writes" ]' "no openssl: exit $RC, $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- OpenShift
echo "== OpenShift: the service CA issues the serving certificate; the installer does not"
reset_store
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
put_foreign_secret "$SERVING" '{"service.beta.openshift.io/originating-service-name":"wva-external-scaler"}'
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent ENVIRONMENT=openshift
ok '[ "$RC" = 0 ] && [ "$(sort "$WORK/writes" | tr "\n" " ")" = "secret/$CA secret/$CLIENT " ]' "OpenShift: exit $RC, wrote '$(sort "$WORK/writes" | tr '\n' ' ')', want the CA and client only: $(cat "$WORK/refusal")"
ok 'grep -q "service.beta.openshift.io/serving-cert-secret-name: $SERVING" "$WORK/k/kustomization.yaml"' "OpenShift: no serving-cert annotation on the scaler Service"
cp "$WORK/k/kustomization.yaml" "$WORK/kustomization.openshift"

echo "== OpenShift: a serving Secret neither the service CA nor the installer wrote is refused"
reset_store
put_foreign_secret "$SERVING"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent ENVIRONMENT=openshift
ok '[ "$RC" = 1 ] && grep -q "service CA" "$WORK/refusal" && [ ! -s "$WORK/writes" ]' "OpenShift foreign Secret: exit $RC, $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- cert-manager
echo "== cert-manager: Issuer chain and Certificates, labelled, waited for"
reset_store
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present
ok '[ "$RC" = 0 ] && [ -f "$WORK/cm-applied.yaml" ]' "cert-manager: exit $RC, nothing applied: $(cat "$WORK/refusal")"
CMA="$WORK/cm-applied.yaml"
ok '[ "$(yqs "select(.kind == \"Issuer\") | .metadata.name" "$CMA" | sort | tr "\n" " ")" = "wva-external-scaler-ca wva-external-scaler-selfsigned " ]' "cert-manager: Issuers $(yqs 'select(.kind == "Issuer") | .metadata.name' "$CMA" 2>/dev/null | tr '\n' ' ')"
ok '[ "$(yqs "select(.kind == \"Certificate\") | .metadata.name" "$CMA" | sort | tr "\n" " ")" = "wva-external-scaler wva-external-scaler-ca wva-external-scaler-client " ]' "cert-manager: Certificates $(yqs 'select(.kind == "Certificate") | .metadata.name' "$CMA" 2>/dev/null | tr '\n' ' ')"
ok '[ "$(yqs "select(.metadata.labels[\"wva.llmd.ai/scaler-tls\"] == \"cert-manager\") | .metadata.name" "$CMA" | grep -c .)" = 5 ]' "cert-manager: not every object carries the ownership label"
ok '[ "$(yqs "select(.kind == \"Certificate\" and .spec.secretTemplate.labels[\"wva.llmd.ai/scaler-tls\"] == \"cert-manager\") | .metadata.name" "$CMA" | grep -c .)" = 3 ]' "cert-manager: a Certificate does not stamp the label on its Secret"
ok '[ "$(yqs "select(.metadata.name == \"wva-external-scaler\") | .spec.dnsNames | sort | join(\" \")" "$CMA") " = "$WANT_SANS" ]' "cert-manager: serving dnsNames are not both Service hosts"
ok 'yqs "select(.metadata.name == \"wva-external-scaler-client\") | .spec.usages[]" "$CMA" | grep -qx "client auth"' "cert-manager: the client Certificate lacks client auth"
ok 'yqs "select(.metadata.name == \"wva-external-scaler-ca\") | .spec.isCA" "$CMA" | grep -qx true' "cert-manager: the CA Certificate is not isCA"
ok '[ "$(grep -c "^wait --for=condition=Ready certificates.cert-manager.io/" "$WORK/kubectl.log")" = 3 ]' "cert-manager: did not wait for each of the 3 Certificates"
printf 'FAKE-CERT-MANAGER-CA\n' >"$WORK/cmca"
ok '[ "$WVA_SCALER_CLIENT_CA_HASH" = "$(sha_of "$WORK/cmca")" ]' "cert-manager: the digest is not the issued client CA's"
ok '! grep -q "^secret/" "$WORK/writes"' "cert-manager: the installer wrote certificate Secrets itself: $(tr '\n' ' ' <"$WORK/writes")"
ok 'grep -q "value: cert-manager" "$WORK/k/kustomization.yaml"' "cert-manager: the provider is not recorded on the pod template"

echo "== cert-manager: a re-run accepts its own objects"
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present
ok '[ "$RC" = 0 ]' "cert-manager re-run: exit $RC, $(cat "$WORK/refusal")"

echo "== cert-manager: a Certificate it did not create is refused"
reset_store
jq -n '{kind: "Certificate", metadata: {name: "wva-external-scaler"}}' >"$STORE/certificates.cert-manager.io__wva-external-scaler.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present
ok '[ "$RC" = 1 ] && grep -q "not created by this installer" "$WORK/refusal" && [ ! -f "$WORK/cm-applied.yaml" ]' "foreign Certificate: exit $RC, $(cat "$WORK/refusal")"

echo "== cert-manager: a webhook that does not answer is a refusal before anything is written"
reset_store
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present STUB_DRYRUN=fail
ok '[ "$RC" = 1 ] && grep -q "WVA_SCALER_TLS_PROVIDER=openssl" "$WORK/refusal" && [ ! -f "$WORK/cm-applied.yaml" ]' "dry-run failure: exit $RC, $(cat "$WORK/refusal")"

echo "== cert-manager: a Certificate that never becomes Ready is a refusal"
reset_store
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present STUB_WAIT=fail
ok '[ "$RC" = 1 ] && grep -q "not Ready" "$WORK/refusal"' "wait failure: exit $RC, $(cat "$WORK/refusal")"

echo "== cert-manager on OpenShift: no serving Certificate"
reset_store
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=present ENVIRONMENT=openshift
ok '[ "$RC" = 0 ] && ! yqs "select(.kind == \"Certificate\") | .metadata.name" "$WORK/cm-applied.yaml" | grep -qx wva-external-scaler' "cert-manager on OpenShift: exit $RC, or a serving Certificate competes with the service CA"

# ---------------------------------------------------------------- the Deployment
# render_case <label> <overlay> <kustomization patches file or ""> <want on|off>
render_case() {
    local label="$1" overlay="$2" patches="$3" want="$4" dir="$WORK/render" out="$WORK/rendered.yaml" dep="$WORK/dep.yaml"
    echo "== render: $label"
    rm -rf "$dir"
    mkdir -p "$dir"
    ln -s "$ROOT/$overlay" "$dir/base"
    if [ -n "$patches" ]; then cp "$patches" "$dir/kustomization.yaml"; else printf 'namespace: %s\nresources:\n- ./base\n' "$NS" >"$dir/kustomization.yaml"; fi
    if ! render "$dir" "$out" 2>"$WORK/err"; then
        fail "$label: does not render: $(head -3 "$WORK/err")"
        return
    fi
    yq 'select(.kind == "Deployment" and .metadata.name == "wva-controller-manager")' "$out" >"$dep"
    local flags mounts vols ann
    flags="$(yq '.spec.template.spec.containers[] | select(.name == "manager") | .args[] | select(test("^--external-scaler-(cert-path|client-ca|client-san)="))' "$dep" | sort | tr '\n' ' ')"
    mounts="$(yq '.spec.template.spec.containers[] | select(.name == "manager") | .volumeMounts[] | select(.name == "scaler-tls" or .name == "scaler-client-ca") | .mountPath' "$dep" | sort | tr '\n' ' ')"
    vols="$(yq '.spec.template.spec.volumes[] | select(.name == "scaler-tls" or .name == "scaler-client-ca") | .secret.secretName' "$dep" | sort | tr '\n' ' ')"
    ann="$(yq '.spec.template.metadata.annotations["wva.llmd.ai/scaler-client-ca-sha256"] // ""' "$dep")"
    if [ "$want" = on ]; then
        ok '[ "$flags" = "--external-scaler-cert-path=/etc/wva/scaler-tls --external-scaler-client-ca=/etc/wva/scaler-client-ca/ca.crt --external-scaler-client-san=wva-keda-client " ]' "$label: flags '$flags'"
        ok '[ "$mounts" = "/etc/wva/scaler-client-ca /etc/wva/scaler-tls " ]' "$label: mounts '$mounts'"
        ok '[ "$vols" = "$CLIENT $SERVING " ]' "$label: volumes '$vols'"
        ok '[ "$(yq ".spec.template.spec.volumes[] | select(.name == \"scaler-client-ca\") | .secret.items[].key" "$dep" | tr "\n" " ")" = "ca.crt " ]' "$label: the controller mounts more of the client Secret than its CA (the client key belongs to KEDA)"
        ok '[ -n "$ann" ]' "$label: no client-CA digest on the pod template, so a new CA never restarts the controller"
    else
        ok '[ -z "$flags$mounts$vols$ann" ]' "$label: TLS is off but rendered: flags '$flags' mounts '$mounts' volumes '$vols' digest '$ann'"
    fi
    local svc_ann
    svc_ann="$(yq 'select(.kind == "Service" and .metadata.name == "wva-external-scaler") | .metadata.annotations["service.beta.openshift.io/serving-cert-secret-name"] // ""' "$out" \
        | grep -vx -e '' -e '---' || true)"
    # (yq 4.44 prints blank lines and --- for the documents a select drops.)
    case "$label" in
        *OpenShift*on*) ok '[ "$svc_ann" = "$SERVING" ]' "$label: scaler Service annotation '$svc_ann', want $SERVING" ;;
        *) ok '[ -z "$svc_ann" ]' "$label: the scaler Service asks the service CA for a certificate ('$svc_ann')" ;;
    esac
}
for overlay in config/overlays/cluster-scoped/kubernetes config/overlays/namespace-scoped/kubernetes \
    config/overlays/cluster-scoped/openshift config/overlays/namespace-scoped/openshift; do
    render_case "$overlay, TLS off" "$overlay" "" off
    case "$overlay" in
        */openshift) render_case "$overlay, OpenShift TLS on" "$overlay" "$WORK/kustomization.openshift" on ;;
        *) render_case "$overlay, TLS on" "$overlay" "$WORK/kustomization.tls" on ;;
    esac
done

# ---------------------------------------------------------------- verify
# Store the Deployment as the TLS render produced it, and the Secrets it names.
echo "== verify: plaintext is reported as plaintext"
reset_store
put_deploy plaintext
run wva_verify_scaler_tls
ok '[ "$RC" = 0 ] && grep -q "plaintext" "$WORK/info" && [ ! -s "$WORK/warnings" ]' "verify plaintext: exit $RC, info '$(head -2 "$WORK/info")'"

echo "== verify: mutual TLS, expiry, names; a ScaledObject with no authenticationRef is named"
reset_store
printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
rm -rf "$WORK/render"; mkdir -p "$WORK/render"
ln -s "$ROOT/config/overlays/namespace-scoped/kubernetes" "$WORK/render/base"
cp "$WORK/k/kustomization.yaml" "$WORK/render/kustomization.yaml"
if render "$WORK/render" "$WORK/rendered.yaml" 2>"$WORK/err"; then
    yq -o=json 'select(.kind == "Deployment" and .metadata.name == "wva-controller-manager")' "$WORK/rendered.yaml" \
        >"$STORE/deployment__wva-controller-manager.json"
else
    fail "verify: the TLS overlay does not render: $(head -3 "$WORK/err")"
fi
jq -n --arg p "wva-external-scaler.$NS.svc.cluster.local:9090" --arg t "wva-external-scaler.$NS.svc:9090" --arg c "wva-external-scaler-$NS" '{items: [
    {metadata: {namespace: "wva-check", name: "plain"}, spec: {triggers: [{type: "external-push", metadata: {scalerAddress: $p}}]}},
    {metadata: {namespace: "wva-check", name: "wrongref"}, spec: {triggers: [{type: "external-push", metadata: {scalerAddress: $t}, authenticationRef: {name: "x"}}]}},
    {metadata: {namespace: "wva-check", name: "authed"}, spec: {triggers: [{type: "external-push", metadata: {scalerAddress: $t}, authenticationRef: {name: $c, kind: "ClusterTriggerAuthentication"}}]}},
    {metadata: {namespace: "wva-check", name: "other"}, spec: {triggers: [{type: "external-push", metadata: {scalerAddress: "some-other-scaler:9090"}}]}}]}' >"$WORK/so.json"
run wva_verify_scaler_tls
ok '[ "$RC" = 0 ] && grep -q "mutual TLS, client identity wva-keda-client" "$WORK/info"' "verify mTLS: mode not reported: $(head -3 "$WORK/info")"
ok 'grep -q "expires" "$WORK/info" && grep -q "wva-external-scaler.$NS.svc.cluster.local" "$WORK/info"' "verify mTLS: expiry or names not reported: $(grep serving "$WORK/info")"
ok 'grep -q "on the plaintext address or without authenticationRef" "$WORK/warnings" && grep -q "wva-check/plain" "$WORK/stderr" && grep -q "wva-check/wrongref" "$WORK/stderr" && ! grep -q "wva-check/authed\|wva-check/other" "$WORK/stderr"' "verify mTLS: plain and wrongref, and only they, should be named: $(cat "$WORK/warnings" "$WORK/stderr" | head -4)"
# On a real cluster such a ScaledObject was Ready and scaled, riding the
# connection an authenticated one opened: "not-Ready" was wrong. The warning
# says it depends on order, what a KEDA restart can do, and the fix.
ok 'grep -q "depends on order" "$WORK/warnings" && grep -q "After a KEDA operator restart" "$WORK/warnings" && grep -q "EVERY ScaledObject on that address" "$WORK/warnings" && ! grep -q "not-Ready" "$WORK/warnings"' "verify mTLS: the warning does not explain the order dependence: $(cat "$WORK/warnings")"
ok 'grep -q "add that authenticationRef and the address wva-external-scaler.$NS.svc:9090" "$WORK/warnings" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/warnings" && grep -q "^=====" "$WORK/warnings"' "verify mTLS: the warning lacks the fix or its banner: $(cat "$WORK/warnings")"
ok 'grep -q "KEDA.s client certificate: ClusterTriggerAuthentication wva-external-scaler-$NS" "$WORK/info"' "verify mTLS: KEDA's side not reported: $(grep -i keda "$WORK/info" | head -2)"

echo "== verify: every ScaledObject authenticated is quiet; unreadable ones are unknown"
jq '.items |= map(select(.metadata.name != "plain" and .metadata.name != "wrongref"))' "$WORK/so.json" >"$WORK/so2.json" && mv "$WORK/so2.json" "$WORK/so.json"
run wva_verify_scaler_tls
ok '[ ! -s "$WORK/warnings" ] && grep -q "Every ScaledObject naming this scaler uses wva-external-scaler.$NS.svc:9090" "$WORK/info"' "verify, all authenticated: $(head -2 "$WORK/warnings")"
run wva_verify_scaler_tls STUB_SO=FORBIDDEN
ok '[ ! -s "$WORK/warnings" ] && grep -q "Could not list ScaledObjects" "$WORK/info"' "verify, ScaledObjects unreadable: reported as none or as a warning"
cp "$STORE/configmap__wva-scaler-keda-auth.json" "$WORK/marker.keep"
rm -f "$STORE/configmap__wva-scaler-keda-auth.json"
run wva_verify_scaler_tls
ok 'grep -q "KEDA.s side is not usable" "$WORK/warnings"' "verify, no marker: KEDA's side not flagged: $(head -2 "$WORK/warnings")"
cp "$WORK/marker.keep" "$STORE/configmap__wva-scaler-keda-auth.json"

echo "== verify: a certificate missing the scalerAddress host is a warning"
mint_serving 365 "wva-external-scaler.$NS.svc"
run wva_verify_scaler_tls
ok 'grep -q "does not name wva-external-scaler.$NS.svc.cluster.local" "$WORK/warnings"' "verify: missing SAN not warned: $(head -2 "$WORK/warnings")"

# ---------------------------------------------------------------- permissions, undeploy
echo "== permissions: what each phase must be allowed to create"
reset_store
run 'wva_scaler_tls_permission_needs' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$(tr "\n" " " <"$WORK/stdout")" = "secrets $NS configmaps $NS clustertriggerauthentications.keda.sh - secrets keda " ]' "permissions, openssl: '$(tr '\n' ' ' <"$WORK/stdout")'"
run 'wva_scaler_tls_permission_needs' WVA_SCALER_TLS=true STUB_CM=present
ok '[ "$(tr "\n" " " <"$WORK/stdout")" = "secrets $NS certificates.cert-manager.io $NS issuers.cert-manager.io $NS configmaps $NS clustertriggerauthentications.keda.sh - secrets keda " ]' "permissions, cert-manager: '$(tr '\n' ' ' <"$WORK/stdout")'"
run 'wva_scaler_tls_permission_needs' WVA_SCALER_TLS=true STUB_CM=absent INSTALL_PHASE=wva
ok '[ "$(tr "\n" " " <"$WORK/stdout")" = "secrets $NS " ]' "permissions, controller phase: '$(tr '\n' ' ' <"$WORK/stdout")' (KEDA's side is the admin's)"
run 'wva_scaler_tls_permission_needs'
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/stdout" ]' "permissions, the default: asked for '$(tr '\n' ' ' <"$WORK/stdout")' (without the rights it stays plaintext)"
put_deploy openssl
run 'wva_scaler_tls_permission_needs' INSTALL_PHASE=wva
ok '[ "$(tr "\n" " " <"$WORK/stdout")" = "secrets $NS " ]' "permissions, kept on: '$(tr '\n' ' ' <"$WORK/stdout")'"


# ---------------------------------------------------------------- KEDA's side, the default
SO_PLAIN_ADDR="wva-external-scaler.$NS.svc.cluster.local:9090"
SO_TLS_ADDR="wva-external-scaler.$NS.svc:9090"
CTA="wva-external-scaler-$NS"
# so_fixture <name> <address> [authenticationRef-json]: one ScaledObject in so.json.
so_fixture() {
    local ref="${3:-null}"
    jq -n --arg n "$1" --arg a "$2" --argjson r "$ref" '{items: [{metadata: {namespace: "apps", name: $n},
        spec: {triggers: [({type: "external-push", metadata: {scalerAddress: $a, modelID: "m"}}
                           + (if $r == null then {} else {authenticationRef: $r} end))]}}]}' >"$WORK/so.json"
}
fresh_k() { printf 'namespace: %s\nresources:\n- ./base\npatches:\n' "$NS" >"$WORK/k/kustomization.yaml"; }

echo "== default: a new install with KEDA present turns TLS on and writes KEDA's side"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ]' "default fresh: exit $RC, on=$WVA_SCALER_TLS_ON: $(cat "$WORK/refusal" "$WORK/warnings")"
ok 'grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "default fresh: TLS flags not rendered"
ok '[ "$(sort "$WORK/keda-writes" | tr "\n" " ")" = "configmap/wva-scaler-keda-auth cta/$CTA secret@keda/$CTA " ]' "default fresh: KEDA side wrote '$(sort "$WORK/keda-writes" | tr '\n' ' ')'"
secret_file "$CLIENT" tls.crt "$WORK/client.pem"
secret_file "$CLIENT" ca.crt "$WORK/client-ca.pem"
jq -r '.data["tls.crt"]' "$STORE/secret@keda__$CTA.json" | base64 -d >"$WORK/copy.crt"
jq -r '.data["ca.crt"]' "$STORE/secret@keda__$CTA.json" | base64 -d >"$WORK/copy.ca"
ok 'cmp -s "$WORK/copy.crt" "$WORK/client.pem" && cmp -s "$WORK/copy.ca" "$WORK/client-ca.pem"' "default fresh: KEDA's copy is not the client certificate and its CA"
ok '[ "$(jq -r ".metadata.labels[\"wva.llmd.ai/scaler-tls\"]" "$STORE/secret@keda__$CTA.json")" = keda-client ]' "default fresh: KEDA's copy is not labelled as the installer's"
ok '[ "$(jq -c "[.spec.secretTargetRef[] | .parameter + \"=\" + .key]" "$STORE/clustertriggerauthentications.keda.sh__$CTA.json")" = "[\"caCert=ca.crt\",\"tlsClientCert=tls.crt\",\"tlsClientKey=tls.key\"]" ] && [ "$(jq -r "[.spec.secretTargetRef[].name] | unique | .[]" "$STORE/clustertriggerauthentications.keda.sh__$CTA.json")" = "$CTA" ]' "default fresh: ClusterTriggerAuthentication does not read caCert, tlsClientCert and tlsClientKey from its copy"
ok '[ "$(jq -r .data.clientCertSha256 "$STORE/configmap__wva-scaler-keda-auth.json")" = "$(sha_of "$WORK/client.pem")" ] && jq -r .data.clientCert "$STORE/configmap__wva-scaler-keda-auth.json" | grep -q "BEGIN CERTIFICATE"' "default fresh: the marker does not record KEDA's certificate"
ok '! jq -r .data.clientCert "$STORE/configmap__wva-scaler-keda-auth.json" | grep -q "PRIVATE KEY"' "default fresh: the marker carries a private key"
cp -r "$STORE" "$WORK/store.admin"

echo "== default: KEDA absent, and this install does not put it there: plaintext, with a warning"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "default, no KEDA: exit $RC, on=$WVA_SCALER_TLS_ON"
ok 'grep -q "stays on plaintext" "$WORK/warnings" && grep -q "KEDA is not installed" "$WORK/warnings"' "default, no KEDA: no warning saying why: $(cat "$WORK/warnings")"

echo "== explicit: KEDA absent is a refusal"
reset_store
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_KEDA=absent
ok '[ "$RC" = 1 ] && grep -q "KEDA is not installed" "$WORK/refusal"' "explicit, no KEDA: exit $RC, $(cat "$WORK/refusal")"

echo "== default: KEDA installed by this run: TLS on, KEDA's side written after KEDA"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA=absent KEDA_HELM_INSTALL=true
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ] && [ "$WVA_SCALER_KEDA_DEFERRED" = true ] && [ ! -s "$WORK/keda-writes" ]' "deferred: exit $RC, on=$WVA_SCALER_TLS_ON deferred=$WVA_SCALER_KEDA_DEFERRED, keda writes '$(tr '\n' ' ' <"$WORK/keda-writes")'"
# The install runs it in the shell wva_scaler_tls_prepare set WANTED in.
run wva_scaler_tls_after_keda WVA_SCALER_KEDA_DEFERRED=true WVA_SCALER_TLS_WANTED=true STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "cta/$CTA" "$WORK/keda-writes"' "deferred: after KEDA, exit $RC, wrote '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/refusal")"
run wva_scaler_tls_after_keda WVA_SCALER_KEDA_DEFERRED=true WVA_SCALER_TLS_WANTED=true STUB_CM=absent STUB_KEDA=absent
ok '[ "$RC" = 1 ]' "deferred: KEDA still absent after its install is a refusal, whatever the default (exit $RC)"

echo "== default: no right to create a ClusterTriggerAuthentication: plaintext, with a warning"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_CANI_CTA=no
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && grep -q "may not create ClusterTriggerAuthentications" "$WORK/warnings" && [ ! -s "$WORK/keda-writes" ]' "default, no CTA right: exit $RC, on=$WVA_SCALER_TLS_ON, $(cat "$WORK/warnings")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_CANI=no
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && [ ! -s "$WORK/writes" ] && grep -q "may not create Secrets" "$WORK/warnings"' "default, no Secret right: exit $RC, on=$WVA_SCALER_TLS_ON, wrote '$(tr '\n' ' ' <"$WORK/writes")'"

echo "== KEDA's side: objects this installer did not write are refused"
reset_store
jq -n --arg n "$CTA" '{kind: "ClusterTriggerAuthentication", metadata: {name: $n}}' >"$STORE/clustertriggerauthentications.keda.sh__$CTA.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "ClusterTriggerAuthentication $CTA was not created by this installer" "$WORK/refusal" && [ ! -s "$WORK/keda-writes" ]' "foreign CTA: exit $RC, $(cat "$WORK/refusal")"
reset_store
jq -n --arg n "$CTA" '{kind: "Secret", metadata: {name: $n}, data: {}}' >"$STORE/secret@keda__$CTA.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "Secret $CTA in keda was not created by this installer" "$WORK/refusal"' "foreign copy: exit $RC, $(cat "$WORK/refusal")"

echo "== OpenShift: KEDA trusts the service CA, not the installer's"
reset_store
fresh_k
put_foreign_secret "$SERVING" '{"service.beta.openshift.io/originating-service-name":"wva-external-scaler"}'
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent ENVIRONMENT=openshift
ok '[ "$RC" = 0 ] && [ "$(jq -r ".data[\"ca.crt\"]" "$STORE/secret@keda__$CTA.json" | base64 -d)" = FAKE-SERVICE-CA ]' "OpenShift: KEDA's caCert is not the service CA: exit $RC, $(cat "$WORK/refusal")"

echo "== ScaledObjects on the plaintext address hold the default back, and refuse an explicit true"
reset_store
fresh_k
so_fixture app-so "$SO_PLAIN_ADDR"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "default with a plaintext ScaledObject: exit $RC, on=$WVA_SCALER_TLS_ON"
ok 'grep -q "apps/app-so" "$WORK/stderr" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/warnings"' "default with a plaintext ScaledObject: not named, or no fix given: $(cat "$WORK/warnings")"
ok 'grep -q "cta/$CTA" "$WORK/keda-writes"' "default held back: KEDA's side should still be written, so the move is one step"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "apps/app-so" "$WORK/refusal" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/refusal"' "explicit with a plaintext ScaledObject: exit $RC, $(cat "$WORK/refusal")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_SO=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "Cannot list the ScaledObjects" "$WORK/refusal"' "explicit, ScaledObjects unreadable: exit $RC, $(cat "$WORK/refusal")"

echo "== WVA_SCALER_TLS_MIGRATE=true moves them, after the controller has TLS"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent WVA_SCALER_TLS_MIGRATE=true
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ] && [ "$WVA_SCALER_TLS_MIGRATE_ROWS" = apps/app-so ] && [ ! -s "$WORK/patches" ]' "migrate: exit $RC, on=$WVA_SCALER_TLS_ON rows='$WVA_SCALER_TLS_MIGRATE_ROWS', patched early: $(cat "$WORK/patches")"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls
PATCHED="$(sed -n 's/^patch scaledobject app-so -n apps --type merge -p //p' "$WORK/patches")"
ok '[ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0].metadata.scalerAddress")" = "$SO_TLS_ADDR" ] && [ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0].authenticationRef | .name + \"/\" + .kind")" = "$CTA/ClusterTriggerAuthentication" ] && [ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0].metadata.modelID")" = m ]' "migrate: patch '$PATCHED'"

echo "== explicit false on a TLS install moves the TLS ScaledObjects back, or refuses"
reset_store
put_deploy openssl
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=false
ok '[ "$RC" = 1 ] && grep -q "apps/app-so" "$WORK/refusal"' "false with TLS ScaledObjects: exit $RC, $(cat "$WORK/refusal")"
ok 'grep -q "depends on order" "$WORK/refusal" && ! grep -q "not-Ready" "$WORK/refusal" && grep -q "WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true NAMESPACE=$NS make deploy-wva" "$WORK/refusal"' "false with TLS ScaledObjects: the refusal does not explain the order dependence or give the docs' command: $(cat "$WORK/refusal")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_MIGRATE_ROWS" = apps/app-so ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "false + migrate: exit $RC, rows '$WVA_SCALER_TLS_MIGRATE_ROWS'"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=plaintext
PATCHED="$(sed -n 's/^patch scaledobject app-so -n apps --type merge -p //p' "$WORK/patches")"
ok '[ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0].metadata.scalerAddress")" = "$SO_PLAIN_ADDR" ] && [ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0] | has(\"authenticationRef\")")" = false ]' "back to plaintext: patch '$PATCHED'"

echo "== controller phase: reads the admin's marker"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && grep -q "setup-prereqs" "$WORK/warnings" && [ ! -s "$WORK/keda-writes" ]' "controller, no marker: exit $RC, on=$WVA_SCALER_TLS_ON, $(cat "$WORK/warnings")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '[ "$RC" = 1 ] && grep -q "has not written KEDA" "$WORK/refusal"' "controller, no marker, explicit: exit $RC, $(cat "$WORK/refusal")"
rm -rf "$STORE"; cp -r "$WORK/store.admin" "$STORE"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ] && [ ! -s "$WORK/keda-writes" ]' "controller, admin's marker: exit $RC, on=$WVA_SCALER_TLS_ON, wrote KEDA objects '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/refusal" "$WORK/warnings")"
jq '.data.clusterTriggerAuthentication = "someone-else"' "$STORE/configmap__wva-scaler-keda-auth.json" >"$WORK/m.json" && mv "$WORK/m.json" "$STORE/configmap__wva-scaler-keda-auth.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '[ "$WVA_SCALER_TLS_ON" = false ] && grep -q "someone-else" "$WORK/warnings"' "controller, marker for another install: on=$WVA_SCALER_TLS_ON"

echo "== kind: this install puts KEDA in but its API is not served yet, so TLS goes on and KEDA's side follows"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA=absent ENVIRONMENT=kind-emulator
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ] && [ "$WVA_SCALER_KEDA_DEFERRED" = true ]' "kind: exit $RC, on=$WVA_SCALER_TLS_ON deferred=$WVA_SCALER_KEDA_DEFERRED: $(cat "$WORK/refusal" "$WORK/warnings")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_KEDA=absent ENVIRONMENT=kind-emulator
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ]' "kind, explicit: a valid install was refused (exit $RC): $(cat "$WORK/refusal")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA=absent SCALER_BACKEND=none ENVIRONMENT=kind-emulator
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ]' "kind, SCALER_BACKEND=none: nothing installs KEDA, so no TLS (on=$WVA_SCALER_TLS_ON)"

echo "== KEDA's client certificate lives as long as the CA (KEDA never reloads it)"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
secret_file "$CLIENT" tls.crt "$WORK/client.pem"
ok 'openssl x509 -in "$WORK/client.pem" -noout -checkend $((3000 * 86400)) >/dev/null 2>&1' "client certificate expires within 3000 days: $(openssl x509 -in "$WORK/client.pem" -noout -enddate 2>&1)"
secret_file "$SERVING" tls.crt "$WORK/serving.pem"
ok '! openssl x509 -in "$WORK/serving.pem" -noout -checkend $((400 * 86400)) >/dev/null 2>&1' "serving certificate should stay a 1-year leaf (the controller reloads it)"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '! grep -q "restart KEDA" "$WORK/warnings"' "unchanged re-run asks for a KEDA restart: $(cat "$WORK/warnings")"
rm -f "$STORE/secret__$CA.json"
put_deploy openssl
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "restart KEDA.s operator now" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "a new CA must ask for a KEDA restart: $(cat "$WORK/warnings")"

echo "== the key copy goes only where a tenant cannot choose"
# The NetworkPolicy in WVA_NS names tenant-pick: that namespace's admin can
# edit it, so it must not decide where the private key goes. infra_wva.sh is
# sourced, so code that asks wva_resolve_scaler_keda_namespace would follow it.
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false KEDA_NAMESPACE=keda-system STUB_NP=tenant-pick
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && grep -q "where KEDA runs is not known" "$WORK/warnings" && ! grep -q "secret@" "$WORK/keda-writes"' "unknown KEDA namespace: on=$WVA_SCALER_TLS_ON, keda writes '$(tr '\n' ' ' <"$WORK/keda-writes")'"

echo "== a tenant uninstall keeps the admin's marker"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
run wva_scaler_tls_undeploy STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '! grep -q "delete configmap wva-scaler-keda-auth" "$WORK/kubectl.log"' "controller-phase undeploy deleted the admin's marker"
run wva_scaler_tls_undeploy STUB_CM=absent
ok 'grep -q "delete configmap wva-scaler-keda-auth" "$WORK/kubectl.log"' "admin undeploy did not delete the marker"

echo "== plaintext with ScaledObjects still on the TLS address: they are named"
reset_store
fresh_k
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent WVA_APPLY_SCOPE=controller
ok '[ "$WVA_SCALER_TLS_ON" = false ] && grep -q "but it stays on plaintext:" "$WORK/warnings" && grep -q "depends on order" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr"' "stranded TLS ScaledObject not named: $(cat "$WORK/warnings")"

echo "== the CA KEDA trusts moved on (OpenShift rotates its service CA)"
reset_store
fresh_k
put_foreign_secret "$SERVING" '{"service.beta.openshift.io/originating-service-name":"wva-external-scaler"}'
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent ENVIRONMENT=openshift
# marker_stale prints what the controller phase concludes about the CA.
marker_stale() { wva_scaler_keda_marker_state; echo "ready=$WVA_SCALER_KEDA_READY stale=$WVA_SCALER_KEDA_CA_STALE"; }
run marker_stale ENVIRONMENT=openshift
ok 'grep -q "ready=true stale=false" "$WORK/stdout"' "unrotated service CA read as stale, or the marker not ready: $(cat "$WORK/stdout" "$WORK/refusal")"
run marker_stale ENVIRONMENT=openshift STUB_SERVICE_CA=ROTATED
ok 'grep -q "stale=true" "$WORK/stdout"' "a rotated service CA was not flagged: $(cat "$WORK/stdout")"

echo "== hand-set flags: WVA_SCALER_TLS=true takes them over, and says which"
reset_store
put_deploy manual
run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "takes over" "$WORK/warnings" && grep -q -- "--external-scaler-cert-path=/mine" "$WORK/stderr"' "takeover: exit $RC, warnings '$(cat "$WORK/warnings")' stderr '$(cat "$WORK/stderr")'"

echo "== undeploy: removes only what carries the installer label, and KEDA's side only from the admin phase"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
run wva_scaler_tls_undeploy STUB_CM=present
ok 'grep -q "^delete secret -n $NS -l wva.llmd.ai/scaler-tls" "$WORK/kubectl.log" && grep -q "^delete certificates.cert-manager.io,issuers.cert-manager.io -n $NS -l wva.llmd.ai/scaler-tls" "$WORK/kubectl.log"' "undeploy: $(tr '\n' ';' <"$WORK/kubectl.log")"
ok 'grep -q "^delete clustertriggerauthentications.keda.sh $CTA" "$WORK/kubectl.log" && grep -q "^delete secret $CTA -n keda" "$WORK/kubectl.log"' "undeploy, admin: KEDA's side not removed: $(tr '\n' ';' <"$WORK/kubectl.log")"
run wva_scaler_tls_undeploy STUB_CM=present WVA_APPLY_SCOPE=controller
ok '! grep -q "clustertriggerauthentications\|-n keda" "$WORK/kubectl.log"' "undeploy, controller phase: touched KEDA's side: $(tr '\n' ';' <"$WORK/kubectl.log")"

# ---------------------------------------------------------------- where KEDA is, and restarting it
echo "== KEDA's Deployments unreadable: where its Secrets go is unknown, not KEDA's namespace"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false \
    "STUB_APISERVICE='keda keda-operator-metrics-apiserver'" STUB_KEDA_DEPLOYS=FORBIDDEN
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && grep -q "KEDA_CLUSTER_OBJECT_NAMESPACE" "$WORK/warnings" && ! grep -q "secret@" "$WORK/keda-writes"' "default, KEDA Deployments Forbidden: exit $RC, on=$WVA_SCALER_TLS_ON, keda writes '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/warnings")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false \
    "STUB_APISERVICE='keda keda-operator-metrics-apiserver'" STUB_KEDA_DEPLOYS=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "KEDA_CLUSTER_OBJECT_NAMESPACE" "$WORK/refusal"' "explicit, KEDA Deployments Forbidden: exit $RC, $(cat "$WORK/refusal")"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA_DEPLOYS=FORBIDDEN
ok '[ "$RC" = 0 ] && grep -q "secret@keda/$CTA" "$WORK/keda-writes"' "KEDA_NAMESPACE given, Deployments Forbidden: the caller's namespace should be used: $(cat "$WORK/warnings")"

echo "== the restart command names where KEDA runs, not install.sh's keda-system default"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false KEDA_NAMESPACE=keda-system \
    "STUB_APISERVICE='openshift-keda keda-metrics-apiserver'"
rm -f "$STORE/secret__$CA.json"
put_deploy openssl
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false KEDA_NAMESPACE=keda-system \
    "STUB_APISERVICE='openshift-keda keda-metrics-apiserver'"
ok '[ "$RC" = 0 ] && grep -q "kubectl rollout restart deployment/keda-operator -n openshift-keda" "$WORK/warnings"' "restart command on a CMA-style cluster: $(grep -i restart "$WORK/warnings")"

echo "== a first write of KEDA's side while KEDA runs and ScaledObjects use the TLS address asks for a restart"
reset_store
fresh_k
echo '{}' >"$STORE/deployment@keda__keda-operator.json"
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "written for the first time while KEDA.s operator is running" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "reinstall: no restart notice: $(cat "$WORK/warnings")"

# A first install and a reinstall look the same to the admin phase (the
# uninstall removed the marker), and an unwired HPA is what a missed restart
# costs. So it is a warning that says it cannot tell, and where to find out.
echo "== a first write of KEDA's side while KEDA runs, with no ScaledObject on the TLS address, warns that it cannot tell a reinstall"
reset_store
fresh_k
echo '{}' >"$STORE/deployment@keda__keda-operator.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = true ] && grep -q "cannot tell a first install from a reinstall" "$WORK/warnings" && grep -q "KEDA keeps the old client certificate until its operator restarts" "$WORK/warnings" && grep -q "HPAs stay unwired" "$WORK/warnings" && grep -q "make verify-deployment" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "first write with KEDA running: exit $RC, on=$WVA_SCALER_TLS_ON, warnings '$(cat "$WORK/warnings")'"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
ok '! grep -q "rollout restart" "$WORK/warnings"' "first write with no KEDA operator running asks for a restart: $(cat "$WORK/warnings")"

echo "== an admin uninstall that removed KEDA's side says to restart KEDA, only when a ScaledObject used the TLS address"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
run wva_scaler_tls_undeploy STUB_CM=absent
# KEDA opens a connection for the TLS address only when a ScaledObject dials it.
ok '[ ! -f "$STORE/clustertriggerauthentications.keda.sh__$CTA.json" ] && ! grep -q "rollout restart" "$WORK/warnings"' "uninstall, no ScaledObject ever on the TLS address: restart advice printed anyway: $(cat "$WORK/warnings")"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run wva_scaler_tls_undeploy STUB_CM=absent
# After the reinstall, not before: a restart with the ClusterTriggerAuthentication
# gone rebuilds the pooled TLS address with empty auth parameters, in plaintext.
ok 'grep -q "keeps a connection with the old client certificate until it restarts" "$WORK/info" && ! grep -q "this uninstall restarts it" "$WORK/info" && ! grep -q "^rollout restart" "$WORK/kubectl.log"' "uninstall with a ScaledObject on the TLS address, default: promised or ran a KEDA restart: $(cat "$WORK/info" "$WORK/warnings")"
# The restart itself, after the ScaledObjects. KEDA is shared, so by default
# the uninstall never restarts it: it says so and gives the command, and the
# reinstall finds the stale connection (the verify cases below). With
# KEDA_RESTART=true it restarts KEDA, but not while a ScaledObject is still on
# the TLS address: KEDA would rebuild it with no auth.
run 'wva_scaler_keda_restart_after_undeploy' WVA_SCALER_KEDA_RESTART_WANTED=true
ok '! grep -q "^rollout restart" "$WORK/kubectl.log" && grep -q "KEDA_RESTART=true lets the uninstall do it" "$WORK/info" && grep -q "the install finds that connection and prints the restart" "$WORK/info" && grep -q "kubectl rollout restart deployment/keda-operator -n keda" "$WORK/info"' "uninstall, default: restarted KEDA, or did not say how: $(cat "$WORK/info" "$WORK/warnings"; cat "$WORK/kubectl.log")"
run 'wva_scaler_keda_restart_after_undeploy' WVA_SCALER_KEDA_RESTART_WANTED=true KEDA_RESTART=true
ok 'grep -q "install this scaler in $NS again, restart it after the install:  kubectl rollout restart deployment/keda-operator -n keda" "$WORK/warnings" && ! grep -q "^rollout restart" "$WORK/kubectl.log"' "KEDA_RESTART=true, a ScaledObject still on the TLS address: KEDA restarted anyway, or no restart-after-reinstall notice: $(cat "$WORK/warnings"; cat "$WORK/kubectl.log")"
rm -f "$WORK/so.json"
run 'wva_scaler_keda_restart_after_undeploy' WVA_SCALER_KEDA_RESTART_WANTED=true KEDA_RESTART=true
ok 'grep -q "^rollout restart deployment/keda-operator -n keda" "$WORK/kubectl.log" && grep -q "Restarted KEDA.s operator in keda" "$WORK/info"' "KEDA_RESTART=true, no ScaledObject left: the uninstall did not restart KEDA: $(cat "$WORK/info" "$WORK/warnings"; cat "$WORK/kubectl.log")"
run 'wva_scaler_keda_restart_after_undeploy' WVA_SCALER_KEDA_RESTART_WANTED=true
ok '! grep -q "^rollout restart" "$WORK/kubectl.log"' "uninstall, default, no ScaledObject left: restarted KEDA: $(cat "$WORK/kubectl.log")"

echo "== undeploy removes KEDA's copy where the marker says it went"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA_OBJ_NS=keda-objs
ok '[ "$RC" = 0 ] && grep -q "secret@keda-objs/$CTA" "$WORK/keda-writes" && [ "$(jq -r .data.secretNamespace "$STORE/configmap__wva-scaler-keda-auth.json")" = keda-objs ]' "moved object namespace: copy not written to keda-objs: '$(tr '\n' ' ' <"$WORK/keda-writes")'"
# Where KEDA runs is no longer known (KEDA uninstalled first), so the marker
# is all there is.
run wva_scaler_tls_undeploy STUB_CM=absent KEDA_NAMESPACE_EXPLICIT=false
ok 'grep -q "^delete secret $CTA -n keda-objs" "$WORK/kubectl.log" && [ ! -f "$STORE/secret@keda-objs__$CTA.json" ]' "undeploy left KEDA's copy in keda-objs: $(grep '^delete' "$WORK/kubectl.log" | tr '\n' ';')"

echo "== undeploy trusts where KEDA reads its Secrets over a marker that names another namespace"
# The marker is in the controller namespace, so its admin can point it at any
# namespace that holds a Secret of that name with the label.
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
# marker_edit <program>: rewrites the marker in the store.
marker_edit() {
    jq "$1" "$STORE/configmap__wva-scaler-keda-auth.json" >"$WORK/m.json" && mv "$WORK/m.json" "$STORE/configmap__wva-scaler-keda-auth.json"
}
marker_edit '.data.secretNamespace = "victim"'
jq '.metadata.namespace = "victim"' "$STORE/secret@keda__$CTA.json" >"$STORE/secret@victim__$CTA.json"
run wva_scaler_tls_undeploy STUB_CM=absent
ok '[ ! -f "$STORE/secret@keda__$CTA.json" ] && grep -q "^delete secret $CTA -n keda" "$WORK/kubectl.log"' "forged marker: our copy in keda not removed: $(grep '^delete' "$WORK/kubectl.log" | tr '\n' ';')"
ok '[ -f "$STORE/secret@victim__$CTA.json" ] && ! grep -q "^delete secret $CTA -n victim" "$WORK/kubectl.log"' "forged marker: deleted the Secret in the namespace the marker named: $(grep '^delete' "$WORK/kubectl.log" | tr '\n' ';')"
ok 'grep -q "says KEDA.s copy of the scaler client certificate went to victim, but KEDA reads its Secrets from keda" "$WORK/warnings"' "forged marker: the mismatch is not reported: $(cat "$WORK/warnings")"

echo "== undeploy leaves a same-named ClusterTriggerAuthentication and copy it did not write"
reset_store
jq -n --arg n "$CTA" '{kind: "ClusterTriggerAuthentication", metadata: {name: $n}}' >"$STORE/clustertriggerauthentications.keda.sh__$CTA.json"
jq -n --arg n "$CTA" '{kind: "Secret", metadata: {name: $n}, data: {}}' >"$STORE/secret@keda__$CTA.json"
run wva_scaler_tls_undeploy STUB_CM=absent
ok '[ -f "$STORE/clustertriggerauthentications.keda.sh__$CTA.json" ] && ! grep -q "^delete clustertriggerauthentications" "$WORK/kubectl.log"' "undeploy deleted a foreign ClusterTriggerAuthentication: $(grep '^delete' "$WORK/kubectl.log" | tr '\n' ';')"
ok '[ -f "$STORE/secret@keda__$CTA.json" ] && grep -q "ClusterTriggerAuthentication $CTA was not created by this installer" "$WORK/warnings"' "undeploy deleted a foreign copy, or did not say it left the ClusterTriggerAuthentication: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- stranded ScaledObjects, every plaintext exit
# stranded_case <label> <run args...>: a plaintext controller, a ScaledObject on
# the TLS address, and a run that stays on plaintext must name it.
stranded_case() {
    local label="$1"
    shift
    echo "== plaintext because $label: ScaledObjects on the TLS address are named"
    reset_store
    fresh_k
    put_deploy plaintext
    so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' "$@"
    ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && grep -q "but it stays on plaintext:" "$WORK/warnings" && grep -q "depends on order" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr"' "$label: exit $RC, on=$WVA_SCALER_TLS_ON, stranded ScaledObject not named: $(cat "$WORK/warnings" "$WORK/refusal")"
}
stranded_case "openssl is missing" STUB_CM=absent "PATH=$WORK/noopenssl"
stranded_case "Secrets may not be created" STUB_CM=absent STUB_CANI=no
stranded_case "cert-manager is unknown" STUB_CM=error
stranded_case "WVA_SCALER_TLS=false" WVA_SCALER_TLS=false

echo "== verify: a plaintext controller names ScaledObjects on the TLS address"
reset_store
put_deploy plaintext
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run wva_verify_scaler_tls
ok '[ "$RC" = 0 ] && grep -q "is on plaintext, but these ScaledObjects use its TLS address" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr"' "verify plaintext: stranded ScaledObject not named: $(cat "$WORK/warnings")"
ok 'grep -q "depends on order" "$WORK/warnings" && ! grep -q "not-Ready" "$WORK/warnings"' "verify plaintext: the warning does not explain the order dependence: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- the CA KEDA trusts, from the controller phase
# store_tls_controller: the admin and controller phases have run, and the
# Deployment in the store is what the TLS overlay renders.
store_tls_controller() {
    reset_store
    fresh_k
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
    rm -rf "$WORK/render"
    mkdir -p "$WORK/render"
    ln -s "$ROOT/config/overlays/namespace-scoped/kubernetes" "$WORK/render/base"
    cp "$WORK/k/kustomization.yaml" "$WORK/render/kustomization.yaml"
    if render "$WORK/render" "$WORK/rendered.yaml" 2>"$WORK/err"; then
        yq -o=json 'select(.kind == "Deployment" and .metadata.name == "wva-controller-manager")' "$WORK/rendered.yaml" \
            >"$STORE/deployment__wva-controller-manager.json"
    else
        fail "the TLS overlay does not render: $(head -3 "$WORK/err")"
    fi
}

echo "== controller phase: KEDA trusts an old CA: warn, with the admin command and the KEDA restart"
store_tls_controller
marker_edit '.data.caSha256 = "0000"'
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "KEDA trusts a different CA" "$WORK/warnings" && grep -q "setup-prereqs" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "controller phase, stale CA: exit $RC, $(cat "$WORK/warnings" "$WORK/refusal")"
run wva_verify_scaler_tls
ok 'grep -q "KEDA trusts a different CA" "$WORK/warnings" && ! grep -q "KEDA trusts the current CA" "$WORK/info"' "verify, stale CA: warning missing, or the positive line printed too: $(cat "$WORK/warnings" "$WORK/info")"

# The docs say verify prints whether KEDA trusts the current CA. On a healthy
# real cluster it printed nothing about it: only a mismatch had a line.
echo "== verify: KEDA trusting the current CA is said; an unreadable CA is not called current"
store_tls_controller
run wva_verify_scaler_tls
ok 'grep -q "KEDA trusts the current CA" "$WORK/info" && ! grep -q "different CA" "$WORK/warnings"' "verify, current CA: no positive line: $(cat "$WORK/info")"
jq 'del(.data["ca.crt"])' "$STORE/secret__$CLIENT.json" >"$WORK/client.json" && mv "$WORK/client.json" "$STORE/secret__$CLIENT.json"
run wva_verify_scaler_tls
ok 'grep -q "current CA could not be read" "$WORK/info" && ! grep -q "KEDA trusts the current CA" "$WORK/info"' "verify, CA unreadable: called current, or not said: $(cat "$WORK/info")"
store_tls_controller

echo "== controller phase and verify: a marker with no caSha256 is reported as cannot tell"
marker_edit 'del(.data.caSha256)'
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent
ok '[ "$RC" = 0 ] && grep -q "Cannot tell which CA KEDA trusts" "$WORK/warnings" && grep -q "setup-prereqs" "$WORK/warnings"' "controller phase, marker without caSha256: $(cat "$WORK/warnings" "$WORK/refusal")"
run wva_verify_scaler_tls
ok 'grep -q "Cannot tell which CA KEDA trusts" "$WORK/warnings"' "verify, marker without caSha256: $(cat "$WORK/warnings")"

echo "== warmpool.sh reads the controller once: one warning when it cannot"
# A kubectl that is Forbidden everything, so the controller's mode is unknown.
mkdir -p "$WORK/kbin"
printf '#!/bin/sh\necho "Error from server (Forbidden): deployments.apps is forbidden in the namespace tenant" >&2\nexit 1\n' >"$WORK/kbin/kubectl"
chmod +x "$WORK/kbin/kubectl"
PATH="$WORK/kbin:$PATH" bash "$ROOT/deploy/warmpool.sh" create -n tenant --name pool \
    --proxy-image example.invalid/proxy:v1 --wva-namespace wva-system \
    --models 2 --model-size 8B --accelerator NVIDIA-H100-80GB-HBM3 --dry-run >"$WORK/wp.out" 2>"$WORK/wp.err"
ok '[ "$(grep -c "whether its scaler serves TLS is unknown" "$WORK/wp.err")" = 1 ] && grep -q "scalerAddress: wva-external-scaler.wva-system.svc.cluster.local:9090" "$WORK/wp.out" && ! grep -q "authenticationRef" "$WORK/wp.out"' "warmpool: the unknown-mode warning printed $(grep -c "whether its scaler serves TLS is unknown" "$WORK/wp.err") times (want 1), or the plaintext trigger is wrong: $(grep -n "scalerAddress\|authenticationRef" "$WORK/wp.out" | tr '\n' ' ')"

echo "== make verify-deployment loads the scaler TLS checks"
ok 'grep -A8 "^verify-deployment:" "$ROOT/Makefile" | grep -q "source deploy/lib/scaler_tls.sh"' "the verify-deployment recipe does not source scaler_tls.sh, so its scaler TLS checks never run"
# KEDA_NAMESPACE given by the caller is trusted as where KEDA runs (the restart
# command), and OpenShift is told apart (the service CA KEDA must trust).
ok 'grep -A8 "^verify-deployment:" "$ROOT/Makefile" | grep -qF "KEDA_NAMESPACE_EXPLICIT=false; [ -z \"\$\${KEDA_NAMESPACE:-}\" ] || KEDA_NAMESPACE_EXPLICIT=true;"' "the verify-deployment recipe does not set KEDA_NAMESPACE_EXPLICIT from KEDA_NAMESPACE"
ok 'grep -A8 "^verify-deployment:" "$ROOT/Makefile" | grep -qF "\$(if \$(ENVIRONMENT_INSTALL),ENVIRONMENT=\$(ENVIRONMENT_INSTALL),)" && grep -A8 "^verify-deployment:" "$ROOT/Makefile" | grep -qF "[ -n \"\$\${ENVIRONMENT:-}\" ] || ! wva_is_openshift || ENVIRONMENT=openshift;"' "the verify-deployment recipe does not pass ENVIRONMENT, or does not detect OpenShift when it is unset"

# ---------------------------------------------------------------- Forbidden is never NotFound
# probe_* print what a reader concluded, for the cases below.
probe_object() { wva_scaler_object_state secret "$CLIENT"; echo "state=$WVA_OBJ_STATE"; }
probe_live() { wva_scaler_tls_read_live; echo "live=$WVA_SCALER_TLS_LIVE"; }
probe_mode() { echo "mode=$(wva_scaler_mode_of "$NS")"; }
probe_marker() { wva_scaler_keda_marker_state; echo "ready=$WVA_SCALER_KEDA_READY why=$WVA_SCALER_KEDA_WHY"; }

echo "== a Forbidden read in a namespace named notfound-prod stays unreadable"
reset_store
put_deploy openssl
run probe_object STUB_SECRETS=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -qx "state=unreadable" "$WORK/stdout"' "Forbidden Secret read as '$(cat "$WORK/stdout")', want unreadable"
run probe_live STUB_DEPLOY=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -qx "live=unreadable" "$WORK/stdout"' "Forbidden Deployment read as '$(cat "$WORK/stdout")', want unreadable"
run probe_mode STUB_DEPLOY=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -qx "mode=unknown" "$WORK/stdout"' "Forbidden Deployment mode '$(cat "$WORK/stdout")', want unknown (plaintext would write the wrong address)"
run probe_marker STUB_CONFIGMAPS=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -q "could not be read" "$WORK/stdout"' "Forbidden marker read as '$(cat "$WORK/stdout")', want could not be read"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_CTA=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok '[ "$RC" = 1 ] && grep -q "Cannot read ClusterTriggerAuthentication" "$WORK/refusal" && ! grep -q "cta/" "$WORK/keda-writes"' "Forbidden ClusterTriggerAuthentication: exit $RC, wrote '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- the client identity
echo "== WVA_SCALER_CLIENT_SAN must be one DNS name"
reset_store
NL='
'
run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent WVA_SCALER_CLIENT_SAN=keda.client-1
ok '[ "$RC" = 0 ]' "a valid client SAN was refused: $(cat "$WORK/refusal")"
# Each is eval'd by run as WVA_SCALER_CLIENT_SAN=<it>, so quoted where it must be.
for bad in 'a,DNS:evil' 'Upper' '-lead' '"two words"' '"ok${NL}evil"'; do
    run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent "WVA_SCALER_CLIENT_SAN=$bad"
    ok '[ "$RC" = 1 ] && grep -q "WVA_SCALER_CLIENT_SAN must be one lowercase DNS name" "$WORK/refusal"' "client SAN $bad accepted (exit $RC)"
done

# ---------------------------------------------------------------- Ctrl-C while minting
# arm_outer_exit stands in for deploy_wva_controller's own EXIT trap;
# arm_outer_int for an INT trap the caller had before.
arm_outer_exit() { trap 'echo outer >"$WORK/outer-exit"' EXIT; }
arm_outer_int() { trap 'echo mine' INT; }
print_int_trap() { trap -p INT; }

echo "== Ctrl-C while a leaf is signed leaves no key on disk, and the caller's EXIT trap still runs"
reset_store
rm -rf "$WORK/tmpd" "$WORK/outer-exit" "$WORK/tmpd-during"
mkdir -p "$WORK/tmpd"
# openssl, as a function, interrupts the shell running the install when it is
# asked to sign the first leaf: the CA key and the leaf key are on disk by then,
# and the function records what the temp directory held at that moment.
run 'arm_outer_exit ; wva_scaler_tls_provision_openssl' "export TMPDIR=$WORK/tmpd" \
    'openssl() { case "$*" in "x509 -req"*) find "$TMPDIR" -name "*.key" >"$WORK/tmpd-during"; kill -INT "$BASHPID" ;; esac; command openssl "$@"; }'
ok '[ -s "$WORK/tmpd-during" ]' "interrupted mint: no key was on disk under TMPDIR when the signal came, so this case proves nothing"
ok '[ "$RC" = 130 ] && [ -z "$(ls -A "$WORK/tmpd")" ]' "interrupted mint: exit $RC, left behind: $(cd "$WORK/tmpd" && find . -type f | tr '\n' ' ')"
ok '[ -s "$WORK/outer-exit" ]' "interrupted mint: the caller's EXIT trap did not run (clobbered)"

echo "== minting puts the caller's INT trap back"
reset_store
run 'arm_outer_int ; wva_scaler_tls_provision_openssl; print_int_trap'
ok '[ "$RC" = 0 ] && grep -q "echo mine" "$WORK/stdout"' "after minting the INT trap is '$(cat "$WORK/stdout")', want the caller's: $(cat "$WORK/refusal")"

echo "== the issued line gives each certificate's own lifetime"
reset_store
run wva_scaler_tls_provision_openssl
ok 'grep -q "serving certificate is valid 365 days, KEDA.s client certificate 3650 days" "$WORK/info"' "issued line: $(grep issued "$WORK/info")"

# ---------------------------------------------------------------- moving back while plaintext stays
# The fix that install and verify print for ScaledObjects stranded on the TLS
# address is WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true; on a controller
# that is already plaintext, that run must move them, or the advice loops.
echo "== plaintext controller, ScaledObjects on the TLS address, WVA_SCALER_TLS=false + migrate: they move back"
reset_store
fresh_k
put_deploy plaintext
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true WVA_APPLY_SCOPE=controller
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && [ "$WVA_SCALER_TLS_MIGRATE_ROWS" = apps/app-so ] && [ "$WVA_SCALER_TLS_MIGRATE_MODE" = plaintext ]' "plaintext + migrate: exit $RC, on=$WVA_SCALER_TLS_ON rows='$WVA_SCALER_TLS_MIGRATE_ROWS' mode='$WVA_SCALER_TLS_MIGRATE_MODE' (want apps/app-so, plaintext): $(cat "$WORK/refusal" "$WORK/warnings")"
ok '! grep -q "but it stays on plaintext:" "$WORK/warnings"' "plaintext + migrate: still warns that they are stranded: $(cat "$WORK/warnings")"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=plaintext
PATCHED="$(sed -n 's/^patch scaledobject app-so -n apps --type merge -p //p' "$WORK/patches")"
ok '[ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0].metadata.scalerAddress")" = "$SO_PLAIN_ADDR" ] && [ "$(printf "%s" "$PATCHED" | jq -r ".spec.triggers[0] | has(\"authenticationRef\")")" = false ]' "plaintext + migrate: patch '$PATCHED'"

echo "== the default stays plaintext (KEDA side not written) with migrate: TLS ScaledObjects move back"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS_MIGRATE=true WVA_APPLY_SCOPE=controller STUB_CM=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && [ "$WVA_SCALER_TLS_MIGRATE_ROWS" = apps/app-so ] && [ "$WVA_SCALER_TLS_MIGRATE_MODE" = plaintext ]' "default, no marker, migrate: exit $RC, on=$WVA_SCALER_TLS_ON rows='$WVA_SCALER_TLS_MIGRATE_ROWS' mode='$WVA_SCALER_TLS_MIGRATE_MODE': $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- a stale CA on the first turn-on
echo "== controller phase, first turn-on, KEDA trusts an old CA: stays plaintext (default) or refuses (explicit)"
rm -rf "$STORE"; cp -r "$WORK/store.admin" "$STORE"
rm -f "$WORK/so.json"
marker_edit '.data.caSha256 = "0000"'
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "stale CA, first turn-on: TLS went on although every handshake would fail (exit $RC, on=$WVA_SCALER_TLS_ON)"
ok 'grep -q "stays on plaintext" "$WORK/warnings" && grep -q "KEDA trusts a different CA" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "stale CA, first turn-on: no warning with the fix: $(cat "$WORK/warnings")"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true WVA_APPLY_SCOPE=controller STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "KEDA trusts a different CA" "$WORK/refusal" && grep -q "setup-prereqs" "$WORK/refusal" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/refusal"' "stale CA, explicit: exit $RC, $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- the marker is the installer's
echo "== a ConfigMap wva-scaler-keda-auth this installer did not write is refused, not overwritten"
reset_store
fresh_k
jq -n '{kind: "ConfigMap", metadata: {name: "wva-scaler-keda-auth"}, data: {clusterTriggerAuthentication: "theirs"}}' >"$STORE/configmap__wva-scaler-keda-auth.json"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent
ok '[ "$RC" = 1 ] && grep -q "ConfigMap wva-scaler-keda-auth in $NS was not created by this installer" "$WORK/refusal"' "foreign marker: exit $RC, $(cat "$WORK/refusal")"
ok '[ ! -s "$WORK/keda-writes" ] && [ "$(jq -r .data.clusterTriggerAuthentication "$STORE/configmap__wva-scaler-keda-auth.json")" = theirs ]' "foreign marker: wrote '$(tr '\n' ' ' <"$WORK/keda-writes")' or overwrote it"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true STUB_CM=absent STUB_CONFIGMAPS=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "Cannot read ConfigMap wva-scaler-keda-auth" "$WORK/refusal" && [ ! -s "$WORK/keda-writes" ]' "unreadable marker: exit $RC, wrote '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/refusal")"

# ---------------------------------------------------------------- no key left behind by a refusal
# provision_tmp_case <label>: an OpenShift refusal over a foreign serving Secret
# must leave TMPDIR empty -- no CA key, and no empty directory either.
provision_tmp_case() {
    echo "== OpenShift refusal ($1): nothing left in TMPDIR"
    put_foreign_secret "$SERVING"
    rm -rf "$WORK/tmpd2"
    mkdir -p "$WORK/tmpd2"
    run wva_scaler_tls_provision_openssl ENVIRONMENT=openshift "export TMPDIR=$WORK/tmpd2"
    ok '[ "$RC" = 1 ] && grep -q "service CA" "$WORK/refusal"' "$1: exit $RC, $(cat "$WORK/refusal")"
    ok '[ -z "$(find "$WORK/tmpd2" -name ca.key)" ] && [ -z "$(ls -A "$WORK/tmpd2")" ]' "$1: left behind: $(cd "$WORK/tmpd2" && find . | tr '\n' ' ')"
}
reset_store
provision_tmp_case "no CA yet"
reset_store
put_foreign_secret "$SERVING" '{"service.beta.openshift.io/originating-service-name":"wva-external-scaler"}'
run wva_scaler_tls_provision_openssl ENVIRONMENT=openshift
ok '[ "$RC" = 0 ] && [ -n "$(jq -r ".data[\"tls.key\"] // empty" "$STORE/secret__$CA.json")" ]' "setup for the CA-key case: no CA with a key was stored: $(cat "$WORK/refusal")"
provision_tmp_case "the CA and its key exist"

# ---------------------------------------------------------------- client identity limits
echo "== WVA_SCALER_CLIENT_SAN: label and length limits"
L63="$(printf 'a%.0s' $(seq 63))"
L61="$(printf 'b%.0s' $(seq 61))"
L62="$(printf 'b%.0s' $(seq 62))"
for good in "$L63.x" "$L63.$L63.$L63.$L61"; do
    run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent "WVA_SCALER_CLIENT_SAN=$good"
    ok '[ "$RC" = 0 ]' "client SAN of ${#good} characters refused: $(cat "$WORK/refusal")"
done
for bad in 'a..b' "${L63}c.x" "$L63.$L63.$L63.$L62" 'a.-b' 'a-.b'; do
    run wva_resolve_scaler_tls WVA_SCALER_TLS=true STUB_CM=absent "WVA_SCALER_CLIENT_SAN=$bad"
    ok '[ "$RC" = 1 ] && grep -q "WVA_SCALER_CLIENT_SAN must be one lowercase DNS name" "$WORK/refusal"' "client SAN '$bad' (${#bad} characters) accepted (exit $RC)"
done

# ---------------------------------------------------------------- where KEDA runs: Forbidden is not absent
# probe_apisvc and probe_np print the return code of the common.sh readers:
# 1 is "none there", 2 "could not look".
probe_apisvc() { local r=0; wva_keda_apiservice_ns >/dev/null || r=$?; echo "rc=$r"; }
probe_np() { local r=0; wva_scaler_np_keda_ns "$NS" >/dev/null || r=$?; echo "rc=$r"; }
echo "== a Forbidden APIService or NetworkPolicy read naming notfound-prod is could-not-look"
run probe_apisvc STUB_APISERVICE=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -qx "rc=2" "$WORK/stdout"' "Forbidden APIService read returned '$(cat "$WORK/stdout")', want rc=2"
run probe_np STUB_NP=FORBIDDEN STUB_FORBID_NS=notfound-prod
ok 'grep -qx "rc=2" "$WORK/stdout"' "Forbidden NetworkPolicy read returned '$(cat "$WORK/stdout")', want rc=2"
run probe_apisvc
ok 'grep -qx "rc=1" "$WORK/stdout"' "NotFound APIService returned '$(cat "$WORK/stdout")', want rc=1"
run probe_np
ok 'grep -qx "rc=1" "$WORK/stdout"' "NotFound NetworkPolicy returned '$(cat "$WORK/stdout")', want rc=1"

# ---------------------------------------------------------------- the default with cert-manager never aborts
echo "== default with cert-manager: no right to read its Certificates: plaintext, nothing applied"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=present "STUB_CANI_NO='get certificates.cert-manager.io'"
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && [ ! -f "$WORK/cm-applied.yaml" ] && [ ! -s "$WORK/writes" ] && grep -q "may not get certificates.cert-manager.io in $NS" "$WORK/warnings"' "default, Certificates unreadable: exit $RC, on=$WVA_SCALER_TLS_ON, applied=$([ -f "$WORK/cm-applied.yaml" ] && echo yes || echo no), $(cat "$WORK/refusal" "$WORK/warnings")"

# cm_fail_case <label> <stub>: cert-manager fails one way; the default stays on
# plaintext and says why, WVA_SCALER_TLS=true and a controller that has TLS refuse.
cm_fail_case() {
    echo "== default with cert-manager: $1 is plaintext with a warning, not an abort"
    reset_store
    fresh_k
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=present "$2"
    ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml" && grep -q "stays on plaintext" "$WORK/warnings" && [ ! -s "$WORK/keda-writes" ]' "default, $1: exit $RC, on=$WVA_SCALER_TLS_ON, keda writes '$(tr '\n' ' ' <"$WORK/keda-writes")': $(cat "$WORK/refusal" "$WORK/warnings")"
    reset_store
    fresh_k
    put_deploy cert-manager
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=present "$2"
    ok '[ "$RC" = 1 ]' "kept TLS, $1: exit $RC, want a refusal (a default must never turn TLS off)"
}
cm_fail_case "a webhook that does not answer" STUB_DRYRUN=fail
cm_fail_case "a Certificate that never becomes Ready" STUB_WAIT=fail

# ---------------------------------------------------------------- the migrate command
echo "== the migrate command is a real make target, in this run's phase"
reset_store
run 'wva_scaler_tls_migrate_cmd true' INSTALL_PHASE_EXPLICIT=all
ok 'grep -q "make deploy-wva" "$WORK/stdout" && grep -q "INSTALL_PHASE=all" "$WORK/stdout" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/stdout" && ! grep -q "make install" "$WORK/stdout"' "single-pass install: '$(cat "$WORK/stdout")'"
run 'wva_scaler_tls_migrate_cmd false' WVA_APPLY_SCOPE=controller INSTALL_PHASE_EXPLICIT=wva
ok 'grep -q "make deploy-wva" "$WORK/stdout" && grep -q "INSTALL_PHASE=wva" "$WORK/stdout" && grep -q "WVA_SCALER_TLS=false" "$WORK/stdout"' "controller phase: '$(cat "$WORK/stdout")'"
# A phase the installer worked out itself is not repeated: on a real cluster
# the refusal said INSTALL_PHASE=wva while the docs gave the command without
# it, and both work.
run 'wva_scaler_tls_migrate_cmd false' WVA_APPLY_SCOPE=controller
ok '[ "$(cat "$WORK/stdout")" = "WVA_SCALER_TLS=false WVA_SCALER_TLS_MIGRATE=true NAMESPACE=$NS make deploy-wva" ]' "resolved phase: '$(cat "$WORK/stdout")', want the docs' command"

# ---------------------------------------------------------------- the client identity KEDA holds
echo "== controller phase: KEDA's certificate names another identity than the controller pins"
rm -rf "$STORE"; cp -r "$WORK/store.admin" "$STORE"
rm -f "$WORK/so.json"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent WVA_SCALER_CLIENT_SAN=new-client
ok '[ "$RC" = 0 ] && [ "$WVA_SCALER_TLS_ON" = false ] && ! grep -q -- "--external-scaler-cert-path" "$WORK/k/kustomization.yaml"' "SAN changed, default: TLS went on although every handshake would be rejected (exit $RC, on=$WVA_SCALER_TLS_ON)"
ok 'grep -q "carries the identity .wva-keda-client., but the controller accepts only .new-client." "$WORK/warnings" && grep -q "WVA_SCALER_CLIENT_SAN=new-client" "$WORK/warnings" && grep -q "rollout restart deployment/keda-operator -n keda" "$WORK/warnings"' "SAN changed, default: no warning with the fix: $(cat "$WORK/warnings")"
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent WVA_SCALER_CLIENT_SAN=new-client WVA_SCALER_TLS=true
ok '[ "$RC" = 1 ] && grep -q "accepts only .new-client." "$WORK/refusal"' "SAN changed, explicit: exit $RC, $(cat "$WORK/refusal")"
put_deploy openssl
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_APPLY_SCOPE=controller STUB_CM=absent WVA_SCALER_CLIENT_SAN=new-client
ok '[ "$RC" = 0 ] && grep -q "rejects every handshake" "$WORK/warnings" && ! grep -q "still works" "$WORK/warnings"' "SAN changed, TLS kept: the warning says it still works: $(cat "$WORK/warnings")"
echo "== verify: a marker naming another client identity is not usable"
store_tls_controller
marker_edit '.data.clientSan = "old-client"'
run wva_verify_scaler_tls
ok 'grep -q "KEDA.s side is not usable" "$WORK/warnings" && grep -q "old-client" "$WORK/warnings"' "verify, identity mismatch not flagged: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- migration waits for the rollout
echo "== ScaledObjects move only once the controller rollout finished"
reset_store
so_fixture app-so "$SO_PLAIN_ADDR"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls WVA_CONTROLLER_ROLLED_OUT=false
ok '[ ! -s "$WORK/patches" ] && grep -q "rollout did not finish" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr" && grep -q "make deploy-wva" "$WORK/warnings" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/warnings"' "rollout not finished: patched '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"
ok 'grep -q "WVA_CONTROLLER_ROLLED_OUT=true" "$ROOT/deploy/lib/infra_wva.sh"' "deploy_wva_controller does not record a finished rollout, so the migration never runs"

# A finished rollout is not enough: `rollout status` returns while the old Pod
# is still terminating and still an endpoint. Measured on an OpenShift cluster,
# KEDA then reconciled the moved ScaledObject against the old plaintext Pod,
# built the HPA without the metric, and never asked again.
echo "== ScaledObjects do not move while an old Pod is still behind the scaler Service"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
mkdir -p "$WORK/eps.d"
eps_json wva-old~ wva-new >"$WORK/eps.d/1.json"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls
ok '[ ! -s "$WORK/patches" ] && grep -q "not yet the only thing serving the scaler" "$WORK/warnings" && grep -q "wva-old" "$WORK/warnings" && grep -q "WVA_SCALER_TLS_MIGRATE=true" "$WORK/warnings"' "old endpoint: moved anyway, or not said why: patched '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

echo "== ScaledObjects do not move while an old controller Pod is terminating"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
stub_pods wva-old~ wva-new >"$WORK/pods.json"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls
ok '[ ! -s "$WORK/patches" ] && grep -q "still terminating: wva-old" "$WORK/warnings"' "terminating Pod: moved anyway: patched '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

echo "== ScaledObjects do not move while the old Pod holds the leader lease"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls STUB_LEASE_HOLDER=wva-old_77aa
ok '[ ! -s "$WORK/patches" ] && grep -q "leader lease" "$WORK/warnings"' "old leader: moved anyway: patched '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"

echo "== the move waits for the old endpoint to go, then moves"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
mkdir -p "$WORK/eps.d"
eps_json wva-old~ wva-new >"$WORK/eps.d/1.json"
eps_json wva-old~ wva-new >"$WORK/eps.d/2.json"
eps_json wva-new >"$WORK/eps.d/3.json"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls WVA_SCALER_SETTLE_TIMEOUT=20
ok '[ "$(grep -c "^get endpointslices" "$WORK/kubectl.log")" = 3 ] && grep -q "^patch scaledobject app-so -n apps --type merge" "$WORK/patches" && ! grep -q "NOT moved" "$WORK/warnings"' "waited move: read the endpoints $(grep -c '^get endpointslices' "$WORK/kubectl.log") times (want 3), patched '$(cat "$WORK/patches")', warnings $(cat "$WORK/warnings")"
ok '[ "$(grep -n "^get endpointslices" "$WORK/kubectl.log" | tail -1 | cut -d: -f1)" -lt "$(grep -n "^patch scaledobject" "$WORK/kubectl.log" | head -1 | cut -d: -f1)" ]' "waited move: patched before the endpoints settled: $(grep -E '^(get endpointslices|patch)' "$WORK/kubectl.log" | tr '\n' ';')"

echo "== EndpointSlices and Leases the installer may not read are not waited on, and it says so"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls STUB_EPS=FORBIDDEN STUB_LEASES=FORBIDDEN
ok 'grep -q "^patch scaledobject app-so -n apps --type merge" "$WORK/patches" && grep -q "Not waited on before the move: the EndpointSlices and the leader Lease" "$WORK/info"' "unreadable endpoints and lease: patched '$(cat "$WORK/patches")', info $(cat "$WORK/info")"

# KEDA asks for the metric spec once per spec change. If that call fails, the
# HPA stays on the CPU default, Ready and Active, for good.
echo "== after the move, an unwired HPA is re-wired by a trigger metadata change, then reported wired"
reset_store
put_deploy openssl
so_fixture app-so "$SO_PLAIN_ADDR"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls STUB_UNWIRED=until-rewire WVA_SCALER_REWIRE_TRIES=3
REWIRES="$(grep -c "^patch scaledobject app-so -n apps --type json" "$WORK/patches")"
ok '[ "$REWIRES" = 1 ] && grep -q "\"path\":\"/spec/triggers/0/metadata/wvaRewire\"" "$WORK/patches" && ! grep -q "has not wired the scaler.s metric onto the HPA" "$WORK/warnings" && grep -q "try 1 of 3" "$WORK/info"' "re-wire: $REWIRES re-wire patches (want 1): $(cat "$WORK/patches"); warnings $(cat "$WORK/warnings")"

echo "== the re-wire is bounded, touches only this scaler's triggers, and ends with the fix"
reset_store
put_deploy openssl
jq -n --arg a "$SO_PLAIN_ADDR" '{items: [{metadata: {namespace: "apps", name: "app-so"},
    spec: {triggers: [{type: "external-push", metadata: {scalerAddress: $a, modelID: "m"}},
                      {type: "external-push", metadata: {scalerAddress: "other-scaler.elsewhere.svc:9090"}}]}}]}' >"$WORK/so.json"
run wva_scaler_tls_finish WVA_SCALER_TLS_MIGRATE_ROWS=apps/app-so WVA_SCALER_TLS_MIGRATE_MODE=tls STUB_UNWIRED=always WVA_SCALER_REWIRE_TRIES=2
REWIRES="$(grep -c "^patch scaledobject app-so -n apps --type json" "$WORK/patches")"
ok '[ "$REWIRES" = 2 ] && ! grep -q "/spec/triggers/1/" "$WORK/patches"' "bounded re-wire: $REWIRES re-wire patches (want 2, trigger 0 only): $(cat "$WORK/patches")"
ok 'grep -q "has not wired the scaler.s metric onto the HPA" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr" && grep -q "kubectl patch scaledobject app-so -n apps --type json -p" "$WORK/stderr"' "bounded re-wire: no warning naming it with the fix: warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"

echo "== verify: an HPA without the scaler's external metric is flagged, with the fix"
reset_store
put_deploy openssl
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run wva_verify_scaler_wiring STUB_UNWIRED=hpa
ok '[ "$RC" = 1 ] && grep -q "did not wire the scaler.s metric onto the HPA" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr" && grep -q "kubectl patch scaledobject app-so -n apps --type json" "$WORK/stderr"' "verify, HPA on the CPU default: exit $RC (want 1), warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"
# The likely cause on older KEDA, and the one command that re-wires them all,
# in this install's shape.
# Nothing is glued to the command, so a copy of it is just the command.
ok 'grep -q "Or re-wire them all:  NAMESPACE=wva-check make scaledobjects-rewire  On KEDA 2.19 and older this follows a WVA controller restart or outage, a KEDA bug fixed in 2.20: upgrade KEDA to 2.21 (docs/reference/keda.md#unwired-hpa-before-keda-220)" "$WORK/warnings"' "verify, HPA on the CPU default: no line naming KEDA below 2.20 and make scaledobjects-rewire: $(cat "$WORK/warnings")"
run wva_verify_scaler_wiring STUB_UNWIRED=hpa WVA_SCOPE=cluster
ok '[ "$RC" = 1 ] && grep -q "Or re-wire them all:  make scaledobjects-rewire SCOPE=cluster WVA_NS=wva-check  On KEDA 2.19" "$WORK/warnings"' "verify, cluster scope: the re-wire command must carry SCOPE=cluster WVA_NS=wva-check: exit $RC (want 1), warnings $(cat "$WORK/warnings")"
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 1 ] && grep -q "apps/app-so" "$WORK/stderr"' "verify, empty externalMetricNames: exit $RC (want 1)"
run wva_verify_scaler_wiring
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/warnings" ] && [ ! -s "$WORK/patches" ]' "verify, wired: exit $RC, warnings $(cat "$WORK/warnings"), patched '$(cat "$WORK/patches")' (verify writes nothing)"
so_fixture app-so "other-scaler.elsewhere.svc:9090"
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/warnings" ]' "verify, a ScaledObject naming another scaler: flagged: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- an unreadable controller and an explicit mode
probe_addr_mode() { echo "mode=$(wva_scaler_address_mode "$NS")"; }
# probe_so_addr sources scaledobject.sh, as the make targets do, and prints
# the address so_scaler_addr writes.
probe_so_addr() { . "$WORK/scaledobject.sh"; echo "addr=$(so_scaler_addr "$NS")"; }
echo "== an unreadable controller: an explicit WVA_SCALER_TLS decides the address"
reset_store
run probe_addr_mode STUB_DEPLOY=FORBIDDEN WVA_SCALER_TLS=true
ok 'grep -qx "mode=tls" "$WORK/stdout"' "explicit true, Deployment Forbidden: '$(cat "$WORK/stdout")'"
run probe_addr_mode STUB_DEPLOY=FORBIDDEN WVA_SCALER_TLS=false
ok 'grep -qx "mode=plaintext" "$WORK/stdout"' "explicit false, Deployment Forbidden: '$(cat "$WORK/stdout")'"
run probe_addr_mode STUB_DEPLOY=FORBIDDEN
ok 'grep -qx "mode=unknown" "$WORK/stdout"' "unset, Deployment Forbidden: '$(cat "$WORK/stdout")'"
run probe_so_addr STUB_DEPLOY=FORBIDDEN WVA_SCALER_TLS=true
ok 'grep -qx "addr=$SO_TLS_ADDR" "$WORK/stdout" && ! grep -q "is unknown" "$WORK/warnings"' "so_scaler_addr, explicit true, Deployment Forbidden: '$(cat "$WORK/stdout")' $(cat "$WORK/warnings" "$WORK/refusal")"
WVA_SCALER_TLS=true PATH="$WORK/kbin:$PATH" bash "$ROOT/deploy/warmpool.sh" create -n tenant --name pool \
    --proxy-image example.invalid/proxy:v1 --wva-namespace wva-system \
    --models 2 --model-size 8B --accelerator NVIDIA-H100-80GB-HBM3 --dry-run >"$WORK/wp.out" 2>"$WORK/wp.err"
ok 'grep -q "scalerAddress: wva-external-scaler.wva-system.svc:9090" "$WORK/wp.out" && grep -q "name: wva-external-scaler-wva-system" "$WORK/wp.out" && ! grep -q "is unknown" "$WORK/wp.err"' "warmpool, explicit true, controller unreadable: $(grep -n "scalerAddress\|authenticationRef\|name: wva-external" "$WORK/wp.out" | tr '\n' ' ') $(grep -c "is unknown" "$WORK/wp.err") unknown-mode warnings"

# ---------------------------------------------------------------- the marker outlives what it records
echo "== an uninstall that cannot remove KEDA's side keeps the marker"
reset_store
fresh_k
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent
run wva_scaler_tls_undeploy STUB_CM=absent STUB_CTA=FORBIDDEN
ok '[ -f "$STORE/configmap__wva-scaler-keda-auth.json" ] && ! grep -q "delete configmap wva-scaler-keda-auth" "$WORK/kubectl.log" && grep -q "wva-scaler-keda-auth in $NS is kept" "$WORK/warnings" && grep -q "undeploy-wva" "$WORK/warnings"' "CTA unreadable: marker deleted, or kept without saying so: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- the client key under set -e, and HUP
echo "== set -e: a failing expiry read while KEDA's key is on disk leaves nothing behind"
reset_store
fresh_k
rm -rf "$WORK/tmpd3"
mkdir -p "$WORK/tmpd3"
# prepare_strict runs the install step as install.sh does, under set -e and pipefail.
prepare_strict() { set -eo pipefail; wva_scaler_tls_prepare "$1"; }
run 'prepare_strict "$WORK/k/kustomization.yaml"' STUB_CM=absent "export TMPDIR=$WORK/tmpd3" \
    'openssl() { case "$*" in *-enddate*) return 1 ;; esac; command openssl "$@"; }'
ok '[ "$RC" = 0 ] && [ -z "$(ls -A "$WORK/tmpd3")" ] && grep -q "configmap/wva-scaler-keda-auth" "$WORK/keda-writes"' "set -e, enddate fails: exit $RC, left in TMPDIR: $(cd "$WORK/tmpd3" && find . -type f | tr '\n' ' '), keda writes '$(tr '\n' ' ' <"$WORK/keda-writes")'"

echo "== a hangup while a leaf is signed leaves no key on disk, and the caller's EXIT trap still runs"
reset_store
rm -rf "$WORK/tmpd4" "$WORK/outer-exit"
mkdir -p "$WORK/tmpd4"
run 'arm_outer_exit ; wva_scaler_tls_provision_openssl' "export TMPDIR=$WORK/tmpd4" \
    'openssl() { case "$*" in "x509 -req"*) kill -HUP "$BASHPID" ;; esac; command openssl "$@"; }'
ok '[ "$RC" = 129 ] && [ -z "$(ls -A "$WORK/tmpd4")" ] && [ -s "$WORK/outer-exit" ]' "hangup: exit $RC, outer EXIT ran=$([ -s "$WORK/outer-exit" ] && echo yes || echo no), left behind: $(cd "$WORK/tmpd4" && find . -type f | tr '\n' ' ')"

# ---------------------------------------------------------------- KEDA-side refusals come before any write
# kside_case <label> <setup> <run args...>: WVA_SCALER_TLS=true, and KEDA's
# side refuses; nothing may be written in the controller namespace either.
kside_case() {
    local label="$1" setup="$2"
    shift 2
    echo "== explicit TLS, $label: refused before anything is written"
    reset_store
    fresh_k
    eval "$setup"
    run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' WVA_SCALER_TLS=true "$@"
    ok '[ "$RC" = 1 ] && [ ! -s "$WORK/writes" ] && [ ! -s "$WORK/keda-writes" ]' "$label: exit $RC, wrote '$(cat "$WORK/writes" "$WORK/keda-writes" | tr '\n' ' ')': $(cat "$WORK/refusal")"
}
kside_case "a foreign ClusterTriggerAuthentication" \
    'jq -n --arg n "$CTA" "{kind: \"ClusterTriggerAuthentication\", metadata: {name: \$n}}" >"$STORE/clustertriggerauthentications.keda.sh__$CTA.json"' STUB_CM=absent
kside_case "a foreign marker" \
    'jq -n "{kind: \"ConfigMap\", metadata: {name: \"wva-scaler-keda-auth\"}, data: {}}" >"$STORE/configmap__wva-scaler-keda-auth.json"' STUB_CM=absent
kside_case "KEDA not installed" ':' STUB_CM=absent STUB_KEDA=absent

# ---------------------------------------------------------------- guards a mutation found missing
echo "== a controller that has TLS, re-installed without openssl, is a refusal, not plaintext"
reset_store
fresh_k
put_deploy openssl
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent "PATH=$WORK/noopenssl"
ok '[ "$RC" = 1 ] && grep -q "openssl" "$WORK/refusal" && [ ! -s "$WORK/writes" ] && [ ! -s "$WORK/keda-writes" ]' "kept TLS, no openssl: exit $RC, wrote '$(tr '\n' ' ' <"$WORK/writes")': $(cat "$WORK/refusal" "$WORK/warnings")"

echo "== KEDA's operator Deployment unreadable: it may be running, so the reinstall warning stays"
reset_store
fresh_k
so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
run 'wva_scaler_tls_prepare "$WORK/k/kustomization.yaml"' STUB_CM=absent STUB_KEDA_OPERATOR=FORBIDDEN
ok '[ "$RC" = 0 ] && grep -q "written for the first time while KEDA.s operator is running" "$WORK/warnings"' "KEDA operator Forbidden: read as absent, no reinstall warning: $(cat "$WORK/warnings")"

# ---------------------------------------------------------------- KEDA still presents an old client certificate
# Measured on an OpenShift cluster: after an uninstall and a reinstall in the
# same namespace, KEDA's operator kept its pooled connection to the scaler
# address, opened with the OLD client certificate. The controller refused it
# about once a minute ("certificate signed by unknown authority", from KEDA's
# Pod IP), the HPA stayed on CPU, and only a KEDA operator restart fixed it.
# The re-wire patch cannot help, so it must not be what the operator is told.

# refusal_line <ip>: one controller log line refusing a handshake from <ip>
# for a client certificate signed by an unknown authority, as the console
# encoder writes it.
refusal_line() {
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' 2026-10-16T17:41:21Z INFO external-scaler scaler/refusal_log.go:118 \
        'KEDA external scaler refused a TLS handshake; this client reaches no call' \
        "{\"remote\": \"$1:34856\", \"reason\": \"tls: failed to verify certificate: x509: certificate signed by unknown authority (possibly because of \\\"crypto/rsa: verification error\\\" while trying to verify candidate authority certificate \\\"wva-external-scaler-ca.$NS\\\")\", \"refusedSinceLastLine\": 7}" \
        >>"$WORK/ctrl.log"
}
# stale_case: the admin and controller phases ran, one ScaledObject is on the
# TLS address, and the controller's log holds whatever the case adds.
stale_case() {
    store_tls_controller
    so_fixture app-so "$SO_TLS_ADDR" "{\"name\":\"$CTA\",\"kind\":\"ClusterTriggerAuthentication\"}"
}
RESTART="rollout restart deployment/keda-operator -n keda"

echo "== verify: refusals for an unknown authority from KEDA's Pod: names the ScaledObjects and the restart, does not restart KEDA, does not fail"
# KEDA is shared: by default the install does not restart it, and does not
# fail over it either. It names what stays unwired and the exact command.
stale_case
refusal_line 10.128.5.168
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 0 ] && grep -q "still presents an OLD client certificate" "$WORK/warnings" && grep -q "keda-operator-7f9c" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && grep -q "apps/app-so" "$WORK/stderr" && grep -q "stay unwired" "$WORK/warnings" && grep -q "Not restarted" "$WORK/warnings" && ! grep -q "^rollout restart" "$WORK/kubectl.log"' "verify, stale KEDA connection, default: exit $RC (want 0), warnings $(cat "$WORK/warnings"), kubectl $(grep rollout "$WORK/kubectl.log")"
ok '! grep -q "kubectl patch scaledobject" "$WORK/stderr" && ! grep -q "Change it again" "$WORK/warnings"' "verify, stale KEDA connection: still offers the re-wire: $(cat "$WORK/stderr")"
# A ScaledObject KEDA wired before the reinstall is refused the same way.
run wva_verify_scaler_wiring
ok '[ "$RC" = 0 ] && grep -q "still presents an OLD client certificate" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings"' "verify, stale KEDA connection with a wired HPA: exit $RC (want 0), warnings $(cat "$WORK/warnings")"
# KEDA_RESTART=true: the install restarts KEDA's operator itself.
run wva_verify_scaler_wiring STUB_UNWIRED=always KEDA_RESTART=true
ok 'grep -q "^rollout restart deployment/keda-operator -n keda" "$WORK/kubectl.log" && grep -q "KEDA_RESTART=true: restarted KEDA.s operator in keda" "$WORK/info" && ! grep -q "Not restarted" "$WORK/warnings"' "verify, stale KEDA connection, KEDA_RESTART=true: KEDA not restarted: $(cat "$WORK/info" "$WORK/warnings"; grep rollout "$WORK/kubectl.log")"

echo "== verify: KEDA refusing the scaler's new certificate (bad certificate) is the same stale connection"
# The other direction, measured on kind after an uninstall and reinstall: KEDA,
# holding the CA it cached, rejects the controller's new serving certificate,
# and the controller logs "remote error: tls: bad certificate".
stale_case
printf '%s\t%s\t%s\t%s\t%s\t%s\n' 2026-10-19T09:42:57Z INFO external-scaler scaler/refusal_log.go:118 \
    'KEDA external scaler refused a TLS handshake; this client reaches no call' \
    '{"remote": "10.128.5.168:45482", "reason": "remote error: tls: bad certificate", "refusedSinceLastLine": 1}' \
    >>"$WORK/ctrl.log"
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 0 ] && grep -q "still presents an OLD client certificate" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && ! grep -q "Change it again" "$WORK/warnings"' "verify, KEDA refusing the new serving certificate: exit $RC (want 0), warnings $(cat "$WORK/warnings")"

echo "== verify: the certificate KEDA was given is not from the current CA: setup-prereqs and the restart"
stale_case
refusal_line 10.128.5.168
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 -subj /CN=another-ca \
    -keyout "$WORK/other.key" -out "$WORK/other.crt" >/dev/null 2>&1
jq --arg ca "$(base64 <"$WORK/other.crt" | tr -d '\n')" '.data["ca.crt"] = $ca' "$STORE/secret__$CLIENT.json" >"$WORK/client.json" \
    && mv "$WORK/client.json" "$STORE/secret__$CLIENT.json"
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 1 ] && grep -q "not signed by the scaler.s current client CA" "$WORK/warnings" && grep -q "setup-prereqs" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && ! grep -q "OLD client certificate" "$WORK/warnings" && ! grep -q "kubectl patch scaledobject" "$WORK/stderr"' "verify, KEDA given a certificate from another CA: exit $RC, warnings $(cat "$WORK/warnings")"

echo "== verify: refusals from a Pod that is not KEDA's are not this case"
stale_case
refusal_line 10.131.0.44
run wva_verify_scaler_wiring STUB_UNWIRED=always
ok '[ "$RC" = 1 ] && ! grep -q "OLD client certificate" "$WORK/warnings" && ! grep -q "$RESTART" "$WORK/warnings" && grep -q "Change it again" "$WORK/warnings" && grep -q "kubectl patch scaledobject app-so -n apps --type json" "$WORK/stderr"' "verify, refusals from another host: exit $RC, warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"
run wva_verify_scaler_wiring
ok '[ "$RC" = 0 ] && [ ! -s "$WORK/warnings" ]' "verify, refusals from another host, HPA wired: exit $RC, warnings $(cat "$WORK/warnings")"

echo "== verify: an unreadable controller log or KEDA Pods: says so, and lists both fixes"
stale_case
refusal_line 10.128.5.168
run wva_verify_scaler_wiring STUB_UNWIRED=always STUB_LOGS=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "controller.s log in $NS cannot be read" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && grep -q "Change it again" "$WORK/warnings" && grep -q "kubectl patch scaledobject app-so -n apps --type json" "$WORK/stderr"' "verify, log unreadable: exit $RC, warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"
run wva_verify_scaler_wiring STUB_UNWIRED=always STUB_KEDA_PODS=FORBIDDEN
ok '[ "$RC" = 1 ] && grep -q "Pods in keda, where KEDA runs, cannot be read" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && grep -q "kubectl patch scaledobject app-so -n apps --type json" "$WORK/stderr"' "verify, KEDA Pods unreadable: exit $RC, warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"

echo "== after a move: the re-wire loop stops at a stale KEDA connection and names the restart"
stale_case
refusal_line 10.128.5.168
run 'wva_scaler_ensure_wired apps/app-so' STUB_UNWIRED=always WVA_SCALER_REWIRE_TRIES=3
REWIRES="$(grep -c "^patch scaledobject" "$WORK/patches")"
ok '[ "$RC" = 1 ] && [ "$REWIRES" = 0 ] && grep -q "still presents an OLD client certificate" "$WORK/warnings" && grep -q "$RESTART" "$WORK/warnings" && grep -q "Not restarted" "$WORK/warnings" && ! grep -q "^rollout restart" "$WORK/kubectl.log" && ! grep -q "kubectl patch scaledobject" "$WORK/stderr" && ! grep -q "change it again" "$WORK/warnings"' "re-wire, stale KEDA connection: exit $RC, $REWIRES re-wire patches (want 0), warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"
run 'wva_scaler_ensure_wired apps/app-so' STUB_UNWIRED=always WVA_SCALER_REWIRE_TRIES=2 STUB_LOGS=FORBIDDEN
REWIRES="$(grep -c "^patch scaledobject" "$WORK/patches")"
ok '[ "$RC" = 1 ] && [ "$REWIRES" = 2 ] && grep -q "$RESTART" "$WORK/warnings" && grep -q "kubectl patch scaledobject app-so -n apps --type json" "$WORK/stderr"' "re-wire, log unreadable: exit $RC, $REWIRES re-wire patches (want 2), both fixes not listed: warnings $(cat "$WORK/warnings") stderr $(cat "$WORK/stderr")"

echo ""
if [ "$FAIL" -gt 0 ]; then
    echo "external-scaler TLS: $FAIL failed, $PASS passed"
    exit 1
fi
if [ "$PASS" = 0 ]; then
    echo "FAIL: no assertion ran"
    exit 1
fi
echo "external-scaler TLS OK ($PASS assertions)"

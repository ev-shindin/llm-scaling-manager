#!/usr/bin/env bash
# Two models, two EPPs, anti-phase bursts, with the warm pool on and off.
#
# THE QUESTION THIS ANSWERS
# -------------------------
# A warm pool is insurance: every pool Pod holds its accelerators continuously,
# lending or idle, so a pool of N lowers the maximum fleet by N. The case for
# paying that is that ONE pool covers MANY models -- an argument nothing in this
# repo measures. Every warm-pool number taken so far is one model bridging its
# own scale-up, where the pool is pure overhead at steady state and the only
# question is whether the bridge beats the ramp.
#
# With two models whose bursts do not coincide, the pool is shared: it lends to
# whichever model is rising and is handed back in time for the other. One GPU of
# insurance covers two ramps. If that does not show up here, the shared-pool
# argument is wrong and this run is how we find out.
#
# WHY ANTI-PHASE, SPECIFICALLY
# ----------------------------
# Because it isolates the sharing. The SUM of the two models' demand is flat, so
# a cluster that could see the whole picture would hold its total fleet constant
# and move replicas between the models. Nothing does see it -- each model scales
# on its own signal -- so without a pool both models pay a full cold load on
# every rise while the other model is giving capacity back.
#
# FOR THE COMPARISON TO MEAN ANYTHING
# -----------------------------------
# The two arms must differ in ONE thing. Four ways they silently did not, each
# of which produces a complete and plausible table:
#
#   * The arms were allowed different ceilings. Every arm gets MAX_REPLICAS
#     per model and the pool sits on top of that; what the pool costs is
#     measured in accelerator-seconds, not imposed through the cap. `run` sets
#     the ceiling; it does not hope.
#   * The fleet was not reset. With scale-down stabilization at 300s and nopool
#     always first, the second arm starts on an already-scaled fleet and pays no
#     cold load at all. `reset` pins both models back to MIN_REPLICAS, waits for
#     it, and only then releases them.
#   * The pool was COLD. Then the first burst pays a model load INTO the pool on
#     top of the replica's own, and the arm reports the cost of a pool nobody
#     would operate that way. `warm` pins both models resident and waits.
#   * The load differed. Both arms render the SAME inference-perf profiles from
#     the same seed, and the report refuses two arms whose schedules differ.
#
# WHO GENERATES THE LOAD
# ----------------------
# inference-perf, the llm-d benchmark harness's own generator, one instance per
# model, in two containers of one Pod. Not a bespoke client: the harness
# tokenizes with each model's own tokenizer (so `input_tokens` means input
# tokens for both of two different models), expresses the schedule as stages
# (so anti-phase is two mirrored ladders rather than a loop keeping time), and
# reports its OWN scheduling delay -- the quantity that decides whether the
# driver or the cluster produced the latency being compared.
#
# STEP BY STEP, DELIBERATELY. The expensive failures here are the ones found at
# minute 40 of a 45-minute run.
#
#   two_model_pool.sh preflight     what the run needs, before anything exists
#   two_model_pool.sh standup       ONE standup, two stacks, one gateway
#   two_model_pool.sh verify        both models answer; two EPPs; pool state
#   two_model_pool.sh pool-create   the shared warm pool
#   two_model_pool.sh pool-delete   remove it
#   two_model_pool.sh warm          pin BOTH models resident, and wait
#   two_model_pool.sh reset         both models back to MIN_REPLICAS, quiesced
#   two_model_pool.sh run <arm>     drive the load, sample GPUs, collect
#                                   arms: nopool (the baseline), pool, and
#                                   floor -- no pool, every model held at
#                                   FLOOR_REPLICAS, the over-provisioning a
#                                   pool competes against. Or the
#                                   utilization-share arms, under one quota:
#                                   today (no optimizer block), shadow (the
#                                   optimizer computing) and share (acting)
#   two_model_pool.sh report        compare every arm that ran against nopool;
#                                   writes report.md, report.json and SVG plots
#   two_model_pool.sh report-share  compare shadow and share against today, and
#                                   what the optimizer recorded in each
#   two_model_pool.sh status        what exists, and what holds accelerators
#   two_model_pool.sh teardown      remove everything this created
#   two_model_pool.sh node-data     whether the controller has the node data a
#                                   mixed-size share run needs, and the exact
#                                   admin commands when it does not;
#                                   `node-data grant` makes the pod-list grant
#   two_model_pool.sh share-arms    REPEATS x (reset, run) for today, shadow
#                                   and share, a report per repeat, then
#                                   report-repeats; TEARDOWN_ON_EXIT=1 tears
#                                   down on every exit path
#   two_model_pool.sh report-repeats  per-burst TTFT, spread across repeats,
#                                   transfers by outcome from the controller
#                                   log, GPU-seconds, and what it shows
#
# Env: BENCHMARK_NAMESPACE (required); STACK_A/STACK_B and MODEL_A/MODEL_B must
# match the scenario spec; the load shape below. KUBECONFIG/KUBE_CONTEXT as
# usual. BENCH_SIM=1 stands simulator stacks up instead of the harness's (a
# kind dry run of the whole flow, no GPUs): see sim_standup.
#
# THREE OR FOUR MODELS: MODEL_COUNT=3|4 (default 2) adds STACK_C/MODEL_C and
# STACK_D/MODEL_D, with HIGH_RPS_C/D, LOW_RPS_C/D, MAX_REPLICAS_C/D and GPUS_PER_REPLICA_C/D
# beside the A/B ones. The bursts then ROTATE -- b, c, d, a, each shifted from
# the last by period/N -- under the same quota and the same arms, and the
# default scenario is guides/n-model-share, stood up with only the first N of
# its stacks. MODEL_COUNT=2 is the two-model run exactly as before.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

NS="${BENCHMARK_NAMESPACE:-}"
# The scenario spec defines both stacks, and its stack NAMES are the path
# prefixes the shared HTTPRoute routes on (`/{stack}/v1/...`) -- which is the
# only name this driver can rely on. The objects in a stack are named after a
# shortName the harness GENERATES and that the scenario cannot override, so
# every lookup here goes through the route rather than through a name.
# Whether the caller named the scenario: teardown trusts a recorded standup
# spec over a default, and refuses rather than guess (see verb_teardown).
BENCH_SPEC_GIVEN="${BENCH_SPEC:+1}"
# How many models burst in rotation: 2 (the anti-phase pair), 3 or 4. Checked
# below, once die exists.
MODEL_COUNT="${MODEL_COUNT:-2}"
if [ "$MODEL_COUNT" = 2 ]; then
    BENCH_SPEC="${BENCH_SPEC:-guides/two-model-warm-pool}"
else
    # Four 1-GPU stacks; the standup keeps the first MODEL_COUNT of them.
    BENCH_SPEC="${BENCH_SPEC:-guides/n-model-share}"
fi
# The ConfigMap the standup records its scenario in, for the teardown.
STANDUP_RECORD="wva-two-model-standup"
STACK_A="${STACK_A:-llama-31-8b}"
STACK_B="${STACK_B:-qwen3-8b}"
MODEL_A="${MODEL_A:-unsloth/Meta-Llama-3.1-8B-Instruct}"
MODEL_B="${MODEL_B:-Qwen/Qwen3-8B}"
# Models three and four, used only when MODEL_COUNT says so. Their defaults
# are the third and fourth stacks of guides/n-model-share, each one GPU per
# replica at tensor 1.
STACK_C="${STACK_C:-qwen3-30b-a3b}"
MODEL_C="${MODEL_C:-Qwen/Qwen3-30B-A3B-FP8}"
STACK_D="${STACK_D:-qwen3-32b}"
MODEL_D="${MODEL_D:-Qwen/Qwen3-32B}"

# The load shape. Defaults give a ~34 minute run: 2 min lead-in, then four
# 8-minute phases.
#
# A PHASE MUST OUTLAST SCALE-DOWN STABILIZATION, which this repo sets to 300s
# (fast up, slow down -- under-provisioning costs TTFT irrecoverably, over-
# provisioning only costs money). At 360s, model B still holds the replicas it
# grew during its own burst for most of model A's rise, so A never actually
# competes for capacity and the anti-phase premise is not exercised. 480s leaves
# ~3 minutes of genuinely contended time per phase.
PHASE_SECONDS="${PHASE_SECONDS:-480}"
CYCLES="${CYCLES:-2}"
LEAD_IN="${LEAD_IN:-120}"
LOW_RPS="${LOW_RPS:-3}"
# 6, not 9. MEASURED at 9: once fully scaled, Llama served it comfortably
# (steady-state p50 83ms) while Qwen did not -- steady-state p50 5717ms and
# 5841ms, which is an unbounded queue, not a transient. 9 rps straddles the two
# models' capacity, and for the slower one the run measured which arm was less
# broken rather than what a bridge is worth. 6 sits inside both models' capacity
# at two replicas while still needing a scale-up from one.
HIGH_RPS="${HIGH_RPS:-9}"
# Model B's burst rate, separately, because the two models do not have the same
# per-replica capacity. MEASURED: at 9 rps Llama served comfortably once scaled
# (steady-state p50 83ms) while Qwen drowned (steady-state p50 5717ms, an
# unbounded queue rather than a transient); at 6 rps Qwen was exercised properly
# and Llama never needed a second replica at all -- its rise window was
# indistinguishable from its steady state. One rate cannot do both.
#
# It also serves the premise rather than bending it: the claim is that the SUM
# OF DEMAND is flat, and with equal request rates against unequal capacity the
# sum of ACCELERATOR demand is not.
HIGH_RPS_B="${HIGH_RPS_B:-8}"
# Models C and D's rates, in a run of three or four: each between its bursts
# (LOW_RPS_C/D) and at its burst (HIGH_RPS_C/D). Sized so each model needs ONE
# replica between bursts and about MAX_REPLICAS (3) at its peak.
#
# AN ESTIMATE, NOT A MEASUREMENT. The anchor is measured: with 1000 tokens in
# and 500 out, an 8B model holds about 3 rps per replica (Llama served 9 rps on
# three, Qwen3-8B needed 8 to stay out of an unbounded queue). Decode is
# memory-bound, so a replica's rate scales with the bytes each step reads:
#   C, Qwen3-30B-A3B-FP8: ~30 GB of FP8 experts, most of them touched per step
#      at a batch of tens, against 16 GB for an 8B model -> ~1.7x slower,
#      ~1.75 rps a replica. 1.5 rps is 0.9 of one replica; 5 rps is 2.9.
#   D, Qwen3-32B (bf16, maxNumSeq 32, one H200): 64 GB a step -> ~4x slower,
#      ~0.75 rps a replica. 0.5 rps is 0.7 of one replica; 2 rps is 2.7.
# Measure them before trusting a result that leans on C or D.
HIGH_RPS_C="${HIGH_RPS_C:-5}"
LOW_RPS_C="${LOW_RPS_C:-1.5}"
HIGH_RPS_D="${HIGH_RPS_D:-2}"
LOW_RPS_D="${LOW_RPS_D:-0.5}"
INPUT_TOKENS="${INPUT_TOKENS:-1000}"
OUTPUT_TOKENS="${OUTPUT_TOKENS:-500}"
# Loader b's shape. Defaults to a's; a single-stack P/D run gives the two
# loaders different shapes (prompt-heavy, then decode-heavy) so the model's
# prefill and decode take turns at needing the GPUs.
INPUT_TOKENS_B="${INPUT_TOKENS_B:-$INPUT_TOKENS}"
OUTPUT_TOKENS_B="${OUTPUT_TOKENS_B:-$OUTPUT_TOKENS}"
REQUEST_TIMEOUT="${REQUEST_TIMEOUT:-300}"
SEED="${SEED:-1729}"
# The prompts. `synthetic` slices every prompt from a random offset into
# inference-perf's corpus, so no two requests share a prefix -- and that is
# what makes a scale-up measurable. llm-d's shipped scheduling profile weights
# the prefix-cache scorer highest, and its hashes chain from the first token,
# so a replica that cached a prompt's opening wins every later request sharing
# it regardless of its queue, and a replica that cached nothing never receives
# one. MEASURED with `shared_prefix` at 32 groups x 64 prompts: per model the
# busiest engine did 99.0% and 98.7% of the work over a whole arm and every
# replica the autoscaler added did 0.3-2.0%. With `synthetic`, an EMPTY second
# replica took 49.4% of 9 rps from its first minute. `shared_prefix` is kept
# to reproduce the pin, with PREFIX_GROUPS distinct prefixes.
LOAD_DATA="${LOAD_DATA:-synthetic}"
PREFIX_GROUPS="${PREFIX_GROUPS:-32}"
# Seconds at the opening of each phase measured as their own stage. The harness
# reports a latency distribution per STAGE and nothing finer, so this is what a
# rise window can be -- and the rise window is this scenario's headline number.
# Must be shorter than PHASE_SECONDS or the phase is not cut at all and every
# scale-up is averaged into eight minutes of steady state.
#
# 240, not 90, and the difference decided a whole run. MEASURED: from a burst
# starting to the model's OWN second replica reporting READY took 127s, 157s,
# 218s. The pool lent 99-100s ahead of that every time -- the mechanism works --
# but a 90s window sees only the first third of the ramp, and on one rise the
# lend itself did not arrive until +119s, AFTER the window had closed. That rise
# then read as the pool being 1992ms WORSE. The window has to contain the event
# it is named after.
RISE_WINDOW="${RISE_WINDOW:-240}"
# Seconds of BOTH models at the low rate between consecutive bursts. 90, sized
# from measurement: at 30 the two ladders drifted 59s apart over a 2130s run and
# the bursts genuinely overlapped for 32s. The drift ACCUMULATES -- it does not
# reset at a boundary -- so the band has to clear the whole run's divergence,
# not one stage's. A stage
# ends when its in-flight requests drain and the BURSTING model drains slower,
# so the two models do not cross a boundary together -- measured on a multi-node H200 cluster,
# per-stage drains of 4-16s and a net divergence that reached 6.1s and changed
# sign with the burst. Without a band that divergence is time when both models
# burst at once, which a pool can only half serve, and it would be recorded as
# the pool failing at the thing this scenario measures. The report refuses a run
# whose measured divergence exceeds this band.
OVERLAP_SECONDS="${OVERLAP_SECONDS:-90}"
# Seconds between creating the load Job and the instant both containers start
# their ladders. It has to cover the image pull and the two tokenizer
# downloads; each container reports whether it made it, and the driver refuses
# the arm if either did not.
PRELOAD_GRACE="${PRELOAD_GRACE:-420}"
# How long past the schedule the loaders may take to DRAIN their last stage and
# WRITE their reports. inference-perf's report step alone was measured at ~5
# minutes (09:18:46 "Generating Reports" to 09:23:53 first file written, the
# nopool arm of 2026-09-18), on top of the last stage draining, which the
# bursting model does slowly. 600s was not enough: a floor arm whose load
# finished on schedule was thrown away 3 minutes before its reports landed.
RESULT_GRACE="${RESULT_GRACE:-1200}"

MAX_REPLICAS="${MAX_REPLICAS:-3}"
MIN_REPLICAS="${MIN_REPLICAS:-1}"
# Per-role bounds for a P/D model; both default to the shared ones, so an
# aggregated run is unchanged. A prefill role rarely needs the replicas its
# decode does, and one ceiling for both would let it hold GPUs it never uses.
MIN_PREFILL="${MIN_PREFILL:-$MIN_REPLICAS}"
MIN_DECODE="${MIN_DECODE:-$MIN_REPLICAS}"
MAX_PREFILL="${MAX_PREFILL:-$MAX_REPLICAS}"
MAX_DECODE="${MAX_DECODE:-$MAX_REPLICAS}"
# The THIRD arm. `floor` is what a pool competes against in practice: no pool,
# but every model kept at FLOOR_REPLICAS at all times -- over-provisioning as
# insurance. It pays for the extra replicas for the whole run, in the way the
# pool pays for its Pods, and the question is which insurance is cheaper for
# what it buys. Its ceiling is MAX_REPLICAS, the same 2 x MAX x GPUS peak as
# the other two arms.
FLOOR_REPLICAS="${FLOOR_REPLICAS:-2}"
GPUS_PER_REPLICA="${GPUS_PER_REPLICA:-1}"
# PER MODEL, for a group of mixed replica sizes (two-model-mixed-sizes: a 2-GPU
# MoE beside a 1-GPU model). Empty reads each stack's size from the scenario --
# its nvidia.com/gpu request, else tensor x dataLocal -- and falls back to
# GPUS_PER_REPLICA. Set, it must agree with the scenario: preflight and standup
# refuse a disagreement, because a run priced on one size and deployed at
# another prices GPUs nobody held. One global figure counted a 3 x 2-GPU model
# as 3 GPUs, and preflight placed half its peak.
GPUS_PER_REPLICA_A="${GPUS_PER_REPLICA_A:-}"
GPUS_PER_REPLICA_B="${GPUS_PER_REPLICA_B:-}"
GPUS_PER_REPLICA_C="${GPUS_PER_REPLICA_C:-}"
GPUS_PER_REPLICA_D="${GPUS_PER_REPLICA_D:-}"
# Per-model ceilings, defaulting to MAX_REPLICAS. The standup registers both at
# the larger; every arm then sets each model's own on its ScaledObject.
MAX_REPLICAS_A="${MAX_REPLICAS_A:-$MAX_REPLICAS}"
MAX_REPLICAS_B="${MAX_REPLICAS_B:-$MAX_REPLICAS}"
MAX_REPLICAS_C="${MAX_REPLICAS_C:-$MAX_REPLICAS}"
MAX_REPLICAS_D="${MAX_REPLICAS_D:-$MAX_REPLICAS}"
# The node every model replica is pinned to (kubernetes.io/hostname), or empty
# for none. A 1-GPU donor set can fund a 2-GPU replica only on ONE node, and on
# a shared cluster the scheduler otherwise spreads the small model's pods.
# Preflight checks that node alone has the GPUs the run peaks at.
BENCH_NODE="${BENCH_NODE:-}"
# BENCH_NODES=<node>,<node>,...: the nodes every model replica may land on, by
# node affinity, with no single-node pin. For a shared cluster where no one
# node has the run's peak free but several have part of it. Preflight sums the
# free GPUs over the list and checks that each node can hold one replica of
# the larger model; the standup points the controller's gpu-inventory at the
# list alone (WVA_NODE_SELECTOR), so it funds a replica only where one fits,
# and the quota still bounds the total. Each arm records where its pods
# landed (placement.json), and the report prints it: a 2-GPU replica needs 2
# free GPUs on ONE node, so how the fleet fragments across the nodes decides
# what the share arms can fund. Not together with BENCH_NODE.
BENCH_NODES="${BENCH_NODES:-}"
# BENCH_TOLERATE=<key>[=<value>][:<effect>],...: the taints model replicas may
# tolerate. Empty by default, and nothing is ever tolerated by default:
# preflight refuses a listed node that carries a NoSchedule or NoExecute taint
# this does not name. A taint is somebody's decision about a node; naming it
# here is the run's owner deciding to use the node anyway.
BENCH_TOLERATE="${BENCH_TOLERATE:-}"
# Whether the share arms need node data (a gpu-inventory limiter beside the
# quota, node read and a cluster-wide pod list for the controller): auto means
# when the two models' replicas differ in size, required always, off never.
NODE_DATA="${NODE_DATA:-auto}"
# Which repeat this arm belongs to: its results go under $OUT_ROOT/rep<N>/.
# Empty keeps $OUT_ROOT/<arm>, as every earlier run had it.
REPEAT="${REPEAT:-}"
# BENCH_SIM=1: simulator stacks on a kind cluster in place of the harness's
# standup (see sim_standup). Everything after the standup is the real path.
BENCH_SIM="${BENCH_SIM:-0}"
SIM_IMAGE="${SIM_IMAGE:-ghcr.io/llm-d/llm-d-inference-sim:v0.9.0}"
# The load Pod's two containers, each. Defaults are the measured ones (see
# render_load_container); a kind dry run has nothing like that much memory.
LOAD_CPU="${LOAD_CPU:-2}"
LOAD_MEMORY="${LOAD_MEMORY:-8Gi}"
LOAD_CPU_LIMIT="${LOAD_CPU_LIMIT:-4}"
LOAD_MEMORY_LIMIT="${LOAD_MEMORY_LIMIT:-32Gi}"
POOL_NAME="${POOL_NAME:-twomodel}"
POOL_REPLICAS="${POOL_REPLICAS:-2}"
# A pool with replicas == reserve can never warm anything: the reserve is what
# it refuses to lend.
POOL_RESERVE="${POOL_RESERVE:-1}"
# The bridge's hold FLOOR and CEILING.
#
# Empty leaves the controller's defaults, which is the old behaviour. Setting
# only the floor would do nothing: MEASURED, bridges were already held 93s, 106s,
# 106s and 119s against the controller's 2m ceiling, so a floor under 120s never
# binds. The ceiling has to move with it.
#
# What the floor buys: a bridge is otherwise handed back 7-19s after the replica
# it covers reports Ready, and Ready is not useful -- that replica has an empty
# KV and prefix cache, and llm-d weights the prefix-cache scorer highest, so it
# serves slower AND is chosen less while it warms.
POOL_MIN_HOLD="${POOL_MIN_HOLD:-}"
POOL_MAX_HOLD="${POOL_MAX_HOLD:-}"
GPU_SAMPLE_SECONDS="${GPU_SAMPLE_SECONDS:-5}"
# Whole seconds, for each of the sampler's API calls (check_sampler_settings).
GPU_SAMPLE_TIMEOUT_SECONDS="${GPU_SAMPLE_TIMEOUT_SECONDS:-20}"
OUT_ROOT="${OUT_ROOT:-$ROOT/two-model-results}"
LOAD_IMAGE="${LOAD_IMAGE:-}"
CACHE_CLAIM="${CACHE_CLAIM:-model-pvc}"
WVA_NS="${WVA_NS:-$NS}"
# Where Prometheus runs, so the pool's Pods are scraped like every other engine.
# Empty means "derive it from the controller's own PROMETHEUS_BASE_URL" at
# pool-create. It matters twice: a lent Pod's load is otherwise invisible to
# the controller (the model reads as having LESS demand while the pool covers
# it), and a failure inside a pool Pod is otherwise invisible to the report --
# seven 503s in one arm's rise were traced to the pool Pod at the moment of the
# lend and no further, because nothing had scraped its engine.
MONITORING_NS="${MONITORING_NS:-}"
ACCELERATOR="${ACCELERATOR:-}"
RESET_TIMEOUT="${RESET_TIMEOUT:-600}"
# Whether the standup reuses a gateway control plane the cluster already runs
# (auto: when an istio.io/gateway-controller GatewayClass exists) instead of
# installing istio-base over it. Applied to the scenario COPY the standup
# renders, never to the tracked file.
GATEWAY_REUSE="${GATEWAY_REUSE:-auto}"
GATEWAY_PROVIDER_NAMESPACE="${GATEWAY_PROVIDER_NAMESPACE:-istio-system}"
# Whether preflight checks that the namespace has the pull secrets a reused
# control plane names for its gateway proxy image (see gateway_pull_preflight).
# 0 skips it, for an image that pulls without them.
GATEWAY_PULL_CHECK="${GATEWAY_PULL_CHECK:-1}"
# A decode Pod's CPU and memory request, for nodes whose spare CPU is smaller
# than the scenario's. Empty keeps the scenario's own values. Applied to the
# scenario copy at standup, and used by preflight's placement count.
DECODE_CPU="${DECODE_CPU:-}"
DECODE_MEMORY="${DECODE_MEMORY:-}"

BLUE=$'\033[0;34m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; RED=$'\033[0;31m'; NC=$'\033[0m'
info()  { echo "${BLUE}[INFO]${NC} $*" >&2; }
ok()    { echo "${GREEN}[OK]${NC} $*" >&2; }
warn()  { echo "${YELLOW}[WARN]${NC} $*" >&2; }
# EXITS. Never call it from inside `$( )` -- the exit ends the subshell and the
# caller carries on with an empty string. hack/check-refusals.sh enforces that.
die()   { echo "${RED}[ERROR]${NC} $*" >&2; exit 1; }

k()  { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} -n "$NS" "$@"; }
kc() { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} "$@"; }

need_ns() { [ -n "$NS" ] || die "BENCHMARK_NAMESPACE is required."; }

# ---------------------------------------------------------------------------
# The models of the run: roles a, b and -- with MODEL_COUNT 3 or 4 -- c and d.
# Every per-model setting is <NAME>_<ROLE>: STACK_C, MODEL_C, MAX_REPLICAS_C...
# ---------------------------------------------------------------------------
case "$MODEL_COUNT" in
    2|3|4) ;;
    *) die "MODEL_COUNT=$MODEL_COUNT: 2, 3 or 4 models" ;;
esac
[ "$MODEL_COUNT" = 2 ] || [ "${PD_SINGLE_STACK:-0}" != 1 ] || \
    die "MODEL_COUNT=$MODEL_COUNT with PD_SINGLE_STACK=1: a single-stack P/D run drives one model"
ROLES="$(printf '%s\n' a b c d | head -n "$MODEL_COUNT" | tr '\n' ' ')"
ROLES="${ROLES% }"
# The roles past the first two, which every two-model command line leaves out.
EXTRA_ROLES="$(printf '%s\n' $ROLES | tail -n +3 | tr '\n' ' ')"
EXTRA_ROLES="${EXTRA_ROLES% }"
# --model-c <id> --model-d <id> for the reports, as words; empty for two models.
MODEL_FLAGS=()
for _r in $EXTRA_ROLES; do
    _v="MODEL_$(printf '%s' "$_r" | tr a-z A-Z)"
    MODEL_FLAGS+=("--model-$_r" "${!_v}")
done
unset _r _v

# upper <role>: A for a.
upper() { printf '%s' "$1" | tr a-z A-Z; }
# and_list <word>...: "x and y", "x, y and z".
and_list() {
    local out="" i=0 x
    for x in "$@"; do
        i=$(( i + 1 ))
        if [ "$i" -eq 1 ]; then out="$x"
        elif [ "$i" -eq "$#" ]; then out="$out and $x"
        else out="$out, $x"; fi
    done
    printf '%s' "$out"
}
# models_word: "two" for the two-model run, as every message always said; the
# count for three or four.
models_word() { if [ "$MODEL_COUNT" = 2 ]; then printf two; else printf '%s' "$MODEL_COUNT"; fi; }

# rvar <NAME> <role>: the value of <NAME>_<ROLE>, e.g. rvar STACK c -> $STACK_C.
rvar() { local v="${1}_$(upper "$2")"; printf '%s' "${!v:-}"; }

# role_stacks: every stack of the run, in model order, one per line.
role_stacks() { local r; for r in $ROLES; do rvar STACK "$r"; echo; done; }
# role_models: every model id of the run, in model order, one per line.
role_models() { local r; for r in $ROLES; do rvar MODEL "$r"; echo; done; }
# role_of_model <model id>: the role that serves it, or nothing.
role_of_model() { local r; for r in $ROLES; do [ "$(rvar MODEL "$r")" = "$1" ] && { printf '%s' "$r"; return; }; done; }

# extra_role_flags <flag-suffix>... : for every role past b, --<suffix>-<role>
# and the value of the matching <NAME>_<ROLE>, one word per line. Empty for a
# two-model run, so its command lines are exactly what they always were.
# Suffixes: model, endpoint (URL_<ROLE> in the caller), high-rps, low-rps.
extra_role_flags() {
    local r s
    [ -n "$EXTRA_ROLES" ] || return 0
    for r in $EXTRA_ROLES; do
        for s in "$@"; do
            case "$s" in
                model)    printf '%s\n' "--model-$r" "$(rvar MODEL "$r")" ;;
                high-rps) printf '%s\n' "--high-rps-$r" "$(rvar HIGH_RPS "$r")" ;;
                low-rps)  printf '%s\n' "--low-rps-$r" "$(rvar LOW_RPS "$r")" ;;
                endpoint) printf '%s\n' "--endpoint-$r" "$(rvar URL "$r")" ;;
            esac
        done
    done
}

# The last few metric-read errors in the controller's own log, or nothing.
#
# Its Pods are found by ownership (controller_pods.py), not by label: warm-pool
# Pods carry app.kubernetes.io/name too, so `logs -l` on it read a pool Pod's
# log as the controller's. No controller Pod means no errors to show, as a
# failed kubectl always has here.
controller_metric_errors() {
    local selector
    selector="$(k get pods -l control-plane=controller-manager -o json 2>/dev/null \
        | python3 "$HERE/controller_pods.py" 2>/dev/null)" || return 0
    [ -n "$selector" ] || return 0
    k logs -l "$selector" --tail=400 2>/dev/null \
        | grep -iE 'prometheus|metrics' | grep -iE 'error|refused|denied|timeout|no such host' | tail -5
}

case "$GATEWAY_REUSE" in
    auto|true|false) ;;
    *) die "GATEWAY_REUSE=$GATEWAY_REUSE: use auto (reuse the cluster's Istio when it runs one), true or false" ;;
esac
[ -z "$BENCH_NODE" ] || [ -z "$BENCH_NODES" ] || \
    die "BENCH_NODE=$BENCH_NODE and BENCH_NODES=$BENCH_NODES are both set: one node, or a list"

# bench_nodes: BENCH_NODES one per line, blanks and repeats dropped, in order.
bench_nodes() {
    printf '%s' "$BENCH_NODES" | tr ',' '\n' | tr -d ' \r' | awk 'NF && !seen[$0]++'
}

bench_nodes_csv() { bench_nodes | paste -sd, -; }

# bench_node_selector: the label selector for the controller's gpu-inventory,
# WVA_NODE_SELECTOR. Set-based, which labels.Parse accepts.
bench_node_selector() { printf 'kubernetes.io/hostname in (%s)' "$(bench_nodes_csv)"; }

# bench_nodes_problem: why BENCH_NODES is not a list of node names, or nothing.
bench_nodes_problem() {
    [ -n "$BENCH_NODES" ] || return 0
    local n
    [ -n "$(bench_nodes)" ] || { echo "BENCH_NODES=$BENCH_NODES names no node"; return; }
    for n in $(bench_nodes); do
        case "$n" in *[!a-z0-9.-]*) echo "BENCH_NODES: '$n' is not a node name" ;; esac
    done
}

# tolerations_json: BENCH_TOLERATE as Kubernetes tolerations (a JSON array),
# or nothing when it is empty. Returns 1, with the reason on stderr, for an
# entry that names no key: a toleration without one tolerates EVERY taint.
tolerations_json() {
    [ -n "$BENCH_TOLERATE" ] || return 0
    local entries=() e rest key val eff out=""
    IFS=',' read -r -a entries <<< "$BENCH_TOLERATE"
    for e in "${entries[@]}"; do
        e="${e// /}"
        [ -n "$e" ] || continue
        rest="$e" eff=""
        case "$rest" in *:*) eff="${rest##*:}"; rest="${rest%:*}" ;; esac
        key="${rest%%=*}" val=""
        case "$rest" in *=*) val="${rest#*=}" ;; esac
        case "$key" in
            ""|"*") echo "BENCH_TOLERATE entry '$e' names no taint key; a toleration without one tolerates every taint" >&2; return 1 ;;
            *[!A-Za-z0-9./_-]*) echo "BENCH_TOLERATE entry '$e': '$key' is not a taint key" >&2; return 1 ;;
        esac
        case "$val" in
            *[!A-Za-z0-9._-]*) echo "BENCH_TOLERATE entry '$e': '$val' is not a taint value" >&2; return 1 ;;
        esac
        case "$eff" in
            ""|NoSchedule|PreferNoSchedule|NoExecute) ;;
            *) echo "BENCH_TOLERATE entry '$e': the effect is NoSchedule, PreferNoSchedule or NoExecute, not '$eff'" >&2; return 1 ;;
        esac
        out="${out}$(jq -cn --arg k "$key" --arg v "$val" --arg e "$eff" '
            {key: $k} + (if $v == "" then {operator: "Exists"} else {operator: "Equal", value: $v} end)
            + (if $e == "" then {} else {effect: $e} end)')
"
    done
    [ -n "$out" ] || { echo "BENCH_TOLERATE=$BENCH_TOLERATE names no taint" >&2; return 1; }
    printf '%s' "$out" | jq -cs '.'
}

# The accelerators this run peaks at, and it is the SAME in both arms by
# construction -- that is the point.
#
#   nopool: 2 models x MAX_REPLICAS
#   pool:   2 models x MAX_REPLICAS + the pool itself
#   floor:  2 models x MAX_REPLICAS, never below FLOOR_REPLICAS each
#
# The SAME per-model ceiling in every arm, and the insurance on top of it. An
# earlier version lowered the pool arm's cap by the pool's share so the arms
# would "peak at the same number of accelerators" -- which also stopped that
# arm from ever holding three real replicas AND a bridge, a fleet nobody would
# run. The cost of an insurance is not imposed through the cap; it is
# MEASURED, in accelerator-seconds, and that is what every arm is priced on.
# Set MAX_REPLICAS to as many replicas as the models could ask for.
#
# A P/D model is TWO scaled roles, each with its own ceiling, so it peaks at
# MAX_PREFILL + MAX_DECODE replicas, not MAX_REPLICAS. Counting it as one role
# halved the peak of a two-P/D-stack run, and preflight then passed on a
# cluster with room for half the fleet.
#
# Aggregated models are priced each at its OWN size and ceiling: a 2-GPU model
# at 3 replicas beside a 1-GPU model at 4 peaks at 10, not 2 x 3 x 1.
peak_gpus() {
    local models="$MODEL_COUNT" per_model="$MAX_REPLICAS"
    [ "$PD_SINGLE_STACK" != 1 ] || models=1
    if scenario_is_pd; then
        per_model=$(( MAX_PREFILL + MAX_DECODE ))
        echo $(( models * per_model * GPUS_PER_REPLICA ))
        return
    fi
    # Every model at its own ceiling and size: the sum of the peaks, which a
    # quota below it bounds (quota_cap).
    local r sum=0
    for r in $ROLES; do
        sum=$(( sum + $(rvar MAX_REPLICAS "$r") * $(model_gpus "$r") ))
    done
    echo "$sum"
}

# The largest of the models' ceilings: what the standup registers every
# ScaledObject at, before each arm sets each model's own.
max_ceiling() {
    local r m best=0
    for r in $ROLES; do
        m="$(rvar MAX_REPLICAS "$r")"
        [ "$m" -gt "$best" ] && best="$m"
    done
    echo "$best"
}

# scenario_stack_field <stack> <yq path under the stack entry>: one value from
# the scenario's entry for that stack, or nothing (no scenario, no yq, no such
# stack or field). A program in double quotes, so no apostrophe can open it.
scenario_stack_field() {
    local file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    [ -f "$file" ] && command -v yq >/dev/null 2>&1 || return 0
    STACK="$1" yq -r ".scenario[] | select(.name == strenv(STACK)) | $2 // \"\"" "$file" 2>/dev/null \
        | tr -d '\r' | grep -v '^null$' | head -1
}

# scenario_shared_field <yq path under shared>: the scenario-wide default a
# stack inherits.
scenario_shared_field() {
    local file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    [ -f "$file" ] && command -v yq >/dev/null 2>&1 || return 0
    yq -r ".shared.$1 // \"\"" "$file" 2>/dev/null | tr -d '\r' | grep -v '^null$' | head -1
}

# scenario_tp <stack>: the stack's tensor-parallel size, inherited from the
# shared block, 1 when neither says.
scenario_tp() {
    local v
    v="$(scenario_stack_field "$1" '.modelservice.decode.parallelism.tensor')"
    [ -n "$v" ] || v="$(scenario_shared_field 'modelservice.decode.parallelism.tensor')"
    echo "${v:-1}"
}

scenario_data_local() {
    local v
    v="$(scenario_stack_field "$1" '.modelservice.decode.parallelism.dataLocal')"
    [ -n "$v" ] || v="$(scenario_shared_field 'modelservice.decode.parallelism.dataLocal')"
    echo "${v:-1}"
}

# scenario_gpus <stack>: the GPUs one replica of the stack requests -- its
# nvidia.com/gpu request, else tensor x dataLocal as the harness renders it --
# or nothing when the scenario does not say.
scenario_gpus() {
    local v
    v="$(scenario_stack_field "$1" '.modelservice.decode.resources.requests["nvidia.com/gpu"]')"
    if [ -z "$v" ] && [ -n "$(scenario_stack_field "$1" '.name')" ]; then
        v=$(( $(scenario_tp "$1") * $(scenario_data_local "$1") ))
    fi
    printf '%s' "$v"
}

# model_gpus <a|b>: the GPUs one replica of that model holds. The override when
# set, else the scenario's, else GPUS_PER_REPLICA. A single-stack P/D run has
# no stacks to read and keeps GPUS_PER_REPLICA.
model_gpus() {
    local v stack
    v="$(rvar GPUS_PER_REPLICA "$1")"; stack="$(rvar STACK "$1")"
    if [ -z "$v" ] && [ "$PD_SINGLE_STACK" != 1 ]; then
        v="$(scenario_gpus "$stack")"
    fi
    echo "${v:-$GPUS_PER_REPLICA}"
}

# model_sizes: every model's GPUs per replica, space-separated, in model order.
model_sizes() { local r out=""; for r in $ROLES; do out="$out${out:+ }$(model_gpus "$r")"; done; printf '%s' "$out"; }

# mixed_sizes: whether the models' replicas differ in GPUs.
mixed_sizes() {
    [ "$PD_SINGLE_STACK" != 1 ] || return 1
    [ "$(printf '%s\n' $(model_sizes) | sort -u | wc -l)" -gt 1 ]
}

# node_data_needed: whether the share arms need node data. A 2-GPU replica is
# funded from 1-GPU donors only when node data shows them on one node; without
# it the receiver reports no-compatible-donor and the run measures that.
node_data_needed() {
    case "$NODE_DATA" in
        required) return 0 ;;
        off) return 1 ;;
        *) mixed_sizes ;;
    esac
}

# scenario_size_problems: one line per way the scenario and the run disagree
# about a stack -- a stack the scenario does not have, a model it does not
# serve, a GPU request that is not TP x dataLocal, an override that is not the
# scenario's size. Nothing when they agree, or when there is no scenario to
# read (single-stack P/D).
scenario_size_problems() {
    [ "$PD_SINGLE_STACK" != 1 ] || return 0
    # Two roles on one stack or one model: the route sends both loaders to the
    # same Deployment and the controller sees one model, so a third model that
    # copied the first's settings is a two-model run reported as three.
    local dup
    dup="$(role_stacks | sort | uniq -d | tr '\n' ' ')"
    [ -z "$dup" ] || echo "two models share stack(s) ${dup% }: every STACK_<role> must name its own stack"
    dup="$(role_models | sort | uniq -d | tr '\n' ' ')"
    [ -z "$dup" ] || echo "two models share model id(s) ${dup% }: every MODEL_<role> must name its own model"
    local file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    [ -f "$file" ] && command -v yq >/dev/null 2>&1 || return 0
    local role stack model want_model req tp dl override
    for role in $ROLES; do
        stack="$(rvar STACK "$role")"; want_model="$(rvar MODEL "$role")"; override="$(rvar GPUS_PER_REPLICA "$role")"
        if [ -z "$(scenario_stack_field "$stack" '.name')" ]; then
            echo "STACK_$(printf '%s' "$role" | tr a-z A-Z)=$stack is not a stack of $BENCH_SPEC (its stacks: $(yq -r '[.scenario[].name] | join(", ")' "$file" 2>/dev/null | tr -d '\r'))"
            continue
        fi
        model="$(scenario_stack_field "$stack" '.model.name')"
        [ -z "$model" ] || [ "$model" = "$want_model" ] || \
            echo "stack $stack serves $model, but MODEL_$(printf '%s' "$role" | tr a-z A-Z)=$want_model: the loader would ask it for a model it does not serve"
        req="$(scenario_stack_field "$stack" '.modelservice.decode.resources.requests["nvidia.com/gpu"]')"
        tp="$(scenario_tp "$stack")"; dl="$(scenario_data_local "$stack")"
        if [ -n "$req" ] && [ "$req" -ne $(( tp * dl )) ] 2>/dev/null; then
            echo "stack $stack requests $req GPU(s) per replica but runs tensor $tp x dataLocal $dl = $(( tp * dl )) rank(s): the engine would hold GPUs no rank uses, or never start"
        fi
        if [ -n "$override" ] && [ "$override" != "$(scenario_gpus "$stack")" ]; then
            echo "GPUS_PER_REPLICA_$(printf '%s' "$role" | tr a-z A-Z)=$override, but stack $stack takes $(scenario_gpus "$stack") per replica: the run would be priced on a size it does not deploy"
        fi
    done
}

# quota_cap prints the GPUs the standup quota allows the models -- WVA_QUOTAS,
# the same value the standup line sets ('<accelerator>=<n>', several separated
# by spaces or commas) -- or nothing when there is no quota to read. With
# several accelerators it takes ACCELERATOR's entry, and without ACCELERATOR it
# gives up rather than guess. A run under a quota below the models' combined
# ceilings never holds more than the quota, so placing the ceilings asked for
# accelerators the run cannot use: preflight refused a 4-GPU-quota run for a
# peak of 6.
quota_cap() {
    [ -n "${WVA_QUOTAS:-}" ] || return 0
    local entries=() e key val pick=""
    read -r -a entries <<< "$(printf '%s' "$WVA_QUOTAS" | tr ',' ' ')"
    for e in "${entries[@]}"; do
        key="${e%%=*}"; val="${e#*=}"
        case "$val" in ""|*[!0-9]*) return 0 ;; esac
        if [ -n "$ACCELERATOR" ]; then
            [ "$key" = "$ACCELERATOR" ] && pick="$val"
        elif [ "${#entries[@]}" -eq 1 ]; then
            pick="$val"
        fi
    done
    printf '%s' "$pick"
}

# Whether the models are P/D: PD_SINGLE_STACK, or a scenario whose prefill is
# enabled (two-model-shapes-pd). Read from the scenario, because preflight runs
# before anything is deployed.
scenario_is_pd() {
    [ "$PD_SINGLE_STACK" != 1 ] || return 0
    local file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    [ -f "$file" ] || return 1
    python3 - "$file" <<'PY' 2>/dev/null
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
# Comments out first: a header that mentions "prefill: enabled" is not a block.
text = re.sub(r"(?m)^\s*#.*$", "", text)
sys.exit(0 if re.search(r"\n\s*prefill:\s*\n(\s*\n)*\s*enabled:\s*true\b", text) else 1)
PY
}

pool_gpus() { echo $(( POOL_REPLICAS * GPUS_PER_REPLICA )); }

# scenario_stack_count: how many stacks the scenario defines; 0 when it cannot
# be read, which keeps every stack rather than guessing which to drop.
scenario_stack_count() {
    local file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml" n
    n="$(yq -r '.scenario | length' "$file" 2>/dev/null | tr -d '\r')"
    case "$n" in ""|*[!0-9]*) n=0 ;; esac
    printf '%s' "$n"
}

case "$REPEAT" in
    ""|[1-9]|[1-9][0-9]) ;;
    *) die "REPEAT=$REPEAT: a repeat number from 1 to 99, or empty" ;;
esac

# Where this repeat's arms live: $OUT_ROOT, or $OUT_ROOT/rep<N> with REPEAT.
share_root() { printf '%s' "$OUT_ROOT${REPEAT:+/rep$REPEAT}"; }
arm_dir() { printf '%s/%s' "$(share_root)" "$1"; }

arm_max_replicas() { echo "$MAX_REPLICAS"; }

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
decode_cpu() {
    # The decode Pod's CPU request in whole cores: DECODE_CPU when set (the value
    # the standup will apply), else the live Deployments when the stacks are up,
    # else the scenario -- preflight runs before standup. A Pod asks for the SUM
    # of its containers, and the largest decode Pod is the one that must fit.
    local v
    v="$DECODE_CPU"
    [ -z "$v" ] && v="$(k get deploy -o json 2>/dev/null | jq -r '
        def mc: if test("m$") then (.[:-1] | tonumber) else (tonumber * 1000) end;
        [.items[] | select(.metadata.name | test("decode"))
         | [.spec.template.spec.containers[]?.resources.requests.cpu // empty | mc] | add // empty]
        | max // empty | (. / 1000 | ceil)' 2>/dev/null)"
    [ -z "$v" ] && v="$(scenario_value 'cpu')"
    printf '%s' "${v:-16}"
}

decode_mem_gi() {
    local v
    v="$DECODE_MEMORY"
    [ -z "$v" ] && v="$(k get deploy -o json 2>/dev/null | jq -r '
        def gi: if test("Gi$") then (.[:-2] | tonumber)
                elif test("Mi$") then ((.[:-2] | tonumber) / 1024)
                elif test("Ki$") then ((.[:-2] | tonumber) / 1048576)
                elif test("G$") then ((.[:-1] | tonumber) * 1000000000 / 1073741824)
                else (tonumber / 1073741824) end;
        [.items[] | select(.metadata.name | test("decode"))
         | [.spec.template.spec.containers[]?.resources.requests.memory // empty | gi] | add // empty]
        | max // empty | ceil | tostring + "Gi"' 2>/dev/null)"
    [ -z "$v" ] && v="$(scenario_value 'memory')"
    printf '%s' "${v:-64Gi}" | sed 's/Gi$//'
}

scenario_value() {
    # One decode resources.requests field out of the scenario spec, so preflight
    # can answer before anything is deployed: the LARGEST across the stacks.
    #
    # Read by indentation, not by the first match. The first `decode:` in these
    # scenarios is the shared block (container ports, no requests), so a regex
    # anchored there found nothing and preflight fell back to 16 CPU for a
    # scenario that asks for 8 -- and refused a cluster with room, saying only
    # five Pods could be placed.
    local key="$1" file="$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    [ -f "$file" ] || return 0
    python3 - "$file" "$key" <<'PY' 2>/dev/null || true
import re, sys
lines = open(sys.argv[1], encoding="utf-8").read().replace("\r\n", "\n").split("\n")
key = sys.argv[2]

def depth(line):
    return len(line) - len(line.lstrip(" "))

def meaningful(line):
    s = line.strip()
    return bool(s) and not s.startswith("#")

def size(v):
    units = {"m": 0.001, "Ki": 1.0 / 1048576, "Mi": 1.0 / 1024, "Gi": 1.0, "G": 1e9 / 1073741824}
    for u, f in units.items():
        if v.endswith(u):
            return float(v[: -len(u)]) * f
    return float(v)

found = []
i = 0
while i < len(lines):
    m = re.match(r"^(\s*)decode:\s*(#.*)?$", lines[i])
    if not m:
        i += 1
        continue
    d = len(m.group(1))
    req = None
    j = i + 1
    while j < len(lines):
        line = lines[j]
        if meaningful(line) and depth(line) <= d:
            break
        r = re.match(r"^(\s*)requests:\s*(#.*)?$", line)
        if r:
            req = len(r.group(1))
        elif req is not None and meaningful(line):
            if depth(line) <= req:
                req = None
            else:
                v = re.match(r'^\s*%s:\s*"?([^"#\s]+)"?' % re.escape(key), line)
                if v:
                    found.append(v.group(1))
        j += 1
    i = j
if found:
    print(max(found, key=size))
PY
}

istio_gatewayclasses() {
    # How many GatewayClasses an existing Istio control plane serves here.
    kc get gatewayclass -o json 2>/dev/null | jq -r '
        [.items[] | select(.spec.controllerName == "istio.io/gateway-controller")] | length' 2>/dev/null
}

gateway_reuse() {
    # true when the standup should reuse the gateway control plane the cluster
    # already runs (skipInstall on the scenario copy) instead of installing
    # istio-base over it. GATEWAY_REUSE=auto decides from the cluster.
    case "$GATEWAY_REUSE" in
        true|false) printf '%s' "$GATEWAY_REUSE" ;;
        *) if [ "$(istio_gatewayclasses)" -gt 0 ] 2>/dev/null; then printf true; else printf false; fi ;;
    esac
}

# gateway_proxy_pull: what a reused control plane gives a gateway Pod, as
# "<proxy image>|<pull secret> ...", read from its injector ConfigMaps in
# GATEWAY_PROVIDER_NAMESPACE. Nothing when they cannot be read.
gateway_proxy_pull() {
    kc -n "$GATEWAY_PROVIDER_NAMESPACE" get configmap -o json 2>/dev/null | jq -r '
        [.items[] | select(.metadata.name | startswith("istio-sidecar-injector"))
         | ((.data.values // "{}") | (fromjson? // {})) | (.global // {})] as $g
        | if ($g | length) == 0 then empty else
            ($g[0]) as $x
            | ($x.proxy.image // "proxyv2") as $img
            | (if ($img | test("/")) then $img else "\($x.hub // "?")/\($img):\($x.tag // "?")" end)
              + "|" + ([$g[] | .imagePullSecrets[]? | if type == "object" then .name else . end]
                       | unique | join(" "))
          end' 2>/dev/null
}

# gateway_pull_preflight: refuses a reused control plane whose gateway proxy
# names pull secrets this namespace lacks. MEASURED on a shared H200 cluster:
# the Istio it now runs hands every gateway Pod a proxy image from a registry
# that needs a login, and a pull secret for it that the benchmark namespace
# does not have. The standup downloaded four models, then found the gateway
# Pod in ErrImagePull and aborted. The scenario was not the cause: the
# two-model runs there had predated GATEWAY_REUSE and installed their own
# Istio (proxy docker.io/istio/proxyv2). Read-only: a ConfigMap read and a
# Secret existence check. A secret that cannot be read (as opposed to absent)
# is reported, not refused.
gateway_pull_preflight() {
    [ "$GATEWAY_PULL_CHECK" = 1 ] || { info "  GATEWAY_PULL_CHECK=$GATEWAY_PULL_CHECK: the gateway proxy pull secrets are not checked"; return 0; }
    local got image secrets s out missing="" unread=""
    got="$(gateway_proxy_pull)"
    if [ -z "$got" ]; then
        info "  could not read the gateway proxy image the control plane in $GATEWAY_PROVIDER_NAMESPACE hands out (no readable istio-sidecar-injector ConfigMap); its pull secrets are unchecked"
        return 0
    fi
    image="${got%%|*}"; secrets="${got#*|}"
    for s in $secrets; do
        out="$(k get secret "$s" -o name 2>&1)" && continue
        case "$out" in
            *NotFound*|*"not found"*) missing="$missing${missing:+ }$s" ;;
            *) unread="$unread${unread:+ }$s" ;;
        esac
    done
    if [ -n "$missing" ]; then
        warn "the reused control plane in $GATEWAY_PROVIDER_NAMESPACE gives the gateway Pod the image"
        warn "  $image"
        warn "  with pull secret(s) '$missing', which $NS does not have. The gateway Pod would fail in"
        warn "  ErrImagePull and the standup abort at gateway infra, after the model downloads. Create the"
        warn "  secret(s) in $NS with credentials for that registry, or set GATEWAY_PULL_CHECK=0 if the image"
        warn "  pulls without them."
        return 1
    fi
    [ -z "$unread" ] || info "  could not read pull secret(s) '$unread' in $NS to tell whether they exist; the gateway proxy may not pull"
    ok "  the gateway proxy image is $image${secrets:+ (pull secrets: $secrets)}"
}

placeable_slots() {
    # How many whole decode Pods fit, node by node, in what is left of each
    # node's CPU, memory and accelerators. Empty output when it cannot be
    # computed, which the caller reports rather than treating as zero.
    local cpu mem
    cpu="$(decode_cpu)"; mem="$(decode_mem_gi)"
    case "$cpu" in *m) cpu=$(( ${cpu%m} / 1000 )) ;; esac
    [ "${cpu:-0}" -gt 0 ] 2>/dev/null || return 0
    kc get pods -A -o json 2>/dev/null | jq -c '
        [.items[] | select(.status.phase=="Running" or .status.phase=="Pending")
         | {n: .spec.nodeName,
            c: ([.spec.containers[].resources.requests.cpu // "0"
                 | if test("m$") then (sub("m$";"") | tonumber / 1000) else tonumber end] | add),
            m: ([.spec.containers[].resources.requests.memory // "0"
                 | if test("Gi$") then (sub("Gi$";"") | tonumber)
                   elif test("Mi$") then (sub("Mi$";"") | tonumber / 1024) else 0 end] | add),
            g: ([.spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add)}]' \
      > "${TMPDIR:-/tmp}/wva-pods.$$" 2>/dev/null || return 0
    kc get nodes -o json 2>/dev/null | jq -r \
        --slurpfile p "${TMPDIR:-/tmp}/wva-pods.$$" --argjson cpu "$cpu" --argjson mem "${mem:-64}" '
        [ .items[]
          | select(.spec.unschedulable != true)
          | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0)
          | .metadata.name as $n
          | (.status.allocatable.cpu
             | if test("m$") then (sub("m$";"") | tonumber / 1000) else tonumber end) as $ac
          | (.status.allocatable.memory | sub("Ki$";"") | tonumber / 1048576) as $am
          | (.status.allocatable["nvidia.com/gpu"] // "0" | tonumber) as $ag
          | select($ag > 0)
          | ([$p[0][] | select(.n == $n) | .c] | add // 0) as $uc
          | ([$p[0][] | select(.n == $n) | .m] | add // 0) as $um
          | ([$p[0][] | select(.n == $n) | .g] | add // 0) as $ug
          | [ ((($ac - $uc) / $cpu) | floor), ((($am - $um) / $mem) | floor), ($ag - $ug) ]
          | min ] | add // 0' 2>/dev/null
    rm -f "${TMPDIR:-/tmp}/wva-pods.$$"
}

# node_free_gpus [node]: free accelerators on that node -- allocatable less
# what Running or Pending pods BOUND to it request -- or, with no node, the
# most on any one node. Only schedulable, Ready nodes; nothing when there is
# no such node or the cluster cannot be read.
node_free_gpus() {
    local want="${1:-}" nodes_f pods_f
    nodes_f="$(mktemp)"; pods_f="$(mktemp)"
    kc get nodes -o json > "$nodes_f" 2>/dev/null || { rm -f "$nodes_f" "$pods_f"; return 0; }
    kc get pods -A -o json > "$pods_f" 2>/dev/null || echo '{"items":[]}' > "$pods_f"
    jq -r --slurpfile p "$pods_f" --arg want "$want" '
        [ .items[]
          | select(.spec.unschedulable != true)
          | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0)
          | select($want == "" or .metadata.name == $want)
          | .metadata.name as $n
          | (.status.allocatable["nvidia.com/gpu"] // "0" | tonumber) as $ag
          | select($ag > 0)
          | ([$p[0].items[] | select(.status.phase=="Running" or .status.phase=="Pending")
              | select(.spec.nodeName == $n)
              | [.spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add] | add // 0) as $used
          | $ag - $used ]
        | if length == 0 then empty else max end' "$nodes_f" 2>/dev/null
    rm -f "$nodes_f" "$pods_f"
}

# largest_replica_gpus: the GPUs one replica of the larger model holds.
largest_replica_gpus() {
    if [ "$PD_SINGLE_STACK" = 1 ] || scenario_is_pd; then echo "$GPUS_PER_REPLICA"; return; fi
    local g best=0
    for g in $(model_sizes); do
        [ "$g" -gt "$best" ] && best="$g"
    done
    echo "$best"
}

# peak_pods <gpus>: the most model Pods the run can hold within <gpus>, each
# model between MIN_REPLICAS and its ceiling at its own size. The quota's 7
# GPUs hold 5 Pods at 2 + 1 x 4 GPUs, and each of them needs its CPU and
# memory somewhere.
peak_pods() {
    local cap="$1"
    if [ "$PD_SINGLE_STACK" = 1 ] || scenario_is_pd; then echo $(( cap / GPUS_PER_REPLICA )); return; fi
    # Every combination of per-model replica counts, as nested loops over the
    # models: for two models exactly the double loop it always was.
    local sizes ceilings r
    sizes="$(model_sizes)"
    ceilings=""
    for r in $ROLES; do ceilings="$ceilings${ceilings:+ }$(rvar MAX_REPLICAS "$r")"; done
    _PEAK_BEST=0
    _peak_pods_from "$cap" 0 0 "$sizes" "$ceilings"
    echo "$_PEAK_BEST"
}

# _peak_pods_from <cap> <gpus so far> <pods so far> <sizes left> <ceilings left>:
# the recursion behind peak_pods, keeping the best count in _PEAK_BEST.
_peak_pods_from() {
    local cap="$1" used="$2" pods="$3" sizes="$4" ceilings="$5" g m n
    if [ -z "$sizes" ]; then
        [ "$used" -le "$cap" ] && [ "$pods" -gt "$_PEAK_BEST" ] && _PEAK_BEST="$pods"
        return 0
    fi
    g="${sizes%% *}"; m="${ceilings%% *}"
    for n in $(seq "$MIN_REPLICAS" "$m"); do
        _peak_pods_from "$cap" $(( used + n * g )) $(( pods + n )) \
            "$( [ "$sizes" = "${sizes#* }" ] || printf '%s' "${sizes#* }")" \
            "$( [ "$ceilings" = "${ceilings#* }" ] || printf '%s' "${ceilings#* }")"
    done
}

# bench_nodes_fit: one JSON object per BENCH_NODES entry, in order -- whether
# it exists, is Ready and schedulable, its free GPUs (allocatable less what
# pods bound to it request), free CPU and memory, and the NoSchedule and
# NoExecute taints that BENCH_TOLERATE does not tolerate. Nothing when the
# cluster cannot be read. node.kubernetes.io/* taints are left to the Ready
# and cordon checks, which say what they mean.
bench_nodes_fit() {
    local tols="$1" nodes_f pods_f names
    names="$(bench_nodes | jq -R . | jq -cs .)"
    nodes_f="$(mktemp)"; pods_f="$(mktemp)"
    kc get nodes -o json > "$nodes_f" 2>/dev/null || { rm -f "$nodes_f" "$pods_f"; return 0; }
    kc get pods -A -o json > "$pods_f" 2>/dev/null || echo '{"items":[]}' > "$pods_f"
    jq -c --slurpfile p "$pods_f" --argjson names "$names" --argjson tols "${tols:-[]}" '
        def cpu: if test("m$") then (sub("m$";"") | tonumber / 1000) else tonumber end;
        def gi: if test("Ki$") then (sub("Ki$";"") | tonumber / 1048576)
                elif test("Mi$") then (sub("Mi$";"") | tonumber / 1024)
                elif test("Gi$") then (sub("Gi$";"") | tonumber)
                else (tonumber / 1073741824) end;
        def tolerates($t): .key == $t.key
            and ((.operator // "Equal") == "Exists" or (.value // "") == ($t.value // ""))
            and ((.effect // "") == "" or .effect == $t.effect);
        [$p[0].items[] | select(.status.phase=="Running" or .status.phase=="Pending")
         | select(.spec.nodeName != null)] as $bound
        | (.items | map({(.metadata.name): .}) | add // {}) as $by
        | $names[] as $n
        | $by[$n] as $node
        | if $node == null then {name: $n, found: false}
          else
            ([$bound[] | select(.spec.nodeName == $n)]) as $on
            | {name: $n, found: true,
               ready: ([$node.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0),
               cordoned: ($node.spec.unschedulable == true),
               gpu_alloc: ($node.status.allocatable["nvidia.com/gpu"] // "0" | tonumber),
               gpu_free: (($node.status.allocatable["nvidia.com/gpu"] // "0" | tonumber)
                          - ([$on[] | .spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0)),
               cpu_free: (($node.status.allocatable.cpu // "0" | cpu)
                          - ([$on[] | .spec.containers[].resources.requests.cpu // "0" | cpu] | add // 0)),
               mem_free: (($node.status.allocatable.memory // "0" | gi)
                          - ([$on[] | .spec.containers[].resources.requests.memory // "0" | gi] | add // 0)),
               taints: [$node.spec.taints[]?
                        | select(.effect == "NoSchedule" or .effect == "NoExecute")
                        | select(.key | startswith("node.kubernetes.io/") | not)
                        | . as $t | select([$tols[] | tolerates($t)] | any | not)
                        | "\(.key)\(if (.value // "") != "" then "=" + .value else "" end):\(.effect)"]}
          end' "$nodes_f" 2>/dev/null
    rm -f "$nodes_f" "$pods_f"
}

# multi_node_preflight <peak GPUs>: the BENCH_NODES checks. Each listed node
# must exist, be Ready, take Pods (no taint BENCH_TOLERATE does not name) and
# hold at least one replica of the larger model -- its GPUs on that ONE node,
# plus a decode Pod's CPU and memory. Together they must have the free GPUs the
# run peaks at, and the CPU and memory for the Pods that peak can be. Returns 1
# on any of it, having said which.
multi_node_preflight() {
    local want="$1" rc=0 tols fit big cpu mem
    local bad
    bad="$(bench_nodes_problem)"
    if [ -n "$bad" ]; then printf '%s\n' "$bad" | while IFS= read -r l; do warn "$l"; done; return 1; fi
    tols="$(tolerations_json)" || { warn "BENCH_TOLERATE=$BENCH_TOLERATE is refused (above)"; return 1; }
    big="$(largest_replica_gpus)"
    cpu="$(decode_cpu)"; mem="$(decode_mem_gi)"
    case "$cpu" in *m) cpu=$(( ${cpu%m} / 1000 )) ;; esac
    fit="$(bench_nodes_fit "$tols")"
    [ -n "$fit" ] || { warn "BENCH_NODES: could not read the nodes and pods to check them"; return 1; }
    info "BENCH_NODES=$(bench_nodes_csv): replicas may land on any of them; the larger model's replica takes ${big} GPU(s), and a decode Pod ${cpu} CPU / ${mem}Gi, on one node"
    [ -z "$tols" ] || info "  tolerated, on these nodes only: $BENCH_TOLERATE"
    local total=0 slots=0 bigslots=0 line name gfree cfree mfree pods_fit node_bigs
    while IFS= read -r line; do
        name="$(jq -r .name <<<"$line")"
        if [ "$(jq -r .found <<<"$line")" != true ]; then
            warn "BENCH_NODES: no node named $name"; rc=1; continue
        fi
        if [ "$(jq -r .ready <<<"$line")" != true ] || [ "$(jq -r .cordoned <<<"$line")" = true ]; then
            warn "BENCH_NODES: $name is not Ready, or is cordoned: it takes no new Pods"; rc=1; continue
        fi
        if [ "$(jq -r '.gpu_alloc' <<<"$line")" -le 0 ]; then
            warn "BENCH_NODES: $name has no accelerators"; rc=1; continue
        fi
        if [ "$(jq -r '.taints | length' <<<"$line")" -gt 0 ]; then
            warn "BENCH_NODES: $name carries $(jq -r '.taints | join(", ")' <<<"$line"), which BENCH_TOLERATE does not name."
            warn "  Nothing is tolerated by default. Name it in BENCH_TOLERATE only if this run may use that node; otherwise drop the node."
            rc=1; continue
        fi
        gfree="$(jq -r '.gpu_free' <<<"$line")"
        cfree="$(jq -r '.cpu_free | floor' <<<"$line")"
        mfree="$(jq -r '.mem_free | floor' <<<"$line")"
        pods_fit="$(jq -r --argjson c "$cpu" --argjson m "${mem:-64}" '[(.cpu_free / $c | floor), (.mem_free / $m | floor)] | min' <<<"$line")"
        [ "$pods_fit" -ge 0 ] 2>/dev/null || pods_fit=0
        node_bigs=$(( gfree / big ))
        [ "$node_bigs" -le "$pods_fit" ] || node_bigs="$pods_fit"
        if [ "$node_bigs" -lt 1 ]; then
            warn "BENCH_NODES: $name has ${gfree} free GPU(s), ${cfree} free CPU and ${mfree}Gi free memory: it cannot hold one ${big}-GPU replica (${big} GPUs, ${cpu} CPU and ${mem}Gi on one node)"
            rc=1; continue
        fi
        info "  $name: ${gfree} free GPU(s), ${cfree} CPU, ${mfree}Gi free: up to ${node_bigs} ${big}-GPU replica(s) at once"
        total=$(( total + gfree ))
        [ "$pods_fit" -le "$gfree" ] && slots=$(( slots + pods_fit )) || slots=$(( slots + gfree ))
        bigslots=$(( bigslots + node_bigs ))
    done <<< "$fit"
    [ "$rc" -eq 0 ] || return 1
    local need_pods
    need_pods="$(peak_pods "$want")"
    if [ "$total" -lt "$want" ]; then
        warn "BENCH_NODES have ${total} free GPU(s) together and the run peaks at ${want}: add a node, or lower the quota or MAX_REPLICAS"
        rc=1
    elif [ "$slots" -lt "$need_pods" ]; then
        warn "BENCH_NODES have ${total} free GPU(s) but CPU and memory for only ${slots} Pod(s), and the run can hold ${need_pods} at ${want} GPUs:"
        warn "  lower DECODE_CPU/DECODE_MEMORY (the standup applies them), or add a node"
        rc=1
    else
        ok "BENCH_NODES have ${total} free GPU(s) together; the run peaks at ${want} (up to ${need_pods} Pods), and room for ${bigslots} ${big}-GPU replica(s) at once"
    fi
    return "$rc"
}

verb_preflight() {
    need_ns
    local rc=0
    for t in kubectl jq python3; do
        command -v "$t" >/dev/null 2>&1 || { warn "$t is not on PATH"; rc=1; }
    done
    kc get ns "$NS" >/dev/null 2>&1 || { warn "namespace $NS does not exist"; rc=1; }

    # FREE accelerators, on nodes that could actually take a Pod. Counting
    # cordoned or NotReady nodes overstates it, and the run then spends its
    # bursts Pending -- which measures the scheduler, identically in both arms,
    # and hides whatever the pool did.
    #
    # Counted the way the placement below counts: a pod holds accelerators only
    # once it is BOUND to a node (spec.nodeName). A Pending pod the scheduler
    # cannot place holds none -- counting two such 8-GPU pods of another team
    # made preflight report "only 2 free" beside "18 placeable" and fail. They
    # are a risk instead (they may take accelerators first), and said so.
    local total used free want nodes_f pods_f unbound
    nodes_f="$(mktemp)"; pods_f="$(mktemp)"
    kc get nodes -o json > "$nodes_f" 2>/dev/null || echo '{"items":[]}' > "$nodes_f"
    kc get pods -A -o json > "$pods_f" 2>/dev/null || echo '{"items":[]}' > "$pods_f"
    total="$(jq '
        [.items[]
         | select(.spec.unschedulable != true)
         | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0)
         | .status.allocatable["nvidia.com/gpu"] // "0" | tonumber] | add // 0' "$nodes_f")"
    used="$(jq --slurpfile n "$nodes_f" '
        [$n[0].items[]
         | select(.spec.unschedulable != true)
         | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0)
         | .metadata.name] as $usable
        | [.items[] | select(.status.phase=="Running" or .status.phase=="Pending")
           | select(.spec.nodeName != null and (.spec.nodeName as $x | $usable | index($x)) != null)
           | .spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0' "$pods_f")"
    unbound="$(jq -r '
        [.items[] | select(.status.phase=="Pending" and .spec.nodeName == null)
         | {g: ([.spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add)}
         | select(.g > 0) | .g] | "\(length) \(add // 0)"' "$pods_f")"
    rm -f "$nodes_f" "$pods_f"
    free=$(( ${total:-0} - ${used:-0} ))
    want="$(peak_gpus)"
    local cap
    cap="$(quota_cap)"
    if [ -n "$cap" ] && [ "$cap" -lt "$want" ]; then
        info "the quota (WVA_QUOTAS=${WVA_QUOTAS}) holds the models to ${cap} GPU(s), below the ${want} their ceilings allow; placement is checked for ${cap}"
        want="$cap"
    fi
    # THE QUOTA'S LOWER BOUND: one model's peak plus every other model's floor.
    # Below it, even a perfect transfer cannot give a rising model its peak
    # while the others keep their floors, and the arms measure how a shortage
    # is cut rather than whether GPUs arrive in time. For two 1-GPU models of
    # up to 3 that is 4; for four, 6.
    if [ -n "$cap" ] && [ "$PD_SINGLE_STACK" != 1 ] && ! scenario_is_pd; then
        local qr qo q_floors q_need=0 q_role="" q_one
        for qr in $ROLES; do
            q_floors=0
            for qo in $ROLES; do
                [ "$qo" = "$qr" ] || q_floors=$(( q_floors + MIN_REPLICAS * $(model_gpus "$qo") ))
            done
            q_one=$(( $(rvar MAX_REPLICAS "$qr") * $(model_gpus "$qr") + q_floors ))
            if [ "$q_one" -gt "$q_need" ]; then q_need="$q_one"; q_role="$qr"; fi
        done
        if [ "$cap" -lt "$q_need" ]; then
            warn "the quota (${cap} GPU(s)) is below one model's peak plus the others' floors: model $(upper "$q_role")"
            warn "  at $(rvar MAX_REPLICAS "$q_role") x $(model_gpus "$q_role") GPU(s) beside $(( MODEL_COUNT - 1 )) model(s) at MIN_REPLICAS=${MIN_REPLICAS} needs ${q_need}."
            warn "  Raise WVA_QUOTAS to at least ${q_need}, or lower MAX_REPLICAS."
            rc=1
        else
            ok "the quota (${cap}) covers one model's peak plus the others' floors (${q_need})"
        fi
    fi
    info "accelerators: ${total:-?} schedulable, ${used:-?} held by pods on those nodes, ${free} free; this run peaks at ${want}"
    if [ "${unbound%% *}" -gt 0 ] 2>/dev/null; then
        warn "${unbound%% *} Pending pod(s) wait for ${unbound#* } accelerator(s) and hold none yet; they are not"
        warn "  counted, but whatever frees up first may go to them rather than to this run."
    fi
    info "  nopool arm: ${MODEL_COUNT} models x 1..${MAX_REPLICAS} replicas"
    info "  floor arm:  ${MODEL_COUNT} models x ${FLOOR_REPLICAS}..${MAX_REPLICAS} replicas"
    info "  pool arm:   ${MODEL_COUNT} models x 1..${MAX_REPLICAS} replicas + a ${POOL_REPLICAS}-Pod pool on top -- the same ceiling in every arm; the insurance is priced in GPU-seconds"
    [ "$MODEL_COUNT" = 2 ] || info "  bursts rotate over ${MODEL_COUNT} models (b, c$([ "$MODEL_COUNT" = 4 ] && echo ', d'), a), each shifted by one phase plus one band from the last"
    if scenario_is_pd; then
        info "  P/D: every model is two roles -- prefill ${MIN_PREFILL}..${MAX_PREFILL} and decode ${MIN_DECODE}..${MAX_DECODE} replicas -- and the peak counts both"
    fi
    if [ "$FLOOR_REPLICAS" -gt "$(floor_ceiling)" ]; then
        warn "FLOOR_REPLICAS=${FLOOR_REPLICAS} is above a role's ceiling ($(floor_ceiling)): the floor arm will refuse to start"
    fi
    if ! scenario_is_pd; then
        local mr
        for mr in $ROLES; do
            info "  model $(upper "$mr") ($(rvar STACK "$mr"), $(rvar MODEL "$mr")): $(model_gpus "$mr") GPU(s) per replica (tensor $(scenario_tp "$(rvar STACK "$mr")")), 1..$(rvar MAX_REPLICAS "$mr") replicas"
        done
    fi
    local size_problems
    size_problems="$(scenario_size_problems)"
    if [ -n "$size_problems" ]; then
        printf '%s\n' "$size_problems" | while IFS= read -r l; do warn "$l"; done
        rc=1
    fi
    # Mixed sizes need node data for the share arms, and that needs an admin
    # once: said here, before anything is stood up, so the run is not found
    # missing it at the first arm.
    if node_data_needed; then
        case ",${WVA_LIMITER:-}," in
            *,gpu-inventory,*|*,inventory,*) info "the share arms need node data; WVA_LIMITER=${WVA_LIMITER} declares gpu-inventory beside the quota" ;;
            *) warn "the $(models_word) models' replicas differ in size ($(model_sizes | sed 's/ / and /g') GPUs), so the share arms need node data:"
               warn "  stand up with WVA_LIMITER=gpu-inventory,quota (now '${WVA_LIMITER:-}'); the share arms refuse a policy without it." ;;
        esac
        info "  after standup an admin grants the controller node read and a cluster-wide pod list once; 'node-data' prints the exact commands"
    fi
    # ONE node holds the whole run: the donor set that funds a 2-GPU replica
    # from two 1-GPU ones exists only on one node, and BENCH_NODE pins every
    # model replica there. Checked here because the cluster-wide free count says
    # nothing about one node.
    if [ -n "$BENCH_NODE" ]; then
        local node_free
        node_free="$(node_free_gpus "$BENCH_NODE")"
        if [ -z "$node_free" ]; then
            warn "BENCH_NODE=$BENCH_NODE: no such schedulable, Ready node with accelerators"
            rc=1
        elif [ "$node_free" -lt "$want" ]; then
            warn "BENCH_NODE=$BENCH_NODE has ${node_free} free accelerator(s) and the run peaks at ${want}: pick a node with at least ${want} free"
            rc=1
        else
            ok "BENCH_NODE=$BENCH_NODE has ${node_free} free accelerator(s); the run peaks at ${want}"
        fi
    elif [ -n "$BENCH_NODES" ]; then
        # SEVERAL nodes: none needs the whole peak, but each must hold one
        # replica of the larger model, and together they must hold the peak.
        multi_node_preflight "$want" || rc=1
    else
        local best
        best="$(node_free_gpus)"
        [ -n "$best" ] && info "  the node with the most free accelerators has ${best}; BENCH_NODE=<node> pins the run to one, BENCH_NODES=<node>,<node> spreads it over several"
    fi
    if [ -n "$BENCH_TOLERATE" ] && [ -z "$BENCH_NODES" ]; then
        warn "BENCH_TOLERATE is set without BENCH_NODES: a toleration is only taken for nodes the run names"
        rc=1
    fi

    # FREE ACCELERATORS ARE NOT PLACEABLE ACCELERATORS, and the difference cost
    # a whole 90-minute A/B.
    #
    # Measured on a multi-node H200 cluster: preflight reported "11 free, this run peaks at 6"
    # and passed. The run then sat with 4 replicas Pending for thirty minutes
    # and 8 by the end, because a decode Pod asks for 16 CPU as well as its
    # accelerator, and the nodes that HAD free accelerators had 7 and 15 free
    # cores. Of 16 free accelerators, four were placeable. The nopool arm ran
    # its whole schedule on one replica per model while its Deployments asked
    # for three, and the arms were not comparable -- for a reason nothing
    # checked.
    #
    # So this bin-packs: per node, how many whole Pods fit in the CPU, the
    # memory AND the accelerators that node has left.
    local slots
    slots="$(placeable_slots)"
    if [ -n "$slots" ]; then
        info "  placeable: ${slots} Pod(s) of $(decode_cpu) CPU / $(decode_mem_gi)Gi / 1 accelerator fit on the nodes as they are now"
    fi
    if [ "$free" -lt "$want" ]; then
        warn "only ${free} free and the run peaks at ${want}. Lower MAX_REPLICAS, or wait."
        rc=1
    elif [ -n "$slots" ] && [ "$slots" -lt "$want" ]; then
        warn "${free} accelerators are free but only ${slots} of them can actually take a Pod: the"
        warn "  run peaks at ${want}. The nodes with spare accelerators do not have $(decode_cpu) spare cores."
        warn "  Replicas would sit Pending and the arm would measure a fleet it never got."
        warn "  Lower the decode request (DECODE_CPU=<cores>, DECODE_MEMORY=<n>Gi -- the standup applies"
        warn "  them to its scenario copy), lower MAX_REPLICAS, or use a cluster with room; then run"
        warn "  preflight again with the same settings."
        rc=1
    fi
    # The pool arm holds its Pods ON TOP of the models' ceiling. Reported, not
    # refused: both models at MAX_REPLICAS at the same instant is the case the
    # anti-phase schedule exists to avoid, so this only binds if it happens.
    if [ -n "$slots" ] && [ "$slots" -lt $(( want + $(pool_gpus) )) ]; then
        warn "the pool arm's theoretical peak is $(( want + $(pool_gpus) )) (models at MAX_REPLICAS plus the pool) against ${slots} placeable;"
        warn "  it binds only if BOTH models reach MAX_REPLICAS at once, which the anti-phase schedule avoids."
    fi

    # ONE accelerator kind. A warm copy is only reusable on the accelerator it
    # was loaded on, so a pool pinned to the wrong product is never eligible to
    # lend -- and nothing fails: the run completes and reports that the pool did
    # nothing. Picking `[0]` of several was exactly that silent mismatch.
    local products count
    products="$(kc get nodes -o json 2>/dev/null | jq -r '
        [.items[].metadata.labels
         | (."nvidia.com/gpu.product", ."gpu.nvidia.com/model", ."gpu.nvidia.com/class",
            ."cloud.google.com/gke-accelerator", ."eks.amazonaws.com/instance-gpu-name")
         | select(. != null and . != "")] | unique | .[]')"
    count="$(printf '%s\n' "$products" | grep -c . || true)"
    if [ -n "$ACCELERATOR" ]; then
        info "accelerator: $ACCELERATOR (from ACCELERATOR)"
    elif [ "${count:-0}" -eq 1 ]; then
        info "accelerator: $products"
    # A POOL-ONLY question, so a warning and not a refusal: the models are placed
    # by their accelerator request, not by this label, and the share arms refuse
    # a pool outright. Refusing here failed preflight for runs that never create
    # one -- on a cluster whose nodes carry the same GPU under two label keys.
    # pool-create refuses without exactly one product, which is where it binds.
    elif [ "${count:-0}" -eq 0 ]; then
        warn "no node advertises an accelerator product label this knows. Only a pool arm needs one:"
        warn "  set ACCELERATOR=<product> before pool-create, which refuses without it. Arms with no pool are unaffected."
    else
        warn "this cluster advertises ${count} accelerator products:"
        printf '%s\n' "$products" | sed 's/^/      /' >&2
        warn "  Only a pool arm needs one: a pool serves ONE product, so set ACCELERATOR=<product> before"
        warn "  pool-create, which refuses without it. Arms with no pool (the share arms refuse one) are unaffected."
    fi

    # WHO OWNS ISTIO. The standup installs istio-base + istiod by helmfile, and
    # on a cluster that already runs Istio by any other means that install does
    # not merge -- it fails on ownership:
    #   ServiceAccount "istio-reader-service-account" in namespace "istio-system"
    #   exists and cannot be imported into the current release
    # Measured on a multi-node H200 cluster: a shared cluster with Istio installed months
    # earlier, not Helm-managed. The standup got as far as deploying one stack
    # before dying, and the half-built namespace then tripped the EPP-reuse
    # guard on the next attempt, so the fix was two failures away from the
    # cause. The standup reuses what is there instead: GATEWAY_REUSE=auto (the
    # default) marks the scenario COPY skipInstall when the cluster runs Istio,
    # so nobody hand-edits a tracked scenario to get past this.
    local gw_installed gw_skips gw_reuse
    gw_installed="$(istio_gatewayclasses)"
    gw_skips="$(grep -c 'skipInstall:[[:space:]]*true' \
        "$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml" 2>/dev/null || true)"
    gw_reuse="$(gateway_reuse)"
    [ "${gw_skips:-0}" -gt 0 ] && gw_reuse=true
    if [ "${gw_installed:-0}" -gt 0 ] && [ "$gw_reuse" != true ]; then
        warn "this cluster already provides Istio (an istio.io/gateway-controller GatewayClass exists),"
        warn "  and GATEWAY_REUSE=$GATEWAY_REUSE. standup would try to install istio-base over it and"
        warn "  fail on ownership metadata, part-way through, leaving a namespace that the next attempt"
        warn "  refuses as someone else's. Run with GATEWAY_REUSE=auto (the default) or true, and"
        warn "  GATEWAY_PROVIDER_NAMESPACE=<namespace of the existing control plane> if it is not istio-system."
        rc=1
    elif [ "${gw_installed:-0}" -eq 0 ] && [ "$gw_reuse" = true ]; then
        warn "the standup would reuse an existing Istio (GATEWAY_REUSE=$GATEWAY_REUSE, or the scenario sets"
        warn "  gateway.skipInstall), but no istio.io/gateway-controller GatewayClass exists here. Nothing"
        warn "  would install a control plane and the Gateway would never be Programmed -- the load then"
        warn "  fails to resolve, in every arm. Use GATEWAY_REUSE=auto, or point it at the cluster that has Istio."
        rc=1
    elif [ "${gw_installed:-0}" -gt 0 ]; then
        ok "Istio is provided by the cluster; the standup reuses it (control plane in $GATEWAY_PROVIDER_NAMESPACE)"
        gateway_pull_preflight || rc=1
    fi

    # The model cache. A pool Pod loads its warm copies through the SAME claim
    # the models use; pointed at a claim they do not use, the engine gets a
    # --model path that is not in the Pod, never answers, and the controller
    # waits out its whole admission timeout before reporting only that the port
    # did not respond.
    #
    # Missing is normal before the first standup, which creates it -- and
    # node-local weights REQUIRE a fresh namespace, so on that path it is
    # always missing here. The real guard is an EXISTING claim on another
    # class when node-local weights are asked for: the harness keeps an
    # existing claim, so the volume it would create never binds.
    local cache_class
    cache_class="$(k get pvc "$CACHE_CLAIM" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)"
    if [ -n "${BENCHMARK_MODEL_HOSTPATH:-}" ] && [ -n "$cache_class" ] && [ "$cache_class" != node-local-weights ]; then
        warn "BENCHMARK_MODEL_HOSTPATH asks for node-local weights, but $CACHE_CLAIM already exists on storage class '$cache_class':"
        warn "  the harness keeps an existing claim, so the node-local volume would never bind. Use a fresh namespace,"
        warn "  or delete the claim (the shared copy of the weights goes with it)."
        rc=1
    elif k get pvc "$CACHE_CLAIM" >/dev/null 2>&1; then
        ok "model cache claim $CACHE_CLAIM exists"
    else
        info "no PVC named $CACHE_CLAIM in $NS yet: the standup creates it"
    fi

    # The phase has to outlast scale-down stabilization or the premise fails --
    # UNLESS the both-low band between bursts is itself longer than
    # stabilization. Then the falling model has given its replicas back before
    # the other model rises whatever the burst length, and short bursts with
    # long quiet stretches are a legitimate shape: it is the one where a held
    # floor is paid for mostly idle, which is the cost question a pool exists
    # to answer.
    if [ "$PHASE_SECONDS" -lt 420 ] && [ "$OVERLAP_SECONDS" -lt 300 ]; then
        warn "PHASE_SECONDS=$PHASE_SECONDS is shorter than scale-down stabilization (300s) plus a ramp,"
        warn "  and the both-low band (OVERLAP_SECONDS=$OVERLAP_SECONDS) is shorter than stabilization."
        warn "  A model then holds the replicas it grew through most of the OTHER model's rise, so"
        warn "  the two never compete and the anti-phase premise is not exercised. Lengthen one of them."
        rc=1
    fi
    if [ "$RISE_WINDOW" -ge "$PHASE_SECONDS" ]; then
        warn "RISE_WINDOW=$RISE_WINDOW is not shorter than PHASE_SECONDS=$PHASE_SECONDS; the profile will refuse to render."
        rc=1
    fi

    # The controller's Prometheus, with the controller's TLS settings, before
    # anything holds an accelerator.
    prom_tls_preflight || rc=1

    [ "$rc" -eq 0 ] && ok "preflight passed" || warn "preflight found problems (above)"
    return "$rc"
}

# ---------------------------------------------------------------------------
# standup -- ONE standup, two stacks
#
# The harness supports this directly: N stacks behind ONE gateway, each with its
# own EPP, InferencePool and decode Deployment, multiplexed by a shared
# HTTPRoute on a path prefix per stack.
#
# Two SEPARATE standups was the first design and it was wrong twice over: the
# single-model guide uses `gateway.className: epponly`, which deploys no Gateway
# at all and which the renderer REFUSES for a multi-stack scenario -- so there
# would have been no shared address to drive, and whatever the driver found
# would have been one model's stack answering for both.
# ---------------------------------------------------------------------------
verb_standup() {
    [ "$PD_SINGLE_STACK" != 1 ] || die "PD_SINGLE_STACK=1: stand the P/D model up with
    make benchmark-standup BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=<model>
  then set PD_ENDPOINT to its router Service and run verify."
    need_ns
    [ -f "$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml" ] || \
        die "no scenario at hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    # The sizes the run is priced on are the sizes it deploys, and the limiter
    # the share arms need is declared at install: both refused HERE, before an
    # hour of standup, not at the first arm.
    local size_problems
    size_problems="$(scenario_size_problems)"
    [ -z "$size_problems" ] || die "the scenario and this run disagree:
$size_problems"
    if node_data_needed; then
        case ",${WVA_LIMITER:-}," in
            *,gpu-inventory,*|*,inventory,*) : ;;
            *) die "the models' replicas differ in size ($(model_sizes | sed 's/ / and /g') GPUs), so the share arms need node data, and WVA_LIMITER='${WVA_LIMITER:-}' declares no gpu-inventory limiter. Stand up with WVA_LIMITER=gpu-inventory,quota WVA_QUOTAS='<accelerator>=<n>', or NODE_DATA=off to run without it (the small model can then never fund the large one)." ;;
        esac
    fi
    local nodes_bad tols
    nodes_bad="$(bench_nodes_problem)"
    [ -z "$nodes_bad" ] || die "$nodes_bad"
    [ -z "$BENCH_TOLERATE" ] || [ -n "$BENCH_NODES" ] || \
        die "BENCH_TOLERATE is set without BENCH_NODES: a toleration is only taken for nodes the run names"
    tols="$(tolerations_json)" || die "BENCH_TOLERATE=$BENCH_TOLERATE is refused (above)"
    if [ "$BENCH_SIM" = 1 ]; then
        sim_standup
        apply_node_selector
        return
    fi
    # BOTH halves. `--spec` resolves a SPECIFICATION
    # (config/specification/<name>.yaml.j2) and never a scenario, so a scenario
    # shipped without one is invisible to the CLI: it reports
    # "Specification '<name>' not found" and prints the list it does know, which
    # reads like a missing file while the scenario sits in the clone. Measured,
    # on the first run of this standup.
    [ -f "$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml.j2" ] || \
        die "no specification at hack/benchmark/scenarios/$BENCH_SPEC.yaml.j2.
    The scenario exists, but --spec resolves the SPECIFICATION, so the CLI cannot see it."
    # RE-RENDERING OUR OWN STACKS IS FINE; clobbering someone else's is not.
    #
    # The standup refuses outright when it finds an EPP, because a second
    # standup applies its own EPP, Service, InferencePool and PodMonitor over
    # whatever is there and the pods keep answering -- silent damage to a stack
    # somebody may be using. But this scenario is edited and re-run (an EPP
    # feature gate, a replica count), and every re-run trips that guard.
    #
    # So the override is taken only when the namespace's shared route already
    # carries BOTH of this scenario's stacks -- which is what "these EPPs are
    # ours" looks like from outside. Anything else is left to the guard.
    local allow_reuse=false epps
    epps="$(k get deploy -o name 2>/dev/null | grep -c -- '-epp' || true)"
    if [ "${epps:-0}" -gt 0 ]; then
        local carried=1 s
        for s in $(role_stacks); do [ -n "$(stack_backend "$s")" ] || carried=0; done
        if [ "$carried" = 1 ]; then
            allow_reuse=true
            info "re-rendering: the route already carries $(and_list $(role_stacks)), so the ${epps} EPP(s) here are this scenario's"
        else
            die "$NS already runs ${epps} EPP(s), and the shared route does not carry both of this scenario's stacks -- so they belong to something else. A standup would apply its own EPP, Service, InferencePool and PodMonitor over them, silently. Use a clean namespace."
        fi
    fi

    # Recorded BEFORE the standup, so a standup that fails half-way still
    # leaves the teardown the scenario it has to undo.
    k create configmap "$STANDUP_RECORD" --from-literal=spec="$BENCH_SPEC" \
        --dry-run=client -o yaml | k apply -f - >/dev/null \
        || die "could not record the scenario in ConfigMap $STANDUP_RECORD; the teardown would not know what to undo"
    # The run stands up only its own stacks whenever the scenario has more: the
    # scenario copy keeps the run's (BENCHMARK_STACKS, scenario_overrides.sh),
    # so three models on n-model-share -- or two, with BENCH_SPEC pointed at it
    # -- do not hold the other models' GPUs for nothing. Empty when the
    # scenario is exactly the run's, as every two-model scenario is.
    local keep_stacks="" n_stacks
    n_stacks="$(scenario_stack_count)"
    [ "$n_stacks" -gt "$MODEL_COUNT" ] && keep_stacks="$(role_stacks | paste -sd, -)"
    if [ -n "$keep_stacks" ]; then
        info "standing up ${MODEL_COUNT} of the ${n_stacks} stacks of $BENCH_SPEC: $keep_stacks"
    elif [ "$MODEL_COUNT" = 2 ]; then
        info "standing up both stacks from $BENCH_SPEC"
    else
        info "standing up all ${MODEL_COUNT} stacks of $BENCH_SPEC"
    fi
    local gw_reuse
    gw_reuse="$(gateway_reuse)"
    [ "$gw_reuse" = true ] && info "reusing the cluster's gateway control plane in $GATEWAY_PROVIDER_NAMESPACE (GATEWAY_REUSE=$GATEWAY_REUSE)"
    ( cd "$ROOT" && \
        BENCHMARK_STACKS="$keep_stacks" \
        BENCHMARK_GATEWAY_SKIP_INSTALL="$gw_reuse" \
        BENCHMARK_GATEWAY_PROVIDER_NAMESPACE="$GATEWAY_PROVIDER_NAMESPACE" \
        BENCHMARK_DECODE_CPU="$DECODE_CPU" \
        BENCHMARK_DECODE_MEMORY="$DECODE_MEMORY" \
        BENCHMARK_DECODE_NODE="$BENCH_NODE" \
        BENCHMARK_DECODE_NODES="$(bench_nodes_csv)" \
        BENCHMARK_DECODE_TOLERATIONS="$tols" \
        BENCHMARK_ALLOW_EPP_REUSE="$allow_reuse" \
        BENCHMARK_NAMESPACE="$NS" \
        BENCHMARK_SPEC="$BENCH_SPEC" \
        BENCHMARK_MODEL_ID= \
        BENCHMARK_DECODE_REPLICAS="$MIN_REPLICAS" \
        BENCHMARK_KEDA_MIN_REPLICAS="$MIN_REPLICAS" \
        BENCHMARK_KEDA_MAX_REPLICAS="$(max_ceiling)" \
        BENCHMARK_SKIP_SMOKETEST=true \
        make --no-print-directory benchmark-standup ) || die "standup failed"
    # The smoketest is skipped because it cannot pass here -- its readiness poll
    # asks the gateway ROOT for /v1/models, which a path-prefixed multi-model
    # stack does not route, so it 404s for its whole 1800s timeout while both
    # models answer at their own prefixes. `verify` does the same job correctly:
    # it asks each model on the path the load will actually use.
    info "the standup's own smoketest was skipped (it cannot see a path-prefixed stack); 'verify' is the check that replaces it"

    # ROLL THE EPPs, because the EndpointPickerConfig is a ConfigMap and the EPP
    # reads it ONCE, at startup.
    #
    # Measured, and it cost a whole A/B: `featureGates: [flowControl]` was added
    # to the scenario and a re-run of this standup wrote it into both EPP
    # ConfigMaps -- verified on the cluster. The Deployment's pod template did
    # not change, so nothing restarted the pods, and
    # `process_start_time_seconds` showed both EPPs still running the config
    # they had loaded 35 minutes BEFORE the gate existed. They stayed that way
    # for the whole benchmark four hours later.
    #
    # The consequence is silent and total: no flow-control layer means no
    # inference_extension_flow_control_queue_size, which is the series WVA's
    # scheduler-queue query reads and the one scale-from-zero's queue fallback
    # depends on. The controller scaled on a signal that was simply absent.
    #
    # Unconditional: a restart of an EPP that was already current costs about
    # thirty seconds, and deciding whether one is needed means comparing a
    # rendered config against a running process, which is the comparison that
    # was got wrong in the first place.
    epp_restart
    apply_node_selector
    verb_status
    ok "stacks stood up. Run 'verify' before any load."
}

# apply_node_selector: with BENCH_NODES, the controller's gpu-inventory counts
# the listed nodes alone (WVA_NODE_SELECTOR), so the free GPUs it funds a
# replica from are per node and on the run's nodes -- not the whole cluster's,
# which would count GPUs on nodes the replicas may never land on. The quota
# still bounds the total. Without BENCH_NODES nothing changes.
apply_node_selector() {
    [ -n "$BENCH_NODES" ] || return 0
    local sel
    sel="$(bench_node_selector)"
    kc -n "$WVA_NS" set env deploy/wva-controller-manager WVA_NODE_SELECTOR="$sel" >/dev/null \
        || die "could not set WVA_NODE_SELECTOR on $WVA_NS/wva-controller-manager"
    kc -n "$WVA_NS" rollout status deploy/wva-controller-manager --timeout=300s >/dev/null \
        || die "the controller did not come back after WVA_NODE_SELECTOR was set"
    ok "the controller's gpu-inventory counts only $(bench_nodes_csv) (WVA_NODE_SELECTOR='$sel')"
}

# node_selector_problem: with BENCH_NODES, why the controller's gpu-inventory
# is not counting exactly those nodes, or nothing.
node_selector_problem() {
    [ -n "$BENCH_NODES" ] || return 0
    local ctl got want
    ctl="$(kc get deploy wva-controller-manager -n "$WVA_NS" -o json 2>/dev/null)" || {
        echo "could not read $WVA_NS/wva-controller-manager to check its WVA_NODE_SELECTOR"; return; }
    got="$(jq -r '[.spec.template.spec.containers[].env[]? | select(.name == "WVA_NODE_SELECTOR") | .value] | first // ""' <<<"$ctl" 2>/dev/null)"
    want="$(bench_node_selector)"
    [ "$got" = "$want" ] || echo "the controller's gpu-inventory reads WVA_NODE_SELECTOR='${got}', not '${want}': it would fund replicas from GPUs on nodes this run does not use. Run standup again with the same BENCH_NODES, or: kubectl -n ${WVA_NS} set env deploy/wva-controller-manager WVA_NODE_SELECTOR='${want}'"
}

# ---------------------------------------------------------------------------
# endpoints -- one gateway, one path per stack
# ---------------------------------------------------------------------------
gateway_host() {
    # REFUSES on ambiguity rather than taking the first. Two gateway Services in
    # one namespace means something else is deployed here, and driving the wrong
    # one produces a run of 404s that reads as a pool result.
    #
    # The HTTP port BY NAME, never ports[0]. Measured on a multi-node H200 cluster: the istio
    # gateway Service lists 15021 (status-port) first and 80 second, so ports[0]
    # sent every request at the readiness port -- which answers, with 404, for
    # the whole run.
    local svcs count
    svcs="$(k get svc -o json 2>/dev/null | jq -r '
        [.items[] | select(.metadata.name | test("inference-gateway|-gateway$"))
         | .metadata.name as $n
         | (.spec.ports[] | select(.name == "http" or .name == "http2" or .port == 80) | .port) as $p
         | $n + ":" + ($p|tostring)] | unique | .[]')"
    count="$(printf '%s\n' "$svcs" | grep -c . || true)"
    [ "${count:-0}" -eq 1 ] || return 1
    printf '%s' "$svcs"
}

# The stack's backend, read from the HTTPRoute the gateway actually routes on.
#
# NOT from the scenario's `model.shortName`: the harness IGNORES that and
# generates `{first8}-{sha256(namespace/model)[:8]}-{last8}` regardless --
# measured, `unsloth/Meta-Llama-3.1-8B-Instruct` in this namespace became
# `unsloth--244120d9-instruct`. Matching the stack name against Deployment names
# therefore finds nothing, silently, and `verify` would report a stack that is
# plainly serving as absent.
#
# The route is also the RIGHT source: it is the same table the load generator's
# requests follow, so what this resolves and what the run measures cannot drift.
stack_backend() {
    k get httproute -o json 2>/dev/null | jq -r --arg p "/$1" '
        [.items[].spec.rules[]?
         | select([.matches[]?.path.value] | index($p))
         | .backendRefs[0].name] | .[0] // ""'
}

decode_deploy_for() {
    local backend prefix
    backend="$(stack_backend "$1")"
    [ -n "$backend" ] || return 0
    # The modelservice chart names the pool `<shortName>-router` and the
    # Deployment `<shortName>-decode`.
    prefix="${backend%-router}"
    k get deploy "${prefix}-decode" -o name >/dev/null 2>&1 && printf '%s' "${prefix}-decode"
}

# ---------------------------------------------------------------------------
# Single-stack P/D: one model, its prefill and decode under one quota
# ---------------------------------------------------------------------------
# PD_SINGLE_STACK=1 drives ONE P/D model, stood up by
#   make benchmark-standup BENCHMARK_SPEC=guides/pd-disaggregation MODEL_ID=<model>
# and not by `standup`: that scenario is single-stack, its router reached through
# its own Service, with no shared Gateway or HTTPRoute to find a stack by. Both
# loaders drive the one model through PD_ENDPOINT -- the router Service's base
# URL, http://<service>:<port> -- with different shapes (INPUT/OUTPUT_TOKENS and
# their _B), so prefill and decode take turns at needing the GPUs. MODEL_A and
# MODEL_B both name that model.
PD_SINGLE_STACK="${PD_SINGLE_STACK:-0}"
PD_ENDPOINT="${PD_ENDPOINT:-}"

# Every model Deployment and its role, one "<deployment> <role>" per line.
# The modelservice chart puts llm-d.ai/role on the POD TEMPLATE, not on the
# Deployment, so a label selector on Deployments finds nothing; the template is
# also where the ScaledObject planner reads it (deploy/lib/scaledobject.sh).
role_deploys() {
    if [ "$PD_SINGLE_STACK" = 1 ]; then
        k get deploy -o json 2>/dev/null | jq -r '
            .items[] | (.spec.template.metadata.labels["llm-d.ai/role"] // "") as $r
            | select($r == "prefill" or $r == "decode") | .metadata.name + " " + $r'
        return
    fi
    local s d
    for s in $(role_stacks); do
        d="$(decode_deploy_for "$s")"
        [ -n "$d" ] || continue
        echo "$d decode"
        k get deploy "${d%-decode}-prefill" -o name >/dev/null 2>&1 && echo "${d%-decode}-prefill prefill"
    done
}

role_min() { if [ "$1" = prefill ]; then echo "$MIN_PREFILL"; else echo "$MIN_DECODE"; fi; }
role_max() { if [ "$1" = prefill ]; then echo "$MAX_PREFILL"; else echo "$MAX_DECODE"; fi; }

# floor_ceiling is the lowest ceiling a fleet floor must fit under:
# set_fleet_floor raises EVERY model ScaledObject, prefill included, and KEDA
# refuses a minReplicaCount above its maxReplicaCount. Read from the roles
# that run, as set_arm_ceiling applies MAX_PREFILL to any prefill it finds.
floor_ceiling() {
    if [ "$MAX_PREFILL" -lt "$MAX_DECODE" ] && role_deploys | grep -q ' prefill$'; then
        echo "$MAX_PREFILL"
    else
        echo "$MAX_DECODE"
    fi
}

# The role of the Deployment a ScaledObject scales: decode unless it is labelled
# prefill, which is every aggregated stack.
so_role() {
    local target
    target="$(k get scaledobject "$1" -o jsonpath='{.spec.scaleTargetRef.name}' 2>/dev/null)"
    if [ -n "$target" ] && [ "$(k get deploy "$target" -o jsonpath='{.spec.template.metadata.labels.llm-d\.ai/role}' 2>/dev/null)" = prefill ]; then
        echo prefill
    else
        echo decode
    fi
}


gateway_ip() {
    # The gateway Service's ClusterIP. Resolved ONCE, here, so the load never
    # resolves anything.
    #
    # Measured twice on a multi-node H200 cluster, at only ~10 rps aggregate: 5.7% of requests
    # failed with `ClientConnectorDNSError: Temporary failure in name
    # resolution`, spread across the whole run rather than bunched at startup --
    # so waiting for the endpoint to answer first does not fix it. aiohttp
    # resolves per connection, and with the default `ndots:5` a name of four
    # dots is tried against every search domain before the absolute one, so each
    # connection costs four lookups. That is the driver's own failure, three
    # times the report's client-side loss threshold, and it voids the arm.
    #
    # A raw IP is safe here because the shared HTTPRoute matches on PATH and
    # declares no hostnames -- checked: `hostnames=[]`. If that ever changes,
    # this has to carry a Host header instead.
    local svc="$1" ip
    ip="$(k get svc "$svc" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
    case "$ip" in
        ""|None) return 1 ;;
    esac
    printf '%s' "$ip"
}

base_url_for_stack() {
    # What inference-perf wants: the gateway plus this stack's path prefix, and
    # NOTHING else. Its client appends the route (`/v1/completions`) itself, so
    # a base_url that already carries one produces `/v1/completions/v1/
    # completions` -- a 404 from the gateway on every request, for a whole run.
    local hostport="$1" stack="$2" host
    host="$(gateway_ip "${hostport%%:*}")" || \
        host="${hostport%%:*}.$NS.svc.cluster.local."
    printf 'http://%s:%s/%s' "$host" "${hostport##*:}" "$stack"
}
endpoint_for_stack() {
    # The full completions URL, for the driver's own probes.
    printf '%s/v1/completions' "$(base_url_for_stack "$@")"
}

# Single-stack P/D replaces the stack lookups. Defined AFTER the originals: a
# bash function is whichever definition ran last, and an override placed above
# base_url_for_stack was silently replaced by it -- the loaders would have
# driven the gateway, not the P/D router.
if [ "$PD_SINGLE_STACK" = 1 ]; then
    gateway_host() {
        [ -n "$PD_ENDPOINT" ] || return 1
        local h="${PD_ENDPOINT#*://}"
        printf '%s' "${h%%/*}"
    }
    base_url_for_stack() { printf '%s' "${PD_ENDPOINT%/}"; }
    decode_deploy_for() { role_deploys | awk '$2 == "decode" { print $1; exit }'; }
fi

# ---------------------------------------------------------------------------
# BENCH_SIM=1: the whole flow on a kind cluster, with simulators for engines
# ---------------------------------------------------------------------------
# A dry run of everything after the standup -- the real controller install and
# ScaledObject registration (benchmark-deploy-wva), reset, the share arms, the
# loader, the sampler, collection and every report -- against
# llm-d-inference-sim engines that hold EMULATED accelerators. What it cannot
# show is anything about latency: the simulator's TTFT is a setting.
#
# Each stack is a Deployment `<stack>-decode` (role decode, the GPUs per
# replica the scenario gives the stack), a Service `<stack>-sim` the loader
# drives directly -- there is no gateway or EPP -- and a PodMonitor so the
# controller reads the simulator's vLLM metrics. Refused on anything but kind:
# the simulators request accelerators, and on a real cluster they would hold
# them doing nothing.
SIM_TTFT="${SIM_TTFT:-200ms}"
SIM_ITL="${SIM_ITL:-20ms}"
SIM_MAX_NUM_SEQS="${SIM_MAX_NUM_SEQS:-4}"
# The node label the simulators are pinned by, the way the e2e pins them:
# nvidia.com/gpu.product=<value>. Empty pins nothing.
SIM_GPU_PRODUCT="${SIM_GPU_PRODUCT:-}"

if [ "$BENCH_SIM" = 1 ]; then
    gateway_host() { printf 'sim:8000'; }
    stack_backend() { k get svc "$1-sim" -o name >/dev/null 2>&1 && printf '%s' "$1-sim"; }
    decode_deploy_for() { k get deploy "$1-decode" -o name >/dev/null 2>&1 && printf '%s' "$1-decode"; }
    base_url_for_stack() {
        local ip
        ip="$(gateway_ip "$2-sim")" || ip="$2-sim.$NS.svc.cluster.local."
        printf 'http://%s:8000' "$ip"
    }
    epp_deploys() { :; }
fi

# sim_manifest <a|b>: one simulator stack, as YAML.
sim_manifest() {
    local role="$1" stack model gpus sel=""
    stack="$(rvar STACK "$role")"; model="$(rvar MODEL "$role")"
    gpus="$(model_gpus "$role")"
    if [ -n "$SIM_GPU_PRODUCT" ] || [ -n "$BENCH_NODE" ]; then
        sel="      nodeSelector:"
        [ -z "$SIM_GPU_PRODUCT" ] || sel="$sel
        nvidia.com/gpu.product: \"$SIM_GPU_PRODUCT\""
        [ -z "$BENCH_NODE" ] || sel="$sel
        kubernetes.io/hostname: \"$BENCH_NODE\""
    fi
    # BENCH_NODES: any of the listed nodes, by a required affinity term -- the
    # shape the chart renders for the real stacks.
    if [ -n "$BENCH_NODES" ]; then
        sel="${sel:+$sel
}      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kubernetes.io/hostname
                    operator: In
                    values: $(bench_nodes | jq -R . | jq -cs .)"
    fi
    local tols
    tols="$(tolerations_json 2>/dev/null)" || tols=""
    [ -z "$tols" ] || sel="${sel:+$sel
}      tolerations: $tols"
    cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${stack}-decode
  namespace: $NS
  labels:
    app.kubernetes.io/part-of: wva-two-model-sim
spec:
  replicas: $MIN_REPLICAS
  selector:
    matchLabels:
      app: ${stack}-decode
  template:
    metadata:
      labels:
        app: ${stack}-decode
        app.kubernetes.io/part-of: wva-two-model-sim
        llm-d.ai/role: decode
        llm-d.ai/inferenceServing: "true"
        llm-d.ai/model: ${stack}
    spec:
$sel
      terminationGracePeriodSeconds: 10
      containers:
        - name: vllm
          image: $SIM_IMAGE
          imagePullPolicy: IfNotPresent
          # --model is a name the simulator does not know, so it tokenizes on
          # its own: given a Hugging Face id it wants a tokenizer sidecar and
          # exits without one. The real id is the name it serves, and the name
          # ScaledObject planning registers.
          args:
            - --model
            - "sim-${stack}"
            - --served-model-name
            - "$model"
            - --port
            - "8000"
            - --time-to-first-token=$SIM_TTFT
            - --inter-token-latency=$SIM_ITL
            - --mode=random
            - --max-num-seqs
            - "$SIM_MAX_NUM_SEQS"
            - --max-model-len
            - "32768"
          ports:
            - name: modelserver
              containerPort: 8000
          readinessProbe:
            tcpSocket:
              port: 8000
            periodSeconds: 5
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
              nvidia.com/gpu: "$gpus"
            limits:
              cpu: "2"
              memory: 2Gi
              nvidia.com/gpu: "$gpus"
---
apiVersion: v1
kind: Service
metadata:
  name: ${stack}-sim
  namespace: $NS
  labels:
    app.kubernetes.io/part-of: wva-two-model-sim
spec:
  selector:
    app: ${stack}-decode
  ports:
    - name: http
      port: 8000
      targetPort: 8000
YAML
}

# sim_monitor: the model-server PodMonitor every install asks for
# (config/modelserver-metrics: decode and prefill pods, port `modelserver`).
# Its CRD arrives with the monitoring stack the controller install brings.
sim_monitor() {
    kc apply -n "$NS" -k "$ROOT/config/modelserver-metrics" >/dev/null
}

# sim_cluster_problem: why this cluster is not one a simulator may hold
# accelerators on, or nothing. Every node's providerID starts kind:// on kind.
sim_cluster_problem() {
    local ids
    ids="$(kc get nodes -o jsonpath='{range .items[*]}{.spec.providerID}{"\n"}{end}' 2>&1)" || {
        echo "could not read the nodes to tell whether this is a kind cluster: $ids"; return; }
    [ -n "$ids" ] || { echo "no nodes"; return; }
    if printf '%s\n' "$ids" | grep -qv '^kind://'; then
        echo "BENCH_SIM=1 runs on kind only: the simulators request accelerators, and on this cluster (providerIDs: $(printf '%s' "$ids" | sort -u | tr '\n' ' ')) they would hold real ones doing nothing"
    fi
}

sim_standup() {
    local why
    why="$(sim_cluster_problem)"
    [ -z "$why" ] || die "$why"
    kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f - >/dev/null || die "could not create namespace $NS"
    k create configmap "$STANDUP_RECORD" --from-literal=spec="sim:$BENCH_SPEC" \
        --dry-run=client -o yaml | k apply -f - >/dev/null \
        || die "could not record the standup in ConfigMap $STANDUP_RECORD"
    local role
    for role in $ROLES; do
        info "simulator stack for model $role: $(model_gpus "$role") GPU(s) per replica"
        sim_manifest "$role" | k apply -f - >/dev/null || die "could not apply the simulator stack for model $role"
    done
    for role in $(role_stacks); do
        k rollout status "deploy/${role}-decode" --timeout=600s >/dev/null || die "${role}-decode did not become ready"
    done
    # The install refuses model servers nothing scrapes, once it can tell: when
    # the PodMonitor CRD exists already (a second standup), the monitors go
    # first. On a fresh cluster the install brings the CRD, and they follow it.
    if kc get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1; then
        sim_monitor || die "could not apply config/modelserver-metrics"
    fi
    ok "simulator stacks are up; installing the controller the way the real standup does"
    ( cd "$ROOT" && \
        BENCHMARK_NAMESPACE="$NS" \
        BENCHMARK_KEDA_MIN_REPLICAS="$MIN_REPLICAS" \
        BENCHMARK_KEDA_MAX_REPLICAS="$(max_ceiling)" \
        make --no-print-directory benchmark-deploy-wva ) || die "benchmark-deploy-wva failed"
    sim_monitor || die "could not apply config/modelserver-metrics (is the PodMonitor CRD installed?)"
    verb_status
    ok "simulator stacks stood up. Run 'verify' before any load."
}

sim_teardown() {
    local sos d
    k delete job -l app.kubernetes.io/name=wva-two-model-load --ignore-not-found --timeout=120s >/dev/null 2>&1
    k delete configmap wva-two-model-load --ignore-not-found --timeout=60s >/dev/null 2>&1
    # The ScaledObjects first: left behind, KEDA would scale the Deployments
    # back up while they are being deleted. EVERY simulator stack present, not
    # only this invocation's models: a teardown run without the standup's
    # MODEL_COUNT would otherwise leave models C and D's ScaledObjects behind.
    local present
    present="$(k get deploy -l app.kubernetes.io/part-of=wva-two-model-sim \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
    for d in $( { printf '%s\n' "$present"; role_stacks | sed 's/$/-decode/'; } | grep -v '^$' | sort -u); do
        sos="$(k get scaledobject -o json 2>/dev/null | jq -r --arg d "$d" \
            '.items[] | select(.spec.scaleTargetRef.name == $d) | .metadata.name')"
        for so in $sos; do k delete scaledobject "$so" --ignore-not-found >/dev/null 2>&1; done
    done
    k delete podmonitor wva-modelserver-metrics --ignore-not-found --timeout=60s >/dev/null 2>&1
    k delete deploy,svc -l app.kubernetes.io/part-of=wva-two-model-sim --ignore-not-found --timeout=120s >/dev/null 2>&1 \
        || warn "could not delete every simulator object; check below"
    k delete configmap "$STANDUP_RECORD" --ignore-not-found --timeout=60s >/dev/null 2>&1
    verb_status
    ok "teardown done (simulator stacks). The controller stays; it holds no accelerators."
}

# The EPP Deployment behind each stack: the route's backendRef names the pool
# (`<shortName>-router`), and the chart names its EPP `<shortName>-router-epp`.
epp_deploys() {
    local s backend
    for s in $(role_stacks); do
        backend="$(stack_backend "$s")"
        [ -n "$backend" ] || continue
        k get deploy "${backend}-epp" -o name >/dev/null 2>&1 && printf '%s\n' "${backend}-epp"
    done
}

epp_restart() {
    local d any=0
    for d in $(epp_deploys); do
        any=1
        info "restarting $d so it re-reads its EndpointPickerConfig"
        k rollout restart "deploy/$d" >/dev/null || warn "could not restart $d"
    done
    [ "$any" = 1 ] || { warn "found no EPP Deployment to restart"; return 0; }
    for d in $(epp_deploys); do
        k rollout status "deploy/$d" --timeout=300s >/dev/null 2>&1 || \
            warn "$d did not report a completed rollout; check it before trusting the queue signal"
    done
    ok "EPPs restarted"
}

# Where WVA reads its metrics, taken from the controller's OWN config so this
# cannot drift from what the controller actually queries.
prometheus_base_url() {
    # WVA_NS, not NS. The controller's ConfigMap lives with the controller, and
    # the two namespaces are only the same in a namespace-scoped install. Read
    # from the model namespace on a cluster-scoped one and this finds nothing --
    # which used to mean the flow-control check quietly passed.
    kc -n "$WVA_NS" get cm wva-manager-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null \
        | sed -n 's/^PROMETHEUS_BASE_URL: *"\{0,1\}\([^"]*\)"\{0,1\}[[:space:]]*$/\1/p' | head -1
}

# prometheus_tls_setting <KEY> echoes one PROMETHEUS_* key of the controller's
# config.yaml, unquoted, or nothing.
prometheus_tls_setting() {
    kc -n "$WVA_NS" get cm wva-manager-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null \
        | sed -n "s/^$1: *\"\{0,1\}\([^\"]*\)\"\{0,1\}[[:space:]]*\$/\1/p" | head -1
}

# prom_tls_env sets PROM_TLS_ENV to the --env arguments a query Pod needs to
# reach Prometheus with the controller's own TLS settings: PROM_CA_B64, the CA
# it verifies with (base64 of key ca.crt of the Secret mounted at its
# PROMETHEUS_CA_CERT_PATH -- wva-prometheus-ca when the installer copied it,
# PROM_CA_SECRET to name another), and PROM_TLS_VERIFY=false only when the
# controller itself does not verify. The CA travels as an environment value,
# not a volume, because the query Pods run in NS and the Secret in WVA_NS.
prom_tls_env() {
    PROM_TLS_ENV=()
    local ca_path secret="${PROM_CA_SECRET:-}" b64=""
    if [ "$(prometheus_tls_setting PROMETHEUS_TLS_INSECURE_SKIP_VERIFY)" = "true" ]; then
        PROM_TLS_ENV=(--env=PROM_TLS_VERIFY=false)
        return 0
    fi
    ca_path="$(prometheus_tls_setting PROMETHEUS_CA_CERT_PATH)"
    if [ -z "$secret" ] && [ -n "$ca_path" ]; then
        secret="$(kc -n "$WVA_NS" get deployment wva-controller-manager -o json 2>/dev/null | jq -r --arg ca "$ca_path" '
            .spec.template.spec as $s
            | ((($s.containers | map(select(.name == "manager")) | .[0]) // $s.containers[0]).volumeMounts // [])
            | map(select(. as $m | $ca | startswith(($m.mountPath | rtrimstr("/")) + "/")))
            | sort_by(.mountPath | length) | last // empty | .name as $v
            | $s.volumes[] | select(.name == $v) | .secret.secretName // empty' 2>/dev/null)"
    fi
    if [ -n "$secret" ]; then
        b64="$(kc -n "$WVA_NS" get secret "$secret" -o jsonpath='{.data.ca\.crt}' 2>/dev/null)"
    fi
    if [ -n "$ca_path" ] && [ -z "$b64" ]; then
        warn "could not read the CA the controller verifies Prometheus with (${secret:-no Secret found for $ca_path} in $WVA_NS);"
        warn "  query Pods verify against the public roots only. Set PROM_CA_SECRET=<secret with ca.crt in $WVA_NS>."
    fi
    [ -z "$b64" ] || PROM_TLS_ENV=("--env=PROM_CA_B64=$b64")
}

# PROM_TLS_PY is the Python a query Pod builds its TLS context with, from
# PROM_TLS_ENV. Chain and hostname are verified. The one check relaxed is
# VERIFY_X509_STRICT (on by default since Python 3.13): it also rejects a CA
# certificate without a keyUsage extension, which the self-signed certificate
# a kube-prometheus-stack is commonly served with lacks, and which Go -- the
# controller -- accepts. PROM_TLS_VERIFY=false mirrors a controller that
# itself runs with PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true.
PROM_TLS_PY='import base64, os, ssl
if os.environ.get("PROM_TLS_VERIFY") == "false":
    ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
else:
    ctx = ssl.create_default_context(cadata=base64.b64decode(os.environ.get("PROM_CA_B64", "")).decode() or None)
    ctx.verify_flags &= ~getattr(ssl, "VERIFY_X509_STRICT", 0)'

# prom_tls_preflight connects to the controller's Prometheus from a Pod in NS
# with the controller's TLS settings -- curl --cacert with its CA, never -k --
# before anything holds an accelerator. A certificate that would be refused
# fails preflight; a probe that could not run says so and does not.
PROM_TLS_PROBE_IMAGE="${PROM_TLS_PROBE_IMAGE:-quay.io/curl/curl:8.11.1}"
prom_tls_preflight() {
    local url pod phase="" code="" out="" waited=0
    url="$(prometheus_base_url)"
    if [ -z "$url" ]; then
        info "no controller config in $WVA_NS yet: the installer probes the Prometheus TLS connection when it installs"
        return 0
    fi
    case "$url" in https://*) ;; *) return 0 ;; esac
    prom_tls_env
    if [ "${PROM_TLS_ENV[*]:-}" = "--env=PROM_TLS_VERIFY=false" ]; then
        warn "the controller does not verify $url (PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true); nothing to probe"
        return 0
    fi
    pod="tlsprobe-$(date +%s)-$RANDOM"
    k run "$pod" --restart=Never --quiet --image="$PROM_TLS_PROBE_IMAGE" ${PROM_TLS_ENV[@]+"${PROM_TLS_ENV[@]}"} \
        --env="PROM_URL=$url" --command -- sh -c '
if [ -n "${PROM_CA_B64:-}" ]; then
    printf %s "$PROM_CA_B64" | base64 -d > /tmp/ca.crt || exit 2
    set -- --cacert /tmp/ca.crt
fi
exec curl -sS -o /dev/null -w "HTTP %{http_code}" --max-time 15 "$@" "$PROM_URL"' >/dev/null 2>&1 \
        || { warn "could not start a Prometheus TLS probe Pod in $NS; not checked"; return 0; }
    while [ "$waited" -lt 120 ]; do
        phase="$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"
        case "$phase" in Succeeded|Failed) break ;; esac
        sleep 2
        waited=$((waited + 2))
    done
    code="$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)"
    out="$(k logs "$pod" 2>/dev/null)"
    if printf '%s\n' "$out" | grep -q '^curl: ('; then
        out="$(printf '%s\n' "$out" | grep -m1 '^curl: (')"
    else
        out="$(printf '%s\n' "$out" | tail -1)"
    fi
    k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    case "$phase:$code" in
        Succeeded:0)
            ok "Prometheus TLS verified with the controller's settings: $url (${out:-no answer})"
            return 0 ;;
        Failed:51|Failed:60|Failed:77)
            warn "the controller's TLS settings do not verify $url: $out"
            warn "  re-run the installer, which copies the Prometheus CA and picks an address its certificate names"
            return 1 ;;
        *)
            warn "could not complete the Prometheus TLS probe (phase ${phase:-unknown}, exit ${code:-none}: ${out:-no output}); not a verdict"
            return 0 ;;
    esac
}

# THE FLOW-CONTROL QUEUE MUST EXIST AS A SERIES, not as a line in a ConfigMap.
#
# This is the check that was missing. `featureGates: [flowControl]` was present
# and correct in both EPP ConfigMaps, `benchmark-deploy-wva`'s preflight was
# satisfied by exactly that, and the metric it gates was never emitted -- so
# WVA's scheduler-queue query returned no series for either model through two
# 34-minute arms. A config is a statement of intent; the series is the fact.
verify_flow_control() {
    # NOT KNOWING IS NOT PASSING. This check exists because the flow-control gate
    # was present and correct in both EPP ConfigMaps while the metric it gates
    # was never emitted, through two 34-minute arms, and nothing said so. A
    # version of it that returns success when it could not look would have let
    # exactly that run through -- so both unknowns below refuse, and
    # SKIP_FLOW_CONTROL_CHECK=1 is the deliberate way past.
    if [ "${SKIP_FLOW_CONTROL_CHECK:-0}" = "1" ]; then
        warn "SKIP_FLOW_CONTROL_CHECK=1: not checking that WVA's scheduler-queue signal exists"
        return 0
    fi
    if [ "$BENCH_SIM" = 1 ]; then
        warn "BENCH_SIM=1: the simulator stacks have no EPP, so there is no flow-control queue to check"
        return 0
    fi
    local url; url="$(prometheus_base_url)"
    if [ -z "$url" ]; then
        warn "could not read PROMETHEUS_BASE_URL from wva-manager-config in $WVA_NS."
        warn "  Set WVA_NS if the controller runs elsewhere. Without it nothing establishes that"
        warn "  WVA's scheduler-queue signal exists, and the run would scale on a metric that"
        warn "  may not be emitted at all. SKIP_FLOW_CONTROL_CHECK=1 to proceed anyway."
        return 1
    fi
    info "checking the EPP flow-control queue is a real series in $url"
    local q out pod
    q='sum by (model_name) (llm_d_epp_flow_control_queue_size) or sum by (model_name) (inference_extension_flow_control_queue_size)'
    pod="fcheck-$(date +%s)-$RANDOM"
    prom_tls_env
    k run "$pod" --restart=Never --quiet --image="$(load_image)" ${PROM_TLS_ENV[@]+"${PROM_TLS_ENV[@]}"} --command -- \
        python3 -c "
import json,time,urllib.parse,urllib.request
$PROM_TLS_PY
url='$url'+'/api/v1/query?'+urllib.parse.urlencode({'query':'''$q'''})
deadline=time.time()+180
while time.time()<deadline:
    try:
        r=json.loads(urllib.request.urlopen(url, timeout=60, context=ctx).read().decode())
        print('MODELS ' + ' '.join(sorted(s['metric'].get('model_name','?') for s in r['data']['result'])))
        break
    except Exception as e:
        last=e; time.sleep(5)
else:
    print('QUERYFAIL', last)
" >/dev/null 2>&1 || true
    local waited=0
    while [ "$waited" -lt 240 ]; do
        case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
            Succeeded|Failed) break ;;
        esac
        sleep 5
        waited=$(( waited + 5 ))
    done
    out="$(k logs "$pod" 2>&1)"
    k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
    case "$out" in
        *QUERYFAIL*)
            warn "could not query Prometheus at $url: $out"
            warn "  The same reasoning as above: an unanswered query says nothing about whether"
            warn "  the signal exists. SKIP_FLOW_CONTROL_CHECK=1 to proceed anyway."
            return 1 ;;
    esac
    local missing="" m
    case "$out" in *"$MODEL_A"*) : ;; *) missing="$MODEL_A" ;; esac
    for m in $(role_models | tail -n +2); do
        case "$out" in *"$m"*) : ;; *) missing="$missing $m" ;; esac
    done
    if [ -n "$missing" ]; then
        warn "the EPP flow-control queue has NO series for:$missing"
        warn "  Prometheus returned: ${out:-<nothing>}"
        warn "  The gate is in the ConfigMap but the EPP is not running it -- it reads that config"
        warn "  ONCE, at startup. WVA's scheduler-queue signal is absent, and the run would scale"
        warn "  on a metric that does not exist. Restart the EPPs and check again:"
        warn "      kubectl rollout restart deploy -n $NS $(epp_deploys | tr '\n' ' ')"
        return 1
    fi
    if [ "$MODEL_COUNT" = 2 ]; then
        ok "the EPP flow-control queue is live for both models"
    else
        ok "the EPP flow-control queue is live for all ${MODEL_COUNT} models"
    fi
}

# ---------------------------------------------------------------------------
# The utilization-share arms: today, shadow, share
# ---------------------------------------------------------------------------
# Three arms on the same traffic under the same quota, differing ONLY in the
# policy's `optimizer:` block: none (today's optimizer), the utilization-share
# optimizer in shadow (computes, moves nothing), and the same acting. The quota
# is set once at standup (WVA_LIMITER=quota WVA_QUOTAS=...), never per arm, and
# each arm keeps the policy it ran under so the report can refuse two arms whose
# limiters differ.

is_share_arm() { case "$1" in today|shadow|share) return 0 ;; esac; return 1; }

# The mode the controller must report before an arm's load starts.
share_arm_mode() {
    case "$1" in today) echo off ;; shadow) echo shadow ;; share) echo active ;; esac
}

# Run PromQL queries from inside the cluster, where Prometheus is reachable, and
# print one JSON object {query: result}. Queries go in through the environment,
# not spliced into the program text, so no quoting can change what is asked.
# EVAL_TIME (unix seconds) evaluates every query at that instant; empty is now.
prom_queries() {
    local eval_time="$1"; shift
    local url; url="$(prometheus_base_url)"
    [ -n "$url" ] || { warn "could not read PROMETHEUS_BASE_URL from wva-manager-config in $WVA_NS"; return 1; }
    local queries; queries="$(jq -cn '$ARGS.positional' --args "$@")"
    local pod="pquery-$(date +%s)-$RANDOM"
    prom_tls_env
    k run "$pod" --restart=Never --quiet --image="$(load_image)" ${PROM_TLS_ENV[@]+"${PROM_TLS_ENV[@]}"} \
        --env="PROM_URL=$url" --env="QUERIES=$queries" --env="EVAL_TIME=$eval_time" \
        --command -- python3 -c '
import json, os, urllib.parse, urllib.request
'"$PROM_TLS_PY"'
out = {}
for q in json.loads(os.environ["QUERIES"]):
    params = {"query": q}
    if os.environ.get("EVAL_TIME"):
        params["time"] = os.environ["EVAL_TIME"]
    try:
        r = json.loads(urllib.request.urlopen(os.environ["PROM_URL"] + "/api/v1/query?" + urllib.parse.urlencode(params), timeout=60, context=ctx).read().decode())
        out[q] = r["data"]["result"] if r.get("status") == "success" else {"error": r.get("error", "query failed")}
    except Exception as e:
        out[q] = {"error": str(e)}
print("RESULT " + json.dumps(out))
' >/dev/null 2>&1 || { warn "could not start a query Pod"; return 1; }
    local waited=0
    while [ "$waited" -lt 240 ]; do
        case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
            Succeeded|Failed) break ;;
        esac
        sleep 3
        waited=$(( waited + 3 ))
    done
    local out; out="$(k logs "$pod" 2>&1)"
    k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
    printf '%s\n' "$out" | sed -n 's/^RESULT //p' | grep . || { warn "the query Pod returned nothing: $out"; return 1; }
}

# The policy this arm rewrites must be the one the controller reads. A
# namespace-scoped controller that manages its own namespace reads, in order:
# the namespace named by the wva.llmd.ai/policy-namespace label on that
# namespace, the wva-policy namespace if it exists, its own namespace. Only the
# last is the benchmark's to edit. The others are shared with other tenants, so
# this refuses rather than rewriting them -- and an arm run against the
# benchmark's own copy while the controller reads another would measure a quota
# nobody wrote down.
check_policy_namespace() {
    # A controller outside the benchmark namespace is cluster-scoped or serves
    # other namespaces: its policy is what every tenant it manages gets, and an
    # arm would switch the optimizer on, or sweep every live transfer, for all
    # of them. The arms rewrite only a policy that is the benchmark's own.
    [ "$WVA_NS" = "$NS" ] || [ "${SHARE_ALLOW_SHARED_POLICY:-0}" = "1" ] || \
        die "the controller runs in $WVA_NS, not in $NS: its policy is shared with every namespace it manages, and the share arms would rewrite it for all of them. Install a namespace-scoped controller in $NS (the standup does), or set SHARE_ALLOW_SHARED_POLICY=1 if that policy really is yours alone."
    # The shared policy namespace itself is never the benchmark's own.
    [ "$NS" != "wva-policy" ] || [ "${SHARE_ALLOW_SHARED_POLICY:-0}" = "1" ] || \
        die "$NS is the shared policy namespace; the share arms do not rewrite it. Run in a namespace of your own, or set SHARE_ALLOW_SHARED_POLICY=1 if that policy really is yours alone."
    # Sitting in $NS is not enough: a cluster-scoped controller installed there
    # still manages every namespace. Namespace-scoped means --watch-namespace
    # set to $NS, directly or through WVA_WATCH_NAMESPACE.
    if [ "${SHARE_ALLOW_SHARED_POLICY:-0}" != "1" ]; then
        local ctl scope errf
        # Stderr apart: kubectl's throttling and API warnings would otherwise
        # land in the JSON, fail the parse, and read as "watches every
        # namespace" -- sending someone to reinstall a controller that is fine.
        errf="$(mktemp)"
        ctl="$(kc get deploy wva-controller-manager -n "$WVA_NS" -o json 2>"$errf")" || {
            local e; e="$(cat "$errf")"; rm -f "$errf"
            die "could not read the controller Deployment in $WVA_NS to check its scope: $e
  Set SHARE_ALLOW_SHARED_POLICY=1 if you know it manages $NS alone."
        }
        rm -f "$errf"
        scope="$(watch_scope <<<"$ctl")" || die "could not parse the controller Deployment in $WVA_NS to check its scope. Set SHARE_ALLOW_SHARED_POLICY=1 if you know it manages $NS alone."
        case "$scope" in
            "$NS"|"<own>") : ;;
            "") die "the controller in $WVA_NS watches every namespace (no --watch-namespace): its policy is shared, and the share arms would rewrite it for all of them. Install a namespace-scoped controller in $NS, or set SHARE_ALLOW_SHARED_POLICY=1." ;;
            "<multiple>") die "the controller in $WVA_NS sets --watch-namespace more than once; it watches the last, which this check will not guess at. Set SHARE_ALLOW_SHARED_POLICY=1 if it manages $NS alone." ;;
            "<unknown>") die "the controller in $WVA_NS takes its watch namespace from something this check cannot resolve (not a literal, not the pod namespace). Set SHARE_ALLOW_SHARED_POLICY=1 if it manages $NS alone." ;;
            *) die "the controller in $WVA_NS watches $scope, not $NS: the share arms would rewrite a policy that is not the benchmark's. Set SHARE_ALLOW_SHARED_POLICY=1 if it is yours alone." ;;
        esac
    fi
    [ "${SKIP_POLICY_NAMESPACE_CHECK:-0}" = "1" ] && {
        warn "SKIP_POLICY_NAMESPACE_CHECK=1: not checking that the controller reads the policy in $WVA_NS"
        return 0
    }
    local out label
    # Forbidden is not absent: a namespace this identity cannot read may still
    # be the one the controller reads.
    out="$(kc get namespace "$NS" -o json 2>&1)" || \
        die "could not read namespace $NS to find which policy the controller reads: $out
  Set SKIP_POLICY_NAMESPACE_CHECK=1 if you know the policy in $WVA_NS is the one in force."
    label="$(jq -r '.metadata.labels["wva.llmd.ai/policy-namespace"] // ""' <<<"$out")"
    if [ -n "$label" ] && [ "$label" != "$WVA_NS" ]; then
        die "namespace $NS is labelled to read its policy from $label, not $WVA_NS. That policy is shared; the share arms do not rewrite it. Run in a namespace without the label."
    fi
    [ -n "$label" ] && return 0
    out="$(kc get namespace wva-policy -o name 2>&1)"
    case "$out" in
        namespace/wva-policy)
            [ "$WVA_NS" = "wva-policy" ] || \
                die "a wva-policy namespace exists, so a controller managing its own namespace reads its policy from there, not from $WVA_NS. That policy is shared; the share arms do not rewrite it." ;;
        *NotFound*|*"not found"*) : ;;
        *) die "could not tell whether a wva-policy namespace exists: $out
  Set SKIP_POLICY_NAMESPACE_CHECK=1 if you know the policy in $WVA_NS is the one in force." ;;
    esac
}

# watch_scope reads a controller Deployment (JSON on stdin) and prints the
# namespace it watches, from its manager container (or the first): the
# --watch-namespace value (=VALUE or a separate VALUE, one dash or two) with
# every $(VAR) in it resolved through the container env as the kubelet does,
# the last definition of a name winning. It prints <own> when that ends at the
# pod namespace fieldRef, <unknown> when it ends anywhere else, <multiple> when
# the flag is given more than once (the controller would take the last; a
# guard that read the first could be fooled), and nothing when it watches
# every namespace. The installs chain it: --watch-namespace=$(WVA_WATCH_NAMESPACE),
# whose value is $(POD_NAMESPACE), which is the fieldRef.
watch_scope() {
    jq -r '
      .spec.template.spec.containers as $cs
      | (($cs | map(select(.name == "manager")) | first) // $cs[0]) as $c
      | ($c.env // []) as $env
      | def resolve($v; $n):
          if $n > 5 then "<unknown>"
          elif ($v | test("^\\$\\([A-Za-z_][A-Za-z0-9_]*\\)$")) then
            ($v | ltrimstr("$(") | rtrimstr(")")) as $name
            | ($env | map(select(.name == $name)) | last // {}) as $e
            | if ($e.value // "") != "" then resolve($e.value; $n + 1)
              elif $e.valueFrom.fieldRef.fieldPath == "metadata.namespace" then "<own>"
              else "<unknown>" end
          else $v end;
      ([($c.command // [])[], ($c.args // [])[]]) as $argv
      | [range(0; $argv | length) as $i
         | if ($argv[$i] | test("^--?watch-namespace=")) then ($argv[$i] | sub("^--?watch-namespace="; ""))
           elif (($argv[$i] == "--watch-namespace") or ($argv[$i] == "-watch-namespace")) and ($i + 1 < ($argv | length))
             then $argv[$i + 1]
           else empty end] as $flags
      | if ($flags | length) == 0 then ""
        elif ($flags | length) > 1 then "<multiple>"
        else resolve($flags[0]; 0) end'
}

# Write the arm's `optimizer:` block into the policy every controller reads,
# replacing whatever block is there and touching nothing else.
set_share_policy() {
    local arm="$1" current updated
    check_policy_namespace
    current="$(kc -n "$WVA_NS" get configmap wva-scaling-policy-config -o jsonpath='{.data.default}' 2>&1)" || \
        die "could not read the scaling policy in $WVA_NS: $current"
    # The comparison is under a quota, and the optimizer has no budget without
    # one -- it would report `invalid` and the arm would measure today's
    # optimizer under the wrong name.
    printf '%s\n' "$current" | grep -Eq '^[[:space:]-]*type:[[:space:]]*quota[[:space:]]*$' || \
        die "the scaling policy in $WVA_NS declares no quota limiter. Stand up with WVA_LIMITER=quota WVA_QUOTAS='<accelerator>=<n>' -- the share arms compare the optimizers under one quota."
    updated="$(python3 "$HERE/two_model_share_report.py" --rewrite-policy "$arm" <<<"$current")" || \
        die "could not rewrite the policy for arm $arm"
    kc -n "$WVA_NS" patch configmap wva-scaling-policy-config --type=merge \
        -p "$(jq -cn --arg d "$updated" '{data:{"default":$d}}')" >/dev/null || \
        die "could not write the policy for arm $arm"
}

# ---------------------------------------------------------------------------
# Node data: what lets 1-GPU donors fund a 2-GPU replica
# ---------------------------------------------------------------------------
# The utilization-share optimizer combines several small donor pods into one
# larger replica only when node data shows them on one node. A namespace-scoped
# controller has node data only with a gpu-inventory limiter in its policy, and
# then it needs two cluster-scoped reads no namespace owner can grant
# themselves: nodes (the install's prerequisites phase grants it) and pods in
# every namespace (nothing in the installer does). Without the pod list it
# counts only its own namespace's GPUs, so another team's pods on the node read
# as free. Without node data at all a mixed-size run measures
# `no-compatible-donor`, which is the documented limit and not the experiment.
CONTROLLER_SA="${CONTROLLER_SA:-wva-controller-manager}"

# can_i_sa <verb> <resource> [--all-namespaces]: yes | no | unknown, asked as
# the controller's ServiceAccount. unknown when this identity may not
# impersonate it -- never folded into no or yes.
can_i_sa() {
    local out rc=0
    out="$(kc auth can-i "$@" --as "system:serviceaccount:${WVA_NS}:${CONTROLLER_SA}" 2>&1)" || rc=$?
    case "$out" in
        yes*) echo yes ;;
        no*) if [ "$rc" -ne 0 ]; then echo no; else echo unknown; fi ;;
        *) echo unknown ;;
    esac
}

# node_data_admin_steps: the exact commands an admin runs, once.
node_data_admin_steps() {
    cat <<EOF
  # 1. Node read for the controller. The install's prerequisites phase grants
  #    it (ClusterRole/ClusterRoleBinding wva-node-reader-*); as a cluster admin:
  make setup-prereqs ENVIRONMENT=kubernetes SCOPE=namespace NAMESPACE=${NS} WVA_NS=${WVA_NS}
  # 2. List on pods in every namespace. Nothing in the installer grants it:
  kubectl create clusterrole wva-pod-lister-${WVA_NS} --verb=list --resource=pods
  kubectl create clusterrolebinding wva-pod-lister-${WVA_NS} --clusterrole=wva-pod-lister-${WVA_NS} --serviceaccount=${WVA_NS}:${CONTROLLER_SA}
  # (or, from this repo as that admin: BENCHMARK_NAMESPACE=${NS} bash hack/benchmark/two_model_pool.sh node-data grant)
  # 3. Restart the controller so it re-checks node access at once:
  kubectl rollout restart deployment/wva-controller-manager -n ${WVA_NS}
  # Remove both after the benchmark:
  kubectl delete clusterrolebinding wva-pod-lister-${WVA_NS}; kubectl delete clusterrole wva-pod-lister-${WVA_NS}
EOF
}

# node_data_problems: one line per thing missing for node data, or nothing.
# Each read that cannot be made is a problem too: not knowing is not passing.
node_data_problems() {
    local policy nodes pods status ready
    policy="$(kc -n "$WVA_NS" get configmap wva-scaling-policy-config -o jsonpath='{.data.default}' 2>&1)" || {
        echo "could not read the scaling policy in $WVA_NS: $policy"; return; }
    printf '%s\n' "$policy" | grep -Eq '^[[:space:]-]*type:[[:space:]]*(gpu-inventory|inventory)[[:space:]]*$' || \
        echo "the policy in $WVA_NS declares no gpu-inventory limiter (stand up with WVA_LIMITER=gpu-inventory,quota)"
    nodes="$(can_i_sa list nodes)"
    if [ "$nodes" = unknown ]; then
        # The controller's readiness gate refuses to serve a gpu-inventory
        # policy it cannot back with a node list, so Ready answers it.
        ready="$(kc -n "$WVA_NS" get deploy wva-controller-manager -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
        [ "${ready:-0}" -ge 1 ] 2>/dev/null && nodes=yes
    fi
    case "$nodes" in
        no) echo "the controller ($WVA_NS/$CONTROLLER_SA) may not list nodes" ;;
        unknown) echo "could not tell whether the controller may list nodes (no impersonation, and the controller is not Ready)" ;;
    esac
    pods="$(can_i_sa list pods --all-namespaces)"
    if [ "$pods" = unknown ]; then
        # What the controller found itself: podListDenied is present in its
        # status ConfigMap while its cluster-wide pod list is refused.
        status="$(kc -n "$WVA_NS" get configmap wva-controller-status -o json 2>/dev/null)"
        if [ -n "$status" ]; then
            if jq -e '.data | has("podListDenied")' <<<"$status" >/dev/null 2>&1; then pods=no; else pods=yes; fi
        fi
    fi
    case "$pods" in
        no) echo "the controller ($WVA_NS/$CONTROLLER_SA) may not list pods in every namespace, so GPUs other namespaces hold on the node read as free" ;;
        unknown) echo "could not tell whether the controller may list pods in every namespace (no impersonation, no status ConfigMap)" ;;
    esac
}

# check_node_data: refuses a share arm that needs node data and lacks it, with
# the admin steps. Every share arm, not only share: the arms are compared under
# one policy, and today's optimizer reads the gpu-inventory limiter too.
check_node_data() {
    node_data_needed || return 0
    local problems
    problems="$(node_data_problems)"
    [ -n "$problems" ] || { ok "node data: gpu-inventory in the policy, node read and a cluster-wide pod list for the controller"; return 0; }
    warn "this run needs node data (NODE_DATA=$NODE_DATA; replicas of $(model_sizes | sed 's/ / and /g') GPUs), and:"
    printf '%s\n' "$problems" | sed 's/^/      /' >&2
    warn "an admin runs, once:"
    node_data_admin_steps >&2
    return 1
}

verb_node_data() {
    need_ns
    if [ "${1:-}" = grant ]; then
        # Step 2 only: step 1 is the install's prerequisites phase, which owns
        # the node-reader names. Idempotent: create-or-apply.
        kc create clusterrole "wva-pod-lister-${WVA_NS}" --verb=list --resource=pods \
            --dry-run=client -o yaml | kc apply -f - >/dev/null || die "could not create ClusterRole wva-pod-lister-${WVA_NS} (a cluster-admin action)"
        kc create clusterrolebinding "wva-pod-lister-${WVA_NS}" --clusterrole="wva-pod-lister-${WVA_NS}" \
            --serviceaccount="${WVA_NS}:${CONTROLLER_SA}" --dry-run=client -o yaml | kc apply -f - >/dev/null \
            || die "could not create ClusterRoleBinding wva-pod-lister-${WVA_NS} (a cluster-admin action)"
        ok "granted ${WVA_NS}/${CONTROLLER_SA} list on pods in every namespace (ClusterRole/ClusterRoleBinding wva-pod-lister-${WVA_NS})"
    fi
    local problems
    problems="$(node_data_problems)"
    if [ -z "$problems" ]; then
        ok "node data: gpu-inventory in the policy, node read and a cluster-wide pod list for the controller"
        return 0
    fi
    warn "node data is missing:"
    printf '%s\n' "$problems" | sed 's/^/      /' >&2
    warn "an admin runs, once:"
    node_data_admin_steps >&2
    return 1
}

# The policy is intent; the controller's mode gauge is the fact. Wait until
# Prometheus shows the mode this arm needs. On a namespace-scoped install whose
# policy lives in another namespace the policy is read once, at start, and this
# is where that shows: the wait times out instead of measuring the wrong arm.
wait_share_mode() {
    local want="$1" deadline=$(( $(date +%s) + ${SHARE_MODE_TIMEOUT:-300} )) res
    local q="max by (mode) (wva_utilization_share_mode{namespace=\"$WVA_NS\"})"
    # Asked once before the deadline is consulted. The clock is whole seconds, so
    # a `while now < deadline` guard skips the loop whenever the second turns
    # between the two reads -- with a short budget, without asking at all.
    while :; do
        res="$(prom_queries "" "$q")" && \
            [ "$(jq -r --arg q "$q" --arg m "$want" '[.[$q][]? | select(.metric.mode==$m) | .value[1]] | first // ""' <<<"$res")" = "1" ] && {
                ok "the controller reports utilization-share mode '$want'"
                return 0
            }
        [ "$(date +%s)" -lt "$deadline" ] || break
        sleep "${SHARE_MODE_POLL:-15}"
    done
    warn "the controller did not report mode '$want' within ${SHARE_MODE_TIMEOUT:-300}s. Last answer: ${res:-<none>}"
    return 1
}

# What the optimizer did over the arm's window, read at the window's end:
# transfers by outcome, model-seconds held back by reason, seconds a replica
# move was planned (the shadow arm's evidence) and the mode it held throughout.
capture_share_metrics() {
    local out="$1" end="$2" window="$3" step=15
    local sel="namespace=\"$WVA_NS\""
    prom_queries "$end" \
        "sum by (outcome) (increase(wva_utilization_share_transfers_total{$sel}[${window}s]))" \
        "sum by (reason) (count_over_time((wva_model_scaling_blocked{$sel} == 1)[${window}s:${step}s])) * $step" \
        "count_over_time((max(wva_utilization_share_replicas_to_move{$sel}) > 0)[${window}s:${step}s]) * $step" \
        "min by (mode) (min_over_time(wva_utilization_share_mode{$sel}[${window}s]))" \
        "sum by (model_name, reason) (count_over_time((wva_model_scaling_blocked{$sel} == 1)[${window}s:${step}s])) * $step" \
        "sum by (variant_name, limiter_name) (increase(wva_decisions_limited_total{$sel}[${window}s]))" \
        > "$out.tmp" && [ -s "$out.tmp" ] && mv "$out.tmp" "$out" || {
            rm -f "$out.tmp"
            warn "could not read the optimizer's metrics for this arm; the report will say so"
        }
}

# What the controller logged over one arm, and the namespace's events, kept
# with the arm's results. The report counts transfers by outcome from this log:
# the counter's increase() missed a series' first sample in the run the
# well-lit path reports, and the log names every transfer, donor set and fill.
# A capture that comes back smaller than one already kept does not replace it.
capture_share_log() {
    local out_dir="$1" since="$2" tmp
    tmp="$(mktemp)"
    kc -n "$WVA_NS" logs deploy/wva-controller-manager --since-time="$since" > "$tmp" 2>&1 || \
        warn "could not read the controller log for this arm; the report will count transfers from the metric alone"
    if [ ! -s "$out_dir/controller.log" ] || [ "$(wc -c < "$tmp")" -gt "$(wc -c < "$out_dir/controller.log")" ]; then
        mv "$tmp" "$out_dir/controller.log"
    else
        rm -f "$tmp"
    fi
    # A restart inside the arm loses everything before it from the live log.
    local restarts
    restarts="$(kc -n "$WVA_NS" get pods -l control-plane=controller-manager -o json 2>/dev/null | jq -r '
        [.items[].status.containerStatuses[]?.restartCount] | add // 0' 2>/dev/null)"
    if [ "${restarts:-0}" -gt 0 ] 2>/dev/null; then
        kc -n "$WVA_NS" logs deploy/wva-controller-manager --previous > "$out_dir/controller.previous.log" 2>&1 || true
        warn "the controller has restarted ${restarts} time(s); its previous log is in controller.previous.log"
    fi
    k get events --sort-by=.lastTimestamp > "$out_dir/events.txt" 2>&1 || true
}

verb_verify() {
    need_ns
    local hostport
    if ! hostport="$(gateway_host)"; then
        die "did not find exactly one inference gateway Service in $NS. Both models are reached through one; driving the wrong one is a run of 404s that reads as a result.
    $(k get svc -o name 2>/dev/null | sed 's/^/      /')"
    fi
    info "gateway: $hostport"

    local rc=0 d ready
    for s in $(role_stacks); do
        d="$(decode_deploy_for "$s")"
        if [ -z "$d" ]; then
            warn "no decode Deployment matching stack '$s'"
            rc=1
            continue
        fi
        ready="$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
        info "  $s -> $d (ready: ${ready:-0})"
        [ "${ready:-0}" -ge 1 ] || { warn "  $s has no ready replica"; rc=1; }
    done
    [ "$rc" -eq 0 ] || die "$([ "$MODEL_COUNT" = 2 ] && echo both || echo "all $MODEL_COUNT") stacks must be serving before anything is measured"

    # Two EPPs, which is the topology this scenario is about. One EPP serving
    # both pools would still run, and the run would be measuring something else.
    # (An EPP per model: three or four for a run of three or four.)
    local epps
    epps="$(k get deploy -o name 2>/dev/null | grep -c -- '-epp' || true)"
    local want_epps="$MODEL_COUNT"
    [ "$PD_SINGLE_STACK" != 1 ] || want_epps=1
    # The simulator stacks have no gateway and no EPP: the loader drives each
    # simulator's Service directly.
    [ "$BENCH_SIM" != 1 ] || want_epps=0
    [ "${epps:-0}" -ge "$want_epps" ] || \
        die "expected an EPP per model and found ${epps:-0}. The standup rendered fewer stacks than this run drives."
    [ "$want_epps" -eq 0 ] || ok "$epps EPP deployment(s), one per model"
    # WHAT WAS DEPLOYED IS WHAT THE RUN IS PRICED ON. The standup renders the
    # engines' resources from a scenario copy, and that copy has been
    # re-rendered under us before (the 32B CPU/memory figures came back over an
    # override). A replica holding other than the GPUs the run counts makes
    # every GPU-second and every quota bound in the report wrong.
    if ! scenario_is_pd; then
        local role held want_g
        for role in $ROLES; do
            d="$(decode_deploy_for "$(rvar STACK "$role")")"
            [ -n "$d" ] || continue
            held="$(k get deploy "$d" -o json 2>/dev/null | jq -r '
                [.spec.template.spec.containers[]?.resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0' 2>/dev/null)"
            want_g="$(model_gpus "$role")"
            [ -z "$held" ] || [ "$held" = "$want_g" ] || \
                die "$d requests $held GPU(s) per replica, but the run counts model $role at $want_g. Fix the scenario or GPUS_PER_REPLICA_$(printf '%s' "$role" | tr a-z A-Z) and stand up again."
            [ -z "$held" ] || ok "  $d: $held GPU(s) per replica, as the run counts it"
        done
    fi
    # Every role serving, prefill included: a P/D model with no ready prefill
    # serves through decode alone, and the run would measure an aggregated model.
    local rd role
    while read -r rd role; do
        [ -n "$rd" ] || continue
        ready="$(k get deploy "$rd" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
        [ "${ready:-0}" -ge 1 ] || die "$role Deployment $rd has no ready replica"
    done < <(role_deploys)

    # A real request per model, through its own path, before half an hour of
    # load. A 404 here is a name; a 404 at minute 12 is a wasted run.
    local probe_rc=0 pr
    for pr in $ROLES; do
        verify_probe "$(endpoint_for_stack "$hostport" "$(rvar STACK "$pr")")" "$(rvar MODEL "$pr")" || probe_rc=1
    done
    [ "$probe_rc" -eq 0 ] || die "at least one model did not answer a single request"

    # IS THE CONTROLLER ACTUALLY READING? This is the failure that produces two
    # flat runs which agree perfectly and mean nothing: a Prometheus that does
    # not scrape this namespace leaves every variant looking idle, so NEITHER
    # arm ever scales and the report compares two straight lines. Nothing else
    # in this scenario would notice -- the stacks serve, the pool lends nothing
    # because nothing ever asks, and both arms finish.
    #
    # Warned, not fatal: a controller that is still starting is normal at this
    # point, and refusing here would block a standup that is merely young.
    local wva_ready
    wva_ready="$(k get deploy -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[*].status.readyReplicas}' 2>/dev/null)"
    if [ -z "${wva_ready:-}" ] || [ "${wva_ready:-0}" = "0" ]; then
        warn "no READY WVA controller in $NS. Nothing will scale, and both arms would be flat."
    else
        ok "WVA controller ready"
    fi
    local not_ready
    not_ready="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[]
         | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null))
         | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length == 0)
         | .metadata.name + " (" + ([.status.conditions[]? | select(.type=="Ready") | .message // "no Ready condition"] | join("; ")) + ")"]
        | .[]' 2>/dev/null)"
    if [ -n "$not_ready" ]; then
        warn "ScaledObjects that are not Ready -- these models cannot scale, so the run would compare two flat lines:"
        printf '%s\n' "$not_ready" | sed 's/^/      /' >&2
    else
        ok "every model ScaledObject reports Ready"
    fi
    local errs
    errs="$(controller_metric_errors)"
    if [ -n "$errs" ]; then
        warn "the controller is logging metric-read errors; it may be pointed at a Prometheus that does not scrape this namespace:"
        printf '%s\n' "$errs" | sed 's/^/      /' >&2
    fi

    verify_flow_control || \
        die "the EPP flow-control queue is not being emitted, so WVA's scheduler-queue signal does not exist. Any comparison run now measures an autoscaler reading a metric that is absent."

    if k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1; then
        info "pool '$POOL_NAME':"
        k get pods -l "llm-d.ai/warm-pool=$POOL_NAME" -o wide 2>/dev/null | sed 's/^/    /' >&2
    else
        info "no pool present -- this is the 'nopool' arm"
    fi
    ok "verify passed"
}

# One real request, from a Pod, RETRIED.
#
# Not `kubectl run --rm -i`: that ties the result to an attach, and on this
# cluster it fails two ways that have nothing to do with the model --
# "timed out waiting for the condition" when the attach loses the race with the
# container, and "Temporary failure in name resolution" when the Pod's DNS is
# not up yet (a service mesh adds a sidecar the application does not wait for).
# Both were reported as "the model did NOT answer", about a model that was
# answering when asked again a second later.
#
# So: create, wait for it to FINISH, read its log, delete. And try more than
# once, because the first attempt after a rollout legitimately loses that race.
#
# On a P/D model the probe has to move KV. A one-word prompt fills no KV block,
# so prefill hands decode nothing and decode serves it alone: a probe like that
# kept answering 200 on a stack whose every real request was a 500 on a broken
# KV link. So P/D sends three prompts of about a thousand tokens, each with its
# own first block so no prefix cache can serve it, and all three must answer.
verify_probe() {
    local url="$1" model="$2" out pod attempt rc words=0 count=1
    if scenario_is_pd; then words=1024; count=3; fi
    info "probing $model at $url"
    for attempt in 1 2 3; do
        pod="probe-$(date +%s)-$RANDOM"
        k run "$pod" --restart=Never --quiet --image="$(load_image)" --command -- \
            python3 -c "
import json,time,urllib.request
# Retried INSIDE the Pod. A fresh Pod here cannot resolve DNS for the first
# half-minute or so -- a mesh sidecar starts alongside the container and the
# application does not wait for it -- and one attempt reports a serving model as
# dead. Measured: attempts 1 and 2 failed to resolve, attempt 3 returned 200.
def body(i):
    prompt='hello' if $words == 0 else 'probe %d %f ' % (i, time.time()) + 'hello ' * $words
    return json.dumps({'model':'$model','prompt':prompt,'max_tokens':4}).encode()
deadline=time.time()+180
last=''
done=0
while time.time()<deadline and done<$count:
    try:
        req=urllib.request.Request('$url', data=body(done), headers={'Content-Type':'application/json'})
        r=urllib.request.urlopen(req, timeout=120)
        if r.status!=200:
            raise Exception('HTTP %d' % r.status)
        done+=1
    except Exception as e:
        last=str(e)
        time.sleep(5)
print('HTTP 200' if done==$count else 'FAILED after %d of $count: %s' % (done, last))
" >/dev/null 2>&1 || true
        # A Pod that runs for a second is never observed Ready, so wait on the
        # phase instead of on a condition.
        local waited=0
        while [ "$waited" -lt 180 ]; do
            case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
                Succeeded|Failed) break ;;
            esac
            sleep 5
            waited=$(( waited + 5 ))
        done
        out="$(k logs "$pod" 2>&1)"
        k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
        case "$out" in
            *"HTTP 200"*) ok "  $model answered (attempt $attempt)"; return 0 ;;
            *"name resolution"*|*"timed out waiting"*|"")
                warn "  attempt $attempt could not reach the network from the probe Pod (${out:-no output}); retrying"
                sleep 10
                continue ;;
            *) warn "  $model did NOT answer: $out"; return 1 ;;
        esac
    done
    warn "  $model: three probe Pods failed to reach the network. That is the probe, not the model -- check DNS/mesh readiness in $NS."
    return 1
}

# The image the load Pod runs, resolved from a Deployment already in the
# namespace so the nodes have pulled it -- a multi-GB pull at the start of a
# timed run puts the registry inside the measurement.
load_image() {
    # The llm-d-benchmark harness image, because inference-perf -- the harness's
    # load generator -- is what drives this scenario.
    #
    # The TAG follows the checked-out clone, not a constant. llm-d-benchmark's
    # own defaults.yaml pins the image to v0.7.0 regardless of the ref, and the
    # Makefile already had to work around that for the standup: a profile
    # schema written for one version against a binary from another fails at
    # parse time, minutes into a run. Same hazard here -- the profiles this
    # scenario renders are read by whatever inference-perf is in this image.
    if [ -n "$LOAD_IMAGE" ]; then printf '%s' "$LOAD_IMAGE"; return 0; fi
    local ref=""
    if [ -d "$ROOT/llm-d-benchmark/.git" ]; then
        ref="$(git -C "$ROOT/llm-d-benchmark" describe --tags --exact-match 2>/dev/null)"
    fi
    printf 'ghcr.io/llm-d/llm-d-benchmark:%s' "${ref:-v0.7.8}"
}

# ---------------------------------------------------------------------------
# the pool
# ---------------------------------------------------------------------------
verb_pool_create() {
    need_ns
    local accel="$ACCELERATOR"
    if [ -z "$accel" ]; then
        accel="$(kc get nodes -o json 2>/dev/null | jq -r '
            [.items[].metadata.labels
             | (."nvidia.com/gpu.product", ."gpu.nvidia.com/model")
             | select(. != null and . != "")] | unique | if length == 1 then .[0] else "" end')"
    fi
    [ -n "$accel" ] || die "set ACCELERATOR=<product as the node label spells it>: this cluster does not advertise exactly one, and a pool on the wrong accelerator is never eligible to lend."
    if [ -z "$MONITORING_NS" ]; then
        # prometheus-operated.<ns>.svc... -- the namespace is the second label.
        MONITORING_NS="$(prometheus_base_url | sed -n 's|^https\{0,1\}://[^.]*\.\([^.:/]*\)\..*$|\1|p')"
        [ -n "$MONITORING_NS" ] && info "pool will be scraped from $MONITORING_NS (from the controller's PROMETHEUS_BASE_URL; set MONITORING_NS to override)" \
            || warn "could not derive the monitoring namespace from the controller's PROMETHEUS_BASE_URL; the pool will NOT be scraped. Set MONITORING_NS."
    fi
    info "creating pool '$POOL_NAME': ${POOL_REPLICAS} Pods, reserve ${POOL_RESERVE}, ${GPUS_PER_REPLICA} GPU each, on ${accel}"
    # --max EQUAL to --replicas: the pool's own ScaledObject must not resize it
    # during a run, or the pool's accelerators move under the measurement and
    # the arm's GPU-seconds stop meaning "what the insurance cost".
    # --models 2 --model-size 8B sizes the Pod's memory limit, which IS the
    # warm-set budget: it decides how many models a Pod can hold, and it is what
    # lets one pool serve both of these.
    "$ROOT/deploy/warmpool.sh" create \
        -n "$NS" \
        --name "$POOL_NAME" \
        --type bridge \
        --replicas "$POOL_REPLICAS" \
        --max "$POOL_REPLICAS" \
        --reserve "$POOL_RESERVE" \
        --gpus "$GPUS_PER_REPLICA" \
        --models 2 \
        --model-size 8B \
        --cache-claim "$CACHE_CLAIM" \
        --wva-namespace "$WVA_NS" \
        --accelerator "$accel" \
        ${POOL_MIN_HOLD:+--min-hold "$POOL_MIN_HOLD"} \
        ${POOL_MAX_HOLD:+--max-hold "$POOL_MAX_HOLD"} \
        ${MONITORING_NS:+--monitoring-namespace "$MONITORING_NS"} \
        || die "pool creation failed"
    ok "pool created. An idle pool Pod reports NotReady ON PURPOSE -- that is what keeps it out of the InferencePool. Do not wait on readyReplicas."
}

verb_pool_delete() {
    need_ns
    if ! k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1; then
        info "no pool named $POOL_NAME in $NS"
        return 0
    fi
    "$ROOT/deploy/warmpool.sh" delete -n "$NS" --name "$POOL_NAME" || die "pool deletion failed"
    ok "pool deleted"
}

# ---------------------------------------------------------------------------
# warm -- BOTH models resident before the load starts
# ---------------------------------------------------------------------------
scaledobject_for_model() {
    k get scaledobject -o json 2>/dev/null | jq -r --arg m "$1" '
        [.items[] | select([.spec.triggers[]?.metadata.modelID] | index($m)) | .metadata.name] | .[0] // ""'
}

known_model_ids() {
    k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[].spec.triggers[]?.metadata.modelID // empty] | unique | join(", ")'
}

pin_warm_copy() {
    local model="$1" so patch
    so="$(scaledobject_for_model "$model")"
    if [ -z "$so" ]; then
        warn "no ScaledObject carries modelID=$model, so nothing declares it to WVA."
        warn "  The modelIDs that DO exist here: $(known_model_ids)"
        return 1
    fi
    # The WHOLE trigger list is sent: a merge patch replaces a list rather than
    # merging into it, so patching one element would drop the others -- the
    # scaler address among them, which is what makes the model exist to WVA.
    patch="$(k get scaledobject "$so" -o json 2>/dev/null | jq -c --arg m "$model" --arg p "$POOL_NAME" '
        {spec: {triggers: [ .spec.triggers[]
            | if .metadata.modelID == $m
              then .metadata += {"warmPoolCopies": "1", "warmPool": $p}
              else . end ]}}')"
    [ -n "$patch" ] || { warn "could not build a trigger patch for $so"; return 1; }
    k patch scaledobject "$so" --type=merge -p "$patch" >/dev/null || {
        warn "could not patch $so"
        return 1
    }
    ok "  $model: pinned one warm copy in pool '$POOL_NAME' (ScaledObject $so)"
}

pool_pods() {
    k get pods -l "llm-d.ai/warm-pool=$POOL_NAME" \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null
}

# Asks each pool Pod's supervisor what it is holding, over LOOPBACK from inside
# the Pod.
#
# Not from a probe Pod: the pool's own NetworkPolicy admits :8001 only from the
# WVA controller, and deliberately so -- deploy/warmpool.sh says a bare
# podSelector "would put :8001 one kubectl run away from anyone who can create a
# Pod here". A probe Pod therefore hangs until its timeout and reports a warm
# pool as cold. `exec` into the container that already owns the port has no such
# problem, and that container runs `python3 /app/launcher.py`, so python3 is
# there by construction.
pool_residency() {
    local pod out
    for pod in $(pool_pods); do
        out="$(k exec "$pod" -c inference-server -- python3 -c "
import urllib.request
try:
    print(urllib.request.urlopen('http://127.0.0.1:8001/v2/vllm/instances', timeout=10).read().decode('utf-8','replace'))
except Exception as e:
    print('ERR', e)
" 2>/dev/null)"
        printf '%s\n' "$pod: $out"
    done
}

wait_resident() {
    local timeout="${1:-900}" pods deadline blob r have line all
    pods="$(pool_pods)"
    if [ -z "$pods" ]; then
        warn "pool '$POOL_NAME' has no Pods"
        return 1
    fi
    deadline=$(( $(date +%s) + timeout ))
    # Read once before the deadline is consulted, as in wait_share_mode.
    while :; do
        blob="$(pool_residency)"
        # Matched on the MODEL ID and on the stack short name, because the
        # supervisor keys instances on the variant and which of the two spellings
        # that is has never been checked against a live pool. Whichever matches,
        # matches; when neither does, the raw payload is printed rather than a
        # bare timeout, so the next run can be told what the identity really is.
        line=""; all=1
        for r in $ROLES; do
            have=0
            case "$blob" in *"$(rvar MODEL "$r")"*|*"$(rvar STACK "$r")"*) have=1 ;; esac
            [ "$have" = 1 ] || all=0
            line="$line${line:+ }$(upper "$r")=$have"
        done
        info "  resident: $line"
        if [ "$all" = 1 ]; then
            return 0
        fi
        [ "$(date +%s)" -lt "$deadline" ] || break
        sleep 10
    done
    warn "timed out. What the supervisors actually reported:"
    printf '%s\n' "$blob" | sed 's/^/      /' >&2
    return 1
}

verb_warm() {
    need_ns
    k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1 || \
        die "no pool named $POOL_NAME. Run pool-create first -- there is nothing to warm."
    local rc=0
    local m
    for m in $(role_models); do pin_warm_copy "$m" || rc=1; done
    [ "$rc" -eq 0 ] || die "could not pin a warm copy for both models; the pool would rank one of them out exactly when its burst arrives."
    if [ "${SKIP_WARM_GATE:-0}" = "1" ]; then
        warn "SKIP_WARM_GATE=1: not waiting for residency. The arm will measure whatever the pool happens to hold."
        return 0
    fi
    wait_resident "${WARM_TIMEOUT:-900}" || \
        die "the pool did not become resident in both models. Starting the pool arm now would measure a COLD pool -- the first burst paying a model load INTO the pool on top of the replica's own -- and report it as the pool's cost."
    ok "both models are resident. The pool arm can start."
}

# ---------------------------------------------------------------------------
# reset -- the fleet back to the floor, before every arm
# ---------------------------------------------------------------------------
pause_at() {
    # KEDA's documented pin. Scaling the Deployment directly does not hold: the
    # HPA KEDA generates puts it back within a cycle.
    local so="$1" n="$2"
    k annotate scaledobject "$so" "autoscaling.keda.sh/paused-replicas=$n" --overwrite >/dev/null
}

unpause() {
    k annotate scaledobject "$1" "autoscaling.keda.sh/paused-replicas-" >/dev/null 2>&1 || true
}

verb_reset() {
    need_ns
    local sos d n waited
    sos="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[] | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null)) | .metadata.name] | .[]')"
    [ -n "$sos" ] || die "no model ScaledObjects in $NS; there is nothing to reset."
    # A floor arm raised minReplicaCount; every arm starts from each role's
    # floor (MIN_REPLICAS, unless MIN_PREFILL/MIN_DECODE say otherwise).
    local role_of
    for so in $sos; do
        role_of="$(so_role "$so")"
        k patch scaledobject "$so" --type=merge \
            -p "{\"spec\":{\"minReplicaCount\":$(role_min "$role_of")}}" >/dev/null || \
            warn "could not set minReplicaCount on $so"
        pause_at "$so" "$(role_min "$role_of")" || warn "could not pin $so"
    done
    # PAUSE THEN SCALE. Pausing alone does not bring the fleet down: measured
    # here, KEDA reported `Paused=True` and deleted the HPA while leaving the
    # Deployment at the 3 replicas it had grown -- the pause FREEZES the count,
    # it does not set it. Waiting on the annotation alone hung until the reset
    # timed out, and an arm started from a frozen 3 would have begun with the
    # capacity the previous arm built.
    #
    # The pause still matters, and has to come first: without it the HPA puts
    # the replicas straight back within a cycle.
    local deploys
    deploys="$(role_deploys)"
    [ -n "$deploys" ] || die "no model Deployment found to reset"
    while read -r d role_of; do
        k scale deploy "$d" --replicas="$(role_min "$role_of")" >/dev/null || warn "could not scale $d"
    done <<<"$deploys"
    info "pinned every model ScaledObject at its role's floor and scaled the fleet down; waiting for it to settle"
    waited=0
    while [ "$waited" -lt "$RESET_TIMEOUT" ]; do
        local settled=1 want ready
        while read -r d role_of; do
            want="$(role_min "$role_of")"
            n="$(k get deploy "$d" -o jsonpath='{.status.replicas}' 2>/dev/null)"
            ready="$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
            [ "${n:-0}" = "$want" ] && [ "${ready:-0}" = "$want" ] || settled=0
        done <<<"$deploys"
        # status.replicas leaves out terminating pods, which still hold their
        # GPUs through their grace period: the next arm would start short.
        [ -z "$(terminating_model_pods "$deploys")" ] || settled=0
        if [ "$settled" = 1 ]; then
            for so in $sos; do unpause "$so"; done
            ok "every role is at its floor and ready; autoscaling released"
            return 0
        fi
        sleep 10
        waited=$(( waited + 10 ))
    done
    for so in $sos; do unpause "$so"; done
    local stuck
    stuck="$(terminating_model_pods "$deploys" | tr '\n' ' ')"
    [ -z "$stuck" ] || warn "still terminating (a pod on an unreachable node never finishes until force-deleted): $stuck"
    die "the fleet did not settle at $MIN_REPLICAS within ${RESET_TIMEOUT}s. Starting an arm from a fleet the previous arm left scaled makes the two incomparable."
}

# terminating_model_pods names the pods of the given model Deployments (one
# "deployment role" per line) that are being deleted, one per line. A pod is a
# Deployment's when its ReplicaSet is named <deployment>-<pod-template-hash>,
# as the Deployment controller names it -- not by prefix, which would count a
# sibling Deployment whose name extends this one's.
terminating_model_pods() {
    local names
    names="$(awk '{print $1}' <<<"$1" | jq -R . | jq -s .)"
    k get pods -o json 2>/dev/null | jq -r --argjson names "$names" '
      .items[] | select(.metadata.deletionTimestamp != null)
      | (.metadata.labels["pod-template-hash"] // "") as $h
      | select($h != "" and any(.metadata.ownerReferences[]?; .kind == "ReplicaSet"
          and (.name as $rs | any($names[]; $rs == . + "-" + $h))))
      | .metadata.name'
}

model_scaledobjects() {
    k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[] | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null)) | .metadata.name] | .[]'
}

# The fleet's floor, on the ScaledObjects. A Deployment scaled by hand does
# not hold: the HPA KEDA generates puts it back to the controller's target
# within a cycle, and at the low rate that target is 1.
set_fleet_floor() {
    local n="$1" so
    # Refused here, by name: KEDA rejects the patch, and the arm would
    # otherwise wait RESET_TIMEOUT for a floor no role can reach and report
    # only that it did not reach it.
    [ "$n" -le "$(floor_ceiling)" ] || die "floor ${n} is above a role's ceiling ($(floor_ceiling)): lower FLOOR_REPLICAS or raise MAX_PREFILL/MAX_DECODE"
    for so in $(model_scaledobjects); do
        k patch scaledobject "$so" --type=merge \
            -p "{\"spec\":{\"minReplicaCount\":$n}}" >/dev/null || die "could not set minReplicaCount=$n on $so"
    done
    info "minReplicaCount=$n on every model ScaledObject"
}

# EVERY role, prefill included: set_fleet_floor raises every model
# ScaledObject, and waiting on decode alone started a P/D floor arm while its
# prefill was still coming up.
wait_fleet_at() {
    local n="$1" waited=0 s d cur ready deploys
    while [ "$waited" -lt "$RESET_TIMEOUT" ]; do
        local settled=1
        for s in $(role_stacks); do
            [ -n "$(decode_deploy_for "$s")" ] || settled=0
        done
        deploys="$(role_deploys)"
        while read -r d _; do
            [ -n "$d" ] || continue
            cur="$(k get deploy "$d" -o jsonpath='{.status.replicas}' 2>/dev/null)"
            ready="$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
            [ "${cur:-0}" = "$n" ] && [ "${ready:-0}" = "$n" ] || settled=0
        done <<<"$deploys"
        [ "$settled" = 1 ] && { ok "every role of $([ "$MODEL_COUNT" = 2 ] && echo both || echo "all $MODEL_COUNT") models is at $n and ready"; return 0; }
        sleep 10
        waited=$(( waited + 10 ))
    done
    return 1
}

set_arm_ceiling() {
    local arm="$1" ceiling sos
    ceiling="$(arm_max_replicas "$arm")"
    sos="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[] | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null)) | .metadata.name] | .[]')"
    # Per role: a prefill ScaledObject gets MAX_PREFILL, every other MAX_DECODE.
    # Both default to the arm's ceiling, so an aggregated run is unchanged.
    local role_ceiling model
    for so in $sos; do
        role_ceiling="$ceiling"
        if [ "$MAX_PREFILL" != "$MAX_REPLICAS" ] || [ "$MAX_DECODE" != "$MAX_REPLICAS" ]; then
            role_ceiling="$(role_max "$(so_role "$so")")"
        fi
        # Per model, when the models' ceilings differ: the ScaledObject says
        # which model it registers (its trigger's modelID).
        if per_model_ceilings; then
            model="$(k get scaledobject "$so" -o json 2>/dev/null | jq -r '
                [.spec.triggers[]?.metadata.modelID // empty] | first // ""' 2>/dev/null)"
            local mrole
            mrole="$(role_of_model "$model")"
            if [ -n "$mrole" ]; then role_ceiling="$(rvar MAX_REPLICAS "$mrole")"
            else warn "ScaledObject $so registers model '${model:-<none>}', $([ "$MODEL_COUNT" = 2 ] && echo "neither MODEL_A nor MODEL_B" || echo "none of MODEL_A..MODEL_$(upper "${ROLES##* }")"); it keeps ceiling $role_ceiling"
            fi
        fi
        k patch scaledobject "$so" --type=merge \
            -p "{\"spec\":{\"maxReplicaCount\":$role_ceiling}}" >/dev/null || \
            warn "could not set maxReplicaCount on $so"
    done
    # The pool's Pods only exist in the pool arm, and saying otherwise here
    # misreports the one number that makes the arms comparable.
    local held=0
    [ "$arm" = "pool" ] && held="$POOL_REPLICAS"
    if scenario_is_pd; then
        info "arm '$arm': each model's prefill may reach ${MAX_PREFILL} replicas and its decode ${MAX_DECODE}; the pool holds ${held} Pod(s) -- ceiling $(( $(peak_gpus) + held * GPUS_PER_REPLICA )) accelerators"
    else
        local reach="model A may reach ${MAX_REPLICAS_A} replicas of $(model_gpus a) GPU(s), model B ${MAX_REPLICAS_B} of $(model_gpus b)" er
        for er in $EXTRA_ROLES; do
            reach="$reach, model $(upper "$er") $(rvar MAX_REPLICAS "$er") of $(model_gpus "$er")"
        done
        info "arm '$arm': $reach; the pool holds ${held} Pod(s) -- ceiling $(( $(peak_gpus) + held * GPUS_PER_REPLICA )) accelerators"
    fi
    echo "$ceiling"
}

# add_budget_fields <budget.json>: models C and D's ceilings and sizes (in a
# run of three or four), and model_deploys -- {role: Deployment prefix}, the
# name every Pod of that model starts with (<prefix>-decode-..., and
# <prefix>-prefill-... for P/D) -- which is how the reports split the sampled
# accelerators by model. A single-stack P/D run has one model and no split.
add_budget_fields() {
    local f="$1" extra="{}" deploys="{}" r d
    for r in $EXTRA_ROLES; do
        extra="$(jq -c --arg r "$r" --argjson m "$(rvar MAX_REPLICAS "$r")" --argjson g "$(model_gpus "$r")" \
            '. + {("max_replicas_" + $r): $m, ("gpus_per_replica_" + $r): $g}' <<<"$extra")" || return 1
    done
    if [ "$PD_SINGLE_STACK" != 1 ]; then
        for r in $ROLES; do
            d="$(decode_deploy_for "$(rvar STACK "$r")")"
            [ -n "$d" ] || continue
            deploys="$(jq -c --arg r "$r" --arg p "${d%-decode}" '. + {($r): $p}' <<<"$deploys")" || return 1
        done
        extra="$(jq -c --argjson d "$deploys" '. + {model_deploys: $d}' <<<"$extra")" || return 1
    fi
    jq -c --argjson e "$extra" '. + $e' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
}

# all_results_written: whether the loader log on stdin carries every model's
# RESULTS-WRITTEN-<role> marker. One container finishing is part of a run.
all_results_written() {
    local text r
    text="$(cat)"
    for r in $ROLES; do
        printf '%s' "$text" | grep -q "RESULTS-WRITTEN-$r" || return 1
    done
    return 0
}

# per_model_ceilings: whether any model's ceiling differs from MAX_REPLICAS.
per_model_ceilings() {
    local r
    for r in $ROLES; do
        [ "$(rvar MAX_REPLICAS "$r")" = "$MAX_REPLICAS" ] || return 0
    done
    return 1
}

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
sample_gpus() {
    # Every RUNNING Pod that holds an accelerator, with the labels that say what
    # it is doing. Pending Pods are excluded: they have requested an accelerator
    # and hold none, so counting them charges an arm for capacity it never got
    # -- and the arm that queues more is the one that would be charged.
    #
    # A pool Pod that has been LENT carries the borrowing model's labels, so
    # lend and return are visible here without any metrics plumbing, which
    # matters on a cluster where Prometheus may not be reachable from this shell.
    local out="$1" until_ts="$2" want now last="" pods_json line
    : > "$out"
    while [ "$(sampler_now)" -lt "$until_ts" ]; do
        # A PASS THAT NEVER RAN must leave a mark too. A wall-clock jump past
        # what one slow pass can take means this loop did not run: the machine
        # running it was suspended, or a call blocked past both timeouts. The
        # load runs in the cluster either way, so its latencies stand and the
        # cost cannot be priced -- the mark lets the report say which, and the
        # driver stop the arm (sampler_stalled).
        now="$(sampler_now)"
        sampler_mark_stall "$out" "$last" "$now"
        last="$now"
        # What the Deployments ASKED for, beside what is actually Running.
        # Without this the arms cannot be told apart when one of them never got
        # the replicas it was allowed -- measured: an arm whose Deployments
        # scaled to 3 within four minutes and never had more than ONE replica
        # Available for the whole 36 minutes, because the rest could not be
        # placed. Its numbers described a one-replica fleet and nothing said so.
        want="$(sampler_k get deploy -o json 2>/dev/null | jq -c '
            [.items[] | select(.metadata.name | test("decode|prefill"))
             | {(.metadata.name): (.spec.replicas // 0)}] | add // {}')"
        # Explicit, not ${want:-{}}: the brace inside that expansion terminates
        # it early in bash, jq then gets nothing, and every sample comes out
        # empty -- which reads as a namespace holding no accelerators at all.
        [ -n "$want" ] || want='{}'
        # A poll that FAILS must leave a mark. `|| true` on its own made every
        # failure invisible: one arm lost 587s of a 2310s run to failed polls
        # and the file simply had no lines for it, which is indistinguishable
        # from a namespace that held no accelerators. The report then priced
        # that arm at a fraction of the other's and printed the ratio.
        #
        # The status of the API call is read on its own, not through a pipe:
        # with no pipefail a pipe returns the status of its last command,
        # which exits 0 on empty input, so a failed poll wrote no mark at all.
        # An answer that renders to nothing is a failed poll as well.
        if ! pods_json="$(sampler_k get pods -o json 2>/dev/null)" || \
           ! line="$(jq -c --argjson now "$(sampler_now)" \
            --argjson want "$want" '
          {ts: $now,
           pods: [ .items[]
             | select(.status.phase=="Running")
             | {name: .metadata.name,
                ready: ([.status.containerStatuses[]?.ready] | all),
                node: (.spec.nodeName // ""),
                gpus: ([.spec.containers[].resources.requests["nvidia.com/gpu"]//"0"|tonumber]|add),
                model: (.metadata.labels["llm-d.ai/model"] // ""),
                pool:  (.metadata.labels["llm-d.ai/warm-pool"] // ""),
                serving: (.metadata.labels["llm-d.ai/inferenceServing"] // "")}
             | select(.gpus > 0) ],
           desired: $want}' <<<"$pods_json" 2>/dev/null)" || [ -z "$line" ]; then
            printf '{"ts": %s, "failed": true}\n' "$(sampler_now)" >> "$out"
        else
            printf '%s\n' "$line" >> "$out"
        fi
        sleep "$GPU_SAMPLE_SECONDS"
    done
    # A stall that runs past the end of the window ends the loop without
    # another pass through it, so it is looked for once more here.
    sampler_mark_stall "$out" "$last" "$(sampler_now)"
}

# The sampler's clock, a function so the check can move it.
sampler_now() { date +%s; }

# Longer than a sleep plus two calls that each ran to their outer timeout
# (sampler_k): past it, the loop did not run at all.
sampler_stall_seconds() {
    echo $(( GPU_SAMPLE_SECONDS + 2 * (GPU_SAMPLE_TIMEOUT_SECONDS + 5) + 10 ))
}

# sampler_mark_stall <out> <last> <now>: a stall mark, and a warning, when
# more than sampler_stall_seconds passed between two passes of the loop.
sampler_mark_stall() {
    local out="$1" last="$2" now="$3"
    { [ -n "$last" ] && [ $(( now - last )) -gt "$(sampler_stall_seconds)" ]; } || return 0
    printf '{"ts": %s, "stalled_since": %s}\n' "$now" "$last" >> "$out"
    warn "the GPU sampler did not run for $(( now - last ))s (the machine running it was suspended, or a call blocked); those seconds have no samples"
}

# sampler_k <kubectl args...>: one of the sampler's API calls, bounded twice.
# --request-timeout bounds the request; an outer timeout, where the machine
# has one, bounds what that does not -- an exec credential plugin, or a
# connection that never gets as far as sending.
sampler_k() {
    local t="$GPU_SAMPLE_TIMEOUT_SECONDS"
    if command -v timeout >/dev/null 2>&1; then
        timeout "$(( t + 5 ))" kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} -n "$NS" "$@" --request-timeout="${t}s"
    else
        k "$@" --request-timeout="${t}s"
    fi
}

# sampler_stalled <gpus.jsonl>: print the seconds the sampler's stall marks
# cover and succeed, or fail when there are none.
sampler_stalled() {
    local secs
    grep -q '"stalled_since"' "$1" 2>/dev/null || return 1
    secs="$(jq -rs '[.[] | select(.stalled_since != null) | .ts - .stalled_since] | add // 0' "$1" 2>/dev/null)"
    printf '%s\n' "${secs:-unknown}"
}

# check_sampler_settings: the sampler does its stall arithmetic in whole
# seconds. A duration kubectl understands (1m, 500ms) would reach kubectl
# and silently switch the stall check off, so anything else is refused,
# before an arm starts. Prints what is wrong and fails.
check_sampler_settings() {
    local v
    for v in GPU_SAMPLE_SECONDS GPU_SAMPLE_TIMEOUT_SECONDS; do
        if ! [[ "${!v}" =~ ^[0-9]+$ ]] || [ "${!v}" -eq 0 ]; then
            echo "$v must be a whole number of seconds above zero (got '${!v}')"
            return 1
        fi
    done
}

RUN_INHIBIT=""
# hold_awake: keep the machine running this driver from sleeping while an arm
# runs. The load runs in the cluster but the GPU sampler runs here, and a
# machine that sleeps stops it, so the arm's cost cannot be priced.
# systemd-inhibit (Linux with logind) and caffeinate (macOS) hold a sleep lock
# for as long as this shell lives; run_cleanup releases it sooner. Neither
# reaches a Windows host from WSL -- the runbook names the Windows settings --
# and wherever none holds, the stall check stops the arm instead.
hold_awake() {
    if command -v systemd-inhibit >/dev/null 2>&1; then
        systemd-inhibit --what=sleep:idle --who=two-model-benchmark \
            --why="a benchmark arm is sampling GPUs" --mode=block \
            tail --pid="$$" -f /dev/null >/dev/null 2>&1 &
    elif command -v caffeinate >/dev/null 2>&1; then
        caffeinate -i -s -w "$$" >/dev/null 2>&1 &
    else
        warn "no sleep inhibitor on this machine (systemd-inhibit, caffeinate): keep it awake for the whole arm, or the arm is stopped when the GPU sampler stalls"
        return 0
    fi
    RUN_INHIBIT=$!
    sleep 1
    if ! kill -0 "$RUN_INHIBIT" 2>/dev/null; then
        warn "the sleep inhibitor exited at once (no logind session?): keep this machine awake for the whole arm"
        RUN_INHIBIT=""
    fi
}

# WHICH REPLICAS ACTUALLY DID THE WORK.
#
# Nothing measured this, and it invalidated three runs. Measured on a multi-node H200 cluster:
# across a whole arm ONE Qwen replica did 6,000,692 prompt tokens and every
# other replica the autoscaler added did exactly ZERO -- including a warm-pool
# Pod that was awake, Ready, in the EndpointSlice and in the EPP's own backend
# list. Adding capacity cannot improve TTFT when none of it is used, so every
# number the run produced was about a one-replica fleet.
#
# Read from each engine's OWN cumulative counter, before and after, so the delta
# is the work done during this arm. `vllm:prompt_tokens_total` rather than a
# request count: it is monotonic, cheap, and present on every engine here.
capture_pod_work() {
    local out="$1" pod port
    : > "$out"
    for pod in $(k get pods -o json 2>/dev/null | jq -r '
        .items[] | select(.status.phase=="Running")
        | select((.metadata.name|test("decode|prefill")) or (.metadata.labels["llm-d.ai/warm-pool"] != null))
        | .metadata.name'); do
        # A decode engine serves on the port the chart gave it; a pool Pod's warm
        # engine listens on the supervisor-assigned port. 8200 covers the former,
        # 9001 the first of the latter, and 8000 a P/D prefill engine, which has
        # no routing sidecar in front of it -- all are read, and whichever
        # answers first is the engine.
        for port in 8200 8000 9001; do
            k exec "$pod" -- python3 -c "
import re,sys,urllib.request
try:
    m=urllib.request.urlopen('http://127.0.0.1:$port/metrics',timeout=10).read().decode()
except Exception:
    sys.exit(1)
tot=0.0
for l in m.splitlines():
    if l.startswith('vllm:prompt_tokens_total'):
        try: tot+=float(l.rsplit(' ',1)[1])
        except Exception: pass
print('%s %.0f' % ('$pod', tot))
" 2>/dev/null >> "$out" && break
            # An engine image with no python3 (the simulator's) is read
            # through the API server's pod proxy instead.
            k get --raw "/api/v1/namespaces/$NS/pods/$pod:$port/proxy/metrics" --request-timeout=10s 2>/dev/null \
                | awk -v pod="$pod" '/^vllm:prompt_tokens_total/ { t += $NF; seen = 1 }
                    END { if (seen) printf "%s %.0f\n", pod, t; else exit 1 }' >> "$out" && break
        done
    done
    [ -s "$out" ] || warn "could not read per-engine work from any Pod; the distribution guard will not run"
}

# Fold one capture into the running record, keeping the HIGHER reading per
# Pod. The counter is monotonic, so a lower reading is a stale or partial
# read, never new information -- and a Pod absent from the new capture keeps
# what it had, which is how a replica scaled away mid-arm stays in evidence.
merge_pod_work() {
    local into="$1" new="$2"
    [ -s "$new" ] || return 0
    python3 - "$into" "$new" <<'PY'
import os, sys
into, new = sys.argv[1], sys.argv[2]
best = {}
for path in (into, new):
    if not os.path.exists(path):
        continue
    for line in open(path):
        parts = line.split()
        if len(parts) != 2:
            continue
        try:
            v = float(parts[1])
        except ValueError:
            continue
        best[parts[0]] = max(best.get(parts[0], 0.0), v)
tmp = into + ".tmp"
with open(tmp, "w") as fh:
    for pod in sorted(best):
        fh.write("%s %.0f\n" % (pod, best[pod]))
os.replace(tmp, into)
PY
}

RUN_JOB=""
RUN_SAMPLER=""
RUN_WORKER=""
run_cleanup() {
    # The JOB is the leak that matters: left behind it drives 12 rps at the
    # models' ceiling for the rest of the schedule, on a shared cluster, with
    # nobody watching. The sampler is a polling loop against the API server.
    [ -n "$RUN_SAMPLER" ] && kill "$RUN_SAMPLER" 2>/dev/null
    [ -n "$RUN_INHIBIT" ] && kill "$RUN_INHIBIT" 2>/dev/null
    [ -n "$RUN_WORKER" ] && kill "$RUN_WORKER" 2>/dev/null
    if [ -n "$RUN_JOB" ]; then
        kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} -n "$NS" \
            delete job "$RUN_JOB" --ignore-not-found >/dev/null 2>&1
    fi
}
run_interrupted() {
    warn "interrupted -- deleting the load Job and stopping the sampler"
    run_cleanup
    exit 130
}

verb_run() {
    need_ns
    local arm="${1:-}"
    case "$arm" in
        pool|nopool|floor|today|shadow|share) ;;
        *) die "run takes an arm: nopool, pool or floor; or today, shadow or share for the utilization-share comparison (got '${arm:-<none>}')" ;;
    esac
    local bad
    bad="$(check_sampler_settings)" || die "$bad"
    # The controller log is cut from here: what the optimizer did in THIS arm,
    # policy rewrite and preload included, and nothing of the arm before.
    local run_started
    run_started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    local have_pool=0
    k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1 && have_pool=1
    if [ "$arm" = "pool" ] && [ "$have_pool" -eq 0 ]; then
        die "arm 'pool' but no pool named $POOL_NAME exists. Run pool-create first."
    fi
    if [ "$arm" != "pool" ] && [ "$have_pool" -eq 1 ]; then
        die "arm '$arm' but pool $POOL_NAME is present and holding accelerators. Run pool-delete first."
    fi
    # The floor arm's insurance is replicas that never go away. Set on the
    # ScaledObject, not by scaling the Deployment: the controller recommends 1
    # at the low rate and KEDA would take the fleet straight back down. The
    # next `reset` puts minReplicaCount back, so a floor never leaks into the
    # arm that follows.
    local floor="$MIN_REPLICAS"
    if [ "$arm" = "floor" ]; then
        floor="$FLOOR_REPLICAS"
        set_fleet_floor "$floor"
        wait_fleet_at "$floor" || die "the fleet did not reach the floor of $floor per model"
    fi
    # The utilization-share arms differ only in the policy's optimizer block.
    # Set it, then wait for the controller to SAY it runs that mode: an arm
    # whose policy was never read measures the previous arm under a new name.
    if is_share_arm "$arm"; then
        check_node_data || die "arm $arm refused: the share arms of this run need node data (above). NODE_DATA=off runs without it, and then measures the small model never funding the large one."
        local sel_bad
        sel_bad="$(node_selector_problem)"
        [ -z "$sel_bad" ] || die "arm $arm refused: $sel_bad"
        set_share_policy "$arm"
        wait_share_mode "$(share_arm_mode "$arm")" || \
            die "the controller is not running the optimizer mode arm $arm needs, so this arm would measure the wrong thing"
    fi
    # A pool arm on a COLD pool is the worst result this scenario can produce:
    # it runs to completion, produces a full table, and reports the cost of a
    # pool nobody would operate that way. Re-checked here rather than trusted
    # from an earlier `warm`, because the pool can lose a copy in between.
    if [ "$arm" = "pool" ] && [ "${SKIP_WARM_GATE:-0}" != "1" ]; then
        wait_resident "${WARM_GATE_TIMEOUT:-120}" || \
            die "the pool is not resident in both models, so this arm would measure a cold pool. Run 'warm' first, or set SKIP_WARM_GATE=1 if a cold pool is deliberately what you are measuring."
    fi

    local hostport
    hostport="$(gateway_host)" || die "did not find exactly one inference gateway Service in $NS"
    local url_a url_b
    url_a="$(base_url_for_stack "$hostport" "$STACK_A")"
    url_b="$(base_url_for_stack "$hostport" "$STACK_B")"
    # Every model's base URL as URL_<ROLE>, which extra_role_flags and the
    # load Job read for models C and D.
    local URL_A="$url_a" URL_B="$url_b" URL_C="" URL_D="" er
    for er in $EXTRA_ROLES; do
        printf -v "URL_$(upper "$er")" '%s' "$(base_url_for_stack "$hostport" "$(rvar STACK "$er")")"
    done

    local ceiling; ceiling="$(set_arm_ceiling "$arm")"

    local out_dir
    out_dir="$(arm_dir "$arm")"
    mkdir -p "$out_dir"
    # bench_nodes: the nodes the replicas could land on ("" when unrestricted
    # or pinned to BENCH_NODE). Two arms on different node sets had different
    # fragmentation to fund replicas from, and the report refuses them.
    printf '{"arm":"%s","max_replicas_per_model":%s,"min_replicas_per_model":%s,"pool_replicas":%s,"gpus_per_replica":%s,"max_prefill":%s,"max_decode":%s,"min_prefill":%s,"min_decode":%s,"max_replicas_a":%s,"max_replicas_b":%s,"gpus_per_replica_a":%s,"gpus_per_replica_b":%s,"bench_nodes":"%s"}\n' \
        "$arm" "$ceiling" "$floor" "$([ "$arm" = pool ] && echo "$POOL_REPLICAS" || echo 0)" "$GPUS_PER_REPLICA" \
        "$MAX_PREFILL" "$MAX_DECODE" "$MIN_PREFILL" "$MIN_DECODE" \
        "$MAX_REPLICAS_A" "$MAX_REPLICAS_B" "$(model_gpus a)" "$(model_gpus b)" \
        "${BENCH_NODE:-$(bench_nodes_csv)}" \
        > "$out_dir/budget.json"
    # Models C and D's own ceilings and sizes, and every model's Deployment
    # prefix (model_deploys), which is how the report splits the accelerators
    # it sampled by model. Added to the line above rather than written into it,
    # so a two-model budget keeps every field it always had.
    add_budget_fields "$out_dir/budget.json" || warn "could not record the per-model budget fields in $out_dir/budget.json"
    # The policy this arm ran under, kept with its results: the report refuses
    # two share arms whose limiters differ.
    if is_share_arm "$arm"; then
        kc -n "$WVA_NS" get configmap wva-scaling-policy-config -o jsonpath='{.data.default}' \
            > "$out_dir/policy.yaml" 2>/dev/null || warn "could not keep the policy this arm ran under"
    fi

    # The two inference-perf profiles and the phase table they were built from,
    # rendered here and kept with the results. The report refuses two arms whose
    # schedules differ, and it can only do that if the schedule the load
    # actually ran is the one written down.
    profile_args() {
        printf '%s\n' \
            --model-a "$MODEL_A" --model-b "$MODEL_B" \
            --endpoint-a "$url_a" --endpoint-b "$url_b" \
            --phase-seconds "$PHASE_SECONDS" --cycles "$CYCLES" --lead-in "$LEAD_IN" \
            --low-rps "$LOW_RPS" --high-rps "$HIGH_RPS" --high-rps-b "$HIGH_RPS_B" \
            --input-tokens "$INPUT_TOKENS" --output-tokens "$OUTPUT_TOKENS" \
            --input-tokens-b "$INPUT_TOKENS_B" --output-tokens-b "$OUTPUT_TOKENS_B" \
            --data "$LOAD_DATA" --prefix-groups "$PREFIX_GROUPS" \
            --request-timeout "$REQUEST_TIMEOUT" \
            --rise-window "$RISE_WINDOW" --overlap "$OVERLAP_SECONDS" \
            --seed "$SEED" --results-root /results
        # Nothing at all for two models; --models N and each further model's
        # id, endpoint, burst rate and low rate for three or four.
        [ -z "$EXTRA_ROLES" ] || printf '%s\n' --models "$MODEL_COUNT"
        extra_role_flags model endpoint high-rps low-rps
    }
    local pyargs; pyargs="$(profile_args)"
    # shellcheck disable=SC2046 # deliberate word splitting of the rendered flags
    python3 "$HERE/two_model_profile.py" --emit schedule $pyargs > "$out_dir/schedule.json" \
        || die "could not render the phase schedule"
    local role
    for role in $ROLES; do
        python3 "$HERE/two_model_profile.py" --emit profile --role "$role" $pyargs \
            > "$out_dir/profile-$role.yaml" || die "could not render profile $role"
        [ -s "$out_dir/profile-$role.yaml" ] || die "profile $role rendered empty"
    done
    # The arm's length is READ FROM THE SCHEDULE, not re-derived here. It used
    # to be LEAD_IN + 2 x CYCLES x PHASE_SECONDS, which forgets the both-low
    # bands between bursts: 2040s for a ladder that runs 2310s. Everything
    # timed off it was 270s early -- the per-engine capture fired while the
    # last phase still had four minutes to run, and read 0 from a replica that
    # was Running but not yet Ready and went on to serve 1.39M tokens, so the
    # routing guard refused a valid arm. The GPU sampler cleared the real end
    # by 30s of slack, by luck.
    local total
    total="$(python3 -c "import json,sys; print(int(max(s['end'] for s in json.load(open(sys.argv[1])))))" "$out_dir/schedule.json")" \
        || die "could not read the arm's length from $out_dir/schedule.json"
    info "arm=$arm  ${total}s of load"

    # Profiles go in as a ConfigMap rather than baked into an image: an edit
    # would otherwise need a build and a registry push between it and a run.
    k delete configmap wva-two-model-load --ignore-not-found >/dev/null 2>&1
    local extra_profiles=()
    for role in $EXTRA_ROLES; do
        extra_profiles+=(--from-file=profile-$role.yaml="$out_dir/profile-$role.yaml")
    done
    k create configmap wva-two-model-load \
        --from-file=profile-a.yaml="$out_dir/profile-a.yaml" \
        --from-file=profile-b.yaml="$out_dir/profile-b.yaml" \
        ${extra_profiles[@]+"${extra_profiles[@]}"} \
        --from-file=run.sh="$HERE/two_model_harness_run.sh" >/dev/null \
        || die "could not create the loader ConfigMap"

    # Both containers start their ladders at this instant. It has to cover the
    # image pull and two tokenizer downloads; a container that reaches the
    # barrier late says so, and the arm is refused rather than reported.
    local start_at=$(( $(date +%s) + PRELOAD_GRACE ))

    RUN_JOB="wva-two-model-load-$arm"
    # EXIT and HUP as well as INT/TERM, because the leak this prevents is not
    # hypothetical: an interrupted arm left its Job driving both models at their
    # ceiling for 37 minutes on a shared cluster, unwatched. Every one of `die`,
    # a closed terminal and a Ctrl-C has to take the Job with it. The handler is
    # a no-op once RUN_JOB is cleared on the success path.
    trap run_interrupted INT TERM HUP
    trap run_cleanup EXIT
    k delete job "$RUN_JOB" --ignore-not-found >/dev/null 2>&1
    render_load_job "$RUN_JOB" "$arm" "$start_at" "$url_a" "$url_b" > "$out_dir/job.yaml"
    [ -s "$out_dir/job.yaml" ] || die "the load Job rendered empty"
    k apply -f "$out_dir/job.yaml" >/dev/null || die "could not create the load Job"

    # The per-engine baseline, BEFORE any load, so the delta is this arm's work.
    capture_pod_work "$out_dir/podwork.before"

    # The sampler has to cover the preload grace as well as the load: the pool
    # is holding accelerators from the moment the arm starts, and a window that
    # begins at the barrier would credit it with fewer than it held.
    local until_ts=$(( $(date +%s) + PRELOAD_GRACE + total + 300 ))
    hold_awake
    sample_gpus "$out_dir/gpus.jsonl" "$until_ts" &
    RUN_SAMPLER=$!

    # The per-engine capture has to happen WHILE THE ADDED REPLICAS STILL EXIST.
    #
    # It used to run after collection, and collection waits for inference-perf
    # to write its report -- which took FOUR MINUTES of Hugging Face calls after
    # the last request. By then KEDA had scaled the fleet back and the two
    # replicas the run added were gone, so the capture showed only the two that
    # had been there all along: the routing guard's evidence, deleted before it
    # was read. Measured exactly that on a 9 rps arm that demonstrably scaled
    # one model to three replicas.
    #
    # So it is timed off the schedule instead. The load ends at start_at+total
    # whatever the report generator does afterwards.
    #
    # And it is PERIODIC, keeping each engine's HIGHEST reading. One capture at
    # the end, however well timed, only sees the replicas alive at that instant:
    # the two a model grew for its first rise are scaled away long before its
    # second, and their work vanished with them -- the nopool arm's Llama total
    # was 12.69M tokens and the end capture accounted for 7.79M. The counter is
    # monotonic, so a Pod's last reading before it is deleted IS its work for
    # the arm, and the merge below can never make a Pod's number shrink.
    local work_at=$(( start_at + total + 30 ))
    ( while :; do
          capture_pod_work "$out_dir/podwork.sample" 2>/dev/null
          merge_pod_work "$out_dir/podwork.after" "$out_dir/podwork.sample"
          [ "$(date +%s)" -lt "$work_at" ] || break
          sleep 60
      done
      rm -f "$out_dir/podwork.sample" ) &
    RUN_WORKER=$!

    # Waited on by the loader's OWN lines, not by the Job's completion, and on
    # BOTH of them: one container finishing means half a run. The Pod stays
    # alive afterwards on purpose -- `kubectl cp` is exec+tar and cannot run in
    # a terminated container, so waiting for Completed and then copying loses
    # the results of every successful run.
    local budget=$(( PRELOAD_GRACE + total + RESULT_GRACE ))
    info "waiting for both loaders to report their results (up to ${budget}s)..."
    local deadline=$(( $(date +%s) + budget )) pod="" done=0 log="" stalled=""
    while [ "$(date +%s)" -lt "$deadline" ]; do
        # The sampler stalled: this arm's cost can no longer be priced and the
        # report would refuse it, so the GPUs it holds on a shared cluster are
        # given back now rather than at the end of the schedule.
        if stalled="$(sampler_stalled "$out_dir/gpus.jsonl")"; then
            break
        fi
        # THIS run's Pod, by its start barrier. The Job's name repeats -- every
        # repeat of an arm has the same one -- and a deleted Job's Pod lives
        # on: it holds its results open for an hour. Selected by Job name
        # alone, repeat 2 of an arm found repeat 1's Pod 20 s after starting,
        # read its RESULTS-WRITTEN lines and copied its results as its own.
        pod="$(k get pods -l "job-name=$RUN_JOB,wva-two-model-run=$start_at" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
        if [ -n "$pod" ]; then
            # Bounded. A log stream that stalls on the API server otherwise
            # holds this loop open past its budget: MEASURED, the loop's
            # budget ended at 18:59 and it exited at 19:26, without ever
            # having seen the two markers that were in the log the whole
            # time. The Pod holds its results for an hour after the run and
            # they were still there when the loop gave up.
            log="$(k logs "$pod" --all-containers --request-timeout=60s 2>/dev/null)"
            if printf '%s' "$log" | all_results_written; then done=1; break; fi
            if printf '%s' "$log" | grep -q 'PRELOAD-FAILED\|RUN-FAILED'; then
                warn "a loader container reported a failure; stopping the wait"
                break
            fi
            # A loader that died in its first second must not cost the full
            # schedule before anyone is told.
            case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
                Failed) warn "the loader Pod failed"; break ;;
            esac
            # A container the KERNEL killed prints nothing, and the Pod stays
            # Running while its sibling lives. MEASURED: the Llama loader was
            # OOMKilled at the start of its fourth rise and this loop carried on
            # for eleven minutes, driving Qwen alone against a fleet that was
            # no longer in anti-phase, until the sibling finished on its own.
            # The wrapper cannot report what it never got to see, so the
            # container state is read directly.
            local dead
            dead="$(k get pod "$pod" -o json 2>/dev/null | jq -r '
                [.status.containerStatuses[]? | select(.state.terminated != null)
                 | .name + "=" + (.state.terminated.reason // "Terminated")] | join(" ")')"
            if [ -n "$dead" ]; then
                warn "a loader container terminated without reporting: $dead"
                break
            fi
        fi
        sleep 15
    done
    kill "$RUN_SAMPLER" 2>/dev/null; RUN_SAMPLER=""
    if [ -n "$stalled" ]; then
        die "arm $arm stopped: the GPU sampler did not run for ${stalled}s (the machine running this driver was suspended, or a call blocked), so the arm's GPU cost cannot be priced and the report would refuse it. Keep this machine awake (docs/guides/benchmarking/two-model-utilization-share.md) and rerun arm $arm."
    fi

    if [ -n "$pod" ]; then
        k logs "$pod" --all-containers --prefix --request-timeout=120s > "$out_dir/loader.log" 2>&1 || true
    fi
    # THE FULL LOG IS THE AUTHORITY, not the loop. If the loop gave up but the
    # log it just fetched carries both markers, the results exist and the Pod
    # is still holding them: copy them. An arm of 24,420 requests was thrown
    # away with both RESULTS-WRITTEN lines sitting in loader.log because this
    # decision was made from the loop's last (stalled) read instead.
    if [ "$done" -eq 0 ] && [ -s "$out_dir/loader.log" ] &&
       all_results_written < "$out_dir/loader.log"; then
        warn "the wait loop gave up, but the loader log shows $([ "$MODEL_COUNT" = 2 ] && echo both || echo "all $MODEL_COUNT") results written; collecting them"
        done=1
    fi
    # The pool Pods' own logs, taken NOW: pool-delete follows this arm, and
    # with it goes the only record of what the proxy and supervisor did at
    # each lend and return. Seven 503s in one rise were traced to the pool Pod
    # at the moment of the lend and no further, because this file did not
    # exist.
    if [ "$arm" = "pool" ]; then
        : > "$out_dir/pool-pods.log"
        for pp in $(pool_pods); do
            k logs "$pp" --all-containers --prefix >> "$out_dir/pool-pods.log" 2>&1 || true
        done
        k logs -n "$WVA_NS" deploy/wva-controller-manager --since="$(( total + PRELOAD_GRACE + 300 ))s" \
            > "$out_dir/wva.log" 2>&1 || true
    fi
    # A container that reached the barrier after it had passed ran its ladder
    # offset from the other one's for the whole run. That is not anti-phase, and
    # a report over it compares a burst against whatever the other model
    # happened to be doing.
    if [ -s "$out_dir/loader.log" ] && grep -q 'LATE-START' "$out_dir/loader.log"; then
        warn "$(grep -o 'LATE-START-[abcd] by [0-9.]*s' "$out_dir/loader.log" | sort -u | tr '\n' ' ')"
        die "a loader missed the start barrier, so the $(models_word) models were not in $([ "$MODEL_COUNT" = 2 ] && echo anti-phase || echo rotation). Raise PRELOAD_GRACE (now ${PRELOAD_GRACE}s) and rerun arm $arm."
    fi
    if [ "$done" -eq 1 ]; then
        # One emptyDir shared by both containers, so one copy takes both.
        rm -rf "$out_dir/harness"
        k cp "$pod:/results" "$out_dir/harness" -c load-a >/dev/null 2>&1 || \
            warn "could not copy the harness results out of $pod"
    elif [ -n "$pod" ]; then
        # A loader that never wrote its results still leaves evidence, and the
        # cleanup below deletes the Pod that holds it. Kept, not reported: the
        # arm is still refused below. A decode-heavy loader once went silent
        # after its load ended and the only copy went with the Pod.
        warn "a loader never wrote its results; keeping what $pod holds in $out_dir/harness-partial"
        rm -rf "$out_dir/harness-partial"
        k cp "$pod:/results" "$out_dir/harness-partial" -c load-a >/dev/null 2>&1 || \
            warn "could not copy the partial results out of $pod"
        k get pod "$pod" -o json > "$out_dir/loader-pod.json" 2>/dev/null || true
        k exec "$pod" -c load-b -- sh -c 'ps aux 2>/dev/null || ls /proc' > "$out_dir/loader-b-ps.txt" 2>&1 || true
    fi
    trap - INT TERM HUP
    run_cleanup
    RUN_JOB=""
    if is_share_arm "$arm"; then
        capture_share_metrics "$out_dir/share.json" "$(( start_at + total ))" "$total"
        capture_share_log "$out_dir" "$run_started"
    fi
    # The timed capture above is the one that matters. This only fills in when
    # it did not run at all -- it never overwrites, because a late capture is
    # precisely the one that has lost the added replicas.
    wait "$RUN_WORKER" 2>/dev/null || true
    RUN_WORKER=""
    [ -s "$out_dir/podwork.after" ] || capture_pod_work "$out_dir/podwork.after"

    [ -d "$out_dir/harness/a" ] && [ -d "$out_dir/harness/b" ] || \
        die "no harness results for arm $arm. See $out_dir/loader.log"
    # Models C and D: their results and ids, which harness_results.py refuses
    # to do without when the schedule drives them.
    local extra_results=()
    for role in $EXTRA_ROLES; do
        [ -d "$out_dir/harness/$role" ] || die "no harness results for model $(upper "$role") of arm $arm. See $out_dir/loader.log"
        extra_results+=(--results-"$role" "$out_dir/harness/$role" --model-"$role" "$(rvar MODEL "$role")")
    done
    python3 "$HERE/harness_results.py" \
        --results-a "$out_dir/harness/a" --results-b "$out_dir/harness/b" \
        ${extra_results[@]+"${extra_results[@]}"} \
        --schedule "$out_dir/schedule.json" \
        --out "$out_dir/requests.jsonl" \
        --t0 "$start_at" --overlap "$OVERLAP_SECONDS" \
        --arm "$arm" --model-a "$MODEL_A" --model-b "$MODEL_B" \
        --input-tokens "$INPUT_TOKENS" --output-tokens "$OUTPUT_TOKENS" \
        --input-tokens-b "$INPUT_TOKENS_B" --output-tokens-b "$OUTPUT_TOKENS_B" \
        --seed "$SEED" --data "$LOAD_DATA" --prefix-groups "$PREFIX_GROUPS" \
        || die "could not convert the harness results for arm $arm. See $out_dir/loader.log"
    mv "$out_dir/requests.jsonl.meta.json" "$out_dir/meta.json"
    # Where every pod of the arm landed, from the sampler's own record. On
    # several nodes this is part of the result: a replica of N GPUs needs N
    # free on one node, so the placement decides what could be funded.
    if python3 "$HERE/two_model_share_report.py" --placement "$out_dir/gpus.jsonl" > "$out_dir/placement.json.tmp"; then
        mv "$out_dir/placement.json.tmp" "$out_dir/placement.json"
        info "arm $arm placement: $(python3 "$HERE/two_model_share_report.py" --placement-summary "$out_dir/placement.json")"
    else
        rm -f "$out_dir/placement.json.tmp"
        warn "could not derive where arm $arm's pods landed from $out_dir/gpus.jsonl"
    fi
    # From the meta, not from requests.jsonl: that file is empty by design now,
    # and "0 requests" on a healthy arm reads as a run that served nothing.
    ok "arm $arm: $(python3 -c "import json;m=json.load(open('$out_dir/meta.json'));print('%d issued, %d served' % (m['issued'], sum(w['overall']['n'] for w in m['windows'].values())))" 2>/dev/null || echo "results written"), $(wc -l < "$out_dir/gpus.jsonl" 2>/dev/null || echo 0) GPU samples in $out_dir"
}

render_load_job() {
    local job="$1" arm="$2" start_at="$3" url_a="${4:-}" url_b="${5:-}"
    # (One container per model: three or four with MODEL_COUNT.)
    # TWO CONTAINERS, ONE POD, on purpose. The models have to burst against each
    # other, so their load must come from one scheduling unit on one node with
    # one image pull: two Pods start whenever the scheduler gets to each of
    # them, and the skew between those two moments is the anti-phase this
    # scenario exists to measure.
    #
    # The results emptyDir is shared, so one `kubectl cp` takes both models'
    # reports. The HF cache is a SEPARATE volume, so a tokenizer download never
    # lands inside what is copied out as results.
    cat <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: $job
  namespace: $NS
  labels:
    app.kubernetes.io/name: wva-two-model-load
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/name: wva-two-model-load
        wva-two-model-run: "$start_at"
    spec:
      restartPolicy: Never
      # ndots:1, against the cluster default of 5. Belt and braces beside the
      # ClusterIP base_url: anything else this Pod resolves -- the tokenizer
      # download, most of all -- otherwise costs one lookup per search domain
      # before the absolute one.
      dnsConfig:
        options:
          - name: ndots
            value: "1"
      containers:
$(render_load_container a "$MODEL_A" "$start_at" "$url_a")
$(render_load_container b "$MODEL_B" "$start_at" "$url_b")$(for r in $EXTRA_ROLES; do echo; render_load_container "$r" "$(rvar MODEL "$r")" "$start_at" "$(rvar URL "$r")"; done)

      volumes:
        - name: profiles
          configMap:
            name: wva-two-model-load
        - name: results
          emptyDir: {}
$(render_hf_volume)
YAML
}

render_hf_volume() {
    # The tokenizer cache, on the SHARED model claim when there is one.
    #
    # Each arm otherwise fetches both tokenizers from the public internet, and a
    # blip there costs the whole arm: measured, a `CAS Client Error: Request
    # middleware error` from the Hugging Face CDN took model A down while model
    # B fetched fine, and the run was refused after five minutes of preload
    # grace with both models' accelerators already held. The retry in the
    # wrapper covers a blip; this removes the fetch entirely from every arm
    # after the first.
    #
    # subPath, so this never writes into the weights tree the models read. The
    # files are a few MB -- this is not a meaningful claim on a 100Gi cache.
    if [ -n "$CACHE_CLAIM" ] && k get pvc "$CACHE_CLAIM" >/dev/null 2>&1; then
        cat <<YAML
        - name: hf
          persistentVolumeClaim:
            claimName: $CACHE_CLAIM
YAML
    else
        # No claim: an emptyDir still works, it just refetches each arm.
        cat <<YAML
        - name: hf
          emptyDir: {}
YAML
    fi
}

render_load_container() {
    local role="$1" tokenizer="$2" start_at="$3" base_url="${4:-}"
    # CPU matters more than it looks: both containers share one Pod, and an
    # open-loop generator that cannot get on a core delays its own arrivals and
    # then reports the delay as the cluster's latency. The report refuses an arm
    # whose driver queueing is large, so starving this is a wasted run, not a
    # quiet bias.
    #
    # MEMORY GROWS FOR THE WHOLE RUN with the synthetic dataset. inference-perf
    # materializes each prompt lazily in one of its worker processes (18 at 9
    # rps) and keeps every one it has made, so the footprint is the corpus and
    # tokenizer per worker PLUS every prompt issued so far. MEASURED: at 9 rps
    # the Llama loader was OOMKilled at 8Gi thirty-three minutes in, at the
    # start of its fourth rise, with ~8,500 prompts materialized; the Qwen
    # loader at 8 rps finished. That is the worst failure this scenario has: a
    # full arm's accelerators spent and no result. The limit below is the
    # measured death point times four, and the request stays modest because
    # the growth is late.
    cat <<YAML
        - name: load-$role
          image: $(load_image)
          command: ["/bin/sh", "/profiles/run.sh"]
          env:
            - name: ROLE
              value: "$role"
            - name: TOKENIZER
              value: "$tokenizer"
            - name: MODEL
              value: "$tokenizer"
            - name: BASE_URL
              value: "$base_url"
            - name: START_AT
              value: "$start_at"
            - name: HF_HOME
              value: /hf/$role
          resources:
            requests:
              cpu: "$LOAD_CPU"
              memory: "$LOAD_MEMORY"
            limits:
              cpu: "$LOAD_CPU_LIMIT"
              memory: "$LOAD_MEMORY_LIMIT"
          volumeMounts:
            - name: profiles
              mountPath: /profiles
            - name: results
              mountPath: /results
            - name: hf
              mountPath: /hf
              subPath: benchmark-tokenizer-cache
YAML
}

# ---------------------------------------------------------------------------
# report / status / teardown
# ---------------------------------------------------------------------------
verb_report() {
    local a="$OUT_ROOT/nopool" b="$OUT_ROOT/pool"
    # meta.json, NOT requests.jsonl. Every number the report prints lives in the
    # meta now that per-request reporting is off, and requests.jsonl is
    # legitimately EMPTY -- so gating on it refused two complete arms, 90
    # minutes of accelerators, after both had already run.
    local f="$OUT_ROOT/floor"
    [ -s "$a/meta.json" ] || die "no nopool results in $a -- the nopool arm is the baseline every other arm is compared against"
    local extra=()
    [ -s "$b/meta.json" ] && extra+=(--pool "$b")
    [ -s "$f/meta.json" ] && extra+=(--floor "$f")
    # Any other arm someone ran into this OUT_ROOT (a `run pool` under a
    # different POOL_REPLICAS, copied in as pool1/, say) goes in by name.
    local d name
    for d in "$OUT_ROOT"/*/; do
        name="$(basename "$d")"
        # The utilization-share arms have their own report: report-share.
        case "$name" in nopool|pool|floor|today|shadow|share) continue ;; esac
        [ -s "$d/meta.json" ] && extra+=(--arm "$name=${d%/}")
    done
    [ "${#extra[@]}" -gt 0 ] || die "only the nopool arm has results in $OUT_ROOT; run the pool and/or floor arm first"
    # Written beside the results, so the tables can be regenerated and plotted
    # without the cluster.
    python3 "$HERE/two_model_report.py" \
        --nopool "$a" "${extra[@]}" \
        --model-a "$MODEL_A" --model-b "$MODEL_B" \
        ${MODEL_FLAGS[@]+"${MODEL_FLAGS[@]}"} \
        --json "$OUT_ROOT/report.json" \
        | tee "$OUT_ROOT/report.md"
    [ "${PIPESTATUS[0]}" -eq 0 ] || die "the report failed"
    python3 "$HERE/two_model_plots.py" --json "$OUT_ROOT/report.json" --out "$OUT_ROOT" \
        && ok "report.md, report.json and the SVG plots are in $OUT_ROOT" \
        || warn "the plots failed; the report itself is in $OUT_ROOT/report.md"
}

# The utilization-share comparison: today is the baseline, shadow and share are
# judged against it on the same TTFT and accelerator-second tables as the pool
# arms, and then on what the optimizer itself recorded.
verb_report_share() {
    local root; root="$(share_root)"
    local a="$root/today"
    [ -s "$a/meta.json" ] || die "no today results in $a -- the today arm is the baseline the share arms are compared against"
    local extra=() n
    for n in shadow share; do
        [ -s "$root/$n/meta.json" ] && extra+=(--arm "$n=$root/$n")
    done
    [ "${#extra[@]}" -gt 0 ] || die "only the today arm has results in $root; run the shadow and/or share arm first"
    {
        python3 "$HERE/two_model_report.py" \
            --nopool "$a" "${extra[@]}" --baseline-name today \
            --model-a "$MODEL_A" --model-b "$MODEL_B" \
            ${MODEL_FLAGS[@]+"${MODEL_FLAGS[@]}"} \
            ${REPORT_ARGS:-} \
            --json "$root/report.json" || exit 1
        echo
        python3 "$HERE/two_model_share_report.py" --out-root "$root" || exit 1
    } | tee "$root/report.md"
    [ "${PIPESTATUS[0]}" -eq 0 ] || die "the report failed"
    python3 "$HERE/two_model_plots.py" --json "$root/report.json" --out "$root" \
        && ok "report.md, report.json and the SVG plots are in $root" \
        || warn "the plots failed; the report itself is in $root/report.md"
}

# The repeats side by side: per-burst TTFT by model and arm, the spread across
# repeats, transfers by outcome from each arm's controller log, refused
# scale-ups and GPU-seconds, and a plain summary of what the run shows.
verb_report_repeats() {
    python3 "$HERE/two_model_share_repeats.py" --out-root "$OUT_ROOT" \
        --model-a "$MODEL_A" --model-b "$MODEL_B" \
        ${MODEL_FLAGS[@]+"${MODEL_FLAGS[@]}"} \
        --json "$OUT_ROOT/repeats.json" > "$OUT_ROOT/repeats.md.tmp" \
        || { cat "$OUT_ROOT/repeats.md.tmp" >&2; rm -f "$OUT_ROOT/repeats.md.tmp"; die "the repeats report failed"; }
    mv "$OUT_ROOT/repeats.md.tmp" "$OUT_ROOT/repeats.md"
    cat "$OUT_ROOT/repeats.md"
    ok "repeats.md and repeats.json are in $OUT_ROOT"
}

# ---------------------------------------------------------------------------
# share-arms: every arm of every repeat, in one command
# ---------------------------------------------------------------------------
# REPEATS (2) x the arms in SHARE_ARMS (today shadow share), each after a
# reset, each in its own process so its own traps stay its own. Odd repeats
# run the arms in the order given and even repeats in reverse, so a drift over
# the hours of a run does not land on one arm. A report per repeat, then
# report-repeats. Stops at the first arm that fails.
#
# TEARDOWN_ON_EXIT=1 tears the stacks down on EVERY exit path -- done, failed,
# interrupted, the terminal closed -- and then prints what still holds an
# accelerator in the namespace. On a shared cluster that is the point: a run
# that dies at 3 a.m. must not hold 7 GPUs until somebody looks.
SHARE_ARMS_RC=""
share_arms_finish() {
    local rc="${SHARE_ARMS_RC:-$?}"
    trap - EXIT INT TERM HUP
    info "share-arms finishing (rc=$rc) at $(date -u +%H:%M:%SZ)"
    if [ "${TEARDOWN_ON_EXIT:-0}" = "1" ]; then
        bash "$HERE/two_model_pool.sh" teardown || warn "teardown reported a problem"
    fi
    # Read until it is 0 after a teardown: pods still in their grace period
    # hold their GPUs, and a count taken at once reports them as a leak.
    local held waited=0
    while :; do
        held="$(k get pods -o json 2>/dev/null | jq -r '[.items[] | select(.status.phase=="Running" or .status.phase=="Pending")
            | ([.spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add)] | add // 0' 2>/dev/null)"
        [ "${TEARDOWN_ON_EXIT:-0}" = "1" ] && [ "${held:-0}" != 0 ] && [ "$waited" -lt "${TEARDOWN_WAIT:-300}" ] || break
        sleep 10
        waited=$(( waited + 10 ))
    done
    if [ "${held:-0}" = 0 ]; then
        ok "no pod in $NS holds or waits for an accelerator"
    else
        warn "${held} accelerator(s) still held or requested in $NS$([ "${TEARDOWN_ON_EXIT:-0}" = 1 ] && echo ' after teardown: a LEAK, look now' || echo ' (TEARDOWN_ON_EXIT is off)')"
    fi
    exit "$rc"
}

verb_share_arms() {
    need_ns
    local repeats="${REPEATS:-2}" arms="${SHARE_ARMS:-today shadow share}" r arm order a
    case "$repeats" in [1-9]) ;; *) die "REPEATS=$repeats: 1 to 9" ;; esac
    for arm in $arms; do is_share_arm "$arm" || die "SHARE_ARMS may name today, shadow and share only (got '$arm')"; done
    trap share_arms_finish EXIT
    trap 'SHARE_ARMS_RC=130; exit 130' INT TERM HUP
    # Refused before the first reset, not at the first arm: a mixed-size run
    # without node data would spend an hour to find out.
    check_node_data || die "the share arms need node data first (above)"
    for r in $(seq 1 "$repeats"); do
        order="$arms"
        if [ $(( r % 2 )) -eq 0 ]; then
            order=""; for a in $arms; do order="$a${order:+ $order}"; done
        fi
        for arm in $order; do
            info "== repeat $r, arm $arm: reset at $(date -u +%H:%M:%SZ)"
            bash "$HERE/two_model_pool.sh" reset || { SHARE_ARMS_RC=1; die "reset failed before repeat $r arm $arm"; }
            info "== repeat $r, arm $arm: run at $(date -u +%H:%M:%SZ)"
            REPEAT="$r" bash "$HERE/two_model_pool.sh" run "$arm" || { SHARE_ARMS_RC=1; die "repeat $r arm $arm failed"; }
        done
        REPEAT="$r" bash "$HERE/two_model_pool.sh" report-share || warn "the report of repeat $r refused or failed; its arms stand, see $OUT_ROOT/rep$r/report.md"
    done
    verb_report_repeats || { SHARE_ARMS_RC=1; die "the repeats report failed"; }
    SHARE_ARMS_RC=0
}

verb_status() {
    need_ns
    echo "--- deployments" >&2
    k get deploy -o wide 2>/dev/null | sed 's/^/  /' >&2
    echo "--- scaledobjects" >&2
    k get scaledobject 2>/dev/null | sed 's/^/  /' >&2
    echo "--- accelerators held in $NS" >&2
    k get pods -o json 2>/dev/null | jq -r '
      [.items[]|select(.status.phase=="Running")
       |{n:.metadata.name, g:([.spec.containers[].resources.requests["nvidia.com/gpu"]//"0"|tonumber]|add)}
       |select(.g>0)] as $p
      | "  " + (([$p[].g]|add // 0)|tostring) + " GPU(s) in " + (($p|length)|tostring) + " pod(s)",
        ($p[] | "    " + .n + "  " + (.g|tostring))' 2>/dev/null >&2
}

verb_teardown() {
    need_ns
    # THE STANDUP'S SCENARIO, not a default. The harness tears down what the
    # spec it is given renders; given its default (guides/workload-autoscaling)
    # for a two-model-shapes-pd namespace it missed that run's node-local
    # downloader and hung on the volume behind it. The standup recorded its
    # spec; a named BENCH_SPEC that disagrees is refused, and with no record
    # and no BENCH_SPEC there is nothing safe to guess.
    local recorded spec
    recorded="$(k get configmap "$STANDUP_RECORD" -o jsonpath='{.data.spec}' 2>/dev/null || true)"
    case "$recorded" in
        sim:*) warn "tearing down the simulator stacks in $NS"; sim_teardown; return ;;
    esac
    if [ -n "$recorded" ]; then
        if [ -n "$BENCH_SPEC_GIVEN" ] && [ "$BENCH_SPEC" != "$recorded" ]; then
            die "BENCH_SPEC=$BENCH_SPEC, but $NS was stood up with $recorded (ConfigMap $STANDUP_RECORD). Unset BENCH_SPEC, or set it to $recorded."
        fi
        spec="$recorded"
    elif [ -n "$BENCH_SPEC_GIVEN" ]; then
        warn "no record of the scenario $NS was stood up with; tearing down with BENCH_SPEC=$BENCH_SPEC as given"
        spec="$BENCH_SPEC"
    else
        die "no record of the scenario $NS was stood up with (no ConfigMap $STANDUP_RECORD), and no BENCH_SPEC. Set BENCH_SPEC to the one the standup used: the harness only removes what that scenario renders."
    fi
    warn "tearing down everything $spec created in $NS"
    verb_pool_delete || true
    k delete job -l app.kubernetes.io/name=wva-two-model-load --ignore-not-found --timeout=120s >/dev/null 2>&1
    k delete configmap wva-two-model-load --ignore-not-found --timeout=60s >/dev/null 2>&1
    ( cd "$ROOT" && BENCHMARK_NAMESPACE="$NS" BENCHMARK_SPEC="$spec" make --no-print-directory benchmark-teardown ) || \
        warn "benchmark-teardown reported a problem; check for leftover GPU pods below"
    k delete configmap "$STANDUP_RECORD" --ignore-not-found --timeout=60s >/dev/null 2>&1
    verb_status
    ok "teardown done. Anything still holding a GPU above is a leak -- this is a shared cluster."
}

usage() { sed -n '2,/^set -u/p' "$0" | sed 's/^# \{0,1\}//; $d'; }

case "${1:-}" in
    preflight)   shift; verb_preflight "$@" ;;
    standup)     shift; verb_standup "$@" ;;
    verify)      shift; verb_verify "$@" ;;
    pool-create) shift; verb_pool_create "$@" ;;
    pool-delete) shift; verb_pool_delete "$@" ;;
    warm)        shift; verb_warm "$@" ;;
    reset)       shift; verb_reset "$@" ;;
    run)         shift; verb_run "$@" ;;
    report)      shift; verb_report "$@" ;;
    report-share) shift; verb_report_share "$@" ;;
    report-repeats) shift; verb_report_repeats "$@" ;;
    share-arms)  shift; verb_share_arms "$@" ;;
    node-data)   shift; verb_node_data "$@" ;;
    status)      shift; verb_status "$@" ;;
    teardown)    shift; verb_teardown "$@" ;;
    residency)   shift; need_ns; pool_residency ;;
    -h|--help|"") usage ;;
    *)           usage; die "unknown verb '$1'" ;;
esac

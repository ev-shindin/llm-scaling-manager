/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	goflag "flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controllerstatus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/scalefromzero"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpuusage"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kubecache"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	prometheusutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool"
	warmpoolpolicy "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	warmpoolpool "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/watchns"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	inferencePoolV1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	inferencePoolV1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(promoperator.AddToScheme(scheme))
	utilruntime.Must(inferencePoolV1.Install(scheme))
	utilruntime.Must(inferencePoolV1alpha2.Install(scheme))
	// KEDA scheme is registered unconditionally so the client can list ScaledObjects
	// when the CRD is present. Listing fails gracefully (NoMatchError) when not installed.
	utilruntime.Must(kedav1alpha1.AddToScheme(scheme))
	// Note: LeaderWorkerSet scheme is added conditionally in main() after checking if CRD exists
	// +kubebuilder:scaffold:scheme
}

// managerGracefulShutdownTimeout is how long the manager waits for runnables to
// stop. It must stay below terminationGracePeriodSeconds in
// config/base/manager/deployment.yaml (40s), which TestGracefulShutdownFitsGracePeriod
// checks.
const managerGracefulShutdownTimeout = 25 * time.Second

// startupExitCode reports a failed startup step and returns the code the
// process should exit with.
//
// Startup steps run under the signal context, so a SIGTERM during one cancels
// it and the step returns the context's error. That is logged as an info line,
// not as the step's own failure ("CRITICAL: Failed to connect to Prometheus"),
// which would read as an outage the signal did not cause.
//
// It still exits non-zero, with exitCodeTerminated. The SIGTERM is not always
// an operator's: the startup probe gives up after 60s, and a controller still
// waiting for Prometheus then gets killed. Exiting 0 there would show a
// Prometheus outage as "Completed".
//
// Both must hold: the context is done AND the step's error is that
// cancellation. A step that failed for its own reason -- a Forbidden, a
// refused connection -- just as a SIGTERM arrived is still a failure, and is
// reported as one, with exit code 1.
//
// It lives in main.go, not a file of its own: `make build` and the Dockerfile
// compile cmd/main.go alone, so a non-test helper in another file of package
// main is undefined there.
func startupExitCode(ctx context.Context, log logr.Logger, err error, msg string) int {
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		log.Info("Shutdown requested during startup; exiting", "step", msg, "error", err.Error())
		return exitCodeTerminated
	}
	log.Error(err, msg)
	return 1
}

// exitCodeTerminated is the exit code for a startup cut short by SIGTERM: 128
// plus the signal number, the status a shell reports for a process SIGTERM
// killed.
const exitCodeTerminated = 143

// optimizeLooper is an engine whose optimize loop runs until its context ends.
type optimizeLooper interface {
	StartOptimizeLoop(ctx context.Context)
}

// optimizeLoopRunnable is the manager runnable for one engine: setup builds
// the engine, and the runnable then runs its loop.
//
// The loop runs IN the runnable, not in a goroutine it spawns: it blocks until
// ctx ends, so the manager's graceful shutdown waits for an in-flight cycle
// before it releases the lease. Returning at once told the manager this
// runnable was finished while the loop was starting.
//
// A setup error is returned, never os.Exit: the manager stops on a runnable's
// error, releases the lease and returns it from mgr.Start. So is a loop that
// returns before ctx ends: returning nil would leave the manager running, and
// holding the lease, with no engine -- a leader that scales nothing while no
// other replica may take over.
func optimizeLoopRunnable(setup func(ctx context.Context) (optimizeLooper, error)) manager.RunnableFunc {
	return func(ctx context.Context) error {
		engine, err := setup(ctx)
		if err != nil {
			return err
		}
		engine.StartOptimizeLoop(ctx)
		if ctx.Err() == nil {
			return fmt.Errorf("the %T optimize loop stopped while the manager is running", engine)
		}
		return nil
	}
}

// kueueWorkloadCache runs the cluster-wide Kueue Workload cache of a
// namespace-scoped install on every replica, not only the leader: a cache
// added to the manager would otherwise start only once leadership is won.
type kueueWorkloadCache struct{ cache.Cache }

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (kueueWorkloadCache) NeedLeaderElection() bool { return false }

// registerMetricsSource adds src to reg under name.
//
// Returned, not os.Exit: this runs inside a manager runnable, and exiting there
// skipped graceful shutdown and the lease release, so the successor waited out
// the full LeaseDuration.
func registerMetricsSource(reg *source.SourceRegistry, name string, src source.MetricsSource) error {
	if err := reg.Register(name, src); err != nil {
		return fmt.Errorf("registering the %s metrics source: %w", name, err)
	}
	return nil
}

// managerOptions builds the manager's options: leader election, probes,
// metrics and webhook servers, and the shutdown budget. The cache is set by
// the caller. A function of its own so a test can check what main hands the
// manager, not only the constants it is built from.
func managerOptions(cfg *config.Config, metricsServerOptions metricsserver.Options,
	webhookServer webhook.Server) ctrl.Options {
	// Configure leader election with configurable timeouts to prevent lease renewal failures
	// Default values are: LeaseDuration=60s, RenewDeadline=50s, RetryPeriod=10s
	// These can be overridden via command-line flags in manager.yaml
	// Increased from controller-runtime defaults (15s, 10s, 2s) to provide more tolerance
	// for network latency and API server delays

	leaseDurationVal := cfg.LeaseDuration()
	gracefulShutdownTimeout := managerGracefulShutdownTimeout
	renewDeadlineVal := cfg.RenewDeadline()
	retryPeriodVal := cfg.RetryPeriod()
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.ProbeAddr(),
		LeaderElection:         cfg.EnableLeaderElection(),
		LeaderElectionID:       cfg.LeaderElectionID(),
		// Leader election timeout configuration (from Config, can be overridden via flags/env/ConfigMap)
		LeaseDuration: &leaseDurationVal,
		RenewDeadline: &renewDeadlineVal,
		RetryPeriod:   &retryPeriodVal,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// This is safe to enable because the program ends immediately after the manager stops
		// (see mgr.Start() call at the end of main()), and because the engine
		// runnables block until their loops have returned, so the lease is not
		// released while a cycle is still publishing. This enables fast failover
		// during deployments and upgrades, reducing downtime from ~60s to ~1-2s.
		LeaderElectionReleaseOnCancel: true,
		// Set explicitly, and below the pod's terminationGracePeriodSeconds
		// (config/base/manager/deployment.yaml): if runnables outlive the grace
		// period the kubelet's SIGKILL lands before the lease is released, and
		// the successor waits the full LeaseDuration with GetMetrics unanswered.
		GracefulShutdownTimeout: &gracefulShutdownTimeout,
	}
}

// defaultLogVerbosity is the -v the shipped deployment runs at: config/base/manager
// passes no -v, so this value decides which V() levels reach the logs. The
// saturation analyzer's per-variant capacity lines (replica-capacity-summary,
// itl-window, itl-fit) sit at V(logging.DEFAULT), so lowering this silently drops
// them. TestDefaultVerbosityKeepsCapacityLogsVisible guards the relationship. Its
// per-replica lines sit one level higher, at V(logging.VERBOSE): they grow with
// the fleet, and hack/benchmark/dump_k2_decisions.py needs --v=3 to read them.
const defaultLogVerbosity = logging.DEFAULT

// nolint:gocyclo
func main() {
	// Command-line flags

	loggerVerbosity := flag.Int("v", defaultLogVerbosity, "number for the log level verbosity")

	configFilePath := flag.String("config-file", "", "Path to the YAML configuration file. "+
		"When set, the main configuration is read from this file instead of a Kubernetes ConfigMap.")

	flag.String("metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.Bool("leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Bool("metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.String("webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.String("webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.String("webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.String("metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.String("metrics-cert-key", "tls.key", "The name of the metrics key file.")
	flag.String("watch-namespace", "",
		"Namespace to watch for updates, or a comma-separated list of them. If unspecified, all namespaces are watched.")

	externalScalerBindAddress := flag.String("external-scaler-bind-address", ":9090",
		"The address the KEDA external scaler gRPC service binds to. "+
			"KEDA ScaledObjects reference this via an external trigger's scalerAddress.")
	externalScalerCertPath := flag.String("external-scaler-cert-path", "",
		"The directory holding tls.crt and tls.key for the KEDA external scaler. "+
			"When set the scaler is served over TLS and KEDA must dial it with a caCert in the "+
			"trigger's authentication. Empty, the default, serves plaintext.")
	externalScalerClientCA := flag.String("external-scaler-client-ca", "",
		"A PEM CA bundle; when set, every external scaler client must present a certificate "+
			"it signed (mutual TLS: KEDA's tlsClientCert and tlsClientKey). "+
			"Requires --external-scaler-cert-path.")
	externalScalerClientSAN := flag.String("external-scaler-client-san", "",
		"A comma-separated list of client identities the external scaler accepts under mutual TLS: "+
			"each matches a client certificate's DNS SAN, URI SAN, or subject Common Name exactly. "+
			"Empty, the default, accepts any certificate --external-scaler-client-ca signed. "+
			"Requires --external-scaler-client-ca.")

	// The warm pool is OFF unless a namespace is named, because it holds GPUs
	// continuously: that is a cost decision an operator makes, never a default.
	// Deprecated, inert, and deliberately still parsed: see where it is read.
	deprecatedWarmPoolNamespace := flag.String("warm-pool-namespace", "",
		"DEPRECATED and ignored. The pool's namespace is the watched one; a "+
			"cluster-scoped install discovers pools wherever a ScaledObject declares one.")
	warmPoolSleepMinSize := flag.Int("warm-pool-sleep-min-size", 1,
		"Floor on FREE pool Pods -- ones with every instance asleep. This is the reserve "+
			"the pool keeps for the next spike, per pool rather than per model.")
	warmPoolMaxHold := flag.Duration("warm-pool-max-hold", 2*time.Minute,
		"How long a borrowed Pod may serve before it is returned regardless. Bounds the case "+
			"where the ordinary replicas never arrive, which would otherwise turn insurance "+
			"into permanent capacity for one variant.")
	warmPoolMinHold := flag.Duration("warm-pool-min-hold", 0,
		"Floor on how long a borrowed Pod serves: it is not handed back as excess until it "+
			"has been lent this long, even once the ordinary replicas report Ready. A replica "+
			"that has just passed its probe has an empty KV and prefix cache, so returning the "+
			"bridge at that instant swaps a warm engine for a cold one at the crossover. "+
			"Zero, the default, leaves the behaviour unchanged; the floor is bounded by "+
			"--warm-pool-max-hold and waived whenever another variant is short.")
	warmPoolMemoryBudget := flag.Int64("warm-pool-memory-bytes", 0,
		"Host memory ONE pool Pod may commit to sleeping weights. A level-1 sleeper keeps its "+
			"weights in host memory, so admitting one model too many does not fail that "+
			"admission -- it OOM-kills the launcher and destroys every model already resident "+
			"in the Pod. Clamped to the container's own limit, so it cannot pretend to be a "+
			"wall it is not; 0 means use that limit. A sleeper costs 2.6GiB + 1.4x its weights, "+
			"measured -- so 120Gi is four 8B models, which is where the break-even sits. "+
			"Defaults to 0: TAKE the container's limit, which is the wall the kubelet actually "+
			"enforces, rather than restating it here where the two can disagree.")
	warmPoolGPUUtil := flag.Float64("warm-pool-gpu-memory-utilization", 0.90,
		"Override --gpu-memory-utilization for warm copies. A workload's value is sized for a "+
			"Pod running ONE engine and typically claims ~95% of the card, which leaves room "+
			"for about three sleepers on an 80GiB GPU rather than the sixteen the warm set "+
			"allows. Lowering it trades KV cache for warm-set size, and costs one extra "+
			"compile (~9s) per model because the value is part of the torch.compile cache key. "+
			"0 inherits the workload's value. Defaults to 0.90, which leaves room for three 8B "+
			"sleepers' GPU residue beside an awake one at a cost of ~6% of KV cache -- the pool "+
			"only pays when it holds several models per GPU, and an inherited 0.95 leaves room "+
			"for none.")
	warmPoolPreloadTop := flag.Int("warm-pool-preload-top", 2,
		"Preload this many of the busiest variants while the pool has spare reserve, instead "+
			"of waiting for each to miss. Popularity is share of desired replicas, which "+
			"tracks share of requests. 0 disables preloading, leaving admission to parking "+
			"and the frequency filter.")

	// Leader election timeout configuration flags
	// These can be overridden in manager.yaml to tune for different environments
	// (e.g., higher values for environments with network latency or API server slowness)
	flag.Duration("leader-election-lease-duration", 60*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership. "+
			"Increased from default 15s to 60s to prevent lease renewal failures in environments with network latency.")
	flag.Duration("leader-election-renew-deadline", 50*time.Second,
		"The duration that the acting master will retry refreshing leadership before giving up. "+
			"Increased from default 10s to 50s to provide more tolerance for network latency and API server delays.")
	flag.Duration("leader-election-retry-period", 10*time.Second,
		"The duration the clients should wait between tries of actions. "+
			"Increased from default 2s to 10s to reduce API server load and provide more time between renewal attempts.")
	flag.Duration("rest-client-timeout", 60*time.Second,
		"The timeout for REST API calls to the Kubernetes API server. "+
			"Increased from default ~30s to 60s for better resilience against network latency.")

	opts := ctrlzap.Options{
		Development: true,
	}
	gfs := goflag.NewFlagSet("zap", goflag.ExitOnError)
	opts.BindFlags(gfs) // zap expects a standard Go FlagSet and pflag.FlagSet is not compatible.
	flag.CommandLine.AddGoFlagSet(gfs)

	flag.Parse()

	logging.InitLogging(&opts, loggerVerbosity)
	defer logging.Sync() // nolint:errcheck

	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("Logger initialized")

	// The process's ONE signal context. ctrl.SetupSignalHandler may be called only
	// once (a second call panics), so it is taken here and handed to every
	// startup step as well as to mgr.Start: a SIGTERM during a Prometheus
	// validation retry or a ConfigMap bootstrap then ends the process at once,
	// instead of after a backoff of unknown length.
	signalCtx := ctrl.SetupSignalHandler()

	// Get REST config early (needed for config loading)
	restConfig := ctrl.GetConfigOrDie()

	// Load unified configuration (fail-fast if invalid)
	// Viper resolves precedence: flags > env > config file > defaults
	// For more information see:
	// docs/reference/configuration.md
	cfg, err := config.Load(flag.CommandLine, *configFilePath)
	if err != nil {
		setupLog.Error(err, "failed to load configuration - this is a fatal error")
		logging.Sync() //nolint:errcheck
		os.Exit(1)     //nolint:gocritic // exitAfterDefer: Sync() called explicitly above
	}
	setupLog.Info("Configuration loaded successfully")

	// Where cluster policy comes from has to be settled BEFORE the manager exists,
	// because the manager's cache is built from the answer — a namespace absent
	// from the cache is a namespace whose limiters can never be read. It needs a
	// live lookup (the well-known namespace only wins if it is actually there), so
	// it uses a direct client rather than the manager's, which is not running yet.
	policyProbe, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build a client to resolve the cluster policy namespace")
		os.Exit(1)
	}
	if err := controller.ResolvePolicyNamespace(signalCtx, policyProbe, cfg); err != nil {
		os.Exit(startupExitCode(signalCtx, setupLog, err, "unable to resolve where cluster policy comes from"))
	}

	// Conditionally add LeaderWorkerSet scheme if CRD exists
	lwsEnabled := crd.CheckLeaderWorkerSetCRD(restConfig, setupLog)
	if lwsEnabled {
		if err := lwsv1.AddToScheme(scheme); err != nil {
			setupLog.Error(err, "failed to add LeaderWorkerSet scheme")
			os.Exit(1)
		}
		setupLog.Info("LeaderWorkerSet CRD detected - support enabled")
	} else {
		setupLog.Info("LeaderWorkerSet CRD not found - support disabled (Deployment-only mode)")
	}

	// KEDA is not optional: WVA discovers the workloads it manages by being called
	// about them over the external-scaler contract, and KEDA is what makes those
	// calls. Without it nothing ever registers, so WVA runs and manages nothing —
	// which is quiet enough to be worth saying loudly here.
	//
	// The CRD check is a diagnostic only. Nothing branches on it: there is no
	// ScaledObject watch, index or cached read left to gate (see
	// docs/plans/engine/keda-driven-discovery.md).
	if crd.CheckKEDACRD(restConfig, setupLog) {
		setupLog.Info("KEDA ScaledObject CRD detected - WVA will be discovered by KEDA calling its external scaler")
	} else {
		setupLog.Info("WARNING: KEDA ScaledObject CRD not found. WVA discovers workloads only when KEDA " +
			"calls its external scaler, so nothing will be discovered or scaled until KEDA is installed " +
			"and a ScaledObject names this scaler in a trigger.")
	}

	tlsOpts := []func(*tls.Config){
		func(c *tls.Config) {
			c.NextProtos = []string{"h2", "http/1.1"}
		},
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(cfg.WebhookCertPath()) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhookCertPath", cfg.WebhookCertPath(),
			"webhookCertName", cfg.WebhookCertName(),
			"webhookCertKey", cfg.WebhookCertKey())

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertName()),
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/base/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.MetricsAddr(),
		SecureServing: cfg.SecureMetrics(),
		TLSOpts:       tlsOpts,
	}

	if cfg.SecureMetrics() {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/base/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/base/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/base/monitoring/kustomization.yaml for TLS certification.
	if len(cfg.MetricsCertPath()) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metricsCertPath", cfg.MetricsCertPath(),
			"metricsCertName", cfg.MetricsCertName(),
			"metricsCertKey", cfg.MetricsCertKey(),
		)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertName()),
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// --- Setup Datastore ---
	ds := datastore.NewDatastore(cfg)

	// Use configurable REST client timeout from Config (default 60s, can be overridden via --rest-client-timeout flag)
	restConfig.Timeout = cfg.RestTimeout()

	mgrOptions := managerOptions(cfg, metricsServerOptions, webhookServer)

	watchNS := cfg.WatchNamespace()
	// The watched namespaces this controller may read now. One it may not --
	// its RoleBinding deleted, or the namespace gone -- would hold every
	// informer, and so the whole controller, short of Ready. It is left out of
	// the cache, reported (wva_watch_namespace_unreadable, the log), and asked
	// again by watchns.Prober, which restarts the manager when the set
	// changes.
	var unreadableWatched map[string]string
	// One watched namespace is cached as before, readable or not: there are no
	// others to keep running.
	readableWatched := cfg.WatchNamespaces()
	if len(readableWatched) > 1 {
		readableWatched, unreadableWatched = watchns.Split(signalCtx, policyProbe, cfg.WatchNamespaces())
		blocked := make([]string, 0, len(unreadableWatched))
		for ns, why := range unreadableWatched {
			blocked = append(blocked, ns)
			setupLog.Error(nil, "A watched namespace cannot be read; it is left unmanaged and the others run. Restore the controller's Role and RoleBinding there (make setup-prereqs), or take it out of WVA_WATCH_NS",
				"namespace", ns, "reason", why)
		}
		cfg.SetUnreadableNamespaces(blocked)
	}
	if watchNS != "" {
		namespaces := cacheNamespaces(readableWatched, config.SystemNamespace())
		setupLog.Info("Watching namespaces", "namespaces", readableWatched, "unreadable", unreadableWatched,
			"controllerNamespace", config.SystemNamespace())
		// The policy namespace is deliberately NOT added to the cache.
		//
		// Adding it starts an informer, and an informer that cannot list its
		// namespace never syncs — so WaitForCacheSync blocks forever and the
		// controller never becomes ready. That is the normal case here, not an edge
		// one: a namespace-scoped install holds RBAC in its own namespace only, and
		// the whole point of the policy namespace is that it belongs to someone
		// else. An admin applying the documented policy-namespace label would have
		// bricked the very controller they were trying to bound.
		//
		// Cluster policy is read through the direct API reader instead (the
		// ConfigMapReconciler holds mgr.GetAPIReader()), which fails as a plain
		// error we can report rather than as a hang.
		//
		// The cost is that a policy edit in that namespace is no longer watched, so
		// it takes effect on restart rather than live. That is the right trade: a
		// bound that updates slowly beats a controller that never starts, and a
		// namespace-scoped install cannot watch that namespace anyway without RBAC
		// it does not have.
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: namespaces,
		}
	} else {
		// Multi-namespace mode: Use label selector to filter ConfigMaps in the cache
		// This significantly reduces memory usage by only caching WVA-related configmaps
		wvaConfigSelector := labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/name": "workload-variant-autoscaler",
		})

		setupLog.Info("Configuring cache with label selector for ConfigMaps",
			"labelSelector", wvaConfigSelector.String())

		// Configure cache to only watch configmaps with the WVA labels
		// Other resource types are cached normally without filtering
		cmNamespaces := map[string]cache.Config{}
		if cfg.PolicyNamespaceIsSeparate() {
			// Exempt the policy namespace from the label filter. That ConfigMap is
			// written by a cluster admin, by hand or by their own tooling, and
			// expecting them to reproduce one of our labels turns a forgotten label
			// into a controller that reads no quota and says nothing. The bound has
			// to survive being authored by someone who has not read our source.
			// labels.Everything() explicitly: a nil LabelSelector is DEFAULTED to
			// ByObject.Label, so leaving it unset would silently reapply the very
			// filter this entry exists to lift.
			policyNS := cfg.PolicyNamespace()
			cmNamespaces[cache.AllNamespaces] = cache.Config{LabelSelector: wvaConfigSelector}
			cmNamespaces[policyNS] = cache.Config{LabelSelector: labels.Everything()}
			setupLog.Info("Caching the cluster policy namespace without the label filter",
				"policyNamespace", policyNS)
		}
		mgrOptions.Cache = cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					// Empty map means cache all namespaces, but filter by label
					Namespaces: cmNamespaces,
					Label:      wvaConfigSelector,
				},
			},
		}
	}
	// Both shapes: keep only what readers use of each cached object. On a
	// cluster-scoped install the Pod, Deployment and Node informers are
	// cluster-wide and cannot be label-filtered (the GPU usage walk counts every
	// Pod on a GPU node), so trimming is what bounds their memory.
	kubecache.Trim(&mgrOptions.Cache)

	mgr, err := ctrl.NewManager(restConfig, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Kueue Workloads -- the borrowing managed check, the preemption check and
	// the lender-waiting signal -- are read through an informer, indexed on
	// owner UID. It starts on the first read, so a cluster without Kueue, or a
	// policy that does not count borrowing, starts none and needs no watch on
	// Workloads; until it syncs, reads go to the API server.
	//
	// The lender signal needs every namespace's Workloads, whatever the
	// install's scope (D1): a lender waiting in another namespace must stop
	// new borrowing. A namespace-scoped manager's cache holds its own
	// namespace only, and a list from it would answer for that namespace
	// alone, so such an install reads Workloads through a cache of its own,
	// cluster-wide, holding Workloads only. Its read is granted by the
	// admin-applied kueue-reader ClusterRole; without it the informer is never
	// started (kueue.InformerWorkloads probes first) and the read reports
	// Forbidden, which withholds borrowing as lenders-unreadable.
	workloadCache := mgr.GetCache()
	if watchNS != "" {
		wc, err := cache.New(restConfig, cache.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(),
			DefaultTransform: kubecache.StripUnused})
		if err != nil {
			setupLog.Error(err, "unable to create the cluster-wide Kueue Workload cache")
			os.Exit(1)
		}
		if err := mgr.Add(kueueWorkloadCache{wc}); err != nil {
			setupLog.Error(err, "unable to add the cluster-wide Kueue Workload cache")
			os.Exit(1)
		}
		workloadCache = wc
	}
	kueue.UseWorkloadSource(kueue.NewInformerWorkloads(workloadCache, mgr.GetClient()))

	// Initialize metrics
	setupLog.Info("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	setupLog.Info("Metrics emitter created successfully")

	// Create ConfigMap reconciler for configuration management.
	// Bootstrap uses the temporary uncached client so ConfigMap-backed settings
	// are loaded before any manager runnables start.
	// The controller-status ConfigMap: what this controller has read and decided,
	// for tools and operators that would otherwise grep the log. Built before the
	// bootstrap so the policy it loads is recorded; written only by the leader.
	controllerStatus := controllerstatus.New(mgr.GetClient(), mgr.GetAPIReader(), config.SystemNamespace())
	if err := mgr.Add(controllerStatus); err != nil {
		setupLog.Error(err, "unable to add the controller-status publisher to the manager")
		os.Exit(1)
	}

	configMapReconciler := &controller.ConfigMapReconciler{
		Reader:    mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		Datastore: ds,
		// The core/v1 events recorder: its replacement, GetEventRecorder, writes
		// events.k8s.io Events, which the manager's Role does not grant.
		Recorder: mgr.GetEventRecorderFor("workload-variant-autoscaler-configmap-reconciler"), //nolint:staticcheck // SA1019, see above
		Status:   controllerStatus,
	}

	ctx := ctrl.LoggerInto(signalCtx, setupLog)
	if err = configMapReconciler.BootstrapInitialConfigMaps(ctx); err != nil {
		os.Exit(startupExitCode(ctx, setupLog, err, "unable to bootstrap initial ConfigMaps"))
	}
	setupLog.Info("Initial ConfigMap bootstrap completed")

	// Frozen registration decision: analyzer registration cannot change without
	// a controller restart, so this value is captured once (after the bootstrap
	// above has loaded ConfigMap-backed settings) and shared with the
	// ConfigMapReconciler so it can detect a later live-config divergence.
	taRegistered := cfg.ThroughputAnalyzerEnabled()
	configMapReconciler.ThroughputRegistered = taRegistered

	// Use Prometheus configuration from unified Config (already validated during Load())
	if cfg.PrometheusBaseURL() == "" {
		setupLog.Error(nil, "no Prometheus configuration found - this should not happen after validation")
		os.Exit(1)
	}

	// Validate Prometheus transport configuration before creating the client.
	if err := prometheusutil.ValidateTLSConfig(cfg); err != nil {
		setupLog.Error(err, "Prometheus transport configuration validation failed")
		os.Exit(1)
	}

	promURL, err := url.Parse(cfg.PrometheusBaseURL())
	if err != nil {
		// ValidateTLSConfig above parses the same string, so this should be
		// unreachable; it is checked rather than assumed because promURL is
		// dereferenced on the next line.
		setupLog.Error(err, "invalid Prometheus URL")
		os.Exit(1)
	}
	setupLog.Info("Initializing Prometheus client",
		"address", promURL.Redacted(),
		"tlsEnabled", prometheusutil.IsHTTPS(cfg.PrometheusBaseURL()),
		"allowHTTP", cfg.PrometheusAllowHTTP(),
	)

	// Create Prometheus client with TLS support
	promClientConfig, err := prometheusutil.CreatePrometheusClientConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client config")
		os.Exit(1)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client")
		os.Exit(1)
	}

	promAPI := promv1.NewAPI(promClient)

	// Validate that the API is working by testing a simple query with retry logic
	if err := prometheusutil.ValidatePrometheusAPI(ctx, promAPI); err != nil {
		os.Exit(startupExitCode(ctx, setupLog, err,
			"CRITICAL: Failed to connect to Prometheus - WVA requires Prometheus connectivity for autoscaling decisions"))
	}
	setupLog.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Keep the cluster's GPU usage picture current, independently of either engine.
	//
	// Registered BEFORE them so it takes its first observation first: both engines
	// consume this, and until it exists the capacity checks have no evidence and
	// degrade to "unknown", which is permissive. It is the SOLE producer of the
	// physical snapshot — see internal/gpuusage.
	//
	// The TIMER is only worth running for a deployment that reads the physical
	// view, which is exactly one condition now that the limiters list is the sole
	// source of truth: a physical limiter is declared. Nothing else consumes this.
	// A deployment that declares no limiter is not limited at all, and one that
	// declares only a quota is charged for WVA's own variants
	// (allocation.ManagedUsage) — both would otherwise list nodes and walk every pod
	// in the cluster every interval to produce a number with no reader.
	//
	// Re-read every tick, because the limiters list is live: switching it must not
	// need a restart.
	usageDiscovery := gpunodes.NewK8sWithGpuOperator(mgr.GetClient())
	if watchNS != "" {
		// The cache holds this namespace's Pods only, and a usage walk through it
		// counts other tenants' GPUs on the same nodes as free. Read Pods uncached;
		// when that is Forbidden the walk falls back to the cache and says so
		// (wva_pod_access_denied) while a physical limiter reads the result. The
		// limiter list is live, so that is asked per list, not decided here.
		usageDiscovery = usageDiscovery.WithPodReader(mgr.GetAPIReader()).
			WithPhysicalLimiterCheck(func() bool { return allocation.PhysicalUsageConfigured(cfg) }).
			WithPodAccessDeniedHook(controllerStatus.SetPodListDenied)
	}
	usageRefresher := &gpuusage.Refresher{
		Discovery: usageDiscovery,
		Periodic: func() bool {
			if watchNS != "" && !allocation.PhysicalUsageConfigured(cfg) {
				// The walk stops when the physical limiter goes, and nothing
				// else would clear a denial it reported: without the limiter
				// the undercount bounds nothing.
				usageDiscovery.ResetPodAccessDenied()
			}
			return allocation.NodeUsageWanted(cfg, watchNS != "")
		},
	}
	if err := mgr.Add(usageRefresher); err != nil {
		setupLog.Error(err, "unable to add the GPU usage refresher to the manager")
		os.Exit(1)
	}

	// Register optimization engine loops with the manager. Only start when leader.
	err = mgr.Add(optimizeLoopRunnable(func(ctx context.Context) (optimizeLooper, error) {
		sourceRegistry := source.NewSourceRegistry()
		setupLog.Info("Initializing metrics source registry")

		// Prometheus cache configuration is loaded via unified Config during startup.
		// The cache config is available in cfg.Dynamic.PrometheusCache and is updated
		// automatically when the ConfigMap changes. We use the default config here
		// as the unified Config system handles cache configuration loading.

		// Register PrometheusSource with default config
		promSource := prometheus.NewPrometheusSource(ctx, promAPI, prometheus.DefaultPrometheusSourceConfig())

		// Register in global source registry (registerMetricsSource returns its
		// error rather than exiting; see optimizeLoopRunnable).
		if err := registerMetricsSource(sourceRegistry, "prometheus", promSource); err != nil {
			setupLog.Error(err, "failed to register prometheus source in source registry")
			return nil, err
		}

		// Build the initial GPU limiter from every declared limiter — the
		// limiters: list on the saturation "default" config, which is the sole
		// source. The ConfigMaps were bootstrapped above, so the selection is
		// already visible here. The engine rebuilds the limiter live (see
		// SetLimiterBuilder) when the ConfigMap changes.
		gpuLimiter, err := allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		if err != nil {
			setupLog.Error(err, "failed to build GPU limiter")
			return nil, err
		}
		limiterTypes := cfg.EffectiveLimiterTypes()
		setupLog.Info("GPU limiter constructed", "limiterTypes", limiterTypes.String(), "limiters", cfg.EffectiveLimiterSummary(),
			"name", gpuLimiter.Name())

		// Declaring no limiter is a legitimate configuration, but it is worth saying
		// out loud: it means scaling is bounded by nothing WVA knows about. The
		// optimizer allocates without a GPU budget and a scale-from-zero wake is
		// published without a capacity check, so a variant can be woken onto an
		// accelerator that is already full. There is no longer a hidden default
		// limiter making this safe.
		if !limiterTypes.Declared() {
			setupLog.Info("No limiters declared in the saturation-scaling ConfigMap: scaling is " +
				"UNCONSTRAINED. The optimizer allocates without a GPU budget and scale-from-zero " +
				"wakes are published without a capacity check. Add a limiters: entry " +
				"(gpu-inventory or quota) to bound scaling.")
		}

		// One physical limiter is built however many physical entries are
		// declared; the duplicates are accepted rather than rejected, since a
		// rejection would discard the whole policy, and named here.
		if dups := cfg.IgnoredPhysicalLimiters(); len(dups) > 0 {
			setupLog.Info(config.IgnoredPhysicalLimitersMessage, "ignoredIndexes", dups)
		}
		// A cluster quota read by a namespace-scoped controller caps this
		// controller only. Said at start; the engine says it again when a
		// rebuilt limiter set still does (steadystate refreshLimiter).
		if cfg.ClusterQuotaPerController() {
			setupLog.Info(config.ClusterQuotaPerControllerMessage,
				"watchNamespace", watchNS, "limiters", cfg.EffectiveLimiterSummary())
		}

		// Quota entries alone mean "no physical-capacity discovery" — including
		// the inventory-collection call in the saturation engine. We honor that
		// at the call site (see scalingpolicy.ShouldCollectClusterInventory),
		// but warn loudly here so an operator who explicitly enabled
		// WVA_LIMITED_MODE sees that their inventory log will be suppressed.
		// A quota beside a physical entry no longer suppresses it: both are built.
		if cfg.LimitedModeEnabled() && limiterTypes.Declared() && !limiterTypes.HasPhysical() {
			setupLog.Info("Only quota limiters are declared; cluster inventory collection is disabled "+
				"despite WVA_LIMITED_MODE=true (no Node API access without a physical limiter). "+
				"To re-enable cluster inventory logging, declare a gpu-inventory limiter.",
				"limiters", cfg.EffectiveLimiterSummary(),
				"limitedModeEnabled", cfg.LimitedModeEnabled())
		}

		engine := steadystate.NewEngine(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			mgr.GetScheme(),
			mgr.GetEventRecorderFor("workload-variant-autoscaler-saturation-engine"), //nolint:staticcheck // SA1019: core/v1 events; see the configmap reconciler
			sourceRegistry,
			cfg, // Pass unified Config to engine
			gpuLimiter,
		)
		// Discovery: the workloads KEDA has called the external scaler about.
		// The enricher reads with GetAPIReader deliberately — a cached read of a
		// ScaledObject is served by a cluster-wide informer, which is the LIST+WATCH
		// this design exists to remove.
		engine.Variants = registry.Default
		engine.Status = controllerStatus
		// Kueue's preemptions of managed pods, recorded from the pod informer
		// the engine already reads pods through: a preempted pod and its
		// Workload are gone within seconds, faster than a cycle's list.
		preemptions := steadystate.NewPreemptionWatch(0)
		if podInformer, err := mgr.GetCache().GetInformer(ctx, &corev1.Pod{}); err != nil {
			setupLog.Error(err, "Cannot watch pods for Kueue preemptions; they are read by listing once per cycle only")
		} else if _, err := podInformer.AddEventHandler(preemptions.Handler()); err != nil {
			setupLog.Error(err, "Cannot watch pods for Kueue preemptions; they are read by listing once per cycle only")
		} else {
			engine.Preemptions = preemptions
		}
		engine.VariantEnricher = registry.NewEnricher(mgr.GetAPIReader(), registry.Default, registry.DefaultTargetMaxAge)
		engine.VariantEnricher.Recorder = mgr.GetEventRecorderFor("workload-variant-autoscaler-registry") //nolint:staticcheck // SA1019: core/v1 events; see the configmap reconciler
		// The registry is also what tells the datastore which namespaces to load
		// configuration for. That used to come from a ScaledObject reconciler, whose
		// watch was a cluster-wide informer; the registry knows the same thing for a
		// better reason — these are the namespaces WVA has actually been called about.
		engine.VariantEnricher.Tracker = ds
		// Rebuild the limiter live when the saturation ConfigMap's limiters: list
		// changes — no restart required. The builder re-reads the effective config.
		engine.SetLimiterBuilder(func() (allocation.Limiter, error) {
			return allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		})
		// Arrival rate is registered whatever the analyzer set: the saturation
		// analyzer's demand floor needs it too, and gating it on the throughput
		// analyzer made that floor structurally inoperable whenever throughput
		// was disabled -- which is the default.
		registration.RegisterAlwaysOnQueries(sourceRegistry)
		if taRegistered {
			if err := engine.RegisterAnalyzer(throughput.AnalyzerName, throughput.NewThroughputAnalyzer().WithClock(engine.Clock())); err != nil {
				return nil, err
			}
			setupLog.Info("ThroughputAnalyzer registered (enabled in saturation config)")
		} else {
			setupLog.Info("ThroughputAnalyzer NOT registered — no saturation config entry " +
				"enables 'throughput'. Add it to the analyzers config and restart the " +
				"controller to enable it.")
		}
		return engine, nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// Register scale from zero engine loop with the manager. Only start when leader.
	err = mgr.Add(optimizeLoopRunnable(func(ctx context.Context) (optimizeLooper, error) {
		engine, err := scalefromzero.NewEngine(mgr.GetClient(), mgr.GetEventRecorderFor("workload-variant-autoscaler-scalezero-engine"), ds, cfg) //nolint:staticcheck // SA1019: core/v1 events; see the configmap reconciler
		if err != nil {
			return nil, err
		}
		// Same registry as the saturation engine: one call registers a workload for
		// both. Its own Enricher, because the two loops run at different rates and
		// each must be free to refresh when it needs to — they share the registry,
		// which is where the freshness actually lives, so neither re-reads what the
		// other has just read.
		engine.Variants = registry.Default
		engine.VariantEnricher = registry.NewEnricher(mgr.GetAPIReader(), registry.Default, registry.DefaultTargetMaxAge)
		engine.VariantEnricher.Recorder = mgr.GetEventRecorderFor("workload-variant-autoscaler-registry") //nolint:staticcheck // SA1019: core/v1 events; see the configmap reconciler
		// Let the engine bring the usage picture up to date immediately before it
		// places a wake. The periodic observation alone can be a full interval old
		// at the moment it is consulted, and a workload that has just started
		// holding GPUs is exactly what a placement must not miss.
		engine.UsageRefresher = usageRefresher
		// Second transport for the wake signal. This engine scrapes the EPP pod
		// directly, which is the only WVA metric path that does not go through
		// Prometheus — so a broken token, missing EPP tokenreview RBAC or a
		// NetworkPolicy takes out wakes alone, silently, while the same queue
		// depth sits readable in Prometheus. Nil source yields a nil fallback and
		// the pre-existing behaviour; see queue_fallback.go.
		engine.QueueFallback = scalefromzero.NewQueueFallback(
			prometheus.NewPrometheusSource(ctx, promAPI, prometheus.DefaultPrometheusSourceConfig()))
		// Give the engine a limiter so a wake is only published for a variant
		// that can actually be placed. This is its own instance rather than the
		// saturation engine's: a limiter supplies constraints from usage passed
		// per call, so instances share no mutable state, and building one here
		// avoids threading the other engine's closure-scoped value across.
		//
		// Rebuilt live on a ConfigMap change, exactly like the saturation engine's.
		// It used to be built once here and never again, so editing the limiters:
		// list changed how the optimizer allocated and left the scale-from-zero
		// capacity check running against whatever was configured at startup — with
		// the list now the sole switch for both, that split is a trap. A limiter
		// that cannot be built is not fatal: the engine then wakes without a
		// capacity check, which is what it did before selection existed.
		if sfzLimiter, limErr := allocation.NewLimiterFromConfig(cfg, mgr.GetClient()); limErr != nil {
			setupLog.Error(limErr, "failed to build GPU limiter for scale-from-zero; waking without a capacity check")
		} else {
			engine.SetGPULimiter(sfzLimiter)
		}
		engine.SetLimiterBuilder(func() (allocation.Limiter, error) {
			return allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		})
		return engine, nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// Register the KEDA external scaler gRPC server. NOT leader-gated: it
	// listens from process start so KEDA's GetMetricSpec is answered in the
	// window between this pod becoming Ready and its winning the lease, which
	// is where a ScaledObject would otherwise be wired to the CPU default for
	// good (see scaler.Server.NeedLeaderElection). Elected tells the handler
	// when the decision store behind it is being fed; until then the calls
	// that read it are refused rather than answered with "no decision".
	//
	// It writes registry.Default as well as reading the decision store: every call
	// registers the workload it names, which is how WVA discovers what it manages
	// (docs/plans/engine/keda-driven-discovery.md). Registering before the lease
	// is held is harmless and useful -- the engines that consume the registry
	// start on election and find the workloads already there.
	externalScaler := &scaler.Server{
		Addr:     *externalScalerBindAddress,
		Client:   mgr.GetAPIReader(),
		Registry: registry.Default,
		// A decision is stale after three optimize intervals (and never before
		// 60 s). The engines read the interval once, when they start, so this
		// matches what they publish at.
		DecisionInterval: cfg.OptimizationInterval,
	}
	// Without leader election this replica is the leader from the start; the
	// manager's Elected channel says so too, but only after it has started
	// the server, which would put it through a standby phase for nothing.
	if cfg.EnableLeaderElection() {
		externalScaler.Elected = mgr.Elected()
	}
	allowedScalerClients, err := scaler.CheckTLSFlags(scaler.TLSFlags{
		CertPath:     *externalScalerCertPath,
		ClientCA:     *externalScalerClientCA,
		ClientCASet:  flag.CommandLine.Changed("external-scaler-client-ca"),
		ClientSAN:    *externalScalerClientSAN,
		ClientSANSet: flag.CommandLine.Changed("external-scaler-client-san"),
	})
	if err != nil {
		setupLog.Error(err, "invalid external scaler TLS flags")
		os.Exit(1)
	}
	if *externalScalerCertPath != "" {
		scalerCertWatcher, err := certwatcher.New(
			filepath.Join(*externalScalerCertPath, "tls.crt"),
			filepath.Join(*externalScalerCertPath, "tls.key"),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize external scaler certificate watcher")
			os.Exit(1)
		}
		externalScaler.TLSConfig, err = scaler.NewTLSConfig(scalerCertWatcher.GetCertificate, *externalScalerClientCA, allowedScalerClients)
		if err != nil {
			setupLog.Error(err, "unable to configure external scaler TLS")
			os.Exit(1)
		}
		if err := mgr.Add(scalerCertWatcher); err != nil {
			setupLog.Error(err, "unable to add external scaler certificate watcher to manager")
			os.Exit(1)
		}
		setupLog.Info("KEDA external scaler serves TLS",
			"certPath", *externalScalerCertPath, "mutualTLS", *externalScalerClientCA != "",
			"allowedClients", allowedScalerClients)
		if *externalScalerClientCA != "" && len(allowedScalerClients) == 0 {
			setupLog.Info("WARNING: the external scaler accepts ANY client certificate the client CA signed; "+
				"set --external-scaler-client-san to accept only KEDA's identity",
				"clientCA", *externalScalerClientCA)
		}
	}
	if err := mgr.Add(externalScaler); err != nil {
		setupLog.Error(err, "unable to add KEDA external scaler to manager")
		os.Exit(1)
	}
	// Ready means "in the Service KEDA dials", so it must not precede the
	// listener: a Ready pod that refuses connections is the window above.
	if err := mgr.AddReadyzCheck("external-scaler", externalScaler.Ready); err != nil {
		setupLog.Error(err, "unable to register the KEDA external scaler readiness check")
		os.Exit(1)
	}

	// The warm pool: GPU-holding Pods that keep models resident but asleep, so a
	// scale-up can be covered in ~0.4 s while the ordinary replicas take ~35 s.
	//
	// Leader-gated with everything else, and driven off the decision store rather
	// than off KEDA: a decision is known here before KEDA is told about it, so a
	// bridge starts at decision time instead of a poll interval later. The pool
	// never touches the metric KEDA reads -- it borrows underneath the same
	// decision KEDA is about to act on, which is what keeps lent capacity out of
	// the scaling arithmetic.
	// The pool namespace is derived, not configured, wherever it can be: a
	// namespace-scoped install watches exactly one namespace, and the pool warms
	// the workloads in it, so the flag could only ever repeat watchNS or
	// contradict it. Contradicting it is the interesting case -- the controller
	// cannot see across the boundary, so every read fails and the pool reports
	// itself empty, which is indistinguishable from a pool that is simply too
	// small.
	// ACCEPTED AND IGNORED, not removed outright. Go's flag package exits on an
	// unknown flag, so deleting this made every Deployment that still passes it
	// crash-loop on upgrade -- which is what happened the first time this build
	// was rolled onto a cluster whose manifest set it. A flag nothing should
	// pass is still a flag something DOES pass.
	//
	// Kept hidden and inert for one release, so an upgrade is a no-op and the
	// operator is told once rather than discovering it as a CrashLoopBackOff.
	if *deprecatedWarmPoolNamespace != "" {
		setupLog.Info("--warm-pool-namespace is deprecated and ignored; the pool's namespace is "+
			"the watched one, and a cluster-scoped install discovers pools wherever a ScaledObject "+
			"declares them. Remove it from your manifest.",
			"ignoredValue", *deprecatedWarmPoolNamespace, "watching", watchNS)
	}
	// The pool's namespace is the WATCHED one, and there is no way to say
	// otherwise. A flag used to exist and could only repeat this or contradict
	// it: contradicting it means the controller cannot read across the boundary,
	// so every pool read fails and the pool reports itself empty, which is
	// indistinguishable from a pool that is simply too small. Nothing set it
	// except the e2e, which used it to force a reconciler into a namespace that
	// declared no pool -- and paid for that with a controller restart per spec.
	// One reconciler per readable watched namespace.
	if watchNS != "" {
		for _, warmPoolNS := range readableWatched {
			if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
				return runWarmPool(ctx, mgr, ds, warmPoolNS, warmPoolGPUUtil, warmPoolSleepMinSize,
					warmPoolMaxHold, warmPoolMinHold, warmPoolPreloadTop, warmPoolMemoryBudget)
			})); err != nil {
				setupLog.Error(err, "unable to add the warm pool to manager", "namespace", warmPoolNS)
				os.Exit(1)
			}
		}
	} else if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// EVERY namespace that declares a pool, discovered as they appear.
		//
		// Reached only by a cluster-scoped install that named no namespace,
		// where the alternative was not a safer default but no warm pool at
		// all: the operator created the workload and the ScaledObject that
		// declares it, and WVA ignored both. The opt-in is unchanged -- it is
		// that declaration -- and this acts on it wherever it appears.
		mux := &warmpool.Multiplexer{
			Snapshot: registry.Default.Snapshot,
			Start: func(ctx context.Context, ns string) (context.CancelFunc, <-chan struct{}) {
				nsCtx, cancel := context.WithCancel(ctx)
				// Closed when this namespace's reconciler has stopped, so the
				// Multiplexer can start it again. runWarmPool returns on its own
				// when the RBAC review denies borrows, and without this the
				// namespace would look permanently handled by a goroutine that
				// had already returned.
				done := make(chan struct{})
				go func() {
					defer close(done)
					if err := runWarmPool(nsCtx, mgr, ds, ns, warmPoolGPUUtil, warmPoolSleepMinSize,
						warmPoolMaxHold, warmPoolMinHold, warmPoolPreloadTop, warmPoolMemoryBudget); err != nil {
						setupLog.Error(err, "warm pool stopped", "namespace", ns)
					}
				}()
				return cancel, done
			},
		}
		return mux.Run(ctx)
	})); err != nil {
		setupLog.Error(err, "unable to add the warm pool multiplexer to manager")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	// Create InferencePool reconciler
	poolGroupEnv := os.Getenv("POOL_GROUP")
	poolGKNN, err := poolutil.GetPoolGKNN(poolGroupEnv)
	if err != nil {
		setupLog.Error(err, "unable to create default pool GKNN from POOL_GROUP", "poolGroup", poolGroupEnv)
		os.Exit(1)
	}
	inferencePoolReconciler := &controller.InferencePoolReconciler{
		Datastore: ds,
		Client:    mgr.GetClient(),
		PoolGKNN:  poolGKNN,
	}

	if err = inferencePoolReconciler.SetupWithManager(mgr); err != nil {
		if !errors.Is(err, controller.ErrInferencePoolCRDNotServed) {
			setupLog.Error(err, "unable to create inferencePool controller")
			os.Exit(1)
		}
		// Starting without it beats the alternative: a controller for an unserved
		// kind never syncs, and once this replica wins the lease the manager exits
		// on the cache-sync timeout, a crash loop that only shows after election.
		setupLog.Error(err, "InferencePool CRD not found: the InferencePool controller is NOT running, so no "+
			"EPP pool is tracked and EPP queue metrics are unavailable. Install the Gateway API Inference "+
			"Extension CRDs and restart the controller.", "poolGroup", poolGKNN.Group)
	}

	if err = configMapReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create configmap controller")
		os.Exit(1)
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Cluster policy this controller cannot serve takes it OUT OF SERVICE, rather
	// than leaving it running and silently refusing to scale anything.
	//
	// It is a readiness check, not a startup check, because the failure has two
	// arrival times and only one of them is an install. On day one it fails the
	// rollout, so `verify_deployment` reports a failed install instead of a green
	// one. On day ninety — when a cluster admin adds a gpu-inventory limiter to
	// cluster policy, which this controller reloads live — the same condition
	// becomes true and the pod goes NotReady, which is visible, alertable, and
	// nothing like the alternative: every variant quietly charged to no accelerator
	// pool, given no budget, and never scaling up again.
	if err := mgr.AddReadyzCheck("gpu-policy", func(req *http.Request) error {
		return gpuPolicyReady(req.Context(), cfg, func(ctx context.Context) error {
			return nodeReadProbe(ctx, policyProbe)
		})
	}); err != nil {
		setupLog.Error(err, "unable to set up the GPU policy ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		if cfg.ConfigMapsBootstrapComplete() {
			return nil
		}
		_, _, syncErr := cfg.ConfigMapsBootstrapSyncStatus()
		if syncErr != "" {
			return fmt.Errorf("initial ConfigMap bootstrap not complete: %s", syncErr)
		}
		return errors.New("initial ConfigMap bootstrap not complete")
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")

	// Sync the custom logger before starting the manager
	if err := logging.Sync(); err != nil {
		setupLog.Error(err, "Failed to sync logger before starting manager")
		os.Exit(1)
	}

	// Register custom metrics with the controller-runtime Prometheus registry
	// This makes the metrics available for scraping by Prometheus and direct endpoint access
	setupLog.Info("Registering custom metrics with Prometheus registry")
	if err := metrics.InitMetrics(crmetrics.Registry); err != nil {
		setupLog.Error(err, "failed to initialize metrics")
		os.Exit(1)
	}
	// The policy namespace was resolved before the registry existed; publish the
	// outcome now so a fallback forced by a Forbidden Namespace read is alertable.
	metrics.SetPolicyNamespaceUnverified(cfg.PolicySource() == config.PolicySourceLocalUnverified)

	metrics.SetWatchNamespacesUnreadable(unreadableWatched)
	if watchNS != "" && len(cfg.WatchNamespaces()) > 1 {
		if err := mgr.Add(&watchns.Prober{Reader: policyProbe, Watched: cfg.WatchNamespaces(),
			Active: readableWatched, Report: metrics.SetWatchNamespacesUnreadable}); err != nil {
			setupLog.Error(err, "unable to add the watched-namespace prober to the manager")
			os.Exit(1)
		}
	}

	if err := mgr.Start(signalCtx); err != nil {
		if errors.Is(err, watchns.ErrSetChanged) {
			// Not a failure: the container restarts and the cache is rebuilt
			// from the namespaces that are readable now.
			setupLog.Info("Exiting to rebuild the cache for the watched namespaces that are readable now")
			os.Exit(1)
		}
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// gpuPolicyReady is the "gpu-policy" readiness check: when cluster policy
// declares a physical limiter ALONE, the controller must be able to list nodes.
//
// A physical limiter alone without node read is a controller that cannot scale
// anything up: every variant is charged to no accelerator pool and gets no GPU
// budget. NotReady says so where an admin looks, and has since before the
// limiters composed.
//
// A physical limiter declared BESIDE quota entries does not fail the pod. The
// physical provider is skipped, as any unavailable provider is
// (allocation.ComputeAvailableConstraints), and the quota entries still bound
// every decision; the reason is published instead, as a log line when it
// changes and as wva_constraint_provider_skipped{reason="node_read_denied"}.
// Such a policy ran quota-only before the limiters composed -- the physical
// entry was never built -- so on an install without node read (a
// namespace-scoped one, typically) an upgrade that armed the gate here took the
// pod NotReady, took the external-scaler Service's endpoints with it, and froze
// scaling for every model the controller serves. Losing the physical bound is
// the lesser failure, and it is the state that install was already in. Only a
// definite denial is published as one: before the API server's first answer
// (errNodeReadUnanswered) a composed policy keeps what it last published.
//
// probe asks the API server, rather than reading the flag the optimization loop
// sets. Reading the flag made this a ONE-WAY LATCH, and the loop that would have
// cleared it runs downstream of the very thing going NotReady breaks: no
// endpoints on the external-scaler Service, so KEDA stops calling, so the
// registry empties on its TTL, so the optimizer returns early every cycle and
// never retries node discovery. Granting the RBAC — which is exactly what the
// error below tells an admin to do — would not have recovered it. Only deleting
// the pod would, and a transient Forbidden during RBAC propagation would have
// parked the controller permanently.
func gpuPolicyReady(ctx context.Context, cfg *config.Config, probe func(context.Context) error) error {
	types := cfg.EffectiveLimiterTypes()
	if !types.HasPhysical() {
		composedNodeRead.report(ctx, nil)
		return nil
	}
	err := probe(ctx)
	if len(types) > 1 {
		// No answer yet is not a denial: publishing node_read_denied for it
		// would send an admin after RBAC that may well be granted. Keep what
		// was published, and stay Ready either way.
		if !errors.Is(err, errNodeReadUnanswered) {
			composedNodeRead.report(ctx, err)
		}
		return nil
	}
	composedNodeRead.report(ctx, nil)
	if err != nil {
		return fmt.Errorf("cluster policy declares a gpu-inventory limiter but this controller cannot confirm it may list nodes: %w. "+
			"Every variant would be charged to no accelerator pool, receive no GPU budget and never scale up. "+
			"Grant its ServiceAccount get/list/watch on nodes, or remove the limiter from cluster policy", err)
	}
	return nil
}

// composedNodeReadState publishes, for a physical limiter declared beside
// quota entries, whether the controller may list nodes: the gate does not fail
// the pod for it, so this is where the reason goes. Logged on change only; the
// readiness probe calls it every few seconds.
type composedNodeReadState struct {
	mu      sync.Mutex
	denied  bool
	lastErr string
}

var composedNodeRead composedNodeReadState

// report records the latest node-read outcome for a composed policy: err nil
// clears it (granted, or no composed policy any more).
func (s *composedNodeReadState) report(ctx context.Context, err error) {
	active := map[string]string{}
	if err != nil {
		active[allocation.PhysicalLimiterName] = constants.ProviderSkipNodeReadDenied
	}
	metrics.SetConstraintProvidersSkipped([]string{constants.ProviderSkipNodeReadDenied}, active)

	s.mu.Lock()
	defer s.mu.Unlock()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if (err != nil) == s.denied && msg == s.lastErr {
		return
	}
	logger := ctrl.LoggerFrom(ctx).WithName("gpu-policy")
	switch {
	case err != nil:
		logger.Error(err, "Cluster policy declares gpu-inventory beside quota limiters, but this controller may not list nodes: "+
			"the physical limiter is skipped and the quota limiters still bound scaling. "+
			"Grant the node read (a cluster admin runs make setup-prereqs), or remove gpu-inventory from the limiters: list",
			"provider", allocation.PhysicalLimiterName, "metric", constants.WVAConstraintProviderSkipped)
	case s.denied:
		logger.Info("This controller may list nodes again: the physical limiter bounds scaling beside the quota limiters",
			"provider", allocation.PhysicalLimiterName)
	}
	s.denied, s.lastErr = err != nil, msg
}

// nodeReadProbeCache rate-limits the readiness gate's node check. A readiness
// probe fires every few seconds on every replica; an unlimited List of nodes on
// that schedule is load the API server does not need to carry.
var (
	nodeReadProbeMu   sync.Mutex
	nodeReadProbeAt   time.Time
	nodeReadProbeErr  error
	nodeReadProbeTTL  = 30 * time.Second
	nodeReadProbeOnce bool
)

// nodeReadProbeTimeout bounds the List inside the readiness probe. Shorter than
// the probe's own timeoutSeconds (3s in config/base), so a slow API server yields
// an answer here rather than a probe the kubelet abandons. A var so a test can
// shorten it.
var nodeReadProbeTimeout = 2 * time.Second

// errNodeReadUnanswered is what nodeReadProbe returns, wrapping the List error,
// before the API server has given a definite answer (success, Forbidden or
// Unauthorized). A physical limiter alone is not ready on it; a composed policy
// stays Ready and does not report it as node_read_denied (gpuPolicyReady).
var errNodeReadUnanswered = errors.New("no answer yet from the API server on whether nodes may be listed")

// nodeReadProbe reports whether this controller may list nodes.
//
// It asks the API server rather than trusting a cached verdict from elsewhere in
// the process, so that granting the RBAC recovers readiness on its own. `Limit: 1`
// keeps it cheap: the question is whether the request is authorized, not what the
// nodes contain.
//
// Only an authorization answer -- Forbidden or Unauthorized -- changes the
// verdict. Any other error (a timeout, a 5xx, an API server restarting) says
// nothing about RBAC, and failing readiness on it would take the pod out of the
// external-scaler Service: with one replica that is the only endpoint KEDA can
// reach, so an API-server blip became an actuation outage. Such an error keeps
// the last verdict, Ready or not, and is retried after the TTL.
//
// Until the first definite answer there is no verdict to keep, and the probe
// reports not ready: Ready before anyone has asked whether the controller may
// list nodes would put a controller with no node access into service. Each
// probe asks again until an answer arrives (the TTL applies only to answers).
// This cannot hold up startup: the startup and liveness probes read /healthz,
// not /readyz, so a slow API server delays readiness and kills nothing.
//
// The mutex is held across the List on purpose. Probes that arrive meanwhile
// queue behind it and then read the answer it stored, rather than each sending
// its own List. The wait is bounded by nodeReadProbeTimeout per List.
func nodeReadProbe(ctx context.Context, c client.Reader) error {
	nodeReadProbeMu.Lock()
	defer nodeReadProbeMu.Unlock()
	if nodeReadProbeOnce && time.Since(nodeReadProbeAt) < nodeReadProbeTTL {
		return nodeReadProbeErr
	}
	listCtx, cancel := context.WithTimeout(ctx, nodeReadProbeTimeout)
	defer cancel()
	nodes := &corev1.NodeList{}
	err := c.List(listCtx, nodes, client.Limit(1))
	switch {
	case err == nil, apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		nodeReadProbeAt, nodeReadProbeOnce = time.Now(), true
		nodeReadProbeErr = err
	case !nodeReadProbeOnce:
		ctrl.Log.WithName("setup").Info("node-list readiness probe has had no answer yet; "+
			"not ready until one arrives. No action unless it persists", "err", err.Error())
		return fmt.Errorf("%w: %w", errNodeReadUnanswered, err)
	default:
		nodeReadProbeAt = time.Now()
		ctrl.Log.WithName("setup").Info("node-list readiness probe could not get an answer; "+
			"keeping the previous verdict. No action unless it persists", "err", err.Error())
	}
	return nodeReadProbeErr
}

// cacheNamespaces is the namespace set a namespace-scoped manager caches: the
// namespaces it manages (--watch-namespace, one or a comma-separated list) and
// its own. Its own is where its scaling policy lives, and a policy edit there
// must reach the ConfigMap reconciler without a restart; the controller holds
// the manager Role in its own namespace as in each managed one. The policy
// namespace, when separate, is deliberately not added (see the caller).
func cacheNamespaces(watched []string, controllerNS string) map[string]cache.Config {
	out := make(map[string]cache.Config, len(watched)+1)
	for _, ns := range watched {
		out[ns] = cache.Config{}
	}
	if controllerNS != "" {
		out[controllerNS] = cache.Config{}
	}
	return out
}

// runWarmPool reconciles the warm pools of ONE namespace until ctx ends.
//
// Hoisted out of main so it can be started per namespace. A cluster-scoped
// install has no namespace to derive, and naming one by flag serves exactly
// that namespace: a pool anywhere else is never used, and a pool created after
// startup needs a restart. Both look the same from outside -- Pods running,
// accelerators held, WVA silent about them.
func runWarmPool(
	ctx context.Context,
	mgr ctrl.Manager,
	ds datastore.Datastore,
	warmPoolNS string,
	warmPoolGPUUtil *float64,
	warmPoolSleepMinSize *int,
	warmPoolMaxHold *time.Duration,
	warmPoolMinHold *time.Duration,
	warmPoolPreloadTop *int,
	warmPoolMemoryBudget *int64,
) error {
	// Its own, because the original sat in main() as a local. Same name so the
	// lines this function emits are indistinguishable from before the hoist.
	setupLog := ctrl.Log.WithName("setup")

	trigger := warmpool.NewDecisionTrigger(decision.Default, nil)
	defer trigger.Close()
	// Discovery is call-driven, so the set of scale targets grows during
	// a run; a trigger built once at startup would never fire for a
	// workload registered later.
	go trigger.WatchRegistry(ctx, registry.Default, 30*time.Second)

	reconciler := warmpool.New(
		warmpoolpool.NewAdapter(mgr.GetClient(), warmPoolNS, warmpoolpool.Ram),
		&warmpool.Demand{
			Namespace:            warmPoolNS,
			GPUMemoryUtilization: *warmPoolGPUUtil,
			Registry:             registry.Default,
			Decisions:            decision.Default,
			Client:               mgr.GetClient(),
			Datastore:            ds,
			// SERVING replicas rather than merely Ready ones, for the return
			// rule -- see Demand.Serving. Unknown falls back to the Ready count,
			// which is all a fleet whose collector has not run yet has.
			Serving: func(namespace, variant string) (int, bool) {
				return decision.Serving(namespace, variant, warmpool.ContentionMaxAge, time.Now())
			},
		},
		warmpoolpolicy.Config{
			SleepMinSize:       *warmPoolSleepMinSize,
			MaxHold:            *warmPoolMaxHold,
			MinHold:            *warmPoolMinHold,
			AdmissionWindow:    time.Hour,
			MinMissesToAdmit:   2,
			PreloadTop:         *warmPoolPreloadTop,
			MaxInstancesPerPod: warmpoolpool.MaxInstancesPerPod,
			PodMemoryBytes:     *warmPoolMemoryBudget,
		},
	)
	// Asked before anything is loaded. Without patch on pods the pool
	// admits happily, holds GPUs, and then fails at the only moment
	// that matters -- once per spike, in a path nobody is watching.
	// An UNANSWERABLE question is not a denial: if the review itself
	// fails the pool starts, because refusing to run on a broken API
	// call would be a worse failure than the one being guarded against.
	switch allowed, err := warmpool.CanBorrow(ctx, mgr.GetClient(), warmPoolNS); {
	case err != nil:
		setupLog.Info("could not check whether this controller may patch Pods; "+
			"starting the warm pool anyway. If borrows fail, check RBAC first",
			"namespace", warmPoolNS, "err", err.Error())
	case !allowed:
		setupLog.Error(nil, warmpool.BorrowDenied, "namespace", warmPoolNS)
		return nil
	}

	reconciler.Name = warmPoolNS
	reconciler.Trigger = trigger
	// Pools are DISCOVERED from the Deployments that declare them, so a
	// second pool is a manifest rather than a manifest plus a controller
	// restart. The flags above become the base every pool's annotations
	// are layered onto, and are used whole when nothing declares a pool.
	// The pool is scaled through KEDA like everything else: WVA
	// publishes a size and the pool's own ScaledObject acts on it. An
	// install with no pool ScaledObject simply never reads this, and
	// the pool stays the size its Deployment says.
	reconciler.Namespace = warmPoolNS
	reconciler.PublishSize = decision.Set
	// Arbitration: while the optimizer is denying GPUs to a model
	// replica on this pool's accelerator, the pool stops asking for
	// more. contentionMaxAge keeps a stale reading from holding it
	// down forever if the optimizer stops running.
	// The pool must not ask KEDA for Pods the namespace cannot afford:
	// they would be created and sit Pending, or be refused outright, and
	// the pool would report itself short forever with nothing able to
	// fill it. Published by the allocation layer, which is the only
	// component that sees every constraint provider at once.
	reconciler.Headroom = func(namespace, accelerator string, poolsVersion uint64) (int, decision.HeadroomState) {
		return decision.GPUHeadroomReading(namespace, accelerator, poolsVersion, warmpool.ContentionMaxAge, time.Now())
	}
	// And in whole Pods per node, when a physical limiter bounds the cluster:
	// the GPUs a namespace may take are no use in holes smaller than a Pod.
	reconciler.NodeUnits = func(accelerator string, unitGPUs int, poolsVersion uint64) (int, bool) {
		return decision.GPUHeadroomUnits(accelerator, unitGPUs, poolsVersion, warmpool.ContentionMaxAge, time.Now())
	}
	// Which model the optimizer says should be awake in a pool. Nothing
	// publishes one yet -- the rule that decides is the open half of this
	// design -- so today it is always absent and every pool decides from demand
	// exactly as before. Wired now so the transport is one change rather than
	// two, and so the pool half can be exercised end to end.
	reconciler.WantAwake = func(namespace, pool string) (string, bool) {
		return decision.Awake(namespace, pool, warmpool.ContentionMaxAge, time.Now())
	}
	// How hard each variant is being pushed, for a RETAINED pool choosing which
	// of its resident models holds the GPUs. Published by the optimizer, which
	// is the only component that measures demand against capacity; a pool
	// without it never switches on its own.
	reconciler.Pressure = func(namespace, variant string) (decision.Pressure, bool) {
		return decision.PressureFor(namespace, variant, warmpool.ContentionMaxAge, time.Now())
	}
	reconciler.Contended = func(namespace, accelerator string) bool {
		return decision.GPUContended(namespace, accelerator, warmpool.ContentionMaxAge, time.Now())
	}
	// Pools are DECLARED by their ScaledObjects, like everything else
	// WVA knows about. The flags above are the base each pool's trigger
	// is layered onto, and the whole config for an install that
	// declares no pool at all.
	reconciler.Pools = &warmpool.RegistryPools{
		Snapshot:  registry.Default.Snapshot,
		Namespace: warmPoolNS,
		Fallback:  reconciler.Config,
	}
	// Listed only to REPORT a pool Deployment nothing declares. It
	// holds GPUs and WVA will not use it, which is exactly the silence
	// moving the declaration to the ScaledObject could have introduced.
	reconciler.Undeclared = (&warmpool.UndeclaredPools{
		Client:    mgr.GetClient(),
		Namespace: warmPoolNS,
	}).Find
	// Read, never written: a pool's shape lets the utilization-share
	// optimizer keep GPUs free for a pool none of whose Pods runs yet.
	reconciler.Shape = (&warmpool.PoolShapes{Client: mgr.GetClient()}).Read
	setupLog.Info("warm pool enabled",
		"namespace", warmPoolNS,
		"sleepMinSize", *warmPoolSleepMinSize,
		"maxHold", *warmPoolMaxHold,
		"minHold", *warmPoolMinHold,
		"memoryBudgetBytes", *warmPoolMemoryBudget,
		"gpuMemoryUtilization", *warmPoolGPUUtil,
		"preloadTop", *warmPoolPreloadTop)
	return reconciler.Start(ctx)
}

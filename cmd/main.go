/*
Copyright 2026.

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
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/api"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/k8sevents"
	"github.com/Bermos/Kitchen/internal/receiver"
	"github.com/Bermos/Kitchen/internal/ui"
	"github.com/Bermos/Kitchen/internal/usage"
	"github.com/Bermos/Kitchen/internal/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kitchenv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	// ReferenceGrant is v1beta1: it is what lets a protected preview's route
	// point at the forward-auth gate in another namespace.
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var gitWebhookAddr string
	var previewGateImage string
	var qualityGateImage string
	var previewGateServiceAccount string
	var apiAddr string
	var apiAudiences string
	var uiClientID string
	var selfUpdate controller.SelfUpdateConfig
	var kedaInstall controller.KedaInstallConfig
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&gitWebhookAddr, "git-webhook-bind-address", ":8090",
		"The address the git webhook receiver binds to.")
	flag.StringVar(&qualityGateImage, "quality-gate-image", "",
		"Image the publisher that carries a quality gate's findings out of its pod runs, and the same "+
			"image the continuous re-evaluation pass runs its two halves from. It is this "+
			"operator's own image — all three are further binaries in it — and a pod cannot read its own "+
			"image back, so the chart passes it in. Without it, configured gates never run and nothing "+
			"is rescanned.")
	flag.StringVar(&previewGateImage, "preview-gate-image", "",
		"Image the forward-auth gate for protected previews runs. It is this operator's own image — "+
			"the gate is a second binary in it — and a pod cannot read its own image back, so the chart passes it in. "+
			"Without it, previews that ask to be protected get no route at all.")
	flag.StringVar(&previewGateServiceAccount, "preview-gate-service-account", "",
		"ServiceAccount the forward-auth gate runs as. It is separate from the manager's, and bound to a "+
			"read-only role on projects and kitchens, because the gate resolves who is on a project itself "+
			"rather than asking the REST API. The chart creates it and passes the name in, since only the "+
			"chart knows its release-name prefix. Without it the gate reads nothing and refuses every "+
			"protected preview.")
	flag.StringVar(&apiAddr, "api-bind-address", ":8092",
		"The address the REST API binds to.")
	flag.StringVar(&apiAudiences, "api-audiences", "",
		"Comma-separated token audiences the API accepts on top of the identity provider's issuer "+
			"and the API's own external URL, both of which come from the Kitchen object.")
	flag.StringVar(&uiClientID, "ui-client-id", "kitchen-ui",
		"OAuth client id the dashboard signs in as. The chart seeds the identity provider "+
			"with the same id, so this only changes for installations that renamed that client.")
	// Self-update. These are flags rather than fields on the Kitchen
	// singleton because the singleton is a post-install hook and is not
	// re-applied on upgrade, so a chart value flipped in a `helm upgrade`
	// would never reach the operator; the Deployment is re-applied every
	// time. An empty chart reference disables the feature outright.
	flag.StringVar(&selfUpdate.Chart, "self-update-chart", "",
		"Chart reference the platform upgrades itself from, e.g. oci://ghcr.io/bermos/charts/kitchen. "+
			"Empty disables self-update, which is the default: the upgrade runs as a job bound to cluster-admin, "+
			"so the chart only passes this when selfUpdate.enabled is set.")
	flag.StringVar(&selfUpdate.Release, "self-update-release", "kitchen",
		"Helm release name the self-update upgrades, which is whatever this installation was installed as.")
	flag.StringVar(&selfUpdate.ServiceAccount, "self-update-service-account", "",
		"ServiceAccount the self-update job runs as. It is separate from the manager's, and bound to "+
			"cluster-admin, because a helm upgrade of this chart applies CRDs, ClusterRoles and the namespace.")
	flag.StringVar(&selfUpdate.HelmImage, "self-update-image", controller.DefaultHelmImage,
		"Image the self-update job runs helm from.")
	flag.DurationVar(&selfUpdate.Timeout, "self-update-timeout", controller.DefaultSelfUpdateTimeout,
		"How long helm is given to complete a self-update. It waits for the whole release, StatefulSets included.")
	flag.BoolVar(&selfUpdate.AllowMinor, "self-update-allow-minor", false,
		"Allow a self-update that crosses a minor version. While Kitchen is pre-1.0 the minor is where breaking "+
			"changes land, so those upgrades are opted into separately.")
	// Installing the platform's own scale-to-zero dependencies. Flags for the
	// same reason self-update's are: the account is the chart's to create, so
	// the chart is what says whether the operator may use one. Whether it
	// does is spec.scaleToZero.install on the singleton.
	flag.StringVar(&kedaInstall.ServiceAccount, "keda-install-service-account", "",
		"ServiceAccount the KEDA install job runs as. It is separate from the manager's, and bound to "+
			"cluster-admin, because installing KEDA applies CRDs, ClusterRoles and a namespace. Empty means "+
			"this installation cannot install KEDA for itself, which is the default.")
	flag.StringVar(&kedaInstall.HelmImage, "keda-install-image", controller.DefaultHelmImage,
		"Image the KEDA install job runs helm from.")
	flag.StringVar(&kedaInstall.Repository, "keda-chart-repository", controller.DefaultKedaChartRepository,
		"Helm repository the KEDA charts are pulled from. Point it at a mirror for a cluster that cannot "+
			"reach kedacore.github.io.")
	flag.StringVar(&kedaInstall.ChartVersion, "keda-chart-version", controller.DefaultKedaChartVersion,
		"Version of the KEDA chart the platform installs. Pinned with the add-on's, not floated.")
	flag.StringVar(&kedaInstall.AddOnChartVersion, "keda-http-chart-version", controller.DefaultKedaHTTPChartVersion,
		"Version of the KEDA HTTP add-on chart the platform installs. It decides the interceptor's Service "+
			"name and port, which spec.scaleToZero.interceptor defaults to.")
	flag.DurationVar(&kedaInstall.Timeout, "keda-install-timeout", controller.DefaultKedaInstallTimeout,
		"How long helm is given for each of the two installs. Both wait for their workloads to be ready.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// First line in the log, so a bug report says which release it came from.
	setupLog.Info("starting kitchen", "version", version.Version)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
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

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cc18c917.bermos.dev",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// The only thing that reads events through the cache is the
				// Warning recorder, and a Normal event is the cluster
				// narrating itself, so the informer is filtered at the API
				// server rather than in Go: what it holds is then what
				// somebody will actually ask about. The component survey is
				// unaffected — it reads events through the uncached reader,
				// because field selectors are not served by the cache.
				&corev1.Event{}: {Field: fields.OneTermEqualSelector("type", corev1.EventTypeWarning)},
			},
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "kitchen-system"
	}

	// The activity recorder feeds the dashboard's recent-activity feed.
	// It is shared by the reconcilers and the API, writes into the telemetry
	// store best-effort, and costs nothing on installations without one.
	recorder := &activity.Recorder{
		Client:    mgr.GetClient(),
		Namespace: controller.PlatformNamespace,
		Singleton: controller.KitchenSingletonName,
	}

	// One audit recorder for the whole process, shared by every reconciler
	// and by the REST API. It has to be one: the chain's next hash is a
	// function of its last, so two of them would produce two chains — which
	// is also why the chart refuses a second replica while the audit log is
	// on (see internal/audit).
	auditor := &audit.Recorder{
		Client: mgr.GetClient(),
		// The chain's head is read back on the very next append, so it is
		// read straight from the API server: a cached read would hand back
		// the version before the last write and every append after the first
		// would conflict with itself.
		Reader:    mgr.GetAPIReader(),
		Namespace: controller.PlatformNamespace,
		Singleton: controller.KitchenSingletonName,
	}

	if err = (&controller.KitchenReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		PreviewGateImage:          previewGateImage,
		PreviewGateServiceAccount: previewGateServiceAccount,
		KedaInstall:               kedaInstall,
		Audit:                     auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Kitchen")
		os.Exit(1)
	}
	if err = (&controller.ConnectionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Audit:  auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Connection")
		os.Exit(1)
	}
	if err = (&controller.ProjectReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Audit:  auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Project")
		os.Exit(1)
	}
	if err = (&controller.BuildReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Activity:         recorder,
		Audit:            auditor,
		QualityGateImage: qualityGateImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Build")
		os.Exit(1)
	}
	if err = (&controller.ReleaseReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Activity: recorder,
		Audit:    auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Release")
		os.Exit(1)
	}
	if err = (&controller.EnvironmentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Activity: recorder,
		Audit:    auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Environment")
		os.Exit(1)
	}
	if err = (&controller.PromotionReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Activity: recorder,
		Audit:    auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Promotion")
		os.Exit(1)
	}
	if err = (&controller.ExceptionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Audit:  auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Exception")
		os.Exit(1)
	}
	if err = (&controller.DomainReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Audit:  auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Domain")
		os.Exit(1)
	}
	if err = (&controller.ResourceClaimReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Activity: recorder,
		Audit:    auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceClaim")
		os.Exit(1)
	}
	if err = (&controller.PlatformUpdateReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		SelfUpdate:     selfUpdate,
		CurrentVersion: version.Version,
		Activity:       recorder,
		Audit:          auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PlatformUpdate")
		os.Exit(1)
	}
	setupLog.Info("self-update", "enabled", selfUpdate.Enabled(),
		"chart", selfUpdate.Chart, "allowMinor", selfUpdate.AllowMinor)
	setupLog.Info("keda-install", "permitted", kedaInstall.Enabled(),
		"keda", kedaInstall.ChartVersion, "httpAddOn", kedaInstall.AddOnChartVersion)
	// +kubebuilder:scaffold:builder

	if err := mgr.Add(&receiver.GitWebhookReceiver{
		Client:    mgr.GetClient(),
		Namespace: operatorNamespace,
		BindAddr:  gitWebhookAddr,
	}); err != nil {
		setupLog.Error(err, "unable to add git webhook receiver to manager")
		os.Exit(1)
	}

	// The flow collector follows Hubble Relay and ships flow observations
	// into the telemetry store — the traffic view's data, and one row per
	// request the platform's edge served. It idles until the Kitchen object
	// names a relay address, so it can be added unconditionally.
	//
	// It is built here rather than inline in the Add below because the API
	// holds on to it: the follower is the only thing that sees Relay's
	// LostEvent notices, so its loss ledger is the only evidence the platform
	// has that a request count under-reports.
	flowCollector := &flows.Collector{
		Client: mgr.GetClient(),
	}
	if err := mgr.Add(flowCollector); err != nil {
		setupLog.Error(err, "unable to add the flow collector to manager")
		os.Exit(1)
	}

	// The REST API. It authenticates every request against the platform's
	// identity provider, which it resolves from the Kitchen object at
	// request time — so it can be added here without waiting for the
	// platform to be configured. The dashboard rides on the same server:
	// static files outside /api/, resolved against the same Kitchen object.
	if err := mgr.Add(&api.Server{
		Client: mgr.GetClient(),
		// Pods and nodes are read through the uncached reader: the
		// introspection endpoints are the only thing that asks for them, and
		// caching them would mean watching every pod in the cluster.
		APIReader:      mgr.GetAPIReader(),
		Namespace:      operatorNamespace,
		BindAddr:       apiAddr,
		ExtraAudiences: splitList(apiAudiences),
		UI:             ui.Handler(api.UIConfig(mgr.GetClient(), uiClientID)),
		Activity:       recorder,
		Audit:          auditor,
		Version:        version.Version,
		SelfUpdate:     selfUpdate,
		// The follower's own accounting of what Hubble told it it lost, which
		// is what the ingest signal and the ingest screen are made of. Note
		// that the counts are the *local* replica's: the follower runs on the
		// leader alone, so a replica that is not the leader reports no loss
		// because it did no following — which is why the screen says which
		// window the counts cover rather than presenting them as a total.
		Flows: flowCollector,
	}); err != nil {
		setupLog.Error(err, "unable to add the api server to manager")
		os.Exit(1)
	}

	// The usage collector samples what only the API server knows about the
	// platform's workloads — restarts, OOM kills, limits, replica counts — and
	// exports it to the node collector over OTLP, where it lands beside the
	// CPU and memory the kubelet reports. It idles until the Kitchen object
	// names an endpoint, so it too is added unconditionally.
	if err := mgr.Add(&usage.Collector{
		Client: mgr.GetClient(),
		// Pods are read uncached for the same reason the API's introspection
		// endpoints read them uncached: a warm informer over every pod in the
		// cluster is a high price for a question asked on a timer.
		Reader: mgr.GetAPIReader(),
	}); err != nil {
		setupLog.Error(err, "unable to add the usage collector to manager")
		os.Exit(1)
	}

	// The continuous re-evaluation pass: every currently-deployed release,
	// matched against a current vulnerability database on an interval and
	// re-judged against its environment's own bar. It is leader-elected and
	// idles until the Kitchen object turns it on and names a scanner, so it
	// is added unconditionally like the collectors above.
	if err := mgr.Add(&controller.RescanSweeper{
		Client:        mgr.GetClient(),
		Audit:         auditor,
		Activity:      recorder,
		OperatorImage: qualityGateImage,
	}); err != nil {
		setupLog.Error(err, "unable to add the rescan sweep to manager")
		os.Exit(1)
	}

	// The event recorder keeps the cluster's Warning events, which the API
	// server expires about an hour after they happen — turning the operator's
	// existing watch into the history the Events screen and the crash report
	// read. It idles until the Kitchen object names a store, so it too is
	// added unconditionally.
	if err := mgr.Add(&k8sevents.Recorder{
		// The cached client, unlike the two collectors above: the recorder's
		// watch is the event informer, and it resolves an event's project by
		// reading the object the event is about rather than on a timer.
		Client: mgr.GetClient(),
	}); err != nil {
		setupLog.Error(err, "unable to add the event recorder to manager")
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
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// splitList reads a comma-separated flag, ignoring the empty entries a
// generated command line tends to produce.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	mapi "github.com/openshift/api/machine/v1beta1"
	mcfg "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	operators "github.com/operator-framework/api/pkg/operators/v2"
	"github.com/operator-framework/operator-lib/leader"
	"github.com/spf13/pflag"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
	//+kubebuilder:scaffold:imports
)

// needed to run on the hostnetwork
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=hostnetwork,verbs=use

// Pod permissions used to get OwnerReference corresponding to the current pod. This is required to ensure that
// the operator pod is the leader in the given namespace. This will not be required if the leader election is done
// by the manager, instead of the "leader" library
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mapi.AddToScheme(scheme))
	utilruntime.Must(operators.AddToScheme(scheme))
	utilruntime.Must(mcfg.Install(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var debugLogging bool

	flag.BoolVar(&debugLogging, "debugLogging", false, "Log debug messages")

	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Parse()

	opts := zap.Options{Development: debugLogging, TimeEncoder: zapcore.RFC3339TimeEncoder}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// add version subcommand to query the operator version
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Printf("%s version: %q, go version: %q\n", os.Args[0], version.Get(),
				version.GoVersion)
			os.Exit(0)
		default:
			fg := strings.Split(os.Args[1], "=")
			arg := strings.Replace(fg[0], "--", "", -1)
			if pflag.Lookup(arg) == nil {
				fmt.Printf("unknown sub-command: %v\n", os.Args[1])
				fmt.Print("available sub-commands:\n\tversion\n")
				os.Exit(1)
			}
		}
	}

	version.Print()

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		setupLog.Error(err, "failed to get the config for talking to a Kubernetes API server")
		os.Exit(1)
	}

	// get cluster configuration
	clusterConfig, err := cluster.NewConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to get cluster configuration")
		os.Exit(1)
	}

	// validate cluster for required configurations
	if err := clusterConfig.Validate(); err != nil {
		setupLog.Error(err, "failed to validate required cluster configuration")
		os.Exit(1)
	}

	// Checking if required files exist before starting the operator
	requiredFiles := []string{
		payload.HostLocalCNIPlugin,
		payload.WinBridgeCNIPlugin,
		payload.WinOverlayCNIPlugin,
		payload.HybridOverlayPath,
		payload.KubeletPath,
		payload.KubeProxyPath,
		payload.KubeLogRunnerPath,
		payload.GcpGetValidHostnameScriptPath,
		payload.WICDPath,
		payload.HNSPSModule,
		payload.WindowsExporterPath,
		payload.AzureCloudNodeManagerPath,
	}
	if err := checkIfRequiredFilesExist(requiredFiles); err != nil {
		setupLog.Error(err, "could not start the operator")
		os.Exit(1)
	}

	if err := payload.PopulateNetworkConfScript(clusterConfig.Network().GetServiceCIDR(), windows.OVNKubeOverlayNetwork,
		windows.HNSPSModule, windows.CniConfDir+"\\cni.conf"); err != nil {
		setupLog.Error(err, "unable to generate CNI config script")
		os.Exit(1)
	}

	ctx := context.TODO()
	// Become the leader before proceeding
	err = leader.Become(ctx, "windows-machine-config-operator-lock")
	if err != nil {
		setupLog.Error(err, "failed to become a leader within current namespace")
		os.Exit(1)
	}

	// Create a new Manager to provide shared dependencies and start components
	// TODO: https://issues.redhat.com/browse/WINC-599
	//       The NewCache field is not being set, as the default is a cluster wide scope, which is what we want
	//       as we need to watch Nodes. A MultiNamespacedCache cannot be used at this point as it has issues working
	//       with cluster scoped resources. Once those issues are resolved, it may be worth switching to using that
	//       cache type.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: fmt.Sprintf("%s:%d", metrics.Host, metrics.Port),
		Port:               9443,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Get the watched namespace. This is originally sourced from from the OperatorGroup associated with the CSV.
	// Because the WMCO CSV only supports the OwnNamespace InstallMode, the watch namespace will always be the namespace
	// that WMCO is deployed in.
	watchNamespace, err := getWatchNamespace()
	if err != nil {
		setupLog.Error(err, "failed to get watch namespace")
		os.Exit(1)
	}
	// This is a defensive check to ensure that the WMCO CSV was not changed from only supporting OwnNamespace only.
	// Check that the OperatorGroup + CSV were not deployed with a cluster scope (namespace = ""), and that the
	// OperatorGroup does not target multiple namespaces. This should not be able to happen as both `AllNamespaces` and
	// `MultiNamespaces` are not supported InstallModes.
	if watchNamespace == "" || strings.Contains(watchNamespace, ",") {
		setupLog.Error(err, "WMCO has an invalid target namespace. "+
			"OperatorGroup target namespace must be a single, non-cluster-scoped value", "target namespace",
			watchNamespace)
		os.Exit(1)
	}

	// Setup all Controllers
	winMachineReconciler, err := controllers.NewWindowsMachineReconciler(mgr, clusterConfig, watchNamespace)
	if err != nil {
		setupLog.Error(err, "unable to create Windows Machine reconciler")
		os.Exit(1)
	}
	if err = winMachineReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Windows Machine controller")
		os.Exit(1)
	}

	nodeReconciler, err := controllers.NewNodeReconciler(mgr, clusterConfig, watchNamespace)
	if err != nil {
		setupLog.Error(err, "unable to create Node reconciler")
		os.Exit(1)
	}
	if err = nodeReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Node controller")
		os.Exit(1)
	}

	secretReconciler := controllers.NewSecretReconciler(mgr, clusterConfig.Platform(), watchNamespace)
	if err = secretReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Secret controller")
		os.Exit(1)
	}
	if err := secretReconciler.RemoveInvalidAnnotationsFromLinuxNodes(mgr.GetConfig()); err != nil {
		setupLog.Error(err, "error removing invalid annotations from Linux nodes")
	}

	proxyEnabled := cluster.IsProxyEnabled()
	configMapReconciler, err := controllers.NewConfigMapReconciler(mgr, clusterConfig, watchNamespace, proxyEnabled)
	if err != nil {
		setupLog.Error(err, "unable to create ConfigMap reconciler")
		os.Exit(1)
	}
	if err = configMapReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ConfigMap")
		os.Exit(1)
	}

	certificateSigningRequestsReconciler, err := controllers.NewCertificateSigningRequestsReconciler(mgr, clusterConfig,
		watchNamespace)
	if err != nil {
		setupLog.Error(err, "unable to create CSR reconciler")
		os.Exit(1)
	}
	if err = certificateSigningRequestsReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CertificateSigningRequests")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder
	// The above marker tells kubebuilder that this is where the SetupWithManager function should be inserted when new
	// controllers are generated by Operator SDK.

	metricsConfig, err := metrics.NewConfig(mgr, cfg, watchNamespace)
	if err != nil {
		setupLog.Error(err, "failed to create MetricsConfig object")
		os.Exit(1)
	}

	// Configure the metric resources
	if err := metricsConfig.Configure(ctx); err != nil {
		setupLog.Error(err, "error setting up metrics")
		os.Exit(1)
	}

	// Create the singleton Windows services ConfigMap
	if err := configMapReconciler.EnsureServicesConfigMapExists(); err != nil {
		setupLog.Error(err, "error ensuring object exists", "singleton", types.NamespacedName{Namespace: watchNamespace,
			Name: servicescm.Name})
		os.Exit(1)
	}

	if err := configMapReconciler.EnsureWICDRBAC(); err != nil {
		setupLog.Error(err, "error ensuring WICD RBAC resources exist", "namespace", watchNamespace)
		os.Exit(1)
	}

	// If proxy is enabled, disabled, or edited during WMCO runtime, the WMCO pod will be restarted by OLM. This could
	// happen in the middle of node configuration, at which the controllers will reconcile once the WMCO pod restarts
	if proxyEnabled {
		if err := configMapReconciler.EnsureTrustedCAConfigMapExists(); err != nil {
			setupLog.Error(err, "error ensuring trusted CA ConfigMap exists", "namespace", watchNamespace)
			os.Exit(1)
		}
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")

		os.Exit(1)
	}
}

// checkIfRequiredFilesExist checks for the existence of required files and binaries before starting WMCO
// sample error message: errors encountered with required files: could not stat /payload/hybrid-overlay-node.exe:
// stat /payload/hybrid-overlay-node.exe: no such file or directory
func checkIfRequiredFilesExist(requiredFiles []string) error {
	var errorMessages []string
	// Iterating through file paths and checking if they are present
	for _, file := range requiredFiles {
		if _, err := os.Stat(file); err != nil {
			errorMessages = append(errorMessages, fmt.Sprintf("could not stat %s: %v", file, err))
		}
	}

	if len(errorMessages) > 0 {
		return fmt.Errorf("errors encountered with required files: %s", strings.Join(errorMessages, ", "))
	}
	return nil
}

// getWatchNamespace returns the Namespace the operator should be watching for changes
// An empty value means the operator is running with cluster scope.
func getWatchNamespace() (string, error) {
	var watchNamespaceEnvVar = "WATCH_NAMESPACE"

	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", watchNamespaceEnvVar)
	}
	return ns, nil
}

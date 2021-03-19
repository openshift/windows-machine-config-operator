package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	"github.com/operator-framework/operator-sdk/pkg/log/zap"
	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/openshift/windows-machine-config-operator/apis"
	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/version"
)

var log = logf.Log.WithName("cmd")

func main() {
	// Add the zap logger flag set to the CLI. The flag set must
	// be added before calling pflag.Parse().
	pflag.CommandLine.AddFlagSet(zap.FlagSet())

	// Add flags registered by imported packages (e.g. glog and
	// controller-runtime)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	pflag.Parse()

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

	// Use a zap logr.Logger implementation. If none of the zap
	// flags are configured (or if the zap flag set is not being
	// used), this defaults to a production zap logger.
	//
	// The logger instantiated here can be changed to any logger
	// implementing the logr.Logger interface. This logger will
	// be propagated through the whole operator, generating
	// uniform and structured logs.
	logf.SetLogger(zap.Logger())

	version.Print()

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "failed to get the config for talking to a Kubernetes API server")
		os.Exit(1)
	}

	// get cluster configuration
	clusterConfig, err := cluster.NewConfig(cfg)
	if err != nil {
		log.Error(err, "failed to get cluster configuration")
		os.Exit(1)
	}

	// validate cluster for required configurations
	if err := clusterConfig.Validate(); err != nil {
		log.Error(err, "failed to validate required cluster configuration")
		os.Exit(1)
	}

	// Checking if required files exist before starting the operator
	requiredFiles := []string{
		payload.FlannelCNIPluginPath,
		payload.HostLocalCNIPlugin,
		payload.WinBridgeCNIPlugin,
		payload.WinOverlayCNIPlugin,
		payload.HybridOverlayPath,
		payload.KubeletPath,
		payload.KubeProxyPath,
		payload.IgnoreWgetPowerShellPath,
		payload.WmcbPath,
		payload.CNIConfigTemplatePath,
		payload.HNSPSModule,
		payload.WindowsExporterPath,
	}
	if err := checkIfRequiredFilesExist(requiredFiles); err != nil {
		log.Error(err, "could not start the operator")
		os.Exit(1)
	}

	// The watched namespace defined by the WMCO CSV
	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Error(err, "failed to get watch namespace")
		os.Exit(1)
	}

	ctx := context.TODO()
	// Become the leader before proceeding
	err = leader.Become(ctx, "windows-machine-config-operator-lock")
	if err != nil {
		log.Error(err, "failed to become a leader within current namespace")
		os.Exit(1)
	}

	// Allow for the watching of cluster-wide resources with "", so that we can watch nodes,
	// as well as resources within the `openshift-machine-api` and WMCO namespace
	namespaces := []string{"", "openshift-machine-api", namespace}
	// Create a new Cmd to provide shared dependencies and start components
	mgr, err := manager.New(cfg, manager.Options{
		NewCache:           cache.MultiNamespacedCacheBuilder(namespaces),
		MetricsBindAddress: fmt.Sprintf("%s:%d", metrics.Host, metrics.Port),
	})
	if err != nil {
		log.Error(err, "failed to create a new Manager")
		os.Exit(1)
	}

	log.Info("registering Components.")

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "failed to add all Resources to the Scheme")
		os.Exit(1)
	}

	// Setup all Controllers
	if err := controller.AddToManager(mgr, clusterConfig, namespace); err != nil {
		log.Error(err, "failed to add all Controllers to the Manager")
		os.Exit(1)
	}

	metricsConfig, err := metrics.NewConfig(mgr, cfg, namespace)
	if err != nil {
		log.Error(err, "failed to create MetricsConfig object")
		os.Exit(1)
	}

	// Best effort to delete stale metric resources from previous operator version.
	// Logs and creates events if stale resource deletion fails.
	metricsConfig.RemoveStaleResources(ctx)

	// Configure the metric resources
	if err := metricsConfig.Configure(ctx); err != nil {
		log.Error(err, "error setting up metrics")
		os.Exit(1)
	}

	log.Info("starting the Cmd.")

	// Start the Cmd
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited non-zero")
		os.Exit(1)
	}
}

// checkIfRequiredFilesExist checks for the existence of required files and binaries before starting WMCO
// sample error message: errors encountered with required files: could not stat /payload/hybrid-overlay-node.exe:
// stat /payload/hybrid-overlay-node.exe: no such file or directory, could not stat /payload/wmcb.exe: stat /payload/wmcb.exe:
// no such file or directory
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

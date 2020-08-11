package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/windows-machine-config-operator/pkg/apis"
	"github.com/openshift/windows-machine-config-operator/pkg/clusternetwork"
	"github.com/openshift/windows-machine-config-operator/pkg/controller"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/version"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	kubemetrics "github.com/operator-framework/operator-sdk/pkg/kube-metrics"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	"github.com/operator-framework/operator-sdk/pkg/log/zap"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost               = "0.0.0.0"
	metricsPort         int32 = 8383
	operatorMetricsPort int32 = 8686
)
var log = logf.Log.WithName("cmd")

const (
	// baseK8sVersion specifies the base k8s version supported by the operator. (For eg. All versions in the format
	// 1.19.x are supported for baseK8sVersion 1.18)
	baseK8sVersion = "1.19"
)

// clusterConfig contains information specific to cluster configuration
type clusterConfig struct {
	// oclient is the OpenShift config client, we will use to interact with the OpenShift API
	oclient configclient.Interface
	// operatorClient is the OpenShift operator client, we will use to interact with OpenShift operator objects
	operatorClient operatorv1.OperatorV1Interface
	// network is the interface containing information on cluster network
	network clusternetwork.ClusterNetworkConfig
}

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
	clusterconfig, err := newClusterConfig(cfg)
	if err != nil {
		log.Error(err, "failed to get cluster configuration")
		os.Exit(1)
	}

	// validate cluster for required configurations
	if err := clusterconfig.validate(); err != nil {
		log.Error(err, "failed to validate required cluster configuration")
		os.Exit(1)
	}

	// Checking if required files exist before starting the operator
	requiredFiles := []string{
		wkl.FlannelCNIPluginPath,
		wkl.HostLocalCNIPlugin,
		wkl.WinBridgeCNIPlugin,
		wkl.WinOverlayCNIPlugin,
		wkl.HybridOverlayPath,
		wkl.KubeletPath,
		wkl.KubeProxyPath,
		wkl.IgnoreWgetPowerShellPath,
		wkl.WmcbPath,
		wkl.CNIConfigTemplatePath,
		wkl.HNSPSModule,
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
		MetricsBindAddress: fmt.Sprintf("%s:%d", metricsHost, metricsPort),
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
	if err := controller.AddToManager(mgr, clusterconfig.network, namespace); err != nil {
		log.Error(err, "failed to add all Controllers to the Manager")
		os.Exit(1)
	}

	// Add the Metrics Service
	addMetrics(ctx, cfg, namespace)

	log.Info("starting the Cmd.")

	// Start the Cmd
	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited non-zero")
		os.Exit(1)
	}
}

// addMetrics will create the Services and Service Monitors to allow the operator export the metrics by using
// the Prometheus operator
func addMetrics(ctx context.Context, cfg *rest.Config, namespace string) {
	if err := serveCRMetrics(cfg); err != nil {
		if errors.Cause(err) == k8sutil.ErrRunLocal {
			log.Info("skipping CR metrics server creation; not running in a cluster.")
			return
		}
		log.Info("could not generate and serve custom resource metrics", "error", err.Error())
	}

	// Add to the below struct any other metrics ports you want to expose.
	servicePorts := []v1.ServicePort{
		{Port: metricsPort, Name: metrics.OperatorPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: metricsPort}},
		{Port: operatorMetricsPort, Name: metrics.CRPortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: operatorMetricsPort}},
	}

	// Create Service object to expose the metrics port(s).
	service, err := metrics.CreateMetricsService(ctx, cfg, servicePorts)
	if err != nil {
		log.Info("could not create metrics Service", "error", err.Error())
	}

	// CreateServiceMonitors will automatically create the prometheus-operator ServiceMonitor resources
	// necessary to configure Prometheus to scrape metrics from this operator.
	services := []*v1.Service{service}
	_, err = metrics.CreateServiceMonitors(cfg, namespace, services)
	if err != nil {
		log.Info("could not create ServiceMonitor object", "error", err.Error())
		// If this operator is deployed to a cluster without the prometheus-operator running, it will return
		// ErrServiceMonitorNotPresent, which can be used to safely skip ServiceMonitor creation.
		if err == metrics.ErrServiceMonitorNotPresent {
			log.Info("install prometheus-operator in your cluster to create ServiceMonitor objects", "error", err.Error())
		}
	}
}

// serveCRMetrics gets the Operator/CustomResource GVKs and generates metrics based on those types.
// It serves those metrics on "http://metricsHost:operatorMetricsPort".
func serveCRMetrics(cfg *rest.Config) error {
	// Below function returns filtered operator/CustomResource specific GVKs.
	// For more control override the below GVK list with your own custom logic.
	filteredGVK, err := k8sutil.GetGVKsFromAddToScheme(apis.AddToScheme)
	if err != nil {
		return err
	}
	// Get the namespace the operator is currently deployed in.
	operatorNs, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		return err
	}
	// To generate metrics in other namespaces, add the values below.
	ns := []string{operatorNs}
	// Generate and serve custom resource specific metrics.
	err = kubemetrics.GenerateAndServeCRMetrics(cfg, ns, filteredGVK, metricsHost, operatorMetricsPort)
	if err != nil {
		return err
	}
	return nil
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

// newClusterConfig creates clusterConfig struct that holds information of the cluster configurations
func newClusterConfig(config *rest.Config) (*clusterConfig, error) {
	// get OpenShift API config client.
	oclient, err := configclient.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "could not create config clientset")
	}

	// get OpenShift API operator client
	operatorClient, err := operatorv1.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "could not create operator clientset")
	}

	// get cluster network configurations
	network, err := clusternetwork.NetworkConfigurationFactory(oclient, operatorClient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting cluster network")
	}

	return &clusterConfig{
		oclient:        oclient,
		operatorClient: operatorClient,
		network:        network,
	}, nil
}

// validateK8sVersion checks for valid k8s version in the cluster. It returns an error for all versions not equal
// to supported major version. This is being done this way, and not by directly getting the cluster version, as OpenShift CI
// returns version in the format 0.0.x and not the actual version attached to its clusters.
func (c *clusterConfig) validateK8sVersion() error {
	versionInfo, err := c.oclient.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrap(err, "error retrieving server version ")
	}
	// split the version in the form Major.Minor. For e.g v1.18.0-rc.1 -> 1.18
	k8sVersion := strings.TrimLeft(versionInfo.GitVersion, "v")
	clusterBaseVersion := strings.Join(strings.SplitN(k8sVersion, ".", 3)[:2], ".")

	if strings.Compare(clusterBaseVersion, baseK8sVersion) != 0 {
		return errors.Errorf("Unsupported server version: v%v. Supported version is v%v.x", k8sVersion,
			baseK8sVersion)
	}
	return nil
}

// validate method checks if the cluster configurations are as required. It throws an error if the configuration could not
// be validated.
func (c *clusterConfig) validate() error {
	err := c.validateK8sVersion()
	if err != nil {
		return errors.Wrap(err, "error validating k8s version")
	}
	if err = c.network.Validate(); err != nil {
		return errors.Wrap(err, "error validating network configuration")
	}
	return nil
}

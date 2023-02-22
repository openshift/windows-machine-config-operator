//go:build windows

/*
Copyright 2022.

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

package controller

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/jsonpath"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/config"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/manager"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/powershell"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
	"github.com/openshift/windows-machine-config-operator/pkg/services"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

// Options contains a list of options available when creating a new ServiceController
type Options struct {
	Config    *rest.Config
	Client    client.Client
	Mgr       manager.Manager
	cmdRunner powershell.CommandRunner
}

// setDefaults returns an Options based on the received options, with all nil or empty fields filled in with reasonable
// defaults.
func setDefaults(o Options) (Options, error) {
	var err error
	if o.Client == nil {
		if o.Config == nil {
			// This instantiates an in-cluster config with all required types present in the scheme
			o.Config, err = rest.InClusterConfig()
			if err != nil {
				return o, err
			}
		}
		// Use a non-caching client
		o.Client, err = NewDirectClient(o.Config)
		if err != nil {
			return o, err
		}
	}
	if o.Mgr == nil {
		o.Mgr, err = manager.New()
		if err != nil {
			return o, err
		}
	}
	if o.cmdRunner == nil {
		o.cmdRunner = powershell.NewCommandRunner()
	}
	return o, nil
}

type ServiceController struct {
	manager.Manager
	client         client.Client
	ctx            context.Context
	nodeName       string
	watchNamespace string
	psCmdRunner    powershell.CommandRunner
}

// Bootstrap starts all Windows services marked as necessary for node bootstrapping as defined in the given data
func (sc *ServiceController) Bootstrap(desiredVersion string) error {
	var cm core.ConfigMap
	err := sc.client.Get(sc.ctx,
		client.ObjectKey{Namespace: sc.watchNamespace, Name: servicescm.NamePrefix + desiredVersion}, &cm)
	if err != nil {
		return err
	}
	cmData, err := servicescm.Parse(cm.Data)
	if err != nil {
		return err
	}
	return sc.reconcileServices(cmData.GetBootstrapServices())
}

// RunController is the entry point of WICD's controller functionality
func RunController(ctx context.Context, apiServerURL, saCA, saToken, watchNamespace string) error {
	cfg, err := config.FromServiceAccount(apiServerURL, saCA, saToken)
	if err != nil {
		klog.Error(err)
		return errors.Wrap(err, "error using service account to build config")
	}
	// This is a client that reads directly from the server, not a cached client. This is required to be used here, as
	// the cached client, created by ctrl.NewManager() will not be functional until the manager is started.
	directClient, err := NewDirectClient(cfg)
	if err != nil {
		return err
	}

	addrs, err := LocalInterfaceAddresses()
	if err != nil {
		return err
	}
	node, err := GetAssociatedNode(directClient, addrs)
	if err != nil {
		return errors.Wrap(err, "could not find node object associated with this instance")
	}

	ctrlMgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Namespace: watchNamespace,
		Scheme:    directClient.Scheme(),
	})
	if err != nil {
		return errors.Wrap(err, "unable to start manager")
	}
	sc, err := NewServiceController(ctx, node.Name, watchNamespace, Options{Client: ctrlMgr.GetClient()})
	if err != nil {
		return err
	}
	if err = sc.SetupWithManager(ctrlMgr); err != nil {
		return err
	}
	klog.Info("Starting manager, awaiting events")
	if err := ctrlMgr.Start(ctx); err != nil {
		return err
	}
	return nil
}

// NewServiceController returns a pointer to a ServiceController object
func NewServiceController(ctx context.Context, nodeName, watchNamespace string, options Options) (*ServiceController, error) {
	o, err := setDefaults(options)
	if err != nil {
		return nil, err
	}
	return &ServiceController{client: o.Client, Manager: o.Mgr, ctx: ctx, nodeName: nodeName, psCmdRunner: o.cmdRunner,
		watchNamespace: watchNamespace}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (sc *ServiceController) SetupWithManager(mgr ctrl.Manager) error {
	nodePredicate := predicate.Funcs{
		// A node's name will never change, so it is fine to use the name for node identification
		// The node must have a desired-version annotation for it to be reconcilable
		CreateFunc: func(e event.CreateEvent) bool {
			return sc.nodeName == e.Object.GetName() && e.Object.GetAnnotations()[nodeconfig.DesiredVersionAnnotation] != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Only process update events if the desired version has changed
			return sc.nodeName == e.ObjectNew.GetName() &&
				e.ObjectOld.GetAnnotations()[nodeconfig.DesiredVersionAnnotation] != e.ObjectNew.GetAnnotations()[nodeconfig.DesiredVersionAnnotation]
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return sc.nodeName == e.Object.GetName() && e.Object.GetAnnotations()[nodeconfig.DesiredVersionAnnotation] != ""
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	cmPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return strings.HasPrefix(object.GetName(), servicescm.NamePrefix)
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Node{}, builder.WithPredicates(nodePredicate)).
		Watches(&source.Kind{Type: &core.ConfigMap{}}, handler.EnqueueRequestsFromMapFunc(sc.mapToCurrentNode),
			builder.WithPredicates(cmPredicate)).
		Complete(sc)
}

// mapToCurrentNode maps all events to the node associated with this Windows instance
func (sc *ServiceController) mapToCurrentNode(_ client.Object) []reconcile.Request {
	return []reconcile.Request{{types.NamespacedName{Name: sc.nodeName}}}
}

// Reconcile fulfills the Reconciler interface
func (sc *ServiceController) Reconcile(_ context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	klog.Infof("reconciling %s", req.NamespacedName)
	var node core.Node
	err := sc.client.Get(sc.ctx, req.NamespacedName, &node)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			klog.Errorf("node %s not found", req.Name)
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	desiredVersion, present := node.Annotations[nodeconfig.DesiredVersionAnnotation]
	if !present {
		// node missing desired version annotation, don't requeue
		return ctrl.Result{}, nil
	}

	// Fetch the CM of the desired version
	var cm core.ConfigMap
	if err := sc.client.Get(sc.ctx,
		client.ObjectKey{Namespace: sc.watchNamespace, Name: servicescm.NamePrefix + desiredVersion}, &cm); err != nil {
		klog.Error(err)
		return ctrl.Result{}, err
	}
	cmData, err := servicescm.Parse(cm.Data)
	if err != nil {
		klog.Error(err)
		return ctrl.Result{}, err
	}

	// Reconcile state of Windows services with the ConfigMap data
	if err = sc.reconcileServices(cmData.Services); err != nil {
		klog.Error(err)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reconcileServices ensures that all the services passed in via the services slice are created, configured properly
// and started
func (sc *ServiceController) reconcileServices(services []servicescm.Service) error {
	existingSvcs, err := sc.GetServices()
	if err != nil {
		return errors.Wrap(err, "could not determine existing Windows services")
	}
	for _, service := range services {
		var winSvcObj winsvc.Service
		if _, present := existingSvcs[service.Name]; !present {
			// create a service placeholder
			winSvcObj, err = sc.CreateService(service.Name, "", mgr.Config{})
			if err != nil {
				return err
			}
			klog.Infof("created service %s", service.Name)
		} else {
			// open the service
			winSvcObj, err = sc.OpenService(service.Name)
			if err != nil {
				return err
			}
			klog.Infof("reconciling existing service %s", service.Name)
		}
		if err := sc.reconcileService(winSvcObj, service); err != nil {
			return err
		}
		klog.Infof("successfully reconciled service %s", service.Name)
	}
	return nil
}

// reconcileService ensures the given service is running and configured according to the expected definition given
func (sc *ServiceController) reconcileService(service winsvc.Service, expected servicescm.Service) error {
	config, err := service.Config()
	if err != nil {
		return err
	}
	cmd, err := sc.expectedServiceCommand(expected)
	if err != nil {
		return err
	}

	updateRequired := false
	if config.BinaryPathName != cmd {
		config.BinaryPathName = cmd
		updateRequired = true
	}

	expectedDescription := fmt.Sprintf("%s %s", windows.ManagedTag, expected.Name)
	if config.Description != expectedDescription {
		config.Description = expectedDescription
		updateRequired = true
	}

	if !slicesEquivalent(config.Dependencies, expected.Dependencies) {
		config.Dependencies = expected.Dependencies
		updateRequired = true
	}

	if updateRequired {
		klog.Infof("updating service %s", expected.Name)
		// Always ensure the service isn't running before updating its config, just to be safe
		if err := sc.EnsureServiceState(service, svc.Stopped); err != nil {
			return err
		}
		err = service.UpdateConfig(config)
		if err != nil {
			return errors.Wrap(err, "error updating service config")
		}
	}
	// always ensure service is started
	return sc.EnsureServiceState(service, svc.Running)
}

// expectedServiceCommand returns the full command that the given service should run with
func (sc *ServiceController) expectedServiceCommand(expected servicescm.Service) (string, error) {
	var nodeVars, psVars map[string]string
	var err error
	if len(expected.NodeVariablesInCommand) > 0 {
		nodeVars, err = sc.resolveNodeVariables(expected)
		if err != nil {
			return "", err
		}
	}
	if len(expected.PowershellVariablesInCommand) > 0 {
		psVars, err = sc.resolvePowershellVariables(expected)
		if err != nil {
			return "", err
		}
	}

	expectedCmd := expected.Command
	for key, value := range nodeVars {
		expectedCmd = strings.ReplaceAll(expectedCmd, key, value)
	}
	for key, value := range psVars {
		expectedCmd = strings.ReplaceAll(expectedCmd, key, value)
	}
	// TODO: This goes against WICD design principles and needs to be changed https://issues.redhat.com/browse/WINC-896
	// WICD should not have special casing like this, and should use the services ConfigMap as its source of truth
	if strings.Contains(expectedCmd, services.NodeIPVar) {
		// Set NodeIP to IPv4 value by matching this instance's addresses to those in the Windows instances ConfigMap
		instances, err := wiparser.GetInstances(sc.client, sc.watchNamespace)
		if err != nil {
			return "", err
		}
		addrs, err := LocalInterfaceAddresses()
		if err != nil {
			return "", err
		}
		nodeIPValue := ""
		for _, addr := range addrs {
			ipv4Addr := getUsableIPv4(addr)
			if ipv4Addr == nil {
				continue
			}
			for _, instance := range instances {
				if instance.IPv4Address == ipv4Addr.String() {
					nodeIPValue = instance.IPv4Address
				}
			}
		}
		if nodeIPValue == "" {
			return "", errors.New("unable to find IPv4 address to use as Node IP for this instance")
		}
		expectedCmd = strings.ReplaceAll(expectedCmd, services.NodeIPVar, nodeIPValue)
	}
	return expectedCmd, nil
}

// resolveNodeVariables returns a map, with the keys being each variable, and the value being the string to replace the
// variable with
func (sc *ServiceController) resolveNodeVariables(svc servicescm.Service) (map[string]string, error) {
	vars := make(map[string]string)
	var node core.Node
	err := sc.client.Get(sc.ctx, client.ObjectKey{Name: sc.nodeName}, &node)
	if err != nil {
		return nil, err
	}
	for _, nodeVar := range svc.NodeVariablesInCommand {
		nodeParser := jsonpath.New("nodeParser")
		if err := nodeParser.Parse(nodeVar.NodeObjectJsonPath); err != nil {
			return nil, err
		}
		values, err := nodeParser.FindResults(node)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, errors.Wrapf(err, "expected node value %s missing", nodeVar.NodeObjectJsonPath)
		}
		if len(values) > 1 {
			return nil, errors.Wrapf(err, "jsonpath %s returned too many results", nodeVar.NodeObjectJsonPath)
		}
		if len(values[0]) != 1 || values[0][0].Kind() != reflect.String {
			return nil, errors.Wrapf(err, "unexpected value type for %s", nodeVar.NodeObjectJsonPath)
		}
		vars[nodeVar.Name] = values[0][0].String()
	}
	return vars, nil
}

// resolvePowershellVariables returns a map, with the keys being each variable, and the value being the string to
// replace the variable with
func (sc *ServiceController) resolvePowershellVariables(svc servicescm.Service) (map[string]string, error) {
	vars := make(map[string]string)
	for _, psVar := range svc.PowershellVariablesInCommand {
		out, err := sc.psCmdRunner.Run(psVar.Path)
		if err != nil {
			return nil, errors.Wrapf(err, "could not resolve PowerShell variable %s", psVar.Name)
		}
		vars[psVar.Name] = strings.TrimSpace(out)
	}
	return vars, nil
}

// NewDirectClient creates and returns an authenticated client that reads directly from the API server.
// It also returns the config and scheme used to created the client.
func NewDirectClient(cfg *rest.Config) (client.Client, error) {
	clientScheme := runtime.NewScheme()
	err := clientgoscheme.AddToScheme(clientScheme)
	if err = clientgoscheme.AddToScheme(clientScheme); err != nil {
		return nil, err
	}

	directClient, err := client.New(cfg, client.Options{Scheme: clientScheme})
	if err != nil {
		return nil, err
	}
	return directClient, nil
}

// GetAssociatedNode uses the given client to find the name of the node associated with the VM this is running on
func GetAssociatedNode(c client.Client, addrs []net.Addr) (*core.Node, error) {
	var nodes core.NodeList
	if err := c.List(context.TODO(), &nodes); err != nil {
		return nil, err
	}
	node, err := findNodeByAddress(&nodes, addrs)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// LocalInterfaceAddresses returns a slice of all addresses associated with local network interfaces
func LocalInterfaceAddresses() ([]net.Addr, error) {
	var addresses []net.Addr
	netIfs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, netInterface := range netIfs {
		addrs, err := netInterface.Addrs()
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, addrs...)
	}
	return addresses, nil
}

// getUsableAddress returns the ipv4 representation of the address, or nil if it is not usable by WICD.
func getUsableIPv4(addr net.Addr) net.IP {
	ipAddr, ok := addr.(*net.IPNet)
	if !ok {
		return nil
	}
	ipv4Addr := ipAddr.IP.To4()
	if ipv4Addr == nil || ipv4Addr.IsLoopback() {
		return nil
	}
	return ipv4Addr
}

// findNodeByAddress returns the node associated with this VM
func findNodeByAddress(nodes *core.NodeList, localAddrs []net.Addr) (*core.Node, error) {
	for _, localAddr := range localAddrs {
		ipv4Addr := getUsableIPv4(localAddr)
		if ipv4Addr == nil {
			continue
		}
		// Go through each node and check if the node has the ipv4 address in the address slice
		if node := nodeutil.FindByAddress(ipv4Addr.String(), nodes); node != nil {
			return node, nil
		}
	}
	return nil, errors.New("unable to find associated node")
}

// slicesEquivalent returns true if the slices have the same content, or if they both have no content
func slicesEquivalent(s1, s2 []string) bool {
	// reflect.DeepEqual considers a nil slice not equal to an empty slice, so we need to include an extra check
	// to see if they both length zero, one being nil, and one being empty
	if len(s1) == len(s2) && len(s1) == 0 {
		return true
	}
	return reflect.DeepEqual(s1, s2)

}

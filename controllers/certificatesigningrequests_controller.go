/*
Copyright 2021.

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

package controllers

import (
	"context"

	oconfig "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	certificatesv1 "k8s.io/api/certificates/v1"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/csr"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
)

// certificateSigningRequestsReconciler reconciles a CertificateSigningRequests object
type certificateSigningRequestsReconciler struct {
	instanceReconciler
	// platform indicates the cloud on which OpenShift cluster is running
	platform oconfig.PlatformType
}

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client-kubelet;kubernetes.io/kubelet-serving
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// the CertificateSigningRequests object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
func (r *certificateSigningRequestsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.log.WithValues("certificatesigningrequests", req.NamespacedName)

	var err error
	// Create a new signer using the private key that the instances will be configured with
	r.signer, err = signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "unable to create signer from private key secret")
	}

	certificateSigningRequest := &certificatesv1.CertificateSigningRequest{}
	if err := r.client.Get(ctx, req.NamespacedName, certificateSigningRequest); err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	configMap, err := r.k8sclientset.CoreV1().ConfigMaps(r.watchNamespace).Get(ctx, InstanceConfigMap, metav1.GetOptions{})
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.reconcileCSR(certificateSigningRequest, configMap)
}

// NewCertificateSigningRequestsReconciler returns a pointer to certificateSigningRequestsReconciler
func NewCertificateSigningRequestsReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*certificateSigningRequestsReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	return &certificateSigningRequestsReconciler{
			instanceReconciler: instanceReconciler{
				client:             mgr.GetClient(),
				log:                ctrl.Log.WithName("controllers").WithName("CertificateSigningRequests"),
				k8sclientset:       clientset,
				clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
				vxlanPort:          "",
				watchNamespace:     watchNamespace,
			},
			platform: clusterConfig.Platform(),
		},
		nil
}

// reconcileCSR handles the CSR validation and approval
func (r *certificateSigningRequestsReconciler) reconcileCSR(request *certificatesv1.CertificateSigningRequest, configMap *core.ConfigMap) error {
	if request == nil || configMap == nil {
		return errors.New("CSR or instance ConfigMap cannot be nil")
	}

	hostNames, err := r.findHostNames(configMap)
	if err != nil {
		return errors.Wrapf(err, "unable to find host names from ConfigMap %s", configMap.Name)
	}

	certificateSigningRequest, err := csr.NewApprover(request, r.k8sclientset, hostNames, r.log)
	if err != nil {
		return errors.Wrapf(err, "could not create WMCO CSR Approver")
	}

	if err := certificateSigningRequest.Approve(); err != nil {
		return errors.Wrapf(err, "WMCO CSR Approver could not approve %s CSR", request.Name)
	}

	return nil
}

// findHostNames returns a list of the instance host names from the instance configMap
// depending on the cloud provider type of the cluster.
func (r *certificateSigningRequestsReconciler) findHostNames(configMap *core.ConfigMap) ([]string, error) {
	// Get the list of hosts that are expected to be Nodes
	hosts, err := parseHosts(configMap.Data)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse hosts from ConfigMap %s", configMap.Name)
	}
	var requireVMHostName bool
	// platform type set None or vSphere indicates hostName required is the actual hostName of the VM
	if r.platform == oconfig.NonePlatformType || r.platform == oconfig.VSpherePlatformType {
		requireVMHostName = true
	}

	var hostNames []string
	for _, host := range hosts {
		hostName := host.Address
		if requireVMHostName {
			nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, host, r.signer,
				nil)
			if err != nil {
				return nil, errors.Wrap(err, "failed to create new nodeconfig")
			}
			// get the VM host name  by running hostname command on remote VM
			hostName, err = nc.Windows.Run("hostname", true)
			if err != nil {
				return nil, errors.Wrapf(err, "error getting the host name, with stdout %s", hostName)
			}
		}
		hostNames = append(hostNames, hostName)
	}
	return hostNames, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *certificateSigningRequestsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	certificateSigningRequestsPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return pendingCSRFilter(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return pendingCSRFilter(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return pendingCSRFilter(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(certificateSigningRequestsPredicate)).
		Complete(r)
}

// pendingCSRFilter looks for a CSR event object and returns true if that CSR
// has status pending
func pendingCSRFilter(obj runtime.Object) bool {
	cert, ok := obj.(*certificatesv1.CertificateSigningRequest)
	return ok && isPending(cert)
}

// isPending returns true if the CSR is neither approved, nor denied
func isPending(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificatesv1.CertificateApproved || c.Type == certificatesv1.CertificateDenied {
			return false
		}
	}
	return true
}

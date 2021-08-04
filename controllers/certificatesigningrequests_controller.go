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

	"github.com/pkg/errors"
	certificates "k8s.io/api/certificates/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/csr"
)

// certificateSigningRequestsReconciler reconciles a CertificateSigningRequests object
type certificateSigningRequestsReconciler struct {
	instanceReconciler
}

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client-kubelet;kubernetes.io/kubelet-serving
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get

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
			recorder:           mgr.GetEventRecorderFor("certificateSigningRequests"),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for a
// CertificateSigningRequests object and aims to move the current state of the cluster closer to the desired state.
func (r *certificateSigningRequestsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.log.WithValues("certificatesigningrequests", req.NamespacedName)

	certificateSigningRequest := &certificates.CertificateSigningRequest{}
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

	return ctrl.Result{}, r.reconcileCSR(certificateSigningRequest)
}

// reconcileCSR handles the CSR validation and approval
func (r *certificateSigningRequestsReconciler) reconcileCSR(request *certificates.CertificateSigningRequest) error {
	certificateSigningRequest, err := csr.NewApprover(r.client, r.k8sclientset, request, r.log, r.recorder, r.watchNamespace)
	if err != nil {
		return errors.Wrapf(err, "could not create WMCO CSR Approver")
	}

	if err := certificateSigningRequest.Approve(); err != nil {
		return errors.Wrapf(err, "WMCO CSR Approver could not approve %s CSR", request.Name)
	}

	return nil
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
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&certificates.CertificateSigningRequest{}, builder.WithPredicates(certificateSigningRequestsPredicate)).
		Complete(r)
}

// pendingCSRFilter looks for a CSR event object and returns true if that CSR
// has status pending
func pendingCSRFilter(obj runtime.Object) bool {
	cert, ok := obj.(*certificates.CertificateSigningRequest)
	return ok && isPending(cert)
}

// isPending returns true if the CSR is neither approved, nor denied
func isPending(csr *certificates.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certificates.CertificateApproved || c.Type == certificates.CertificateDenied {
			return false
		}
	}
	return true
}

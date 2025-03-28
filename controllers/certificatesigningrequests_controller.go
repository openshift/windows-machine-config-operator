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
	"fmt"

	certificates "k8s.io/api/certificates/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sretry "k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/csr"
)

const (
	// CSRController is the name of this controller in logs and other outputs.
	CSRController = "certificatesigningrequests"
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
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	return &certificateSigningRequestsReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(CSRController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     watchNamespace,
			recorder:           mgr.GetEventRecorderFor(CSRController),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for a
// CertificateSigningRequests object and aims to move the current state of the cluster closer to the desired state.
func (r *certificateSigningRequestsReconciler) Reconcile(ctx context.Context,
	req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	_ = r.log.WithValues(CSRController, req.NamespacedName)

	// Prevent WMCO upgrades while CSRs are being processed
	if err := condition.MarkAsBusy(ctx, r.client, r.watchNamespace, r.recorder, CSRController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		reconcileErr = markAsFreeOnSuccess(ctx, r.client, r.watchNamespace, r.recorder, CSRController,
			result.Requeue, reconcileErr)
	}()

	return ctrl.Result{}, r.reconcileCSR(ctx, req.NamespacedName)
}

// reconcileCSR handles the CSR validation and approval. Process wrapped in retry logic case of update conflicts
func (r *certificateSigningRequestsReconciler) reconcileCSR(ctx context.Context, namespacedName types.NamespacedName) error {
	certificateSigningRequest := &certificates.CertificateSigningRequest{}
	err := k8sretry.RetryOnConflict(k8sretry.DefaultBackoff, func() error {
		// Fetch object reference
		if err := r.client.Get(ctx, namespacedName, certificateSigningRequest); err != nil {
			if k8sapierrors.IsNotFound(err) {
				// Request object not found, could have been deleted after reconcile request.
				// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
				// Return and don't requeue
				return nil
			}
			// Error reading the object - return error to requeue the request.
			return err
		}

		// If a CSR is approved/denied after being added to the queue, but before we reconcile it,
		// trying to approve it again will result in an error and cause a loop.
		// Return early if the CSR has been approved/denied externally.
		if !isPending(certificateSigningRequest) {
			r.log.Info("CSR is already approved/denied", "Name", certificateSigningRequest.Name)
			return nil
		}

		csrApprover, err := csr.NewApprover(r.client, r.k8sclientset, certificateSigningRequest, r.log, r.recorder,
			r.watchNamespace)
		if err != nil {
			return fmt.Errorf("could not create WMCO CSR Approver: %w", err)
		}

		return csrApprover.Approve(ctx)
	})
	if err != nil {
		// Max retries were hit, or unrelated issue like permissions or a network error
		return fmt.Errorf("WMCO CSR Approver could not approve CSR %s: %w", certificateSigningRequest.Name, err)
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

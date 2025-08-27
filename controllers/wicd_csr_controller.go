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
	"strings"

	"github.com/go-logr/logr"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	csrvalidation "github.com/openshift/windows-machine-config-operator/pkg/csr/validation"
	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=get;update;patch

const (
	// WICDCSRController is the name of this controller in logs
	WICDCSRController = "wicd-csr"
)

// wicdCSRReconciler handles certificate signing requests for WICD
type wicdCSRReconciler struct {
	client         client.Client
	k8sclientset   *kubernetes.Clientset
	log            logr.Logger
	watchNamespace string
	recorder       record.EventRecorder
	validator      *csrvalidation.CSRValidator
}

// NewWICDCSRController creates a new WICD CSR controller following the existing pattern
func NewWICDCSRController(mgr manager.Manager, watchNamespace string) (*wicdCSRReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	validator := csrvalidation.NewCSRValidator(mgr.GetClient(), csrvalidation.WICDCertType)

	return &wicdCSRReconciler{
		client:         mgr.GetClient(),
		log:            ctrl.Log.WithName("controllers").WithName(WICDCSRController),
		k8sclientset:   clientset,
		watchNamespace: watchNamespace,
		recorder:       mgr.GetEventRecorderFor(WICDCSRController),
		validator:      validator,
	}, nil
}

// isWICDCSR checks if this CSR is from a WICD identity (service account or certificate-based)
func (r *wicdCSRReconciler) isWICDCSR(obj runtime.Object) bool {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok {
		return false
	}
	return r.isWICDServiceAccount(csr) || r.isWICDCertificateIdentity(csr)
}

// isPendingCSR checks if this CSR is pending (neither approved nor denied)
func isPendingCSR(obj runtime.Object) bool {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)
	if !ok {
		return false
	}
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved || condition.Type == certificatesv1.CertificateDenied {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager
func (r *wicdCSRReconciler) SetupWithManager(mgr ctrl.Manager) error {
	wicdCSRPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isWICDCSR(e.Object) && isPendingCSR(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isWICDCSR(e.ObjectNew) && isPendingCSR(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return r.isWICDCSR(e.Object) && isPendingCSR(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// We don't need to handle delete events for CSRs
			return false
		},
	}
	return builder.ControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(wicdCSRPredicate)).
		Named(WICDCSRController).
		Complete(r)
}

// Reconcile processes WICD CertificateSigningRequests
func (r *wicdCSRReconciler) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, err error) {
	// Prevent WMCO upgrades while CSRs are being processed
	if err := condition.MarkAsBusy(ctx, r.client, r.watchNamespace, r.recorder, NodeController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err = markAsFreeOnSuccess(ctx, r.client, r.watchNamespace, r.recorder, NodeController, result.Requeue, err)
	}()
	r.log.V(1).Info("reconciling", "name", req.NamespacedName.String())

	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.client.Get(ctx, req.NamespacedName, csr); err != nil {
		if k8sapierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get CertificateSigningRequest: %w", err)
	}

	// If a CSR is approved/denied after being added to the queue, but before we reconcile it,
	// trying to approve it again will result in an error and cause a loop.
	// Return early if the CSR has been approved/denied externally.
	if !isPendingCSR(csr) {
		r.log.Info("CSR is already approved/denied", "Name", csr.Name)
		return reconcile.Result{}, nil
	}

	// Validate signer name for WICD certificates
	if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientSignerName {
		r.log.Info("Ignoring CSR with unexpected signerName", "name", csr.Name, "signer", csr.Spec.SignerName)
		return reconcile.Result{}, nil
	}

	// Validate the CSR content
	if err := r.validator.ValidateCSR(ctx, csr); err != nil {
		r.log.Error(err, "WICD CSR validation failed, ignoring CSR")
		return reconcile.Result{}, nil
	}

	return r.approveCSR(ctx, csr)
}

// isWICDServiceAccount checks if this CSR is from the WICD service account
func (r *wicdCSRReconciler) isWICDServiceAccount(csr *certificatesv1.CertificateSigningRequest) bool {
	expectedUsername := fmt.Sprintf("system:serviceaccount:%s:%s", r.watchNamespace, windows.WicdServiceName)
	return csr.Spec.Username == expectedUsername
}

// isWICDCertificateIdentity checks if this CSR is from a WICD certificate-based identity
func (r *wicdCSRReconciler) isWICDCertificateIdentity(csr *certificatesv1.CertificateSigningRequest) bool {
	return strings.HasPrefix(csr.Spec.Username, rbac.WICDUserPrefix)
}

// approveCSR approves the certificate signing request using the proper UpdateApproval API
func (r *wicdCSRReconciler) approveCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (reconcile.Result, error) {
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           certificatesv1.CertificateApproved,
		Status:         corev1.ConditionTrue,
		Reason:         "WICDAutoApproved",
		Message:        "This CSR was approved by the WICD certificate controller",
		LastUpdateTime: metav1.Now(),
	})

	if _, err := r.k8sclientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to approve CSR: %w", err)
	}
	r.log.Info("CSR approved", "CSR", csr.Name)
	return reconcile.Result{}, nil
}

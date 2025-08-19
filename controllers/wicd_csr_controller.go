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
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	csrvalidation "github.com/openshift/windows-machine-config-operator/pkg/csr/validation"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=get;update;patch

const (
	// WICDCSRController is the name of this controller in logs
	WICDCSRController = "WICD-CSR"
	// MaxCertificateDuration is the maximum allowed certificate duration
	MaxCertificateDuration = 1 * time.Hour // Reduced from 24h to allow shorter durations for testing
)

// wicdCSRReconciler handles certificate signing requests for WICD
type wicdCSRReconciler struct {
	instanceReconciler
	validator *csrvalidation.CSRValidator
}

// NewWICDCSRController creates a new WICD CSR controller following the existing pattern
func NewWICDCSRController(mgr manager.Manager, clusterConfig cluster.Config) (*wicdCSRReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	// Create the WICD CSR validator
	validator := csrvalidation.NewCSRValidator(mgr.GetClient(), csrvalidation.WICDCertType)

	return &wicdCSRReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(WICDCSRController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     "", // WICD CSRs are cluster-scoped
			recorder:           mgr.GetEventRecorderFor(WICDCSRController),
		},
		validator: validator,
	}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *wicdCSRReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Build controller for CertificateSigningRequests
	return builder.ControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}).
		Named(WICDCSRController).
		Complete(r)
}

// Reconcile processes WICD CertificateSigningRequests
func (r *wicdCSRReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("CertificateSigningRequest", req.NamespacedName)

	// Get the CSR
	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.client.Get(ctx, req.NamespacedName, csr); err != nil {
		if k8sapierrors.IsNotFound(err) {
			log.Info("CertificateSigningRequest not found, ignoring")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("failed to get CertificateSigningRequest: %w", err)
	}

	// Check if this is a WICD CSR using shared validator
	if !r.validator.IsCorrectCertificateType(csr) {
		return reconcile.Result{}, nil // Not a WICD CSR, ignore
	}

	// Additional check for service account authentication (WICD-specific)
	if !r.isWICDServiceAccount(csr) {
		return reconcile.Result{}, nil // Not from WICD service account, ignore
	}

	// Skip if already processed
	if r.isCSRApproved(csr) || r.isCSRDenied(csr) {
		return reconcile.Result{}, nil
	}

	// Validate the CSR using shared validator
	if err := r.validator.ValidateCSR(ctx, csr); err != nil {
		log.Error(err, "WICD CSR validation failed, ignoring CSR (no approval)")
		return reconcile.Result{}, nil // Don't try to deny - just ignore
	}

	// Approve the CSR
	log.Info("WICD CSR validation successful, approving CSR")
	return r.approveCSR(ctx, csr)
}

// isWICDServiceAccount checks if this CSR is from the WICD service account
func (r *wicdCSRReconciler) isWICDServiceAccount(csr *certificatesv1.CertificateSigningRequest) bool {
	// CSRs from WICD service account - Kubernetes API server overrides username/groups
	// with the actual authenticated user (service account)
	return csr.Spec.Username == "system:serviceaccount:openshift-windows-machine-config-operator:windows-instance-config-daemon"
}

// approveCSR approves the certificate signing request using the proper UpdateApproval API
func (r *wicdCSRReconciler) approveCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (reconcile.Result, error) {
	log := r.log.WithValues("CertificateSigningRequest", csr.Name)

	// Add approval condition
	csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           certificatesv1.CertificateApproved,
		Status:         corev1.ConditionTrue,
		Reason:         "WICDAutoApproved",
		Message:        "This CSR was approved by the WICD certificate controller",
		LastUpdateTime: metav1.Now(),
	})

	// Use the specialized UpdateApproval API (like the original CSR controller)
	if _, err := r.k8sclientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
		log.Error(err, "Failed to approve CSR")
		return reconcile.Result{}, fmt.Errorf("failed to approve CSR: %w", err)
	}

	return reconcile.Result{}, nil
}

// isCSRApproved checks if the CSR is already approved
func (r *wicdCSRReconciler) isCSRApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isCSRDenied checks if the CSR is already denied
func (r *wicdCSRReconciler) isCSRDenied(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateDenied && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

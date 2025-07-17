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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/status,verbs=get;update;patch

const (
	// WICDCSRController is the name of this controller in logs
	WICDCSRController = "WICD-CSR"
	// MaxCertificateDuration is the maximum allowed certificate duration
	MaxCertificateDuration = 24 * time.Hour
)

// wicdCSRReconciler handles certificate signing requests for WICD
type wicdCSRReconciler struct {
	instanceReconciler
}

// NewWICDCSRController creates a new WICD CSR controller following the existing pattern
func NewWICDCSRController(mgr manager.Manager, clusterConfig cluster.Config) (*wicdCSRReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	return &wicdCSRReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(WICDCSRController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     "", // WICD CSRs are cluster-scoped
			recorder:           mgr.GetEventRecorderFor(WICDCSRController),
		},
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

	// Check if this is a WICD CSR
	if !r.isWICDCSR(csr) {
		return reconcile.Result{}, nil // Not a WICD CSR, ignore
	}

	// Skip if already processed
	if r.isCSRApproved(csr) || r.isCSRDenied(csr) {
		return reconcile.Result{}, nil
	}

	log.Info("Processing WICD CSR")

	// Validate the CSR
	if err := r.validateWICDCSR(ctx, csr); err != nil {
		log.Error(err, "WICD CSR validation failed, ignoring CSR (no approval)")
		return reconcile.Result{}, nil // Don't try to deny - just ignore
	}

	// Approve the CSR
	log.Info("WICD CSR validation successful, approving CSR")
	return r.approveCSR(ctx, csr)
}

// isWICDCSR checks if this is a WICD certificate signing request
func (r *wicdCSRReconciler) isWICDCSR(csr *certificatesv1.CertificateSigningRequest) bool {
	// CSRs from WICD service account - Kubernetes API server overrides username/groups
	// with the actual authenticated user (service account)
	if csr.Spec.Username == "system:serviceaccount:openshift-windows-machine-config-operator:windows-instance-config-daemon" {
		// Validate the certificate request content to ensure it's a legitimate WICD CSR
		return r.isValidWICDCertificateRequest(csr)
	}

	return false
}

// isValidWICDCertificateRequest validates that the certificate request content is from WICD
func (r *wicdCSRReconciler) isValidWICDCertificateRequest(csr *certificatesv1.CertificateSigningRequest) bool {
	// Parse the certificate request
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return false
	}

	certReq, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return false
	}

	// Check if Common Name starts with WICD prefix
	if !strings.HasPrefix(certReq.Subject.CommonName, rbac.WICDUserPrefix+":") {
		return false
	}

	// Check if Organization includes WICD group
	for _, org := range certReq.Subject.Organization {
		if org == rbac.WICDGroupName {
			return true
		}
	}

	return false
}

// validateWICDCSR validates a WICD certificate signing request
func (r *wicdCSRReconciler) validateWICDCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) error {
	// Extract node name from certificate request content (since username is service account)
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return fmt.Errorf("failed to decode certificate request")
	}

	certReq, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate request: %w", err)
	}

	// Extract node name from Common Name
	if !strings.HasPrefix(certReq.Subject.CommonName, rbac.WICDUserPrefix+":") {
		return fmt.Errorf("invalid common name format: %s", certReq.Subject.CommonName)
	}
	nodeName := strings.TrimPrefix(certReq.Subject.CommonName, rbac.WICDUserPrefix+":")

	if nodeName == "" {
		return fmt.Errorf("empty node name in CSR: %s", csr.Name)
	}

	// Verify the node exists
	node := &corev1.Node{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("node %s does not exist", nodeName)
		}
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	// Validate the certificate request
	if err := r.validateCertificateRequest(csr, nodeName); err != nil {
		return fmt.Errorf("certificate request validation failed: %w", err)
	}

	return nil
}

// validateCertificateRequest validates the actual certificate request
func (r *wicdCSRReconciler) validateCertificateRequest(csr *certificatesv1.CertificateSigningRequest, nodeName string) error {
	// Parse the certificate request
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return fmt.Errorf("failed to decode certificate request")
	}

	certReq, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate request: %w", err)
	}

	// Validate common name
	expectedCommonName := fmt.Sprintf("%s:%s", rbac.WICDUserPrefix, nodeName)
	if certReq.Subject.CommonName != expectedCommonName {
		return fmt.Errorf("invalid common name: expected %s, got %s", expectedCommonName, certReq.Subject.CommonName)
	}

	// Validate organization includes WICD group
	hasWICDGroup := false
	for _, org := range certReq.Subject.Organization {
		if org == rbac.WICDGroupName {
			hasWICDGroup = true
			break
		}
	}
	if !hasWICDGroup {
		return fmt.Errorf("certificate request missing required organization: %s", rbac.WICDGroupName)
	}

	// Validate key usages
	expectedUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageClientAuth,
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageKeyEncipherment,
	}

	if !r.hasExpectedUsages(csr.Spec.Usages, expectedUsages) {
		return fmt.Errorf("invalid usages: expected %v, got %v", expectedUsages, csr.Spec.Usages)
	}

	return nil
}

// hasExpectedUsages checks if the CSR has the expected key usages
func (r *wicdCSRReconciler) hasExpectedUsages(actual, expected []certificatesv1.KeyUsage) bool {
	if len(actual) != len(expected) {
		return false
	}

	actualMap := make(map[certificatesv1.KeyUsage]bool)
	for _, usage := range actual {
		actualMap[usage] = true
	}

	for _, usage := range expected {
		if !actualMap[usage] {
			return false
		}
	}

	return true
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

	log.Info("Successfully approved WICD CSR")
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

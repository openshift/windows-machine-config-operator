//go:build windows

/*
Copyright 2024.

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

package certs

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"path/filepath"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/certificate"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

// WICDCertificateManager manages WICD node certificates
type WICDCertificateManager struct {
	started           bool
	currentCertPath   string
	certificateConfig *rest.Config
	certManager       certificate.Manager
}

// NewWICDCertificateManager creates a new WICD certificate manager
func NewWICDCertificateManager(nodeName, certDir, bootstrapKubeconfigPath, apiServer string, certDuration time.Duration) (*WICDCertificateManager, error) {
	bootstrapConfig, err := clientcmd.BuildConfigFromFlags("", bootstrapKubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load bootstrap kubeconfig from %s: %w", bootstrapKubeconfigPath, err)
	}

	currentCertPath := filepath.Join(certDir, rbac.WICDCertNamePrefix+"-current.pem")
	currentKeyPath := filepath.Join(certDir, rbac.WICDCertNamePrefix+"-current.pem")

	certificateConfig := &rest.Config{
		Host: apiServer,
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: currentCertPath,
			KeyFile:  currentKeyPath,
			CAFile:   bootstrapConfig.TLSClientConfig.CAFile,
			CAData:   bootstrapConfig.TLSClientConfig.CAData,
		},
	}

	newClientsetFn := func(current *tls.Certificate) (kubernetes.Interface, error) {
		cfg := bootstrapConfig
		if current != nil {
			cfg = certificateConfig
		}
		return kubernetes.NewForConfig(cfg)
	}

	// Create certificate store
	certificateStore, err := certificate.NewFileStore(rbac.WICDCertNamePrefix, certDir, certDir, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize certificate store: %w", err)
	}

	wicdCertUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
		certificatesv1.UsageKeyEncipherment,
	}

	certCommonName := fmt.Sprintf("%s:%s", rbac.WICDUserPrefix, nodeName)
	certManager, err := certificate.NewManager(&certificate.Config{
		ClientsetFn: newClientsetFn,
		Template: &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   certCommonName,
				Organization: []string{rbac.WICDGroupName},
			},
		},
		RequestedCertificateLifetime: &certDuration,
		SignerName:                   certificatesv1.KubeAPIServerClientSignerName,
		Usages:                       wicdCertUsages,
		CertificateStore:             certificateStore,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize certificate manager: %w", err)
	}

	wcm := &WICDCertificateManager{
		currentCertPath:   currentCertPath,
		certificateConfig: certificateConfig,
		certManager:       certManager,
	}

	return wcm, nil
}

// StartCertificateManagement starts certificate management for WICD.
// The certificate manager automatically handles renewal at 80% of certificate lifetime.
// NOTE: Kubernetes does not support certificate revocation for CSR-issued certificates.
// Once issued, certificates remain valid until expiration. For security, certificates should have
// short lifetimes and be rotated frequently.
func (wcm *WICDCertificateManager) StartCertificateManagement() error {
	if wcm.started {
		return nil
	}

	wcm.certManager.Start()
	wcm.started = true
	return nil
}

// Stop stops the certificate manager
func (wcm *WICDCertificateManager) Stop() {
	if wcm.certManager != nil {
		wcm.certManager.Stop()
	}
	wcm.started = false
}

// GetCertificatePaths returns the paths to the current certificate files
func (wcm *WICDCertificateManager) GetCertificatePaths() (certFile string) {
	return wcm.currentCertPath
}

// GetCertificateConfig returns the certificate-based rest.Config
func (wcm *WICDCertificateManager) GetCertificateConfig() *rest.Config {
	return wcm.certificateConfig
}

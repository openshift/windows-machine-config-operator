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
	"context"
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
	"k8s.io/klog/v2"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

const (
	// WICD certificate configuration
	wicdCertNamePrefix       = "wicd-client"
	wicdCertCommonNamePrefix = rbac.WICDUserPrefix
	wicdCertOrganization     = rbac.WICDGroupName
)

// WICDCertificateManager manages WICD node certificates using k8s.io/client-go certificate manager
type WICDCertificateManager struct {
	nodeName            string
	certDir             string
	bootstrapKubeconfig string
	apiServer           string
	certDuration        time.Duration
	started             bool

	// Certificate paths
	currentCertPath string
	currentKeyPath  string

	// Built-in certificate manager
	certManager certificate.Manager
}

// NewWICDCertificateManager creates a new WICD certificate manager
func NewWICDCertificateManager(nodeName, certDir, bootstrapKubeconfig, apiServer string, certDuration time.Duration) *WICDCertificateManager {
	return &WICDCertificateManager{
		nodeName:            nodeName,
		certDir:             certDir,
		bootstrapKubeconfig: bootstrapKubeconfig,
		apiServer:           apiServer,
		certDuration:        certDuration,
		currentCertPath:     filepath.Join(certDir, wicdCertNamePrefix+"-current.pem"),
		currentKeyPath:      filepath.Join(certDir, wicdCertNamePrefix+"-current.pem"),
	}
}

// StartCertificateManagement starts certificate management for WICD using k8s.io/client-go certificate manager.
// The certificate manager automatically handles renewal at 80% of certificate lifetime using built-in goroutines.
// NOTE: Kubernetes does not support certificate revocation for CSR-issued certificates.
// Once issued, certificates remain valid until expiration. For security, certificates should have
// short lifetimes and be rotated frequently.
func (wcm *WICDCertificateManager) StartCertificateManagement(ctx context.Context) error {
	if wcm.started {
		return nil
	}

	klog.Infof("Starting WICD certificate management for node %s", wcm.nodeName)

	// Load bootstrap kubeconfig for initial authentication
	bootstrapConfig, err := clientcmd.BuildConfigFromFlags("", wcm.bootstrapKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load bootstrap kubeconfig from %s: %w", wcm.bootstrapKubeconfig, err)
	}

	// Create default config for certificate-based authentication
	defaultConfig := &rest.Config{
		Host: wcm.apiServer,
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: wcm.currentCertPath,
			KeyFile:  wcm.currentKeyPath,
			CAData:   bootstrapConfig.CAData,
		},
	}

	// Function to create appropriate client based on certificate availability
	newClientsetFn := func(current *tls.Certificate) (kubernetes.Interface, error) {
		cfg := bootstrapConfig
		if current != nil {
			cfg = defaultConfig
		}
		return kubernetes.NewForConfig(cfg)
	}

	// Initialize certificate store with default naming - let it handle symlinks naturally
	certificateStore, err := certificate.NewFileStore(wicdCertNamePrefix, wcm.certDir, wcm.certDir, "", "")
	if err != nil {
		return fmt.Errorf("failed to initialize certificate store: %w", err)
	}

	// WICD certificate usages
	wicdCertUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
		certificatesv1.UsageKeyEncipherment,
	}

	// Create certificate manager using k8s.io/client-go standard pattern
	certCommonName := fmt.Sprintf("%s:%s", wicdCertCommonNamePrefix, wcm.nodeName)
	wcm.certManager, err = certificate.NewManager(&certificate.Config{
		ClientsetFn: newClientsetFn,
		Template: &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   certCommonName,
				Organization: []string{wicdCertOrganization},
			},
		},
		RequestedCertificateLifetime: &wcm.certDuration,
		SignerName:                   certificatesv1.KubeAPIServerClientSignerName,
		Usages:                       wicdCertUsages,
		CertificateStore:             certificateStore,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize certificate manager: %w", err)
	}

	// Start the certificate manager - this handles renewal automatically
	wcm.certManager.Start()

	wcm.started = true
	klog.Infof("WICD certificate management started successfully for node %s", wcm.nodeName)
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
func (wcm *WICDCertificateManager) GetCertificatePaths() (certFile, keyFile string) {
	return wcm.currentCertPath, wcm.currentKeyPath
}

// GetCurrentCertificate returns the current certificate (if available)
func (wcm *WICDCertificateManager) GetCurrentCertificate() (*tls.Certificate, error) {
	if wcm.certManager != nil {
		cert := wcm.certManager.Current()
		if cert == nil {
			return nil, fmt.Errorf("no certificate available from certificate manager")
		}
		return cert, nil
	}
	cert, err := tls.LoadX509KeyPair(wcm.currentCertPath, wcm.currentKeyPath)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// IsStarted returns whether certificate management has been started
func (wcm *WICDCertificateManager) IsStarted() bool {
	return wcm.started
}

// CreateCertificateBasedKubeconfig creates a kubeconfig that uses the certificate for authentication
func (wcm *WICDCertificateManager) CreateCertificateBasedKubeconfig() (*rest.Config, error) {
	if !wcm.IsStarted() {
		return nil, fmt.Errorf("certificate management not started")
	}

	// Load bootstrap config for CA data
	bootstrapConfig, err := clientcmd.BuildConfigFromFlags("", wcm.bootstrapKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load bootstrap kubeconfig: %w", err)
	}

	// Create certificate-based config
	config := &rest.Config{
		Host: wcm.apiServer,
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: wcm.currentCertPath,
			KeyFile:  wcm.currentKeyPath,
			CAData:   bootstrapConfig.CAData,
		},
	}

	return config, nil
}

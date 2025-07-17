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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

const (
	// WICD certificate configuration
	wicdCertNamePrefix       = "wicd-client"
	wicdCertCommonNamePrefix = rbac.WICDUserPrefix
	wicdCertOrganization     = rbac.WICDGroupName
)

var (
	// WICD certificate usages
	wicdCertUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
		certificatesv1.UsageKeyEncipherment,
	}
)

// WICDCertificateManager manages WICD node certificates
type WICDCertificateManager struct {
	nodeName            string
	certDir             string
	bootstrapKubeconfig string
	apiServer           string
	certDuration        time.Duration
	started             bool
	mu                  sync.Mutex

	// Certificate paths
	currentCertPath string
	currentKeyPath  string
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
		currentKeyPath:      filepath.Join(certDir, wicdCertNamePrefix+"-current-key.pem"),
	}
}

// StartCertificateManagement starts certificate management for WICD
func (wcm *WICDCertificateManager) StartCertificateManagement(ctx context.Context, wg *sync.WaitGroup) error {
	wcm.mu.Lock()
	defer wcm.mu.Unlock()

	if wcm.started {
		return nil
	}

	klog.Infof("Starting WICD certificate management for node %s", wcm.nodeName)

	// Load bootstrap kubeconfig
	bootstrapConfig, err := clientcmd.BuildConfigFromFlags("", wcm.bootstrapKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load bootstrap kubeconfig from %s: %w", wcm.bootstrapKubeconfig, err)
	}

	// Create bootstrap client
	bootstrapClient, err := kubernetes.NewForConfig(bootstrapConfig)
	if err != nil {
		return fmt.Errorf("failed to create bootstrap client: %w", err)
	}

	// Certificate directory should already exist (created during node setup in RequiredDirectories)
	klog.Infof("DEBUG: Using certificate directory: %s", wcm.certDir)

	// Request initial certificate
	klog.Infof("DEBUG: Starting initial certificate request for node %s", wcm.nodeName)
	if err := wcm.requestCertificate(ctx, bootstrapClient); err != nil {
		klog.Errorf("DEBUG: Initial certificate request failed: %v", err)
		return fmt.Errorf("failed to request certificate: %w", err)
	}
	klog.Infof("DEBUG: Initial certificate request completed successfully")

	// Start certificate monitoring and renewal
	wg.Add(1)
	go wcm.certificateRenewalLoop(ctx, wg, bootstrapClient)

	wcm.started = true
	klog.Infof("WICD certificate management started successfully for node %s", wcm.nodeName)
	return nil
}

// requestCertificate creates and submits a CSR, then waits for approval
func (wcm *WICDCertificateManager) requestCertificate(ctx context.Context, client kubernetes.Interface) error {
	klog.Infof("Requesting certificate for WICD node %s", wcm.nodeName)
	klog.Infof("DEBUG: Certificate duration: %v", wcm.certDuration)

	// Generate private key
	klog.V(4).Infof("DEBUG: Generating RSA private key")
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		klog.Errorf("DEBUG: Failed to generate private key: %v", err)
		return fmt.Errorf("failed to generate private key: %w", err)
	}
	klog.V(4).Infof("DEBUG: Private key generated successfully")

	// Create certificate request
	certCommonName := fmt.Sprintf("%s:%s", wicdCertCommonNamePrefix, wcm.nodeName)
	klog.Infof("DEBUG: Creating certificate request with CN=%s, Org=%s", certCommonName, wicdCertOrganization)
	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   certCommonName,
			Organization: []string{wicdCertOrganization},
		},
	}

	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, template, privateKey)
	if err != nil {
		klog.Errorf("DEBUG: Failed to create certificate request: %v", err)
		return fmt.Errorf("failed to create certificate request: %w", err)
	}
	klog.V(4).Infof("DEBUG: Certificate request created successfully")

	// Create CSR object
	csrName := fmt.Sprintf("wicd-%s-%d", wcm.nodeName, time.Now().Unix())
	expirationSeconds := int32(wcm.certDuration.Seconds())
	klog.Infof("DEBUG: Creating CSR %s with expiration %d seconds (%v)", csrName, expirationSeconds, wcm.certDuration)
	csrObj := &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: csrName,
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrBytes}),
			SignerName:        certificatesv1.KubeAPIServerClientSignerName,
			Usages:            wicdCertUsages,
			ExpirationSeconds: &expirationSeconds,
			// Username and Groups will be automatically set by Kubernetes based on the authenticated ServiceAccount
		},
	}

	// Submit CSR using bootstrap client (not impersonated)
	klog.Infof("DEBUG: Submitting CSR %s using bootstrap client", csrName)
	_, err = client.CertificatesV1().CertificateSigningRequests().Create(ctx, csrObj, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("DEBUG: Failed to submit CSR %s: %v", csrName, err)
		return fmt.Errorf("failed to create CSR: %w", err)
	}
	klog.Infof("CSR %s created, waiting for approval", csrName)

	// Wait for CSR to be approved and certificate to be issued
	var signedCert []byte
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		klog.V(4).Infof("DEBUG: Checking CSR %s status...", csrName)
		updatedCSR, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, csrName, metav1.GetOptions{})
		if err != nil {
			klog.V(4).Infof("Failed to get CSR %s: %v", csrName, err)
			klog.Errorf("DEBUG: Error retrieving CSR %s: %v", csrName, err)
			return false, nil
		}

		klog.V(4).Infof("DEBUG: CSR %s found, checking conditions...", csrName)
		// Check if CSR was denied
		for _, condition := range updatedCSR.Status.Conditions {
			klog.V(4).Infof("DEBUG: CSR %s condition: Type=%s, Status=%s, Reason=%s",
				csrName, condition.Type, condition.Status, condition.Reason)
			if condition.Type == certificatesv1.CertificateDenied {
				klog.Errorf("DEBUG: CSR %s was denied: %s", csrName, condition.Message)
				return false, fmt.Errorf("CSR was denied: %s", condition.Message)
			}
		}

		// Check if certificate is available
		if len(updatedCSR.Status.Certificate) > 0 {
			klog.Infof("DEBUG: CSR %s approved! Certificate received (%d bytes)", csrName, len(updatedCSR.Status.Certificate))
			signedCert = updatedCSR.Status.Certificate
			return true, nil
		}

		klog.V(4).Infof("DEBUG: CSR %s still pending approval, continuing to wait...", csrName)
		return false, nil
	})

	if err != nil {
		klog.Errorf("DEBUG: CSR %s approval wait failed: %v", csrName, err)
		return fmt.Errorf("failed to wait for CSR approval: %w", err)
	}

	if len(signedCert) == 0 {
		klog.Errorf("DEBUG: CSR %s approval timeout - no certificate received within 2 minutes", csrName)
		return fmt.Errorf("CSR approval timeout")
	}

	// Save certificate and key
	klog.Infof("DEBUG: Saving certificate and key to %s", wcm.certDir)
	if err := wcm.saveCertificateAndKey(signedCert, privateKey); err != nil {
		klog.Errorf("DEBUG: Failed to save certificate and key: %v", err)
		return fmt.Errorf("failed to save certificate and key: %w", err)
	}

	klog.Infof("Certificate obtained and saved successfully for node %s", wcm.nodeName)
	klog.Infof("DEBUG: Certificate request process completed successfully for CSR %s", csrName)
	return nil
}

// saveCertificateAndKey saves the certificate and private key to disk with proper Windows directory handling
func (wcm *WICDCertificateManager) saveCertificateAndKey(certData []byte, privateKey *rsa.PrivateKey) error {
	klog.Infof("DEBUG: Encoding private key for saving")
	// Encode private key
	keyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: keyBytes,
	})
	klog.V(4).Infof("DEBUG: Private key encoded successfully (%d bytes)", len(keyPEM))

	klog.Infof("DEBUG: Writing separate certificate and key files")
	klog.Infof("DEBUG: Certificate path: %s", wcm.currentCertPath)
	klog.Infof("DEBUG: Key path: %s", wcm.currentKeyPath)

	// Ensure certificate directory exists with proper permissions
	klog.Infof("DEBUG: Ensuring certificate directory exists: %s", wcm.certDir)
	if err := os.MkdirAll(wcm.certDir, 0700); err != nil {
		klog.Errorf("DEBUG: Failed to create certificate directory %s: %v", wcm.certDir, err)
		return fmt.Errorf("failed to create certificate directory: %w", err)
	}

	// Write certificate file
	klog.Infof("DEBUG: Writing certificate file to %s", wcm.currentCertPath)
	if err := os.WriteFile(wcm.currentCertPath, certData, 0600); err != nil {
		klog.Errorf("DEBUG: Failed to write certificate file %s: %v", wcm.currentCertPath, err)
		return fmt.Errorf("failed to write certificate file: %w", err)
	}
	klog.Infof("DEBUG: Certificate file written successfully")

	// Write key file
	klog.Infof("DEBUG: Writing key file to %s", wcm.currentKeyPath)
	if err := os.WriteFile(wcm.currentKeyPath, keyPEM, 0600); err != nil {
		klog.Errorf("DEBUG: Failed to write key file %s: %v", wcm.currentKeyPath, err)
		return fmt.Errorf("failed to write key file: %w", err)
	}
	klog.Infof("DEBUG: Key file written successfully")

	// Verify both files were written correctly
	if _, err := os.Stat(wcm.currentCertPath); err != nil {
		klog.Errorf("DEBUG: Certificate file verification failed: %v", err)
		return fmt.Errorf("certificate file verification failed: %w", err)
	}
	if _, err := os.Stat(wcm.currentKeyPath); err != nil {
		klog.Errorf("DEBUG: Key file verification failed: %v", err)
		return fmt.Errorf("key file verification failed: %w", err)
	}

	klog.Infof("Certificate and key files saved successfully for node %s", wcm.nodeName)
	klog.Infof("DEBUG: Certificate: %s", wcm.currentCertPath)
	klog.Infof("DEBUG: Key: %s", wcm.currentKeyPath)
	return nil
}

// certificateRenewalLoop monitors certificate validity and renews when needed
func (wcm *WICDCertificateManager) certificateRenewalLoop(ctx context.Context, wg *sync.WaitGroup, client kubernetes.Interface) {
	defer wg.Done()

	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds for faster rotation testing
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := wcm.checkAndRenewCertificate(ctx, client); err != nil {
				klog.Errorf("Certificate renewal check failed: %v", err)
			}
		}
	}
}

// checkAndRenewCertificate checks if certificate needs renewal and renews if necessary
func (wcm *WICDCertificateManager) checkAndRenewCertificate(ctx context.Context, client kubernetes.Interface) error {
	// Load current certificate
	cert, err := wcm.loadCurrentCertificate()
	if err != nil {
		klog.Errorf("Failed to load current certificate, requesting new one: %v", err)
		return wcm.requestCertificate(ctx, client)
	}

	// Check if renewal is needed (renew when 80% of lifetime has passed)
	now := time.Now()
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	renewalTime := cert.NotBefore.Add(time.Duration(0.8 * float64(lifetime)))

	if now.After(renewalTime) {
		klog.Infof("Certificate for node %s needs renewal", wcm.nodeName)
		return wcm.requestCertificate(ctx, client)
	}

	return nil
}

// loadCurrentCertificate loads and parses the current certificate
func (wcm *WICDCertificateManager) loadCurrentCertificate() (*x509.Certificate, error) {
	data, err := os.ReadFile(wcm.currentCertPath)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no certificate found in file")
	}

	return x509.ParseCertificate(block.Bytes)
}

// GetCertificatePaths returns the paths to the current certificate files
func (wcm *WICDCertificateManager) GetCertificatePaths() (certFile, keyFile string) {
	return wcm.currentCertPath, wcm.currentKeyPath
}

// GetCurrentCertificate returns the current certificate (if available)
func (wcm *WICDCertificateManager) GetCurrentCertificate() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(wcm.currentCertPath, wcm.currentKeyPath)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// IsStarted returns whether certificate management has been started
func (wcm *WICDCertificateManager) IsStarted() bool {
	wcm.mu.Lock()
	defer wcm.mu.Unlock()
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

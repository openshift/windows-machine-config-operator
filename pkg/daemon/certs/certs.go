//go:build windows

/*
Copyright 2023.

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
	"bufio"
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"k8s.io/klog/v2"
)

// Reconcile ensures the certificates in the given trusted CA bundle file are imported as system certificates.
// Returns a boolean whether any certs needed to be reconciled
func Reconcile(caBundle string) (bool, error) {
	if caBundle == "" {
		// TODO: ensure that all certs that we've added previously are removed https://issues.redhat.com/browse/WINC-688
		return false, nil
	}
	// Read expected certs from CA trust bundle file
	expectedCerts, err := readCertsFromFile(caBundle)
	if err != nil {
		return false, fmt.Errorf("Failed to read certificate file: %v", err)
	}
	return ensureCertsAreImported(expectedCerts)
}

// readCertsFromFile reads in any PEM-encoded certificates from the given file
func readCertsFromFile(path string) ([]*x509.Certificate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Failed to open file: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			klog.Errorf("Failed to close file %s", f.Name())
		}
	}()

	// Process the file one certificate at a time
	scanner := bufio.NewScanner(f)
	scanner.Split(splitAtPEMCert())
	var certs []*x509.Certificate
	for scanner.Scan() {
		// Read next token
		pemData := scanner.Text()
		block, _ := pem.Decode([]byte(pemData))
		if block == nil {
			return nil, fmt.Errorf("No PEM data read from token")
		}
		// Decode and parse the certificate
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse certificate: %v\n", err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

// splitAtPEMCert is a custom closure function to split data into complete PEM-encoded certificates
func splitAtPEMCert() func(data []byte, atEOF bool) (advance int, token []byte, err error) {
	const endCertTag = "-----END CERTIFICATE-----"
	searchBytes := []byte(endCertTag)
	searchLen := len(searchBytes)
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		dataLen := len(data)
		if atEOF && dataLen == 0 {
			// Empty file, do nothing
			return 0, nil, nil
		}

		// Find next separator and return the token
		if i := bytes.Index(data, searchBytes); i >= 0 {
			return i + searchLen, data[0 : i+searchLen], nil
		}

		if atEOF {
			return dataLen, data, fmt.Errorf("Hit end of file without finding terminating separator %s", endCertTag)
		}
		// Otherwise, continue reading file data
		return 0, nil, nil
	}
}

// ensureCertsAreImported imports the expected certificates into the instance's root system trust store, if not already
// present. Returns a boolean if any certificates were imported
func ensureCertsAreImported(expectedCerts []*x509.Certificate) (bool, error) {
	// Open the root certificate store
	systemStore, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("ROOT"))))
	if err != nil {
		return false, fmt.Errorf("Failed to open root certificate store: %v", err)
	}
	defer func() {
		if err := windows.CertCloseStore(systemStore, 0); err != nil {
			klog.Error("Failed to close root system certificate store")
		}
	}()

	// Get all existing from system store
	existingCerts, err := getAllCerts(systemStore)
	if err != nil {
		return false, fmt.Errorf("Failed to read certificate file: %v", err)
	}

	certChange := false
	for _, cert := range expectedCerts {
		if containsCert(existingCerts, cert) {
			// Cert already exists as expected, do nothing
			continue
		}

		// Add the certificate to the instance's system-wide trust store
		if err := addCertToStore(systemStore, cert); err != nil {
			return false, err
		}
		certChange = true
	}
	return certChange, nil
}

// getAllCerts reads all the certs from the given certificate store
func getAllCerts(store windows.Handle) ([]*x509.Certificate, error) {
	var existingSystemCerts []*x509.Certificate
	var certContext *windows.CertContext
	var err error
	for {
		certContext, err = windows.CertEnumCertificatesInStore(store, certContext)
		if err != nil {
			if errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				// Error code implies we have read all certs:
				// https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certenumcertificatesinstore
				break
			}
			return nil, fmt.Errorf("Error reading existing system certificate: %v", err)
		}

		// Convert the certificate bytes to Golang x509.Certificate
		certBytes := unsafe.Slice(certContext.EncodedCert, certContext.Length)
		x509Cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse certificate from bytes: %v", err)
		}
		existingSystemCerts = append(existingSystemCerts, x509Cert)
	}
	klog.Infof("found %d existing system certificates", len(existingSystemCerts))
	return existingSystemCerts, nil
}

// containsCert checks if the target certificate exists in the given slice
func containsCert(storeContents []*x509.Certificate, target *x509.Certificate) bool {
	for _, cert := range storeContents {
		if cert.Equal(target) {
			return true
		}
	}
	return false
}

// addCertToStore imports the given certificate into the given certificate store
func addCertToStore(store windows.Handle, cert *x509.Certificate) error {
	// Convert x509 certificate to a Windows CertContext
	certContext, err := windows.CertCreateCertificateContext(
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &cert.Raw[0], uint32(len(cert.Raw)))
	if err != nil {
		return fmt.Errorf("Failed to create content from x509 cert %s: %v\n", cert.Subject.String(), err)
	}
	if err = windows.CertAddCertificateContextToStore(store, certContext,
		windows.CERT_STORE_ADD_REPLACE_EXISTING, nil); err != nil {
		return fmt.Errorf("Failed to import certificate %s: %v\n", cert.Subject.String(), err)
	}
	klog.Infof("Certificate %s imported successfully.", cert.Subject.String())
	return nil
}

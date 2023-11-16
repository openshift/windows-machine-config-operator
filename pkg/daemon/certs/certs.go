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
	"io"
	"os"
	"reflect"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"k8s.io/klog/v2"
)

// importedCABundleFile tracks all the certs that the operator has imported into the node's local trust store
const importedCABundleFile = "C:\\k\\imported-certs.pem"

// Reconcile ensures the node's system certificates match the expected state as given by the trusted CA bundle file.
// Returns a boolean whether any certs needed to be reconciled.
// If this function returns true and a non-nil error, the instance must be rebooted anyway.
func Reconcile(caBundlePath string) (bool, error) {
	// caBundlePath will be empty when a cluster-wide proxy is being removed or not in use
	certChange, err := reconcileCerts(caBundlePath)
	if err != nil {
		return false, err
	}
	// An error updating the imported bundle file will be self-corrected on the next reconcile, since it is the source
	// of truth for determining whether certs need to be updated
	return certChange, updateImportedCABundle(caBundlePath, certChange)
}

// reconcileCerts reconciles any discrepency between expected and existing Windows certificates by importing or
// deleting certificates from the root system store. Returns a boolean if any certificates were imported or deleted
func reconcileCerts(caBundlePath string) (bool, error) {
	expectedCerts, err := getExpectedCerts(caBundlePath)
	if err != nil {
		return false, err
	}
	existingCerts, err := getExistingCerts()
	if err != nil {
		return false, err
	}

	if reflect.DeepEqual(expectedCerts, existingCerts) {
		return false, nil
	}

	// Open the root certificate store
	systemStore, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("ROOT"))))
	if err != nil {
		return false, fmt.Errorf("Failed to open root certificate store: %w", err)
	}
	defer func() {
		if err := windows.CertCloseStore(systemStore, 0); err != nil {
			klog.Error("Failed to close root system certificate store")
		}
	}()

	certsImported, err := ensureCertsAreImported(systemStore, expectedCerts, existingCerts)
	if err != nil {
		return false, fmt.Errorf("Failed to import certificates: %w", err)
	}
	certsDeleted, err := ensureCertsAreDeleted(systemStore, expectedCerts, existingCerts)
	if err != nil {
		return false, fmt.Errorf("Failed to delete certificates: %w", err)
	}
	return (certsImported || certsDeleted), nil
}

// getExpectedCerts reads expected state of certs from the CA bundle file. Returns an empty slice if no proxy is in use.
func getExpectedCerts(path string) ([]*x509.Certificate, error) {
	var expectedCerts []*x509.Certificate
	var err error
	if path == "" {
		return expectedCerts, nil
	}

	if expectedCerts, err = readCertsFromFile(path); err != nil {
		return nil, fmt.Errorf("Failed to read certificate file %s: %w", path, err)
	}
	return expectedCerts, nil
}

// getExistingCerts populates existing certs using the previously imported CA trust bundle
func getExistingCerts() ([]*x509.Certificate, error) {
	var existingCerts []*x509.Certificate
	exists, err := fileExists(importedCABundleFile)
	if err != nil {
		// If there is an error here due to file corruption, the file will get deleted and re-created when the node is
		// reconfigured. This has the potential to leave stale certificates behind on the node.
		return nil, err
	}
	// The file will not exist on first reconcile in which case existingCerts will be empty.
	if !exists {
		return existingCerts, nil
	}

	if existingCerts, err = readCertsFromFile(importedCABundleFile); err != nil {
		return nil, fmt.Errorf("Failed to read certificate file %s: %w", importedCABundleFile, err)
	}
	return existingCerts, nil
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

// fileExists checks if the file at the given path exists. Returns true if it exists, false if not, and error otherwise.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, newFileIOError(fmt.Errorf("failed to check if file %s exists: %w", path, err))
	}
}

// updateImportedCABundle updates the previously imported CA bundle file, if needed.
func updateImportedCABundle(caBundlePath string, certChange bool) error {
	if caBundlePath == "" {
		// When proxy is deconfigured, ensure the file is deleted to avoid leaking certs
		return ensureFileIsRemoved(importedCABundleFile)
	}
	if certChange {
		klog.Infof("copied file contents from %s to %s", caBundlePath, importedCABundleFile)
		// When certs have been reconciled, update the previously imported CA trust bundle to hold the new cert bundle
		return copyFile(caBundlePath, importedCABundleFile)
	}
	return nil
}

// ensureFileIsRemoved deletes the file at the given path. No-op if the file already does not exist
func ensureFileIsRemoved(path string) error {
	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	klog.Infof("removed file %s", path)
	return nil
}

// copyFile copies the file contents from the source file to the destination. Creates the destination file if needed.
func copyFile(src, dst string) error {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Close(); err != nil {
			klog.Errorf("Failed to close file %s", r.Name())
		}
	}()
	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err := w.Close(); err != nil {
			klog.Errorf("Failed to close file %s", w.Name())
		}
	}()
	_, err = io.Copy(w, r)
	return err
}

// ensureCertsAreImported imports the expected certificates into the instance's root system trust store, if not already
// present. Returns a boolean if any certificates were imported
func ensureCertsAreImported(store windows.Handle, expectedCerts, existingCerts []*x509.Certificate) (bool, error) {
	certImported := false
	for _, cert := range expectedCerts {
		if containsCert(existingCerts, cert) {
			// Cert already exists as expected, do nothing
			continue
		}

		// Add the certificate to the instance's system-wide trust store
		if err := addCertToStore(store, cert); err != nil {
			return false, err
		}
		certImported = true
	}
	return certImported, nil
}

// ensureCertsAreDeleted deletes any existing certificates that are not in the expected list.
// Returns a boolean if any certificates were deleted.
func ensureCertsAreDeleted(store windows.Handle, expectedCerts, existingCerts []*x509.Certificate) (bool, error) {
	certDeleted := false
	for _, cert := range existingCerts {
		// Any imported previously certs that are NOT in the new cert bundle should be removed
		if !containsCert(expectedCerts, cert) {
			if err := removeCertFromStore(store, cert); err != nil {
				return false, err
			}
			certDeleted = true
		}
	}
	return certDeleted, nil
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

// removeCertFromStore removes the given certificate from its associated certificate store
func removeCertFromStore(store windows.Handle, cert *x509.Certificate) error {
	// Windows CertContext holds the store which imported the cert, which is needed when deleting from the system store
	certContext, err := retrieveCertContext(store, cert)
	if err != nil {
		return fmt.Errorf("failed to create content from x509 cert %s: %w", cert.Subject.String(), err)
	}
	if certContext == nil {
		// Cert already doesn't exist, nothing to do
		return nil
	}

	if err = windows.CertDeleteCertificateFromStore(certContext); err != nil {
		return fmt.Errorf("failed to remove certificate %s: %w", cert.Subject.String(), err)
	}
	klog.V(1).Infof("Certificate %s removed successfully.", cert.Subject.String())
	return nil
}

// retrieveCertContext finds the given certificate in the given store and returns its full context per Windows OS
// Returns a nil CertContext if the cert is not found.
func retrieveCertContext(store windows.Handle, targetCert *x509.Certificate) (*windows.CertContext, error) {
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
			return nil, fmt.Errorf("error reading existing system certificate: %w", err)
		}

		// Convert the certificate bytes to Golang x509.Certificate
		certBytes := unsafe.Slice(certContext.EncodedCert, certContext.Length)
		current, err := x509.ParseCertificate(certBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse certificate from bytes: %w", err)
		}

		if current.Equal(targetCert) {
			klog.Infof("found %s existing system certificate", certContext.CertInfo.Subject)
			return certContext, nil
		}
	}
	klog.Infof("target cert not found in root system store: %s", targetCert.Subject)
	return nil, nil
}

// FileIOError occurs when there is an failure interacting with the Windows filesystem
type FileIOError struct {
	err error
}

func (e *FileIOError) Error() string {
	return e.err.Error()
}

// newFileIOError returns a new FileIOError
func newFileIOError(err error) *FileIOError {
	return &FileIOError{err: err}
}

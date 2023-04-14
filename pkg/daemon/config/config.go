//go:build windows

/*
Copyright 2022.

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

package config

import (
	"fmt"
	"io/ioutil"

	"k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"
)

// FromServiceAccount uses service account credentials to create a client config able to authenticate with a cluster
func FromServiceAccount(apiServerURL, caFile, tokenFile string) (*rest.Config, error) {
	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("error reading token file %s: %w", tokenFile, err)
	}
	if _, err := certutil.NewPool(caFile); err != nil {
		return nil, fmt.Errorf("error loading CA config file %s: %w", caFile, err)
	}
	tlsClientConfig := rest.TLSClientConfig{CAFile: caFile}

	return &rest.Config{
		Host:            apiServerURL,
		TLSClientConfig: tlsClientConfig,
		BearerToken:     string(token),
		BearerTokenFile: tokenFile,
	}, nil
}

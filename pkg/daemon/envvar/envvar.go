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

package envvar

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
	"k8s.io/klog/v2"
)

// systemEnvVarRegistryPath is where system level environment variables are stored in the Windows OS
const systemEnvVarRegistryPath = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// Reconcile ensures that the proxy environment variables are set as expected on the instance
// If there's any changes, it returns true indicating an instance restart is required to ensure all processes
// pick up the updated values.
func Reconcile(envVars map[string]string, watchedEnvVars []string) (bool, error) {
	envVarsUpdated := false
	registryKey, err := registry.OpenKey(registry.LOCAL_MACHINE, systemEnvVarRegistryPath, registry.ALL_ACCESS)
	if err != nil {
		return false, fmt.Errorf("unable to open Windows system registry key %s: %w",
			systemEnvVarRegistryPath, err)
	}
	defer func() { // always close the registry key, without swallowing any error returned before the defer call
		closeErr := registryKey.Close()
		if closeErr != nil {
			klog.Errorf("could not close key %v: %v", registryKey, closeErr)
		}
	}()

	var envVarsToRemove []string
	for _, watchedEnvVar := range watchedEnvVars {
		if _, ok := envVars[watchedEnvVar]; !ok {
			envVarsToRemove = append(envVarsToRemove, watchedEnvVar)
		}
	}
	if len(envVarsToRemove) != 0 {
		envVarsUpdated, err = EnsureEnvVarsAreRemoved(registryKey, envVarsToRemove)
		if err != nil {
			return false, fmt.Errorf("error removing envionment variables %v: %v", envVarsToRemove, err)
		}
	}

	for key, expectedVal := range envVars {
		actualVal, _, err := registryKey.GetStringValue(key)
		if err != nil && err != registry.ErrNotExist {
			return false, fmt.Errorf("unable to read environment variable %s: %w", key, err)
		}

		if actualVal != expectedVal {
			klog.Infof("updating environment variable %s", key)
			// Because we modify env vars are the "system" level rather than the ephemeral "process" level,
			// we cannot use os.Setenv, which is a wrapper for syscall.SetEnvironmentVariable
			// As per Microsoft docs: "Calling SetEnvironmentVariable has no effect on the system environment variables"
			err := registryKey.SetStringValue(key, expectedVal)
			if err != nil {
				// Do not log value as proxy information is sensitive
				return false, fmt.Errorf("unable to set environment variable %s: %w", key, err)
			}
			envVarsUpdated = true
		}
	}
	return envVarsUpdated, nil
}

// EnsureEnvVarsAreRemoved ensures that the given environment variables are removed from the instance's Windows registry
// An instance restart is required after they are removed to ensure all processes pick up the updated values.
func EnsureEnvVarsAreRemoved(registryKey registry.Key, envVarsToRemove []string) (bool, error) {
	envVarsRemoved := false
	for _, envVar := range envVarsToRemove {
		err := registryKey.DeleteValue(envVar)
		if err != nil {
			if err != registry.ErrNotExist {
				return false, err
			}
		} else {
			klog.Infof("Removed environment variable %s", envVar)
			envVarsRemoved = true
		}
	}
	return envVarsRemoved, nil
}

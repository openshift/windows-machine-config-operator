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

package main

import (
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/cleanup"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/config"
)

var (
	cleanupCmd = &cobra.Command{
		Use:   "cleanup",
		Short: "Cleans up WMCO-managed Windows services",
		Long: "Stops and removes all Windows services as part of Node deconfiguration, " +
			"according to information given by Windows Service ConfigMaps present within the cluster",
		Run: runCleanupCmd,
	}
	// preserveNode is an optional flag that instructs WICD to deconfigure an instance without deleting the Node object.
	// This is useful in the node upgrade scenario.
	preserveNode bool
)

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.PersistentFlags().BoolVar(&preserveNode, "preserveNode", false,
		"If set to true, preserves the Node associated with this instance. Defaults to false.")
}

func runCleanupCmd(cmd *cobra.Command, args []string) {
	cfg, err := config.FromServiceAccount(apiServerURL, saCA, saToken)
	if err != nil {
		klog.Exitf("error using service account to build config: %s", err.Error())
	}
	ctx := ctrl.SetupSignalHandler()
	if err := cleanup.Deconfigure(cfg, ctx, preserveNode, namespace); err != nil {
		klog.Exitf(err.Error())
	}
}

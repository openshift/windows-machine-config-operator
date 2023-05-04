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
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/cleanup"
)

var (
	cleanupCmd = &cobra.Command{
		Use:   "cleanup",
		Short: "Cleans up WMCO-managed Windows services",
		Long: "Stops and removes all Windows services as part of Node deconfiguration, " +
			"according to information given by Windows Service ConfigMaps present within the cluster",
		Run: runCleanupCmd,
	}
)

func init() {
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanupCmd(cmd *cobra.Command, args []string) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Exitf("error building config: %s", err.Error())
	}
	ctx := ctrl.SetupSignalHandler()
	if err := cleanup.Deconfigure(cfg, ctx, namespace); err != nil {
		klog.Exitf(err.Error())
	}
}

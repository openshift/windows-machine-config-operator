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

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/controller"
)

var (
	bootstrapCmd = &cobra.Command{
		Use:   "bootstrap",
		Short: "Starts required Windows services to bootstrap a Node.",
		Long: "Starts all Windows services on an instance that are pre-requisites for Node object creation. " +
			"Services are configured according to the information given by the Windows services ConfigMap. " +
			"Requires a desired version argument, specifying which ConfigMap to use as the source of truth.",
		Run: runBootstrapCmd,
	}
	desiredVersion string
)

func init() {
	rootCmd.AddCommand(bootstrapCmd)
	bootstrapCmd.PersistentFlags().StringVar(&desiredVersion, "desired-version", "",
		"Version of the services ConfigMap to use as the source of truth for service configuration")
	bootstrapCmd.MarkPersistentFlagRequired("desired-version")
}

// runBootstrapCmd runs WICD's one-shot bootstrap operation, starting services as per the desired services ConfigMap
func runBootstrapCmd(cmd *cobra.Command, args []string) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Exitf("error building config: %s", err.Error())
	}
	sc, err := controller.NewServiceController(ctrl.SetupSignalHandler(), "", namespace, controller.Options{Config: cfg})
	if err != nil {
		klog.Exitf("error creating Service Controller: %s", err.Error())
	}
	klog.Info("bootstrapping Windows instance")
	if err := sc.Bootstrap(desiredVersion); err != nil {
		klog.Exit(err.Error())
	}
}

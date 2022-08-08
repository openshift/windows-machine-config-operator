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
	"context"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/config"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/controller"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/manager"
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
	cfg, err := config.FromServiceAccount(apiServerURL, saCA, saToken)
	if err != nil {
		klog.Exitf("error using service account to build config: %s", err.Error())
	}

	// Cannot use a cached client as no manager will be started to populate cache
	directClient, err := controller.NewDirectClient(cfg)
	if err != nil {
		klog.Exitf("could not create authenticated client from service account: %s", err.Error())
	}

	svcMgr, err := manager.New()
	if err != nil {
		klog.Exitf("could not create service manager: %s", err.Error())
	}
	sc := controller.NewServiceController(context.TODO(), directClient, svcMgr, "")

	klog.Info("bootstrapping Windows instance")
	if err := sc.Bootstrap(desiredVersion); err != nil {
		klog.Exit(err.Error())
	}
}

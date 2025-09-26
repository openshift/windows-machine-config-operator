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
	"flag"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/controller"
)

var (
	controllerCmd = &cobra.Command{
		Use:   "controller",
		Short: "Manages local Windows Services",
		Long: "Manages the state of Windows Services, according to information given by Windows Service ConfigMaps " +
			"present within the cluster",
		Run: runControllerCmd,
	}
	windowsService bool
	logDir         string
	caBundle       string
	// Certificate-based authentication options
	certDir      string
	certDuration string
)

func init() {
	rootCmd.AddCommand(controllerCmd)
	controllerCmd.PersistentFlags().StringVar(&logDir, "log-dir", "", "Directory to write logs to, "+
		"if not provided, the command will log to stdout/stderr")
	controllerCmd.PersistentFlags().BoolVar(&windowsService, "windows-service", false,
		"Enables running as a Windows service")
	controllerCmd.PersistentFlags().StringVar(&caBundle, "ca-bundle", "",
		"the full path to CA bundle file containing certificates trusted by the cluster")
	controllerCmd.PersistentFlags().StringVar(&certDir, "cert-dir", "C:\\k\\wicd-certs",
		"Directory to store WICD client certificates")
	controllerCmd.PersistentFlags().StringVar(&certDuration, "cert-duration", "1h",
		"Duration for WICD certificates (e.g., 10m, 1h, 24h)")
}

func runControllerCmd(cmd *cobra.Command, args []string) {
	if logDir != "" {
		var fs flag.FlagSet
		klog.InitFlags(&fs)
		// When the logtostderr flag is set to true, which is the default, the log_dir arg is ignored
		fs.Set("logtostderr", "false")
		fs.Set("log_dir", logDir)
	}
	duration, err := time.ParseDuration(certDuration)
	if err != nil {
		klog.Errorf("invalid cert-duration %s: %v", certDuration, err)
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if windowsService {
		if err := initService(ctx); err != nil {
			klog.Error(err)
			os.Exit(1)
		}
	}
	klog.Info("service controller running")
	if err := controller.RunController(ctx, namespace, kubeconfig, caBundle, certDir, duration); err != nil {
		klog.Error(err)
		os.Exit(1)
	}
}

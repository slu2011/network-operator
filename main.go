/*
Copyright 2021.

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

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	netattdefv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	osconfigv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mellanoxcomv1alpha1 "github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/Mellanox/network-operator/controllers"
	"github.com/Mellanox/network-operator/pkg/upgrade"
	"github.com/Mellanox/network-operator/pkg/utils"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(mellanoxcomv1alpha1.AddToScheme(scheme))
	utilruntime.Must(netattdefv1.AddToScheme(scheme))
	utilruntime.Must(osconfigv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func setupCRDControllers(mgr ctrl.Manager) error {
	if err := (&controllers.NicClusterPolicyReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("NicClusterPolicy"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NicClusterPolicy")
		return err
	}
	if err := (&controllers.MacvlanNetworkReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("MacvlanNetwork"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MacvlanNetwork")
		return err
	}
	if err := (&controllers.HostDeviceNetworkReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("HostDeviceNetwork"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HostDeviceNetwork")
		return err
	}
	if err := (&controllers.IPoIBNetworkReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("IPoIBNetwork"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "IPoIBNetwork")
		return err
	}
	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "12620820.mellanox.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	err = setupCRDControllers(mgr)
	if err != nil {
		os.Exit(1)
	}

	upgradeLogger := ctrl.Log.WithName("controllers").WithName("Upgrade")
	k8sInterface, err := utils.CreateK8sInterface()
	if err != nil {
		setupLog.Error(err, "unable to create k8s interface", "controller", "Upgrade")
		os.Exit(1)
	}
	nodeUpgradeStateProvider := upgrade.NewNodeUpgradeStateProvider(
		mgr.GetClient(), upgradeLogger.WithName("nodeUpgradeStateProvider"))
	drainManager := upgrade.NewDrainManager(
		k8sInterface, nodeUpgradeStateProvider, upgradeLogger.WithName("drainManager"))
	uncordonManager := upgrade.NewUncordonManager(k8sInterface, upgradeLogger.WithName("uncordonManager"))
	podDeleteManager := upgrade.NewPodDeleteManager(mgr.GetClient(), upgradeLogger.WithName("podDeleteManager"))
	clusterUpdateStateManager := upgrade.NewClusterUpdateStateManager(
		drainManager, podDeleteManager, uncordonManager, nodeUpgradeStateProvider,
		upgradeLogger.WithName("clusterUpgradeManager"), mgr.GetClient(), k8sInterface)
	if err = (&controllers.UpgradeReconciler{
		Client:                   mgr.GetClient(),
		Log:                      upgradeLogger,
		Scheme:                   mgr.GetScheme(),
		StateManager:             clusterUpdateStateManager,
		NodeUpgradeStateProvider: nodeUpgradeStateProvider,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Upgrade")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

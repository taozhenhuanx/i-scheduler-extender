package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	"github.com/lixd96/i-scheduler-extender/internal/controller"
	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"github.com/lixd96/i-scheduler-extender/pkg/server"
	"github.com/lixd96/i-scheduler-extender/pkg/settings"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(schedulerv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var configFile string

	flag.StringVar(&configFile, "config", "/etc/i-scheduler-extender/config.yaml", "path to the operator YAML configuration")
	flag.Parse()

	ctrl.SetLogger(klogr.New())

	if err := run(configFile); err != nil {
		setupLog.Error(err, "operator stopped")
		os.Exit(1)
	}
}

func run(configFile string) error {
	appSettings, err := settings.Load(configFile)
	if err != nil {
		return err
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes client configuration: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}

	operatorExtender := extender.NewExtenderWithClients(
		clientset,
		dynamicClient,
		float64(schedulerv1alpha1.DefaultCPUWeightPercent)/100,
		float64(schedulerv1alpha1.DefaultMemoryWeightPercent)/100,
	)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: appSettings.MetricsBindAddress},
		HealthProbeBindAddress: appSettings.HealthProbeBindAddress,
		LeaderElection:         appSettings.LeaderElection,
		LeaderElectionID:       "scheduler.example.com-scheduler-policy",
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}

	reconciler := &controller.SchedulerPolicyReconciler{
		Client:           mgr.GetClient(),
		KubernetesClient: clientset,
		DynamicClient:    dynamicClient,
		Extender:         operatorExtender,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up scheduler policy controller: %w", err)
	}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return serveExtender(ctx, appSettings.ExtenderBindAddress, server.NewHandler(operatorExtender).Routes())
	})); err != nil {
		return fmt.Errorf("add scheduler extender HTTP server: %w", err)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readiness check: %w", err)
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}

func serveExtender(ctx context.Context, address string, handler http.Handler) error {
	httpServer := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverError := make(chan error, 1)
	go func() {
		serverError <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverError:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return err
		}
		if err := <-serverError; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

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

	// 监听地址和 leader election 从启动配置读取；调度策略由 SchedulerPolicy CR 管理。
	flag.StringVar(&configFile, "config", "/etc/i-scheduler-extender/config.yaml", "path to the operator YAML configuration")
	flag.Parse()

	ctrl.SetLogger(klogr.New())

	if err := run(configFile); err != nil {
		setupLog.Error(err, "operator stopped")
		os.Exit(1)
	}
}

// run 创建 Kubernetes 客户端和 controller-runtime manager，并注册策略控制器与 Extender 服务。
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

	// CR 首次协调前使用默认权重，之后由 SchedulerPolicy 更新 Extender 权重。
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

	// 首次权重加载与后续协调串行衔接，避免启动时的旧读结果覆盖新的策略。
	startupReady := make(chan struct{})
	reconciler := &controller.SchedulerPolicyReconciler{
		Client:           mgr.GetClient(),
		KubernetesClient: clientset,
		DynamicClient:    dynamicClient,
		Extender:         operatorExtender,
		StartupReady:     startupReady,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up scheduler policy controller: %w", err)
	}
	// 服务仅在 leader 上监听；先读取现有策略、恢复内存预留，再允许 readiness 通过。
	extenderServer := newExtenderHTTPServer(
		appSettings.ExtenderBindAddress,
		server.NewHandler(operatorExtender).Routes(),
		func(ctx context.Context) error {
			if err := initializePolicyWeights(ctx, mgr.GetAPIReader(), operatorExtender); err != nil {
				return err
			}
			if err := operatorExtender.InitializeMemoryReservations(ctx); err != nil {
				return err
			}
			close(startupReady)
			return nil
		},
	)
	if err := mgr.Add(extenderServer); err != nil {
		return fmt.Errorf("add scheduler extender HTTP server: %w", err)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("extender", extenderServer.Ready); err != nil {
		return fmt.Errorf("add readiness check: %w", err)
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}

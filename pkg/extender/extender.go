package extender

import (
	"log"
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const Label = "priority.lixueduan.com"

// Extender 提供 Scheduler Extender 接口，并复用 Kubernetes 客户端给节点负载保护器读取节点和 metrics.k8s.io 数据。
type Extender struct {
	// ClientSet 用于访问 Kubernetes 核心 API，例如 Node 和 Pod。
	ClientSet kubernetes.Interface
	// DynamicClient 用于访问 metrics.k8s.io 等非核心 API。
	DynamicClient dynamic.Interface
}

func NewExtender() *Extender {
	config, err := NewConfig()
	if err != nil {
		log.Fatalf("failed to create kubernetes config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create k8s clientset: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create dynamic client: %v", err)
	}
	return &Extender{ClientSet: clientset, DynamicClient: dynamicClient}
}

// NewConfig 集群内优先使用 ServiceAccount；本地运行时回退到 kubeconfig。
func NewConfig() (*rest.Config, error) {
	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig == "" {
		kubeConfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
		if err != nil {
			return nil, err
		}
	}
	return config, nil
}

// NewClient 保留原有 API，方便其他代码直接创建 typed clientset。
func NewClient() (kubernetes.Interface, error) {
	config, err := NewConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

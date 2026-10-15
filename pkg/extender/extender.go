package extender

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const nodeMetricsCacheTTL = 5 * time.Second
const nodeMetricsErrorCacheTTL = 2 * time.Second

// Extender 提供 Scheduler Extender 接口，并复用 Kubernetes 客户端读取节点和 metrics.k8s.io 数据。
type Extender struct {
	// ClientSet 用于访问 Kubernetes 核心 API，例如 Node 和 Pod。
	ClientSet kubernetes.Interface
	// DynamicClient 用于访问 metrics.k8s.io 等非核心 API。
	DynamicClient dynamic.Interface

	weightsMu    sync.RWMutex
	cpuWeight    float64
	memoryWeight float64

	admissionMu    sync.Mutex
	metricsMu      sync.Mutex
	metricsAt      time.Time
	metricsErr     error
	metricsByNode  map[string]map[string]interface{}
	podMemoryMu    sync.Mutex
	podMemoryAt    time.Time
	podMemory      *podMemorySnapshot
	pendingMemMu   sync.Mutex
	pendingMemory  map[string]pendingMemoryReservation
	filterTieBreak atomic.Uint64
}

// SetWeights updates the CPU and memory scoring weights applied to new requests.
// SetWeights 在线更新 CPU 与内存评分权重；并发评分通过读写锁读取一致的权重。
func (ex *Extender) SetWeights(cpuWeight, memoryWeight float64) error {
	cpuWeight, memoryWeight, err := nodeusage.ParseWeights(
		strconv.FormatFloat(cpuWeight, 'f', -1, 64),
		strconv.FormatFloat(memoryWeight, 'f', -1, 64),
	)
	if err != nil {
		return err
	}
	ex.weightsMu.Lock()
	ex.cpuWeight = cpuWeight
	ex.memoryWeight = memoryWeight
	ex.weightsMu.Unlock()
	return nil
}

func (ex *Extender) scoringWeights() (float64, float64) {
	ex.weightsMu.RLock()
	defer ex.weightsMu.RUnlock()
	return ex.cpuWeight, ex.memoryWeight
}

func NewExtender() *Extender {
	return NewExtenderWithWeights(0.1, 0.9)
}

// NewExtenderWithWeights creates an extender whose scoring weights match the
// node load protection configuration.
func NewExtenderWithWeights(cpuWeight, memoryWeight float64) *Extender {
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
	return NewExtenderWithClients(clientset, dynamicClient, cpuWeight, memoryWeight)
}

// NewExtenderWithClients builds an extender with clients owned by the operator manager.
// NewExtenderWithClients 复用 manager 已创建的客户端，避免 Extender 再单独加载 kubeconfig。
func NewExtenderWithClients(clientset kubernetes.Interface, dynamicClient dynamic.Interface, cpuWeight, memoryWeight float64) *Extender {
	return &Extender{
		ClientSet:     clientset,
		DynamicClient: dynamicClient,
		cpuWeight:     cpuWeight,
		memoryWeight:  memoryWeight,
	}
}

// nodeMetrics 读取并缓存节点 Metrics API 数据，避免每次 Filter 和评分都重复请求。
func (ex *Extender) nodeMetrics(ctx context.Context) (map[string]map[string]interface{}, error) {
	ex.metricsMu.Lock()
	defer ex.metricsMu.Unlock()

	if !ex.metricsAt.IsZero() {
		age := time.Since(ex.metricsAt)
		if ex.metricsErr != nil && age < nodeMetricsErrorCacheTTL {
			return nil, ex.metricsErr
		}
		if ex.metricsErr == nil && ex.metricsByNode != nil && age < nodeMetricsCacheTTL {
			return ex.metricsByNode, nil
		}
	}
	if ex.DynamicClient == nil {
		ex.metricsAt = time.Now()
		ex.metricsErr = fmt.Errorf("dynamic Kubernetes client is unavailable")
		return nil, ex.metricsErr
	}
	metrics, err := ex.DynamicClient.Resource(nodeusage.NodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		ex.metricsByNode = nil
		ex.metricsAt = time.Now()
		ex.metricsErr = err
		return nil, err
	}
	metricsByNode := make(map[string]map[string]interface{}, len(metrics.Items))
	for _, metric := range metrics.Items {
		metricsByNode[metric.GetName()] = metric.Object
	}
	ex.metricsByNode = metricsByNode
	ex.metricsAt = time.Now()
	ex.metricsErr = nil
	return ex.metricsByNode, nil
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

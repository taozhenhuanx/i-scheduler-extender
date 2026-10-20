// Package nodeusage 为 Extender 和节点负载保护提供统一的资源用量、预算及采样校验。
package nodeusage

import (
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NodeMetricsGVR 指向 Metrics API 的节点指标资源，用于读取实测 CPU 和内存用量。
var NodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

const (
	MemoryBlockedLabel = "scheduler.example.com/memory-blocked"
	// Extender 准入和节点可用性计数共用采样校验，避免旧指标被当成可用容量。
	MemorySampleMaxAge = 2 * time.Minute
	MemoryClockSkew    = 10 * time.Second
)

// MemorySampleStart 校验节点指标的时效并返回完整采样窗口的起点。
// Extender 用起点确认容器启动已被采样覆盖；Balancer 用同一规则排除缺失或陈旧指标。
func MemorySampleStart(metric map[string]interface{}, now time.Time) (time.Time, error) {
	timestamp, ok := metric["timestamp"].(string)
	if !ok {
		return time.Time{}, fmt.Errorf("missing metrics timestamp")
	}
	sampledAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid metrics timestamp: %w", err)
	}
	if sampledAt.After(now.Add(MemoryClockSkew)) || now.Sub(sampledAt) > MemorySampleMaxAge {
		return time.Time{}, fmt.Errorf("metrics timestamp %s is stale or in the future", timestamp)
	}
	window, ok := metric["window"].(string)
	if !ok {
		return time.Time{}, fmt.Errorf("missing metrics window")
	}
	duration, err := time.ParseDuration(window)
	if err != nil || duration <= 0 || duration > MemorySampleMaxAge {
		return time.Time{}, fmt.Errorf("invalid metrics window %q", window)
	}
	// 接受小幅时钟偏差，但不能用未来样本提前释放容器预算。
	if sampledAt.After(now) {
		sampledAt = now
	}
	return sampledAt.Add(-duration), nil
}

// ValidateWeights 校验运行时权重，输入始终是 0..1 的比例，禁止猜测百分比单位。
func ValidateWeights(cpuWeight, memoryWeight float64) error {
	for _, weight := range []float64{cpuWeight, memoryWeight} {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 || weight > 1 {
			return fmt.Errorf("cpu and memory weights must each be finite and between 0 and 1")
		}
	}
	if math.Abs(cpuWeight+memoryWeight-1) > 1e-9 {
		return fmt.Errorf("cpu and memory weights must sum to 1")
	}
	return nil
}

// WeightedUsagePercentage 用配置权重合并节点 CPU 和内存使用率，供调度评分和负载均衡共用。
func WeightedUsagePercentage(node corev1.Node, metric map[string]interface{}, cpuWeight, memoryWeight float64) (float64, error) {
	usage, ok, err := unstructured.NestedMap(metric, "usage")
	if err != nil || !ok {
		return 0, fmt.Errorf("metrics usage is missing")
	}
	cpu, err := metricQuantity(usage, "cpu")
	if err != nil {
		return 0, fmt.Errorf("parse cpu usage: %w", err)
	}
	memory, err := metricQuantity(usage, "memory")
	if err != nil {
		return 0, fmt.Errorf("parse memory usage: %w", err)
	}
	allocatableCPU := node.Status.Allocatable[corev1.ResourceCPU]
	allocatableMemory := node.Status.Allocatable[corev1.ResourceMemory]
	if allocatableCPU.Sign() <= 0 || allocatableMemory.Sign() <= 0 {
		return 0, fmt.Errorf("node allocatable CPU and memory must be positive")
	}
	if err := ValidateWeights(cpuWeight, memoryWeight); err != nil {
		return 0, err
	}
	totalWeight := cpuWeight + memoryWeight
	cpuPercentage := float64(cpu.MilliValue()) / float64(allocatableCPU.MilliValue()) * 100
	memoryPercentage := float64(memory.Value()) / float64(allocatableMemory.Value()) * 100
	return (cpuPercentage*cpuWeight + memoryPercentage*memoryWeight) / totalWeight, nil
}

// MemoryUsageBytes 解析指标中的节点实测内存，返回字节数，拒绝缺失、非法或负数用量。
func MemoryUsageBytes(metric map[string]interface{}) (int64, error) {
	usage, ok, err := unstructured.NestedMap(metric, "usage")
	if err != nil || !ok {
		return 0, fmt.Errorf("metrics usage is missing")
	}
	memory, err := metricQuantity(usage, "memory")
	if err != nil {
		return 0, fmt.Errorf("parse memory usage: %w", err)
	}
	return memory.Value(), nil
}

// MemoryAllocatableBytes 返回节点可分配内存，作为准入占比的分母，容量必须为正数。
func MemoryAllocatableBytes(node corev1.Node) (int64, error) {
	allocatable := node.Status.Allocatable[corev1.ResourceMemory]
	if allocatable.Sign() <= 0 {
		return 0, fmt.Errorf("node allocatable memory must be positive")
	}
	return allocatable.Value(), nil
}

// PodMemoryReservationBytes 优先按 request 统计调度预留；没有正数 request 时回退到 limit，并计入 InitContainer 峰值及 Pod Overhead。
func PodMemoryReservationBytes(pod corev1.Pod) int64 {
	appMemory := resource.Quantity{}
	for _, container := range pod.Spec.Containers {
		appMemory.Add(containerMemoryReservation(container))
	}

	sidecarMemory := resource.Quantity{}
	initPeak := resource.Quantity{}
	for _, container := range pod.Spec.InitContainers {
		containerMemory := containerMemoryReservation(container)
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			// 可重启 init 容器作为 sidecar 持续运行，后续 init 和应用阶段都需要加上它。
			sidecarMemory.Add(containerMemory)
			if sidecarMemory.Cmp(initPeak) > 0 {
				initPeak = sidecarMemory.DeepCopy()
			}
			continue
		}
		// 普通 init 依次运行，本阶段预算为当前 init 加上此前启动的所有 sidecar。
		initStageMemory := sidecarMemory.DeepCopy()
		initStageMemory.Add(containerMemory)
		if initStageMemory.Cmp(initPeak) > 0 {
			initPeak = initStageMemory
		}
	}

	// 取应用阶段与 init 阶段的较大预算，再计入 Pod Overhead。
	appMemory.Add(sidecarMemory)
	if initPeak.Cmp(appMemory) > 0 {
		appMemory = initPeak
	}
	if overhead, ok := pod.Spec.Overhead[corev1.ResourceMemory]; ok {
		appMemory.Add(overhead)
	}
	return appMemory.Value()
}

// containerMemoryReservation 优先使用正数 request，仅在缺省或非正数时回退 limit，保留超卖空间。
func containerMemoryReservation(container corev1.Container) resource.Quantity {
	request := container.Resources.Requests[corev1.ResourceMemory]
	if request.Sign() > 0 {
		return request.DeepCopy()
	}
	return container.Resources.Limits[corev1.ResourceMemory].DeepCopy()
}

func metricQuantity(usage map[string]interface{}, name string) (resource.Quantity, error) {
	raw, ok := usage[name].(string)
	if !ok {
		return resource.Quantity{}, fmt.Errorf("%s is missing or is not a string", name)
	}
	quantity, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, err
	}
	if quantity.Sign() < 0 {
		return resource.Quantity{}, fmt.Errorf("%s usage must not be negative", name)
	}
	return quantity, nil
}

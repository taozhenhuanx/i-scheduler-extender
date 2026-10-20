// Package nodeusage contains shared helpers for calculating node resource usage.
package nodeusage

import (
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NodeMetricsGVR identifies node usage resources in the Kubernetes Metrics API.
var NodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

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
	if allocatableCPU.IsZero() || allocatableMemory.IsZero() {
		return 0, fmt.Errorf("node allocatable CPU or memory is zero")
	}
	if err := ValidateWeights(cpuWeight, memoryWeight); err != nil {
		return 0, err
	}
	totalWeight := cpuWeight + memoryWeight
	cpuPercentage := float64(cpu.MilliValue()) / float64(allocatableCPU.MilliValue()) * 100
	memoryPercentage := float64(memory.Value()) / float64(allocatableMemory.Value()) * 100
	return (cpuPercentage*cpuWeight + memoryPercentage*memoryWeight) / totalWeight, nil
}

// MemoryUsageBytes returns the current memory usage reported for a node.
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

// MemoryAllocatableBytes returns the node's allocatable memory.
func MemoryAllocatableBytes(node corev1.Node) (int64, error) {
	allocatable := node.Status.Allocatable[corev1.ResourceMemory]
	if allocatable.IsZero() {
		return 0, fmt.Errorf("node allocatable memory is zero")
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
			sidecarMemory.Add(containerMemory)
			if sidecarMemory.Cmp(initPeak) > 0 {
				initPeak = sidecarMemory.DeepCopy()
			}
			continue
		}
		initStageMemory := sidecarMemory.DeepCopy()
		initStageMemory.Add(containerMemory)
		if initStageMemory.Cmp(initPeak) > 0 {
			initPeak = initStageMemory
		}
	}

	appMemory.Add(sidecarMemory)
	if initPeak.Cmp(appMemory) > 0 {
		appMemory = initPeak
	}
	if overhead, ok := pod.Spec.Overhead[corev1.ResourceMemory]; ok {
		appMemory.Add(overhead)
	}
	return appMemory.Value()
}

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
	return resource.ParseQuantity(raw)
}

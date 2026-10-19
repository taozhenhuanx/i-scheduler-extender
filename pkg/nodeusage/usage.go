// Package nodeusage contains shared helpers for calculating node resource usage.
package nodeusage

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// NodeMetricsGVR identifies node usage resources in the Kubernetes Metrics API.
var NodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// ParseWeights parses CPU and memory weights. Values greater than 1 are
// interpreted as percentages, so 10, 10%, and 0.1 all mean a 10% weight.
func ParseWeights(cpuRaw, memoryRaw string) (float64, float64, error) {
	cpu, err := parseWeight(cpuRaw)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid cpu weight: %w", err)
	}
	memory, err := parseWeight(memoryRaw)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid memory weight: %w", err)
	}
	if cpu+memory <= 0 {
		return 0, 0, fmt.Errorf("cpu and memory weights cannot both be zero")
	}
	return cpu, memory, nil
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
	totalWeight := cpuWeight + memoryWeight
	if totalWeight <= 0 {
		return 0, fmt.Errorf("cpu and memory weights cannot both be zero")
	}
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

func parseWeight(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("weight is empty")
	}
	if strings.HasSuffix(value, "%") {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "%")), 64)
		if err != nil {
			return 0, err
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
			return 0, fmt.Errorf("percentage weight must be between 0 and 100")
		}
		return parsed / 100, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0, fmt.Errorf("weight must be finite and non-negative")
	}
	if parsed > 1 {
		if parsed > 100 {
			return 0, fmt.Errorf("percentage weight must be at most 100")
		}
		return parsed / 100, nil
	}
	return parsed, nil
}

// Package rebalancer 实现节点负载保护逻辑。
//
// 它不驱逐已有 Pod，也不参与某个 Pod 的 Bind；只周期性检查节点实际资源使用率，
// 并通过 Node.spec.unschedulable 控制后续新 Pod 是否还能调度到该节点。
package rebalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

var (
	nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}
)

const autoCordonAnnotation = "scheduler.example.com/auto-cordoned"

type Config struct {
	CheckInterval       time.Duration
	CPUWeight           string
	MemoryWeight        string
	NodeSelector        map[string]string
	IgnoreNodes         map[string]struct{}
	HighThreshold       float64
	RecoveryThreshold   float64
	MinSchedulableNodes int
	TopNodesCount       int
}

type Balancer struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface
	config        Config
	cpuWeight     float64
	memoryWeight  float64
}

type nodeUsage struct {
	node       corev1.Node
	percentage float64
}

func New(client kubernetes.Interface, dynamicClient dynamic.Interface, config Config) (*Balancer, error) {
	if client == nil || dynamicClient == nil {
		return nil, fmt.Errorf("kubernetes and dynamic clients are required")
	}
	cpuWeight, memoryWeight, err := parseWeights(config.CPUWeight, config.MemoryWeight)
	if err != nil {
		return nil, err
	}
	if config.RecoveryThreshold < 0 {
		config.RecoveryThreshold = config.HighThreshold - 5
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Balancer{
		client:        client,
		dynamicClient: dynamicClient,
		config:        config,
		cpuWeight:     cpuWeight,
		memoryWeight:  memoryWeight,
	}, nil
}

func (b *Balancer) Run(ctx context.Context) {
	for {
		b.reconcile(ctx)
		timer := time.NewTimer(b.config.CheckInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (b *Balancer) reconcile(ctx context.Context) {
	config := b.config

	nodes, err := b.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[node-balancer] list nodes failed: %v", err)
		return
	}
	metrics, err := b.dynamicClient.Resource(nodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[node-balancer] list node metrics failed: %v", err)
		return
	}

	metricsByNode := make(map[string]unstructured.Unstructured, len(metrics.Items))
	for _, item := range metrics.Items {
		metricsByNode[item.GetName()] = item
	}
	managedNodes := make([]corev1.Node, 0, len(nodes.Items))
	usageList := make([]nodeUsage, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if _, ignored := config.IgnoreNodes[node.Name]; ignored {
			continue
		}
		if !matchesNodeSelector(node, config.NodeSelector) {
			continue
		}
		managedNodes = append(managedNodes, node)
		metric, ok := metricsByNode[node.Name]
		if !ok {
			continue
		}
		percentage, err := weightedUsagePercentage(node, metric.Object, b.cpuWeight, b.memoryWeight)
		if err != nil {
			log.Printf("[node-balancer] calculate usage for %q failed: %v", node.Name, err)
			continue
		}
		usageList = append(usageList, nodeUsage{node: node, percentage: percentage})
	}

	sort.Slice(usageList, func(i, j int) bool {
		if usageList[i].percentage == usageList[j].percentage {
			return usageList[i].node.Name < usageList[j].node.Name
		}
		return usageList[i].percentage > usageList[j].percentage
	})
	topCount := config.TopNodesCount
	if topCount > len(usageList) {
		topCount = len(usageList)
	}
	highNodes := make(map[string]bool, topCount)
	for i := 0; i < topCount; i++ {
		if usageList[i].percentage >= config.HighThreshold {
			highNodes[usageList[i].node.Name] = true
		}
	}

	schedulableCount := 0
	for _, node := range managedNodes {
		if !node.Spec.Unschedulable {
			schedulableCount++
		}
	}
	for _, item := range usageList {
		if item.node.Spec.Unschedulable && hasAutoCordonAnnotation(item.node) &&
			item.percentage <= config.RecoveryThreshold {
			if err := b.setUnschedulable(ctx, item.node, false); err != nil {
				log.Printf("[node-balancer] uncordon %q failed: %v", item.node.Name, err)
				continue
			}
			schedulableCount++
			log.Printf("[node-balancer] uncordon %q, weighted usage=%.2f%%", item.node.Name, item.percentage)
		}
	}
	for _, item := range usageList {
		if !highNodes[item.node.Name] || item.node.Spec.Unschedulable {
			continue
		}
		if schedulableCount <= config.MinSchedulableNodes {
			break
		}
		if err := b.setUnschedulable(ctx, item.node, true); err != nil {
			log.Printf("[node-balancer] cordon %q failed: %v", item.node.Name, err)
			continue
		}
		schedulableCount--
		log.Printf("[node-balancer] cordon %q, weighted usage=%.2f%%", item.node.Name, item.percentage)
	}
	log.Printf("[node-balancer] checked nodes=%d metrics=%d schedulable=%d high=%d",
		len(managedNodes), len(usageList), schedulableCount, len(highNodes))
}

func validateConfig(config Config) error {
	if config.CheckInterval <= 0 || config.MinSchedulableNodes < 1 || config.TopNodesCount < 1 {
		return fmt.Errorf("check interval, min schedulable nodes and top nodes count must be positive")
	}
	if config.HighThreshold < 0 || config.HighThreshold > 100 {
		return fmt.Errorf("high threshold must be between 0 and 100")
	}
	if config.RecoveryThreshold < 0 || config.RecoveryThreshold >= config.HighThreshold {
		return fmt.Errorf("recovery threshold must be >= 0 and lower than high threshold")
	}
	return nil
}

func parseWeights(cpuRaw, memoryRaw string) (float64, float64, error) {
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

func weightedUsagePercentage(node corev1.Node, metric map[string]interface{}, cpuWeight, memoryWeight float64) (float64, error) {
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
	cpuPercentage := float64(cpu.MilliValue()) / float64(allocatableCPU.MilliValue()) * 100
	memoryPercentage := float64(memory.Value()) / float64(allocatableMemory.Value()) * 100
	totalWeight := cpuWeight + memoryWeight
	return (cpuPercentage*cpuWeight + memoryPercentage*memoryWeight) / totalWeight, nil
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
		return parsed / 100, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if parsed > 1 {
		return parsed / 100, nil
	}
	return parsed, nil
}

func ParseNodeSelector(raw string) (map[string]string, error) {
	selector := make(map[string]string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return selector, nil
	}
	for _, expression := range strings.Split(raw, ",") {
		expression = strings.TrimSpace(expression)
		if expression == "" {
			continue
		}
		parts := strings.SplitN(expression, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("selector %q must use key=value format", expression)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" || value == "" {
			return nil, fmt.Errorf("selector %q has empty key or value", expression)
		}
		if old, exists := selector[key]; exists && old != value {
			return nil, fmt.Errorf("selector key %q has conflicting values", key)
		}
		selector[key] = value
	}
	return selector, nil
}

// ParseIgnoreNodes 将逗号分隔的节点名称解析为忽略集合。
func ParseIgnoreNodes(raw string) (map[string]struct{}, error) {
	ignored := make(map[string]struct{})
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		ignored[name] = struct{}{}
	}
	return ignored, nil
}

func matchesNodeSelector(node corev1.Node, selector map[string]string) bool {
	for key, expected := range selector {
		if node.Labels[key] != expected {
			return false
		}
	}
	return true
}

func hasAutoCordonAnnotation(node corev1.Node) bool {
	return node.Annotations != nil && node.Annotations[autoCordonAnnotation] == "true"
}

func (b *Balancer) setUnschedulable(ctx context.Context, node corev1.Node, value bool) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := b.client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		annotations := map[string]interface{}{autoCordonAnnotation: "true"}
		if !value {
			annotations[autoCordonAnnotation] = nil
		}
		patch := map[string]interface{}{
			"spec": map[string]bool{"unschedulable": value},
			"metadata": map[string]interface{}{
				"resourceVersion": current.ResourceVersion,
				"annotations":     annotations,
			},
		}
		payload, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		_, err = b.client.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, payload, metav1.PatchOptions{})
		return err
	})
}

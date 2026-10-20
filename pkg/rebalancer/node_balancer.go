// Package rebalancer 实现节点负载保护逻辑。
//
// 它不驱逐已有 Pod，也不参与某个 Pod 的 Bind；只周期性检查节点实际资源使用率，
// 并通过 Node.spec.unschedulable 控制后续新 Pod 是否还能调度到该节点。
package rebalancer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const autoCordonAnnotation = "scheduler.example.com/auto-cordoned"

type Config struct {
	CheckInterval       time.Duration
	CPUWeight           float64
	MemoryWeight        float64
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

// New 校验客户端、权重和阈值，并为后续协调构造 Balancer。
func New(client kubernetes.Interface, dynamicClient dynamic.Interface, config Config) (*Balancer, error) {
	if client == nil || dynamicClient == nil {
		return nil, fmt.Errorf("kubernetes and dynamic clients are required")
	}
	// CR 百分比只在 API 边界转换；负载保护和 Extender 使用相同的比例单位。
	if err := nodeusage.ValidateWeights(config.CPUWeight, config.MemoryWeight); err != nil {
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
		cpuWeight:     config.CPUWeight,
		memoryWeight:  config.MemoryWeight,
	}, nil
}

// Run 按策略间隔重复协调；收到 manager 的取消信号后退出。
func (b *Balancer) Run(ctx context.Context) {
	for {
		_ = b.Reconcile(ctx)
		timer := time.NewTimer(b.config.CheckInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Reconcile 执行一轮节点负载保护：筛选受管节点、计算负载，并按阈值恢复或 cordon 节点。
func (b *Balancer) Reconcile(ctx context.Context) error {
	config := b.config
	nodes, err := b.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	var reconcileErrors []error
	nodeUIDByName := make(map[string]types.UID, len(nodes.Items))
	for _, node := range nodes.Items {
		nodeUIDByName[node.Name] = node.UID
	}
	managedNodes := make([]corev1.Node, 0, len(nodes.Items))
	// 退出范围的恢复和人工 uncordon 遗留标记清理不依赖 Metrics API。
	for _, node := range nodes.Items {
		managed := b.manages(node)
		if hasAutoCordonAnnotation(node) && (!managed || !node.Spec.Unschedulable) {
			guard := func(current corev1.Node) bool {
				return !node.Spec.Unschedulable || !b.manages(current)
			}
			updated, _, cleanupErr := updateUnschedulable(ctx, b.client, node, false, guard)
			if cleanupErr != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("cleanup node %q: %w", node.Name, cleanupErr))
			} else if updated != nil {
				node = *updated
			}
		}
		if b.manages(node) {
			managedNodes = append(managedNodes, node)
		}
	}
	metrics, err := b.dynamicClient.Resource(nodeusage.NodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[node-balancer] list node metrics failed: %v", err)
		reconcileErrors = append(reconcileErrors, fmt.Errorf("list node metrics: %w", err))
		return errors.Join(reconcileErrors...)
	}

	metricsByNode := make(map[string]unstructured.Unstructured, len(metrics.Items))
	for _, item := range metrics.Items {
		metricsByNode[item.GetName()] = item
	}
	usageList := make([]nodeUsage, 0, len(nodes.Items))
	now := time.Now()
	// 缺失或非法指标按节点汇总错误；其他节点仍能继续恢复和执行安全的保护动作。
	for _, node := range managedNodes {
		metric, ok := metricsByNode[node.Name]
		if !ok {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("node %q: metrics are missing", node.Name))
			continue
		}
		if _, err := nodeusage.MemorySampleStart(metric.Object, now); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("node %q: invalid metrics sample: %w", node.Name, err))
			continue
		}
		percentage, err := nodeusage.WeightedUsagePercentage(node, metric.Object, b.cpuWeight, b.memoryWeight)
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("node %q: calculate usage: %w", node.Name, err))
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
	// 先恢复负载已低于恢复阈值的本控制器管理节点，再检查是否需要 cordon 新节点。
	for _, item := range usageList {
		if item.node.Spec.Unschedulable && hasAutoCordonAnnotation(item.node) &&
			item.percentage <= config.RecoveryThreshold {
			metric := metricsByNode[item.node.Name]
			guard := func(current corev1.Node) bool {
				if !b.manages(current) {
					return false
				}
				if _, err := nodeusage.MemorySampleStart(metric.Object, time.Now()); err != nil {
					return false
				}
				// 最新容量可能已经改变，恢复必须重新确认当前节点的负载已回落。
				percentage, err := nodeusage.WeightedUsagePercentage(current, metric.Object, b.cpuWeight, b.memoryWeight)
				return err == nil && percentage <= config.RecoveryThreshold
			}
			_, changed, err := updateUnschedulable(ctx, b.client, item.node, false, guard)
			if err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("uncordon %q: %w", item.node.Name, err))
				continue
			}
			if changed {
				log.Printf("[node-balancer] uncordon %q, weighted usage=%.2f%%", item.node.Name, item.percentage)
			}
		}
	}
	// 恢复结果和管理员的并发改动通过重新读取确认；失败或跳过恢复不增加可用计数。
	currentNodes, err := b.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		reconcileErrors = append(reconcileErrors, fmt.Errorf("list nodes before cordon: %w", err))
		return errors.Join(reconcileErrors...)
	}
	currentByNode := make(map[string]corev1.Node, len(currentNodes.Items))
	for _, node := range currentNodes.Items {
		currentByNode[node.Name] = node
	}
	highNodes := make(map[string]bool, config.TopNodesCount)
	// Top N 仅在真正可用的业务节点中选取，避免故障节点挤占保护名额。
	for _, item := range usageList {
		node, exists := currentByNode[item.node.Name]
		if !exists || node.UID != item.node.UID || !b.available(node, metricsByNode, time.Now()) {
			continue
		}
		if len(highNodes) >= config.TopNodesCount || item.percentage < config.HighThreshold {
			break
		}
		highNodes[node.Name] = true
	}
	// 同名节点重建后不能复用旧节点的指标作为本轮容量，下轮重新采样后再计入。
	countAvailable := func(currentNodes []corev1.Node) int {
		count := 0
		for _, node := range currentNodes {
			uid, exists := nodeUIDByName[node.Name]
			if exists && node.UID == uid && b.available(node, metricsByNode, time.Now()) {
				count++
			}
		}
		return count
	}
	schedulableCount := countAvailable(currentNodes.Items)
	// 每次新增 cordon 前校准整个可用集合，再在 Retry GET 中校验候选范围和状态。
	for _, item := range usageList {
		if !highNodes[item.node.Name] {
			continue
		}
		currentNodes, err = b.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("list nodes before cordon %q: %w", item.node.Name, err))
			break
		}
		schedulableCount = countAvailable(currentNodes.Items)
		if schedulableCount <= config.MinSchedulableNodes {
			break
		}
		guard := func(current corev1.Node) bool {
			if !b.available(current, metricsByNode, time.Now()) {
				return false
			}
			percentage, err := nodeusage.WeightedUsagePercentage(current, metricsByNode[current.Name].Object, b.cpuWeight, b.memoryWeight)
			return err == nil && percentage >= config.HighThreshold
		}
		_, changed, err := updateUnschedulable(ctx, b.client, item.node, true, guard)
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("cordon %q: %w", item.node.Name, err))
			// 超时或服务端/网络故障可能发生在写入之后，停止新增 cordon 防止重复消耗容量。
			if !definitelyRejected(err) {
				break
			}
			continue
		}
		if changed {
			schedulableCount--
			log.Printf("[node-balancer] cordon %q, weighted usage=%.2f%%", item.node.Name, item.percentage)
		}
	}
	log.Printf("[node-balancer] checked nodes=%d metrics=%d schedulable=%d high=%d",
		len(managedNodes), len(usageList), schedulableCount, len(highNodes))
	return errors.Join(reconcileErrors...)
}

func (b *Balancer) manages(node corev1.Node) bool {
	_, ignored := b.config.IgnoreNodes[node.Name]
	return !ignored && matchesNodeSelector(node, b.config.NodeSelector)
}

// available 统计可承接普通业务 Pod 的节点；允许专用 Pod 的 taint 不作为通用容量。
func (b *Balancer) available(node corev1.Node, metrics map[string]unstructured.Unstructured, now time.Time) bool {
	if !b.manages(node) || node.DeletionTimestamp != nil || node.Spec.Unschedulable ||
		node.Labels[nodeusage.MemoryBlockedLabel] == "true" {
		return false
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
			break
		}
	}
	if !ready {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	metric, exists := metrics[node.Name]
	if !exists {
		return false
	}
	if _, err := nodeusage.MemorySampleStart(metric.Object, now); err != nil {
		return false
	}
	_, err := nodeusage.WeightedUsagePercentage(node, metric.Object, b.cpuWeight, b.memoryWeight)
	return err == nil
}

func definitelyRejected(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsNotFound(err) || apierrors.IsConflict(err) ||
		apierrors.IsMethodNotSupported(err) || apierrors.IsRequestEntityTooLargeError(err)
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
		actual, exists := node.Labels[key]
		if !exists || actual != expected {
			return false
		}
	}
	return true
}

func hasAutoCordonAnnotation(node corev1.Node) bool {
	return node.Annotations != nil && node.Annotations[autoCordonAnnotation] == "true"
}

func (b *Balancer) setUnschedulable(ctx context.Context, node corev1.Node, value bool) error {
	return setUnschedulable(ctx, b.client, node, value)
}

// UncordonAutoCordonedNodes restores nodes this controller cordoned before the
// SchedulerPolicy that owned the protection is deleted.
// 删除策略时只恢复带有本控制器标记的节点，避免改动管理员手动 cordon 的节点。
func UncordonAutoCordonedNodes(ctx context.Context, client kubernetes.Interface) error {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes for policy cleanup: %w", err)
	}
	var cleanupErrors []error
	for _, node := range nodes.Items {
		if !hasAutoCordonAnnotation(node) {
			continue
		}
		if err := setUnschedulable(ctx, client, node, false); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("uncordon %q: %w", node.Name, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

// setUnschedulable 通过带 resourceVersion 的合并补丁更新节点，并在并发冲突时重试。
func setUnschedulable(ctx context.Context, client kubernetes.Interface, node corev1.Node, value bool) error {
	_, _, err := updateUnschedulable(ctx, client, node, value, nil)
	return err
}

// updateUnschedulable 每次冲突重试都重新核对 UID 和所有权，避免覆盖管理员的 cordon。
// guard 在新的读取结果上检查范围/健康；changed 只表示确认过的实际变更。
func updateUnschedulable(ctx context.Context, client kubernetes.Interface, node corev1.Node, value bool,
	guard func(corev1.Node) bool) (*corev1.Node, bool, error) {
	var result *corev1.Node
	changed := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		result = current
		if current.UID != node.UID {
			return fmt.Errorf("node %q UID changed from %q to %q", node.Name, node.UID, current.UID)
		}
		if guard != nil && !guard(*current) {
			return nil
		}
		if value && current.Spec.Unschedulable {
			return nil // 已人工或自动 cordon，均不抢占所有权。
		}
		if !value && !hasAutoCordonAnnotation(*current) {
			return nil
		}
		annotations := map[string]interface{}{autoCordonAnnotation: "true"}
		if !value {
			annotations[autoCordonAnnotation] = nil
		}
		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"resourceVersion": current.ResourceVersion,
				"annotations":     annotations,
			},
		}
		// 旧快照已经被人工 uncordon 时只删除陈旧标记，即使其后又被人工 cordon。
		if value || node.Spec.Unschedulable {
			patch["spec"] = map[string]bool{"unschedulable": value}
		}
		payload, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		result, err = client.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, payload, metav1.PatchOptions{})
		if err == nil {
			changed = result != nil && (result.Spec.Unschedulable != current.Spec.Unschedulable ||
				hasAutoCordonAnnotation(*result) != hasAutoCordonAnnotation(*current))
		}
		return err
	})
	return result, changed, err
}

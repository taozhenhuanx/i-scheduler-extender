package extender

import (
	"context"
	"fmt"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Filter 使用后台上下文执行内存准入过滤。
func (ex *Extender) Filter(args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	return ex.FilterWithContext(context.Background(), args)
}

// FilterWithContext 按节点当前内存和 Pod 内存预算估算调度后的占用。
// 预测值超过 80% 时给节点打保护标签并过滤；当前占用降到 75% 且新 Pod 放入后不超 80% 才恢复。
func (ex *Extender) FilterWithContext(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	if args.Nodes == nil {
		return &extenderv1.ExtenderFilterResult{Error: "nodes is nil in extender args"}
	}
	if args.Pod == nil {
		return &extenderv1.ExtenderFilterResult{Error: "pod is nil in extender args; cannot estimate memory usage"}
	}
	if len(args.Nodes.Items) == 0 {
		return &extenderv1.ExtenderFilterResult{Error: "no candidate nodes"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ex.admissionMu.Lock()
	defer ex.admissionMu.Unlock()

	podMemory := nodeusage.PodMemoryReservationBytes(*args.Pod)
	if podMemory <= 0 {
		return &extenderv1.ExtenderFilterResult{
			Error: fmt.Sprintf("pod %s/%s must set a memory request or limit for the %.0f%% node memory guard", args.Pod.Namespace, args.Pod.Name, memoryBlockThresholdPercent),
		}
	}
	metricsByNode, err := ex.nodeMetrics(ctx)
	if err != nil {
		return &extenderv1.ExtenderFilterResult{Error: fmt.Sprintf("cannot read node memory metrics: %v", err)}
	}
	pendingByNode, err := ex.pendingMemoryByNode(ctx, metricsByNode)
	if err != nil {
		return &extenderv1.ExtenderFilterResult{Error: fmt.Sprintf("cannot read Pod assignment snapshot: %v", err)}
	}

	nodes := make([]v1.Node, 0, len(args.Nodes.Items))
	nodeNames := make([]string, 0, len(args.Nodes.Items))
	reservedNode := ex.pendingPodNode(args.Pod)
	for _, node := range args.Nodes.Items {
		if reservedNode != "" && reservedNode != node.Name {
			continue
		}
		metric, ok := metricsByNode[node.Name]
		if !ok {
			klog.V(4).Infof("[Filter] metrics missing for node %q; excluding it from memory-safe candidates", node.Name)
			continue
		}
		if _, err := memorySampleStart(metric, time.Now()); err != nil {
			klog.Warningf("[Filter] excluding node %q with invalid memory sample: %v", node.Name, err)
			continue
		}
		memoryUsed, err := nodeusage.MemoryUsageBytes(metric)
		if err != nil {
			klog.V(4).Infof("[Filter] memory usage for node %q unavailable: %v", node.Name, err)
			continue
		}
		memoryCapacity, err := nodeusage.MemoryAllocatableBytes(node)
		if err != nil {
			klog.V(4).Infof("[Filter] memory capacity for node %q unavailable: %v", node.Name, err)
			continue
		}

		memoryUsed += pendingByNode[node.Name] - ex.ownPendingMemory(args.Pod, node.Name)
		projectedMemory := memoryUsed + podMemory
		currentPercent := memoryPercent(memoryUsed, memoryCapacity)
		projectedPercent := memoryPercent(projectedMemory, memoryCapacity)
		isBlocked := node.Labels[memoryBlockedLabel] == "true"

		switch decideMemoryGuard(currentPercent, projectedPercent, isBlocked) {
		case memoryGuardKeepBlocked:
			continue
		case memoryGuardRecover:
			if err := ex.setMemoryBlocked(ctx, node.Name, false); err != nil {
				klog.Errorf("[Filter] failed to clear memory block label on node %q: %v", node.Name, err)
				continue
			}
		case memoryGuardBlock:
			if err := ex.setMemoryBlocked(ctx, node.Name, true); err != nil {
				klog.Errorf("[Filter] failed to set memory block label on node %q: %v", node.Name, err)
			}
			klog.V(2).Infof("[Filter] excluded node=%q and set %s=true; projected memory=%.2f%%", node.Name, memoryBlockedLabel, projectedPercent)
			continue
		}

		nodes = append(nodes, node)
		nodeNames = append(nodeNames, node.Name)
	}

	if len(nodes) == 0 {
		filteredNodes := *args.Nodes
		filteredNodes.Items = nodes
		return &extenderv1.ExtenderFilterResult{Nodes: &filteredNodes, NodeNames: &nodeNames}
	}
	filteredNodes := *args.Nodes
	filteredNodes.Items = nodes
	return &extenderv1.ExtenderFilterResult{Nodes: &filteredNodes, NodeNames: &nodeNames}
}

// FilterOnlyOne 使用后台上下文执行内存过滤和单节点选择。
func (ex *Extender) FilterOnlyOne(args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	return ex.FilterOnlyOneWithContext(context.Background(), args)
}

// FilterOnlyOneWithContext 先执行内存保护过滤，再从安全候选节点中选动态分数最高的节点。
func (ex *Extender) FilterOnlyOneWithContext(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.ExtenderFilterResult {
	filtered := ex.FilterWithContext(ctx, args)
	if filtered.Error != "" || filtered.Nodes == nil || len(filtered.Nodes.Items) == 0 {
		return filtered
	}

	filteredArgs := args
	filteredArgs.Nodes = filtered.Nodes
	scores := ex.PrioritizeWithContext(ctx, filteredArgs)
	if scores == nil || len(*scores) == 0 {
		return filtered
	}
	scoreList := *scores
	maxScore := scoreList[0].Score
	bestNames := make([]string, 0, len(scoreList))
	for _, score := range scoreList {
		if score.Score > maxScore {
			maxScore = score.Score
			bestNames = bestNames[:0]
		}
		if score.Score == maxScore {
			bestNames = append(bestNames, score.Host)
		}
	}
	chosenName := bestNames[(ex.filterTieBreak.Add(1)-1)%uint64(len(bestNames))]
	for _, node := range filtered.Nodes.Items {
		if node.Name == chosenName {
			filtered.Nodes.Items = []v1.Node{node}
			filtered.NodeNames = &[]string{node.Name}
			return filtered
		}
	}
	filtered.Error = fmt.Sprintf("selected node %q is missing from memory-safe candidates", chosenName)
	return filtered
}

func memoryPercent(usedBytes, capacityBytes int64) float64 {
	return float64(usedBytes) / float64(capacityBytes) * 100
}

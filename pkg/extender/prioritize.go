package extender

import (
	"context"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Prioritize 使用后台上下文按节点实测负载评分，低负载节点获得更高分。
func (ex *Extender) Prioritize(args extenderv1.ExtenderArgs) *extenderv1.HostPriorityList {
	return ex.PrioritizeWithContext(context.Background(), args)
}

// PrioritizeWithContext 使用节点实时 CPU 和内存使用率动态打分，并响应调度请求的取消信号。
// 节点负载越低，分数越高；指标不可用时返回中性分数。
func (ex *Extender) PrioritizeWithContext(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.HostPriorityList {
	// 先为所有候选节点准备相同的 0 分；任一指标不可用时整体降级，避免只偏向有指标的节点。
	result := neutralPriorities(args)
	if args.Nodes == nil || len(args.Nodes.Items) == 0 {
		return &result
	}
	if ctx == nil {
		ctx = context.Background()
	}

	metricsByNode, err := ex.nodeMetrics(ctx)
	if err != nil {
		klog.Errorf("[Prioritize] node metrics unavailable; returning neutral scores: %v", err)
		return &result
	}

	// 评分只看实测负载；过渡预留和本次 Pod 预算在 Filter/Bind 的内存准入中计算。
	usagePercentages := make([]float64, len(args.Nodes.Items))
	cpuWeight, memoryWeight := ex.scoringWeights()
	for i, node := range args.Nodes.Items {
		metric, ok := metricsByNode[node.Name]
		if !ok {
			klog.V(4).Infof("[Prioritize] metrics missing for node %q; returning neutral scores", node.Name)
			return &result
		}
		usage, err := nodeusage.WeightedUsagePercentage(node, metric, cpuWeight, memoryWeight)
		if err != nil {
			klog.V(4).Infof("[Prioritize] calculate usage for node %q failed; returning neutral scores: %v", node.Name, err)
			return &result
		}
		usagePercentages[i] = usage
	}

	for i, usage := range usagePercentages {
		result[i].Score = scoreForUsage(usage)
		klog.V(4).Infof("[Prioritize] node=%q weighted-usage=%.2f%% score=%d", result[i].Host, usage, result[i].Score)
	}
	return &result
}

// neutralPriorities 给候选节点相同的 Extender 分数，最终选择仍由 kube-scheduler 合并原生分数决定。
func neutralPriorities(args extenderv1.ExtenderArgs) extenderv1.HostPriorityList {
	if args.Nodes == nil {
		return extenderv1.HostPriorityList{}
	}
	result := make(extenderv1.HostPriorityList, 0, len(args.Nodes.Items))
	for _, node := range args.Nodes.Items {
		result = append(result, extenderv1.HostPriority{Host: node.Name, Score: 0})
	}
	return result
}

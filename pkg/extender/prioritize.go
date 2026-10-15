package extender

import (
	"context"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Prioritize scores feasible nodes by their current weighted CPU and memory
// usage. Lower utilization receives a higher score.
func (ex *Extender) Prioritize(args extenderv1.ExtenderArgs) *extenderv1.HostPriorityList {
	return ex.PrioritizeWithContext(context.Background(), args)
}

// PrioritizeWithContext 使用节点实时 CPU 和内存使用率动态打分，并响应调度请求的取消信号。
// 节点负载越低，分数越高；指标不可用时返回中性分数。
func (ex *Extender) PrioritizeWithContext(ctx context.Context, args extenderv1.ExtenderArgs) *extenderv1.HostPriorityList {
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

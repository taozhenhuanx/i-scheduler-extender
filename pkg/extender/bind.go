package extender

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

// Bind 使用后台上下文调用带内存保护的绑定流程。
func (ex *Extender) Bind(args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult {
	return ex.BindWithContext(context.Background(), args)
}

// BindWithContext 在绑定前再次检查 80% 内存门槛，并先预约 Pod 预算以协调并发 Bind。
func (ex *Extender) BindWithContext(ctx context.Context, args extenderv1.ExtenderBindingArgs) *extenderv1.ExtenderBindingResult {
	result := new(extenderv1.ExtenderBindingResult)
	if ctx == nil {
		ctx = context.Background()
	}
	ex.admissionMu.Lock()
	defer ex.admissionMu.Unlock()

	if ex.ClientSet == nil {
		result.Error = "Kubernetes client is unavailable"
		return result
	}
	pod, err := ex.ClientSet.CoreV1().Pods(args.PodNamespace).Get(ctx, args.PodName, metav1.GetOptions{})
	if err != nil {
		result.Error = fmt.Sprintf("get pod before binding: %v", err)
		return result
	}
	if args.PodUID != "" && pod.UID != "" && pod.UID != args.PodUID {
		result.Error = fmt.Sprintf("pod %s/%s UID changed before binding", args.PodNamespace, args.PodName)
		return result
	}
	if pod.Spec.NodeName != "" {
		if pod.Spec.NodeName == args.Node {
			return result
		}
		result.Error = fmt.Sprintf("pod %s/%s is already assigned to node %q", args.PodNamespace, args.PodName, pod.Spec.NodeName)
		return result
	}
	if reservedNode := ex.pendingPodNode(pod); reservedNode != "" && reservedNode != args.Node {
		result.Error = fmt.Sprintf("pod %s/%s has an unresolved binding to node %q; retry that node or recreate the Pod", args.PodNamespace, args.PodName, reservedNode)
		return result
	}
	podMemory := nodeusage.PodMemoryReservationBytes(*pod)
	if podMemory <= 0 {
		result.Error = "pod must set a memory request or limit for the 80% node memory guard"
		return result
	}

	metricsByNode, err := ex.nodeMetrics(ctx)
	if err != nil {
		result.Error = fmt.Sprintf("cannot read node memory metrics before binding: %v", err)
		return result
	}
	metric, ok := metricsByNode[args.Node]
	if !ok {
		result.Error = fmt.Sprintf("memory metrics are unavailable for node %q", args.Node)
		return result
	}
	if _, err := memorySampleStart(metric, time.Now()); err != nil {
		result.Error = fmt.Sprintf("invalid memory metrics for node %q: %v", args.Node, err)
		return result
	}
	memoryUsed, err := nodeusage.MemoryUsageBytes(metric)
	if err != nil {
		result.Error = fmt.Sprintf("cannot read memory usage for node %q: %v", args.Node, err)
		return result
	}
	node, err := ex.ClientSet.CoreV1().Nodes().Get(ctx, args.Node, metav1.GetOptions{})
	if err != nil {
		result.Error = fmt.Sprintf("get node before binding: %v", err)
		return result
	}
	memoryCapacity, err := nodeusage.MemoryAllocatableBytes(*node)
	if err != nil {
		result.Error = fmt.Sprintf("cannot read memory capacity for node %q: %v", args.Node, err)
		return result
	}
	pendingByNode, err := ex.pendingMemoryByNode(ctx, metricsByNode)
	if err != nil {
		result.Error = fmt.Sprintf("cannot read pending memory reservations before binding: %v", err)
		return result
	}
	ownReservation := ex.ownPendingMemory(pod, args.Node)
	memoryUsed += pendingByNode[args.Node] - ownReservation
	currentPercent := memoryPercent(memoryUsed, memoryCapacity)
	projectedPercent := memoryPercent(memoryUsed+podMemory, memoryCapacity)

	switch decideMemoryGuard(currentPercent, projectedPercent, node.Labels[memoryBlockedLabel] == "true") {
	case memoryGuardKeepBlocked:
		result.Error = fmt.Sprintf("node %q is blocked by the %.0f%% memory guard", args.Node, memoryBlockThresholdPercent)
		return result
	case memoryGuardBlock:
		if err := ex.setMemoryBlocked(ctx, args.Node, true); err != nil {
			klog.ErrorS(err, "Failed to label node at memory threshold", "node", args.Node)
		}
		result.Error = fmt.Sprintf("binding pod would raise node %q memory to %.2f%%, above %.0f%%", args.Node, projectedPercent, memoryBlockThresholdPercent)
		return result
	case memoryGuardRecover:
		if err := ex.setMemoryBlocked(ctx, args.Node, false); err != nil {
			result.Error = fmt.Sprintf("clear memory block label on node %q: %v", args.Node, err)
			return result
		}
	}

	ex.reservePodMemory(pod, args.Node, podMemory)
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: args.PodName, Namespace: args.PodNamespace, UID: pod.UID},
		Target:     corev1.ObjectReference{Kind: "Node", APIVersion: "v1", Name: args.Node},
	}
	if err := ex.ClientSet.CoreV1().Pods(args.PodNamespace).Bind(ctx, binding, metav1.CreateOptions{}); err != nil {
		// 超时或断连不能证明 API Server 没有完成绑定，保留预算供重试或新采样接替。
		// Conflict 也可能是此前超时的 Bind 已完成；重试前已有预约时同样不盲目清账。
		if ownReservation == 0 && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err)) {
			ex.releasePodMemory(pod)
		}
		klog.ErrorS(err, "Failed to bind pod", "pod", args.PodName, "namespace", args.PodNamespace, "podUID", args.PodUID, "node", args.Node)
		result.Error = err.Error()
	} else {
		log.Printf("bound pod: %s/%s to node:%s", args.PodNamespace, args.PodName, args.Node)
	}
	return result
}

package extender

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const (
	memoryBlockedLabel = "scheduler.example.com/memory-blocked"
	// 80% 是阻止新 Pod 的阈值；降到 75% 才解除标签，用滞回减少标签反复切换。
	memoryBlockThresholdPercent = 80.0
	memoryRecoveryPercent       = 75.0
	podMemoryCacheTTL           = time.Second
	pendingMemoryReservationTTL = 10 * time.Minute
)

type memoryGuardDecision uint8

const (
	memoryGuardAllow memoryGuardDecision = iota
	memoryGuardBlock
	memoryGuardKeepBlocked
	memoryGuardRecover
)

// decideMemoryGuard 根据当前和预计内存占比决定放行、拦截或恢复节点标签。
func decideMemoryGuard(currentPercent, projectedPercent float64, isBlocked bool) memoryGuardDecision {
	if isBlocked {
		if currentPercent > memoryRecoveryPercent || projectedPercent > memoryBlockThresholdPercent {
			return memoryGuardKeepBlocked
		}
		return memoryGuardRecover
	}
	if projectedPercent > memoryBlockThresholdPercent {
		return memoryGuardBlock
	}
	return memoryGuardAllow
}

type podMemorySnapshot struct {
	nodeByPod map[string]string
}

type pendingMemoryReservation struct {
	nodeName  string
	bytes     int64
	createdAt time.Time
}

// pendingMemoryByNode 汇总正在 Bind 的 Pod 临时预算；已运行 Pod 的占用以 Metrics API 实测值为准。
func (ex *Extender) pendingMemoryByNode(ctx context.Context) (map[string]int64, error) {
	snapshot, err := ex.podMemorySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	pending := make(map[string]int64)

	now := time.Now()
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	for key, reservation := range ex.pendingMemory {
		if now.Sub(reservation.createdAt) > pendingMemoryReservationTTL {
			delete(ex.pendingMemory, key)
			continue
		}
		if nodeName, seen := snapshot.nodeByPod[key]; seen && nodeName != "" {
			delete(ex.pendingMemory, key)
			continue
		}
		pending[reservation.nodeName] += reservation.bytes
	}
	return pending, nil
}

// podMemorySnapshot 缓存 Pod 所属节点，用于确认 Bind 临时预留何时已被实测内存接替。
func (ex *Extender) podMemorySnapshot(ctx context.Context) (*podMemorySnapshot, error) {
	ex.podMemoryMu.Lock()
	defer ex.podMemoryMu.Unlock()
	if ex.podMemory != nil && time.Since(ex.podMemoryAt) < podMemoryCacheTTL {
		return ex.podMemory, nil
	}
	if ex.ClientSet == nil {
		return nil, fmt.Errorf("Kubernetes client is unavailable")
	}
	pods, err := ex.ClientSet.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	snapshot := &podMemorySnapshot{
		nodeByPod: make(map[string]string, len(pods.Items)),
	}
	for _, pod := range pods.Items {
		key := podMemoryKey(pod.Namespace, pod.Name)
		snapshot.nodeByPod[key] = pod.Spec.NodeName
	}
	ex.podMemory = snapshot
	ex.podMemoryAt = time.Now()
	return snapshot, nil
}

// reservePodMemory 在调用 Kubernetes Bind 前登记预算，供后续准入检查立即计入。
func (ex *Extender) reservePodMemory(namespace, name, nodeName string, bytes int64) {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	if ex.pendingMemory == nil {
		ex.pendingMemory = make(map[string]pendingMemoryReservation)
	}
	ex.pendingMemory[podMemoryKey(namespace, name)] = pendingMemoryReservation{
		nodeName:  nodeName,
		bytes:     bytes,
		createdAt: time.Now(),
	}
}

// releasePodMemory 在 Bind 失败时撤销临时预约，避免后续调度一直重复占用这部分预算。
func (ex *Extender) releasePodMemory(namespace, name string) {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	delete(ex.pendingMemory, podMemoryKey(namespace, name))
}

func podMemoryKey(namespace, name string) string {
	return namespace + "/" + name
}

// setMemoryBlocked 通过合并补丁添加或删除节点保护标签，并在资源版本冲突时重试。
func (ex *Extender) setMemoryBlocked(ctx context.Context, nodeName string, blocked bool) error {
	if ex.ClientSet == nil {
		return fmt.Errorf("Kubernetes client is unavailable")
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := ex.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		isBlocked := node.Labels[memoryBlockedLabel] == "true"
		if isBlocked == blocked {
			return nil
		}

		var labelValue interface{} = "true"
		if !blocked {
			labelValue = nil
		}
		patch := map[string]interface{}{
			"metadata": map[string]interface{}{
				"resourceVersion": node.ResourceVersion,
				"labels": map[string]interface{}{
					memoryBlockedLabel: labelValue,
				},
			},
		}
		payload, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		_, err = ex.ClientSet.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, payload, metav1.PatchOptions{})
		return err
	})
}

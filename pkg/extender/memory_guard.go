package extender

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const (
	memoryBlockedLabel = nodeusage.MemoryBlockedLabel
	// 80% 是阻止新 Pod 的阈值；降到 75% 才解除标签，用滞回减少标签反复切换。
	memoryBlockThresholdPercent = 80.0
	memoryRecoveryPercent       = 75.0
	podMemoryCacheTTL           = time.Second
	// 指标超过两分钟或明显来自未来时停止准入，不能用旧样本释放预留。
	memorySampleMaxAge = nodeusage.MemorySampleMaxAge
	memoryClockSkew    = nodeusage.MemoryClockSkew
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
	pods       map[string]*corev1.Pod
	observedAt time.Time
}

type pendingMemoryReservation struct {
	podKey    string
	podUID    types.UID
	nodeName  string
	bytes     int64
	createdAt time.Time
}

// memorySampleStart 返回采样窗口起点。整个窗口都晚于容器启动，才能确认实测值已接替预算。
func memorySampleStart(metric map[string]interface{}, now time.Time) (time.Time, error) {
	return nodeusage.MemorySampleStart(metric, now)
}

func terminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// podMemoryCovered 要求所有应用容器和可重启 init 容器均已运行且被完整采样窗口覆盖。
// 镜像拉取、普通 init 执行及容器重启等待期间继续计入预算；nodeName 本身不能证明覆盖。
func podMemoryCovered(pod *corev1.Pod, metric map[string]interface{}, now time.Time) bool {
	start, err := memorySampleStart(metric, now)
	if err != nil || len(pod.Spec.Containers) == 0 {
		return false
	}
	covered := func(name string, statuses []corev1.ContainerStatus) bool {
		for _, status := range statuses {
			if status.Name == name {
				running := status.State.Running
				return running != nil && !running.StartedAt.IsZero() && !running.StartedAt.Time.After(start)
			}
		}
		return false
	}
	for _, container := range pod.Spec.Containers {
		if !covered(container.Name, pod.Status.ContainerStatuses) {
			return false
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			if !covered(container.Name, pod.Status.InitContainerStatuses) {
				return false
			}
		}
	}
	return true
}

// pendingMemoryByNode 汇总尚未被新指标覆盖的预算。没有固定 TTL，防止拉取镜像时间较长时漏算。
func (ex *Extender) pendingMemoryByNode(ctx context.Context, metricsByNode map[string]map[string]interface{}) (map[string]int64, error) {
	snapshot, err := ex.podMemorySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	pending := make(map[string]int64)

	now := time.Now()
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	for key, reservation := range ex.pendingMemory {
		pod, seen := snapshot.pods[reservation.podKey]
		// 缓存早于 Bind 预约时不能据此判定 Pod 已删除或同名替换。
		if !snapshot.observedAt.Before(reservation.createdAt) && (!seen || pod.UID != reservation.podUID || terminalPod(pod) || (pod.Spec.NodeName != "" && pod.Spec.NodeName != reservation.nodeName)) {
			delete(ex.pendingMemory, key)
			continue
		}
		if seen && pod.UID == reservation.podUID && pod.Spec.NodeName == reservation.nodeName && podMemoryCovered(pod, metricsByNode[reservation.nodeName], now) {
			delete(ex.pendingMemory, key)
			continue
		}
		pending[reservation.nodeName] += reservation.bytes
	}
	return pending, nil
}

// podMemorySnapshot 缓存 Pod UID 和容器运行状态，用于判断预算是否可释放。
func (ex *Extender) podMemorySnapshot(ctx context.Context) (*podMemorySnapshot, error) {
	ex.podMemoryMu.Lock()
	defer ex.podMemoryMu.Unlock()
	if ex.podMemory != nil && time.Since(ex.podMemoryAt) < podMemoryCacheTTL {
		return ex.podMemory, nil
	}
	if ex.ClientSet == nil {
		return nil, fmt.Errorf("Kubernetes client is unavailable")
	}
	observedAt := time.Now()
	pods, err := ex.ClientSet.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	snapshot := &podMemorySnapshot{
		pods:       make(map[string]*corev1.Pod, len(pods.Items)),
		observedAt: observedAt,
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		key := podMemoryKey(pod.Namespace, pod.Name)
		snapshot.pods[key] = pod
	}
	ex.podMemory = snapshot
	ex.podMemoryAt = time.Now()
	return snapshot, nil
}

// reservePodMemory 在调用 Kubernetes Bind 前登记预算，供后续准入检查立即计入。
func (ex *Extender) reservePodMemory(pod *corev1.Pod, nodeName string, bytes int64) {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	if ex.pendingMemory == nil {
		ex.pendingMemory = make(map[string]pendingMemoryReservation)
	}
	ex.pendingMemory[podReservationKey(pod)] = pendingMemoryReservation{
		podKey:    podMemoryKey(pod.Namespace, pod.Name),
		podUID:    pod.UID,
		nodeName:  nodeName,
		bytes:     bytes,
		createdAt: time.Now(),
	}
}

// releasePodMemory 在 Bind 明确被拒绝时撤销新预约；结果不确定时等待 API 状态或新采样。
func (ex *Extender) releasePodMemory(pod *corev1.Pod) {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	delete(ex.pendingMemory, podReservationKey(pod))
}

func podReservationKey(pod *corev1.Pod) string {
	return podMemoryKey(pod.Namespace, pod.Name) + "/" + string(pod.UID)
}

// ownPendingMemory 在同一个 Pod 重试准入时扣除其旧预约，避免重复加上新 Pod 预算。
func (ex *Extender) ownPendingMemory(pod *corev1.Pod, nodeName string) int64 {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	reservation := ex.pendingMemory[podReservationKey(pod)]
	if reservation.nodeName == nodeName {
		return reservation.bytes
	}
	return 0
}

// pendingPodNode 防止结果不确定的 Bind 重试改写旧节点预算。确认绑定状态前只能重试原节点。
func (ex *Extender) pendingPodNode(pod *corev1.Pod) string {
	ex.pendingMemMu.Lock()
	defer ex.pendingMemMu.Unlock()
	return ex.pendingMemory[podReservationKey(pod)].nodeName
}

// InitializeMemoryReservations 在 leader 开放调度接口前，从 API 重建启动和采样过渡期的预算。
// 已被实测覆盖的稳定 Pod 不计入 request 总账；升级后尚未启动的已绑定 Pod 仍受保护。
func (ex *Extender) InitializeMemoryReservations(ctx context.Context) error {
	ex.admissionMu.Lock()
	defer ex.admissionMu.Unlock()
	metrics, err := ex.nodeMetrics(ctx)
	if err != nil {
		return fmt.Errorf("load node metrics for memory reservations: %w", err)
	}
	snapshot, err := ex.podMemorySnapshot(ctx)
	if err != nil {
		return fmt.Errorf("load Pods for memory reservations: %w", err)
	}
	now := time.Now()
	for _, pod := range snapshot.pods {
		if pod.Spec.NodeName == "" || terminalPod(pod) || podMemoryCovered(pod, metrics[pod.Spec.NodeName], now) {
			continue
		}
		if budget := nodeusage.PodMemoryReservationBytes(*pod); budget > 0 {
			ex.reservePodMemory(pod, pod.Spec.NodeName, budget)
		}
	}
	return nil
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

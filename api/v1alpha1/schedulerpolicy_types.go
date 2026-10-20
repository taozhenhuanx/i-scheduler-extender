/*
Copyright 2026.

*/

package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultCPUWeightPercent    int32 = 10
	DefaultMemoryWeightPercent int32 = 90
)

// SchedulerPolicy configures cluster-wide scheduler scoring and node load protection.
// Only the cluster-scoped object named "cluster" is reconciled.
// SchedulerPolicy 是集群级调度策略资源，用于配置节点打分和负载保护。
// 控制器只协调名称为 "cluster" 的单例对象。
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=spol
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SchedulerPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   SchedulerPolicySpec   `json:"spec,omitempty"`
	Status SchedulerPolicyStatus `json:"status,omitempty"`
}

// SchedulerPolicySpec describes the policy used for scheduler scoring and node protection.
// SchedulerPolicySpec 定义调度评分与节点负载保护所使用的策略参数。
type SchedulerPolicySpec struct {
	// CheckIntervalSeconds is the interval between cluster-wide balancing passes.
	// CheckIntervalSeconds 指定控制器检查节点负载的周期，单位为秒。
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	CheckIntervalSeconds int32 `json:"checkIntervalSeconds,omitempty"`

	// CPUWeight is the percentage of the scoring weight assigned to CPU utilization.
	// CPUWeight 是评分中分配给 CPU 使用率的权重百分比，须与 MemoryWeight 之和为 100。
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// 可显式设置为 0；未设置时使用默认百分比。
	CPUWeight *int32 `json:"cpuWeight,omitempty"`

	// MemoryWeight is the percentage of the scoring weight assigned to memory utilization.
	// MemoryWeight 是评分中分配给内存使用率的权重百分比，须与 CPUWeight 之和为 100。
	// +kubebuilder:default=90
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MemoryWeight *int32 `json:"memoryWeight,omitempty"`

	// HighThreshold is the weighted utilization percentage at which a node can be cordoned.
	// HighThreshold 是节点允许被 cordon（标记为不可调度）的加权负载阈值。
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=100
	HighThreshold int32 `json:"highThreshold,omitempty"`

	// RecoveryThreshold is the utilization percentage below which an automatically cordoned
	// node can be uncordoned. When omitted, it defaults to highThreshold minus 5.
	// RecoveryThreshold 是自动 cordon 的节点允许恢复调度的负载阈值；未设置时默认为 HighThreshold 减 5。
	// +kubebuilder:validation:Minimum=0
	RecoveryThreshold *int32 `json:"recoveryThreshold,omitempty"`

	// MinSchedulableNodes prevents new cordons from reducing healthy, available managed nodes below this count.
	// MinSchedulableNodes 阻止新增 cordon 将受管可用节点减少到该值以下。
	// 可用节点须 Ready、未删除、未 cordon、指标新鲜有效，且无内存保护标记及 NoSchedule/NoExecute taint。
	// 现有故障导致数量不足时停止新增 cordon，该字段不会自动修复故障或解除人工 cordon。
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	MinSchedulableNodes int32 `json:"minSchedulableNodes,omitempty"`

	// TopNodesCount limits cordon decisions to the most heavily utilized eligible nodes.
	// TopNodesCount 限制每轮参与 cordon 判断的节点数量，只考虑负载最高的前 N 个节点。
	// 排名只包含当前受管可用节点；无效指标或故障节点不占候选名额。
	// +kubebuilder:default=9
	// +kubebuilder:validation:Minimum=1
	TopNodesCount int32 `json:"topNodesCount,omitempty"`

	// NodeSelector limits policy evaluation to nodes with all listed labels.
	// An empty selector matches every node.
	// NodeSelector 将策略限定到同时匹配所列标签的节点；空选择器匹配所有节点。
	// 节点退出范围后清理本控制器的自动 cordon；Extender 内存准入仍作用于 scheduler 提供的候选节点。
	// +kubebuilder:validation:Optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// IgnoreNodes lists node names excluded from policy evaluation.
	// IgnoreNodes 按节点名称排除节点；被列出的节点不受本策略管理。
	// 加入忽略列表时清理本控制器的自动 cordon，保留未带自动标记的人工 cordon。
	// +kubebuilder:validation:Optional
	IgnoreNodes []string `json:"ignoreNodes,omitempty"`
}

// ResolveWeights 应用缺省百分比并校验权重总和，只在 API 边界转换为运行时的 0..1 比例。
// 保留运行时跨字段校验，以兼容不支持 CEL schema 规则的 Kubernetes 1.22。
func (spec SchedulerPolicySpec) ResolveWeights() (float64, float64, error) {
	cpu, memory := DefaultCPUWeightPercent, DefaultMemoryWeightPercent
	if spec.CPUWeight != nil {
		cpu = *spec.CPUWeight
	}
	if spec.MemoryWeight != nil {
		memory = *spec.MemoryWeight
	}
	if cpu < 0 || cpu > 100 || memory < 0 || memory > 100 {
		return 0, 0, fmt.Errorf("cpuWeight and memoryWeight must each be between 0 and 100")
	}
	if cpu+memory != 100 {
		return 0, 0, fmt.Errorf("cpuWeight and memoryWeight must sum to 100")
	}
	return float64(cpu) / 100, float64(memory) / 100, nil
}

// SchedulerPolicyStatus reports whether the latest policy was applied successfully.
// SchedulerPolicyStatus 记录控制器最近一次协调的结果。
type SchedulerPolicyStatus struct {
	// ObservedGeneration is the most recent spec generation processed by the controller.
	// ObservedGeneration 表示控制器最近处理过的 spec 版本号。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describes the state of the policy.
	// Conditions 描述策略当前是否已成功应用以及失败原因。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SchedulerPolicyList contains a list of SchedulerPolicy.
// SchedulerPolicyList 是 SchedulerPolicy 资源列表。
// +kubebuilder:object:root=true
type SchedulerPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SchedulerPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SchedulerPolicy{}, &SchedulerPolicyList{})
}

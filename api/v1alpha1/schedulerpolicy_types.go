/*
Copyright 2026.

*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultCPUWeightPercent    int32 = 10
	DefaultMemoryWeightPercent int32 = 90
)

// SchedulerPolicy configures cluster-wide scheduler scoring and node load protection.
// Only the cluster-scoped object named "cluster" is reconciled.
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
type SchedulerPolicySpec struct {
	// CheckIntervalSeconds is the interval between cluster-wide balancing passes.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	CheckIntervalSeconds int32 `json:"checkIntervalSeconds,omitempty"`

	// CPUWeight is the percentage of the scoring weight assigned to CPU utilization.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	CPUWeight int32 `json:"cpuWeight,omitempty"`

	// MemoryWeight is the percentage of the scoring weight assigned to memory utilization.
	// +kubebuilder:default=90
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MemoryWeight int32 `json:"memoryWeight,omitempty"`

	// HighThreshold is the weighted utilization percentage at which a node can be cordoned.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=100
	HighThreshold int32 `json:"highThreshold,omitempty"`

	// RecoveryThreshold is the utilization percentage below which an automatically cordoned
	// node can be uncordoned. When omitted, it defaults to highThreshold minus 5.
	// +kubebuilder:validation:Minimum=0
	RecoveryThreshold *int32 `json:"recoveryThreshold,omitempty"`

	// MinSchedulableNodes is the minimum number of eligible nodes kept schedulable.
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	MinSchedulableNodes int32 `json:"minSchedulableNodes,omitempty"`

	// TopNodesCount limits cordon decisions to the most heavily utilized eligible nodes.
	// +kubebuilder:default=9
	// +kubebuilder:validation:Minimum=1
	TopNodesCount int32 `json:"topNodesCount,omitempty"`

	// NodeSelector limits policy evaluation to nodes with all listed labels.
	// An empty selector matches every node.
	// +kubebuilder:validation:Optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// IgnoreNodes lists node names excluded from policy evaluation.
	// +kubebuilder:validation:Optional
	IgnoreNodes []string `json:"ignoreNodes,omitempty"`
}

// SchedulerPolicyStatus reports whether the latest policy was applied successfully.
type SchedulerPolicyStatus struct {
	// ObservedGeneration is the most recent spec generation processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describes the state of the policy.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SchedulerPolicyList contains a list of SchedulerPolicy.
// +kubebuilder:object:root=true
type SchedulerPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SchedulerPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SchedulerPolicy{}, &SchedulerPolicyList{})
}

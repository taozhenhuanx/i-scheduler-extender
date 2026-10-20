package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"github.com/lixd96/i-scheduler-extender/pkg/rebalancer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	clusterPolicyName        = "cluster"
	clusterRequestName       = "cluster"
	readyConditionType       = "Ready"
	cleanupFinalizer         = "scheduler.example.com/policy-cleanup"
	conditionMessageMaxRunes = 32768
)

// +kubebuilder:rbac:groups=scheduler.example.com,resources=schedulerpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=scheduler.example.com,resources=schedulerpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/binding,verbs=create
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=nodes,verbs=get;list

// SchedulerPolicyReconciler applies the cluster-wide SchedulerPolicy named "cluster".
// SchedulerPolicyReconciler 负责把单例策略协调到节点负载保护和 Extender 评分权重。
type SchedulerPolicyReconciler struct {
	client.Client
	KubernetesClient kubernetes.Interface
	DynamicClient    dynamic.Interface
	Extender         *extender.Extender
	// StartupReady 在 Extender 初始化策略和内存预留后关闭，避免启动加载覆盖首次协调的新权重。
	StartupReady <-chan struct{}
}

// Reconcile 校验单例资源，处理删除清理，应用策略后更新 Ready 状态并安排下一轮检查。
func (r *SchedulerPolicyReconciler) Reconcile(ctx context.Context, request reconcile.Request) (ctrl.Result, error) {
	if r.StartupReady != nil {
		select {
		case <-r.StartupReady:
		case <-ctx.Done():
			return ctrl.Result{}, ctx.Err()
		}
	}
	if request.Name != clusterPolicyName {
		unsupportedPolicy := &schedulerv1alpha1.SchedulerPolicy{}
		if err := r.Get(ctx, types.NamespacedName{Name: request.Name}, unsupportedPolicy); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if err := r.setReadyCondition(ctx, unsupportedPolicy, metav1.ConditionFalse, "UnsupportedName", "the cluster policy must be named cluster"); err != nil {
			return ctrl.Result{}, fmt.Errorf("update unsupported policy status: %w", err)
		}
		return ctrl.Result{}, nil
	}
	policy := &schedulerv1alpha1.SchedulerPolicy{}
	key := types.NamespacedName{Name: clusterPolicyName}
	if err := r.Get(ctx, key, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, err
	}
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(policy, cleanupFinalizer) {
			// 删除策略时恢复本控制器 cordon 的节点，并将 Extender 权重重置为默认值。
			if err := rebalancer.UncordonAutoCordonedNodes(ctx, r.KubernetesClient); err != nil {
				return ctrl.Result{}, r.setFailureCondition(ctx, policy, "CleanupError", err)
			}
			if err := r.Extender.SetWeights(
				float64(schedulerv1alpha1.DefaultCPUWeightPercent)/100,
				float64(schedulerv1alpha1.DefaultMemoryWeightPercent)/100,
			); err != nil {
				return ctrl.Result{}, r.setFailureCondition(ctx, policy, "CleanupError", fmt.Errorf("reset Extender weights: %w", err))
			}
			if err := r.patchCleanupFinalizer(ctx, policy, false); err != nil {
				return ctrl.Result{}, r.setFailureCondition(ctx, policy, "CleanupError", fmt.Errorf("remove policy cleanup finalizer: %w", err))
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(policy, cleanupFinalizer) {
		// 先登记 finalizer，确保资源删除时有机会执行节点恢复和权重清理。
		if err := r.patchCleanupFinalizer(ctx, policy, true); err != nil {
			return ctrl.Result{}, fmt.Errorf("add policy cleanup finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// CRD 字段可单独校验的范围由 schema 约束；权重总和等跨字段规则在这里校验。
	config, cpuWeight, memoryWeight, err := balancerConfig(policy.Spec)
	var balancer *rebalancer.Balancer
	if err == nil {
		balancer, err = rebalancer.New(r.KubernetesClient, r.DynamicClient, config)
	}
	if err == nil {
		// 评分和负载保护共用 CR 中的权重，随后执行一轮节点协调。
		err = r.Extender.SetWeights(cpuWeight, memoryWeight)
	}
	if err == nil {
		err = balancer.Reconcile(ctx)
	}
	if err != nil {
		return ctrl.Result{}, r.setFailureCondition(ctx, policy, "ReconcileError", err)
	}
	if err := r.setReadyCondition(ctx, policy, metav1.ConditionTrue, "Reconciled", "scheduler policy was applied"); err != nil {
		return ctrl.Result{}, fmt.Errorf("update scheduler policy status: %w", err)
	}

	return ctrl.Result{RequeueAfter: config.CheckInterval}, nil
}

// patchCleanupFinalizer 只补丁更新 metadata，并用 resourceVersion 防止覆盖并发修改。
// 避免整对象 Update 重新序列化 spec，使显式零权重和未知字段保持原值。
func (r *SchedulerPolicyReconciler) patchCleanupFinalizer(ctx context.Context, policy *schedulerv1alpha1.SchedulerPolicy, add bool) error {
	base := policy.DeepCopy()
	updated := policy.DeepCopy()
	if add {
		controllerutil.AddFinalizer(updated, cleanupFinalizer)
	} else {
		controllerutil.RemoveFinalizer(updated, cleanupFinalizer)
	}
	return r.Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// setFailureCondition 保留节点操作失败和状态写入失败的错误链，让协调器自动重试。
// 删除清理失败时不移除 finalizer；下一轮继续恢复剩余节点，避免 CR 仍显示上一轮的 Ready=True。
func (r *SchedulerPolicyReconciler) setFailureCondition(
	ctx context.Context,
	policy *schedulerv1alpha1.SchedulerPolicy,
	reason string,
	failure error,
) error {
	if err := r.setReadyCondition(ctx, policy, metav1.ConditionFalse, reason, failure.Error()); err != nil {
		return errors.Join(failure, fmt.Errorf("update scheduler policy status: %w", err))
	}
	return failure
}

// setReadyCondition 只在状态变化时写回 status，避免重复更新产生无意义事件。
func (r *SchedulerPolicyReconciler) setReadyCondition(
	ctx context.Context,
	policy *schedulerv1alpha1.SchedulerPolicy,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	updated := policy.DeepCopy()
	updated.Status.ObservedGeneration = policy.Generation
	// 多节点错误可能超过 Condition schema 的消息上限；仅截断状态消息，返回错误保留完整详情。
	if utf8.RuneCountInString(message) > conditionMessageMaxRunes {
		const suffix = "\n... additional errors omitted; see controller logs"
		message = string([]rune(message)[:conditionMessageMaxRunes-utf8.RuneCountInString(suffix)]) + suffix
	}
	meta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             status,
		ObservedGeneration: policy.Generation,
		Reason:             reason,
		Message:            message,
	})
	if updated.Status.ObservedGeneration == policy.Status.ObservedGeneration &&
		reflect.DeepEqual(updated.Status.Conditions, policy.Status.Conditions) {
		return nil
	}
	// 只补丁更新本轮状态，并以 resourceVersion 检测并发更新，避免覆盖其他协调产生的状态。
	return r.Status().Patch(ctx, updated, client.MergeFromWithOptions(policy, client.MergeFromWithOptimisticLock{}))
}

// balancerConfig 将 CR 字段转换成运行时配置，并校验需要联合判断的策略约束。
func balancerConfig(spec schedulerv1alpha1.SchedulerPolicySpec) (rebalancer.Config, float64, float64, error) {
	cpuWeight, memoryWeight, err := spec.ResolveWeights()
	if err != nil {
		return rebalancer.Config{}, 0, 0, err
	}
	if spec.HighThreshold < 5 || spec.HighThreshold > 100 {
		return rebalancer.Config{}, 0, 0, fmt.Errorf("highThreshold must be between 5 and 100")
	}
	recoveryThreshold := float64(spec.HighThreshold - 5)
	if spec.RecoveryThreshold != nil {
		recoveryThreshold = float64(*spec.RecoveryThreshold)
	}
	if recoveryThreshold < 0 || recoveryThreshold >= float64(spec.HighThreshold) {
		return rebalancer.Config{}, 0, 0, fmt.Errorf("recoveryThreshold must be >= 0 and lower than highThreshold")
	}
	ignored := make(map[string]struct{}, len(spec.IgnoreNodes))
	for _, nodeName := range spec.IgnoreNodes {
		ignored[nodeName] = struct{}{}
	}
	return rebalancer.Config{
		CheckInterval:       time.Duration(spec.CheckIntervalSeconds) * time.Second,
		CPUWeight:           cpuWeight,
		MemoryWeight:        memoryWeight,
		NodeSelector:        spec.NodeSelector,
		IgnoreNodes:         ignored,
		HighThreshold:       float64(spec.HighThreshold),
		RecoveryThreshold:   recoveryThreshold,
		MinSchedulableNodes: int(spec.MinSchedulableNodes),
		TopNodesCount:       int(spec.TopNodesCount),
	}, cpuWeight, memoryWeight, nil
}

// SetupWithManager 监听策略和 Node 变化；节点变化时重新排队协调集群级单例策略。
func (r *SchedulerPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toClusterRequest := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: clusterRequestName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&schedulerv1alpha1.SchedulerPolicy{}).
		Watches(&corev1.Node{}, toClusterRequest).
		Named("scheduler-policy").
		Complete(r)
}

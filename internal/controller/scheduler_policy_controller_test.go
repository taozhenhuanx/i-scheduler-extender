package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	"github.com/lixd96/i-scheduler-extender/pkg/extender"
	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func percent(value int32) *int32 { return &value }

func TestBalancerConfigUsesSameFractionWeightsAsExtender(t *testing.T) {
	for _, weights := range [][2]int32{{0, 100}, {100, 0}, {1, 99}, {10, 90}} {
		spec := schedulerv1alpha1.SchedulerPolicySpec{
			CPUWeight: percent(weights[0]), MemoryWeight: percent(weights[1]),
			HighThreshold: 70, CheckIntervalSeconds: 5, MinSchedulableNodes: 1, TopNodesCount: 9,
		}
		config, cpu, memory, err := balancerConfig(spec)
		if err != nil {
			t.Fatal(err)
		}
		if config.CPUWeight != cpu || config.MemoryWeight != memory || cpu != float64(weights[0])/100 || memory != float64(weights[1])/100 {
			t.Errorf("weights %v became NodeBalancer (%v, %v) and Extender (%v, %v)", weights, config.CPUWeight, config.MemoryWeight, cpu, memory)
		}
	}
}

type metadataPatchClient struct {
	client.Client
	patches []map[string]interface{}
}

func (c *metadataPatchClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return fmt.Errorf("unexpected full-object Update")
}

func (c *metadataPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	c.patches = append(c.patches, decoded)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestFinalizerPatchChangesOnlyMetadataAndPreservesZeroWeights(t *testing.T) {
	ctx := context.Background()
	for _, weights := range [][2]int32{{0, 100}, {100, 0}, {1, 99}} {
		t.Run(fmt.Sprintf("%d-%d", weights[0], weights[1]), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := schedulerv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			policy := &schedulerv1alpha1.SchedulerPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster", ResourceVersion: "1", Finalizers: []string{"example.com/another-finalizer"}, Labels: map[string]string{"keep": "label"}},
				Spec:       schedulerv1alpha1.SchedulerPolicySpec{CPUWeight: percent(weights[0]), MemoryWeight: percent(weights[1]), HighThreshold: 70},
			}
			originalSpec := policy.Spec.DeepCopy()
			c := &metadataPatchClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()}
			r := &SchedulerPolicyReconciler{Client: c}
			// 实际 Reconcile 的首次处理必须登记 finalizer，且不经过整对象 Update。
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
				t.Fatal(err)
			}
			current := &schedulerv1alpha1.SchedulerPolicy{}
			if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Spec, *originalSpec) {
				t.Fatalf("adding finalizer modified spec: %#v", current.Spec)
			}
			if err := r.patchCleanupFinalizer(ctx, current, false); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Spec, *originalSpec) || !reflect.DeepEqual(current.Finalizers, policy.Finalizers) || !reflect.DeepEqual(current.Labels, policy.Labels) {
				t.Fatalf("removing finalizer modified spec or unrelated metadata: %#v", current)
			}
			if len(c.patches) != 2 {
				t.Fatalf("expected add/remove patches, got %d", len(c.patches))
			}
			for _, patch := range c.patches {
				metadata, ok := patch["metadata"].(map[string]interface{})
				if len(patch) != 1 || !ok || metadata["resourceVersion"] == nil || metadata["finalizers"] == nil {
					t.Fatalf("patch must contain only metadata with optimistic lock: %#v", patch)
				}
			}
		})
	}
}

func TestFinalizerPatchRejectsStaleResourceVersion(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := schedulerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	policy := &schedulerv1alpha1.SchedulerPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cluster", ResourceVersion: "1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := &SchedulerPolicyReconciler{Client: c}
	stale := policy.DeepCopy()
	current := policy.DeepCopy()
	current.Finalizers = []string{"example.com/concurrent-finalizer"}
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := r.patchCleanupFinalizer(ctx, stale, true); !apierrors.IsConflict(err) {
		t.Fatalf("stale finalizer patch must conflict, got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Finalizers, []string{"example.com/concurrent-finalizer"}) {
		t.Fatalf("concurrent finalizer was overwritten: %v", current.Finalizers)
	}
}

func TestReconcileWaitsForStartupInitialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &SchedulerPolicyReconciler{StartupReady: make(chan struct{})}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed context while startup is pending must abort reconciliation, got %v", err)
	}
}

func TestReconcileReportsPartialNodeFailureAndRecoversReady(t *testing.T) {
	ctx := context.Background()
	policy := controllerTestPolicy()
	c := controllerTestClient(t, policy)
	kube := kubernetesfake.NewSimpleClientset(controllerTestNode("blocked"), controllerTestNode("busy"), controllerTestNode("spare"))
	patchFailure := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "blocked", errors.New("node update denied"))
	failPatch := true
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == "blocked" && failPatch {
			return true, nil, patchFailure
		}
		return false, nil, nil
	})
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeusage.NodeMetricsGVR: "NodeMetricsList"})
	dynamicClient.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			controllerTestMetric("blocked", "95Mi"), controllerTestMetric("busy", "95Mi"), controllerTestMetric("spare", "10Mi"),
		}}, nil
	})
	r := &SchedulerPolicyReconciler{
		Client: c, KubernetesClient: kube, DynamicClient: dynamicClient,
		Extender: extender.NewExtenderWithClients(kube, dynamicClient, 0.1, 0.9),
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}
	if _, err := r.Reconcile(ctx, request); !errors.Is(err, patchFailure) {
		t.Fatalf("partial node failure must trigger retry with the original error, got %v", err)
	}
	current := &schedulerv1alpha1.SchedulerPolicy{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, readyConditionType)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ReconcileError" ||
		!strings.Contains(condition.Message, "blocked") || !strings.Contains(condition.Message, patchFailure.Error()) ||
		condition.ObservedGeneration != policy.Generation || current.Status.ObservedGeneration != policy.Generation {
		t.Fatalf("Ready must identify the failed node and observed generation, got %#v", current.Status)
	}
	busy, err := kube.CoreV1().Nodes().Get(ctx, "busy", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !busy.Spec.Unschedulable {
		t.Fatal("another busy node must still be cordoned after the first node fails")
	}

	// 下一轮节点 API 恢复后，协调器必须重新尝试失败的操作，并清除失败状态。
	failPatch = false
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	condition = meta.FindStatusCondition(current.Status.Conditions, readyConditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "Reconciled" {
		t.Fatalf("successful retry must restore Ready=True, got %#v", current.Status)
	}
	blocked, err := kube.CoreV1().Nodes().Get(ctx, "blocked", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.Spec.Unschedulable {
		t.Fatal("successful retry must cordon the previously failed node")
	}
}

func TestPolicyDeletionKeepsFinalizerOnCleanupFailure(t *testing.T) {
	ctx := context.Background()
	policy := controllerTestPolicy()
	deletingAt := metav1.Now()
	policy.DeletionTimestamp = &deletingAt
	c := controllerTestClient(t, policy)
	failed, restored := controllerTestNode("failed"), controllerTestNode("restored")
	for _, node := range []*corev1.Node{failed, restored} {
		node.Spec.Unschedulable = true
		node.Annotations = map[string]string{"scheduler.example.com/auto-cordoned": "true"}
	}
	kube := kubernetesfake.NewSimpleClientset(failed, restored)
	cleanupFailure := errors.New("cleanup denied")
	failPatch := true
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == "failed" && failPatch {
			return true, nil, cleanupFailure
		}
		return false, nil, nil
	})
	r := &SchedulerPolicyReconciler{Client: c, KubernetesClient: kube, Extender: extender.NewExtenderWithClients(kube, nil, 0.1, 0.9)}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}
	if _, err := r.Reconcile(ctx, request); !errors.Is(err, cleanupFailure) {
		t.Fatalf("cleanup failure must trigger retry, got %v", err)
	}
	current := &schedulerv1alpha1.SchedulerPolicy{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, readyConditionType)
	if !reflect.DeepEqual(current.Finalizers, []string{cleanupFinalizer}) || condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "CleanupError" || !strings.Contains(condition.Message, "failed") {
		t.Fatalf("cleanup failure must retain finalizer and report Ready=False, got %#v", current)
	}
	restoredCurrent, err := kube.CoreV1().Nodes().Get(ctx, "restored", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restoredCurrent.Spec.Unschedulable {
		t.Fatal("cleanup must continue restoring other nodes after a node fails")
	}
	failPatch = false
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, current); !apierrors.IsNotFound(err) {
		t.Fatalf("successful cleanup must remove finalizer and complete deletion, got %v", err)
	}
}

type failingStatusClient struct {
	client.Client
	failure error
}

func (c *failingStatusClient) Status() client.SubResourceWriter {
	return &failingStatusWriter{SubResourceWriter: c.Client.Status(), failure: c.failure}
}

type failingStatusWriter struct {
	client.SubResourceWriter
	failure error
}

func (w *failingStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.failure
}

func TestFailureConditionPreservesOperationAndStatusErrors(t *testing.T) {
	policy := controllerTestPolicy()
	operationFailure, statusFailure := errors.New("node patch failed"), errors.New("status patch failed")
	c := &failingStatusClient{Client: controllerTestClient(t, policy), failure: statusFailure}
	r := &SchedulerPolicyReconciler{Client: c}
	err := r.setFailureCondition(context.Background(), policy, "ReconcileError", operationFailure)
	if !errors.Is(err, operationFailure) || !errors.Is(err, statusFailure) {
		t.Fatalf("both error chains must survive a status write failure, got %v", err)
	}
}

func TestReadyConditionPatchRejectsConcurrentStatusUpdate(t *testing.T) {
	ctx := context.Background()
	policy := controllerTestPolicy()
	c := controllerTestClient(t, policy)
	current := &schedulerv1alpha1.SchedulerPolicy{}
	key := types.NamespacedName{Name: "cluster"}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	stale := current.DeepCopy()
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: "ExternalCondition", Status: metav1.ConditionTrue, Reason: "ConcurrentUpdate", Message: "keep this condition",
	})
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	r := &SchedulerPolicyReconciler{Client: c}
	if err := r.setReadyCondition(ctx, stale, metav1.ConditionFalse, "ReconcileError", "node patch failed"); !apierrors.IsConflict(err) {
		t.Fatalf("stale status patch must conflict, got %v", err)
	}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, "ExternalCondition"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("concurrent status condition must remain intact, got %#v", current.Status)
	}
}

func TestFailureConditionTruncatesStatusMessageAndKeepsFullError(t *testing.T) {
	ctx := context.Background()
	policy := controllerTestPolicy()
	c := controllerTestClient(t, policy)
	r := &SchedulerPolicyReconciler{Client: c}
	operationFailure := errors.New(strings.Repeat("节点错误", 10000))
	if err := r.setFailureCondition(ctx, policy, "ReconcileError", operationFailure); err != operationFailure {
		t.Fatalf("the returned operation error must retain full details, got %v", err)
	}
	current := &schedulerv1alpha1.SchedulerPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, readyConditionType)
	if condition == nil || !utf8.ValidString(condition.Message) || utf8.RuneCountInString(condition.Message) != conditionMessageMaxRunes ||
		!strings.HasSuffix(condition.Message, "see controller logs") {
		t.Fatalf("long status message must fit schema and preserve valid UTF-8, got %#v", condition)
	}
}

func controllerTestPolicy() *schedulerv1alpha1.SchedulerPolicy {
	return &schedulerv1alpha1.SchedulerPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", ResourceVersion: "1", Generation: 8, Finalizers: []string{cleanupFinalizer}},
		Spec: schedulerv1alpha1.SchedulerPolicySpec{
			CPUWeight: percent(10), MemoryWeight: percent(90), HighThreshold: 70,
			CheckIntervalSeconds: 5, MinSchedulableNodes: 1, TopNodesCount: 3,
		},
		Status: schedulerv1alpha1.SchedulerPolicyStatus{Conditions: []metav1.Condition{{
			Type: readyConditionType, Status: metav1.ConditionTrue, Reason: "Reconciled",
			Message: "previous reconciliation succeeded", LastTransitionTime: metav1.Now(),
		}}},
	}
}

func controllerTestClient(t *testing.T, policy *schedulerv1alpha1.SchedulerPolicy) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := schedulerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(policy).WithObjects(policy).Build()
}

func controllerTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func controllerTestMetric(name, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata":  map[string]interface{}{"name": name},
		"timestamp": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), "window": "10s",
		"usage": map[string]interface{}{"cpu": "0", "memory": memory},
	}}
}

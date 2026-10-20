package rebalancer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

var round2NodesGVR = corev1.SchemeGroupVersion.WithResource("nodes")

func round2Node(name string, unschedulable, automatic bool) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "1",
			Labels: map[string]string{"managed": "true"},
		},
		Spec: corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		},
	}
	if automatic {
		node.Annotations = map[string]string{autoCordonAnnotation: "true"}
	}
	return node
}

func round2Metric(name, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata":  map[string]interface{}{"name": name},
		"usage":     map[string]interface{}{"cpu": "100m", "memory": memory},
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "window": "30s",
	}}
}

func round2Metrics(items []unstructured.Unstructured, listErr error) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeusage.NodeMetricsGVR: "NodeMetricsList"})
	client.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if listErr != nil {
			return true, nil, listErr
		}
		return true, &unstructured.UnstructuredList{Items: items}, nil
	})
	return client
}

func round2Balancer(t *testing.T, kube *kubernetesfake.Clientset, metrics *dynamicfake.FakeDynamicClient) *Balancer {
	t.Helper()
	balancer, err := New(kube, metrics, Config{
		CheckInterval: time.Second, CPUWeight: 0, MemoryWeight: 1,
		HighThreshold: 70, RecoveryThreshold: 65, MinSchedulableNodes: 1, TopNodesCount: 9,
		NodeSelector: map[string]string{"managed": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return balancer
}

func round2CurrentNode(t *testing.T, kube *kubernetesfake.Clientset, name string) *corev1.Node {
	t.Helper()
	node, err := kube.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// 指标不可用时，退出管理范围的清理仍要完成，并保留人工 cordon。
func TestBalancerScopeCleanupDoesNotDependOnMetrics(t *testing.T) {
	excluded := round2Node("excluded", true, true)
	excluded.Labels["managed"] = "false"
	ignored := round2Node("ignored", true, true)
	manual := round2Node("manual", true, false)
	manual.Labels["managed"] = "false"
	managed := round2Node("managed", true, true)
	stale := round2Node("stale", false, true)
	kube := kubernetesfake.NewSimpleClientset(excluded, ignored, manual, managed, stale)
	metricsErr := errors.New("metrics service unavailable")
	balancer := round2Balancer(t, kube, round2Metrics(nil, metricsErr))
	balancer.config.IgnoreNodes = map[string]struct{}{"ignored": {}}
	if err := balancer.Reconcile(context.Background()); !errors.Is(err, metricsErr) {
		t.Fatalf("metrics error must remain retryable, got %v", err)
	}
	for _, name := range []string{"excluded", "ignored", "stale"} {
		node := round2CurrentNode(t, kube, name)
		if node.Spec.Unschedulable || hasAutoCordonAnnotation(*node) {
			t.Errorf("%q retained stale automatic cordon: %#v", name, node)
		}
	}
	for _, name := range []string{"manual", "managed"} {
		if !round2CurrentNode(t, kube, name).Spec.Unschedulable {
			t.Errorf("%q was unexpectedly uncordoned", name)
		}
	}
	if hasAutoCordonAnnotation(*round2CurrentNode(t, kube, "manual")) {
		t.Error("manual cordon must not be claimed by controller")
	}
}

func TestBalancerScopeCleanupAggregatesFailuresAndContinues(t *testing.T) {
	failed, restored := round2Node("failed", true, true), round2Node("restored", true, true)
	failed.Labels["managed"], restored.Labels["managed"] = "false", "false"
	kube := kubernetesfake.NewSimpleClientset(failed, restored)
	patchErr, metricsErr := errors.New("patch denied"), errors.New("metrics unavailable")
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == "failed" {
			return true, nil, patchErr
		}
		return false, nil, nil
	})
	err := round2Balancer(t, kube, round2Metrics(nil, metricsErr)).Reconcile(context.Background())
	if !errors.Is(err, patchErr) || !errors.Is(err, metricsErr) || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("cleanup and metrics errors must be retained with node context: %v", err)
	}
	if !round2CurrentNode(t, kube, "failed").Spec.Unschedulable || round2CurrentNode(t, kube, "restored").Spec.Unschedulable {
		t.Fatal("failed cleanup must stay pending while other cleanup succeeds")
	}
}

func TestMatchesNodeSelectorRequiresLabelPresenceForEmptyValue(t *testing.T) {
	selector := map[string]string{"managed": ""}
	for _, test := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "missing labels"},
		{name: "different key", labels: map[string]string{"other": ""}},
		{name: "explicit empty value", labels: map[string]string{"managed": ""}, want: true},
		{name: "different value", labels: map[string]string{"managed": "true"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: test.labels}}
			if got := matchesNodeSelector(node, selector); got != test.want {
				t.Fatalf("matchesNodeSelector = %v, want %v", got, test.want)
			}
		})
	}
}

// 最小可用节点数只计算当前确实能接收工作负载且指标有效的节点。
func TestBalancerMinimumExcludesUnavailableNodes(t *testing.T) {
	for _, test := range []struct {
		name           string
		mutate         func(*corev1.Node, *unstructured.Unstructured)
		missingMetrics bool
	}{
		{name: "NotReady", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			node.Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{name: "Ready unknown", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			node.Status.Conditions[0].Status = corev1.ConditionUnknown
		}},
		{name: "missing Ready", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) { node.Status.Conditions = nil }},
		{name: "missing metrics", missingMetrics: true},
		{name: "stale metrics", mutate: func(_ *corev1.Node, metric *unstructured.Unstructured) {
			metric.Object["timestamp"] = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
		}},
		{name: "missing sample window", mutate: func(_ *corev1.Node, metric *unstructured.Unstructured) { delete(metric.Object, "window") }},
		{name: "malformed usage", mutate: func(_ *corev1.Node, metric *unstructured.Unstructured) {
			metric.Object["usage"] = map[string]interface{}{"cpu": "100m", "memory": "invalid"}
		}},
		{name: "NoSchedule taint", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			node.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "other", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{name: "NoExecute taint", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			node.Spec.Taints = []corev1.Taint{{Key: "unreachable", Effect: corev1.TaintEffectNoExecute}}
		}},
		{name: "memory blocked", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			node.Labels["scheduler.example.com/memory-blocked"] = "true"
		}},
		{name: "deleting", mutate: func(node *corev1.Node, _ *unstructured.Unstructured) {
			now := metav1.Now()
			node.DeletionTimestamp = &now
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			busy, spare := round2Node("busy", false, false), round2Node("spare", false, false)
			spareMetric := round2Metric("spare", "20Mi")
			if test.mutate != nil {
				test.mutate(spare, &spareMetric)
			}
			items := []unstructured.Unstructured{round2Metric("busy", "90Mi")}
			if !test.missingMetrics {
				items = append(items, spareMetric)
			}
			kube := kubernetesfake.NewSimpleClientset(busy, spare)
			// Invalid metrics may report a retryable error; the safety decision must hold either way.
			_ = round2Balancer(t, kube, round2Metrics(items, nil)).Reconcile(context.Background())
			if round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
				t.Fatalf("%s spare must not permit cordoning the only usable node", test.name)
			}
		})
	}
}

func TestBalancerHealthySparePermitsCordon(t *testing.T) {
	for _, effect := range []corev1.TaintEffect{"", corev1.TaintEffectPreferNoSchedule} {
		t.Run(string(effect), func(t *testing.T) {
			busy, spare := round2Node("busy", false, false), round2Node("spare", false, false)
			if effect != "" {
				spare.Spec.Taints = []corev1.Taint{{Key: "preferred", Effect: effect}}
			}
			kube := kubernetesfake.NewSimpleClientset(busy, spare)
			if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
				round2Metric("busy", "90Mi"), round2Metric("spare", "20Mi"),
			}, nil)).Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			current := round2CurrentNode(t, kube, "busy")
			if !current.Spec.Unschedulable || !hasAutoCordonAnnotation(*current) {
				t.Fatal("healthy spare must allow automatic cordon")
			}
		})
	}
}

func TestBalancerRecoveryCountsOnlyUsableNodes(t *testing.T) {
	busy, recovery := round2Node("busy", false, false), round2Node("recovery", true, true)
	recovery.Status.Conditions[0].Status = corev1.ConditionFalse
	kube := kubernetesfake.NewSimpleClientset(busy, recovery)
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("recovery", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if round2CurrentNode(t, kube, "recovery").Spec.Unschedulable {
		t.Error("low usage automatic cordon should be restored even while NotReady")
	}
	if round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("NotReady recovery must not count as new usable capacity")
	}
}

func TestBalancerHealthyRecoveryPermitsCordon(t *testing.T) {
	busy, recovery := round2Node("busy", false, false), round2Node("recovery", true, true)
	kube := kubernetesfake.NewSimpleClientset(busy, recovery)
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("recovery", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if round2CurrentNode(t, kube, "recovery").Spec.Unschedulable || !round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("successful healthy recovery must permit replacing the high usage schedulable node")
	}
}

func TestBalancerRecoveryRechecksUsageAgainstCurrentAllocatable(t *testing.T) {
	recovery := round2Node("recovery", true, true)
	kube := kubernetesfake.NewSimpleClientset(recovery)
	changed := false
	kube.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() != "recovery" || changed {
			return false, nil, nil
		}
		changed = true
		current := recovery.DeepCopy()
		current.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("10Mi")
		current.ResourceVersion = "2"
		if err := kube.Tracker().Update(round2NodesGVR, current, ""); err != nil {
			return true, nil, err
		}
		return true, current, nil
	})
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("recovery", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !changed || !round2CurrentNode(t, kube, "recovery").Spec.Unschedulable {
		t.Error("recovery must re-evaluate utilization using current allocatable")
	}
}

func TestBalancerCordonRechecksUsageAgainstCurrentAllocatable(t *testing.T) {
	busy := round2Node("busy", false, false)
	kube := kubernetesfake.NewSimpleClientset(busy, round2Node("spare", false, false))
	changed := false
	kube.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() != "busy" || changed {
			return false, nil, nil
		}
		changed = true
		current := busy.DeepCopy()
		current.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("1000Mi")
		current.ResourceVersion = "2"
		if err := kube.Tracker().Update(round2NodesGVR, current, ""); err != nil {
			return true, nil, err
		}
		return true, current, nil
	})
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := round2CurrentNode(t, kube, "busy")
	if !changed || current.Spec.Unschedulable || hasAutoCordonAnnotation(*current) {
		t.Error("cordon must re-evaluate utilization using current allocatable")
	}
}

func TestBalancerReplacementSpareDoesNotReuseOldMetrics(t *testing.T) {
	spare := round2Node("spare", false, false)
	kube := kubernetesfake.NewSimpleClientset(round2Node("busy", false, false), spare)
	lists := 0
	kube.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		lists++
		if lists == 2 {
			replacement := spare.DeepCopy()
			replacement.UID = "replacement-uid"
			replacement.ResourceVersion = "2"
			if err := kube.Tracker().Update(round2NodesGVR, replacement, ""); err != nil {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lists < 2 {
		t.Fatal("test must replace spare after initial node and metrics snapshot")
	}
	if round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("same-name replacement must not count as spare capacity using old node metrics")
	}
}

func TestBalancerTopCountExcludesUnavailableCandidates(t *testing.T) {
	unavailable := round2Node("unavailable", false, false)
	unavailable.Status.Conditions[0].Status = corev1.ConditionFalse
	kube := kubernetesfake.NewSimpleClientset(unavailable, round2Node("busy", false, false), round2Node("spare", false, false))
	balancer := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("unavailable", "99Mi"), round2Metric("busy", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil))
	balancer.config.TopNodesCount = 1
	if err := balancer.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("an unavailable node must not consume the top-N cordon candidate slot")
	}
	if round2CurrentNode(t, kube, "unavailable").Spec.Unschedulable {
		t.Error("an unavailable node must not be newly cordoned")
	}
}

func TestBalancerManualTakeoverDuringRecoveryDoesNotIncreaseUsableCount(t *testing.T) {
	busy, recovery := round2Node("busy", false, false), round2Node("recovery", true, true)
	kube := kubernetesfake.NewSimpleClientset(busy, recovery)
	takenOver := false
	kube.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() != "recovery" || takenOver {
			return false, nil, nil
		}
		takenOver = true
		manual := recovery.DeepCopy()
		delete(manual.Annotations, autoCordonAnnotation)
		manual.ResourceVersion = "2"
		if err := kube.Tracker().Update(round2NodesGVR, manual, ""); err != nil {
			return true, nil, err
		}
		return true, manual, nil
	})
	if err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("recovery", "20Mi"),
	}, nil)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !takenOver || !round2CurrentNode(t, kube, "recovery").Spec.Unschedulable {
		t.Error("fresh manual cordon must remain in place")
	}
	if round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("skipped recovery must not be counted as newly available capacity")
	}
}

func TestBalancerConcurrentManualCordonReducesUsableCount(t *testing.T) {
	first := round2Node("first", false, false)
	kube := kubernetesfake.NewSimpleClientset(first, round2Node("second", false, false), round2Node("spare", false, false))
	takenOver := false
	kube.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() != "first" || takenOver {
			return false, nil, nil
		}
		takenOver = true
		manual := first.DeepCopy()
		manual.Spec.Unschedulable = true
		manual.ResourceVersion = "2"
		if err := kube.Tracker().Update(round2NodesGVR, manual, ""); err != nil {
			return true, nil, err
		}
		return true, manual, nil
	})
	balancer := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("first", "95Mi"), round2Metric("second", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil))
	balancer.config.MinSchedulableNodes = 2
	if err := balancer.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !takenOver || hasAutoCordonAnnotation(*round2CurrentNode(t, kube, "first")) {
		t.Error("controller must not claim concurrently created manual cordon")
	}
	if round2CurrentNode(t, kube, "second").Spec.Unschedulable {
		t.Error("fresh manual cordon must reduce remaining usable count before another cordon decision")
	}
}

func TestBalancerUncordonFailureDoesNotIncreaseUsableCount(t *testing.T) {
	busy, recovery := round2Node("busy", false, false), round2Node("recovery", true, true)
	kube := kubernetesfake.NewSimpleClientset(busy, recovery)
	patchErr := errors.New("uncordon forbidden")
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == "recovery" {
			return true, nil, patchErr
		}
		return false, nil, nil
	})
	err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("busy", "90Mi"), round2Metric("recovery", "20Mi"),
	}, nil)).Reconcile(context.Background())
	if !errors.Is(err, patchErr) || !strings.Contains(err.Error(), "recovery") {
		t.Fatalf("uncordon failure must return node context: %v", err)
	}
	if round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("failed recovery must not permit cordoning remaining usable node")
	}
	if !round2CurrentNode(t, kube, "recovery").Spec.Unschedulable {
		t.Error("failed recovery unexpectedly uncordoned")
	}
}

func TestBalancerAggregatesCordonErrorsAndContinues(t *testing.T) {
	kube := kubernetesfake.NewSimpleClientset(
		round2Node("failed-a", false, false), round2Node("failed-b", false, false),
		round2Node("success", false, false), round2Node("spare", false, false),
	)
	errA := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "failed-a", errors.New("cordon A denied"))
	errB := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "failed-b", errors.New("cordon B denied"))
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		switch action.(clienttesting.PatchAction).GetName() {
		case "failed-a":
			return true, nil, errA
		case "failed-b":
			return true, nil, errB
		default:
			return false, nil, nil
		}
	})
	err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("failed-a", "95Mi"), round2Metric("failed-b", "90Mi"),
		round2Metric("success", "85Mi"), round2Metric("spare", "20Mi"),
	}, nil)).Reconcile(context.Background())
	if !errors.Is(err, errA) || !errors.Is(err, errB) || !strings.Contains(err.Error(), "failed-a") || !strings.Contains(err.Error(), "failed-b") {
		t.Fatalf("all cordon failures must remain in error chain with node context: %v", err)
	}
	if !round2CurrentNode(t, kube, "success").Spec.Unschedulable {
		t.Error("one patch failure must not prevent other node decisions")
	}
}

// 超时可能发生在 API 已应用补丁后，本轮须保守停止继续 cordon。
func TestBalancerAmbiguousCordonFailureStopsAdditionalCordon(t *testing.T) {
	kube := kubernetesfake.NewSimpleClientset(
		round2Node("first", false, false), round2Node("second", false, false), round2Node("spare", false, false),
	)
	patchErr := apierrors.NewTimeoutError("response lost after successful patch", 1)
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() != "first" {
			return false, nil, nil
		}
		committed := round2Node("first", true, true)
		committed.ResourceVersion = "2"
		if err := kube.Tracker().Update(round2NodesGVR, committed, ""); err != nil {
			return true, nil, err
		}
		return true, nil, patchErr
	})
	balancer := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		round2Metric("first", "95Mi"), round2Metric("second", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil))
	balancer.config.MinSchedulableNodes = 2
	if err := balancer.Reconcile(context.Background()); !errors.Is(err, patchErr) {
		t.Fatalf("timeout must remain retryable: %v", err)
	}
	if !round2CurrentNode(t, kube, "first").Spec.Unschedulable {
		t.Error("test must simulate committed patch with missing response")
	}
	if round2CurrentNode(t, kube, "second").Spec.Unschedulable {
		t.Error("uncertain result must prevent further cordon from violating minimum")
	}
}

func TestBalancerMetricErrorsDoNotPreventHealthyNodeActions(t *testing.T) {
	kube := kubernetesfake.NewSimpleClientset(
		round2Node("broken-a", false, false), round2Node("broken-b", false, false),
		round2Node("busy", false, false), round2Node("spare", false, false),
	)
	brokenUsage, stale := round2Metric("broken-a", "invalid"), round2Metric("broken-b", "20Mi")
	stale.Object["timestamp"] = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
	err := round2Balancer(t, kube, round2Metrics([]unstructured.Unstructured{
		brokenUsage, stale, round2Metric("busy", "90Mi"), round2Metric("spare", "20Mi"),
	}, nil)).Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broken-a") || !strings.Contains(err.Error(), "broken-b") {
		t.Fatalf("invalid usage and stale sample must both be reported: %v", err)
	}
	if !round2CurrentNode(t, kube, "busy").Spec.Unschedulable {
		t.Error("invalid metrics on another node must not stop safe decisions for healthy nodes")
	}
}

func TestSetUnschedulablePreservesManualOwnership(t *testing.T) {
	for _, value := range []bool{false, true} {
		name := "uncordon"
		if value {
			name = "cordon"
		}
		t.Run(name, func(t *testing.T) {
			stale := round2Node("node", !value, !value)
			manual := round2Node("node", true, false)
			kube := kubernetesfake.NewSimpleClientset(manual)
			if err := setUnschedulable(context.Background(), kube, *stale, value); err != nil {
				t.Fatal(err)
			}
			current := round2CurrentNode(t, kube, "node")
			if !current.Spec.Unschedulable || hasAutoCordonAnnotation(*current) {
				t.Fatal("fresh manual ownership must be preserved")
			}
		})
	}
}

func TestSetUnschedulableRejectsReplacementNode(t *testing.T) {
	for _, value := range []bool{false, true} {
		stale := round2Node("node", !value, !value)
		replacement := stale.DeepCopy()
		replacement.UID = "replacement-uid"
		kube := kubernetesfake.NewSimpleClientset(replacement)
		if err := setUnschedulable(context.Background(), kube, *stale, value); err == nil {
			t.Fatalf("UID replacement must abort transition %v", value)
		}
		current := round2CurrentNode(t, kube, "node")
		if current.Spec.Unschedulable != replacement.Spec.Unschedulable || hasAutoCordonAnnotation(*current) != hasAutoCordonAnnotation(*replacement) {
			t.Error("stale snapshot must not modify same-name replacement")
		}
	}
}

func TestSetUnschedulableRechecksOwnershipAfterConflict(t *testing.T) {
	for _, value := range []bool{false, true} {
		name := "uncordon"
		if value {
			name = "cordon"
		}
		t.Run(name, func(t *testing.T) {
			initial := round2Node("node", !value, !value)
			kube := kubernetesfake.NewSimpleClientset(initial)
			patches := 0
			kube.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				patches++
				if patches != 1 {
					return false, nil, nil
				}
				// 在首个补丁冲突期间，管理员取得 cordon 所有权。
				manual := round2Node("node", true, false)
				manual.ResourceVersion = "2"
				if err := kube.Tracker().Update(round2NodesGVR, manual, ""); err != nil {
					return true, nil, err
				}
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node", errors.New("changed concurrently"))
			})
			if err := setUnschedulable(context.Background(), kube, *initial, value); err != nil {
				t.Fatal(err)
			}
			current := round2CurrentNode(t, kube, "node")
			if !current.Spec.Unschedulable || hasAutoCordonAnnotation(*current) || patches != 1 {
				t.Fatalf("retry must respect fresh manual ownership, patches=%d node=%#v", patches, current)
			}
		})
	}
}

func TestStaleAnnotationCleanupPreservesLaterManualCordon(t *testing.T) {
	stale := round2Node("node", false, true)
	manual := stale.DeepCopy()
	manual.Spec.Unschedulable = true
	manual.ResourceVersion = "2"
	kube := kubernetesfake.NewSimpleClientset(manual)
	if err := setUnschedulable(context.Background(), kube, *stale, false); err != nil {
		t.Fatal(err)
	}
	current := round2CurrentNode(t, kube, "node")
	if !current.Spec.Unschedulable || hasAutoCordonAnnotation(*current) {
		t.Fatal("stale annotation cleanup must preserve later manual cordon and remove only stale annotation")
	}
}

func TestPolicyDeletionCleanupAggregatesErrorsAndKeepsManualNodes(t *testing.T) {
	kube := kubernetesfake.NewSimpleClientset(
		round2Node("failed", true, true), round2Node("success", true, true),
		round2Node("manual", true, false), round2Node("stale", false, true),
	)
	patchErr := errors.New("cleanup denied")
	kube.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == "failed" {
			return true, nil, patchErr
		}
		return false, nil, nil
	})
	if err := UncordonAutoCordonedNodes(context.Background(), kube); !errors.Is(err, patchErr) || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("deletion cleanup must retain node error: %v", err)
	}
	if round2CurrentNode(t, kube, "success").Spec.Unschedulable {
		t.Error("cleanup must continue after failed node")
	}
	if !round2CurrentNode(t, kube, "manual").Spec.Unschedulable {
		t.Error("cleanup modified manual cordon")
	}
	if hasAutoCordonAnnotation(*round2CurrentNode(t, kube, "stale")) {
		t.Error("deletion cleanup must remove stale automatic ownership annotation")
	}
}

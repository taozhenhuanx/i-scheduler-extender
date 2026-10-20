package extender

import (
	"context"
	"errors"
	"fmt"
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
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

type memoryFixture struct {
	ex        *Extender
	core      *kubernetesfake.Clientset
	metrics   map[string]interface{}
	bindCalls int
}

func newMemoryFixture(t *testing.T, pods ...*corev1.Pod) *memoryFixture {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1000Mi"),
			corev1.ResourceCPU:    resource.MustParse("4"),
		}},
	}
	objects := []runtime.Object{node}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	fixture := &memoryFixture{
		core:    kubernetesfake.NewSimpleClientset(objects...),
		metrics: memoryMetric(time.Now().Add(-30*time.Second), "10s", "600Mi"),
	}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nodeusage.NodeMetricsGVR: "NodeMetricsList",
	})
	// NodeMetrics 的资源名是 nodes。直接提供 List 响应，避免 fake tracker 猜测 plural。
	dynamic.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: runtime.DeepCopyJSON(fixture.metrics)}}}, nil
	})
	fixture.core.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "binding" {
			return false, nil, nil
		}
		binding := action.(clienttesting.CreateAction).GetObject().(*corev1.Binding)
		fixture.bindCalls++
		object, err := fixture.core.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), binding.Namespace, binding.Name)
		if err != nil {
			return true, nil, err
		}
		pod := object.(*corev1.Pod).DeepCopy()
		pod.Spec.NodeName = binding.Target.Name
		err = fixture.core.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, binding.Namespace)
		return true, binding, err
	})
	fixture.ex = NewExtenderWithClients(fixture.core, dynamic, 0.1, 0.9)
	return fixture
}

func memoryMetric(timestamp time.Time, window, memory string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "metrics.k8s.io/v1beta1",
		"kind":       "NodeMetrics",
		"metadata":   map[string]interface{}{"name": "node-a"},
		"timestamp":  timestamp.Format(time.RFC3339Nano),
		"window":     window,
		"usage":      map[string]interface{}{"cpu": "1", "memory": memory},
	}
}

func memoryPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(name + "-uid")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("150Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("300Mi")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func runningMemoryPod(name string, startedAt time.Time) *corev1.Pod {
	pod := memoryPod(name)
	pod.Spec.NodeName = "node-a"
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(startedAt)}},
	}}
	return pod
}

func (fixture *memoryFixture) invalidateSnapshots() {
	fixture.ex.podMemoryAt = time.Time{}
	fixture.ex.metricsAt = time.Time{}
}

func bindingArgs(pod *corev1.Pod) extenderv1.ExtenderBindingArgs {
	return extenderv1.ExtenderBindingArgs{PodName: pod.Name, PodNamespace: pod.Namespace, PodUID: pod.UID, Node: "node-a"}
}

func updateMemoryPod(t *testing.T, fixture *memoryFixture, name string, update func(*corev1.Pod)) {
	t.Helper()
	pod, err := fixture.core.CoreV1().Pods("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	update(pod)
	if _, err := fixture.core.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture.invalidateSnapshots()
}

func TestBindRetainsMemoryUntilStartupSampleCoversPod(t *testing.T) {
	ctx := context.Background()
	first, second := memoryPod("first"), memoryPod("second")
	fixture := newMemoryFixture(t, first, second)
	if result := fixture.ex.BindWithContext(ctx, bindingArgs(first)); result.Error != "" {
		t.Fatalf("first Bind failed: %s", result.Error)
	}
	assigned, err := fixture.core.CoreV1().Pods(first.Namespace).Get(ctx, first.Name, metav1.GetOptions{})
	if err != nil || assigned.Spec.NodeName != "node-a" {
		t.Fatalf("fake Bind must assign the Pod to the node: pod=%v, error=%v", assigned, err)
	}
	// 即使 API 已设置 nodeName，镜像仍在拉取且样本仍旧时，第二个 Pod 会使预计占用到 90%。
	fixture.invalidateSnapshots()
	if result := fixture.ex.BindWithContext(ctx, bindingArgs(second)); !strings.Contains(result.Error, "above 80%") {
		t.Fatalf("second Bind should retain first reservation and fail the 80%% guard, got %q", result.Error)
	}
	if fixture.bindCalls != 1 {
		t.Fatalf("rejected second Pod reached Bind API; calls=%d", fixture.bindCalls)
	}
	// Running 本身仍不足以释放预算：旧窗口起点早于容器本次启动。
	updateMemoryPod(t, fixture, first.Name, func(pod *corev1.Pod) {
		pod.Status = runningMemoryPod(first.Name, time.Now().Add(-10*time.Second)).Status
	})
	if result := fixture.ex.BindWithContext(ctx, bindingArgs(second)); result.Error == "" {
		t.Fatal("old sample incorrectly released the newly running Pod reservation")
	}
	// 新采样已覆盖 first：实测 65% 加 second 的 15% 正好到 80%，同时满足标签恢复条件。
	fixture.metrics = memoryMetric(time.Now(), "5s", "650Mi")
	fixture.invalidateSnapshots()
	if result := fixture.ex.BindWithContext(ctx, bindingArgs(second)); result.Error != "" {
		t.Fatalf("covered first reservation should allow second Pod: %s", result.Error)
	}
	if _, exists := fixture.ex.pendingMemory[podReservationKey(first)]; exists {
		t.Fatal("first reservation was not removed after a covering sample")
	}
	if len(fixture.ex.pendingMemory) != 1 || fixture.bindCalls != 2 {
		t.Fatalf("want only second reservation and two API binds, reservations=%v calls=%d", fixture.ex.pendingMemory, fixture.bindCalls)
	}
	node, err := fixture.core.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil || node.Labels[memoryBlockedLabel] == "true" {
		t.Fatalf("covering sample should recover the node label: node=%v error=%v", node, err)
	}
}

func TestPendingMemoryReservationCleanup(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *memoryFixture, *corev1.Pod)
	}{
		{name: "deleted", change: func(t *testing.T, fixture *memoryFixture, pod *corev1.Pod) {
			if err := fixture.core.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "succeeded", change: func(t *testing.T, fixture *memoryFixture, pod *corev1.Pod) {
			updateMemoryPod(t, fixture, pod.Name, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodSucceeded })
		}},
		{name: "failed", change: func(t *testing.T, fixture *memoryFixture, pod *corev1.Pod) {
			updateMemoryPod(t, fixture, pod.Name, func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed })
		}},
		{name: "same name new UID", change: func(t *testing.T, fixture *memoryFixture, pod *corev1.Pod) {
			updateMemoryPod(t, fixture, pod.Name, func(pod *corev1.Pod) { pod.UID = "replacement-uid" })
		}},
		{name: "assigned to another node", change: func(t *testing.T, fixture *memoryFixture, pod *corev1.Pod) {
			updateMemoryPod(t, fixture, pod.Name, func(pod *corev1.Pod) { pod.Spec.NodeName = "node-b" })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := memoryPod("reserved")
			pod.Spec.NodeName = "node-a"
			fixture := newMemoryFixture(t, pod)
			fixture.ex.reservePodMemory(pod, "node-a", 150)
			test.change(t, fixture, pod)
			fixture.invalidateSnapshots()
			pending, err := fixture.ex.pendingMemoryByNode(context.Background(), map[string]map[string]interface{}{"node-a": fixture.metrics})
			if err != nil || pending["node-a"] != 0 || len(fixture.ex.pendingMemory) != 0 {
				t.Fatalf("obsolete reservation remains: pending=%v reservations=%v error=%v", pending, fixture.ex.pendingMemory, err)
			}
		})
	}
}

func TestPendingMemoryLongPendingHasNoExpiry(t *testing.T) {
	pod := memoryPod("slow-image-pull")
	pod.Spec.NodeName = "node-a"
	fixture := newMemoryFixture(t, pod)
	fixture.ex.reservePodMemory(pod, "node-a", 150)
	key := podReservationKey(pod)
	reservation := fixture.ex.pendingMemory[key]
	reservation.createdAt = time.Now().Add(-24 * time.Hour)
	fixture.ex.pendingMemory[key] = reservation
	pending, err := fixture.ex.pendingMemoryByNode(context.Background(), map[string]map[string]interface{}{"node-a": fixture.metrics})
	if err != nil || pending["node-a"] != 150 {
		t.Fatalf("long Pending Pod must retain memory budget: pending=%v error=%v", pending, err)
	}
}

func TestMemoryReservationKeysKeepSameNameUIDsSeparate(t *testing.T) {
	oldPod := memoryPod("recreated")
	newPod := oldPod.DeepCopy()
	newPod.UID = "new-uid"
	fixture := newMemoryFixture(t, newPod)
	fixture.ex.reservePodMemory(oldPod, "node-a", 100)
	fixture.ex.reservePodMemory(newPod, "node-a", 150)
	if len(fixture.ex.pendingMemory) != 2 {
		t.Fatal("same-name Pods with different UIDs overwrote each other's reservations")
	}
	if got := fixture.ex.ownPendingMemory(newPod, "node-a"); got != 150 {
		t.Fatalf("new UID's own reservation=%d, want 150", got)
	}
	fixture.ex.releasePodMemory(oldPod)
	if got := fixture.ex.ownPendingMemory(newPod, "node-a"); got != 150 {
		t.Fatalf("releasing old UID removed new UID's reservation: %d", got)
	}
	fixture.ex.reservePodMemory(oldPod, "node-a", 100)
	pending, err := fixture.ex.pendingMemoryByNode(context.Background(), nil)
	if err != nil || pending["node-a"] != 150 || len(fixture.ex.pendingMemory) != 1 {
		t.Fatalf("fresh snapshot should remove only old UID: pending=%v reservations=%v error=%v", pending, fixture.ex.pendingMemory, err)
	}
}

func TestPendingMemoryDoesNotCleanupFromSnapshotBeforeReservation(t *testing.T) {
	for _, oldPod := range []*corev1.Pod{nil, memoryPod("same-name")} {
		name := "missing from cache"
		if oldPod != nil {
			name = "different UID in cache"
		}
		t.Run(name, func(t *testing.T) {
			pod := memoryPod("same-name")
			pod.UID = "new-uid"
			fixture := newMemoryFixture(t, pod)
			cachedPods := map[string]*corev1.Pod{}
			if oldPod != nil {
				cachedPods[podMemoryKey(oldPod.Namespace, oldPod.Name)] = oldPod
			}
			fixture.ex.podMemory = &podMemorySnapshot{pods: cachedPods, observedAt: time.Now().Add(-time.Second)}
			fixture.ex.podMemoryAt = time.Now()
			fixture.ex.reservePodMemory(pod, "node-a", 150)
			pending, err := fixture.ex.pendingMemoryByNode(context.Background(), nil)
			if err != nil || pending["node-a"] != 150 {
				t.Fatalf("pre-reservation snapshot must not clean a new reservation: pending=%v error=%v", pending, err)
			}
		})
	}
}

func TestInitializeMemoryReservationsRebuildsOnlyUncoveredAssignedPods(t *testing.T) {
	now := time.Now()
	pending := memoryPod("pending")
	pending.Spec.NodeName = "node-a"
	starting := runningMemoryPod("starting", now.Add(-time.Second))
	stable := runningMemoryPod("stable", now.Add(-time.Hour))
	terminal := memoryPod("terminal")
	terminal.Spec.NodeName = "node-a"
	terminal.Status.Phase = corev1.PodSucceeded
	unassigned := memoryPod("unassigned")
	noBudget := memoryPod("no-budget")
	noBudget.Spec.NodeName = "node-a"
	noBudget.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	fixture := newMemoryFixture(t, pending, starting, stable, terminal, unassigned, noBudget)
	fixture.metrics = memoryMetric(now, "10s", "600Mi")
	if err := fixture.ex.InitializeMemoryReservations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.ex.pendingMemory) != 2 {
		t.Fatalf("only Pending and sample-uncovered Running Pods should be rebuilt, got %v", fixture.ex.pendingMemory)
	}
	for _, pod := range []*corev1.Pod{pending, starting} {
		reservation, ok := fixture.ex.pendingMemory[podReservationKey(pod)]
		if !ok || reservation.bytes != 150*1024*1024 {
			t.Fatalf("missing request-based reservation for %q: %v", pod.Name, reservation)
		}
	}
	if err := fixture.ex.InitializeMemoryReservations(context.Background()); err != nil || len(fixture.ex.pendingMemory) != 2 {
		t.Fatalf("repeated initialization must not duplicate budgets: reservations=%v error=%v", fixture.ex.pendingMemory, err)
	}
}

func TestMemorySampleStartValidatesFreshnessAndWindow(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		mutate  func(map[string]interface{})
		want    time.Time
		wantErr bool
	}{
		{name: "fresh window", want: now.Add(-20 * time.Second)},
		{name: "missing timestamp", mutate: func(metric map[string]interface{}) { delete(metric, "timestamp") }, wantErr: true},
		{name: "malformed timestamp", mutate: func(metric map[string]interface{}) { metric["timestamp"] = "bad" }, wantErr: true},
		{name: "nonstring timestamp", mutate: func(metric map[string]interface{}) { metric["timestamp"] = int64(1) }, wantErr: true},
		{name: "stale timestamp", mutate: func(metric map[string]interface{}) {
			metric["timestamp"] = now.Add(-memorySampleMaxAge - time.Second).Format(time.RFC3339)
		}, wantErr: true},
		{name: "future beyond skew", mutate: func(metric map[string]interface{}) {
			metric["timestamp"] = now.Add(memoryClockSkew + time.Second).Format(time.RFC3339)
		}, wantErr: true},
		{name: "future within skew clamps to now", mutate: func(metric map[string]interface{}) {
			metric["timestamp"] = now.Add(5 * time.Second).Format(time.RFC3339)
		}, want: now.Add(-10 * time.Second)},
		{name: "missing window", mutate: func(metric map[string]interface{}) { delete(metric, "window") }, wantErr: true},
		{name: "invalid window", mutate: func(metric map[string]interface{}) { metric["window"] = "bad" }, wantErr: true},
		{name: "zero window", mutate: func(metric map[string]interface{}) { metric["window"] = "0s" }, wantErr: true},
		{name: "negative window", mutate: func(metric map[string]interface{}) { metric["window"] = "-1s" }, wantErr: true},
		{name: "overlong window", mutate: func(metric map[string]interface{}) { metric["window"] = "3m" }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			metric := memoryMetric(now.Add(-10*time.Second), "10s", "600Mi")
			if test.mutate != nil {
				test.mutate(metric)
			}
			got, err := memorySampleStart(metric, now)
			if (err != nil) != test.wantErr || (!test.wantErr && !got.Equal(test.want)) {
				t.Fatalf("memorySampleStart()=(%v, %v), want (%v, error=%t)", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestPodMemoryCoveredRequiresAllRunningContainers(t *testing.T) {
	now := time.Now()
	metric := memoryMetric(now, "10s", "600Mi")
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
		want   bool
	}{
		{name: "complete window after startup", want: true},
		{name: "startup equals window start", mutate: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(now.Add(-10 * time.Second))
		}, want: true},
		{name: "startup during window", mutate: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(now.Add(-5 * time.Second))
		}},
		{name: "missing container status", mutate: func(pod *corev1.Pod) { pod.Status.ContainerStatuses = nil }},
		{name: "waiting after restart", mutate: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		}},
		{name: "missing startedAt", mutate: func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Time{} }},
		{name: "second app not running", mutate: func(pod *corev1.Pod) {
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "second"})
		}},
		{name: "sidecar init not running", mutate: func(pod *corev1.Pod) {
			always := corev1.ContainerRestartPolicyAlways
			pod.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: &always}}
		}},
		{name: "sidecar init covered", mutate: func(pod *corev1.Pod) {
			always := corev1.ContainerRestartPolicyAlways
			pod.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: &always}}
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}
		}, want: true},
		{name: "completed ordinary init does not require running status", mutate: func(pod *corev1.Pod) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
		}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := runningMemoryPod("covered", now.Add(-time.Minute))
			if test.mutate != nil {
				test.mutate(pod)
			}
			if got := podMemoryCovered(pod, metric, now); got != test.want {
				t.Fatalf("podMemoryCovered()=%t, want %t", got, test.want)
			}
		})
	}
	if podMemoryCovered(runningMemoryPod("stale", now.Add(-time.Hour)), memoryMetric(now.Add(-3*time.Minute), "10s", "600Mi"), now) {
		t.Fatal("stale samples must not release even a stable Pod reservation")
	}
}

func TestInvalidMemorySampleStopsBindAndFilter(t *testing.T) {
	for _, metric := range []map[string]interface{}{
		memoryMetric(time.Now().Add(-3*time.Minute), "10s", "600Mi"),
		memoryMetric(time.Now().Add(time.Minute), "10s", "600Mi"),
		memoryMetric(time.Now(), "0s", "600Mi"),
	} {
		t.Run(fmt.Sprint(metric["timestamp"], "/", metric["window"]), func(t *testing.T) {
			pod := memoryPod("new")
			fixture := newMemoryFixture(t, pod)
			fixture.metrics = metric
			if result := fixture.ex.BindWithContext(context.Background(), bindingArgs(pod)); !strings.Contains(result.Error, "invalid memory metrics") {
				t.Fatalf("Bind accepted invalid sample: %v", result)
			}
			node, err := fixture.core.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			filtered := fixture.ex.FilterWithContext(context.Background(), extenderv1.ExtenderArgs{Pod: pod, Nodes: &corev1.NodeList{Items: []corev1.Node{*node}}})
			if filtered.Error != "" || filtered.Nodes == nil || len(filtered.Nodes.Items) != 0 || fixture.bindCalls != 0 {
				t.Fatalf("invalid sample should exclude node without API Bind: result=%v calls=%d", filtered, fixture.bindCalls)
			}
		})
	}
}

func TestBindErrorReservationsAndRetry(t *testing.T) {
	for _, test := range []struct {
		name       string
		bindError  error
		wantRetain bool
	}{
		{name: "ambiguous transport error retains budget", bindError: errors.New("connection reset by peer"), wantRetain: true},
		{name: "forbidden releases budget", bindError: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "retry", errors.New("denied"))},
		{name: "not found releases budget", bindError: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "retry")},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := memoryPod("retry")
			fixture := newMemoryFixture(t, pod)
			failed := false
			fixture.core.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "binding" && !failed {
					failed = true
					return true, nil, test.bindError
				}
				return false, nil, nil
			})
			if result := fixture.ex.BindWithContext(context.Background(), bindingArgs(pod)); result.Error == "" {
				t.Fatal("expected injected Bind failure")
			}
			_, retained := fixture.ex.pendingMemory[podReservationKey(pod)]
			if retained != test.wantRetain {
				t.Fatalf("reservation retained=%t, want %t", retained, test.wantRetain)
			}
			// 重试自己的预约必须先扣除，不能将同一 Pod 的预算累计两次而变成 90%。
			fixture.invalidateSnapshots()
			node, err := fixture.core.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			filtered := fixture.ex.FilterWithContext(context.Background(), extenderv1.ExtenderArgs{Pod: pod, Nodes: &corev1.NodeList{Items: []corev1.Node{*node}}})
			if filtered.Error != "" || filtered.Nodes == nil || len(filtered.Nodes.Items) != 1 {
				t.Fatalf("same-Pod Filter retry double-counted budget: %v", filtered)
			}
			if result := fixture.ex.BindWithContext(context.Background(), bindingArgs(pod)); result.Error != "" {
				t.Fatalf("same-Pod retry double-counted budget: %s", result.Error)
			}
		})
	}
}

func TestBindConflictAfterTimeoutKeepsReservationUntilSampleCoversPod(t *testing.T) {
	for _, test := range []struct {
		name       string
		retryError error
	}{
		{name: "conflict", retryError: apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "racing", errors.New("already bound"))},
		{name: "forbidden after prior request", retryError: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "racing", errors.New("permission changed"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := memoryPod("racing")
			fixture := newMemoryFixture(t, pod)
			attempts := 0
			fixture.core.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "binding" {
					return false, nil, nil
				}
				attempts++
				if attempts == 1 {
					return true, nil, apierrors.NewTimeoutError("response lost after request was accepted", 1)
				}
				// 重试 Get 尚看到未绑定；进入第二 Bind 后，第一次请求才在 API 完成绑定。
				binding := action.(clienttesting.CreateAction).GetObject().(*corev1.Binding)
				object, err := fixture.core.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), binding.Namespace, binding.Name)
				if err != nil {
					return true, nil, err
				}
				assigned := object.(*corev1.Pod).DeepCopy()
				assigned.Spec.NodeName = binding.Target.Name
				if err := fixture.core.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), assigned, assigned.Namespace); err != nil {
					return true, nil, err
				}
				return true, nil, test.retryError
			})
			for attempt := 1; attempt <= 2; attempt++ {
				fixture.invalidateSnapshots()
				if result := fixture.ex.BindWithContext(context.Background(), bindingArgs(pod)); result.Error == "" {
					t.Fatalf("attempt %d should fail with injected API error", attempt)
				}
				if _, retained := fixture.ex.pendingMemory[podReservationKey(pod)]; !retained {
					t.Fatalf("attempt %d removed potentially completed Bind's memory reservation", attempt)
				}
			}
			fixture.invalidateSnapshots()
			metrics := map[string]map[string]interface{}{"node-a": fixture.metrics}
			pending, err := fixture.ex.pendingMemoryByNode(context.Background(), metrics)
			if err != nil || pending["node-a"] != 150*1024*1024 {
				t.Fatalf("nodeName set after timeout/conflict must retain budget: pending=%v error=%v", pending, err)
			}
			updateMemoryPod(t, fixture, pod.Name, func(pod *corev1.Pod) {
				pod.Status = runningMemoryPod(pod.Name, time.Now().Add(-time.Minute)).Status
			})
			metrics["node-a"] = memoryMetric(time.Now(), "10s", "650Mi")
			pending, err = fixture.ex.pendingMemoryByNode(context.Background(), metrics)
			if err != nil || pending["node-a"] != 0 || len(fixture.ex.pendingMemory) != 0 {
				t.Fatalf("covering sample did not release reservation: pending=%v reservations=%v error=%v", pending, fixture.ex.pendingMemory, err)
			}
		})
	}
}

func TestUnresolvedBindingCannotMoveReservationToAnotherNode(t *testing.T) {
	ctx := context.Background()
	pod := memoryPod("unresolved")
	fixture := newMemoryFixture(t, pod)
	firstNode, err := fixture.core.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondNode := firstNode.DeepCopy()
	secondNode.Name = "node-b"
	if _, err := fixture.core.CoreV1().Nodes().Create(ctx, secondNode, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// 提供两节点的有效指标，使测试能证明确实是未决 Bind 规则排除 node-b。
	dynamic := fixture.ex.DynamicClient.(*dynamicfake.FakeDynamicClient)
	dynamic.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		secondMetric := runtime.DeepCopyJSON(fixture.metrics)
		secondMetric["metadata"] = map[string]interface{}{"name": "node-b"}
		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: runtime.DeepCopyJSON(fixture.metrics)}, {Object: secondMetric},
		}}, nil
	})
	apiCalls := 0
	fixture.core.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "binding" {
			return false, nil, nil
		}
		apiCalls++
		return true, nil, apierrors.NewTimeoutError("API binding result is unknown", 1)
	})
	if result := fixture.ex.BindWithContext(ctx, bindingArgs(pod)); result.Error == "" {
		t.Fatal("first Bind should return the injected timeout")
	}
	reservation := fixture.ex.pendingMemory[podReservationKey(pod)]
	fixture.invalidateSnapshots()
	secondArgs := bindingArgs(pod)
	secondArgs.Node = "node-b"
	if result := fixture.ex.BindWithContext(ctx, secondArgs); !strings.Contains(result.Error, "unresolved binding") {
		t.Fatalf("retry on another node should return unresolved binding error: %q", result.Error)
	}
	if apiCalls != 1 {
		t.Fatalf("retry on different node reached Bind API: calls=%d", apiCalls)
	}
	if got := fixture.ex.pendingMemory[podReservationKey(pod)]; got != reservation || got.nodeName != "node-a" {
		t.Fatalf("node-a's unresolved reservation was overwritten: before=%v after=%v", reservation, got)
	}
	filtered := fixture.ex.FilterWithContext(ctx, extenderv1.ExtenderArgs{
		Pod: pod, Nodes: &corev1.NodeList{Items: []corev1.Node{*firstNode, *secondNode}},
	})
	if filtered.Error != "" || filtered.Nodes == nil || len(filtered.Nodes.Items) != 1 || filtered.Nodes.Items[0].Name != "node-a" {
		t.Fatalf("Filter should allow retry only on unresolved original node: %v", filtered)
	}
	if filtered.NodeNames == nil || len(*filtered.NodeNames) != 1 || (*filtered.NodeNames)[0] != "node-a" {
		t.Fatalf("Filter NodeNames should contain only node-a: %v", filtered.NodeNames)
	}
}

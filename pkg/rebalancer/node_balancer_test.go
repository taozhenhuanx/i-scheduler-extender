package rebalancer

import (
	"context"
	"testing"
	"time"

	"github.com/lixd96/i-scheduler-extender/pkg/nodeusage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBalancerCordonUsesZeroAndOnePercentWeights(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name        string
		cpu, memory float64
		wantCordon  bool
	}{
		{name: "memory only", cpu: 0, memory: 1},
		{name: "cpu only", cpu: 1, memory: 0, wantCordon: true},
		{name: "one percent CPU", cpu: 0.01, memory: 0.99},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "busy"},
				Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("100Mi"),
				}},
			}
			spare := node.DeepCopy()
			spare.Name = "spare"
			kube := kubernetesfake.NewSimpleClientset(&node, spare)
			metrics := func(name, cpu, memory string) *unstructured.Unstructured {
				return &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
					"metadata": map[string]interface{}{"name": name},
					"usage":    map[string]interface{}{"cpu": cpu, "memory": memory},
				}}
			}
			dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{nodeusage.NodeMetricsGVR: "NodeMetricsList"})
			// Metrics API 的 nodes 路径与 NodeMetrics 的默认复数不一致，明确模拟真实 List 返回。
			dynamicClient.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					*metrics("busy", "1", "50Mi"), *metrics("spare", "0", "0"),
				}}, nil
			})
			balancer, err := New(kube, dynamicClient, Config{
				CheckInterval: time.Second, CPUWeight: test.cpu, MemoryWeight: test.memory,
				HighThreshold: 60, RecoveryThreshold: 55, MinSchedulableNodes: 1, TopNodesCount: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := balancer.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := kube.CoreV1().Nodes().Get(ctx, "busy", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if current.Spec.Unschedulable != test.wantCordon {
				t.Fatalf("cordon = %v, want %v for weights (%v, %v)", current.Spec.Unschedulable, test.wantCordon, test.cpu, test.memory)
			}
		})
	}
}

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	schedulerv1alpha1 "github.com/lixd96/i-scheduler-extender/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

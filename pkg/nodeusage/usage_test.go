package nodeusage

import (
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMemorySampleStartUsesSharedFreshnessRules(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		timestamp interface{}
		window    interface{}
		wantStart time.Time
		wantErr   bool
	}{
		{name: "fresh", timestamp: now.Add(-10 * time.Second).Format(time.RFC3339), window: "10s", wantStart: now.Add(-20 * time.Second)},
		{name: "future within skew", timestamp: now.Add(5 * time.Second).Format(time.RFC3339), window: "10s", wantStart: now.Add(-10 * time.Second)},
		{name: "old", timestamp: now.Add(-3 * time.Minute).Format(time.RFC3339), window: "10s", wantErr: true},
		{name: "future", timestamp: now.Add(11 * time.Second).Format(time.RFC3339), window: "10s", wantErr: true},
		{name: "missing timestamp", window: "10s", wantErr: true},
		{name: "bad timestamp", timestamp: "bad", window: "10s", wantErr: true},
		{name: "missing window", timestamp: now.Format(time.RFC3339), wantErr: true},
		{name: "zero window", timestamp: now.Format(time.RFC3339), window: "0s", wantErr: true},
		{name: "overlong window", timestamp: now.Format(time.RFC3339), window: "3m", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := MemorySampleStart(map[string]interface{}{"timestamp": test.timestamp, "window": test.window}, now)
			if (err != nil) != test.wantErr || (!test.wantErr && !got.Equal(test.wantStart)) {
				t.Fatalf("MemorySampleStart = (%v, %v), want (%v, error=%t)", got, err, test.wantStart, test.wantErr)
			}
		})
	}
}

func TestInvalidResourceQuantitiesAreNotUsableMetrics(t *testing.T) {
	for _, test := range []struct {
		name, cpu, memory, capacityCPU, capacityMemory string
	}{
		{name: "negative CPU usage", cpu: "-1", memory: "10Mi", capacityCPU: "1", capacityMemory: "100Mi"},
		{name: "negative memory usage", cpu: "0", memory: "-1Mi", capacityCPU: "1", capacityMemory: "100Mi"},
		{name: "zero CPU allocatable", cpu: "0", memory: "10Mi", capacityCPU: "0", capacityMemory: "100Mi"},
		{name: "negative memory allocatable", cpu: "0", memory: "10Mi", capacityCPU: "1", capacityMemory: "-100Mi"},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(test.capacityCPU), corev1.ResourceMemory: resource.MustParse(test.capacityMemory),
			}}}
			metric := map[string]interface{}{"usage": map[string]interface{}{"cpu": test.cpu, "memory": test.memory}}
			if _, err := WeightedUsagePercentage(node, metric, 0.1, 0.9); err == nil {
				t.Fatal("invalid quantity must not produce weighted usage")
			}
		})
	}
	if _, err := MemoryUsageBytes(map[string]interface{}{"usage": map[string]interface{}{"memory": "-1Mi"}}); err == nil {
		t.Fatal("negative memory must not lower the admission baseline")
	}
}

func TestValidateWeightsRequiresFractionUnitsAndUnitTotal(t *testing.T) {
	for _, weights := range [][2]float64{{0, 1}, {1, 0}, {0.01, 0.99}, {0.1, 0.9}} {
		if err := ValidateWeights(weights[0], weights[1]); err != nil {
			t.Errorf("ValidateWeights(%v) rejected valid fractions: %v", weights, err)
		}
	}
	for _, weights := range [][2]float64{{0, 0}, {1, 99}, {10, 90}, {0.1, 0.1}, {-0.1, 1.1}, {math.NaN(), 1}, {0, math.Inf(1)}} {
		if err := ValidateWeights(weights[0], weights[1]); err == nil {
			t.Errorf("ValidateWeights(%v) accepted invalid fractions", weights)
		}
	}
}

func TestWeightedUsagePercentagePreservesOnePercentWeight(t *testing.T) {
	node := corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("100Mi"),
	}}}
	metric := map[string]interface{}{"usage": map[string]interface{}{"cpu": "1", "memory": "50Mi"}}
	for _, test := range []struct{ cpu, memory, want float64 }{
		{0, 1, 50}, {1, 0, 100}, {0.01, 0.99, 50.5},
	} {
		got, err := WeightedUsagePercentage(node, metric, test.cpu, test.memory)
		if err != nil || math.Abs(got-test.want) > 1e-9 {
			t.Errorf("weighted usage with (%v, %v) = (%v, %v), want %v", test.cpu, test.memory, got, err, test.want)
		}
	}
}

func TestPodMemoryReservationBytesUsesContainerBudgetsAndInitPeak(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("100Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("200Mi")},
				}},
				{Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				}},
				{Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				}},
			},
			InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}}},
			Overhead: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")},
		},
	}

	wantQuantity := resource.MustParse("436Mi")
	want := wantQuantity.Value()
	if got := PodMemoryReservationBytes(pod); got != want {
		t.Fatalf("PodMemoryReservationBytes() = %d, want %d", got, want)
	}
}

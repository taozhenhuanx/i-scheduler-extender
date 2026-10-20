package nodeusage

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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

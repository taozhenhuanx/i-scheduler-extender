package nodeusage

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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
			},
			InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}}},
			Overhead: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")},
		},
	}

	wantQuantity := resource.MustParse("528Mi")
	want := wantQuantity.Value()
	if got := PodMemoryReservationBytes(pod); got != want {
		t.Fatalf("PodMemoryReservationBytes() = %d, want %d", got, want)
	}
}

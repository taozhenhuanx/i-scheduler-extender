package v1alpha1

import (
	"encoding/json"
	"testing"
)

func weightPointer(value int32) *int32 { return &value }

func TestResolveWeightsPreservesZeroAndOnePercent(t *testing.T) {
	for _, test := range []struct {
		name        string
		spec        SchedulerPolicySpec
		cpu, memory float64
	}{
		{name: "defaults", cpu: 0.1, memory: 0.9},
		{name: "memory only", spec: SchedulerPolicySpec{CPUWeight: weightPointer(0), MemoryWeight: weightPointer(100)}, cpu: 0, memory: 1},
		{name: "cpu only", spec: SchedulerPolicySpec{CPUWeight: weightPointer(100), MemoryWeight: weightPointer(0)}, cpu: 1, memory: 0},
		{name: "one percent", spec: SchedulerPolicySpec{CPUWeight: weightPointer(1), MemoryWeight: weightPointer(99)}, cpu: 0.01, memory: 0.99},
		{name: "cpu default", spec: SchedulerPolicySpec{MemoryWeight: weightPointer(90)}, cpu: 0.1, memory: 0.9},
	} {
		t.Run(test.name, func(t *testing.T) {
			cpu, memory, err := test.spec.ResolveWeights()
			if err != nil || cpu != test.cpu || memory != test.memory {
				t.Fatalf("ResolveWeights() = (%v, %v, %v), want (%v, %v, nil)", cpu, memory, err, test.cpu, test.memory)
			}
		})
	}
}

func TestResolveWeightsRejectsInvalidPercentages(t *testing.T) {
	for _, weights := range [][2]int32{{0, 0}, {1, 90}, {-1, 101}, {101, -1}, {100, 100}} {
		spec := SchedulerPolicySpec{CPUWeight: weightPointer(weights[0]), MemoryWeight: weightPointer(weights[1])}
		if _, _, err := spec.ResolveWeights(); err == nil {
			t.Errorf("ResolveWeights(%v) accepted invalid percentages", weights)
		}
	}
	if _, _, err := (SchedulerPolicySpec{CPUWeight: weightPointer(0)}).ResolveWeights(); err == nil {
		t.Error("explicit zero CPU plus default 90% memory must fail the total check")
	}
}

func TestWeightSerializationAndDeepCopyPreserveExplicitZero(t *testing.T) {
	spec := SchedulerPolicySpec{CPUWeight: weightPointer(0), MemoryWeight: weightPointer(100)}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SchedulerPolicySpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CPUWeight == nil || *decoded.CPUWeight != 0 || decoded.MemoryWeight == nil || *decoded.MemoryWeight != 100 {
		t.Fatalf("zero or hundred-percent weight changed on JSON round trip: %s", data)
	}
	copied := spec.DeepCopy()
	*copied.CPUWeight, *copied.MemoryWeight = 10, 90
	if *spec.CPUWeight != 0 || *spec.MemoryWeight != 100 {
		t.Fatal("weight pointers were shared by DeepCopy")
	}
}

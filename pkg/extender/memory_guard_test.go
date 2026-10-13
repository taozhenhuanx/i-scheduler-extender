package extender

import "testing"

func TestDecideMemoryGuard(t *testing.T) {
	tests := []struct {
		name             string
		currentPercent   float64
		projectedPercent float64
		isBlocked        bool
		want             memoryGuardDecision
	}{
		{name: "allow exactly at block threshold", currentPercent: 70, projectedPercent: 80, want: memoryGuardAllow},
		{name: "block above threshold", currentPercent: 70, projectedPercent: 80.01, want: memoryGuardBlock},
		{name: "recover at recovery threshold", currentPercent: 75, projectedPercent: 80, isBlocked: true, want: memoryGuardRecover},
		{name: "stay blocked above recovery threshold", currentPercent: 75.01, projectedPercent: 78, isBlocked: true, want: memoryGuardKeepBlocked},
		{name: "stay blocked if incoming pod exceeds threshold", currentPercent: 70, projectedPercent: 80.01, isBlocked: true, want: memoryGuardKeepBlocked},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideMemoryGuard(test.currentPercent, test.projectedPercent, test.isBlocked)
			if got != test.want {
				t.Fatalf("decideMemoryGuard() = %v, want %v", got, test.want)
			}
		})
	}
}

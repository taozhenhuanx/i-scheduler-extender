package extender

import "math"

const maxExtenderScore int64 = 10

func scoreForUsage(usagePercentage float64) int64 {
	if math.IsNaN(usagePercentage) || math.IsInf(usagePercentage, 0) {
		return 0
	}
	if usagePercentage < 0 {
		usagePercentage = 0
	}
	if usagePercentage > 100 {
		usagePercentage = 100
	}
	remainingCapacity := 100 - usagePercentage
	return int64(math.Round(remainingCapacity * float64(maxExtenderScore) / 100))
}

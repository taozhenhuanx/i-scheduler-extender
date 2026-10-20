package extender

import "math"

const maxExtenderScore int64 = 10

// scoreForUsage 将实测加权负载反向映射到 0～10 分：round(10 * (1 - usage/100))。
// 负载越低分数越高；非法值返回 0，越界使用率先限制到 0～100%。
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

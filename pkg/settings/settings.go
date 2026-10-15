// Package settings loads the operator process settings from a YAML file.
package settings

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Settings contains manager and HTTP server options that are independent of the
// cluster policy managed through SchedulerPolicy resources.
// Settings 保存进程级选项；集群调度策略应通过 SchedulerPolicy 资源配置。
type Settings struct {
	MetricsBindAddress     string `json:"metricsBindAddress"`
	HealthProbeBindAddress string `json:"healthProbeBindAddress"`
	ExtenderBindAddress    string `json:"extenderBindAddress"`
	LeaderElection         bool   `json:"leaderElection"`
}

// Default 返回监听地址和 leader election 的安全默认值。
func Default() Settings {
	return Settings{
		MetricsBindAddress:     "0",
		HealthProbeBindAddress: ":8082",
		ExtenderBindAddress:    ":8080",
		LeaderElection:         true,
	}
}

// Load 读取 YAML 配置，缺省字段沿用默认值，并拒绝未知字段以便尽早发现配置拼写错误。
func Load(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("read settings file %q: %w", path, err)
	}

	settings := Default()
	if err := yaml.UnmarshalStrict(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("parse settings file %q: %w", path, err)
	}
	if settings.HealthProbeBindAddress == "" || settings.ExtenderBindAddress == "" {
		return Settings{}, fmt.Errorf("healthProbeBindAddress and extenderBindAddress must not be empty")
	}
	return settings, nil
}

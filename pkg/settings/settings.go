// Package settings loads the operator process settings from a YAML file.
package settings

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Settings contains manager and HTTP server options that are independent of the
// cluster policy managed through SchedulerPolicy resources.
type Settings struct {
	MetricsBindAddress     string `json:"metricsBindAddress"`
	HealthProbeBindAddress string `json:"healthProbeBindAddress"`
	ExtenderBindAddress    string `json:"extenderBindAddress"`
	LeaderElection         bool   `json:"leaderElection"`
}

func Default() Settings {
	return Settings{
		MetricsBindAddress:     "0",
		HealthProbeBindAddress: ":8082",
		ExtenderBindAddress:    ":8080",
		LeaderElection:         true,
	}
}

// Load reads settings from path, retaining defaults for omitted values and rejecting unknown keys.
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

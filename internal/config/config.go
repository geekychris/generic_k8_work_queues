package config

import (
	"os"
	"time"

	"github.com/chris/kqueue/pkg/api"
	"gopkg.in/yaml.v3"
)

// Config holds the complete application configuration
type Config struct {
	Server  ServerConfig      `yaml:"server"`
	NATS    NATSConfig        `yaml:"nats"`
	Queues  []api.QueueConfig `yaml:"queues"`
	Metrics MetricsConfig     `yaml:"metrics"`
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Port      int  `yaml:"port"`
	UIEnabled bool `yaml:"ui_enabled"`
}

// NATSConfig holds NATS connection settings
type NATSConfig struct {
	URL          string `yaml:"url"`
	StreamPrefix string `yaml:"stream_prefix"`
}

// MetricsConfig holds Prometheus metrics settings
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// Load reads and parses the configuration file at the given path
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Server:  ServerConfig{Port: 8080, UIEnabled: true},
		NATS:    NATSConfig{URL: "nats://nats:4222", StreamPrefix: "kqueue"},
		Metrics: MetricsConfig{Enabled: true, Port: 9090},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Apply defaults to queues
	for i := range cfg.Queues {
		q := &cfg.Queues[i]
		if q.Subject == "" {
			q.Subject = "kqueue." + q.Name
		}
		if q.MaxRetries == 0 {
			q.MaxRetries = 3
		}
		if q.ProcessingTimeout == 0 {
			q.ProcessingTimeout = 5 * time.Minute
		}
		if q.Replicas.Min == 0 {
			q.Replicas.Min = 1
		}
		if q.Replicas.Max == 0 {
			q.Replicas.Max = 10
		}
		if q.ScaleStrategy.Type == "" {
			q.ScaleStrategy.Type = "threshold"
		}
		if q.ScaleStrategy.ScaleUpThreshold == 0 {
			q.ScaleStrategy.ScaleUpThreshold = 10
		}
		if q.ScaleStrategy.ScaleDownThreshold == 0 {
			q.ScaleStrategy.ScaleDownThreshold = 2
		}
		if q.ScaleStrategy.TargetPerWorker == 0 {
			q.ScaleStrategy.TargetPerWorker = 5
		}
		if q.ScaleStrategy.CooldownSeconds == 0 {
			q.ScaleStrategy.CooldownSeconds = 30
		}
		if q.ScaleStrategy.ScaleUpStep == 0 {
			q.ScaleStrategy.ScaleUpStep = 2
		}
		if q.ScaleStrategy.ScaleDownStep == 0 {
			q.ScaleStrategy.ScaleDownStep = 1
		}
		// Cost-aware scaling defaults
		// StartupCostSeconds defaults to 0 (no startup cost)
		// ScaleDownDelaySeconds defaults to 0 (no extra delay)
		// ScaleDownRate defaults to 0.5 if unset (0 means use default)
		if q.ScaleStrategy.ScaleDownRate == 0 {
			q.ScaleStrategy.ScaleDownRate = 0.5
		}
		if q.Resources.CPURequest == "" {
			q.Resources.CPURequest = "100m"
		}
		if q.Resources.CPULimit == "" {
			q.Resources.CPULimit = "500m"
		}
		if q.Resources.MemoryRequest == "" {
			q.Resources.MemoryRequest = "128Mi"
		}
		if q.Resources.MemoryLimit == "" {
			q.Resources.MemoryLimit = "512Mi"
		}
	}

	return cfg, nil
}

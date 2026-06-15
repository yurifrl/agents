// Package config loads controller configuration from a file and/or environment.
//
// Every setting is addressable two ways: a key in the config file and an
// environment variable. Viper binds them together so "everything that is an env
// var is also a file key" — env vars override file values, file values override
// built-in defaults.
package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// EnvPrefix namespaces every environment variable, e.g. AGENTS_NAMESPACE.
const EnvPrefix = "AGENTS"

// Config is the fully-resolved controller configuration.
type Config struct {
	// Namespace is where agent pods/services are created.
	Namespace string `mapstructure:"namespace"`

	// AgentImage is the image that runs agent-entrypoint inside task pods.
	AgentImage string `mapstructure:"agent_image"`

	// SecretName is the Kubernetes Secret (the single "agents" item) mounted
	// wholesale into every agent pod.
	SecretName string `mapstructure:"secret_name"`

	// ImagePullSecret, when set, is added to each agent pod's imagePullSecrets so
	// it can pull a private image (e.g. the ghcr pull secret).
	ImagePullSecret string `mapstructure:"image_pull_secret"`

	// MaxConcurrent caps simultaneously-running ephemeral pods (0 = unlimited).
	MaxConcurrent int `mapstructure:"max_concurrent"`

	// RestartBudget is how many times a crashed pod is recreated before failing.
	RestartBudget int32 `mapstructure:"restart_budget"`

	// DefaultDeadlineSeconds is the activeDeadlineSeconds applied to ephemeral
	// pods when the AgentTask does not specify one.
	DefaultDeadlineSeconds int64 `mapstructure:"default_deadline_seconds"`

	// Manager holds controller-manager runtime knobs.
	Manager ManagerConfig `mapstructure:"manager"`

	// Log holds logging configuration.
	Log LogConfig `mapstructure:"log"`
}

// ManagerConfig holds controller-runtime manager settings.
type ManagerConfig struct {
	MetricsAddr    string `mapstructure:"metrics_addr"`
	HealthAddr     string `mapstructure:"health_addr"`
	LeaderElection bool   `mapstructure:"leader_election"`
	LeaderElectID  string `mapstructure:"leader_election_id"`
}

// LogConfig controls the charm logger.
type LogConfig struct {
	// Level is one of debug, info, warn, error.
	Level string `mapstructure:"level"`
	// Format is "text" or "json".
	Format string `mapstructure:"format"`
}

// Load reads configuration from the optional file at path (may be empty) and
// overlays environment variables (AGENTS_*). An empty path means env+defaults.
func Load(path string) (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	setDefaults(v)
	bindEnv(v)

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("namespace", "agents")
	v.SetDefault("agent_image", "ghcr.io/yurifrl/agents:latest")
	v.SetDefault("secret_name", "agents")
	v.SetDefault("image_pull_secret", "")
	v.SetDefault("max_concurrent", 0)
	v.SetDefault("restart_budget", 3)
	v.SetDefault("default_deadline_seconds", 3600)
	v.SetDefault("manager.metrics_addr", ":8080")
	v.SetDefault("manager.health_addr", ":8081")
	v.SetDefault("manager.leader_election", false)
	v.SetDefault("manager.leader_election_id", "agents-controller.agents.yurifrl.dev")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "text")
}

// bindEnv makes nested keys reachable via env even without a config file,
// since AutomaticEnv only resolves keys viper already knows about.
func bindEnv(v *viper.Viper) {
	keys := []string{
		"namespace",
		"agent_image",
		"secret_name",
		"image_pull_secret",
		"max_concurrent",
		"restart_budget",
		"default_deadline_seconds",
		"manager.metrics_addr",
		"manager.health_addr",
		"manager.leader_election",
		"manager.leader_election_id",
		"log.level",
		"log.format",
	}
	for _, k := range keys {
		_ = v.BindEnv(k)
	}
}

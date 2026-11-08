package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yurifrl/agents/internal/protocol"
)

// Config mirrors the original TS server config (packages/server, deleted in
// the Go unification): env-driven, same variables.
type Config struct {
	Port     int
	StateDir string
	// Surface is "auto" | "process" | "herdr" | "k8s".
	Surface  string
	OmpBin   string
	Model    string
	MaxTimeS int
	URL      string
	// K8s configures the k8s surface; required when Surface is "k8s".
	K8s *K8sSurfaceConfig
}

// K8sSurfaceConfig carries the in-cluster worker pod settings.
type K8sSurfaceConfig struct {
	Namespace  string
	Image      string
	SecretName string
}

// DefaultServerBin is the binary the extension spawns once the TS server is
// gone: `agents server` on PATH (overridable via REMOTE_AGENTS_SERVER_BIN).
const DefaultServerBin = "agents"

// LoadConfig reads the REMOTE_AGENTS_* environment (mirrors loadConfig).
func LoadConfig() (Config, error) {
	port := protocol.DEFAULT_PORT
	if raw := os.Getenv(protocol.EnvPort); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 65535 {
			return Config{}, fmt.Errorf("invalid %s: %s", protocol.EnvPort, raw)
		}
		port = parsed
	}

	stateDir := os.Getenv(protocol.EnvStateDir)
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve home: %w", err)
		}
		stateDir = filepath.Join(home, ".remote-agents")
	}
	stateDir = expandHome(stateDir)

	surfaceSetting := os.Getenv(protocol.EnvSurface)
	if surfaceSetting == "" {
		surfaceSetting = "auto"
	}
	switch surfaceSetting {
	case "auto", string(protocol.SurfaceProcess), string(protocol.SurfaceHerdr), string(protocol.SurfaceK8s):
	default:
		return Config{}, fmt.Errorf("invalid %s: %s (expected auto|process|herdr|k8s)", protocol.EnvSurface, surfaceSetting)
	}

	ompBin := strings.TrimSpace(os.Getenv(protocol.EnvOmpBin))
	if ompBin == "" {
		ompBin = "omp"
	}
	model := strings.TrimSpace(os.Getenv(protocol.EnvModel))

	maxTimeS := 1800
	if raw := os.Getenv(protocol.EnvMaxTimeS); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("invalid %s: %s", protocol.EnvMaxTimeS, raw)
		}
		maxTimeS = parsed
	}

	url := strings.TrimSpace(os.Getenv(protocol.EnvURL))
	if url == "" {
		url = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	var k8sConfig *K8sSurfaceConfig
	if surfaceSetting == string(protocol.SurfaceK8s) {
		namespace := envOr(protocol.EnvK8sNamespace, "agents")
		image := strings.TrimSpace(os.Getenv(protocol.EnvK8sImage))
		if image == "" {
			image = "ghcr.io/yurifrl/agents:latest"
		}
		secret := strings.TrimSpace(os.Getenv(protocol.EnvK8sSecret))
		k8sConfig = &K8sSurfaceConfig{Namespace: namespace, Image: image, SecretName: secret}
	}

	return Config{
		Port:     port,
		StateDir: stateDir,
		Surface:  surfaceSetting,
		OmpBin:   ompBin,
		Model:    model,
		MaxTimeS: maxTimeS,
		URL:      url,
		K8s:      k8sConfig,
	}, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func expandHome(p string) string {
	if p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home
		}
		return p
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// ResolveSurfaceKind resolves `auto` to a concrete surface via a which-style
// PATH check for herdr.
func ResolveSurfaceKind(config Config) protocol.SurfaceKind {
	if config.Surface != "auto" {
		return protocol.SurfaceKind(config.Surface)
	}
	if _, err := exec.LookPath("herdr"); err == nil {
		return protocol.SurfaceHerdr
	}
	return protocol.SurfaceProcess
}

// ServerBinEnv overrides the spawned server binary (extension side).
const ServerBinEnv = "REMOTE_AGENTS_SERVER_BIN"

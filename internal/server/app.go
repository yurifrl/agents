package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/yurifrl/agents/internal/kube"
	"github.com/yurifrl/agents/internal/protocol"
	"github.com/yurifrl/agents/internal/surface"
)

// App composes store + supervisor + HTTP server (mirror of createApp).
type App struct {
	Config     Config
	Store      *TaskStore
	Supervisor *Supervisor
	Server     *http.Server
	Surface    surface.Surface

	listener net.Listener
}

// AppOptions injects test doubles; zero fields build production components.
type AppOptions struct {
	Surface     surface.Surface
	Store       *TaskStore
	Supervisor  *Supervisor
	SupervisorO SupervisorOpts
	Heartbeat   time.Duration
}

// CreateApp assembles the server app from config and binds its listener.
func CreateApp(config Config, opts AppOptions) (*App, error) {
	store := opts.Store
	if store == nil {
		var err error
		store, err = NewTaskStore(config.StateDir)
		if err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}
	surf := opts.Surface
	if surf == nil {
		surf = buildSurface(ResolveSurfaceKind(config), config)
	}
	supervisor := opts.Supervisor
	if supervisor == nil {
		supervisor = NewSupervisor(store, surf, SupervisorOpts{
			MaxTime: time.Duration(config.MaxTimeS) * time.Second,
			OmpBin:  config.OmpBin,
			Model:   config.Model,
		})
	}
	handler := NewHTTPHandler(config, store, supervisor, opts.Heartbeat)
	addr := fmt.Sprintf("127.0.0.1:%d", config.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return &App{
		Config:     config,
		Store:      store,
		Supervisor: supervisor,
		Surface:    surf,
		listener:   listener,
		Server: &http.Server{
			Handler: handler,
			// SSE streams are kept alive by heartbeats; no idle timeouts.
		},
	}, nil
}

// buildSurface constructs the production surface for a kind.
func buildSurface(kind protocol.SurfaceKind, config Config) surface.Surface {
	if kind == protocol.SurfaceHerdr {
		s := surface.NewHerdrSurface()
		s.BridgePath = filepath.Join(config.StateDir, "bridge.mjs")
		return s
	}
	if kind == protocol.SurfaceK8s {
		cfg := config.K8s
		restCfg, err := kube.RestConfig()
		if err != nil {
			panic(fmt.Sprintf("k8s surface selected but no cluster config: %v", err))
		}
		scheme, err := kube.NewScheme()
		if err != nil {
			panic(fmt.Sprintf("k8s scheme: %v", err))
		}
		c, err := kube.NewClient(restCfg, scheme)
		if err != nil {
			panic(fmt.Sprintf("k8s client: %v", err))
		}
		return surface.NewK8sSurface(c, cfg.Namespace, cfg.Image, cfg.SecretName)
	}
	return surface.NewProcessSurface()
}

// Serve runs the HTTP server on the pre-bound listener until Shutdown.
func (a *App) Serve() error {
	return a.Server.Serve(a.listener)
}

// Addr is the bound address (port resolution for port=0).
func (a *App) Addr() string { return a.listener.Addr().String() }

// Port is the bound port.
func (a *App) Port() int {
	if tcp, ok := a.listener.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return a.Config.Port
}

// Shutdown gracefully stops the HTTP server (supervisor shutdown is the
// caller's sequence, mirroring the TS main.ts ordering).
func (a *App) Shutdown(ctx context.Context) error {
	return a.Server.Shutdown(ctx)
}

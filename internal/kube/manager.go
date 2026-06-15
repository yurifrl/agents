// Package kube builds the controller-runtime manager and scheme.
package kube

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/yurifrl/agents/api/v1alpha1"
	"github.com/yurifrl/agents/internal/config"
)

// NewScheme builds a scheme with the core k8s types plus the agents CRDs.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// NewManager constructs the controller-runtime manager from config. The logger
// is also installed as the controller-runtime global logger.
func NewManager(cfg *config.Config, logger logr.Logger) (manager.Manager, error) {
	ctrl.SetLogger(logger)

	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.Manager.MetricsAddr,
		},
		HealthProbeBindAddress: cfg.Manager.HealthAddr,
		LeaderElection:         cfg.Manager.LeaderElection,
		LeaderElectionID:       cfg.Manager.LeaderElectID,
	})
	if err != nil {
		return nil, err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, err
	}

	return mgr, nil
}

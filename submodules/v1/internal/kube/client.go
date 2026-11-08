package kube

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RestConfig resolves the cluster connection (in-cluster or kubeconfig).
func RestConfig() (*rest.Config, error) {
	return ctrl.GetConfig()
}

// NewClient builds a direct controller-runtime client for components that are
// not controller managers (e.g. the gateway).
func NewClient(cfg *rest.Config, scheme *runtime.Scheme) (client.Client, error) {
	return client.New(cfg, client.Options{Scheme: scheme})
}

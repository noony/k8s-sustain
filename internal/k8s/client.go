// Package k8s holds small helpers shared by every binary that needs a
// controller-runtime client without taking on the full manager machinery.
package k8s

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// New builds a controller-runtime client against the in-cluster (or kubeconfig)
// REST config, using the supplied scheme. Used by the webhook and the
// dashboard binaries, which both run outside the controller-manager and
// therefore can't reuse manager.GetClient().
func New(scheme *runtime.Scheme) (client.Client, error) {
	restCfg := ctrl.GetConfigOrDie()
	return client.New(restCfg, client.Options{Scheme: scheme})
}

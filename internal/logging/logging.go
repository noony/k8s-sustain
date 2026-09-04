// Package logging provides a shared logger setup for all subcommands.
package logging

import (
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Setup installs a production zap logger at the given level as the
// controller-runtime global logger and returns a named child. Call once.
func Setup(level, name string) logr.Logger {
	cfg := zap.NewProductionConfig()

	atomicLevel, err := zap.ParseAtomicLevel(level)
	if err != nil {
		atomicLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	cfg.Level = atomicLevel

	zapLogger, err := cfg.Build()
	if err != nil {
		return logr.Discard()
	}

	logger := zapr.NewLogger(zapLogger)
	ctrl.SetLogger(logger)

	return ctrl.Log.WithName(name)
}

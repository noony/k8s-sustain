// Package logging provides a shared logger setup for all subcommands.
package logging

import (
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Setup configures a production zap logger (JSON, sampling, caller info,
// stacktraces on error) at the given level, sets it as the controller-runtime
// global logger, and returns a named child logger.
// Safe to call once per process.
func Setup(level, name string) logr.Logger {
	cfg := zap.NewProductionConfig()

	atomicLevel, err := zap.ParseAtomicLevel(level)
	if err != nil {
		atomicLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	cfg.Level = atomicLevel

	zapLogger, err := cfg.Build()
	if err != nil {
		// Fallback to nop — should never happen with a valid production config.
		return logr.Discard()
	}

	logger := zapr.NewLogger(zapLogger)
	ctrl.SetLogger(logger)

	return ctrl.Log.WithName(name)
}

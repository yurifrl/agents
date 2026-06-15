// Package logging adapts charmbracelet/log to the logr.Logger interface that
// controller-runtime expects, so the whole stack logs through one charm logger.
package logging

import (
	"os"

	charm "github.com/charmbracelet/log"
	"github.com/go-logr/logr"

	"github.com/yurifrl/agents/internal/config"
)

// New builds a logr.Logger backed by a charm logger configured from cfg.
func New(cfg config.LogConfig) logr.Logger {
	c := charm.NewWithOptions(os.Stderr, charm.Options{
		ReportTimestamp: true,
	})
	c.SetLevel(parseLevel(cfg.Level))
	if cfg.Format == "json" {
		c.SetFormatter(charm.JSONFormatter)
	}
	return logr.New(&sink{logger: c})
}

func parseLevel(level string) charm.Level {
	switch level {
	case "debug":
		return charm.DebugLevel
	case "warn":
		return charm.WarnLevel
	case "error":
		return charm.ErrorLevel
	default:
		return charm.InfoLevel
	}
}

// sink implements logr.LogSink over a charm logger. logr V-levels map to charm
// levels: V(0)=info, V(1+)=debug.
type sink struct {
	logger *charm.Logger
	name   string
	values []any
}

func (s *sink) Init(logr.RuntimeInfo) {}

func (s *sink) Enabled(level int) bool {
	if level > 0 {
		return s.logger.GetLevel() <= charm.DebugLevel
	}
	return s.logger.GetLevel() <= charm.InfoLevel
}

func (s *sink) Info(level int, msg string, kv ...any) {
	args := s.merge(kv)
	if level > 0 {
		s.logger.Debug(s.prefix(msg), args...)
		return
	}
	s.logger.Info(s.prefix(msg), args...)
}

func (s *sink) Error(err error, msg string, kv ...any) {
	args := append([]any{"error", err}, s.merge(kv)...)
	s.logger.Error(s.prefix(msg), args...)
}

func (s *sink) WithValues(kv ...any) logr.LogSink {
	return &sink{
		logger: s.logger,
		name:   s.name,
		values: append(append([]any{}, s.values...), kv...),
	}
}

func (s *sink) WithName(name string) logr.LogSink {
	next := name
	if s.name != "" {
		next = s.name + "." + name
	}
	return &sink{logger: s.logger, name: next, values: s.values}
}

func (s *sink) prefix(msg string) string {
	if s.name == "" {
		return msg
	}
	return s.name + ": " + msg
}

func (s *sink) merge(kv []any) []any {
	if len(s.values) == 0 {
		return kv
	}
	return append(append([]any{}, s.values...), kv...)
}

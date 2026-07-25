// Package logging configures slog from the config file (design §11.5).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aghman/meshbbs/internal/config"
)

// New builds a logger from configuration. It returns the logger and, when
// logging to a file, a closer the caller must invoke on shutdown.
func New(cfg config.Log) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stderr
	var closer io.Closer

	if cfg.File != "" {
		if dir := filepath.Dir(cfg.File); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, nil, fmt.Errorf("create log directory: %w", err)
			}
		}
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", cfg.File, err)
		}
		w, closer = f, f
	}

	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		// Validate() rejects anything else, so reaching here means a caller
		// built a Log struct by hand.
		return nil, nil, fmt.Errorf("unknown log level %q", cfg.Level)
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h), closer, nil
}

// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

// Package logging configures structured slog loggers compatible with both
// Google Cloud Logging (JSON severity/message fields) and local text output.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds a structured *slog.Logger for the given format ("json" or "text")
// and minimum level ("debug", "info", "warn", or "error"). Empty values default
// to "json" and "info"; a nil writer defaults to os.Stderr.
func New(format, level string, w io.Writer) (*slog.Logger, error) {
	if w == nil {
		w = os.Stderr
	}
	parsedLevel, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	normFormat := strings.ToLower(strings.TrimSpace(format))
	if normFormat == "" {
		normFormat = "json"
	}

	switch normFormat {
	case "json":
		h := slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:       parsedLevel,
			ReplaceAttr: cloudLoggingReplaceAttr,
		})
		return slog.New(h), nil
	case "text":
		h := slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: parsedLevel,
		})
		return slog.New(h), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q (want json or text)", format)
	}
}

func parseLevel(raw string) (slog.Level, error) {
	norm := strings.ToLower(strings.TrimSpace(raw))
	if norm == "" {
		return slog.LevelInfo, nil
	}
	switch norm {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q (want debug, info, warn, or error)", raw)
	}
}

func cloudLoggingReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		lvl, ok := a.Value.Any().(slog.Level)
		if !ok {
			return slog.Attr{Key: "severity", Value: a.Value}
		}
		if lvl == slog.LevelWarn {
			return slog.String("severity", "WARNING")
		}
		return slog.String("severity", lvl.String())
	case slog.MessageKey:
		return slog.Attr{Key: "message", Value: a.Value}
	default:
		return a
	}
}

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// SetupLogging installs a process-wide slog logger. format is "text" or
// "json"; JSON output uses the "severity" and "message" keys that Cloud
// Logging understands. level is "debug", "info", "warn" or "error".
func SetupLogging(w io.Writer, format, level string) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(level))); err != nil {
		return fmt.Errorf("invalid log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		opts.ReplaceAttr = cloudLoggingKeys
		h = slog.NewJSONHandler(w, opts)
	default:
		return fmt.Errorf("invalid log format %q: want text or json", format)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

// cloudLoggingKeys renames slog's level/msg keys to Cloud Logging's
// severity/message and maps WARN to WARNING.
func cloudLoggingKeys(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.LevelKey:
		a.Key = "severity"
		if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == slog.LevelWarn {
			a.Value = slog.StringValue("WARNING")
		}
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// ServeHTTP serves the registry at /metrics and a liveness probe at /healthz
// on addr until ctx is cancelled. An empty addr disables the server.
func ServeHTTP(ctx context.Context, addr string, reg *Registry) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics listener on %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("serving metrics", "addr", lis.Addr().String())
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

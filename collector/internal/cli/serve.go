package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"vpsm-collector/internal/api"
	"vpsm-collector/internal/config"
	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/storage"
)

// shutdownGrace is how long in-flight requests get to finish on shutdown.
// Reports are small and fast; anything still running after this is stuck.
const shutdownGrace = 10 * time.Second

// Serve runs the collector API until the context is cancelled.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	handler := api.Handler(api.Options{
		Store:  store,
		Source: store,
		Logger: log,
		Token:  cfg.Token,
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// A collector is reachable by whatever can route to it, so no
		// request gets to occupy a connection indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	// Bind before announcing, so a port clash fails immediately with a
	// clear error instead of after a "listening" line.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}

	log.Info("collector listening",
		slog.String("addr", listener.Addr().String()),
		slog.String("db", cfg.DBPath),
		slog.String("ingest", protocol.ReportsPath),
		slog.String("retention", retentionLabel(cfg.Retention)))
	if cfg.Token == "" {
		log.Warn("serving without authentication: anyone who can reach this port can submit or read metrics")
	}

	pruneCtx, stopPruning := context.WithCancel(ctx)
	defer stopPruning()
	go prune(pruneCtx, store, cfg, log)

	// ListenAndServe-style errors arrive asynchronously; shutdown is driven
	// by ctx. Whichever happens first wins.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)

	case <-ctx.Done():
		log.Info("shutting down", slog.Duration("grace", shutdownGrace))
		stopPruning()

		// Deliberately not derived from ctx, which is already cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// prune sweeps expired samples on an interval, keeping the database bounded
// by time rather than letting it grow forever.
func prune(ctx context.Context, store *storage.Store, cfg config.Config, log *slog.Logger) {
	if cfg.Retention == 0 {
		log.Info("retention disabled, samples are kept indefinitely")
		return
	}

	run := func() {
		cutoff := time.Now().Add(-cfg.Retention)
		removed, err := store.Prune(ctx, cutoff)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			log.Error("prune", slog.Any("err", err))
			return
		}
		if removed > 0 {
			log.Info("pruned expired samples",
				slog.Int64("removed", removed),
				slog.String("older_than", cfg.Retention.String()))
		}
	}

	// Sweep once at startup so a collector restarted after a long gap does
	// not carry stale data until the first tick.
	run()

	ticker := time.NewTicker(cfg.PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func retentionLabel(d time.Duration) string {
	if d == 0 {
		return "forever"
	}
	return d.String()
}

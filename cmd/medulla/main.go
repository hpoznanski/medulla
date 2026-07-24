package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/config"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
	"github.com/hpoznanski/medulla/internal/web"
)

func main() {
	configPath := flag.String("config", "/etc/medulla/config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	sessions, ephemeral, err := auth.NewCodec(cfg.Session.Secret.Value(), cfg.Session.TTL)
	if err != nil {
		return err
	}
	if ephemeral {
		logger.Warn("session secret not set: using ephemeral key, sessions reset on restart and multi-replica deployments will not work")
	}

	registry, err := es.NewRegistry(cfg.Clusters)
	if err != nil {
		return err
	}

	server, err := web.NewServer(
		cfg,
		auth.New(cfg, logger),
		sessions,
		rbac.New(cfg.Roles),
		registry,
		logger,
	)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "clusters", len(cfg.Clusters), "env", cfg.Env)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger.Info("shutting down")
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

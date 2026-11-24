package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	store, err := openTaskStore(cfg.DataPath)
	if err != nil {
		logger.Error("task store failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	blobs, err := newR2Store(ctx, cfg)
	if err != nil {
		logger.Error("R2 client failed", "error", err)
		os.Exit(1)
	}
	service := newProxyService(cfg, store, blobs, logger)
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: service.routes(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 70 * time.Second,
		WriteTimeout: 70 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
	go service.run(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("AZ proxy listening", "address", cfg.ListenAddr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/clients/telegram"
	"github.com/foae/marstek-energy-trading/handler"
	"github.com/foae/marstek-energy-trading/internal/config"
	"github.com/foae/marstek-energy-trading/service"
)

func main() {
	// Load .env file (optional, falls back to env vars)
	_ = godotenv.Load()

	// Parse configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Setup structured logging
	logLevel := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	slog.Info("starting energy trader",
		"service", cfg.ServiceName,
		"listen_addr", cfg.HTTPListenAddr,
		"nordpool_area", cfg.NordPoolArea,
		"min_spread", cfg.MinPriceSpread,
		"efficiency", cfg.BatteryEfficiency,
	)

	// Initialize clients with configured timezone
	nordpoolClient := nordpool.NewWithLocation(cfg.NordPoolArea, cfg.NordPoolCurrency, cfg.Location())
	marstekClient := marstek.New(cfg.BatteryUDPAddr)
	defer marstekClient.Close() // Ensure UDP connection is closed on shutdown
	telegramClient := telegram.New(cfg.TelegramBotToken, cfg.TelegramChatID)

	if telegramClient.Enabled() {
		slog.Info("telegram notifications enabled")
	}

	// Initialize recorder with configured timezone
	recorder := service.NewRecorder(cfg.DataDir, cfg.BatteryEfficiency, cfg.Location())

	// Initialize trading service
	tradingSvc := service.New(cfg, nordpoolClient, marstekClient, telegramClient, recorder)

	// Setup HTTP handler
	h := handler.New(tradingSvc)
	router := h.NewRouter()

	server := &http.Server{
		Addr:         cfg.HTTPListenAddr,
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// WaitGroup for graceful shutdown
	var wg sync.WaitGroup

	// Start HTTP server
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("HTTP server listening", "addr", cfg.HTTPListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Create context for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start trading loop in background
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tradingSvc.Start(ctx); err != nil && err != context.Canceled {
			slog.Error("trading service error", "error", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	slog.Info("shutting down...")

	// Shutdown HTTP server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	slog.Info("shutdown complete")
}

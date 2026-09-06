package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/foae/marstek-energy-trading/clients/esphome"
	"github.com/foae/marstek-energy-trading/clients/homewizard"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/clients/telegram"
	"github.com/foae/marstek-energy-trading/handler"
	"github.com/foae/marstek-energy-trading/internal/config"
	"github.com/foae/marstek-energy-trading/service"
)

func main() {
	var envFileInvalid bool
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Do not include the parser error: it can quote a credential-bearing line.
		slog.Error("could not load .env; automatic operation will not start")
		envFileInvalid = true
	}

	// Parse configuration
	cfg, err := config.Load()
	if err != nil {
		if envFileInvalid {
			emergencyURL := os.Getenv("ESPHOME_URL")
			if emergencyURL == "" {
				emergencyURL = emergencyESPHomeURL(".env")
			}
			reconcileBatteryAfterEnvFailure(emergencyURL)
		}
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

	slog.Info(
		"starting energy trader",
		"service", cfg.ServiceName,
		"listen_addr", cfg.HTTPListenAddr,
		"nordpool_area", cfg.NordPoolArea,
		"min_spread", cfg.MinPriceSpread,
		"efficiency", cfg.BatteryEfficiency,
		"energy_tax_eur_kwh", cfg.EnergyTaxEURPerKWh,
		"vat_rate", cfg.VATRate,
		"supplier_fee_eur_kwh", cfg.SupplierFeeEURPerKWh,
	)

	// Initialize clients with configured timezone and all-in pricing.
	nordpoolClient := nordpool.NewWithLocation(
		cfg.NordPoolArea,
		cfg.NordPoolCurrency,
		cfg.Location(),
		nordpool.AllInPricing{
			EnergyTaxEURPerKWh:   cfg.EnergyTaxEURPerKWh,
			VATRate:              cfg.VATRate,
			SupplierFeeEURPerKWh: cfg.SupplierFeeEURPerKWh,
		},
	)
	minSOC := cfg.MinSOCPercent()
	esphomeClient := esphome.New(cfg.ESPHomeURL, minSOC)
	defer esphomeClient.Close()
	esphomeClient.SetRestartButton(cfg.ESPHomeRestartButton)
	slog.Info("using ESPHome battery backend", "host", endpointHost(cfg.ESPHomeURL), "min_soc", minSOC, "bridge_restart", esphomeClient.RestartAvailable())
	if envFileInvalid {
		reconcileBatteryAfterEnvFailure(cfg.ESPHomeURL)
		os.Exit(1)
	}
	p1URL := cfg.HomeWizardP1URL
	if p1URL == "auto" {
		if discovered, err := homewizard.Discover(context.Background()); err != nil {
			slog.Info("HomeWizard P1 not discovered, meter disabled", "error", err)
			p1URL = ""
		} else {
			p1URL = discovered.URL
			slog.Info(
				"HomeWizard P1 auto-discovered",
				"host", endpointHost(p1URL),
				"serial", discovered.Serial,
				"hostname", discovered.Hostname,
				"method", discovered.Method,
			)
		}
	}
	p1Client := homewizard.New(p1URL)
	if p1Client.Enabled() {
		if info, err := p1Client.GetDeviceInfo(); err != nil {
			slog.Warn("HomeWizard P1 meter unreachable at startup, will retry during operation", "host", endpointHost(p1URL), "error", err)
		} else {
			slog.Info("HomeWizard P1 meter enabled", "host", endpointHost(p1URL), "product", info.ProductName, "serial", info.Serial, "firmware", info.Firmware)
		}
	} else {
		slog.Info("HomeWizard P1 meter disabled (no URL configured)")
	}

	telegramClient, err := telegram.New(
		cfg.TelegramBotToken,
		cfg.TelegramChatID,
		filepath.Join(cfg.DataDir, "telegram-update-offset"),
	)
	if err != nil {
		// Telegram is optional and must not prevent startup battery reconciliation.
		slog.Warn("failed to initialize Telegram client; integration disabled", "error", err)
		telegramClient, _ = telegram.New("", "", "")
	}

	if telegramClient.Enabled() {
		if err := telegramClient.RegisterCommands(context.Background()); err != nil {
			slog.Warn("failed to register Telegram commands", "error", err)
		} else {
			slog.Info("telegram notifications and commands enabled")
		}
	}

	// Initialize recorder with configured timezone
	recorder := service.NewRecorder(cfg.DataDir, cfg.BatteryEfficiency, cfg.Location())

	// Initialize trading service
	tradingSvc := service.New(cfg, nordpoolClient, esphomeClient, p1Client, telegramClient, recorder)

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

	// Create context for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// WaitGroup for graceful shutdown
	var wg sync.WaitGroup

	// Start HTTP server
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("HTTP server listening", "addr", cfg.HTTPListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			stop()
		}
	}()

	// Start trading loop in background
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tradingSvc.Start(ctx); err != nil && err != context.Canceled {
			slog.Error("trading service error", "error", err)
			stop()
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

func endpointHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid]"
	}
	return parsed.Host
}

func reconcileBatteryAfterEnvFailure(rawURL string) {
	rawURL = safeEmergencyESPHomeURL(rawURL)
	if rawURL == "" {
		slog.Error("cannot identify a safe ESPHome endpoint for emergency stop after .env load failure")
		return
	}
	client := esphome.New(rawURL, 11)
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	err := client.IdleContext(stopCtx)
	cancel()
	if err != nil {
		slog.Error("failed to confirm battery stop after .env load failure", "error", err)
		return
	}
	slog.Info("battery stop confirmed after .env load failure")
}

// emergencyESPHomeURL extracts only the battery endpoint from a malformed
// dotenv file. It is used to stop the battery, never to start normal operation.
func emergencyESPHomeURL(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var result string
	for _, line := range strings.Split(string(data), "\n") {
		assignment := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, _, found := strings.Cut(assignment, "=")
		if !found || strings.TrimSpace(key) != "ESPHOME_URL" {
			continue
		}
		// Every later assignment supersedes the earlier one, including an empty or
		// invalid value. Parse the line with the same syntax as the normal loader.
		result = ""
		values, err := godotenv.Unmarshal(line)
		if err == nil {
			result = safeEmergencyESPHomeURL(values["ESPHOME_URL"])
		}
	}
	return result
}

func safeEmergencyESPHomeURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return rawURL
}

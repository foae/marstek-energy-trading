package config

import (
	"fmt"
	"math"
	"net/url"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/shopspring/decimal"
)

const (
	MinDischargePowerW = 800
	MaxDischargePowerW = 2500
)

// Config holds all configuration for the energy trader service.
type Config struct {
	// Service
	ServiceName    string `env:"SERVICE_NAME" envDefault:"energy-trader"`
	LogLevel       string `env:"LOG_LEVEL" envDefault:"info"`
	HTTPListenAddr string `env:"HTTP_LISTEN_ADDR" envDefault:"127.0.0.1:8080"`
	DataDir        string `env:"DATA_DIR" envDefault:"./data"`
	TZ             string `env:"TZ" envDefault:"Europe/Amsterdam"`

	// NordPool and all-in price components
	NordPoolArea         string          `env:"NORDPOOL_AREA" envDefault:"NL"`
	NordPoolCurrency     string          `env:"NORDPOOL_CURRENCY" envDefault:"EUR"`
	EnergyTaxEURPerKWh   decimal.Decimal `env:"ENERGY_TAX_EUR_PER_KWH" envDefault:"0.09161"`
	VATRate              decimal.Decimal `env:"VAT_RATE" envDefault:"0.21"`
	SupplierFeeEURPerKWh decimal.Decimal `env:"SUPPLIER_FEE_EUR_PER_KWH" envDefault:"0.02"`

	// Trading
	MinPriceSpread     float64 `env:"MIN_PRICE_SPREAD" envDefault:"0.05"` // Historical name: minimum expected profit after efficiency loss.
	BatteryEfficiency  float64 `env:"BATTERY_EFFICIENCY" envDefault:"0.90"`
	BatteryCapacityKWh float64 `env:"BATTERY_CAPACITY_KWH" envDefault:"5.12"`
	BatteryMinSOC      float64 `env:"BATTERY_MIN_SOC" envDefault:"0.11"`
	MaxCyclesPerDay    int     `env:"MAX_CYCLES_PER_DAY" envDefault:"2"`

	// Battery
	BatteryUDPAddr       string `env:"BATTERY_UDP_ADDR"` // No default (optional, for UDP client)
	ESPHomeURL           string `env:"ESPHOME_URL"`      // Required ESPHome REST API URL
	ChargePowerW         int    `env:"CHARGE_POWER_W" envDefault:"2500"`
	DischargePowerW      int    `env:"DISCHARGE_POWER_W" envDefault:"2500"`
	PassiveModeTimeoutS  int    `env:"PASSIVE_MODE_TIMEOUT_S" envDefault:"300"`
	ESPHomeRestartButton string `env:"ESPHOME_RESTART_BUTTON"` // ESPHome restart button name as exposed in its web URLs, e.g. "Restart"; empty = manual power-cycle only

	// HomeWizard P1 meter (optional)
	HomeWizardP1URL  string `env:"HOMEWIZARD_P1_URL"`                    // Empty = disabled; "auto" = explicit discovery
	SolarMinSurplusW int    `env:"SOLAR_MIN_SURPLUS_W" envDefault:"100"` // Min surplus watts to start solar charging

	// Telegram (optional)
	TelegramBotToken string `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID   string `env:"TELEGRAM_CHAT_ID"`
}

// Load parses environment variables into Config.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate checks that config values are within expected bounds.
func (c *Config) validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("DATA_DIR is required for automatic cycle commitment persistence")
	}
	if c.NordPoolCurrency != "EUR" {
		return fmt.Errorf("NORDPOOL_CURRENCY must be EUR when all-in pricing is enabled, got %q", c.NordPoolCurrency)
	}
	if !isFinite(c.BatteryEfficiency) || c.BatteryEfficiency <= 0 || c.BatteryEfficiency > 1.0 {
		return fmt.Errorf("BATTERY_EFFICIENCY must be finite and in (0.0, 1.0], got %f", c.BatteryEfficiency)
	}
	if !isFinite(c.BatteryMinSOC) || c.BatteryMinSOC < 0 || c.BatteryMinSOC >= 1.0 {
		return fmt.Errorf("BATTERY_MIN_SOC must be finite and in [0.0, 1.0), got %f", c.BatteryMinSOC)
	}
	if !isFinite(c.MinPriceSpread) || c.MinPriceSpread < 0 {
		return fmt.Errorf("MIN_PRICE_SPREAD must be finite and >= 0, got %f", c.MinPriceSpread)
	}
	if !isFinite(c.BatteryCapacityKWh) || c.BatteryCapacityKWh <= 0 {
		return fmt.Errorf("BATTERY_CAPACITY_KWH must be finite and > 0, got %f", c.BatteryCapacityKWh)
	}
	if c.ChargePowerW < 75 || c.ChargePowerW > 2500 {
		return fmt.Errorf("CHARGE_POWER_W must be between 75 and 2500, got %d", c.ChargePowerW)
	}
	if c.PassiveModeTimeoutS <= 0 || c.PassiveModeTimeoutS > 86400 {
		return fmt.Errorf("PASSIVE_MODE_TIMEOUT_S must be between 1 and 86400, got %d", c.PassiveModeTimeoutS)
	}
	if c.SolarMinSurplusW <= 0 {
		return fmt.Errorf("SOLAR_MIN_SURPLUS_W must be > 0, got %d", c.SolarMinSurplusW)
	}
	if c.MaxCyclesPerDay < 1 || c.MaxCyclesPerDay > 48 {
		return fmt.Errorf("MAX_CYCLES_PER_DAY must be between 1 and 48, got %d", c.MaxCyclesPerDay)
	}
	if c.EnergyTaxEURPerKWh.IsNegative() {
		return fmt.Errorf("ENERGY_TAX_EUR_PER_KWH must be >= 0, got %s", c.EnergyTaxEURPerKWh)
	}
	if c.VATRate.IsNegative() || c.VATRate.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("VAT_RATE must be in [0.0, 1.0], got %s", c.VATRate)
	}
	if c.SupplierFeeEURPerKWh.IsNegative() {
		return fmt.Errorf("SUPPLIER_FEE_EUR_PER_KWH must be >= 0, got %s", c.SupplierFeeEURPerKWh)
	}
	if c.DischargePowerW < MinDischargePowerW || c.DischargePowerW > MaxDischargePowerW {
		return fmt.Errorf("DISCHARGE_POWER_W must be between %d and %d, got %d", MinDischargePowerW, MaxDischargePowerW, c.DischargePowerW)
	}
	if c.ESPHomeURL == "" {
		return fmt.Errorf("ESPHOME_URL is required")
	}
	for _, setting := range [...]struct{ name, endpoint string }{
		{"ESPHOME_URL", c.ESPHomeURL}, {"HOMEWIZARD_P1_URL", c.HomeWizardP1URL},
	} {
		if setting.name == "HOMEWIZARD_P1_URL" && setting.endpoint == "" {
			continue
		}
		if setting.name == "HOMEWIZARD_P1_URL" && setting.endpoint == "auto" {
			continue
		}
		parsed, err := url.Parse(setting.endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s must be an absolute HTTP(S) URL", setting.name)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s must not contain credentials, a query, or a fragment", setting.name)
		}
	}
	if _, err := time.LoadLocation(c.TZ); err != nil {
		return fmt.Errorf("invalid TZ %q: %w", c.TZ, err)
	}
	return nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// MinSOCPercent rounds protection upward to the battery's integer SOC resolution.
func (c *Config) MinSOCPercent() int {
	return int(decimal.NewFromFloat(c.BatteryMinSOC).Mul(decimal.NewFromInt(100)).Ceil().IntPart())
}

// TelegramEnabled returns true if Telegram notifications are configured.
func (c *Config) TelegramEnabled() bool {
	return c.TelegramBotToken != "" && c.TelegramChatID != ""
}

// Location returns the configured timezone location.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.TZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

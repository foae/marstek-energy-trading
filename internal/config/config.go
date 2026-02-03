package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds all configuration for the energy trader service.
type Config struct {
	// Service
	ServiceName    string `env:"SERVICE_NAME" envDefault:"energy-trader"`
	LogLevel       string `env:"LOG_LEVEL" envDefault:"info"`
	HTTPListenAddr string `env:"HTTP_LISTEN_ADDR" envDefault:":8080"`
	DataDir        string `env:"DATA_DIR" envDefault:"./data"`
	TZ             string `env:"TZ" envDefault:"Europe/Amsterdam"`

	// NordPool
	NordPoolArea     string `env:"NORDPOOL_AREA" envDefault:"NL"`
	NordPoolCurrency string `env:"NORDPOOL_CURRENCY" envDefault:"EUR"`

	// Trading
	MinPriceSpread     float64 `env:"MIN_PRICE_SPREAD" envDefault:"0.05"`
	BatteryEfficiency  float64 `env:"BATTERY_EFFICIENCY" envDefault:"0.90"`
	BatteryCapacityKWh float64 `env:"BATTERY_CAPACITY_KWH" envDefault:"5.12"`
	BatteryMinSOC      float64 `env:"BATTERY_MIN_SOC" envDefault:"0.11"`
	MaxCyclesPerDay    int     `env:"MAX_CYCLES_PER_DAY" envDefault:"2"`

	// Battery
	BatteryUDPAddr      string `env:"BATTERY_UDP_ADDR"`                             // No default (optional, for UDP client)
	ESPHomeURL          string `env:"ESPHOME_URL" envDefault:"http://192.168.1.50"` // ESPHome REST API
	ChargePowerW        int    `env:"CHARGE_POWER_W" envDefault:"2500"`
	DischargePowerW     int    `env:"DISCHARGE_POWER_W" envDefault:"2500"`
	PassiveModeTimeoutS int    `env:"PASSIVE_MODE_TIMEOUT_S" envDefault:"300"`

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
	return cfg, nil
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

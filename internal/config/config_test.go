package config

import (
	"testing"
	"time"
)

func TestTelegramEnabled(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		chatID   string
		expected bool
	}{
		{"both set", "bot123:token", "12345", true},
		{"token missing", "", "12345", false},
		{"chatID missing", "bot123:token", "", false},
		{"both missing", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				TelegramBotToken: tt.token,
				TelegramChatID:   tt.chatID,
			}
			if got := cfg.TelegramEnabled(); got != tt.expected {
				t.Errorf("TelegramEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestLocation_ValidTimezone(t *testing.T) {
	cfg := &Config{TZ: "Europe/Amsterdam"}
	loc := cfg.Location()

	if loc.String() != "Europe/Amsterdam" {
		t.Errorf("Location() = %s, want Europe/Amsterdam", loc.String())
	}
}

func TestLocation_InvalidTimezone(t *testing.T) {
	cfg := &Config{TZ: "Invalid/Timezone"}
	loc := cfg.Location()

	if loc != time.UTC {
		t.Errorf("Location() = %s, want UTC for invalid timezone", loc.String())
	}
}

func TestLocation_EmptyTimezone(t *testing.T) {
	cfg := &Config{TZ: ""}
	loc := cfg.Location()

	if loc != time.UTC {
		t.Errorf("Location() = %s, want UTC for empty timezone", loc.String())
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Clear relevant env vars to test defaults
	for _, key := range []string{
		"SERVICE_NAME", "LOG_LEVEL", "HTTP_LISTEN_ADDR", "DATA_DIR", "TZ",
		"NORDPOOL_AREA", "NORDPOOL_CURRENCY",
		"MIN_PRICE_SPREAD", "BATTERY_EFFICIENCY", "BATTERY_CAPACITY_KWH",
		"BATTERY_MIN_SOC", "MAX_CYCLES_PER_DAY",
		"ESPHOME_URL", "CHARGE_POWER_W", "DISCHARGE_POWER_W", "PASSIVE_MODE_TIMEOUT_S",
		"HOMEWIZARD_P1_URL", "SOLAR_MIN_SURPLUS_W",
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"BATTERY_UDP_ADDR",
	} {
		t.Setenv(key, "")
	}

	// env/v11 treats empty string as "set" for string fields, so we need to
	// just verify the numeric defaults work when env vars are unset.
	// Unset the numeric ones so they fall back to envDefault.
	t.Setenv("SOLAR_MIN_SURPLUS_W", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.SolarMinSurplusW != 100 {
		t.Errorf("SolarMinSurplusW = %d, want 100 (default)", cfg.SolarMinSurplusW)
	}
	if cfg.BatteryEfficiency != 0.90 {
		t.Errorf("BatteryEfficiency = %f, want 0.90", cfg.BatteryEfficiency)
	}
	if cfg.ChargePowerW != 2500 {
		t.Errorf("ChargePowerW = %d, want 2500", cfg.ChargePowerW)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("HOMEWIZARD_P1_URL", "http://192.168.1.100")
	t.Setenv("SOLAR_MIN_SURPLUS_W", "200")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HomeWizardP1URL != "http://192.168.1.100" {
		t.Errorf("HomeWizardP1URL = %q, want %q", cfg.HomeWizardP1URL, "http://192.168.1.100")
	}
	if cfg.SolarMinSurplusW != 200 {
		t.Errorf("SolarMinSurplusW = %d, want 200", cfg.SolarMinSurplusW)
	}
}

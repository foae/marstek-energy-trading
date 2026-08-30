package config

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
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
		"NORDPOOL_AREA", "NORDPOOL_CURRENCY", "ENERGY_TAX_EUR_PER_KWH",
		"VAT_RATE", "SUPPLIER_FEE_EUR_PER_KWH",
		"MIN_PRICE_SPREAD", "BATTERY_EFFICIENCY", "BATTERY_CAPACITY_KWH",
		"BATTERY_MIN_SOC", "MAX_CYCLES_PER_DAY",
		"ESPHOME_URL", "CHARGE_POWER_W", "DISCHARGE_POWER_W", "PASSIVE_MODE_TIMEOUT_S",
		"HOMEWIZARD_P1_URL", "SOLAR_MIN_SURPLUS_W",
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
		"BATTERY_UDP_ADDR",
	} {
		t.Setenv(key, "")
	}

	// All-in pricing is deliberately EUR-only; keep the string field valid
	// while empty numeric fields exercise envDefault parsing.
	t.Setenv("NORDPOOL_CURRENCY", "EUR")

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
	if !cfg.EnergyTaxEURPerKWh.Equal(decimal.RequireFromString("0.09161")) {
		t.Errorf("EnergyTaxEURPerKWh = %s, want 0.09161 (2026 rate)", cfg.EnergyTaxEURPerKWh)
	}
	if !cfg.VATRate.Equal(decimal.RequireFromString("0.21")) {
		t.Errorf("VATRate = %s, want 0.21", cfg.VATRate)
	}
	if !cfg.SupplierFeeEURPerKWh.Equal(decimal.RequireFromString("0.02")) {
		t.Errorf("SupplierFeeEURPerKWh = %s, want 0.02", cfg.SupplierFeeEURPerKWh)
	}
}

func TestValidate_BatteryEfficiency(t *testing.T) {
	tests := []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"valid 0.90", 0.90, false},
		{"valid 1.0", 1.0, false},
		{"valid 0.5", 0.5, false},
		{"invalid 0", 0, true},
		{"invalid negative", -0.5, true},
		{"invalid >1", 1.1, true},
		{"invalid integer-like 90", 90, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{NordPoolCurrency: "EUR", BatteryEfficiency: tt.value, BatteryMinSOC: 0.11, DischargePowerW: 2500}
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_BatteryMinSOC(t *testing.T) {
	tests := []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"valid 0.11", 0.11, false},
		{"valid 0", 0, false},
		{"valid 0.99", 0.99, false},
		{"invalid 1.0", 1.0, true},
		{"invalid negative", -0.1, true},
		{"invalid integer-like 11", 11, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{NordPoolCurrency: "EUR", BatteryEfficiency: 0.90, BatteryMinSOC: tt.value, DischargePowerW: 2500}
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_MinPriceSpread(t *testing.T) {
	cfg := &Config{NordPoolCurrency: "EUR", BatteryEfficiency: 0.90, BatteryMinSOC: 0.11, MinPriceSpread: -0.01, DischargePowerW: 2500}
	if err := cfg.validate(); err == nil {
		t.Error("expected error for negative MinPriceSpread")
	}

	cfg.MinPriceSpread = 0
	if err := cfg.validate(); err != nil {
		t.Errorf("unexpected error for zero MinPriceSpread: %v", err)
	}
}

func TestValidate_AllInPricing(t *testing.T) {
	valid := Config{
		NordPoolCurrency:     "EUR",
		BatteryEfficiency:    0.90,
		BatteryMinSOC:        0.11,
		EnergyTaxEURPerKWh:   decimal.RequireFromString("0.09161"),
		VATRate:              decimal.RequireFromString("0.21"),
		SupplierFeeEURPerKWh: decimal.RequireFromString("0.02"),
		DischargePowerW:      2500,
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "non-EUR NordPool currency",
			mutate: func(cfg *Config) {
				cfg.NordPoolCurrency = "GBP"
			},
		},
		{
			name: "negative energy tax",
			mutate: func(cfg *Config) {
				cfg.EnergyTaxEURPerKWh = decimal.RequireFromString("-0.01")
			},
		},
		{
			name: "negative VAT",
			mutate: func(cfg *Config) {
				cfg.VATRate = decimal.RequireFromString("-0.01")
			},
		},
		{
			name: "VAT above one",
			mutate: func(cfg *Config) {
				cfg.VATRate = decimal.RequireFromString("1.01")
			},
		},
		{
			name: "negative supplier fee",
			mutate: func(cfg *Config) {
				cfg.SupplierFeeEURPerKWh = decimal.RequireFromString("-0.01")
			},
		},
	}

	if err := valid.validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("validate() error = nil, want error")
			}
		})
	}
}

func TestValidateDischargePower(t *testing.T) {
	for _, powerW := range []int{MinDischargePowerW, 1500, MaxDischargePowerW} {
		cfg := Config{
			NordPoolCurrency:  "EUR",
			BatteryEfficiency: 0.90,
			BatteryMinSOC:     0.11,
			DischargePowerW:   powerW,
		}
		if err := cfg.validate(); err != nil {
			t.Errorf("DischargePowerW %d: unexpected error: %v", powerW, err)
		}
	}

	for _, powerW := range []int{MinDischargePowerW - 1, MaxDischargePowerW + 1} {
		cfg := Config{
			NordPoolCurrency:  "EUR",
			BatteryEfficiency: 0.90,
			BatteryMinSOC:     0.11,
			DischargePowerW:   powerW,
		}
		if err := cfg.validate(); err == nil {
			t.Errorf("DischargePowerW %d: expected validation error", powerW)
		}
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

func TestLoad_AllInPricing(t *testing.T) {
	t.Setenv("ENERGY_TAX_EUR_PER_KWH", "0.09161")
	t.Setenv("VAT_RATE", "0.21")
	t.Setenv("SUPPLIER_FEE_EUR_PER_KWH", "0.02")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := cfg.EnergyTaxEURPerKWh.String(); got != "0.09161" {
		t.Errorf("EnergyTaxEURPerKWh = %s, want 0.09161", got)
	}
	if got := cfg.VATRate.String(); got != "0.21" {
		t.Errorf("VATRate = %s, want 0.21", got)
	}
	if got := cfg.SupplierFeeEURPerKWh.String(); got != "0.02" {
		t.Errorf("SupplierFeeEURPerKWh = %s, want 0.02", got)
	}
}

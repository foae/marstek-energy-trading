package config

import (
	"math"
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

func validConfig() Config {
	return Config{
		TZ:                  "Europe/Amsterdam",
		ESPHomeURL:          "http://192.168.1.50",
		NordPoolCurrency:    "EUR",
		MinPriceSpread:      0.05,
		BatteryEfficiency:   0.90,
		BatteryCapacityKWh:  5.12,
		BatteryMinSOC:       0.11,
		MaxCyclesPerDay:     2,
		ChargePowerW:        2500,
		DischargePowerW:     2500,
		PassiveModeTimeoutS: 300,
		SolarMinSurplusW:    100,
	}
}

func TestValidate_RejectsNonFiniteFloatValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"NaN battery efficiency", func(cfg *Config) { cfg.BatteryEfficiency = math.NaN() }},
		{"infinite battery efficiency", func(cfg *Config) { cfg.BatteryEfficiency = math.Inf(1) }},
		{"NaN battery minimum SOC", func(cfg *Config) { cfg.BatteryMinSOC = math.NaN() }},
		{"infinite battery minimum SOC", func(cfg *Config) { cfg.BatteryMinSOC = math.Inf(1) }},
		{"NaN price spread", func(cfg *Config) { cfg.MinPriceSpread = math.NaN() }},
		{"infinite price spread", func(cfg *Config) { cfg.MinPriceSpread = math.Inf(1) }},
		{"NaN battery capacity", func(cfg *Config) { cfg.BatteryCapacityKWh = math.NaN() }},
		{"infinite battery capacity", func(cfg *Config) { cfg.BatteryCapacityKWh = math.Inf(1) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			if err := cfg.validate(); err == nil {
				t.Fatal("validate() error = nil, want error")
			}
		})
	}
}

func TestValidate_SafetyBoundaryValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"minimum charge power", func(cfg *Config) { cfg.ChargePowerW = 75 }},
		{"maximum charge power", func(cfg *Config) { cfg.ChargePowerW = 2500 }},
		{"positive battery capacity", func(cfg *Config) { cfg.BatteryCapacityKWh = 0.01 }},
		{"minimum passive mode timeout", func(cfg *Config) { cfg.PassiveModeTimeoutS = 1 }},
		{"maximum passive mode timeout", func(cfg *Config) { cfg.PassiveModeTimeoutS = 86400 }},
		{"minimum solar surplus", func(cfg *Config) { cfg.SolarMinSurplusW = 1 }},
		{"minimum maximum cycles", func(cfg *Config) { cfg.MaxCyclesPerDay = 1 }},
		{"maximum maximum cycles", func(cfg *Config) { cfg.MaxCyclesPerDay = 48 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
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
			cfg := validConfig()
			cfg.BatteryEfficiency = tt.value
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
			cfg := validConfig()
			cfg.BatteryMinSOC = tt.value
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_MinPriceSpread(t *testing.T) {
	cfg := validConfig()
	cfg.MinPriceSpread = -0.01
	if err := cfg.validate(); err == nil {
		t.Error("expected error for negative MinPriceSpread")
	}

	cfg.MinPriceSpread = 0
	if err := cfg.validate(); err != nil {
		t.Errorf("unexpected error for zero MinPriceSpread: %v", err)
	}
}

func TestValidate_AllInPricing(t *testing.T) {
	valid := validConfig()
	valid.EnergyTaxEURPerKWh = decimal.RequireFromString("0.09161")
	valid.VATRate = decimal.RequireFromString("0.21")
	valid.SupplierFeeEURPerKWh = decimal.RequireFromString("0.02")

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
		cfg := validConfig()
		cfg.DischargePowerW = powerW
		if err := cfg.validate(); err != nil {
			t.Errorf("DischargePowerW %d: unexpected error: %v", powerW, err)
		}
	}

	for _, powerW := range []int{MinDischargePowerW - 1, MaxDischargePowerW + 1} {
		cfg := validConfig()
		cfg.DischargePowerW = powerW
		if err := cfg.validate(); err == nil {
			t.Errorf("DischargePowerW %d: expected validation error", powerW)
		}
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("HOMEWIZARD_P1_URL", "http://192.168.1.100")
	t.Setenv("SOLAR_MIN_SURPLUS_W", "200")
	t.Setenv("ESPHOME_RESTART_BUTTON", "restart")

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
	if cfg.ESPHomeRestartButton != "restart" {
		t.Errorf("ESPHomeRestartButton = %q, want %q", cfg.ESPHomeRestartButton, "restart")
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

func setValidLoadEnvironment(t *testing.T) {
	t.Helper()

	for key, value := range map[string]string{
		"TZ":                       "Europe/Amsterdam",
		"NORDPOOL_CURRENCY":        "EUR",
		"MIN_PRICE_SPREAD":         "0.05",
		"BATTERY_EFFICIENCY":       "0.90",
		"BATTERY_CAPACITY_KWH":     "5.12",
		"BATTERY_MIN_SOC":          "0.11",
		"MAX_CYCLES_PER_DAY":       "2",
		"CHARGE_POWER_W":           "2500",
		"DISCHARGE_POWER_W":        "2500",
		"PASSIVE_MODE_TIMEOUT_S":   "300",
		"SOLAR_MIN_SURPLUS_W":      "100",
		"ENERGY_TAX_EUR_PER_KWH":   "0.09161",
		"VAT_RATE":                 "0.21",
		"SUPPLIER_FEE_EUR_PER_KWH": "0.02",
	} {
		t.Setenv(key, value)
	}
}

func TestLoad_RejectsInvalidSafetyValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"NaN battery efficiency", "BATTERY_EFFICIENCY", "NaN"},
		{"infinite battery efficiency", "BATTERY_EFFICIENCY", "Inf"},
		{"NaN battery minimum SOC", "BATTERY_MIN_SOC", "NaN"},
		{"infinite battery minimum SOC", "BATTERY_MIN_SOC", "Inf"},
		{"NaN price spread", "MIN_PRICE_SPREAD", "NaN"},
		{"infinite price spread", "MIN_PRICE_SPREAD", "Inf"},
		{"NaN battery capacity", "BATTERY_CAPACITY_KWH", "NaN"},
		{"infinite battery capacity", "BATTERY_CAPACITY_KWH", "Inf"},
		{"zero battery capacity", "BATTERY_CAPACITY_KWH", "0"},
		{"charge power below minimum", "CHARGE_POWER_W", "74"},
		{"charge power above maximum", "CHARGE_POWER_W", "2501"},
		{"zero passive mode timeout", "PASSIVE_MODE_TIMEOUT_S", "0"},
		{"excessive passive mode timeout", "PASSIVE_MODE_TIMEOUT_S", "86401"},
		{"zero solar surplus", "SOLAR_MIN_SURPLUS_W", "0"},
		{"zero maximum cycles", "MAX_CYCLES_PER_DAY", "0"},
		{"excessive maximum cycles", "MAX_CYCLES_PER_DAY", "49"},
		{"invalid timezone", "TZ", "Invalid/Timezone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidLoadEnvironment(t)
			t.Setenv(tt.key, tt.value)

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

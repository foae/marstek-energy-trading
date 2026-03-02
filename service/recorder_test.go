package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestRecordTrade_Basic(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)

	trade := Trade{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.05),
		PowerW:    2500,
		DurationS: 3600,
		EnergyKWh: decimal.NewFromFloat(2.5),
		StartSOC:  20,
		EndSOC:    70,
	}

	if err := r.RecordTrade(trade); err != nil {
		t.Fatalf("RecordTrade() error = %v", err)
	}

	history := r.GetHistory()
	if len(history.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(history.Days))
	}
	if history.Days[0].ChargeCycles != 1 {
		t.Errorf("expected 1 charge cycle, got %d", history.Days[0].ChargeCycles)
	}
}

func TestGetHistory_SolarChargeZeroCost(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Solar charge: free energy
	r.RecordTrade(Trade{
		Timestamp: ts,
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		PowerW:    500,
		DurationS: 3600,
		EnergyKWh: decimal.NewFromFloat(0.5),
		StartSOC:  50,
		EndSOC:    60,
	})

	// Grid discharge at 0.20 EUR/kWh
	r.RecordTrade(Trade{
		Timestamp: ts.Add(2 * time.Hour),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.20),
		PowerW:    2500,
		DurationS: 720,
		EnergyKWh: decimal.NewFromFloat(0.5),
		StartSOC:  60,
		EndSOC:    50,
	})

	history := r.GetHistory()
	if len(history.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(history.Days))
	}

	day := history.Days[0]

	// Solar charge should add to chargedKWh but not to cost
	expectedCharged := decimal.NewFromFloat(0.5)
	if !day.ChargedKWh.Equal(expectedCharged) {
		t.Errorf("ChargedKWh = %s, want %s", day.ChargedKWh, expectedCharged)
	}

	// P&L should be pure profit: revenue(0.5*0.20) - cost(0) = 0.10
	expectedPnL := decimal.NewFromFloat(0.10)
	if !day.PnLEUR.Equal(expectedPnL) {
		t.Errorf("PnLEUR = %s, want %s", day.PnLEUR, expectedPnL)
	}
}

func TestGetHistory_MixedChargeAndSolarCharge(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// Grid charge at 0.05 EUR/kWh
	r.RecordTrade(Trade{
		Timestamp: ts,
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.05),
		PowerW:    2500,
		DurationS: 3600,
		EnergyKWh: decimal.NewFromFloat(2.0),
		StartSOC:  20,
		EndSOC:    60,
	})

	// Solar charge: free
	r.RecordTrade(Trade{
		Timestamp: ts.Add(4 * time.Hour),
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		PowerW:    800,
		DurationS: 3600,
		EnergyKWh: decimal.NewFromFloat(0.8),
		StartSOC:  60,
		EndSOC:    75,
	})

	// Discharge at 0.15 EUR/kWh
	r.RecordTrade(Trade{
		Timestamp: ts.Add(8 * time.Hour),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.15),
		PowerW:    2500,
		DurationS: 3600,
		EnergyKWh: decimal.NewFromFloat(2.5),
		StartSOC:  75,
		EndSOC:    25,
	})

	day := r.GetHistory().Days[0]

	// Total charged = grid(2.0) + solar(0.8) = 2.8
	expectedCharged := decimal.NewFromFloat(2.8)
	if !day.ChargedKWh.Equal(expectedCharged) {
		t.Errorf("ChargedKWh = %s, want %s", day.ChargedKWh, expectedCharged)
	}

	// Cost = grid charge only: 2.0 * 0.05 = 0.10
	// Revenue = discharge: 2.5 * 0.15 = 0.375
	// PnL = 0.375 - 0.10 = 0.275
	expectedPnL := decimal.NewFromFloat(0.275)
	if !day.PnLEUR.Equal(expectedPnL) {
		t.Errorf("PnLEUR = %s, want %s", day.PnLEUR, expectedPnL)
	}

	// Charge cycles = 2 (grid + solar)
	if day.ChargeCycles != 2 {
		t.Errorf("ChargeCycles = %d, want 2", day.ChargeCycles)
	}

	// Min charge price should be from grid charge only (solar has no price entry)
	expectedMinPrice := decimal.NewFromFloat(0.05)
	if !day.MinChargePrice.Equal(expectedMinPrice) {
		t.Errorf("MinChargePrice = %s, want %s", day.MinChargePrice, expectedMinPrice)
	}
}

func TestGetTotalPnL_SolarChargeExcludedFromCost(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Only solar charge + discharge
	r.RecordTrade(Trade{
		Timestamp: ts,
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		EnergyKWh: decimal.NewFromFloat(1.0),
	})
	r.RecordTrade(Trade{
		Timestamp: ts.Add(time.Hour),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.20),
		EnergyKWh: decimal.NewFromFloat(0.9),
	})

	// PnL = revenue(0.9*0.20) - cost(0) = 0.18
	expected := decimal.NewFromFloat(0.18)
	got := r.GetTotalPnL()
	if !got.Equal(expected) {
		t.Errorf("GetTotalPnL() = %s, want %s", got, expected)
	}
}

func TestGetLastChargeTrade_IgnoresSolarCharge(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// Grid charge
	r.RecordTrade(Trade{
		Timestamp: ts,
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.05),
		EnergyKWh: decimal.NewFromFloat(2.0),
	})

	// Solar charge (should be ignored by GetLastChargeTrade)
	r.RecordTrade(Trade{
		Timestamp: ts.Add(time.Hour),
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		EnergyKWh: decimal.NewFromFloat(1.0),
	})

	// Discharge
	r.RecordTrade(Trade{
		Timestamp: ts.Add(2 * time.Hour),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.20),
		EnergyKWh: decimal.NewFromFloat(1.0),
	})

	last := r.GetLastChargeTrade()
	if last == nil {
		t.Fatal("GetLastChargeTrade() returned nil")
	}
	if last.Action != ActionCharge {
		t.Errorf("expected ActionCharge, got %s", last.Action)
	}
	if !last.PriceEUR.Equal(decimal.NewFromFloat(0.05)) {
		t.Errorf("expected price 0.05, got %s", last.PriceEUR)
	}
}

func TestGetLastChargeTrade_NoCharges(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)

	r.RecordTrade(Trade{
		Timestamp: time.Now(),
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		EnergyKWh: decimal.NewFromFloat(1.0),
	})

	if r.GetLastChargeTrade() != nil {
		t.Error("GetLastChargeTrade() should return nil when only solar charges exist")
	}
}

func TestGetHistory_EmptyRecorder(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	history := r.GetHistory()

	if len(history.Days) != 0 {
		t.Errorf("expected 0 days, got %d", len(history.Days))
	}
	if history.TotalDays != 0 {
		t.Errorf("expected TotalDays=0, got %d", history.TotalDays)
	}
}

func TestGetHistory_MultiDaySorting(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)

	// Add trades on different days, out of order
	r.RecordTrade(Trade{
		Timestamp: time.Date(2024, 1, 10, 10, 0, 0, 0, time.UTC),
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.05),
		EnergyKWh: decimal.NewFromFloat(1.0),
	})
	r.RecordTrade(Trade{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.06),
		EnergyKWh: decimal.NewFromFloat(1.0),
	})
	r.RecordTrade(Trade{
		Timestamp: time.Date(2024, 1, 12, 10, 0, 0, 0, time.UTC),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.15),
		EnergyKWh: decimal.NewFromFloat(1.0),
	})

	history := r.GetHistory()
	if len(history.Days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(history.Days))
	}

	// Should be sorted descending
	if history.Days[0].Date != "2024-01-15" {
		t.Errorf("first day = %s, want 2024-01-15", history.Days[0].Date)
	}
	if history.Days[1].Date != "2024-01-12" {
		t.Errorf("second day = %s, want 2024-01-12", history.Days[1].Date)
	}
	if history.Days[2].Date != "2024-01-10" {
		t.Errorf("third day = %s, want 2024-01-10", history.Days[2].Date)
	}
}

func TestGetTodaySummary_NoTrades(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	summary := r.GetTodaySummary()

	today := time.Now().In(time.UTC).Format("2006-01-02")
	if summary.Date != today {
		t.Errorf("Date = %s, want %s", summary.Date, today)
	}
	if len(summary.Trades) != 0 {
		t.Errorf("expected 0 trades, got %d", len(summary.Trades))
	}
}

func TestLoadTrades_FileNotFound(t *testing.T) {
	r := NewRecorder(t.TempDir(), 0.90, time.UTC)

	// Should return nil (graceful) when file doesn't exist
	if err := r.LoadTrades(); err != nil {
		t.Errorf("LoadTrades() error = %v, want nil for missing file", err)
	}
}

func TestLoadTrades_CorruptedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trades.json")
	os.WriteFile(path, []byte("{invalid json"), 0644)

	r := NewRecorder(dir, 0.90, time.UTC)
	if err := r.LoadTrades(); err == nil {
		t.Error("LoadTrades() error = nil, want error for corrupted JSON")
	}
}

func TestLoadTrades_EmptyDataDir(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)

	// Should be a no-op
	if err := r.LoadTrades(); err != nil {
		t.Errorf("LoadTrades() error = %v, want nil for empty dataDir", err)
	}
}

func TestSaveTrades_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, 0.90, time.UTC)

	trade := Trade{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		PowerW:    500,
		DurationS: 600,
		EnergyKWh: decimal.NewFromFloat(0.083),
		StartSOC:  50,
		EndSOC:    52,
	}
	r.RecordTrade(trade)

	// Load into a new recorder
	r2 := NewRecorder(dir, 0.90, time.UTC)
	if err := r2.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}

	history := r2.GetHistory()
	if len(history.Days) != 1 {
		t.Fatalf("expected 1 day after load, got %d", len(history.Days))
	}
	if history.Days[0].Trades[0].Action != ActionSolarCharge {
		t.Errorf("expected ActionSolarCharge, got %s", history.Days[0].Trades[0].Action)
	}
}

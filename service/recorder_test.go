package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	// Grid charge cycles = 1
	if day.ChargeCycles != 1 {
		t.Errorf("ChargeCycles = %d, want 1", day.ChargeCycles)
	}

	// Solar charge cycles = 1
	if day.SolarChargeCycles != 1 {
		t.Errorf("SolarChargeCycles = %d, want 1", day.SolarChargeCycles)
	}

	// Solar charged kWh = 0.8
	expectedSolar := decimal.NewFromFloat(0.8)
	if !day.SolarChargedKWh.Equal(expectedSolar) {
		t.Errorf("SolarChargedKWh = %s, want %s", day.SolarChargedKWh, expectedSolar)
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
	dir := t.TempDir()
	r := NewRecorder(dir, 0.90, time.UTC)

	if err := r.LoadTrades(); err != nil {
		t.Errorf("LoadTrades() error = %v, want nil for missing file", err)
	}
	info, err := os.Stat(filepath.Join(dir, "trades.json"))
	if err != nil {
		t.Fatalf("stat initialized trades file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("trades file mode = %o, want 600", got)
	}
}

func TestLoadTrades_CreatesPrivateDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	r := NewRecorder(dir, 0.90, time.UTC)

	if err := r.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("data directory mode = %o, want 700", got)
	}
}

func TestRecordTradeProtectsStaleTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "trades.json.tmp")
	if err := os.WriteFile(tmpPath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("create stale temp file: %v", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		t.Fatalf("set stale temp mode: %v", err)
	}

	r := NewRecorder(dir, 0.90, time.UTC)
	if err := r.RecordTrade(Trade{Timestamp: time.Now(), Action: ActionCharge}); err != nil {
		t.Fatalf("RecordTrade() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "trades.json"))
	if err != nil {
		t.Fatalf("stat trades file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("trades file mode = %o, want 600", got)
	}
}

func TestLoadTrades_CorruptedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trades.json")
	corruptData := []byte("{invalid json")
	if err := os.WriteFile(path, corruptData, 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRecorder(dir, 0.90, time.UTC)
	if err := r.LoadTrades(); err == nil {
		t.Fatal("LoadTrades() error = nil, want error for corrupted JSON")
	}

	trade := Trade{Timestamp: time.Now(), Action: ActionCharge}
	if err := r.RecordTrade(trade); err == nil {
		t.Error("RecordTrade() error = nil after failed LoadTrades()")
	}
	if history := r.GetHistory(); len(history.Days) != 0 {
		t.Errorf("RecordTrade() mutated history after failed LoadTrades(): %+v", history)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corruptData) {
		t.Errorf("trades.json was overwritten after failed LoadTrades(): got %q, want %q", got, corruptData)
	}

	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v after repairing file", err)
	}
	if err := r.RecordTrade(trade); err != nil {
		t.Errorf("RecordTrade() error = %v after successful LoadTrades()", err)
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

func TestAutomaticCycleCommitmentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
		Profit:          decimal.NewFromFloat(.17),
	}
	r := NewRecorder(dir, .90, time.UTC)
	if err := r.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}

	loaded, err := NewRecorder(dir, .90, time.UTC).LoadAutomaticCycleCommitment()
	if err != nil {
		t.Fatalf("load commitment: %v", err)
	}
	if loaded == nil || !loaded.DischargeWindow.End.Equal(cycle.DischargeWindow.End) ||
		!loaded.DischargeWindow.Price.Equal(cycle.DischargeWindow.Price) {
		t.Fatalf("loaded commitment = %+v, want %+v", loaded, cycle)
	}
	if err := r.SaveAutomaticCycleCommitment(nil); err != nil {
		t.Fatalf("clear commitment: %v", err)
	}
	loaded, err = r.LoadAutomaticCycleCommitment()
	if err != nil || loaded != nil {
		t.Fatalf("commitment after clear = %+v, error = %v", loaded, err)
	}
}

func TestAutomaticCycleCommitmentClearRetrySyncsAbsentDeletion(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, .90, time.UTC)
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}
	if err := r.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}

	syncCalls := 0
	r.syncDirectoryFn = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	if err := r.SaveAutomaticCycleCommitment(nil); err == nil {
		t.Fatal("first clear unexpectedly ignored directory sync failure")
	}
	if err := r.SaveAutomaticCycleCommitment(nil); err != nil {
		t.Fatalf("retry clear with already-absent file: %v", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want retry after absent deletion", syncCalls)
	}
}

func TestFlushTradesRetriesFailedRecordPersistence(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, .90, time.UTC)
	syncCalls := 0
	r.syncDirectoryFn = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	trade := Trade{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		Action:    ActionCharge,
		EnergyKWh: decimal.RequireFromString("0.5"),
	}
	if err := r.RecordTrade(trade); err == nil {
		t.Fatal("RecordTrade() error = nil, want injected persistence failure")
	}
	if err := r.FlushTrades(); err != nil {
		t.Fatalf("FlushTrades() retry error = %v", err)
	}

	reloaded := NewRecorder(dir, .90, time.UTC)
	if err := reloaded.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}
	history := reloaded.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 || !history.Days[0].Trades[0].EnergyKWh.Equal(trade.EnergyKWh) {
		t.Fatalf("flushed trade history = %+v", history)
	}
}

func TestRecordTradeRejectsInconsistentDayAllocations(t *testing.T) {
	r := NewRecorder("", .90, time.UTC)
	err := r.RecordTrade(Trade{
		Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC),
		Action:    ActionCharge,
		PriceEUR:  decimal.RequireFromString("0.10"),
		DurationS: 60,
		EnergyKWh: decimal.NewFromInt(1),
		DayAllocations: []TradeDayAllocation{{
			Timestamp:      time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC),
			DurationS:      60,
			EnergyKWh:      decimal.NewFromInt(2),
			PricedValueEUR: decimal.RequireFromString("0.20"),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "do not match aggregate") {
		t.Fatalf("RecordTrade() error = %v, want inconsistent allocation rejection", err)
	}
	if history := r.GetHistory(); len(history.Days) != 0 {
		t.Fatalf("invalid trade mutated history: %+v", history)
	}

	for _, test := range []struct {
		name  string
		trade Trade
	}{
		{
			name: "zero duration",
			trade: Trade{
				Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), Action: ActionCharge,
				PriceEUR: decimal.RequireFromString("0.10"), DurationS: 60, EnergyKWh: decimal.NewFromInt(1),
				DayAllocations: []TradeDayAllocation{{Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), EnergyKWh: decimal.NewFromInt(1), PricedValueEUR: decimal.RequireFromString("0.10")}},
			},
		},
		{
			name: "partial duration coverage",
			trade: Trade{
				Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), Action: ActionCharge,
				PriceEUR: decimal.RequireFromString("0.10"), DurationS: 60, EnergyKWh: decimal.NewFromInt(1),
				DayAllocations: []TradeDayAllocation{{Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), DurationS: 30, EnergyKWh: decimal.NewFromInt(1), PricedValueEUR: decimal.RequireFromString("0.10")}},
			},
		},
		{
			name: "overlapping intervals",
			trade: Trade{
				Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), Action: ActionCharge,
				PriceEUR: decimal.RequireFromString("0.10"), DurationS: 60, EnergyKWh: decimal.NewFromInt(1),
				DayAllocations: []TradeDayAllocation{
					{Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), DurationS: 40, EnergyKWh: decimal.RequireFromString("0.5"), PricedValueEUR: decimal.RequireFromString("0.05")},
					{Timestamp: time.Date(2024, 1, 15, 23, 30, 20, 0, time.UTC), DurationS: 40, EnergyKWh: decimal.RequireFromString("0.5"), PricedValueEUR: decimal.RequireFromString("0.05")},
				},
			},
		},
		{
			name: "overlapping solar components",
			trade: Trade{
				Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), Action: ActionSolarCharge, DurationS: 60,
				EnergyKWh: decimal.NewFromInt(1), GridEnergyKWh: decimal.RequireFromString("0.8"), GridUnpricedKWh: decimal.RequireFromString("0.8"), UnpricedKWh: decimal.RequireFromString("0.5"),
				DayAllocations: []TradeDayAllocation{{Timestamp: time.Date(2024, 1, 15, 23, 30, 0, 0, time.UTC), DurationS: 60, EnergyKWh: decimal.NewFromInt(1), GridEnergyKWh: decimal.RequireFromString("0.8"), GridUnpricedKWh: decimal.RequireFromString("0.8"), UnpricedKWh: decimal.RequireFromString("0.5")}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := NewRecorder("", .90, time.UTC).RecordTrade(test.trade); err == nil {
				t.Fatal("RecordTrade() error = nil, want invalid allocation rejection")
			}
		})
	}
}

func TestRetiredDischargeWindowsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	windows := []TimeWindow{{
		Start: time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC),
		End:   time.Date(2024, 1, 15, 19, 0, 0, 0, time.UTC),
		Price: decimal.RequireFromString("0.30"),
	}}
	if err := NewRecorder(dir, .90, time.UTC).SaveRetiredDischargeWindows(windows); err != nil {
		t.Fatalf("SaveRetiredDischargeWindows() error = %v", err)
	}
	loaded, err := NewRecorder(dir, .90, time.UTC).LoadRetiredDischargeWindows()
	if err != nil {
		t.Fatalf("LoadRetiredDischargeWindows() error = %v", err)
	}
	if len(loaded) != 1 || !sameWindowPeriod(loaded[0], windows[0]) || !loaded[0].Price.Equal(windows[0].Price) {
		t.Fatalf("loaded retired windows = %+v, want %+v", loaded, windows)
	}
}

func TestAutomaticCycleCommitmentClearRecreatesMissingDataDirectory(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "nested", "data")
	r := NewRecorder(dataDir, .90, time.UTC)
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove data directory: %v", err)
	}

	if err := r.SaveAutomaticCycleCommitment(nil); err != nil {
		t.Fatalf("clear commitment with missing data directory: %v", err)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("recreated data directory info = %+v, error = %v", info, err)
	}
}

func TestAutomaticCycleCommitmentPublishesNewDataDirectoryParents(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "nested", "data")
	r := NewRecorder(dataDir, .90, time.UTC)
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	var synced []string
	r.syncDirectoryFn = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}
	if err := r.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	want := []string{root, filepath.Join(root, "nested"), dataDir}
	if len(synced) != len(want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != filepath.Clean(want[i]) {
			t.Fatalf("synced directories = %v, want %v", synced, want)
		}
	}
}

func TestAutomaticCycleCommitmentRetriesFailedParentDirectorySync(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "nested", "data")
	r := NewRecorder(dataDir, .90, time.UTC)
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}
	var synced []string
	r.syncDirectoryFn = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		if len(synced) == 1 {
			return errors.New("injected parent sync failure")
		}
		return nil
	}

	if err := r.SaveAutomaticCycleCommitment(cycle); err == nil {
		t.Fatal("first save unexpectedly ignored parent-directory sync failure")
	}
	if err := r.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("retry save: %v", err)
	}
	want := []string{root, root, filepath.Join(root, "nested"), dataDir}
	if len(synced) != len(want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != filepath.Clean(want[i]) {
			t.Fatalf("synced directories = %v, want %v", synced, want)
		}
	}
}

func TestLoadTradesPublishesFreshDataDirectoryParents(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "nested", "data")
	r := NewRecorder(dataDir, .90, time.UTC)
	var synced []string
	r.syncDirectoryFn = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}

	if err := r.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}
	want := []string{root, filepath.Join(root, "nested"), dataDir}
	if len(synced) != len(want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != filepath.Clean(want[i]) {
			t.Fatalf("synced directories = %v, want %v", synced, want)
		}
	}
}

func TestAutomaticCycleCommitmentRetriesAfterPartialDirectoryCreation(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "nested", "data")
	r := NewRecorder(dataDir, .90, time.UTC)
	mkdirCalls := 0
	r.mkdirAllFn = func(path string, mode os.FileMode) error {
		mkdirCalls++
		if mkdirCalls == 1 {
			if err := os.MkdirAll(filepath.Dir(path), mode); err != nil {
				return err
			}
			return errors.New("injected partial mkdir failure")
		}
		return os.MkdirAll(path, mode)
	}
	var synced []string
	r.syncDirectoryFn = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}

	if err := r.SaveAutomaticCycleCommitment(cycle); err == nil {
		t.Fatal("first save unexpectedly ignored partial directory creation failure")
	}
	if err := r.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("retry save: %v", err)
	}
	want := []string{root, filepath.Join(root, "nested"), filepath.Join(root, "nested"), dataDir}
	if len(synced) != len(want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != filepath.Clean(want[i]) {
			t.Fatalf("synced directories = %v, want %v", synced, want)
		}
	}
}

func TestAutomaticCycleCommitmentLoadErrorNamesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, automaticCycleCommitmentFile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt commitment: %v", err)
	}

	_, err := NewRecorder(dir, .90, time.UTC).LoadAutomaticCycleCommitment()
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("load error = %v, want path %s", err, path)
	}
}

func TestGetHistory_SolarGridAccountingPersists(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir, 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	if err := r.RecordTrade(Trade{
		Timestamp: ts,
		Action:    ActionCharge,
		PriceEUR:  decimal.NewFromFloat(0.20),
		EnergyKWh: decimal.NewFromFloat(1),
	}); err != nil {
		t.Fatalf("RecordTrade(grid charge) error = %v", err)
	}
	if err := r.RecordTrade(Trade{
		Timestamp:          ts.Add(time.Hour),
		Action:             ActionSolarCharge,
		EnergyKWh:          decimal.NewFromFloat(2),
		GridEnergyKWh:      decimal.NewFromFloat(1),
		GridCostEUR:        decimal.NewFromFloat(-0.10),
		GridUnpricedKWh:    decimal.NewFromFloat(0.5),
		UnpricedKWh:        decimal.NewFromFloat(0.25),
		OpportunityCostEUR: decimal.NewFromFloat(0.15),
		EnergyBasis:        "measured_battery_power",
	}); err != nil {
		t.Fatalf("RecordTrade(mixed solar charge) error = %v", err)
	}
	if err := r.RecordTrade(Trade{
		Timestamp: ts.Add(2 * time.Hour),
		Action:    ActionDischarge,
		PriceEUR:  decimal.NewFromFloat(0.20),
		EnergyKWh: decimal.NewFromFloat(3),
	}); err != nil {
		t.Fatalf("RecordTrade(discharge) error = %v", err)
	}

	loaded := NewRecorder(dir, 0.90, time.UTC)
	if err := loaded.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}

	history := loaded.GetHistory()
	if len(history.Days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(history.Days))
	}
	day := history.Days[0]
	if !day.ChargedKWh.Equal(decimal.NewFromFloat(3)) {
		t.Errorf("ChargedKWh = %s, want 3", day.ChargedKWh)
	}
	if !day.SolarChargedKWh.Equal(decimal.NewFromFloat(1)) {
		t.Errorf("SolarChargedKWh = %s, want 1", day.SolarChargedKWh)
	}
	if !day.GridChargedKWh.Equal(decimal.NewFromFloat(2)) {
		t.Errorf("GridChargedKWh = %s, want 2", day.GridChargedKWh)
	}
	if !day.UnpricedGridKWh.Equal(decimal.NewFromFloat(0.5)) {
		t.Errorf("UnpricedGridKWh = %s, want 0.5", day.UnpricedGridKWh)
	}
	if !day.UnpricedKWh.Equal(decimal.NewFromFloat(0.75)) {
		t.Errorf("UnpricedKWh = %s, want 0.75", day.UnpricedKWh)
	}
	if !day.SolarOpportunityCostEUR.Equal(decimal.NewFromFloat(0.15)) {
		t.Errorf("SolarOpportunityCostEUR = %s, want 0.15", day.SolarOpportunityCostEUR)
	}
	expectedAvgPrice := decimal.NewFromFloat(0.10).Div(decimal.NewFromFloat(1.5))
	if !day.AvgChargePrice.Equal(expectedAvgPrice) {
		t.Errorf("AvgChargePrice = %s, want %s", day.AvgChargePrice, expectedAvgPrice)
	}
	if !day.MinChargePrice.Equal(decimal.NewFromFloat(-0.20)) {
		t.Errorf("MinChargePrice = %s, want -0.2", day.MinChargePrice)
	}
	expectedPnL := decimal.NewFromFloat(0.5)
	if !day.PnLEUR.Equal(expectedPnL) {
		t.Errorf("PnLEUR = %s, want %s", day.PnLEUR, expectedPnL)
	}
	if !history.TotalPnL.Equal(expectedPnL) {
		t.Errorf("TotalPnL = %s, want %s", history.TotalPnL, expectedPnL)
	}
	if !loaded.GetTotalPnL().Equal(expectedPnL) {
		t.Errorf("GetTotalPnL() = %s, want %s", loaded.GetTotalPnL(), expectedPnL)
	}

	solarTrade := day.Trades[1]
	if !solarTrade.GridEnergyKWh.Equal(decimal.NewFromFloat(1)) ||
		!solarTrade.GridCostEUR.Equal(decimal.NewFromFloat(-0.10)) ||
		!solarTrade.GridUnpricedKWh.Equal(decimal.NewFromFloat(0.5)) ||
		!solarTrade.UnpricedKWh.Equal(decimal.NewFromFloat(0.25)) ||
		!solarTrade.OpportunityCostEUR.Equal(decimal.NewFromFloat(0.15)) ||
		solarTrade.EnergyBasis != "measured_battery_power" {
		t.Errorf("persisted solar accounting = %+v", solarTrade)
	}
}

func TestLoadTrades_HistoricalSolarChargeRemainsAllSolar(t *testing.T) {
	dir := t.TempDir()
	legacyTrade := `[
  {
    "timestamp": "2024-01-15T10:00:00Z",
    "action": "solar_charge",
    "price_eur": "0",
    "power_w": 500,
    "duration_s": 3600,
    "energy_kwh": "0.5",
    "start_soc": 50,
    "end_soc": 60
  }
]`
	if err := os.WriteFile(filepath.Join(dir, "trades.json"), []byte(legacyTrade), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	r := NewRecorder(dir, 0.90, time.UTC)
	if err := r.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() error = %v", err)
	}

	day := r.GetHistory().Days[0]
	if !day.ChargedKWh.Equal(decimal.NewFromFloat(0.5)) {
		t.Errorf("ChargedKWh = %s, want 0.5", day.ChargedKWh)
	}
	if !day.SolarChargedKWh.Equal(decimal.NewFromFloat(0.5)) {
		t.Errorf("SolarChargedKWh = %s, want 0.5", day.SolarChargedKWh)
	}
	if !day.GridChargedKWh.IsZero() {
		t.Errorf("GridChargedKWh = %s, want 0", day.GridChargedKWh)
	}
	if !day.UnpricedGridKWh.IsZero() {
		t.Errorf("UnpricedGridKWh = %s, want 0", day.UnpricedGridKWh)
	}
	if !day.UnpricedKWh.IsZero() {
		t.Errorf("UnpricedKWh = %s, want 0", day.UnpricedKWh)
	}
	if !day.SolarOpportunityCostEUR.IsZero() {
		t.Errorf("SolarOpportunityCostEUR = %s, want 0", day.SolarOpportunityCostEUR)
	}
	if trade := day.Trades[0]; !trade.UnpricedKWh.IsZero() || !trade.OpportunityCostEUR.IsZero() || trade.EnergyBasis != "" {
		t.Errorf("legacy trade additions = %+v, want zero values", trade)
	}
}

func TestGetHistory_ExcludesUnpricedEnergyFromCashFlow(t *testing.T) {
	r := NewRecorder("", 0.90, time.UTC)
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	for _, trade := range []Trade{
		{
			Timestamp:   ts,
			Action:      ActionCharge,
			PriceEUR:    decimal.NewFromFloat(0.10),
			EnergyKWh:   decimal.NewFromFloat(2),
			UnpricedKWh: decimal.NewFromFloat(0.5),
			EnergyBasis: "measured_battery_power",
		},
		{
			Timestamp:          ts.Add(time.Hour),
			Action:             ActionSolarCharge,
			EnergyKWh:          decimal.NewFromFloat(1.5),
			GridEnergyKWh:      decimal.NewFromFloat(0.5),
			GridCostEUR:        decimal.NewFromFloat(0.08),
			GridUnpricedKWh:    decimal.NewFromFloat(0.1),
			UnpricedKWh:        decimal.NewFromFloat(0.5),
			OpportunityCostEUR: decimal.NewFromFloat(0.18),
			EnergyBasis:        "measured_battery_power",
		},
		{
			Timestamp:   ts.Add(2 * time.Hour),
			Action:      ActionDischarge,
			PriceEUR:    decimal.NewFromFloat(0.20),
			EnergyKWh:   decimal.NewFromFloat(3),
			UnpricedKWh: decimal.NewFromFloat(1),
			EnergyBasis: "measured_battery_power",
		},
	} {
		if err := r.RecordTrade(trade); err != nil {
			t.Fatalf("RecordTrade() error = %v", err)
		}
	}

	day := r.GetHistory().Days[0]
	if !day.UnpricedKWh.Equal(decimal.NewFromFloat(2.1)) {
		t.Errorf("UnpricedKWh = %s, want 2.1", day.UnpricedKWh)
	}
	if !day.CashFlowUnpricedKWh.Equal(decimal.NewFromFloat(1.6)) {
		t.Errorf("CashFlowUnpricedKWh = %s, want 1.6", day.CashFlowUnpricedKWh)
	}
	if !day.SolarOpportunityCostEUR.Equal(decimal.NewFromFloat(0.18)) {
		t.Errorf("SolarOpportunityCostEUR = %s, want 0.18", day.SolarOpportunityCostEUR)
	}

	// Cash flow excludes unknown scheduled energy and solar opportunity cost.
	expectedCashFlow := decimal.NewFromFloat(0.17)
	if !day.PnLEUR.Equal(expectedCashFlow) {
		t.Errorf("PnLEUR = %s, want %s", day.PnLEUR, expectedCashFlow)
	}
	if !r.GetTotalPnL().Equal(expectedCashFlow) {
		t.Errorf("GetTotalPnL() = %s, want %s", r.GetTotalPnL(), expectedCashFlow)
	}
}

func TestGetHistory_SplitsCrossMidnightTradeByLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRecorder("", 0.90, loc)
	if err := r.RecordTrade(Trade{
		Timestamp:   time.Date(2024, 1, 15, 23, 30, 0, 0, loc),
		Action:      ActionCharge,
		PriceEUR:    decimal.NewFromFloat(0.10),
		DurationS:   3600,
		EnergyKWh:   decimal.NewFromFloat(2),
		UnpricedKWh: decimal.NewFromFloat(0.5),
		EnergyBasis: "measured_battery_power",
	}); err != nil {
		t.Fatalf("RecordTrade() error = %v", err)
	}

	history := r.GetHistory()
	if len(history.Days) != 2 {
		t.Fatalf("len(Days) = %d, want 2", len(history.Days))
	}
	if history.Days[0].Date != "2024-01-16" || history.Days[1].Date != "2024-01-15" {
		t.Fatalf("Dates = [%s %s], want [2024-01-16 2024-01-15]", history.Days[0].Date, history.Days[1].Date)
	}

	for _, day := range history.Days {
		if !day.ChargedKWh.Equal(decimal.NewFromFloat(1)) {
			t.Errorf("%s ChargedKWh = %s, want 1", day.Date, day.ChargedKWh)
		}
		if !day.UnpricedKWh.Equal(decimal.NewFromFloat(0.25)) {
			t.Errorf("%s UnpricedKWh = %s, want 0.25", day.Date, day.UnpricedKWh)
		}
		if !day.PnLEUR.Equal(decimal.NewFromFloat(-0.075)) {
			t.Errorf("%s PnLEUR = %s, want -0.075", day.Date, day.PnLEUR)
		}
		if len(day.Trades) != 1 || day.Trades[0].DurationS != 1800 {
			t.Errorf("%s trade duration = %+v, want one 1800-second fragment", day.Date, day.Trades)
		}
	}
	if history.Days[0].ChargeCycles+history.Days[1].ChargeCycles != 1 {
		t.Errorf("charge cycles = %d, want 1", history.Days[0].ChargeCycles+history.Days[1].ChargeCycles)
	}
	if !history.TotalPnL.Equal(decimal.NewFromFloat(-0.15)) {
		t.Errorf("TotalPnL = %s, want -0.15", history.TotalPnL)
	}
	if !r.GetTotalPnL().Equal(decimal.NewFromFloat(-0.15)) {
		t.Errorf("GetTotalPnL() = %s, want -0.15", r.GetTotalPnL())
	}
}

func TestGetHistoryUsesExactCrossMidnightDayAllocations(t *testing.T) {
	loc := time.UTC
	start := time.Date(2024, 1, 15, 23, 30, 0, 0, loc)
	r := NewRecorder("", .90, loc)
	if err := r.RecordTrade(Trade{
		Timestamp: start,
		Action:    ActionCharge,
		PriceEUR:  decimal.RequireFromString("0.20"),
		DurationS: 3600,
		EnergyKWh: decimal.RequireFromString("2"),
		DayAllocations: []TradeDayAllocation{
			{Timestamp: start, DurationS: 1800, EnergyKWh: decimal.NewFromInt(1), PricedValueEUR: decimal.RequireFromString("0.10")},
			{Timestamp: start.Add(30 * time.Minute), DurationS: 1800, EnergyKWh: decimal.NewFromInt(1), PricedValueEUR: decimal.RequireFromString("0.30")},
		},
	}); err != nil {
		t.Fatalf("RecordTrade() error = %v", err)
	}

	history := r.GetHistory()
	if len(history.Days) != 2 {
		t.Fatalf("history days = %+v", history.Days)
	}
	if !history.Days[0].PnLEUR.Equal(decimal.RequireFromString("-0.30")) ||
		!history.Days[1].PnLEUR.Equal(decimal.RequireFromString("-0.10")) {
		t.Fatalf("daily cash flow = [%s %s], want [-0.30 -0.10]", history.Days[0].PnLEUR, history.Days[1].PnLEUR)
	}
	if !history.Days[0].AvgChargePrice.Equal(decimal.RequireFromString("0.30")) ||
		!history.Days[1].AvgChargePrice.Equal(decimal.RequireFromString("0.10")) {
		t.Fatalf("daily prices = [%s %s], want [0.30 0.10]", history.Days[0].AvgChargePrice, history.Days[1].AvgChargePrice)
	}
}

package service

import (
	"math"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

const floatTolerance = 0.0001

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < floatTolerance
}

// Helper to create prices for testing.
func makePrices(baseTime time.Time, values ...float64) []nordpool.Price {
	prices := make([]nordpool.Price, len(values))
	for i, v := range values {
		prices[i] = nordpool.Price{
			Time:  baseTime.Add(time.Duration(i) * 15 * time.Minute),
			Value: v,
		}
	}
	return prices
}

// Default test config: 5.12 kWh battery, 2500W charge/discharge, 90% efficiency, 11% min SOC
func defaultTestConfig() AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:         0.90,
		MinPriceSpread:     0.05,
		BatteryCapacityKWh: 5.12,
		BatteryMinSOC:      0.11,
		ChargePowerW:       2500,
		DischargePowerW:    2500,
		MaxCyclesPerDay:    2,
	}
}

// Small window config for testing with less data: small battery for 1-slot windows
func smallWindowConfig() AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:         0.90,
		MinPriceSpread:     0.01,
		BatteryCapacityKWh: 0.5,
		BatteryMinSOC:      0.0, // No min SOC for simpler test calculations
		ChargePowerW:       2000,
		DischargePowerW:    2000,
		MaxCyclesPerDay:    2,
	}
}

func TestAnalyzePrices_EmptyInput(t *testing.T) {
	plan := AnalyzePrices(nil, defaultTestConfig())

	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if len(plan.ChargeWindows) != 0 {
		t.Errorf("expected no charge windows, got %d", len(plan.ChargeWindows))
	}
	if len(plan.DischargeWindows) != 0 {
		t.Errorf("expected no discharge windows, got %d", len(plan.DischargeWindows))
	}
	if plan.IsProfitable {
		t.Error("expected not profitable for empty input")
	}
}

func TestAnalyzePrices_MinMaxSpread(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// Create 96 slots (full day) with a clear pattern
	values := make([]float64, 96)
	for i := range values {
		values[i] = 0.10 // baseline
	}
	// Low prices in morning (slots 0-15)
	for i := 0; i < 16; i++ {
		values[i] = 0.05
	}
	// High prices in afternoon (slots 40-55)
	for i := 40; i < 56; i++ {
		values[i] = 0.20
	}

	prices := makePrices(baseTime, values...)
	plan := AnalyzePrices(prices, defaultTestConfig())

	if !floatEqual(plan.MinPrice, 0.05) {
		t.Errorf("expected min price 0.05, got %f", plan.MinPrice)
	}
	if !floatEqual(plan.MaxPrice, 0.20) {
		t.Errorf("expected max price 0.20, got %f", plan.MaxPrice)
	}
	if !floatEqual(plan.Spread, 0.15) {
		t.Errorf("expected spread 0.15, got %f", plan.Spread)
	}
}

func TestAnalyzePrices_FlatPrices_NoCycles(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// All same price - no profitable trade possible
	values := make([]float64, 96)
	for i := range values {
		values[i] = 0.10
	}

	prices := makePrices(baseTime, values...)
	plan := AnalyzePrices(prices, defaultTestConfig())

	if plan.IsProfitable {
		t.Error("expected not profitable for flat prices")
	}
	if len(plan.Cycles) != 0 {
		t.Errorf("expected 0 cycles for flat prices, got %d", len(plan.Cycles))
	}
}

func TestAnalyzePrices_OneCycle(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// Create pattern with one clear charge/discharge opportunity
	values := make([]float64, 96)
	for i := range values {
		values[i] = 0.10 // baseline
	}
	// Low prices early morning (slots 0-15) - good for charging
	for i := 0; i < 16; i++ {
		values[i] = 0.04
	}
	// High prices in afternoon (slots 40-55) - good for discharging
	for i := 40; i < 56; i++ {
		values[i] = 0.20
	}

	cfg := defaultTestConfig()
	cfg.MinPriceSpread = 0.05

	prices := makePrices(baseTime, values...)
	plan := AnalyzePrices(prices, cfg)

	if !plan.IsProfitable {
		t.Error("expected profitable trade")
	}
	if len(plan.Cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(plan.Cycles))
	}

	// Verify charge window comes before discharge window
	cycle := plan.Cycles[0]
	if !cycle.ChargeWindow.End.Before(cycle.DischargeWindow.Start) && !cycle.ChargeWindow.End.Equal(cycle.DischargeWindow.Start) {
		t.Errorf("charge window should end before discharge window starts: charge end=%v, discharge start=%v",
			cycle.ChargeWindow.End, cycle.DischargeWindow.Start)
	}

	// Verify the windows are in the expected time ranges
	if cycle.ChargeWindow.Start.Hour() > 4 {
		t.Errorf("charge window should be in early morning, got start hour %d", cycle.ChargeWindow.Start.Hour())
	}
	if cycle.DischargeWindow.Start.Hour() < 10 {
		t.Errorf("discharge window should be in afternoon, got start hour %d", cycle.DischargeWindow.Start.Hour())
	}
}

func TestAnalyzePrices_TwoCycles(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// Create pattern with two charge/discharge opportunities (morning + evening peaks)
	values := make([]float64, 96)
	for i := range values {
		values[i] = 0.10 // baseline
	}

	// First cycle: Night low (slots 0-15), morning peak (slots 28-43)
	for i := 0; i < 16; i++ {
		values[i] = 0.03
	}
	for i := 28; i < 44; i++ {
		values[i] = 0.18
	}

	// Second cycle: Afternoon low (slots 52-67), evening peak (slots 72-87)
	for i := 52; i < 68; i++ {
		values[i] = 0.04
	}
	for i := 72; i < 88; i++ {
		values[i] = 0.22
	}

	cfg := defaultTestConfig()
	cfg.MinPriceSpread = 0.05

	prices := makePrices(baseTime, values...)
	plan := AnalyzePrices(prices, cfg)

	if !plan.IsProfitable {
		t.Error("expected profitable trade")
	}

	// With 5.12 kWh / 2.5 kW = ~2 hours = 8 slots per window
	// We should get at least 1 cycle, possibly 2 depending on timing
	if len(plan.Cycles) < 1 {
		t.Fatalf("expected at least 1 cycle, got %d", len(plan.Cycles))
	}

	// Verify chronological order for all cycles
	for i, cycle := range plan.Cycles {
		if !cycle.ChargeWindow.End.Before(cycle.DischargeWindow.Start) && !cycle.ChargeWindow.End.Equal(cycle.DischargeWindow.Start) {
			t.Errorf("cycle %d: charge window should end before discharge window starts", i)
		}
		if i > 0 {
			prevCycle := plan.Cycles[i-1]
			if !prevCycle.DischargeWindow.End.Before(cycle.ChargeWindow.Start) && !prevCycle.DischargeWindow.End.Equal(cycle.ChargeWindow.Start) {
				t.Errorf("cycle %d should start after cycle %d ends", i, i-1)
			}
		}
	}
}

func TestAnalyzePrices_DischargeAfterCharge(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// High prices early, low prices late - should NOT trade
	// (can't discharge before you charge)
	values := make([]float64, 96)
	for i := range values {
		values[i] = 0.10
	}
	// High prices at start (slots 0-15)
	for i := 0; i < 16; i++ {
		values[i] = 0.20
	}
	// Low prices at end (slots 80-95)
	for i := 80; i < 96; i++ {
		values[i] = 0.04
	}

	cfg := defaultTestConfig()
	cfg.MinPriceSpread = 0.05

	prices := makePrices(baseTime, values...)
	plan := AnalyzePrices(prices, cfg)

	// The algorithm should still find a cycle if there's ANY profitable window after the charge
	// In this case, the best charge is at the end, but there's no time left to discharge
	// So we should get 0 cycles
	if len(plan.Cycles) > 0 {
		// Verify that any found cycles have discharge AFTER charge
		for i, cycle := range plan.Cycles {
			if !cycle.ChargeWindow.End.Before(cycle.DischargeWindow.Start) && !cycle.ChargeWindow.End.Equal(cycle.DischargeWindow.Start) {
				t.Errorf("cycle %d: discharge must come after charge", i)
			}
		}
	}
}

func TestAnalyzePrices_EfficiencyCheck(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		chargePrice    float64
		dischargePrice float64
		efficiency     float64
		minSpread      float64
		wantProfitable bool
	}{
		{
			name:           "profitable - large spread",
			chargePrice:    0.05,
			dischargePrice: 0.15,
			efficiency:     0.90,
			minSpread:      0.05,
			wantProfitable: true,
		},
		{
			name:           "not profitable - efficiency loss exceeds spread",
			chargePrice:    0.10,
			dischargePrice: 0.11, // breakeven = 0.10/0.90 = 0.111
			efficiency:     0.90,
			minSpread:      0.01,
			wantProfitable: false,
		},
		{
			name:           "not profitable - spread below threshold",
			chargePrice:    0.10,
			dischargePrice: 0.13,
			efficiency:     0.90,
			minSpread:      0.05, // spread is only 0.03
			wantProfitable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a 96-slot day with charge prices early, discharge prices later
			values := make([]float64, 96)
			for i := range values {
				values[i] = 0.10 // baseline between charge and discharge
			}
			// Charge window (slots 0-15)
			for i := 0; i < 16; i++ {
				values[i] = tt.chargePrice
			}
			// Discharge window (slots 40-55)
			for i := 40; i < 56; i++ {
				values[i] = tt.dischargePrice
			}

			prices := makePrices(baseTime, values...)
			cfg := AnalyzerConfig{
				Efficiency:         tt.efficiency,
				MinPriceSpread:     tt.minSpread,
				BatteryCapacityKWh: 5.12,
				BatteryMinSOC:      0.11,
				ChargePowerW:       2500,
				DischargePowerW:    2500,
				MaxCyclesPerDay:    2,
			}
			plan := AnalyzePrices(prices, cfg)

			if plan.IsProfitable != tt.wantProfitable {
				t.Errorf("expected profitable=%v, got %v (cycles=%d)",
					tt.wantProfitable, plan.IsProfitable, len(plan.Cycles))
			}
		})
	}
}

func TestAnalyzePrices_ShortPriceList(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// Only 4 price slots - not enough for a full charge/discharge cycle with default config
	prices := makePrices(baseTime, 0.05, 0.06, 0.15, 0.20)
	plan := AnalyzePrices(prices, defaultTestConfig())

	// Should handle gracefully without panic
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}

	// With 5.12 kWh battery at 2500W, we need 8+ slots, so no cycles should be found
	if plan.IsProfitable {
		t.Error("expected not profitable with only 4 price slots")
	}
}

func TestAnalyzePrices_SmallWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// Test with a smaller window size (1 slot = 15 min)
	// Pattern: low, high, low, high
	prices := makePrices(baseTime, 0.05, 0.20, 0.06, 0.18, 0.04, 0.22)

	cfg := smallWindowConfig()
	plan := AnalyzePrices(prices, cfg)

	// With 1-slot windows, we can find profitable trades
	if !plan.IsProfitable {
		t.Error("expected profitable with small windows")
	}
}

func TestCalculateWindowSize(t *testing.T) {
	tests := []struct {
		name        string
		capacityKWh float64
		powerW      int
		wantSlots   int
	}{
		{
			name:        "5.12 kWh at 2500W",
			capacityKWh: 5.12,
			powerW:      2500,
			wantSlots:   9, // 5.12/2.5 = 2.048 hours * 4 = 8.19 -> ceil = 9
		},
		{
			name:        "5 kWh at 2500W",
			capacityKWh: 5.0,
			powerW:      2500,
			wantSlots:   8, // 5/2.5 = 2 hours * 4 = 8
		},
		{
			name:        "1 kWh at 2000W",
			capacityKWh: 1.0,
			powerW:      2000,
			wantSlots:   2, // 1/2 = 0.5 hours * 4 = 2
		},
		{
			name:        "zero power defaults to 8",
			capacityKWh: 5.0,
			powerW:      0,
			wantSlots:   8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateWindowSize(tt.capacityKWh, tt.powerW)
			if got != tt.wantSlots {
				t.Errorf("calculateWindowSize(%f, %d) = %d, want %d",
					tt.capacityKWh, tt.powerW, got, tt.wantSlots)
			}
		})
	}
}

func TestFindBestWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	// Prices: 0.10, 0.08, 0.05, 0.06, 0.15, 0.20, 0.18, 0.12
	// Indices:  0     1     2     3     4     5     6     7
	prices := makePrices(baseTime, 0.10, 0.08, 0.05, 0.06, 0.15, 0.20, 0.18, 0.12)

	// Find lowest 2-slot window
	startIdx, avg, found := findBestWindow(prices, 0, 2, true)
	if !found {
		t.Fatal("expected to find a window")
	}
	if startIdx != 2 { // slots 2-3 have 0.05 and 0.06, avg = 0.055
		t.Errorf("expected startIdx=2, got %d", startIdx)
	}
	if !floatEqual(avg, 0.055) {
		t.Errorf("expected avg=0.055, got %f", avg)
	}

	// Find highest 2-slot window
	// Slot pairs: (0,1)=0.09, (1,2)=0.065, (2,3)=0.055, (3,4)=0.105, (4,5)=0.175, (5,6)=0.19, (6,7)=0.15
	// Highest is (5,6) = 0.19
	startIdx, avg, found = findBestWindow(prices, 0, 2, false)
	if !found {
		t.Fatal("expected to find a window")
	}
	if startIdx != 5 { // slots 5-6 have 0.20 and 0.18, avg = 0.19
		t.Errorf("expected startIdx=5, got %d", startIdx)
	}
	if !floatEqual(avg, 0.19) {
		t.Errorf("expected avg=0.19, got %f", avg)
	}

	// Find highest after index 5 (only (5,6) and (6,7) are valid)
	// (5,6)=0.19, (6,7)=0.15
	startIdx, avg, found = findBestWindow(prices, 5, 2, false)
	if !found {
		t.Fatal("expected to find a window")
	}
	if startIdx != 5 { // slots 5-6 have 0.20 and 0.18, avg = 0.19
		t.Errorf("expected startIdx=5, got %d", startIdx)
	}
	if !floatEqual(avg, 0.19) {
		t.Errorf("expected avg=0.19, got %f", avg)
	}
}

func TestGetCurrentPrice(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.11, 0.12, 0.13)

	tests := []struct {
		name      string
		queryTime time.Time
		wantPrice float64
		wantOK    bool
	}{
		{
			name:      "exact slot start",
			queryTime: baseTime,
			wantPrice: 0.10,
			wantOK:    true,
		},
		{
			name:      "middle of first slot",
			queryTime: baseTime.Add(7 * time.Minute),
			wantPrice: 0.10,
			wantOK:    true,
		},
		{
			name:      "end of first slot (exclusive)",
			queryTime: baseTime.Add(15 * time.Minute),
			wantPrice: 0.11, // Should be second slot
			wantOK:    true,
		},
		{
			name:      "third slot",
			queryTime: baseTime.Add(35 * time.Minute),
			wantPrice: 0.12,
			wantOK:    true,
		},
		{
			name:      "before all slots",
			queryTime: baseTime.Add(-1 * time.Hour),
			wantPrice: 0,
			wantOK:    false,
		},
		{
			name:      "after all slots",
			queryTime: baseTime.Add(2 * time.Hour),
			wantPrice: 0,
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			price, ok := GetCurrentPrice(prices, tt.queryTime)
			if ok != tt.wantOK {
				t.Errorf("expected ok=%v, got %v", tt.wantOK, ok)
			}
			if price != tt.wantPrice {
				t.Errorf("expected price=%f, got %f", tt.wantPrice, price)
			}
		})
	}
}

func TestTradingPlan_IsInChargeWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 2, 0, 0, 0, time.UTC)
	plan := &TradingPlan{
		ChargeWindows: []TimeWindow{
			{Start: baseTime, End: baseTime.Add(1 * time.Hour)},
			{Start: baseTime.Add(4 * time.Hour), End: baseTime.Add(5 * time.Hour)},
		},
	}

	tests := []struct {
		name string
		time time.Time
		want bool
	}{
		{"before first window", baseTime.Add(-30 * time.Minute), false},
		{"start of first window", baseTime, true},
		{"middle of first window", baseTime.Add(30 * time.Minute), true},
		{"end of first window (exclusive)", baseTime.Add(1 * time.Hour), false},
		{"between windows", baseTime.Add(2 * time.Hour), false},
		{"in second window", baseTime.Add(4*time.Hour + 30*time.Minute), true},
		{"after all windows", baseTime.Add(6 * time.Hour), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plan.IsInChargeWindow(tt.time)
			if got != tt.want {
				t.Errorf("IsInChargeWindow(%v) = %v, want %v", tt.time, got, tt.want)
			}
		})
	}
}

func TestTradingPlan_IsInDischargeWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC)
	plan := &TradingPlan{
		DischargeWindows: []TimeWindow{
			{Start: baseTime, End: baseTime.Add(2 * time.Hour)},
		},
	}

	tests := []struct {
		name string
		time time.Time
		want bool
	}{
		{"before window", baseTime.Add(-1 * time.Hour), false},
		{"start of window", baseTime, true},
		{"middle of window", baseTime.Add(1 * time.Hour), true},
		{"end of window (exclusive)", baseTime.Add(2 * time.Hour), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plan.IsInDischargeWindow(tt.time)
			if got != tt.want {
				t.Errorf("IsInDischargeWindow(%v) = %v, want %v", tt.time, got, tt.want)
			}
		})
	}
}

func TestTradingPlan_ShouldTrade(t *testing.T) {
	tests := []struct {
		name string
		plan *TradingPlan
		want bool
	}{
		{
			name: "profitable with charge windows",
			plan: &TradingPlan{
				IsProfitable:  true,
				ChargeWindows: []TimeWindow{{Start: time.Now()}},
			},
			want: true,
		},
		{
			name: "profitable with discharge windows",
			plan: &TradingPlan{
				IsProfitable:     true,
				DischargeWindows: []TimeWindow{{Start: time.Now()}},
			},
			want: true,
		},
		{
			name: "not profitable",
			plan: &TradingPlan{
				IsProfitable:  false,
				ChargeWindows: []TimeWindow{{Start: time.Now()}},
			},
			want: false,
		},
		{
			name: "profitable but no windows",
			plan: &TradingPlan{
				IsProfitable: true,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.plan.ShouldTrade()
			if got != tt.want {
				t.Errorf("ShouldTrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

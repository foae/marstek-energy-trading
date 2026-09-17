package service

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func inventoryBudgetConfig(now time.Time) AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:         1,
		ChargeEfficiency:   1,
		MinPriceSpread:     .01,
		BatteryCapacityKWh: 1,
		ChargePowerW:       1000,
		DischargePowerW:    1000,
		MaxCyclesPerDay:    1,
		Now:                now,
		InitialSOC:         100,
		InitialSOCKnown:    true,
	}
}

func windowEnergyKWh(window TimeWindow, powerKW float64) float64 {
	return powerKW * window.End.Sub(window.Start).Hours()
}

func TestAnalyzePricesZeroInventoryCapDoesNotAuthorizeSale(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for name, cap := range map[string]float64{
		"zero":     0,
		"negative": -1,
		"NaN":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := inventoryBudgetConfig(base)
			cfg.AvailableInventoryDCKWh = &cap
			plan := AnalyzePrices(plannerPrices(base, []float64{.10, .10}, []float64{.50, .50}), cfg)

			if plan.InventorySale != nil || len(plan.Cycles) != 0 || len(plan.DischargeWindows) != 0 {
				t.Fatalf("untrusted inventory produced executable discharge: %+v", plan)
			}
		})
	}
}

func TestAnalyzePricesFractionalInventoryCapBoundsSaleEnergy(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cfg := inventoryBudgetConfig(base)
	available := .3
	cfg.AvailableInventoryDCKWh = &available
	plan := AnalyzePrices(plannerPrices(base, []float64{.10, .10}, []float64{.50, .50}), cfg)

	if plan.InventorySale == nil || len(plan.Cycles) != 0 {
		t.Fatalf("plan = %+v, want only a trusted-inventory sale", plan)
	}
	if !plan.InventorySale.Start.Equal(base) || !plan.InventorySale.End.Equal(base.Add(18*time.Minute)) {
		t.Fatalf("sale window = %+v, want 12:00-12:18", plan.InventorySale)
	}
	if got := windowEnergyKWh(*plan.InventorySale, 1); math.Abs(got-available) > plannerEnergyEpsilon {
		t.Fatalf("sale energy = %.9f kWh, want trusted %.9f kWh", got, available)
	}
}

func TestAnalyzePricesQuarantinedHighSOCOnlyCyclesChargedHeadroom(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cfg := inventoryBudgetConfig(base)
	cfg.InitialSOC = 75
	available := 0.0
	cfg.AvailableInventoryDCKWh = &available
	plan := AnalyzePrices(plannerPrices(base, []float64{.10, .10}, []float64{.50, .50}), cfg)

	if plan.InventorySale != nil || len(plan.Cycles) != 1 {
		t.Fatalf("plan = %+v, want one charged grid cycle and no inventory sale", plan)
	}
	cycle := plan.Cycles[0]
	if !cycle.ChargeWindow.Start.Equal(base) || !cycle.ChargeWindow.End.Equal(base.Add(15*time.Minute)) ||
		!cycle.DischargeWindow.Start.Equal(base.Add(15*time.Minute)) || !cycle.DischargeWindow.End.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("cycle windows = %+v, want charge 12:00-12:15 then discharge 12:15-12:30", cycle)
	}
	charged := windowEnergyKWh(cycle.ChargeWindow, 1)
	discharged := windowEnergyKWh(cycle.DischargeWindow, 1)
	if math.Abs(charged-.25) > plannerEnergyEpsilon || math.Abs(discharged-charged) > plannerEnergyEpsilon {
		t.Fatalf("charged/discharged energy = %.9f/%.9f kWh, want only .25 kWh headroom", charged, discharged)
	}
	if !cycle.Profit.Equal(decimal.NewFromFloat(.4)) {
		t.Fatalf("cycle profit = %s EUR/kWh, want 0.4 from 0.50 discharge revenue minus 0.10 charge cost", cycle.Profit)
	}
}

func TestAnalyzePricesNilInventoryCapKeepsObservedSOCBehavior(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cfg := inventoryBudgetConfig(base)
	cfg.InitialSOC = 50
	cfg.AvailableInventoryDCKWh = nil
	plan := AnalyzePrices(plannerPrices(base, []float64{.10, .10}, []float64{.50, .50}), cfg)

	if plan.InventorySale == nil || len(plan.Cycles) != 0 {
		t.Fatalf("plan = %+v, want observed SOC inventory sale without a grid cycle", plan)
	}
	if !plan.InventorySale.Start.Equal(base) || !plan.InventorySale.End.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("sale window = %+v, want 12:00-12:30", plan.InventorySale)
	}
	if got := windowEnergyKWh(*plan.InventorySale, 1); math.Abs(got-.5) > plannerEnergyEpsilon {
		t.Fatalf("sale energy = %.9f kWh, want observed .5 kWh", got)
	}
}

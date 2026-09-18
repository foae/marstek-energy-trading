package service

import (
	"math"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/shopspring/decimal"
)

func inventoryBudgetConfig(now time.Time) AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:           1,
		ChargeEfficiency:     1,
		MinPriceSpread:       .01,
		BatteryCapacityKWh:   1,
		ChargePowerW:         1000,
		ChargePlanningDerate: 1,
		DischargePowerW:      1000,
		MaxCyclesPerDay:      1,
		Now:                  now,
		InitialSOC:           100,
		InitialSOCKnown:      true,
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

// minimumGainPrices builds an afternoon where grid charging never pays (import
// is flat and high), so the only value on the table is the inventory sale and
// the plan's gain is exactly the sale revenue.
func minimumGainPrices(base time.Time, firstWindowExport float64) []nordpool.Price {
	imports := make([]float64, 38)
	exports := make([]float64, 38)
	for i := range imports {
		imports[i] = .50
		exports[i] = .05
		if i < 2 {
			exports[i] = firstWindowExport
		}
	}
	return plannerPrices(base, imports, exports)
}

func minimumGainConfig(base time.Time, minGain float64) AnalyzerConfig {
	cfg := inventoryBudgetConfig(base)
	cfg.Efficiency = .79
	cfg.ChargeEfficiency = .95
	cfg.InventorySaleMinGainEUR = minGain
	available := .08
	cfg.AvailableInventoryDCKWh = &available
	return cfg
}

// TestInventorySaleRequiresMinimumGain: 0.08 kWh DC delivers about 0.066 kWh AC,
// worth 0.0146 EUR at 0.22 and 0.0299 EUR at 0.45.
func TestInventorySaleRequiresMinimumGain(t *testing.T) {
	base := time.Date(2026, 9, 17, 11, 30, 0, 0, time.UTC)

	plan := AnalyzePrices(minimumGainPrices(base, .22), minimumGainConfig(base, .02))
	if plan.InventorySale != nil {
		t.Fatalf("sale below the minimum gain was selected anyway: %+v", plan.InventorySale)
	}

	plan = AnalyzePrices(minimumGainPrices(base, .22), minimumGainConfig(base, 0))
	if plan.InventorySale == nil {
		t.Fatalf("plan = %+v, want the marginal sale without a minimum gain", plan)
	}

	plan = AnalyzePrices(minimumGainPrices(base, .45), minimumGainConfig(base, .02))
	if plan.InventorySale == nil {
		t.Fatalf("plan = %+v, want a sale that clears the minimum gain", plan)
	}
}

// observedDayHalfHourPrices is the all-in import series observed on
// 2026-09-18 (Europe/Amsterdam), one value per half hour from 00:00.
var observedDayHalfHourPrices = []float64{
	.3163, .2973, .2986, .2888, .2878, .2955, .2895, .2921, .2892, .2916, .3025, .3153,
	.3459, .3662, .3934, .404, .409, .3837, .3859, .3123, .2864, .2319, .2166, .1748,
	.1468, .134, .1314, .1309, .1308, .1308, .1311, .1357, .1368, .1873, .1695, .2762,
	.3033, .3711, .3649, .3708, .3808, .3702, .3647, .3489, .3478, .3373, .3287, .2949,
}

func observedDayPrices(t *testing.T) ([]nordpool.Price, time.Time) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	midnight := time.Date(2026, 9, 18, 0, 0, 0, 0, loc)
	values := make([]float64, 0, 2*len(observedDayHalfHourPrices))
	for _, value := range observedDayHalfHourPrices {
		values = append(values, value, value)
	}
	return plannerPrices(midnight, values, values), midnight
}

func observedDayConfig(now time.Time, inventoryDCKWh, minGain float64, soc int) AnalyzerConfig {
	available := inventoryDCKWh
	return AnalyzerConfig{
		Efficiency:              .79,
		ChargeEfficiency:        .95,
		MinPriceSpread:          .05,
		BatteryCapacityKWh:      5.12,
		BatteryMinSOC:           .11,
		ChargePowerW:            2200,
		DischargePowerW:         2200,
		MaxCyclesPerDay:         2,
		ChargePlanningDerate:    .9,
		Now:                     now,
		InitialSOC:              soc,
		InitialSOCKnown:         true,
		AvailableInventoryDCKWh: &available,
		InventorySaleMinGainEUR: minGain,
	}
}

// TestInventorySaleMinimumGainBlocksSliverSaleOnObservedDay reproduces the
// 2026-09-18 midday sliver: 17% SOC worth about a cent of gain was sold into a
// mediocre window instead of being kept for the evening cycle.
func TestInventorySaleMinimumGainBlocksSliverSaleOnObservedDay(t *testing.T) {
	prices, midnight := observedDayPrices(t)
	retired := []TimeWindow{{Start: midnight.Add(11 * time.Hour), End: midnight.Add(11*time.Hour + 22*time.Minute)}}

	at1134 := midnight.Add(11*time.Hour + 34*time.Minute)
	cfg := observedDayConfig(at1134, .25, 0, 17)
	cfg.RetiredDischargeWindows = retired
	plan := AnalyzePrices(prices, cfg)
	if plan.InventorySale == nil || !plan.InventorySale.Start.Equal(at1134) {
		t.Fatalf("sale = %+v, want the observed sliver starting 11:34", plan.InventorySale)
	}

	cfg = observedDayConfig(at1134, .25, .02, 17)
	cfg.RetiredDischargeWindows = retired
	plan = AnalyzePrices(prices, cfg)
	if plan.InventorySale != nil {
		t.Fatalf("sliver sale survived the minimum gain: %+v", plan.InventorySale)
	}
	if len(plan.Cycles) != 1 {
		t.Fatalf("plan = %+v, want the single grid cycle retained", plan.Cycles)
	}

	at1104 := midnight.Add(11*time.Hour + 4*time.Minute)
	cfg = observedDayConfig(at1104, 1, .02, 32)
	plan = AnalyzePrices(prices, cfg)
	if plan.InventorySale == nil || !plan.InventorySale.Start.Equal(at1104) {
		t.Fatalf("sale = %+v, want the worthwhile morning sale starting 11:04", plan.InventorySale)
	}
}

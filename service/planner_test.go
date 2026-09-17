package service

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/shopspring/decimal"
)

func plannerPrices(start time.Time, imports, exports []float64) []nordpool.Price {
	prices := make([]nordpool.Price, len(imports))
	for i := range imports {
		prices[i] = nordpool.Price{
			Time:           start.Add(time.Duration(i) * 15 * time.Minute),
			Value:          imports[i],
			HasExportValue: true,
			ExportValue:    exports[i],
		}
	}
	return prices
}

func plannerConfig(now time.Time) AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:         1,
		ChargeEfficiency:   1,
		MinPriceSpread:     .01,
		BatteryCapacityKWh: 1,
		ChargePowerW:       4000,
		DischargePowerW:    4000,
		MaxCyclesPerDay:    1,
		Now:                now,
	}
}

func TestCheapestChargeAllocationKeepsNonContiguousSlicesAndPartialEndpoint(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	allocation := allocateCheapestChargeSlices([]TimeWindow{
		{Start: base, End: base.Add(15 * time.Minute), Price: decimal.NewFromFloat(.10)},
		{Start: base.Add(15 * time.Minute), End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.90)},
		{Start: base.Add(30 * time.Minute), End: base.Add(45 * time.Minute), Price: decimal.NewFromFloat(.20)},
	}, .30, 1)
	if !allocation.Feasible || allocation.Count != 2 {
		t.Fatalf("allocation = %+v, want two cheapest slices", allocation)
	}
	if !allocation.Windows[0].Start.Equal(base) || !allocation.Windows[1].Start.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("selected slices = %+v, want non-contiguous 0 and 2", allocation.Windows[:allocation.Count])
	}
	if got := allocation.Windows[1].End.Sub(allocation.Windows[1].Start); got != 3*time.Minute {
		t.Fatalf("partial final slice duration = %s, want 3m", got)
	}
}

func TestCheapestChargeAllocationSkipsUnstartableSlice(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	allocation := allocateCheapestChargeSlices([]TimeWindow{
		{Start: base, End: base.Add(minimumAutomaticControlWindow), Price: decimal.NewFromFloat(.05)},
		{Start: base.Add(minimumAutomaticControlWindow), End: base.Add(16 * time.Minute), Price: decimal.NewFromFloat(.10)},
		{Start: base.Add(16 * time.Minute), End: base.Add(31 * time.Minute), Price: decimal.NewFromFloat(.20)},
	}, .30, 1)
	if !allocation.Feasible || allocation.Count != 2 {
		t.Fatalf("allocation = %+v, want two executable slices", allocation)
	}
	if allocation.Windows[0].Start.Equal(base) {
		t.Fatalf("selected unstartable one-minute slice: %+v", allocation.Windows[:allocation.Count])
	}
	if got := allocation.Windows[1].End.Sub(allocation.Windows[1].Start); got != 3*time.Minute {
		t.Fatalf("partial final slice duration = %s, want 3m", got)
	}
}

func TestCheapestChargeAllocationRedistributesShortTail(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	requiredKWh := .51
	allocation := allocateCheapestChargeSlices([]TimeWindow{
		{Start: base, End: base.Add(15 * time.Minute), Price: decimal.NewFromFloat(.10)},
		{Start: base.Add(15 * time.Minute), End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.20)},
	}, requiredKWh, 2)
	if !allocation.Feasible || allocation.Count != 2 {
		t.Fatalf("allocation = %+v, want two executable slices", allocation)
	}
	if got := allocation.Windows[0].End.Sub(allocation.Windows[0].Start); got != 14*time.Minute+17*time.Second {
		t.Fatalf("prefix duration = %s, want 14m17s", got)
	}
	if got := allocation.Windows[1].End.Sub(allocation.Windows[1].Start); got != minimumAutomaticControlWindow+time.Second {
		t.Fatalf("tail duration = %s, want %s", got, minimumAutomaticControlWindow+time.Second)
	}
	if allocation.ReservedKWh != requiredKWh {
		t.Fatalf("reserved energy = %.17g, want %.17g", allocation.ReservedKWh, requiredKWh)
	}
	expectedCost := decimal.NewFromFloat(857.0 / 1800).Mul(decimal.NewFromFloat(.10)).
		Add(decimal.NewFromFloat(61.0 / 1800).Mul(decimal.NewFromFloat(.20)))
	if !allocation.Cost.Equal(expectedCost) {
		t.Fatalf("cost = %s, want %s", allocation.Cost, expectedCost)
	}
}

func TestCheapestChargeAllocationRejectsTinyTotal(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	allocation := allocateCheapestChargeSlices([]TimeWindow{
		{Start: base, End: base.Add(15 * time.Minute), Price: decimal.NewFromFloat(.10)},
		{Start: base.Add(15 * time.Minute), End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.20)},
	}, .01, 2)
	if allocation.Feasible {
		t.Fatalf("allocation = %+v, want infeasible tiny total", allocation)
	}
}

func TestGridValueChargesOnlyObservedInventoryShortfall(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cfg := plannerConfig(base)
	slots := []priceSlot{
		{Time: base, Value: decimal.NewFromFloat(.10), Export: decimal.NewFromFloat(.10)},
		{Time: base.Add(15 * time.Minute), Value: decimal.NewFromFloat(.10), Export: decimal.NewFromFloat(.50)},
	}
	value := newGridPlanner(slots, cfg).best(base, .5, 1)
	if len(value.cycles) != 1 || !value.value.Equal(decimal.NewFromFloat(.45)) {
		t.Fatalf("partial-inventory grid value = %+v, want one 0.45 EUR cycle", value)
	}
}

func TestAnalyzePricesDoesNotDoubleCountObservedInventory(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cfg := plannerConfig(base)
	cfg.InitialSOC, cfg.InitialSOCKnown = 50, true
	plan := AnalyzePrices(plannerPrices(base, []float64{.05, .05, .05, .05}, []float64{.10, .70, .10, .80}), cfg)
	if plan.InventorySale == nil {
		t.Fatalf("expected observed inventory sale, got %+v", plan)
	}
	for _, cycle := range plan.Cycles {
		if plan.InventorySale.Start.Before(cycle.DischargeWindow.End) && cycle.DischargeWindow.Start.Before(plan.InventorySale.End) {
			t.Fatalf("inventory sale overlaps grid discharge: sale=%+v cycle=%+v", plan.InventorySale, cycle)
		}
	}
	seen := 0
	for _, window := range plan.DischargeWindows {
		if window.Start.Equal(plan.InventorySale.Start) && window.End.Equal(plan.InventorySale.End) {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("inventory sale appeared %d times in discharge windows", seen)
	}
}

func TestAnalyzePricesInventorySaleAllowsPartialEndpoint(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cfg := plannerConfig(base)
	cfg.ChargePowerW, cfg.DischargePowerW = 1000, 1000
	cfg.InitialSOC, cfg.InitialSOCKnown = 38, true
	plan := AnalyzePrices(plannerPrices(base, []float64{.10, .10, .10}, []float64{.50, .50, .50}), cfg)
	if plan.InventorySale == nil {
		t.Fatalf("expected partial observed-inventory sale, got %+v", plan)
	}
	if got := plan.InventorySale.End.Sub(plan.InventorySale.Start); got != 22*time.Minute+48*time.Second {
		t.Fatalf("sale duration = %s, want 22m48s", got)
	}
	if !plan.EarliestChargeStart.Equal(plan.InventorySale.End) {
		t.Fatalf("EarliestChargeStart = %s, want inventory end %s", plan.EarliestChargeStart, plan.InventorySale.End)
	}
}

func TestPartialInventorySaleChecksOnlyItsActualRetirementOverlap(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cfg := plannerConfig(base)
	cfg.DischargePowerW = 1000
	cfg.InitialSOC, cfg.InitialSOCKnown = 10, true
	cfg.RetiredDischargeWindows = []TimeWindow{{Start: base.Add(6 * time.Minute), End: base.Add(15 * time.Minute)}}
	prices := plannerPrices(base, []float64{.50}, []float64{.50})
	plan := AnalyzePrices(prices, cfg)
	if plan.InventorySale == nil || !plan.InventorySale.End.Equal(base.Add(6*time.Minute)) {
		t.Fatalf("non-overlapping partial sale excluded by unused tariff tail: %+v", plan)
	}
	cfg.RetiredDischargeWindows[0].Start = base.Add(5 * time.Minute)
	if plan := AnalyzePrices(prices, cfg); plan.InventorySale == nil || !plan.InventorySale.End.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("sale did not stop at the interior retirement boundary: %+v", plan.InventorySale)
	}
	cfg.RetiredDischargeWindows[0] = TimeWindow{Start: base, End: base.Add(5 * time.Minute)}
	if plan := AnalyzePrices(prices, cfg); plan.InventorySale == nil ||
		!plan.InventorySale.Start.Equal(base.Add(5*time.Minute)) ||
		!plan.InventorySale.End.Equal(base.Add(11*time.Minute)) {
		t.Fatalf("sale after interior retirement end = %+v, want 00:05-00:11", plan.InventorySale)
	}
}

func TestInventorySaleUsesActualDSTHorizon(t *testing.T) {
	location, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 29, 1, 45, 0, 0, location)
	cfg := plannerConfig(base)
	cfg.InitialSOC, cfg.InitialSOCKnown = 100, true
	prices := plannerPrices(base, []float64{.10, .10, .10, .10}, []float64{.30, .30, .30, .30})
	plan := AnalyzePrices(prices, cfg)
	if plan.InventorySale == nil {
		t.Fatalf("DST horizon lost an otherwise executable inventory sale: %+v", plan)
	}
	if got := plan.InventorySale.End.Sub(plan.InventorySale.Start); math.Abs(got.Hours()-.25) > .000001 {
		t.Fatalf("sale duration = %s, want real 15-minute duration", got)
	}
}

func TestRetirementOverlapExcludesOnlyActualOverlap(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	retired := []TimeWindow{{Start: base.Add(15 * time.Minute), End: base.Add(30 * time.Minute)}}
	if overlapsRetired(base, base.Add(15*time.Minute), retired) {
		t.Fatal("window ending at retirement boundary must remain eligible")
	}
	if !overlapsRetired(base.Add(14*time.Minute), base.Add(16*time.Minute), retired) {
		t.Fatal("window crossing retirement boundary must be excluded")
	}
}

func TestInventorySaleRefillsAtInteriorOptimum(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cfg := plannerConfig(base)
	cfg.InitialSOC, cfg.InitialSOCKnown = 100, true
	prices := plannerPrices(base, []float64{.05, .05}, []float64{.30, .80})
	plan := AnalyzePrices(prices, cfg)
	if plan.InventorySale == nil || len(plan.Cycles) != 1 {
		t.Fatalf("want partial inventory sale followed by refill and full sale: %+v", plan)
	}
	// Sell 0.5 kWh at .30, refill it at .05, then sell 1 kWh at .80.
	// Tariff-only/full-exhaustion endpoints get .80 instead of .925.
	endpoint := base.Add(7*time.Minute + 30*time.Second)
	if delta := plan.InventorySale.End.Sub(endpoint); delta < -time.Nanosecond || delta > time.Millisecond {
		t.Fatalf("sale endpoint=%s, want 7m30s (within allocator energy epsilon)", plan.InventorySale.End)
	}
	p := newGridPlanner([]priceSlot{
		{Time: base, Value: decimal.NewFromFloat(.05), Export: decimal.NewFromFloat(.30)},
		{Time: base.Add(15 * time.Minute), Value: decimal.NewFromFloat(.05), Export: decimal.NewFromFloat(.80)},
	}, cfg)
	best, _ := p.bestWithInventory(1, 1)
	if best.value.LessThan(decimal.NewFromFloat(.925)) || best.value.GreaterThan(decimal.NewFromFloat(.925+plannerEnergyEpsilon)) {
		t.Fatalf("combined value=%s, want .925 EUR within physical energy epsilon", best.value)
	}
}

// The oracle scans executable times independently of the breakpoint equations.
// It is a lower bound, not an exact optimum: the analytical plan may be better.
func TestInventoryPlanDominatesDenseExecutableEndpoints(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for trial := range 256 {
		cfg := plannerConfig(base)
		cfg.InitialSOC = 1 + rng.IntN(100)
		cfg.Now = base.Add(time.Duration(rng.IntN(900)) * time.Second)
		cfg.InitialSOCKnown = true
		cfg.ChargePowerW = 1000 * (1 + rng.IntN(4))
		cfg.DischargePowerW = 1000 * (1 + rng.IntN(4))
		cfg.Efficiency = .8 + .1*float64(rng.IntN(3))
		cfg.ChargeEfficiency = .9 + .1*float64(rng.IntN(2))
		imports, exports := make([]float64, 5), make([]float64, 5)
		for i := range imports {
			imports[i] = float64(rng.IntN(9)-2) / 10
			exports[i] = float64(rng.IntN(10)) / 10
		}
		slots := make([]priceSlot, len(imports))
		for i := range slots {
			slots[i] = priceSlot{Time: base.Add(time.Duration(i) * 15 * time.Minute), Value: decimal.NewFromFloat(imports[i]), Export: decimal.NewFromFloat(exports[i])}
		}
		p := newGridPlanner(slots, cfg)
		dc := float64(cfg.InitialSOC) / 100
		best, selected := p.bestWithInventory(dc, 1)
		oracle := p.best(p.planningStart, dc, 1).value
		ed := p.roundTrip / p.chargeEfficiency
		for _, sale := range inventorySaleCandidates(slots, cfg, dc*ed, p.minimumSaleKWh, p.dischargePowerKW) {
			idx := sort.Search(len(slots), func(i int) bool { return !slots[i].Time.Before(sale.window.End) }) - 1
			a := slots[idx].Time
			if a.Before(sale.window.Start) {
				a = sale.window.Start
			}
			for at := a; !at.After(sale.window.End); at = at.Add(5 * time.Second) {
				if at.Sub(sale.window.Start) <= minimumAutomaticControlWindow {
					continue
				}
				energy := p.dischargePowerKW * at.Sub(sale.window.Start).Hours()
				if energy < p.minimumSaleKWh {
					continue // the planner refuses sliver endpoints by design
				}
				revenue := sale.revenue.Sub(slots[idx].Export.Mul(decimal.NewFromFloat(p.dischargePowerKW * sale.window.End.Sub(at).Hours())))
				v := revenue.Add(p.best(at, math.Max(0, dc-energy/ed), 1).value)
				if v.GreaterThan(oracle) {
					oracle = v
				}
			}
		}
		if oracle.Sub(best.value).GreaterThan(decimal.NewFromFloat(1e-9)) {
			t.Fatalf("trial=%d got=%s oracle=%s sale=%+v cfg=%+v imports=%v exports=%v", trial, best.value, oracle, selected, cfg, imports, exports)
		}
	}
}

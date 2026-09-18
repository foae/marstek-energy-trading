package service

import (
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

// localMidnight returns midnight in the time's local timezone.
// Unlike Truncate(24h) which truncates to UTC midnight, this preserves the local date.
func localMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// priceSlot is an internal type used by the analyzer. Prices are converted from
// nordpool.Price (float64 API boundary) to decimal.Decimal for precise arithmetic.
type priceSlot struct {
	Time   time.Time
	Value  decimal.Decimal // all-in import price
	Export decimal.Decimal // export credit
}

// TimeWindow represents a time window for charging or discharging.
type TimeWindow struct {
	Start time.Time       `json:"start"`
	End   time.Time       `json:"end"`
	Price decimal.Decimal `json:"price"` // energy-weighted average price
}

// TradeCycle represents a paired grid charge and discharge window. The durable
// shape is retained for existing commitments; Cycles never contains inventory sales.
type TradeCycle struct {
	ChargeWindow    TimeWindow
	DischargeWindow TimeWindow
	Profit          decimal.Decimal // Expected profit per charged AC kWh
	// ExportPriceMode records the export tariff mode the persisted windows were
	// priced under. Empty means symmetric, so pre-existing commitments written
	// before the mode existed restore unchanged.
	ExportPriceMode string `json:"export_price_mode,omitempty"`
}

// TradingPlan contains executable grid cycles plus, when observed inventory has
// positive known export value, one uncommitted contiguous inventory sale.
type TradingPlan struct {
	Date                time.Time
	ChargeWindows       []TimeWindow
	DischargeWindows    []TimeWindow
	Cycles              []TradeCycle // Grid-only paired charge/discharge windows
	InventorySale       *TimeWindow  // Observed stored energy; never a fake TradeCycle
	EarliestChargeStart time.Time    // Reservations must not purchase before this time
	MinPrice            decimal.Decimal
	MaxPrice            decimal.Decimal
	Spread              decimal.Decimal // MaxPrice - MinPrice
	IsProfitable        bool            // At least one grid cycle or inventory sale exists
	DischargeOnly       bool            // Restored commitment may discharge but cannot resume grid charging
}

// AnalyzerConfig contains parameters for price analysis.
type AnalyzerConfig struct {
	Efficiency              float64      // Battery round-trip efficiency (0.0-1.0)
	ChargeEfficiency        float64      // Charging efficiency; values outside (0, 1] are treated as 1.0
	MinPriceSpread          float64      // Minimum expected profit in EUR/kWh after efficiency loss
	BatteryCapacityKWh      float64      // Battery capacity in kWh
	BatteryMinSOC           float64      // Minimum SOC (0.0-1.0), e.g., 0.11 for 11%
	ChargePowerW            int          // Charge power in watts
	ChargePlanningDerate    float64      // Charge power derate for sizing; values outside (0, 1] leave the nameplate power unchanged
	InventorySaleMinGainEUR float64      // Minimum EUR an inventory sale must add over the no-sale plan
	DischargePowerW         int          // Discharge power in watts
	MaxCyclesPerDay         int          // Maximum grid charge/discharge cycles per day
	Now                     time.Time    // Current time; zero retains static-horizon analysis
	RetiredDischargeWindows []TimeWindow // Completed discharge windows excluded from new cycles
	InitialSOC              int          // Fresh observed SOC; meaningful only when InitialSOCKnown
	InitialSOCKnown         bool         // Explicitly distinguishes no observation from empty inventory
	AvailableInventoryDCKWh *float64     // Trusted measured DC inventory; non-nil quarantines other observed energy
}

// AnalyzePrices selects executable chronological grid cycles by total EUR. A
// fresh SOC observation is consumed at most once: it either offsets the first
// grid purchase or is sold in one contiguous inventory-only window before any
// following grid reservation. Static callers without an observation retain
// grid-only analysis and never authorize an inventory sale.
func AnalyzePrices(prices []nordpool.Price, cfg AnalyzerConfig) *TradingPlan {
	if len(prices) == 0 {
		return &TradingPlan{EarliestChargeStart: cfg.Now}
	}

	sorted := append([]nordpool.Price(nil), prices...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	slots := make([]priceSlot, 0, len(sorted))
	for _, price := range sorted {
		if len(slots) > 0 && slots[len(slots)-1].Time.Equal(price.Time) {
			continue
		}
		slots = append(slots, priceSlot{
			Time:   price.Time,
			Value:  decimal.NewFromFloat(price.Value),
			Export: decimal.NewFromFloat(price.Export()),
		})
	}

	minPrice, maxPrice := slots[0].Value, slots[0].Value
	for _, slot := range slots[1:] {
		if slot.Value.LessThan(minPrice) {
			minPrice = slot.Value
		}
		if slot.Value.GreaterThan(maxPrice) {
			maxPrice = slot.Value
		}
	}
	plan := &TradingPlan{
		Date:                localMidnight(slots[0].Time),
		EarliestChargeStart: cfg.Now,
		MinPrice:            minPrice,
		MaxPrice:            maxPrice,
		Spread:              maxPrice.Sub(minPrice),
	}

	planner := newGridPlanner(slots, cfg)
	maxCycles := cfg.MaxCyclesPerDay
	if maxCycles <= 0 {
		maxCycles = 2
	}

	best, inventorySale := planner.bestWithInventory(planner.initialTrustedDCKWh, maxCycles)

	plan.Cycles = best.cycles
	for _, cycle := range plan.Cycles {
		plan.ChargeWindows = append(plan.ChargeWindows, cycle.ChargeWindow)
		plan.DischargeWindows = append(plan.DischargeWindows, cycle.DischargeWindow)
	}
	if inventorySale != nil {
		plan.InventorySale = inventorySale
		plan.EarliestChargeStart = inventorySale.End
		plan.DischargeWindows = append([]TimeWindow{*inventorySale}, plan.DischargeWindows...)
	}
	plan.IsProfitable = len(plan.Cycles) > 0 || plan.InventorySale != nil
	return plan
}

func physicalUsableDCKWh(cfg AnalyzerConfig) float64 {
	usable := cfg.BatteryCapacityKWh * (1 - cfg.BatteryMinSOC)
	if usable < 0 {
		return 0
	}
	return usable
}

func inventoryBudgetDCKWh(cfg AnalyzerConfig, physicalUsableDCKWh float64) (trusted, quarantined float64) {
	observed := observedUsableDCKWh(cfg, physicalUsableDCKWh)
	if cfg.AvailableInventoryDCKWh == nil {
		return observed, 0
	}
	if !cfg.InitialSOCKnown {
		return 0, 0
	}
	available := *cfg.AvailableInventoryDCKWh
	if math.IsNaN(available) || math.IsInf(available, 0) || available < 0 {
		available = 0
	}
	trusted = math.Min(observed, available)
	return trusted, observed - trusted
}

func observedUsableDCKWh(cfg AnalyzerConfig, capacity float64) float64 {
	if !cfg.InitialSOCKnown || capacity <= 0 {
		return 0
	}
	soc := min(100, max(0, cfg.InitialSOC))
	minSOC := cfg.BatteryMinSOC * 100
	if float64(soc) <= minSOC {
		return 0
	}
	value := cfg.BatteryCapacityKWh * (float64(soc)/100 - cfg.BatteryMinSOC)
	return math.Min(capacity, math.Max(0, value))
}

func betterInventoryAlternative(candidate valuePlan, sale TimeWindow, current valuePlan, currentSale *TimeWindow, minGain decimal.Decimal) bool {
	// The first sale must clear a minimum gain over the no-sale plan; refinements
	// of an already selected sale are compared on value alone.
	if currentSale == nil && minGain.IsPositive() {
		return candidate.value.GreaterThanOrEqual(current.value.Add(minGain))
	}
	if candidate.value.GreaterThan(current.value) {
		return true
	}
	if !candidate.value.Equal(current.value) {
		return false
	}
	if candidate.sessions != current.sessions {
		return candidate.sessions < current.sessions
	}
	currentStart := time.Time{}
	if currentSale != nil {
		currentStart = currentSale.Start
	} else if len(current.cycles) > 0 {
		currentStart = current.cycles[0].ChargeWindow.Start
	}
	return currentStart.IsZero() || sale.Start.Before(currentStart)
}

// GetCurrentPrice returns the price for the current time slot.
func GetCurrentPrice(prices []nordpool.Price, t time.Time) (decimal.Decimal, bool) {
	for _, price := range prices {
		slotEnd := price.Time.Add(15 * time.Minute)
		if !t.Before(price.Time) && t.Before(slotEnd) {
			return decimal.NewFromFloat(price.Value), true
		}
	}
	return decimal.Zero, false
}

// GetCurrentExportPrice returns the export price for the current time slot.
func GetCurrentExportPrice(prices []nordpool.Price, t time.Time) (decimal.Decimal, bool) {
	for _, price := range prices {
		slotEnd := price.Time.Add(15 * time.Minute)
		if !t.Before(price.Time) && t.Before(slotEnd) {
			return decimal.NewFromFloat(price.Export()), true
		}
	}
	return decimal.Zero, false
}

// IsInChargeWindow checks if the given time is within a charge window.
func (p *TradingPlan) IsInChargeWindow(t time.Time) bool {
	for _, window := range p.ChargeWindows {
		if !t.Before(window.Start) && t.Before(window.End) {
			return true
		}
	}
	return false
}

// IsInDischargeWindow checks if the given time is within a discharge window.
func (p *TradingPlan) IsInDischargeWindow(t time.Time) bool {
	for _, window := range p.DischargeWindows {
		if !t.Before(window.Start) && t.Before(window.End) {
			return true
		}
	}
	return false
}

// ShouldTrade returns true if trading should occur at the given time.
func (p *TradingPlan) ShouldTrade() bool {
	return p.IsProfitable && (len(p.ChargeWindows) > 0 || len(p.DischargeWindows) > 0)
}

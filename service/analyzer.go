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
	Time  time.Time
	Value decimal.Decimal
}

type averagedWindow struct {
	price decimal.Decimal
	valid bool
}

type cycleChoice struct {
	cycle    TradeCycle
	next     int
	selected bool
}

// TimeWindow represents a time window for charging or discharging.
type TimeWindow struct {
	Start time.Time       `json:"start"`
	End   time.Time       `json:"end"`
	Price decimal.Decimal `json:"price"` // Average price in this window
}

// TradeCycle represents a paired charge and discharge window.
type TradeCycle struct {
	ChargeWindow    TimeWindow
	DischargeWindow TimeWindow
	Profit          decimal.Decimal // Expected profit per kWh (accounting for efficiency)
}

// TradingPlan contains the charge and discharge windows for a day.
type TradingPlan struct {
	Date             time.Time
	ChargeWindows    []TimeWindow
	DischargeWindows []TimeWindow
	Cycles           []TradeCycle // Paired charge/discharge windows
	MinPrice         decimal.Decimal
	MaxPrice         decimal.Decimal
	Spread           decimal.Decimal // MaxPrice - MinPrice
	IsProfitable     bool            // At least one profitable cycle exists
	DischargeOnly    bool            // Restored commitment may discharge but cannot resume grid charging
	recoveryCycle    *TradeCycle     // Historical pairing for stored energy; never consumes the new-cycle cap
}

// AnalyzerConfig contains parameters for price analysis.
type AnalyzerConfig struct {
	Efficiency              float64      // Battery round-trip efficiency (0.0-1.0)
	MinPriceSpread          float64      // Minimum expected profit in EUR/kWh after efficiency loss
	BatteryCapacityKWh      float64      // Battery capacity in kWh
	BatteryMinSOC           float64      // Minimum SOC (0.0-1.0), e.g., 0.11 for 11%
	ChargePowerW            int          // Charge power in watts
	DischargePowerW         int          // Discharge power in watts
	MaxCyclesPerDay         int          // Maximum charge/discharge cycles per day
	Now                     time.Time    // Current time; zero retains static-horizon analysis
	RetiredDischargeWindows []TimeWindow // Completed discharge windows excluded from new cycles
}

// AnalyzePrices analyzes the day-ahead prices and returns a trading plan.
// It selects the globally most profitable chronological charge/discharge cycles.
func AnalyzePrices(prices []nordpool.Price, cfg AnalyzerConfig) *TradingPlan {
	if len(prices) == 0 {
		return &TradingPlan{}
	}

	// Sort prices by time
	sorted := make([]nordpool.Price, len(prices))
	copy(sorted, prices)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})

	// Convert API float64 prices to decimal for precise monetary arithmetic
	slots := make([]priceSlot, len(sorted))
	for i, p := range sorted {
		slots[i] = priceSlot{Time: p.Time, Value: decimal.NewFromFloat(p.Value)}
	}

	// Find min and max prices
	minPrice := slots[0].Value
	maxPrice := slots[0].Value
	for _, s := range slots {
		if s.Value.LessThan(minPrice) {
			minPrice = s.Value
		}
		if s.Value.GreaterThan(maxPrice) {
			maxPrice = s.Value
		}
	}

	spread := maxPrice.Sub(minPrice)

	// Calculate usable capacity accounting for min SOC protection
	usableCapacity := cfg.BatteryCapacityKWh * (1.0 - cfg.BatteryMinSOC)

	// Calculate window size based on usable capacity and power
	chargeWindowSize := calculateWindowSize(usableCapacity, cfg.ChargePowerW)
	dischargeWindowSize := calculateWindowSize(usableCapacity, cfg.DischargePowerW)

	// Handle case where we don't have enough data points
	if len(slots) < chargeWindowSize || len(slots) < dischargeWindowSize {
		return &TradingPlan{
			Date:     localMidnight(slots[0].Time),
			MinPrice: minPrice,
			MaxPrice: maxPrice,
			Spread:   spread,
		}
	}

	efficiency := decimal.NewFromFloat(cfg.Efficiency)
	minProfit := decimal.NewFromFloat(cfg.MinPriceSpread)

	// Determine max cycles (default to 2 if not configured).
	maxCycles := cfg.MaxCyclesPerDay
	if maxCycles <= 0 {
		maxCycles = 2
	}
	cutoff := cfg.Now
	if !cutoff.IsZero() {
		cutoff = cutoff.Add(minimumAutomaticControlWindow)
	}

	cycles := selectOptimalCycles(
		slots,
		chargeWindowSize,
		dischargeWindowSize,
		efficiency,
		minProfit,
		maxCycles,
		cutoff,
		cfg.RetiredDischargeWindows,
	)

	// Extract charge and discharge windows from cycles for backwards compatibility
	var chargeWindows, dischargeWindows []TimeWindow
	for _, c := range cycles {
		chargeWindows = append(chargeWindows, c.ChargeWindow)
		dischargeWindows = append(dischargeWindows, c.DischargeWindow)
	}

	return &TradingPlan{
		Date:             localMidnight(slots[0].Time),
		ChargeWindows:    chargeWindows,
		DischargeWindows: dischargeWindows,
		Cycles:           cycles,
		recoveryCycle:    selectRecoveryCycle(slots, chargeWindowSize, dischargeWindowSize, efficiency, minProfit, cfg, cycles),
		MinPrice:         minPrice,
		MaxPrice:         maxPrice,
		Spread:           spread,
		IsProfitable:     len(cycles) > 0,
	}
}

// selectOptimalCycles uses dynamic programming to maximize total profit from up
// to maxCycles chronological, non-overlapping charge/discharge pairs.
func selectOptimalCycles(prices []priceSlot, chargeWindowSize, dischargeWindowSize int, efficiency, minProfit decimal.Decimal, maxCycles int, now time.Time, retiredDischargeWindows []TimeWindow) []TradeCycle {
	maxCycles = min(maxCycles, len(prices)/(chargeWindowSize+dischargeWindowSize))
	if maxCycles == 0 {
		return nil
	}

	chargeAverages := precomputeWindowAverages(prices, chargeWindowSize, now, nil)
	dischargeAverages := precomputeWindowAverages(prices, dischargeWindowSize, now, retiredDischargeWindows)
	profits := make([][]decimal.Decimal, maxCycles+1)
	choices := make([][]cycleChoice, maxCycles+1)
	for cycleCount := range profits {
		profits[cycleCount] = make([]decimal.Decimal, len(prices)+1)
		choices[cycleCount] = make([]cycleChoice, len(prices))
	}

	for cycleCount := 1; cycleCount <= maxCycles; cycleCount++ {
		for chargeStart := len(prices) - 1; chargeStart >= 0; chargeStart-- {
			bestProfit := profits[cycleCount][chargeStart+1]
			if chargeStart+chargeWindowSize <= len(prices) && chargeAverages[chargeStart].valid {
				chargeAverage := chargeAverages[chargeStart].price
				for dischargeStart := chargeStart + chargeWindowSize; dischargeStart+dischargeWindowSize <= len(prices); dischargeStart++ {
					if !dischargeAverages[dischargeStart].valid {
						continue
					}

					dischargeAverage := dischargeAverages[dischargeStart].price
					profit := dischargeAverage.Mul(efficiency).Sub(chargeAverage)
					if !profit.IsPositive() || profit.LessThan(minProfit) {
						continue
					}

					next := dischargeStart + dischargeWindowSize
					totalProfit := profit.Add(profits[cycleCount-1][next])
					if !totalProfit.GreaterThan(bestProfit) {
						continue
					}

					bestProfit = totalProfit
					choices[cycleCount][chargeStart] = cycleChoice{
						cycle: TradeCycle{
							ChargeWindow: TimeWindow{
								Start: prices[chargeStart].Time,
								End:   prices[chargeStart+chargeWindowSize-1].Time.Add(15 * time.Minute),
								Price: chargeAverage,
							},
							DischargeWindow: TimeWindow{
								Start: prices[dischargeStart].Time,
								End:   prices[dischargeStart+dischargeWindowSize-1].Time.Add(15 * time.Minute),
								Price: dischargeAverage,
							},
							Profit: profit,
						},
						next:     next,
						selected: true,
					}
				}
			}
			profits[cycleCount][chargeStart] = bestProfit
		}
	}

	cycles := make([]TradeCycle, 0, maxCycles)
	for chargeStart, cycleCount := 0, maxCycles; chargeStart < len(prices) && cycleCount > 0; {
		choice := choices[cycleCount][chargeStart]
		if !choice.selected {
			chargeStart++
			continue
		}
		cycles = append(cycles, choice.cycle)
		chargeStart = choice.next
		cycleCount--
	}
	return cycles
}

// selectRecoveryCycle preserves a discharge opportunity for energy already in
// the battery. It cannot crowd out new cycles or overlap their charge windows.
func selectRecoveryCycle(prices []priceSlot, chargeSize, dischargeSize int, efficiency, minProfit decimal.Decimal, cfg AnalyzerConfig, cycles []TradeCycle) *TradeCycle {
	if cfg.Now.IsZero() || len(prices) == 0 || !prices[0].Time.Before(cfg.Now) {
		return nil
	}
	var before time.Time
	if len(cycles) > 0 {
		before = cycles[0].ChargeWindow.Start
		if !before.After(cfg.Now.Add(minimumAutomaticControlWindow)) {
			return nil
		}
	}
	charges := precomputeWindowAverages(prices, chargeSize, time.Time{}, nil)
	discharges := precomputeWindowAverages(prices, dischargeSize, cfg.Now.Add(minimumAutomaticControlWindow), cfg.RetiredDischargeWindows)
	var best *TradeCycle
	for chargeStart, charge := range charges {
		if !charge.valid {
			continue
		}
		chargeEnd := prices[chargeStart+chargeSize-1].Time.Add(15 * time.Minute)
		if chargeEnd.After(cfg.Now) {
			break
		}
		for dischargeStart := chargeStart + chargeSize; dischargeStart+dischargeSize <= len(prices); dischargeStart++ {
			discharge := discharges[dischargeStart]
			dischargeEnd := prices[dischargeStart+dischargeSize-1].Time.Add(15 * time.Minute)
			if !before.IsZero() && dischargeEnd.After(before) {
				break
			}
			if !discharge.valid {
				continue
			}
			profit := discharge.price.Mul(efficiency).Sub(charge.price)
			if !profit.IsPositive() || profit.LessThan(minProfit) || (best != nil && !profit.GreaterThan(best.Profit)) {
				continue
			}
			if best == nil {
				best = &TradeCycle{}
			}
			*best = TradeCycle{
				ChargeWindow:    TimeWindow{Start: prices[chargeStart].Time, End: chargeEnd, Price: charge.price},
				DischargeWindow: TimeWindow{Start: prices[dischargeStart].Time, End: dischargeEnd, Price: discharge.price},
				Profit:          profit,
			}
		}
	}
	return best
}

// precomputeWindowAverages calculates each contiguous quarter-hour window once.
// A window is invalid when it spans a missing or duplicate price slot, has ended,
// or matches a retired discharge window.
func precomputeWindowAverages(prices []priceSlot, windowSize int, now time.Time, retiredWindows []TimeWindow) []averagedWindow {
	averages := make([]averagedWindow, len(prices))
	if windowSize <= 0 || windowSize > len(prices) {
		return averages
	}

	sum := decimal.Zero
	invalidTransitions := 0
	for i := range windowSize {
		sum = sum.Add(prices[i].Value)
		if i > 0 && !prices[i-1].Time.Add(15*time.Minute).Equal(prices[i].Time) {
			invalidTransitions++
		}
	}
	divisor := decimal.NewFromInt(int64(windowSize))
	for start := 0; start+windowSize <= len(prices); start++ {
		end := prices[start+windowSize-1].Time.Add(15 * time.Minute)
		if invalidTransitions == 0 && (now.IsZero() || end.After(now)) && !isRetiredWindow(prices[start].Time, end, retiredWindows) {
			averages[start] = averagedWindow{
				price: sum.Div(divisor),
				valid: true,
			}
		}
		if start+windowSize == len(prices) {
			break
		}

		if windowSize > 1 && !prices[start].Time.Add(15*time.Minute).Equal(prices[start+1].Time) {
			invalidTransitions--
		}
		sum = sum.Sub(prices[start].Value).Add(prices[start+windowSize].Value)
		if windowSize > 1 && !prices[start+windowSize-1].Time.Add(15*time.Minute).Equal(prices[start+windowSize].Time) {
			invalidTransitions++
		}
	}
	return averages
}

func isRetiredWindow(start, end time.Time, retiredWindows []TimeWindow) bool {
	for _, retired := range retiredWindows {
		if retired.Start.Equal(start) && retired.End.Equal(end) {
			return true
		}
	}
	return false
}

// calculateWindowSize calculates the number of 15-minute slots needed for a full charge/discharge.
// windowSize = (capacity_kWh / power_kW) * 4 slots_per_hour
func calculateWindowSize(capacityKWh float64, powerW int) int {
	if powerW <= 0 {
		return 8 // Default: 2 hours = 8 slots
	}
	powerKW := float64(powerW) / 1000.0
	hours := capacityKWh / powerKW
	slots := int(math.Ceil(hours * 4)) // 4 slots per hour (15 min each)
	if slots < 1 {
		return 1
	}
	return slots
}

// GetCurrentPrice returns the price for the current time slot.
func GetCurrentPrice(prices []nordpool.Price, t time.Time) (decimal.Decimal, bool) {
	for _, p := range prices {
		slotEnd := p.Time.Add(15 * time.Minute)
		if !t.Before(p.Time) && t.Before(slotEnd) {
			return decimal.NewFromFloat(p.Value), true
		}
	}
	return decimal.Zero, false
}

// IsInChargeWindow checks if the given time is within a charge window.
func (p *TradingPlan) IsInChargeWindow(t time.Time) bool {
	for _, w := range p.ChargeWindows {
		if !t.Before(w.Start) && t.Before(w.End) {
			return true
		}
	}
	return false
}

// IsInDischargeWindow checks if the given time is within a discharge window.
func (p *TradingPlan) IsInDischargeWindow(t time.Time) bool {
	for _, w := range p.DischargeWindows {
		if !t.Before(w.Start) && t.Before(w.End) {
			return true
		}
	}
	return false
}

// ShouldTrade returns true if trading should occur at the given time.
func (p *TradingPlan) ShouldTrade() bool {
	return p.IsProfitable && (len(p.ChargeWindows) > 0 || len(p.DischargeWindows) > 0)
}

package service

import (
	"math"
	"sort"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

// localMidnight returns midnight in the time's local timezone.
// Unlike Truncate(24h) which truncates to UTC midnight, this preserves the local date.
func localMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// TimeWindow represents a time window for charging or discharging.
type TimeWindow struct {
	Start time.Time
	End   time.Time
	Price float64 // Average price in this window
}

// TradeCycle represents a paired charge and discharge window.
type TradeCycle struct {
	ChargeWindow    TimeWindow
	DischargeWindow TimeWindow
	Profit          float64 // Expected profit per kWh (accounting for efficiency)
}

// TradingPlan contains the charge and discharge windows for a day.
type TradingPlan struct {
	Date             time.Time
	ChargeWindows    []TimeWindow
	DischargeWindows []TimeWindow
	Cycles           []TradeCycle // Paired charge/discharge windows
	MinPrice         float64
	MaxPrice         float64
	Spread           float64 // MaxPrice - MinPrice
	IsProfitable     bool    // At least one profitable cycle exists
}

// AnalyzerConfig contains parameters for price analysis.
type AnalyzerConfig struct {
	Efficiency         float64 // Battery round-trip efficiency (0.0-1.0)
	MinPriceSpread     float64 // Minimum EUR/kWh spread to trigger trading
	BatteryCapacityKWh float64 // Battery capacity in kWh
	BatteryMinSOC      float64 // Minimum SOC (0.0-1.0), e.g., 0.11 for 11%
	ChargePowerW       int     // Charge power in watts
	DischargePowerW    int     // Discharge power in watts
	MaxCyclesPerDay    int     // Maximum charge/discharge cycles per day
}

// AnalyzePrices analyzes the day-ahead prices and returns a trading plan.
// It finds optimal charge/discharge window pairs using a sliding window algorithm.
// Each discharge window is guaranteed to come AFTER its paired charge window.
func AnalyzePrices(prices []nordpool.Price, cfg AnalyzerConfig) *TradingPlan {
	if len(prices) == 0 {
		return &TradingPlan{}
	}

	// Sort prices by time
	sortedByTime := make([]nordpool.Price, len(prices))
	copy(sortedByTime, prices)
	sort.Slice(sortedByTime, func(i, j int) bool {
		return sortedByTime[i].Time.Before(sortedByTime[j].Time)
	})

	// Find min and max prices
	minPrice := sortedByTime[0].Value
	maxPrice := sortedByTime[0].Value
	for _, p := range sortedByTime {
		if p.Value < minPrice {
			minPrice = p.Value
		}
		if p.Value > maxPrice {
			maxPrice = p.Value
		}
	}

	spread := maxPrice - minPrice

	// Calculate usable capacity accounting for min SOC protection
	usableCapacity := cfg.BatteryCapacityKWh * (1.0 - cfg.BatteryMinSOC)

	// Calculate window size based on usable capacity and power
	chargeWindowSize := calculateWindowSize(usableCapacity, cfg.ChargePowerW)
	dischargeWindowSize := calculateWindowSize(usableCapacity, cfg.DischargePowerW)

	// Handle case where we don't have enough data points
	if len(sortedByTime) < chargeWindowSize || len(sortedByTime) < dischargeWindowSize {
		return &TradingPlan{
			Date:     localMidnight(sortedByTime[0].Time),
			MinPrice: minPrice,
			MaxPrice: maxPrice,
			Spread:   spread,
		}
	}

	// Find trade cycles using sliding window algorithm
	var cycles []TradeCycle
	searchStartIdx := 0

	// Determine max cycles (default to 2 if not configured)
	maxCycles := cfg.MaxCyclesPerDay
	if maxCycles <= 0 {
		maxCycles = 2
	}

	// Try to find profitable cycles
	for i := 0; i < maxCycles; i++ {
		cycle, found := findBestCycle(sortedByTime, searchStartIdx, chargeWindowSize, dischargeWindowSize, cfg.Efficiency, cfg.MinPriceSpread)
		if !found {
			break
		}
		cycles = append(cycles, cycle)
		// Next search starts after the discharge window ends
		searchStartIdx = findSlotIndex(sortedByTime, cycle.DischargeWindow.End)
		if searchStartIdx < 0 || searchStartIdx >= len(sortedByTime) {
			break
		}
	}

	// Extract charge and discharge windows from cycles for backwards compatibility
	var chargeWindows, dischargeWindows []TimeWindow
	for _, c := range cycles {
		chargeWindows = append(chargeWindows, c.ChargeWindow)
		dischargeWindows = append(dischargeWindows, c.DischargeWindow)
	}

	return &TradingPlan{
		Date:             localMidnight(sortedByTime[0].Time),
		ChargeWindows:    chargeWindows,
		DischargeWindows: dischargeWindows,
		Cycles:           cycles,
		MinPrice:         minPrice,
		MaxPrice:         maxPrice,
		Spread:           spread,
		IsProfitable:     len(cycles) > 0,
	}
}

// findBestCycle finds the most profitable charge/discharge pair starting from the given index.
// It evaluates ALL possible charge windows and picks the pair with maximum profit.
// Returns the cycle and true if a profitable pair was found.
func findBestCycle(prices []nordpool.Price, startIdx, chargeWindowSize, dischargeWindowSize int, efficiency, minSpread float64) (TradeCycle, bool) {
	var bestCycle TradeCycle
	bestProfit := -math.MaxFloat64
	found := false

	// Evaluate every possible charge window position
	// For each charge window, find the best discharge window after it
	for chargeStart := startIdx; chargeStart+chargeWindowSize <= len(prices); chargeStart++ {
		// Calculate average price for this charge window
		chargeAvg := windowAverage(prices, chargeStart, chargeWindowSize)

		// Discharge must start after charge ends
		dischargeSearchStart := chargeStart + chargeWindowSize
		if dischargeSearchStart+dischargeWindowSize > len(prices) {
			// No room for discharge window after this charge window
			continue
		}

		// Find the best (highest) discharge window after this charge window
		dischargeStart, dischargeAvg, dischargeFound := findBestWindow(prices, dischargeSearchStart, dischargeWindowSize, false)
		if !dischargeFound {
			continue
		}

		// Check if the trade is profitable
		// Profitable if: discharge_price > charge_price / efficiency AND spread >= minSpread
		breakEvenPrice := chargeAvg / efficiency
		if dischargeAvg <= breakEvenPrice || (dischargeAvg-chargeAvg) < minSpread {
			continue
		}

		// Calculate expected profit per kWh
		// profit = discharge_price * efficiency - charge_price
		profit := dischargeAvg*efficiency - chargeAvg

		// Keep the most profitable pair
		if profit > bestProfit {
			bestProfit = profit
			found = true

			bestCycle = TradeCycle{
				ChargeWindow: TimeWindow{
					Start: prices[chargeStart].Time,
					End:   prices[chargeStart+chargeWindowSize-1].Time.Add(15 * time.Minute),
					Price: chargeAvg,
				},
				DischargeWindow: TimeWindow{
					Start: prices[dischargeStart].Time,
					End:   prices[dischargeStart+dischargeWindowSize-1].Time.Add(15 * time.Minute),
					Price: dischargeAvg,
				},
				Profit: profit,
			}
		}
	}

	return bestCycle, found
}

// windowAverage calculates the average price for a window starting at startIdx.
func windowAverage(prices []nordpool.Price, startIdx, windowSize int) float64 {
	var sum float64
	for i := startIdx; i < startIdx+windowSize; i++ {
		sum += prices[i].Value
	}
	return sum / float64(windowSize)
}

// findBestWindow finds the best contiguous window of the given size starting from startIdx.
// If findLowest is true, finds the window with the lowest average price.
// If findLowest is false, finds the window with the highest average price.
// Returns the start index, average price, and whether a valid window was found.
func findBestWindow(prices []nordpool.Price, startIdx, windowSize int, findLowest bool) (int, float64, bool) {
	if startIdx+windowSize > len(prices) {
		return 0, 0, false
	}

	bestStart := -1
	var bestAvg float64
	if findLowest {
		bestAvg = math.MaxFloat64
	} else {
		bestAvg = -math.MaxFloat64
	}

	// Sliding window: compute initial sum
	var windowSum float64
	for i := startIdx; i < startIdx+windowSize; i++ {
		windowSum += prices[i].Value
	}

	// Check first window
	avg := windowSum / float64(windowSize)
	if findLowest {
		if avg < bestAvg {
			bestAvg = avg
			bestStart = startIdx
		}
	} else {
		if avg > bestAvg {
			bestAvg = avg
			bestStart = startIdx
		}
	}

	// Slide the window
	for i := startIdx + 1; i+windowSize <= len(prices); i++ {
		// Remove element leaving window, add element entering window
		windowSum -= prices[i-1].Value
		windowSum += prices[i+windowSize-1].Value
		avg = windowSum / float64(windowSize)

		if findLowest {
			if avg < bestAvg {
				bestAvg = avg
				bestStart = i
			}
		} else {
			if avg > bestAvg {
				bestAvg = avg
				bestStart = i
			}
		}
	}

	if bestStart < 0 {
		return 0, 0, false
	}

	return bestStart, bestAvg, true
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

// findSlotIndex finds the index of the slot that starts at or after the given time.
// Returns -1 if not found.
func findSlotIndex(prices []nordpool.Price, t time.Time) int {
	for i, p := range prices {
		if !p.Time.Before(t) {
			return i
		}
	}
	return -1
}

// GetCurrentPrice returns the price for the current time slot.
func GetCurrentPrice(prices []nordpool.Price, t time.Time) (float64, bool) {
	for _, p := range prices {
		slotEnd := p.Time.Add(15 * time.Minute)
		if !t.Before(p.Time) && t.Before(slotEnd) {
			return p.Value, true
		}
	}
	return 0, false
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

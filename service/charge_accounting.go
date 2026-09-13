package service

import (
	"time"

	"github.com/shopspring/decimal"
)

// scheduledChargeAccounting attributes measured AC-side scheduled charging to
// the grid import reported by P1. It is deliberately separate from battery
// control: callers retain the last successful P1 value across read failures.
type scheduledChargeAccounting struct {
	meterEnabled bool
	lastUpdate   time.Time
	lastACPowerW float64
	lastImportW  float64
	importKnown  bool
	intervals    []scheduledChargeIntervalAccounting
}

type scheduledChargeIntervalAccounting struct {
	start, end            time.Time
	energyKWh             decimal.Decimal
	gridEnergyKWh         decimal.Decimal
	gridCostEUR           decimal.Decimal
	gridUnpricedKWh       decimal.Decimal
	unattributedEnergyKWh decimal.Decimal
	unpricedKWh           decimal.Decimal
	opportunityCostEUR    decimal.Decimal
}

type chargePriceLookup func(at, until time.Time, export bool) (decimal.Decimal, time.Time, bool)

// Begin starts attribution immediately after a scheduled charge command is
// confirmed. meterSampleKnown must be false for a failed initial P1 read; that
// energy remains explicitly unattributed until the first successful sample.
func (a *scheduledChargeAccounting) Begin(at time.Time, measuredACPowerW float64, meterEnabled bool, importPowerW float64, meterSampleKnown bool) {
	*a = scheduledChargeAccounting{
		meterEnabled: meterEnabled,
		lastUpdate:   at,
		lastACPowerW: measuredACPowerW,
	}
	if meterEnabled && meterSampleKnown {
		a.lastImportW = importPowerW
		a.importKnown = true
	}
}

// Sample settles the preceding interval then accepts the current AC/P1 sample.
// A failed P1 sample never clears a previously known import estimate, and thus
// must not interrupt scheduled charging. The caller must serialize calls while
// holding the Service lock and pass the session's snapshot price lookup.
func (a *scheduledChargeAccounting) Sample(at time.Time, measuredACPowerW, importPowerW float64, meterSampleKnown bool, loc *time.Location, priceAt chargePriceLookup) {
	if !a.meterEnabled || a.lastUpdate.IsZero() {
		return
	}
	a.settle(at, loc, priceAt)
	a.lastACPowerW = measuredACPowerW
	if meterSampleKnown {
		a.lastImportW = importPowerW
		a.importKnown = true
	}
}

// Apply replaces the generic measured-trade accounting with the measured P1
// split, including its interval-derived total energy. A disabled meter
// deliberately remains on the old all-grid ActionCharge interpretation.
func (a *scheduledChargeAccounting) Apply(trade *Trade, loc *time.Location) {
	if !a.meterEnabled || trade.Action != ActionCharge || a.lastUpdate.IsZero() {
		return
	}

	trade.ChargeSourceAttribution = ChargeSourceMeasuredP1Split
	trade.EnergyKWh = decimal.Zero
	trade.GridEnergyKWh = decimal.Zero
	trade.GridCostEUR = decimal.Zero
	trade.GridUnpricedKWh = decimal.Zero
	trade.UnattributedEnergyKWh = decimal.Zero
	trade.UnpricedKWh = decimal.Zero
	trade.OpportunityCostEUR = decimal.Zero
	for i := range trade.DayAllocations {
		allocation := &trade.DayAllocations[i]
		allocation.EnergyKWh = decimal.Zero
		allocation.GridEnergyKWh = decimal.Zero
		allocation.GridCostEUR = decimal.Zero
		allocation.GridUnpricedKWh = decimal.Zero
		allocation.UnattributedEnergyKWh = decimal.Zero
		allocation.UnpricedKWh = decimal.Zero
		allocation.OpportunityCostEUR = decimal.Zero
		allocation.PricedValueEUR = decimal.Zero
		allocationEnd := allocation.Timestamp.Add(time.Duration(allocation.DurationS) * time.Second)
		for _, interval := range a.intervals {
			start := allocation.Timestamp
			if interval.start.After(start) {
				start = interval.start
			}
			end := allocationEnd
			if interval.end.Before(end) {
				end = interval.end
			}
			if !end.After(start) {
				continue
			}
			portion := decimal.NewFromInt(end.Sub(start).Nanoseconds()).Div(decimal.NewFromInt(interval.end.Sub(interval.start).Nanoseconds()))
			addScheduledChargePortion(allocation, interval, portion)
		}
		trade.EnergyKWh = trade.EnergyKWh.Add(allocation.EnergyKWh)
		trade.GridEnergyKWh = trade.GridEnergyKWh.Add(allocation.GridEnergyKWh)
		trade.GridCostEUR = trade.GridCostEUR.Add(allocation.GridCostEUR)
		trade.GridUnpricedKWh = trade.GridUnpricedKWh.Add(allocation.GridUnpricedKWh)
		trade.UnattributedEnergyKWh = trade.UnattributedEnergyKWh.Add(allocation.UnattributedEnergyKWh)
		trade.UnpricedKWh = trade.UnpricedKWh.Add(allocation.UnpricedKWh)
		trade.OpportunityCostEUR = trade.OpportunityCostEUR.Add(allocation.OpportunityCostEUR)
	}
	pricedGridKWh := trade.GridEnergyKWh.Sub(trade.GridUnpricedKWh)
	if pricedGridKWh.IsPositive() {
		trade.PriceEUR = trade.GridCostEUR.Div(pricedGridKWh)
	} else {
		trade.PriceEUR = decimal.Zero
	}
}

func addScheduledChargePortion(allocation *TradeDayAllocation, interval scheduledChargeIntervalAccounting, portion decimal.Decimal) {
	allocation.EnergyKWh = allocation.EnergyKWh.Add(interval.energyKWh.Mul(portion))
	allocation.GridEnergyKWh = allocation.GridEnergyKWh.Add(interval.gridEnergyKWh.Mul(portion))
	allocation.GridCostEUR = allocation.GridCostEUR.Add(interval.gridCostEUR.Mul(portion))
	allocation.GridUnpricedKWh = allocation.GridUnpricedKWh.Add(interval.gridUnpricedKWh.Mul(portion))
	allocation.UnattributedEnergyKWh = allocation.UnattributedEnergyKWh.Add(interval.unattributedEnergyKWh.Mul(portion))
	allocation.UnpricedKWh = allocation.UnpricedKWh.Add(interval.unpricedKWh.Mul(portion))
	allocation.OpportunityCostEUR = allocation.OpportunityCostEUR.Add(interval.opportunityCostEUR.Mul(portion))
	allocation.PricedValueEUR = allocation.PricedValueEUR.Add(interval.gridCostEUR.Mul(portion))
}

func (a *scheduledChargeAccounting) settle(at time.Time, loc *time.Location, priceAt chargePriceLookup) {
	if !a.lastUpdate.Before(at) {
		return
	}
	powerW := max(a.lastACPowerW, 0)
	for cursor := a.lastUpdate; cursor.Before(at); {
		_, importEnd, _ := priceAt(cursor, at, false)
		_, exportEnd, _ := priceAt(cursor, at, true)
		end := importEnd
		if exportEnd.Before(end) {
			end = exportEnd
		}
		if dayEnd := localMidnight(cursor.In(loc)).AddDate(0, 0, 1); dayEnd.Before(end) {
			end = dayEnd
		}
		if !end.After(cursor) {
			end = at
		}
		energyKWh := decimal.NewFromFloat(powerW * end.Sub(cursor).Seconds() / 3_600_000)
		interval := scheduledChargeIntervalAccounting{
			start:     cursor,
			end:       end,
			energyKWh: energyKWh,
		}
		if !a.importKnown {
			interval.unattributedEnergyKWh = energyKWh
		} else {
			gridPowerW := min(max(a.lastImportW, 0), powerW)
			interval.gridEnergyKWh = decimal.NewFromFloat(gridPowerW * end.Sub(cursor).Seconds() / 3_600_000)
			interval.gridCostEUR, interval.gridUnpricedKWh = scheduledChargeGridCost(interval.gridEnergyKWh, cursor, end, priceAt)
			solarEnergyKWh := energyKWh.Sub(interval.gridEnergyKWh)
			if solarEnergyKWh.IsPositive() {
				exportPrice, _, exportKnown := priceAt(cursor, end, true)
				if exportKnown {
					interval.opportunityCostEUR = exportPrice.Mul(solarEnergyKWh)
				} else {
					interval.unpricedKWh = solarEnergyKWh
				}
			}
		}
		a.intervals = append(a.intervals, interval)
		cursor = end
	}
	a.lastUpdate = at
}

func scheduledChargeGridCost(energyKWh decimal.Decimal, at, until time.Time, priceAt chargePriceLookup) (decimal.Decimal, decimal.Decimal) {
	if !energyKWh.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	price, _, known := priceAt(at, until, false)
	if known {
		return price.Mul(energyKWh), decimal.Zero
	}
	return decimal.Zero, energyKWh
}

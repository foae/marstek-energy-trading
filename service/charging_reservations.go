package service

import (
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// chargingReservation uses no forecast: only energy already reflected in SOC
// reduces the grid requirement. Charge efficiency converts the DC shortfall
// into the AC input that must be drawn from the grid; the remaining round-trip
// loss sits on the discharge side and is not reserved for here.
type chargingReservation struct {
	Deadline            time.Time
	RequiredKWh         float64
	ReservedKWh         float64
	Feasible            bool
	LimitedByEconomics  bool
	Windows             []TimeWindow
	maxChargePrice      decimal.Decimal
	currentPriceTooHigh bool
	pairedCycle         *TradeCycle
}

func (s *Service) chargeReservationLocked(now time.Time, soc int) chargingReservation {
	var result chargingReservation
	if s.currentPlan == nil || soc >= 100 || s.automaticCycleCleanupPending ||
		(s.automaticCycleCommit != nil && !now.Before(s.automaticCycleCommit.DischargeWindow.End)) {
		return result
	}
	earliest := s.currentPlan.EarliestChargeStart
	var dischargePrice decimal.Decimal
	for _, cycle := range s.currentPlan.Cycles {
		if cycle.ChargeWindow.End.After(now) {
			result.Deadline = cycle.ChargeWindow.End
			dischargePrice = cycle.DischargeWindow.Price
			cycleCopy := cycle
			result.pairedCycle = &cycleCopy
			break
		}
		if cycle.DischargeWindow.End.After(earliest) {
			earliest = cycle.DischargeWindow.End
		}
	}
	if result.Deadline.IsZero() {
		return result
	}
	if now.Before(earliest) {
		return chargingReservation{}
	}
	if earliest.Before(now) {
		earliest = now
	}
	chargeEff := s.cfg.BatteryChargeEfficiency
	if !(chargeEff > 0 && chargeEff <= 1) {
		chargeEff = 1
	}
	result.RequiredKWh = s.cfg.BatteryCapacityKWh * float64(100-soc) / 100 / chargeEff
	powerKW := s.cfg.PlanningChargePowerW() / 1000
	// Observed taper can only reduce assumed deliverability, never promise more
	// than nameplate. Fresh samples are supplied by the serialized control owner.
	if s.state == StateCharging && s.observedChargePowerW > 0 && now.Sub(s.currentTradeStart) >= 30*time.Second {
		powerKW = min(powerKW, s.observedChargePowerW/1000)
	}
	result.maxChargePrice = dischargePrice.
		Mul(decimal.NewFromFloat(s.cfg.BatteryEfficiency)).
		Sub(decimal.NewFromFloat(s.cfg.MinPriceSpread))
	var eligibleCapacityKWh, totalCapacityKWh float64
	for _, price := range s.futurePriceHorizonLocked(now) {
		start, end := price.Time, price.Time.Add(15*time.Minute)
		isCurrentSlice := !now.Before(start) && now.Before(end)
		if start.Before(earliest) {
			start = earliest
		}
		if end.After(result.Deadline) {
			end = result.Deadline
		}
		if !start.Before(end) {
			continue
		}
		capacityKWh := powerKW * end.Sub(start).Hours()
		totalCapacityKWh += capacityKWh
		priceDecimal := decimal.NewFromFloat(price.Value)
		if !s.chargePriceMeetsProfitFloorLocked(priceDecimal, result.maxChargePrice) {
			result.currentPriceTooHigh = result.currentPriceTooHigh || isCurrentSlice
			continue
		}
		eligibleCapacityKWh += capacityKWh
		result.Windows = append(result.Windows, TimeWindow{Start: start, End: end, Price: priceDecimal})
	}
	allocation := allocateDeferredChargeSlices(result.Windows, result.RequiredKWh, powerKW,
		s.cfg.ChargeDeferToleranceEURPerKWh, now, s.state == StateCharging, s.meterEnabled())
	result.Windows = allocation.Windows[:allocation.Count]
	result.ReservedKWh = allocation.ReservedKWh
	remaining := result.RequiredKWh - result.ReservedKWh
	result.Feasible = remaining <= 0.000001
	result.LimitedByEconomics = eligibleCapacityKWh+0.000001 < result.RequiredKWh &&
		eligibleCapacityKWh+0.000001 < totalCapacityKWh
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].Start.Before(result.Windows[j].Start) })
	return result
}

// chargeContinuationToleranceEUR bounds what is paid to finish a running slice.
// Finishing a running slice that has drifted above the price cap is cheaper than
// a stop/start pair when the price penalty on its remaining energy is under one
// cent; a whole dear slot entered at a boundary costs far more and is not kept.
const chargeContinuationToleranceEUR = 0.01

// deferredStartLead widens a front-truncated deferred piece backwards. The
// control loop ticks about once a minute and is not aligned to the piece, so a
// piece that begins exactly when its energy is due can be seen up to a minute
// late, and a piece only slightly longer than the minimum control window would
// never be startable at all. The lead costs at most a minute of early charging
// and is corrected by the next SOC recalculation.
const deferredStartLead = time.Minute

// allocateDeferredChargeSlices reserves the required AC energy as late as
// possible without paying materially more than the cheapest selection, so
// solar captured in the meantime shrinks the grid purchase. Slices priced at
// most toleranceEURPerKWh above the cheapest allocation's average price are
// "affordable"; any shortfall left after them comes from the cheapest of the
// remaining slices. Before charging starts, affordable slices are taken from
// the latest backwards and the earliest one is truncated at its start. Once
// charging, the selection runs contiguously forward from now (the slice
// containing now is kept whatever its remaining length when it is affordable or
// when finishing its remaining energy costs under chargeContinuationToleranceEUR
// above the cheapest average, so a running charge is never interrupted mid-slot
// to save a fraction of a cent; a dearer slot entered at a boundary costs far
// more than that and is only used if the shortfall needs it) and rising SOC
// shrinks its tail, never its head. A truncated piece is never shorter than
// minimumAutomaticControlWindow plus one second; it is widened within its slot
// instead, which over-reserves by under a minute and is corrected by the next
// SOC recalculation. A front-truncated piece additionally starts
// deferredStartLead earlier, bounded by its slot.
//
// deferral is false without a P1 meter: SOC cannot rise while waiting, so
// waiting can only cost money and the cheapest slices are reserved, keeping the
// running slice when it is affordable. That still runs contiguously from the
// running slice, which is what keeps a meterless install from stopping and
// restarting at every tariff boundary.
//
// An infeasible cheapest allocation has no meaningful average price, so the
// price cap is dropped and every slice counts as affordable: the same total
// energy is reserved, the shortfall is still reported, and a running charge
// keeps its slice.
func allocateDeferredChargeSlices(windows []TimeWindow, requiredKWh, powerKW, toleranceEURPerKWh float64, now time.Time, charging, deferral bool) chargeAllocation {
	cheapest := allocateCheapestChargeSlices(windows, requiredKWh, powerKW)
	if cheapest.Count == 0 || powerKW <= 0 || requiredKWh <= plannerEnergyEpsilon {
		return cheapest
	}
	if !deferral && !charging {
		return cheapest
	}
	cheapestAverage := cheapest.Cost.Div(decimal.NewFromFloat(cheapest.ReservedKWh))
	priceCap := cheapestAverage.Add(decimal.NewFromFloat(max(toleranceEURPerKWh, 0)))

	owned := append([]TimeWindow(nil), windows...)
	var running *TimeWindow
	var affordable, rest []TimeWindow
	for i := range owned {
		window := owned[i]
		// A running slice is kept whatever its remaining length when it is
		// affordable, or when its remaining energy costs under a cent more than
		// the cheapest average: it is already being charged, so dropping it stops
		// the inverter mid-slot. A whole dear slot merely entered at a tariff
		// boundary costs far more than that and is classified like any other
		// window.
		if charging && running == nil && !now.Before(window.Start) && now.Before(window.End) &&
			(!cheapest.Feasible || window.Price.LessThanOrEqual(priceCap) ||
				window.Price.Sub(cheapestAverage).
					Mul(decimal.NewFromFloat(powerKW*window.End.Sub(window.Start).Hours())).
					LessThan(decimal.NewFromFloat(chargeContinuationToleranceEUR))) {
			candidate := window
			running = &candidate
			continue
		}
		if window.End.Sub(window.Start) <= minimumAutomaticControlWindow {
			continue
		}
		if !cheapest.Feasible || window.Price.LessThanOrEqual(priceCap) {
			affordable = append(affordable, window)
			continue
		}
		rest = append(rest, window)
	}
	sort.SliceStable(affordable, func(i, j int) bool {
		switch {
		case !deferral:
			// Meterless: nothing can arrive while waiting, so pay the least.
			if affordable[i].Price.Equal(affordable[j].Price) {
				return affordable[i].Start.Before(affordable[j].Start)
			}
			return affordable[i].Price.LessThan(affordable[j].Price)
		case charging:
			return affordable[i].Start.Before(affordable[j].Start)
		default:
			return affordable[j].Start.Before(affordable[i].Start)
		}
	})
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Price.Equal(rest[j].Price) {
			return rest[i].Start.Before(rest[j].Start)
		}
		return rest[i].Price.LessThan(rest[j].Price)
	})
	candidates := make([]TimeWindow, 0, len(owned))
	if running != nil {
		candidates = append(candidates, *running)
	}
	affordableCount := len(candidates) + len(affordable)
	candidates = append(candidates, affordable...)
	candidates = append(candidates, rest...)

	selected := make([]TimeWindow, 0, len(candidates))
	remaining := requiredKWh
	for i, window := range candidates {
		if remaining <= plannerEnergyEpsilon {
			break
		}
		full := window.End.Sub(window.Start)
		energy := powerKW * full.Hours()
		if energy <= 0 {
			continue
		}
		if energy <= remaining+plannerEnergyEpsilon {
			selected = append(selected, window)
			remaining -= energy
			continue
		}
		duration := time.Duration(math.Round(remaining / powerKW * float64(time.Hour)))
		if duration <= minimumAutomaticControlWindow {
			duration = minimumAutomaticControlWindow + time.Second
		}
		if duration > full {
			duration = full
		}
		// Deferral truncates from the front so the piece keeps its slot's end;
		// forward selections (a running charge, or any fallback slice) keep their
		// start instead.
		if !charging && i < affordableCount {
			slotStart := window.Start
			window.Start = window.End.Add(-duration - deferredStartLead)
			if window.Start.Before(slotStart) {
				window.Start = slotStart
			}
		} else {
			window.End = window.Start.Add(duration)
		}
		selected = append(selected, window)
		remaining -= powerKW * duration.Hours()
		break
	}
	result := chargeAllocation{Windows: selected, Count: len(selected)}
	recalculateChargeAllocation(&result, powerKW)
	result.Feasible = remaining <= plannerEnergyEpsilon
	return result
}

func (s *Service) gridReservedLocked(now time.Time, soc int) bool {
	return s.chargeReservationLocked(now, soc).contains(now)
}

func (r chargingReservation) contains(now time.Time) bool {
	_, ok := r.windowAt(now)
	return ok
}

func (r chargingReservation) windowAt(now time.Time) (TimeWindow, bool) {
	for _, window := range r.Windows {
		if !now.Before(window.Start) && now.Before(window.End) {
			return window, true
		}
	}
	return TimeWindow{}, false
}

// solarBlockReasonLocked labels why solar charging yields, for the decision log.
func (s *Service) solarBlockReasonLocked() string {
	if s.retiredDischargeWindowsDirty {
		return "retired windows pending persistence"
	}
	return "plan window"
}

func (s *Service) solarBlockedLocked(now time.Time, soc int) bool {
	if s.retiredDischargeWindowsDirty {
		return true
	}
	reservation := s.chargeReservationLocked(now, soc)
	return reservation.contains(now) ||
		(s.currentPlan != nil && s.currentPlan.IsInDischargeWindow(now))
}

func (s *Service) chargePriceMeetsProfitFloorLocked(price, maxChargePrice decimal.Decimal) bool {
	return chargePriceEligible(price, maxChargePrice, s.cfg.MinPriceSpread)
}

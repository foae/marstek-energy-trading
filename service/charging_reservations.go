package service

import (
	"log/slog"
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
	powerKW := float64(s.cfg.ChargePowerW) / 1000
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
	allocation := allocateCheapestChargeSlices(result.Windows, result.RequiredKWh, powerKW)
	result.Windows = allocation.Windows
	count := allocation.Count
	result.ReservedKWh = allocation.ReservedKWh
	remaining := result.RequiredKWh - result.ReservedKWh
	if newCount, applied := s.extendRunningReservationSlice(now, result.Windows, count, powerKW, result.Deadline); applied {
		count = newCount
		result.ReservedKWh = 0
		for _, window := range result.Windows[:count] {
			result.ReservedKWh += powerKW * window.End.Sub(window.Start).Hours()
		}
	}
	result.Windows = result.Windows[:count]
	result.Feasible = remaining <= 0.000001
	result.LimitedByEconomics = eligibleCapacityKWh+0.000001 < result.RequiredKWh &&
		eligibleCapacityKWh+0.000001 < totalCapacityKWh
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].Start.Before(result.Windows[j].Start) })
	return result
}

// chargeContinuationToleranceEUR bounds the extra energy cost accepted to keep a
// running charge going to its tariff boundary: never stop and restart the
// inverter to save less than one cent. Each stop/start costs about two minutes
// of charging and extra Modbus writes.
const chargeContinuationToleranceEUR = 0.01

// extendRunningReservationSlice keeps a charge session that is running inside a
// reservation slice alive until that slice's tariff boundary, displacing the
// same energy from the next most expensive selected slices. Falling afternoon
// prices otherwise re-truncate the running slice on every tick, producing a
// stop/start pair per slot.
//
// The running slice is located anywhere in the full price-sorted slice, not only
// within the current selection: rising SOC can shrink the requirement until the
// cheaper future slices cover it alone and the running slice drops out of the
// selection entirely. In that case it is re-added as the marginal slice with
// zero energy before the extension is priced, so a still-running charge is never
// silently stopped mid-slot.
//
// windows must be the full price-sorted eligible list, of which the first
// count entries are the selection; it is mutated in place only once
// the displacement is known to be both affordable and fully absorbable. Returns
// the new selected count and whether the displacement was applied.
func (s *Service) extendRunningReservationSlice(now time.Time, windows []TimeWindow, count int, powerKW float64, deadline time.Time) (int, bool) {
	if s.state != StateCharging || powerKW <= 0 {
		return count, false
	}
	index := -1
	for i := range windows {
		slotStart := windows[i].Start.Truncate(15 * time.Minute)
		if !now.Before(slotStart) && now.Before(slotStart.Add(15*time.Minute)) {
			index = i
			break
		}
	}
	if index < 0 {
		return count, false
	}
	marginal := &windows[index]
	slotStart := marginal.Start.Truncate(15 * time.Minute)
	slotEnd := slotStart.Add(15 * time.Minute)
	if !deadline.IsZero() && slotEnd.After(deadline) {
		slotEnd = deadline
	}
	if now.Before(slotStart) || !now.Before(slotEnd) {
		return count, false
	}
	// An unselected running slice re-enters the selection with zero energy; a
	// selected one extends from where the sizing loop truncated it.
	reAdd := index >= count
	newCount := count
	currentEnd := marginal.End
	if reAdd {
		newCount = count + 1
		currentEnd = marginal.Start
	}
	// There must be at least one cheaper selected slice to displace energy onto.
	if newCount < 2 {
		return count, false
	}
	if !currentEnd.Before(slotEnd) {
		return count, false
	}
	extraKWh := powerKW * slotEnd.Sub(currentEnd).Hours()
	if extraKWh <= 0 {
		return count, false
	}
	type displacement struct {
		index int
		kWh   float64
	}
	var displacements []displacement
	penalty := decimal.Zero
	remaining := extraKWh
	for i := newCount - 2; i >= 0 && remaining > 0; i-- {
		available := powerKW * windows[i].End.Sub(windows[i].Start).Hours()
		take := min(remaining, available)
		if take <= 0 {
			continue
		}
		penalty = penalty.Add(marginal.Price.Sub(windows[i].Price).Mul(decimal.NewFromFloat(take)))
		displacements = append(displacements, displacement{index: i, kWh: take})
		remaining -= take
	}
	// Exactly one cent is already too expensive: the tolerance is "under a cent".
	if !penalty.LessThan(decimal.NewFromFloat(chargeContinuationToleranceEUR)) {
		return count, false
	}
	// The cheaper slices cannot absorb the whole extension; extending anyway would
	// reserve more energy than the requirement. Keep the truncation instead.
	if remaining > 1e-9 {
		return count, false
	}
	if reAdd {
		windows[index], windows[count] = windows[count], windows[index]
		marginal = &windows[count]
		count = newCount
	}
	marginal.End = slotEnd
	drop := make(map[int]bool, len(displacements))
	for _, d := range displacements {
		window := &windows[d.index]
		window.End = window.End.Add(-time.Duration(math.Round(d.kWh / powerKW * float64(time.Hour))))
		if !window.Start.Before(window.End) {
			drop[d.index] = true
		}
	}
	if len(drop) > 0 {
		kept := 0
		for i := 0; i < count; i++ {
			if drop[i] {
				continue
			}
			windows[kept] = windows[i]
			kept++
		}
		count = kept
	}
	slog.Debug("extending running reservation slice to tariff boundary",
		"slot_end", slotEnd, "penalty_eur", penalty.String(), "re_added", reAdd)
	return count, true
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

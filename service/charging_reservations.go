package service

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// chargingReservation uses no forecast: only energy already reflected in SOC
// reduces the grid requirement. Efficiency is a conservative charging estimate,
// not a claim that round-trip loss is entirely on the charging side.
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
	var earliest time.Time
	var dischargePrice decimal.Decimal
	for _, cycle := range s.currentPlan.Cycles {
		if cycle.ChargeWindow.End.After(now) {
			result.Deadline = cycle.ChargeWindow.End
			dischargePrice = cycle.DischargeWindow.Price
			cycleCopy := cycle
			result.pairedCycle = &cycleCopy
			break
		}
		earliest = cycle.DischargeWindow.End
	}
	if result.Deadline.IsZero() {
		return result
	}
	if now.Before(earliest) {
		return chargingReservation{}
	}
	result.RequiredKWh = s.cfg.BatteryCapacityKWh * float64(100-soc) / 100 / s.cfg.BatteryEfficiency
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
		if start.Before(now) {
			start = now
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
	sort.SliceStable(result.Windows, func(i, j int) bool {
		if result.Windows[i].Price.Equal(result.Windows[j].Price) {
			return result.Windows[i].Start.Before(result.Windows[j].Start)
		}
		return result.Windows[i].Price.LessThan(result.Windows[j].Price)
	})
	remaining := result.RequiredKWh
	count := 0
	for i := range result.Windows {
		if remaining <= 0 {
			break
		}
		window := &result.Windows[i]
		energy := powerKW * window.End.Sub(window.Start).Hours()
		if energy > remaining {
			energy = remaining
			window.End = window.Start.Add(time.Duration(energy / powerKW * float64(time.Hour)))
		}
		remaining -= energy
		result.ReservedKWh += energy
		count++
	}
	result.Windows = result.Windows[:count]
	result.Feasible = remaining <= 0.000001
	result.LimitedByEconomics = eligibleCapacityKWh+0.000001 < result.RequiredKWh &&
		eligibleCapacityKWh+0.000001 < totalCapacityKWh
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].Start.Before(result.Windows[j].Start) })
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

func (s *Service) solarBlockedLocked(now time.Time, soc int) bool {
	reservation := s.chargeReservationLocked(now, soc)
	return reservation.contains(now) ||
		(s.currentPlan != nil && s.currentPlan.IsInDischargeWindow(now)) ||
		!s.solarEconomicalForReservationLocked(now, reservation)
}

func (s *Service) solarEconomicalForReservationLocked(now time.Time, reservation chargingReservation) bool {
	// When only time or measured taper makes the grid reservation infeasible,
	// accept every available solar watt as best effort toward the commitment.
	timeLimitedCommittedCycle := s.automaticCycleCommit != nil && now.Before(s.automaticCycleCommit.ChargeWindow.End) &&
		sameTradeCycle(reservation.pairedCycle, s.automaticCycleCommit)
	timeLimitedRetainedSolarCycle := s.solarCycleRetention != nil && now.Before(s.solarCycleRetention.ChargeWindow.End)
	if !reservation.Deadline.IsZero() && !reservation.Feasible && !reservation.LimitedByEconomics &&
		(timeLimitedCommittedCycle || timeLimitedRetainedSolarCycle) {
		return true
	}
	if maxChargePrice, committed := s.pendingCycleMaxChargePriceLocked(now); committed {
		price, known := s.currentPriceLocked(now)
		if !known || !s.chargePriceMeetsProfitFloorLocked(price, maxChargePrice) {
			return false
		}
	}
	if reservation.Deadline.IsZero() {
		return true
	}
	price, known := s.currentPriceLocked(now)
	if !known {
		return false
	}
	if !reservation.Feasible {
		return s.chargePriceMeetsProfitFloorLocked(price, reservation.maxChargePrice)
	}
	for _, window := range reservation.Windows {
		if !price.LessThanOrEqual(window.Price) {
			continue
		}
		return true
	}
	return false
}

func (s *Service) chargePriceMeetsProfitFloorLocked(price, maxChargePrice decimal.Decimal) bool {
	minProfit := decimal.NewFromFloat(s.cfg.MinPriceSpread)
	expectedProfit := maxChargePrice.Add(minProfit).Sub(price)
	return expectedProfit.IsPositive() && !expectedProfit.LessThan(minProfit)
}

func (s *Service) cycleForSolarRetentionLocked(now time.Time, reservation chargingReservation) *TradeCycle {
	if s.automaticCycleCommit != nil || s.solarCycleRetention != nil {
		return nil
	}
	if s.currentPlan != nil {
		for _, cycle := range s.currentPlan.Cycles {
			if !now.Before(cycle.ChargeWindow.End) && now.Before(cycle.DischargeWindow.Start) {
				cycleCopy := cycle
				return &cycleCopy
			}
		}
	}
	if reservation.pairedCycle != nil {
		cycleCopy := *reservation.pairedCycle
		return &cycleCopy
	}
	return nil
}

func (s *Service) pendingCycleMaxChargePriceLocked(now time.Time) (decimal.Decimal, bool) {
	var cycle *TradeCycle
	if s.automaticCycleCommit != nil && now.Before(s.automaticCycleCommit.DischargeWindow.End) {
		cycle = s.automaticCycleCommit
	} else if s.solarCycleRetention != nil && !now.Before(s.solarCycleRetention.ChargeWindow.End) &&
		now.Before(s.solarCycleRetention.DischargeWindow.End) {
		cycle = s.solarCycleRetention
	} else if s.currentPlan != nil {
		for i := range s.currentPlan.Cycles {
			candidate := &s.currentPlan.Cycles[i]
			if !now.Before(candidate.ChargeWindow.End) && now.Before(candidate.DischargeWindow.End) {
				cycle = candidate
				break
			}
		}
	}
	if cycle == nil {
		return decimal.Zero, false
	}
	return cycle.DischargeWindow.Price.
		Mul(decimal.NewFromFloat(s.cfg.BatteryEfficiency)).
		Sub(decimal.NewFromFloat(s.cfg.MinPriceSpread)), true
}

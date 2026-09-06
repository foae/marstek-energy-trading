package service

import (
	"sort"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/shopspring/decimal"
)

// chargingReservation uses no forecast: only energy already reflected in SOC
// reduces the grid requirement. Efficiency is a conservative charging estimate,
// not a claim that round-trip loss is entirely on the charging side.
type chargingReservation struct {
	Deadline    time.Time    `json:"deadline"`
	RequiredKWh float64      `json:"required_input_kwh"`
	ReservedKWh float64      `json:"reserved_input_kwh"`
	Feasible    bool         `json:"feasible"`
	Windows     []TimeWindow `json:"windows"`
}

func (s *Service) chargeReservationLocked(now time.Time, soc int) chargingReservation {
	var result chargingReservation
	if s.currentPlan == nil || soc >= 100 {
		return result
	}
	var earliest time.Time
	for _, cycle := range s.currentPlan.Cycles {
		if cycle.ChargeWindow.End.After(now) {
			result.Deadline = cycle.ChargeWindow.End
			break
		}
		earliest = cycle.DischargeWindow.End
	}
	if result.Deadline.IsZero() || now.Before(earliest) {
		return result
	}
	result.RequiredKWh = s.cfg.BatteryCapacityKWh * float64(100-soc) / 100 / s.cfg.BatteryEfficiency
	powerKW := float64(s.cfg.ChargePowerW) / 1000
	// Observed taper can only reduce assumed deliverability, never promise more
	// than nameplate. Fresh samples are supplied by the serialized control owner.
	if s.state == StateCharging && s.observedChargePowerW > 0 && s.now().Sub(s.currentTradeStart) >= 30*time.Second {
		powerKW = min(powerKW, s.observedChargePowerW/1000)
	}
	for _, prices := range [][]nordpool.Price{s.todayPrices, s.tomorrowPrices} {
		for _, price := range prices {
			start, end := price.Time, price.Time.Add(15*time.Minute)
			if start.Before(now) {
				start = now
			}
			if start.Before(earliest) {
				start = earliest
			}
			if end.After(result.Deadline) {
				end = result.Deadline
			}
			if !start.Before(end) {
				continue
			}
			result.Windows = append(result.Windows, TimeWindow{Start: start, End: end, Price: decimal.NewFromFloat(price.Value)})
		}
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
	sort.Slice(result.Windows, func(i, j int) bool { return result.Windows[i].Start.Before(result.Windows[j].Start) })
	return result
}

func (s *Service) gridReservedLocked(now time.Time, soc int) bool {
	for _, window := range s.chargeReservationLocked(now, soc).Windows {
		if !now.Before(window.Start) && now.Before(window.End) {
			return true
		}
	}
	return false
}

// Solar has the same opportunity cost as exporting at the current tariff.
// Prefer it only when it replaces grid energy costing at least as much.
func (s *Service) solarEconomicalLocked(now time.Time, soc int) bool {
	reservation := s.chargeReservationLocked(now, soc)
	if reservation.Deadline.IsZero() || !reservation.Feasible {
		return true
	}
	price, known := s.currentPriceLocked(now)
	if !known {
		return false
	}
	for _, window := range reservation.Windows {
		if !price.LessThanOrEqual(window.Price) {
			continue
		}
		return true
	}
	return false
}

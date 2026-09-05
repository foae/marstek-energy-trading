package service

import (
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/internal/config"
)

func reservationFixture() (*Service, time.Time) {
	now := time.Date(2026, 9, 5, 23, 30, 0, 0, time.UTC)
	s := &Service{
		loc:            time.UTC,
		cfg:            &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, ChargePowerW: 1000},
		nowFunc:        func() time.Time { return now },
		currentPlan:    &TradingPlan{Cycles: []TradeCycle{{ChargeWindow: TimeWindow{Start: now, End: now.Add(time.Hour)}}}},
		todayPrices:    []nordpool.Price{{Time: now, Value: .30}, {Time: now.Add(15 * time.Minute), Value: .20}},
		tomorrowPrices: []nordpool.Price{{Time: now.Add(30 * time.Minute), Value: .05}, {Time: now.Add(45 * time.Minute), Value: .10}},
	}
	return s, now
}

func TestReservationUsesCheapestCrossMidnightSlotsAndActualSolar(t *testing.T) {
	s, now := reservationFixture()
	reservation := s.chargeReservationLocked(now, 50)
	if !reservation.Feasible || len(reservation.Windows) != 2 || !reservation.Windows[0].Start.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("expected half kWh in two cheap tomorrow slots: %+v", reservation)
	}
	if s.gridReservedLocked(now, 50) {
		t.Fatal("expensive current slot reserved despite sufficient cheaper future capacity")
	}
	// Actual solar gained 25 percentage points; one previously needed grid slot is released.
	reservation = s.chargeReservationLocked(now, 75)
	if len(reservation.Windows) != 1 || reservation.ReservedKWh != .25 || !reservation.Windows[0].Start.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("actual solar did not free the marginal grid slot: %+v", reservation)
	}
}

func TestSolarOpportunityCostPrefersCheaperFutureGrid(t *testing.T) {
	s, now := reservationFixture()
	if s.solarEconomicalLocked(now, 50) {
		t.Fatal("charging from 30ct export surplus instead of reserving 5/10ct grid energy")
	}
	s.todayPrices[0].Value = .01
	if !s.solarEconomicalLocked(now, 50) {
		t.Fatal("cheap solar should replace more expensive grid energy")
	}
}

func TestReservationReportsTaperInfeasibleWithoutInventingEnergy(t *testing.T) {
	s, now := reservationFixture()
	s.state = StateCharging
	s.currentTradeStart = now.Add(-time.Minute)
	s.observedChargePowerW = 100
	reservation := s.chargeReservationLocked(now, 50)
	if reservation.Feasible || reservation.RequiredKWh != .5 || reservation.ReservedKWh > .100001 {
		t.Fatalf("taper must expose insufficient delivery capacity: %+v", reservation)
	}
	if !s.gridReservedLocked(now, 50) {
		t.Fatal("infeasible deadline must use remaining available time best effort")
	}
}

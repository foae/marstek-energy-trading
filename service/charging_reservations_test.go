package service

import (
	"math"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/internal/config"
	"github.com/shopspring/decimal"
)

func reservationFixture() (*Service, time.Time) {
	now := time.Date(2026, 9, 5, 23, 30, 0, 0, time.UTC)
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.50)},
		}}},
		todayPrices:    []nordpool.Price{{Time: now, Value: .30}, {Time: now.Add(15 * time.Minute), Value: .20}},
		tomorrowPrices: []nordpool.Price{{Time: now.Add(30 * time.Minute), Value: .05}, {Time: now.Add(45 * time.Minute), Value: .10}},
	}
	return s, now
}

func solarEconomicalAt(s *Service, now time.Time, soc int) bool {
	return s.solarEconomicalForReservationLocked(now, s.chargeReservationLocked(now, soc))
}

func TestReservationOnlyUsesSlicesMeetingExpectedProfitThreshold(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	prices := make([]nordpool.Price, 9)
	for i := range prices {
		prices[i] = nordpool.Price{Time: now.Add(time.Duration(i) * 15 * time.Minute), Value: .13}
	}
	prices[0].Value = .30
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: 5.12,
			BatteryEfficiency:  .90,
			MinPriceSpread:     .05,
			ChargePowerW:       2500,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(9 * 15 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.20)},
		}}, ChargeWindows: []TimeWindow{{Start: now, End: now.Add(9 * 15 * time.Minute)}}, IsProfitable: true},
		todayPrices: prices,
	}

	reservation := s.chargeReservationLocked(now, 11)
	if reservation.Feasible {
		t.Fatalf("expected expensive extra slice to leave the reservation infeasible: %+v", reservation)
	}
	if !reservation.LimitedByEconomics {
		t.Fatalf("expected reservation to report its economic limit: %+v", reservation)
	}
	if len(reservation.Windows) != 8 || math.Abs(reservation.ReservedKWh-5) > 0.000001 {
		t.Fatalf("expected only eight profitable 0.13 slots to be reserved: %+v", reservation)
	}
	for _, window := range reservation.Windows {
		if !window.Price.Equal(decimal.NewFromFloat(.13)) {
			t.Fatalf("sub-threshold reservation included price %s", window.Price)
		}
	}
	if s.gridReservedLocked(now, 11) {
		t.Fatal("expensive current slice must not start physical charging")
	}
	s.batteryTelemetryAvailable = true
	s.batteryTelemetrySOC = 11
	status := s.GetCurrentStatus(t.Context())
	if status.ChargeReservation == nil || !status.ChargeReservation.LimitedByEconomics {
		t.Fatalf("status did not expose economic limit: %+v", status.ChargeReservation)
	}
	if status.NextAction != "current charge slice skipped: expected profit below configured minimum" {
		t.Fatalf("next action = %q", status.NextAction)
	}
}

func TestEconomicShortfallDoesNotBypassSolarOpportunityCost(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: 1,
			BatteryEfficiency:  .90,
			MinPriceSpread:     .05,
			ChargePowerW:       1000,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.40)},
		}}},
		todayPrices: []nordpool.Price{
			{Time: now, Value: .45},
			{Time: now.Add(15 * time.Minute), Value: .30},
			{Time: now.Add(30 * time.Minute), Value: .30},
			{Time: now.Add(45 * time.Minute), Value: .30},
		},
	}

	reservation := s.chargeReservationLocked(now, 11)
	if reservation.Feasible || !reservation.LimitedByEconomics {
		t.Fatalf("expected economics-limited reservation: %+v", reservation)
	}
	if solarEconomicalAt(s, now, 11) {
		t.Fatal("solar with 0.45 export value must not charge for a 0.40 discharge at 90% efficiency")
	}
}

func TestTimeLimitedReservationAllowsBestEffortSolarWithCommitment(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	s := &Service{
		cfg:                  testConfigSmallBattery(),
		loc:                  time.UTC,
		todayPrices:          []nordpool.Price{{Time: now, Value: .50}},
		automaticCycleCommit: cycle,
	}
	reservation := chargingReservation{Deadline: cycle.ChargeWindow.End, Feasible: false, LimitedByEconomics: false, pairedCycle: cycle}

	if !s.solarEconomicalForReservationLocked(now, reservation) {
		t.Fatal("time-limited committed reservation rejected best-effort solar")
	}
}

func TestLaterCycleDoesNotCreateReservationBeforeEarlierDischargeCompletes(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	committed := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	later := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: decimal.NewFromFloat(.12)},
		DischargeWindow: TimeWindow{Start: now.Add(5 * time.Hour), End: now.Add(6 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	s := &Service{
		cfg:                  testConfigSmallBattery(),
		loc:                  time.UTC,
		todayPrices:          []nordpool.Price{{Time: now, Value: .10}, {Time: later.ChargeWindow.Start, Value: .12}},
		currentPlan:          &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{committed, later}},
		automaticCycleCommit: &committed,
	}

	reservation := s.chargeReservationLocked(now, 50)
	if !reservation.Deadline.IsZero() || reservation.pairedCycle != nil || len(reservation.Windows) != 0 {
		t.Fatalf("later cycle leaked into pre-discharge reservation: %+v", reservation)
	}
	if !s.solarEconomicalForReservationLocked(now, reservation) {
		t.Fatal("empty later-cycle reservation blocked solar allowed by the committed cycle ceiling")
	}
}

func TestMixedCapacityAndEconomicShortfallRetainsSolarProfitCeiling(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: 1,
			BatteryEfficiency:  .90,
			MinPriceSpread:     .05,
			ChargePowerW:       1000,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(time.Hour)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.40)},
		}}},
		todayPrices: []nordpool.Price{
			{Time: now, Value: .45},
			{Time: now.Add(15 * time.Minute), Value: .30},
			{Time: now.Add(30 * time.Minute), Value: .30},
			{Time: now.Add(45 * time.Minute), Value: .30},
		},
	}

	reservation := s.chargeReservationLocked(now, 0)
	if reservation.Feasible || !reservation.LimitedByEconomics {
		t.Fatalf("expected mixed time/economic shortfall: %+v", reservation)
	}
	if solarEconomicalAt(s, now, 0) {
		t.Fatal("mixed shortfall bypassed the paired cycle's solar profit ceiling")
	}
}

func TestCommittedCycleRetainsSolarProfitCeilingAfterChargeDeadline(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	cycle := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	s := &Service{
		loc:                  time.UTC,
		cfg:                  &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: .90, MinPriceSpread: .05, ChargePowerW: 1000},
		nowFunc:              func() time.Time { return now },
		currentPlan:          &TradingPlan{Cycles: []TradeCycle{cycle}},
		automaticCycleCommit: &cycle,
		todayPrices:          []nordpool.Price{{Time: now, Value: .45}},
	}

	if reservation := s.chargeReservationLocked(now, 50); !reservation.Deadline.IsZero() {
		t.Fatalf("post-deadline reservation unexpectedly active: %+v", reservation)
	}
	if solarEconomicalAt(s, now, 50) {
		t.Fatal("expensive solar bypassed the committed cycle ceiling after its charge deadline")
	}
}

func TestCommittedCycleSolarCeilingPrecedesLaterCycleReservation(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	committed := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	later := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(5 * time.Hour), End: now.Add(6 * time.Hour), Price: decimal.NewFromFloat(.50)},
	}
	s := &Service{
		loc:                  time.UTC,
		cfg:                  &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: .90, MinPriceSpread: .05, ChargePowerW: 1000},
		nowFunc:              func() time.Time { return now },
		currentPlan:          &TradingPlan{Cycles: []TradeCycle{committed, later}},
		automaticCycleCommit: &committed,
		todayPrices:          []nordpool.Price{{Time: now, Value: .45}},
	}

	if reservation := s.chargeReservationLocked(now, 50); !reservation.Deadline.IsZero() {
		t.Fatalf("later cycle leaked into the committed cycle's pre-discharge interval: %+v", reservation)
	}
	if solarEconomicalAt(s, now, 50) {
		t.Fatal("later reservation bypassed the earlier committed cycle's solar ceiling")
	}
	if !s.solarBlockedLocked(now, 50) {
		t.Fatal("physical solar entry was not blocked by the committed cycle ceiling")
	}
}

func TestReservationRejectsZeroProfitSliceAtZeroThreshold(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: .5, MinPriceSpread: 0, ChargePowerW: 1000},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(15 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.20)},
		}}},
		todayPrices: []nordpool.Price{{Time: now, Value: .10}},
	}

	reservation := s.chargeReservationLocked(now, 90)
	if len(reservation.Windows) != 0 || !reservation.LimitedByEconomics {
		t.Fatalf("zero-profit slice was reserved: %+v", reservation)
	}
}

func TestSolarRejectsZeroProfitAtZeroThreshold(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cycle := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.20)},
	}
	s := &Service{
		loc:                  time.UTC,
		cfg:                  &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: .5, MinPriceSpread: 0, ChargePowerW: 1000},
		nowFunc:              func() time.Time { return now },
		currentPlan:          &TradingPlan{Cycles: []TradeCycle{cycle}},
		automaticCycleCommit: &cycle,
		todayPrices:          []nordpool.Price{{Time: now, Value: .10}},
	}

	if solarEconomicalAt(s, now, 50) {
		t.Fatal("solar with exact break-even opportunity cost was accepted")
	}
}

func TestStatusWaitsForFeasibleReservationWhenCurrentSliceIsExcluded(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(45 * time.Minute)},
			DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.20)},
		}}},
		todayPrices: []nordpool.Price{
			{Time: now, Value: .30},
			{Time: now.Add(15 * time.Minute), Value: .05},
			{Time: now.Add(30 * time.Minute), Value: .05},
		},
		batteryTelemetryAvailable: true,
		batteryTelemetrySOC:       50,
	}

	reservation := s.chargeReservationLocked(now, 50)
	if !reservation.Feasible || !reservation.currentPriceTooHigh {
		t.Fatalf("expected feasible future reservation with excluded current slice: %+v", reservation)
	}
	if status := s.GetCurrentStatus(t.Context()); status.NextAction != "waiting for reserved charge window" {
		t.Fatalf("next action = %q, want waiting for reserved charge window", status.NextAction)
	}
}

func TestFeasibleReservationIsNotMarkedEconomicsLimited(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: 1,
			BatteryEfficiency:  1,
			MinPriceSpread:     .05,
			ChargePowerW:       1000,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(45 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.20)},
		}}, ChargeWindows: []TimeWindow{{Start: now, End: now.Add(45 * time.Minute)}}, IsProfitable: true},
		todayPrices: []nordpool.Price{
			{Time: now, Value: .12},
			{Time: now.Add(15 * time.Minute), Value: .05},
			{Time: now.Add(30 * time.Minute), Value: .30},
		},
		batteryTelemetryAvailable: true,
		batteryTelemetrySOC:       90,
	}

	reservation := s.chargeReservationLocked(now, 90)
	if !reservation.Feasible || reservation.LimitedByEconomics {
		t.Fatalf("unrelated expensive slice mislabeled a feasible reservation: %+v", reservation)
	}
	status := s.GetCurrentStatus(t.Context())
	if status.NextAction != "waiting for reserved charge window" {
		t.Fatalf("next action = %q, want deferred reservation", status.NextAction)
	}
}

func TestReservationUsesDischargePriceFromMatchingCycle(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: 1,
			BatteryEfficiency:  .90,
			MinPriceSpread:     .05,
			ChargePowerW:       1000,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{
			{
				ChargeWindow:    TimeWindow{End: now.Add(-2 * time.Hour)},
				DischargeWindow: TimeWindow{End: now, Price: decimal.NewFromFloat(.50)},
			},
			{
				ChargeWindow:    TimeWindow{Start: now, End: now.Add(15 * time.Minute)},
				DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.20)},
			},
		}},
		todayPrices: []nordpool.Price{{Time: now, Value: .14}},
	}

	reservation := s.chargeReservationLocked(now, 90)
	if len(reservation.Windows) != 0 || !reservation.LimitedByEconomics {
		t.Fatalf("current slice was not checked against the matching cycle: %+v", reservation)
	}
}

func TestReservationProfitFloorHoldsAcrossControlTicks(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	clock := now
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh: .5,
			BatteryEfficiency:  .90,
			MinPriceSpread:     .05,
			ChargePowerW:       1000,
		},
		nowFunc: func() time.Time { return clock },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(45 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.20)},
		}}},
		todayPrices: []nordpool.Price{
			{Time: now, Value: .13},
			{Time: now.Add(15 * time.Minute), Value: .14},
			{Time: now.Add(30 * time.Minute), Value: .12},
		},
	}

	checks := []struct {
		at       time.Time
		soc      int
		reserved bool
	}{
		{at: now, soc: 50, reserved: true},
		{at: now.Add(15 * time.Minute), soc: 75, reserved: false},
		{at: now.Add(30 * time.Minute), soc: 75, reserved: true},
	}
	maxPrice := decimal.NewFromFloat(.13)
	for _, check := range checks {
		clock = check.at
		reservation := s.chargeReservationLocked(clock, check.soc)
		for _, window := range reservation.Windows {
			if window.Price.GreaterThan(maxPrice) {
				t.Fatalf("tick at %s reserved sub-threshold price %s", clock, window.Price)
			}
		}
		if got := s.gridReservedLocked(clock, check.soc); got != check.reserved {
			t.Fatalf("gridReservedLocked(%s, %d) = %t, want %t", clock, check.soc, got, check.reserved)
		}
	}
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
	if solarEconomicalAt(s, now, 50) {
		t.Fatal("charging from 30ct export surplus instead of reserving 5/10ct grid energy")
	}
	s.todayPrices[0].Value = .01
	if !solarEconomicalAt(s, now, 50) {
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
	if !solarEconomicalAt(s, now, 50) {
		t.Fatal("taper-only infeasibility must continue capturing solar")
	}
}

func TestSolarRetentionCeilingStartsAtChargeDeadline(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
	}
	s := &Service{
		cfg:                 &config.Config{BatteryEfficiency: .90, MinPriceSpread: .05},
		solarCycleRetention: cycle,
	}
	reservation := chargingReservation{Deadline: cycle.ChargeWindow.End, Feasible: false, LimitedByEconomics: false}

	if !s.solarEconomicalForReservationLocked(now, reservation) {
		t.Fatal("live retention overrode time/taper-only solar admission before the charge deadline")
	}
	if s.solarEconomicalForReservationLocked(cycle.ChargeWindow.End, reservation) {
		t.Fatal("retained cycle ceiling was not enforced after the charge deadline with no known tariff")
	}
}

func TestReservationDeduplicatesOverlappingPriceCalendars(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	price := nordpool.Price{Time: now, Value: .10}
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: now, End: now.Add(15 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.30)},
		}}},
		todayPrices:    []nordpool.Price{price},
		tomorrowPrices: []nordpool.Price{price},
	}

	reservation := s.chargeReservationLocked(now, 50)
	if reservation.Feasible || len(reservation.Windows) != 1 || reservation.ReservedKWh != .25 {
		t.Fatalf("duplicate calendar slot was counted more than once: %+v", reservation)
	}
}

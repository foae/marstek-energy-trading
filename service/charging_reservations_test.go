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
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000, ChargePlanningDerate: 1},
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
			BatteryCapacityKWh:      5.12,
			BatteryEfficiency:       .90,
			BatteryChargeEfficiency: .90,
			MinPriceSpread:          .05,
			ChargePowerW:            2500,
			ChargePlanningDerate:    1,
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

func TestEconomicShortfallDoesNotBlockSolarWithoutActiveReservation(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh:   1,
			BatteryEfficiency:    .90,
			MinPriceSpread:       .05,
			ChargePowerW:         1000,
			ChargePlanningDerate: 1,
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
	if s.solarBlockedLocked(now, 11) {
		t.Fatal("economic shortfall without an active reservation must not block solar charging")
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
	if s.solarBlockedLocked(now, 50) {
		t.Fatal("without a reservation or discharge, solar must remain eligible")
	}
}

func TestReservationRejectsZeroProfitSliceAtZeroThreshold(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: .5, MinPriceSpread: 0, ChargePowerW: 1000, ChargePlanningDerate: 1},
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

func TestStatusWaitsForFeasibleReservationWhenCurrentSliceIsExcluded(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000, ChargePlanningDerate: 1},
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
			BatteryCapacityKWh:   1,
			BatteryEfficiency:    1,
			MinPriceSpread:       .05,
			ChargePowerW:         1000,
			ChargePlanningDerate: 1,
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
			BatteryCapacityKWh:   1,
			BatteryEfficiency:    .90,
			MinPriceSpread:       .05,
			ChargePowerW:         1000,
			ChargePlanningDerate: 1,
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
			BatteryCapacityKWh:      .5,
			BatteryEfficiency:       .90,
			BatteryChargeEfficiency: .90,
			MinPriceSpread:          .05,
			ChargePowerW:            1000,
			ChargePlanningDerate:    1,
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

func TestReservationDeduplicatesOverlappingPriceCalendars(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	price := nordpool.Price{Time: now, Value: .10}
	s := &Service{
		loc:     time.UTC,
		cfg:     &config.Config{BatteryCapacityKWh: 1, BatteryEfficiency: 1, MinPriceSpread: .05, ChargePowerW: 1000, ChargePlanningDerate: 1},
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

// continuationFixture builds a reservation where the currently running slice is
// the marginal (most expensive selected) one, as happens whenever prices fall
// slot by slot, so the selection loop truncates the slice being charged.
func continuationFixture(state State, prices []float64) (*Service, time.Time) {
	slotStart := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	now := slotStart.Add(5 * time.Minute)
	today := make([]nordpool.Price, len(prices))
	for i, value := range prices {
		today[i] = nordpool.Price{Time: slotStart.Add(time.Duration(i) * 15 * time.Minute), Value: value}
	}
	s := &Service{
		loc:   time.UTC,
		state: state,
		cfg: &config.Config{
			BatteryCapacityKWh:      1.1,
			BatteryEfficiency:       1,
			BatteryChargeEfficiency: 1,
			MinPriceSpread:          .05,
			ChargePowerW:            1000,
			ChargePlanningDerate:    1,
		},
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: slotStart, End: slotStart.Add(45 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.50)},
		}}},
		todayPrices: today,
	}
	return s, now
}

// deferralFixture builds an idle service whose whole price series is a single
// charge window, so the allocator alone decides which slices are reserved.
func deferralFixture(prices []float64, capacityKWh float64) (*Service, time.Time) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	today := make([]nordpool.Price, len(prices))
	for i, value := range prices {
		today[i] = nordpool.Price{Time: base.Add(time.Duration(i) * 15 * time.Minute), Value: value}
	}
	s := &Service{
		loc: time.UTC,
		cfg: &config.Config{
			BatteryCapacityKWh:            capacityKWh,
			BatteryEfficiency:             1,
			BatteryChargeEfficiency:       1,
			MinPriceSpread:                .05,
			ChargePowerW:                  1000,
			ChargePlanningDerate:          1,
			ChargeDeferToleranceEURPerKWh: .01,
		},
		nowFunc: func() time.Time { return base },
		currentPlan: &TradingPlan{Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Duration(len(prices)) * 15 * time.Minute)},
			DischargeWindow: TimeWindow{Price: decimal.NewFromFloat(.50)},
		}}},
		todayPrices: today,
		meter:       NewMockMeter(true, 0),
	}
	return s, base
}

// cheapAndExpensive returns four hours of quarter-hour prices: cheap until the
// final hour, which is priced beyond the deferral tolerance.
func cheapAndExpensive() []float64 {
	prices := make([]float64, 16)
	for i := range prices {
		prices[i] = .13
		if i >= 12 {
			prices[i] = .18
		}
	}
	return prices
}

func reservationSpan(reservation chargingReservation) (time.Time, time.Time) {
	return reservation.Windows[0].Start, reservation.Windows[len(reservation.Windows)-1].End
}

func TestReservationDefersToLatestAffordableSlices(t *testing.T) {
	// 2 kWh at 1 kW is two hours; the cheap slices end at 03:00, so the latest
	// affordable selection is 01:00-03:00, not the earliest 00:00-02:00.
	s, base := deferralFixture(cheapAndExpensive(), 4)

	reservation := s.chargeReservationLocked(base, 50)

	if !reservation.Feasible || math.Abs(reservation.ReservedKWh-2) > 1e-9 {
		t.Fatalf("reservation = %+v, want a feasible 2 kWh reservation", reservation)
	}
	start, end := reservationSpan(reservation)
	if !start.Equal(base.Add(time.Hour)) || !end.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("reserved span = %s-%s, want 01:00-03:00", start, end)
	}
	for _, window := range reservation.Windows {
		if !window.Price.Equal(decimal.NewFromFloat(.13)) {
			t.Fatalf("reserved a slice above the deferral price cap: %+v", window)
		}
	}
}

func TestReservationTruncatesEarliestDeferredSliceAtItsStart(t *testing.T) {
	// 1.4 kWh is five whole quarters plus nine minutes: the earliest reserved
	// piece keeps its slot's end and starts late.
	s, base := deferralFixture(cheapAndExpensive(), 2.8)

	reservation := s.chargeReservationLocked(base, 50)

	if !reservation.Feasible || math.Abs(reservation.ReservedKWh-1.4) > 1e-9 {
		t.Fatalf("reservation = %+v, want a feasible 1.4 kWh reservation", reservation)
	}
	start, end := reservationSpan(reservation)
	if !start.Equal(base.Add(96*time.Minute)) || !end.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("reserved span = %s-%s, want 01:36-03:00", start, end)
	}
	if got := reservation.Windows[0].End; !got.Equal(base.Add(105 * time.Minute)) {
		t.Fatalf("truncated piece end = %s, want its slot end 01:45", got)
	}
}

func TestReservationFillsShortfallFromCheapestRemainingSlices(t *testing.T) {
	// One cheap hour, then an expensive hour, then a middling one. The affordable
	// slices hold 1 kWh of the 1.5 kWh requirement; the rest comes from the
	// cheapest remaining slices, not from the chronologically nearer expensive ones.
	prices := []float64{.13, .13, .13, .13, .30, .30, .30, .30, .20, .20, .20, .20}
	s, base := deferralFixture(prices, 3)

	reservation := s.chargeReservationLocked(base, 50)

	if !reservation.Feasible || math.Abs(reservation.ReservedKWh-1.5) > 1e-9 {
		t.Fatalf("reservation = %+v, want a feasible 1.5 kWh reservation", reservation)
	}
	for _, window := range reservation.Windows {
		if window.Price.Equal(decimal.NewFromFloat(.30)) {
			t.Fatalf("shortfall was taken from the expensive slices: %+v", reservation.Windows)
		}
	}
	last := reservation.Windows[len(reservation.Windows)-1]
	if !last.End.Equal(base.Add(150 * time.Minute)) {
		t.Fatalf("shortfall span ends at %s, want 02:30 of the cheapest remaining slices", last.End)
	}
}

func TestReservationRunsForwardWhileCharging(t *testing.T) {
	prices := make([]float64, 16)
	for i := range prices {
		prices[i] = .13
	}
	s, base := deferralFixture(prices, 2)
	s.state = StateCharging
	now := base.Add(20 * time.Minute)
	s.nowFunc = func() time.Time { return now }

	reservation := s.chargeReservationLocked(now, 50)

	if !reservation.contains(now) {
		t.Fatalf("running slice not reserved: %+v", reservation.Windows)
	}
	if !reservation.Windows[0].Start.Equal(now) {
		t.Fatalf("selection starts at %s, want the running slice at %s", reservation.Windows[0].Start, now)
	}
	for i := 1; i < len(reservation.Windows); i++ {
		if !reservation.Windows[i].Start.Equal(reservation.Windows[i-1].End) {
			t.Fatalf("selection is not contiguous: %+v", reservation.Windows)
		}
	}
	_, end := reservationSpan(reservation)

	// Rising SOC shrinks the tail, never the head.
	shrunk := s.chargeReservationLocked(now, 75)
	if !shrunk.contains(now) || !shrunk.Windows[0].Start.Equal(now) {
		t.Fatalf("rising SOC moved the running head: %+v", shrunk.Windows)
	}
	_, shrunkEnd := reservationSpan(shrunk)
	if !shrunkEnd.Before(end) {
		t.Fatalf("reserved tail = %s, want it shorter than %s", shrunkEnd, end)
	}
}

func TestReservationKeepsRunningSliceAbovePriceCap(t *testing.T) {
	prices := make([]float64, 16)
	for i := range prices {
		prices[i] = .13
	}
	prices[1] = .30
	s, base := deferralFixture(prices, 2)
	s.state = StateCharging
	now := base.Add(20 * time.Minute)
	s.nowFunc = func() time.Time { return now }

	reservation := s.chargeReservationLocked(now, 50)

	if !reservation.Windows[0].Start.Equal(now) || !reservation.Windows[0].Price.Equal(decimal.NewFromFloat(.30)) {
		t.Fatalf("expensive running slice was dropped: %+v", reservation.Windows)
	}
}

func TestReservationSizesAtDeratedPower(t *testing.T) {
	prices := make([]float64, 16)
	for i := range prices {
		prices[i] = .13
	}
	s, base := deferralFixture(prices, 1)

	full := s.chargeReservationLocked(base, 50)
	s.cfg.ChargePlanningDerate = .5
	derated := s.chargeReservationLocked(base, 50)

	fullStart, fullEnd := reservationSpan(full)
	deratedStart, deratedEnd := reservationSpan(derated)
	if fullEnd.Sub(fullStart) != 30*time.Minute {
		t.Fatalf("nameplate reservation = %s, want 30m", fullEnd.Sub(fullStart))
	}
	if deratedEnd.Sub(deratedStart) != time.Hour {
		t.Fatalf("derated reservation = %s, want 1h", deratedEnd.Sub(deratedStart))
	}
}

func TestReservationInfeasibleUnchangedByDeferral(t *testing.T) {
	// Only the first two slices clear the expected-profit floor, so the 2 kWh
	// requirement cannot be met and the shortfall is economic.
	s, base := deferralFixture([]float64{.13, .13, .50, .50}, 4)

	reservation := s.chargeReservationLocked(base, 50)

	if reservation.Feasible {
		t.Fatalf("reservation = %+v, want infeasible", reservation)
	}
	if !reservation.LimitedByEconomics {
		t.Fatalf("reservation = %+v, want the shortfall attributed to economics", reservation)
	}
	if math.Abs(reservation.ReservedKWh-.5) > 1e-9 {
		t.Fatalf("ReservedKWh = %f, want the eligible 0.5 kWh", reservation.ReservedKWh)
	}
}

func TestReservationUsesCheapestSlicesWithoutMeter(t *testing.T) {
	// Without a P1 meter, SOC cannot rise while waiting, so deferral can only
	// cost money: the earliest cheap slices are reserved as before.
	s, base := deferralFixture(cheapAndExpensive(), 4)
	s.meter = nil

	reservation := s.chargeReservationLocked(base, 50)

	if !reservation.Feasible || math.Abs(reservation.ReservedKWh-2) > 1e-9 {
		t.Fatalf("reservation = %+v, want a feasible 2 kWh reservation", reservation)
	}
	start, end := reservationSpan(reservation)
	if !start.Equal(base) || !end.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("reserved span = %s-%s, want the cheapest 00:00-02:00", start, end)
	}
}

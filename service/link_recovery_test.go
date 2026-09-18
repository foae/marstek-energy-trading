package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Charging credit must survive the multi-second HTTP polls ESPHome answers
// with in practice; only a gap beyond the tolerance earns nothing.
func TestInventoryCreditsAcrossShortSampleGaps(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(50)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.inventory = dischargeInventory{}
	svc.state = StateCharging
	battery.CurrentPower = 1000
	for i := 0; i <= 6; i++ {
		svc.sampleDischargeInventory(context.Background())
		now = now.Add(10 * time.Second)
	}
	if math.Abs(svc.inventory.remainingDCKWh-60.0/3600) > 1e-9 {
		t.Fatalf("60 s at 1000 W sampled every 10 s credited %g kWh, want %g", svc.inventory.remainingDCKWh, 60.0/3600)
	}
	now = now.Add(inventoryCreditGapTolerance)
	svc.sampleDischargeInventory(context.Background())
	if math.Abs(svc.inventory.remainingDCKWh-60.0/3600) > 1e-9 {
		t.Fatalf("gap beyond the tolerance credited inventory: %g", svc.inventory.remainingDCKWh)
	}
}

// The debit estimates the DC draw from AC watts over the discharge-side
// efficiency (round-trip over charge-side), not over the round-trip figure.
func TestInventoryDebitUsesDischargeSideEfficiency(t *testing.T) {
	now := time.Now()
	cfg := testConfig()
	cfg.BatteryEfficiency = 0.79
	cfg.BatteryChargeEfficiency = 0.95
	battery := NewMockBattery(80)
	svc := newTestService(cfg, battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.inventory = dischargeInventory{remainingDCKWh: 3}
	svc.batteryTelemetryAvailable = false

	svc.mu.Lock()
	_, err := svc.prepareDischargeInventoryLocked(80, 2200, true, now.Add(2*time.Hour))
	svc.mu.Unlock()
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	want := 2200 / (0.79 / 0.95)
	if math.Abs(svc.inventory.debitPowerW-want) > 1e-6 {
		t.Fatalf("debit rate = %g W, want %g W", svc.inventory.debitPowerW, want)
	}
}

// A SOC that moves on a live link is a fresh measurement of the battery and
// may raise an in-flight allowance up to its cap; a SOC that stops moving, or
// one read over a link reported down, cannot.
func TestInventoryLiveSOCReplenishesInFlightSaleOnly(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(60)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.inventory = dischargeInventory{remainingDCKWh: 0.5}
	svc.state = StateDischarging
	windowEnd := now.Add(3 * time.Hour)

	svc.mu.Lock()
	svc.cacheBatteryTelemetryLocked(60, -2500)
	deadline, err := svc.prepareDischargeInventoryLocked(60, 2200, true, windowEnd)
	svc.mu.Unlock()
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if deadline.After(now.Add(15 * time.Minute)) {
		t.Fatalf("0.5 kWh allowance should end within minutes, got deadline %s", deadline.Sub(now))
	}

	now = now.Add(90 * time.Second)
	svc.mu.Lock()
	svc.cacheBatteryTelemetryLocked(59, -2500) // SOC moved on a live link
	svc.mu.Unlock()
	if got, want := svc.inventory.remainingDCKWh, svc.inventorySOCCap(59); math.Abs(got-want) > 1e-9 {
		t.Fatalf("live SOC did not replenish allowance: got %g, want cap %g", got, want)
	}
	if !svc.inventory.deadline.After(deadline) || svc.inventory.deadline.After(windowEnd) {
		t.Fatalf("deadline not extended within the window: %s (was %s, window end %s)", svc.inventory.deadline, deadline, windowEnd)
	}

	// A frozen link invalidates the allowance; the unchanged SOC afterwards
	// must not rebuild it even though the SOC moved recently.
	now = now.Add(30 * time.Second)
	svc.mu.Lock()
	svc.linkDownSince = now
	svc.invalidateDischargeInventoryLocked()
	svc.cacheBatteryTelemetryLocked(59, -2500)
	svc.mu.Unlock()
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("frozen link allowed replenishment: %g", svc.inventory.remainingDCKWh)
	}

	// The link returns but the SOC stops moving: after the live window the
	// stale value is only a cap again.
	svc.mu.Lock()
	svc.linkDownSince = time.Time{}
	svc.inventory.remainingDCKWh = 0.2
	svc.mu.Unlock()
	now = now.Add(inventorySOCLiveWindow + time.Second)
	svc.mu.Lock()
	svc.cacheBatteryTelemetryLocked(59, -2500)
	svc.mu.Unlock()
	if svc.inventory.remainingDCKWh > 0.2 {
		t.Fatalf("stale SOC replenished allowance: %g", svc.inventory.remainingDCKWh)
	}
}

// A frozen link keeps serving the last Modbus reading. Energy after detection
// is booked as zero and the seconds are recorded as a telemetry gap.
func TestTickBooksFrozenLinkAsTelemetryGap(t *testing.T) {
	battery := NewMockBattery(64)
	battery.CurrentPower = -2000
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)
	svc.inventory = dischargeInventory{remainingDCKWh: 4, inFlight: true}
	svc.activeInventorySale = &TimeWindow{Start: now, End: now.Add(30 * time.Minute), Price: decimal.NewFromFloat(.22)}
	ctx := context.Background()

	svc.tick(ctx) // first sample anchors the integration
	now = now.Add(time.Minute)
	battery.checkLinkErr = linkDownErr()
	svc.tick(ctx) // books one live minute, then detects the freeze
	now = now.Add(time.Minute)
	svc.tick(ctx) // frozen minute: zero energy, one minute of gap

	if got, want := svc.currentTradeEnergyWs, 2000.0*60; math.Abs(got-want) > 1 {
		t.Fatalf("energy booked %g Ws, want %g (only the live minute)", got, want)
	}
	if svc.currentTradeTelemetryGapS != 60 {
		t.Fatalf("telemetry gap = %d s, want 60", svc.currentTradeTelemetryGapS)
	}

	battery.checkLinkErr = nil
	battery.SOC = 5 // below the floor: the next tick stops and records the trade
	now = now.Add(time.Minute)
	svc.tick(ctx)
	if svc.state != StateIdle {
		t.Fatalf("expected the session to stop, state=%s", svc.state)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) == 0 || len(history.Days[len(history.Days)-1].Trades) == 0 {
		t.Fatal("no trade recorded")
	}
	trade := history.Days[len(history.Days)-1].Trades[len(history.Days[len(history.Days)-1].Trades)-1]
	if trade.TelemetryGapS < 60 {
		t.Fatalf("trade telemetry_gap_s = %d, want at least 60", trade.TelemetryGapS)
	}
	if energy, _ := trade.EnergyKWh.Float64(); energy > 2000.0*60/3_600_000+1e-6 {
		t.Fatalf("frozen reading was booked as energy: %g kWh", energy)
	}
}

// Integer SOC a few points above the floor is a sliver worth cents; the 5%
// delivery share admits a sale only from about 16% SOC on this battery.
func TestInventorySaleRejectsSliverAboveMinimumSOC(t *testing.T) {
	base := time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC)
	prices := plannerPrices(base, []float64{.40, .40, .40, .40}, []float64{.40, .40, .40, .40})
	for soc, wantSale := range map[int]bool{12: false, 13: false, 15: false, 16: true, 20: true} {
		cfg := AnalyzerConfig{
			Efficiency: 0.79, ChargeEfficiency: 0.95, MinPriceSpread: .05,
			BatteryCapacityKWh: 5.12, BatteryMinSOC: 0.11,
			ChargePowerW: 2200, ChargePlanningDerate: 1, DischargePowerW: 2200, MaxCyclesPerDay: 2,
			Now: base, InitialSOC: soc, InitialSOCKnown: true,
		}
		available := cfg.BatteryCapacityKWh * float64(soc-11) / 100
		cfg.AvailableInventoryDCKWh = &available
		plan := AnalyzePrices(prices, cfg)
		if (plan.InventorySale != nil) != wantSale {
			t.Fatalf("SOC %d%%: sale=%+v, want sale=%v", soc, plan.InventorySale, wantSale)
		}
	}
}

// The one-second scheduled-charge sampler feeds the P1 source split; it must
// honor the same frozen-link gate as the minute tick.
func TestScheduledChargeSamplerBooksZeroWhileLinkFrozen(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	battery.CurrentPower = 2000
	meter := NewMockMeter(true, 1500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, makePrices(baseTime, .10, .25), now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateCharging
	svc.currentTradeStart = now
	svc.mu.Lock()
	svc.beginMeasuredTradeLocked(2000)
	svc.scheduledChargeAccounting.Begin(now, 2000, true, 1500, true)
	svc.mu.Unlock()

	now = now.Add(10 * time.Second)
	svc.sampleScheduledCharge(context.Background())
	live := svc.currentTradeEnergyWs
	if live <= 0 {
		t.Fatalf("live sample booked no energy")
	}
	svc.mu.Lock()
	svc.linkDownSince = now
	svc.mu.Unlock()
	now = now.Add(10 * time.Second)
	svc.sampleScheduledCharge(context.Background()) // integrates the last live sample through detection
	settled := svc.currentTradeEnergyWs
	now = now.Add(10 * time.Second)
	svc.sampleScheduledCharge(context.Background()) // frozen interval: nothing
	if svc.currentTradeEnergyWs != settled {
		t.Fatalf("frozen link booked energy in the sampler: %g -> %g", settled, svc.currentTradeEnergyWs)
	}
	if svc.currentTradeTelemetryGapS < 19 {
		t.Fatalf("telemetry gap = %d s, want about 20", svc.currentTradeTelemetryGapS)
	}
}

// The sliver floor is a share of a physically full delivery, so a mostly full
// battery with little trusted inventory still cannot start a sliver sale.
func TestInventorySaleFloorIgnoresQuarantinedEnergy(t *testing.T) {
	base := time.Date(2026, 9, 17, 7, 0, 0, 0, time.UTC)
	prices := plannerPrices(base, []float64{.40, .40, .40, .40}, []float64{.40, .40, .40, .40})
	cfg := AnalyzerConfig{
		Efficiency: 0.79, ChargeEfficiency: 0.95, MinPriceSpread: .05,
		BatteryCapacityKWh: 5.12, BatteryMinSOC: 0.11,
		ChargePowerW: 2200, ChargePlanningDerate: 1, DischargePowerW: 2200, MaxCyclesPerDay: 2,
		Now: base, InitialSOC: 100, InitialSOCKnown: true,
	}
	trusted := 0.05
	cfg.AvailableInventoryDCKWh = &trusted
	if plan := AnalyzePrices(prices, cfg); plan.InventorySale != nil {
		t.Fatalf("0.05 kWh of trusted inventory in a full battery selected a sliver sale: %+v", plan.InventorySale)
	}
}

// A start that fails and whose cleanup stop also fails on a dead link must
// back off like any other dead-link stop, not retry every five seconds.
func TestStartFailureCleanupOnDeadLinkArmsLinkDownBackoff(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)
	battery := NewMockBattery(50)
	battery.ChargeErr = linkDownErr()
	battery.IdleErr = linkDownErr()
	now := baseTime
	svc := newTestService(testConfigSmallBattery(), battery, prices, now)
	svc.nowFunc = func() time.Time { return now }

	svc.tick(context.Background())
	if svc.state != StateStopping || !svc.lastStopLinkDown {
		t.Fatalf("expected a pending stop with the link-down backoff armed: state=%s linkDown=%v", svc.state, svc.lastStopLinkDown)
	}
	if got := svc.stopRetryDelay(); got != batteryLinkDownStopRetryInterval {
		t.Fatalf("stopRetryDelay() = %s, want %s", got, batteryLinkDownStopRetryInterval)
	}
}

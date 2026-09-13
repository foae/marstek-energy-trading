package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestChargeSessionRecordsACMeasuredEnergy(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = 2085
	battery.ACPower = 2192
	battery.ACPowerSet = true
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.10, 0.10, 0.10, 0.10), now)
	svc.nowFunc = func() time.Time { return now }
	setReservedChargePlan(svc, baseTime, decimal.RequireFromString("0.10"))

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.RequireFromString("0.10"), 50)
	svc.mu.Unlock()
	now = now.Add(30 * time.Second)
	svc.mu.Lock()
	svc.stopChargingLocked(context.Background(), 50)
	svc.mu.Unlock()

	trade := svc.recorder.GetHistory().Days[0].Trades[0]
	wantEnergy := decimal.NewFromFloat(2192 * 30.0 / 3_600_000)
	if !trade.EnergyKWh.Equal(wantEnergy) {
		t.Fatalf("charge energy = %s, want %s (AC power, not DC 2085 W)", trade.EnergyKWh, wantEnergy)
	}
	if trade.EnergyBasis != measuredACPowerEnergyBasis {
		t.Fatalf("energy basis = %q, want %q", trade.EnergyBasis, measuredACPowerEnergyBasis)
	}
}

func TestDischargeSessionRecordsACMeasuredEnergy(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(80)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = -2505
	battery.ACPower = -2200
	battery.ACPowerSet = true
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.40, 0.40, 0.40, 0.40), now)
	svc.nowFunc = func() time.Time { return now }

	svc.mu.Lock()
	svc.startDischargingLocked(context.Background(), decimal.RequireFromString("0.40"), true, 80, 2500, StateManualDischarging)
	svc.mu.Unlock()
	now = now.Add(30 * time.Second)
	svc.mu.Lock()
	svc.stopDischargingLocked(context.Background(), 80)
	svc.mu.Unlock()

	trade := svc.recorder.GetHistory().Days[0].Trades[0]
	wantEnergy := decimal.NewFromFloat(2200 * 30.0 / 3_600_000)
	if !trade.EnergyKWh.Equal(wantEnergy) {
		t.Fatalf("discharge energy = %s, want %s (AC power, not DC 2505 W)", trade.EnergyKWh, wantEnergy)
	}
	if trade.EnergyBasis != measuredACPowerEnergyBasis {
		t.Fatalf("energy basis = %q, want %q", trade.EnergyBasis, measuredACPowerEnergyBasis)
	}
}

func TestACPowerReadFailureStopsActiveCharge(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = 2000
	battery.ACPower = 2100
	battery.ACPowerSet = true
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.10, 0.10, 0.10, 0.10), now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier
	setReservedChargePlan(svc, baseTime, decimal.RequireFromString("0.10"))

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.RequireFromString("0.10"), 50)
	svc.mu.Unlock()

	battery.GetACPowerErr = errors.New("AC power unavailable")
	now = now.Add(30 * time.Second)
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("state after AC telemetry failure = %s, want idle", svc.state)
	}
	if status := svc.GetCurrentStatus(context.Background()); status.BatteryAvailable {
		t.Fatalf("failed AC read left cached telemetry available: %+v", status)
	}
	if len(notifier.ErrorCalls) == 0 {
		t.Fatal("expected AC telemetry failure notification")
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one recorded charge, got %+v", history.Days)
	}
}

func TestSolarSessionRecordsACMeasuredEnergy(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = 1200
	battery.ACPower = 1300
	battery.ACPowerSet = true
	meter := NewMockMeter(true, 500) // net import of 500 W
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 700
	svc.solarLastUpdate = now
	svc.lastPassiveRefresh = now

	svc.solarTick(context.Background())
	if svc.state != StateSolarCharging {
		t.Fatalf("state after solar tick = %s, want solar charging", svc.state)
	}
	if svc.solarMeasuredChargePowerW != 1300 {
		t.Fatalf("solar measured charge power = %v, want 1300 (AC)", svc.solarMeasuredChargePowerW)
	}
	if svc.solarGridPowerW != 500 {
		t.Fatalf("solar grid power = %v, want 500 (min of AC charge power and net import)", svc.solarGridPowerW)
	}

	now = now.Add(60 * time.Second)
	svc.mu.Lock()
	svc.stopSolarChargingLocked(context.Background(), 50, solarStopReasonYieldWindow)
	svc.mu.Unlock()

	trade := svc.recorder.GetHistory().Days[0].Trades[0]
	wantEnergy := decimal.NewFromFloat(1300 * 60.0 / 3_600_000)
	if !trade.EnergyKWh.Equal(wantEnergy) {
		t.Fatalf("solar battery energy = %s, want %s (AC power, not DC 1200 W)", trade.EnergyKWh, wantEnergy)
	}
	wantGrid := decimal.NewFromFloat(500 * 60.0 / 3_600_000)
	if !trade.GridEnergyKWh.Equal(wantGrid) {
		t.Fatalf("solar grid energy = %s, want %s", trade.GridEnergyKWh, wantGrid)
	}
	if trade.EnergyBasis != measuredACPowerEnergyBasis {
		t.Fatalf("energy basis = %q, want %q", trade.EnergyBasis, measuredACPowerEnergyBasis)
	}
}

func TestChargeStartSeedsFromDCWhenACReadFails(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = 2000
	battery.ACPower = 2100
	battery.ACPowerSet = true
	// Fail only the start-of-session AC read: the tick-level read before the
	// charge command must succeed so the session is allowed to start.
	battery.ChargeHook = func() { battery.GetACPowerErr = errors.New("AC power unavailable") }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.05, 0.05, 0.05, 0.05), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)
	setReservedChargePlan(svc, baseTime, decimal.NewFromFloat(0.05))

	svc.tick(context.Background())
	battery.GetACPowerErr = nil

	if svc.state != StateCharging {
		t.Fatalf("state after AC read failure at start = %s, want charging", svc.state)
	}
	if battery.IdleCalls != 0 {
		t.Fatalf("failed start-of-session AC read idled the battery: idle=%d", battery.IdleCalls)
	}
	if svc.observedChargePowerW != 2000 {
		t.Fatalf("observed charge power = %v, want 2000 (DC verification power seed)", svc.observedChargePowerW)
	}

	now = now.Add(30 * time.Second)
	svc.tick(context.Background())

	if svc.state != StateCharging {
		t.Fatalf("state after recovered AC read = %s, want charging", svc.state)
	}
}

func TestCommittedCycleRecordsExportPriceMode(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cfg := testConfigSmallBattery()
	cfg.ExportPriceMode = "wholesale"

	battery := NewMockBattery(50)
	battery.IgnorePowerCommands = true
	battery.CurrentPower = 2000
	battery.ACPower = 2100
	battery.ACPowerSet = true
	svc := newTestService(cfg, battery, makePrices(baseTime, 0.05, 0.05, 0.05, 0.05), baseTime)
	svc.recorder = NewRecorder(dir, cfg.BatteryEfficiency, time.UTC)
	setReservedChargePlan(svc, baseTime, decimal.NewFromFloat(0.05))

	svc.tick(context.Background())
	if svc.state != StateCharging {
		t.Fatalf("state after charge start = %s, want charging", svc.state)
	}

	commitment, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil {
		t.Fatalf("load persisted commitment: %v", err)
	}
	if commitment == nil {
		t.Fatal("charge start did not persist an automatic cycle commitment")
	}
	if commitment.ExportPriceMode != "wholesale" {
		t.Fatalf("persisted export price mode = %q, want %q", commitment.ExportPriceMode, "wholesale")
	}

	// Pin the persistence half: a restart under the same export mode must restore
	// the cycle as chargeable, not discharge-only.
	restartBattery := NewMockBattery(50)
	restarted := newTestService(cfg, restartBattery, makePrices(baseTime, 0.05, 0.05, 0.05, 0.05), baseTime)
	restarted.recorder = NewRecorder(dir, cfg.BatteryEfficiency, time.UTC)
	if err := restarted.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("restore persisted commitment: %v", err)
	}
	if restarted.currentPlan == nil {
		t.Fatal("restore did not install a plan")
	}
	if restarted.currentPlan.DischargeOnly {
		t.Fatal("cycle persisted and restored under the same export mode was demoted to discharge-only")
	}
}

package service

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

func TestInventoryStaleSOCDoesNotReauthorizeSpentEnergy(t *testing.T) {
	now := time.Now().UTC().Truncate(15 * time.Minute).Add(15 * time.Minute)
	cfg := testConfig()
	cfg.BatteryEfficiency = 0.79
	cfg.BatteryChargeEfficiency = 0.95
	cfg.DischargePowerW = 2200
	battery := NewMockBattery(30)
	prices := make([]nordpool.Price, 8)
	for i := range prices {
		prices[i] = nordpool.Price{Time: now.Add(time.Duration(i) * 15 * time.Minute), Value: 0.4}
	}
	svc := newTestService(cfg, battery, prices, now)
	svc.nowFunc = func() time.Time { return now }
	svc.tick(context.Background())
	if len(battery.DischargeCalls) != 1 {
		t.Fatalf("first measured inventory sale did not start: %+v", battery.DischargeCalls)
	}
	battery.CurrentPower = -2510
	// 19% usable at 2200 W AC is roughly 22 minutes; SOC never moves, so the
	// allowance is never replenished and the budget must stop the sale.
	for i := 0; i < 30*60; i++ {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	if svc.state != StateIdle {
		t.Fatalf("budget failed to stop stale-SOC sale: %s", svc.state)
	}
	for i := 0; i < 6; i++ {
		now = now.Add(15 * time.Minute)
		svc.tick(context.Background())
	}
	if len(battery.DischargeCalls) != 1 {
		t.Fatalf("unchanged SOC minted %d sales without charging", len(battery.DischargeCalls))
	}
}

func TestInventoryCreditsOnlyConsecutiveMeasuredCharging(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(50)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.inventory = dischargeInventory{}
	svc.state = StateCharging
	battery.CurrentPower = 1000
	for i := 0; i <= 36; i++ {
		svc.sampleDischargeInventory(context.Background())
		now = now.Add(time.Second)
	}
	if math.Abs(svc.inventory.remainingDCKWh-0.01) > 1e-9 {
		t.Fatalf("measured 36 seconds at 1000W credited %g kWh", svc.inventory.remainingDCKWh)
	}
	now = now.Add(time.Minute)
	battery.SOC = 90
	svc.sampleDischargeInventory(context.Background())
	if math.Abs(svc.inventory.remainingDCKWh-0.01) > 1e-9 {
		t.Fatalf("gap or SOC rise credited inventory: %g", svc.inventory.remainingDCKWh)
	}
	svc.state = StateIdle
	now = now.Add(time.Second)
	svc.cacheBatteryTelemetryLocked(90, 1000)
	if math.Abs(svc.inventory.remainingDCKWh-0.01) > 1e-9 {
		t.Fatal("uncontrolled charging credited inventory")
	}
}

func TestInventoryCrashAndFailedStopCannotRestoreAllowance(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.recorder = NewRecorder(t.TempDir(), 0.9, time.UTC)
	svc.nowFunc = func() time.Time { return now }
	svc.mu.Lock()
	if _, err := svc.prepareDischargeInventoryLocked(80, 2500, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	svc.state = StateManualDischarging
	svc.currentTradeStart = now
	svc.currentTradeLastSOC = 80
	svc.currentTradePowerW = 2500
	battery.CurrentPower = -2500
	battery.IdleErr = errors.New("bridge unavailable")
	now = now.Add(time.Minute)
	svc.stopDischargingLocked(context.Background(), 80)
	svc.mu.Unlock()
	snapshot, err := svc.recorder.LoadInventorySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil || !snapshot.InFlight {
		t.Fatal("failed idle cleared crash marker")
	}
	restored := newTestService(testConfig(), NewMockBattery(80), nil, now)
	restored.recorder = svc.recorder
	if err := restored.restoreDischargeInventory(); err != nil {
		t.Fatal(err)
	}
	if restored.inventory.remainingDCKWh != 0 {
		t.Fatalf("crashed discharge restored %g kWh", restored.inventory.remainingDCKWh)
	}
}

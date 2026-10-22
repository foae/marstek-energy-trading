package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

func TestDischargeFloorAllowsOutputUntilEleven(t *testing.T) {
	now := time.Now().UTC()
	battery := NewMockBattery(12)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = &MockNotifier{}
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	for range 40 {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	if svc.state != StateManualDischarging || len(battery.DischargeCalls) != 1 {
		t.Fatalf("healthy discharge at 12%% interrupted: state=%s calls=%v", svc.state, battery.DischargeCalls)
	}
	battery.SOC = 11
	svc.sampleDischargeInventory(context.Background())
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Fatalf("11%% did not stop: state=%s power=%d", svc.state, battery.CurrentPower)
	}
	battery.SOC = 13 // a rebound is not recharging
	now = now.Add(time.Hour)
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	if len(battery.DischargeCalls) != 1 {
		t.Fatal("SOC rebound reissued discharge without measured recharge")
	}
}

func TestDischargeFloorCutoffSuppressesRefreshAndRestart(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			now := time.Now().UTC().Truncate(15 * time.Minute).Add(time.Minute)
			battery := NewMockBattery(30)
			prices := []nordpool.Price{{Time: now.Truncate(15 * time.Minute), Value: .4}, {Time: now.Truncate(15 * time.Minute).Add(15 * time.Minute), Value: .4}}
			svc := newTestService(testConfig(), battery, prices, now)
			svc.nowFunc = func() time.Time { return now }
			svc.recorder = NewRecorder(t.TempDir(), .9, time.UTC)
			svc.telegram = &MockNotifier{}
			if automatic {
				svc.tick(context.Background())
			} else {
				svc.handleManualDischargeCommand(context.Background(), []string{"800"})
			}
			if len(battery.DischargeCalls) != 1 {
				t.Fatal("scenario did not start discharge")
			}
			battery.SOC, battery.CurrentPower = 13, 0
			now = now.Add(time.Second)
			svc.sampleDischargeInventory(context.Background())
			svc.lastPassiveRefresh = now.Add(-time.Hour)
			svc.tick(context.Background())
			if len(battery.refreshCalls) != 0 {
				t.Fatal("passive refresh retried near-floor hardware cutoff")
			}
			for range 30 {
				now = now.Add(time.Second)
				svc.sampleDischargeInventory(context.Background())
			}
			if svc.state != StateIdle {
				t.Fatalf("sustained cutoff did not stop: %s", svc.state)
			}
			restarted := newTestService(testConfig(), battery, prices, now)
			restarted.recorder = svc.recorder
			if err := restarted.restoreDischargeInventory(); err != nil {
				t.Fatal(err)
			}
			restarted.telegram = &MockNotifier{}
			restarted.handleManualDischargeCommand(context.Background(), []string{"800"})
			if len(battery.DischargeCalls) != 1 {
				t.Fatal("restart lost cutoff hold")
			}
		})
	}
}

func TestDischargeFloorFailedStartDoesNotRetry(t *testing.T) {
	for _, commandErr := range []error{nil, marstek.ErrLinkDown} {
		t.Run(map[bool]string{false: "no output", true: "unconfirmed command"}[commandErr != nil], func(t *testing.T) {
			now := time.Now().UTC()
			battery := NewMockBattery(12)
			battery.IgnorePowerCommands = true
			battery.DischargeErr = commandErr
			svc := newTestService(testConfig(), battery, nil, now)
			svc.nowFunc = func() time.Time { return now }
			svc.batteryVerificationTimeout = 5 * time.Millisecond
			svc.batteryVerificationInterval = time.Millisecond
			svc.telegram = &MockNotifier{}
			svc.handleManualDischargeCommand(context.Background(), []string{"800"})
			calls := len(battery.DischargeCalls)
			now = now.Add(2 * time.Hour)
			battery.DischargeErr = nil
			svc.handleManualDischargeCommand(context.Background(), []string{"800"})
			if len(battery.DischargeCalls) != calls {
				t.Fatal("failed near-floor start retried after cooldown")
			}
		})
	}
}

func TestDischargeFloorTransientAndReadGapDoNotQualify(t *testing.T) {
	now := time.Now().UTC()
	battery := NewMockBattery(13)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = &MockNotifier{}
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	battery.CurrentPower = 0
	for range 20 {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	battery.GetPowerErr = errors.New("temporary read failure")
	now = now.Add(time.Second)
	svc.sampleDischargeInventory(context.Background())
	battery.GetPowerErr = nil
	for range 20 {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	battery.CurrentPower = -800
	now = now.Add(time.Second)
	svc.sampleDischargeInventory(context.Background())
	svc.lastPassiveRefresh = now.Add(-time.Hour)
	svc.tick(context.Background())
	if svc.state != StateManualDischarging || len(battery.refreshCalls) != 1 {
		t.Fatalf("interrupted cutoff evidence latched hold: state=%s refresh=%v", svc.state, battery.refreshCalls)
	}
}

func TestDischargeFloorNormalStopDoesNotQualifyTransientCutoff(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(13)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = &MockNotifier{}
	ctx := context.Background()
	svc.handleManualDischargeCommand(ctx, []string{"800"})
	battery.CurrentPower = 0
	now = now.Add(time.Second)
	svc.sampleDischargeInventory(ctx)
	svc.manualOverrideUntil = now
	svc.tick(ctx)
	if svc.state != StateIdle {
		t.Fatalf("manual timeout did not stop discharge: %s", svc.state)
	}
	svc.handleManualDischargeCommand(ctx, []string{"800"})
	if svc.state != StateManualDischarging || len(battery.DischargeCalls) != 2 {
		t.Fatalf("normal stop qualified transient cutoff: state=%s calls=%v", svc.state, battery.DischargeCalls)
	}
}

func TestDischargeFloorRearmsOnlyAfterLiveMeasuredRecharge(t *testing.T) {
	now := time.Now().UTC()
	battery := NewMockBattery(11)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = &MockNotifier{}
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	battery.SOC = 16
	svc.state = StateSolarCharging
	battery.CurrentPower = 2200
	battery.checkLinkErr = marstek.ErrLinkDown
	for range 300 {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	if !svc.GetCurrentStatus(context.Background()).DischargeFloorHeld {
		t.Fatal("frozen charge telemetry cleared the hold")
	}
	battery.checkLinkErr = nil
	for range 300 {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	if svc.GetCurrentStatus(context.Background()).DischargeFloorHeld {
		t.Fatal("live measured recharge did not clear the hold")
	}
	svc.state = StateIdle
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	if len(battery.DischargeCalls) != 1 {
		t.Fatal("measured recharge did not permit another discharge")
	}
}

func TestDischargeFloorHoldPreservesChargingPriority(t *testing.T) {
	now := time.Now().UTC()
	battery := NewMockBattery(13)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.telegram = &MockNotifier{}
	svc.mu.Lock()
	svc.setDischargeFloorHoldLocked(true)
	svc.mu.Unlock()
	svc.currentPlan = &TradingPlan{DischargeWindows: []TimeWindow{{Start: now.Add(-time.Minute), End: now.Add(time.Hour)}}}
	if svc.solarBlockedLocked(now, 13) {
		t.Fatal("held discharge window blocks solar recharge")
	}
	svc.state = StateCharging
	battery.CurrentPower = 2200
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	if svc.state != StateCharging || battery.IdleAttempts != 0 || len(battery.DischargeCalls) != 0 {
		t.Fatal("rejected discharge interrupted charging")
	}
	svc.lastPassiveRefresh = now.Add(-time.Hour)
	svc.mu.Lock()
	svc.refreshPassiveModeLocked(context.Background(), -2200)
	svc.mu.Unlock()
	if len(battery.refreshCalls) != 1 {
		t.Fatal("hold suppressed a charging refresh")
	}
}

func TestDischargeFloorHoldSurvivesUnconfirmedStop(t *testing.T) {
	now := time.Now().UTC()
	battery := NewMockBattery(12)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), .9, time.UTC)
	svc.telegram = &MockNotifier{}
	svc.handleManualDischargeCommand(context.Background(), []string{"800"})
	battery.IdleErr = errors.New("stop unconfirmed")
	battery.SOC = 11
	now = now.Add(time.Second)
	svc.sampleDischargeInventory(context.Background())
	snapshot, err := svc.recorder.LoadInventorySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil || !snapshot.InFlight || !snapshot.DischargeFloorHeld {
		t.Fatalf("failed stop lost durable safety state: %+v", snapshot)
	}
	restarted := newTestService(testConfig(), battery, nil, now)
	restarted.recorder = svc.recorder
	if err := restarted.restoreDischargeInventory(); err != nil {
		t.Fatal(err)
	}
	if !restarted.inventory.floorHeld || restarted.inventory.remainingDCKWh != 0 {
		t.Fatal("interrupted discharge lost hold or restored untrusted inventory")
	}
}

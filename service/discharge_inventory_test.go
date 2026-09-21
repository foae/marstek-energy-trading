package service

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
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

// A bridge that times out for one tick is not evidence that the pack emptied.
// Discarding on the first failure cost a full battery its allowance, and a
// full battery cannot charge to earn another one.
func TestInventoryTransientTelemetryFailureKeepsAllowance(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	battery.GetStatusErr = errors.New("context deadline exceeded")
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.mu.Lock()
	svc.inventory.remainingDCKWh = 3
	svc.mu.Unlock()

	svc.tick(context.Background())
	if svc.inventory.remainingDCKWh != 3 {
		t.Fatalf("a single failed read discarded %g kWh of measured allowance", 3-svc.inventory.remainingDCKWh)
	}

	// Still inside the grace, and then a good read ends the outage entirely.
	now = now.Add(inventoryTelemetryFailureGrace - time.Second)
	svc.tick(context.Background())
	if svc.inventory.remainingDCKWh != 3 {
		t.Fatalf("allowance discarded inside the grace: %g", svc.inventory.remainingDCKWh)
	}
	battery.GetStatusErr = nil
	now = now.Add(time.Second)
	svc.tick(context.Background())
	if !svc.inventory.telemetryFailureSince.IsZero() {
		t.Fatal("a successful read did not end the telemetry outage")
	}

	// Sustained failure still discards, and records the repairable amount.
	battery.GetStatusErr = errors.New("context deadline exceeded")
	svc.tick(context.Background())
	now = now.Add(inventoryTelemetryFailureGrace)
	svc.tick(context.Background())
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("sustained telemetry failure kept %g kWh", svc.inventory.remainingDCKWh)
	}
	if svc.inventory.lostDCKWh != 3 {
		t.Fatalf("discarded allowance recorded as %g kWh", svc.inventory.lostDCKWh)
	}
}

// discardedInventoryService returns a service whose telemetry history is old
// enough for the link check to mean something, holding a repairable discard of
// balanceDCKWh made at the battery's current SOC.
func discardedInventoryService(t *testing.T, battery *MockBattery, balanceDCKWh float64, now *time.Time) *Service {
	t.Helper()
	svc := newTestService(testConfig(), battery, nil, *now)
	svc.nowFunc = func() time.Time { return *now }
	svc.recorder = NewRecorder(t.TempDir(), 0.9, time.UTC)
	svc.mu.Lock()
	svc.cacheBatteryTelemetryLocked(battery.SOC, 0)
	svc.inventory.remainingDCKWh = balanceDCKWh
	svc.invalidateDischargeInventoryLocked(true)
	svc.mu.Unlock()
	if svc.inventory.remainingDCKWh != 0 || svc.inventory.lostDCKWh != balanceDCKWh {
		t.Fatalf("discard did not record a repair: remaining=%g lost=%g", svc.inventory.remainingDCKWh, svc.inventory.lostDCKWh)
	}
	return svc
}

// An allowance discarded outside a sale is repairable: the energy never left
// the pack. The repair and the discard both have to reach disk.
func TestInventoryDiscardedOutsideSaleIsRepaired(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)

	// The discard alone must be durable: a restart before the repair must not
	// read the pre-discard balance straight back.
	now = now.Add(time.Second)
	svc.sampleDischargeInventory(context.Background())
	discarded, err := svc.recorder.LoadInventorySnapshot()
	if err != nil || discarded == nil || discarded.RemainingDCKWh != 0 || discarded.LostDCKWh != 3 {
		t.Fatalf("discard was not made durable: %+v (%v)", discarded, err)
	}

	now = now.Add(inventoryRepairWarmup + inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 3 {
		t.Fatalf("confirmed-live link did not repair the allowance: %g", svc.inventory.remainingDCKWh)
	}
	if svc.inventory.lostDCKWh != 0 {
		t.Fatal("repair marker survived the repair and can be replayed")
	}
	repaired, err := svc.recorder.LoadInventorySnapshot()
	if err != nil || repaired == nil || repaired.RemainingDCKWh != 3 || repaired.LostDCKWh != 0 {
		t.Fatalf("repair was not made durable: %+v (%v)", repaired, err)
	}
}

// CheckLink reports healthy until it has seen telemetry move, so a fresh
// process cannot tell a live bridge from a frozen one serving cached values.
func TestInventoryRepairWaitsForTheLinkCheckToMeanSomething(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)

	now = now.Add(inventoryRepairWarmup - time.Second)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("repaired %g kWh behind a link check that could not yet fail", svc.inventory.remainingDCKWh)
	}
	if battery.checkLinkCalls != 0 {
		t.Fatalf("probed the link %d times inside the warm-up", battery.checkLinkCalls)
	}

	now = now.Add(time.Second + inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 3 {
		t.Fatalf("repair did not resume after the warm-up: %g", svc.inventory.remainingDCKWh)
	}
}

// A frozen link must not repair, and the repair probe is throttled.
func TestInventoryRepairNeedsALiveLink(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	battery.checkLinkErr = marstek.ErrLinkDown
	svc := discardedInventoryService(t, battery, 3, &now)

	now = now.Add(inventoryRepairWarmup + inventoryRecoveryInterval)
	for i := 0; i < 30; i++ {
		now = now.Add(time.Second)
		svc.sampleDischargeInventory(context.Background())
	}
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("a frozen link repaired %g kWh from SOC", svc.inventory.remainingDCKWh)
	}
	if battery.checkLinkCalls != 1 {
		t.Fatalf("repair probe ran %d times in 30s, want once per %s", battery.checkLinkCalls, inventoryRecoveryInterval)
	}
}

// Energy that left the pack must not come back when SOC does. A drain and an
// unmeasured refill between discard and repair is exactly that.
func TestInventoryRepairIsCappedByTheLowestSOCSinceTheDiscard(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)

	// The pack drains to 20% and is refilled to 80% by something other than
	// this service. SOC reads high again; the energy was never measured in.
	// The minute tick is what observes SOC while the battery sits idle.
	for _, soc := range []int{60, 40, 20, 40, 60, 80} {
		battery.SOC = soc
		now = now.Add(time.Minute)
		svc.tick(context.Background())
	}
	now = now.Add(inventoryRepairWarmup + inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())

	capped := svc.inventorySOCCap(20)
	if math.Abs(svc.inventory.remainingDCKWh-capped) > 1e-9 {
		t.Fatalf("repair ignored the SOC floor: %g, want the 20%% cap %g", svc.inventory.remainingDCKWh, capped)
	}
}

// The repair must survive the restart it is most likely to be interrupted by.
func TestInventoryRepairMarkerSurvivesRestart(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)
	battery.SOC = 60
	now = now.Add(time.Second)
	svc.tick(context.Background())                     // the idle tick lowers the floor
	svc.sampleDischargeInventory(context.Background()) // still inside the warm-up: flush only

	restarted := newTestService(testConfig(), NewMockBattery(80), nil, now)
	restarted.nowFunc = func() time.Time { return now }
	restarted.recorder = svc.recorder
	if err := restarted.restoreDischargeInventory(); err != nil {
		t.Fatal(err)
	}
	if restarted.inventory.lostDCKWh != 3 {
		t.Fatalf("restart lost the repair marker: %g", restarted.inventory.lostDCKWh)
	}
	if !restarted.inventory.lostSOCFloorKnown || restarted.inventory.lostSOCFloor != 60 {
		t.Fatalf("restart lost the SOC floor: known=%v floor=%d", restarted.inventory.lostSOCFloorKnown, restarted.inventory.lostSOCFloor)
	}
}

// A settlement failure during the repair must not make the accident it exists
// to fix permanent.
func TestInventoryRepairSurvivesAFailedPersist(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)
	// A regular file where the data directory should be: creating it fails
	// with ENOTDIR whatever the process runs as.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.recorder = NewRecorder(filepath.Join(blocker, "ledger"), 0.9, time.UTC)

	now = now.Add(inventoryRepairWarmup + inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 0 || !svc.inventory.blocked {
		t.Fatalf("failed settlement left a usable balance: remaining=%g blocked=%v", svc.inventory.remainingDCKWh, svc.inventory.blocked)
	}
	if svc.inventory.lostDCKWh != 3 || !svc.inventory.lostSOCFloorKnown {
		t.Fatalf("failed settlement destroyed the repair marker: lost=%g floorKnown=%v", svc.inventory.lostDCKWh, svc.inventory.lostSOCFloorKnown)
	}
}

// A confirmed frozen link earns its last credits from readings that may
// already be cached, so that discard is final.
func TestInventoryConfirmedLinkDownDiscardIsNotRepairable(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)
	if svc.inventory.lostDCKWh != 3 {
		t.Fatal("precondition: expected a repairable discard")
	}

	svc.mu.Lock()
	svc.invalidateDischargeInventoryLocked(false)
	svc.mu.Unlock()
	if svc.inventory.lostDCKWh != 0 || svc.inventory.lostSOCFloorKnown {
		t.Fatalf("a confirmed link-down left %g kWh repairable", svc.inventory.lostDCKWh)
	}

	now = now.Add(inventoryRepairWarmup + inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("SOC repaired %g kWh after a confirmed frozen link", svc.inventory.remainingDCKWh)
	}
}

// A rejected sale attempt moves no energy, so it must not consume the repair.
func TestInventoryRejectedSaleKeepsTheRepairMarker(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := discardedInventoryService(t, battery, 3, &now)

	svc.mu.Lock()
	// Zero power gives an invalid debit rate, so the attempt is rejected
	// before anything is committed or commanded.
	_, err := svc.prepareDischargeInventoryLocked(80, 0, false, time.Time{})
	svc.mu.Unlock()
	if err == nil {
		t.Fatal("expected the zero-power attempt to be rejected")
	}
	if svc.inventory.lostDCKWh != 3 {
		t.Fatalf("a rejected attempt consumed the repair marker: %g", svc.inventory.lostDCKWh)
	}
}

// After a sale only measurement knows what is left. SOC is exactly the witness
// a frozen link can forge, so a discard during a sale is never repaired.
func TestInventoryDiscardDuringSaleIsNotRepaired(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(80)
	svc := newTestService(testConfig(), battery, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.mu.Lock()
	if _, err := svc.prepareDischargeInventoryLocked(80, 2500, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	svc.invalidateDischargeInventoryLocked(true)
	lost := svc.inventory.lostDCKWh
	svc.inventory.inFlight = false
	svc.inventory.lastSOC, svc.inventory.lastSOCKnown = 80, true
	svc.mu.Unlock()
	if lost != 0 {
		t.Fatalf("a discard during a sale recorded %g kWh as repairable", lost)
	}

	now = now.Add(inventoryRecoveryInterval)
	svc.sampleDischargeInventory(context.Background())
	if svc.inventory.remainingDCKWh != 0 {
		t.Fatalf("SOC re-authorized %g kWh after a sale", svc.inventory.remainingDCKWh)
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

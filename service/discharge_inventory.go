package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// The bridge has no hardware command expiry. This margin reserves time for a
// normal confirmed stop; an unreachable bridge can still exceed it. In that
// case the allowance is quarantined, not reconstructed from integer SOC.
const (
	inventoryStopMargin = 45 * time.Second
	// Service-controlled charging earns credit for the interval between two
	// valid DC samples at the lower of the two readings. ESPHome HTTP polls
	// regularly take longer than the nominal second; a longer gap earns nothing
	// (a frozen link invalidates the allowance before this tolerance matters).
	inventoryCreditGapTolerance = time.Minute
	// A SOC that moved this recently on a link the check reports live is a
	// fresh measurement of what is in the battery: it may raise the in-flight
	// allowance up to its cap. A frozen SOC never moves and never replenishes.
	inventorySOCLiveWindow = 10 * time.Minute
)

type dischargeInventory struct {
	remainingDCKWh     float64
	inFlight           bool
	blocked            bool
	deadline           time.Time
	windowEnd          time.Time
	debitAt            time.Time
	debitPowerW        float64
	sampleAt           time.Time
	chargePowerW       float64
	sampleCharging     bool
	lastSOC            int
	lastSOCKnown       bool
	socLiveAt          time.Time
	lastPersistAttempt time.Time
}

// dischargeEfficiency is the configured DC-to-AC share: round-trip divided by
// the charge-side efficiency. AC watts divided by it give the DC draw.
func (s *Service) dischargeEfficiency() float64 {
	roundTrip := s.cfg.BatteryEfficiency
	charge := s.cfg.BatteryChargeEfficiency
	if !(roundTrip > 0 && roundTrip <= 1) || !(charge > 0 && charge <= 1) {
		return 0.5
	}
	efficiency := roundTrip / charge
	if !(efficiency > 0) || math.IsNaN(efficiency) {
		return 0.5
	}
	return min(efficiency, 1)
}

func (s *Service) inventorySOCCap(soc int) float64 {
	return s.cfg.BatteryCapacityKWh * float64(max(0, min(100, soc)-s.cfg.MinSOCPercent())) / 100
}

func (s *Service) inventorySnapshotLocked() InventorySnapshot {
	return InventorySnapshot{
		Version: 1, RemainingDCKWh: s.inventory.remainingDCKWh,
		CapacityKWh: s.cfg.BatteryCapacityKWh, MinSOCPercent: s.cfg.MinSOCPercent(), InFlight: s.inventory.inFlight,
	}
}

// restoreDischargeInventory must run only after a confirmed startup idle.
func (s *Service) restoreDischargeInventory() error {
	snapshot, err := s.recorder.LoadInventorySnapshot()
	if err != nil {
		return fmt.Errorf("load discharge inventory: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inventory = dischargeInventory{}
	if snapshot != nil && !snapshot.InFlight && snapshot.CapacityKWh == s.cfg.BatteryCapacityKWh && snapshot.MinSOCPercent == s.cfg.MinSOCPercent() {
		s.inventory.remainingDCKWh = snapshot.RemainingDCKWh
	} else {
		slog.Warn("discharge inventory quarantined; measured charging required", "missing", snapshot == nil)
	}
	if !s.settleDischargeInventoryLocked() {
		return fmt.Errorf("persist reconciled discharge inventory")
	}
	return nil
}

func (s *Service) debitDischargeInventoryLocked(now time.Time, powerW float64) {
	inv := &s.inventory
	if !inv.inFlight {
		return
	}
	if !math.IsNaN(powerW) && !math.IsInf(powerW, 0) {
		inv.debitPowerW = max(inv.debitPowerW, -powerW)
	}
	if now.After(inv.debitAt) {
		inv.remainingDCKWh = max(0, inv.remainingDCKWh-inv.debitPowerW*now.Sub(inv.debitAt).Hours()/1000)
		inv.debitAt = now
	}
	// A newly observed higher draw can only advance the stop request.
	if !inv.deadline.IsZero() && inv.debitPowerW > 0 {
		deadline := now.Add(time.Duration(inv.remainingDCKWh*3_600_000/inv.debitPowerW*float64(time.Second)) - inventoryStopMargin)
		if deadline.Before(inv.deadline) {
			inv.deadline = deadline
		}
	}
}

// Consecutive valid DC samples during service-controlled charging earn
// inventory; a gap beyond the tolerance earns nothing. SOC caps the allowance
// downward, and while a sale is in flight a SOC that is demonstrably moving on
// a live link also raises it back up to that cap. A SOC frozen by a dead link
// cannot: it does not move, and the link check has already invalidated the
// allowance.
func (s *Service) observeDischargeInventoryLocked(soc int, powerW float64) {
	if math.IsNaN(powerW) || math.IsInf(powerW, 0) {
		s.invalidateDischargeInventoryLocked()
		return
	}
	now := s.now()
	inv := &s.inventory
	s.debitDischargeInventoryLocked(now, powerW)
	if inv.lastSOCKnown && soc != inv.lastSOC {
		inv.socLiveAt = now
	}
	inv.lastSOC = soc
	inv.lastSOCKnown = true
	charging := !inv.inFlight && !s.stopPending && s.linkDownSince.IsZero() && (s.state == StateCharging || s.state == StateSolarCharging)
	elapsed := now.Sub(inv.sampleAt)
	if !inv.blocked && charging && inv.sampleCharging && elapsed > 0 && elapsed <= inventoryCreditGapTolerance {
		inv.remainingDCKWh += min(inv.chargePowerW, max(0, powerW)) * elapsed.Hours() / 1000
	}
	socCap := s.inventorySOCCap(soc)
	socLive := !inv.socLiveAt.IsZero() && now.Sub(inv.socLiveAt) <= inventorySOCLiveWindow
	if inv.inFlight && !inv.blocked && socLive && s.linkDownSince.IsZero() && socCap > inv.remainingDCKWh {
		inv.remainingDCKWh = socCap
		if !inv.deadline.IsZero() && inv.debitPowerW > 0 {
			deadline := now.Add(time.Duration(inv.remainingDCKWh*3_600_000/inv.debitPowerW*float64(time.Second)) - inventoryStopMargin)
			if !inv.windowEnd.IsZero() && inv.windowEnd.Before(deadline) {
				deadline = inv.windowEnd
			}
			inv.deadline = deadline
		}
	}
	inv.remainingDCKWh = min(inv.remainingDCKWh, socCap)
	inv.sampleAt = now
	inv.chargePowerW = max(0, powerW)
	inv.sampleCharging = charging
}

func (s *Service) invalidateDischargeInventoryLocked() {
	s.inventory.remainingDCKWh = 0
	s.inventory.sampleCharging = false
	s.inventory.sampleAt = time.Time{}
	// Liveness must be proven again by a SOC change after the invalidation.
	s.inventory.socLiveAt = time.Time{}
	if s.inventory.inFlight && !s.inventory.deadline.IsZero() {
		s.inventory.deadline = s.now()
	}
}

func (s *Service) prepareDischargeInventoryLocked(soc, powerW int, automatic bool, windowEnd time.Time) (time.Time, error) {
	inv := &s.inventory
	if inv.blocked || inv.inFlight {
		return time.Time{}, fmt.Errorf("discharge inventory persistence or settlement pending")
	}
	inv.remainingDCKWh = min(inv.remainingDCKWh, s.inventorySOCCap(soc))
	// AC watts over the discharge-side efficiency estimate the DC draw; actual
	// observed draw may increase this estimate but never decrease it.
	rate := float64(powerW) / s.dischargeEfficiency()
	if s.batteryTelemetryAvailable {
		rate = max(rate, -s.batteryTelemetryPowerW)
	}
	if !(rate > 0) || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return time.Time{}, fmt.Errorf("invalid discharge debit rate")
	}
	deadline := time.Time{}
	if automatic {
		deadline = s.now().Add(time.Duration(inv.remainingDCKWh*3_600_000/rate*float64(time.Second)) - inventoryStopMargin)
		if windowEnd.Before(deadline) {
			deadline = windowEnd
		}
		if deadline.Sub(s.now()) <= minimumAutomaticControlWindow {
			return time.Time{}, fmt.Errorf("trusted discharge inventory cannot cover minimum session and confirmed-stop margin")
		}
	}
	inv.inFlight = true
	inv.deadline = deadline
	inv.windowEnd = windowEnd
	inv.debitAt = s.now()
	inv.debitPowerW = rate
	inv.sampleCharging = false
	snapshot := s.inventorySnapshotLocked()
	s.mu.Unlock()
	err := s.recorder.SaveInventorySnapshot(snapshot)
	s.mu.Lock()
	if err != nil {
		inv.remainingDCKWh = 0
		inv.inFlight = false // no battery command has been issued
		inv.blocked = true
		inv.deadline = time.Time{}
		inv.lastPersistAttempt = s.now()
		return time.Time{}, fmt.Errorf("persist discharge intent: %w", err)
	}
	// Persistence latency is also conservatively debited, and must not extend
	// the absolute endpoint calculated before publication.
	s.debitDischargeInventoryLocked(s.now(), 0)
	if automatic && deadline.Sub(s.now()) <= minimumAutomaticControlWindow {
		s.settleDischargeInventoryLocked() // no command attempted; safe to clear
		return time.Time{}, fmt.Errorf("discharge inventory deadline elapsed during intent persistence")
	}
	return inv.deadline, nil
}

// Caller has confirmed physical idle (or knows no command was attempted).
// A failed publication may already have renamed the file: discard the balance
// and prohibit another command until a zero-balance settlement is durable.
func (s *Service) settleDischargeInventoryLocked() bool {
	inv := &s.inventory
	s.debitDischargeInventoryLocked(s.now(), 0)
	inv.sampleCharging = false
	snapshot := s.inventorySnapshotLocked()
	snapshot.InFlight = false
	inv.lastPersistAttempt = s.now()
	s.mu.Unlock()
	err := s.recorder.SaveInventorySnapshot(snapshot)
	s.mu.Lock()
	if err != nil {
		inv.remainingDCKWh = 0
		inv.blocked = true
		slog.Error("failed to settle discharge inventory; automatic sales blocked", "error", err)
		return false
	}
	inv.inFlight = false
	inv.blocked = false
	inv.deadline = time.Time{}
	inv.windowEnd = time.Time{}
	inv.debitAt = time.Time{}
	s.lastPlanSlot = time.Time{}
	return true
}

func (s *Service) dischargeInventoryDeadlineLocked(windowEnd time.Time) time.Time {
	deadline := s.inventory.deadline
	if deadline.IsZero() || (!windowEnd.IsZero() && windowEnd.Before(deadline)) {
		return windowEnd
	}
	return deadline
}

// Always runs on the serialized control loop, including without a P1 meter.
// Do not run a second command goroutine: command/stop ordering must stay owned
// by the existing service state machine.
func (s *Service) sampleDischargeInventory(ctx context.Context) {
	if s.retryStopping(ctx) {
		return
	}
	s.mu.Lock()
	if s.inventory.blocked && s.state == StateIdle && s.now().Sub(s.inventory.lastPersistAttempt) >= s.stopRetryDelay() {
		s.settleDischargeInventoryLocked()
	}
	active := s.inventory.inFlight || s.state == StateCharging || s.state == StateSolarCharging
	s.mu.Unlock()
	if !active {
		return
	}
	sampleCtx, cancel := context.WithTimeout(ctx, solarTelemetryGapTolerance)
	s.checkLinkDuringSession(sampleCtx)
	status, err := s.battery.GetBatteryStatusContext(sampleCtx)
	var powerW float64
	if err == nil {
		powerW, err = s.battery.GetBatteryPower(sampleCtx)
	}
	cancel()
	s.mu.Lock()
	if err != nil {
		s.invalidateDischargeInventoryLocked()
		s.batteryTelemetryAvailable = false
	} else {
		s.cacheBatteryTelemetryLocked(status.SOC, powerW)
	}
	deadline := s.inventory.deadline
	if err != nil {
		switch s.state {
		case StateCharging:
			s.stopChargingLocked(ctx, s.currentTradeLastSOC)
		case StateDischarging, StateManualDischarging:
			s.stopDischargingLocked(ctx, s.currentTradeLastSOC)
		case StateSolarCharging:
			s.stopSolarChargingLocked(ctx, s.currentTradeLastSOC, solarStopReasonTelemetryFailure)
		}
	} else if s.state == StateDischarging && !deadline.IsZero() && !s.now().Before(deadline) {
		s.stopDischargingLocked(ctx, status.SOC)
	}
	s.mu.Unlock()
}

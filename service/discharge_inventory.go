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
	// A failed read is not evidence that the allowance is wrong: the bridge
	// times out under load while the battery keeps holding exactly the charge
	// it was credited for. Failures are tolerated for this long before the
	// allowance is discarded. A link the checker confirms dead still
	// invalidates immediately, so this never delays the dangerous case.
	inventoryTelemetryFailureGrace = 2 * time.Minute
	// Re-basing reads the battery directly, so it is throttled rather than run
	// on every sampling tick: the RS485 hub fails under load.
	inventoryRecoveryInterval = time.Minute
	// CheckLink reports healthy until the bridge client has seen a telemetry
	// value change, so a freshly started process cannot tell a live bridge
	// from a frozen one serving cached values. A repair waits out that window
	// before treating a passing link check as proof of anything. It matches
	// the client's own link-stale threshold.
	inventoryRepairWarmup = 2 * time.Minute
	// The repair probe runs at most once a minute while idle, so it can afford
	// a budget that a slow-but-alive bridge can actually meet.
	inventoryRepairProbeTimeout = 10 * time.Second
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
	// Telemetry failures are held, not acted on, until the grace elapses.
	telemetryFailureSince time.Time
	telemetryInvalidated  bool
	// lostDCKWh is a measured allowance discarded while the battery was NOT
	// discharging and the link was not known to be frozen, so the energy is
	// still in the pack and the discard was an accident to be repaired rather
	// than a sale to be honoured. It bounds the repair: SOC can restore what
	// was measured, never more.
	lostDCKWh float64
	// lostSOCFloor is the lowest SOC seen since that discard. Energy that left
	// the pack in the meantime must not come back when SOC does, so the repair
	// is capped by the floor rather than by present SOC.
	lostSOCFloor      int
	lostSOCFloorKnown bool
	// persistPending marks ledger state that changed outside a settlement and
	// must reach disk before a restart can read a stale balance back.
	persistPending      bool
	firstSampleAt       time.Time
	lastRecoveryAttempt time.Time
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
		// A restart is the most likely event right after an outage discarded an
		// allowance, so the pending repair has to outlive the process. Its SOC
		// floor travels with it: without the floor a restart would re-open the
		// drain-and-refill window the floor exists to close.
		LostDCKWh:         s.inventory.lostDCKWh,
		LostSOCFloor:      s.inventory.lostSOCFloor,
		LostSOCFloorKnown: s.inventory.lostSOCFloorKnown,
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
		s.inventory.lostDCKWh = snapshot.LostDCKWh
		s.inventory.lostSOCFloor = snapshot.LostSOCFloor
		s.inventory.lostSOCFloorKnown = snapshot.LostSOCFloorKnown
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
		s.noteInventoryTelemetryFailureLocked()
		return
	}
	now := s.now()
	inv := &s.inventory
	// A valid sample ends the outage: the battery answered, so the balance the
	// previous samples measured is trusted again.
	inv.telemetryFailureSince = time.Time{}
	inv.telemetryInvalidated = false
	if inv.firstSampleAt.IsZero() {
		inv.firstSampleAt = now
	}
	s.debitDischargeInventoryLocked(now, powerW)
	if inv.lastSOCKnown && soc != inv.lastSOC {
		inv.socLiveAt = now
	}
	inv.lastSOC = soc
	inv.lastSOCKnown = true
	// Track how far the pack fell while a repair is pending: only energy that
	// provably never left it may come back.
	if inv.lostDCKWh > 0 && (!inv.lostSOCFloorKnown || soc < inv.lostSOCFloor) {
		inv.lostSOCFloor, inv.lostSOCFloorKnown = soc, true
		inv.persistPending = true
	}
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

// noteInventoryTelemetryFailureLocked records a failed or corrupt telemetry
// read without discarding the measured allowance. Discarding on the first
// failure was unsound and unrecoverable: a single ESPHome HTTP timeout wiped a
// full battery's allowance, and because only measured charging replenishes it,
// a battery already at 100% could never rebuild one. Sustained failure still
// invalidates, and a confirmed dead link bypasses the grace entirely.
func (s *Service) noteInventoryTelemetryFailureLocked() {
	inv := &s.inventory
	now := s.now()
	if inv.telemetryFailureSince.IsZero() {
		inv.telemetryFailureSince = now
	}
	// Credit already requires consecutive samples inside the gap tolerance, so
	// no energy is earned across the outage; only the balance is held.
	inv.sampleCharging = false
	if inv.telemetryInvalidated || now.Sub(inv.telemetryFailureSince) < inventoryTelemetryFailureGrace {
		return
	}
	inv.telemetryInvalidated = true
	slog.Warn("discharge inventory discarded after sustained telemetry failure",
		"unavailable_for", now.Sub(inv.telemetryFailureSince).Round(time.Second),
		"discarded_dc_kwh", inv.remainingDCKWh)
	// Unreachable telemetry is not evidence that the pack emptied, so this
	// discard stays repairable; a confirmed frozen link does not.
	s.invalidateDischargeInventoryLocked(true)
}

// recoverDischargeInventoryLocked repairs an allowance that was discarded
// while the battery was not discharging. Only measured charging credits the
// ledger, so an allowance discarded at a high SOC can never be rebuilt: a full
// battery cannot charge. The repair is bounded twice over. It restores no more
// than the allowance that was actually measured and then discarded, so neither
// SOC nor uncontrolled charging can mint energy that was never bought, and it
// is capped by present SOC, so energy that has since left the pack stays gone.
// Any discharge clears the marker: after a sale, only measurement knows what
// is left, and SOC is exactly the witness that a frozen link can forge.
func (s *Service) recoverDischargeInventoryLocked(soc int) {
	inv := &s.inventory
	if inv.blocked || inv.inFlight || s.state != StateIdle || !s.linkDownSince.IsZero() || inv.lostDCKWh <= 0 {
		return
	}
	// Cap by the lowest SOC seen since the discard, not by present SOC: a pack
	// that drained and was refilled from an unmeasured source reads back high,
	// and repairing to that would sell energy the service never measured.
	floor := soc
	if inv.lostSOCFloorKnown && inv.lostSOCFloor < floor {
		floor = inv.lostSOCFloor
	}
	// Merge rather than replace: credit measured after the discard is real too,
	// and the SOC cap bounds the sum to what the pack can physically hold.
	restored := min(inv.remainingDCKWh+inv.lostDCKWh, s.inventorySOCCap(floor))
	if restored <= inv.remainingDCKWh+plannerEnergyEpsilon {
		return
	}
	previous, lost := inv.remainingDCKWh, inv.lostDCKWh
	floorValue, floorKnown := inv.lostSOCFloor, inv.lostSOCFloorKnown
	inv.remainingDCKWh = restored
	// The marker is cleared in the same snapshot that carries the repair, so a
	// crash between the two cannot replay it.
	inv.lostDCKWh, inv.lostSOCFloorKnown = 0, false
	if !s.settleDischargeInventoryLocked() {
		// Settlement zeroed the balance and blocked sales. Keep the marker so
		// the accident stays repairable once persistence recovers, instead of
		// making the very failure it repairs permanent.
		inv.lostDCKWh = max(lost, previous)
		inv.lostSOCFloor, inv.lostSOCFloorKnown = floorValue, floorKnown
		return
	}
	slog.Info("discarded discharge inventory repaired",
		"soc", soc, "soc_floor", floor, "previous_dc_kwh", previous, "available_dc_kwh", inv.remainingDCKWh)
}

// invalidateDischargeInventoryLocked discards the allowance. repairable says
// whether the energy is known to still be in the pack, and so whether the
// discard may later be undone against SOC.
func (s *Service) invalidateDischargeInventoryLocked(repairable bool) {
	inv := &s.inventory
	// An allowance discarded while a sale is in flight is gone for good: the
	// battery may be draining behind a link that cannot be trusted to say so.
	// A confirmed frozen link is equally final, because the credits it earned
	// last may themselves have come from readings that were already stale.
	switch {
	case !repairable:
		inv.lostDCKWh, inv.lostSOCFloorKnown = 0, false
	case !inv.inFlight && inv.remainingDCKWh > inv.lostDCKWh:
		inv.lostDCKWh = inv.remainingDCKWh
		inv.lostSOCFloor, inv.lostSOCFloorKnown = inv.lastSOC, inv.lastSOCKnown
	}
	if inv.remainingDCKWh > 0 || inv.lostDCKWh > 0 {
		// The discard itself must reach disk. Left in memory, a restart reads
		// the pre-discard balance straight back and skips every repair gate.
		inv.persistPending = true
	}
	inv.remainingDCKWh = 0
	inv.sampleCharging = false
	inv.sampleAt = time.Time{}
	// Liveness must be proven again by a SOC change after the invalidation.
	inv.socLiveAt = time.Time{}
	if inv.inFlight && !inv.deadline.IsZero() {
		inv.deadline = s.now()
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
	// Only now, with the attempt validated and about to be committed, does an
	// earlier discarded allowance stop being repairable: once energy starts
	// leaving the pack, only measurement knows what is left behind. Clearing
	// it any earlier would let a rejected attempt consume the repair.
	inv.lostDCKWh, inv.lostSOCFloorKnown = 0, false
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
	inv.persistPending = false
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

// recoverableDischargeInventoryLocked reports whether re-basing is worth a
// battery read this tick. It decides on the cached SOC; the re-base itself
// uses the fresh reading the caller takes afterwards.
func (s *Service) recoverableDischargeInventoryLocked() bool {
	inv := &s.inventory
	if inv.blocked || inv.inFlight || s.state != StateIdle || !s.linkDownSince.IsZero() || !inv.lastSOCKnown {
		return false
	}
	if inv.lostDCKWh <= 0 {
		return false
	}
	// Until the bridge client has had time to observe telemetry move, its link
	// check cannot fail, and a gate that cannot fail proves nothing.
	if inv.firstSampleAt.IsZero() || s.now().Sub(inv.firstSampleAt) < inventoryRepairWarmup {
		return false
	}
	if !inv.lastRecoveryAttempt.IsZero() && s.now().Sub(inv.lastRecoveryAttempt) < inventoryRecoveryInterval {
		return false
	}
	floor := inv.lastSOC
	if inv.lostSOCFloorKnown && inv.lostSOCFloor < floor {
		floor = inv.lostSOCFloor
	}
	return min(inv.remainingDCKWh+inv.lostDCKWh, s.inventorySOCCap(floor)) > inv.remainingDCKWh+plannerEnergyEpsilon
}

// dischargeInventoryLinkLive confirms the battery itself is still answering.
// A backend without a link checker cannot prove it, so it never re-bases.
func (s *Service) dischargeInventoryLinkLive(ctx context.Context) bool {
	lc, ok := s.battery.(LinkChecker)
	if !ok {
		return false
	}
	if err := lc.CheckLink(ctx); err != nil {
		slog.Debug("discharge inventory re-base skipped; link not confirmed live", "error", err)
		return false
	}
	return true
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
	// Flush a discard that only happened in memory. This needs no battery read
	// and must not wait on one: until it lands, a restart reads back a balance
	// the service has already decided it cannot trust.
	if s.inventory.persistPending && !s.inventory.inFlight && !s.inventory.blocked {
		s.settleDischargeInventoryLocked()
	}
	active := s.inventory.inFlight || s.state == StateCharging || s.state == StateSolarCharging
	recovering := !active && s.recoverableDischargeInventoryLocked()
	if recovering {
		s.inventory.lastRecoveryAttempt = s.now()
	}
	s.mu.Unlock()
	if !active && !recovering {
		return
	}
	timeout := solarTelemetryGapTolerance
	if recovering {
		// Re-basing reads four sensors through the link check; the sampling
		// tolerance that paces an active session is too tight for that.
		timeout = inventoryRepairProbeTimeout
	}
	sampleCtx, cancel := context.WithTimeout(ctx, timeout)
	linkLive := false
	if active {
		s.checkLinkDuringSession(sampleCtx)
	} else {
		// The idle path must not inherit the session checker's notifications
		// and bridge restarts: it only needs to know whether the battery is
		// answering before SOC is allowed to re-authorize an allowance.
		linkLive = s.dischargeInventoryLinkLive(sampleCtx)
	}
	status, err := s.battery.GetBatteryStatusContext(sampleCtx)
	var powerW float64
	if err == nil {
		powerW, err = s.battery.GetBatteryPower(sampleCtx)
	}
	cancel()
	s.mu.Lock()
	switch {
	case err == nil:
		s.cacheBatteryTelemetryLocked(status.SOC, powerW)
		if recovering && linkLive {
			s.recoverDischargeInventoryLocked(status.SOC)
		}
	case !active:
		// An optional repair probe is extra load the service chose to add. Its
		// failure must not count against the telemetry grace or mark telemetry
		// unavailable for everyone else; the next attempt is a minute away.
		slog.Debug("discharge inventory repair probe failed", "error", err)
	default:
		s.noteInventoryTelemetryFailureLocked()
		s.batteryTelemetryAvailable = false
	}
	deadline := s.inventory.deadline
	// Only a failure on the session path owns stopping; the repair probe runs
	// exclusively while idle and has nothing to stop.
	if err != nil && active {
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

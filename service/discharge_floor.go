package service

import (
	"log/slog"
	"time"
)

const (
	dischargeFloorBandPercent   = 2
	dischargeFloorRearmPercent  = 5
	dischargeFloorQuietPowerW   = 50
	dischargeFloorQuietDuration = 30 * time.Second
)

// A hold blocks discharge only. Persist it before stopping, without clearing the
// in-flight marker: a crash or failed stop must still quarantine the balance.
func (s *Service) setDischargeFloorHoldLocked(held bool) {
	inv := &s.inventory
	if inv.floorHeld == held {
		return
	}
	inv.floorHeld = held
	inv.floorRechargeDCKWh = 0
	inv.floorLowSince = time.Time{}
	inv.floorSampleAt = time.Time{}
	inv.persistPending = true
	s.lastPlanSlot = time.Time{}
	snapshot := s.inventorySnapshotLocked()
	inv.lastPersistAttempt = s.now()
	s.mu.Unlock()
	err := s.recorder.SaveInventorySnapshot(snapshot)
	s.mu.Lock()
	if err != nil {
		// Failed publication can have renamed already. Keep the conservative
		// state in memory and prohibit discharge until settlement reaches disk.
		inv.floorHeld = true
		inv.blocked = true
		slog.Error("failed to persist low-SOC discharge hold", "error", err)
		return
	}
	inv.persistPending = false
	slog.Info("low-SOC discharge hold changed", "held", held, "soc", inv.lastSOC)
}

func (s *Service) observeDischargeFloorLocked(soc int, powerW float64, charging bool, elapsed time.Duration) {
	inv := &s.inventory
	now := s.now()
	floor := s.cfg.MinSOCPercent()
	if soc <= floor {
		s.setDischargeFloorHoldLocked(true)
		return
	}
	live := !inv.linkLiveAt.IsZero() && now.Sub(inv.linkLiveAt) <= inventoryReadTimeout
	if inv.floorHeld {
		// SOC rebound, recovered inventory and elapsed cooldown cannot rearm.
		// Require measured service-controlled recharge of at least 3% capacity
		// on a checked link, as well as SOC at least five points above the floor.
		if !inv.blocked && live && charging && inv.sampleCharging && elapsed > 0 && elapsed <= inventoryCreditGapTolerance {
			inv.floorRechargeDCKWh += min(inv.chargePowerW, max(0, powerW)) * elapsed.Hours() / 1000
			if soc >= floor+dischargeFloorRearmPercent && inv.floorRechargeDCKWh >= s.cfg.BatteryCapacityKWh*0.03 {
				s.setDischargeFloorHoldLocked(false)
			}
		}
		return
	}
	discharging := s.state == StateDischarging || s.state == StateManualDischarging
	quiet := discharging && live && soc <= floor+dischargeFloorBandPercent && powerW > -dischargeFloorQuietPowerW
	if !quiet {
		inv.floorLowSince = time.Time{}
		inv.floorSampleAt = time.Time{}
		return
	}
	if inv.floorLowSince.IsZero() || now.Sub(inv.floorSampleAt) > inventoryReadTimeout {
		inv.floorLowSince = now
	}
	inv.floorSampleAt = now
	if now.Sub(inv.floorLowSince) >= dischargeFloorQuietDuration {
		s.setDischargeFloorHoldLocked(true)
	}
}

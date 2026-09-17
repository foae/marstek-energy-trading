package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

const (
	inventoryLedgerFile                 = "inventory-ledger.json"
	inventoryRemainingRelativeTolerance = 1e-9
)

// InventorySnapshot records the energy that is trusted to have been measured
// into the battery and is therefore eligible for discharge planning.
type InventorySnapshot struct {
	Version        int     `json:"version"`
	RemainingDCKWh float64 `json:"remaining_dc_kwh"`
	CapacityKWh    float64 `json:"capacity_kwh"`
	MinSOCPercent  int     `json:"min_soc_percent"`
	InFlight       bool    `json:"in_flight"`
}

// SaveInventorySnapshot atomically persists the trusted inventory and its
// in-flight marker together.
func (r *Recorder) SaveInventorySnapshot(snapshot InventorySnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateInventorySnapshot(snapshot); err != nil {
		return fmt.Errorf("validate inventory snapshot: %w", err)
	}
	if r.dataDir == "" {
		return nil
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal inventory snapshot: %w", err)
	}
	if err := r.saveJSONFile(inventoryLedgerFile, data); err != nil {
		return fmt.Errorf("save inventory snapshot: %w", err)
	}
	return nil
}

// LoadInventorySnapshot restores the trusted inventory snapshot. A missing
// snapshot means no previously measured inventory is available.
func (r *Recorder) LoadInventorySnapshot() (*InventorySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dataDir == "" {
		return nil, nil
	}

	path := filepath.Join(r.dataDir, inventoryLedgerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read inventory snapshot %s: %w", path, err)
	}

	snapshot, err := decodeInventorySnapshot(data)
	if err != nil {
		return nil, fmt.Errorf("decode inventory snapshot %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect inventory snapshot %s: %w", path, err)
	}
	return snapshot, nil
}

type inventorySnapshotJSON struct {
	Version        *int     `json:"version"`
	RemainingDCKWh *float64 `json:"remaining_dc_kwh"`
	CapacityKWh    *float64 `json:"capacity_kwh"`
	MinSOCPercent  *int     `json:"min_soc_percent"`
	InFlight       *bool    `json:"in_flight"`
}

func decodeInventorySnapshot(data []byte) (*InventorySnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var persisted inventorySnapshotJSON
	if err := decoder.Decode(&persisted); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, err
	}
	if persisted.Version == nil || persisted.RemainingDCKWh == nil || persisted.CapacityKWh == nil || persisted.MinSOCPercent == nil || persisted.InFlight == nil {
		return nil, fmt.Errorf("missing required inventory snapshot field")
	}

	snapshot := InventorySnapshot{
		Version:        *persisted.Version,
		RemainingDCKWh: *persisted.RemainingDCKWh,
		CapacityKWh:    *persisted.CapacityKWh,
		MinSOCPercent:  *persisted.MinSOCPercent,
		InFlight:       *persisted.InFlight,
	}
	if err := validateInventorySnapshot(snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func validateInventorySnapshot(snapshot InventorySnapshot) error {
	if snapshot.Version != 1 {
		return fmt.Errorf("unsupported inventory snapshot version %d", snapshot.Version)
	}
	if !isFinite(snapshot.CapacityKWh) || snapshot.CapacityKWh <= 0 {
		return fmt.Errorf("invalid inventory capacity")
	}
	if snapshot.MinSOCPercent < 0 || snapshot.MinSOCPercent > 99 {
		return fmt.Errorf("invalid inventory minimum SOC")
	}
	if !isFinite(snapshot.RemainingDCKWh) || snapshot.RemainingDCKWh < 0 {
		return fmt.Errorf("invalid remaining inventory")
	}

	maximum := snapshot.CapacityKWh * float64(100-snapshot.MinSOCPercent) / 100
	tolerance := inventoryRemainingRelativeTolerance * math.Max(1, maximum)
	if snapshot.RemainingDCKWh > maximum+tolerance {
		return fmt.Errorf("remaining inventory exceeds usable capacity")
	}
	return nil
}

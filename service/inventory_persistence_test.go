package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventorySnapshotMissing(t *testing.T) {
	snapshot, err := NewRecorder(t.TempDir(), .9, time.UTC).LoadInventorySnapshot()
	if err != nil {
		t.Fatalf("LoadInventorySnapshot() error = %v", err)
	}
	if snapshot != nil {
		t.Fatalf("LoadInventorySnapshot() = %+v, want nil for missing ledger", snapshot)
	}
}

func TestInventorySnapshotRoundTripPreservesActiveResidual(t *testing.T) {
	dir := t.TempDir()
	want := InventorySnapshot{
		Version:        1,
		RemainingDCKWh: 3.25,
		CapacityKWh:    10,
		MinSOCPercent:  20,
		InFlight:       true,
	}
	if err := NewRecorder(dir, .9, time.UTC).SaveInventorySnapshot(want); err != nil {
		t.Fatalf("SaveInventorySnapshot() error = %v", err)
	}

	path := filepath.Join(dir, inventoryLedgerFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("make ledger mode permissive: %v", err)
	}
	got, err := NewRecorder(dir, .9, time.UTC).LoadInventorySnapshot()
	if err != nil {
		t.Fatalf("LoadInventorySnapshot() error = %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("LoadInventorySnapshot() = %+v, want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat inventory ledger: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("inventory ledger mode = %o, want 600", mode)
	}
}

func TestInventorySnapshotLoadRejectsInvalidLedger(t *testing.T) {
	valid := `{"version":1,"remaining_dc_kwh":3,"capacity_kwh":10,"min_soc_percent":20,"in_flight":false}`
	cases := map[string]string{
		"corrupt JSON":          `{`,
		"unknown field":         valid[:len(valid)-1] + `,"unexpected":true}`,
		"trailing JSON":         valid + ` {}`,
		"missing version":       `{"remaining_dc_kwh":3,"capacity_kwh":10,"min_soc_percent":20,"in_flight":false}`,
		"missing residual":      `{"version":1,"capacity_kwh":10,"min_soc_percent":20,"in_flight":false}`,
		"missing capacity":      `{"version":1,"remaining_dc_kwh":3,"min_soc_percent":20,"in_flight":false}`,
		"missing floor":         `{"version":1,"remaining_dc_kwh":3,"capacity_kwh":10,"in_flight":false}`,
		"missing marker":        `{"version":1,"remaining_dc_kwh":3,"capacity_kwh":10,"min_soc_percent":20}`,
		"null marker":           `{"version":1,"remaining_dc_kwh":3,"capacity_kwh":10,"min_soc_percent":20,"in_flight":null}`,
		"null ledger":           `null`,
		"nonfinite residual":    `{"version":1,"remaining_dc_kwh":1e999,"capacity_kwh":10,"min_soc_percent":20,"in_flight":false}`,
		"residual out of range": `{"version":1,"remaining_dc_kwh":8,"capacity_kwh":10,"min_soc_percent":50,"in_flight":false}`,
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, inventoryLedgerFile), []byte(contents), 0o600); err != nil {
				t.Fatalf("write ledger: %v", err)
			}
			if snapshot, err := NewRecorder(dir, .9, time.UTC).LoadInventorySnapshot(); err == nil || snapshot != nil {
				t.Fatalf("LoadInventorySnapshot() = %+v, %v; want nil, error", snapshot, err)
			}
		})
	}
}

func TestInventorySnapshotSaveValidatesWithoutPersistence(t *testing.T) {
	invalid := InventorySnapshot{
		Version:        1,
		RemainingDCKWh: 1,
		CapacityKWh:    1,
		MinSOCPercent:  100,
	}
	if err := NewRecorder("", .9, time.UTC).SaveInventorySnapshot(invalid); err == nil {
		t.Fatal("SaveInventorySnapshot() error = nil for invalid in-memory snapshot")
	}
}

func TestInventorySnapshotSaveReportsDataDirectoryWriteFailure(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("block persistence"), 0o600); err != nil {
		t.Fatalf("create data directory blocker: %v", err)
	}
	snapshot := InventorySnapshot{
		Version:        1,
		RemainingDCKWh: 3,
		CapacityKWh:    10,
		MinSOCPercent:  20,
	}
	if err := NewRecorder(dataDir, .9, time.UTC).SaveInventorySnapshot(snapshot); err == nil {
		t.Fatal("SaveInventorySnapshot() error = nil for blocked data directory")
	}
}

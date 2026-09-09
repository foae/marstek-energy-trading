package service

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEfficiencyTrackerMeasuresMidpointBoundaries(t *testing.T) {
	tracker := newEfficiencyTracker()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observeEfficiencyCycle(t, tracker, start, 4_000, -3_600)

	summary := tracker.Summary()
	if summary.Cycles != 1 || summary.RejectedWindows != 0 {
		t.Fatalf("summary counts = %+v, want one accepted window", summary)
	}
	assertFloatNear(t, summary.InputKWh, 4_000*615.0/3_600_000, "input kWh")
	assertFloatNear(t, summary.OutputKWh, 3_600*615.0/3_600_000, "output kWh")
	assertFloatNear(t, summary.WindowHours, 1230.0/3600, "window hours")
	if summary.Percent == nil {
		t.Fatal("summary percent = nil, want 90")
	}
	assertFloatNear(t, *summary.Percent, 90, "percent")
}

func TestEfficiencyTrackerWeightsCompletedWindowsByEnergy(t *testing.T) {
	tracker := newEfficiencyTracker()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observeEfficiencyCycle(t, tracker, start, 4_000, -3_600)                  // 90%, 0.683333... kWh input
	observeEfficiencyCycle(t, tracker, start.Add(2*time.Hour), 8_000, -6_400) // 80%, twice the input

	summary := tracker.Summary()
	if summary.Cycles != 2 || summary.Percent == nil {
		t.Fatalf("summary = %+v, want two completed windows with a percentage", summary)
	}
	assertFloatNear(t, *summary.Percent, 250.0/3, "weighted percent")
	if math.Abs(*summary.Percent-85) < 0.1 {
		t.Fatalf("percent = %v, appears to be the unweighted mean", *summary.Percent)
	}
}

func TestEfficiencyTrackerRejectsPartialWindowsAcrossGapsAndInvalidation(t *testing.T) {
	tracker := newEfficiencyTracker()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if tracker.Observe(start, 49, 4_000) || tracker.Observe(start.Add(30*time.Second), 50, 4_000) {
		t.Fatal("opening transition changed completed summary")
	}
	if !tracker.Observe(start.Add(61*time.Second), 51, 4_000) {
		t.Fatal("sample gap did not reject active window")
	}

	// A process restart/integration reset cannot join the discarded partial window.
	if tracker.Observe(start.Add(2*time.Minute), 49, 4_000) || tracker.Observe(start.Add(150*time.Second), 50, 4_000) {
		t.Fatal("fresh opening transition changed completed summary")
	}
	tracker.Invalidate()

	summary := tracker.Summary()
	if summary.Cycles != 0 || summary.RejectedWindows != 2 || summary.Percent != nil {
		t.Fatalf("summary = %+v, want two rejected partial windows", summary)
	}
}

func TestEfficiencyTrackerRejectsBackwardsTimeAndOverUnityWindows(t *testing.T) {
	tracker := newEfficiencyTracker()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tracker.Observe(start, 49, 4_000)
	tracker.Observe(start.Add(30*time.Second), 50, 4_000)
	if !tracker.Observe(start.Add(29*time.Second), 50, 4_000) {
		t.Fatal("backward timestamp did not reject active window")
	}

	observeEfficiencyCycle(t, tracker, start.Add(2*time.Hour), 4_000, -4_500)
	summary := tracker.Summary()
	if summary.Cycles != 0 || summary.RejectedWindows != 2 || summary.Percent != nil {
		t.Fatalf("summary = %+v, want backward and >100%% windows rejected", summary)
	}
}

func TestEfficiencySummaryPersistenceRejectsCorruptAggregate(t *testing.T) {
	dir := t.TempDir()
	recorder := NewRecorder(dir, .9, time.UTC)
	percent := 90.0
	valid := efficiencySummary{
		Percent:         &percent,
		Cycles:          1,
		InputKWh:        1,
		OutputKWh:       .9,
		WindowHours:     1,
		RejectedWindows: 2,
	}
	if err := recorder.SaveEfficiencySummary(valid); err != nil {
		t.Fatalf("SaveEfficiencySummary() error = %v", err)
	}
	loaded, err := NewRecorder(dir, .9, time.UTC).LoadEfficiencySummary()
	if err != nil {
		t.Fatalf("LoadEfficiencySummary() error = %v", err)
	}
	if loaded.Percent == nil || *loaded.Percent != 90 || loaded.Cycles != 1 || loaded.RejectedWindows != 2 {
		t.Fatalf("loaded summary = %+v, want persisted summary", loaded)
	}

	corrupt := []byte(`{"percent":90,"cycles":1,"input_kwh":1,"output_kwh":1.1,"window_hours":1,"rejected_windows":0}`)
	path := filepath.Join(dir, measuredEfficiencyFile)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(dir, .9, time.UTC).LoadEfficiencySummary(); err == nil {
		t.Fatal("LoadEfficiencySummary() error = nil for corrupt aggregate")
	}
	if err := newEfficiencyTracker().Restore(efficiencySummary{
		Percent:     &percent,
		Cycles:      1,
		InputKWh:    1,
		OutputKWh:   1.1,
		WindowHours: 1,
	}); err == nil {
		t.Fatal("Restore() error = nil for corrupt aggregate")
	}
}

func observeEfficiencyCycle(t *testing.T, tracker *efficiencyTracker, start time.Time, chargeW, dischargeW float64) {
	t.Helper()
	if tracker.Observe(start, 49, chargeW) {
		t.Fatal("initial sample changed completed summary")
	}
	at := start.Add(30 * time.Second)
	if tracker.Observe(at, 50, chargeW) {
		t.Fatal("upward crossing changed completed summary")
	}
	for soc := 51; soc <= 70; soc++ {
		at = at.Add(30 * time.Second)
		power := chargeW
		if soc == 70 {
			power = dischargeW
		}
		if tracker.Observe(at, soc, power) {
			t.Fatalf("rise at SOC %d changed completed summary", soc)
		}
	}
	for soc := 69; soc >= 50; soc-- {
		at = at.Add(30 * time.Second)
		if tracker.Observe(at, soc, dischargeW) {
			t.Fatalf("descent at SOC %d changed completed summary", soc)
		}
	}
	at = at.Add(30 * time.Second)
	if !tracker.Observe(at, 49, dischargeW) {
		t.Fatal("closing transition did not change completed summary")
	}
}

func assertFloatNear(t *testing.T, got, want float64, name string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %.12f, want %.12f", name, got, want)
	}
}

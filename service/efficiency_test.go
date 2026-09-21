package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
)

func TestEfficiencyTrackerMeasuresMidpointBoundaries(t *testing.T) {
	tracker := newEfficiencyTracker(30 * time.Second)
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
	tracker := newEfficiencyTracker(30 * time.Second)
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
	tracker := newEfficiencyTracker(30 * time.Second)
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
	tracker := newEfficiencyTracker(30 * time.Second)
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
	if err := newEfficiencyTracker(30 * time.Second).Restore(efficiencySummary{
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

// A one-point SOC movement opens a window that needs a 20-point rise, so it
// can only ever be discarded. Counting that as the same kind of event as a
// telemetry failure made the rejection total unreadable: on the live unit most
// of it was solar top-ups nudging a full battery between 98 and 99 percent.
func TestEfficiencyTrackerSeparatesSmallSwingsFromFaults(t *testing.T) {
	tracker := newEfficiencyTracker(30 * time.Second)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// 49 -> 50 -> 49: a window that never had a chance to qualify.
	tracker.Observe(start, 49, 1_000)
	tracker.Observe(start.Add(10*time.Second), 50, 1_000)
	if !tracker.Observe(start.Add(20*time.Second), 49, -1_000) {
		t.Fatal("closing the small window did not change the summary")
	}
	summary := tracker.Summary()
	if summary.UnqualifiedWindows != 1 || summary.InterruptedWindows != 0 {
		t.Fatalf("small swing counted as %+v, want one below-minimum-swing window", summary)
	}

	// A genuine telemetry interruption in the middle of a real rise.
	at := start.Add(time.Minute)
	tracker.Observe(at, 49, 1_000)
	tracker.Observe(at.Add(10*time.Second), 50, 1_000)
	tracker.Invalidate()
	summary = tracker.Summary()
	if summary.InterruptedWindows != 1 || summary.UnqualifiedWindows != 1 {
		t.Fatalf("interruption counted as %+v, want one of each", summary)
	}
	if summary.RejectedWindows != 2 {
		t.Fatalf("rejected total = %d, want the sum of the breakdown", summary.RejectedWindows)
	}
	if got := efficiencyRejectionSince(efficiencySummary{UnqualifiedWindows: 1, InterruptedWindows: 0}, summary); got != efficiencyRejectedInterrupted {
		t.Fatalf("reason = %q, want %q", got, efficiencyRejectedInterrupted)
	}
}

// A completed window whose energies cannot describe a round trip is a fault of
// its own kind, not a swing that was merely too small.
func TestEfficiencyTrackerFlagsImplausibleEnergySeparately(t *testing.T) {
	tracker := newEfficiencyTracker(30 * time.Second)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// Charging throughout, including the descent: output never accumulates.
	observeEfficiencyCycle(t, tracker, start, 4_000, 4_000)

	summary := tracker.Summary()
	if summary.ImplausibleWindows != 1 || summary.UnqualifiedWindows != 0 || summary.InterruptedWindows != 0 {
		t.Fatalf("summary = %+v, want one implausible window", summary)
	}
	if summary.Cycles != 0 {
		t.Fatalf("accepted %d windows with no measured output", summary.Cycles)
	}
}

// An aggregate written before the breakdown existed still loads: its parts are
// empty, which is less than the total rather than inconsistent with it.
func TestEfficiencySummaryAcceptsLegacyAggregateWithoutBreakdown(t *testing.T) {
	input, output := 13.82, 10.99
	percent := output / input * 100
	if err := validateEfficiencySummary(efficiencySummary{
		Percent: &percent, Cycles: 3, InputKWh: input, OutputKWh: output,
		WindowHours: 26.27, RejectedWindows: 13,
	}); err != nil {
		t.Fatalf("legacy aggregate rejected: %v", err)
	}
	if err := validateEfficiencySummary(efficiencySummary{
		RejectedWindows: 2, UnqualifiedWindows: 2, InterruptedWindows: 1,
	}); err == nil {
		t.Fatal("accepted a breakdown totalling more than its own total")
	}
}

// One ESPHome timeout used to discard the whole window. On the live unit a
// window spans a charge, an idle hold and a discharge, so a single failed read
// out of thousands destroyed hours of measurement.
func TestEfficiencySampleClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want efficiencySampleOutcome
	}{
		"good read":      {nil, efficiencySampleAccepted},
		"http timeout":   {context.DeadlineExceeded, efficiencySampleSkipped},
		"read failure":   {errors.New("GET /sensor/AC Power: connection reset"), efficiencySampleSkipped},
		"frozen link":    {fmt.Errorf("check link: %w", marstek.ErrLinkDown), efficiencySampleDiscarded},
		"wrapped freeze": {fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", marstek.ErrLinkDown)), efficiencySampleDiscarded},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyEfficiencySample(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// A skipped sample must leave the window intact, and the tracker must still
// reject it when the outage genuinely outlasts the tolerance.
func TestEfficiencyTrackerSurvivesASkippedSampleButNotALongOutage(t *testing.T) {
	tracker := newEfficiencyTracker(30 * time.Second)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tracker.Observe(start, 49, 4_000)
	tracker.Observe(start.Add(5*time.Second), 50, 4_000) // window opens

	// One read fails and is skipped, so the next sample simply lands later.
	resumed := start.Add(15 * time.Second)
	if tracker.Observe(resumed, 50, 4_000) {
		t.Fatal("a sample after a skipped read rejected the window")
	}
	if tracker.Summary().RejectedWindows != 0 {
		t.Fatalf("skipped sample discarded the window: %+v", tracker.Summary())
	}

	// An outage past the tolerance still rejects it, and says why.
	if !tracker.Observe(resumed.Add(maximumEfficiencySampleGap+time.Second), 50, 4_000) {
		t.Fatal("an outage beyond the tolerance did not reject the window")
	}
	summary := tracker.Summary()
	if summary.InterruptedWindows != 1 || summary.UnqualifiedWindows != 0 {
		t.Fatalf("long outage counted as %+v, want one telemetry interruption", summary)
	}
}

// observeEfficiencyCycleAt drives one qualifying cycle at the given cadence,
// optionally stalling once mid-charge so the window has to hold the last
// reading across an interval it never observed.
func observeEfficiencyCycleAt(tracker *efficiencyTracker, start time.Time, cadence, gap time.Duration, samplesPerPoint int, chargeW, dischargeW float64) {
	at := start
	tracker.Observe(at, 49, chargeW)
	at = at.Add(cadence)
	tracker.Observe(at, 50, chargeW)
	for soc := 51; soc <= 70; soc++ {
		for i := 0; i < samplesPerPoint; i++ {
			step := cadence
			if gap > 0 && soc == 60 && i == 0 {
				step, gap = gap, 0
			}
			at = at.Add(step)
			tracker.Observe(at, soc, chargeW)
		}
	}
	for soc := 69; soc >= 50; soc-- {
		for i := 0; i < samplesPerPoint; i++ {
			at = at.Add(cadence)
			tracker.Observe(at, soc, dischargeW)
		}
	}
	tracker.Observe(at.Add(cadence), 49, dischargeW)
}

// Tolerating a skipped read keeps the window alive, but the energy across the
// gap is a zero-order hold of the last reading, and it is wrong in direction
// whenever the gap spans a session start or stop. A window that had to invent
// too much of its own input is an estimate, not a measurement.
func TestEfficiencyTrackerRejectsWindowsBuiltOnHeldEnergy(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	clean := newEfficiencyTracker(5 * time.Second)
	observeEfficiencyCycleAt(clean, start, 5*time.Second, 0, 10, 4_000, -3_600)
	if summary := clean.Summary(); summary.Cycles != 1 || summary.RejectedWindows != 0 {
		t.Fatalf("uninterrupted cycle = %+v, want one accepted window", summary)
	}

	stalled := newEfficiencyTracker(5 * time.Second)
	observeEfficiencyCycleAt(stalled, start, 5*time.Second, 25*time.Second, 10, 4_000, -3_600)
	summary := stalled.Summary()
	if summary.Cycles != 0 || summary.InterruptedWindows != 1 {
		t.Fatalf("stalled cycle = %+v, want it discarded as interrupted", summary)
	}
}

// The tracker cannot tell a gap from a sample when the sampler's own cadence is
// as long as the tolerance, so the guard stays inert instead of rejecting every
// window. This is what the 30-second-cadence tests above rely on.
func TestEfficiencyHeldGuardInertAtCoarseCadence(t *testing.T) {
	tracker := newEfficiencyTracker(maximumEfficiencySampleGap)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observeEfficiencyCycleAt(tracker, start, 30*time.Second, 0, 2, 4_000, -3_600)
	if summary := tracker.Summary(); summary.Cycles != 1 {
		t.Fatalf("coarse-cadence cycle = %+v, want it accepted", summary)
	}
}

type acStep struct {
	soc    int
	powerW float64
	err    error
}

// scriptedACSampler plays a fixed sequence of read results and then signals,
// so the sampler loop can be driven deterministically.
type scriptedACSampler struct {
	mu       sync.Mutex
	steps    []acStep
	next     int
	finished chan struct{}
}

func newScriptedACSampler(steps ...acStep) *scriptedACSampler {
	return &scriptedACSampler{steps: steps, finished: make(chan struct{})}
}

func (s *scriptedACSampler) GetACSample(context.Context) (int, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.steps) {
		select {
		case <-s.finished:
		default:
			close(s.finished)
		}
		return 0, 0, context.Canceled
	}
	step := s.steps[s.next]
	s.next++
	return step.soc, step.powerW, step.err
}

func runScriptedEfficiencySampler(t *testing.T, reader *scriptedACSampler) efficiencySummary {
	t.Helper()
	svc := &Service{efficiency: newEfficiencyTracker(time.Millisecond)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.runEfficiencyTracking(ctx, reader, time.Millisecond)
	}()
	select {
	case <-reader.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not consume the script")
	}
	cancel()
	<-done
	return svc.efficiency.Summary()
}

// The regression lived in the sampler loop, not in the classifier: it called
// Invalidate() on every read error, so one HTTP timeout discarded a window
// that had been accumulating for hours.
func TestEfficiencySamplerKeepsWindowThroughFailedReads(t *testing.T) {
	timeout := errors.New("Get /sensor/AC Power: context deadline exceeded")
	summary := runScriptedEfficiencySampler(t, newScriptedACSampler(
		acStep{soc: 49, powerW: 4_000},
		acStep{soc: 50, powerW: 4_000}, // window opens
		acStep{err: timeout},
		acStep{err: timeout},
		acStep{err: timeout},
		acStep{soc: 50, powerW: 4_000},
	))
	if summary.RejectedWindows != 0 {
		t.Fatalf("failed reads discarded the window: %+v", summary)
	}
}

// A confirmed freeze is different: further readings are cached values that
// would be integrated as if they were live.
func TestEfficiencySamplerDiscardsWindowOnConfirmedFreeze(t *testing.T) {
	summary := runScriptedEfficiencySampler(t, newScriptedACSampler(
		acStep{soc: 49, powerW: 4_000},
		acStep{soc: 50, powerW: 4_000}, // window opens
		acStep{err: fmt.Errorf("get SOC: %w", marstek.ErrLinkDown)},
	))
	if summary.InterruptedWindows != 1 || summary.RejectedWindows != 1 {
		t.Fatalf("confirmed freeze did not discard the window: %+v", summary)
	}
}

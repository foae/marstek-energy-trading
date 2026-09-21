package service

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	measuredEfficiencyFile     = "measured-efficiency.json"
	maximumEfficiencySampleGap = 30 * time.Second
	maximumEfficiencyWindow    = 72 * time.Hour
	minimumEfficiencySOCRise   = 20
	minimumEfficiencyInputKWh  = 0.5
	// An interval this many times the expected sampling cadence is a gap the
	// sampler did not observe. Its energy is a zero-order hold of the last
	// reading, not a measurement, and the held value is wrong in direction
	// whenever the gap spans a session start or stop.
	efficiencyHeldIntervalFactor = 2
	// Tolerating a skipped read keeps a window alive through a brief outage,
	// but a window that had to invent this share of its own input is an
	// estimate rather than a measurement, so it is discarded instead.
	maximumEfficiencyHeldShare      = 0.02
	maximumEfficiencySummaryCycles  = 1_000_000_000
	maximumEfficiencyAggregateKWh   = 1_000_000_000
	percentConsistencyRelativeError = 1e-9
)

// efficiencySummary is the completed, measured AC round-trip efficiency.
// Percent is nil until at least one acceptable cycle has completed.
type efficiencySummary struct {
	Percent         *float64 `json:"percent"`
	Cycles          int      `json:"cycles"`
	InputKWh        float64  `json:"input_kwh"`
	OutputKWh       float64  `json:"output_kwh"`
	WindowHours     float64  `json:"window_hours"`
	RejectedWindows int      `json:"rejected_windows"`
	// Breakdown of RejectedWindows; each rejected window lands in exactly one.
	// Without it the total reads as a fault count, when in practice most of it
	// is the expected churn of small SOC movements that could never qualify:
	// a one-point solar top-up opens a window needing a 20-point rise.
	// Restored aggregates written before the split have an empty breakdown.
	UnqualifiedWindows int `json:"unqualified_windows"`
	InterruptedWindows int `json:"interrupted_windows"`
	ImplausibleWindows int `json:"implausible_windows"`
}

// efficiencyRejection explains why a window did not become a measurement.
type efficiencyRejection string

const (
	// The swing was too small to measure, which is normal and expected.
	efficiencyRejectedUnqualified efficiencyRejection = "below_minimum_swing"
	// Telemetry stopped or jumped, so the energy integral has a hole in it.
	efficiencyRejectedInterrupted efficiencyRejection = "telemetry_interrupted"
	// The window completed but its energies cannot describe a round trip.
	efficiencyRejectedImplausible efficiencyRejection = "implausible_energy"
	efficiencyRejectedUnknown     efficiencyRejection = "unknown"
)

// efficiencyRejectionSince names the bucket that grew between two summaries.
func efficiencyRejectionSince(before, after efficiencySummary) efficiencyRejection {
	switch {
	case after.UnqualifiedWindows > before.UnqualifiedWindows:
		return efficiencyRejectedUnqualified
	case after.InterruptedWindows > before.InterruptedWindows:
		return efficiencyRejectedInterrupted
	case after.ImplausibleWindows > before.ImplausibleWindows:
		return efficiencyRejectedImplausible
	default:
		return efficiencyRejectedUnknown
	}
}

type efficiencyObservation struct {
	at       time.Time
	soc      int
	acPowerW float64
}

type activeEfficiencyWindow struct {
	anchorSOC int
	startedAt time.Time
	qualified bool
	inputKWh  float64
	outputKWh float64
	// heldKWh is the part of the above that was integrated across intervals
	// the sampler never observed, and so bounds how much of the window is
	// assumption rather than measurement.
	heldKWh   float64
	durationS float64
}

// efficiencyTracker turns consecutive SOC transitions and AC power samples into
// completed round-trip efficiency windows. Its mutex protects both observations
// and the accumulated summary.
type efficiencyTracker struct {
	mu      sync.Mutex
	summary efficiencySummary
	// heldIntervalS is the interval beyond which an interval is treated as an
	// unobserved gap rather than a sample. It follows the cadence the sampler
	// says it will keep, because only that says what "longer than expected"
	// means. A cadence at or beyond maximumEfficiencySampleGap cannot tell a
	// gap from a normal interval, which leaves the guard inert rather than
	// rejecting every window.
	heldIntervalS float64
	previous      *efficiencyObservation
	active        *activeEfficiencyWindow
}

func newEfficiencyTracker(expectedInterval time.Duration) *efficiencyTracker {
	return &efficiencyTracker{heldIntervalS: efficiencyHeldIntervalFactor * expectedInterval.Seconds()}
}

// Observe records one AC power sample. It reports whether the completed summary
// changed, which happens only when a window is accepted or rejected.
func (t *efficiencyTracker) Observe(at time.Time, soc int, acPowerW float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !validEfficiencySample(at, soc, acPowerW) {
		return t.invalidateLocked()
	}
	current := efficiencyObservation{at: at, soc: soc, acPowerW: acPowerW}
	if t.previous == nil {
		t.previous = &current
		return false
	}

	previous := *t.previous
	elapsed := current.at.Sub(previous.at)
	if elapsed <= 0 || elapsed > maximumEfficiencySampleGap || absInt(current.soc-previous.soc) > 1 {
		changed := t.invalidateLocked()
		// The current valid sample is a new observation, never a continuation of
		// the discarded interval.
		t.previous = &current
		return changed
	}

	if t.active == nil {
		if current.soc == previous.soc+1 {
			active := &activeEfficiencyWindow{
				anchorSOC: current.soc,
				startedAt: previous.at.Add(elapsed / 2),
			}
			if !active.addInterval(previous.acPowerW, elapsed.Seconds()/2, t.heldIntervalS) {
				t.previous = &current
				t.active = active
				return t.invalidateLocked()
			}
			t.active = active
		}
		t.previous = &current
		return false
	}

	active := t.active
	if current.soc == active.anchorSOC-1 && previous.soc == active.anchorSOC {
		if active.startedAt.Add(maximumEfficiencyWindow).Before(previous.at.Add(elapsed / 2)) {
			t.previous = &current
			return t.invalidateLocked()
		}
		if !active.addInterval(previous.acPowerW, elapsed.Seconds()/2, t.heldIntervalS) {
			t.previous = &current
			return t.invalidateLocked()
		}
		t.active = nil
		t.previous = &current
		if !active.qualified || active.inputKWh < minimumEfficiencyInputKWh {
			t.rejectLocked(efficiencyRejectedUnqualified)
			return true
		}
		if active.heldKWh > maximumEfficiencyHeldShare*active.inputKWh {
			t.rejectLocked(efficiencyRejectedInterrupted)
			return true
		}
		if active.outputKWh <= 0 || active.outputKWh > active.inputKWh {
			t.rejectLocked(efficiencyRejectedImplausible)
			return true
		}
		t.summary.Cycles++
		t.summary.InputKWh += active.inputKWh
		t.summary.OutputKWh += active.outputKWh
		t.summary.WindowHours += active.durationS / 3600
		t.updatePercentLocked()
		return true
	}

	if active.startedAt.Add(maximumEfficiencyWindow).Before(current.at) {
		t.previous = &current
		return t.invalidateLocked()
	}
	if !active.addInterval(previous.acPowerW, elapsed.Seconds(), t.heldIntervalS) {
		t.previous = &current
		return t.invalidateLocked()
	}
	if current.soc >= active.anchorSOC+minimumEfficiencySOCRise {
		active.qualified = true
	}
	t.previous = &current
	return false
}

// Invalidate discards incomplete telemetry. It records a rejection only when a
// window was active; samples collected while waiting cannot be a rejected cycle.
func (t *efficiencyTracker) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.invalidateLocked()
}

func (t *efficiencyTracker) invalidateLocked() bool {
	changed := t.active != nil
	if changed {
		t.rejectLocked(efficiencyRejectedInterrupted)
	}
	t.active = nil
	t.previous = nil
	return changed
}

// rejectLocked records one non-accepted window under its reason. The total is
// kept as the sum so existing metrics and history keep their meaning.
func (t *efficiencyTracker) rejectLocked(reason efficiencyRejection) {
	t.summary.RejectedWindows++
	switch reason {
	case efficiencyRejectedUnqualified:
		t.summary.UnqualifiedWindows++
	case efficiencyRejectedInterrupted:
		t.summary.InterruptedWindows++
	case efficiencyRejectedImplausible:
		t.summary.ImplausibleWindows++
	}
}

// Summary returns a stable copy of the completed aggregate.
func (t *efficiencyTracker) Summary() efficiencySummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return copyEfficiencySummary(t.summary)
}

// Restore replaces the completed aggregate. Incomplete observations are always
// discarded so a process restart cannot join old and new sample streams.
func (t *efficiencyTracker) Restore(summary efficiencySummary) error {
	if err := validateEfficiencySummary(summary); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.summary = copyEfficiencySummary(summary)
	t.active = nil
	t.previous = nil
	return nil
}

func validEfficiencySample(at time.Time, soc int, acPowerW float64) bool {
	return !at.IsZero() && soc >= 0 && soc <= 100 && isFinite(acPowerW)
}

func (w *activeEfficiencyWindow) addInterval(powerW, seconds, heldIntervalS float64) bool {
	if seconds < 0 || !isFinite(seconds) {
		return false
	}
	energyKWh := math.Abs(powerW) * seconds / 3_600_000
	if !isFinite(energyKWh) {
		return false
	}
	if powerW > 0 {
		w.inputKWh += energyKWh
	} else if powerW < 0 {
		w.outputKWh += energyKWh
	}
	if heldIntervalS > 0 && seconds > heldIntervalS {
		w.heldKWh += energyKWh
	}
	w.durationS += seconds
	return isFinite(w.inputKWh) && isFinite(w.outputKWh) && isFinite(w.heldKWh) && isFinite(w.durationS)
}

func (t *efficiencyTracker) updatePercentLocked() {
	if t.summary.InputKWh == 0 {
		t.summary.Percent = nil
		return
	}
	percent := t.summary.OutputKWh / t.summary.InputKWh * 100
	t.summary.Percent = &percent
}

func copyEfficiencySummary(summary efficiencySummary) efficiencySummary {
	copy := summary
	if summary.Percent != nil {
		percent := *summary.Percent
		copy.Percent = &percent
	}
	return copy
}

func validateEfficiencySummary(summary efficiencySummary) error {
	if summary.Cycles < 0 || summary.Cycles > maximumEfficiencySummaryCycles {
		return fmt.Errorf("invalid measured efficiency cycle count")
	}
	if summary.RejectedWindows < 0 || summary.RejectedWindows > maximumEfficiencySummaryCycles {
		return fmt.Errorf("invalid measured efficiency rejected window count")
	}
	// Each is bounded on its own, so the sum below cannot overflow past the
	// comparison it is meant to fail.
	for _, count := range []int{summary.UnqualifiedWindows, summary.InterruptedWindows, summary.ImplausibleWindows} {
		if count < 0 || count > maximumEfficiencySummaryCycles {
			return fmt.Errorf("invalid measured efficiency rejection breakdown")
		}
	}
	// Aggregates written before the split carry an empty breakdown, so the
	// parts may total less than the whole but never more.
	if summary.UnqualifiedWindows+summary.InterruptedWindows+summary.ImplausibleWindows > summary.RejectedWindows {
		return fmt.Errorf("measured efficiency rejection breakdown exceeds its total")
	}
	if !validEfficiencyAggregate(summary.InputKWh) || !validEfficiencyAggregate(summary.OutputKWh) || !validEfficiencyAggregate(summary.WindowHours) {
		return fmt.Errorf("invalid measured efficiency aggregate")
	}
	if summary.Cycles == 0 {
		if summary.Percent != nil || summary.InputKWh != 0 || summary.OutputKWh != 0 || summary.WindowHours != 0 {
			return fmt.Errorf("measured efficiency has aggregate data without completed cycles")
		}
		return nil
	}
	if summary.Percent == nil || !isFinite(*summary.Percent) || summary.InputKWh < minimumEfficiencyInputKWh*float64(summary.Cycles) || summary.OutputKWh <= 0 || summary.OutputKWh > summary.InputKWh || summary.WindowHours <= 0 || summary.WindowHours > maximumEfficiencyWindow.Hours()*float64(summary.Cycles) {
		return fmt.Errorf("invalid measured efficiency completed aggregate")
	}
	expectedPercent := summary.OutputKWh / summary.InputKWh * 100
	if *summary.Percent < 0 || *summary.Percent > 100 || math.Abs(*summary.Percent-expectedPercent) > percentConsistencyRelativeError*math.Max(1, math.Abs(expectedPercent)) {
		return fmt.Errorf("inconsistent measured efficiency percentage")
	}
	return nil
}

func validEfficiencyAggregate(value float64) bool {
	return isFinite(value) && value >= 0 && value <= maximumEfficiencyAggregateKWh
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// SaveEfficiencySummary persists the measured aggregate without affecting trade
// persistence state: measured telemetry is informational and must not block trading.
func (r *Recorder) SaveEfficiencySummary(summary efficiencySummary) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dataDir == "" {
		return nil
	}
	if err := validateEfficiencySummary(summary); err != nil {
		return fmt.Errorf("validate measured efficiency summary: %w", err)
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal measured efficiency summary: %w", err)
	}
	if err := r.saveJSONFile(measuredEfficiencyFile, data); err != nil {
		return fmt.Errorf("save measured efficiency summary: %w", err)
	}
	return nil
}

// LoadEfficiencySummary loads the completed measured aggregate. A missing file
// is an empty, valid aggregate.
func (r *Recorder) LoadEfficiencySummary() (efficiencySummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dataDir == "" {
		return efficiencySummary{}, nil
	}
	path := filepath.Join(r.dataDir, measuredEfficiencyFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return efficiencySummary{}, nil
		}
		return efficiencySummary{}, fmt.Errorf("read measured efficiency summary %s: %w", path, err)
	}
	var summary efficiencySummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return efficiencySummary{}, fmt.Errorf("unmarshal measured efficiency summary %s: %w", path, err)
	}
	if err := validateEfficiencySummary(summary); err != nil {
		return efficiencySummary{}, fmt.Errorf("validate measured efficiency summary %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return efficiencySummary{}, fmt.Errorf("protect measured efficiency summary %s: %w", path, err)
	}
	return copyEfficiencySummary(summary), nil
}

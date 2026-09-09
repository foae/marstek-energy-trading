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
	measuredEfficiencyFile          = "measured-efficiency.json"
	maximumEfficiencySampleGap      = 30 * time.Second
	maximumEfficiencyWindow         = 72 * time.Hour
	minimumEfficiencySOCRise        = 20
	minimumEfficiencyInputKWh       = 0.5
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
	durationS float64
}

// efficiencyTracker turns consecutive SOC transitions and AC power samples into
// completed round-trip efficiency windows. Its mutex protects both observations
// and the accumulated summary.
type efficiencyTracker struct {
	mu       sync.Mutex
	summary  efficiencySummary
	previous *efficiencyObservation
	active   *activeEfficiencyWindow
}

func newEfficiencyTracker() *efficiencyTracker {
	return &efficiencyTracker{}
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
			if !active.addInterval(previous.acPowerW, elapsed.Seconds()/2) {
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
		if !active.addInterval(previous.acPowerW, elapsed.Seconds()/2) {
			t.previous = &current
			return t.invalidateLocked()
		}
		t.active = nil
		t.previous = &current
		if !active.qualified || active.inputKWh < minimumEfficiencyInputKWh || active.outputKWh <= 0 || active.outputKWh > active.inputKWh {
			t.summary.RejectedWindows++
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
	if !active.addInterval(previous.acPowerW, elapsed.Seconds()) {
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
		t.summary.RejectedWindows++
	}
	t.active = nil
	t.previous = nil
	return changed
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

func (w *activeEfficiencyWindow) addInterval(powerW, seconds float64) bool {
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
	w.durationS += seconds
	return isFinite(w.inputKWh) && isFinite(w.outputKWh) && isFinite(w.durationS)
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

package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/telegram"
)

const (
	efficiencySampleInterval = 5 * time.Second
	// Two sequential HTTP reads on a hub that is also serving the control loop
	// need more than a couple of seconds under load. This stays well inside
	// maximumEfficiencySampleGap so a slow read costs one sample, not a window.
	efficiencySampleTimeout = 8 * time.Second
	// The link probe costs four more sensor reads on the bus whose overload
	// causes the timeouts in the first place. It is throttled rather than run
	// on every sample: the service's other one-second telemetry readers keep
	// the client's staleness signal current between probes, and the client
	// only reports a freeze after two minutes of unchanged telemetry, so a
	// probe this often still detects one well inside that threshold.
	efficiencyLinkProbeInterval = 15 * time.Second
)

// efficiencySampleOutcome is what a read result means for the window in
// progress. The distinction is the whole point: discarding on every failed
// read threw away hours of accumulated measurement for one HTTP timeout.
type efficiencySampleOutcome int

const (
	efficiencySampleAccepted efficiencySampleOutcome = iota
	// The read failed, but the battery may be perfectly fine. Skip the sample
	// and let the tracker's own gap tolerance decide the window's fate.
	efficiencySampleSkipped
	// Telemetry is confirmed frozen, so further readings are cached values
	// that would silently corrupt the energy integral.
	efficiencySampleDiscarded
)

func classifyEfficiencySample(err error) efficiencySampleOutcome {
	switch {
	case err == nil:
		return efficiencySampleAccepted
	case errors.Is(err, marstek.ErrLinkDown):
		return efficiencySampleDiscarded
	default:
		return efficiencySampleSkipped
	}
}

// acEfficiencySampler is informational telemetry, not a control confirmation.
// Power is measured at the AC boundary, positive for input and negative for output.
// Legacy backends without a verified AC measurement do not implement it.
type acEfficiencySampler interface {
	GetACSample(context.Context) (soc int, powerW float64, err error)
}

func (s *Service) measuredEfficiencySummary() efficiencySummary {
	if s.efficiency == nil {
		return efficiencySummary{}
	}
	return s.efficiency.Summary()
}

func (s *Service) measuredEfficiencyData() telegram.EfficiencyData {
	summary := s.measuredEfficiencySummary()
	return telegram.EfficiencyData{
		Percent:     summary.Percent,
		Cycles:      summary.Cycles,
		WindowHours: summary.WindowHours,
	}
}

// startEfficiencyTracking runs independently of the serialized control loop:
// confirmed battery writes can block that loop for longer than a sample interval.
// This worker only reads HTTP telemetry and never sends battery commands.
// Its returned function cancels and joins it before Start returns.
func (s *Service) startEfficiencyTracking(ctx context.Context) func() {
	reader, ok := s.battery.(acEfficiencySampler)
	if !ok || s.efficiency == nil {
		slog.Info("measured AC round-trip efficiency unavailable", "reason", "backend has no verified AC telemetry")
		return func() {}
	}
	if s.recorder != nil {
		summary, err := s.recorder.LoadEfficiencySummary()
		if err == nil {
			err = s.efficiency.Restore(summary)
		}
		if err != nil {
			slog.Warn("prior measured efficiency unavailable; collecting new complete windows", "error", err)
		}
	}
	slog.Info("measured AC round-trip efficiency", "measurement", s.efficiency.Summary(), "includes_standby", true)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runEfficiencyTracking(workerCtx, reader, efficiencySampleInterval)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *Service) runEfficiencyTracking(ctx context.Context, reader acEfficiencySampler, interval time.Duration) {
	l := slog.With("measurement", "operational_ac_round_trip_efficiency")
	// Paced by a timer reset after each iteration rather than a free-running
	// ticker. The read budget is longer than the interval, so a ticker would
	// already have a tick waiting whenever a read timed out and the next
	// attempt would start immediately, hammering a bus that is failing
	// precisely because it is overloaded.
	timer := time.NewTimer(interval)
	defer timer.Stop()
	var lastReadWarning, lastSaveWarning, nextSaveAttempt, lastLinkProbe time.Time
	dirty := false
	for {
		if ctx.Err() != nil {
			return
		}
		before := s.efficiency.Summary()
		// Bound the whole read sequence, not each individual HTTP request.
		sampleCtx, cancel := context.WithTimeout(ctx, efficiencySampleTimeout)
		var probeErr error
		if checker, ok := s.battery.(LinkChecker); ok && (lastLinkProbe.IsZero() || time.Since(lastLinkProbe) >= efficiencyLinkProbeInterval) {
			// Probe before reading, and throttle the probe itself: a failing
			// bus must not be retried four extra times every sample.
			lastLinkProbe = time.Now()
			probeErr = checker.CheckLink(sampleCtx)
		}
		err := probeErr
		var soc int
		var powerW float64
		// Only a confirmed freeze makes the reading worthless. An ordinary
		// probe failure says nothing about the battery, so it must not turn an
		// otherwise healthy sample into a skip.
		if !errors.Is(probeErr, marstek.ErrLinkDown) {
			soc, powerW, err = reader.GetACSample(sampleCtx)
		}
		cancel()
		at := time.Now()
		if ctx.Err() != nil {
			return
		}
		switch classifyEfficiencySample(err) {
		case efficiencySampleAccepted:
			s.efficiency.Observe(at, soc, powerW)
		case efficiencySampleDiscarded:
			s.efficiency.Invalidate()
			if lastReadWarning.IsZero() || at.Sub(lastReadWarning) >= 15*time.Minute {
				l.Warn("efficiency telemetry frozen; incomplete measurement discarded", "error", err)
				lastReadWarning = at
			}
		case efficiencySampleSkipped:
			if lastReadWarning.IsZero() || at.Sub(lastReadWarning) >= 15*time.Minute {
				l.Warn("efficiency telemetry read failed; sample skipped", "error", err)
				lastReadWarning = at
			}
		}
		summary := s.efficiency.Summary()
		if summary.Cycles != before.Cycles || summary.RejectedWindows != before.RejectedWindows {
			dirty = true
			if summary.Cycles != before.Cycles {
				l.Info("measured AC round-trip efficiency updated", "result", summary, "includes_standby", true)
			} else {
				l.Info("efficiency window discarded",
					"reason", string(efficiencyRejectionSince(before, summary)),
					"rejected_windows", summary.RejectedWindows,
					"below_minimum_swing", summary.UnqualifiedWindows,
					"telemetry_interrupted", summary.InterruptedWindows,
					"implausible_energy", summary.ImplausibleWindows)
			}
		}
		if dirty && s.recorder != nil && !at.Before(nextSaveAttempt) {
			if err := s.recorder.SaveEfficiencySummary(summary); err != nil {
				nextSaveAttempt = at.Add(time.Minute)
				if lastSaveWarning.IsZero() || at.Sub(lastSaveWarning) >= 15*time.Minute {
					l.Warn("measured efficiency is not durable; retrying persistence", "error", err)
					lastSaveWarning = at
				}
			} else {
				dirty = false
				nextSaveAttempt = time.Time{}
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

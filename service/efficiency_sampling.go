package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/foae/marstek-energy-trading/clients/telegram"
)

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
		s.runEfficiencyTracking(workerCtx, reader)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *Service) runEfficiencyTracking(ctx context.Context, reader acEfficiencySampler) {
	l := slog.With("measurement", "operational_ac_round_trip_efficiency")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastReadWarning, lastSaveWarning, nextSaveAttempt time.Time
	dirty := false
	for {
		if ctx.Err() != nil {
			return
		}
		before := s.efficiency.Summary()
		// Bound the whole read sequence, not each individual HTTP request.
		sampleCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		var err error
		if checker, ok := s.battery.(LinkChecker); ok {
			err = checker.CheckLink(sampleCtx)
		}
		var soc int
		var powerW float64
		if err == nil {
			soc, powerW, err = reader.GetACSample(sampleCtx)
		}
		cancel()
		at := time.Now()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.efficiency.Invalidate()
			if lastReadWarning.IsZero() || at.Sub(lastReadWarning) >= 15*time.Minute {
				l.Warn("efficiency telemetry unavailable; incomplete measurement discarded", "error", err)
				lastReadWarning = at
			}
		} else {
			s.efficiency.Observe(at, soc, powerW)
		}
		summary := s.efficiency.Summary()
		if summary.Cycles != before.Cycles || summary.RejectedWindows != before.RejectedWindows {
			dirty = true
			if summary.Cycles != before.Cycles {
				l.Info("measured AC round-trip efficiency updated", "result", summary, "includes_standby", true)
			} else {
				l.Info("incomplete or invalid efficiency window discarded", "rejected_windows", summary.RejectedWindows)
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
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/shopspring/decimal"
)

func TestInventorySaleRetainsEndpointAcrossSOCAndMissingTariffs(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(now, .30, .30), now)
	svc.nowFunc = func() time.Time { return now }
	svc.tick(context.Background())
	if svc.state != StateDischarging || svc.activeInventorySale == nil {
		t.Fatalf("inventory sale did not start: state=%s plan=%+v", svc.state, svc.currentPlan)
	}
	end := svc.activeInventorySale.End
	now = now.Add(time.Minute)
	battery.SOC = 40
	svc.todayPrices = nil
	svc.tick(context.Background())
	if svc.state != StateDischarging || svc.activeInventorySale == nil || !svc.activeInventorySale.End.Equal(end) {
		t.Fatalf("missing tariff or lower SOC changed active sale: state=%s sale=%+v", svc.state, svc.activeInventorySale)
	}
	now = now.Add(time.Minute)
	svc.todayPrices = []nordpool.Price{{Time: now.Truncate(15 * time.Minute), Value: .20, HasExportValue: true, ExportValue: 0}}
	svc.tick(context.Background())
	if svc.state != StateIdle || svc.activeInventorySale != nil {
		t.Fatalf("confirmed zero export did not stop inventory sale: state=%s", svc.state)
	}
	if persisted, err := svc.recorder.LoadAutomaticCycleCommitment(); err != nil || persisted != nil {
		t.Fatalf("inventory created grid obligation: %+v %v", persisted, err)
	}
}

func TestSolarCaptureIgnoresUnknownNegativeAndExpensiveTariffs(t *testing.T) {
	for _, prices := range []struct {
		name            string
		current, future float64
		missing         bool
	}{
		{name: "unknown", missing: true},
		{name: "negative", current: -.20, future: -.10},
		{name: "expensive export before cheaper grid", current: .80, future: .01},
	} {
		t.Run(prices.name, func(t *testing.T) {
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			battery := NewMockBattery(50)
			meter := NewMockMeter(true, -500)
			svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
			svc.nowFunc = func() time.Time { return now }
			// A grid commitment owns a later sale, leaving this interval free for
			// capture even when its paired profit floor rejects this export value.
			cycle := &TradeCycle{ChargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}, DischargeWindow: TimeWindow{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: decimal.NewFromFloat(.30)}}
			svc.automaticCycleCommit = cycle
			svc.automaticCycleCommitDurable = true
			if !prices.missing {
				svc.todayPrices = []nordpool.Price{{Time: now, Value: prices.current}, {Time: now.Add(time.Hour), Value: prices.future}}
			}
			svc.solarTick(context.Background())
			for range 30 {
				now = now.Add(time.Second)
				svc.solarTick(context.Background())
			}
			if svc.state != StateSolarCharging {
				t.Fatalf("price vetoed surplus capture: state=%s", svc.state)
			}
			svc.todayPrices = nil
			now = now.Add(time.Second)
			svc.solarTick(context.Background())
			if svc.state != StateSolarCharging {
				t.Fatalf("missing tariff stopped capture: state=%s", svc.state)
			}
		})
	}
}

func TestTelemetryRecoveryPlansKnownInventoryAtUnchangedSOC(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(now, .30, .30), now)
	svc.mu.Lock()
	svc.batteryTelemetryAvailable = false
	svc.batteryTelemetrySOC = 50
	svc.refreshCurrentPlanLocked(now)
	svc.mu.Unlock()
	svc.tick(context.Background())
	if svc.state != StateDischarging || svc.activeInventorySale == nil {
		t.Fatalf("recovered inventory was not offered: state=%s plan=%+v", svc.state, svc.currentPlan)
	}
}

func TestUnchargedGridCycleCannotAuthorizeInventorySale(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(now, .30, .30), now)
	window := TimeWindow{Start: now, End: now.Add(15 * time.Minute), Price: decimal.NewFromFloat(.30)}
	svc.currentPlan = &TradingPlan{
		IsProfitable:     true,
		Cycles:           []TradeCycle{{ChargeWindow: TimeWindow{Start: now.Add(-time.Hour), End: now}, DischargeWindow: window}},
		DischargeWindows: []TimeWindow{window},
	}
	svc.mu.Lock()
	svc.startDischargingLocked(context.Background(), window.Price, true, 50, 1000, StateDischarging)
	svc.mu.Unlock()
	if svc.state != StateIdle || len(battery.DischargeCalls) != 0 {
		t.Fatalf("uncharged cycle authorized an unevaluated inventory sale: state=%s calls=%d", svc.state, len(battery.DischargeCalls))
	}
}

func TestControlLoopStopsPartialSaleWithoutTelemetryOrMinuteTick(t *testing.T) {
	now := time.Now()
	battery := NewMockBattery(50)
	battery.GetStatusErr = errors.New("telemetry unavailable")
	stopped := make(chan struct{}, 1)
	battery.IdleHook = func() {
		// Startup first resets control. The next idle must come from the
		// selected endpoint, not the one-minute ticker or shutdown.
		if battery.IdleAttempts == 2 {
			stopped <- struct{}{}
		}
	}
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.nowFunc = time.Now
	svc.state = StateDischarging
	svc.activeInventorySale = &TimeWindow{Start: now.Add(-time.Second), End: now.Add(100 * time.Millisecond)}
	svc.currentTradeStart = now.Add(-time.Second)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 50
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx) }()
	select {
	case <-stopped:
		if ctx.Err() != nil {
			t.Fatal("sale stopped only during shutdown")
		}
	case <-ctx.Done():
		t.Fatal("partial sale endpoint did not request idle")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("service shutdown: %v", err)
	}
}

package service

import (
	"context"
	"testing"
	"time"
)

func TestFullBatteryDischargesAfterPriceRefresh(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "idle_refresh"
		if restart {
			name = "restart_during_discharge"
		}
		t.Run(name, func(t *testing.T) {
			base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			now := base
			battery := NewMockBattery(99)
			prices := makePrices(base, 0.05, 0.10, 0.40, 0.35)
			svc := newTestService(testConfigSmallBattery(), battery, prices, now)
			svc.nowFunc = func() time.Time { return now }
			now = base.Add(30 * time.Minute)
			if restart {
				svc.currentPlan = nil
			}
			svc.checkPriceFetch(context.Background())
			svc.tick(context.Background())
			if svc.state != StateDischarging || battery.CurrentPower >= 0 {
				t.Fatalf("charged battery did not discharge after refresh: state=%s power=%d", svc.state, battery.CurrentPower)
			}
		})
	}
}

func TestIdleChargedCycleSurvivesMidnightRefresh(t *testing.T) {
	base := time.Date(2026, 9, 5, 23, 45, 0, 0, time.UTC)
	now := base
	battery := NewMockBattery(99)
	prices := makePrices(base, 0.05, 0.40, 0.35)
	svc := newTestService(testConfigSmallBattery(), battery, prices, now)
	svc.todayPrices = prices[:1]
	svc.tomorrowPrices = prices[1:]
	svc.nowFunc = func() time.Time { return now }
	now = base.Add(15 * time.Minute)
	svc.checkPriceFetch(context.Background())
	svc.tick(context.Background())
	if svc.state != StateDischarging || battery.CurrentPower >= 0 {
		t.Fatalf("midnight refresh lost charged cycle: state=%s power=%d", svc.state, battery.CurrentPower)
	}
}

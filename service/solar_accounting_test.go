package service

import (
	"context"
	"testing"
	"time"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/shopspring/decimal"
)

func TestSolarAccountingPricesIntervalsAndExposesGaps(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	prices := []nordpool.Price{{Time: base, Value: 0.1}, {Time: base.Add(15 * time.Minute), Value: -0.2}, {Time: base.Add(45 * time.Minute), Value: 0.3}}
	now := base.Add(14 * time.Minute)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), NewMockBattery(50), NewMockMeter(true, 600), prices, now)
	svc.SetClock(func() time.Time { return now })
	svc.state = StateSolarCharging
	svc.currentTradeStart = now
	svc.currentTradeSOC = 40
	svc.solarLastUpdate = now
	svc.solarMeasuredChargePowerW = 1200
	svc.solarGridPowerW = 600
	now = base.Add(46 * time.Minute)
	svc.mu.Lock()
	svc.stopSolarChargingLocked(context.Background(), 50, solarStopReasonYieldWindow)
	svc.mu.Unlock()
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one recorded session: %+v", history)
	}
	trade := history.Days[0].Trades[0]
	for name, check := range map[string]struct {
		got  decimal.Decimal
		want string
	}{
		"total":    {trade.EnergyKWh, "0.64"},
		"grid":     {trade.GridEnergyKWh, "0.32"},
		"unpriced": {trade.GridUnpricedKWh, "0.15"},
		"cost":     {trade.GridCostEUR, "-0.026"},
		"solar":    {history.Days[0].SolarChargedKWh, "0.32"},
	} {
		if !check.got.Equal(decimal.RequireFromString(check.want)) {
			t.Errorf("%s = %s, want %s", name, check.got, check.want)
		}
	}
}

package service

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestScheduledChargeAccountingSplitsMeasuredP1AcrossDays(t *testing.T) {
	start := time.Date(2026, 9, 5, 23, 59, 30, 0, time.UTC)
	end := start.Add(time.Minute)
	prices := func(at, until time.Time, export bool) (decimal.Decimal, time.Time, bool) {
		midnight := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
		if at.Before(midnight) {
			if export {
				return decimal.RequireFromString("-0.10"), midnight, true
			}
			return decimal.RequireFromString("0.10"), midnight, true
		}
		if export {
			return decimal.RequireFromString("0.20"), until, true
		}
		return decimal.RequireFromString("0.20"), until, true
	}

	var accounting scheduledChargeAccounting
	accounting.Begin(start, 3600, true, 1800, true)
	accounting.Sample(end, 3600, 1800, true, time.UTC, prices)
	trade := Trade{
		Timestamp: start,
		Action:    ActionCharge,
		DurationS: 60,
		EnergyKWh: decimal.RequireFromString("0.06"),
		DayAllocations: []TradeDayAllocation{
			{Timestamp: start, DurationS: 30, EnergyKWh: decimal.RequireFromString("0.03"), PricedValueEUR: decimal.RequireFromString("0.003")},
			{Timestamp: start.Add(30 * time.Second), DurationS: 30, EnergyKWh: decimal.RequireFromString("0.03"), PricedValueEUR: decimal.RequireFromString("0.006")},
		},
	}
	accounting.Apply(&trade, time.UTC)

	if trade.ChargeSourceAttribution != ChargeSourceMeasuredP1Split ||
		!trade.GridEnergyKWh.Equal(decimal.RequireFromString("0.03")) ||
		!trade.GridCostEUR.Equal(decimal.RequireFromString("0.0045")) ||
		!trade.OpportunityCostEUR.Equal(decimal.RequireFromString("0.0015")) {
		t.Fatalf("split trade = %+v", trade)
	}
	if err := NewRecorder("", .9, time.UTC).RecordTrade(trade); err != nil {
		t.Fatalf("split day allocations rejected: %v", err)
	}

	recorder := NewRecorder("", .9, time.UTC)
	if err := recorder.RecordTrade(trade); err != nil {
		t.Fatal(err)
	}
	history := recorder.GetHistory()
	if len(history.Days) != 2 {
		t.Fatalf("days = %+v, want two local days", history.Days)
	}
	if !history.Days[0].PnLEUR.Equal(decimal.RequireFromString("-0.003")) ||
		!history.Days[0].OpportunityAdjustedPnLEUR.Equal(decimal.RequireFromString("-0.006")) ||
		!history.Days[1].PnLEUR.Equal(decimal.RequireFromString("-0.0015")) ||
		!history.Days[1].OpportunityAdjustedPnLEUR.IsZero() {
		t.Fatalf("daily cash and signed opportunity values = %+v", history.Days)
	}
	if !history.TotalPnL.Equal(decimal.RequireFromString("-0.0045")) ||
		!history.TotalOpportunityAdjustedPnLEUR.Equal(decimal.RequireFromString("-0.006")) {
		t.Fatalf("history totals = %+v", history)
	}
}

func TestScheduledChargeAccountingUsesIntervalEnergyForFractionalSamples(t *testing.T) {
	start := time.Date(2026, 9, 5, 23, 59, 0, 0, time.UTC)
	midnight := start.Add(time.Minute)
	end := midnight.Add(30 * time.Second)
	prices := func(at, until time.Time, export bool) (decimal.Decimal, time.Time, bool) {
		if at.Before(midnight) {
			priceEnd := until
			if midnight.Before(until) {
				priceEnd = midnight
			}
			if export {
				return decimal.RequireFromString("-0.05"), priceEnd, true
			}
			return decimal.RequireFromString("0.10"), priceEnd, true
		}
		if export {
			return decimal.RequireFromString("0.30"), until, true
		}
		return decimal.RequireFromString("0.20"), until, true
	}

	var accounting scheduledChargeAccounting
	firstInterval := 31*time.Second + 125*time.Millisecond
	secondBeforeMidnight := midnight.Sub(start.Add(firstInterval))
	secondAfterMidnight := end.Sub(midnight)
	accounting.Begin(start, 2513.25, true, 711.5, true)
	accounting.Sample(start.Add(firstInterval), 1837.75, 512.25, true, time.UTC, prices)
	accounting.Sample(end, 0, 0, true, time.UTC, prices)
	trade := Trade{
		Timestamp: start,
		Action:    ActionCharge,
		DurationS: 90,
		DayAllocations: []TradeDayAllocation{
			{Timestamp: start, DurationS: 60},
			{Timestamp: midnight, DurationS: 30},
		},
	}
	accounting.Apply(&trade, time.UTC)

	firstEnergy := decimal.NewFromFloat(2513.25 * firstInterval.Seconds() / 3_600_000)
	beforeMidnightEnergy := decimal.NewFromFloat(1837.75 * secondBeforeMidnight.Seconds() / 3_600_000)
	afterMidnightEnergy := decimal.NewFromFloat(1837.75 * secondAfterMidnight.Seconds() / 3_600_000)
	firstGridEnergy := decimal.NewFromFloat(711.5 * firstInterval.Seconds() / 3_600_000)
	beforeMidnightGridEnergy := decimal.NewFromFloat(512.25 * secondBeforeMidnight.Seconds() / 3_600_000)
	afterMidnightGridEnergy := decimal.NewFromFloat(512.25 * secondAfterMidnight.Seconds() / 3_600_000)
	wantEnergy := firstEnergy.Add(beforeMidnightEnergy).Add(afterMidnightEnergy)
	wantGridEnergy := firstGridEnergy.Add(beforeMidnightGridEnergy).Add(afterMidnightGridEnergy)
	wantGridCost := firstGridEnergy.Add(beforeMidnightGridEnergy).Mul(decimal.RequireFromString("0.10")).
		Add(afterMidnightGridEnergy.Mul(decimal.RequireFromString("0.20")))
	wantOpportunityCost := firstEnergy.Sub(firstGridEnergy).Add(beforeMidnightEnergy.Sub(beforeMidnightGridEnergy)).
		Mul(decimal.RequireFromString("-0.05")).
		Add(afterMidnightEnergy.Sub(afterMidnightGridEnergy).Mul(decimal.RequireFromString("0.30")))
	if !trade.EnergyKWh.Equal(wantEnergy) ||
		!trade.GridEnergyKWh.Equal(wantGridEnergy) ||
		!trade.GridCostEUR.Equal(wantGridCost) ||
		!trade.OpportunityCostEUR.Equal(wantOpportunityCost) {
		t.Fatalf("interval-derived split trade = %+v", trade)
	}
	if !trade.DayAllocations[0].EnergyKWh.Equal(firstEnergy.Add(beforeMidnightEnergy)) ||
		!trade.DayAllocations[0].GridEnergyKWh.Equal(firstGridEnergy.Add(beforeMidnightGridEnergy)) ||
		!trade.DayAllocations[1].EnergyKWh.Equal(afterMidnightEnergy) ||
		!trade.DayAllocations[1].GridEnergyKWh.Equal(afterMidnightGridEnergy) {
		t.Fatalf("interval-derived day allocations = %+v", trade.DayAllocations)
	}
	if err := NewRecorder("", .9, time.UTC).RecordTrade(trade); err != nil {
		t.Fatalf("fractional measured split rejected: %v", err)
	}
}

func TestScheduledChargeAccountingKeepsLastP1AndMarksPreObservationEnergyUnattributed(t *testing.T) {
	start := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	price := func(_ time.Time, until time.Time, export bool) (decimal.Decimal, time.Time, bool) {
		if export {
			return decimal.RequireFromString("0.30"), until, true
		}
		return decimal.RequireFromString("0.10"), until, true
	}

	var accounting scheduledChargeAccounting
	accounting.Begin(start, 3600, true, 0, false)
	accounting.Sample(start.Add(30*time.Second), 3600, 1800, true, time.UTC, price)
	// The failed P1 sample keeps the prior 1800 W estimate for the second half.
	accounting.Sample(start.Add(time.Minute), 3600, 0, false, time.UTC, price)
	trade := Trade{
		Timestamp:      start,
		Action:         ActionCharge,
		DurationS:      60,
		EnergyKWh:      decimal.RequireFromString("0.06"),
		DayAllocations: []TradeDayAllocation{{Timestamp: start, DurationS: 60, EnergyKWh: decimal.RequireFromString("0.06")}},
	}
	accounting.Apply(&trade, time.UTC)

	if !trade.UnattributedEnergyKWh.Equal(decimal.RequireFromString("0.03")) ||
		!trade.GridEnergyKWh.Equal(decimal.RequireFromString("0.015")) ||
		!trade.GridCostEUR.Equal(decimal.RequireFromString("0.0015")) ||
		!trade.OpportunityCostEUR.Equal(decimal.RequireFromString("0.0045")) {
		t.Fatalf("gap attribution = %+v", trade)
	}
	r := NewRecorder("", .9, time.UTC)
	if err := r.RecordTrade(trade); err != nil {
		t.Fatal(err)
	}
	day := r.GetHistory().Days[0]
	if !day.UnattributedChargeKWh.Equal(decimal.RequireFromString("0.03")) ||
		!day.SolarChargedKWh.Equal(decimal.RequireFromString("0.015")) ||
		!day.GridChargedKWh.Equal(decimal.RequireFromString("0.015")) {
		t.Fatalf("day source limitations = %+v", day)
	}
}

func TestLegacyUnsplitScheduledChargeRemainsAllGrid(t *testing.T) {
	r := NewRecorder("", .9, time.UTC)
	trade := Trade{
		Timestamp: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		Action:    ActionCharge,
		PriceEUR:  decimal.RequireFromString("0.20"),
		EnergyKWh: decimal.NewFromInt(1),
	}
	if err := r.RecordTrade(trade); err != nil {
		t.Fatal(err)
	}
	day := r.GetHistory().Days[0]
	if !day.GridChargedKWh.Equal(decimal.NewFromInt(1)) || !day.SolarChargedKWh.IsZero() ||
		!day.PnLEUR.Equal(decimal.RequireFromString("-0.20")) {
		t.Fatalf("legacy unsplit charge changed interpretation: %+v", day)
	}
}

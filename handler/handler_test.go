package handler

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/service"
)

func TestHistoryMetricValuesReportsKnownCashFlowAndUnpricedEnergy(t *testing.T) {
	history := service.History{
		TotalPnL: decimal.NewFromFloat(.42),
		Days: []service.DailySummary{
			{CashFlowUnpricedKWh: decimal.NewFromFloat(.25)},
			{CashFlowUnpricedKWh: decimal.NewFromFloat(.10)},
		},
	}

	pnl, unpricedKWh := historyMetricValues(history)
	if math.Abs(pnl-.42) > .000001 || math.Abs(unpricedKWh-.35) > .000001 {
		t.Fatalf("metric values = pnl %.4f, unpriced %.4f; want 0.42 and 0.35", pnl, unpricedKWh)
	}
}

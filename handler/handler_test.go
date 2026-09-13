package handler

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/service"
)

func TestHistoryMetricValuesReportsAccountingLimitations(t *testing.T) {
	history := service.History{
		TotalPnL:                       decimal.NewFromFloat(.42),
		TotalOpportunityAdjustedPnLEUR: decimal.NewFromFloat(.31),
		TotalUnattributedChargeKWh:     decimal.NewFromFloat(.12),
		Days: []service.DailySummary{
			{CashFlowUnpricedKWh: decimal.NewFromFloat(.25), UnpricedKWh: decimal.NewFromFloat(.25)},
			{CashFlowUnpricedKWh: decimal.NewFromFloat(.10), UnpricedKWh: decimal.NewFromFloat(.35)},
		},
	}

	pnl, unpricedKWh, opportunityAdjusted, unattributedKWh, opportunityUnpricedKWh := historyMetricValues(history)
	if math.Abs(pnl-.42) > .000001 || math.Abs(unpricedKWh-.35) > .000001 ||
		math.Abs(opportunityAdjusted-.31) > .000001 || math.Abs(unattributedKWh-.12) > .000001 ||
		math.Abs(opportunityUnpricedKWh-.25) > .000001 {
		t.Fatalf("metric values = pnl %.4f unpriced %.4f adjusted %.4f unattributed %.4f opportunity-unpriced %.4f", pnl, unpricedKWh, opportunityAdjusted, unattributedKWh, opportunityUnpricedKWh)
	}
}

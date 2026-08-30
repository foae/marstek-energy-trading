package nordpool

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestAllInPricingApply(t *testing.T) {
	pricing := AllInPricing{
		EnergyTaxEURPerKWh:   decimal.RequireFromString("0.09161"),
		VATRate:              decimal.RequireFromString("0.21"),
		SupplierFeeEURPerKWh: decimal.RequireFromString("0.02"),
	}

	tests := []struct {
		name      string
		wholesale string
		want      string
	}{
		{name: "positive wholesale price", wholesale: "0.20051", want: "0.3734652"},
		{name: "near-zero wholesale price", wholesale: "0.00105", want: "0.1321186"},
		{name: "negative wholesale price", wholesale: "-0.01", want: "0.1187481"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pricing.Apply(decimal.RequireFromString(tt.wholesale))
			want := decimal.RequireFromString(tt.want)
			if !got.Equal(want) {
				t.Fatalf("Apply(%s) = %s, want %s", tt.wholesale, got, want)
			}
		})
	}
}

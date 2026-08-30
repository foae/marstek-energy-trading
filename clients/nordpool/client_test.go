package nordpool

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchDayAheadPricesAppliesAllInPricing(t *testing.T) {
	pricing := AllInPricing{
		EnergyTaxEURPerKWh:   decimal.RequireFromString("0.09161"),
		VATRate:              decimal.RequireFromString("0.21"),
		SupplierFeeEURPerKWh: decimal.RequireFromString("0.02"),
	}
	client := NewWithLocation("NL", "EUR", time.UTC, pricing)
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		if got := query.Get("date"); got != "2026-08-30" {
			t.Errorf("date query = %q, want 2026-08-30", got)
		}
		if got := query.Get("currency"); got != "EUR" {
			t.Errorf("currency query = %q, want EUR", got)
		}
		if got := query.Get("resolutionInMinutes"); got != "15" {
			t.Errorf("resolution query = %q, want 15", got)
		}

		body := `{
			"currency": "EUR",
			"resolutionInMinutes": 15,
			"multiIndexEntries": [{
				"deliveryStart": "2026-08-30T12:00:00Z",
				"deliveryEnd": "2026-08-30T12:15:00Z",
				"entryPerArea": {"NL": 89.68}
			}]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	prices, err := client.FetchDayAheadPrices(context.Background(), time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchDayAheadPrices() error = %v", err)
	}
	if len(prices) != 1 {
		t.Fatalf("len(prices) = %d, want 1", len(prices))
	}

	const want = 0.2393609 // (89.68/1000 + 0.09161) * 1.21 + 0.02
	if math.Abs(prices[0].Value-want) > 1e-12 {
		t.Errorf("price = %.10f, want %.10f", prices[0].Value, want)
	}
}

package nordpool

import (
	"context"
	"encoding/json"
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

		body := makeDayAheadResponse(t, time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), 89.68)
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
	if len(prices) != 96 {
		t.Fatalf("len(prices) = %d, want 96", len(prices))
	}

	const want = 0.2393609 // (89.68/1000 + 0.09161) * 1.21 + 0.02
	if math.Abs(prices[0].Value-want) > 1e-12 {
		t.Errorf("price = %.10f, want %.10f", prices[0].Value, want)
	}
}

func TestFetchDayAheadPricesRejectsInvalidResponseMetadata(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "wrong currency",
			body: `{"currency":"GBP","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-30T12:00:00Z","deliveryEnd":"2026-08-30T12:15:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "unexpected currency",
		},
		{
			name: "wrong resolution",
			body: `{"currency":"EUR","resolutionInMinutes":60,"multiIndexEntries":[{"deliveryStart":"2026-08-30T12:00:00Z","deliveryEnd":"2026-08-30T13:00:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "unexpected resolution",
		},
		{
			name: "null price",
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-30T00:00:00Z","deliveryEnd":"2026-08-30T00:15:00Z","entryPerArea":{"NL":null}}]}`,
			want: "no price for area",
		},
		{
			name: "wrong day",
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-31T12:00:00Z","deliveryEnd":"2026-08-31T12:15:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "outside requested day",
		},
		{
			name: "partial day",
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-30T00:00:00Z","deliveryEnd":"2026-08-30T00:15:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "incomplete price series",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewWithLocation("NL", "EUR", time.UTC, AllInPricing{})
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Header:     make(http.Header),
				}, nil
			})

			_, err := client.FetchDayAheadPrices(context.Background(), time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("FetchDayAheadPrices() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFetchDayAheadPricesAcceptsDSTDayLengths(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	tests := []struct {
		day       time.Time
		wantSlots int
	}{
		{day: time.Date(2026, 3, 29, 0, 0, 0, 0, loc), wantSlots: 92},
		{day: time.Date(2026, 10, 25, 0, 0, 0, 0, loc), wantSlots: 100},
	}
	for _, tt := range tests {
		t.Run(tt.day.Format("2006-01-02"), func(t *testing.T) {
			body := makeDayAheadResponse(t, tt.day, 50)
			client := NewWithLocation("NL", "EUR", loc, AllInPricing{})
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			})
			prices, err := client.FetchDayAheadPrices(context.Background(), tt.day)
			if err != nil {
				t.Fatalf("FetchDayAheadPrices() error = %v", err)
			}
			if len(prices) != tt.wantSlots {
				t.Fatalf("price slots = %d, want %d", len(prices), tt.wantSlots)
			}
		})
	}
}

func makeDayAheadResponse(t *testing.T, day time.Time, value float64) string {
	t.Helper()
	entries := make([]multiIndexEntry, 0, 96)
	for slot := day; slot.Before(day.AddDate(0, 0, 1)); slot = slot.Add(15 * time.Minute) {
		price := value
		entries = append(entries, multiIndexEntry{
			DeliveryStart: slot.Format(time.RFC3339),
			DeliveryEnd:   slot.Add(15 * time.Minute).Format(time.RFC3339),
			EntryPerArea:  map[string]*float64{"NL": &price},
		})
	}
	body, err := json.Marshal(apiResponse{
		DeliveryDateCET:   day.Format("2006-01-02"),
		Market:            "DayAhead",
		IndexNames:        []string{"NL"},
		Currency:          "EUR",
		ResolutionInMin:   15,
		MultiIndexEntries: entries,
	})
	if err != nil {
		t.Fatalf("marshal day-ahead response: %v", err)
	}
	return string(body)
}

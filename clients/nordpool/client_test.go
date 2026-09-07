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
	marketLoc := loadLocation(t, "Europe/Oslo")
	client := NewWithLocation("NL", "EUR", time.UTC, pricing)
	var requestedDates []string
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		requestedDates = append(requestedDates, query.Get("date"))
		if got := query.Get("currency"); got != "EUR" {
			t.Errorf("currency query = %q, want EUR", got)
		}
		if got := query.Get("resolutionInMinutes"); got != "15" {
			t.Errorf("resolution query = %q, want 15", got)
		}

		marketDay := parseMarketDate(t, query.Get("date"), marketLoc)
		return response(makeDayAheadResponse(t, marketDay, 89.68)), nil
	})

	requestedDay := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	prices, err := client.FetchDayAheadPrices(context.Background(), requestedDay)
	if err != nil {
		t.Fatalf("FetchDayAheadPrices() error = %v", err)
	}
	if len(prices) != 96 {
		t.Fatalf("len(prices) = %d, want 96", len(prices))
	}
	assertDateQueries(t, requestedDates, []string{"2026-08-30", "2026-08-31"})
	if !prices[0].Time.Equal(requestedDay) || prices[0].Time.Location() != time.UTC {
		t.Errorf("first price time = %s in %s, want %s in UTC", prices[0].Time, prices[0].Time.Location(), requestedDay)
	}

	const want = 0.2393609 // (89.68/1000 + 0.09161) * 1.21 + 0.02
	if math.Abs(prices[0].Value-want) > 1e-12 {
		t.Errorf("price = %.10f, want %.10f", prices[0].Value, want)
	}
}

func TestFetchDayAheadPricesUsesConfiguredCalendarAcrossMarketDays(t *testing.T) {
	helsinki := loadLocation(t, "Europe/Helsinki")
	tests := []struct {
		name             string
		loc              *time.Location
		day              time.Time
		wantMarketDates  []string
		firstMarketSlots int
	}{
		{
			name:             "Helsinki",
			loc:              helsinki,
			day:              time.Date(2026, 1, 15, 0, 0, 0, 0, helsinki),
			wantMarketDates:  []string{"2026-01-14", "2026-01-15"},
			firstMarketSlots: 4,
		},
		{
			name:             "UTC default",
			day:              time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
			wantMarketDates:  []string{"2026-01-15", "2026-01-16"},
			firstMarketSlots: 92,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marketLoc := loadLocation(t, "Europe/Oslo")
			var client *Client
			if tt.loc == nil {
				client = New("NL", "EUR", AllInPricing{})
			} else {
				client = NewWithLocation("NL", "EUR", tt.loc, AllInPricing{})
			}

			var requestedDates []string
			client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				date := req.URL.Query().Get("date")
				requestedDates = append(requestedDates, date)
				marketDay := parseMarketDate(t, date, marketLoc)
				value := 10.0
				if date == tt.wantMarketDates[1] {
					value = 20
				}
				return response(makeDayAheadResponse(t, marketDay, value)), nil
			})

			prices, err := client.FetchDayAheadPrices(context.Background(), tt.day)
			if err != nil {
				t.Fatalf("FetchDayAheadPrices() error = %v", err)
			}
			if len(prices) != 96 {
				t.Fatalf("price slots = %d, want 96", len(prices))
			}
			assertDateQueries(t, requestedDates, tt.wantMarketDates)
			if !prices[0].Time.Equal(tt.day) || prices[0].Time.Location() != tt.day.Location() {
				t.Errorf(
					"first price time = %s in %s, want %s in %s",
					prices[0].Time,
					prices[0].Time.Location(),
					tt.day,
					tt.day.Location(),
				)
			}
			if got := prices[tt.firstMarketSlots-1].Value; got != 0.01 {
				t.Errorf("last first-market price = %v, want 0.01", got)
			}
			if got := prices[tt.firstMarketSlots].Value; got != 0.02 {
				t.Errorf("first second-market price = %v, want 0.02", got)
			}
			wantLast := tt.day.AddDate(0, 0, 1).Add(-15 * time.Minute)
			if !prices[len(prices)-1].Time.Equal(wantLast) {
				t.Errorf("last price time = %s, want %s", prices[len(prices)-1].Time, wantLast)
			}
		})
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
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-29T22:00:00Z","deliveryEnd":"2026-08-29T22:15:00Z","entryPerArea":{"NL":null}}]}`,
			want: "no price for area",
		},
		{
			name: "wrong day",
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-31T12:00:00Z","deliveryEnd":"2026-08-31T12:15:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "outside requested day",
		},
		{
			name: "partial day",
			body: `{"deliveryDateCET":"2026-08-30","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":15,"multiIndexEntries":[{"deliveryStart":"2026-08-29T22:00:00Z","deliveryEnd":"2026-08-29T22:15:00Z","entryPerArea":{"NL":89.68}}]}`,
			want: "incomplete price series",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewWithLocation("NL", "EUR", time.UTC, AllInPricing{})
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(tt.body), nil
			})

			_, err := client.FetchDayAheadPrices(
				context.Background(),
				time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("FetchDayAheadPrices() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFetchDayAheadPricesUsesCETCESTMarketDayDSTBoundaries(t *testing.T) {
	loc := loadLocation(t, "Europe/Amsterdam")
	marketLoc := loadLocation(t, "Europe/Oslo")
	tests := []struct {
		name      string
		day       time.Time
		wantSlots int
	}{
		{
			name:      "spring forward",
			day:       time.Date(2026, 3, 29, 0, 0, 0, 0, loc),
			wantSlots: 92,
		},
		{
			name:      "fall back",
			day:       time.Date(2026, 10, 25, 0, 0, 0, 0, loc),
			wantSlots: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestedDates []string
			client := NewWithLocation("NL", "EUR", loc, AllInPricing{})
			client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				date := req.URL.Query().Get("date")
				requestedDates = append(requestedDates, date)
				return response(makeDayAheadResponse(t, parseMarketDate(t, date, marketLoc), 50)), nil
			})

			prices, err := client.FetchDayAheadPrices(context.Background(), tt.day)
			if err != nil {
				t.Fatalf("FetchDayAheadPrices() error = %v", err)
			}
			if len(prices) != tt.wantSlots {
				t.Fatalf("price slots = %d, want %d", len(prices), tt.wantSlots)
			}
			assertDateQueries(t, requestedDates, []string{tt.day.Format("2006-01-02")})
			if !prices[0].Time.Equal(tt.day) {
				t.Errorf("first price time = %s, want %s", prices[0].Time, tt.day)
			}
			wantLast := tt.day.AddDate(0, 0, 1).Add(-15 * time.Minute)
			if !prices[len(prices)-1].Time.Equal(wantLast) {
				t.Errorf("last price time = %s, want %s", prices[len(prices)-1].Time, wantLast)
			}
		})
	}
}

func TestFetchDayAheadPricesReturnsPublishedPrefixBeforeUnpublishedMarketDay(t *testing.T) {
	marketLoc := loadLocation(t, "Europe/Oslo")
	client := NewWithLocation("NL", "EUR", time.UTC, AllInPricing{})
	var requestedDates []string
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		date := req.URL.Query().Get("date")
		requestedDates = append(requestedDates, date)
		marketDay := parseMarketDate(t, date, marketLoc)
		if date == "2026-01-16" {
			return response(makeEmptyDayAheadResponse(t, marketDay)), nil
		}
		return response(makeDayAheadResponse(t, marketDay, 50)), nil
	})

	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	prices, err := client.FetchDayAheadPrices(context.Background(), day)
	if err != nil {
		t.Fatalf("FetchDayAheadPrices() error = %v", err)
	}
	assertDateQueries(t, requestedDates, []string{"2026-01-15", "2026-01-16"})
	if len(prices) != 92 {
		t.Fatalf("price slots = %d, want 92", len(prices))
	}
	if !prices[0].Time.Equal(day) {
		t.Errorf("first price time = %s, want %s", prices[0].Time, day)
	}
	wantLast := time.Date(2026, 1, 15, 22, 45, 0, 0, time.UTC)
	if !prices[len(prices)-1].Time.Equal(wantLast) {
		t.Errorf("last price time = %s, want %s", prices[len(prices)-1].Time, wantLast)
	}
}

func TestFetchDayAheadPricesRejectsUnpublishedInitialMarketDay(t *testing.T) {
	marketLoc := loadLocation(t, "Europe/Oslo")
	client := NewWithLocation("NL", "EUR", time.UTC, AllInPricing{})
	var requestedDates []string
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		date := req.URL.Query().Get("date")
		requestedDates = append(requestedDates, date)
		return response(makeEmptyDayAheadResponse(t, parseMarketDate(t, date, marketLoc))), nil
	})

	prices, err := client.FetchDayAheadPrices(
		context.Background(),
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), "prices unavailable") {
		t.Fatalf("FetchDayAheadPrices() error = %v, want unavailable initial market day error", err)
	}
	if prices != nil {
		t.Errorf("FetchDayAheadPrices() prices = %v, want nil for unpublished initial market day", prices)
	}
	assertDateQueries(t, requestedDates, []string{"2026-01-15"})
}

func TestFetchDayAheadPricesRejectsMalformedAdjacentMarketDay(t *testing.T) {
	marketLoc := loadLocation(t, "Europe/Oslo")
	client := NewWithLocation("NL", "EUR", time.UTC, AllInPricing{})
	var requestedDates []string
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		date := req.URL.Query().Get("date")
		requestedDates = append(requestedDates, date)
		if date == "2026-01-16" {
			return response(
				`{"deliveryDateCET":"2026-01-16","market":"DayAhead","indexNames":["NL"],"currency":"EUR","resolutionInMinutes":60,"multiIndexEntries":[]}`,
			), nil
		}
		return response(makeDayAheadResponse(t, parseMarketDate(t, date, marketLoc), 50)), nil
	})

	prices, err := client.FetchDayAheadPrices(
		context.Background(),
		time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
	)
	if err == nil || !strings.Contains(err.Error(), "unexpected resolution") {
		t.Fatalf("FetchDayAheadPrices() error = %v, want malformed adjacent market day error", err)
	}
	if prices != nil {
		t.Errorf("FetchDayAheadPrices() prices = %v, want nil after malformed adjacent market day", prices)
	}
	assertDateQueries(t, requestedDates, []string{"2026-01-15", "2026-01-16"})
}

func loadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load timezone %q: %v", name, err)
	}
	return loc
}

func parseMarketDate(t *testing.T, date string, loc *time.Location) time.Time {
	t.Helper()
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		t.Fatalf("parse market date %q: %v", date, err)
	}
	return day
}

func assertDateQueries(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("market date queries = %v, want %v", got, want)
	}
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func makeEmptyDayAheadResponse(t *testing.T, day time.Time) string {
	t.Helper()
	body, err := json.Marshal(apiResponse{
		DeliveryDateCET: day.Format("2006-01-02"),
		Market:          "DayAhead",
		IndexNames:      []string{"NL"},
		Currency:        "EUR",
		ResolutionInMin: 15,
	})
	if err != nil {
		t.Fatalf("marshal empty day-ahead response: %v", err)
	}
	return string(body)
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

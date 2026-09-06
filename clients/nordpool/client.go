package nordpool

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/shopspring/decimal"
)

const (
	baseURL    = "https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices"
	resolution = "15" // 15-minute resolution
)

// Price represents a single price point.
type Price struct {
	Time  time.Time
	Value float64 // EUR/kWh
}

// AllInPricing converts wholesale prices to the amount paid or received per kWh.
type AllInPricing struct {
	EnergyTaxEURPerKWh   decimal.Decimal
	VATRate              decimal.Decimal
	SupplierFeeEURPerKWh decimal.Decimal
}

// Apply returns (wholesale price + energy tax) including VAT, plus the supplier fee.
func (p AllInPricing) Apply(wholesaleEURPerKWh decimal.Decimal) decimal.Decimal {
	return wholesaleEURPerKWh.
		Add(p.EnergyTaxEURPerKWh).
		Mul(decimal.NewFromInt(1).Add(p.VATRate)).
		Add(p.SupplierFeeEURPerKWh)
}

// Client is a NordPool API client.
type Client struct {
	httpClient *http.Client
	area       string
	currency   string
	loc        *time.Location
	pricing    AllInPricing
}

// New creates a new NordPool client.
func New(area, currency string, pricing AllInPricing) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		area:     area,
		currency: currency,
		pricing:  pricing,
	}
}

// NewWithLocation creates a new NordPool client with a specific timezone.
func NewWithLocation(area, currency string, loc *time.Location, pricing AllInPricing) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		area:     area,
		currency: currency,
		loc:      loc,
		pricing:  pricing,
	}
}

// apiResponse represents the NordPool API response structure.
type apiResponse struct {
	DeliveryDateCET   string            `json:"deliveryDateCET"`
	Version           int               `json:"version"`
	ExchangeTimeCET   string            `json:"exchangeTimeCET"`
	Market            string            `json:"market"`
	IndexNames        []string          `json:"indexNames"`
	Currency          string            `json:"currency"`
	ResolutionInMin   int               `json:"resolutionInMinutes"`
	MultiIndexEntries []multiIndexEntry `json:"multiIndexEntries"`
}

type multiIndexEntry struct {
	DeliveryStart string              `json:"deliveryStart"`
	DeliveryEnd   string              `json:"deliveryEnd"`
	EntryPerArea  map[string]*float64 `json:"entryPerArea"`
}

// FetchDayAheadPrices fetches day-ahead prices for the given date.
// Returns prices in EUR/kWh (converted from EUR/MWh).
func (c *Client) FetchDayAheadPrices(ctx context.Context, date time.Time) ([]Price, error) {
	loc := c.loc
	if loc == nil {
		loc = date.Location()
	}
	requestedDay := date.In(loc)
	dateStr := requestedDay.Format("2006-01-02")

	params := url.Values{}
	params.Set("date", dateStr)
	params.Set("indexNames", c.area)
	params.Set("currency", c.currency)
	params.Set("market", "DayAhead")
	params.Set("resolutionInMinutes", resolution)

	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch prices: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if apiResp.Currency != c.currency {
		return nil, fmt.Errorf("unexpected currency %q, want %q", apiResp.Currency, c.currency)
	}
	if apiResp.ResolutionInMin != 15 {
		return nil, fmt.Errorf("unexpected resolution %d minutes, want 15", apiResp.ResolutionInMin)
	}
	if apiResp.DeliveryDateCET != dateStr {
		return nil, fmt.Errorf("unexpected delivery date %q, want %q", apiResp.DeliveryDateCET, dateStr)
	}
	if apiResp.Market != "DayAhead" {
		return nil, fmt.Errorf("unexpected market %q, want %q", apiResp.Market, "DayAhead")
	}
	areaFound := false
	for _, area := range apiResp.IndexNames {
		if area == c.area {
			areaFound = true
			break
		}
	}
	if !areaFound {
		return nil, fmt.Errorf("response index names do not include area %q", c.area)
	}
	if len(apiResp.MultiIndexEntries) == 0 {
		return nil, fmt.Errorf("no price entries returned for %s", dateStr)
	}

	prices := make([]Price, 0, len(apiResp.MultiIndexEntries))
	seen := make(map[time.Time]struct{}, len(apiResp.MultiIndexEntries))
	dayStart := time.Date(requestedDay.Year(), requestedDay.Month(), requestedDay.Day(), 0, 0, 0, 0, requestedDay.Location())
	expectedStart := dayStart
	for _, entry := range apiResp.MultiIndexEntries {
		// Parse delivery start time
		t, err := time.Parse(time.RFC3339, entry.DeliveryStart)
		if err != nil {
			return nil, fmt.Errorf("parse time %q: %w", entry.DeliveryStart, err)
		}
		t = t.In(loc)
		if t.Year() != requestedDay.Year() || t.Month() != requestedDay.Month() || t.Day() != requestedDay.Day() {
			return nil, fmt.Errorf("price slot %s is outside requested day %s", entry.DeliveryStart, dateStr)
		}
		end, err := time.Parse(time.RFC3339, entry.DeliveryEnd)
		if err != nil {
			return nil, fmt.Errorf("parse delivery end %q: %w", entry.DeliveryEnd, err)
		}
		if end.Sub(t) != 15*time.Minute {
			return nil, fmt.Errorf("unexpected slot duration at %s: %s", entry.DeliveryStart, end.Sub(t))
		}
		if _, duplicate := seen[t]; duplicate {
			return nil, fmt.Errorf("duplicate price slot at %s", entry.DeliveryStart)
		}
		seen[t] = struct{}{}

		// Get price for our area (in EUR/MWh)
		pricePerMWh, ok := entry.EntryPerArea[c.area]
		if !ok || pricePerMWh == nil {
			return nil, fmt.Errorf("no price for area %q at %s", c.area, entry.DeliveryStart)
		}
		if math.IsNaN(*pricePerMWh) || math.IsInf(*pricePerMWh, 0) {
			return nil, fmt.Errorf("non-finite price for area %q at %s", c.area, entry.DeliveryStart)
		}
		if !t.Equal(expectedStart) {
			return nil, fmt.Errorf("incomplete or unordered price series: got slot %s, want %s", t, expectedStart)
		}
		expectedStart = end

		// Convert from EUR/MWh to EUR/kWh, then add the configured taxes and fee.
		wholesalePrice := decimal.NewFromFloat(*pricePerMWh).Div(decimal.NewFromInt(1000))
		allInPrice, _ := c.pricing.Apply(wholesalePrice).Float64()
		prices = append(prices, Price{
			Time:  t,
			Value: allInPrice,
		})
	}
	dayEnd := dayStart.AddDate(0, 0, 1)
	if !expectedStart.Equal(dayEnd) {
		return nil, fmt.Errorf("incomplete price series: ends at %s, want %s", expectedStart, dayEnd)
	}

	return prices, nil
}

// FetchTodayPrices fetches day-ahead prices for today.
// Uses the provided location to determine "today" (defaults to UTC if nil).
func (c *Client) FetchTodayPrices(ctx context.Context) ([]Price, error) {
	return c.FetchDayAheadPrices(ctx, c.now())
}

// FetchTomorrowPrices fetches day-ahead prices for tomorrow.
// Uses the provided location to determine "tomorrow" (defaults to UTC if nil).
func (c *Client) FetchTomorrowPrices(ctx context.Context) ([]Price, error) {
	return c.FetchDayAheadPrices(ctx, c.now().AddDate(0, 0, 1))
}

// now returns current time in the client's configured location.
func (c *Client) now() time.Time {
	if c.loc != nil {
		return time.Now().In(c.loc)
	}
	return time.Now()
}

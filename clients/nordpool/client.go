package nordpool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
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

// Client is a NordPool API client.
type Client struct {
	httpClient *http.Client
	area       string
	currency   string
	loc        *time.Location
}

// New creates a new NordPool client.
func New(area, currency string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		area:     area,
		currency: currency,
	}
}

// NewWithLocation creates a new NordPool client with a specific timezone.
func NewWithLocation(area, currency string, loc *time.Location) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		area:     area,
		currency: currency,
		loc:      loc,
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
	DeliveryStart string             `json:"deliveryStart"`
	DeliveryEnd   string             `json:"deliveryEnd"`
	EntryPerArea  map[string]float64 `json:"entryPerArea"`
}

// FetchDayAheadPrices fetches day-ahead prices for the given date.
// Returns prices in EUR/kWh (converted from EUR/MWh).
func (c *Client) FetchDayAheadPrices(ctx context.Context, date time.Time) ([]Price, error) {
	dateStr := date.Format("2006-01-02")

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

	prices := make([]Price, 0, len(apiResp.MultiIndexEntries))
	for _, entry := range apiResp.MultiIndexEntries {
		// Parse delivery start time
		t, err := time.Parse(time.RFC3339, entry.DeliveryStart)
		if err != nil {
			return nil, fmt.Errorf("parse time %q: %w", entry.DeliveryStart, err)
		}

		// Get price for our area (in EUR/MWh)
		pricePerMWh, ok := entry.EntryPerArea[c.area]
		if !ok {
			return nil, fmt.Errorf("no price for area %q at %s", c.area, entry.DeliveryStart)
		}

		// Convert from EUR/MWh to EUR/kWh
		pricePerKWh := pricePerMWh / 1000.0

		prices = append(prices, Price{
			Time:  t,
			Value: pricePerKWh,
		})
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

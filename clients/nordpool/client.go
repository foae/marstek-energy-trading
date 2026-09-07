package nordpool

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrPricesUnavailable means the requested local-day prefix is not published yet.
var ErrPricesUnavailable = errors.New("prices unavailable")

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

// FetchDayAheadPrices returns the configured local day's prices in EUR/kWh.
// When an adjacent market day is unpublished, it returns only the contiguous
// known prefix; malformed or failed market-day requests remain errors.
func (c *Client) FetchDayAheadPrices(ctx context.Context, date time.Time) ([]Price, error) {
	loc := c.loc
	if loc == nil {
		loc = date.Location()
	}
	requestedDate := date.In(loc)
	requestedStart := time.Date(
		requestedDate.Year(), requestedDate.Month(), requestedDate.Day(), 0, 0, 0, 0, loc,
	)
	requestedEnd := requestedStart.AddDate(0, 0, 1)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	marketLoc, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		return nil, fmt.Errorf("load Nord Pool market timezone: %w", err)
	}
	marketDate := requestedStart.In(marketLoc)
	marketStart := time.Date(marketDate.Year(), marketDate.Month(), marketDate.Day(), 0, 0, 0, 0, marketLoc)

	prices := make([]Price, 0, int(requestedEnd.Sub(requestedStart)/(15*time.Minute)))
	expectedStart := requestedStart
	for marketStart.Before(requestedEnd) {
		marketEnd := marketStart.AddDate(0, 0, 1)

		marketPrices, published, err := c.fetchMarketDay(ctx, marketStart, marketEnd)
		if err != nil {
			return nil, err
		}
		if !published {
			if len(prices) == 0 {
				return nil, fmt.Errorf(
					"%w for requested day %s: market day %s is unpublished",
					ErrPricesUnavailable,
					requestedStart.Format("2006-01-02"),
					marketStart.Format("2006-01-02"),
				)
			}
			return prices, nil
		}

		for _, price := range marketPrices {
			if price.Time.Before(requestedStart) {
				continue
			}
			if !price.Time.Before(requestedEnd) {
				break
			}
			if !price.Time.Equal(expectedStart) {
				return nil, fmt.Errorf(
					"incomplete or unordered price series: got slot %s, want %s",
					price.Time,
					expectedStart,
				)
			}

			prices = append(prices, Price{
				Time:  price.Time.In(loc),
				Value: price.Value,
			})
			expectedStart = expectedStart.Add(15 * time.Minute)
		}

		marketStart = marketEnd
	}
	if !expectedStart.Equal(requestedEnd) {
		return nil, fmt.Errorf(
			"incomplete price series: ends at %s, want %s",
			expectedStart,
			requestedEnd,
		)
	}

	return prices, nil
}

func (c *Client) fetchMarketDay(
	ctx context.Context,
	marketStart time.Time,
	marketEnd time.Time,
) ([]Price, bool, error) {
	dateStr := marketStart.Format("2006-01-02")
	params := url.Values{}
	params.Set("date", dateStr)
	params.Set("indexNames", c.area)
	params.Set("currency", c.currency)
	params.Set("market", "DayAhead")
	params.Set("resolutionInMinutes", resolution)

	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch prices: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, false, fmt.Errorf("decode response: %w", err)
	}
	if apiResp.Currency != c.currency {
		return nil, false, fmt.Errorf("unexpected currency %q, want %q", apiResp.Currency, c.currency)
	}
	if apiResp.ResolutionInMin != 15 {
		return nil, false, fmt.Errorf("unexpected resolution %d minutes, want 15", apiResp.ResolutionInMin)
	}
	if apiResp.DeliveryDateCET != dateStr {
		return nil, false, fmt.Errorf("unexpected delivery date %q, want %q", apiResp.DeliveryDateCET, dateStr)
	}
	if apiResp.Market != "DayAhead" {
		return nil, false, fmt.Errorf("unexpected market %q, want %q", apiResp.Market, "DayAhead")
	}
	areaFound := false
	for _, area := range apiResp.IndexNames {
		if area == c.area {
			areaFound = true
			break
		}
	}
	if !areaFound {
		return nil, false, fmt.Errorf("response index names do not include area %q", c.area)
	}
	if len(apiResp.MultiIndexEntries) == 0 {
		return nil, false, nil
	}

	prices := make([]Price, 0, len(apiResp.MultiIndexEntries))
	seen := make(map[time.Time]struct{}, len(apiResp.MultiIndexEntries))
	expectedStart := marketStart
	for _, entry := range apiResp.MultiIndexEntries {
		start, err := time.Parse(time.RFC3339, entry.DeliveryStart)
		if err != nil {
			return nil, false, fmt.Errorf("parse time %q: %w", entry.DeliveryStart, err)
		}
		start = start.In(marketStart.Location())
		if start.Before(marketStart) || !start.Before(marketEnd) {
			return nil, false, fmt.Errorf("price slot %s is outside requested day %s", entry.DeliveryStart, dateStr)
		}
		end, err := time.Parse(time.RFC3339, entry.DeliveryEnd)
		if err != nil {
			return nil, false, fmt.Errorf("parse delivery end %q: %w", entry.DeliveryEnd, err)
		}
		if end.Sub(start) != 15*time.Minute {
			return nil, false, fmt.Errorf("unexpected slot duration at %s: %s", entry.DeliveryStart, end.Sub(start))
		}
		if _, duplicate := seen[start]; duplicate {
			return nil, false, fmt.Errorf("duplicate price slot at %s", entry.DeliveryStart)
		}
		seen[start] = struct{}{}

		pricePerMWh, ok := entry.EntryPerArea[c.area]
		if !ok || pricePerMWh == nil {
			return nil, false, fmt.Errorf("no price for area %q at %s", c.area, entry.DeliveryStart)
		}
		if math.IsNaN(*pricePerMWh) || math.IsInf(*pricePerMWh, 0) {
			return nil, false, fmt.Errorf("non-finite price for area %q at %s", c.area, entry.DeliveryStart)
		}
		if !start.Equal(expectedStart) {
			return nil, false, fmt.Errorf(
				"incomplete or unordered price series: got slot %s, want %s",
				start,
				expectedStart,
			)
		}
		expectedStart = end.In(marketStart.Location())

		// Convert from EUR/MWh to EUR/kWh, then add the configured taxes and fee.
		wholesalePrice := decimal.NewFromFloat(*pricePerMWh).Div(decimal.NewFromInt(1000))
		allInPrice, _ := c.pricing.Apply(wholesalePrice).Float64()
		prices = append(prices, Price{
			Time:  start,
			Value: allInPrice,
		})
	}
	if !expectedStart.Equal(marketEnd) {
		return nil, false, fmt.Errorf(
			"incomplete price series: ends at %s, want %s",
			expectedStart,
			marketEnd,
		)
	}

	return prices, true, nil
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

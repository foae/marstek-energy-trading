package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// TradeAction represents the type of trade action.
type TradeAction string

const (
	ActionCharge    TradeAction = "charge"
	ActionDischarge TradeAction = "discharge"
)

// Trade represents a single trade record.
type Trade struct {
	Timestamp time.Time       `json:"timestamp"`
	Action    TradeAction     `json:"action"`
	PriceEUR  decimal.Decimal `json:"price_eur"`  // EUR/kWh
	PowerW    int             `json:"power_w"`    // Watts
	DurationS int             `json:"duration_s"` // Seconds
	EnergyKWh decimal.Decimal `json:"energy_kwh"` // kWh traded
	StartSOC  int             `json:"start_soc"`  // SOC at start
	EndSOC    int             `json:"end_soc"`    // SOC at end
}

// DailySummary contains the daily trading summary.
type DailySummary struct {
	Date              string          `json:"date"`
	ChargedKWh        decimal.Decimal `json:"charged_kwh"`
	DischargedKWh     decimal.Decimal `json:"discharged_kwh"`
	ChargeCycles      int             `json:"charge_cycles"`
	DischargeCycles   int             `json:"discharge_cycles"`
	PnLEUR            decimal.Decimal `json:"pnl_eur"`
	AvgChargePrice    decimal.Decimal `json:"avg_charge_price"`
	MinChargePrice    decimal.Decimal `json:"min_charge_price"`
	AvgDischargePrice decimal.Decimal `json:"avg_discharge_price"`
	MaxDischargePrice decimal.Decimal `json:"max_discharge_price"`
	Trades            []Trade         `json:"trades"`
}

// History contains the full trading history.
type History struct {
	Days       []DailySummary  `json:"days"`
	TotalPnL   decimal.Decimal `json:"total_pnl_eur"`
	TotalDays  int             `json:"total_days"`
	FirstTrade *time.Time      `json:"first_trade,omitempty"`
	LastTrade  *time.Time      `json:"last_trade,omitempty"`
}

// Recorder records trades and calculates P&L.
type Recorder struct {
	mu         sync.Mutex
	dataDir    string
	efficiency decimal.Decimal
	trades     []Trade
	loc        *time.Location
}

// NewRecorder creates a new trade recorder.
func NewRecorder(dataDir string, efficiency float64, loc *time.Location) *Recorder {
	if loc == nil {
		loc = time.UTC
	}
	return &Recorder{
		dataDir:    dataDir,
		efficiency: decimal.NewFromFloat(efficiency),
		trades:     make([]Trade, 0),
		loc:        loc,
	}
}

// RecordTrade records a completed trade.
func (r *Recorder) RecordTrade(trade Trade) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.trades = append(r.trades, trade)
	return r.saveTrades()
}

// GetHistory returns the full trading history grouped by day.
func (r *Recorder) GetHistory() History {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.trades) == 0 {
		return History{Days: []DailySummary{}}
	}

	// Group trades by day
	dayTrades := make(map[string][]Trade)
	var firstTrade, lastTrade time.Time

	for _, t := range r.trades {
		dayKey := t.Timestamp.Format("2006-01-02")
		dayTrades[dayKey] = append(dayTrades[dayKey], t)

		if firstTrade.IsZero() || t.Timestamp.Before(firstTrade) {
			firstTrade = t.Timestamp
		}
		if lastTrade.IsZero() || t.Timestamp.After(lastTrade) {
			lastTrade = t.Timestamp
		}
	}

	// Build summaries
	var days []DailySummary
	totalPnL := decimal.Zero

	for dayKey, trades := range dayTrades {
		chargedKWh := decimal.Zero
		dischargedKWh := decimal.Zero
		chargeCost := decimal.Zero
		dischargeRevenue := decimal.Zero
		var chargeCycles, dischargeCycles int

		// Track min/max prices
		minChargePrice := decimal.Zero
		maxDischargePrice := decimal.Zero

		for _, t := range trades {
			switch t.Action {
			case ActionCharge:
				chargedKWh = chargedKWh.Add(t.EnergyKWh)
				chargeCost = chargeCost.Add(t.PriceEUR.Mul(t.EnergyKWh))
				if minChargePrice.IsZero() || t.PriceEUR.LessThan(minChargePrice) {
					minChargePrice = t.PriceEUR
				}
				chargeCycles++
			case ActionDischarge:
				dischargedKWh = dischargedKWh.Add(t.EnergyKWh)
				dischargeRevenue = dischargeRevenue.Add(t.PriceEUR.Mul(t.EnergyKWh))
				if t.PriceEUR.GreaterThan(maxDischargePrice) {
					maxDischargePrice = t.PriceEUR
				}
				dischargeCycles++
			}
		}

		// Calculate energy-weighted average prices: sum(price * energy) / sum(energy)
		avgChargePrice := decimal.Zero
		if !chargedKWh.IsZero() {
			avgChargePrice = chargeCost.Div(chargedKWh)
		}
		avgDischargePrice := decimal.Zero
		if !dischargedKWh.IsZero() {
			avgDischargePrice = dischargeRevenue.Div(dischargedKWh)
		}

		pnl := dischargeRevenue.Sub(chargeCost)
		totalPnL = totalPnL.Add(pnl)

		days = append(days, DailySummary{
			Date:              dayKey,
			ChargedKWh:        chargedKWh,
			DischargedKWh:     dischargedKWh,
			ChargeCycles:      chargeCycles,
			DischargeCycles:   dischargeCycles,
			PnLEUR:            pnl,
			AvgChargePrice:    avgChargePrice,
			MinChargePrice:    minChargePrice,
			AvgDischargePrice: avgDischargePrice,
			MaxDischargePrice: maxDischargePrice,
			Trades:            trades,
		})
	}

	// Sort by date descending
	for i := 0; i < len(days)-1; i++ {
		for j := i + 1; j < len(days); j++ {
			if days[j].Date > days[i].Date {
				days[i], days[j] = days[j], days[i]
			}
		}
	}

	return History{
		Days:       days,
		TotalPnL:   totalPnL,
		TotalDays:  len(days),
		FirstTrade: &firstTrade,
		LastTrade:  &lastTrade,
	}
}

// GetTodaySummary returns today's trading summary using the configured timezone.
func (r *Recorder) GetTodaySummary() DailySummary {
	history := r.GetHistory()
	today := time.Now().In(r.loc).Format("2006-01-02")

	for _, day := range history.Days {
		if day.Date == today {
			return day
		}
	}

	return DailySummary{Date: today, Trades: []Trade{}}
}

// GetTotalPnL returns the total P&L across all recorded trades.
func (r *Recorder) GetTotalPnL() decimal.Decimal {
	r.mu.Lock()
	defer r.mu.Unlock()

	totalRevenue := decimal.Zero
	totalCost := decimal.Zero

	for _, t := range r.trades {
		switch t.Action {
		case ActionCharge:
			totalCost = totalCost.Add(t.PriceEUR.Mul(t.EnergyKWh))
		case ActionDischarge:
			totalRevenue = totalRevenue.Add(t.PriceEUR.Mul(t.EnergyKWh))
		}
	}

	return totalRevenue.Sub(totalCost)
}

// GetLastChargeTrade returns the most recent charge trade, or nil if none.
func (r *Recorder) GetLastChargeTrade() *Trade {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := len(r.trades) - 1; i >= 0; i-- {
		if r.trades[i].Action == ActionCharge {
			t := r.trades[i]
			return &t
		}
	}
	return nil
}

// saveTrades persists trades to a JSON file atomically.
func (r *Recorder) saveTrades() error {
	if r.dataDir == "" {
		return nil // No persistence configured
	}

	if err := os.MkdirAll(r.dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	path := filepath.Join(r.dataDir, "trades.json")
	tmpPath := path + ".tmp"

	data, err := json.MarshalIndent(r.trades, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trades: %w", err)
	}

	// Write to temp file first
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	// Atomic rename (on POSIX systems)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("rename trades file: %w", err)
	}

	return nil
}

// LoadTrades loads trades from the JSON file.
func (r *Recorder) LoadTrades() error {
	if r.dataDir == "" {
		return nil // No persistence configured
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	path := filepath.Join(r.dataDir, "trades.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read trades file: %w", err)
	}

	if err := json.Unmarshal(data, &r.trades); err != nil {
		return fmt.Errorf("unmarshal trades: %w", err)
	}

	return nil
}

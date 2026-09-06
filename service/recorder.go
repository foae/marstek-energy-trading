package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// TradeAction represents the type of trade action.
type TradeAction string

const (
	ActionCharge      TradeAction = "charge"
	ActionDischarge   TradeAction = "discharge"
	ActionSolarCharge TradeAction = "solar_charge"
)

// Trade represents a single trade record.
type Trade struct {
	Timestamp          time.Time       `json:"timestamp"`
	Action             TradeAction     `json:"action"`
	PriceEUR           decimal.Decimal `json:"price_eur"`            // EUR/kWh
	PowerW             int             `json:"power_w"`              // Watts
	DurationS          int             `json:"duration_s"`           // Seconds
	EnergyKWh          decimal.Decimal `json:"energy_kwh"`           // kWh battery input/output
	GridEnergyKWh      decimal.Decimal `json:"grid_energy_kwh"`      // Grid portion of a solar charge
	GridCostEUR        decimal.Decimal `json:"grid_cost_eur"`        // Interval-priced grid cost of a solar charge
	GridUnpricedKWh    decimal.Decimal `json:"grid_unpriced_kwh"`    // Grid portion without an available price
	StartSOC           int             `json:"start_soc"`            // SOC at start
	EndSOC             int             `json:"end_soc"`              // SOC at end
	UnpricedKWh        decimal.Decimal `json:"unpriced_kwh"`         // Battery energy without an interval price
	OpportunityCostEUR decimal.Decimal `json:"opportunity_cost_eur"` // Estimated value of solar energy not exported
	EnergyBasis        string          `json:"energy_basis"`         // Method used to estimate EnergyKWh; blank for historic trades
}

// DailySummary contains the daily trading summary.
type DailySummary struct {
	Date                    string          `json:"date"`
	ChargedKWh              decimal.Decimal `json:"charged_kwh"`
	DischargedKWh           decimal.Decimal `json:"discharged_kwh"`
	ChargeCycles            int             `json:"charge_cycles"`
	DischargeCycles         int             `json:"discharge_cycles"`
	SolarChargedKWh         decimal.Decimal `json:"solar_charged_kwh"`
	GridChargedKWh          decimal.Decimal `json:"grid_charged_kwh"`
	UnpricedGridKWh         decimal.Decimal `json:"unpriced_grid_kwh"`
	UnpricedKWh             decimal.Decimal `json:"unpriced_kwh"`
	SolarChargeCycles       int             `json:"solar_charge_cycles"`
	SolarOpportunityCostEUR decimal.Decimal `json:"solar_opportunity_cost_eur"`
	PnLEUR                  decimal.Decimal `json:"pnl_eur"` // Cash flow, not inventory-matched trading profit
	AvgChargePrice          decimal.Decimal `json:"avg_charge_price"`
	MinChargePrice          decimal.Decimal `json:"min_charge_price"`
	AvgDischargePrice       decimal.Decimal `json:"avg_discharge_price"`
	MaxDischargePrice       decimal.Decimal `json:"max_discharge_price"`
	Trades                  []Trade         `json:"trades"`
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
	mu             sync.Mutex
	dataDir        string
	persistenceErr error
	efficiency     decimal.Decimal
	trades         []Trade
	loc            *time.Location
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

	if r.persistenceErr != nil {
		return fmt.Errorf("trade persistence blocked after failed load: %w", r.persistenceErr)
	}

	r.trades = append(r.trades, trade)
	return r.saveTrades()
}

// GetHistory returns the full trading history grouped by local calendar day.
func (r *Recorder) GetHistory() History {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.trades) == 0 {
		return History{Days: []DailySummary{}}
	}

	dayTrades := make(map[string][]tradeFragment)
	var firstTrade, lastTrade time.Time

	for _, t := range r.trades {
		for _, fragment := range splitTradeByLocalDay(t, r.loc) {
			dayKey := fragment.trade.Timestamp.In(r.loc).Format("2006-01-02")
			dayTrades[dayKey] = append(dayTrades[dayKey], fragment)
		}

		if firstTrade.IsZero() || t.Timestamp.Before(firstTrade) {
			firstTrade = t.Timestamp
		}
		if lastTrade.IsZero() || t.Timestamp.After(lastTrade) {
			lastTrade = t.Timestamp
		}
	}
	dayKeys := make([]string, 0, len(dayTrades))
	for dayKey := range dayTrades {
		dayKeys = append(dayKeys, dayKey)
	}
	sort.Strings(dayKeys)

	var days []DailySummary
	totalPnL := decimal.Zero

	for i := len(dayKeys) - 1; i >= 0; i-- {
		dayKey := dayKeys[i]
		fragments := dayTrades[dayKey]
		trades := make([]Trade, 0, len(fragments))
		chargedKWh := decimal.Zero
		dischargedKWh := decimal.Zero
		chargeCost := decimal.Zero
		dischargeRevenue := decimal.Zero
		solarChargedKWh := decimal.Zero
		gridChargedKWh := decimal.Zero
		unpricedGridKWh := decimal.Zero
		unpricedKWh := decimal.Zero
		solarOpportunityCostEUR := decimal.Zero
		pricedChargeKWh := decimal.Zero
		pricedDischargeKWh := decimal.Zero
		var chargeCycles, dischargeCycles, solarChargeCycles int

		// Track min/max prices with seen flags to handle negative prices correctly.
		var minChargePrice, maxDischargePrice decimal.Decimal
		var seenCharge, seenDischarge bool

		for _, fragment := range fragments {
			t := fragment.trade
			trades = append(trades, t)

			switch t.Action {
			case ActionCharge:
				pricedEnergyKWh := t.EnergyKWh.Sub(t.UnpricedKWh)
				chargedKWh = chargedKWh.Add(t.EnergyKWh)
				gridChargedKWh = gridChargedKWh.Add(t.EnergyKWh)
				unpricedKWh = unpricedKWh.Add(t.UnpricedKWh)
				chargeCost = chargeCost.Add(t.PriceEUR.Mul(pricedEnergyKWh))
				pricedChargeKWh = pricedChargeKWh.Add(pricedEnergyKWh)
				if pricedEnergyKWh.GreaterThan(decimal.Zero) {
					if !seenCharge || t.PriceEUR.LessThan(minChargePrice) {
						minChargePrice = t.PriceEUR
						seenCharge = true
					}
				}
				if fragment.startsTrade {
					chargeCycles++
				}
			case ActionSolarCharge:
				pricedGridKWh := t.GridEnergyKWh.Sub(t.GridUnpricedKWh)
				chargedKWh = chargedKWh.Add(t.EnergyKWh)
				solarChargedKWh = solarChargedKWh.Add(t.EnergyKWh.Sub(t.GridEnergyKWh))
				gridChargedKWh = gridChargedKWh.Add(t.GridEnergyKWh)
				unpricedGridKWh = unpricedGridKWh.Add(t.GridUnpricedKWh)
				unpricedKWh = unpricedKWh.Add(t.UnpricedKWh).Add(t.GridUnpricedKWh)
				solarOpportunityCostEUR = solarOpportunityCostEUR.Add(t.OpportunityCostEUR)
				chargeCost = chargeCost.Add(t.GridCostEUR)
				pricedChargeKWh = pricedChargeKWh.Add(pricedGridKWh)
				if pricedGridKWh.GreaterThan(decimal.Zero) {
					gridPrice := t.GridCostEUR.Div(pricedGridKWh)
					if !seenCharge || gridPrice.LessThan(minChargePrice) {
						minChargePrice = gridPrice
						seenCharge = true
					}
				}
				if fragment.startsTrade {
					solarChargeCycles++
				}
			case ActionDischarge:
				pricedEnergyKWh := t.EnergyKWh.Sub(t.UnpricedKWh)
				dischargedKWh = dischargedKWh.Add(t.EnergyKWh)
				unpricedKWh = unpricedKWh.Add(t.UnpricedKWh)
				dischargeRevenue = dischargeRevenue.Add(t.PriceEUR.Mul(pricedEnergyKWh))
				pricedDischargeKWh = pricedDischargeKWh.Add(pricedEnergyKWh)
				if pricedEnergyKWh.GreaterThan(decimal.Zero) {
					if !seenDischarge || t.PriceEUR.GreaterThan(maxDischargePrice) {
						maxDischargePrice = t.PriceEUR
						seenDischarge = true
					}
				}
				if fragment.startsTrade {
					dischargeCycles++
				}
			}
		}

		avgChargePrice := decimal.Zero
		if !pricedChargeKWh.IsZero() {
			avgChargePrice = chargeCost.Div(pricedChargeKWh)
		}
		avgDischargePrice := decimal.Zero
		if !pricedDischargeKWh.IsZero() {
			avgDischargePrice = dischargeRevenue.Div(pricedDischargeKWh)
		}

		pnl := dischargeRevenue.Sub(chargeCost)
		totalPnL = totalPnL.Add(pnl)

		days = append(days, DailySummary{
			Date:                    dayKey,
			ChargedKWh:              chargedKWh,
			DischargedKWh:           dischargedKWh,
			ChargeCycles:            chargeCycles,
			DischargeCycles:         dischargeCycles,
			SolarChargedKWh:         solarChargedKWh,
			GridChargedKWh:          gridChargedKWh,
			UnpricedGridKWh:         unpricedGridKWh,
			UnpricedKWh:             unpricedKWh,
			SolarChargeCycles:       solarChargeCycles,
			SolarOpportunityCostEUR: solarOpportunityCostEUR,
			PnLEUR:                  pnl,
			AvgChargePrice:          avgChargePrice,
			MinChargePrice:          minChargePrice,
			AvgDischargePrice:       avgDischargePrice,
			MaxDischargePrice:       maxDischargePrice,
			Trades:                  trades,
		})
	}

	return History{
		Days:       days,
		TotalPnL:   totalPnL,
		TotalDays:  len(days),
		FirstTrade: &firstTrade,
		LastTrade:  &lastTrade,
	}
}

type tradeFragment struct {
	trade       Trade
	startsTrade bool
}

// splitTradeByLocalDay proportionally estimates each local-day share of an aggregate trade.
func splitTradeByLocalDay(trade Trade, loc *time.Location) []tradeFragment {
	if trade.DurationS <= 0 {
		return []tradeFragment{{trade: trade, startsTrade: true}}
	}

	end := trade.Timestamp.Add(time.Duration(trade.DurationS) * time.Second)
	if !end.After(trade.Timestamp) {
		return []tradeFragment{{trade: trade, startsTrade: true}}
	}

	totalDurationS := decimal.NewFromInt(int64(trade.DurationS))
	cursor := trade.Timestamp
	allocatedDurationS := 0
	allocatedEnergyKWh := decimal.Zero
	allocatedGridEnergyKWh := decimal.Zero
	allocatedGridCostEUR := decimal.Zero
	allocatedGridUnpricedKWh := decimal.Zero
	allocatedUnpricedKWh := decimal.Zero
	allocatedOpportunityCostEUR := decimal.Zero
	fragments := make([]tradeFragment, 0, 2)

	for cursor.Before(end) {
		localCursor := cursor.In(loc)
		dayStart := time.Date(localCursor.Year(), localCursor.Month(), localCursor.Day(), 0, 0, 0, 0, loc)
		nextDayStart := dayStart.AddDate(0, 0, 1)
		fragmentEnd := end
		if nextDayStart.Before(end) {
			fragmentEnd = nextDayStart
		}

		fragmentDurationS := int(fragmentEnd.Sub(cursor).Seconds())
		lastFragment := fragmentEnd.Equal(end)
		if lastFragment {
			fragmentDurationS = trade.DurationS - allocatedDurationS
		}
		portion := decimal.NewFromInt(int64(fragmentDurationS)).Div(totalDurationS)
		fragment := trade
		fragment.Timestamp = cursor
		fragment.DurationS = fragmentDurationS
		if lastFragment {
			fragment.EnergyKWh = trade.EnergyKWh.Sub(allocatedEnergyKWh)
			fragment.GridEnergyKWh = trade.GridEnergyKWh.Sub(allocatedGridEnergyKWh)
			fragment.GridCostEUR = trade.GridCostEUR.Sub(allocatedGridCostEUR)
			fragment.GridUnpricedKWh = trade.GridUnpricedKWh.Sub(allocatedGridUnpricedKWh)
			fragment.UnpricedKWh = trade.UnpricedKWh.Sub(allocatedUnpricedKWh)
			fragment.OpportunityCostEUR = trade.OpportunityCostEUR.Sub(allocatedOpportunityCostEUR)
		} else {
			fragment.EnergyKWh = trade.EnergyKWh.Mul(portion)
			fragment.GridEnergyKWh = trade.GridEnergyKWh.Mul(portion)
			fragment.GridCostEUR = trade.GridCostEUR.Mul(portion)
			fragment.GridUnpricedKWh = trade.GridUnpricedKWh.Mul(portion)
			fragment.UnpricedKWh = trade.UnpricedKWh.Mul(portion)
			fragment.OpportunityCostEUR = trade.OpportunityCostEUR.Mul(portion)
			allocatedEnergyKWh = allocatedEnergyKWh.Add(fragment.EnergyKWh)
			allocatedGridEnergyKWh = allocatedGridEnergyKWh.Add(fragment.GridEnergyKWh)
			allocatedGridCostEUR = allocatedGridCostEUR.Add(fragment.GridCostEUR)
			allocatedGridUnpricedKWh = allocatedGridUnpricedKWh.Add(fragment.GridUnpricedKWh)
			allocatedUnpricedKWh = allocatedUnpricedKWh.Add(fragment.UnpricedKWh)
			allocatedOpportunityCostEUR = allocatedOpportunityCostEUR.Add(fragment.OpportunityCostEUR)
		}

		fragments = append(fragments, tradeFragment{
			trade:       fragment,
			startsTrade: cursor.Equal(trade.Timestamp),
		})
		allocatedDurationS += fragmentDurationS
		cursor = fragmentEnd
	}

	return fragments
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

// GetTotalPnL returns total cash flow, not inventory-matched trading profit.
func (r *Recorder) GetTotalPnL() decimal.Decimal {
	r.mu.Lock()
	defer r.mu.Unlock()

	totalRevenue := decimal.Zero
	totalCost := decimal.Zero

	for _, t := range r.trades {
		switch t.Action {
		case ActionCharge:
			totalCost = totalCost.Add(t.PriceEUR.Mul(t.EnergyKWh.Sub(t.UnpricedKWh)))
		case ActionSolarCharge:
			totalCost = totalCost.Add(t.GridCostEUR)
		case ActionDischarge:
			totalRevenue = totalRevenue.Add(t.PriceEUR.Mul(t.EnergyKWh.Sub(t.UnpricedKWh)))
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

	if err := os.MkdirAll(r.dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(r.dataDir, 0o700); err != nil {
		return fmt.Errorf("protect data dir: %w", err)
	}

	path := filepath.Join(r.dataDir, "trades.json")
	tmpPath := path + ".tmp"

	data, err := json.MarshalIndent(r.trades, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trades: %w", err)
	}

	// Persist file contents before publishing the new snapshot.
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("protect temp file: %w", err)
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("persist temp file: %w", err)
	}

	// Atomic rename (on POSIX systems)
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("rename trades file: %w", err)
	}

	dir, err := os.Open(r.dataDir)
	if err != nil {
		return fmt.Errorf("open data directory for sync: %w", err)
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// LoadTrades loads trades from the JSON file.
func (r *Recorder) LoadTrades() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.dataDir == "" {
		r.persistenceErr = nil
		return nil // No persistence configured
	}
	if err := os.MkdirAll(r.dataDir, 0o700); err != nil {
		loadErr := fmt.Errorf("create data dir: %w", err)
		r.persistenceErr = loadErr
		return loadErr
	}
	if err := os.Chmod(r.dataDir, 0o700); err != nil {
		loadErr := fmt.Errorf("protect data dir: %w", err)
		r.persistenceErr = loadErr
		return loadErr
	}

	path := filepath.Join(r.dataDir, "trades.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if saveErr := r.saveTrades(); saveErr != nil {
				r.persistenceErr = saveErr
				return saveErr
			}
			r.persistenceErr = nil
			return nil
		}
		loadErr := fmt.Errorf("read trades file: %w", err)
		r.persistenceErr = loadErr
		return loadErr
	}

	var trades []Trade
	if err := json.Unmarshal(data, &trades); err != nil {
		loadErr := fmt.Errorf("unmarshal trades: %w", err)
		r.persistenceErr = loadErr
		return loadErr
	}
	if err := os.Chmod(path, 0o600); err != nil {
		loadErr := fmt.Errorf("protect trades file: %w", err)
		r.persistenceErr = loadErr
		return loadErr
	}

	r.trades = trades
	r.persistenceErr = nil
	return nil
}

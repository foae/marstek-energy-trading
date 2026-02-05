package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/clients/telegram"
	"github.com/foae/marstek-energy-trading/internal/config"
)

// State represents the current trading state.
type State string

const (
	StateIdle        State = "idle"
	StateCharging    State = "charging"
	StateDischarging State = "discharging"
)

// Service is the main trading engine.
type Service struct {
	cfg      *config.Config
	nordpool PriceProvider
	battery  BatteryController
	telegram *telegram.Client
	recorder *Recorder
	loc      *time.Location   // timezone location
	nowFunc  func() time.Time // clock function for testing

	mu                 sync.RWMutex
	state              State
	currentPlan        *TradingPlan
	todayPrices        []nordpool.Price
	tomorrowPrices     []nordpool.Price
	lastPassiveRefresh time.Time
	currentTradeStart  time.Time
	currentTradePrice  decimal.Decimal
	currentTradeSOC    int
	lastChargePrice    decimal.Decimal // track last charge price for profitability check
	lastErrorNotify    time.Time       // rate limit error notifications
	lastMidnightSwap   time.Time       // track last midnight price swap to avoid repeated fetches
}

// New creates a new trading service.
func New(
	cfg *config.Config,
	nordpoolClient PriceProvider,
	batteryClient BatteryController,
	telegramClient *telegram.Client,
	recorder *Recorder,
) *Service {
	return &Service{
		cfg:      cfg,
		nordpool: nordpoolClient,
		battery:  batteryClient,
		telegram: telegramClient,
		recorder: recorder,
		state:    StateIdle,
		loc:      cfg.Location(),
		nowFunc:  time.Now,
	}
}

// now returns the current time using the configured clock and timezone.
func (s *Service) now() time.Time {
	if s.nowFunc == nil {
		return time.Now().In(s.loc)
	}
	return s.nowFunc().In(s.loc)
}

// SetClock sets the clock function for testing. Not thread-safe, call before Start().
func (s *Service) SetClock(fn func() time.Time) {
	s.nowFunc = fn
}

// telegramEnabled returns true if telegram notifications are configured.
func (s *Service) telegramEnabled() bool {
	return s.telegram != nil && s.telegram.Enabled()
}

// analyzerConfig returns the AnalyzerConfig derived from service config.
func (s *Service) analyzerConfig() AnalyzerConfig {
	return AnalyzerConfig{
		Efficiency:         s.cfg.BatteryEfficiency,
		MinPriceSpread:     s.cfg.MinPriceSpread,
		BatteryCapacityKWh: s.cfg.BatteryCapacityKWh,
		BatteryMinSOC:      s.cfg.BatteryMinSOC,
		ChargePowerW:       s.cfg.ChargePowerW,
		DischargePowerW:    s.cfg.DischargePowerW,
		MaxCyclesPerDay:    s.cfg.MaxCyclesPerDay,
	}
}

// calculateAveragePrice calculates the time-weighted average price over a time range.
// Prices are weighted by the actual overlap duration with each 15-minute slot.
func (s *Service) calculateAveragePrice(start, end time.Time) decimal.Decimal {
	if len(s.todayPrices) == 0 {
		return decimal.Zero
	}

	var weightedSum float64
	var totalDuration time.Duration

	for _, p := range s.todayPrices {
		slotEnd := p.Time.Add(15 * time.Minute)
		// Check if this slot overlaps with our time range
		// Slot overlaps if: slot starts before our end AND slot ends after our start
		if p.Time.Before(end) && slotEnd.After(start) {
			// Calculate actual overlap duration
			overlapStart := p.Time
			if start.After(p.Time) {
				overlapStart = start
			}
			overlapEnd := slotEnd
			if end.Before(slotEnd) {
				overlapEnd = end
			}
			overlap := overlapEnd.Sub(overlapStart)
			if overlap > 0 {
				weightedSum += p.Value * overlap.Seconds()
				totalDuration += overlap
			}
		}
	}

	if totalDuration == 0 {
		return decimal.Zero
	}

	return decimal.NewFromFloat(weightedSum / totalDuration.Seconds())
}

// Start begins the trading loop.
func (s *Service) Start(ctx context.Context) error {
	slog.Info("starting trading service")

	// Load existing trades
	if err := s.recorder.LoadTrades(); err != nil {
		slog.Warn("failed to load trades", "error", err)
	}

	// Restore last charge price for profitability checks after restart
	if lastCharge := s.recorder.GetLastChargeTrade(); lastCharge != nil {
		s.lastChargePrice = lastCharge.PriceEUR
		slog.Info("restored last charge price", "price", s.lastChargePrice)
	}

	// Connect to battery
	if err := s.battery.Connect(); err != nil {
		return err
	}

	// Discover battery
	device, err := s.battery.Discover()
	if err != nil {
		slog.Warn("battery discovery failed, will retry", "error", err)
	} else {
		slog.Info("battery discovered", "device", device.Device, "ip", device.IP)
	}

	// Fetch initial prices
	if err := s.fetchTodayPrices(ctx); err != nil {
		slog.Warn("failed to fetch today's prices", "error", err)
	}

	// Try to fetch tomorrow's prices (may not be available yet)
	if err := s.fetchTomorrowPrices(ctx); err != nil {
		slog.Debug("tomorrow's prices not available yet", "error", err)
	}

	// Send startup notification
	if s.telegramEnabled() {
		if err := s.telegram.SendStartup(ctx, s.cfg.ServiceName); err != nil {
			slog.Warn("failed to send startup notification", "error", err)
		}
	}

	// Start main loop
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Price fetch ticker (check every 15 minutes, fetch at 13:00)
	priceTicker := time.NewTicker(15 * time.Minute)
	defer priceTicker.Stop()

	// Daily summary ticker (at 23:59)
	dailyTicker := time.NewTicker(1 * time.Minute) // Check every minute for 23:59
	defer dailyTicker.Stop()

	// Telegram command polling (every 5 seconds)
	cmdTicker := time.NewTicker(5 * time.Second)
	defer cmdTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("stopping trading service")
			// Return to idle mode
			if err := s.battery.Idle(); err != nil {
				slog.Warn("failed to set idle mode on shutdown", "error", err)
			}
			return ctx.Err()

		case <-ticker.C:
			s.tick(ctx)

		case <-priceTicker.C:
			s.checkPriceFetch(ctx)

		case <-dailyTicker.C:
			s.checkDailySummary(ctx)

		case <-cmdTicker.C:
			s.handleTelegramCommands(ctx)
		}
	}
}

// tick is called every minute to evaluate trading decisions.
func (s *Service) tick(ctx context.Context) {
	now := s.now()

	// Get battery status OUTSIDE lock (network I/O)
	batStatus, err := s.battery.GetBatteryStatus()
	if err != nil {
		slog.Error("failed to get battery status", "error", err)
		s.notifyError(ctx, "Battery unreachable: "+err.Error())
		return
	}

	// Now lock for state access and updates
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create contextual logger for this tick
	l := slog.With(
		"state", s.state,
		"soc", batStatus.SOC,
		"time", now.Format("15:04"),
	)

	l.Debug("tick", "charging_enabled", batStatus.ChargingFlag, "discharging_enabled", batStatus.DischargFlag)

	// Check if we have a valid trading plan
	if s.currentPlan == nil || !s.currentPlan.ShouldTrade() {
		if s.state != StateIdle {
			l.Info("no profitable trading plan, transitioning to idle", "has_plan", s.currentPlan != nil)
			s.transitionToIdleLocked(ctx, batStatus.SOC)
		}
		return
	}

	// Get current price
	currentPriceF, ok := GetCurrentPrice(s.todayPrices, now)
	if !ok {
		l.Warn("no price for current time slot")
		return
	}
	currentPrice := decimal.NewFromFloat(currentPriceF)

	// Enrich logger with price context
	l = l.With("price_eur_kwh", currentPriceF)

	// Decide action based on current time window
	inChargeWindow := s.currentPlan.IsInChargeWindow(now)
	inDischargeWindow := s.currentPlan.IsInDischargeWindow(now)

	switch s.state {
	case StateIdle:
		if inChargeWindow {
			if batStatus.SOC >= 100 {
				l.Debug("in charge window but battery full")
			} else if !batStatus.ChargingFlag {
				l.Warn("in charge window but battery charging disabled")
			} else {
				l.Info("decision: start charging",
					"min_price", s.currentPlan.MinPrice,
					"charge_threshold", s.currentPlan.MinPrice+s.currentPlan.Spread*0.25)
				s.startChargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		} else if inDischargeWindow {
			minSOC := int(s.cfg.BatteryMinSOC * 100)
			if batStatus.SOC <= minSOC {
				l.Debug("in discharge window but battery at min SOC", "min_soc", minSOC)
			} else if !batStatus.DischargFlag {
				l.Warn("in discharge window but battery discharging disabled")
			} else if !s.isDischargeProfitiable(currentPrice) {
				efficiency := decimal.NewFromFloat(s.cfg.BatteryEfficiency)
				breakEven := s.lastChargePrice.Div(efficiency)
				breakEvenF, _ := breakEven.Float64()
				lastChargeF, _ := s.lastChargePrice.Float64()
				l.Info("decision: skip discharge - not profitable",
					"last_charge_price", lastChargeF,
					"break_even_price", breakEvenF)
			} else {
				lastChargeF, _ := s.lastChargePrice.Float64()
				l.Info("decision: start discharging",
					"last_charge_price", lastChargeF,
					"max_price", s.currentPlan.MaxPrice,
					"discharge_threshold", s.currentPlan.MaxPrice-s.currentPlan.Spread*0.25)
				s.startDischargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		}

	case StateCharging:
		if !inChargeWindow {
			l.Info("decision: stop charging - left charge window")
			s.stopChargingLocked(ctx, batStatus.SOC)
		} else if batStatus.SOC >= 100 {
			l.Info("decision: stop charging - battery full")
			s.stopChargingLocked(ctx, batStatus.SOC)
		} else {
			s.refreshPassiveModeLocked(ctx, -s.cfg.ChargePowerW)
		}

	case StateDischarging:
		minSOC := int(s.cfg.BatteryMinSOC * 100)
		if !inDischargeWindow {
			l.Info("decision: stop discharging - left discharge window")
			s.stopDischargingLocked(ctx, batStatus.SOC)
		} else if batStatus.SOC <= minSOC {
			l.Info("decision: stop discharging - battery at min SOC", "min_soc", minSOC)
			s.stopDischargingLocked(ctx, batStatus.SOC)
		} else {
			s.refreshPassiveModeLocked(ctx, s.cfg.DischargePowerW)
		}
	}
}

// isDischargeProfitiable checks if discharging at the given price is profitable
// compared to the last charge price, accounting for battery efficiency.
func (s *Service) isDischargeProfitiable(dischargePrice decimal.Decimal) bool {
	// If we don't have a last charge price, block discharge - we need a known cost basis
	if s.lastChargePrice.IsZero() {
		return false
	}
	// Profitable if: discharge_price > charge_price / efficiency
	efficiency := decimal.NewFromFloat(s.cfg.BatteryEfficiency)
	breakEvenPrice := s.lastChargePrice.Div(efficiency)
	return dischargePrice.GreaterThan(breakEvenPrice)
}

// startChargingLocked begins a charge session. Caller must hold s.mu.
func (s *Service) startChargingLocked(ctx context.Context, price decimal.Decimal, soc int) {
	priceF, _ := price.Float64()
	l := slog.With("action", "charge", "price_eur_kwh", priceF, "soc", soc, "power_w", s.cfg.ChargePowerW)
	l.Info("starting charge session")

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.Charge(s.cfg.ChargePowerW, s.cfg.PassiveModeTimeoutS)
	s.mu.Lock()

	if err != nil {
		l.Error("failed to start charging", "error", err)
		s.notifyError(ctx, "Failed to start charging: "+err.Error())
		return
	}

	s.state = StateCharging
	s.currentTradeStart = s.now()
	s.currentTradePrice = price
	s.currentTradeSOC = soc
	s.lastPassiveRefresh = s.now()
	s.lastChargePrice = price // Track for per-trade profitability

	l.Info("charge session started", "state", s.state)

	// Release lock for notification
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeStart(ctx, "Charging", priceF, soc); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// stopChargingLocked ends a charge session and records the trade. Caller must hold s.mu.
func (s *Service) stopChargingLocked(ctx context.Context, endSOC int) {
	now := s.now()
	duration := now.Sub(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(float64(s.cfg.ChargePowerW) * duration.Hours() / 1000.0)
	energyF, _ := energyKWh.Float64()

	// Calculate average price paid during the actual charge period
	avgPrice := s.calculateAveragePrice(s.currentTradeStart, now)
	if avgPrice.IsZero() {
		// Fallback to start price if we can't calculate average
		avgPrice = s.currentTradePrice
	}
	avgPriceF, _ := avgPrice.Float64()

	// Update lastChargePrice to the actual average (for accurate profitability check)
	s.lastChargePrice = avgPrice

	l := slog.With(
		"action", "charge",
		"avg_price_eur_kwh", avgPriceF,
		"start_soc", s.currentTradeSOC,
		"end_soc", endSOC,
		"duration", duration,
		"energy_kwh", energyF,
	)
	l.Info("stopping charge session")

	trade := Trade{
		Timestamp: s.currentTradeStart,
		Action:    ActionCharge,
		PriceEUR:  avgPrice,
		PowerW:    s.cfg.ChargePowerW,
		DurationS: int(duration.Seconds()),
		EnergyKWh: energyKWh,
		StartSOC:  s.currentTradeSOC,
		EndSOC:    endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record trade", "error", err)
	}
	s.mu.Lock()

	s.transitionToIdleLocked(ctx, endSOC)

	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Charging", energyF, avgPriceF); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// startDischargingLocked begins a discharge session. Caller must hold s.mu.
func (s *Service) startDischargingLocked(ctx context.Context, price decimal.Decimal, soc int) {
	priceF, _ := price.Float64()
	lastChargeF, _ := s.lastChargePrice.Float64()
	l := slog.With("action", "discharge", "price_eur_kwh", priceF, "soc", soc, "power_w", s.cfg.DischargePowerW, "last_charge_price", lastChargeF)
	l.Info("starting discharge session")

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.Discharge(s.cfg.DischargePowerW, s.cfg.PassiveModeTimeoutS)
	s.mu.Lock()

	if err != nil {
		l.Error("failed to start discharging", "error", err)
		s.notifyError(ctx, "Failed to start discharging: "+err.Error())
		return
	}

	s.state = StateDischarging
	s.currentTradeStart = s.now()
	s.currentTradePrice = price
	s.currentTradeSOC = soc
	s.lastPassiveRefresh = s.now()

	l.Info("discharge session started", "state", s.state)

	// Release lock for notification
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeStart(ctx, "Discharging", priceF, soc); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// stopDischargingLocked ends a discharge session and records the trade. Caller must hold s.mu.
func (s *Service) stopDischargingLocked(ctx context.Context, endSOC int) {
	duration := time.Since(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(float64(s.cfg.DischargePowerW) * duration.Hours() / 1000.0)
	energyF, _ := energyKWh.Float64()
	priceF, _ := s.currentTradePrice.Float64()

	l := slog.With(
		"action", "discharge",
		"price_eur_kwh", priceF,
		"start_soc", s.currentTradeSOC,
		"end_soc", endSOC,
		"duration", duration,
		"energy_kwh", energyF,
	)
	l.Info("stopping discharge session")

	trade := Trade{
		Timestamp: s.currentTradeStart,
		Action:    ActionDischarge,
		PriceEUR:  s.currentTradePrice,
		PowerW:    s.cfg.DischargePowerW,
		DurationS: int(duration.Seconds()),
		EnergyKWh: energyKWh,
		StartSOC:  s.currentTradeSOC,
		EndSOC:    endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record trade", "error", err)
	}
	s.mu.Lock()

	s.transitionToIdleLocked(ctx, endSOC)

	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Discharging", energyF, priceF); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// transitionToIdleLocked returns to idle state. Caller must hold s.mu.
func (s *Service) transitionToIdleLocked(ctx context.Context, soc int) {
	// Release lock during network I/O
	s.mu.Unlock()
	if err := s.battery.Idle(); err != nil {
		slog.Warn("failed to set idle mode", "error", err)
	}
	s.mu.Lock()

	s.state = StateIdle
	slog.Info("transitioned to idle", "soc", soc)
}

// refreshPassiveModeLocked refreshes the passive mode command before timeout. Caller must hold s.mu.
func (s *Service) refreshPassiveModeLocked(ctx context.Context, power int) {
	// Refresh if we're past 80% of the timeout period
	refreshThreshold := time.Duration(float64(s.cfg.PassiveModeTimeoutS)*0.8) * time.Second
	if time.Since(s.lastPassiveRefresh) < refreshThreshold {
		return
	}

	slog.Debug("refreshing passive mode", "power", power)

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.SetPassiveMode(power, s.cfg.PassiveModeTimeoutS)
	s.mu.Lock()

	if err != nil {
		slog.Error("failed to refresh passive mode", "error", err)
		return
	}
	s.lastPassiveRefresh = time.Now()
}

// checkPriceFetch checks if we should fetch new prices.
func (s *Service) checkPriceFetch(ctx context.Context) {
	now := s.now()

	// Fetch tomorrow's prices at 13:00 CET
	if now.Hour() == 13 && len(s.tomorrowPrices) == 0 {
		if err := s.fetchTomorrowPrices(ctx); err != nil {
			slog.Warn("failed to fetch tomorrow's prices", "error", err)
			s.notifyError(ctx, "Failed to fetch tomorrow's prices: "+err.Error())
		}
	}

	// At midnight, move tomorrow's prices to today (only once per day)
	today := now.Truncate(24 * time.Hour)
	alreadySwappedToday := s.lastMidnightSwap.Truncate(24 * time.Hour).Equal(today)

	if now.Hour() == 0 && now.Minute() < 15 && !alreadySwappedToday {
		s.mu.Lock()
		if len(s.tomorrowPrices) > 0 {
			s.todayPrices = s.tomorrowPrices
			s.tomorrowPrices = nil
			s.currentPlan = AnalyzePrices(s.todayPrices, s.analyzerConfig())
			s.lastMidnightSwap = now
			plan := s.currentPlan
			slotsTotal := len(s.todayPrices)
			s.mu.Unlock()

			l := slog.With("day", "today", "slots_total", slotsTotal)
			l.Info("switched to new day's prices",
				"price_min_eur_kwh", plan.MinPrice,
				"price_max_eur_kwh", plan.MaxPrice,
			)
			// Log and notify (outside lock for network I/O)
			s.logAndNotifyTradingPlan(ctx, l, plan, "today", slotsTotal, slotsTotal)
		} else {
			// Fallback: tomorrow's prices weren't fetched, fetch today's prices now
			slog.Warn("tomorrow's prices not available at midnight, fetching today's prices")
			s.lastMidnightSwap = now // Mark as handled to avoid repeated fetches
			s.mu.Unlock()
			if err := s.fetchTodayPrices(ctx); err != nil {
				slog.Error("failed to fetch today's prices at midnight", "error", err)
				s.notifyError(ctx, "Failed to fetch today's prices at midnight: "+err.Error())
			}
		}
	}
}

// fetchTodayPrices fetches today's prices from NordPool.
func (s *Service) fetchTodayPrices(ctx context.Context) error {
	prices, err := s.nordpool.FetchTodayPrices(ctx)
	if err != nil {
		return err
	}

	// Filter to only future prices for analysis (handles late-start scenarios)
	now := s.now()
	futurePrices := make([]nordpool.Price, 0, len(prices))
	for _, p := range prices {
		slotEnd := p.Time.Add(15 * time.Minute)
		if !slotEnd.Before(now) { // include slots not yet ended
			futurePrices = append(futurePrices, p)
		}
	}

	s.mu.Lock()
	s.todayPrices = prices                                          // full day for price lookups
	s.currentPlan = AnalyzePrices(futurePrices, s.analyzerConfig()) // analyze only future
	s.mu.Unlock()

	l := slog.With(
		"day", "today",
		"slots_total", len(prices),
		"slots_analyzed", len(futurePrices),
	)
	l.Info("fetched prices",
		"price_min_eur_kwh", s.currentPlan.MinPrice,
		"price_max_eur_kwh", s.currentPlan.MaxPrice,
	)

	// Log and notify trading plan
	s.logAndNotifyTradingPlan(ctx, l, s.currentPlan, "today", len(prices), len(futurePrices))

	return nil
}

// fetchTomorrowPrices fetches tomorrow's prices from NordPool.
func (s *Service) fetchTomorrowPrices(ctx context.Context) error {
	prices, err := s.nordpool.FetchTomorrowPrices(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.tomorrowPrices = prices
	s.mu.Unlock()

	plan := AnalyzePrices(prices, s.analyzerConfig())
	l := slog.With(
		"day", "tomorrow",
		"slots_total", len(prices),
	)
	l.Info("fetched prices",
		"price_min_eur_kwh", plan.MinPrice,
		"price_max_eur_kwh", plan.MaxPrice,
	)

	// Log and notify trading plan
	s.logAndNotifyTradingPlan(ctx, l, plan, "tomorrow", len(prices), len(prices))

	return nil
}

// logAndNotifyTradingPlan logs the trading plan and sends a Telegram notification.
func (s *Service) logAndNotifyTradingPlan(ctx context.Context, l *slog.Logger, plan *TradingPlan, day string, slotsTotal, slotsAnalyzed int) {
	// Calculate break-even spread needed to overcome efficiency loss
	breakEvenDischarge := plan.MinPrice / s.cfg.BatteryEfficiency
	minProfitableSpread := breakEvenDischarge - plan.MinPrice

	if !plan.IsProfitable {
		l.Info("no profitable charge→discharge sequence found",
			"reason", "window-averaged prices don't meet spread/efficiency requirements",
			"min_spread_for_efficiency", minProfitableSpread,
			"min_spread_configured", s.cfg.MinPriceSpread,
			"battery_efficiency", s.cfg.BatteryEfficiency,
		)
	} else {
		// Log each profitable cycle
		for i, c := range plan.Cycles {
			l.Info("profitable cycle found",
				"cycle", i+1,
				"charge_start", c.ChargeWindow.Start.Format("15:04"),
				"charge_end", c.ChargeWindow.End.Format("15:04"),
				"charge_avg_eur_kwh", c.ChargeWindow.Price,
				"discharge_start", c.DischargeWindow.Start.Format("15:04"),
				"discharge_end", c.DischargeWindow.End.Format("15:04"),
				"discharge_avg_eur_kwh", c.DischargeWindow.Price,
				"expected_profit_eur_kwh", c.Profit,
			)
		}
	}

	// Send Telegram notification
	if !s.telegramEnabled() {
		return
	}

	// Build notification data
	data := telegram.TradingPlanData{
		Day:                    day,
		Date:                   plan.Date,
		SlotsTotal:             slotsTotal,
		SlotsAnalyzed:          slotsAnalyzed,
		PriceMin:               plan.MinPrice,
		PriceMax:               plan.MaxPrice,
		IsProfitable:           plan.IsProfitable,
		Reason:                 "Window-averaged prices don't meet spread/efficiency requirements",
		MinSpreadForEfficiency: minProfitableSpread,
		MinSpreadConfigured:    s.cfg.MinPriceSpread,
		BatteryEfficiency:      s.cfg.BatteryEfficiency,
	}

	// Add cycles if profitable
	for _, c := range plan.Cycles {
		data.Cycles = append(data.Cycles, telegram.TradingPlanCycle{
			ChargeStart:    c.ChargeWindow.Start.Format("15:04"),
			ChargeEnd:      c.ChargeWindow.End.Format("15:04"),
			ChargePrice:    c.ChargeWindow.Price,
			DischargeStart: c.DischargeWindow.Start.Format("15:04"),
			DischargeEnd:   c.DischargeWindow.End.Format("15:04"),
			DischargePrice: c.DischargeWindow.Price,
			ProfitPerKWh:   c.Profit,
		})
	}

	if err := s.telegram.SendTradingPlan(ctx, data); err != nil {
		slog.Warn("failed to send trading plan notification", "error", err)
	}
}

// checkDailySummary sends the daily summary at 23:59.
func (s *Service) checkDailySummary(ctx context.Context) {
	now := s.now()
	if now.Hour() == 23 && now.Minute() == 59 {
		summary := s.recorder.GetTodaySummary()
		totalPnL := s.recorder.GetTotalPnL()

		pnlF, _ := summary.PnLEUR.Float64()
		chargedF, _ := summary.ChargedKWh.Float64()
		dischargedF, _ := summary.DischargedKWh.Float64()
		totalPnLF, _ := totalPnL.Float64()
		avgChargeF, _ := summary.AvgChargePrice.Float64()
		minChargeF, _ := summary.MinChargePrice.Float64()
		avgDischargeF, _ := summary.AvgDischargePrice.Float64()
		maxDischargeF, _ := summary.MaxDischargePrice.Float64()

		summaryData := telegram.DailySummaryData{
			Date:              now,
			PnLEUR:            pnlF,
			ChargedKWh:        chargedF,
			DischargedKWh:     dischargedF,
			ChargeCycles:      summary.ChargeCycles,
			DischargeCycles:   summary.DischargeCycles,
			AvgChargePrice:    avgChargeF,
			MinChargePrice:    minChargeF,
			AvgDischargePrice: avgDischargeF,
			MaxDischargePrice: maxDischargeF,
			TotalPnLEUR:       totalPnLF,
		}

		if s.telegramEnabled() {
			if err := s.telegram.SendDailySummaryFull(ctx, summaryData); err != nil {
				slog.Warn("failed to send daily summary", "error", err)
			}
		}
	}
}

// notifyError sends an error notification with rate limiting (max 1 per 15 minutes).
func (s *Service) notifyError(ctx context.Context, msg string) {
	if !s.telegramEnabled() {
		return
	}
	// Rate limit: only send one error notification per 15 minutes
	if time.Since(s.lastErrorNotify) < 15*time.Minute {
		slog.Debug("error notification rate limited", "msg", msg)
		return
	}
	s.lastErrorNotify = time.Now()

	if err := s.telegram.SendError(ctx, msg); err != nil {
		slog.Warn("failed to send error notification", "error", err)
	}
}

// handleTelegramCommands polls for and handles Telegram bot commands.
func (s *Service) handleTelegramCommands(ctx context.Context) {
	if !s.telegramEnabled() {
		return
	}
	commands, err := s.telegram.PollCommands(ctx)
	if err != nil {
		slog.Debug("failed to poll telegram commands", "error", err)
		return
	}

	for _, cmd := range commands {
		switch cmd {
		case "/status":
			s.sendTelegramStatus(ctx)
		}
	}
}

// sendTelegramStatus sends current status via Telegram.
func (s *Service) sendTelegramStatus(ctx context.Context) {
	if !s.telegramEnabled() {
		return
	}
	status := s.GetCurrentStatus()
	summary := s.recorder.GetTodaySummary()
	totalPnL := s.recorder.GetTotalPnL()

	todayPnLF, _ := summary.PnLEUR.Float64()
	totalPnLF, _ := totalPnL.Float64()

	data := telegram.StatusData{
		State:        string(status.State),
		BatterySOC:   status.BatterySOC,
		CurrentPrice: status.CurrentPrice,
		NextAction:   status.NextAction,
		TodayPnL:     todayPnLF,
		TotalPnL:     totalPnLF,
	}

	if err := s.telegram.SendStatus(ctx, data); err != nil {
		slog.Warn("failed to send status via telegram", "error", err)
	}
}

// State returns the current trading state.
func (s *Service) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// GetRecorder returns the trade recorder for metrics.
func (s *Service) GetRecorder() *Recorder {
	return s.recorder
}

// CurrentStatus contains all current state info.
type CurrentStatus struct {
	State        State   `json:"state"`
	BatterySOC   int     `json:"battery_soc"`
	CurrentPrice float64 `json:"current_price_eur_kwh,omitempty"`
	NextAction   string  `json:"next_action,omitempty"`
}

// GetCurrentStatus returns the current battery and trading status.
func (s *Service) GetCurrentStatus() CurrentStatus {
	// Get battery status OUTSIDE lock (network I/O)
	var batterySOC int
	if batStatus, err := s.battery.GetBatteryStatus(); err == nil {
		batterySOC = batStatus.SOC
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()

	status := CurrentStatus{
		State:      s.state,
		BatterySOC: batterySOC,
	}

	// Get current price
	if price, ok := GetCurrentPrice(s.todayPrices, now); ok {
		status.CurrentPrice = price
	}

	// Determine next action
	if s.currentPlan != nil && s.currentPlan.IsProfitable {
		if s.currentPlan.IsInChargeWindow(now) {
			status.NextAction = "in charge window"
		} else if s.currentPlan.IsInDischargeWindow(now) {
			status.NextAction = "in discharge window"
		} else {
			status.NextAction = "waiting for next window"
		}
	} else {
		status.NextAction = "no profitable trades today"
	}

	return status
}

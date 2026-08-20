package service

import (
	"context"
	"fmt"
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
	StateIdle          State = "idle"
	StateCharging      State = "charging"
	StateDischarging   State = "discharging"
	StateSolarCharging State = "solar_charging"
	StateStopping      State = "stopping"
)

// solarStopReason classifies why a solar charge session ended. Used to decide
// whether to apply anti-cycling backoff (only marginal-surplus stops should count).
type solarStopReason int

const (
	solarStopReasonSurplusGone solarStopReason = iota // debounce or EMA-below-floor
	solarStopReasonBatteryFull
	solarStopReasonYieldWindow
	solarStopReasonTelemetryFailure
)

// Solar anti-cycling constants.
const (
	solarMinSessionDuration       = 60 * time.Second // grace period: no "surplus-gone" stops before this
	solarRestartCooldown          = 60 * time.Second // baseline cooldown after a long session
	solarShortSessionCooldown     = 5 * time.Minute  // cooldown after a short session ended by surplus loss
	solarLongBackoffCooldown      = 15 * time.Minute // cooldown after repeated short sessions
	solarShortSessionThreshold    = 2 * time.Minute  // duration below which a session counts as "short"
	solarShortSessionBackoffCount = 3                // consecutive short sessions that trigger long backoff
	solarStartQualificationCount  = 10               // consecutive raw surplus readings before starting
	solarMinChargePowerW          = 75               // floor clamp for charge power
	solarStopDebounceCount        = 10               // consecutive low readings before stop
	solarEMAAlpha                 = 0.05             // EMA smoothing factor (~20s effective window)
)

const (
	batteryStartVerificationTimeout  = 10 * time.Second
	batteryStartVerificationInterval = time.Second
	batteryActivePowerThresholdW     = 50.0
	batteryControlFailureCooldown    = 5 * time.Minute
	batteryShutdownTimeout           = 60 * time.Second
	batteryShutdownAttemptTimeout    = 30 * time.Second
	batteryStopRetryInterval         = 5 * time.Second
	statusBatteryTimeout             = 5 * time.Second
	solarStatusFailureThreshold      = 10
	solarStatusFallbackTimeout       = 3 * time.Second
)

// Service is the main trading engine.
type Service struct {
	cfg      *config.Config
	nordpool PriceProvider
	battery  BatteryController
	meter    MeterReader
	telegram *telegram.Client
	recorder *Recorder
	loc      *time.Location   // timezone location
	nowFunc  func() time.Time // clock function for testing

	mu                          sync.RWMutex
	state                       State
	currentPlan                 *TradingPlan
	todayPrices                 []nordpool.Price
	tomorrowPrices              []nordpool.Price
	lastPassiveRefresh          time.Time
	currentTradeStart           time.Time
	currentTradePrice           decimal.Decimal
	currentTradeSOC             int
	lastChargePrice             decimal.Decimal // track last charge price for profitability check
	lastErrorNotify             time.Time       // rate limit error notifications
	lastMidnightSwap            time.Time       // track last midnight price swap to avoid repeated fetches
	lastDailySummary            time.Time       // track last daily summary to avoid duplicates on restart
	batteryCooldownUntil        time.Time       // suppress command retries after the battery ignores a command
	batteryVerificationTimeout  time.Duration   // test override for battery start verification timeout
	batteryVerificationInterval time.Duration   // test override for battery start verification polling
	batteryStopRetryDelay       time.Duration   // test override for failed-stop retry delay
	lastStopAttempt             time.Time       // throttle retries when a stop command fails

	// Solar charging state
	solarSurplusCount             int       // consecutive surplus readings above threshold
	solarStopCount                int       // consecutive readings below stop threshold
	solarChargePower              int       // current solar charge wattage
	solarMeasuredChargePowerW     float64   // latest measured battery charge power
	solarEnergyWs                 float64   // cumulative watt-seconds during solar charging
	solarLastUpdate               time.Time // last time solar energy was accumulated
	solarCooldownUntil            time.Time // no new session may start before this time
	solarSurplusEMA               float64   // exponentially weighted moving average of surplus
	solarConsecutiveShortSessions int       // count of successive short sessions ended by surplus loss
	solarStatusFailures           int       // consecutive telemetry failures during solar charging
}

// waitForBatteryPower confirms that the inverter acted on a successful control request.
func (s *Service) waitForBatteryPower(ctx context.Context, charging bool, commandedPowerW int) (float64, error) {
	timeout := s.batteryVerificationTimeout
	if timeout <= 0 {
		timeout = batteryStartVerificationTimeout
	}
	interval := s.batteryVerificationInterval
	if interval <= 0 {
		interval = batteryStartVerificationInterval
	}

	verificationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastPower float64
	var lastErr error
	threshold := min(batteryActivePowerThresholdW, max(10.0, float64(commandedPowerW)*0.5))
	for {
		power, err := s.battery.GetBatteryPower(verificationCtx)
		if err != nil {
			lastErr = err
		} else {
			lastPower = power
			if (charging && lastPower >= threshold) ||
				(!charging && lastPower <= -threshold) {
				return lastPower, nil
			}
		}

		select {
		case <-verificationCtx.Done():
			if ctx.Err() != nil {
				return lastPower, ctx.Err()
			}
			if lastErr != nil {
				return lastPower, fmt.Errorf("battery power verification failed: %w", lastErr)
			}
			return lastPower, fmt.Errorf("battery remained at %.0f W", lastPower)
		case <-ticker.C:
		}
	}
}

// New creates a new trading service.
func New(
	cfg *config.Config,
	nordpoolClient PriceProvider,
	batteryClient BatteryController,
	meterClient MeterReader,
	telegramClient *telegram.Client,
	recorder *Recorder,
) *Service {
	return &Service{
		cfg:      cfg,
		nordpool: nordpoolClient,
		battery:  batteryClient,
		meter:    meterClient,
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

// meterEnabled returns true if the P1 meter is configured.
func (s *Service) meterEnabled() bool {
	return s.meter != nil && s.meter.Enabled()
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

	weightedSum := decimal.Zero
	totalSeconds := decimal.Zero

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
				overlapSec := decimal.NewFromFloat(overlap.Seconds())
				weightedSum = weightedSum.Add(decimal.NewFromFloat(p.Value).Mul(overlapSec))
				totalSeconds = totalSeconds.Add(overlapSec)
			}
		}
	}

	if totalSeconds.IsZero() {
		return decimal.Zero
	}

	return weightedSum.Div(totalSeconds)
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
	if err := s.battery.IdleContext(ctx); err != nil {
		return fmt.Errorf("reset battery control on startup: %w", err)
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

	// Solar ticker: 1s interval when P1 meter enabled, nil channel when disabled
	var solarTickCh <-chan time.Time
	if s.meterEnabled() {
		solarTicker := time.NewTicker(1 * time.Second)
		defer solarTicker.Stop()
		solarTickCh = solarTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("stopping trading service")
			if err := s.stopBatteryOnShutdown(); err != nil {
				slog.Error("failed to stop battery during shutdown", "error", err)
				return fmt.Errorf("stop battery during shutdown: %w", err)
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

		case <-solarTickCh:
			s.solarTick(ctx)
		}
	}
}

// tick is called every minute to evaluate trading decisions.
func (s *Service) tick(ctx context.Context) {
	now := s.now()
	if s.retryStopping(ctx) {
		return
	}

	// Get battery status OUTSIDE lock (network I/O)
	batStatus, err := s.battery.GetBatteryStatusContext(ctx)
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
		switch s.state {
		case StateCharging:
			l.Info("no profitable trading plan, transitioning to idle", "has_plan", s.currentPlan != nil)
			s.stopChargingLocked(ctx, batStatus.SOC)
		case StateDischarging:
			l.Info("no profitable trading plan, transitioning to idle", "has_plan", s.currentPlan != nil)
			s.stopDischargingLocked(ctx, batStatus.SOC)
		case StateSolarCharging:
			// Solar charging doesn't depend on trading plan; let solarTick manage it
		case StateIdle:
			// Nothing to do
		}
		return
	}

	// Get current price
	currentPrice, ok := GetCurrentPrice(s.todayPrices, now)
	if !ok {
		l.Warn("no price for current time slot")
		return
	}

	// Enrich logger with price context
	l = l.With("price_eur_kwh", currentPrice)

	// Decide action based on current time window
	inChargeWindow := s.currentPlan.IsInChargeWindow(now)
	inDischargeWindow := s.currentPlan.IsInDischargeWindow(now)

	switch s.state {
	case StateIdle:
		if now.Before(s.batteryCooldownUntil) {
			l.Debug("battery control retry cooling down", "retry_at", s.batteryCooldownUntil)
			return
		}
		if inChargeWindow {
			if batStatus.SOC >= 100 {
				l.Debug("in charge window but battery full")
			} else if !batStatus.ChargingFlag {
				l.Warn("in charge window but battery charging disabled")
			} else {
				l.Info("decision: start charging",
					"min_price", s.currentPlan.MinPrice,
					"charge_threshold", s.currentPlan.MinPrice.Add(s.currentPlan.Spread.Mul(decimal.NewFromFloat(0.25))))
				s.startChargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		} else if inDischargeWindow {
			minSOC := int(s.cfg.BatteryMinSOC * 100)
			if batStatus.SOC <= minSOC {
				l.Debug("in discharge window but battery at min SOC", "min_soc", minSOC)
			} else if !batStatus.DischargFlag {
				l.Warn("in discharge window but battery discharging disabled")
			} else {
				lastChargeF, _ := s.lastChargePrice.Float64()
				l.Info("decision: start discharging",
					"last_charge_price", lastChargeF,
					"max_price", s.currentPlan.MaxPrice,
					"discharge_threshold", s.currentPlan.MaxPrice.Sub(s.currentPlan.Spread.Mul(decimal.NewFromFloat(0.25))))
				s.startDischargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		}

	case StateSolarCharging:
		// Yield to scheduled windows: stop solar charging and transition
		if inChargeWindow {
			l.Info("decision: stop solar charging - scheduled charge window started")
			s.stopSolarChargingLocked(ctx, batStatus.SOC, solarStopReasonYieldWindow)
			if s.state != StateIdle {
				return
			}
			if batStatus.SOC < 100 && batStatus.ChargingFlag {
				s.startChargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		} else if inDischargeWindow {
			minSOC := int(s.cfg.BatteryMinSOC * 100)
			l.Info("decision: stop solar charging - scheduled discharge window started")
			s.stopSolarChargingLocked(ctx, batStatus.SOC, solarStopReasonYieldWindow)
			if s.state != StateIdle {
				return
			}
			if batStatus.SOC > minSOC && batStatus.DischargFlag {
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

// accumulateSolarEnergyLocked integrates measured battery power since the previous reading.
// Caller must hold s.mu.
func (s *Service) accumulateSolarEnergyLocked(measuredChargePowerW float64) {
	now := s.now()
	if !s.solarLastUpdate.IsZero() {
		elapsed := now.Sub(s.solarLastUpdate).Seconds()
		if elapsed > 0 {
			s.solarEnergyWs += s.solarMeasuredChargePowerW * elapsed
		}
	}
	s.solarMeasuredChargePowerW = measuredChargePowerW
	s.solarLastUpdate = now
}

// solarTick is called every 1 second to manage solar self-consumption charging.
func (s *Service) solarTick(ctx context.Context) {
	if s.retryStopping(ctx) {
		return
	}

	// Read P1 meter + battery status OUTSIDE lock (network I/O)
	activePowerW, err := s.meter.GetActivePowerW()
	if err != nil {
		slog.Debug("failed to read P1 meter", "error", err)
		return
	}

	esStatus, err := s.battery.GetESStatus(ctx)
	if err != nil {
		s.handleSolarStatusFailure(ctx, err)
		return
	}
	batterySOC := esStatus.BatterySOC
	measuredChargePowerW := max(esStatus.BatteryPower, 0)

	// surplus = negative active power means exporting to grid
	surplus := -activePowerW

	s.mu.Lock()
	defer s.mu.Unlock()
	s.solarStatusFailures = 0

	switch s.state {
	case StateIdle:
		if s.now().Before(s.batteryCooldownUntil) {
			s.solarSurplusCount = 0
			return
		}

		// Restart cooldown: don't start a new session too soon after stopping.
		// The cooldown duration is adaptive — short/repeated sessions set a longer
		// cooldown when stopSolarChargingLocked records the stop.
		if s.now().Before(s.solarCooldownUntil) {
			return
		}

		// Use raw surplus (not EMA) for start decision — EMA memory from
		// previous sessions could cause false starts from a single spike.
		minSurplus := float64(s.cfg.SolarMinSurplusW)
		if surplus < minSurplus {
			s.solarSurplusCount = 0
			return
		}

		// Skip if in a scheduled window (let tick() handle it)
		if s.currentPlan != nil {
			now := s.now()
			if s.currentPlan.IsInChargeWindow(now) || s.currentPlan.IsInDischargeWindow(now) {
				s.solarSurplusCount = 0
				return
			}
		}

		// Skip if battery full
		if batterySOC >= 100 {
			s.solarSurplusCount = 0
			return
		}

		s.solarSurplusCount++
		if s.solarSurplusCount >= solarStartQualificationCount {
			power := int(surplus)
			power = max(power, solarMinChargePowerW)
			power = min(power, s.cfg.ChargePowerW)
			s.startSolarChargingLocked(ctx, power, batterySOC)
		}

	case StateSolarCharging:
		s.accumulateSolarEnergyLocked(measuredChargePowerW)

		// Compensate for feedback loop: the battery's charge power is visible on
		// the P1 meter as consumption, so measured surplus is artificially low.
		// Real surplus = what P1 sees + what the battery is currently drawing.
		effectiveSurplus := surplus + measuredChargePowerW

		// Update EMA with effective surplus during charging
		if s.solarSurplusEMA == 0 {
			s.solarSurplusEMA = effectiveSurplus
		} else {
			s.solarSurplusEMA = solarEMAAlpha*effectiveSurplus + (1-solarEMAAlpha)*s.solarSurplusEMA
		}

		sessionAge := s.now().Sub(s.currentTradeStart)

		// Stop if battery full (unconditional safety check)
		if batterySOC >= 100 {
			slog.Info("solar charging: battery full")
			s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonBatteryFull)
			return
		}

		// Yield immediately to scheduled windows (unconditional priority check)
		if s.currentPlan != nil {
			now := s.now()
			if s.currentPlan.IsInChargeWindow(now) || s.currentPlan.IsInDischargeWindow(now) {
				slog.Info("solar charging: yielding to scheduled window")
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonYieldWindow)
				return
			}
		}

		// Hysteresis: stop threshold is lower than start threshold to avoid
		// cycling when surplus hovers near the boundary. Requires solarStopDebounceCount
		// consecutive low readings AND minimum session duration before stopping.
		stopThreshold := float64(s.cfg.SolarMinSurplusW) / 4 // 25W default (vs 100W start)
		if s.solarSurplusEMA < stopThreshold {
			s.solarStopCount++
			if s.solarStopCount >= solarStopDebounceCount && sessionAge >= solarMinSessionDuration {
				slog.Info("solar charging: surplus dropped below threshold",
					"measured_surplus_w", surplus, "charge_power_w", s.solarChargePower,
					"measured_battery_power_w", measuredChargePowerW,
					"effective_surplus_w", effectiveSurplus, "stop_threshold_w", stopThreshold)
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonSurplusGone)
				return
			}
		} else {
			s.solarStopCount = 0
		}

		// Wait for battery to settle after start/adjustment before re-adjusting.
		// The battery takes ~3s to ramp to the target power; adjusting during
		// ramp-up causes a positive feedback spiral (overestimated effective surplus
		// → higher target → even higher next tick → overshoot → stop).
		sinceLastChange := s.now().Sub(s.lastPassiveRefresh)
		if sinceLastChange < 5*time.Second {
			return
		}

		// Adjust charge power to match smoothed effective surplus (with 50W deadband).
		// If EMA drops below the minimum useful charge power, stop the session
		// instead of clamping up — clamping would import from grid.
		targetPower := int(s.solarSurplusEMA)
		if targetPower < solarMinChargePowerW {
			if sessionAge >= solarMinSessionDuration {
				slog.Info("solar charging: surplus too low for minimum charge power",
					"ema_w", s.solarSurplusEMA, "min_charge_w", solarMinChargePowerW)
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonSurplusGone)
				return
			}
			// During min session, clamp to floor as safety bound
			targetPower = solarMinChargePowerW
		}
		targetPower = min(targetPower, s.cfg.ChargePowerW)

		diff := targetPower - s.solarChargePower
		if diff < 0 {
			diff = -diff
		}
		if diff > 50 {
			slog.Info("solar charging: adjusting power",
				"old_w", s.solarChargePower, "new_w", targetPower,
				"measured_surplus_w", surplus, "measured_battery_power_w", measuredChargePowerW,
				"effective_surplus_w", effectiveSurplus)

			// Release lock during network I/O
			s.mu.Unlock()
			err := s.battery.ChargeContext(ctx, targetPower, s.cfg.PassiveModeTimeoutS)
			s.mu.Lock()

			if err != nil {
				slog.Warn("solar charging: failed to adjust power", "error", err)
			} else {
				s.solarChargePower = targetPower
				s.lastPassiveRefresh = s.now()
			}
		} else {
			// Refresh passive mode to prevent timeout
			s.refreshPassiveModeLocked(ctx, -s.solarChargePower)
		}

	// StateCharging, StateDischarging: managed by regular tick, ignore
	default:
		return
	}
}

func (s *Service) handleSolarStatusFailure(ctx context.Context, telemetryErr error) {
	s.mu.Lock()
	if s.state != StateSolarCharging {
		s.mu.Unlock()
		slog.Debug("solar tick: failed to get battery telemetry", "error", telemetryErr)
		return
	}

	s.solarStatusFailures++
	failures := s.solarStatusFailures
	if failures < solarStatusFailureThreshold {
		s.mu.Unlock()
		slog.Warn("solar charging telemetry unavailable", "consecutive_failures", failures, "error", telemetryErr)
		return
	}

	endSOC := s.currentTradeSOC
	s.mu.Unlock()
	statusCtx, cancel := context.WithTimeout(ctx, solarStatusFallbackTimeout)
	status, statusErr := s.battery.GetBatteryStatusContext(statusCtx)
	cancel()

	s.mu.Lock()
	if s.state != StateSolarCharging {
		s.mu.Unlock()
		return
	}
	if statusErr == nil {
		endSOC = status.SOC
	}
	slog.Error("stopping solar charging after repeated telemetry failures",
		"consecutive_failures", failures, "status_error", statusErr, "error", telemetryErr)
	s.stopSolarChargingLocked(ctx, endSOC, solarStopReasonTelemetryFailure)
	stopped := s.state == StateIdle
	s.mu.Unlock()
	if stopped {
		s.notifyError(ctx, "Solar charging stopped because battery telemetry is unavailable: "+telemetryErr.Error())
	} else {
		s.notifyError(ctx, "Solar charging telemetry is unavailable and the battery stop is not yet confirmed: "+telemetryErr.Error())
	}
}

// startSolarChargingLocked begins a solar charge session. Caller must hold s.mu.
func (s *Service) startSolarChargingLocked(ctx context.Context, powerW int, soc int) {
	l := slog.With("action", "solar_charge", "power_w", powerW, "soc", soc)
	l.Info("starting solar charge session")

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.ChargeContext(ctx, powerW, s.cfg.PassiveModeTimeoutS)
	var measuredPowerW float64
	var idleErr error
	if err == nil {
		measuredPowerW, err = s.waitForBatteryPower(ctx, true, powerW)
	}
	if err != nil {
		if idleErr = s.idleBattery(ctx); idleErr != nil {
			l.Warn("failed to return battery to idle after start failure", "error", idleErr)
		}
	}
	s.mu.Lock()

	if err != nil {
		if idleErr != nil {
			s.state = StateStopping
			s.lastStopAttempt = s.now()
		}
		l.Error("failed to start solar charging", "error", err)
		s.solarSurplusCount = 0
		s.batteryCooldownUntil = s.now().Add(batteryControlFailureCooldown)
		s.mu.Unlock()
		s.notifyError(ctx, "Battery did not start solar charging: "+err.Error())
		s.mu.Lock()
		return
	}

	s.state = StateSolarCharging
	s.currentTradeStart = s.now()
	s.currentTradePrice = decimal.Zero // Solar is free
	s.currentTradeSOC = soc
	s.lastPassiveRefresh = s.now()
	s.solarChargePower = powerW
	s.solarSurplusCount = 0
	s.solarStopCount = 0
	s.solarEnergyWs = 0
	s.solarLastUpdate = s.now()
	s.solarMeasuredChargePowerW = max(measuredPowerW, 0)
	s.solarSurplusEMA = 0
	s.batteryCooldownUntil = time.Time{}

	l.Info("solar charge session started", "state", s.state, "measured_battery_power_w", measuredPowerW)

	// Release lock for notification
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeStart(ctx, "Solar charging", 0, soc); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// stopSolarChargingLocked ends a solar charge session and records the trade. Caller must hold s.mu.
// The reason classifies the stop for anti-cycling purposes: only surplus-gone stops on short
// sessions extend the restart cooldown.
func (s *Service) stopSolarChargingLocked(ctx context.Context, endSOC int, reason solarStopReason) {
	stopTime := s.now()
	s.accumulateSolarEnergyLocked(s.solarMeasuredChargePowerW)
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	duration := stopTime.Sub(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(s.solarEnergyWs / 3_600_000.0) // watt-seconds to kWh
	energyF, _ := energyKWh.Float64()

	// Adaptive cooldown: penalize micro-cycling.
	// Only "surplus gone" on a short session indicates marginal conditions; battery-full
	// and window-yield are legitimate and should not trigger backoff.
	cooldown := solarRestartCooldown
	if reason == solarStopReasonTelemetryFailure {
		cooldown = batteryControlFailureCooldown
		s.solarConsecutiveShortSessions = 0
	} else if reason == solarStopReasonSurplusGone && duration < solarShortSessionThreshold {
		s.solarConsecutiveShortSessions++
		if s.solarConsecutiveShortSessions >= solarShortSessionBackoffCount {
			cooldown = solarLongBackoffCooldown
		} else {
			cooldown = solarShortSessionCooldown
		}
	} else {
		s.solarConsecutiveShortSessions = 0
	}

	l := slog.With(
		"action", "solar_charge",
		"start_soc", s.currentTradeSOC,
		"end_soc", endSOC,
		"duration", duration,
		"energy_kwh", energyF,
		"cooldown", cooldown,
		"consecutive_short_sessions", s.solarConsecutiveShortSessions,
	)
	l.Info("stopping solar charge session")

	trade := Trade{
		Timestamp: s.currentTradeStart,
		Action:    ActionSolarCharge,
		PriceEUR:  decimal.Zero,
		PowerW:    s.solarChargePower,
		DurationS: int(duration.Seconds()),
		EnergyKWh: energyKWh,
		StartSOC:  s.currentTradeSOC,
		EndSOC:    endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record solar trade", "error", err)
	}
	s.mu.Lock()

	s.solarSurplusCount = 0
	s.solarStopCount = 0
	s.solarChargePower = 0
	s.solarMeasuredChargePowerW = 0
	s.solarStatusFailures = 0
	s.solarEnergyWs = 0
	s.solarCooldownUntil = s.now().Add(cooldown)
	s.solarSurplusEMA = 0
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Solar charging", energyF, 0, endSOC); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// startChargingLocked begins a charge session. Caller must hold s.mu.
func (s *Service) startChargingLocked(ctx context.Context, price decimal.Decimal, soc int) {
	priceF, _ := price.Float64()
	l := slog.With("action", "charge", "price_eur_kwh", priceF, "soc", soc, "power_w", s.cfg.ChargePowerW)
	l.Info("starting charge session")

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.ChargeContext(ctx, s.cfg.ChargePowerW, s.cfg.PassiveModeTimeoutS)
	var measuredPowerW float64
	var idleErr error
	if err == nil {
		measuredPowerW, err = s.waitForBatteryPower(ctx, true, s.cfg.ChargePowerW)
	}
	if err != nil {
		if idleErr = s.idleBattery(ctx); idleErr != nil {
			l.Warn("failed to return battery to idle after start failure", "error", idleErr)
		}
	}
	s.mu.Lock()

	if err != nil {
		if idleErr != nil {
			s.state = StateStopping
			s.lastStopAttempt = s.now()
		}
		l.Error("failed to start charging", "error", err)
		s.batteryCooldownUntil = s.now().Add(batteryControlFailureCooldown)
		errMsg := "Failed to start charging: " + err.Error()
		s.mu.Unlock()
		s.notifyError(ctx, errMsg)
		s.mu.Lock()
		return
	}

	s.state = StateCharging
	s.currentTradeStart = s.now()
	s.currentTradePrice = price
	s.currentTradeSOC = soc
	s.lastPassiveRefresh = s.now()
	s.lastChargePrice = price // Track for per-trade profitability
	s.batteryCooldownUntil = time.Time{}

	l.Info("charge session started", "state", s.state, "measured_battery_power_w", measuredPowerW)

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
	stopTime := s.now()
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	duration := stopTime.Sub(s.currentTradeStart)
	socDelta := max(endSOC-s.currentTradeSOC, 0)
	energyKWh := decimal.NewFromFloat(s.cfg.BatteryCapacityKWh).
		Mul(decimal.NewFromInt(int64(socDelta))).
		Div(decimal.NewFromInt(100))
	energyF, _ := energyKWh.Float64()

	// Calculate average price paid during the actual charge period
	avgPrice := s.calculateAveragePrice(s.currentTradeStart, stopTime)
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

	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Charging", energyF, avgPriceF, endSOC); err != nil {
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
	err := s.battery.DischargeContext(ctx, s.cfg.DischargePowerW, s.cfg.PassiveModeTimeoutS)
	var measuredPowerW float64
	var idleErr error
	if err == nil {
		measuredPowerW, err = s.waitForBatteryPower(ctx, false, s.cfg.DischargePowerW)
	}
	if err != nil {
		if idleErr = s.idleBattery(ctx); idleErr != nil {
			l.Warn("failed to return battery to idle after start failure", "error", idleErr)
		}
	}
	s.mu.Lock()

	if err != nil {
		if idleErr != nil {
			s.state = StateStopping
			s.lastStopAttempt = s.now()
		}
		l.Error("failed to start discharging", "error", err)
		s.batteryCooldownUntil = s.now().Add(batteryControlFailureCooldown)
		errMsg := "Failed to start discharging: " + err.Error()
		s.mu.Unlock()
		s.notifyError(ctx, errMsg)
		s.mu.Lock()
		return
	}

	s.state = StateDischarging
	s.currentTradeStart = s.now()
	s.currentTradePrice = price
	s.currentTradeSOC = soc
	s.lastPassiveRefresh = s.now()
	s.batteryCooldownUntil = time.Time{}

	l.Info("discharge session started", "state", s.state, "measured_battery_power_w", measuredPowerW)

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
	stopTime := s.now()
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	duration := stopTime.Sub(s.currentTradeStart)
	socDelta := max(s.currentTradeSOC-endSOC, 0)
	energyKWh := decimal.NewFromFloat(s.cfg.BatteryCapacityKWh).
		Mul(decimal.NewFromInt(int64(socDelta))).
		Div(decimal.NewFromInt(100)).
		Mul(decimal.NewFromFloat(s.cfg.BatteryEfficiency))
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

	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Discharging", energyF, priceF, endSOC); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// transitionToIdleLocked returns to idle state. Caller must hold s.mu.
func (s *Service) transitionToIdleLocked(ctx context.Context, soc int) bool {
	if !s.lastStopAttempt.IsZero() && s.now().Sub(s.lastStopAttempt) < s.stopRetryDelay() {
		return false
	}
	s.lastStopAttempt = s.now()

	// Release lock during network I/O
	s.mu.Unlock()
	if err := s.idleBattery(ctx); err != nil {
		s.mu.Lock()
		slog.Error("failed to set idle mode; retaining active state for retry", "state", s.state, "error", err)
		s.mu.Unlock()
		s.notifyError(ctx, "Battery stop failed; forced operation may still be active: "+err.Error())
		s.mu.Lock()
		return false
	}
	s.mu.Lock()

	s.state = StateIdle
	s.lastStopAttempt = time.Time{}
	slog.Info("transitioned to idle", "soc", soc)
	return true
}

func (s *Service) stopRetryDelay() time.Duration {
	if s.batteryStopRetryDelay > 0 {
		return s.batteryStopRetryDelay
	}
	return batteryStopRetryInterval
}

func (s *Service) retryStopping(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateStopping {
		return false
	}
	s.transitionToIdleLocked(ctx, 0)
	return true
}

func (s *Service) stopBatteryOnShutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), batteryShutdownTimeout)
	defer cancel()

	var lastErr error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(shutdownCtx, batteryShutdownAttemptTimeout)
		lastErr = s.battery.IdleContext(attemptCtx)
		attemptCancel()
		if lastErr == nil {
			return nil
		}

		timer := time.NewTimer(s.stopRetryDelay())
		select {
		case <-shutdownCtx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
	}
}

func (s *Service) idleBattery(ctx context.Context) error {
	err := s.battery.IdleContext(ctx)
	if err == nil || ctx.Err() == nil {
		return err
	}

	safetyCtx, cancel := context.WithTimeout(context.Background(), batteryShutdownAttemptTimeout)
	defer cancel()
	return s.battery.IdleContext(safetyCtx)
}

// refreshPassiveModeLocked refreshes the passive mode command before timeout. Caller must hold s.mu.
func (s *Service) refreshPassiveModeLocked(ctx context.Context, power int) {
	// Refresh if we're past 80% of the timeout period
	refreshThreshold := time.Duration(float64(s.cfg.PassiveModeTimeoutS)*0.8) * time.Second
	if s.now().Sub(s.lastPassiveRefresh) < refreshThreshold {
		return
	}

	slog.Debug("refreshing passive mode", "power", power)

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.SetPassiveModeContext(ctx, power, s.cfg.PassiveModeTimeoutS)
	s.mu.Lock()

	if err != nil {
		slog.Error("failed to refresh passive mode", "error", err)
		return
	}
	s.lastPassiveRefresh = s.now()
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
	today := localMidnight(now)
	alreadySwappedToday := localMidnight(s.lastMidnightSwap).Equal(today)

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

	if len(prices) == 0 {
		slog.Debug("tomorrow's prices not available yet")
		return nil
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
	efficiency := decimal.NewFromFloat(s.cfg.BatteryEfficiency)
	breakEvenDischarge := plan.MinPrice.Div(efficiency)
	minProfitableSpread := breakEvenDischarge.Sub(plan.MinPrice)

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

	// Build notification data (convert decimal to float64 at Telegram API boundary)
	minProfitableSpreadF, _ := minProfitableSpread.Float64()
	data := telegram.TradingPlanData{
		Day:                    day,
		Date:                   plan.Date,
		SlotsTotal:             slotsTotal,
		SlotsAnalyzed:          slotsAnalyzed,
		PriceMin:               plan.MinPrice.InexactFloat64(),
		PriceMax:               plan.MaxPrice.InexactFloat64(),
		IsProfitable:           plan.IsProfitable,
		Reason:                 "Window-averaged prices don't meet spread/efficiency requirements",
		MinSpreadForEfficiency: minProfitableSpreadF,
		MinSpreadConfigured:    s.cfg.MinPriceSpread,
		BatteryEfficiency:      s.cfg.BatteryEfficiency,
	}

	// Add cycles if profitable
	for _, c := range plan.Cycles {
		data.Cycles = append(data.Cycles, telegram.TradingPlanCycle{
			ChargeStart:    c.ChargeWindow.Start.Format("15:04"),
			ChargeEnd:      c.ChargeWindow.End.Format("15:04"),
			ChargePrice:    c.ChargeWindow.Price.InexactFloat64(),
			DischargeStart: c.DischargeWindow.Start.Format("15:04"),
			DischargeEnd:   c.DischargeWindow.End.Format("15:04"),
			DischargePrice: c.DischargeWindow.Price.InexactFloat64(),
			ProfitPerKWh:   c.Profit.InexactFloat64(),
		})
	}

	if err := s.telegram.SendTradingPlan(ctx, data); err != nil {
		slog.Warn("failed to send trading plan notification", "error", err)
	}
}

// checkDailySummary sends the daily summary at 23:59 (once per day).
func (s *Service) checkDailySummary(ctx context.Context) {
	now := s.now()
	if now.Hour() == 23 && now.Minute() == 59 && !localMidnight(s.lastDailySummary).Equal(localMidnight(now)) {
		s.lastDailySummary = now
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

		solarChargedF, _ := summary.SolarChargedKWh.Float64()

		summaryData := telegram.DailySummaryData{
			Date:              now,
			PnLEUR:            pnlF,
			ChargedKWh:        chargedF,
			DischargedKWh:     dischargedF,
			ChargeCycles:      summary.ChargeCycles,
			DischargeCycles:   summary.DischargeCycles,
			SolarChargedKWh:   solarChargedF,
			SolarChargeCycles: summary.SolarChargeCycles,
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
	status := s.GetCurrentStatus(ctx)
	summary := s.recorder.GetTodaySummary()
	totalPnL := s.recorder.GetTotalPnL()

	todayPnLF, _ := summary.PnLEUR.Float64()
	totalPnLF, _ := totalPnL.Float64()

	data := telegram.StatusData{
		State:            string(status.State),
		BatteryAvailable: status.BatteryAvailable,
		BatterySOC:       status.BatterySOC,
		BatteryPowerW:    status.BatteryPowerW,
		CurrentPrice:     status.CurrentPrice,
		NextAction:       status.NextAction,
		TodayPnL:         todayPnLF,
		TotalPnL:         totalPnLF,
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
	State            State   `json:"state"`
	BatteryAvailable bool    `json:"battery_available"`
	BatterySOC       int     `json:"battery_soc"`
	BatteryPowerW    float64 `json:"battery_power_w"`
	CurrentPrice     float64 `json:"current_price_eur_kwh,omitempty"`
	NextAction       string  `json:"next_action,omitempty"`
}

// GetCurrentStatus returns the current battery and trading status.
func (s *Service) GetCurrentStatus(ctx context.Context) CurrentStatus {
	// Get battery status OUTSIDE lock (network I/O)
	var batterySOC int
	var batteryPowerW float64
	var batteryAvailable bool
	batteryCtx, cancel := context.WithTimeout(ctx, statusBatteryTimeout)
	defer cancel()
	if esStatus, err := s.battery.GetESStatus(batteryCtx); err == nil {
		batterySOC = esStatus.BatterySOC
		batteryPowerW = esStatus.BatteryPower
		batteryAvailable = true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()

	status := CurrentStatus{
		State:            s.state,
		BatteryAvailable: batteryAvailable,
		BatterySOC:       batterySOC,
		BatteryPowerW:    batteryPowerW,
	}

	// Get current price (convert to float64 for JSON API boundary)
	if price, ok := GetCurrentPrice(s.todayPrices, now); ok {
		status.CurrentPrice = price.InexactFloat64()
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

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/clients/telegram"
	"github.com/foae/marstek-energy-trading/internal/config"
)

// State represents the current trading state.
type State string

const (
	StateIdle              State = "idle"
	StateCharging          State = "charging"
	StateDischarging       State = "discharging"
	StateManualDischarging State = "manual_discharging"
	StateSolarCharging     State = "solar_charging"
	StateStopping          State = "stopping"
)

// solarStopReason classifies why a solar charge session ended. Used to decide
// whether to apply anti-cycling backoff (only marginal-surplus stops should count).
type solarStopReason int

const (
	solarStopReasonSurplusGone solarStopReason = iota // sustained insufficient surplus
	solarStopReasonBatteryFull
	solarStopReasonYieldWindow
	solarStopReasonTelemetryFailure
	solarStopReasonControlFailure
)

// Solar anti-cycling and telemetry timing constants.
const (
	solarLowSurplusGrace          = 60 * time.Second // bridge a brief dip at minimum charge power
	solarRestartCooldown          = 60 * time.Second // baseline cooldown after a long session
	solarShortSessionCooldown     = 5 * time.Minute  // cooldown after a short session ended by surplus loss
	solarLongBackoffCooldown      = 15 * time.Minute // cooldown after repeated short sessions
	solarShortSessionThreshold    = 10 * time.Minute // marginal sessions retain stronger restart protection
	solarShortSessionBackoffCount = 3                // consecutive short sessions that trigger long backoff
	solarStartQualification       = 30 * time.Second // sustained raw surplus required before starting
	solarTelemetryGapTolerance    = 2 * time.Second  // more than two nominal samples breaks qualification
	solarMinChargePowerW          = 75               // floor clamp for charge power
	solarChargeUpperSOC           = 99               // stop solar charging when integer SOC reaches this limit
	solarChargeResumeSOC          = 97               // re-arm only after SOC falls enough to reject 98/99 telemetry flicker
	solarEMAAlpha                 = 0.05             // EMA smoothing factor at a one-second sample interval
)

const (
	batteryStartVerificationTimeout  = 10 * time.Second
	batteryStartVerificationInterval = time.Second
	batteryActivePowerThresholdW     = 50.0
	batteryControlFailureCooldown    = 5 * time.Minute
	batteryLinkDownCooldown          = 30 * time.Minute
	batteryShutdownTimeout           = 60 * time.Second
	batteryShutdownAttemptTimeout    = 30 * time.Second
	batteryStopRetryInterval         = 5 * time.Second
	// A dead RS485 link fails the stop command instantly, so the normal 5s retry
	// would spin. Writes are provably dropped, so nothing is running to stop.
	batteryLinkDownStopRetryInterval = 5 * time.Minute
	// A wedged RS485 link only recovers with an ESP32 reboot; don't reboot in a loop.
	bridgeRestartMinInterval       = 10 * time.Minute
	bridgeRebootGrace              = time.Minute // ESP32 is unreachable for ~30 s after a restart
	linkDownNotifyInterval         = 15 * time.Minute
	statusBatteryTimeout           = 5 * time.Second
	solarTelemetryFailureThreshold = 10 * time.Second
	solarStatusFallbackTimeout     = 3 * time.Second
)

const manualOverrideMaxDuration = 2 * time.Hour

// Service is the main trading engine.
type Service struct {
	cfg      *config.Config
	nordpool PriceProvider
	battery  BatteryController
	meter    MeterReader
	telegram Notifier
	recorder *Recorder
	loc      *time.Location   // timezone location
	nowFunc  func() time.Time // clock function for testing

	mu                          sync.RWMutex
	errorNotifyMu               sync.Mutex
	linkDownNotifyMu            sync.Mutex
	state                       State
	currentPlan                 *TradingPlan
	pendingPlan                 *TradingPlan // plan fetched while an automatic cycle is still committed
	todayPrices                 []nordpool.Price
	tomorrowPrices              []nordpool.Price
	lastPassiveRefresh          time.Time
	currentTradeStart           time.Time
	currentTradeSOC             int
	currentTradeLastSOC         int
	currentTradePowerW          int
	currentTradeLastPowerW      float64
	currentTradeLastUpdate      time.Time
	currentTradeEnergyWs        float64
	currentTradePricedEnergyWs  float64
	currentTradeUnpricedWs      float64
	currentTradeCostEUR         decimal.Decimal
	currentTradePrices          []nordpool.Price
	lastChargePrice             decimal.Decimal // informational price of the most recent grid charge
	observedChargePowerW        float64
	manualOverrideUntil         time.Time
	lastErrorNotify             time.Time     // rate limit error notifications
	lastMidnightSwap            time.Time     // track last midnight price swap to avoid repeated fetches
	lastDailySummary            time.Time     // track last midnight price of a successfully sent summary
	batteryCooldownUntil        time.Time     // suppress command retries after the battery ignores a command
	batteryVerificationTimeout  time.Duration // test override for battery start verification timeout
	batteryVerificationInterval time.Duration // test override for battery start verification polling
	batteryStopRetryDelay       time.Duration // test override for failed-stop retry delay
	lastStopAttempt             time.Time     // throttle retries when a stop command fails
	lastStopLinkDown            bool          // last stop failure was a dead RS485 link; back off harder
	stopPending                 bool          // irreversible intent until a confirmed stop
	pendingSolarStopReason      solarStopReason
	linkDownSince               time.Time // first detection of a frozen RS485 link during an active session
	lastLinkDownNotify          time.Time // rate limit link-down notifications (own limiter)
	lastBridgeRestart           time.Time // rate limit ESPHome bridge restarts

	// Battery telemetry is sampled only by the serialized control loop. Status
	// readers expose this cache together with its observation time.
	batteryTelemetryAvailable bool
	batteryTelemetrySOC       int
	batteryTelemetryPowerW    float64
	batteryTelemetryUpdatedAt time.Time

	// Solar charging state
	solarSurplusSince             time.Time // first continuous raw-surplus observation
	solarLastSampleAt             time.Time // last valid combined battery and meter sample
	solarLowSurplusSince          time.Time // beginning of the current smoothed surplus deficit
	solarChargePower              int       // current solar charge wattage
	solarMeasuredChargePowerW     float64   // latest measured battery charge power
	solarEnergyWs                 float64   // cumulative watt-seconds during solar charging
	solarGridPowerW               float64
	solarGridEnergyWs             float64
	solarGridCostEUR              decimal.Decimal
	solarGridUnpricedWs           float64
	solarOpportunityCostEUR       decimal.Decimal
	solarOpportunityUnpricedWs    float64
	solarControlFailed            bool
	solarLastUpdate               time.Time // last time solar energy was accumulated
	solarCooldownUntil            time.Time // no new session may start before this time
	solarSurplusEMA               float64   // exponentially weighted moving average of surplus
	solarEMALastSampleAt          time.Time // EMA input timestamp for elapsed-time smoothing
	solarConsecutiveShortSessions int       // count of successive short sessions ended by surplus loss
	solarTelemetryFailureSince    time.Time // first consecutive telemetry failure during solar charging
	solarUpperSOCHold             bool      // latch set near full until SOC falls to the resume threshold
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
	telegramClient Notifier,
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

const measuredBatteryPowerEnergyBasis = "measured_battery_power"

// snapshotSessionPricesLocked retains every price slot seen while a session is
// active. Fetches may replace the live day slices at midnight, but settlement
// must still price energy consumed before that swap.
func (s *Service) snapshotSessionPricesLocked() {
	for _, prices := range [][]nordpool.Price{s.todayPrices, s.tomorrowPrices} {
		for _, price := range prices {
			known := false
			for _, retained := range s.currentTradePrices {
				if retained.Time.Equal(price.Time) {
					known = true
					break
				}
			}
			if !known {
				s.currentTradePrices = append(s.currentTradePrices, price)
			}
		}
	}
	sort.Slice(s.currentTradePrices, func(i, j int) bool {
		return s.currentTradePrices[i].Time.Before(s.currentTradePrices[j].Time)
	})
}

// sessionPriceAtLocked returns the retained price that applies at at, along
// with the end of that priced or explicitly-unpriced interval.
func (s *Service) sessionPriceAtLocked(at, until time.Time) (decimal.Decimal, time.Time, bool) {
	end := until
	for _, price := range s.currentTradePrices {
		slotEnd := price.Time.Add(15 * time.Minute)
		if !at.Before(price.Time) && at.Before(slotEnd) {
			if slotEnd.Before(end) {
				end = slotEnd
			}
			return decimal.NewFromFloat(price.Value), end, true
		}
		if price.Time.After(at) && price.Time.Before(end) {
			end = price.Time
		}
	}
	return decimal.Zero, end, false
}

func (s *Service) cacheBatteryTelemetryLocked(soc int, powerW float64) {
	s.batteryTelemetryAvailable = true
	s.batteryTelemetrySOC = soc
	s.batteryTelemetryPowerW = powerW
	s.batteryTelemetryUpdatedAt = s.now()
}

func (s *Service) beginMeasuredTradeLocked(measuredPowerW float64) {
	s.currentTradeLastPowerW = measuredPowerW
	s.currentTradeLastUpdate = s.now()
	s.currentTradeEnergyWs = 0
	s.currentTradePricedEnergyWs = 0
	s.currentTradeUnpricedWs = 0
	s.currentTradeCostEUR = decimal.Zero
	s.currentTradePrices = nil
	s.snapshotSessionPricesLocked()
}

func (s *Service) accumulateMeasuredTradeEnergyLocked(measuredPowerW float64) {
	switch s.state {
	case StateCharging:
		s.accumulateMeasuredTradeEnergyAtLocked(measuredPowerW, s.now(), true)
	case StateDischarging, StateManualDischarging:
		s.accumulateMeasuredTradeEnergyAtLocked(measuredPowerW, s.now(), false)
	}
}

// accumulateMeasuredTradeEnergyAtLocked integrates the preceding measured
// sample through at. charging supplies direction explicitly so a confirmed
// stop can settle energy before state is changed to idle.
func (s *Service) accumulateMeasuredTradeEnergyAtLocked(measuredPowerW float64, at time.Time, charging bool) {
	if s.currentTradeLastUpdate.IsZero() || !s.currentTradeLastUpdate.Before(at) {
		s.currentTradeLastPowerW = measuredPowerW
		s.currentTradeLastUpdate = at
		return
	}

	// Integrate the sample observed at the start of the interval, then retain
	// the newly observed sample for the next interval.
	powerW := max(s.currentTradeLastPowerW, 0)
	if !charging {
		powerW = max(-s.currentTradeLastPowerW, 0)
	}
	s.snapshotSessionPricesLocked()
	for cursor := s.currentTradeLastUpdate; cursor.Before(at); {
		price, end, known := s.sessionPriceAtLocked(cursor, at)
		ws := powerW * end.Sub(cursor).Seconds()
		s.currentTradeEnergyWs += ws
		if known {
			s.currentTradePricedEnergyWs += ws
			s.currentTradeCostEUR = s.currentTradeCostEUR.Add(
				price.Mul(decimal.NewFromFloat(ws / 3_600_000)),
			)
		} else {
			s.currentTradeUnpricedWs += ws
		}
		cursor = end
	}
	s.currentTradeLastPowerW = measuredPowerW
	s.currentTradeLastUpdate = at
}

func (s *Service) measuredTradePriceLocked() (decimal.Decimal, bool) {
	if s.currentTradePricedEnergyWs <= 0 {
		return decimal.Zero, false
	}
	return s.currentTradeCostEUR.Div(decimal.NewFromFloat(s.currentTradePricedEnergyWs / 3_600_000)), true
}

// Start begins the trading loop.
func (s *Service) Start(ctx context.Context) error {
	slog.Info("starting trading service")

	// Load existing trades. A corrupt history blocks recording, so place the
	// battery in its authoritative idle mode before returning the load error.
	if err := s.recorder.LoadTrades(); err != nil {
		slog.Error("failed to load trades; refusing to trade without persistence", "error", err)
		if connectErr := s.battery.Connect(); connectErr != nil {
			return fmt.Errorf("load trades: %w; connect battery for safe idle: %v", err, connectErr)
		}
		if idleErr := s.battery.IdleContext(ctx); idleErr != nil {
			return fmt.Errorf("load trades: %w; set battery idle: %v", err, idleErr)
		}
		return err
	}

	// Restore the last charge price for informational status and logging only.
	if lastCharge := s.recorder.GetLastChargeTrade(); lastCharge != nil {
		s.lastChargePrice = lastCharge.PriceEUR
		slog.Info("restored informational last charge price", "price", s.lastChargePrice)
	}

	// Connect to battery
	if err := s.battery.Connect(); err != nil {
		return err
	}
	if err := s.battery.IdleContext(ctx); err != nil {
		s.mu.Lock()
		s.state = StateStopping
		s.lastStopAttempt = s.now()
		s.mu.Unlock()
		slog.Error("failed to reset battery control on startup; will retry", "error", err)
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

	// Get battery telemetry OUTSIDE the lock (network I/O).
	batStatus, err := s.battery.GetBatteryStatusContext(ctx)
	if err != nil {
		s.mu.Lock()
		switch s.state {
		case StateCharging:
			s.stopChargingLocked(ctx, s.currentTradeLastSOC)
		case StateDischarging, StateManualDischarging:
			s.stopDischargingLocked(ctx, s.currentTradeLastSOC)
		case StateSolarCharging:
			s.stopSolarChargingLocked(ctx, s.currentTradeLastSOC, solarStopReasonTelemetryFailure)
		}
		s.mu.Unlock()
		slog.Error("failed to get battery status", "error", err)
		s.notifyError(ctx, "Battery unreachable: "+err.Error())
		return
	}
	measuredPowerW, powerErr := s.battery.GetBatteryPower(ctx)

	s.mu.Lock()
	s.currentTradeLastSOC = batStatus.SOC
	if powerErr == nil {
		s.cacheBatteryTelemetryLocked(batStatus.SOC, measuredPowerW)
		switch s.state {
		case StateCharging:
			s.observedChargePowerW = max(measuredPowerW, 0)
			s.accumulateMeasuredTradeEnergyLocked(measuredPowerW)
		case StateDischarging, StateManualDischarging:
			s.accumulateMeasuredTradeEnergyLocked(measuredPowerW)
		}
	} else {
		switch s.state {
		case StateCharging:
			s.stopChargingLocked(ctx, batStatus.SOC)
		case StateDischarging, StateManualDischarging:
			s.stopDischargingLocked(ctx, batStatus.SOC)
		case StateSolarCharging:
			s.stopSolarChargingLocked(ctx, batStatus.SOC, solarStopReasonTelemetryFailure)
		}
	}
	s.mu.Unlock()
	if powerErr != nil {
		slog.Error("failed to get battery power", "error", powerErr)
		s.notifyError(ctx, "Battery power telemetry unavailable: "+powerErr.Error())
		return
	}

	// Telemetry can look healthy while the RS485 link is frozen: check staleness
	// during active sessions, unlocked (network I/O).
	s.mu.RLock()
	activeSession := s.state == StateCharging || s.state == StateDischarging ||
		s.state == StateManualDischarging || s.state == StateSolarCharging
	s.mu.RUnlock()
	if activeSession {
		s.checkLinkDuringSession(ctx)
	}

	// Now lock for state access and updates.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentTradeLastSOC = batStatus.SOC

	// Create contextual logger for this tick
	l := slog.With(
		"state", s.state,
		"soc", batStatus.SOC,
		"time", now.Format("15:04"),
	)

	l.Debug("tick", "charging_enabled", batStatus.ChargingFlag, "discharging_enabled", batStatus.DischargFlag)

	if s.state == StateManualDischarging {
		s.currentTradeLastSOC = batStatus.SOC
		minSOC := s.cfg.MinSOCPercent()
		switch {
		case batStatus.SOC <= minSOC:
			l.Info("stopping manual discharge - battery at min SOC", "min_soc", minSOC)
			s.stopDischargingLocked(ctx, batStatus.SOC)
		case !batStatus.DischargFlag:
			l.Warn("stopping manual discharge - battery discharging disabled")
			s.stopDischargingLocked(ctx, batStatus.SOC)
		case !now.Before(s.manualOverrideUntil):
			l.Info("stopping manual discharge - safety timeout reached")
			s.stopDischargingLocked(ctx, batStatus.SOC)
		default:
			if !s.refreshPassiveModeLocked(ctx, s.currentTradePowerW) {
				l.Error("manual discharge refresh failed; stopping override")
				s.stopDischargingLocked(ctx, batStatus.SOC)
			}
		}
		return
	}
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
	currentPrice, ok := s.currentPriceLocked(now)
	if !ok {
		l.Warn("no price for current time slot")
		switch s.state {
		case StateCharging:
			s.stopChargingLocked(ctx, batStatus.SOC)
		case StateDischarging:
			s.stopDischargingLocked(ctx, batStatus.SOC)
		}
		return
	}

	// Enrich logger with price context
	l = l.With("price_eur_kwh", currentPrice)

	// Decide action based on current time window
	inChargeWindow := s.gridReservedLocked(now, batStatus.SOC)
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
				l.Info("decision: start charging", "min_price", s.currentPlan.MinPrice)
				s.startChargingLocked(ctx, currentPrice, batStatus.SOC)
			}
		} else if inDischargeWindow {
			minSOC := s.cfg.MinSOCPercent()
			if batStatus.SOC <= minSOC {
				l.Debug("in discharge window but battery at min SOC", "min_soc", minSOC)
			} else if !batStatus.DischargFlag {
				l.Warn("in discharge window but battery discharging disabled")
			} else {
				lastChargeF, _ := s.lastChargePrice.Float64()
				l.Info("decision: start discharging",
					"last_charge_price", lastChargeF,
					"max_price", s.currentPlan.MaxPrice)
				s.startDischargingLocked(ctx, currentPrice, batStatus.SOC, s.cfg.DischargePowerW, StateDischarging)
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
			minSOC := s.cfg.MinSOCPercent()
			l.Info("decision: stop solar charging - scheduled discharge window started")
			s.stopSolarChargingLocked(ctx, batStatus.SOC, solarStopReasonYieldWindow)
			if s.state != StateIdle {
				return
			}
			if batStatus.SOC > minSOC && batStatus.DischargFlag {
				s.startDischargingLocked(ctx, currentPrice, batStatus.SOC, s.cfg.DischargePowerW, StateDischarging)
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
			if !s.refreshPassiveModeLocked(ctx, -s.cfg.ChargePowerW) {
				s.stopChargingLocked(ctx, batStatus.SOC)
			}
		}

	case StateDischarging:
		minSOC := s.cfg.MinSOCPercent()
		if !inDischargeWindow {
			l.Info("decision: stop discharging - left discharge window")
			s.stopDischargingLocked(ctx, batStatus.SOC)
		} else if batStatus.SOC <= minSOC {
			l.Info("decision: stop discharging - battery at min SOC", "min_soc", minSOC)
			s.stopDischargingLocked(ctx, batStatus.SOC)
		} else {
			if !s.refreshPassiveModeLocked(ctx, s.cfg.DischargePowerW) {
				s.stopDischargingLocked(ctx, batStatus.SOC)
			}
		}
	}
}

// accumulateSolarEnergyLocked integrates measured battery power through now.
// Caller must hold s.mu.
func (s *Service) accumulateSolarEnergyLocked(measuredChargePowerW float64) {
	s.accumulateSolarEnergyAtLocked(measuredChargePowerW, s.now())
}

// accumulateSolarEnergyAtLocked settles the preceding measured sample through at.
// It remains valid after the state changes to idle, allowing a confirmed stop to
// include successful control-command latency.
func (s *Service) accumulateSolarEnergyAtLocked(measuredChargePowerW float64, at time.Time) {
	if !s.solarLastUpdate.IsZero() && s.solarLastUpdate.Before(at) {
		elapsed := at.Sub(s.solarLastUpdate).Seconds()
		gridPowerW := max(s.solarGridPowerW, 0)
		solarPowerW := max(s.solarMeasuredChargePowerW-gridPowerW, 0)
		s.solarEnergyWs += s.solarMeasuredChargePowerW * elapsed
		s.solarGridEnergyWs += gridPowerW * elapsed
		s.snapshotSessionPricesLocked()
		for cursor := s.solarLastUpdate; cursor.Before(at); {
			price, end, known := s.sessionPriceAtLocked(cursor, at)
			intervalSeconds := end.Sub(cursor).Seconds()
			gridEnergyKWh := decimal.NewFromFloat(gridPowerW * intervalSeconds / 3_600_000)
			solarEnergyKWh := decimal.NewFromFloat(solarPowerW * intervalSeconds / 3_600_000)
			if known {
				s.solarGridCostEUR = s.solarGridCostEUR.Add(price.Mul(gridEnergyKWh))
				s.solarOpportunityCostEUR = s.solarOpportunityCostEUR.Add(price.Mul(solarEnergyKWh))
			} else {
				s.solarGridUnpricedWs += gridPowerW * intervalSeconds
				s.solarOpportunityUnpricedWs += solarPowerW * intervalSeconds
			}
			cursor = end
		}
	}
	s.solarMeasuredChargePowerW = measuredChargePowerW
	s.solarLastUpdate = at
}

// solarTick is called every 1 second to manage solar self-consumption charging.
func (s *Service) solarTick(ctx context.Context) {
	if s.retryStopping(ctx) {
		return
	}
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	if state != StateIdle && state != StateSolarCharging {
		return
	}

	// Battery protection and scheduled priority must not depend on P1 availability.
	esStatus, err := s.battery.GetESStatus(ctx)
	if err != nil {
		s.handleSolarStatusFailure(ctx, err)
		return
	}
	batterySOC := esStatus.BatterySOC
	measuredChargePowerW := max(esStatus.BatteryPower, 0)
	s.mu.Lock()
	s.currentTradeLastSOC = batterySOC
	s.cacheBatteryTelemetryLocked(batterySOC, esStatus.BatteryPower)
	if s.state == StateSolarCharging {
		s.solarGridPowerW = min(s.solarGridPowerW, measuredChargePowerW)
		if batterySOC >= solarChargeUpperSOC {
			s.solarUpperSOCHold = true
			s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonBatteryFull)
			s.mu.Unlock()
			return
		}
		now := s.now()
		if s.gridReservedLocked(now, batterySOC) ||
			(s.currentPlan != nil && s.currentPlan.IsInDischargeWindow(now)) ||
			!s.solarEconomicalLocked(now, batterySOC) {
			s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonYieldWindow)
			s.mu.Unlock()
			s.tick(ctx)
			return
		}
	}
	s.mu.Unlock()

	activePowerW, err := s.meter.GetActivePowerW()
	if err != nil {
		s.handleSolarStatusFailure(ctx, fmt.Errorf("P1 meter: %w", err))
		return
	}

	// surplus = negative active power means exporting to grid
	surplus := -activePowerW

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.solarTelemetryFailureSince = time.Time{}
	if !s.solarLastSampleAt.IsZero() && now.Sub(s.solarLastSampleAt) > solarTelemetryGapTolerance {
		s.solarSurplusSince = time.Time{}
	}
	s.solarLastSampleAt = now

	switch s.state {
	case StateIdle:
		if batterySOC >= solarChargeUpperSOC {
			s.solarUpperSOCHold = true
		}
		if s.solarUpperSOCHold {
			if batterySOC > solarChargeResumeSOC {
				s.solarSurplusSince = time.Time{}
				return
			}
			s.solarUpperSOCHold = false
		}
		if now.Before(s.batteryCooldownUntil) {
			s.solarSurplusSince = time.Time{}
			return
		}

		// Restart cooldown: don't start a new session too soon after stopping.
		// The cooldown duration is adaptive — short/repeated sessions set a longer
		// cooldown when stopSolarChargingLocked records the stop.
		if now.Before(s.solarCooldownUntil) {
			s.solarSurplusSince = time.Time{}
			return
		}

		// Use raw surplus (not EMA) for start decision — EMA memory from
		// previous sessions could cause false starts from a single spike.
		minSurplus := float64(s.cfg.SolarMinSurplusW)
		if surplus < minSurplus {
			s.solarSurplusSince = time.Time{}
			return
		}

		// Yield to reserved grid charging, planned discharge, or a cheaper
		// reserved grid slot that solar can no longer economically replace.
		if s.gridReservedLocked(now, batterySOC) ||
			(s.currentPlan != nil && s.currentPlan.IsInDischargeWindow(now)) ||
			!s.solarEconomicalLocked(now, batterySOC) {
			s.solarSurplusSince = time.Time{}
			return
		}

		// The upper-SOC latch above handles both the hard stop and telemetry
		// flicker near full. Once cleared, SOC is below the resume threshold.
		if s.solarSurplusSince.IsZero() {
			s.solarSurplusSince = now
			return
		}
		if now.Sub(s.solarSurplusSince) >= solarStartQualification {
			power := int(surplus)
			power = max(power, solarMinChargePowerW)
			power = min(power, s.cfg.ChargePowerW)
			s.startSolarChargingLocked(ctx, power, batterySOC)
		}

	case StateSolarCharging:
		s.accumulateSolarEnergyLocked(measuredChargePowerW)
		// Attribute only the part of battery draw covered by net grid import.
		s.solarGridPowerW = min(max(activePowerW, 0), measuredChargePowerW)

		// Compensate for feedback loop: the battery's charge power is visible on
		// the P1 meter as consumption, so measured surplus is artificially low.
		// Real surplus = what P1 sees + what the battery is currently drawing.
		effectiveSurplus := surplus + measuredChargePowerW

		s.updateSolarSurplusEMALocked(effectiveSurplus, now)

		// One elapsed-time grace replaces both the stop debounce and the
		// immediate below-floor stop. The floor must not be bypassed by deadband.
		stopThreshold := max(float64(solarMinChargePowerW), float64(s.cfg.SolarMinSurplusW)/4)
		lowSurplus := s.solarSurplusEMA < stopThreshold
		if lowSurplus {
			if s.solarLowSurplusSince.IsZero() {
				s.solarLowSurplusSince = s.now()
			}
			if s.now().Sub(s.solarLowSurplusSince) >= solarLowSurplusGrace {
				slog.Info("solar charging: sustained insufficient surplus",
					"ema_w", s.solarSurplusEMA, "stop_threshold_w", stopThreshold,
					"measured_surplus_w", surplus, "charge_power_w", s.solarChargePower,
					"measured_battery_power_w", measuredChargePowerW, "effective_surplus_w", effectiveSurplus,
					"grace", solarLowSurplusGrace)
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonSurplusGone)
				return
			}
		} else {
			s.solarLowSurplusSince = time.Time{}
		}

		// Wait for battery to settle after start/adjustment before re-adjusting.
		// The battery takes ~3s to ramp to the target power; adjusting during
		// ramp-up causes a positive feedback spiral (overestimated effective surplus
		// → higher target → even higher next tick → overshoot → stop).
		sinceLastChange := s.now().Sub(s.lastPassiveRefresh)
		if sinceLastChange < 5*time.Second {
			return
		}

		// Bridge insufficient surplus at the floor, not the previous high target.
		targetPower := max(int(s.solarSurplusEMA), solarMinChargePowerW)
		if lowSurplus {
			targetPower = solarMinChargePowerW
		}
		targetPower = min(targetPower, s.cfg.ChargePowerW)

		diff := targetPower - s.solarChargePower
		if diff < 0 {
			diff = -diff
		}
		if diff > 50 || (lowSurplus && diff != 0) {
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
				s.solarControlFailed = true
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonControlFailure)
			} else {
				s.solarChargePower = targetPower
				s.lastPassiveRefresh = s.now()
			}
		} else {
			// Refresh passive mode to prevent timeout
			if !s.refreshPassiveModeLocked(ctx, -s.solarChargePower) {
				s.stopSolarChargingLocked(ctx, batterySOC, solarStopReasonControlFailure)
			}
		}

	// StateCharging, StateDischarging: managed by regular tick, ignore
	default:
		return
	}
}

func (s *Service) updateSolarSurplusEMALocked(surplus float64, now time.Time) {
	if s.solarEMALastSampleAt.IsZero() {
		s.solarSurplusEMA = surplus
		s.solarEMALastSampleAt = now
		return
	}
	elapsed := now.Sub(s.solarEMALastSampleAt)
	if elapsed > 0 {
		// Match the existing alpha at one second while making gaps converge by
		// the same continuous-time exponential rather than repeated callbacks.
		alpha := 1 - math.Pow(1-solarEMAAlpha, elapsed.Seconds())
		s.solarSurplusEMA += alpha * (surplus - s.solarSurplusEMA)
		s.solarEMALastSampleAt = now
	}
}

func (s *Service) handleSolarStatusFailure(ctx context.Context, telemetryErr error) {
	s.mu.Lock()
	now := s.now()
	if s.state != StateSolarCharging {
		s.solarSurplusSince = time.Time{}
		s.solarLastSampleAt = time.Time{}
		s.mu.Unlock()
		slog.Debug("solar tick: telemetry unavailable", "error", telemetryErr)
		return
	}

	if s.solarTelemetryFailureSince.IsZero() {
		s.solarTelemetryFailureSince = now
	}
	unavailableFor := now.Sub(s.solarTelemetryFailureSince)
	if unavailableFor < solarTelemetryFailureThreshold {
		s.mu.Unlock()
		slog.Warn("solar charging telemetry unavailable", "unavailable_for", unavailableFor, "error", telemetryErr)
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
	slog.Error("stopping solar charging after sustained telemetry failure",
		"unavailable_for", unavailableFor, "status_error", statusErr, "error", telemetryErr)
	s.stopSolarChargingLocked(ctx, endSOC, solarStopReasonTelemetryFailure)
	stopped := s.state == StateIdle
	s.mu.Unlock()
	if stopped {
		s.notifyError(ctx, "Solar charging stopped because telemetry is unavailable: "+telemetryErr.Error())
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
		l.Error("failed to start solar charging", "error", err, "link_down", errors.Is(err, marstek.ErrLinkDown))
		s.solarSurplusSince = time.Time{}
		s.solarLastSampleAt = time.Time{}
		s.batteryCooldownUntil = s.now().Add(batteryFailureCooldown(err))
		errMsg := batteryFailureMessage("Battery did not start solar charging", err)
		s.mu.Unlock()
		s.notifyError(ctx, errMsg)
		s.mu.Lock()
		return
	}

	s.state = StateSolarCharging
	s.currentTradeStart = s.now()
	s.currentTradeSOC = soc
	s.currentTradeLastSOC = soc
	s.currentTradePrices = nil
	s.snapshotSessionPricesLocked()
	s.lastPassiveRefresh = s.now()
	s.solarChargePower = powerW
	s.solarSurplusSince = time.Time{}
	s.solarLowSurplusSince = time.Time{}
	s.solarEnergyWs = 0
	s.solarGridPowerW = 0
	s.solarGridEnergyWs = 0
	s.solarGridCostEUR = decimal.Zero
	s.solarGridUnpricedWs = 0
	s.solarOpportunityCostEUR = decimal.Zero
	s.cacheBatteryTelemetryLocked(soc, measuredPowerW)
	s.solarOpportunityUnpricedWs = 0
	s.solarControlFailed = false
	s.solarLastUpdate = s.now()
	s.solarMeasuredChargePowerW = max(measuredPowerW, 0)
	s.solarSurplusEMA = 0
	s.solarEMALastSampleAt = time.Time{}
	s.solarTelemetryFailureSince = time.Time{}
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
	if !s.stopPending {
		s.pendingSolarStopReason = reason
	}
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	stopTime := s.now()
	s.accumulateSolarEnergyAtLocked(s.solarMeasuredChargePowerW, stopTime)
	duration := stopTime.Sub(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(s.solarEnergyWs / 3_600_000.0) // watt-seconds to kWh
	energyF, _ := energyKWh.Float64()

	// Adaptive cooldown: penalize micro-cycling.
	// Only "surplus gone" on a short session indicates marginal conditions; battery-full
	// and window-yield are legitimate and should not trigger backoff.
	cooldown := solarRestartCooldown
	if reason == solarStopReasonTelemetryFailure || reason == solarStopReasonControlFailure {
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
		Timestamp:          s.currentTradeStart,
		Action:             ActionSolarCharge,
		PriceEUR:           decimal.Zero,
		PowerW:             s.solarChargePower,
		DurationS:          int(duration.Seconds()),
		EnergyKWh:          energyKWh,
		GridEnergyKWh:      decimal.NewFromFloat(s.solarGridEnergyWs / 3_600_000),
		GridCostEUR:        s.solarGridCostEUR,
		GridUnpricedKWh:    decimal.NewFromFloat(s.solarGridUnpricedWs / 3_600_000),
		UnpricedKWh:        decimal.NewFromFloat(s.solarOpportunityUnpricedWs / 3_600_000),
		OpportunityCostEUR: s.solarOpportunityCostEUR,
		EnergyBasis:        measuredBatteryPowerEnergyBasis,
		StartSOC:           s.currentTradeSOC,
		EndSOC:             endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record solar trade", "error", err)
	}
	s.mu.Lock()

	s.solarSurplusSince = time.Time{}
	s.solarLastSampleAt = time.Time{}
	s.solarLowSurplusSince = time.Time{}
	s.solarChargePower = 0
	s.solarMeasuredChargePowerW = 0
	s.solarTelemetryFailureSince = time.Time{}
	s.solarEnergyWs = 0
	s.solarGridPowerW = 0
	s.solarControlFailed = false
	s.solarCooldownUntil = s.now().Add(cooldown)
	s.solarSurplusEMA = 0
	s.solarEMALastSampleAt = time.Time{}
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
		l.Error("failed to start charging", "error", err, "link_down", errors.Is(err, marstek.ErrLinkDown))
		s.batteryCooldownUntil = s.now().Add(batteryFailureCooldown(err))
		errMsg := batteryFailureMessage("Failed to start charging", err)
		linkDown := errors.Is(err, marstek.ErrLinkDown)
		s.mu.Unlock()
		if linkDown {
			s.notifyLinkDown(ctx, errMsg)
			s.tryRestartBridge(ctx)
		} else {
			s.notifyError(ctx, errMsg)
		}
		s.mu.Lock()
		return
	}

	s.state = StateCharging
	s.currentTradeStart = s.now()
	s.currentTradeSOC = soc
	s.currentTradeLastSOC = soc
	s.beginMeasuredTradeLocked(measuredPowerW)
	s.cacheBatteryTelemetryLocked(soc, measuredPowerW)
	s.observedChargePowerW = max(measuredPowerW, 0)
	_, _, priceKnown := s.sessionPriceAtLocked(s.currentTradeStart, s.currentTradeStart.Add(time.Nanosecond))
	if priceKnown {
		s.lastChargePrice = price // Track the known start price for profitability.
	}
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
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	stopTime := s.now()
	s.accumulateMeasuredTradeEnergyAtLocked(s.currentTradeLastPowerW, stopTime, true)
	duration := stopTime.Sub(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(s.currentTradeEnergyWs / 3_600_000)
	avgPrice, hasPricedEnergy := s.measuredTradePriceLocked()
	avgPriceF, _ := avgPrice.Float64()
	if hasPricedEnergy {
		// Informational only; discharge eligibility is based on the current plan.
		s.lastChargePrice = avgPrice
	}
	energyF, _ := energyKWh.Float64()

	l := slog.With(
		"action", "charge",
		"avg_price_eur_kwh", avgPriceF,
		"price_known", hasPricedEnergy,
		"start_soc", s.currentTradeSOC,
		"end_soc", endSOC,
		"duration", duration,
		"energy_kwh", energyF,
	)
	l.Info("stopping charge session")

	trade := Trade{
		Timestamp:   s.currentTradeStart,
		Action:      ActionCharge,
		PriceEUR:    avgPrice,
		PowerW:      s.cfg.ChargePowerW,
		DurationS:   int(duration.Seconds()),
		EnergyKWh:   energyKWh,
		UnpricedKWh: decimal.NewFromFloat(s.currentTradeUnpricedWs / 3_600_000),
		EnergyBasis: measuredBatteryPowerEnergyBasis,
		StartSOC:    s.currentTradeSOC,
		EndSOC:      endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record trade", "error", err)
	}
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, "Charging", energyF, avgPriceF, endSOC); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// startDischargingLocked begins a scheduled or manual discharge session. Caller must hold s.mu.
func (s *Service) startDischargingLocked(ctx context.Context, price decimal.Decimal, soc, powerW int, targetState State) {
	priceF, _ := price.Float64()
	lastChargeF, _ := s.lastChargePrice.Float64()
	l := slog.With("action", "discharge", "price_eur_kwh", priceF, "soc", soc, "power_w", powerW, "last_charge_price", lastChargeF, "target_state", targetState)
	l.Info("starting discharge session")

	// Release lock during network I/O
	s.mu.Unlock()
	err := s.battery.DischargeContext(ctx, powerW, s.cfg.PassiveModeTimeoutS)
	var measuredPowerW float64
	var idleErr error
	if err == nil {
		measuredPowerW, err = s.waitForBatteryPower(ctx, false, powerW)
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
		l.Error("failed to start discharging", "error", err, "link_down", errors.Is(err, marstek.ErrLinkDown))
		s.batteryCooldownUntil = s.now().Add(batteryFailureCooldown(err))
		errMsg := batteryFailureMessage("Failed to start discharging", err)
		linkDown := errors.Is(err, marstek.ErrLinkDown)
		s.mu.Unlock()
		if linkDown {
			s.notifyLinkDown(ctx, errMsg)
			s.tryRestartBridge(ctx)
		} else {
			s.notifyError(ctx, errMsg)
		}
		s.mu.Lock()
		return
	}

	s.state = targetState
	s.cacheBatteryTelemetryLocked(soc, measuredPowerW)
	s.currentTradeStart = s.now()
	s.currentTradeSOC = soc
	s.currentTradeLastSOC = soc
	s.currentTradePowerW = powerW
	s.beginMeasuredTradeLocked(measuredPowerW)
	s.lastPassiveRefresh = s.now()
	s.batteryCooldownUntil = time.Time{}
	if targetState == StateManualDischarging {
		s.manualOverrideUntil = s.now().Add(manualOverrideMaxDuration)
	} else {
		s.manualOverrideUntil = time.Time{}
	}

	l.Info("discharge session started", "state", s.state, "measured_battery_power_w", measuredPowerW)

	notificationAction := "Discharging"
	if targetState == StateManualDischarging {
		notificationAction = "Manual discharging"
	}
	// Release lock for notification
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeStart(ctx, notificationAction, priceF, soc); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// stopDischargingLocked ends a discharge session and records the trade. Caller must hold s.mu.
func (s *Service) stopDischargingLocked(ctx context.Context, endSOC int) {
	previousState := s.state
	tradePowerW := s.currentTradePowerW
	if !s.transitionToIdleLocked(ctx, endSOC) {
		return
	}
	stopTime := s.now()
	s.accumulateMeasuredTradeEnergyAtLocked(s.currentTradeLastPowerW, stopTime, false)
	duration := stopTime.Sub(s.currentTradeStart)
	energyKWh := decimal.NewFromFloat(s.currentTradeEnergyWs / 3_600_000)
	avgPrice, hasPricedEnergy := s.measuredTradePriceLocked()
	priceF, _ := avgPrice.Float64()
	energyF, _ := energyKWh.Float64()

	l := slog.With(
		"action", "discharge",
		"price_eur_kwh", priceF,
		"price_known", hasPricedEnergy,
		"start_soc", s.currentTradeSOC,
		"end_soc", endSOC,
		"duration", duration,
		"energy_kwh", energyF,
	)
	l.Info("stopping discharge session")

	trade := Trade{
		Timestamp:   s.currentTradeStart,
		Action:      ActionDischarge,
		PriceEUR:    avgPrice,
		PowerW:      tradePowerW,
		DurationS:   int(duration.Seconds()),
		EnergyKWh:   energyKWh,
		UnpricedKWh: decimal.NewFromFloat(s.currentTradeUnpricedWs / 3_600_000),
		EnergyBasis: measuredBatteryPowerEnergyBasis,
		StartSOC:    s.currentTradeSOC,
		EndSOC:      endSOC,
	}

	// Release lock for I/O
	s.mu.Unlock()
	if err := s.recorder.RecordTrade(trade); err != nil {
		l.Error("failed to record trade", "error", err)
	}
	s.mu.Lock()

	notificationAction := "Discharging"
	if previousState == StateManualDischarging {
		notificationAction = "Manual discharging"
	}
	s.manualOverrideUntil = time.Time{}
	s.currentTradePowerW = 0
	s.mu.Unlock()
	if s.telegramEnabled() {
		if err := s.telegram.SendTradeEnd(ctx, notificationAction, energyF, priceF, endSOC); err != nil {
			l.Warn("failed to send trade notification", "error", err)
		}
	}
	s.mu.Lock()
}

// transitionToIdleLocked returns to idle state. Caller must hold s.mu.
func (s *Service) transitionToIdleLocked(ctx context.Context, soc int) bool {
	s.stopPending = true
	if !s.lastStopAttempt.IsZero() && s.now().Sub(s.lastStopAttempt) < s.stopRetryDelay() {
		return false
	}
	s.lastStopAttempt = s.now()

	// Release lock during network I/O
	s.mu.Unlock()
	if err := s.idleBattery(ctx); err != nil {
		linkDown := errors.Is(err, marstek.ErrLinkDown)
		s.mu.Lock()
		s.lastStopLinkDown = linkDown
		slog.Error("failed to set idle mode; retaining active state for retry", "state", s.state, "error", err, "link_down", linkDown)
		s.mu.Unlock()
		stopMsg := batteryFailureMessage("Battery stop failed; forced operation may still be active", err)
		if linkDown {
			s.notifyLinkDown(ctx, stopMsg)
			s.tryRestartBridge(ctx)
		} else {
			s.notifyError(ctx, stopMsg)
		}
		s.mu.Lock()
		return false
	}
	s.mu.Lock()

	s.state = StateIdle
	s.stopPending = false
	s.lastStopAttempt = time.Time{}
	s.lastStopLinkDown = false
	if s.pendingPlan != nil && !s.automaticCycleCommittedLocked() {
		s.currentPlan = s.pendingPlan
		s.pendingPlan = nil
	}
	slog.Info("transitioned to idle", "soc", soc)
	return true
}

func (s *Service) stopRetryDelay() time.Duration {
	if s.batteryStopRetryDelay > 0 {
		return s.batteryStopRetryDelay
	}
	if s.lastStopLinkDown {
		// Right after a bridge reboot the link is expected back within a minute,
		// and the same tick's failed stop must not re-arm the long backoff.
		if !s.lastBridgeRestart.IsZero() && s.now().Sub(s.lastBridgeRestart) < bridgeRestartMinInterval {
			return batteryStopRetryInterval
		}
		return batteryLinkDownStopRetryInterval
	}
	return batteryStopRetryInterval
}

func (s *Service) retryStopping(ctx context.Context) bool {
	s.mu.Lock()
	if s.state != StateStopping && !s.stopPending {
		s.mu.Unlock()
		return false
	}
	throttled := !s.lastStopAttempt.IsZero() && s.now().Sub(s.lastStopAttempt) < s.stopRetryDelay()
	s.mu.Unlock()

	if throttled {
		// Even while a dead-link stop retry is throttled, sample the link once.
		// A second consecutive failure can restart the bridge, which clears
		// lastStopAttempt and makes this pending safety command retry immediately.
		s.checkLinkDuringSession(ctx)
		s.mu.RLock()
		recovered := s.lastStopAttempt.IsZero()
		s.mu.RUnlock()
		if !recovered {
			return true
		}
	}

	status, err := s.battery.GetBatteryStatusContext(ctx)
	if err != nil {
		slog.Warn("failed to read battery status while retrying stop", "error", err)
	}
	s.mu.Lock()
	endSOC := s.currentTradeLastSOC
	if err == nil {
		endSOC = status.SOC
	}

	if s.state != StateStopping && !s.stopPending {
		s.mu.Unlock()
		return true
	}
	switch s.state {
	case StateCharging:
		s.stopChargingLocked(ctx, endSOC)
	case StateDischarging, StateManualDischarging:
		s.stopDischargingLocked(ctx, endSOC)
	case StateSolarCharging:
		s.stopSolarChargingLocked(ctx, endSOC, s.pendingSolarStopReason)
	default:
		s.transitionToIdleLocked(ctx, endSOC)
	}
	s.mu.Unlock()
	return true
}

func (s *Service) stopBatteryOnShutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), batteryShutdownTimeout)
	defer cancel()

	for {
		s.mu.Lock()
		idle := s.state == StateIdle
		if !idle {
			s.stopPending = true
		}
		s.mu.Unlock()

		if idle {
			// Software idle is not evidence that the inverter accepted its last
			// command. Always assert idle once during shutdown.
			if err := s.idleBattery(shutdownCtx); err == nil {
				return nil
			} else {
				slog.Warn("failed to set battery idle during shutdown", "error", err)
			}
		} else {
			s.retryStopping(shutdownCtx)

			s.mu.RLock()
			idle = s.state == StateIdle
			s.mu.RUnlock()
			if idle {
				return nil
			}
		}

		timer := time.NewTimer(s.stopRetryDelay())
		select {
		case <-shutdownCtx.Done():
			timer.Stop()
			return fmt.Errorf("battery did not confirm idle before shutdown timeout")
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

// refreshPassiveModeLocked refreshes the passive mode command before timeout.
// It returns false when a required refresh fails. Caller must hold s.mu.
func (s *Service) refreshPassiveModeLocked(ctx context.Context, power int) bool {
	// Refresh if we're past 80% of the timeout period
	refreshThreshold := time.Duration(float64(s.cfg.PassiveModeTimeoutS)*0.8) * time.Second
	if s.now().Sub(s.lastPassiveRefresh) < refreshThreshold {
		return true
	}

	slog.Debug("refreshing passive mode", "power", power)

	// Release lock during network I/O. With ESPHome the refresh is a read-back that
	// only re-writes when the battery reports another mode or power.
	s.mu.Unlock()
	var err error
	if r, ok := s.battery.(PassiveModeRefresher); ok {
		err = r.RefreshPassiveModeContext(ctx, power, s.cfg.PassiveModeTimeoutS)
	} else {
		err = s.battery.SetPassiveModeContext(ctx, power, s.cfg.PassiveModeTimeoutS)
	}
	s.mu.Lock()

	if err != nil {
		slog.Error("failed to refresh passive mode", "error", err)
		s.batteryCooldownUntil = s.now().Add(batteryFailureCooldown(err))
		return false
	}
	s.lastPassiveRefresh = s.now()
	return true
}

// checkPriceFetch refreshes the two-day executable planning horizon.
func (s *Service) checkPriceFetch(ctx context.Context) {
	now := s.now()
	today := localMidnight(now)
	s.mu.Lock()
	haveToday := len(s.todayPrices) > 0 && localMidnight(s.todayPrices[0].Time).Equal(today)
	if !haveToday && len(s.tomorrowPrices) > 0 && localMidnight(s.tomorrowPrices[0].Time).Equal(today) {
		s.todayPrices = s.tomorrowPrices
		s.tomorrowPrices = nil
		s.lastMidnightSwap = now
		haveToday = true
	}
	if haveToday {
		s.refreshCurrentPlanLocked(now)
	}
	haveTomorrow := len(s.tomorrowPrices) > 0 && localMidnight(s.tomorrowPrices[0].Time).Equal(today.AddDate(0, 0, 1))
	s.mu.Unlock()
	if !haveToday {
		if err := s.fetchTodayPrices(ctx); err != nil {
			slog.Error("failed to fetch current day's prices; will retry", "error", err)
			s.notifyError(ctx, "Failed to fetch today's prices: "+err.Error())
		}
	}
	// Publication may be delayed or temporarily unavailable; retry beyond 13:00.
	if now.Hour() >= 13 && !haveTomorrow {
		if err := s.fetchTomorrowPrices(ctx); err != nil {
			slog.Warn("failed to fetch tomorrow's prices; will retry", "error", err)
			s.notifyError(ctx, "Failed to fetch tomorrow's prices: "+err.Error())
		}
	}
}

// futurePriceHorizonLocked deduplicates and orders all non-expired price slots
// from the loaded today and tomorrow calendars.
func (s *Service) futurePriceHorizonLocked(now time.Time) []nordpool.Price {
	byStart := make(map[time.Time]nordpool.Price, len(s.todayPrices)+len(s.tomorrowPrices))
	for _, prices := range [][]nordpool.Price{s.todayPrices, s.tomorrowPrices} {
		for _, price := range prices {
			if !now.Before(price.Time.Add(15 * time.Minute)) {
				continue
			}
			if _, duplicate := byStart[price.Time]; !duplicate {
				byStart[price.Time] = price
			}
		}
	}
	horizon := make([]nordpool.Price, 0, len(byStart))
	for _, price := range byStart {
		horizon = append(horizon, price)
	}
	sort.Slice(horizon, func(i, j int) bool {
		return horizon[i].Time.Before(horizon[j].Time)
	})
	return horizon
}

func (s *Service) currentPriceLocked(now time.Time) (decimal.Decimal, bool) {
	if price, ok := GetCurrentPrice(s.todayPrices, now); ok {
		return price, true
	}
	return GetCurrentPrice(s.tomorrowPrices, now)
}

func (s *Service) automaticCycleCommittedLocked() bool {
	if s.state == StateCharging || s.state == StateDischarging {
		return true
	}
	// A charged battery can be idle between the paired windows, including
	// across midnight. Keep its discharge commitment until that window ends.
	if s.currentPlan != nil {
		now := s.now()
		for _, cycle := range s.currentPlan.Cycles {
			if !now.Before(cycle.ChargeWindow.Start) && now.Before(cycle.DischargeWindow.End) {
				return true
			}
		}
	}
	return false
}

// refreshCurrentPlanLocked preserves a started cycle through its discharge end,
// including idle time between charging and discharging.
func (s *Service) refreshCurrentPlanLocked(now time.Time) *TradingPlan {
	// Retain today's charge prices: removing them also removes the paired
	// evening discharge, even when the battery is already full. This also
	// reconstructs today's discharge schedule after a service restart.
	plan := AnalyzePrices(s.futurePriceHorizonLocked(localMidnight(now)), s.analyzerConfig())
	if s.automaticCycleCommittedLocked() {
		s.pendingPlan = plan
		return plan
	}
	s.currentPlan = plan
	s.pendingPlan = nil
	return plan
}

// fetchTodayPrices fetches today's prices from NordPool.
func (s *Service) fetchTodayPrices(ctx context.Context) error {
	prices, err := s.nordpool.FetchTodayPrices(ctx)
	if err != nil {
		return err
	}

	now := s.now()
	s.mu.Lock()
	s.todayPrices = prices // full day remains available for price settlement
	plan := s.refreshCurrentPlanLocked(now)
	futurePrices := len(s.futurePriceHorizonLocked(localMidnight(now)))
	slotsTotal := len(s.todayPrices) + len(s.tomorrowPrices)
	s.mu.Unlock()

	l := slog.With(
		"day", "horizon",
		"slots_total", slotsTotal,
		"slots_analyzed", futurePrices,
	)
	l.Info(
		"fetched prices",
		"price_min_eur_kwh", plan.MinPrice,
		"price_max_eur_kwh", plan.MaxPrice,
	)

	// Log and notify the executable horizon, including the following day.
	s.logAndNotifyTradingPlan(ctx, l, plan, "horizon", slotsTotal, futurePrices)

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

	now := s.now()
	s.mu.Lock()
	s.tomorrowPrices = prices
	plan := s.refreshCurrentPlanLocked(now)
	futurePrices := len(s.futurePriceHorizonLocked(localMidnight(now)))
	slotsTotal := len(s.todayPrices) + len(s.tomorrowPrices)
	s.mu.Unlock()

	l := slog.With(
		"day", "horizon",
		"slots_total", slotsTotal,
		"slots_analyzed", futurePrices,
	)
	l.Info(
		"fetched prices",
		"price_min_eur_kwh", plan.MinPrice,
		"price_max_eur_kwh", plan.MaxPrice,
	)

	// Re-notify with the combined executable horizon once tomorrow publishes.
	s.logAndNotifyTradingPlan(ctx, l, plan, "horizon", slotsTotal, futurePrices)

	return nil
}

// logAndNotifyTradingPlan logs the trading plan and sends a Telegram notification.
func (s *Service) logAndNotifyTradingPlan(ctx context.Context, l *slog.Logger, plan *TradingPlan, day string, slotsTotal, slotsAnalyzed int) {
	// Calculate break-even spread needed to overcome efficiency loss
	efficiency := decimal.NewFromFloat(s.cfg.BatteryEfficiency)
	breakEvenDischarge := plan.MinPrice.Div(efficiency)
	minProfitableSpread := breakEvenDischarge.Sub(plan.MinPrice)

	if !plan.IsProfitable {
		l.Info(
			"no profitable charge→discharge sequence found",
			"reason", "window-averaged prices don't meet spread/efficiency requirements",
			"min_spread_for_efficiency", minProfitableSpread,
			"min_spread_configured", s.cfg.MinPriceSpread,
			"battery_efficiency", s.cfg.BatteryEfficiency,
		)
	} else {
		// Log each profitable cycle
		for i, c := range plan.Cycles {
			l.Info(
				"profitable cycle found",
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

	// Build notification data (convert decimal to float64 at Telegram API boundary).
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

// checkDailySummary sends at 23:59 and recovers a missed delivery on the next
// day using the recorder's completed-day history, never the new day's snapshot.
func (s *Service) checkDailySummary(ctx context.Context) {
	now := s.now()
	today := localMidnight(now)
	target := time.Time{}
	if now.Hour() == 23 && now.Minute() == 59 {
		target = today
	} else if now.After(today) {
		target = today.AddDate(0, 0, -1)
	}
	if target.IsZero() || localMidnight(s.lastDailySummary).Equal(target) {
		return
	}

	targetDate := target.In(s.loc).Format("2006-01-02")
	summary := DailySummary{Date: targetDate, Trades: []Trade{}}
	for _, day := range s.recorder.GetHistory().Days {
		if day.Date == targetDate {
			summary = day
			break
		}
	}
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
		Date:              target,
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
			slog.Warn("failed to send daily summary", "date", targetDate, "error", err)
			return
		}
	}
	s.lastDailySummary = target
}

// batteryFailureCooldown backs off much harder when the RS485 link is down:
// retrying every few minutes cannot help and only spams notifications.
func batteryFailureCooldown(err error) time.Duration {
	if errors.Is(err, marstek.ErrLinkDown) {
		return batteryLinkDownCooldown
	}
	return batteryControlFailureCooldown
}

// batteryFailureMessage leads with the actionable cause when the link is down.
func batteryFailureMessage(prefix string, err error) string {
	if errors.Is(err, marstek.ErrLinkDown) {
		return "Battery RS485 link is down — telemetry is frozen and control writes are dropped. Power-cycle the ESPHome dongle (or configure ESPHOME_RESTART_BUTTON so this happens automatically). (" + prefix + ": " + err.Error() + ")"
	}
	return prefix + ": " + err.Error()
}

// checkLinkDuringSession detects a frozen RS485 link while the battery is running.
// The battery keeps executing the last accepted command, so a dropped stop drains it
// silently. Must be called WITHOUT s.mu held: it performs network I/O.
func (s *Service) checkLinkDuringSession(ctx context.Context) {
	lc, ok := s.battery.(LinkChecker)
	if !ok {
		return
	}

	err := lc.CheckLink(ctx)
	switch {
	case err == nil:
		// Live telemetry proves the link is back (bridge rebooted, by us or by
		// hand), so a pending stop may retry at the normal cadence again.
		s.mu.Lock()
		s.linkDownSince = time.Time{}
		s.lastStopLinkDown = false
		s.mu.Unlock()
		return
	case !errors.Is(err, marstek.ErrLinkDown):
		slog.Debug("link check failed", "error", err)
		return
	}

	s.mu.Lock()
	if s.linkDownSince.IsZero() {
		s.linkDownSince = s.now()
	}
	downSince := s.linkDownSince
	state := s.state
	powerW := s.currentTradePowerW
	verb := "running"
	switch state {
	case StateCharging:
		verb = "charging"
		if powerW == 0 {
			powerW = s.cfg.ChargePowerW
		}
	case StateDischarging:
		verb = "discharging"
	case StateManualDischarging:
		verb = "manual discharging"
	case StateSolarCharging:
		verb = "solar charging"
		powerW = s.solarChargePower
	}
	s.mu.Unlock()

	logLinkFrozen := slog.Warn
	if downSince.Equal(s.now()) {
		logLinkFrozen = slog.Error
	}
	logLinkFrozen("battery RS485 link frozen during active session",
		"state", state, "power_w", powerW, "down_since", downSince, "error", err)

	msg := fmt.Sprintf(
		"Battery RS485 link is down: telemetry frozen while %s at %d W (%s). "+
			"The battery keeps running and cannot be stopped until the link is back — "+
			"power-cycle the ESPHome dongle now.", verb, powerW, err.Error(),
	)
	s.notifyLinkDown(ctx, msg)

	// Rebooting hardware needs one extra minute of confirmation: only restart from
	// the second consecutive tick that still reports the link down.
	if s.now().Sub(downSince) >= time.Minute {
		s.tryRestartBridge(ctx)
	}
}

// notifyLinkDown sends a link-down alert with its own rate limiter, so it is never
// swallowed by an unrelated recent error notification.
func (s *Service) notifyLinkDown(ctx context.Context, msg string) {
	if !s.telegramEnabled() {
		return
	}
	s.linkDownNotifyMu.Lock()
	defer s.linkDownNotifyMu.Unlock()
	now := s.now()
	elapsed := now.Sub(s.lastLinkDownNotify)
	if !s.lastLinkDownNotify.IsZero() && elapsed >= 0 && elapsed < linkDownNotifyInterval {
		slog.Debug("link down notification rate limited", "msg", msg)
		return
	}
	if err := s.telegram.SendError(ctx, msg); err != nil {
		slog.Warn("failed to send link down notification", "error", err)
		return
	}
	s.lastLinkDownNotify = now
}

// tryRestartBridge reboots the ESPHome bridge to recover a wedged RS485 link.
// Must be called WITHOUT s.mu held.
func (s *Service) tryRestartBridge(ctx context.Context) {
	r, ok := s.battery.(DeviceRestarter)
	if !ok || !r.RestartAvailable() {
		return
	}

	s.mu.Lock()
	if !s.lastBridgeRestart.IsZero() && s.now().Sub(s.lastBridgeRestart) < bridgeRestartMinInterval {
		s.mu.Unlock()
		slog.Debug("bridge restart rate limited", "last_restart", s.lastBridgeRestart)
		return
	}
	s.mu.Unlock()

	if err := r.RestartDevice(ctx); err != nil {
		// A failed POST rebooted nothing, so it must not consume the 10 minute
		// limiter: the next tick should be free to try again.
		slog.Error("failed to restart ESPHome bridge", "error", err)
		return
	}
	slog.Warn("restarted ESPHome bridge to recover RS485 link")

	// A pending stop should retry at the normal cadence once the bridge is back,
	// and the 30 minute link-down cooldown a failed start armed must not outlive
	// the recovery that just fixed its cause. The bridge needs ~30 s to reboot,
	// so hold starts for one tick instead of retrying into the outage.
	s.mu.Lock()
	s.lastBridgeRestart = s.now()
	s.lastStopLinkDown = false
	s.lastStopAttempt = time.Time{}
	s.batteryCooldownUntil = s.now().Add(bridgeRebootGrace)
	s.mu.Unlock()

	if s.telegramEnabled() {
		if err := s.telegram.SendMessage(ctx, "Restarting the ESPHome bridge to recover the frozen RS485 link; the pending battery command will be retried automatically."); err != nil {
			slog.Warn("failed to send bridge restart notification", "error", err)
		}
	}
}

// notifyError sends an error notification with rate limiting (max 1 per 15 minutes).
func (s *Service) notifyError(ctx context.Context, msg string) {
	if !s.telegramEnabled() {
		return
	}
	s.errorNotifyMu.Lock()
	defer s.errorNotifyMu.Unlock()
	now := s.now()
	// Rate limit: only send one error notification per 15 minutes
	elapsed := now.Sub(s.lastErrorNotify)
	if !s.lastErrorNotify.IsZero() && elapsed >= 0 && elapsed < 15*time.Minute {
		slog.Debug("error notification rate limited", "msg", msg)
		return
	}
	if err := s.telegram.SendError(ctx, msg); err != nil {
		slog.Warn("failed to send error notification", "error", err)
		return
	}
	s.lastErrorNotify = now
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

	var controlCommand string
	var controlArgs []string
	statusRequested := false
	for _, rawCommand := range commands {
		fields := strings.Fields(rawCommand)
		if len(fields) == 0 {
			continue
		}
		command := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
		switch command {
		case "/status":
			statusRequested = true
		case "/discharge", "/auto":
			controlCommand = command
			controlArgs = fields[1:]
		}
	}

	switch controlCommand {
	case "/discharge":
		s.handleManualDischargeCommand(ctx, controlArgs)
	case "/auto":
		s.handleAutoCommand(ctx)
	}
	if statusRequested {
		s.sendTelegramStatus(ctx)
	}
}

func (s *Service) handleManualDischargeCommand(ctx context.Context, args []string) {
	powerW := s.cfg.DischargePowerW
	if len(args) > 1 {
		s.sendTelegramCommandResponse(ctx, "Usage: <code>/discharge [watts]</code>")
		return
	}
	if len(args) == 1 {
		parsedPowerW, err := strconv.Atoi(args[0])
		if err != nil {
			s.sendTelegramCommandResponse(ctx, "Discharge power must be a whole number of watts.")
			return
		}
		powerW = parsedPowerW
	}
	if powerW < config.MinDischargePowerW || powerW > config.MaxDischargePowerW {
		s.sendTelegramCommandResponse(ctx, fmt.Sprintf("Discharge power must be between %d W and %d W.", config.MinDischargePowerW, config.MaxDischargePowerW))
		return
	}

	batStatus, err := s.battery.GetBatteryStatusContext(ctx)
	if err != nil {
		s.sendTelegramCommandResponse(ctx, "Manual discharge not started: battery status is unavailable.")
		return
	}
	minSOC := s.cfg.MinSOCPercent()

	now := s.now()
	s.mu.RLock()
	currentPrice, _ := s.currentPriceLocked(now)
	s.mu.RUnlock()

	s.mu.Lock()
	stoppedPreviousOperation := false
	switch s.state {
	case StateManualDischarging:
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Manual discharge is already active. Send <code>/auto</code> first.")
		return
	case StateStopping:
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Manual discharge not started: the battery is still stopping. Try again after <code>/status</code> shows idle.")
		return
	case StateCharging:
		s.stopChargingLocked(ctx, batStatus.SOC)
		stoppedPreviousOperation = true
	case StateDischarging:
		s.stopDischargingLocked(ctx, batStatus.SOC)
		stoppedPreviousOperation = true
	case StateSolarCharging:
		s.stopSolarChargingLocked(ctx, batStatus.SOC, solarStopReasonYieldWindow)
		stoppedPreviousOperation = true
	}
	if s.state != StateIdle {
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Manual discharge not started: the previous battery operation could not be stopped.")
		return
	}
	if now.Before(s.batteryCooldownUntil) {
		retryAt := s.batteryCooldownUntil.In(s.loc).Format("15:04 MST")
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Manual discharge not started: battery control is cooling down after a failure. Retry after "+retryAt+".")
		return
	}
	if stoppedPreviousOperation {
		s.mu.Unlock()
		batStatus, err = s.battery.GetBatteryStatusContext(ctx)
		s.mu.Lock()
		if err != nil {
			s.mu.Unlock()
			s.sendTelegramCommandResponse(ctx, "Manual discharge not started: fresh battery status is unavailable after stopping the previous operation.")
			return
		}
		if s.state != StateIdle {
			s.mu.Unlock()
			s.sendTelegramCommandResponse(ctx, "Manual discharge not started: battery state changed while status was refreshed.")
			return
		}
	}
	if batStatus.SOC <= minSOC {
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, fmt.Sprintf("Manual discharge not started: battery SOC is %d%% (minimum %d%%).", batStatus.SOC, minSOC))
		return
	}
	if !batStatus.DischargFlag {
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Manual discharge not started: battery discharging is disabled.")
		return
	}

	s.startDischargingLocked(ctx, currentPrice, batStatus.SOC, powerW, StateManualDischarging)
	started := s.state == StateManualDischarging
	stopPending := s.state == StateStopping
	overrideUntil := s.manualOverrideUntil
	s.mu.Unlock()
	if !started {
		if stopPending {
			s.sendTelegramCommandResponse(ctx, "Manual discharge failed to start, and the battery stop is not yet confirmed. The service will keep retrying.")
		} else {
			s.sendTelegramCommandResponse(ctx, "Manual discharge failed to start. The battery is idle.")
		}
		return
	}

	s.sendTelegramCommandResponse(ctx, fmt.Sprintf(
		"Manual discharge active at <b>%d W</b>. Safety stop: %d%% SOC or %s. Send <code>/auto</code> to stop and restore automatic trading.",
		powerW,
		minSOC,
		overrideUntil.In(s.loc).Format("15:04 MST"),
	))
}

func (s *Service) handleAutoCommand(ctx context.Context) {
	s.mu.Lock()
	switch s.state {
	case StateStopping:
		s.transitionToIdleLocked(ctx, 0)
		stopped := s.state == StateIdle
		s.mu.Unlock()
		if stopped {
			s.sendTelegramCommandResponse(ctx, "Battery stop confirmed. Automatic trading and solar control are active again.")
		} else {
			s.sendTelegramCommandResponse(ctx, "Automatic control requested, but the battery stop is not yet confirmed. The service will keep retrying.")
		}
		return
	case StateManualDischarging:
		// Continue below after obtaining current SOC without holding the lock.
	default:
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Automatic trading is already in control.")
		return
	}
	fallbackSOC := s.currentTradeLastSOC
	s.mu.Unlock()

	endSOC := fallbackSOC
	if batStatus, err := s.battery.GetBatteryStatusContext(ctx); err == nil {
		endSOC = batStatus.SOC
	}

	s.mu.Lock()
	if s.state != StateManualDischarging {
		s.mu.Unlock()
		s.sendTelegramCommandResponse(ctx, "Automatic control was requested, but the battery state changed before the stop command.")
		return
	}
	s.stopDischargingLocked(ctx, endSOC)
	stopped := s.state == StateIdle
	if !stopped {
		s.manualOverrideUntil = s.now()
	}
	s.mu.Unlock()

	if !stopped {
		s.sendTelegramCommandResponse(ctx, "Automatic control requested, but the battery stop is not yet confirmed. The service will keep retrying.")
		return
	}
	s.sendTelegramCommandResponse(ctx, "Manual discharge stopped. Automatic trading and solar control are active again.")
}

func (s *Service) sendTelegramCommandResponse(ctx context.Context, message string) {
	if err := s.telegram.SendMessage(ctx, message); err != nil {
		slog.Warn("failed to send Telegram command response", "error", err)
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

// ChargeReservationStatus exposes the current grid-charge reservation without
// leaking the internal planning type through the status API.
type ChargeReservationStatus struct {
	Deadline    time.Time    `json:"deadline,omitempty"`
	RequiredKWh float64      `json:"required_input_kwh"`
	ReservedKWh float64      `json:"reserved_input_kwh"`
	Feasible    bool         `json:"feasible"`
	Windows     []TimeWindow `json:"windows"`
}

// CurrentStatus contains all current state info.
type CurrentStatus struct {
	State             State                    `json:"state"`
	BatteryAvailable  bool                     `json:"battery_available"`
	BatterySOC        int                      `json:"battery_soc"`
	BatteryPowerW     float64                  `json:"battery_power_w"`
	BatteryObservedAt time.Time                `json:"battery_observed_at,omitempty"`
	CurrentPrice      float64                  `json:"current_price_eur_kwh"`
	CurrentPriceKnown bool                     `json:"current_price_known"`
	ChargeReservation *ChargeReservationStatus `json:"charge_reservation,omitempty"`
	NextAction        string                   `json:"next_action,omitempty"`
}

// GetCurrentStatus returns cached control-loop telemetry and current trading
// status. It intentionally performs no battery I/O so status reads cannot
// contend with safety-critical hardware control.
func (s *Service) GetCurrentStatus(ctx context.Context) CurrentStatus {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()
	status := CurrentStatus{
		State:             s.state,
		BatteryAvailable:  s.batteryTelemetryAvailable,
		BatterySOC:        s.batteryTelemetrySOC,
		BatteryPowerW:     s.batteryTelemetryPowerW,
		BatteryObservedAt: s.batteryTelemetryUpdatedAt,
	}
	if status.BatteryAvailable {
		reservation := s.chargeReservationLocked(now, status.BatterySOC)
		status.ChargeReservation = &ChargeReservationStatus{
			Deadline:    reservation.Deadline,
			RequiredKWh: reservation.RequiredKWh,
			ReservedKWh: reservation.ReservedKWh,
			Feasible:    reservation.Feasible,
			Windows:     reservation.Windows,
		}
	}

	// Keep a zero-valued valid price distinct from an unavailable price.
	if price, ok := s.currentPriceLocked(now); ok {
		status.CurrentPrice = price.InexactFloat64()
		status.CurrentPriceKnown = true
	}

	// Determine next action.
	if s.state == StateManualDischarging {
		status.NextAction = fmt.Sprintf(
			"manual override until %s; /auto to resume",
			s.manualOverrideUntil.In(s.loc).Format("15:04"),
		)
	} else if s.currentPlan != nil && s.currentPlan.IsProfitable {
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

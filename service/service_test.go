package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
	"github.com/foae/marstek-energy-trading/clients/telegram"
	"github.com/foae/marstek-energy-trading/internal/config"
)

// --- Mock implementations ---

// MockPriceProvider implements PriceProvider for testing.
type MockPriceProvider struct {
	TodayPrices    []nordpool.Price
	TomorrowPrices []nordpool.Price
	TodayErr       error
	TomorrowErr    error
}

func (m *MockPriceProvider) FetchTodayPrices(ctx context.Context) ([]nordpool.Price, error) {
	return m.TodayPrices, m.TodayErr
}

func (m *MockPriceProvider) FetchTomorrowPrices(ctx context.Context) ([]nordpool.Price, error) {
	return m.TomorrowPrices, m.TomorrowErr
}

// MockBattery implements BatteryController for testing.
type MockBattery struct {
	mu sync.Mutex

	// State
	SOC                 int
	ChargingFlag        bool
	DischargFlag        bool
	CurrentMode         string
	CurrentPower        int
	IgnorePowerCommands bool
	IdleAttempts        int
	IdleFailures        int
	RespectIdleContext  bool

	// Call tracking
	ConnectCalled     bool
	ChargeAttempts    int
	ChargeCalls       []ChargeCall
	ChargeDeadline    time.Time
	DischargeDeadline time.Time
	RefreshDeadline   time.Time
	DischargeCalls    []DischargeCall
	IdleCalls         int

	// Error injection
	ConnectErr    error
	GetStatusErr  error
	ChargeErr     error
	DischargeErr  error
	IdleErr       error
	PassiveErr    error
	StatusCalls   int
	ESCalls       int
	PowerCalls    int
	StatusHook    func()
	ChargeHook    func()
	DischargeHook func()
	IdleHook      func()
	PassiveHook   func()

	// Optional interfaces (LinkChecker, PassiveModeRefresher, DeviceRestarter)
	checkLinkErr     error
	checkLinkCalls   int
	passiveCalls     int
	refreshCalls     []int
	restartAvailable bool
	restartCalls     int
	restartErr       error
}

type ChargeCall struct {
	PowerW   int
	TimeoutS int
}

type DischargeCall struct {
	PowerW   int
	TimeoutS int
}

func NewMockBattery(soc int) *MockBattery {
	return &MockBattery{
		SOC:          soc,
		ChargingFlag: true,
		DischargFlag: true,
		CurrentMode:  "Auto",
	}
}

func (m *MockBattery) Connect() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConnectCalled = true
	return m.ConnectErr
}

func (m *MockBattery) Close() error {
	return nil
}

func (m *MockBattery) Discover() (*marstek.DeviceInfo, error) {
	return &marstek.DeviceInfo{Device: "MockBattery", IP: "192.168.1.100"}, nil
}

func (m *MockBattery) GetBatteryStatusContext(_ context.Context) (*marstek.BatteryStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StatusCalls++
	if m.StatusHook != nil {
		m.StatusHook()
	}
	if m.GetStatusErr != nil {
		return nil, m.GetStatusErr
	}
	return &marstek.BatteryStatus{
		SOC:          m.SOC,
		ChargingFlag: m.ChargingFlag,
		DischargFlag: m.DischargFlag,
	}, nil
}

func (m *MockBattery) GetBatteryPower(_ context.Context) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PowerCalls++
	if m.GetStatusErr != nil {
		return 0, m.GetStatusErr
	}
	return float64(m.CurrentPower), nil
}

func (m *MockBattery) GetESStatus(_ context.Context) (*marstek.ESStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ESCalls++
	if m.GetStatusErr != nil {
		return nil, m.GetStatusErr
	}
	return &marstek.ESStatus{BatterySOC: m.SOC, BatteryPower: float64(m.CurrentPower)}, nil
}

func (m *MockBattery) ChargeContext(ctx context.Context, powerW int, timeoutS int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ChargeAttempts++
	if deadline, ok := ctx.Deadline(); ok {
		m.ChargeDeadline = deadline
	}
	if m.ChargeHook != nil {
		m.ChargeHook()
	}
	if m.ChargeErr != nil {
		return m.ChargeErr
	}
	m.ChargeCalls = append(m.ChargeCalls, ChargeCall{powerW, timeoutS})
	m.CurrentMode = "Passive"
	if !m.IgnorePowerCommands {
		m.CurrentPower = powerW
	}
	return nil
}

func (m *MockBattery) DischargeContext(ctx context.Context, powerW int, timeoutS int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		m.DischargeDeadline = deadline
	}
	if m.DischargeHook != nil {
		m.DischargeHook()
	}
	if m.DischargeErr != nil {
		return m.DischargeErr
	}
	m.DischargeCalls = append(m.DischargeCalls, DischargeCall{powerW, timeoutS})
	m.CurrentMode = "Passive"
	if !m.IgnorePowerCommands {
		m.CurrentPower = -powerW
	}
	return nil
}

func (m *MockBattery) SetPassiveModeContext(_ context.Context, power int, cdTime int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passiveCalls++
	return m.applyPassiveLocked(power)
}

// RefreshPassiveModeContext implements PassiveModeRefresher; it records the power
// separately but applies the same state changes as SetPassiveModeContext.
func (m *MockBattery) RefreshPassiveModeContext(ctx context.Context, power int, cdTime int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		m.RefreshDeadline = deadline
	}
	m.refreshCalls = append(m.refreshCalls, power)
	return m.applyPassiveLocked(power)
}

func (m *MockBattery) applyPassiveLocked(power int) error {
	if m.PassiveHook != nil {
		m.PassiveHook()
	}
	if m.PassiveErr != nil {
		return m.PassiveErr
	}
	m.CurrentMode = "Passive"
	m.CurrentPower = -power
	return nil
}

// CheckLink implements LinkChecker.
func (m *MockBattery) CheckLink(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkLinkCalls++
	return m.checkLinkErr
}

// RestartAvailable implements DeviceRestarter; disabled unless a test opts in.
func (m *MockBattery) RestartAvailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartAvailable
}

// RestartDevice implements DeviceRestarter.
func (m *MockBattery) RestartDevice(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restartCalls++
	return m.restartErr
}

func (m *MockBattery) IdleContext(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IdleAttempts++
	if m.IdleHook != nil {
		m.IdleHook()
	}
	if m.RespectIdleContext && ctx.Err() != nil {
		return ctx.Err()
	}
	if m.IdleFailures > 0 {
		m.IdleFailures--
		return errors.New("idle failed")
	}
	if m.IdleErr != nil {
		return m.IdleErr
	}
	m.IdleCalls++
	m.CurrentMode = "Auto"
	m.CurrentPower = 0
	return nil
}

// MockNotifier implements Notifier for testing.
type MockNotifier struct {
	mu sync.Mutex

	Messages          []string
	StartupCalls      int
	TradeStartCalls   []TradeStartCall
	TradeEndCalls     []TradeEndCall
	ErrorCalls        []string
	ErrorErr          error
	PollErr           error
	DailySummaryCalls []telegram.DailySummaryData
	DailySummaryErr   error
	StatusCalls       []telegram.StatusData
	TradingPlanCalls  []telegram.TradingPlanData
	Commands          []string
}

type TradeStartCall struct {
	Action string
	Price  float64
	SOC    int
}

type TradeEndCall struct {
	Action    string
	EnergyKWh float64
	AvgPrice  float64
	EndSOC    int
}

func (m *MockNotifier) Enabled() bool { return true }

func (m *MockNotifier) SendMessage(ctx context.Context, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Messages = append(m.Messages, text)
	return nil
}

func (m *MockNotifier) SendStartup(ctx context.Context, serviceName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartupCalls++
	return nil
}

func (m *MockNotifier) SendTradeStart(ctx context.Context, action string, price float64, soc int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TradeStartCalls = append(m.TradeStartCalls, TradeStartCall{action, price, soc})
	return nil
}

func (m *MockNotifier) SendTradeEnd(ctx context.Context, action string, energyKWh float64, avgPrice float64, endSOC int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TradeEndCalls = append(m.TradeEndCalls, TradeEndCall{action, energyKWh, avgPrice, endSOC})
	return nil
}

func (m *MockNotifier) SendError(ctx context.Context, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ErrorCalls = append(m.ErrorCalls, msg)
	return m.ErrorErr
}

func (m *MockNotifier) SendStatus(ctx context.Context, data telegram.StatusData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StatusCalls = append(m.StatusCalls, data)
	return nil
}

func (m *MockNotifier) SendTradingPlan(ctx context.Context, data telegram.TradingPlanData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TradingPlanCalls = append(m.TradingPlanCalls, data)
	return nil
}

func (m *MockNotifier) SendDailySummaryFull(ctx context.Context, data telegram.DailySummaryData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DailySummaryCalls = append(m.DailySummaryCalls, data)
	return m.DailySummaryErr
}

func (m *MockNotifier) PollCommands(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmds := m.Commands
	m.Commands = nil
	return cmds, m.PollErr
}

// MockMeterReader implements MeterReader for testing.
type MockMeterReader struct {
	mu               sync.Mutex
	enabled          bool
	ActivePowerW     float64
	ActivePowerCalls int
	ActivePowerErr   error
	ActivePowerHook  func()
}

func NewMockMeter(enabled bool, activePowerW float64) *MockMeterReader {
	return &MockMeterReader{
		enabled:      enabled,
		ActivePowerW: activePowerW,
	}
}

func (m *MockMeterReader) Enabled() bool {
	return m.enabled
}

func (m *MockMeterReader) GetActivePowerW() (float64, error) {
	m.mu.Lock()
	m.ActivePowerCalls++
	powerW, err, hook := m.ActivePowerW, m.ActivePowerErr, m.ActivePowerHook
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return powerW, err
}

func (m *MockMeterReader) SetActivePowerW(w float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ActivePowerW = w
}

// --- Test helpers ---

func testConfig() *config.Config {
	return &config.Config{
		ServiceName:         "test-trader",
		LogLevel:            "debug",
		MinPriceSpread:      0.05,
		BatteryEfficiency:   0.90,
		BatteryCapacityKWh:  5.12,
		BatteryMinSOC:       0.11,
		MaxCyclesPerDay:     2,
		ChargePowerW:        2500,
		DischargePowerW:     2500,
		PassiveModeTimeoutS: 300,
		SolarMinSurplusW:    100,
		TZ:                  "UTC",
	}
}

// testConfigSmallBattery returns a config with smaller battery for tests with fewer price slots.
func testConfigSmallBattery() *config.Config {
	return &config.Config{
		ServiceName:         "test-trader",
		LogLevel:            "debug",
		MinPriceSpread:      0.05,
		BatteryEfficiency:   0.90,
		BatteryCapacityKWh:  0.5, // Small battery = 1 slot window
		BatteryMinSOC:       0.11,
		MaxCyclesPerDay:     2,
		ChargePowerW:        2000,
		DischargePowerW:     2000,
		PassiveModeTimeoutS: 300,
		SolarMinSurplusW:    100,
		TZ:                  "UTC",
	}
}

// newTestService creates a Service configured for testing with the given mocks and clock.
func newTestService(cfg *config.Config, battery *MockBattery, prices []nordpool.Price, clockTime time.Time) *Service {
	recorder := NewRecorder("", cfg.BatteryEfficiency, time.UTC)
	svc := &Service{
		cfg:         cfg,
		nordpool:    &MockPriceProvider{TodayPrices: prices},
		battery:     battery,
		recorder:    recorder,
		state:       StateIdle,
		loc:         time.UTC,
		todayPrices: prices,
		nowFunc:     func() time.Time { return clockTime },
	}
	// Use the service's analyzerConfig method to create the plan
	svc.currentPlan = AnalyzePrices(prices, svc.analyzerConfig())
	return svc
}

func setReservedChargePlan(svc *Service, start time.Time, chargePrice decimal.Decimal) {
	svc.currentPlan = &TradingPlan{
		IsProfitable:  true,
		ChargeWindows: []TimeWindow{{Start: start, End: start.Add(time.Hour), Price: chargePrice}},
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: start, End: start.Add(time.Hour), Price: chargePrice},
			DischargeWindow: TimeWindow{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour), Price: decimal.NewFromFloat(.40)},
		}},
	}
}

// --- Integration tests that call actual tick() ---

func TestTick_ChargeInLowPriceWindow(t *testing.T) {
	// Scenario: Battery at 50% SOC, current time is in charge window (low prices)
	// Expected: tick() should start charging
	// Using small battery for shorter window sizes

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window (low price)
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window (high price)
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	clockTime := baseTime // 00:00 - in charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	// Verify plan detected windows
	if len(svc.currentPlan.ChargeWindows) == 0 {
		t.Fatal("expected charge windows to be detected")
	}
	if !svc.currentPlan.IsProfitable {
		t.Fatal("expected plan to be profitable")
	}

	// Call actual tick()
	ctx := context.Background()
	svc.tick(ctx)

	// Verify state changed to charging
	if svc.state != StateCharging {
		t.Errorf("expected state=charging, got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 1 {
		t.Errorf("expected 1 charge call, got %d", len(mockBattery.ChargeCalls))
	}
	if mockBattery.ChargeCalls[0].PowerW != cfg.ChargePowerW {
		t.Errorf("expected charge power=%d, got %d", cfg.ChargePowerW, mockBattery.ChargeCalls[0].PowerW)
	}
	// Verify lastChargePrice was set
	expectedPrice := decimal.NewFromFloat(0.05)
	if !svc.lastChargePrice.Equal(expectedPrice) {
		t.Errorf("expected lastChargePrice=%s, got %s", expectedPrice, svc.lastChargePrice)
	}
}

func TestTick_ChargeFailureCooldownDependsOnLinkDown(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		cooldown time.Duration
	}{
		{
			name:     "link down",
			err:      fmt.Errorf("set charge mode: %w", marstek.ErrLinkDown),
			cooldown: batteryLinkDownCooldown,
		},
		{
			name:     "ordinary error",
			err:      errors.New("boom"),
			cooldown: batteryControlFailureCooldown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
			prices := makePrices(baseTime, 0.05, 0.15, 0.25, 0.10)
			mockBattery := NewMockBattery(50)
			mockBattery.ChargeErr = tt.err
			svc := newTestService(testConfigSmallBattery(), mockBattery, prices, baseTime)

			svc.tick(context.Background())

			if !svc.batteryCooldownUntil.Equal(baseTime.Add(tt.cooldown)) {
				t.Errorf("cooldown = %s, want %s", svc.batteryCooldownUntil, baseTime.Add(tt.cooldown))
			}
		})
	}
}

func TestTick_ChargeVerificationFailureSetsCooldown(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.15, 0.25, 0.10)
	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	mockBattery.IgnorePowerCommands = true
	svc := newTestService(cfg, mockBattery, prices, baseTime)
	svc.batteryVerificationTimeout = 10 * time.Millisecond
	svc.batteryVerificationInterval = time.Millisecond

	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Errorf("state = %s, want idle after power verification failure", svc.state)
	}
	if mockBattery.IdleCalls != 1 {
		t.Errorf("idle calls = %d, want 1 after power verification failure", mockBattery.IdleCalls)
	}
	if !svc.batteryCooldownUntil.Equal(baseTime.Add(batteryControlFailureCooldown)) {
		t.Errorf("cooldown = %s, want %s", svc.batteryCooldownUntil, baseTime.Add(batteryControlFailureCooldown))
	}

	svc.tick(context.Background())
	if len(mockBattery.ChargeCalls) != 1 {
		t.Errorf("charge calls = %d, want no retry during cooldown", len(mockBattery.ChargeCalls))
	}
}

func TestWaitForBatteryPower_ScalesThresholdForLowPowerCharge(t *testing.T) {
	mockBattery := NewMockBattery(50)
	mockBattery.CurrentPower = 40
	svc := newTestService(testConfigSmallBattery(), mockBattery, nil, time.Now())
	svc.batteryVerificationTimeout = 10 * time.Millisecond
	svc.batteryVerificationInterval = time.Millisecond

	power, err := svc.waitForBatteryPower(context.Background(), true, 75)
	if err != nil {
		t.Fatalf("waitForBatteryPower() error = %v", err)
	}
	if power != 40 {
		t.Errorf("power = %v, want 40", power)
	}
}

func TestTick_ChargeVerificationCleanupFailureRetriesIdle(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.15, 0.25, 0.10)
	mockBattery := NewMockBattery(50)
	mockBattery.IgnorePowerCommands = true
	mockBattery.IdleErr = errors.New("idle failed")
	svc := newTestService(testConfigSmallBattery(), mockBattery, prices, baseTime)
	svc.batteryVerificationTimeout = 10 * time.Millisecond
	svc.batteryVerificationInterval = time.Millisecond

	svc.tick(context.Background())
	if svc.state != StateStopping {
		t.Fatalf("state = %s, want stopping after cleanup failure", svc.state)
	}

	mockBattery.IdleErr = nil
	svc.nowFunc = func() time.Time { return baseTime.Add(batteryStopRetryInterval) }
	svc.tick(context.Background())
	if svc.state != StateIdle {
		t.Errorf("state = %s, want idle after cleanup retry", svc.state)
	}
}

func TestTick_StateStoppingRetriesBeforeStatusRead(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	mockBattery := NewMockBattery(50)
	mockBattery.GetStatusErr = errors.New("status unavailable")
	svc := newTestService(testConfigSmallBattery(), mockBattery, nil, baseTime)
	svc.state = StateStopping

	svc.tick(context.Background())
	if svc.state != StateIdle {
		t.Errorf("state = %s, want idle after retry independent of status", svc.state)
	}
	if mockBattery.IdleCalls != 1 {
		t.Errorf("idle calls = %d, want 1", mockBattery.IdleCalls)
	}
}

func TestStopBatteryOnShutdown_Retries(t *testing.T) {
	mockBattery := NewMockBattery(50)
	mockBattery.IdleFailures = 1
	svc := newTestService(testConfigSmallBattery(), mockBattery, nil, time.Now())
	svc.batteryStopRetryDelay = time.Millisecond

	if err := svc.stopBatteryOnShutdown(); err != nil {
		t.Fatalf("stopBatteryOnShutdown() error = %v", err)
	}
	if mockBattery.IdleAttempts != 2 {
		t.Errorf("idle attempts = %d, want 2", mockBattery.IdleAttempts)
	}
}

func TestStartReturnsShutdownStopFailure(t *testing.T) {
	mockBattery := NewMockBattery(50)
	mockBattery.IdleErr = errors.New("idle unavailable")
	svc := newTestService(testConfigSmallBattery(), mockBattery, nil, time.Now())
	svc.nordpool = &MockPriceProvider{}
	svc.batteryShutdownTimeout = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "stop battery during shutdown") {
		t.Fatalf("Start() error = %v, want shutdown stop failure", err)
	}
}

func TestShutdownReportsPersistenceFailuresIndependently(t *testing.T) {
	for _, tt := range []struct {
		name            string
		dirtyTrades     bool
		dirtyRetirement bool
		want            []string
		dontWant        []string
	}{
		{name: "trade history", dirtyTrades: true, want: []string{"flush trade history"}, dontWant: []string{"flush discharge retirements"}},
		{name: "discharge retirements", dirtyRetirement: true, want: []string{"flush discharge retirements"}, dontWant: []string{"flush trade history"}},
		{name: "both", dirtyTrades: true, dirtyRetirement: true, want: []string{"flush trade history", "flush discharge retirements"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
			blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
				t.Fatalf("create blocked data path: %v", err)
			}
			svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
			svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)
			if tt.dirtyTrades {
				err := svc.recorder.RecordTrade(Trade{Timestamp: now, Action: ActionCharge, EnergyKWh: decimal.NewFromInt(1)})
				if err == nil {
					t.Fatal("RecordTrade() error = nil, want blocked data path failure")
				}
			}
			if tt.dirtyRetirement {
				svc.retiredDischargeWindows = []TimeWindow{{Start: now, End: now.Add(time.Hour)}}
				svc.retiredDischargeWindowsDirty = true
			}

			err := svc.shutdown()
			if err == nil {
				t.Fatal("shutdown() error = nil, want persistence failure")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("shutdown() error = %q, want %q", err, want)
				}
			}
			for _, dontWant := range tt.dontWant {
				if strings.Contains(err.Error(), dontWant) {
					t.Errorf("shutdown() error = %q, do not want %q", err, dontWant)
				}
			}
		})
	}
}

func TestBatteryFailureMessageDoesNotClaimAmbiguousWritesWereDropped(t *testing.T) {
	message := batteryFailureMessage("stop failed", marstek.ErrLinkDown)
	if strings.Contains(message, "writes are dropped") || !strings.Contains(message, "may not reach") || !strings.Contains(message, "remain active") {
		t.Fatalf("link-down message = %q, want explicit ambiguous control outcome", message)
	}
}

func TestIdleBattery_RetriesWithSafetyContextAfterCancellation(t *testing.T) {
	mockBattery := NewMockBattery(50)
	mockBattery.RespectIdleContext = true
	svc := newTestService(testConfigSmallBattery(), mockBattery, nil, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.idleBattery(ctx); err != nil {
		t.Fatalf("idleBattery() error = %v", err)
	}
	if mockBattery.IdleAttempts != 2 {
		t.Errorf("idle attempts = %d, want cancelled attempt plus safety retry", mockBattery.IdleAttempts)
	}
}

func TestTick_DischargeInHighPriceWindow(t *testing.T) {
	// Scenario: Battery at 80% SOC, current time is in discharge window (high prices)
	// Expected: tick() should start discharging
	// Using small battery for shorter window sizes

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window (low price)
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window (high price)
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	clockTime := baseTime.Add(2 * 15 * time.Minute) // Slot 2 - in discharge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.lastChargePrice = decimal.NewFromFloat(0.05) // Simulate previous charge

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateDischarging {
		t.Errorf("expected state=discharging, got %s", svc.state)
	}
	if len(mockBattery.DischargeCalls) != 1 {
		t.Errorf("expected 1 discharge call, got %d", len(mockBattery.DischargeCalls))
	}
}

func TestTick_NoActionOutsideWindows(t *testing.T) {
	// Scenario: Battery at 60% SOC, current time is between windows
	// Expected: tick() should keep state idle
	// Using small battery for 1-slot windows to clearly define window boundaries

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window (lowest)
		0.10, // slot 1 - middle (outside windows)
		0.25, // slot 2 - discharge window (highest)
		0.12, // slot 3 - middle (outside windows)
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(60)
	clockTime := baseTime.Add(1 * 15 * time.Minute) // Slot 1 - between charge and discharge windows

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle, got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 0 {
		t.Errorf("expected no charge calls, got %d", len(mockBattery.ChargeCalls))
	}
	if len(mockBattery.DischargeCalls) != 0 {
		t.Errorf("expected no discharge calls, got %d", len(mockBattery.DischargeCalls))
	}
}

func TestTick_NoChargeWhenBatteryFull(t *testing.T) {
	// Scenario: Battery at 100% SOC, in charge window
	// Expected: tick() should not charge

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(100) // Full battery
	clockTime := baseTime              // In charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle (battery full), got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 0 {
		t.Errorf("expected no charge calls when battery is full, got %d", len(mockBattery.ChargeCalls))
	}
}

func TestTick_NoDischargeWhenBatteryLow(t *testing.T) {
	// Scenario: Battery at 11% SOC (min threshold), in discharge window
	// Expected: tick() should not discharge

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(11)               // At minimum
	clockTime := baseTime.Add(2 * 15 * time.Minute) // In discharge window
	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.lastChargePrice = decimal.NewFromFloat(0.05)

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle (battery at min), got %s", svc.state)
	}
	if len(mockBattery.DischargeCalls) != 0 {
		t.Errorf("expected no discharge calls when battery at min, got %d", len(mockBattery.DischargeCalls))
	}
}

func TestTick_DischargeRegardlessOfProfitability(t *testing.T) {
	// Scenario: Last charge was at high price, current discharge price is below breakeven
	// Expected: tick() should still discharge (profitability gate removed — any revenue is better than none)
	// Using small battery so windows fit in 4 slots

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	clockTime := baseTime.Add(2 * 15 * time.Minute) // In discharge window (price=0.25)

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	// Set last charge price high so breakeven (0.25/0.90 = 0.278) exceeds discharge price (0.25)
	// Previously this would block discharge; now it should still discharge
	svc.lastChargePrice = decimal.NewFromFloat(0.25)

	ctx := context.Background()
	svc.tick(ctx)

	// Should discharge even though price is at breakeven
	if svc.state != StateDischarging {
		t.Errorf("expected state=discharging (always discharge in window), got %s", svc.state)
	}
	if len(mockBattery.DischargeCalls) != 1 {
		t.Errorf("expected 1 discharge call, got %d", len(mockBattery.DischargeCalls))
	}
}

func TestTick_StopChargingWhenWindowEnds(t *testing.T) {
	// Scenario: Battery is charging, time moves outside charge window
	// Expected: tick() should stop charging and return to idle

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, 0.06, // Charge window - slots 0-1
		0.15, 0.20, // Outside window
	)

	cfg := testConfig()
	mockBattery := NewMockBattery(70)
	clockTime := baseTime.Add(2 * 15 * time.Minute) // Outside charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.state = StateCharging // Already charging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 50

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after leaving window, got %s", svc.state)
	}
	if mockBattery.IdleCalls != 1 {
		t.Errorf("expected 1 idle call, got %d", mockBattery.IdleCalls)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one recorded charge trade, got %+v", history.Days)
	}
}

func TestTick_StopChargingFailureRetainsSessionUntilRetry(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.15, 0.20)
	mockBattery := NewMockBattery(70)
	mockBattery.IdleErr = errors.New("idle failed")
	svc := newTestService(testConfig(), mockBattery, prices, baseTime.Add(30*time.Minute))
	svc.state = StateCharging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 50

	svc.tick(context.Background())
	if svc.state != StateCharging {
		t.Errorf("state = %s, want charging while stop is unconfirmed", svc.state)
	}
	if history := svc.recorder.GetHistory(); len(history.Days) != 0 {
		t.Fatalf("recorded trade before stop confirmation: %+v", history.Days)
	}

	mockBattery.IdleErr = nil
	svc.nowFunc = func() time.Time { return baseTime.Add(30*time.Minute + batteryStopRetryInterval) }
	svc.tick(context.Background())
	if svc.state != StateIdle {
		t.Errorf("state = %s, want idle after successful retry", svc.state)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("trades after successful retry = %+v, want exactly one", history.Days)
	}
}

func TestTick_StopRetryThrottleSkipsBatteryCommand(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.15, 0.20)
	mockBattery := NewMockBattery(70)
	svc := newTestService(testConfig(), mockBattery, prices, baseTime.Add(30*time.Minute))
	svc.state = StateCharging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 50
	svc.lastStopAttempt = svc.now().Add(-time.Second)

	svc.tick(context.Background())
	if mockBattery.IdleAttempts != 0 {
		t.Errorf("idle attempts = %d, want none during retry throttle", mockBattery.IdleAttempts)
	}
	if svc.state != StateCharging {
		t.Errorf("state = %s, want charging until stop is confirmed", svc.state)
	}
}

func TestTick_StopChargingWhenBatteryFull(t *testing.T) {
	// Scenario: Battery reaches 100% while charging
	// Expected: tick() should stop charging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(100) // Just became full
	clockTime := baseTime              // Still in charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.state = StateCharging
	svc.currentTradeStart = baseTime.Add(-30 * time.Minute)
	svc.currentTradeSOC = 80

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after battery full, got %s", svc.state)
	}
}

func TestTick_StopDischargingWhenBatteryLow(t *testing.T) {
	// Scenario: Battery reaches 11% while discharging
	// Expected: tick() should stop discharging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(11)               // Just hit minimum
	clockTime := baseTime.Add(2 * 15 * time.Minute) // In discharge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.state = StateDischarging
	svc.currentTradeStart = baseTime.Add(-30 * time.Minute)
	svc.currentTradeSOC = 50

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after battery low, got %s", svc.state)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 2 || len(history.Days[0].Trades) != 1 || len(history.Days[1].Trades) != 1 {
		t.Fatalf("expected discharge crossing midnight to be split across two days, got %+v", history.Days)
	}
}

func TestTick_NotProfitablePlan(t *testing.T) {
	// Scenario: Prices have very small spread, plan is not profitable
	// Expected: tick() should stay idle regardless of windows

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.11, 0.12, 0.11) // Very small spread

	cfg := testConfigSmallBattery()
	cfg.MinPriceSpread = 0.05 // Requires 5 cents of expected profit after efficiency loss.

	mockBattery := NewMockBattery(50)
	clockTime := baseTime

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	if svc.currentPlan.IsProfitable {
		t.Fatalf("expected plan to be not profitable (spread=%s)", svc.currentPlan.Spread)
	}

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle when plan not profitable, got %s", svc.state)
	}
}

func TestLogAndNotifyTradingPlanIncludesHorizonDatesAndThreshold(t *testing.T) {
	notifier := &MockNotifier{}
	cfg := testConfig()
	svc := &Service{cfg: cfg, telegram: notifier}
	start := time.Date(2026, 9, 7, 1, 45, 0, 0, time.UTC)
	plan := &TradingPlan{
		Date:         start.AddDate(0, 0, -1),
		MinPrice:     decimal.NewFromFloat(.10),
		MaxPrice:     decimal.NewFromFloat(.40),
		IsProfitable: true,
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: start, End: start.Add(2 * time.Hour), Price: decimal.NewFromFloat(.10)},
			DischargeWindow: TimeWindow{Start: start.Add(5 * time.Hour), End: start.Add(7 * time.Hour), Price: decimal.NewFromFloat(.40)},
			Profit:          decimal.NewFromFloat(.26),
		}},
	}

	svc.logAndNotifyTradingPlan(context.Background(), slog.Default(), plan, "horizon", 192, 192, false)
	if len(notifier.TradingPlanCalls) != 1 {
		t.Fatalf("trading plan notifications = %d, want 1", len(notifier.TradingPlanCalls))
	}
	data := notifier.TradingPlanCalls[0]
	if data.MinExpectedProfit != .05 || data.BatteryEfficiency != .90 {
		t.Fatalf("threshold data = %+v", data)
	}
	if got := data.Cycles[0].ChargeStart; got != "Mon 07 Sep 01:45" {
		t.Fatalf("charge start = %q, want dated horizon timestamp", got)
	}

	dischargeOnly := &TradingPlan{
		Date:             plan.Date,
		MinPrice:         plan.MinPrice,
		MaxPrice:         plan.MaxPrice,
		IsProfitable:     true,
		DischargeOnly:    true,
		DischargeWindows: []TimeWindow{plan.Cycles[0].DischargeWindow},
	}
	svc.logAndNotifyTradingPlan(context.Background(), slog.Default(), dischargeOnly, "horizon", 192, 192, true)
	if len(notifier.TradingPlanCalls) != 2 {
		t.Fatalf("trading plan notifications = %d, want 2", len(notifier.TradingPlanCalls))
	}
	data = notifier.TradingPlanCalls[1]
	if !data.PlanRetained || !data.DischargeOnly || data.DischargeStart != "Mon 07 Sep 06:45" {
		t.Fatalf("discharge-only notification = %+v", data)
	}
}

func TestTick_ChargingFlagDisabled(t *testing.T) {
	// Scenario: Battery charging flag is disabled
	// Expected: tick() should not charge even in charge window

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(50)
	mockBattery.ChargingFlag = false // Disabled!
	clockTime := baseTime            // In charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle when charging disabled, got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 0 {
		t.Errorf("expected no charge calls when flag disabled, got %d", len(mockBattery.ChargeCalls))
	}
}

func TestTick_DischargeFlagDisabled(t *testing.T) {
	// Scenario: Battery discharge flag is disabled
	// Expected: tick() should not discharge even in discharge window

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(80)
	mockBattery.DischargFlag = false                // Disabled!
	clockTime := baseTime.Add(2 * 15 * time.Minute) // In discharge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.lastChargePrice = decimal.NewFromFloat(0.05)

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle when discharging disabled, got %s", svc.state)
	}
	if len(mockBattery.DischargeCalls) != 0 {
		t.Errorf("expected no discharge calls when flag disabled, got %d", len(mockBattery.DischargeCalls))
	}
}

func TestTick_BatteryUnreachable(t *testing.T) {
	// Scenario: Battery status call fails
	// Expected: tick() should handle error gracefully, stay idle

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)

	cfg := testConfig()
	mockBattery := NewMockBattery(50)
	mockBattery.GetStatusErr = &mockError{"connection timeout"}
	clockTime := baseTime

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	ctx := context.Background()
	svc.tick(ctx) // Should not panic

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after battery error, got %s", svc.state)
	}
}

type mockError struct {
	msg string
}

func (e *mockError) Error() string { return e.msg }

// --- Edge cases for profitability check ---

// --- Trade recording tests ---

func TestTick_RecordsTrade(t *testing.T) {
	// Scenario: Complete a charge cycle
	// Expected: Trade should be recorded
	// Using small battery for shorter window sizes

	// Use "today" as base time so GetTodaySummary matches
	now := time.Now().UTC()
	baseTime := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - outside
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - outside
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(70)
	clockTime := baseTime.Add(15 * time.Minute) // Slot 1 - outside charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.state = StateCharging
	svc.currentTradeStart = baseTime // Trade started today
	svc.currentTradeSOC = 50

	ctx := context.Background()
	svc.tick(ctx) // Should stop charging and record trade

	// Check trade was recorded
	history := svc.recorder.GetHistory()
	if len(history.Days) == 0 {
		t.Fatal("expected trade to be recorded")
	}

	todaySummary := svc.recorder.GetTodaySummary()
	if todaySummary.ChargeCycles != 1 {
		t.Errorf("expected 1 charge cycle, got %d", todaySummary.ChargeCycles)
	}
}

// --- Multiple ticks simulation ---

func TestTick_FullChargeDischargeSequence(t *testing.T) {
	// Scenario: Simulate multiple ticks through charge and discharge windows
	// Expected: Should charge during low prices, discharge during high prices
	// Using small battery config for faster window sizes (1 slot = 15 min per window)

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // Charge window slot 0 (00:00-00:15)
		0.15, // Middle slot 1
		0.25, // Discharge window slot 2 (00:30-00:45)
		0.10, // Middle slot 3
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	ctx := context.Background()

	// Tick 1: Start charging at 00:00
	svc := newTestService(cfg, mockBattery, prices, baseTime)
	svc.tick(ctx)

	if svc.state != StateCharging {
		t.Errorf("tick 1: expected charging, got %s", svc.state)
	}

	// Tick 2: Leave charge window at 00:15, enter middle zone
	svc.nowFunc = func() time.Time { return baseTime.Add(15 * time.Minute) }
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("tick 2: expected idle after charge window ends, got %s", svc.state)
	}

	// Tick 3: Enter discharge window at 00:30
	svc.nowFunc = func() time.Time { return baseTime.Add(30 * time.Minute) }
	svc.tick(ctx)

	if svc.state != StateDischarging {
		t.Errorf("tick 3: expected discharging, got %s", svc.state)
	}

	// Tick 4: Leave discharge window at 00:45
	svc.nowFunc = func() time.Time { return baseTime.Add(45 * time.Minute) }
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("tick 4: expected idle after discharge window ends, got %s", svc.state)
	}

	// Verify both charge and discharge calls were made
	if len(mockBattery.ChargeCalls) != 1 {
		t.Errorf("expected 1 charge call, got %d", len(mockBattery.ChargeCalls))
	}
	if len(mockBattery.DischargeCalls) != 1 {
		t.Errorf("expected 1 discharge call, got %d", len(mockBattery.DischargeCalls))
	}
}

// --- Solar charging tests ---

// newTestServiceWithMeter creates a Service with a meter for solar testing.
func newTestServiceWithMeter(cfg *config.Config, battery *MockBattery, meter *MockMeterReader, prices []nordpool.Price, clockTime time.Time) *Service {
	recorder := NewRecorder("", cfg.BatteryEfficiency, time.UTC)
	svc := &Service{
		cfg:         cfg,
		nordpool:    &MockPriceProvider{TodayPrices: prices},
		battery:     battery,
		meter:       meter,
		recorder:    recorder,
		state:       StateIdle,
		loc:         time.UTC,
		todayPrices: prices,
		nowFunc:     func() time.Time { return clockTime },
	}
	svc.currentPlan = AnalyzePrices(prices, svc.analyzerConfig())
	return svc
}

func TestSolarTick_StartsAfterSustainedElapsedSurplus(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	for range 29 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	now = now.Add(time.Second - time.Nanosecond)
	svc.solarTick(context.Background())
	if svc.state != StateIdle {
		t.Fatalf("state before sustained qualification = %s, want idle", svc.state)
	}

	now = now.Add(time.Nanosecond)
	svc.solarTick(context.Background())
	if svc.state != StateSolarCharging {
		t.Fatalf("state after sustained qualification = %s, want solar_charging", svc.state)
	}
	if len(battery.ChargeCalls) != 1 || battery.ChargeCalls[0].PowerW != 500 {
		t.Fatalf("expected one 500 W charge command, got %+v", battery.ChargeCalls)
	}
}

func TestSolarTick_QualificationGapRequiresNewSustainedSurplus(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	now = now.Add(solarTelemetryGapTolerance + time.Nanosecond)
	svc.solarTick(context.Background())
	if svc.state != StateIdle {
		t.Fatalf("state after qualification telemetry gap = %s, want idle", svc.state)
	}

	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	if svc.state != StateSolarCharging {
		t.Fatalf("state after replacement sustained surplus = %s, want solar_charging", svc.state)
	}
}

func TestSolarTick_StopsAfterSustainedTelemetryFailure(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.GetStatusErr = errors.New("power sensor unavailable")
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-2 * time.Minute)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 54
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = now.Add(-time.Second)

	svc.solarTick(context.Background())
	now = now.Add(solarTelemetryFailureThreshold - time.Nanosecond)
	svc.solarTick(context.Background())
	if svc.state != StateSolarCharging {
		t.Fatalf("state before telemetry failure duration = %s, want solar_charging", svc.state)
	}

	now = now.Add(time.Nanosecond)
	svc.solarTick(context.Background())
	if svc.state != StateIdle {
		t.Errorf("state = %s, want idle after telemetry failure duration", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("idle calls = %d, want 1", battery.IdleCalls)
	}
	if battery.StatusCalls != 1 {
		t.Errorf("fallback status calls = %d, want 1 after sustained failure", battery.StatusCalls)
	}
	if !svc.solarCooldownUntil.Equal(now.Add(batteryControlFailureCooldown)) {
		t.Errorf("solar cooldown = %s, want %s", svc.solarCooldownUntil, now.Add(batteryControlFailureCooldown))
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 || history.Days[0].Trades[0].EndSOC != 54 {
		t.Fatalf("telemetry fallback trade = %+v, want last observed end SOC 54", history.Days)
	}
}

func TestSolarTick_StopFailureRetainsSessionAndEnergy(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(now, 0.10, 0.10, 0.10, 0.10)
	mockBattery := NewMockBattery(50)
	mockBattery.CurrentPower = 500
	mockBattery.IdleFailures = 1
	meter := NewMockMeter(true, 490)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), mockBattery, meter, prices, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = now.Add(-time.Second)
	svc.solarSurplusEMA = 5
	svc.solarLowSurplusSince = now.Add(-solarLowSurplusGrace)

	ctx := context.Background()
	svc.solarTick(ctx)
	if svc.state != StateSolarCharging {
		t.Fatalf("state after failed stop = %s, want solar_charging", svc.state)
	}
	if mockBattery.IdleAttempts != 1 {
		t.Fatalf("idle attempts after failed stop = %d, want 1", mockBattery.IdleAttempts)
	}
	if history := svc.recorder.GetHistory(); len(history.Days) != 0 {
		t.Fatalf("recorded trade before stop confirmation: %+v", history.Days)
	}
	if svc.solarEnergyWs <= 0 {
		t.Errorf("solar energy = %v, want accumulated energy retained", svc.solarEnergyWs)
	}

	// A one-second solar loop must not spam the failed stop command.
	svc.solarTick(ctx)
	if mockBattery.IdleAttempts != 1 {
		t.Fatalf("idle attempts during retry throttle = %d, want 1", mockBattery.IdleAttempts)
	}

	now = now.Add(svc.stopRetryDelay())
	svc.solarTick(ctx)
	if svc.state != StateIdle {
		t.Fatalf("state after successful retry = %s, want idle", svc.state)
	}
	if mockBattery.IdleAttempts != 2 {
		t.Fatalf("idle attempts after retry = %d, want 2", mockBattery.IdleAttempts)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("recorded trades after successful retry = %+v, want one", history.Days)
	}
	if got := history.Days[0].Trades[0].Timestamp; !got.Equal(svc.currentTradeStart) {
		t.Errorf("trade timestamp = %s, want original session start %s", got, svc.currentTradeStart)
	}
}

func TestSolarTick_YieldToDischargeWindow(t *testing.T) {
	// Scenario: Solar charging active, then a discharge window starts
	// Expected: tick() should stop solar charging and start discharging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	meter := NewMockMeter(true, -500)

	// Time is in discharge window (slot 2)
	clockTime := baseTime.Add(2 * 15 * time.Minute)
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	// Pre-set solar charging state
	svc.state = StateSolarCharging
	svc.currentTradeStart = clockTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 75
	svc.solarChargePower = 500

	ctx := context.Background()
	svc.tick(ctx) // Regular tick should handle the transition

	if svc.state != StateDischarging {
		t.Errorf("expected discharging after yield, got %s", svc.state)
	}
}

func TestSolarHandoffRechecksDischargeWindowAfterStop(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(30 * time.Minute)
	battery := NewMockBattery(80)
	battery.CurrentPower = 500
	battery.IdleHook = func() { now = baseTime.Add(45*time.Minute + time.Second) }
	svc := newTestService(
		testConfigSmallBattery(),
		battery,
		makePrices(baseTime, .05, .15, .25, .10),
		now,
	)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 75
	svc.solarChargePower = 500

	svc.tick(context.Background())

	if svc.state != StateIdle || len(battery.DischargeCalls) != 0 {
		t.Fatalf("expired solar handoff issued discharge: state=%s calls=%v", svc.state, battery.DischargeCalls)
	}
}

func TestAutomaticDischargeStartStopsWhenCommandCrossesWindowEnd(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(43 * time.Minute)
	battery := NewMockBattery(80)
	battery.DischargeHook = func() { now = baseTime.Add(45*time.Minute + time.Second) }
	battery.IdleHook = func() { now = baseTime.Add(45*time.Minute + 3*time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, .05, .15, .25, .10), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)

	svc.mu.Lock()
	svc.startDischargingLocked(context.Background(), decimal.NewFromFloat(.25), true, 80, svc.cfg.DischargePowerW, StateDischarging)
	svc.mu.Unlock()

	if svc.state != StateIdle || len(battery.DischargeCalls) != 1 || battery.IdleCalls != 1 {
		t.Fatalf("discharge crossing window end was not stopped: state=%s calls=%v idle=%d", svc.state, battery.DischargeCalls, battery.IdleCalls)
	}
	if battery.DischargeDeadline.IsZero() || time.Until(battery.DischargeDeadline) <= 0 || time.Until(battery.DischargeDeadline) > 2*time.Minute+time.Second {
		t.Fatalf("automatic discharge deadline = %s, want active-window bound", battery.DischargeDeadline)
	}
	if history := svc.recorder.GetHistory(); len(history.Days) != 1 || len(history.Days[0].Trades) != 1 || !history.Days[0].Trades[0].EnergyKWh.IsPositive() {
		t.Fatalf("cancelled discharge energy was not recorded: %+v", history)
	}
}

func TestAutomaticDischargeStartSkipsFinalMinute(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(45*time.Minute - time.Minute)
	battery := NewMockBattery(80)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), now)
	svc.nowFunc = func() time.Time { return now }

	svc.mu.Lock()
	svc.startDischargingLocked(context.Background(), decimal.NewFromFloat(.25), true, 80, svc.cfg.DischargePowerW, StateDischarging)
	svc.mu.Unlock()

	if len(battery.DischargeCalls) != 0 || svc.state != StateIdle {
		t.Fatalf("final-minute discharge started: calls=%v state=%s", battery.DischargeCalls, svc.state)
	}
}

func TestChargeStartSkipsAtExactFinalMinute(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base, Value: .05}}, base)
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Minute), Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(time.Hour), End: base.Add(2 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}}}

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.NewFromFloat(.05), 50)
	svc.mu.Unlock()

	if battery.ChargeAttempts != 0 || svc.automaticCycleCommit != nil {
		t.Fatalf("exact-final-minute charge started: attempts=%d commitment=%+v", battery.ChargeAttempts, svc.automaticCycleCommit)
	}
}

func TestActiveChargeStopsBeforeTruncatedReservationEnd(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(99)
	battery.CurrentPower = 2000
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .10}}, now)
	setReservedChargePlan(svc, now, decimal.NewFromFloat(.10))
	svc.state = StateCharging
	svc.currentTradeStart = now.Add(-30 * time.Second)
	svc.currentTradeSOC = 98
	svc.currentTradeLastSOC = 99
	svc.currentTradePowerW = svc.cfg.ChargePowerW
	svc.beginMeasuredTradeLocked(2000)

	svc.tick(context.Background())

	if svc.state != StateIdle || battery.IdleCalls != 1 || len(battery.refreshCalls) != 0 {
		t.Fatalf("near-end truncated reservation was not stopped: state=%s idle=%d refreshes=%v", svc.state, battery.IdleCalls, battery.refreshCalls)
	}
}

func TestActiveChargeContinuesToExactTariffBoundaryWithoutRefresh(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(14*time.Minute + 30*time.Second)
	battery := NewMockBattery(11)
	battery.CurrentPower = 2000
	prices := []nordpool.Price{{Time: base, Value: .10}, {Time: base.Add(15 * time.Minute), Value: .10}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, now)
	setReservedChargePlan(svc, base, decimal.NewFromFloat(.10))
	svc.state = StateCharging
	svc.currentTradeStart = base
	svc.currentTradeSOC = 11
	svc.currentTradeLastSOC = 11
	svc.currentTradePowerW = svc.cfg.ChargePowerW
	svc.beginMeasuredTradeLocked(2000)

	svc.tick(context.Background())

	if svc.state != StateCharging || battery.IdleCalls != 0 || len(battery.refreshCalls) != 0 {
		t.Fatalf("charge did not continue to tariff boundary without refresh: state=%s idle=%d refreshes=%v", svc.state, battery.IdleCalls, battery.refreshCalls)
	}
}

func TestRetainedDischargeStartsWithoutCurrentTariff(t *testing.T) {
	base := time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC)
	now := base.Add(14 * time.Minute)
	window := TimeWindow{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.30)}
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: window,
	}
	battery := NewMockBattery(80)
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base.Add(15 * time.Minute), Value: .35}}, now)
	svc.nowFunc = func() time.Time { return now }
	notifier := &MockNotifier{}
	svc.telegram = notifier
	svc.currentPlan = &TradingPlan{IsProfitable: true, DischargeOnly: true, DischargeWindows: []TimeWindow{window}}
	svc.automaticCycleCommit = cycle

	svc.tick(context.Background())

	if len(battery.DischargeCalls) != 1 || svc.state != StateDischarging {
		t.Fatalf("retained unpriced discharge not started: calls=%v state=%s", battery.DischargeCalls, svc.state)
	}
	if len(notifier.TradeStartCalls) != 0 || len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "recorded as unpriced") {
		t.Fatalf("retained unpriced discharge notification: starts=%v messages=%v", notifier.TradeStartCalls, notifier.Messages)
	}
}

func TestManualDischargeStartsWithoutFabricatedTariff(t *testing.T) {
	now := time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.telegram = notifier

	svc.mu.Lock()
	svc.startDischargingLocked(context.Background(), decimal.Zero, false, 80, svc.cfg.DischargePowerW, StateManualDischarging)
	svc.mu.Unlock()

	if len(notifier.TradeStartCalls) != 0 || len(notifier.Messages) != 1 ||
		!strings.Contains(notifier.Messages[0], "Started manual discharge") || !strings.Contains(notifier.Messages[0], "recorded as unpriced") {
		t.Fatalf("manual unpriced discharge notification: starts=%v messages=%v", notifier.TradeStartCalls, notifier.Messages)
	}
}

func TestManualDischargeResamplesTariffAfterStoppingPreviousOperation(t *testing.T) {
	base := time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC)
	now := base.Add(14*time.Minute + 59*time.Second)
	battery := NewMockBattery(80)
	battery.CurrentPower = 2000
	battery.IdleHook = func() { now = base.Add(15*time.Minute + time.Second) }
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base, Value: .10}}, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier
	svc.state = StateCharging
	svc.currentTradeStart = base
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 80
	svc.currentTradePowerW = svc.cfg.ChargePowerW
	svc.currentTradePrices = []nordpool.Price{{Time: base, Value: .10}}
	svc.beginMeasuredTradeLocked(2000)

	svc.handleManualDischargeCommand(context.Background(), nil)

	if svc.state != StateManualDischarging || len(notifier.TradeStartCalls) != 0 {
		t.Fatalf("manual boundary discharge state=%s priced_starts=%v", svc.state, notifier.TradeStartCalls)
	}
	foundUnpricedStart := false
	for _, message := range notifier.Messages {
		foundUnpricedStart = foundUnpricedStart || strings.Contains(message, "Started manual discharge") && strings.Contains(message, "recorded as unpriced")
	}
	if !foundUnpricedStart {
		t.Fatalf("manual discharge did not use post-stop tariff availability: messages=%v", notifier.Messages)
	}
}

func TestDischargeStartNotificationResamplesTariffAfterControl(t *testing.T) {
	base := time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name         string
		initial      time.Time
		afterControl time.Time
		initialPrice decimal.Decimal
		initialKnown bool
		wantKnown    bool
	}{
		{name: "known to unavailable", initial: base.Add(14*time.Minute + 59*time.Second), afterControl: base.Add(15*time.Minute + time.Second), initialPrice: decimal.NewFromFloat(.10), initialKnown: true},
		{name: "unavailable to known", initial: base.Add(-time.Second), afterControl: base, wantKnown: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := tt.initial
			battery := NewMockBattery(80)
			battery.DischargeHook = func() { now = tt.afterControl }
			notifier := &MockNotifier{}
			svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base, Value: .20}}, now)
			svc.nowFunc = func() time.Time { return now }
			svc.telegram = notifier

			svc.mu.Lock()
			svc.startDischargingLocked(context.Background(), tt.initialPrice, tt.initialKnown, 80, svc.cfg.DischargePowerW, StateManualDischarging)
			svc.mu.Unlock()

			if tt.wantKnown {
				if len(notifier.TradeStartCalls) != 1 || math.Abs(notifier.TradeStartCalls[0].Price-.20) > .000001 || len(notifier.Messages) != 0 {
					t.Fatalf("known post-control tariff notification: starts=%v messages=%v", notifier.TradeStartCalls, notifier.Messages)
				}
			} else if len(notifier.TradeStartCalls) != 0 || len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "recorded as unpriced") {
				t.Fatalf("unavailable post-control tariff notification: starts=%v messages=%v", notifier.TradeStartCalls, notifier.Messages)
			}
		})
	}
}

func TestUnpricedChargeCompletionReportsIncompleteCost(t *testing.T) {
	now := time.Date(2024, 1, 15, 1, 0, 0, 0, time.UTC)
	battery := NewMockBattery(60)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.telegram = notifier
	svc.state = StateCharging
	svc.currentTradeStart = now.Add(-time.Hour)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 60
	svc.currentTradeLastUpdate = now
	svc.currentTradeEnergyWs = 3_600_000
	svc.currentTradePricedEnergyWs = 1_800_000
	svc.currentTradeUnpricedWs = 1_800_000
	svc.currentTradeCostEUR = decimal.NewFromFloat(.05)

	svc.mu.Lock()
	svc.stopChargingLocked(context.Background(), 60)
	svc.mu.Unlock()

	if len(notifier.TradeEndCalls) != 0 || len(notifier.Messages) != 1 {
		t.Fatalf("completion notifications: trade_end=%v messages=%v", notifier.TradeEndCalls, notifier.Messages)
	}
	for _, want := range []string{"Priced energy: 0.50 kWh", "Unpriced energy: 0.50 kWh", "Known cost: 0.0500 EUR", "Total cost: incomplete"} {
		if !strings.Contains(notifier.Messages[0], want) {
			t.Errorf("completion message %q does not contain %q", notifier.Messages[0], want)
		}
	}
}

func TestSolarCompletionReportsGridAndOpportunityCostCompleteness(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	battery := NewMockBattery(60)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.telegram = notifier
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-time.Hour)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 60
	svc.solarLastUpdate = now
	svc.solarEnergyWs = 3_600_000
	svc.solarGridEnergyWs = 900_000
	svc.solarGridUnpricedWs = 360_000
	svc.solarGridCostEUR = decimal.NewFromFloat(.03)
	svc.solarOpportunityUnpricedWs = 720_000
	svc.solarOpportunityCostEUR = decimal.NewFromFloat(.05)

	svc.mu.Lock()
	svc.stopSolarChargingLocked(context.Background(), 60, solarStopReasonSurplusGone)
	svc.mu.Unlock()

	if len(notifier.TradeEndCalls) != 0 || len(notifier.Messages) != 1 {
		t.Fatalf("solar completion notifications: trade_end=%v messages=%v", notifier.TradeEndCalls, notifier.Messages)
	}
	for _, want := range []string{
		"Solar energy: 0.75 kWh", "Grid energy: 0.25 kWh", "Unpriced grid energy: 0.10 kWh",
		"Known grid cost: 0.0300 EUR", "Total grid cost: incomplete", "Unpriced solar energy: 0.20 kWh",
		"Known forgone export value: 0.0500 EUR", "Total forgone export value: incomplete",
	} {
		if !strings.Contains(notifier.Messages[0], want) {
			t.Errorf("solar completion message %q does not contain %q", notifier.Messages[0], want)
		}
	}
}

func TestSolarUpperSOCSettlesPreviousGridPowerBeforeTaper(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	battery := NewMockBattery(99)
	battery.CurrentPower = 100
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, NewMockMeter(true, 0), []nordpool.Price{{Time: now.Add(-time.Minute), Value: .20}}, now)
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-time.Minute)
	svc.currentTradeSOC = 90
	svc.currentTradeLastSOC = 90
	svc.solarLastUpdate = now.Add(-time.Minute)
	svc.solarMeasuredChargePowerW = 1000
	svc.solarGridPowerW = 800
	svc.currentTradePrices = []nordpool.Price{{Time: now.Add(-time.Minute), Value: .20}}

	svc.solarTick(context.Background())

	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("solar stop history = %+v", history)
	}
	wantGridEnergy := decimal.NewFromFloat(800.0 * 60 / 3_600_000)
	if got := history.Days[0].Trades[0].GridEnergyKWh; !got.Equal(wantGridEnergy) {
		t.Fatalf("settled grid energy = %s, want prior 800 W interval %s", got, wantGridEnergy)
	}
}

func TestTradePersistenceFailureNotifiesOperator(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(60), nil, now)
	svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)
	svc.telegram = notifier
	svc.state = StateCharging
	svc.currentTradeStart = now.Add(-time.Minute)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 60
	svc.currentTradeLastUpdate = now
	svc.currentTradeEnergyWs = 120_000

	svc.mu.Lock()
	svc.stopChargingLocked(context.Background(), 60)
	svc.mu.Unlock()

	if len(notifier.ErrorCalls) != 1 || !strings.Contains(notifier.ErrorCalls[0], "Failed to persist completed charge") {
		t.Fatalf("persistence notifications = %v", notifier.ErrorCalls)
	}
}

func TestTickRetriesFailedTradePersistence(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.UTC)
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(dataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(60), nil, now)
	svc.recorder = NewRecorder(dataDir, svc.cfg.BatteryEfficiency, time.UTC)
	trade := Trade{Timestamp: now, Action: ActionCharge, EnergyKWh: decimal.RequireFromString("0.5")}
	if err := svc.recorder.RecordTrade(trade); err == nil {
		t.Fatal("RecordTrade() error = nil, want blocked data path failure")
	}
	if err := os.Remove(dataDir); err != nil {
		t.Fatalf("remove blocked data path: %v", err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("create usable data directory: %v", err)
	}

	svc.tick(context.Background())

	reloaded := NewRecorder(dataDir, svc.cfg.BatteryEfficiency, time.UTC)
	if err := reloaded.LoadTrades(); err != nil {
		t.Fatalf("LoadTrades() after retry error = %v", err)
	}
	history := reloaded.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("retried trade history = %+v", history)
	}
}

func TestUnpricedDischargeCompletionReportsIncompleteValue(t *testing.T) {
	now := time.Date(2024, 1, 15, 18, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		pricedWs   float64
		unpricedWs float64
		knownValue decimal.Decimal
		want       []string
	}{
		{
			name:       "wholly unpriced",
			unpricedWs: 3_600_000,
			want:       []string{"Priced energy: 0.00 kWh", "Unpriced energy: 1.00 kWh", "Known value: 0.0000 EUR", "Total value: incomplete"},
		},
		{
			name:       "partially unpriced",
			pricedWs:   1_800_000,
			unpricedWs: 1_800_000,
			knownValue: decimal.NewFromFloat(.15),
			want:       []string{"Priced energy: 0.50 kWh", "Unpriced energy: 0.50 kWh", "Known value: 0.1500 EUR", "Total value: incomplete"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			battery := NewMockBattery(50)
			notifier := &MockNotifier{}
			svc := newTestService(testConfigSmallBattery(), battery, nil, now)
			svc.telegram = notifier
			svc.state = StateDischarging
			svc.currentTradeStart = now.Add(-time.Hour)
			svc.currentTradeSOC = 80
			svc.currentTradeLastSOC = 50
			svc.currentTradePowerW = 1000
			svc.currentTradeLastUpdate = now
			svc.currentTradeEnergyWs = tt.pricedWs + tt.unpricedWs
			svc.currentTradePricedEnergyWs = tt.pricedWs
			svc.currentTradeUnpricedWs = tt.unpricedWs
			svc.currentTradeCostEUR = tt.knownValue

			svc.mu.Lock()
			svc.stopDischargingLocked(context.Background(), 50)
			svc.mu.Unlock()

			if len(notifier.TradeEndCalls) != 0 || len(notifier.Messages) != 1 {
				t.Fatalf("completion notifications: trade_end=%v messages=%v", notifier.TradeEndCalls, notifier.Messages)
			}
			for _, want := range tt.want {
				if !strings.Contains(notifier.Messages[0], want) {
					t.Errorf("completion message %q does not contain %q", notifier.Messages[0], want)
				}
			}
		})
	}
}

func TestActiveDischargeStopsWhenRefreshCrossesWindowEnd(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(43 * time.Minute)
	battery := NewMockBattery(80)
	battery.CurrentPower = -2000
	battery.PassiveHook = func() { now = baseTime.Add(45*time.Minute + time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, .05, .15, .25, .10), now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateDischarging
	svc.currentTradeStart = now.Add(-10 * time.Minute)
	svc.currentTradeSOC = 90
	svc.currentTradeLastSOC = 80
	svc.currentTradePowerW = svc.cfg.DischargePowerW
	svc.beginMeasuredTradeLocked(-2000)
	svc.lastPassiveRefresh = now.Add(-time.Hour)

	svc.tick(context.Background())

	if svc.state != StateIdle || battery.IdleCalls != 1 {
		t.Fatalf("discharge refresh crossing window end was not stopped: state=%s idle=%d", svc.state, battery.IdleCalls)
	}
	if battery.RefreshDeadline.IsZero() || time.Until(battery.RefreshDeadline) <= 0 || time.Until(battery.RefreshDeadline) > 2*time.Minute+time.Second {
		t.Fatalf("discharge refresh deadline = %s, want active-window bound", battery.RefreshDeadline)
	}
}

func TestSolarTick_YieldToChargeWindow(t *testing.T) {
	// Scenario: Solar charging active, then a scheduled charge window starts
	// Expected: tick() should stop solar charging and start scheduled charging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)

	// Time is in charge window (slot 0)
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	// Pre-set solar charging state
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500

	ctx := context.Background()
	svc.tick(ctx) // Regular tick should handle the transition

	if svc.state != StateCharging {
		t.Errorf("expected charging after yield to charge window, got %s", svc.state)
	}
}

func TestSolarHandoffRechecksReservationAfterStop(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	battery.IdleHook = func() { now = baseTime.Add(15*time.Minute + time.Second) }
	svc := newTestServiceWithMeter(
		testConfigSmallBattery(),
		battery,
		NewMockMeter(true, -500),
		makePrices(baseTime, .05, .40, .40, .40),
		now,
	)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500

	svc.tick(context.Background())

	if svc.state != StateIdle || battery.ChargeAttempts != 0 {
		t.Fatalf("expired solar handoff issued grid charge: state=%s attempts=%d", svc.state, battery.ChargeAttempts)
	}
}

func TestSolarStartStopsWhenCommandCrossesIntoDischargeWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.ChargeHook = func() { now = baseTime.Add(15*time.Minute + time.Second) }
	battery.IdleHook = func() { now = baseTime.Add(15*time.Minute + 3*time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, .10, .25), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)
	svc.currentPlan = &TradingPlan{
		IsProfitable: true,
		DischargeWindows: []TimeWindow{{
			Start: baseTime.Add(15 * time.Minute), End: baseTime.Add(30 * time.Minute), Price: decimal.NewFromFloat(.25),
		}},
	}

	svc.mu.Lock()
	svc.startSolarChargingLocked(context.Background(), 500, 50)
	svc.mu.Unlock()

	if svc.state != StateIdle || len(battery.ChargeCalls) != 1 || battery.IdleCalls != 1 {
		t.Fatalf("solar command crossing discharge boundary was not stopped: state=%s charge=%v idle=%d", svc.state, battery.ChargeCalls, battery.IdleCalls)
	}
	if history := svc.recorder.GetHistory(); len(history.Days) != 1 || len(history.Days[0].Trades) != 1 || !history.Days[0].Trades[0].EnergyKWh.IsPositive() {
		t.Fatalf("cancelled solar charge energy was not recorded: %+v", history)
	}
}

func TestSolarAdjustmentStopsWhenCommandCrossesIntoDischargeWindow(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	battery.ChargeHook = func() { now = baseTime.Add(15*time.Minute + time.Second) }
	meter := NewMockMeter(true, -1500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, makePrices(baseTime, .10, .25), now)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = &TradingPlan{
		IsProfitable: true,
		DischargeWindows: []TimeWindow{{
			Start: baseTime.Add(15 * time.Minute), End: baseTime.Add(30 * time.Minute), Price: decimal.NewFromFloat(.25),
		}},
	}
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarSurplusEMA = 1000
	svc.solarEMALastSampleAt = now.Add(-time.Second)
	svc.solarLastUpdate = now.Add(-time.Second)
	svc.lastPassiveRefresh = now.Add(-10 * time.Second)

	svc.solarTick(context.Background())

	if svc.state != StateIdle || len(battery.ChargeCalls) != 1 || battery.IdleCalls != 1 {
		t.Fatalf("solar adjustment crossing discharge boundary was not stopped: state=%s charge=%v idle=%d", svc.state, battery.ChargeCalls, battery.IdleCalls)
	}
}

func TestSolarAdjustmentRetainsAppliedTargetUntilStopIsConfirmed(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	battery.ChargeHook = func() { now = baseTime.Add(15*time.Minute + time.Second) }
	battery.IdleErr = errors.New("idle unavailable")
	meter := NewMockMeter(true, -1500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, makePrices(baseTime, .10, .25), now)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = &TradingPlan{IsProfitable: true, DischargeWindows: []TimeWindow{{
		Start: baseTime.Add(15 * time.Minute), End: baseTime.Add(30 * time.Minute), Price: decimal.NewFromFloat(.25),
	}}}
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarSurplusEMA = 1000
	svc.solarEMALastSampleAt = now.Add(-time.Second)
	svc.solarLastUpdate = now.Add(-time.Second)
	svc.lastPassiveRefresh = now.Add(-10 * time.Second)

	svc.solarTick(context.Background())

	if svc.state != StateSolarCharging || svc.solarChargePower != 1050 || !svc.stopPending {
		t.Fatalf("unconfirmed stop lost applied target: state=%s target=%d stop_pending=%t", svc.state, svc.solarChargePower, svc.stopPending)
	}
}

func TestSolarTick_YieldsScheduledWindowBeforeReadingP1(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.15, 0.25, 0.10)
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	meter := NewMockMeter(true, -500)
	meter.ActivePowerErr = errors.New("P1 unavailable")
	cfg := testConfigSmallBattery()
	svc := newTestServiceWithMeter(cfg, battery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = baseTime.Add(-time.Second)

	svc.solarTick(context.Background())

	if meter.ActivePowerCalls != 0 {
		t.Errorf("P1 calls = %d, want 0 while yielding scheduled window", meter.ActivePowerCalls)
	}
	if svc.state != StateCharging || battery.CurrentPower != cfg.ChargePowerW {
		t.Errorf("scheduled-window yield: state=%s power=%d, want charging at %dW", svc.state, battery.CurrentPower, cfg.ChargePowerW)
	}
}

func TestSolarTickRechecksScheduledPriorityAfterP1Read(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	meter := NewMockMeter(true, -500)
	meter.ActivePowerHook = func() { now = base.Add(15 * time.Minute) }
	cfg := testConfigSmallBattery()
	svc := newTestServiceWithMeter(cfg, battery, meter, makePrices(base, .10, .25), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), cfg.BatteryEfficiency, time.UTC)
	svc.currentPlan = &TradingPlan{IsProfitable: true, DischargeWindows: []TimeWindow{{
		Start: base.Add(15 * time.Minute), End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.25),
	}}}
	svc.state = StateSolarCharging
	svc.currentTradeStart = base
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = now.Add(-time.Second)

	svc.solarTick(context.Background())

	if meter.ActivePowerCalls != 1 {
		t.Fatalf("P1 hook was not exercised: calls=%d", meter.ActivePowerCalls)
	}
	if svc.state != StateDischarging || battery.IdleCalls != 1 || len(battery.DischargeCalls) != 1 {
		t.Fatalf("P1 boundary crossing did not hand off to discharge: state=%s idle=%d discharge_calls=%d", svc.state, battery.IdleCalls, len(battery.DischargeCalls))
	}
}

func TestSolarTick_ClampToMaxPower(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	cfg := testConfigSmallBattery()
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -5000)
	svc := newTestServiceWithMeter(cfg, battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}

	if svc.state != StateSolarCharging {
		t.Fatalf("expected solar_charging, got %s", svc.state)
	}
	if len(battery.ChargeCalls) != 1 || battery.ChargeCalls[0].PowerW != cfg.ChargePowerW {
		t.Fatalf("expected one %d W charge command, got %+v", cfg.ChargePowerW, battery.ChargeCalls)
	}
}

func TestSolarTick_NoStartDuringDischargeWindow(t *testing.T) {
	// Scenario: Surplus available but we're in a discharge window
	// Expected: Should not start solar charging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	meter := NewMockMeter(true, -500) // exporting

	// Time is in discharge window (slot 2)
	clockTime := baseTime.Add(2 * 15 * time.Minute)
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	ctx := context.Background()
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle during discharge window, got %s", svc.state)
	}
}

func TestSolarTick_NoStartDuringChargeWindow(t *testing.T) {
	// Scenario: Surplus available but we're in a scheduled charge window
	// Expected: Should not start solar charging (let tick() handle scheduled charging)

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500) // exporting

	// Time is in charge window (slot 0)
	clockTime := baseTime
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	ctx := context.Background()
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle during charge window, got %s", svc.state)
	}
}

func TestSolarTick_IgnoresStateCharging(t *testing.T) {
	// Scenario: Battery is in scheduled charging state
	// Expected: solarTick should be a no-op

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateCharging

	ctx := context.Background()
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateCharging {
		t.Errorf("expected state unchanged at charging, got %s", svc.state)
	}
	// No charge calls should be made by solarTick
	if len(mockBattery.ChargeCalls) != 0 {
		t.Errorf("expected no charge calls from solarTick, got %d", len(mockBattery.ChargeCalls))
	}
}

func TestSolarTick_IgnoresStateDischarging(t *testing.T) {
	// Scenario: Battery is in scheduled discharging state
	// Expected: solarTick should be a no-op

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateDischarging

	ctx := context.Background()
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateDischarging {
		t.Errorf("expected state unchanged at discharging, got %s", svc.state)
	}
}

func TestSolarTick_NoStartAtUpperSOC(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(solarChargeUpperSOC)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	now = now.Add(solarStartQualification)
	svc.solarTick(context.Background())

	if svc.state != StateIdle {
		t.Errorf("expected idle at solar upper SOC limit, got %s", svc.state)
	}
	if len(battery.ChargeCalls) != 0 {
		t.Errorf("expected no charge calls at solar upper SOC limit, got %d", len(battery.ChargeCalls))
	}
}

func TestSolarTick_StopAtUpperSOC(t *testing.T) {
	// A solar session that reaches the upper SOC limit must stop immediately,
	// even though the battery has not rounded its telemetry up to 100%.
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(solarChargeUpperSOC)
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-10 * time.Minute)
	svc.currentTradeSOC = 95
	svc.solarChargePower = 500

	ctx := context.Background()
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle at solar upper SOC limit, got %s", svc.state)
	}
	if mockBattery.IdleCalls != 1 {
		t.Errorf("expected one idle command at solar upper SOC limit, got %d", mockBattery.IdleCalls)
	}
}

func TestSolarTick_StopsAtUpperSOCBeforeReadingP1(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(solarChargeUpperSOC)
	battery.CurrentPower = 500
	meter := NewMockMeter(true, -500)
	meter.ActivePowerErr = errors.New("P1 unavailable")
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 95
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = now.Add(-time.Second)

	svc.solarTick(context.Background())

	if meter.ActivePowerCalls != 0 {
		t.Errorf("P1 calls = %d, want 0 after upper-SOC safety stop", meter.ActivePowerCalls)
	}
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Errorf("upper-SOC safety stop: state=%s power=%d, want idle and 0W", svc.state, battery.CurrentPower)
	}
}

func TestSolarTick_UpperSOCHoldRejectsTelemetryFlicker(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(solarChargeUpperSOC)
	battery.CurrentPower = 500
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-10 * time.Minute)
	svc.currentTradeSOC = 95
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarLastUpdate = now.Add(-time.Second)

	svc.solarTick(context.Background())
	if svc.state != StateIdle {
		t.Fatalf("state after upper-SOC stop = %s, want idle", svc.state)
	}

	now = now.Add(solarRestartCooldown + time.Second)
	battery.SOC = solarChargeUpperSOC - 1
	svc.solarTick(context.Background())
	if svc.state != StateIdle || len(battery.ChargeCalls) != 0 {
		t.Fatalf("upper-SOC hold should reject flicker, state=%s calls=%+v", svc.state, battery.ChargeCalls)
	}

	battery.SOC = solarChargeResumeSOC
	svc.solarTick(context.Background())
	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	if svc.state != StateSolarCharging {
		t.Fatalf("state after SOC falls to resume threshold = %s, want solar_charging", svc.state)
	}
}

func TestSolarTick_MeterFailureRequiresNewSustainedQualification(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	now = now.Add(solarStartQualification / 2)
	meter.ActivePowerErr = errors.New("P1 unavailable")
	svc.solarTick(context.Background())

	meter.ActivePowerErr = nil
	svc.solarTick(context.Background())
	if svc.state != StateIdle {
		t.Fatalf("state immediately after meter recovery = %s, want idle", svc.state)
	}
	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	if svc.state != StateSolarCharging {
		t.Fatalf("state after replacement sustained surplus = %s, want solar_charging", svc.state)
	}
}

func TestSolarTick_BelowThresholdRequiresNewSustainedQualification(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }

	svc.solarTick(context.Background())
	now = now.Add(solarStartQualification / 2)
	meter.SetActivePowerW(-50)
	svc.solarTick(context.Background())

	meter.SetActivePowerW(-500)
	svc.solarTick(context.Background())
	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	if svc.state != StateSolarCharging {
		t.Fatalf("state after replacement sustained surplus = %s, want solar_charging", svc.state)
	}
}

func TestSolarTick_PowerAdjustmentDeadband(t *testing.T) {
	// Scenario: Solar charging at 500W, real surplus is 530W (within 50W deadband of current power)
	// Meter shows -30W (exporting 30W) because battery draws 500W of the 530W surplus.
	// effective_surplus = 30 + 500 = 530 → target=530 → diff=30 < 50 deadband → no adjust
	// Expected: Should NOT send a new charge command

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -30) // meter sees 30W export (530W real - 500W battery)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.lastPassiveRefresh = baseTime // Recent refresh

	ctx := context.Background()
	svc.solarTick(ctx)

	// Power should stay at 500 (30W diff < 50W deadband)
	if svc.solarChargePower != 500 {
		t.Errorf("expected power unchanged at 500, got %d", svc.solarChargePower)
	}
}

func TestSolarTick_EMA80StaysAboveDefaultStopHysteresis(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.CurrentPower = 80
	meter := NewMockMeter(true, 0)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 80
	svc.solarSurplusEMA = 80
	svc.lastPassiveRefresh = now

	svc.solarTick(context.Background())

	if svc.state != StateSolarCharging {
		t.Errorf("state at 80W EMA = %s, want solar_charging above default 75W stop threshold", svc.state)
	}
	if battery.IdleCalls != 0 {
		t.Errorf("idle calls at 80W EMA = %d, want 0", battery.IdleCalls)
	}
}

func TestSolarTick_UsesGraceBelowScaledStopThreshold(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	cfg := testConfigSmallBattery()
	cfg.SolarMinSurplusW = 400
	battery := NewMockBattery(50)
	battery.CurrentPower = 80
	meter := NewMockMeter(true, 0)
	svc := newTestServiceWithMeter(cfg, battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 80
	svc.solarSurplusEMA = 80

	svc.solarTick(context.Background())
	now = now.Add(solarLowSurplusGrace - time.Second)
	svc.solarTick(context.Background())
	if svc.state != StateSolarCharging {
		t.Fatalf("state before 100W-threshold grace expires = %s, want solar_charging", svc.state)
	}

	now = now.Add(time.Second)
	svc.solarTick(context.Background())
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Errorf("state after sustained 80W EMA below 100W threshold = %s power=%d, want idle and 0W", svc.state, battery.CurrentPower)
	}
}

func TestSolarTick_AdjustmentFailureStopsHighPowerSession(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.CurrentPower = 1500
	battery.ChargeErr = errors.New("power adjustment failed")
	battery.IdleFailures = 1
	meter := NewMockMeter(true, 1500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 1500
	svc.lastPassiveRefresh = now.Add(-5 * time.Second)

	ctx := context.Background()
	svc.solarTick(ctx)
	if svc.state != StateSolarCharging || battery.CurrentPower != 1500 {
		t.Fatalf("failed stop after 1500W-to-75W adjustment: state=%s power=%d, want retained session until stop retry", svc.state, battery.CurrentPower)
	}
	if battery.ChargeAttempts != 1 || battery.IdleAttempts != 1 {
		t.Fatalf("attempts after failed adjustment: charge=%d idle=%d, want one each", battery.ChargeAttempts, battery.IdleAttempts)
	}

	// The retry throttle must prevent another high-power adjustment every second.
	svc.solarTick(ctx)
	if battery.ChargeAttempts != 1 || battery.IdleAttempts != 1 {
		t.Fatalf("attempts during retry throttle: charge=%d idle=%d, want one each", battery.ChargeAttempts, battery.IdleAttempts)
	}

	now = now.Add(svc.stopRetryDelay())
	svc.solarTick(ctx)
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Fatalf("state after confirmed stop retry: state=%s power=%d, want idle and 0W", svc.state, battery.CurrentPower)
	}
	if battery.ChargeAttempts != 1 || battery.IdleAttempts != 2 {
		t.Errorf("attempts after confirmed stop retry: charge=%d idle=%d, want 1 charge and 2 idle", battery.ChargeAttempts, battery.IdleAttempts)
	}
}

func TestMeterEnabled(t *testing.T) {
	tests := []struct {
		name     string
		meter    MeterReader
		expected bool
	}{
		{"nil meter", nil, false},
		{"disabled meter", NewMockMeter(false, 0), false},
		{"enabled meter", NewMockMeter(true, 0), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &Service{meter: tt.meter}
			if got := svc.meterEnabled(); got != tt.expected {
				t.Errorf("meterEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSolarTick_RecordsTrade(t *testing.T) {
	// Scenario: Solar charge starts and stops, trade should be recorded with ActionSolarCharge and zero price

	now := time.Now().UTC()
	baseTime := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	// Meter shows +510W import. effective surplus = -510 + 500 = -10W, below the 75W stop threshold.
	meter := NewMockMeter(true, 510)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-10 * time.Minute) // session old enough to stop
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarLastUpdate = baseTime.Add(-10 * time.Minute)
	svc.solarSurplusEMA = 5
	svc.solarLowSurplusSince = baseTime.Add(-solarLowSurplusGrace)

	ctx := context.Background()
	svc.solarTick(ctx)

	// Check trade was recorded
	history := svc.recorder.GetHistory()
	if len(history.Days) == 0 {
		t.Fatal("expected trade to be recorded")
	}

	trade := history.Days[0].Trades[0]
	if trade.Action != ActionSolarCharge {
		t.Errorf("expected ActionSolarCharge, got %s", trade.Action)
	}
	if !trade.PriceEUR.IsZero() {
		t.Errorf("expected zero price, got %s", trade.PriceEUR)
	}
}

func TestSolarTick_EnergyAccumulatesWithVaryingPower(t *testing.T) {
	// Scenario: Solar charging at 500W for 5 min, then 1500W for 5 min
	// Expected energy: (500*300 + 1500*300) / 3_600_000 = 0.1667 kWh
	// Old bug would have calculated: 1500*600 / 3_600_000 = 0.25 kWh

	now := time.Now().UTC()
	baseTime := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	// At stop time the mock reports no measured charge power, so the EMA floor stops the session.
	meter := NewMockMeter(true, 1510)

	clockTime := baseTime
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	// Simulate: started at 500W at baseTime
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 40
	svc.solarChargePower = 500
	svc.solarMeasuredChargePowerW = 500
	svc.solarEnergyWs = 0
	svc.solarLastUpdate = baseTime

	// Advance 5 minutes and accumulate at 500W
	clockTime = baseTime.Add(5 * time.Minute)
	svc.SetClock(func() time.Time { return clockTime })
	svc.accumulateSolarEnergyLocked(1500)
	// Should have 500 * 300 = 150000 Ws
	if svc.solarEnergyWs != 150000 {
		t.Errorf("expected 150000 Ws after 5min at 500W, got %f", svc.solarEnergyWs)
	}

	// Change requested power to 1500W; measured power was updated above.
	svc.solarChargePower = 1500

	// Advance another 5 minutes and stop
	clockTime = baseTime.Add(10 * time.Minute)
	svc.SetClock(func() time.Time { return clockTime })
	svc.solarSurplusEMA = 5
	svc.solarLowSurplusSince = clockTime.Add(-solarLowSurplusGrace)

	ctx := context.Background()
	svc.solarTick(ctx)

	history := svc.recorder.GetHistory()
	if len(history.Days) == 0 {
		t.Fatal("expected trade to be recorded")
	}

	trade := history.Days[0].Trades[0]
	energyF, _ := trade.EnergyKWh.Float64()
	// 500W*300s + 1500W*300s = 600000 Ws = 0.1667 kWh
	expectedKWh := 600000.0 / 3_600_000.0
	tolerance := 0.001
	diff := energyF - expectedKWh
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Errorf("energy = %.4f kWh, want ~%.4f kWh (varying power accumulation)", energyF, expectedKWh)
	}
}

func TestSolarTick_AdaptiveCooldown(t *testing.T) {
	// Stops classified as "surplus gone" on short sessions must extend the cooldown.
	// After solarShortSessionBackoffCount consecutive short sessions, cooldown escalates
	// to solarLongBackoffCooldown. Legitimate reasons (battery full, window yield) reset
	// the streak and use the baseline cooldown.

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	ctx := context.Background()

	// Helper: simulate a short surplus-gone stop from the StateSolarCharging state.
	shortSurplusGoneStop := func() {
		svc.state = StateSolarCharging
		svc.currentTradeStart = svc.now().Add(-5 * time.Minute) // marginal, even beyond the old two-minute cutoff
		svc.currentTradeSOC = 50
		svc.solarChargePower = 500
		svc.solarEnergyWs = 0
		svc.mu.Lock()
		svc.stopSolarChargingLocked(ctx, 50, solarStopReasonSurplusGone)
		svc.mu.Unlock()
	}

	// First short stop → short-session cooldown
	shortSurplusGoneStop()
	if svc.solarConsecutiveShortSessions != 1 {
		t.Errorf("after 1st short stop: expected streak=1, got %d", svc.solarConsecutiveShortSessions)
	}
	gotCooldown := svc.solarCooldownUntil.Sub(svc.now())
	if gotCooldown != solarShortSessionCooldown {
		t.Errorf("after 1st short stop: expected cooldown=%v, got %v",
			solarShortSessionCooldown, gotCooldown)
	}

	// Second short stop → still short-session cooldown
	shortSurplusGoneStop()
	if svc.solarConsecutiveShortSessions != 2 {
		t.Errorf("after 2nd short stop: expected streak=2, got %d", svc.solarConsecutiveShortSessions)
	}
	gotCooldown = svc.solarCooldownUntil.Sub(svc.now())
	if gotCooldown != solarShortSessionCooldown {
		t.Errorf("after 2nd short stop: expected cooldown=%v, got %v",
			solarShortSessionCooldown, gotCooldown)
	}

	// Third short stop → escalates to long backoff
	shortSurplusGoneStop()
	if svc.solarConsecutiveShortSessions != solarShortSessionBackoffCount {
		t.Errorf("after 3rd short stop: expected streak=%d, got %d",
			solarShortSessionBackoffCount, svc.solarConsecutiveShortSessions)
	}
	gotCooldown = svc.solarCooldownUntil.Sub(svc.now())
	if gotCooldown != solarLongBackoffCooldown {
		t.Errorf("after 3rd short stop: expected cooldown=%v (long backoff), got %v",
			solarLongBackoffCooldown, gotCooldown)
	}

	// Battery-full stop (legitimate) resets streak and uses baseline cooldown
	svc.state = StateSolarCharging
	svc.currentTradeStart = svc.now().Add(-10 * time.Second) // short, but battery full
	svc.currentTradeSOC = 95
	svc.solarChargePower = 500
	svc.solarEnergyWs = 0
	svc.mu.Lock()
	svc.stopSolarChargingLocked(ctx, 100, solarStopReasonBatteryFull)
	svc.mu.Unlock()
	if svc.solarConsecutiveShortSessions != 0 {
		t.Errorf("after battery-full stop: expected streak reset to 0, got %d",
			svc.solarConsecutiveShortSessions)
	}
	gotCooldown = svc.solarCooldownUntil.Sub(svc.now())
	if gotCooldown != solarRestartCooldown {
		t.Errorf("after battery-full stop: expected baseline cooldown=%v, got %v",
			solarRestartCooldown, gotCooldown)
	}

	// Long session (above short threshold) with surplus-gone also resets streak
	svc.solarConsecutiveShortSessions = 2
	svc.state = StateSolarCharging
	svc.currentTradeStart = svc.now().Add(-solarShortSessionThreshold) // exactly at the short-session boundary, so it resets the streak
	svc.currentTradeSOC = 50
	svc.solarChargePower = 500
	svc.solarEnergyWs = 0
	svc.mu.Lock()
	svc.stopSolarChargingLocked(ctx, 70, solarStopReasonSurplusGone)
	svc.mu.Unlock()
	if svc.solarConsecutiveShortSessions != 0 {
		t.Errorf("after long surplus-gone stop: expected streak reset to 0, got %d",
			svc.solarConsecutiveShortSessions)
	}
	gotCooldown = svc.solarCooldownUntil.Sub(svc.now())
	if gotCooldown != solarRestartCooldown {
		t.Errorf("after long session: expected baseline cooldown=%v, got %v",
			solarRestartCooldown, gotCooldown)
	}
}

func TestSolarTick_RestartCooldown(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.solarCooldownUntil = now.Add(50 * time.Second)

	svc.solarTick(context.Background())
	if svc.state != StateIdle || !svc.solarSurplusSince.IsZero() {
		t.Fatalf("restart cooldown must not accumulate qualification, state=%s since=%s", svc.state, svc.solarSurplusSince)
	}

	now = now.Add(51 * time.Second)
	svc.solarTick(context.Background())
	for range 30 {
		now = now.Add(time.Second)
		svc.solarTick(context.Background())
	}
	if svc.state != StateSolarCharging {
		t.Errorf("expected solar_charging after cooldown and sustained surplus, got %s", svc.state)
	}
}

func TestSolarTick_MeterFailureStopsWithoutLosingTrade(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.CurrentPower = 75
	meter := NewMockMeter(true, 75)
	meter.ActivePowerErr = errors.New("meter offline")
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-time.Minute)
	svc.currentTradeSOC = 50
	svc.solarChargePower = 75
	svc.solarMeasuredChargePowerW = 75
	svc.solarLastUpdate = now.Add(-time.Minute)

	svc.solarTick(context.Background())
	now = now.Add(solarTelemetryFailureThreshold)
	svc.solarTick(context.Background())
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Fatalf("meter outage: state=%s power=%d, want idle", svc.state, battery.CurrentPower)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one completed solar trade, got %+v", history)
	}
}

func TestSolarTick_BridgesBriefDipsAtMinimumPower(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.CurrentPower = 100
	meter := NewMockMeter(true, 100)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 50
	svc.solarChargePower = 100
	svc.solarSurplusEMA = 50

	// The meter follows actual battery consumption: no underlying solar surplus.
	tick := func() {
		meter.SetActivePowerW(float64(battery.CurrentPower))
		svc.solarTick(context.Background())
	}
	tick()
	if svc.state != StateSolarCharging || battery.CurrentPower != 75 {
		t.Fatalf("dip: state=%s power=%d, want charging at 75W", svc.state, battery.CurrentPower)
	}
	now = now.Add(59 * time.Second)
	tick()
	if svc.state != StateSolarCharging {
		t.Fatal("stopped before the 60-second grace expired")
	}
	// Real recovery clears the grace; a later dip gets its own full interval.
	now = now.Add(time.Second)
	meter.SetActivePowerW(-2000)
	svc.solarTick(context.Background())
	now = now.Add(time.Second)
	meter.SetActivePowerW(float64(battery.CurrentPower) + 2000)
	svc.solarTick(context.Background())
	now = now.Add(59 * time.Second)
	tick()
	if svc.state != StateSolarCharging {
		t.Fatal("recovered dip reused the previous grace deadline")
	}
	now = now.Add(time.Second)
	tick()
	if svc.state != StateIdle || battery.CurrentPower != 0 {
		t.Fatalf("sustained deficit: state=%s power=%d, want idle", svc.state, battery.CurrentPower)
	}
}

func TestSolarTick_EMAUsesElapsedSampleInterval(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	meter := NewMockMeter(true, -2000)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateSolarCharging
	svc.currentTradeStart = now.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarSurplusEMA = 500
	svc.solarEMALastSampleAt = now
	svc.lastPassiveRefresh = now

	now = now.Add(10 * time.Second)
	svc.solarTick(context.Background())

	alpha := 1 - math.Pow(1-solarEMAAlpha, 10)
	expectedEMA := 500 + alpha*(2500-500)
	if math.Abs(svc.solarSurplusEMA-expectedEMA) > 0.001 {
		t.Errorf("EMA after 10 s = %.6f, want %.6f", svc.solarSurplusEMA, expectedEMA)
	}
}

func TestSolarTick_YieldsDirectlyToDischargeWindow(t *testing.T) {
	// Verify solarTick() itself stops solar charging when a discharge window starts
	// (not relying on tick() which runs every 60s)

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(
		baseTime,
		0.05, // slot 0 - charge window
		0.15, // slot 1 - middle
		0.25, // slot 2 - discharge window
		0.10, // slot 3 - middle
	)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(80)
	meter := NewMockMeter(true, -500) // still has surplus

	// Time is in discharge window (slot 2)
	clockTime := baseTime.Add(2 * 15 * time.Minute)
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	svc.state = StateSolarCharging
	svc.currentTradeStart = clockTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 75
	svc.solarChargePower = 500
	svc.solarLastUpdate = clockTime.Add(-5 * time.Minute)

	ctx := context.Background()
	svc.solarTick(ctx) // solarTick should yield directly

	if svc.state != StateDischarging || mockBattery.CurrentPower != -cfg.DischargePowerW {
		t.Errorf("expected direct discharge after solar yield: state=%s power=%d", svc.state, mockBattery.CurrentPower)
	}
}

func TestTelegramManualDischargeAndAuto(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	cfg := testConfigSmallBattery()
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(cfg, battery, prices, currentTime)
	svc.nowFunc = func() time.Time { return currentTime }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())

	if svc.state != StateManualDischarging {
		t.Fatalf("expected manual discharge state, got %s", svc.state)
	}
	if len(battery.DischargeCalls) != 1 || battery.DischargeCalls[0].PowerW != cfg.DischargePowerW {
		t.Fatalf("expected one %d W discharge command, got %+v", cfg.DischargePowerW, battery.DischargeCalls)
	}
	expectedDeadline := currentTime.Add(manualOverrideMaxDuration)
	if !svc.manualOverrideUntil.Equal(expectedDeadline) {
		t.Errorf("expected deadline %s, got %s", expectedDeadline, svc.manualOverrideUntil)
	}
	if len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "Manual discharge active") {
		t.Fatalf("expected manual discharge confirmation, got %v", notifier.Messages)
	}
	status := svc.GetCurrentStatus(context.Background())
	if status.State != StateManualDischarging || !strings.Contains(status.NextAction, "/auto") {
		t.Errorf("expected manual override in status, got state=%s next_action=%q", status.State, status.NextAction)
	}

	currentTime = currentTime.Add(30 * time.Minute)
	battery.SOC = 75
	notifier.Commands = []string{"/auto"}
	svc.handleTelegramCommands(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected automatic control to restore idle state, got %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command, got %d", battery.IdleCalls)
	}
	if len(notifier.Messages) != 2 || !strings.Contains(notifier.Messages[1], "Automatic trading and solar control are active again") {
		t.Fatalf("expected automatic-control confirmation, got %v", notifier.Messages)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one recorded manual discharge, got %+v", history.Days)
	}
	trade := history.Days[0].Trades[0]
	if trade.Action != ActionDischarge || trade.PowerW != cfg.DischargePowerW || trade.StartSOC != 80 || trade.EndSOC != 75 {
		t.Errorf("unexpected recorded trade: %+v", trade)
	}
	if !trade.PriceEUR.Equal(decimal.RequireFromString("0.155")) {
		t.Errorf("expected time-weighted average price 0.155, got %s", trade.PriceEUR)
	}
}

func TestTelegramAutoFailureInvalidatesBatteryCache(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(80)
	battery.GetStatusErr = errors.New("telemetry unavailable")
	notifier := &MockNotifier{Commands: []string{"/auto", "/status"}}
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.telegram = notifier
	svc.state = StateManualDischarging
	svc.currentTradeStart = now.Add(-time.Minute)
	svc.currentTradeSOC = 80
	svc.currentTradeLastSOC = 75
	svc.currentTradePowerW = 800
	svc.batteryTelemetryAvailable = true
	svc.batteryTelemetrySOC = 80

	svc.handleTelegramCommands(context.Background())

	if len(notifier.StatusCalls) != 1 || notifier.StatusCalls[0].BatteryAvailable {
		t.Fatalf("status after failed /auto telemetry read = %+v", notifier.StatusCalls)
	}
}

func TestTelegramPollFailureUsesIndependentWarningCooldown(t *testing.T) {
	notifier := &MockNotifier{PollErr: errors.New("persist update offset: disk unavailable")}
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())
	if !svc.lastTelegramPollWarning.Equal(now) || len(notifier.ErrorCalls) != 0 {
		t.Fatalf("poll warning state = %s, outbound alerts = %v", svc.lastTelegramPollWarning, notifier.ErrorCalls)
	}
	now = now.Add(time.Minute)
	svc.handleTelegramCommands(context.Background())
	if !svc.lastTelegramPollWarning.Equal(now.Add(-time.Minute)) {
		t.Fatalf("poll warning cooldown reset early: %s", svc.lastTelegramPollWarning)
	}
}

func TestTelegramManualDischargeCustomPowerStopsAtSafetyTimeout(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge 800"}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, currentTime)
	svc.nowFunc = func() time.Time { return currentTime }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())
	if svc.state != StateManualDischarging {
		t.Fatalf("expected manual discharge state, got %s", svc.state)
	}
	if len(battery.DischargeCalls) != 1 || battery.DischargeCalls[0].PowerW != 800 {
		t.Fatalf("expected one 800 W discharge command, got %+v", battery.DischargeCalls)
	}

	currentTime = currentTime.Add(manualOverrideMaxDuration)
	battery.SOC = 70
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected safety timeout to restore idle state, got %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command at safety timeout, got %d", battery.IdleCalls)
	}
}

func TestTelegramManualDischargeRejectsUnsafeRequests(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.15, 0.16, 0.17, 0.18)

	t.Run("power below battery limit", func(t *testing.T) {
		battery := NewMockBattery(80)
		notifier := &MockNotifier{Commands: []string{"/discharge 799"}}
		svc := newTestService(testConfigSmallBattery(), battery, prices, baseTime)
		svc.telegram = notifier

		svc.handleTelegramCommands(context.Background())

		if svc.state != StateIdle || len(battery.DischargeCalls) != 0 {
			t.Fatalf("expected unsafe power to be rejected, state=%s calls=%v", svc.state, battery.DischargeCalls)
		}
		if len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "between 800 W and 2500 W") {
			t.Fatalf("expected power-range response, got %v", notifier.Messages)
		}
	})

	t.Run("battery at minimum SOC", func(t *testing.T) {
		battery := NewMockBattery(11)
		notifier := &MockNotifier{Commands: []string{"/discharge"}}
		svc := newTestService(testConfigSmallBattery(), battery, prices, baseTime)
		svc.telegram = notifier

		svc.handleTelegramCommands(context.Background())

		if svc.state != StateIdle || len(battery.DischargeCalls) != 0 {
			t.Fatalf("expected minimum SOC to block discharge, state=%s calls=%v", svc.state, battery.DischargeCalls)
		}
		if len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "minimum 11%") {
			t.Fatalf("expected minimum-SOC response, got %v", notifier.Messages)
		}
	})

	t.Run("battery status unavailable invalidates cache", func(t *testing.T) {
		battery := NewMockBattery(80)
		battery.GetStatusErr = errors.New("telemetry unavailable")
		notifier := &MockNotifier{Commands: []string{"/discharge", "/status"}}
		svc := newTestService(testConfigSmallBattery(), battery, prices, baseTime)
		svc.telegram = notifier
		svc.batteryTelemetryAvailable = true
		svc.batteryTelemetrySOC = 80

		svc.handleTelegramCommands(context.Background())

		if status := svc.GetCurrentStatus(context.Background()); status.BatteryAvailable {
			t.Fatalf("failed command-path battery read left stale telemetry available: %+v", status)
		}
	})
}

func TestManualDischargeStopsWhenTelemetryFails(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, currentTime)
	svc.nowFunc = func() time.Time { return currentTime }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())
	currentTime = currentTime.Add(time.Minute)
	battery.SOC = 70
	svc.tick(context.Background())

	battery.GetStatusErr = errors.New("telemetry unavailable")
	currentTime = currentTime.Add(time.Minute)
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected telemetry failure to stop manual discharge, got %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command after telemetry failure, got %d", battery.IdleCalls)
	}
	if len(notifier.ErrorCalls) == 0 {
		t.Fatal("expected telemetry failure notification")
	}
	if status := svc.GetCurrentStatus(context.Background()); status.BatteryAvailable {
		t.Fatalf("failed battery read left cached telemetry available: %+v", status)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
		t.Fatalf("expected one recorded manual discharge, got %+v", history.Days)
	}
	trade := history.Days[0].Trades[0]
	if trade.EndSOC != 70 || !trade.EnergyKWh.IsPositive() {
		t.Errorf("expected last known SOC and non-zero energy, got %+v", trade)
	}
}

func TestManualDischargeStopsWhenRefreshFails(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	cfg := testConfigSmallBattery()
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(cfg, battery, prices, currentTime)
	svc.nowFunc = func() time.Time { return currentTime }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())
	battery.PassiveErr = errors.New("refresh failed")
	currentTime = currentTime.Add(time.Duration(cfg.PassiveModeTimeoutS) * time.Second)
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected refresh failure to stop manual discharge, got %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command after refresh failure, got %d", battery.IdleCalls)
	}
}

func TestManualDischargeStopsAtRuntimeMinimumSOC(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, currentTime)
	svc.nowFunc = func() time.Time { return currentTime }
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())
	battery.SOC = 11
	currentTime = currentTime.Add(time.Minute)
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected minimum SOC to stop manual discharge, got %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command at minimum SOC, got %d", battery.IdleCalls)
	}
}

func TestTelegramManualDischargeStartsWithoutCurrentPrice(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(testConfigSmallBattery(), battery, nil, currentTime)
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())

	if svc.state != StateManualDischarging {
		t.Fatalf("expected manual discharge without price data, got %s", svc.state)
	}
	if len(battery.DischargeCalls) != 1 {
		t.Fatalf("expected one discharge command, got %+v", battery.DischargeCalls)
	}
}

func TestTelegramControlCommandBurstUsesLatestCommand(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge 800", "/auto", "/discharge 1500"}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, currentTime)
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())

	if svc.state != StateManualDischarging {
		t.Fatalf("expected latest command to start manual discharge, got %s", svc.state)
	}
	if len(battery.DischargeCalls) != 1 || battery.DischargeCalls[0].PowerW != 1500 {
		t.Fatalf("expected only the latest 1500 W command, got %+v", battery.DischargeCalls)
	}
	if battery.IdleCalls != 0 {
		t.Fatalf("expected intermediate /auto to be collapsed, got %d idle calls", battery.IdleCalls)
	}
}

func TestTelegramAutoReportsUnconfirmedStop(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	battery := NewMockBattery(80)
	battery.IdleErr = errors.New("stop failed")
	notifier := &MockNotifier{Commands: []string{"/auto"}}
	svc := newTestService(testConfigSmallBattery(), battery, nil, currentTime)
	svc.state = StateStopping
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())

	if svc.state != StateStopping {
		t.Fatalf("expected failed stop to remain stopping, got %s", svc.state)
	}
	if len(notifier.Messages) != 1 || !strings.Contains(notifier.Messages[0], "not yet confirmed") {
		t.Fatalf("expected unconfirmed-stop response, got %v", notifier.Messages)
	}
}

func TestTelegramManualDischargeStopsPreviousOperationBeforeStarting(t *testing.T) {
	currentTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(currentTime, 0.15, 0.16, 0.17, 0.18)
	battery := NewMockBattery(80)
	notifier := &MockNotifier{Commands: []string{"/discharge"}}
	svc := newTestService(testConfigSmallBattery(), battery, prices, currentTime)
	svc.state = StateCharging
	svc.currentTradeStart = currentTime.Add(-time.Minute)
	svc.currentTradeSOC = 79
	svc.currentTradePowerW = 2000
	svc.telegram = notifier

	svc.handleTelegramCommands(context.Background())

	if svc.state != StateManualDischarging {
		t.Fatalf("expected manual discharge after stopping scheduled charge, got %s", svc.state)
	}
	if battery.IdleCalls != 1 || len(battery.DischargeCalls) != 1 {
		t.Fatalf("expected one stop then one discharge, idle=%d discharge=%+v", battery.IdleCalls, battery.DischargeCalls)
	}
	if battery.StatusCalls != 2 {
		t.Fatalf("expected fresh status after stopping previous operation, got %d reads", battery.StatusCalls)
	}
}

// --- RS485 link freeze detection ---

func linkDownErr() error {
	return fmt.Errorf("telemetry frozen for 3m0s: %w", marstek.ErrLinkDown)
}

func newLinkDownDischargeService(t *testing.T, battery *MockBattery, notifier *MockNotifier, now *time.Time) *Service {
	t.Helper()
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)
	*now = baseTime.Add(3 * 15 * time.Minute) // in discharge window

	svc := newTestService(testConfigSmallBattery(), battery, prices, *now)
	svc.nowFunc = func() time.Time { return *now }
	svc.telegram = notifier
	svc.state = StateDischarging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 80
	svc.currentTradePowerW = 2200
	svc.lastPassiveRefresh = *now
	return svc
}

func TestTick_LinkDownDuringDischargeNotifiesOnce(t *testing.T) {
	battery := NewMockBattery(64)
	battery.checkLinkErr = linkDownErr()
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)

	ctx := context.Background()
	svc.tick(ctx)

	if len(notifier.ErrorCalls) != 1 {
		t.Fatalf("expected exactly one link down notification, got %d: %v", len(notifier.ErrorCalls), notifier.ErrorCalls)
	}
	msg := notifier.ErrorCalls[0]
	if !strings.Contains(msg, "link is down") || !strings.Contains(msg, "power-cycle") {
		t.Errorf("unexpected link down message: %q", msg)
	}

	// A second tick a minute later must be swallowed by the 15 minute limiter.
	now = now.Add(time.Minute)
	svc.tick(ctx)
	if len(notifier.ErrorCalls) != 1 {
		t.Fatalf("expected link down notification to be rate limited, got %v", notifier.ErrorCalls)
	}

	// The link down limiter must not consume notifyError's limiter.
	svc.notifyError(ctx, "unrelated failure")
	if len(notifier.ErrorCalls) != 2 || notifier.ErrorCalls[1] != "unrelated failure" {
		t.Fatalf("expected unrelated error notification to be sent, got %v", notifier.ErrorCalls)
	}
}

func TestTick_LinkDownRestartsBridgeAndRetriesPendingStop(t *testing.T) {
	battery := NewMockBattery(64)
	battery.checkLinkErr = linkDownErr()
	battery.restartAvailable = true
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)
	// A stop already failed with a dead link one minute ago: without a bridge
	// restart the retry would wait the 5 minute link-down interval.
	svc.state = StateDischarging
	svc.lastStopLinkDown = true
	svc.lastStopAttempt = now.Add(-time.Minute)
	battery.SOC = 5 // below min SOC, so this tick tries to stop again

	ctx := context.Background()
	// The first link-down tick only confirms and notifies; hardware is not rebooted
	// until a second consecutive tick still reports the link down.
	svc.tick(ctx)
	if battery.restartCalls != 0 {
		t.Fatalf("expected no bridge restart on the first link-down tick, got %d", battery.restartCalls)
	}

	now = now.Add(time.Minute)
	svc.tick(ctx)

	if battery.restartCalls != 1 {
		t.Fatalf("expected one bridge restart, got %d", battery.restartCalls)
	}
	if svc.state != StateIdle {
		t.Fatalf("expected pending stop to be retried immediately after restart, got state %s", svc.state)
	}
	if battery.IdleCalls != 1 {
		t.Errorf("expected one idle command, got %d", battery.IdleCalls)
	}

	// A second link down within 10 minutes must not restart the bridge again.
	svc.state = StateDischarging
	svc.currentTradeStart = now.Add(-time.Hour)
	svc.currentTradeSOC = 80
	battery.SOC = 64
	now = now.Add(time.Minute)
	svc.tick(ctx)
	if battery.restartCalls != 1 {
		t.Fatalf("expected bridge restart to be rate limited, got %d calls", battery.restartCalls)
	}
}

func TestTick_LinkNotCheckedWhenIdle(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.11, 0.12, 0.11) // not profitable, stays idle
	battery := NewMockBattery(50)
	battery.checkLinkErr = linkDownErr()
	svc := newTestService(testConfig(), battery, prices, baseTime)

	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("expected idle state, got %s", svc.state)
	}
	if battery.checkLinkCalls != 0 {
		t.Errorf("expected no link checks while idle, got %d", battery.checkLinkCalls)
	}
}

func TestRefreshPassiveModeUsesRefresher(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22)
	cfg := testConfigSmallBattery()
	battery := NewMockBattery(50)
	now := baseTime
	svc := newTestService(cfg, battery, prices, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateCharging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 50
	svc.lastPassiveRefresh = now

	ctx := context.Background()
	// Before 80% of the passive mode timeout, nothing is refreshed.
	now = now.Add(time.Duration(float64(cfg.PassiveModeTimeoutS)*0.5) * time.Second)
	svc.tick(ctx)
	if len(battery.refreshCalls) != 0 {
		t.Fatalf("expected no refresh before the 80%% threshold, got %v", battery.refreshCalls)
	}

	// Past the threshold, the refresher is used instead of a blind passive write.
	now = now.Add(time.Duration(float64(cfg.PassiveModeTimeoutS)*0.4) * time.Second)
	svc.tick(ctx)
	if len(battery.refreshCalls) != 1 || battery.refreshCalls[0] != -cfg.ChargePowerW {
		t.Fatalf("expected one refresh at %d W, got %v", -cfg.ChargePowerW, battery.refreshCalls)
	}
	if battery.passiveCalls != 0 {
		t.Errorf("expected SetPassiveModeContext not to be called, got %d calls", battery.passiveCalls)
	}
}

func TestCheckLinkDuringSession_RestartsOnSecondTick(t *testing.T) {
	battery := NewMockBattery(64)
	battery.checkLinkErr = linkDownErr()
	battery.restartAvailable = true
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)

	ctx := context.Background()
	svc.checkLinkDuringSession(ctx)
	if battery.restartCalls != 0 {
		t.Fatalf("expected no restart on the first link-down tick, got %d", battery.restartCalls)
	}
	if len(notifier.ErrorCalls) != 1 {
		t.Fatalf("expected the first tick to notify, got %v", notifier.ErrorCalls)
	}

	now = now.Add(time.Minute)
	svc.checkLinkDuringSession(ctx)
	if battery.restartCalls != 1 {
		t.Fatalf("expected one restart on the second link-down tick, got %d", battery.restartCalls)
	}
}

func TestTryRestartBridge_FailedRestartDoesNotConsumeLimiter(t *testing.T) {
	battery := NewMockBattery(64)
	battery.checkLinkErr = linkDownErr()
	battery.restartAvailable = true
	battery.restartErr = fmt.Errorf("boom")
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)

	ctx := context.Background()
	svc.checkLinkDuringSession(ctx) // first tick: confirm only
	now = now.Add(time.Minute)
	svc.checkLinkDuringSession(ctx)

	if battery.restartCalls != 1 {
		t.Fatalf("expected one restart attempt, got %d", battery.restartCalls)
	}
	if !svc.lastBridgeRestart.IsZero() {
		t.Fatalf("failed restart consumed the limiter: lastBridgeRestart = %v", svc.lastBridgeRestart)
	}

	// The next tick must try again instead of waiting out the 10 minute interval.
	now = now.Add(time.Minute)
	svc.checkLinkDuringSession(ctx)
	if battery.restartCalls != 2 {
		t.Fatalf("expected a second restart attempt, got %d", battery.restartCalls)
	}
}

func TestStopRetryDelay_NormalCadenceRightAfterBridgeRestart(t *testing.T) {
	battery := NewMockBattery(64)
	battery.restartAvailable = true
	notifier := &MockNotifier{}
	var now time.Time
	svc := newLinkDownDischargeService(t, battery, notifier, &now)
	svc.lastStopLinkDown = true

	if got := svc.stopRetryDelay(); got != batteryLinkDownStopRetryInterval {
		t.Fatalf("stopRetryDelay() before restart = %s, want %s", got, batteryLinkDownStopRetryInterval)
	}

	svc.tryRestartBridge(context.Background())
	if battery.restartCalls != 1 {
		t.Fatalf("expected one bridge restart, got %d", battery.restartCalls)
	}

	// tryRestartBridge clears lastStopLinkDown; a stop failing again on the same
	// tick must still retry at the normal cadence, not re-arm the long backoff.
	svc.lastStopLinkDown = true
	if got := svc.stopRetryDelay(); got != batteryStopRetryInterval {
		t.Fatalf("stopRetryDelay() after restart = %s, want %s", got, batteryStopRetryInterval)
	}
}

func TestTryRestartBridge_ClearsLinkDownStartCooldown(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.20, 0.22) // slot 0 is a charge window
	battery := NewMockBattery(50)
	battery.ChargeErr = fmt.Errorf("set charge mode: %w", marstek.ErrLinkDown)
	now := baseTime
	svc := newTestService(testConfigSmallBattery(), battery, prices, now)
	svc.nowFunc = func() time.Time { return now }

	ctx := context.Background()
	svc.tick(ctx)

	if svc.batteryCooldownUntil.IsZero() {
		t.Fatal("expected a link-down start failure to arm the battery cooldown")
	}
	if want := now.Add(batteryLinkDownCooldown); !svc.batteryCooldownUntil.Equal(want) {
		t.Fatalf("batteryCooldownUntil = %v, want %v", svc.batteryCooldownUntil, want)
	}

	battery.restartAvailable = true
	svc.tryRestartBridge(ctx)
	if battery.restartCalls != 1 {
		t.Fatalf("expected one bridge restart, got %d", battery.restartCalls)
	}
	if want := now.Add(bridgeRebootGrace); !svc.batteryCooldownUntil.Equal(want) {
		t.Fatalf("expected the restart to shrink the cooldown to the reboot grace %v, got %v", want, svc.batteryCooldownUntil)
	}
}

func TestMeasuredChargeSettlementPreservesZeroPrice(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0, 0, 0, 0), now)
	svc.nowFunc = func() time.Time { return now }
	setReservedChargePlan(svc, baseTime, decimal.Zero)

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.Zero, 50)
	svc.mu.Unlock()
	now = now.Add(30 * time.Second)
	svc.mu.Lock()
	svc.stopChargingLocked(context.Background(), 50)
	svc.mu.Unlock()

	history := svc.recorder.GetHistory()
	trade := history.Days[0].Trades[0]
	if !trade.PriceEUR.IsZero() {
		t.Fatalf("zero-priced session recorded price %s", trade.PriceEUR)
	}
	if !trade.UnpricedKWh.IsZero() {
		t.Fatalf("zero-priced session recorded unpriced energy %s", trade.UnpricedKWh)
	}
	if trade.EnergyKWh.LessThanOrEqual(decimal.Zero) {
		t.Fatalf("expected measured energy for short session, got %s", trade.EnergyKWh)
	}
}

func TestMeasuredChargeSettlementUsesPowerWhenSOCUnchanged(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.10, 0.10, 0.10, 0.10), now)
	svc.nowFunc = func() time.Time { return now }
	setReservedChargePlan(svc, baseTime, decimal.RequireFromString("0.10"))

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.RequireFromString("0.10"), 50)
	svc.mu.Unlock()
	now = now.Add(30 * time.Second)
	svc.mu.Lock()
	svc.stopChargingLocked(context.Background(), 50)
	svc.mu.Unlock()

	trade := svc.recorder.GetHistory().Days[0].Trades[0]
	wantEnergy := decimal.NewFromFloat(float64(svc.cfg.ChargePowerW) * 30 / 3_600_000)
	if !trade.EnergyKWh.Equal(wantEnergy) {
		t.Fatalf("measured short-session energy = %s, want %s", trade.EnergyKWh, wantEnergy)
	}
	if trade.EnergyBasis != measuredBatteryPowerEnergyBasis {
		t.Fatalf("energy basis = %q, want %q", trade.EnergyBasis, measuredBatteryPowerEnergyBasis)
	}
}

func TestSolarStopIntentPersistsAfterSurplusClears(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)
	svc := newTestServiceWithMeter(testConfigSmallBattery(), battery, meter, makePrices(baseTime, 0.10, 0.10, 0.10, 0.10), now)
	svc.nowFunc = func() time.Time { return now }

	svc.mu.Lock()
	svc.startSolarChargingLocked(context.Background(), 500, 50)
	battery.IdleFailures = 1
	svc.stopSolarChargingLocked(context.Background(), 50, solarStopReasonSurplusGone)
	stopping := svc.stopPending && svc.state == StateSolarCharging
	svc.mu.Unlock()
	if !stopping {
		t.Fatalf("failed solar stop lost durable intent: state=%s pending=%t", svc.state, svc.stopPending)
	}

	meter.SetActivePowerW(300) // The surplus trigger clears before the retry.
	now = now.Add(batteryStopRetryInterval)
	svc.solarTick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("durable solar stop did not settle after trigger cleared: %s", svc.state)
	}
	if got := len(svc.recorder.GetHistory().Days[0].Trades); got != 1 {
		t.Fatalf("recorded solar trades = %d, want 1", got)
	}
}

func TestShutdownSettlesActiveMeasuredTradeExactlyOnce(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := baseTime
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(baseTime, 0.10, 0.10, 0.10, 0.10), now)
	svc.nowFunc = func() time.Time { return now }
	setReservedChargePlan(svc, baseTime, decimal.RequireFromString("0.10"))

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.RequireFromString("0.10"), 50)
	svc.mu.Unlock()
	now = now.Add(30 * time.Second)
	if err := svc.stopBatteryOnShutdown(); err != nil {
		t.Fatalf("shutdown stop: %v", err)
	}
	if svc.state != StateIdle {
		t.Fatalf("shutdown state = %s, want idle", svc.state)
	}
	if err := svc.stopBatteryOnShutdown(); err != nil {
		t.Fatalf("second shutdown stop: %v", err)
	}
	trades := svc.recorder.GetHistory().Days[0].Trades
	if len(trades) != 1 {
		t.Fatalf("recorded trades after repeated shutdown = %d, want 1", len(trades))
	}
	if trades[0].EnergyKWh.LessThanOrEqual(decimal.Zero) {
		t.Fatalf("shutdown trade did not include measured energy: %s", trades[0].EnergyKWh)
	}
}

func TestCurrentStatusUsesCachedTelemetryAndExposesReservation(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(
		testConfigSmallBattery(),
		battery,
		makePrices(baseTime, 0.05, 0.10, 0.25, 0.20),
		baseTime,
	)
	svc.mu.Lock()
	svc.cacheBatteryTelemetryLocked(50, 375)
	svc.mu.Unlock()

	status := svc.GetCurrentStatus(context.Background())
	if battery.ESCalls != 0 || battery.StatusCalls != 0 || battery.PowerCalls != 0 {
		t.Fatalf("status performed battery I/O: ES=%d status=%d power=%d", battery.ESCalls, battery.StatusCalls, battery.PowerCalls)
	}
	if !status.BatteryAvailable || status.BatteryObservedAt.IsZero() {
		t.Fatalf("cached telemetry not exposed: %+v", status)
	}
	if status.ChargeReservation == nil {
		t.Fatal("expected charge reservation from cached SOC")
	}
	if !status.CurrentPriceKnown {
		t.Fatal("expected known current price")
	}
}

func TestFetchTomorrowPricesStagesCrossDayPlanDuringActiveCycle(t *testing.T) {
	now := time.Date(2024, 1, 15, 23, 45, 0, 0, time.UTC)
	today := makePrices(now, 0.05)
	tomorrow := makePrices(now.Add(15*time.Minute), 0.25)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, today, now)
	svc.state = StateCharging
	activePlan := svc.currentPlan
	svc.nordpool = &MockPriceProvider{TomorrowPrices: tomorrow}

	if err := svc.fetchTomorrowPrices(context.Background()); err != nil {
		t.Fatalf("fetchTomorrowPrices() error = %v", err)
	}
	if svc.currentPlan != activePlan {
		t.Fatal("tomorrow update replaced the active cycle plan")
	}
	if svc.pendingPlan == nil ||
		!svc.pendingPlan.IsInChargeWindow(now) ||
		!svc.pendingPlan.IsInDischargeWindow(now.Add(15*time.Minute)) {
		t.Fatalf("staged plan does not retain the cross-day charge/discharge cycle: %+v", svc.pendingPlan)
	}
}

func TestEarlyReservationRetainsPairedDischargeWhileIdle(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(now, 0.05, 0.06, 0.30, 0.30, 0.30), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), .90, time.UTC)
	activePlan := &TradingPlan{
		IsProfitable: true,
		ChargeWindows: []TimeWindow{{
			Start: now.Add(30 * time.Minute),
			End:   now.Add(45 * time.Minute),
		}},
		DischargeWindows: []TimeWindow{{
			Start: now.Add(60 * time.Minute),
			End:   now.Add(75 * time.Minute),
		}},
		Cycles: []TradeCycle{{
			ChargeWindow: TimeWindow{
				Start: now.Add(30 * time.Minute),
				End:   now.Add(45 * time.Minute),
				Price: decimal.NewFromFloat(.06),
			},
			DischargeWindow: TimeWindow{
				Start: now.Add(60 * time.Minute),
				End:   now.Add(75 * time.Minute),
				Price: decimal.NewFromFloat(.30),
			},
		}},
	}
	svc.currentPlan = activePlan

	svc.tick(context.Background())
	if svc.state != StateCharging {
		t.Fatalf("early reservation did not start charging: %s", svc.state)
	}
	if svc.automaticCycleCommit == nil ||
		!svc.automaticCycleCommit.DischargeWindow.End.Equal(activePlan.Cycles[0].DischargeWindow.End) {
		t.Fatalf("commitment = %+v, want discharge end %s", svc.automaticCycleCommit, activePlan.Cycles[0].DischargeWindow.End)
	}
	persisted, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted == nil || !persisted.DischargeWindow.End.Equal(activePlan.Cycles[0].DischargeWindow.End) {
		t.Fatalf("persisted commitment = %+v, error = %v", persisted, err)
	}

	pendingPlan := &TradingPlan{IsProfitable: true}
	svc.pendingPlan = pendingPlan
	now = now.Add(5 * time.Minute)
	battery.SOC = 100
	svc.tick(context.Background())

	if svc.state != StateIdle {
		t.Fatalf("state = %s, want idle", svc.state)
	}
	if svc.currentPlan != activePlan || svc.pendingPlan != pendingPlan {
		t.Fatal("idle transition before the planned charge start replaced the paired discharge plan")
	}
}

func TestDurationUntilNextPriceBoundary(t *testing.T) {
	base := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		now  time.Time
		want time.Duration
	}{
		{now: base, want: 15 * time.Minute},
		{now: base.Add(7*time.Minute + 30*time.Second), want: 7*time.Minute + 30*time.Second},
		{now: base.Add(15*time.Minute - time.Nanosecond), want: time.Nanosecond},
	}
	for _, tt := range tests {
		if got := durationUntilNextPriceBoundary(tt.now); got != tt.want {
			t.Errorf("durationUntilNextPriceBoundary(%s) = %s, want %s", tt.now, got, tt.want)
		}
	}
}

func TestTickRechecksTimeAfterTelemetryCrossesTariffBoundary(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.StatusHook = func() { now = base.Add(15*time.Minute + time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), now)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = &TradingPlan{
		IsProfitable:  true,
		ChargeWindows: []TimeWindow{{Start: base, End: base.Add(30 * time.Minute)}},
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.05)},
			DischargeWindow: TimeWindow{Start: base.Add(45 * time.Minute), End: base.Add(time.Hour), Price: decimal.NewFromFloat(.40)},
		}},
	}

	svc.tick(context.Background())
	if len(battery.ChargeCalls) != 0 {
		t.Fatalf("stale pre-boundary price started charging: %+v", battery.ChargeCalls)
	}
}

func TestChargeStartStopsWhenCommandCrossesTariffBoundary(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(13 * time.Minute)
	battery := NewMockBattery(50)
	battery.ChargeHook = func() { now = base.Add(15*time.Minute + time.Second) }
	battery.IdleHook = func() { now = base.Add(15*time.Minute + 3*time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), now)
	svc.nowFunc = func() time.Time { return now }
	svc.recorder = NewRecorder(t.TempDir(), .90, time.UTC)
	svc.currentPlan = &TradingPlan{
		IsProfitable:  true,
		ChargeWindows: []TimeWindow{{Start: base, End: base.Add(30 * time.Minute)}},
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.05)},
			DischargeWindow: TimeWindow{Start: base.Add(45 * time.Minute), End: base.Add(time.Hour), Price: decimal.NewFromFloat(.40)},
		}},
	}

	svc.tick(context.Background())
	if svc.state != StateIdle || battery.IdleCalls != 1 || len(battery.ChargeCalls) != 1 {
		t.Fatalf("cross-boundary start was not stopped: state=%s idle=%d charge=%v", svc.state, battery.IdleCalls, battery.ChargeCalls)
	}
	commitment, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || commitment == nil {
		t.Fatalf("cross-boundary charge did not retain its conservative commitment: commitment=%+v error=%v", commitment, err)
	}
	if svc.automaticCycleCommit == nil {
		t.Fatal("cross-boundary charge did not retain its commitment in the live service")
	}
	if history := svc.recorder.GetHistory(); len(history.Days) != 1 || len(history.Days[0].Trades) != 1 || !history.Days[0].Trades[0].EnergyKWh.IsPositive() {
		t.Fatalf("cancelled charge energy was not recorded: %+v", history)
	}
}

func TestChargeStartSkipsReservationShorterThanControlBudget(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(99)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), base)
	setReservedChargePlan(svc, base, decimal.NewFromFloat(.05))

	svc.tick(context.Background())

	if battery.ChargeAttempts != 0 || svc.automaticCycleCommit != nil || svc.state != StateIdle {
		t.Fatalf("short reservation started control: attempts=%d commitment=%+v state=%s", battery.ChargeAttempts, svc.automaticCycleCommit, svc.state)
	}
}

func TestChargeStartDoesNotControlBatteryWhenCommitmentPersistenceFails(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), base)
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)

	svc.tick(context.Background())

	if battery.ChargeAttempts != 0 || svc.state != StateIdle || svc.automaticCycleCommit == nil || svc.automaticCycleCommitDurable {
		t.Fatalf("persistence failure was not retained fail-closed: attempts=%d state=%s commitment=%+v durable=%t", battery.ChargeAttempts, svc.state, svc.automaticCycleCommit, svc.automaticCycleCommitDurable)
	}
}

func TestUncertainCommitmentRetriesPersistenceBeforeCharge(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), base)
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)
	cycle := svc.currentPlan.Cycles[0]
	svc.automaticCycleCommit = &cycle
	svc.automaticCycleCommitDurable = false

	svc.tick(context.Background())

	if battery.ChargeAttempts != 0 || svc.state != StateIdle {
		t.Fatalf("uncertain commitment bypassed persistence retry: attempts=%d state=%s", battery.ChargeAttempts, svc.state)
	}
}

func TestChargeStartRetainsCommitmentWhenBatteryCommandFails(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.ChargeErr = errors.New("command outcome unknown")
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), base)
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)

	svc.tick(context.Background())

	persisted, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted == nil || svc.automaticCycleCommit == nil {
		t.Fatalf("failed command lost conservative commitment: persisted=%+v in_memory=%+v error=%v", persisted, svc.automaticCycleCommit, err)
	}
	if battery.ChargeAttempts != 1 || svc.state != StateIdle {
		t.Fatalf("failed command outcome: attempts=%d state=%s", battery.ChargeAttempts, svc.state)
	}
}

func TestChargeStartClearsNewCommitmentWhenControlWasNotAttempted(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.ChargeErr = fmt.Errorf("%w: %w", marstek.ErrControlNotAttempted, marstek.ErrLinkDown)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), base)
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)

	svc.tick(context.Background())

	persisted, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted != nil || svc.automaticCycleCommit != nil {
		t.Fatalf("rejected command retained commitment: persisted=%+v in_memory=%+v error=%v", persisted, svc.automaticCycleCommit, err)
	}
}

func TestChargeStartRetainsCommitmentWhenLinkDownOutcomeIsUnknown(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	battery.ChargeErr = fmt.Errorf("set charge mode: %w", marstek.ErrLinkDown)
	battery.IdleErr = errors.New("stop outcome unknown")
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .15, .25, .10), base)
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)

	svc.tick(context.Background())

	persisted, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted == nil || svc.automaticCycleCommit == nil {
		t.Fatalf("ambiguous link-down lost commitment: persisted=%+v in_memory=%+v error=%v", persisted, svc.automaticCycleCommit, err)
	}
	if svc.state != StateStopping {
		t.Fatalf("unconfirmed stop state = %s, want %s", svc.state, StateStopping)
	}
}

func TestChargeStartClearsCommitmentWhenReservationExpiresDuringPersistence(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .30, .30, .30), base)
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)
	setReservedChargePlan(svc, base, decimal.NewFromFloat(.05))
	nowCalls := 0
	svc.nowFunc = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return base
		}
		return base.Add(time.Hour)
	}

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), decimal.NewFromFloat(.05), 50)
	svc.mu.Unlock()

	persisted, err := svc.recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted != nil || svc.automaticCycleCommit != nil {
		t.Fatalf("unstarted commitment was not cleared: persisted=%+v in_memory=%+v error=%v", persisted, svc.automaticCycleCommit, err)
	}
	if battery.ChargeAttempts != 0 {
		t.Fatalf("expired reservation reached battery control: attempts=%d", battery.ChargeAttempts)
	}
}

func TestChargeStartRejectsDifferentCycleAfterPersistence(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base
	cycleA := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(15 * time.Minute), Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(30 * time.Minute), End: base.Add(45 * time.Minute), Price: decimal.NewFromFloat(.40)},
	}
	cycleB := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base.Add(time.Hour), End: base.Add(time.Hour + 15*time.Minute), Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(90 * time.Minute), End: base.Add(105 * time.Minute), Price: decimal.NewFromFloat(.40)},
	}
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40, .05, .40, .40, .40), now)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{cycleA, cycleB}}
	recorder := NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)
	recorder.syncDirectoryFn = func(string) error {
		now = cycleB.ChargeWindow.Start
		return nil
	}
	svc.recorder = recorder

	svc.mu.Lock()
	svc.startChargingLocked(context.Background(), cycleA.ChargeWindow.Price, 50)
	svc.mu.Unlock()

	if battery.ChargeAttempts != 0 {
		t.Fatalf("cycle B reservation started under cycle A commitment: attempts=%d", battery.ChargeAttempts)
	}
	if persisted, err := recorder.LoadAutomaticCycleCommitment(); err != nil || persisted != nil {
		t.Fatalf("obsolete cycle A commitment was not cleared: commitment=%+v error=%v", persisted, err)
	}
}

func TestChargeCommandContextEndsWithReservedWindow(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), base)
	svc.recorder = NewRecorder(t.TempDir(), svc.cfg.BatteryEfficiency, time.UTC)
	startedAt := time.Now()

	svc.tick(context.Background())

	if battery.ChargeDeadline.IsZero() {
		t.Fatal("grid charge command had no reservation deadline")
	}
	remaining := battery.ChargeDeadline.Sub(startedAt)
	if remaining <= 0 || remaining > 15*time.Minute {
		t.Fatalf("grid charge deadline offset = %s, want within active 15-minute reservation", remaining)
	}
}

func TestExpiredCommitmentCleanupFailureStillBlocksNewChargeOnPersistenceFailure(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .30, .30, .30), base)
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)
	svc.automaticCycleCommit = &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base.Add(-3 * time.Hour), End: base.Add(-2 * time.Hour)},
		DischargeWindow: TimeWindow{Start: base.Add(-time.Hour), End: base},
	}
	setReservedChargePlan(svc, base, decimal.NewFromFloat(.05))

	svc.tick(context.Background())

	if battery.ChargeAttempts != 0 || svc.state != StateIdle {
		t.Fatalf("stale cleanup failure bypassed new commitment persistence: attempts=%d state=%s", battery.ChargeAttempts, svc.state)
	}
}

func TestCleanupPendingCommitmentBlocksChargeWhenCleanupFails(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.NewFromFloat(.40)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	syncCalls := 0
	recorder.syncDirectoryFn = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected cleanup sync failure")
		}
		return nil
	}
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base, Value: .05}}, base)
	svc.recorder = recorder
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}}
	svc.automaticCycleCommit = cycle
	svc.automaticCycleCleanupPending = true

	svc.tick(context.Background())

	if battery.ChargeAttempts != 0 || !svc.automaticCycleCleanupPending {
		t.Fatalf("cleanup-pending cycle admitted charge: attempts=%d pending=%t", battery.ChargeAttempts, svc.automaticCycleCleanupPending)
	}
	if syncCalls != 1 {
		t.Fatalf("directory sync calls = %d, want cleanup only with no re-persist", syncCalls)
	}
}

func TestExpiredCommitmentCleanupFailureCannotAuthorizeDifferentDischarge(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Hour)
	battery := NewMockBattery(80)
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .40}}, now)
	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedDataDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("create blocked data path: %v", err)
	}
	svc.recorder = NewRecorder(blockedDataDir, svc.cfg.BatteryEfficiency, time.UTC)
	svc.automaticCycleCommit = &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: base.Add(time.Hour), End: base.Add(2 * time.Hour)},
	}
	svc.currentPlan = &TradingPlan{
		IsProfitable: true,
		DischargeWindows: []TimeWindow{{
			Start: now, End: now.Add(time.Hour), Price: decimal.NewFromFloat(.40),
		}},
	}

	svc.tick(context.Background())

	if len(battery.DischargeCalls) != 0 || svc.state != StateIdle {
		t.Fatalf("stale commitment authorized another discharge: calls=%v state=%s", battery.DischargeCalls, svc.state)
	}
}

func TestCompletedCommitmentCleanupFailureCannotRestartAfterSOCRebound(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(2*time.Hour + 5*time.Minute)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	syncCalls := 0
	recorder.syncDirectoryFn = func(string) error {
		syncCalls++
		return errors.New("injected directory sync failure")
	}
	battery := NewMockBattery(11)
	battery.CurrentPower = -2000
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base.Add(2 * time.Hour), Value: .30}}, now)
	svc.recorder = recorder
	svc.state = StateDischarging
	svc.currentPlan = &TradingPlan{IsProfitable: true, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	svc.automaticCycleCommit = cycle
	svc.automaticCycleCommitDurable = true
	svc.currentTradeStart = base.Add(2 * time.Hour)
	svc.currentTradeSOC = 80
	svc.currentTradeLastSOC = 11
	svc.currentTradePowerW = svc.cfg.DischargePowerW
	svc.beginMeasuredTradeLocked(-2000)

	svc.tick(context.Background())
	if svc.state != StateIdle || !svc.automaticCycleCleanupPending {
		t.Fatalf("failed completed-cycle cleanup state=%s pending=%t", svc.state, svc.automaticCycleCleanupPending)
	}
	battery.SOC = 50
	svc.tick(context.Background())
	if len(battery.DischargeCalls) != 0 || svc.state != StateIdle {
		t.Fatalf("completed cycle restarted after SOC rebound: calls=%v state=%s", battery.DischargeCalls, svc.state)
	}
	if syncCalls < 2 {
		t.Fatalf("commitment cleanup sync calls = %d, want retry", syncCalls)
	}
}

func TestUncommittedAutomaticDischargePersistsRetirementAtMinimumSOC(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(2*time.Hour + 5*time.Minute)
	window := TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.RequireFromString("0.30")}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(11), nil, now)
	svc.recorder = recorder
	svc.state = StateDischarging
	svc.currentPlan = &TradingPlan{IsProfitable: true, DischargeOnly: true, DischargeWindows: []TimeWindow{window}}
	svc.pendingPlan = &TradingPlan{IsProfitable: false}
	svc.currentTradeStart = now.Add(-time.Minute)
	svc.currentTradeSOC = 50
	svc.currentTradeLastSOC = 11
	svc.currentTradePowerW = 2000

	svc.mu.Lock()
	svc.stopDischargingLocked(context.Background(), 11)
	svc.mu.Unlock()

	if len(svc.retiredDischargeWindows) != 1 || !sameWindowPeriod(svc.retiredDischargeWindows[0], window) ||
		svc.currentPlan.IsInDischargeWindow(now) {
		t.Fatalf("uncommitted completion retirement state: retired=%+v plan=%+v", svc.retiredDischargeWindows, svc.currentPlan)
	}
	loaded, err := NewRecorder(recorder.dataDir, .90, time.UTC).LoadRetiredDischargeWindows()
	if err != nil || len(loaded) != 1 || !sameWindowPeriod(loaded[0], window) {
		t.Fatalf("persisted uncommitted retirement = %+v, error = %v", loaded, err)
	}
}

func TestRefreshCannotReintroduceCompletedWindowDuringCleanupRetry(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(base, .05, .40)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), prices, base.Add(16*time.Minute))
	if svc.currentPlan == nil || len(svc.currentPlan.Cycles) != 1 {
		t.Fatalf("test plan = %+v, want one cycle", svc.currentPlan)
	}
	completed := svc.currentPlan.Cycles[0]
	svc.automaticCycleCommit = &completed
	svc.automaticCycleCleanupPending = true
	svc.currentPlan = retireDischargeWindow(svc.currentPlan, completed.DischargeWindow)

	svc.refreshCurrentPlanLocked(svc.now())

	if svc.pendingPlan == nil {
		t.Fatal("cleanup-pending refresh did not stage a plan")
	}
	for _, window := range svc.pendingPlan.DischargeWindows {
		if sameWindowPeriod(window, completed.DischargeWindow) {
			t.Fatalf("completed discharge window was reintroduced: %+v", window)
		}
	}

	svc.automaticCycleCommit = nil
	svc.automaticCycleCleanupPending = false
	svc.retiredDischargeWindows = []TimeWindow{completed.DischargeWindow}
	svc.pendingPlan = nil
	svc.refreshCurrentPlanLocked(svc.now())
	for _, window := range svc.currentPlan.DischargeWindows {
		if sameWindowPeriod(window, completed.DischargeWindow) {
			t.Fatalf("successfully retired discharge window was reintroduced: %+v", window)
		}
	}
}

func TestTickStopsActiveChargeAfterTariffBoundary(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(15*time.Minute - time.Second)
	battery := NewMockBattery(50)
	battery.CurrentPower = 2000
	battery.StatusHook = func() { now = base.Add(15*time.Minute + time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateCharging
	svc.currentTradeStart = base
	svc.currentTradeSOC = 40
	svc.currentTradeLastSOC = 50
	svc.beginMeasuredTradeLocked(2000)
	svc.currentPlan = &TradingPlan{
		IsProfitable:  true,
		ChargeWindows: []TimeWindow{{Start: base, End: base.Add(30 * time.Minute)}},
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.05)},
			DischargeWindow: TimeWindow{Start: base.Add(45 * time.Minute), End: base.Add(time.Hour), Price: decimal.NewFromFloat(.40)},
		}},
	}

	svc.tick(context.Background())
	if svc.state != StateIdle || battery.IdleCalls != 1 {
		t.Fatalf("active charge crossed excluded tariff: state=%s idle_calls=%d", svc.state, battery.IdleCalls)
	}
}

func TestActiveChargeStopsWhenRefreshCrossesTariffBoundary(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(13 * time.Minute)
	battery := NewMockBattery(50)
	battery.CurrentPower = 2000
	battery.PassiveHook = func() { now = base.Add(15*time.Minute + time.Second) }
	svc := newTestService(testConfigSmallBattery(), battery, makePrices(base, .05, .40, .40, .40), now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateCharging
	svc.currentTradeStart = base
	svc.currentTradeSOC = 40
	svc.currentTradeLastSOC = 50
	svc.currentTradePowerW = svc.cfg.ChargePowerW
	svc.beginMeasuredTradeLocked(2000)
	svc.currentPlan = &TradingPlan{
		IsProfitable:  true,
		ChargeWindows: []TimeWindow{{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.05)}},
		DischargeWindows: []TimeWindow{{
			Start: base.Add(45 * time.Minute), End: base.Add(time.Hour), Price: decimal.NewFromFloat(.40),
		}},
		Cycles: []TradeCycle{{
			ChargeWindow:    TimeWindow{Start: base, End: base.Add(30 * time.Minute), Price: decimal.NewFromFloat(.05)},
			DischargeWindow: TimeWindow{Start: base.Add(45 * time.Minute), End: base.Add(time.Hour), Price: decimal.NewFromFloat(.40)},
		}},
	}

	svc.tick(context.Background())

	if svc.state != StateIdle || battery.IdleCalls != 1 {
		t.Fatalf("charge refresh crossed excluded tariff: state=%s idle_calls=%d", svc.state, battery.IdleCalls)
	}
	if battery.RefreshDeadline.IsZero() || time.Until(battery.RefreshDeadline) <= 0 || time.Until(battery.RefreshDeadline) > 2*time.Minute+time.Second {
		t.Fatalf("charge refresh deadline = %s, want reservation bound", battery.RefreshDeadline)
	}
}

func TestRestoreAutomaticCycleCommitment(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: decimal.NewFromFloat(.30)},
		Profit:          decimal.NewFromFloat(.17),
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("restore commitment: %v", err)
	}
	if svc.automaticCycleCommit == nil || svc.currentPlan == nil ||
		!svc.currentPlan.IsInDischargeWindow(cycle.DischargeWindow.Start) {
		t.Fatalf("committed cycle was not restored: commitment=%+v plan=%+v", svc.automaticCycleCommit, svc.currentPlan)
	}
}

func TestRestoreAutomaticCycleCommitmentBlocksFurtherChargeBelowCurrentFloor(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-5 * time.Minute), End: now.Add(10 * time.Minute), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.NewFromFloat(.16)},
		Profit:          decimal.NewFromFloat(.99), // Stored values are not trusted.
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("restore commitment: %v", err)
	}
	if svc.automaticCycleCommit == nil || svc.currentPlan == nil || len(svc.currentPlan.Cycles) != 0 {
		t.Fatalf("sub-threshold restored cycle remained charge-eligible: commitment=%+v plan=%+v", svc.automaticCycleCommit, svc.currentPlan)
	}
	if !svc.currentPlan.IsInDischargeWindow(cycle.DischargeWindow.Start) {
		t.Fatal("sub-threshold restored cycle lost its conservative discharge obligation")
	}
	if !svc.currentPlan.DischargeOnly || !svc.GetCurrentStatus(t.Context()).PlanDischargeOnly {
		t.Fatal("sub-threshold restored cycle was not exposed as discharge-only")
	}
}

func TestRestoreExpiredCommitmentDefersFailedCleanup(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(-time.Hour), End: now},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	syncCalls := 0
	recorder.syncDirectoryFn = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("transient expired cleanup stopped startup: %v", err)
	}
	if svc.automaticCycleCommit == nil || svc.automaticCycleCommitDurable {
		t.Fatalf("failed cleanup was not retained for retry: commitment=%+v durable=%t", svc.automaticCycleCommit, svc.automaticCycleCommitDurable)
	}
	svc.mu.Lock()
	svc.clearExpiredAutomaticCycleCommitmentLocked(context.Background(), now)
	svc.mu.Unlock()
	if svc.automaticCycleCommit != nil || syncCalls != 3 {
		t.Fatalf("deferred cleanup did not retry: commitment=%+v sync_calls=%d", svc.automaticCycleCommit, syncCalls)
	}
}

func TestRestoreClearsCommitmentForPersistedRetiredWindow(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now, Price: decimal.RequireFromString("0.10")},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: decimal.RequireFromString("0.30")},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveRetiredDischargeWindows([]TimeWindow{cycle.DischargeWindow}); err != nil {
		t.Fatalf("save retirement: %v", err)
	}
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save stale commitment: %v", err)
	}
	svc := &Service{
		cfg:                     testConfigSmallBattery(),
		recorder:                recorder,
		loc:                     time.UTC,
		nowFunc:                 func() time.Time { return now },
		retiredDischargeWindows: []TimeWindow{cycle.DischargeWindow},
	}

	if err := svc.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("restore retired commitment: %v", err)
	}
	if svc.automaticCycleCommit != nil || svc.currentPlan != nil {
		t.Fatalf("retired commitment was restored: commitment=%+v plan=%+v", svc.automaticCycleCommit, svc.currentPlan)
	}
	if persisted, err := recorder.LoadAutomaticCycleCommitment(); err != nil || persisted != nil {
		t.Fatalf("retired commitment file = %+v, error = %v", persisted, err)
	}
}

func TestRestoreAutomaticCycleCommitmentRejectsInvalidChronology(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err == nil {
		t.Fatal("invalid persisted cycle chronology was accepted")
	}
}

func TestRestoreAutomaticCycleCommitmentRejectsExpiredInvalidRecord(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(&TradeCycle{}); err != nil {
		t.Fatalf("save invalid commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err == nil {
		t.Fatal("expired structurally invalid commitment was silently discarded")
	}
}

func TestRestoreAutomaticCycleCommitmentRejectsImplausibleFutureWindow(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(100 * time.Hour), End: now.Add(101 * time.Hour)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err == nil {
		t.Fatal("implausibly distant persisted cycle was accepted")
	}
}

func TestStartIdlesBatteryWhenCommitmentRestoreFails(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, automaticCycleCommitmentFile), []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt commitment: %v", err)
	}
	battery := NewMockBattery(50)
	battery.RespectIdleContext = true
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.recorder = NewRecorder(dir, svc.cfg.BatteryEfficiency, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.Start(ctx); err == nil {
		t.Fatal("service started with corrupt automatic cycle commitment")
	}
	if !battery.ConnectCalled || battery.IdleAttempts != 2 || battery.IdleCalls != 1 {
		t.Fatalf("restore refusal did not retry safe idle independently of cancellation: connected=%t attempts=%d idle=%d", battery.ConnectCalled, battery.IdleAttempts, battery.IdleCalls)
	}
}

func TestStartIdlesBatteryWhenTradeHistoryLoadFailsWithCanceledContext(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trades.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt trade history: %v", err)
	}
	battery := NewMockBattery(50)
	battery.RespectIdleContext = true
	svc := newTestService(testConfigSmallBattery(), battery, nil, now)
	svc.recorder = NewRecorder(dir, svc.cfg.BatteryEfficiency, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.Start(ctx); err == nil {
		t.Fatal("service started with corrupt trade history")
	}
	if !battery.ConnectCalled || battery.IdleAttempts != 2 || battery.IdleCalls != 1 {
		t.Fatalf("history refusal did not retry safe idle independently of cancellation: connected=%t attempts=%d idle=%d", battery.ConnectCalled, battery.IdleAttempts, battery.IdleCalls)
	}
}

func TestStatusKeepsManualOverrideVisibleDuringCommitmentCleanup(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.state = StateManualDischarging
	svc.manualOverrideUntil = now.Add(time.Hour)
	svc.automaticCycleCommit = &TradeCycle{DischargeWindow: TimeWindow{End: now.Add(2 * time.Hour)}}
	svc.automaticCycleCleanupPending = true

	status := svc.GetCurrentStatus(context.Background())
	for _, want := range []string{"manual override until 13:00", "/auto to resume", "automatic cycle commitment cleanup pending"} {
		if !strings.Contains(status.NextAction, want) {
			t.Errorf("next action %q does not contain %q", status.NextAction, want)
		}
	}
}

func TestTelegramStatusPreservesUnavailableCurrentPrice(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.telegram = notifier

	svc.sendTelegramStatus(context.Background())

	if len(notifier.StatusCalls) != 1 || notifier.StatusCalls[0].CurrentPriceKnown {
		t.Fatalf("Telegram status did not preserve unavailable price: %+v", notifier.StatusCalls)
	}
}

func TestDailySummaryMarksUnpricedCashFlowIncomplete(t *testing.T) {
	now := time.Date(2024, 1, 15, 23, 59, 0, 0, time.UTC)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.telegram = notifier
	if err := svc.recorder.RecordTrade(Trade{
		Timestamp:   now.Add(-time.Hour),
		Action:      ActionDischarge,
		EnergyKWh:   decimal.NewFromInt(1),
		UnpricedKWh: decimal.NewFromFloat(.25),
		PriceEUR:    decimal.NewFromFloat(.30),
	}); err != nil {
		t.Fatalf("record unpriced trade: %v", err)
	}

	svc.checkDailySummary(context.Background())

	if len(notifier.DailySummaryCalls) != 1 {
		t.Fatalf("daily summary calls = %d, want 1", len(notifier.DailySummaryCalls))
	}
	got := notifier.DailySummaryCalls[0]
	if !got.PnLIncomplete || !got.TotalPnLIncomplete || math.Abs(got.UnpricedKWh-.25) > .000001 {
		t.Fatalf("daily summary incompleteness = %+v", got)
	}
}

func TestDailySummaryWaitsForDirtyTradeHistory(t *testing.T) {
	now := time.Date(2024, 1, 15, 23, 59, 0, 0, time.UTC)
	notifier := &MockNotifier{}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	recorder.syncDirectoryFn = func(string) error { return errors.New("storage unavailable") }
	if err := recorder.RecordTrade(Trade{Timestamp: now.Add(-time.Hour), Action: ActionCharge, EnergyKWh: decimal.NewFromInt(1)}); err == nil {
		t.Fatal("RecordTrade() error = nil, want dirty history")
	}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.telegram = notifier
	svc.recorder = recorder

	svc.checkDailySummary(context.Background())

	if len(notifier.DailySummaryCalls) != 0 || !svc.lastDailySummary.IsZero() {
		t.Fatalf("dirty history was summarized: calls=%d last=%s", len(notifier.DailySummaryCalls), svc.lastDailySummary)
	}
}

func TestRestoreAutomaticCycleCommitmentClearsExpiredFile(t *testing.T) {
	now := time.Date(2024, 1, 15, 4, 0, 0, 0, time.UTC)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-4 * time.Hour), End: now.Add(-3 * time.Hour)},
		DischargeWindow: TimeWindow{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	svc := &Service{cfg: testConfigSmallBattery(), recorder: recorder, loc: time.UTC, nowFunc: func() time.Time { return now }}

	if err := svc.restoreAutomaticCycleCommitment(); err != nil {
		t.Fatalf("restore expired commitment: %v", err)
	}
	if persisted, err := recorder.LoadAutomaticCycleCommitment(); err != nil || persisted != nil {
		t.Fatalf("expired commitment was not cleared: commitment=%+v error=%v", persisted, err)
	}
}

func TestTickClearsCommitmentWhenDischargeWindowExpiresUnused(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Hour)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	battery := NewMockBattery(11)
	battery.DischargFlag = false
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .10}}, now)
	svc.recorder = recorder
	svc.automaticCycleCommit = cycle
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	svc.pendingPlan = &TradingPlan{}

	svc.tick(context.Background())

	persisted, err := recorder.LoadAutomaticCycleCommitment()
	if err != nil || persisted != nil || svc.automaticCycleCommit != nil {
		t.Fatalf("elapsed unused commitment was not cleared: persisted=%+v in_memory=%+v error=%v", persisted, svc.automaticCycleCommit, err)
	}
}

func TestExpiredCommitmentCleanupRetriesDirectorySyncAndNotifies(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Hour)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	syncCalls := 0
	recorder.syncDirectoryFn = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	pendingPlan := &TradingPlan{IsProfitable: true}
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.recorder = recorder
	svc.telegram = notifier
	svc.automaticCycleCommit = cycle
	svc.automaticCycleCommitDurable = true
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}}
	svc.pendingPlan = pendingPlan

	svc.mu.Lock()
	svc.clearExpiredAutomaticCycleCommitmentLocked(context.Background(), now)
	svc.mu.Unlock()
	if svc.automaticCycleCommit == nil || svc.automaticCycleCommitDurable {
		t.Fatalf("failed cleanup did not retain fail-closed state: commitment=%+v durable=%t", svc.automaticCycleCommit, svc.automaticCycleCommitDurable)
	}
	if len(notifier.ErrorCalls) != 1 || !strings.Contains(notifier.ErrorCalls[0], "expired automatic cycle commitment") {
		t.Fatalf("cleanup notifications = %v, want one actionable alert", notifier.ErrorCalls)
	}
	status := svc.GetCurrentStatus(context.Background())
	if status.CommitmentType != "grid" || status.CommitmentDurable || !status.PlanPending ||
		status.CommitmentDischargeWindowEnd == nil || status.NextAction != "automatic cycle commitment cleanup pending" {
		t.Fatalf("cleanup failure not exposed in status: %+v", status)
	}

	svc.mu.Lock()
	svc.clearExpiredAutomaticCycleCommitmentLocked(context.Background(), now)
	svc.mu.Unlock()
	if svc.automaticCycleCommit != nil || svc.currentPlan != pendingPlan || svc.pendingPlan != nil {
		t.Fatalf("successful retry did not release commitment and promote plan: commitment=%+v current=%p pending=%p", svc.automaticCycleCommit, svc.currentPlan, svc.pendingPlan)
	}
	if syncCalls != 3 {
		t.Fatalf("directory sync calls = %d, want retry after absent deletion", syncCalls)
	}
}

func TestUnattemptedCycleDoesNotPinRefreshedPlan(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		state:   StateIdle,
		nowFunc: func() time.Time { return now },
		currentPlan: &TradingPlan{
			Cycles: []TradeCycle{{
				ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
				DischargeWindow: TimeWindow{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
			}},
		},
	}

	if svc.automaticCycleCommittedLocked() {
		t.Fatal("an unattempted plan cycle was treated as a purchased-energy commitment")
	}
}

func TestRefreshReturnsRetainedExecutablePlanWhenCandidateIsStaged(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	cycle := TradeCycle{
		ChargeWindow:    TimeWindow{Start: now.Add(-time.Hour), End: now},
		DischargeWindow: TimeWindow{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)},
	}
	activePlan := &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	svc := &Service{
		cfg:                  testConfigSmallBattery(),
		state:                StateIdle,
		nowFunc:              func() time.Time { return now },
		loc:                  time.UTC,
		currentPlan:          activePlan,
		automaticCycleCommit: &cycle,
	}

	if got := svc.refreshCurrentPlanLocked(now); got != activePlan {
		t.Fatalf("refresh returned staged candidate %p instead of executable plan %p", got, activePlan)
	}
	if svc.pendingPlan == nil {
		t.Fatal("refreshed candidate was not staged")
	}
}

func TestRefreshDoesNotReportStagedPlanWithoutExecutablePlan(t *testing.T) {
	now := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		cfg:     testConfigSmallBattery(),
		state:   StateIdle,
		nowFunc: func() time.Time { return now },
		loc:     time.UTC,
		todayPrices: []nordpool.Price{
			{Time: now, Value: .05},
			{Time: now.Add(15 * time.Minute), Value: .40},
		},
		automaticCycleCommit: &TradeCycle{
			ChargeWindow:    TimeWindow{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour)},
			DischargeWindow: TimeWindow{Start: now.Add(-time.Hour), End: now},
		},
	}

	if got := svc.refreshCurrentPlanLocked(now); got != nil {
		t.Fatalf("refresh returned staged plan %p with no executable plan", got)
	}
	if svc.currentPlan != nil || svc.pendingPlan == nil {
		t.Fatalf("refresh state: current=%p pending=%p, want nil current and staged pending", svc.currentPlan, svc.pendingPlan)
	}
}

func TestTickStopsExpiredActiveDischargeBeforePromotingOverlappingPlan(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Hour)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
	}
	recorder := NewRecorder(t.TempDir(), .90, time.UTC)
	if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
		t.Fatalf("save commitment: %v", err)
	}
	battery := NewMockBattery(50)
	battery.CurrentPower = -2000
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .30}}, now)
	svc.recorder = recorder
	svc.state = StateDischarging
	svc.automaticCycleCommit = cycle
	activePlan := &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	pendingPlan := &TradingPlan{IsProfitable: true, DischargeWindows: []TimeWindow{{Start: now, End: now.Add(time.Hour), Price: decimal.NewFromFloat(.40)}}}
	svc.currentPlan = activePlan
	svc.pendingPlan = pendingPlan
	svc.currentTradeStart = now.Add(-time.Hour)
	svc.currentTradeSOC = 80
	svc.currentTradeLastSOC = 50
	svc.currentTradePowerW = 2000
	svc.beginMeasuredTradeLocked(-2000)

	svc.tick(context.Background())

	if svc.state != StateIdle || battery.IdleCalls != 1 {
		t.Fatalf("expired active discharge continued under staged plan: state=%s idle_calls=%d", svc.state, battery.IdleCalls)
	}
	if svc.currentPlan != pendingPlan || svc.pendingPlan != nil {
		t.Fatalf("staged plan was not promoted after stopping: current=%p pending=%p", svc.currentPlan, svc.pendingPlan)
	}
}

func TestExpiredUnclearedCommitmentPinsPlanUntilCleanup(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Hour)
	cycle := &TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: now, Price: decimal.NewFromFloat(.30)},
	}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), []nordpool.Price{{Time: now, Value: .10}}, now)
	svc.state = StateDischarging
	svc.automaticCycleCommit = cycle
	activePlan := &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	pendingPlan := &TradingPlan{IsProfitable: true}
	svc.currentPlan = activePlan
	svc.pendingPlan = pendingPlan

	svc.mu.Lock()
	if !svc.transitionToIdleLocked(context.Background(), 50) {
		t.Fatal("failed to transition test service to idle")
	}
	svc.refreshCurrentPlanLocked(now)
	svc.mu.Unlock()

	if svc.currentPlan != activePlan || svc.pendingPlan == nil || svc.automaticCycleCommit == nil {
		t.Fatalf("uncleared commitment released active plan: current=%p active=%p pending=%p commitment=%+v", svc.currentPlan, activePlan, svc.pendingPlan, svc.automaticCycleCommit)
	}
}

func TestSolarChargedCycleSurvivesPlanRefresh(t *testing.T) {
	base := time.Date(2026, 9, 5, 23, 30, 0, 0, time.UTC)
	now := base.Add(15 * time.Minute)
	cycle := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: now, Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(30 * time.Minute), End: base.Add(45 * time.Minute), Price: decimal.NewFromFloat(.40)},
	}
	battery := NewMockBattery(50)
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .05}}, now)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}

	svc.mu.Lock()
	svc.startSolarChargingLocked(context.Background(), 500, 50)
	svc.stopSolarChargingLocked(context.Background(), 60, solarStopReasonSurplusGone)
	activePlan := svc.currentPlan
	now = cycle.DischargeWindow.Start
	svc.todayPrices = []nordpool.Price{{Time: now, Value: .40}}
	svc.refreshCurrentPlanLocked(now)
	svc.mu.Unlock()

	if svc.solarCycleRetention == nil || svc.currentPlan != activePlan || svc.pendingPlan == nil {
		t.Fatalf("solar-charged cycle was not retained: retention=%+v current=%p active=%p pending=%p", svc.solarCycleRetention, svc.currentPlan, activePlan, svc.pendingPlan)
	}
	svc.tick(context.Background())
	if svc.state != StateDischarging {
		t.Fatalf("retained solar-charged cycle did not discharge: state=%s", svc.state)
	}
}

func TestActiveSolarSessionAcquiresCycleRetentionAfterPlanAppears(t *testing.T) {
	base := time.Date(2026, 9, 5, 23, 30, 0, 0, time.UTC)
	now := base.Add(15 * time.Minute)
	cycle := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: now, Price: decimal.NewFromFloat(.05)},
		DischargeWindow: TimeWindow{Start: base.Add(30 * time.Minute), End: base.Add(45 * time.Minute), Price: decimal.NewFromFloat(.40)},
	}
	battery := NewMockBattery(50)
	battery.CurrentPower = 500
	svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: now, Value: .05}}, now)
	svc.meter = NewMockMeter(true, 0)
	svc.state = StateSolarCharging
	svc.solarChargePower = 500
	svc.solarLastUpdate = now
	svc.lastPassiveRefresh = now

	// A price refresh can first introduce the relevant cycle after the solar
	// command has already been confirmed.
	svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
	svc.solarTick(context.Background())

	if svc.solarCycleRetention == nil || !svc.solarCycleRetention.DischargeWindow.Start.Equal(cycle.DischargeWindow.Start) {
		t.Fatalf("active solar session did not retain newly available cycle: %+v", svc.solarCycleRetention)
	}
}

func TestAutomaticDischargeClearsCommitmentOnlyWhenCycleCompletes(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		now       time.Time
		endSOC    int
		wantClear bool
	}{
		{name: "interrupted mid-window", now: base.Add(2*time.Hour + 15*time.Minute), endSOC: 50},
		{name: "window completed", now: base.Add(3 * time.Hour), endSOC: 50, wantClear: true},
		{name: "minimum SOC reached", now: base.Add(2*time.Hour + 15*time.Minute), endSOC: 11, wantClear: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cycle := &TradeCycle{
				ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
				DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
			}
			recorder := NewRecorder(t.TempDir(), .90, time.UTC)
			if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
				t.Fatalf("save commitment: %v", err)
			}
			svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, tt.now)
			svc.recorder = recorder
			svc.nowFunc = func() time.Time { return tt.now }
			svc.state = StateDischarging
			svc.automaticCycleCommit = cycle
			svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
			svc.currentTradeStart = tt.now.Add(-time.Minute)
			svc.currentTradeSOC = 60
			svc.currentTradeLastSOC = tt.endSOC
			svc.currentTradePowerW = 2000
			svc.beginMeasuredTradeLocked(-2000)

			svc.mu.Lock()
			svc.stopDischargingLocked(context.Background(), tt.endSOC)
			svc.mu.Unlock()

			persisted, err := recorder.LoadAutomaticCycleCommitment()
			if err != nil {
				t.Fatalf("load commitment: %v", err)
			}
			if tt.wantClear && (persisted != nil || svc.automaticCycleCommit != nil) {
				t.Fatalf("completed cycle retained commitment: persisted=%+v in_memory=%+v", persisted, svc.automaticCycleCommit)
			}
			if !tt.wantClear && (persisted == nil || svc.automaticCycleCommit == nil) {
				t.Fatalf("interrupted cycle lost commitment: persisted=%+v in_memory=%+v", persisted, svc.automaticCycleCommit)
			}
		})
	}
}

func TestCompletedDischargeRetiresWindowBeforeSOCRebound(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(2*time.Hour + 15*time.Minute)
	completed := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
	}
	future := TradeCycle{
		ChargeWindow:    TimeWindow{Start: base.Add(4 * time.Hour), End: base.Add(5 * time.Hour), Price: decimal.NewFromFloat(.10)},
		DischargeWindow: TimeWindow{Start: base.Add(6 * time.Hour), End: base.Add(7 * time.Hour), Price: decimal.NewFromFloat(.30)},
	}
	for _, withPendingPlan := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", withPendingPlan), func(t *testing.T) {
			recorder := NewRecorder(t.TempDir(), .90, time.UTC)
			if err := recorder.SaveAutomaticCycleCommitment(&completed); err != nil {
				t.Fatalf("save commitment: %v", err)
			}
			battery := NewMockBattery(11)
			battery.CurrentPower = -2000
			svc := newTestService(testConfigSmallBattery(), battery, []nordpool.Price{{Time: base.Add(2 * time.Hour), Value: .30}}, now)
			svc.recorder = recorder
			svc.state = StateDischarging
			svc.automaticCycleCommit = &completed
			svc.automaticCycleCommitDurable = true
			svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{completed}, ChargeWindows: []TimeWindow{completed.ChargeWindow}, DischargeWindows: []TimeWindow{completed.DischargeWindow}}
			if withPendingPlan {
				svc.pendingPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{completed, future}, ChargeWindows: []TimeWindow{completed.ChargeWindow, future.ChargeWindow}, DischargeWindows: []TimeWindow{completed.DischargeWindow, future.DischargeWindow}}
			}
			svc.currentTradeStart = now.Add(-time.Minute)
			svc.currentTradeSOC = 20
			svc.currentTradeLastSOC = 11
			svc.currentTradePowerW = svc.cfg.DischargePowerW
			svc.beginMeasuredTradeLocked(-2000)

			svc.mu.Lock()
			svc.stopDischargingLocked(context.Background(), 11)
			svc.mu.Unlock()
			battery.SOC = 50
			battery.CurrentPower = 0
			svc.tick(context.Background())

			if len(battery.DischargeCalls) != 0 || svc.state != StateIdle {
				t.Fatalf("fulfilled window restarted after SOC rebound: calls=%v state=%s", battery.DischargeCalls, svc.state)
			}
			wantDischargeWindows := 0
			if withPendingPlan {
				wantDischargeWindows = 1
			}
			if svc.currentPlan == nil || len(svc.currentPlan.DischargeWindows) != wantDischargeWindows {
				t.Fatalf("retired plan = %+v", svc.currentPlan)
			}
			if withPendingPlan && (len(svc.currentPlan.Cycles) != 1 || !sameTradeCycle(&svc.currentPlan.Cycles[0], &future)) {
				t.Fatalf("future cycle was not preserved: %+v", svc.currentPlan)
			}
		})
	}
}

func TestCompletedManualDischargeClearsRetainedAutomaticCycle(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := base.Add(2*time.Hour + 15*time.Minute)
	for _, source := range []string{"persisted", "solar"} {
		t.Run(source, func(t *testing.T) {
			cycle := &TradeCycle{
				ChargeWindow:    TimeWindow{Start: base, End: base.Add(time.Hour), Price: decimal.NewFromFloat(.10)},
				DischargeWindow: TimeWindow{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour), Price: decimal.NewFromFloat(.30)},
			}
			recorder := NewRecorder(t.TempDir(), .90, time.UTC)
			svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
			svc.recorder = recorder
			svc.nowFunc = func() time.Time { return now }
			svc.state = StateManualDischarging
			svc.currentPlan = &TradingPlan{IsProfitable: true, Cycles: []TradeCycle{*cycle}, DischargeWindows: []TimeWindow{cycle.DischargeWindow}}
			pendingPlan := &TradingPlan{IsProfitable: true}
			svc.pendingPlan = pendingPlan
			svc.currentTradeStart = now.Add(-time.Minute)
			svc.currentTradeSOC = 20
			svc.currentTradeLastSOC = 11
			svc.currentTradePowerW = 800
			svc.beginMeasuredTradeLocked(-800)
			if source == "persisted" {
				if err := recorder.SaveAutomaticCycleCommitment(cycle); err != nil {
					t.Fatalf("save commitment: %v", err)
				}
				svc.automaticCycleCommit = cycle
				svc.automaticCycleCommitDurable = true
			} else {
				svc.solarCycleRetention = cycle
			}

			svc.mu.Lock()
			svc.stopDischargingLocked(context.Background(), 11)
			svc.mu.Unlock()

			if svc.automaticCycleCommit != nil || svc.solarCycleRetention != nil || svc.currentPlan != pendingPlan {
				t.Fatalf("completed manual discharge retained cycle: commitment=%+v solar=%+v current=%p pending=%p", svc.automaticCycleCommit, svc.solarCycleRetention, svc.currentPlan, pendingPlan)
			}
			if len(svc.retiredDischargeWindows) != 1 || !sameWindowPeriod(svc.retiredDischargeWindows[0], cycle.DischargeWindow) {
				t.Fatalf("completed discharge retirement markers = %+v", svc.retiredDischargeWindows)
			}
			persisted, err := recorder.LoadAutomaticCycleCommitment()
			if err != nil || persisted != nil {
				t.Fatalf("commitment after manual completion = %+v, error = %v", persisted, err)
			}
		})
	}
}

func TestCurrentStatusReportsUnavailableReservationWithoutTelemetry(t *testing.T) {
	now := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	svc := newTestService(
		testConfigSmallBattery(),
		NewMockBattery(50),
		makePrices(now, 0.05, 0.10, 0.25, 0.20),
		now,
	)

	status := svc.GetCurrentStatus(t.Context())
	if status.NextAction != "charge reservation unavailable: battery telemetry unavailable" {
		t.Fatalf("next action = %q", status.NextAction)
	}
}

func TestDailySummaryRetriesCompletedPriorDayAfterMidnight(t *testing.T) {
	now := time.Date(2024, 1, 15, 23, 59, 0, 0, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	notifier := &MockNotifier{DailySummaryErr: errors.New("telegram unavailable")}
	svc.telegram = notifier
	if err := svc.recorder.RecordTrade(Trade{
		Timestamp: now.Add(-12 * time.Hour),
		Action:    ActionCharge,
		PriceEUR:  decimal.RequireFromString("0.10"),
		DurationS: 60,
		EnergyKWh: decimal.RequireFromString("0.10"),
	}); err != nil {
		t.Fatalf("recording completed-day trade: %v", err)
	}

	svc.checkDailySummary(context.Background())
	if !svc.lastDailySummary.IsZero() {
		t.Fatal("failed 23:59 summary was marked delivered")
	}
	now = now.Add(30 * time.Second)
	svc.checkDailySummary(context.Background())
	if len(notifier.DailySummaryCalls) != 1 {
		t.Fatalf("daily summary retried before midnight recovery: calls=%d", len(notifier.DailySummaryCalls))
	}

	now = now.Add(90 * time.Second)
	notifier.DailySummaryErr = nil
	svc.checkDailySummary(context.Background())

	if len(notifier.DailySummaryCalls) != 2 {
		t.Fatalf("daily summary attempts = %d, want failed 23:59 attempt plus midnight recovery", len(notifier.DailySummaryCalls))
	}
	target := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if !notifier.DailySummaryCalls[1].Date.Equal(target) {
		t.Fatalf("recovered summary date = %s, want completed prior day %s", notifier.DailySummaryCalls[1].Date, target)
	}
	if !svc.lastDailySummary.Equal(target) {
		t.Fatalf("lastDailySummary = %s, want recovered day %s", svc.lastDailySummary, target)
	}
}

func TestDailySummaryBacksOffAfterMidnightRetryFails(t *testing.T) {
	now := time.Date(2024, 1, 16, 0, 1, 0, 0, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	notifier := &MockNotifier{DailySummaryErr: errors.New("telegram unavailable")}
	svc.telegram = notifier

	svc.checkDailySummary(context.Background())
	now = now.Add(time.Minute)
	svc.checkDailySummary(context.Background())
	if len(notifier.DailySummaryCalls) != 1 {
		t.Fatalf("daily summary retried during backoff: calls=%d", len(notifier.DailySummaryCalls))
	}
	now = now.Add(14 * time.Minute)
	svc.checkDailySummary(context.Background())
	if len(notifier.DailySummaryCalls) != 2 {
		t.Fatalf("daily summary did not retry after backoff: calls=%d", len(notifier.DailySummaryCalls))
	}
}

func TestDailySummaryWaitsForCrossMidnightSessionToFinish(t *testing.T) {
	now := time.Date(2024, 1, 15, 23, 59, 0, 0, time.UTC)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier
	svc.state = StateCharging
	svc.currentTradeStart = now.Add(-time.Minute)

	svc.checkDailySummary(context.Background())
	if len(notifier.DailySummaryCalls) != 0 || !svc.lastDailySummary.IsZero() {
		t.Fatalf("active cross-midnight session was summarized early: calls=%d last=%s", len(notifier.DailySummaryCalls), svc.lastDailySummary)
	}

	now = now.Add(3 * time.Minute)
	svc.state = StateIdle
	if err := svc.recorder.RecordTrade(Trade{
		Timestamp: svc.currentTradeStart,
		Action:    ActionCharge,
		PriceEUR:  decimal.RequireFromString("0.10"),
		DurationS: 4 * 60,
		EnergyKWh: decimal.RequireFromString("1.0"),
	}); err != nil {
		t.Fatalf("record cross-midnight trade: %v", err)
	}
	svc.checkDailySummary(context.Background())

	if len(notifier.DailySummaryCalls) != 1 {
		t.Fatalf("completed prior-day summary calls = %d, want 1", len(notifier.DailySummaryCalls))
	}
	got := notifier.DailySummaryCalls[0]
	if got.Date.Day() != 15 || math.Abs(got.ChargedKWh-.50) > .000001 {
		t.Fatalf("prior-day cross-midnight summary = %+v", got)
	}
}

func TestMeasuredTradeEnergyUsesPrecedingPowerSample(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, start)
	svc.state = StateCharging
	svc.nowFunc = func() time.Time { return start }
	svc.beginMeasuredTradeLocked(500)

	svc.accumulateMeasuredTradeEnergyAtLocked(1500, start.Add(time.Minute), true)
	if svc.currentTradeEnergyWs != 500*60 {
		t.Fatalf("first interval energy = %.0f Ws, want %d", svc.currentTradeEnergyWs, 500*60)
	}
	svc.accumulateMeasuredTradeEnergyAtLocked(0, start.Add(2*time.Minute), true)
	if svc.currentTradeEnergyWs != (500+1500)*60 {
		t.Fatalf("total energy = %.0f Ws, want %d", svc.currentTradeEnergyWs, (500+1500)*60)
	}
}

func TestMeasuredTradeEnergyPreservesExactCrossMidnightPrices(t *testing.T) {
	start := time.Date(2026, 9, 6, 23, 59, 0, 0, time.UTC)
	now := start
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), []nordpool.Price{
		{Time: start.Truncate(15 * time.Minute), Value: .10},
		{Time: start.Add(time.Minute), Value: .30},
	}, now)
	svc.nowFunc = func() time.Time { return now }
	svc.state = StateCharging
	svc.currentTradeStart = start
	svc.currentTradeSOC = 50
	svc.beginMeasuredTradeLocked(1000)
	now = start.Add(2 * time.Minute)
	svc.accumulateMeasuredTradeEnergyAtLocked(1000, now, true)
	avgPrice, known := svc.measuredTradePriceLocked()
	trade := Trade{
		Timestamp:      start,
		Action:         ActionCharge,
		PriceEUR:       avgPrice,
		DurationS:      120,
		EnergyKWh:      decimal.NewFromFloat(svc.currentTradeEnergyWs / 3_600_000),
		DayAllocations: append([]TradeDayAllocation(nil), svc.currentTradeDayAllocations...),
	}
	if !known || len(trade.DayAllocations) != 2 {
		t.Fatalf("cross-midnight allocations = %+v, known=%t", trade.DayAllocations, known)
	}
	if err := svc.recorder.RecordTrade(trade); err != nil {
		t.Fatalf("RecordTrade() error = %v", err)
	}
	history := svc.recorder.GetHistory()
	if len(history.Days) != 2 || !history.Days[0].AvgChargePrice.Equal(decimal.RequireFromString("0.3")) ||
		!history.Days[1].AvgChargePrice.Equal(decimal.RequireFromString("0.1")) {
		t.Fatalf("cross-midnight daily prices = %+v", history.Days)
	}
}

func TestCompleteTradeDayAllocationsCoversTelemetryGapAcrossMidnight(t *testing.T) {
	start := time.Date(2026, 9, 6, 23, 59, 0, 0, time.UTC)
	energy := decimal.RequireFromString("0.5")
	completed := completeTradeDayAllocations([]TradeDayAllocation{{
		Timestamp: start.Add(time.Minute),
		DurationS: 60,
		EnergyKWh: energy,
	}}, start, 120, time.UTC)

	if len(completed) != 2 || completed[0].DurationS != 60 || completed[1].DurationS != 60 {
		t.Fatalf("completed allocations = %+v, want two complete one-minute fragments", completed)
	}
	if !completed[0].EnergyKWh.IsZero() || !completed[1].EnergyKWh.Equal(energy) {
		t.Fatalf("completed allocation energy = %+v, want telemetry energy retained on second day", completed)
	}
}

func TestErrorNotificationFailureDoesNotConsumeRateLimit(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	notifier := &MockNotifier{ErrorErr: errors.New("telegram unavailable")}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier

	svc.notifyError(context.Background(), "first")
	if !svc.lastErrorNotify.IsZero() {
		t.Fatal("failed notification consumed the rate limit")
	}
	notifier.ErrorErr = nil
	svc.notifyError(context.Background(), "retry")
	svc.notifyError(context.Background(), "throttled")
	if len(notifier.ErrorCalls) != 2 {
		t.Fatalf("notification attempts = %d, want failed attempt plus successful retry", len(notifier.ErrorCalls))
	}
}

func TestLinkDownNotificationFailureDoesNotConsumeRateLimit(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	notifier := &MockNotifier{ErrorErr: errors.New("telegram unavailable")}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier

	svc.notifyLinkDown(context.Background(), "first")
	if !svc.lastLinkDownNotify.IsZero() {
		t.Fatal("failed link-down notification consumed the rate limit")
	}
	notifier.ErrorErr = nil
	svc.notifyLinkDown(context.Background(), "retry")
	svc.notifyLinkDown(context.Background(), "throttled")
	if len(notifier.ErrorCalls) != 2 {
		t.Fatalf("notification attempts = %d, want failed attempt plus successful retry", len(notifier.ErrorCalls))
	}
}

func TestBackwardClockCorrectionDoesNotSuppressNotifications(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	notifier := &MockNotifier{}
	svc := newTestService(testConfigSmallBattery(), NewMockBattery(50), nil, now)
	svc.nowFunc = func() time.Time { return now }
	svc.telegram = notifier
	svc.lastErrorNotify = now.Add(time.Hour)
	svc.lastLinkDownNotify = now.Add(time.Hour)

	svc.notifyError(context.Background(), "battery error")
	svc.notifyLinkDown(context.Background(), "link down")
	if len(notifier.ErrorCalls) != 2 {
		t.Fatalf("notification attempts = %d, want both alerts after backward clock correction", len(notifier.ErrorCalls))
	}
}

func TestCompletedTradeRetainsFractionalMidnightEnergy(t *testing.T) {
	for _, state := range []State{StateCharging, StateDischarging, StateSolarCharging} {
		t.Run(string(state), func(t *testing.T) {
			start := time.Date(2026, 9, 6, 23, 59, 59, 900_000_000, time.UTC)
			now := start
			cfg := testConfig()
			svc := newTestService(cfg, NewMockBattery(50), nil, now)
			svc.recorder = NewRecorder(t.TempDir(), cfg.BatteryEfficiency, time.UTC)
			svc.nowFunc = func() time.Time { return now }
			svc.state = state
			svc.currentTradeStart = start
			svc.currentTradeSOC = 50
			powerW := 1000.0
			if state == StateDischarging {
				powerW = -powerW
			}
			svc.beginMeasuredTradeLocked(powerW)
			svc.solarLastUpdate = start
			svc.solarMeasuredChargePowerW = 1000
			now = time.Date(2026, 9, 7, 0, 0, 1, 0, time.UTC)
			svc.mu.Lock()
			switch state {
			case StateCharging:
				svc.stopChargingLocked(context.Background(), 50)
			case StateDischarging:
				svc.stopDischargingLocked(context.Background(), 50)
			case StateSolarCharging:
				svc.stopSolarChargingLocked(context.Background(), 50, solarStopReasonSurplusGone)
			}
			svc.mu.Unlock()

			reloaded := NewRecorder(svc.recorder.dataDir, cfg.BatteryEfficiency, time.UTC)
			if err := reloaded.LoadTrades(); err != nil {
				t.Fatal(err)
			}
			history := reloaded.GetHistory()
			if len(history.Days) != 2 {
				t.Fatalf("persisted days = %d, want both sides of midnight", len(history.Days))
			}
			for _, day := range history.Days {
				seconds := 0.1
				if day.Date == "2026-09-07" {
					seconds = 1
				}
				got := day.ChargedKWh
				if state == StateDischarging {
					got = day.DischargedKWh
				}
				want := decimal.NewFromFloat(1000 * seconds / 3_600_000)
				if got.Sub(want).Abs().GreaterThan(decimal.New(1, -9)) {
					t.Errorf("day %s energy = %s, want %s", day.Date, got, want)
				}
			}
		})
	}
}

func TestSolarPriorityStopSettlesLatestBatteryPower(t *testing.T) {
	for _, batteryFull := range []bool{true, false} {
		t.Run(fmt.Sprintf("battery_full=%t", batteryFull), func(t *testing.T) {
			now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
			battery := NewMockBattery(50)
			if batteryFull {
				battery.SOC = 99
			}
			battery.CurrentPower = 0
			battery.DischargFlag = false
			battery.IdleHook = func() { now = now.Add(10 * time.Second) }
			svc := newTestServiceWithMeter(testConfig(), battery, NewMockMeter(true, 500), nil, now)
			svc.nowFunc = func() time.Time { return now }
			svc.state = StateSolarCharging
			svc.currentTradeStart = now.Add(-time.Second)
			svc.currentTradeSOC = 50
			svc.solarLastUpdate = svc.currentTradeStart
			svc.solarMeasuredChargePowerW = 500
			svc.solarGridPowerW = 500
			svc.solarChargePower = 500
			if !batteryFull {
				svc.currentPlan = &TradingPlan{
					IsProfitable:     true,
					DischargeWindows: []TimeWindow{{Start: now, End: now.Add(time.Hour)}},
				}
			}
			svc.solarTick(context.Background())
			history := svc.recorder.GetHistory()
			if len(history.Days) != 1 || len(history.Days[0].Trades) != 1 {
				t.Fatalf("completed solar session was lost: %+v", history)
			}
			trade := history.Days[0].Trades[0]
			want := decimal.NewFromFloat(500.0 / 3_600_000)
			if trade.EnergyKWh.Sub(want).Abs().GreaterThan(decimal.New(1, -9)) ||
				trade.GridEnergyKWh.Sub(want).Abs().GreaterThan(decimal.New(1, -9)) {
				t.Fatalf("battery/grid energy = %s/%s, want %s each", trade.EnergyKWh, trade.GridEnergyKWh, want)
			}
		})
	}
}

func TestPriceRefreshExecutesFuturePurchaseAfterMissedCycle(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.MaxCyclesPerDay = 1
	prices := make([]nordpool.Price, 96)
	for i := range prices {
		value := .20
		switch {
		case i < 8:
			value = .05
		case i < 16:
			value = .50
		case i >= 48 && i < 56:
			value = .10
		case i >= 56 && i < 64:
			value = .30
		}
		prices[i] = nordpool.Price{Time: day.Add(time.Duration(i) * 15 * time.Minute), Value: value}
	}
	now := day.Add(10 * time.Hour)
	battery := NewMockBattery(11)
	svc := newTestService(cfg, battery, prices, now)
	svc.recorder = NewRecorder(t.TempDir(), cfg.BatteryEfficiency, time.UTC)
	svc.nowFunc = func() time.Time { return now }
	svc.checkPriceFetch(context.Background())
	now = day.Add(12 * time.Hour)
	svc.tick(context.Background())
	if svc.State() != StateCharging || len(battery.ChargeCalls) != 1 {
		t.Fatalf("future purchase suppressed by expired cycle: state=%s calls=%v", svc.State(), battery.ChargeCalls)
	}
}

func TestRecoveredDischargeLeavesFuturePurchaseExecutable(t *testing.T) {
	base := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	now := base.Add(30 * time.Minute)
	cfg := testConfigSmallBattery()
	cfg.MaxCyclesPerDay = 1
	prices := makePrices(base, .05, .40, .20, .20, .10, .50)
	battery := NewMockBattery(99)
	svc := newTestService(cfg, battery, prices, now)
	svc.recorder = NewRecorder(t.TempDir(), cfg.BatteryEfficiency, time.UTC)
	svc.nowFunc = func() time.Time { return now }
	svc.currentPlan = nil
	svc.checkPriceFetch(context.Background())
	svc.tick(context.Background())
	if svc.State() != StateDischarging {
		t.Fatalf("stored energy recovery did not start: %s", svc.State())
	}
	now = base.Add(45 * time.Minute)
	battery.SOC = 11
	svc.tick(context.Background())
	now = base.Add(time.Hour)
	svc.tick(context.Background())
	if svc.State() != StateCharging || len(battery.ChargeCalls) != 1 {
		t.Fatalf("recovery blocked subsequent purchase: state=%s calls=%v", svc.State(), battery.ChargeCalls)
	}
}

func TestPriceRefreshCompletesPromotedLocalDayPrefix(t *testing.T) {
	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	values := make([]float64, 96)
	for i := range values {
		values[i] = .25
	}
	prices := makePrices(day, values...)
	now := day.Add(15 * time.Minute)
	svc := newTestService(testConfig(), NewMockBattery(50), makePrices(day.AddDate(0, 0, -1), values...), now)
	svc.nowFunc = func() time.Time { return now }
	svc.tomorrowPrices = prices[:88] // UTC's trailing two hours are published later.
	svc.nordpool = &MockPriceProvider{TodayPrices: prices}
	svc.checkPriceFetch(context.Background())
	now = day.Add(23 * time.Hour)
	status := svc.GetCurrentStatus(context.Background())
	if !status.CurrentPriceKnown || status.CurrentPrice != .25 {
		t.Fatalf("promoted prefix was not completed: %+v", status)
	}
}

func TestPriceRefreshRetainsKnownSuffixAfterShorterPublication(t *testing.T) {
	for _, tomorrow := range []bool{false, true} {
		t.Run(fmt.Sprintf("tomorrow=%t", tomorrow), func(t *testing.T) {
			day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			values := make([]float64, 96)
			for i := range values {
				values[i] = .25
			}
			prices := makePrices(day, values...)
			prefix := append([]nordpool.Price(nil), prices[:88]...)
			prefix[0].Value = .15
			now := day
			svc := newTestService(testConfig(), NewMockBattery(50), prices, now)
			svc.nowFunc = func() time.Time { return now }
			provider := &MockPriceProvider{TodayPrices: prefix, TomorrowPrices: prefix}
			svc.nordpool = provider
			var err error
			if tomorrow {
				svc.todayPrices = nil
				svc.tomorrowPrices = prices
				err = svc.fetchTomorrowPrices(context.Background())
			} else {
				err = svc.fetchTodayPrices(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			if status := svc.GetCurrentStatus(context.Background()); !status.CurrentPriceKnown || status.CurrentPrice != .15 {
				t.Fatalf("updated prefix not applied: %+v", status)
			}
			now = day.Add(23 * time.Hour)
			if status := svc.GetCurrentStatus(context.Background()); !status.CurrentPriceKnown || status.CurrentPrice != .25 {
				t.Fatalf("previously published suffix was erased: %+v", status)
			}
		})
	}
}

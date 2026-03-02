package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
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
	SOC          int
	ChargingFlag bool
	DischargFlag bool
	CurrentMode  string
	CurrentPower int

	// Call tracking
	ConnectCalled  bool
	ChargeCalls    []ChargeCall
	DischargeCalls []DischargeCall
	IdleCalls      int

	// Error injection
	ConnectErr   error
	GetStatusErr error
	ChargeErr    error
	DischargeErr error
	IdleErr      error
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

func (m *MockBattery) GetBatteryStatus() (*marstek.BatteryStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.GetStatusErr != nil {
		return nil, m.GetStatusErr
	}
	return &marstek.BatteryStatus{
		SOC:          m.SOC,
		ChargingFlag: m.ChargingFlag,
		DischargFlag: m.DischargFlag,
	}, nil
}

func (m *MockBattery) GetESStatus() (*marstek.ESStatus, error) {
	return &marstek.ESStatus{BatterySOC: m.SOC}, nil
}

func (m *MockBattery) Charge(powerW int, timeoutS int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ChargeErr != nil {
		return m.ChargeErr
	}
	m.ChargeCalls = append(m.ChargeCalls, ChargeCall{powerW, timeoutS})
	m.CurrentMode = "Passive"
	m.CurrentPower = -powerW
	return nil
}

func (m *MockBattery) Discharge(powerW int, timeoutS int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DischargeErr != nil {
		return m.DischargeErr
	}
	m.DischargeCalls = append(m.DischargeCalls, DischargeCall{powerW, timeoutS})
	m.CurrentMode = "Passive"
	m.CurrentPower = powerW
	return nil
}

func (m *MockBattery) SetPassiveMode(power int, cdTime int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CurrentMode = "Passive"
	m.CurrentPower = power
	return nil
}

func (m *MockBattery) Idle() error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

	StartupCalls    int
	TradeStartCalls []TradeStartCall
	TradeEndCalls   []TradeEndCall
	ErrorCalls      []string
	Commands        []string
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
}

func (m *MockNotifier) Enabled() bool { return true }

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

func (m *MockNotifier) SendTradeEnd(ctx context.Context, action string, energyKWh float64, avgPrice float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TradeEndCalls = append(m.TradeEndCalls, TradeEndCall{action, energyKWh, avgPrice})
	return nil
}

func (m *MockNotifier) SendError(ctx context.Context, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ErrorCalls = append(m.ErrorCalls, msg)
	return nil
}

func (m *MockNotifier) PollCommands(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmds := m.Commands
	m.Commands = nil
	return cmds, nil
}

// MockMeterReader implements MeterReader for testing.
type MockMeterReader struct {
	mu            sync.Mutex
	enabled       bool
	ActivePowerW  float64
	ActivePowerErr error
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
	defer m.mu.Unlock()
	if m.ActivePowerErr != nil {
		return 0, m.ActivePowerErr
	}
	return m.ActivePowerW, nil
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

// MockTelegram is a minimal mock for telegram.Client that does nothing.
type MockTelegram struct{}

func (m *MockTelegram) Enabled() bool                                             { return false }
func (m *MockTelegram) SendStartup(ctx context.Context, serviceName string) error { return nil }
func (m *MockTelegram) SendTradeStart(ctx context.Context, a string, p float64, s int) error {
	return nil
}
func (m *MockTelegram) SendTradeEnd(ctx context.Context, a string, e float64, p float64) error {
	return nil
}
func (m *MockTelegram) SendError(ctx context.Context, msg string) error    { return nil }
func (m *MockTelegram) PollCommands(ctx context.Context) ([]string, error) { return nil, nil }

// newTestService creates a Service configured for testing with the given mocks and clock.
func newTestService(cfg *config.Config, battery *MockBattery, prices []nordpool.Price, clockTime time.Time) *Service {
	recorder := NewRecorder("", cfg.BatteryEfficiency, time.UTC)
	svc := &Service{
		cfg:         cfg,
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

// --- Integration tests that call actual tick() ---

func TestTick_ChargeInLowPriceWindow(t *testing.T) {
	// Scenario: Battery at 50% SOC, current time is in charge window (low prices)
	// Expected: tick() should start charging
	// Using small battery for shorter window sizes

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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

func TestTick_DischargeInHighPriceWindow(t *testing.T) {
	// Scenario: Battery at 80% SOC, current time is in discharge window (high prices)
	// Expected: tick() should start discharging
	// Using small battery for shorter window sizes

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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
	prices := makePrices(baseTime,
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
	prices := makePrices(baseTime,
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
	prices := makePrices(baseTime,
		0.05, 0.06, // Charge window - slots 0-1
		0.15, 0.20, // Outside window
	)

	cfg := testConfig()
	mockBattery := NewMockBattery(70)
	clockTime := baseTime.Add(2 * 15 * time.Minute) // Outside charge window

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.state = StateCharging // Already charging
	svc.currentTradeStart = baseTime
	svc.currentTradePrice = decimal.NewFromFloat(0.05)
	svc.currentTradeSOC = 50

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after leaving window, got %s", svc.state)
	}
	if mockBattery.IdleCalls != 1 {
		t.Errorf("expected 1 idle call, got %d", mockBattery.IdleCalls)
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
	svc.currentTradePrice = decimal.NewFromFloat(0.05)
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
	svc.currentTradePrice = decimal.NewFromFloat(0.20)
	svc.currentTradeSOC = 50

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle after battery low, got %s", svc.state)
	}
}

func TestTick_NotProfitablePlan(t *testing.T) {
	// Scenario: Prices have very small spread, plan is not profitable
	// Expected: tick() should stay idle regardless of windows

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.11, 0.12, 0.11) // Very small spread

	cfg := testConfig()
	cfg.MinPriceSpread = 0.05 // Requires 5 cent spread

	mockBattery := NewMockBattery(50)
	clockTime := baseTime

	svc := newTestService(cfg, mockBattery, prices, clockTime)

	if svc.currentPlan.IsProfitable {
		t.Fatalf("expected plan to be not profitable (spread=%f)", svc.currentPlan.Spread)
	}

	ctx := context.Background()
	svc.tick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected state=idle when plan not profitable, got %s", svc.state)
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
	prices := makePrices(baseTime,
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
	svc.currentTradePrice = decimal.NewFromFloat(0.05)
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
	prices := makePrices(baseTime,
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

func TestSolarTick_StartAfter3Confirmations(t *testing.T) {
	// Scenario: P1 meter shows -500W (exporting 500W surplus) consistently
	// Expected: After 3 consecutive readings, solar charging should start

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	// Flat prices = no profitable windows, so no scheduled trading
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500) // exporting 500W

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	ctx := context.Background()

	// Tick 1-2: accumulate confirmations, no charging yet
	svc.solarTick(ctx)
	if svc.state != StateIdle {
		t.Errorf("tick 1: expected idle, got %s", svc.state)
	}
	svc.solarTick(ctx)
	if svc.state != StateIdle {
		t.Errorf("tick 2: expected idle, got %s", svc.state)
	}

	// Tick 3: should start solar charging
	svc.solarTick(ctx)
	if svc.state != StateSolarCharging {
		t.Errorf("tick 3: expected solar_charging, got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 1 {
		t.Fatalf("expected 1 charge call, got %d", len(mockBattery.ChargeCalls))
	}
	if mockBattery.ChargeCalls[0].PowerW != 500 {
		t.Errorf("expected charge power=500, got %d", mockBattery.ChargeCalls[0].PowerW)
	}
}

func TestSolarTick_StopOnSurplusDrop(t *testing.T) {
	// Scenario: Solar charging active, then surplus drops below threshold
	// Expected: Should stop solar charging

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	// Pre-set solar charging state
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-5 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500

	ctx := context.Background()

	// Surplus drops to 50W (below 100W threshold)
	meter.SetActivePowerW(-50)
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle after surplus drop, got %s", svc.state)
	}
}

func TestSolarTick_YieldToDischargeWindow(t *testing.T) {
	// Scenario: Solar charging active, then a discharge window starts
	// Expected: tick() should stop solar charging and start discharging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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

func TestSolarTick_YieldToChargeWindow(t *testing.T) {
	// Scenario: Solar charging active, then a scheduled charge window starts
	// Expected: tick() should stop solar charging and start scheduled charging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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

func TestSolarTick_ClampToMaxPower(t *testing.T) {
	// Scenario: Surplus is 5000W but max charge power is 2000W
	// Expected: Should clamp to max charge power

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery() // ChargePowerW = 2000
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -5000) // 5000W surplus

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	ctx := context.Background()
	// 3 ticks to start
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateSolarCharging {
		t.Fatalf("expected solar_charging, got %s", svc.state)
	}
	if len(mockBattery.ChargeCalls) != 1 {
		t.Fatalf("expected 1 charge call, got %d", len(mockBattery.ChargeCalls))
	}
	if mockBattery.ChargeCalls[0].PowerW != cfg.ChargePowerW {
		t.Errorf("expected charge power clamped to %d, got %d", cfg.ChargePowerW, mockBattery.ChargeCalls[0].PowerW)
	}
}

func TestSolarTick_NoStartDuringDischargeWindow(t *testing.T) {
	// Scenario: Surplus available but we're in a discharge window
	// Expected: Should not start solar charging

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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
	prices := makePrices(baseTime,
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

func TestSolarTick_NoStartWhenBatteryFull(t *testing.T) {
	// Scenario: Surplus available but battery at 100%
	// Expected: Should not start solar charging

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(100) // Full
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	ctx := context.Background()
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle when battery full, got %s", svc.state)
	}
}

func TestSolarTick_StopWhenBatteryFull(t *testing.T) {
	// Scenario: Solar charging active, battery reaches 100%
	// Expected: Should stop solar charging

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(100) // Just became full
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-10 * time.Minute)
	svc.currentTradeSOC = 95
	svc.solarChargePower = 500

	ctx := context.Background()
	svc.solarTick(ctx)

	if svc.state != StateIdle {
		t.Errorf("expected idle when battery full during solar, got %s", svc.state)
	}
}

func TestSolarTick_SurplusCountResets(t *testing.T) {
	// Scenario: Surplus readings interrupted by a below-threshold reading
	// Expected: Counter resets and needs 3 new readings to start

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -500)

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)

	ctx := context.Background()

	// 2 surplus readings
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	if svc.solarSurplusCount != 2 {
		t.Fatalf("expected count=2, got %d", svc.solarSurplusCount)
	}

	// Surplus drops below threshold
	meter.SetActivePowerW(-50) // 50W surplus < 100W threshold
	svc.solarTick(ctx)
	if svc.solarSurplusCount != 0 {
		t.Errorf("expected count reset to 0, got %d", svc.solarSurplusCount)
	}

	// Need 3 more readings to start
	meter.SetActivePowerW(-500)
	svc.solarTick(ctx)
	svc.solarTick(ctx)
	if svc.state != StateIdle {
		t.Error("should still be idle after only 2 new readings")
	}
	svc.solarTick(ctx)
	if svc.state != StateSolarCharging {
		t.Errorf("expected solar_charging after 3 new readings, got %s", svc.state)
	}
}

func TestSolarTick_PowerAdjustmentDeadband(t *testing.T) {
	// Scenario: Solar charging at 500W, surplus changes to 530W (within 50W deadband)
	// Expected: Should NOT send a new charge command

	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.10, 0.10, 0.10, 0.10)

	cfg := testConfigSmallBattery()
	mockBattery := NewMockBattery(50)
	meter := NewMockMeter(true, -530)

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
	meter := NewMockMeter(true, -50) // low surplus to stop

	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, baseTime)
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime.Add(-10 * time.Minute)
	svc.currentTradeSOC = 45
	svc.solarChargePower = 500
	svc.solarLastUpdate = baseTime.Add(-10 * time.Minute)

	ctx := context.Background()
	svc.solarTick(ctx) // Should stop due to low surplus

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
	meter := NewMockMeter(true, -50)

	clockTime := baseTime
	svc := newTestServiceWithMeter(cfg, mockBattery, meter, prices, clockTime)

	// Simulate: started at 500W at baseTime
	svc.state = StateSolarCharging
	svc.currentTradeStart = baseTime
	svc.currentTradeSOC = 40
	svc.solarChargePower = 500
	svc.solarEnergyWs = 0
	svc.solarLastUpdate = baseTime

	// Advance 5 minutes and accumulate at 500W
	clockTime = baseTime.Add(5 * time.Minute)
	svc.SetClock(func() time.Time { return clockTime })
	svc.accumulateSolarEnergyLocked()
	// Should have 500 * 300 = 150000 Ws
	if svc.solarEnergyWs != 150000 {
		t.Errorf("expected 150000 Ws after 5min at 500W, got %f", svc.solarEnergyWs)
	}

	// Change power to 1500W
	svc.solarChargePower = 1500

	// Advance another 5 minutes and stop
	clockTime = baseTime.Add(10 * time.Minute)
	svc.SetClock(func() time.Time { return clockTime })

	ctx := context.Background()
	svc.solarTick(ctx) // will stop due to low surplus

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

func TestSolarTick_YieldsDirectlyToDischargeWindow(t *testing.T) {
	// Verify solarTick() itself stops solar charging when a discharge window starts
	// (not relying on tick() which runs every 60s)

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime,
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

	// Should be idle (solarTick stops solar, tick() needed to start discharge)
	if svc.state != StateIdle {
		t.Errorf("expected idle after solarTick yield, got %s", svc.state)
	}
}

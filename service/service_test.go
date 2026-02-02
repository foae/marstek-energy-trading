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

func TestTick_NoDischargeWhenNotProfitable(t *testing.T) {
	// Scenario: Last charge was at high price, current discharge price is not profitable
	// Expected: tick() should not discharge

	baseTime := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	prices := makePrices(baseTime, 0.05, 0.06, 0.15, 0.16)

	cfg := testConfig()
	mockBattery := NewMockBattery(80)
	clockTime := baseTime.Add(2 * 15 * time.Minute) // In discharge window (price=0.15)

	svc := newTestService(cfg, mockBattery, prices, clockTime)
	svc.lastChargePrice = decimal.NewFromFloat(0.15) // Breakeven = 0.15/0.90 = 0.167

	ctx := context.Background()
	svc.tick(ctx)

	// Should stay idle because discharge at 0.15 < breakeven 0.167
	if svc.state != StateIdle {
		t.Errorf("expected state=idle (not profitable), got %s", svc.state)
	}
	if len(mockBattery.DischargeCalls) != 0 {
		t.Errorf("expected no discharge calls when not profitable, got %d", len(mockBattery.DischargeCalls))
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

func TestIsDischargeProfitiable_ZeroLastChargePrice(t *testing.T) {
	// Scenario: No previous charge recorded (lastChargePrice = 0)
	// Expected: Should allow discharge

	cfg := testConfig()
	svc := &Service{
		cfg:             cfg,
		lastChargePrice: decimal.Zero,
	}

	if !svc.isDischargeProfitiable(decimal.NewFromFloat(0.10)) {
		t.Error("discharge should be allowed when no previous charge recorded")
	}
}

func TestIsDischargeProfitiable_ExactBreakevenPrice(t *testing.T) {
	// Scenario: Discharge price at/below breakeven
	// Expected: Should NOT be profitable

	cfg := testConfig()
	svc := &Service{
		cfg:             cfg,
		lastChargePrice: decimal.NewFromFloat(0.10), // Breakeven = 0.10 / 0.90 = 0.1111
	}

	// Below breakeven - should NOT be profitable
	if svc.isDischargeProfitiable(decimal.NewFromFloat(0.11)) {
		t.Error("discharge at 0.11 should not be profitable (breakeven=0.1111)")
	}

	// Above breakeven - should be profitable
	if !svc.isDischargeProfitiable(decimal.NewFromFloat(0.12)) {
		t.Error("discharge at 0.12 should be profitable (breakeven=0.1111)")
	}
}

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

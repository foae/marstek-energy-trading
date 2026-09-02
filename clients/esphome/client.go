package esphome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
)

const (
	defaultTimeout = 10 * time.Second

	// ESPHome can acknowledge a REST select write even when the underlying
	// Modbus command is dropped. Retry idempotent select writes with backoff
	// while waiting for the next published value. Retries start only after
	// ESPHome had a chance to poll the register back from the battery —
	// re-writing sooner just piles duplicate Modbus frames onto the hub.
	espHomeControlPublicationInterval = 15 * time.Second
	controlConfirmationTimeout        = espHomeControlPublicationInterval + 5*time.Second
	controlConfirmationInterval       = 500 * time.Millisecond
	controlWriteRetryDelay            = 4 * time.Second
	controlWriteMaxAttempts           = 3

	// The RS485 link is declared down only after telemetry stays bit-identical for
	// longer than two ESPHome publication cycles, so a slow-publishing but healthy
	// link is never misread as dead.
	linkProbeWindow    = 35 * time.Second
	linkProbeInterval  = 5 * time.Second
	linkDownVerdictTTL = 10 * time.Minute

	// Pack voltage (0.01 V resolution) and current (0.01 A resolution) flicker
	// continuously, even at a 75 W solar trickle, and AC voltage follows the grid.
	// Two full minutes bit-identical across all four sampled sensors is a frozen
	// link, not a quiet one.
	linkStaleThreshold = 2 * time.Minute

	// ESPHome sensor/entity paths (URL-encoded where needed)
	sensorSOC          = "/sensor/Battery%20State%20Of%20Charge"
	sensorTemperature  = "/sensor/Internal%20Temperature"
	sensorRemainingCap = "/sensor/Battery%20Remaining%20Capacity"
	sensorTotalEnergy  = "/sensor/Battery%20Total%20Energy"
	sensorBatteryPower = "/sensor/Battery%20Power"
	sensorACVoltage    = "/sensor/AC%20Voltage"
	// Pack voltage/current averages are the fastest-moving battery-sourced values.
	sensorBatteryVoltageAvg = "/sensor/Battery%20Voltage%20%28Average%29"
	sensorBatteryCurrentAvg = "/sensor/Battery%20Current%20%28Average%29"
	textSensorDeviceName    = "/text_sensor/Device%20Name"
	textSensorEspIP         = "/text_sensor/Esp%20ip"
	numberChargepower       = "/number/Forcible%20Charge%20Power"
	numberDischargePower    = "/number/Forcible%20Discharge%20Power"
	selectRS485ControlMode  = "/select/RS485%20Control%20Mode"
	// Note: Unicode division slash (U+2044) in "Charge⁄Discharge"
	selectForceMode = "/select/Forcible%20Charge%E2%81%84Discharge"
)

// Client is an ESPHome HTTP client for battery control.
// It implements the service.BatteryController interface.
type Client struct {
	baseURL    string
	httpClient *http.Client
	minSOC     int // Minimum SOC percentage for discharge flag

	restartButton   string        // ESPHome restart button object id; empty disables bridge restarts
	writeRetryDelay time.Duration // test override

	mu            sync.Mutex
	lastValues    map[string]float64 // battery-sourced telemetry, keyed by entity path
	lastChangeAt  time.Time          // when a battery-sourced value last moved
	linkDownAt    time.Time          // zero when the link is believed healthy
	now           func() time.Time   // clock, overridable in tests
	probeWindow   time.Duration      // test override
	probeInterval time.Duration      // test override
}

// New creates a new ESPHome client.
// minSOC is the minimum SOC percentage (e.g., 11 for 11%).
func New(baseURL string, minSOC int) *Client {
	// Remove trailing slash if present
	baseURL = strings.TrimRight(baseURL, "/")
	if minSOC <= 0 {
		minSOC = 11 // Default fallback
	}
	return &Client{
		baseURL: baseURL,
		minSOC:  minSOC,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		lastValues:      make(map[string]float64),
		writeRetryDelay: controlWriteRetryDelay,
		now:             time.Now,
		probeWindow:     linkProbeWindow,
		probeInterval:   linkProbeInterval,
	}
}

// Connect verifies connectivity to the ESPHome device.
// For HTTP this is a simple health check - actual connection is per-request.
func (c *Client) Connect() error {
	_, err := c.getTextSensor(textSensorDeviceName)
	if err != nil {
		return fmt.Errorf("connect to ESPHome: %w", err)
	}
	return nil
}

// Close is a no-op for HTTP (stateless protocol).
func (c *Client) Close() error {
	return nil
}

// Discover returns device information from ESPHome.
func (c *Client) Discover() (*marstek.DeviceInfo, error) {
	deviceName, err := c.getTextSensor(textSensorDeviceName)
	if err != nil {
		return nil, fmt.Errorf("get device name: %w", err)
	}

	ip, err := c.getTextSensor(textSensorEspIP)
	if err != nil {
		// IP is optional, don't fail
		ip = ""
	}

	return &marstek.DeviceInfo{
		Device: deviceName,
		IP:     ip,
	}, nil
}

// GetBatteryStatus returns the current battery status.
func (c *Client) GetBatteryStatus() (*marstek.BatteryStatus, error) {
	return c.GetBatteryStatusContext(context.Background())
}

// GetBatteryStatusContext returns battery status with cancellation support.
func (c *Client) GetBatteryStatusContext(ctx context.Context) (*marstek.BatteryStatus, error) {
	soc, err := c.getSensorFloatContext(ctx, sensorSOC)
	if err != nil {
		return nil, fmt.Errorf("get SOC: %w", err)
	}

	// Temperature is optional - don't fail if unavailable
	temp, _ := c.getSensorFloatContext(ctx, sensorTemperature)

	// Capacity is optional
	capacity, _ := c.getSensorFloatContext(ctx, sensorRemainingCap)

	// Total energy for rated capacity
	ratedCapacity, _ := c.getSensorFloatContext(ctx, sensorTotalEnergy)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ESPHome doesn't have direct charging/discharging flags.
	// Infer from SOC: can charge if SOC < 100, can discharge if SOC > minSOC
	socInt := int(soc)

	return &marstek.BatteryStatus{
		SOC:           socInt,
		ChargingFlag:  socInt < 100,
		DischargFlag:  socInt > c.minSOC,
		Temperature:   temp,
		Capacity:      capacity * 1000, // kWh to Wh
		RatedCapacity: ratedCapacity * 1000,
	}, nil
}

// GetESStatus returns the energy system status.
func (c *Client) GetESStatus(ctx context.Context) (*marstek.ESStatus, error) {
	soc, err := c.getSensorFloatContext(ctx, sensorSOC)
	if err != nil {
		return nil, fmt.Errorf("get SOC: %w", err)
	}

	power, err := c.getSensorFloatContext(ctx, sensorBatteryPower)
	if err != nil {
		return nil, fmt.Errorf("get battery power: %w", err)
	}

	return &marstek.ESStatus{
		BatterySOC:   int(soc),
		BatteryPower: power,
	}, nil
}

// GetBatteryPower returns the signed battery power: positive charging, negative discharging.
func (c *Client) GetBatteryPower(ctx context.Context) (float64, error) {
	return c.getSensorFloatContext(ctx, sensorBatteryPower)
}

// Charge starts charging at the specified power (watts).
// timeoutS is ignored - ESPHome has no auto-timeout, service handles refresh.
func (c *Client) Charge(powerW int, _ int) error {
	return c.ChargeContext(context.Background(), powerW, 0)
}

// ChargeContext starts charging and allows cancellation while ESPHome applies each control write.
func (c *Client) ChargeContext(ctx context.Context, powerW int, _ int) error {
	if err := c.ensureRS485ControlMode(ctx); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("enable RS485 control mode: %w", err))
	}

	// Then set charge power
	if err := c.setNumberConfirmed(ctx, numberChargepower, float64(powerW)); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("set charge power: %w", err))
	}

	// Finally activate charge mode
	if err := c.setSelectConfirmed(ctx, selectForceMode, "charge"); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("set charge mode: %w", err))
	}

	return nil
}

// Discharge starts discharging at the specified power (watts).
// timeoutS is ignored - ESPHome has no auto-timeout, service handles refresh.
func (c *Client) Discharge(powerW int, _ int) error {
	return c.DischargeContext(context.Background(), powerW, 0)
}

// DischargeContext starts discharging and allows cancellation while ESPHome applies each control write.
func (c *Client) DischargeContext(ctx context.Context, powerW int, _ int) error {
	if err := c.ensureRS485ControlMode(ctx); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("enable RS485 control mode: %w", err))
	}

	// Then set discharge power
	if err := c.setNumberConfirmed(ctx, numberDischargePower, float64(powerW)); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("set discharge power: %w", err))
	}

	// Finally activate discharge mode
	if err := c.setSelectConfirmed(ctx, selectForceMode, "discharge"); err != nil {
		return c.classifyControlFailure(ctx, fmt.Errorf("set discharge mode: %w", err))
	}

	return nil
}

// SetPassiveMode sets the battery mode based on power direction.
// Positive power = discharge, negative power = charge, zero = idle.
// cdTime is ignored - ESPHome has no auto-timeout.
func (c *Client) SetPassiveMode(power int, cdTime int) error {
	return c.SetPassiveModeContext(context.Background(), power, cdTime)
}

// SetPassiveModeContext sets the battery mode and allows cancellation during control confirmation.
func (c *Client) SetPassiveModeContext(ctx context.Context, power int, cdTime int) error {
	switch {
	case power < 0:
		// Negative = charge
		return c.ChargeContext(ctx, -power, cdTime)
	case power > 0:
		// Positive = discharge
		return c.DischargeContext(ctx, power, cdTime)
	default:
		// Zero = idle
		return c.IdleContext(ctx)
	}
}

// Idle stops any forced charge/discharge operation.
func (c *Client) Idle() error {
	return c.IdleContext(context.Background())
}

// IdleContext stops forced operation and allows cancellation during control confirmation.
func (c *Client) IdleContext(ctx context.Context) error {
	var enableErr error
	if err := c.ensureRS485ControlMode(ctx); err != nil {
		enableErr = fmt.Errorf("enable RS485 control mode: %w", err)
	}

	if err := c.setSelectConfirmed(ctx, selectForceMode, "stop"); err != nil {
		return c.classifyControlFailure(ctx, errors.Join(enableErr, fmt.Errorf("stop forcible mode: %w", err)))
	}
	if enableErr != nil {
		return c.classifyControlFailure(ctx, enableErr)
	}
	return nil
}

// sensorResponse represents ESPHome sensor JSON response.
// Note: ESPHome returns "state" as a formatted string with unit (e.g., "11.0 %"),
// while "value" is the raw numeric value. We only use "value".
type sensorResponse struct {
	ID    string  `json:"id"`
	State string  `json:"state"` // Formatted string with unit, not used
	Value float64 `json:"value"` // Raw numeric value
}

// textSensorResponse represents ESPHome text sensor JSON response.
type textSensorResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Value string `json:"value"`
}

type controlResponse struct {
	State string          `json:"state"`
	Value json.RawMessage `json:"value"`
}

func (c *Client) getSensorFloatContext(ctx context.Context, path string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("create GET %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	var sensor sensorResponse
	if err := json.NewDecoder(resp.Body).Decode(&sensor); err != nil {
		return 0, fmt.Errorf("decode sensor response: %w", err)
	}

	c.recordTelemetry(path, sensor.Value)

	return sensor.Value, nil
}

// recordTelemetry notes a battery-sourced reading. Any change proves the RS485
// link is alive, which clears a previous link-down verdict.
func (c *Client) recordTelemetry(path string, value float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, seen := c.lastValues[path]
	c.lastValues[path] = value
	if !seen || prev != value {
		c.lastChangeAt = c.now()
	}
	if seen && prev != value {
		c.linkDownAt = time.Time{}
	}
}

// CheckLink samples fast-moving telemetry and reports marstek.ErrLinkDown when no
// battery-sourced value has changed for linkStaleThreshold. Cheap enough to run every tick.
func (c *Client) CheckLink(ctx context.Context) error {
	for _, path := range []string{sensorACVoltage, sensorBatteryPower, sensorBatteryVoltageAvg, sensorBatteryCurrentAvg} {
		if _, err := c.getSensorFloatContext(ctx, path); err != nil {
			return fmt.Errorf("check link: %w", err)
		}
	}

	// Decide and record the verdict under one lock: releasing between the
	// staleness read and the write lets a concurrent telemetry change be
	// overwritten by a stale down verdict.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastChangeAt.IsZero() {
		return nil
	}
	now := c.now()
	staleFor := now.Sub(c.lastChangeAt)
	if staleFor < linkStaleThreshold {
		return nil
	}
	// Keep an earlier still-valid timestamp so the reported age keeps growing.
	if c.linkDownAt.IsZero() || now.Sub(c.linkDownAt) > linkDownVerdictTTL {
		c.linkDownAt = now
	}
	return fmt.Errorf("%w: telemetry frozen for %s", marstek.ErrLinkDown, staleFor.Round(time.Second))
}

// RefreshPassiveModeContext re-asserts a running mode only when the battery no longer
// reports it. ESPHome polls the forcible-mode register from the battery, so a matching
// select value means the command is still in effect and re-writing it would only add
// Modbus traffic to an already saturated hub.
//
// The power number read-back is ESPHome's optimistic cache, so it only catches a
// service-side change of the commanded power, never a dropped Modbus frame; the
// selects are polled from the battery and are the real signal.
func (c *Client) RefreshPassiveModeContext(ctx context.Context, power int, cdTime int) error {
	if since, down := c.linkDown(); down {
		return fmt.Errorf("%w for %s", marstek.ErrLinkDown, c.now().Sub(since).Round(time.Second))
	}

	expected := "stop"
	numberPath := ""
	switch {
	case power < 0:
		expected = "charge"
		numberPath = numberChargepower
	case power > 0:
		expected = "discharge"
		numberPath = numberDischargePower
	}

	// RS485 control mode is polled from the battery: if it is no longer enabled the
	// battery dropped the control session, so everything must be re-asserted.
	controlMode, err := c.getControlValue(ctx, selectRS485ControlMode)
	if err != nil {
		return fmt.Errorf("read RS485 control mode: %w", err)
	}
	if controlMode != "enable" {
		return c.SetPassiveModeContext(ctx, power, cdTime)
	}

	mode, err := c.getControlValue(ctx, selectForceMode)
	if err != nil {
		return fmt.Errorf("read forcible mode: %w", err)
	}

	if mode == expected {
		if numberPath == "" {
			return nil
		}
		actual, err := c.getControlValue(ctx, numberPath)
		if err != nil {
			return fmt.Errorf("read forcible power: %w", err)
		}
		actualNumber, parseErr := strconv.ParseFloat(actual, 64)
		if parseErr == nil && math.Abs(actualNumber-math.Abs(float64(power))) <= 0.5 {
			return nil
		}
	}

	return c.SetPassiveModeContext(ctx, power, cdTime)
}

// SetRestartButton names the ESPHome restart button object id (e.g. "restart"). Empty disables bridge restarts.
func (c *Client) SetRestartButton(objectID string) {
	c.restartButton = objectID
}

// RestartAvailable reports whether a restart button is configured.
func (c *Client) RestartAvailable() bool {
	return c.restartButton != ""
}

// RestartDevice presses the ESPHome restart button to reboot the bridge, which is the
// only known way to recover a frozen RS485 link.
func (c *Client) RestartDevice(ctx context.Context) error {
	if c.restartButton == "" {
		return fmt.Errorf("restart button not configured")
	}
	endpoint := fmt.Sprintf("%s/button/%s/press", c.baseURL, url.PathEscape(c.restartButton))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("create POST restart request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST restart: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST restart: status %d: %s", resp.StatusCode, string(body))
	}

	// The bridge is rebooting: every value collected before it is meaningless as a
	// staleness baseline, and the link-down verdict it produced must not survive
	// the recovery it triggered. Post-reboot telemetry becomes a fresh baseline.
	c.mu.Lock()
	c.lastValues = make(map[string]float64)
	c.lastChangeAt = time.Time{}
	c.linkDownAt = time.Time{}
	c.mu.Unlock()
	return nil
}

// linkDown reports whether a recent probe concluded the RS485 link is down.
// The verdict expires so a recovered link is always retried eventually.
func (c *Client) linkDown() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linkDownAt.IsZero() {
		return time.Time{}, false
	}
	if c.now().Sub(c.linkDownAt) > linkDownVerdictTTL {
		return time.Time{}, false
	}
	return c.linkDownAt, true
}

// markLinkDown records a link-down verdict, keeping any earlier still-valid
// timestamp so the reported age keeps growing.
func (c *Client) markLinkDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !c.linkDownAt.IsZero() && now.Sub(c.linkDownAt) <= linkDownVerdictTTL {
		return
	}
	c.linkDownAt = now
}

// probeLinkDown samples battery-sourced telemetry repeatedly and reports true
// only if every value stayed bit-identical for the whole window. It returns as
// soon as any value moves, so a healthy link costs one extra read.
func (c *Client) probeLinkDown(ctx context.Context) bool {
	paths := []string{sensorACVoltage, sensorBatteryPower, sensorTemperature}

	baseline := make(map[string]float64, len(paths))
	for _, path := range paths {
		value, err := c.getSensorFloatContext(ctx, path)
		if err != nil {
			return false
		}
		baseline[path] = value
	}

	start := c.now()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(c.probeInterval):
		}

		for _, path := range paths {
			value, err := c.getSensorFloatContext(ctx, path)
			if err != nil {
				return false
			}
			if value != baseline[path] {
				return false
			}
		}

		if c.now().Sub(start) >= c.probeWindow {
			return true
		}
	}
}

// classifyControlFailure upgrades a control failure to ErrLinkDown when telemetry
// proves the RS485 link is dead. ESPHome acknowledges REST writes and echoes
// number writes optimistically, so a dropped Modbus frame is otherwise invisible.
func (c *Client) classifyControlFailure(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	if errors.Is(err, marstek.ErrLinkDown) {
		return err
	}
	if !c.probeLinkDown(ctx) {
		return err
	}
	c.markLinkDown()
	return fmt.Errorf("%w: telemetry frozen and control writes dropped: %w", marstek.ErrLinkDown, err)
}

// getTextSensor retrieves a text sensor value.
func (c *Client) getTextSensor(path string) (string, error) {
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	var sensor textSensorResponse
	if err := json.NewDecoder(resp.Body).Decode(&sensor); err != nil {
		return "", fmt.Errorf("decode text sensor response: %w", err)
	}

	// ESPHome may return value in either "state" or "value" field
	if sensor.Value != "" {
		return sensor.Value, nil
	}
	return sensor.State, nil
}

// setNumber sets a number entity value via POST.
func (c *Client) setNumber(ctx context.Context, path string, value float64) error {
	endpoint := fmt.Sprintf("%s%s/set?value=%v", c.baseURL, path, value)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("create POST %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	return nil
}

// setSelect sets a select entity option via POST.
func (c *Client) setSelect(ctx context.Context, path string, option string) error {
	endpoint := fmt.Sprintf("%s%s/set?option=%s", c.baseURL, path, url.QueryEscape(option))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("create POST %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) ensureRS485ControlMode(ctx context.Context) error {
	if since, down := c.linkDown(); down {
		return fmt.Errorf("%w for %s", marstek.ErrLinkDown, c.now().Sub(since).Round(time.Second))
	}
	mode, err := c.getControlValue(ctx, selectRS485ControlMode)
	if err == nil && mode == "enable" {
		return nil
	}
	return c.setSelectConfirmed(ctx, selectRS485ControlMode, "enable")
}

func (c *Client) setSelectConfirmed(ctx context.Context, path string, option string) error {
	if err := c.setSelect(ctx, path, option); err != nil {
		return err
	}
	return c.waitForControlValue(ctx, path, option)
}

// setNumberConfirmed writes a number entity and reads it back. NOTE: ESPHome
// publishes number writes optimistically — the read-back returns the requested
// value even when the Modbus frame was dropped — so this confirms the ESPHome
// entity, not the battery. Dropped writes are detected by probeLinkDown instead.
func (c *Client) setNumberConfirmed(ctx context.Context, path string, value float64) error {
	if err := c.setNumber(ctx, path, value); err != nil {
		return err
	}
	confirmationCtx, cancel := context.WithTimeout(ctx, controlConfirmationTimeout)
	defer cancel()
	var lastValue string
	var lastErr error
	for {
		actual, err := c.getControlValue(confirmationCtx, path)
		if err == nil {
			lastValue = actual
			lastErr = nil
			actualNumber, parseErr := strconv.ParseFloat(actual, 64)
			if parseErr == nil && math.Abs(actualNumber-value) <= 0.5 {
				return nil
			}
		} else if confirmationCtx.Err() == nil {
			lastErr = err
		}
		select {
		case <-confirmationCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("confirm value %v: %w", value, lastErr)
			}
			return fmt.Errorf("confirm value %v: still %q", value, lastValue)
		case <-time.After(controlConfirmationInterval):
		}
	}
}

func (c *Client) waitForControlValue(ctx context.Context, path string, expected string) error {
	confirmationCtx, cancel := context.WithTimeout(ctx, controlConfirmationTimeout)
	defer cancel()

	pollTicker := time.NewTicker(controlConfirmationInterval)
	defer pollTicker.Stop()
	retryDelay := c.writeRetryDelay
	retryTimer := time.NewTimer(retryDelay)
	defer retryTimer.Stop()

	attempts := 1
	var lastValue string
	var lastReadErr error
	var lastWriteErr error
	for {
		actual, err := c.getControlValue(confirmationCtx, path)
		if err == nil {
			lastValue = actual
			lastReadErr = nil
			if actual == expected {
				return nil
			}
		} else if confirmationCtx.Err() == nil {
			lastReadErr = err
		}

		select {
		case <-confirmationCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastReadErr != nil {
				return fmt.Errorf("confirm option %q: %w", expected, lastReadErr)
			}
			if lastWriteErr != nil {
				return fmt.Errorf("confirm option %q after retry: %w", expected, lastWriteErr)
			}
			return fmt.Errorf("confirm option %q: still %q", expected, lastValue)
		case <-retryTimer.C:
			attempts++
			if err := c.setSelect(confirmationCtx, path, expected); confirmationCtx.Err() == nil {
				lastWriteErr = err
			}
			if attempts < controlWriteMaxAttempts {
				retryDelay *= 2
				retryTimer.Reset(retryDelay)
			}
		case <-pollTicker.C:
		}
	}
}

func (c *Client) getControlValue(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("create GET %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, string(body))
	}

	var entity controlResponse
	if err := json.NewDecoder(resp.Body).Decode(&entity); err != nil {
		return "", fmt.Errorf("decode control response: %w", err)
	}
	if len(entity.Value) > 0 {
		var value string
		if err := json.Unmarshal(entity.Value, &value); err == nil {
			return value, nil
		}
		return strings.TrimSpace(string(entity.Value)), nil
	}
	return entity.State, nil
}

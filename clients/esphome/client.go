package esphome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
)

const (
	defaultTimeout              = 10 * time.Second
	controlConfirmationTimeout  = 15 * time.Second
	controlConfirmationInterval = 500 * time.Millisecond

	// ESPHome sensor/entity paths (URL-encoded where needed)
	sensorSOC              = "/sensor/Battery%20State%20Of%20Charge"
	sensorTemperature      = "/sensor/Internal%20Temperature"
	sensorRemainingCap     = "/sensor/Battery%20Remaining%20Capacity"
	sensorTotalEnergy      = "/sensor/Battery%20Total%20Energy"
	sensorBatteryPower     = "/sensor/Battery%20Power"
	textSensorDeviceName   = "/text_sensor/Device%20Name"
	textSensorEspIP        = "/text_sensor/Esp%20ip"
	numberChargepower      = "/number/Forcible%20Charge%20Power"
	numberDischargePower   = "/number/Forcible%20Discharge%20Power"
	selectRS485ControlMode = "/select/RS485%20Control%20Mode"
	// Note: Unicode division slash (U+2044) in "Charge⁄Discharge"
	selectForceMode = "/select/Forcible%20Charge%E2%81%84Discharge"
)

// Client is an ESPHome HTTP client for battery control.
// It implements the service.BatteryController interface.
type Client struct {
	baseURL    string
	httpClient *http.Client
	minSOC     int // Minimum SOC percentage for discharge flag
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
	// Enable RS485 control mode first
	if err := c.setSelectConfirmed(ctx, selectRS485ControlMode, "enable"); err != nil {
		return fmt.Errorf("enable RS485 control mode: %w", err)
	}

	// Then set charge power
	if err := c.setNumberConfirmed(ctx, numberChargepower, float64(powerW)); err != nil {
		return fmt.Errorf("set charge power: %w", err)
	}

	// Finally activate charge mode
	if err := c.setSelectConfirmed(ctx, selectForceMode, "charge"); err != nil {
		return fmt.Errorf("set charge mode: %w", err)
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
	// Enable RS485 control mode first
	if err := c.setSelectConfirmed(ctx, selectRS485ControlMode, "enable"); err != nil {
		return fmt.Errorf("enable RS485 control mode: %w", err)
	}

	// Then set discharge power
	if err := c.setNumberConfirmed(ctx, numberDischargePower, float64(powerW)); err != nil {
		return fmt.Errorf("set discharge power: %w", err)
	}

	// Finally activate discharge mode
	if err := c.setSelectConfirmed(ctx, selectForceMode, "discharge"); err != nil {
		return fmt.Errorf("set discharge mode: %w", err)
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
	if err := c.setSelectConfirmed(ctx, selectRS485ControlMode, "enable"); err != nil {
		enableErr = fmt.Errorf("enable RS485 control mode: %w", err)
	}

	if err := c.setSelectConfirmed(ctx, selectForceMode, "stop"); err != nil {
		return errors.Join(enableErr, fmt.Errorf("stop forcible mode: %w", err))
	}

	// Once stop is confirmed the battery is physically safe; disabling RS485 is cleanup.
	disableErr := c.setSelectConfirmed(ctx, selectRS485ControlMode, "disable")
	if enableErr != nil || disableErr != nil {
		slog.Warn("battery stopped but RS485 control cleanup was incomplete",
			"error", errors.Join(enableErr, disableErr))
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

	return sensor.Value, nil
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

func (c *Client) setSelectConfirmed(ctx context.Context, path string, option string) error {
	if err := c.setSelect(ctx, path, option); err != nil {
		return err
	}
	return c.waitForControlValue(ctx, path, option)
}

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
			actualNumber, parseErr := strconv.ParseFloat(actual, 64)
			if parseErr == nil && math.Abs(actualNumber-value) <= 0.5 {
				return nil
			}
		} else {
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
	var lastValue string
	var lastErr error
	for {
		actual, err := c.getControlValue(confirmationCtx, path)
		if err == nil {
			lastValue = actual
			if actual == expected {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-confirmationCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if lastErr != nil {
				return fmt.Errorf("confirm option %q: %w", expected, lastErr)
			}
			return fmt.Errorf("confirm option %q: still %q", expected, lastValue)
		case <-time.After(controlConfirmationInterval):
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

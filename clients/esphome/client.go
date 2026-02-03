package esphome

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/foae/marstek-energy-trading/clients/marstek"
)

const (
	defaultTimeout = 10 * time.Second

	// ESPHome sensor/entity paths (URL-encoded where needed)
	sensorSOC            = "/sensor/Battery%20State%20Of%20Charge"
	sensorTemperature    = "/sensor/Internal%20Temperature"
	sensorRemainingCap   = "/sensor/Battery%20Remaining%20Capacity"
	sensorTotalEnergy    = "/sensor/Battery%20Total%20Energy"
	sensorBatteryPower   = "/sensor/Battery%20Power"
	textSensorDeviceName = "/text_sensor/Device%20Name"
	textSensorEspIP      = "/text_sensor/Esp%20ip"
	numberChargepower    = "/number/Forcible%20Charge%20Power"
	numberDischargePower = "/number/Forcible%20Discharge%20Power"
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
	soc, err := c.getSensorFloat(sensorSOC)
	if err != nil {
		return nil, fmt.Errorf("get SOC: %w", err)
	}

	// Temperature is optional - don't fail if unavailable
	temp, _ := c.getSensorFloat(sensorTemperature)

	// Capacity is optional
	capacity, _ := c.getSensorFloat(sensorRemainingCap)

	// Total energy for rated capacity
	ratedCapacity, _ := c.getSensorFloat(sensorTotalEnergy)

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
func (c *Client) GetESStatus() (*marstek.ESStatus, error) {
	soc, err := c.getSensorFloat(sensorSOC)
	if err != nil {
		return nil, fmt.Errorf("get SOC: %w", err)
	}

	// Battery power is optional
	power, _ := c.getSensorFloat(sensorBatteryPower)

	// Capacity is optional
	capacity, _ := c.getSensorFloat(sensorRemainingCap)

	return &marstek.ESStatus{
		BatterySOC:      int(soc),
		BatteryPower:    power,
		BatteryCapacity: capacity * 1000, // kWh to Wh
	}, nil
}

// Charge starts charging at the specified power (watts).
// timeoutS is ignored - ESPHome has no auto-timeout, service handles refresh.
func (c *Client) Charge(powerW int, _ int) error {
	// Set charge power first
	if err := c.setNumber(numberChargepower, float64(powerW)); err != nil {
		return fmt.Errorf("set charge power: %w", err)
	}

	// Then activate charge mode
	if err := c.setSelect(selectForceMode, "charge"); err != nil {
		return fmt.Errorf("set charge mode: %w", err)
	}

	return nil
}

// Discharge starts discharging at the specified power (watts).
// timeoutS is ignored - ESPHome has no auto-timeout, service handles refresh.
func (c *Client) Discharge(powerW int, _ int) error {
	// Set discharge power first
	if err := c.setNumber(numberDischargePower, float64(powerW)); err != nil {
		return fmt.Errorf("set discharge power: %w", err)
	}

	// Then activate discharge mode
	if err := c.setSelect(selectForceMode, "discharge"); err != nil {
		return fmt.Errorf("set discharge mode: %w", err)
	}

	return nil
}

// SetPassiveMode sets the battery mode based on power direction.
// Positive power = discharge, negative power = charge, zero = idle.
// cdTime is ignored - ESPHome has no auto-timeout.
func (c *Client) SetPassiveMode(power int, cdTime int) error {
	switch {
	case power < 0:
		// Negative = charge
		return c.Charge(-power, cdTime)
	case power > 0:
		// Positive = discharge
		return c.Discharge(power, cdTime)
	default:
		// Zero = idle
		return c.Idle()
	}
}

// Idle stops any forced charge/discharge operation.
func (c *Client) Idle() error {
	if err := c.setSelect(selectForceMode, "stop"); err != nil {
		return fmt.Errorf("set idle mode: %w", err)
	}
	return nil
}

// sensorResponse represents ESPHome sensor JSON response.
type sensorResponse struct {
	ID    string  `json:"id"`
	State float64 `json:"state"`
	Value float64 `json:"value"`
}

// textSensorResponse represents ESPHome text sensor JSON response.
type textSensorResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Value string `json:"value"`
}

// getSensorFloat retrieves a numeric sensor value.
func (c *Client) getSensorFloat(path string) (float64, error) {
	resp, err := c.httpClient.Get(c.baseURL + path)
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

	// ESPHome may return value in either "state" or "value" field
	if sensor.Value != 0 {
		return sensor.Value, nil
	}
	return sensor.State, nil
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
func (c *Client) setNumber(path string, value float64) error {
	endpoint := fmt.Sprintf("%s%s/set?value=%v", c.baseURL, path, value)
	resp, err := c.httpClient.Post(endpoint, "", nil)
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
func (c *Client) setSelect(path string, option string) error {
	endpoint := fmt.Sprintf("%s%s/set?option=%s", c.baseURL, path, url.QueryEscape(option))
	resp, err := c.httpClient.Post(endpoint, "", nil)
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

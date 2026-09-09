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
	defaultTimeout          = 10 * time.Second
	controlOperationTimeout = 45 * time.Second

	// ESPHome publishes select values every 15 seconds. Verify the current
	// battery-backed value first; when it differs, permit one retry only after
	// a full publication interval so duplicate Modbus frames do not pile up.
	espHomeControlPublicationInterval = 15 * time.Second
	controlConfirmationTimeout        = 35 * time.Second
	controlConfirmationInterval       = 500 * time.Millisecond
	controlWriteRetryDelay            = espHomeControlPublicationInterval
	controlWriteMaxAttempts           = 2

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
	sensorBatteryPower = "/sensor/Battery%20Power"
	sensorACPower      = "/sensor/AC%20Power"
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
	minSOC     int // Minimum SOC threshold for the service discharge flag

	restartButton string // ESPHome restart button name as used in its URLs; empty disables bridge restarts

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
	if minSOC < 0 || minSOC > 100 {
		minSOC = 11 // Default fallback
	}
	return &Client{
		baseURL: baseURL,
		minSOC:  minSOC,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		lastValues:    make(map[string]float64),
		now:           time.Now,
		probeWindow:   linkProbeWindow,
		probeInterval: linkProbeInterval,
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
	socInt, err := socAsInt(soc)
	if err != nil {
		return nil, err
	}

	// These are service-side SOC eligibility hints, not BMS interlock states.
	return &marstek.BatteryStatus{
		SOC:          socInt,
		ChargingFlag: socInt < 100,
		DischargFlag: socInt > c.minSOC,
	}, nil
}

// GetESStatus returns the energy system status.
func (c *Client) GetESStatus(ctx context.Context) (*marstek.ESStatus, error) {
	soc, err := c.getSensorFloatContext(ctx, sensorSOC)
	if err != nil {
		return nil, fmt.Errorf("get SOC: %w", err)
	}
	socInt, err := socAsInt(soc)
	if err != nil {
		return nil, err
	}

	power, err := c.getSensorFloatContext(ctx, sensorBatteryPower)
	if err != nil {
		return nil, fmt.Errorf("get battery power: %w", err)
	}

	return &marstek.ESStatus{
		BatterySOC:   socInt,
		BatteryPower: power,
	}, nil
}

// GetBatteryPower returns the signed battery power: positive charging, negative discharging.
func (c *Client) GetBatteryPower(ctx context.Context) (float64, error) {
	return c.getSensorFloatContext(ctx, sensorBatteryPower)
}

// GetACSample returns the current SOC and normalized AC power: positive charging, negative discharging.
func (c *Client) GetACSample(ctx context.Context) (int, float64, error) {
	soc, err := c.getSensorFloatContext(ctx, sensorSOC)
	if err != nil {
		return 0, 0, fmt.Errorf("get SOC: %w", err)
	}
	socInt, err := socAsInt(soc)
	if err != nil {
		return 0, 0, err
	}

	power, err := c.getSensorFloatContext(ctx, sensorACPower)
	if err != nil {
		return 0, 0, fmt.Errorf("get AC power: %w", err)
	}
	return socInt, -power, nil
}

// Charge starts charging at the specified power (watts).
// timeoutS is ignored - ESPHome has no auto-timeout, service handles refresh.
func (c *Client) Charge(powerW int, _ int) error {
	return c.ChargeContext(context.Background(), powerW, 0)
}

// ChargeContext starts charging and allows cancellation while ESPHome applies each control write.
func (c *Client) ChargeContext(ctx context.Context, powerW int, _ int) error {
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()

	if err := c.ensureRS485ControlMode(ctx); err != nil {
		return fmt.Errorf("%w: %w", marstek.ErrControlNotAttempted, c.classifyControlFailure(ctx, fmt.Errorf("enable RS485 control mode: %w", err)))
	}

	if err := c.setNumber(ctx, numberChargepower, float64(powerW)); err != nil {
		return fmt.Errorf("%w: %w", marstek.ErrControlNotAttempted, c.classifyControlFailure(ctx, fmt.Errorf("set charge power: %w", err)))
	}
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
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()

	if err := c.ensureRS485ControlMode(ctx); err != nil {
		return fmt.Errorf("%w: %w", marstek.ErrControlNotAttempted, c.classifyControlFailure(ctx, fmt.Errorf("enable RS485 control mode: %w", err)))
	}

	if err := c.setNumber(ctx, numberDischargePower, float64(powerW)); err != nil {
		return fmt.Errorf("%w: %w", marstek.ErrControlNotAttempted, c.classifyControlFailure(ctx, fmt.Errorf("set discharge power: %w", err)))
	}
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
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()

	switch {
	case power < 0:
		return c.ChargeContext(ctx, -power, cdTime)
	case power > 0:
		return c.DischargeContext(ctx, power, cdTime)
	default:
		return c.IdleContext(ctx)
	}
}

// Idle stops any forced charge/discharge operation.
func (c *Client) Idle() error {
	return c.IdleContext(context.Background())
}

// IdleContext stops forced operation and allows cancellation during control confirmation.
func (c *Client) IdleContext(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()

	var enableErr error
	if err := c.ensureRS485ControlMode(ctx); err != nil {
		enableErr = fmt.Errorf("enable RS485 control mode: %w", err)
	}
	if err := c.setSelectConfirmed(ctx, selectForceMode, "stop"); err != nil {
		return c.classifyControlFailure(ctx, errors.Join(enableErr, fmt.Errorf("stop forcible mode: %w", err)))
	}
	// A cached select cannot prove a stop while the Modbus link is frozen.
	if errors.Is(enableErr, marstek.ErrLinkDown) {
		return enableErr
	}
	return nil
}

// sensorResponse represents ESPHome sensor JSON response.
// Note: ESPHome returns "state" as a formatted string with unit (e.g., "11.0 %"),
// while "value" is the raw numeric value. We only use "value".
type sensorResponse struct {
	ID    string          `json:"id"`
	State string          `json:"state"` // Formatted string with unit, not used
	Value json.RawMessage `json:"value"` // Raw numeric value
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

func sensorFloatValue(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing numeric value")
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, errors.New("null numeric value")
	}

	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric value: %w", err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("non-finite numeric value %q", raw)
	}
	return value, nil
}

func socAsInt(soc float64) (int, error) {
	if soc < 0 || soc > 100 {
		return 0, fmt.Errorf("SOC %.2f outside [0, 100]", soc)
	}
	return int(soc), nil
}

func (c *Client) getSensorFloatContext(ctx context.Context, path string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("create GET %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, c.redactedTransportError("GET "+path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, responseBodyMessage(body))
	}

	var sensor sensorResponse
	if err := json.NewDecoder(resp.Body).Decode(&sensor); err != nil {
		return 0, fmt.Errorf("decode sensor response: %w", err)
	}
	value, err := sensorFloatValue(sensor.Value)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}

	c.recordTelemetry(path, value)
	return value, nil
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
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()

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

// SetRestartButton names the ESPHome restart button as it appears in the device's web
// server URLs (e.g. "Restart" for `name: "Restart"`). Empty disables bridge restarts.
func (c *Client) SetRestartButton(name string) {
	c.restartButton = name
}

// RestartAvailable reports whether a restart button is configured.
func (c *Client) RestartAvailable() bool {
	return c.restartButton != ""
}

// restartButtonCandidates lists the URL spellings to try for the restart button.
// Depending on the ESPHome build and config, the web server keys entities by the
// display name ("Restart", what the Marstek bridge does — see the other entity paths
// above) or by the snake_case object id ("restart"). A 404 costs nothing, so try the
// configured value first and the other convention after it.
func restartButtonCandidates(configured string) []string {
	slug := strings.ToLower(strings.Join(strings.Fields(configured), "_"))
	title := slug
	if title != "" {
		words := strings.Split(slug, "_")
		for i, w := range words {
			if w != "" {
				words[i] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		title = strings.Join(words, " ")
	}
	seen := map[string]bool{}
	var out []string
	for _, cand := range []string{configured, title, slug} {
		if cand == "" || seen[cand] {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
	}
	return out
}

// RestartDevice presses the ESPHome restart button to reboot the bridge, which is the
// only known way to recover a frozen RS485 link.
func (c *Client) RestartDevice(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	defer cancel()
	if c.restartButton == "" {
		return fmt.Errorf("restart button not configured")
	}
	var lastErr error
	for _, name := range restartButtonCandidates(c.restartButton) {
		status, err := c.pressButton(ctx, name)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			lastErr = fmt.Errorf("POST /button/%s/press: status 404", name)
			continue
		}
		if status < 200 || status > 299 {
			return fmt.Errorf("POST /button/%s/press: status %d", name, status)
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return fmt.Errorf("restart button %q not found on the device: %w", c.restartButton, lastErr)
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

// pressButton POSTs an ESPHome button press and returns the HTTP status; transport
// failures are returned as errors.
func (c *Client) pressButton(ctx context.Context, name string) (int, error) {
	endpoint := fmt.Sprintf("%s/button/%s/press", c.baseURL, url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("create POST restart request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, c.redactedTransportError("POST restart", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
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
// proves the RS485 link is dead. The link probe detects only complete freezes; it
// cannot confirm or reject an individual optimistic control write.
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
	return fmt.Errorf("%w: telemetry frozen and control outcome unknown: %w", marstek.ErrLinkDown, err)
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
		return "", fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, responseBodyMessage(body))
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

// setNumber sends a number entity POST and returns when ESPHome acknowledges it.
// ESPHome echoes number writes optimistically, so the service must monitor measured
// response to establish physical power; the link probe detects only full freezes.
func (c *Client) setNumber(ctx context.Context, path string, value float64) error {
	endpoint := fmt.Sprintf("%s%s/set?value=%v", c.baseURL, path, value)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("create POST %s request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.redactedTransportError("POST "+path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, responseBodyMessage(body))
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
		return c.redactedTransportError("POST "+path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, responseBodyMessage(body))
	}

	return nil
}

func (c *Client) ensureRS485ControlMode(ctx context.Context) error {
	if since, down := c.linkDown(); down {
		return fmt.Errorf("%w: %w for %s", marstek.ErrControlNotAttempted, marstek.ErrLinkDown, c.now().Sub(since).Round(time.Second))
	}
	return c.setSelectConfirmed(ctx, selectRS485ControlMode, "enable")
}

func (c *Client) setSelectConfirmed(ctx context.Context, path string, option string) error {
	actual, err := c.getControlValue(ctx, path)
	if err == nil && actual == option {
		return nil
	}
	if err := c.setSelect(ctx, path, option); err != nil {
		return err
	}
	return c.waitForControlValue(ctx, path, option)
}

func (c *Client) waitForControlValue(ctx context.Context, path string, expected string) error {
	confirmationCtx, cancel := context.WithTimeout(ctx, controlConfirmationTimeout)
	defer cancel()

	pollTicker := time.NewTicker(controlConfirmationInterval)
	defer pollTicker.Stop()
	retryTimer := time.NewTimer(controlWriteRetryDelay)
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
			if attempts < controlWriteMaxAttempts {
				attempts++
				if err := c.setSelect(confirmationCtx, path, expected); confirmationCtx.Err() == nil {
					lastWriteErr = err
				}
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
		return "", c.redactedTransportError("GET "+path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, responseBodyMessage(body))
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

func responseBodyMessage(body []byte) string {
	return strings.Join(strings.Fields(string(body)), " ")
}

func (c *Client) redactedTransportError(action string, err error) error {
	return &endpointTransportError{action: action, message: redactEndpoint(err.Error(), c.baseURL), err: err}
}

type endpointTransportError struct {
	action  string
	message string
	err     error
}

func (e *endpointTransportError) Error() string { return e.action + ": " + e.message }
func (e *endpointTransportError) Unwrap() error { return e.err }

func redactEndpoint(message, baseURL string) string {
	values := []string{baseURL}
	if parsed, err := url.Parse(baseURL); err == nil {
		values = append(values, parsed.Host, parsed.Hostname())
	}
	for _, value := range values {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED_ENDPOINT]")
		}
	}
	return message
}

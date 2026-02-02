package marstek

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPort    = 30000
	defaultTimeout = 5 * time.Second
)

// Client is a Marstek battery UDP client.
type Client struct {
	addr      string
	conn      *net.UDPConn
	requestID atomic.Int64
	mu        sync.Mutex // protects UDP operations
}

// New creates a new Marstek client.
func New(addr string) *Client {
	return &Client{
		addr: addr,
	}
}

// Connect establishes the UDP connection.
// The client must bind to port 30000 as the source port.
func (c *Client) Connect() error {
	// Parse the target address
	raddr, err := net.ResolveUDPAddr("udp", c.addr)
	if err != nil {
		return fmt.Errorf("resolve address: %w", err)
	}

	// Bind to local port 30000 (required by Marstek protocol)
	laddr := &net.UDPAddr{Port: defaultPort}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return fmt.Errorf("bind to port %d: %w", defaultPort, err)
	}

	c.conn = conn

	// Store the remote address for sending
	_ = raddr
	return nil
}

// Close closes the UDP connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// request represents a JSON-RPC style request.
type request struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// response represents a JSON-RPC style response.
type response struct {
	ID     int64           `json:"id"`
	Src    string          `json:"src"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// DeviceInfo contains device discovery information.
type DeviceInfo struct {
	Device   string `json:"device"`
	Version  int    `json:"ver"`
	BLEMAC   string `json:"ble_mac"`
	WiFiMAC  string `json:"wifi_mac"`
	WiFiName string `json:"wifi_name"`
	IP       string `json:"ip"`
}

// BatteryStatus contains battery state information.
type BatteryStatus struct {
	ID            int     `json:"id"`
	SOC           int     `json:"soc"`            // State of charge (%)
	ChargingFlag  bool    `json:"charg_flag"`     // Charging permitted
	DischargFlag  bool    `json:"dischrg_flag"`   // Discharging permitted
	Temperature   float64 `json:"bat_temp"`       // Battery temperature (°C)
	Capacity      float64 `json:"bat_capacity"`   // Remaining capacity (Wh)
	RatedCapacity float64 `json:"rated_capacity"` // Rated capacity (Wh)
}

// ESStatus contains energy system status.
type ESStatus struct {
	ID                    int     `json:"id"`
	BatterySOC            int     `json:"bat_soc"`                  // Battery SOC (%)
	BatteryCapacity       float64 `json:"bat_cap"`                  // Battery capacity (Wh)
	PVPower               float64 `json:"pv_power"`                 // Solar power (W)
	OnGridPower           float64 `json:"ongrid_power"`             // Grid-tied power (W)
	OffGridPower          float64 `json:"offgrid_power"`            // Off-grid power (W)
	BatteryPower          float64 `json:"bat_power"`                // Battery power (W)
	TotalPVEnergy         float64 `json:"total_pv_energy"`          // Total solar energy (Wh)
	TotalGridOutputEnergy float64 `json:"total_grid_output_energy"` // Total export (Wh)
	TotalGridInputEnergy  float64 `json:"total_grid_input_energy"`  // Total import (Wh)
	TotalLoadEnergy       float64 `json:"total_load_energy"`        // Total load (Wh)
}

// ESMode contains the current operating mode.
type ESMode struct {
	ID           int     `json:"id"`
	Mode         string  `json:"mode"`          // "Auto", "AI", "Manual", "Passive"
	OnGridPower  float64 `json:"ongrid_power"`  // Grid-tied power (W)
	OffGridPower float64 `json:"offgrid_power"` // Off-grid power (W)
	BatterySOC   int     `json:"bat_soc"`       // Battery SOC (%)
}

// send sends a request and waits for response with matching ID.
func (c *Client) send(method string, params interface{}) (*response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	id := c.requestID.Add(1)
	req := request{
		ID:     id,
		Method: method,
		Params: params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Send to broadcast address
	raddr, err := net.ResolveUDPAddr("udp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("resolve address: %w", err)
	}

	_, err = c.conn.WriteToUDP(data, raddr)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Set read deadline
	deadline := time.Now().Add(defaultTimeout)
	c.conn.SetReadDeadline(deadline)

	// Read responses until we get one with matching ID or timeout
	buf := make([]byte, 4096)
	for {
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		var resp response
		if err := json.Unmarshal(buf[:n], &resp); err != nil {
			// Invalid JSON, try next packet
			continue
		}

		// Check if response ID matches our request
		if resp.ID != id {
			// Wrong response (maybe from previous request or another device)
			// Keep reading until timeout
			continue
		}

		// Skip echoed requests (broadcast packets we sent that we receive back).
		// Valid responses always have a "src" field from the device.
		if resp.Src == "" {
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}

		return &resp, nil
	}
}

// Discover discovers Marstek devices on the network.
func (c *Client) Discover() (*DeviceInfo, error) {
	params := map[string]string{"ble_mac": "0"}
	resp, err := c.send("Marstek.GetDevice", params)
	if err != nil {
		return nil, err
	}

	var info DeviceInfo
	if err := json.Unmarshal(resp.Result, &info); err != nil {
		return nil, fmt.Errorf("unmarshal device info: %w", err)
	}

	return &info, nil
}

// GetBatteryStatus gets the current battery status.
// It retries on timeout and falls back to ES.GetStatus if Bat.GetStatus fails.
func (c *Client) GetBatteryStatus() (*BatteryStatus, error) {
	params := map[string]int{"id": 0}

	// Try Bat.GetStatus with retry (battery can be intermittently unresponsive)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.send("Bat.GetStatus", params)
		if err != nil {
			lastErr = err
			continue
		}

		var status BatteryStatus
		if err := json.Unmarshal(resp.Result, &status); err != nil {
			lastErr = fmt.Errorf("unmarshal battery status: %w", err)
			continue
		}

		// Default ChargingFlag to true if not present in response
		// (some firmware versions don't include it)
		if status.SOC < 100 {
			status.ChargingFlag = true
		}

		return &status, nil
	}

	// Fallback to ES.GetStatus for SOC (more reliable on some firmware)
	esStatus, err := c.GetESStatus()
	if err != nil {
		// Return original error if fallback also fails
		return nil, lastErr
	}

	// Construct minimal BatteryStatus from ES status
	return &BatteryStatus{
		SOC:          esStatus.BatterySOC,
		ChargingFlag: true, // Assume charging is allowed
		DischargFlag: true, // Assume discharging is allowed
	}, nil
}

// GetESStatus gets the energy system status.
func (c *Client) GetESStatus() (*ESStatus, error) {
	params := map[string]int{"id": 0}
	resp, err := c.send("ES.GetStatus", params)
	if err != nil {
		return nil, err
	}

	var status ESStatus
	if err := json.Unmarshal(resp.Result, &status); err != nil {
		return nil, fmt.Errorf("unmarshal ES status: %w", err)
	}

	return &status, nil
}

// GetESMode gets the current operating mode.
func (c *Client) GetESMode() (*ESMode, error) {
	params := map[string]int{"id": 0}
	resp, err := c.send("ES.GetMode", params)
	if err != nil {
		return nil, err
	}

	var mode ESMode
	if err := json.Unmarshal(resp.Result, &mode); err != nil {
		return nil, fmt.Errorf("unmarshal ES mode: %w", err)
	}

	return &mode, nil
}

// SetPassiveMode sets the battery to passive mode with specified power.
// Positive power = discharge, negative power = charge.
// cdTime is the countdown in seconds before reverting to previous mode.
func (c *Client) SetPassiveMode(power int, cdTime int) error {
	params := map[string]interface{}{
		"id": 0,
		"config": map[string]interface{}{
			"mode": "Passive",
			"passive_cfg": map[string]int{
				"power":   power,
				"cd_time": cdTime,
			},
		},
	}

	resp, err := c.send("ES.SetMode", params)
	if err != nil {
		return err
	}

	var result struct {
		ID        int  `json:"id"`
		SetResult bool `json:"set_result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("unmarshal set result: %w", err)
	}

	if !result.SetResult {
		return fmt.Errorf("set mode failed")
	}

	return nil
}

// SetAutoMode sets the battery to auto mode.
func (c *Client) SetAutoMode() error {
	params := map[string]interface{}{
		"id": 0,
		"config": map[string]interface{}{
			"mode": "Auto",
			"auto_cfg": map[string]int{
				"enable": 1,
			},
		},
	}

	resp, err := c.send("ES.SetMode", params)
	if err != nil {
		return err
	}

	var result struct {
		ID        int  `json:"id"`
		SetResult bool `json:"set_result"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return fmt.Errorf("unmarshal set result: %w", err)
	}

	if !result.SetResult {
		return fmt.Errorf("set mode failed")
	}

	return nil
}

// Charge starts charging at the specified power (watts).
func (c *Client) Charge(powerW int, timeoutS int) error {
	// Negative power = charge
	return c.SetPassiveMode(-powerW, timeoutS)
}

// Discharge starts discharging at the specified power (watts).
func (c *Client) Discharge(powerW int, timeoutS int) error {
	// Positive power = discharge
	return c.SetPassiveMode(powerW, timeoutS)
}

// Idle sets the battery to auto/idle mode.
func (c *Client) Idle() error {
	return c.SetAutoMode()
}

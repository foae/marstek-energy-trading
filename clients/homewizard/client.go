package homewizard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultTimeout = 5 * time.Second

// DeviceInfo contains basic HomeWizard device information.
type DeviceInfo struct {
	ProductName string `json:"product_name"`
	ProductType string `json:"product_type"`
	Serial      string `json:"serial"`
	Firmware    string `json:"firmware_version"`
	APIVersion  string `json:"api_version"`
}

// dataResponse represents the /api/v1/data response from a HomeWizard P1 meter.
type dataResponse struct {
	ActivePowerW float64 `json:"active_power_w"`
}

// Client is a HomeWizard P1 meter HTTP client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	enabled    bool
}

// New creates a new HomeWizard P1 client.
// If baseURL is empty, the client is disabled and all methods return gracefully.
func New(baseURL string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		enabled: baseURL != "",
	}
}

// Enabled returns true if the P1 meter is configured.
func (c *Client) Enabled() bool {
	return c.enabled
}

// GetDeviceInfo fetches device info from the P1 meter (connectivity check).
func (c *Client) GetDeviceInfo() (*DeviceInfo, error) {
	if !c.enabled {
		return nil, fmt.Errorf("homewizard P1 meter not configured")
	}

	resp, err := c.httpClient.Get(c.baseURL + "/api")
	if err != nil {
		return nil, fmt.Errorf("GET /api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /api: status %d: %s", resp.StatusCode, string(body))
	}

	var info DeviceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode device info: %w", err)
	}

	return &info, nil
}

// GetActivePowerW returns the current active power in watts from the P1 meter.
// Positive values mean importing from grid, negative values mean exporting (solar surplus).
func (c *Client) GetActivePowerW() (float64, error) {
	if !c.enabled {
		return 0, fmt.Errorf("homewizard P1 meter not configured")
	}

	resp, err := c.httpClient.Get(c.baseURL + "/api/v1/data")
	if err != nil {
		return 0, fmt.Errorf("GET /api/v1/data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GET /api/v1/data: status %d: %s", resp.StatusCode, string(body))
	}

	var data dataResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, fmt.Errorf("decode data response: %w", err)
	}

	return data.ActivePowerW, nil
}

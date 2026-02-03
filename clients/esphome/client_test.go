package esphome

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnect_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/text_sensor/Device%20Name" || r.URL.Path == "/text_sensor/Device Name" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"text_sensor-device_name","state":"Marstek Venus E"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	err := client.Connect()
	if err != nil {
		t.Errorf("Connect() error = %v, want nil", err)
	}
}

func TestConnect_Failure(t *testing.T) {
	// Connect to a non-existent server
	client := New("http://127.0.0.1:1", 11)
	err := client.Connect()
	if err == nil {
		t.Error("Connect() error = nil, want error for unreachable device")
	}
}

func TestDiscover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "Device"):
			w.Write([]byte(`{"id":"text_sensor-device_name","state":"Marstek Venus E"}`))
		case strings.Contains(r.URL.Path, "ip"):
			w.Write([]byte(`{"id":"text_sensor-esp_ip","state":"192.168.1.50"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	info, err := client.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if info.Device != "Marstek Venus E" {
		t.Errorf("Device = %q, want %q", info.Device, "Marstek Venus E")
	}
	if info.IP != "192.168.1.50" {
		t.Errorf("IP = %q, want %q", info.IP, "192.168.1.50")
	}
}

func TestGetBatteryStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge"):
			w.Write([]byte(`{"id":"sensor-soc","state":75}`))
		case strings.Contains(r.URL.Path, "Temperature"):
			w.Write([]byte(`{"id":"sensor-temp","state":25.5}`))
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","state":3.84}`)) // kWh
		case strings.Contains(r.URL.Path, "Total%20Energy") || strings.Contains(r.URL.Path, "Total Energy"):
			w.Write([]byte(`{"id":"sensor-total","state":5.12}`)) // kWh
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetBatteryStatus()
	if err != nil {
		t.Fatalf("GetBatteryStatus() error = %v", err)
	}
	if status.SOC != 75 {
		t.Errorf("SOC = %d, want 75", status.SOC)
	}
	if status.Temperature != 25.5 {
		t.Errorf("Temperature = %v, want 25.5", status.Temperature)
	}
	if status.Capacity != 3840 { // kWh * 1000 = Wh
		t.Errorf("Capacity = %v, want 3840", status.Capacity)
	}
	if !status.ChargingFlag {
		t.Error("ChargingFlag = false, want true (SOC < 100)")
	}
	if !status.DischargFlag {
		t.Error("DischargFlag = false, want true (SOC > 11)")
	}
}

func TestGetBatteryStatus_PartialFailure(t *testing.T) {
	// Only SOC available, other sensors fail
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge") {
			w.Write([]byte(`{"id":"sensor-soc","state":50}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetBatteryStatus()
	if err != nil {
		t.Fatalf("GetBatteryStatus() error = %v", err)
	}
	if status.SOC != 50 {
		t.Errorf("SOC = %d, want 50", status.SOC)
	}
	// Other fields should be zero but not cause failure
	if status.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0 (unavailable)", status.Temperature)
	}
}

func TestGetBatteryStatus_ChargingFlags(t *testing.T) {
	tests := []struct {
		name         string
		soc          int
		wantCharging bool
		wantDischarg bool
	}{
		{"SOC 100 - full", 100, false, true},
		{"SOC 11 - min", 11, true, false},
		{"SOC 10 - below min", 10, true, false},
		{"SOC 50 - normal", 50, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			soc := tt.soc // capture for closure
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge") {
					fmt.Fprintf(w, `{"id":"sensor-soc","state":%d}`, soc)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			client := New(server.URL, 11)
			status, err := client.GetBatteryStatus()
			if err != nil {
				t.Fatalf("GetBatteryStatus() error = %v", err)
			}
			if status.ChargingFlag != tt.wantCharging {
				t.Errorf("ChargingFlag = %v, want %v", status.ChargingFlag, tt.wantCharging)
			}
			if status.DischargFlag != tt.wantDischarg {
				t.Errorf("DischargFlag = %v, want %v", status.DischargFlag, tt.wantDischarg)
			}
		})
	}
}

func TestCharge(t *testing.T) {
	var calledPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths = append(calledPaths, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	err := client.Charge(2500, 300)
	if err != nil {
		t.Fatalf("Charge() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should set power
	if !strings.Contains(calledPaths[0], "Charge%20Power") || !strings.Contains(calledPaths[0], "value=2500") {
		t.Errorf("first call should set charge power, got: %s", calledPaths[0])
	}

	// Second call should set mode to charge
	if !strings.Contains(calledPaths[1], "Discharge") || !strings.Contains(calledPaths[1], "option=charge") {
		t.Errorf("second call should set mode to charge, got: %s", calledPaths[1])
	}
}

func TestDischarge(t *testing.T) {
	var calledPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths = append(calledPaths, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	err := client.Discharge(800, 300)
	if err != nil {
		t.Fatalf("Discharge() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should set power
	if !strings.Contains(calledPaths[0], "Discharge%20Power") || !strings.Contains(calledPaths[0], "value=800") {
		t.Errorf("first call should set discharge power, got: %s", calledPaths[0])
	}

	// Second call should set mode to discharge
	if !strings.Contains(calledPaths[1], "option=discharge") {
		t.Errorf("second call should set mode to discharge, got: %s", calledPaths[1])
	}
}

func TestIdle(t *testing.T) {
	var calledPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	err := client.Idle()
	if err != nil {
		t.Fatalf("Idle() error = %v", err)
	}

	if !strings.Contains(calledPath, "option=stop") {
		t.Errorf("Idle should set mode to stop, got: %s", calledPath)
	}
}

func TestSetPassiveMode_Charge(t *testing.T) {
	var calledPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths = append(calledPaths, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	// Negative power = charge
	err := client.SetPassiveMode(-2500, 300)
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls for charge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[1], "option=charge") {
		t.Errorf("negative power should trigger charge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Discharge(t *testing.T) {
	var calledPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths = append(calledPaths, r.URL.String())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	// Positive power = discharge
	err := client.SetPassiveMode(800, 300)
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls for discharge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[1], "option=discharge") {
		t.Errorf("positive power should trigger discharge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Idle(t *testing.T) {
	var calledPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	// Zero power = idle
	err := client.SetPassiveMode(0, 300)
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if !strings.Contains(calledPath, "option=stop") {
		t.Errorf("zero power should trigger idle/stop mode, got: %s", calledPath)
	}
}

func TestGetESStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge"):
			w.Write([]byte(`{"id":"sensor-soc","state":80}`))
		case strings.Contains(r.URL.Path, "Battery%20Power") || strings.Contains(r.URL.Path, "Battery Power"):
			w.Write([]byte(`{"id":"sensor-power","state":-1500}`)) // negative = charging
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","state":4.1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetESStatus()
	if err != nil {
		t.Fatalf("GetESStatus() error = %v", err)
	}
	if status.BatterySOC != 80 {
		t.Errorf("BatterySOC = %d, want 80", status.BatterySOC)
	}
	if status.BatteryPower != -1500 {
		t.Errorf("BatteryPower = %v, want -1500", status.BatteryPower)
	}
	if status.BatteryCapacity != 4100 {
		t.Errorf("BatteryCapacity = %v, want 4100", status.BatteryCapacity)
	}
}

func TestClose(t *testing.T) {
	client := New("http://localhost", 11)
	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v, want nil (no-op)", err)
	}
}

func TestHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL, 11)

	_, err := client.GetBatteryStatus()
	if err == nil {
		t.Error("GetBatteryStatus() error = nil, want error for 500 response")
	}

	err = client.Charge(2500, 300)
	if err == nil {
		t.Error("Charge() error = nil, want error for 500 response")
	}
}

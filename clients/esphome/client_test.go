package esphome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newControlTestServer(t *testing.T, failOption string) (*httptest.Server, *[]string) {
	t.Helper()
	calledPaths := make([]string, 0)
	values := map[string]string{
		"/select/RS485 Control Mode":        "disable",
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
		"/number/Forcible Discharge Power":  "0",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"value": values[r.URL.Path], "state": values[r.URL.Path]})
			return
		}

		calledPaths = append(calledPaths, r.URL.String())
		if failOption != "" && r.URL.Query().Get("option") == failOption {
			http.Error(w, failOption+" failed", http.StatusInternalServerError)
			return
		}
		entityPath := strings.TrimSuffix(r.URL.Path, "/set")
		if option := r.URL.Query().Get("option"); option != "" {
			values[entityPath] = option
		}
		if value := r.URL.Query().Get("value"); value != "" {
			values[entityPath] = value
		}
		w.WriteHeader(http.StatusOK)
	}))
	return server, &calledPaths
}

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
			w.Write([]byte(`{"id":"sensor-soc","value":75,"state":"75 %"}`))
		case strings.Contains(r.URL.Path, "Temperature"):
			w.Write([]byte(`{"id":"sensor-temp","value":25.5,"state":"25.5 °C"}`))
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","value":3.84,"state":"3.84 kWh"}`))
		case strings.Contains(r.URL.Path, "Total%20Energy") || strings.Contains(r.URL.Path, "Total Energy"):
			w.Write([]byte(`{"id":"sensor-total","value":5.12,"state":"5.12 kWh"}`))
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
			w.Write([]byte(`{"id":"sensor-soc","value":50,"state":"50 %"}`))
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
					fmt.Fprintf(w, `{"id":"sensor-soc","value":%d,"state":"%d %%"}`, soc, soc)
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
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Charge(2500, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Charge() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should enable RS485 control mode
	if !strings.Contains(calledPaths[0], "RS485%20Control%20Mode") || !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}

	// Second call should set charge power
	if !strings.Contains(calledPaths[1], "Charge%20Power") || !strings.Contains(calledPaths[1], "value=2500") {
		t.Errorf("second call should set charge power, got: %s", calledPaths[1])
	}

	// Third call should force mode to charge
	if !strings.Contains(calledPaths[2], "option=charge") {
		t.Errorf("third call should force mode to charge, got: %s", calledPaths[2])
	}
}

func TestControlConfirmationTimeoutCoversESPHomePublicationCycle(t *testing.T) {
	if controlConfirmationTimeout <= espHomeControlPublicationInterval {
		t.Fatalf(
			"control confirmation timeout %s must exceed ESPHome publication interval %s",
			controlConfirmationTimeout,
			espHomeControlPublicationInterval,
		)
	}
}

func TestChargeWaitsForControlConfirmation(t *testing.T) {
	values := map[string]string{
		"/select/RS485 Control Mode":        "disable",
		"/select/Forcible Charge⁄Discharge": "stop",
		"/number/Forcible Charge Power":     "0",
	}
	pending := make(map[string]string)
	getCounts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entityPath := strings.TrimSuffix(r.URL.Path, "/set")
		if r.Method == http.MethodPost {
			pending[entityPath] = r.URL.Query().Get("option")
			if pending[entityPath] == "" {
				pending[entityPath] = r.URL.Query().Get("value")
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		if next, ok := pending[entityPath]; ok {
			getCounts[entityPath]++
			if getCounts[entityPath] >= 2 {
				if entityPath == "/number/Forcible Charge Power" {
					next = "500.4"
				}
				values[entityPath] = next
				delete(pending, entityPath)
			}
		}
		if strings.HasPrefix(entityPath, "/number/") {
			_, _ = fmt.Fprintf(w, `{"value":%s}`, values[entityPath])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": values[entityPath]})
	}))
	defer server.Close()

	if err := New(server.URL, 11).Charge(500, 300); err != nil {
		t.Fatalf("Charge() error = %v", err)
	}
	for _, entityPath := range []string{
		"/select/RS485 Control Mode",
		"/number/Forcible Charge Power",
		"/select/Forcible Charge⁄Discharge",
	} {
		if getCounts[entityPath] < 2 {
			t.Errorf("%s GET count = %d, want at least 2", entityPath, getCounts[entityPath])
		}
	}
}

func TestChargeContext_CancelsControlConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "disable"})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := New(server.URL, 11).ChargeContext(ctx, 500, 300)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ChargeContext() error = %v, want context deadline exceeded", err)
	}
}

func TestDischarge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Discharge(800, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Discharge() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	// First call should enable RS485 control mode
	if !strings.Contains(calledPaths[0], "RS485%20Control%20Mode") || !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}

	// Second call should set discharge power
	if !strings.Contains(calledPaths[1], "Discharge%20Power") || !strings.Contains(calledPaths[1], "value=800") {
		t.Errorf("second call should set discharge power, got: %s", calledPaths[1])
	}

	// Third call should force mode to discharge
	if !strings.Contains(calledPaths[2], "option=discharge") {
		t.Errorf("third call should force mode to discharge, got: %s", calledPaths[2])
	}
}

func TestIdle(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Idle()
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("Idle() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calledPaths), calledPaths)
	}

	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should enable RS485 control mode, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("second call should force mode to stop, got: %s", calledPaths[1])
	}
	if !strings.Contains(calledPaths[2], "RS485%20Control%20Mode") || !strings.Contains(calledPaths[2], "option=disable") {
		t.Errorf("third call should disable RS485 control mode, got: %s", calledPaths[2])
	}
}

func TestIdle_StopFailureKeepsRS485Enabled(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "stop")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	err := client.Idle()
	calledPaths = *calledPathsPtr
	if err == nil {
		t.Fatal("Idle() error = nil, want error when stop fails")
	}

	if len(calledPaths) != 2 {
		t.Fatalf("expected 2 calls (enable + stop), got %d: %v", len(calledPaths), calledPaths)
	}

	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("first call should be enable, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("second call should be stop, got: %s", calledPaths[1])
	}
	for _, path := range calledPaths {
		if strings.Contains(path, "option=disable") {
			t.Errorf("RS485 must remain enabled when stop is unconfirmed: %v", calledPaths)
		}
	}

	// Error should identify the stop failure
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("error should mention stop, got: %v", err)
	}
}

func TestIdle_CleanupFailuresAreSafeAfterConfirmedStop(t *testing.T) {
	for _, failOption := range []string{"enable", "disable"} {
		t.Run(failOption, func(t *testing.T) {
			server, calledPathsPtr := newControlTestServer(t, failOption)
			defer server.Close()

			if err := New(server.URL, 11).Idle(); err != nil {
				t.Fatalf("Idle() error = %v after confirmed stop", err)
			}
			calledPaths := *calledPathsPtr
			if len(calledPaths) != 3 {
				t.Fatalf("calls = %d, want enable + stop + disable: %v", len(calledPaths), calledPaths)
			}
			if !strings.Contains(calledPaths[1], "option=stop") {
				t.Errorf("second call should confirm stop, got %s", calledPaths[1])
			}
		})
	}
}

func TestSetPassiveMode_Charge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Negative power = charge
	err := client.SetPassiveMode(-2500, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls for charge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[2], "option=charge") {
		t.Errorf("negative power should trigger charge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Discharge(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Positive power = discharge
	err := client.SetPassiveMode(800, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls for discharge, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[2], "option=discharge") {
		t.Errorf("positive power should trigger discharge mode, got: %v", calledPaths)
	}
}

func TestSetPassiveMode_Idle(t *testing.T) {
	server, calledPathsPtr := newControlTestServer(t, "")
	defer server.Close()
	calledPaths := *calledPathsPtr

	client := New(server.URL, 11)
	// Zero power = idle
	err := client.SetPassiveMode(0, 300)
	calledPaths = *calledPathsPtr
	if err != nil {
		t.Fatalf("SetPassiveMode() error = %v", err)
	}

	if len(calledPaths) != 3 {
		t.Fatalf("expected 3 calls for idle, got %d", len(calledPaths))
	}
	if !strings.Contains(calledPaths[0], "option=enable") {
		t.Errorf("zero power should enable RS485 control mode first, got: %s", calledPaths[0])
	}
	if !strings.Contains(calledPaths[1], "option=stop") {
		t.Errorf("zero power should trigger idle/stop mode, got: %s", calledPaths[1])
	}
	if !strings.Contains(calledPaths[2], "option=disable") {
		t.Errorf("third call should disable RS485 control mode, got: %s", calledPaths[2])
	}
}

func TestGetESStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "State%20Of%20Charge") || strings.Contains(r.URL.Path, "State Of Charge"):
			w.Write([]byte(`{"id":"sensor-soc","value":80,"state":"80 %"}`))
		case strings.Contains(r.URL.Path, "Battery%20Power") || strings.Contains(r.URL.Path, "Battery Power"):
			w.Write([]byte(`{"id":"sensor-power","value":1500,"state":"1500 W"}`)) // positive = charging
		case strings.Contains(r.URL.Path, "Remaining%20Capacity") || strings.Contains(r.URL.Path, "Remaining Capacity"):
			w.Write([]byte(`{"id":"sensor-cap","value":4.1,"state":"4.1 kWh"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(server.URL, 11)
	status, err := client.GetESStatus(context.Background())
	if err != nil {
		t.Fatalf("GetESStatus() error = %v", err)
	}
	if status.BatterySOC != 80 {
		t.Errorf("BatterySOC = %d, want 80", status.BatterySOC)
	}
	if status.BatteryPower != 1500 {
		t.Errorf("BatteryPower = %v, want 1500", status.BatteryPower)
	}
	if status.BatteryCapacity != 0 {
		t.Errorf("BatteryCapacity = %v, want 0", status.BatteryCapacity)
	}
}

func TestGetESStatus_RequiresBatteryPower(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "State Of Charge") {
			w.Write([]byte(`{"value":80}`))
			return
		}
		http.Error(w, "power unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.URL, 11)
	_, err := client.GetESStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "get battery power") {
		t.Fatalf("GetESStatus() error = %v, want battery power error", err)
	}
}

func TestGetESStatus_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := New(server.URL, 11)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetESStatus(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("GetESStatus() error = %v, want context cancellation", err)
	}
}

func TestGetBatteryStatusContext_CancellationDuringOptionalSensors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "State Of Charge"):
			_, _ = w.Write([]byte(`{"value":80}`))
		case strings.Contains(r.URL.Path, "Battery Power"):
			_, _ = w.Write([]byte(`{"value":500}`))
		default:
			<-r.Context().Done()
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := New(server.URL, 11).GetBatteryStatusContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetESStatus() error = %v, want context deadline exceeded", err)
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
